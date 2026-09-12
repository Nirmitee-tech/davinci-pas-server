#!/usr/bin/env python3
"""Run the Inferno Da Vinci PAS Server suites (v2.2.1, v2.0.1) headlessly through the Inferno JSON API.

Python 3 standard library only.

Flow per suite:
  1. GET  {inferno}/api/test_suites                      (reachability)
  2. POST {inferno}/api/test_sessions                    (new session)
  3. GET  {inferno}/api/test_suites/{suite_id}           (top-level group order, titles)
  4. for each selected top-level group:
       POST {inferno}/api/test_runs                      (all resolved inputs every time)
       poll GET {inferno}/api/test_runs/{id}?include_results=true
       resolve waits without a human:
         - Subscription Setup handshake wait (subscriptions_r4_server_notification_delivery):
           wait until the handshake request is attached to the waiting result (max 60 s),
           then GET the confirmation URL.
         - Pended workflow notification wait (*subscription_notification_wait*):
           POST {control}/admin/pended/finalize, then keep polling until the server's
           event notification resumes the run (max --notification-timeout).
  5. GET {inferno}/api/test_sessions/{id}/results; write reports/<ts>_<label>_<suite>/ and LATEST.md.

Exit code: 0 when every selected test passed (omits allowed; skips allowed only with --allow-skips),
1 when anything failed or a wait could not be resolved, 2 for setup problems (Inferno unreachable, bad inputs),
3 when the only non-passing tests are documented kit defects in known_kit_issues.json (still a failure).
Concurrent runs are serialized with a lock file (reports/.suite-run.lock): Inferno uses SQLite and one validator.
"""

import argparse
import datetime as dt
import fcntl
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
PAS_DIR = HERE.parent
DEFAULT_REPORTS = HERE / "reports"
DEFAULT_LATEST = HERE / "LATEST.md"
DEFAULT_KNOWN_ISSUES = HERE / "known_kit_issues.json"

SUITES = {
    "v221": "davinci_pas_server_suite_v221",
    "v201": "davinci_pas_server_suite_v201",
}
SUBSCRIPTIONS_SERVER_SUITE = "subscriptions_r5_backport_r4_server"
DELIVERY_WAIT_SUFFIX = "subscriptions_r4_server_notification_delivery"
NOTIFICATION_WAIT_MARK = "subscription_notification_wait"
BAD_RESULTS = {"fail", "error", "cancel"}


class HarnessError(Exception):
    pass


def log(msg):
    stamp = dt.datetime.now().strftime("%H:%M:%S")
    print(f"[{stamp}] {msg}", flush=True)


def http(method, url, body=None, headers=None, timeout=60):
    """Return (status, parsed JSON or text). Never raises for HTTP status codes."""
    data = None
    hdrs = {"Accept": "application/json"}
    if headers:
        hdrs.update(headers)
    if body is not None:
        data = body if isinstance(body, bytes) else json.dumps(body).encode("utf-8")
        hdrs.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            status = resp.status
    except urllib.error.HTTPError as err:
        raw = err.read()
        status = err.code
    text = raw.decode("utf-8", errors="replace") if raw else ""
    try:
        return status, json.loads(text) if text else None
    except ValueError:
        return status, text


class Inferno:
    def __init__(self, base):
        self.base = base.rstrip("/")
        self.api = self.base + "/api"

    def get(self, path, **kw):
        return http("GET", self.api + path, **kw)

    def post(self, path, body, **kw):
        return http("POST", self.api + path, body=body, **kw)

    def session_url(self, suite_id, session_id):
        return f"{self.base}/{suite_id}/{session_id}"


# ---------------------------------------------------------------- inputs


def substitute(text, variables):
    for key, val in variables.items():
        text = text.replace("${" + key + "}", val)
    return text


def resolve_inputs(inputs_path, variables, bearer_token):
    path = Path(inputs_path)
    if not path.is_file():
        raise HarnessError(f"inputs file not found: {path}")
    spec = json.loads(path.read_text())
    resolved = {}
    for item in spec.get("inputs", []):
        name = item["name"]
        if "file" in item:
            file_path = (path.parent / item["file"]).resolve()
            if not file_path.is_file():
                raise HarnessError(f"input {name}: file not found {file_path}")
            raw = substitute(file_path.read_text(), variables)
            try:
                value = json.dumps(json.loads(raw), indent=2)
            except ValueError as err:
                raise HarnessError(f"input {name}: {file_path} is not valid JSON ({err})")
        else:
            value = item.get("value", "")
            value = substitute(value, variables) if isinstance(value, str) else json.dumps(value)
        resolved[name] = value
    # Harness-controlled inputs always win.
    resolved["server_endpoint"] = variables["FHIR_BASE"]
    resolved["access_token"] = variables["NOTIFICATION_TOKEN"]
    if bearer_token:
        # auth_type public + access_token and no refresh_token/token_url: Inferno never tries to refresh.
        resolved["smart_credentials"] = json.dumps({"auth_type": "public", "access_token": bearer_token})
    else:
        resolved.pop("smart_credentials", None)
    return spec.get("suite_id"), resolved


# ---------------------------------------------------------------- suite model


def index_runnables(suite):
    """Map runnable id -> {title, short_id, optional, top_group, kind} and return top-level groups in order."""
    index = {}
    top_groups = suite.get("test_groups") or []

    def walk(group, top_id):
        index[group["id"]] = {
            "title": group.get("title"),
            "short_id": group.get("short_id"),
            "optional": bool(group.get("optional")),
            "top_group": top_id,
            "kind": "group",
        }
        for test in group.get("tests") or []:
            index[test["id"]] = {
                "title": test.get("title"),
                "short_id": test.get("short_id"),
                "optional": bool(test.get("optional")),
                "top_group": top_id,
                "kind": "test",
            }
        for child in group.get("test_groups") or []:
            walk(child, top_id)

    for group in top_groups:
        walk(group, group["id"])
    return index, top_groups


def select_groups(top_groups, filters):
    if not filters:
        return list(top_groups)
    wanted = [f.strip().lower() for f in filters.split(",") if f.strip()]
    chosen = []
    for group in top_groups:
        keys = [str(group.get("short_id", "")).lower(), group["id"].lower(), (group.get("title") or "").lower()]
        if any(w == keys[0] or w == keys[1] or (w and w in keys[2]) for w in wanted):
            chosen.append(group)
    return chosen


# ---------------------------------------------------------------- waits


def waiting_result(run):
    """Return the waiting test result, or a stand-in built from the deepest waiting group.

    Inferno 1.4.3 lists the waiting test result; inferno.healthit.gov (inferno_core 1.4.4) was seen listing only the
    enclosing groups with result "wait" and test_id null, so the harness classifies the wait by group id then.
    """
    waits = [r for r in run.get("results") or [] if r.get("result") == "wait"]
    tests = [r for r in waits if r.get("test_id")]
    if tests:
        return tests[0]
    groups = [r for r in waits if r.get("test_group_id")]
    if not groups:
        return None
    deepest = max(groups, key=lambda r: len(r["test_group_id"]))
    stand_in = dict(deepest)
    stand_in["test_id"] = deepest["test_group_id"]
    stand_in["id"] = f"group-wait:{run.get('id')}:{deepest['test_group_id']}"
    stand_in["outputs"] = []
    return stand_in


def output_value(result, name):
    for out in result.get("outputs") or []:
        if out.get("name") == name:
            return out.get("value")
    return None


class WaitHandler:
    def __init__(self, args, inferno, suite_key, suite_id, token):
        self.args = args
        self.inferno = inferno
        self.suite_key = suite_key
        self.suite_id = suite_id
        self.token = token
        self.handled = {}
        self.problems = []

    def handle(self, run_id, run):
        result = waiting_result(run)
        if result is None:
            return True
        test_id = result.get("test_id") or ""
        key = result.get("id")
        state = self.handled.setdefault(key, {"since": time.time(), "done": False})

        if test_id.endswith(DELIVERY_WAIT_SUFFIX) or test_id.endswith("subscription_setup"):
            if not state["done"]:
                self._confirm_handshake(run_id, result, state)
            return True

        if NOTIFICATION_WAIT_MARK in test_id or "pended_use_case" in test_id:
            if not state["done"]:
                self._trigger_finalize()
                state["done"] = True
            waited = time.time() - state["since"]
            if waited > self.args.notification_timeout:
                skip_url = (
                    f"{self.inferno.base}/custom/{self.suite_id}/resume_skip?token="
                    + urllib.parse.quote_plus(f"notification {self.token}")
                )
                msg = (
                    f"No event notification resumed {test_id} within {self.args.notification_timeout} s. "
                    f"Manual step: make the server send the notification, or open {skip_url} "
                    "(records a skip for the wait test)."
                )
                log(msg)
                self.problems.append(msg)
                return False
            return True

        msg = f"Unexpected wait at {test_id}: {(result.get('result_message') or '')[:300]}"
        log(msg)
        self.problems.append(msg)
        return False

    def _confirm_handshake(self, run_id, result, state):
        # Inferno persists the incoming handshake but does not expose it on the waiting result through the JSON API
        # (verified live with kit 0.15.2 / inferno_core 1.4.3), so the harness asks the server instead:
        # GET {control}/admin/notifications lists delivery attempts (SPEC.md section 11). Servers without that route
        # get a fixed grace period; they retry the handshake until Inferno answers 2xx (SPEC.md 7.3).
        url = self.args.control_url.rstrip("/") + "/admin/notifications"
        deadline = state["since"] + 60
        delivered = None
        supported = True
        while time.time() < deadline:
            status, run = self.inferno.get(f"/test_runs/{run_id}?include_results=true")
            if waiting_result(run if status == 200 and isinstance(run, dict) else {}) is None:
                return
            try:
                cstatus, body = http("GET", url, timeout=10)
            except (urllib.error.URLError, OSError):
                cstatus, body = 0, None
            if cstatus == 200 and isinstance(body, dict):
                delivered = next((n for n in body.get("notifications", [])
                                  if n.get("kind") == "handshake" and n.get("delivered")
                                  and n.get("endpoint_token") == self.token), None)
                if delivered:
                    break
            else:
                supported = False
                break
            time.sleep(2)
        if delivered:
            log(f"Handshake delivered to Inferno (HTTP {delivered.get('status')}); confirming the Subscription Setup wait.")
        elif not supported:
            log(f"{url} not available (start the server with PAS_ADMIN_ENABLED=true for the run); "
                f"waiting {self.args.handshake_grace} s for the server's handshake retries, then confirming.")
            time.sleep(self.args.handshake_grace)
        else:
            log("Server reported no delivered handshake within 60 s; confirming anyway so the conformance tests record the gap.")
        url = output_value(result, "confirmation_url")
        if not url or "is too large" in str(url):
            url = (
                f"{self.inferno.base}/custom/{SUBSCRIPTIONS_SERVER_SUITE}/resume_pass?token="
                + urllib.parse.quote(f"notification {self.token}")
            )
        status, _ = http("GET", url, headers={"Accept": "*/*"})
        if status >= 400:
            log(f"Confirmation URL returned HTTP {status}: {url}")
        state["done"] = True

    def _trigger_finalize(self):
        if self.args.no_trigger:
            log("Pended notification wait reached; --no-trigger set, relying on server auto-finalize.")
            return
        url = self.args.control_url.rstrip("/") + "/admin/pended/finalize"
        try:
            status, body = http("POST", url, body={"version": self.suite_key}, timeout=30)
            log(f"POST {url} -> HTTP {status} {json.dumps(body)[:200] if body is not None else ''}")
            if status == 404:
                log("Control routes are disabled on the server (start it with PAS_ADMIN_ENABLED=true for the run); "
                    "relying on server auto-finalize.")
        except (urllib.error.URLError, OSError) as err:
            log(f"POST {url} failed ({err}); relying on server auto-finalize.")


# ---------------------------------------------------------------- runs


def start_run(inferno, session_id, group_id, inputs):
    body = {
        "test_session_id": session_id,
        "test_group_id": group_id,
        "inputs": [{"name": k, "value": v} for k, v in inputs.items()],
    }
    for attempt in range(30):
        status, run = inferno.post("/test_runs", body)
        if status in (200, 201) and isinstance(run, dict) and run.get("id"):
            return run
        if status == 409:
            log("Another test run is active in this session; waiting for it to finish.")
            time.sleep(5)
            continue
        raise HarnessError(f"POST /api/test_runs for {group_id} -> HTTP {status}: {str(run)[:500]}")
    raise HarnessError(f"Could not start run for {group_id}: session stayed busy")


def run_group(args, inferno, handler, session_id, group, inputs):
    log(f"Group {group.get('short_id')} {group.get('title')} ({group['id']})")
    run = start_run(inferno, session_id, group["id"], inputs)
    run_id = run["id"]
    started = time.time()
    last_status = None
    while True:
        status, run = inferno.get(f"/test_runs/{run_id}?include_results=true")
        if status != 200 or not isinstance(run, dict):
            raise HarnessError(f"GET test run {run_id} -> HTTP {status}")
        run_status = run.get("status")
        if run_status != last_status:
            log(f"  run {run_id} status {run_status}")
            last_status = run_status
        if run_status == "done":
            return True
        if run_status == "waiting" and not handler.handle(run_id, run):
            return False
        if time.time() - started > args.timeout:
            handler.problems.append(f"Group {group['id']} did not finish within {args.timeout} s")
            log(handler.problems[-1])
            return False
        time.sleep(args.poll)


# ---------------------------------------------------------------- report


def acquire_run_lock(reports_dir, label):
    """Serialize run_suite.py runs against one Inferno (SQLite database, shared validator)."""
    lock_path = Path(reports_dir) / ".suite-run.lock"
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    handle = open(lock_path, "a+")
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        handle.seek(0)
        log(f"Waiting for another run_suite.py run to finish ({handle.read().strip() or 'unknown'})")
        fcntl.flock(handle, fcntl.LOCK_EX)
    handle.seek(0)
    handle.truncate()
    handle.write(f"pid {os.getpid()} {label}\n")
    handle.flush()
    return handle


def load_known_issues(path, suite_id, public):
    """Entries: {id, suite, test_suffix, scope: all|local|public, reason, evidence}."""
    if not path or not Path(path).is_file():
        return []
    issues = json.loads(Path(path).read_text()).get("issues", [])
    wanted = {"all", "public"} if public else {"all", "local"}
    return [i for i in issues if i.get("suite") == suite_id and i.get("scope", "all") in wanted]


def summarize(results, index, selected, allow_skips, problems, known=()):
    latest = {}
    for result in results:
        rid = result.get("test_id") or result.get("test_group_id") or result.get("test_suite_id")
        if rid:
            latest[rid] = result
    selected_ids = {g["id"] for g in selected}
    per_group = {g["id"]: {"pass": 0, "fail": 0, "skip": 0, "omit": 0, "error": 0, "cancel": 0, "wait": 0,
                           "not_run": 0} for g in selected}
    non_pass = []
    failing = False
    known_only = True
    for rid, meta in index.items():
        if meta["kind"] != "test" or meta["top_group"] not in selected_ids:
            continue
        result = latest.get(rid)
        outcome = result.get("result") if result else "not_run"
        counts = per_group[meta["top_group"]]
        counts[outcome] = counts.get(outcome, 0) + 1
        if outcome in ("pass", "omit"):
            continue
        issue = next((k for k in known if rid.endswith(k["test_suffix"])), None)
        entry = {
            "id": rid,
            "short_id": meta["short_id"],
            "title": meta["title"],
            "result": outcome,
            "optional": meta["optional"],
            "known_issue": issue["id"] if issue else None,
            "result_message": (result or {}).get("result_message"),
            "errors": [m.get("message") for m in (result or {}).get("messages") or [] if m.get("type") == "error"],
        }
        non_pass.append(entry)
        bad = outcome in BAD_RESULTS or outcome in ("wait", "not_run") or \
            (outcome == "skip" and not meta["optional"] and not allow_skips)
        if bad:
            failing = True
            if not issue:
                known_only = False
    if problems:
        failing = True
        known_only = False
    totals = {}
    for counts in per_group.values():
        for k, v in counts.items():
            totals[k] = totals.get(k, 0) + v
    verdict = "PASS" if not failing else ("FAIL (known kit issues only)" if known_only else "FAIL")
    return {"per_group": per_group, "totals": totals, "non_pass": non_pass, "ok": not failing,
            "known_only": failing and known_only, "verdict": verdict,
            "group_results": {gid: (latest.get(gid) or {}).get("result", "not_run") for gid in selected_ids}}


def clean(text):
    return (text or "").replace(chr(0x2014), "-").strip()


def write_report(out_dir, meta, session, results, summary, selected):
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / "session.json").write_text(json.dumps({"meta": meta, "session": session}, indent=2))
    (out_dir / "results.json").write_text(json.dumps(results, indent=2))
    lines = [
        f"# PAS server suite report: {meta['suite_id']}",
        "",
        f"- Verdict: **{summary['verdict']}**",
        f"- Label: {meta['label']}",
        f"- Started (UTC): {meta['started']}",
        f"- Finished (UTC): {meta['finished']}",
        f"- Inferno: {meta['inferno']}",
        f"- Inferno session: {meta['session_url']}",
        f"- FHIR base under test: {meta['fhir_base']}",
        f"- Control URL: {meta['control_url']}",
        f"- Notification token: `{meta['notification_token']}`",
        f"- Totals: " + ", ".join(f"{k} {v}" for k, v in sorted(summary["totals"].items()) if v),
        "",
        "## Groups",
        "",
        "| Group | Title | Group result | pass | fail | skip | omit | error | other |",
        "|---|---|---|---|---|---|---|---|---|",
    ]
    for group in selected:
        c = summary["per_group"][group["id"]]
        other = c.get("cancel", 0) + c.get("wait", 0) + c.get("not_run", 0)
        lines.append(
            f"| {group.get('short_id')} | {clean(group.get('title'))} | {summary['group_results'][group['id']]} | "
            f"{c['pass']} | {c['fail']} | {c['skip']} | {c['omit']} | {c['error']} | {other} |"
        )
    if meta.get("problems"):
        lines += ["", "## Harness problems", ""] + [f"- {clean(p)}" for p in meta["problems"]]
    lines += ["", "## Tests not passing", ""]
    if not summary["non_pass"]:
        lines.append("None.")
    for entry in summary["non_pass"]:
        opt = " (optional)" if entry["optional"] else ""
        known = f" [known kit issue {entry['known_issue']}]" if entry.get("known_issue") else ""
        lines.append(f"### {entry['short_id']} {clean(entry['title'])}: {entry['result']}{opt}{known}")
        lines.append("")
        lines.append(f"- id: `{entry['id']}`")
        if entry["result_message"]:
            lines.append(f"- message: {clean(entry['result_message'])[:1500]}")
        for err in entry["errors"][:20]:
            lines.append(f"- error: {clean(err)[:800]}")
        lines.append("")
    (out_dir / "summary.md").write_text("\n".join(lines) + "\n")
    return out_dir / "summary.md"


def write_latest(latest_path, entries):
    """Keep the newest result per (label, suite) in latest_index.json next to LATEST.md and render all of them."""
    index_path = latest_path.parent / "latest_index.json"
    try:
        index = json.loads(index_path.read_text()) if index_path.is_file() else {}
    except ValueError:
        index = {}
    for entry in entries:
        rel = entry["summary"]
        try:
            rel = entry["summary"].relative_to(latest_path.parent)
        except ValueError:
            pass
        totals = ", ".join(f"{k} {v}" for k, v in sorted(entry["totals"].items()) if v) or "no results"
        index[f"{entry['label']}|{entry['suite_id']}"] = {
            "label": entry["label"], "suite_id": entry["suite_id"], "verdict": entry["verdict"], "totals": totals,
            "report": str(rel), "session_url": entry["session_url"],
            "finished": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
        }
    index_path.write_text(json.dumps(index, indent=2) + "\n")
    lines = ["# Latest PAS harness runs", "",
             "Newest run per label and suite. Exit codes: 0 pass, 1 fail, 2 setup problem, 3 known kit issues only.", "",
             "| Label | Suite | Verdict | Totals | Finished (UTC) | Report | Inferno session |",
             "|---|---|---|---|---|---|---|"]
    for item in sorted(index.values(), key=lambda i: (i["label"], i["suite_id"])):
        lines.append(f"| {item['label']} | {item['suite_id']} | **{item['verdict']}** | {item['totals']} | "
                     f"{item['finished']} | [{item['report']}]({item['report']}) | {item['session_url']} |")
    lines.append("")
    latest_path.write_text("\n".join(lines))


# ---------------------------------------------------------------- main


def run_suite(args, suite_key, stamp):
    suite_id = SUITES[suite_key]
    fhir_base = args.fhir_base.replace("{suite}", suite_key)
    token = args.notification_token or f"nirmitee-pas-{suite_key}-{secrets.token_hex(6)}"
    inputs_path = (args.inputs or str(PAS_DIR / "testdata" / "{suite}" / "inputs.json")).replace("{suite}", suite_key)
    declared_suite, inputs = resolve_inputs(inputs_path, {"FHIR_BASE": fhir_base, "NOTIFICATION_TOKEN": token},
                                            args.bearer_token)
    if declared_suite and declared_suite != suite_id:
        raise HarnessError(f"{inputs_path} declares suite {declared_suite}, expected {suite_id}")

    if args.dry_run:
        print(f"== {suite_id} inputs from {inputs_path}")
        for name, value in inputs.items():
            shown = value if len(value) <= 200 else value[:200] + f"... ({len(value)} chars)"
            print(f"  {name}: {shown}")
        return {"ok": True, "dry_run": True}

    inferno = Inferno(args.inferno)
    status, suites = inferno.get("/test_suites", timeout=30)
    if status != 200 or not isinstance(suites, list):
        raise HarnessError(f"Inferno not reachable at {inferno.api}/test_suites (HTTP {status})")
    if suite_id not in {s.get("id") for s in suites}:
        raise HarnessError(f"Suite {suite_id} not loaded in Inferno at {inferno.base}")

    status, session = inferno.post("/test_sessions", {"test_suite_id": suite_id})
    if status not in (200, 201) or not isinstance(session, dict):
        raise HarnessError(f"POST /api/test_sessions -> HTTP {status}: {str(session)[:300]}")
    session_id = session["id"]
    status, suite = inferno.get(f"/test_suites/{suite_id}")
    if status != 200 or not isinstance(suite, dict):
        raise HarnessError(f"GET /api/test_suites/{suite_id} -> HTTP {status}")
    index, top_groups = index_runnables(suite)
    selected = select_groups(top_groups, args.groups)
    if not selected:
        raise HarnessError(f"--groups {args.groups!r} matched no top-level group")
    session_url = inferno.session_url(suite_id, session_id)
    log(f"Session {session_id} for {suite_id}: {session_url}")
    log(f"FHIR base {fhir_base}, notification token {token}")

    started = dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds")
    handler = WaitHandler(args, inferno, suite_key, suite_id, token)
    for group in selected:
        if not run_group(args, inferno, handler, session_id, group, inputs):
            log("Stopping: a wait could not be resolved or the group timed out.")
            break

    status, results = inferno.get(f"/test_sessions/{session_id}/results")
    if status != 200 or not isinstance(results, list):
        results = []
        handler.problems.append(f"Could not read session results (HTTP {status})")
    public = "inferno.healthit.gov" in inferno.base
    known = load_known_issues(args.known_issues, suite_id, public)
    summary = summarize(results, index, selected, args.allow_skips, handler.problems, known)
    meta = {
        "suite_id": suite_id, "label": args.label, "started": started,
        "finished": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
        "inferno": inferno.base, "session_id": session_id, "session_url": session_url,
        "fhir_base": fhir_base, "control_url": args.control_url, "notification_token": token,
        "groups": [g["id"] for g in selected], "problems": handler.problems,
    }
    out_dir = Path(args.reports_dir) / f"{stamp}_{args.label}_{suite_key}"
    summary_path = write_report(out_dir, meta, session, results, summary, selected)
    log(f"{suite_id}: {summary['verdict']} {summary['totals']} -> {summary_path}")
    return {"ok": summary["ok"], "known_only": summary["known_only"], "verdict": summary["verdict"],
            "suite_id": suite_id, "label": args.label, "summary": summary_path,
            "totals": summary["totals"], "session_url": session_url}


def parse_args(argv):
    p = argparse.ArgumentParser(description="Run the Inferno Da Vinci PAS Server suites headlessly.")
    p.add_argument("--inferno", default="http://localhost:8280",
                   help="Inferno base URL (public: https://inferno.healthit.gov/suites)")
    p.add_argument("--suite", choices=["v221", "v201", "both"], default="v221")
    p.add_argument("--fhir-base", default="http://host.docker.internal:8085/{suite}/fhir",
                   help="FHIR base as seen by Inferno; {suite} is replaced by v221/v201")
    p.add_argument("--control-url", default="http://localhost:8085",
                   help="Server root as seen by this harness (for /admin/pended/finalize)")
    p.add_argument("--label", default="local")
    p.add_argument("--groups", help="Comma separated top-level group short ids, ids or title substrings")
    p.add_argument("--bearer-token", help="Access token Inferno sends to the server (smart_credentials)")
    p.add_argument("--notification-token", help="Token the server sends back on notifications (default random)")
    p.add_argument("--timeout", type=int, default=1800, help="Max seconds per group run")
    p.add_argument("--notification-timeout", type=int, default=150,
                   help="Max seconds to wait for the pended event notification")
    p.add_argument("--poll", type=float, default=2.0, help="Polling interval in seconds")
    p.add_argument("--handshake-grace", type=int, default=15,
                   help="Seconds to wait before confirming the handshake wait when the server has no /admin/notifications")
    p.add_argument("--no-trigger", action="store_true", help="Do not call /admin/pended/finalize")
    p.add_argument("--allow-skips", action="store_true", help="Do not treat skips on required tests as failure")
    p.add_argument("--inputs", help="Inputs file; {suite} is replaced (default testdata/{suite}/inputs.json)")
    p.add_argument("--known-issues", default=str(DEFAULT_KNOWN_ISSUES),
                   help="Documented kit defects; runs whose only failures are listed exit 3")
    p.add_argument("--reports-dir", default=str(DEFAULT_REPORTS))
    p.add_argument("--latest", default=str(DEFAULT_LATEST), help="LATEST.md path to rewrite")
    p.add_argument("--dry-run", action="store_true", help="Print resolved inputs and exit")
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv if argv is not None else sys.argv[1:])
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    suites = ["v221", "v201"] if args.suite == "both" else [args.suite]
    outcomes = []
    lock = None if args.dry_run else acquire_run_lock(args.reports_dir, f"{args.suite} {args.label}")  # noqa: F841
    try:
        for suite_key in suites:
            outcomes.append(run_suite(args, suite_key, stamp))
    except HarnessError as err:
        log(f"ERROR: {err}")
        return 2
    except (urllib.error.URLError, OSError) as err:
        log(f"ERROR: network problem: {err}")
        return 2
    if args.dry_run:
        return 0
    write_latest(Path(args.latest), outcomes)
    if all(o["ok"] for o in outcomes):
        return 0
    return 3 if all(o["ok"] or o["known_only"] for o in outcomes) else 1


if __name__ == "__main__":
    sys.exit(main())

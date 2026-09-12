#!/usr/bin/env python3
"""Minimal fake of the Inferno JSON API used only to validate run_suite.py offline.

It mimics the parts of Inferno the harness depends on:
  GET  /api/test_suites, GET /api/test_suites/{id}
  POST /api/test_sessions, GET /api/test_sessions/{id}/results, GET /api/test_sessions/{id}/last_test_run
  POST /api/test_runs (409 while a run is active), GET /api/test_runs/{id}?include_results=true
  POST /custom/subscriptions_r5_backport_r4_server/subscription/channel/notification_listener
       (identifier "notification <bearer>"; 500 when no run is waiting, like Inferno's SuiteEndpoint)
  GET  /custom/subscriptions_r5_backport_r4_server/resume_pass?token=...
  GET  /custom/{suite_id}/resume_skip?token=...

The group model is a small imitation of the real PAS server suites (same top-level order, same wait
semantics: the Subscription Setup delivery wait accepts multiple requests and needs the confirmation URL;
the pended wait resumes on the first notification carrying the token). It does no FHIR validation.
"""

import argparse
import json
import re
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LISTENER = "/custom/subscriptions_r5_backport_r4_server/subscription/channel/notification_listener"
LOCK = threading.Lock()
SESSIONS, RUNS, WAITS = {}, {}, {}
BASE = {"url": "http://127.0.0.1"}
X12_306 = "https://codesystem.x12.org/005010/306"


def now():
    return datetime.now(timezone.utc).isoformat()


class Wait(Exception):
    def __init__(self, identifier, multiple, outputs=None):
        super().__init__(identifier)
        self.identifier, self.multiple, self.outputs = identifier, multiple, outputs or []


class Check(Exception):
    def __init__(self, result, message):
        super().__init__(message)
        self.result, self.message = result, message


def call(method, url, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Content-Type": "application/fhir+json", "Accept": "application/fhir+json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            status, raw = resp.status, resp.read()
    except urllib.error.HTTPError as err:
        status, raw = err.code, err.read()
    try:
        return status, json.loads(raw) if raw else None
    except ValueError:
        return status, None


def need(ctx, name):
    value = ctx["inputs"].get(name)
    if not value:
        raise Check("skip", f"input {name} not provided")
    return json.loads(value)


def codes_in(bundle):
    codes = set()
    for entry in (bundle or {}).get("entry") or []:
        res = entry.get("resource") or {}
        if res.get("resourceType") != "ClaimResponse":
            continue
        for item in res.get("item") or []:
            for adj in item.get("adjudication") or []:
                for ext in adj.get("extension") or []:
                    for inner in ext.get("extension") or []:
                        for coding in (inner.get("valueCodeableConcept") or {}).get("coding") or []:
                            if coding.get("system") == X12_306:
                                codes.add(coding.get("code"))
    return codes


# ---------------------------------------------------------------- test bodies


def t_capability(ctx):
    status, cs = call("GET", ctx["base"] + "/metadata")
    if status != 200:
        raise Check("fail", f"metadata HTTP {status}")
    rest = next((r for r in cs.get("rest", []) if r.get("mode") == "server"), {})
    sub = next((r for r in rest.get("resource", []) if r.get("type") == "Subscription"), {})
    have = {i.get("code") for i in sub.get("interaction", [])}
    if not {"create", "update", "delete"} <= have:
        raise Check("fail", f"Subscription interactions missing: {have}")


def t_subscription_input(ctx):
    sub = need(ctx, "subscription_resource")
    content = (((sub.get("channel") or {}).get("_payload") or {}).get("extension") or [{}])[0].get("valueCode")
    expected = "full-resource" if ctx["version"] == "v221" else "id-only"
    if content != expected:
        raise Check("fail", f"payload content {content}, expected {expected}")


def t_delivery(ctx, resumed):
    if resumed:
        return
    sub = need(ctx, "subscription_resource")
    token = ctx["inputs"]["access_token"]
    sub.setdefault("channel", {})["endpoint"] = BASE["url"] + LISTENER
    sub["channel"]["header"] = [f"Authorization: Bearer {token}"]
    status, created = call("POST", ctx["base"] + "/Subscription", sub)
    if status != 201:
        raise Check("fail", f"Subscription create HTTP {status}")
    ctx["session"]["subscription"] = created
    confirmation = f"{BASE['url']}/custom/subscriptions_r5_backport_r4_server/resume_pass?token=notification%20{token}"
    raise Wait(f"notification {token}", True, [{"name": "confirmation_url", "value": confirmation}])


def t_creation_response(ctx):
    created = ctx["session"].get("subscription")
    if not created:
        raise Check("skip", "no Subscription created")
    if created.get("status") != "requested":
        raise Check("fail", f"status {created.get('status')}")


def notifications(ctx, kind):
    found = []
    for req in ctx["session"]["incoming"]:
        entry0 = ((req["body"] or {}).get("entry") or [{}])[0].get("resource") or {}
        types = [p.get("valueCode") for p in entry0.get("parameter", []) if p.get("name") == "type"]
        if kind in types:
            found.append(req)
    return found


def t_handshake(ctx):
    if not notifications(ctx, "handshake"):
        raise Check("fail", "no handshake received")


def submit_expect(input_name, code):
    def body(ctx):
        bundle = need(ctx, input_name)
        for b in bundle if isinstance(bundle, list) else [bundle]:
            status, resp = call("POST", ctx["base"] + "/Claim/$submit", b)
            if status not in (200, 201):
                raise Check("fail", f"$submit HTTP {status}")
            if code and code not in codes_in(resp):
                raise Check("fail", f"expected adjudication code {code}, got {sorted(codes_in(resp))}")
    return body


def inquire(input_name):
    def body(ctx):
        bundle = need(ctx, input_name)
        for b in bundle if isinstance(bundle, list) else [bundle]:
            status, resp = call("POST", ctx["base"] + "/Claim/$inquire", b)
            if status not in (200, 201):
                raise Check("fail", f"$inquire HTTP {status}")
            expected = "Parameters" if ctx["version"] == "v221" else "Bundle"
            if (resp or {}).get("resourceType") != expected:
                raise Check("fail", f"expected {expected}, got {(resp or {}).get('resourceType')}")
    return body


def t_pended_wait(ctx, resumed):
    if resumed:
        return
    raise Wait(f"notification {ctx['inputs']['access_token']}", False)


def t_event_conformance(ctx):
    events = notifications(ctx, "event-notification")
    if not events:
        raise Check("skip", "no event-notification received")
    bundle = events[-1]["body"]
    entry1 = (bundle.get("entry") or [{}, {}])[1] if len(bundle.get("entry") or []) > 1 else {}
    if ctx["version"] == "v221" and "resource" not in entry1:
        raise Check("fail", "full-resource notification without the response Bundle")
    if ctx["version"] == "v201" and "resource" in entry1:
        raise Check("fail", "id-only notification must not carry the resource")


def t_error(op):
    def body(ctx):
        status, resp = call("POST", ctx["base"] + f"/Claim/${op}", {"resourceType": "Bundle", "type": "collection",
                                                                     "entry": []})
        if 200 <= status < 300 or (resp or {}).get("resourceType") != "OperationOutcome":
            raise Check("fail", f"expected non-2xx OperationOutcome, got HTTP {status}")
    return body


def build_suite(version):
    suite_id = f"davinci_pas_server_suite_{version}"
    v = version

    def test(gid, key, title, fn, waits=False):
        return {"id": f"{gid}-{key}", "title": title, "fn": fn, "waits": waits, "optional": False}

    def group(gid, title, tests=None, groups=None):
        return {"id": gid, "title": title, "tests": tests or [], "groups": groups or []}

    g1 = f"{suite_id}-pas_server_{v}_subscription_setup"
    setup_tests = []
    if v == "v221":
        setup_tests.append(test(g1, f"pas_server_{v}_subscription_capability_statement_test", "CapabilityStatement",
                                t_capability))
    setup_tests += [
        test(g1, f"pas_server_{v}_subscription_input_conformance", "Verify Subscription PAS conformance",
             t_subscription_input),
        test(g1, "subscriptions_r4_server_notification_delivery", "Send Subscription and Receive Handshake",
             t_delivery, waits=True),
        test(g1, "subscriptions_r4_server_creation_response_conformance", "Creation response", t_creation_response),
        test(g1, "subscriptions_r4_server_handshake_conformance", "Handshake verification", t_handshake),
    ]
    g2 = f"{suite_id}-Group02"
    approval = group(f"{g2}-pas_server_{v}_approval_use_case", "Successful Approval Workflow", tests=[
        test(f"{g2}-a", "submit", "Approval $submit", submit_expect("approval_pa_submit_request_payload", "A1"))])
    denial = group(f"{g2}-pas_server_{v}_denial_use_case", "Successful Denial Workflow", tests=[
        test(f"{g2}-d", "submit", "Denial $submit", submit_expect("denial_pa_submit_request_payload", "A3"))])
    pg = f"{g2}-pas_server_{v}_pended_use_case"
    pended_notify = [
        test(pg, f"pas_server_{v}_subscription_notification_wait_pended", "Notification wait", t_pended_wait, True),
        test(pg, f"pas_server_{v}_subscription_notification_conformance_pended", "Event notification", t_event_conformance),
    ]
    if v == "v201":
        pended_notify.append(test(pg, f"pas_v201_claim_inquire_operation_test_pended", "Pended $inquire",
                                  inquire("pended_pa_inquire_request_payload")))
    pended = group(pg, "Successful Pended Workflow", tests=[
        test(pg, "submit", "Pended $submit", submit_expect("pended_pa_submit_request_payload", "A4"))] + pended_notify)
    workflow = [approval, denial, pended]
    if v == "v221":
        cu = f"{g2}-pas_server_v221_claim_updates"
        workflow.append(group(cu, "Claim Updates", tests=[
            test(cu, key, key, submit_expect(f"claim_update_{key}_submit_payload", None))
            for key in ("initial", "add_item", "modify_cancel", "cancel_all")]))
    g3 = f"{suite_id}-pas_server_{v}_must_support"
    g4 = f"{suite_id}-pas_{v}_error_group"
    top = [
        group(g1, "Subscription Setup", tests=setup_tests),
        group(g2, "Demonstrate Workflow Support", groups=workflow),
        group(g3, "Demonstrate Element Support", tests=[
            test(g3, "submit", "Must support $submit", submit_expect("must_support_pa_submit_request_payload", None)),
            test(g3, "inquire", "Must support $inquire", inquire("must_support_pa_inquire_request_payload"))]),
        group(g4, "Demonstrate Error Handling", tests=[
            test(g4, f"pas_server_{v}_prior_auth_submission_error", "Submit error", t_error("submit")),
            test(g4, f"pas_server_{v}_prior_auth_inquiry_error", "Inquire error", t_error("inquire"))]),
    ]

    def number(groups, prefix=""):
        for i, grp in enumerate(groups, 1):
            grp["short_id"] = f"{prefix}{i}"
            for j, t in enumerate(grp["tests"], 1):
                t["short_id"] = f"{grp['short_id']}.{j:02d}"
            number(grp["groups"], grp["short_id"] + ".")

    number(top)
    return {"id": suite_id, "title": f"Da Vinci PAS Server Suite {version} (fake)", "groups": top}


SUITES = {s["id"]: s for s in (build_suite("v221"), build_suite("v201"))}


def serialize_group(grp):
    return {"id": grp["id"], "short_id": grp["short_id"], "title": grp["title"], "optional": False,
            "tests": [{"id": t["id"], "short_id": t["short_id"], "title": t["title"], "optional": False}
                      for t in grp["tests"]],
            "test_groups": [serialize_group(g) for g in grp["groups"]]}


def find_group(suite, gid):
    stack = list(suite["groups"])
    while stack:
        grp = stack.pop()
        if grp["id"] == gid:
            return grp
        stack.extend(grp["groups"])
    return None


# ---------------------------------------------------------------- execution


def record(session, run, rid, key, result, message="", outputs=None):
    res = session["results"].get(rid) or {"id": str(uuid.uuid4()), "created_at": now(), "requests": []}
    res.update({key: rid, "test_run_id": run["id"], "test_session_id": session["id"], "result": result,
                "result_message": message, "outputs": outputs or res.get("outputs", []), "messages": [],
                "optional": False, "updated_at": now()})
    session["results"][rid] = res
    return res


def execute_group(session, run, grp):
    outcomes = []
    for t in grp["tests"]:
        ctx = {"inputs": session["inputs"], "base": session["inputs"].get("server_endpoint", "").rstrip("/"),
               "version": session["suite_id"][-4:], "session": session}
        resumed = False
        while True:
            try:
                if t["waits"]:
                    t["fn"](ctx, resumed)
                else:
                    t["fn"](ctx)
                record(session, run, t["id"], "test_id", "pass")
                outcomes.append("pass")
                break
            except Wait as w:
                res = record(session, run, t["id"], "test_id", "wait", f"waiting for {w.identifier}", w.outputs)
                event = threading.Event()
                with LOCK:
                    WAITS[w.identifier] = {"run": run, "result": res, "multiple": w.multiple, "event": event,
                                           "session": session}
                    run["status"] = "waiting"
                event.wait(600)
                with LOCK:
                    run["status"] = "running"
                    info = WAITS.pop(w.identifier, {})
                verdict = info.get("verdict", "fail")
                if verdict != "pass":
                    record(session, run, t["id"], "test_id", verdict, "wait ended with " + verdict)
                    outcomes.append(verdict)
                    break
                resumed = True
            except Check as c:
                record(session, run, t["id"], "test_id", c.result, c.message)
                outcomes.append(c.result)
                break
            except Exception as err:  # noqa: BLE001
                record(session, run, t["id"], "test_id", "error", repr(err))
                outcomes.append("error")
                break
    for child in grp["groups"]:
        outcomes.append(execute_group(session, run, child))
    overall = "fail" if {"fail", "error"} & set(outcomes) else "skip" if "skip" in outcomes else "pass"
    record(session, run, grp["id"], "test_group_id", overall)
    return overall


def run_thread(session, run, grp):
    run["status"] = "running"
    execute_group(session, run, grp)
    run["status"] = "done"


def resume(identifier, verdict):
    with LOCK:
        info = WAITS.get(identifier)
        if not info:
            return False
        info["verdict"] = verdict
        info["event"].set()
    return True


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def reply(self, status, body):
        data = json.dumps(body).encode() if body is not None else b""
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def read(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            return json.loads(raw) if raw else {}
        except ValueError:
            return {}

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        path, query = parsed.path, urllib.parse.parse_qs(parsed.query)
        if path == "/api/test_suites":
            return self.reply(200, [{"id": s["id"], "title": s["title"]} for s in SUITES.values()])
        m = re.match(r"^/api/test_suites/([^/]+)$", path)
        if m and m.group(1) in SUITES:
            s = SUITES[m.group(1)]
            return self.reply(200, {"id": s["id"], "title": s["title"],
                                    "test_groups": [serialize_group(g) for g in s["groups"]]})
        m = re.match(r"^/api/test_runs/([^/]+)$", path)
        if m and m.group(1) in RUNS:
            run = RUNS[m.group(1)]
            body = {"id": run["id"], "test_session_id": run["session"], "status": run["status"],
                    "test_group_id": run["group"]}
            if query.get("include_results") == ["true"]:
                session = SESSIONS[run["session"]]
                body["results"] = [r for r in session["results"].values() if r["test_run_id"] == run["id"]]
            return self.reply(200, body)
        m = re.match(r"^/api/test_sessions/([^/]+)/results$", path)
        if m and m.group(1) in SESSIONS:
            return self.reply(200, list(SESSIONS[m.group(1)]["results"].values()))
        if path == "/custom/subscriptions_r5_backport_r4_server/resume_pass":
            ok = resume(query.get("token", [""])[0], "pass")
            return self.reply(200 if ok else 500, {"resumed": ok})
        m = re.match(r"^/custom/([^/]+)/resume_skip$", path)
        if m:
            ok = resume(query.get("token", [""])[0], "skip")
            return self.reply(200 if ok else 500, {"resumed": ok})
        return self.reply(404, {"error": "not found"})

    def do_POST(self):
        path = urllib.parse.urlparse(self.path).path
        body = self.read()
        if path == "/api/test_sessions":
            sid = str(uuid.uuid4())
            SESSIONS[sid] = {"id": sid, "suite_id": body.get("test_suite_id"), "inputs": {}, "results": {},
                             "incoming": [], "active": None}
            return self.reply(200, {"id": sid, "test_suite_id": body.get("test_suite_id")})
        if path == "/api/test_runs":
            session = SESSIONS.get(body.get("test_session_id"))
            if not session:
                return self.reply(422, {"errors": "unknown session"})
            active = session["active"]
            if active and RUNS[active]["status"] != "done":
                return self.reply(409, {"error": "Cannot run new test while another test run is in progress"})
            grp = find_group(SUITES[session["suite_id"]], body.get("test_group_id"))
            if not grp:
                return self.reply(422, {"errors": "unknown group"})
            for item in body.get("inputs") or []:
                session["inputs"][item["name"]] = item["value"]
            rid = str(uuid.uuid4())
            run = {"id": rid, "session": session["id"], "group": grp["id"], "status": "queued"}
            RUNS[rid] = run
            session["active"] = rid
            threading.Thread(target=run_thread, args=(session, run, grp), daemon=True).start()
            return self.reply(200, {"id": rid, "status": "queued", "test_session_id": session["id"]})
        if path == LISTENER:
            token = (self.headers.get("Authorization") or "").replace("Bearer ", "", 1)
            identifier = f"notification {token}"
            with LOCK:
                info = WAITS.get(identifier)
                if info:
                    info["session"]["incoming"].append({"body": body, "headers": dict(self.headers)})
                    info["result"]["requests"].append({"id": str(uuid.uuid4()), "direction": "incoming",
                                                       "verb": "POST", "status": 200, "url": path})
            if not info:
                return self.reply(500, {"error": f"identifier '{identifier}' is not associated with a waiting session"})
            if not info["multiple"]:
                resume(identifier, "pass")
            return self.reply(200, None)
        return self.reply(404, {"error": "not found"})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8280)
    parser.add_argument("--host", default="127.0.0.1")
    args = parser.parse_args()
    BASE["url"] = f"http://{args.host}:{args.port}"
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"[fake-inferno] listening on {BASE['url']}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()

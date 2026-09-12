#!/usr/bin/env python3
"""Tiny PAS server stub used only to validate the harness flow offline. It is NOT the contract.

The real behavior contract is spec/SPEC.md. This stub implements just enough of the interface:
  GET  {prefix}/health, POST {prefix}/admin/reset, POST {prefix}/admin/pended/finalize
  per version v in (v221, v201), base {prefix}/{v}/fhir:
    GET  metadata                     CapabilityStatement with Subscription create/update/delete
    POST Claim/$submit                PAS-shaped response Bundle (200)
    POST Claim/$inquire               v201: Bundle, v221: Parameters with parameter "return"
    POST Subscription                 201, status requested, async handshake with retries
    GET/PUT/DELETE Subscription/{id}
  Empty or non-Claim Bundles get HTTP 400 with an OperationOutcome.

Trivial decision rule (stub only): the first Claim.identifier value, lower-cased:
  contains "deny" -> A3 (Not Certified), contains "pend" -> A4 (Pending, finalized to A1 later), else A1.

Environment:
  PAS_PENDED_AUTO_FINALIZE_SECONDS  default 10; 0 disables auto-finalize
  PAS_NOTIFY_RETRY_SECONDS          default 3
  PAS_NOTIFY_MAX_SECONDS            default 280
"""

import argparse
import json
import os
import re
import threading
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

X12_306 = "https://codesystem.x12.org/005010/306"
REVIEW_ACTION = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewAction"
REVIEW_ACTION_CODE = "http://hl7.org/fhir/us/davinci-pas/StructureDefinition/extension-reviewActionCode"
TOPIC = "http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic"
DISPLAY = {"A1": "Certified in total", "A3": "Not Certified", "A4": "Pending"}

AUTO_FINALIZE = float(os.environ.get("PAS_PENDED_AUTO_FINALIZE_SECONDS", "10"))
RETRY = float(os.environ.get("PAS_NOTIFY_RETRY_SECONDS", "3"))
MAX_NOTIFY = float(os.environ.get("PAS_NOTIFY_MAX_SECONDS", "280"))

LOCK = threading.Lock()
STATE = {"subscriptions": {}, "pended": [], "responses": {}}


def now():
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def log(msg):
    print(f"[stub-pas {datetime.now().strftime('%H:%M:%S')}] {msg}", flush=True)


def outcome(code, text):
    return {"resourceType": "OperationOutcome",
            "issue": [{"severity": "error", "code": code, "details": {"text": text}}]}


def decision_for(claim):
    ident = ((claim.get("identifier") or [{}])[0].get("value") or "").lower()
    if "deny" in ident:
        return "A3"
    if "pend" in ident:
        return "A4"
    return "A1"


def response_bundle(request_bundle, claim_entry, code, inquiry=False):
    claim = claim_entry["resource"]
    cr_id = str(uuid.uuid4())
    items = []
    for item in claim.get("item") or [{"sequence": 1}]:
        items.append({
            "itemSequence": item.get("sequence", 1),
            "adjudication": [{
                "extension": [{"url": REVIEW_ACTION, "extension": [{
                    "url": REVIEW_ACTION_CODE,
                    "valueCodeableConcept": {"coding": [{"system": X12_306, "code": code, "display": DISPLAY[code]}]},
                }]}],
                "category": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/adjudication",
                                         "code": "submitted"}]},
            }],
        })
    profile = "profile-claiminquiryresponse" if inquiry else "profile-claimresponse"
    claim_response = {
        "resourceType": "ClaimResponse", "id": cr_id,
        "meta": {"profile": [f"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/{profile}"]},
        "identifier": claim.get("identifier"), "status": "active", "type": claim.get("type"),
        "use": "preauthorization", "patient": claim.get("patient"), "created": now(),
        "insurer": claim.get("insurer"), "requestor": claim.get("provider"),
        "request": {"reference": claim_entry.get("fullUrl")}, "outcome": "complete", "item": items,
    }
    wanted = {(claim.get(k) or {}).get("reference") for k in ("patient", "insurer", "provider")}
    echoed = [e for e in request_bundle.get("entry", [])[1:]
              if e.get("fullUrl") in wanted or any(e.get("fullUrl", "").endswith("/" + (w or "#")) for w in wanted)]
    bundle_profile = "profile-pas-inquiry-response-bundle" if inquiry else "profile-pas-response-bundle"
    return {
        "resourceType": "Bundle", "id": str(uuid.uuid4()),
        "meta": {"profile": [f"http://hl7.org/fhir/us/davinci-pas/StructureDefinition/{bundle_profile}"]},
        "identifier": {"system": "urn:ietf:rfc:3986", "value": f"urn:uuid:{uuid.uuid4()}"},
        "type": "collection", "timestamp": now(),
        "entry": [{"fullUrl": f"urn:uuid:{cr_id}", "resource": claim_response}] + echoed,
    }


def status_parameters(sub, sub_url, kind, status, events, focus=None):
    params = [
        {"name": "subscription", "valueReference": {"reference": sub_url}},
        {"name": "topic", "valueCanonical": sub.get("criteria", TOPIC)},
        {"name": "status", "valueCode": status},
        {"name": "type", "valueCode": kind},
        {"name": "events-since-subscription-start", "valueString": str(events)},
    ]
    if focus:
        params.append({"name": "notification-event", "part": [
            {"name": "event-number", "valueString": str(events)},
            {"name": "timestamp", "valueInstant": now()},
            {"name": "focus", "valueReference": {"reference": focus}},
        ]})
    return {"resourceType": "Parameters", "parameter": params}


def send_with_retry(sub, body, label):
    channel = sub.get("channel") or {}
    endpoint = channel.get("endpoint")
    headers = {"Content-Type": channel.get("payload") or "application/fhir+json"}
    for header in channel.get("header") or []:
        name, _, value = header.partition(": ")
        headers[name] = value
    data = json.dumps(body).encode()
    deadline = time.time() + MAX_NOTIFY
    attempt = 0
    while time.time() < deadline:
        attempt += 1
        req = urllib.request.Request(endpoint, data=data, method="POST", headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                status = resp.status
        except urllib.error.HTTPError as err:
            status = err.code
        except (urllib.error.URLError, OSError):
            status = 0
        if 200 <= status < 300:
            log(f"{label} delivered on attempt {attempt}")
            return True
        time.sleep(RETRY)
    log(f"{label} gave up after {attempt} attempts")
    return False


def handshake(sub_id):
    time.sleep(1)
    with LOCK:
        entry = STATE["subscriptions"].get(sub_id)
    if not entry:
        return
    sub, base_url = entry["resource"], entry["base_url"]
    sub_url = f"{base_url}/Subscription/{sub_id}"
    bundle = {"resourceType": "Bundle", "id": str(uuid.uuid4()), "type": "history", "timestamp": now(),
              "entry": [{"fullUrl": f"urn:uuid:{uuid.uuid4()}",
                         "resource": status_parameters(sub, sub_url, "handshake", "requested", 0),
                         "request": {"method": "GET", "url": f"{sub_url}/$status"},
                         "response": {"status": "200"}}]}
    if send_with_retry(sub, bundle, f"handshake {sub_id}"):
        with LOCK:
            sub["status"] = "active"


def notify(version, final_bundle):
    with LOCK:
        subs = [(sid, e) for sid, e in STATE["subscriptions"].items() if e["version"] == version]
    count = 0
    for sub_id, entry in subs:
        sub, base_url = entry["resource"], entry["base_url"]
        sub_url = f"{base_url}/Subscription/{sub_id}"
        entry["events"] += 1
        content = (((sub.get("channel") or {}).get("_payload") or {}).get("extension") or [{}])[0].get("valueCode")
        if content == "full-resource":
            focus = f"urn:uuid:{final_bundle['id']}"
            extra = {"fullUrl": focus, "resource": final_bundle,
                     "request": {"method": "POST", "url": f"{base_url}/Bundle"}, "response": {"status": "201"}}
        else:
            cr_id = final_bundle["entry"][0]["resource"]["id"]
            focus = f"{base_url}/ClaimResponse/{cr_id}"
            extra = {"fullUrl": focus, "request": {"method": "GET", "url": focus}, "response": {"status": "200"}}
        bundle = {"resourceType": "Bundle", "id": str(uuid.uuid4()), "type": "history", "timestamp": now(),
                  "entry": [{"fullUrl": f"urn:uuid:{uuid.uuid4()}",
                             "resource": status_parameters(sub, sub_url, "event-notification", "active",
                                                           entry["events"], focus),
                             "request": {"method": "GET", "url": f"{sub_url}/$status"},
                             "response": {"status": "200"}}, extra]}
        threading.Thread(target=send_with_retry, args=(sub, bundle, f"event {sub_id}"), daemon=True).start()
        count += 1
    return count


def finalize(version=None):
    with LOCK:
        todo = [p for p in STATE["pended"] if not p["done"] and (version is None or p["version"] == version)]
        for p in todo:
            p["done"] = True
    finalized, notes = [], 0
    for p in todo:
        final = response_bundle(p["request"], p["claim_entry"], "A1")
        with LOCK:
            STATE["responses"][p["version"]] = final
        finalized.append(p["claim_entry"]["resource"].get("id"))
        notes += notify(p["version"], final)
    return finalized, notes


class Handler(BaseHTTPRequestHandler):
    prefix = ""

    def log_message(self, fmt, *args):
        pass

    def reply(self, status, body=None, content_type="application/fhir+json", headers=None):
        data = b"" if body is None else json.dumps(body).encode()
        self.send_response(status)
        if body is not None:
            self.send_header("Content-Type", content_type)
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def body_json(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            return json.loads(raw) if raw else None
        except ValueError:
            return None

    def route(self):
        path = self.path.split("?")[0]
        if not path.startswith(self.prefix):
            return None, None
        rest = path[len(self.prefix):]
        m = re.match(r"^/(v221|v201)/fhir(/.*)?$", rest)
        if m:
            return m.group(1), m.group(2) or "/"
        return None, rest

    def do_GET(self):
        version, rest = self.route()
        if version is None:
            if rest == "/health":
                return self.reply(200, {"status": "ok"}, "application/json")
            return self.reply(404, outcome("not-found", "unknown path"))
        if rest == "/metadata":
            return self.reply(200, {
                "resourceType": "CapabilityStatement", "status": "active", "date": "2026-09-12",
                "kind": "instance", "fhirVersion": "4.0.1", "format": ["json"],
                "implementation": {"description": "stub"},
                "rest": [{"mode": "server", "resource": [
                    {"type": "Claim", "operation": [{"name": "submit", "definition": "http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-submit"}]},
                    {"type": "Subscription", "interaction": [{"code": c} for c in ("create", "read", "update", "delete")]},
                ]}],
            })
        m = re.match(r"^/Subscription/([^/]+)$", rest)
        if m:
            with LOCK:
                entry = STATE["subscriptions"].get(m.group(1))
            return self.reply(200, entry["resource"]) if entry else self.reply(404, outcome("not-found", "no such Subscription"))
        return self.reply(404, outcome("not-found", "unknown path"))

    def do_DELETE(self):
        version, rest = self.route()
        m = re.match(r"^/Subscription/([^/]+)$", rest or "")
        if version and m:
            with LOCK:
                STATE["subscriptions"].pop(m.group(1), None)
            return self.reply(204)
        return self.reply(404, outcome("not-found", "unknown path"))

    def do_PUT(self):
        version, rest = self.route()
        m = re.match(r"^/Subscription/([^/]+)$", rest or "")
        body = self.body_json()
        if version and m and isinstance(body, dict):
            with LOCK:
                entry = STATE["subscriptions"].get(m.group(1))
                if entry:
                    body["id"] = m.group(1)
                    entry["resource"] = body
            return self.reply(200, body) if entry else self.reply(404, outcome("not-found", "no such Subscription"))
        return self.reply(400, outcome("invalid", "bad request"))

    def do_POST(self):
        version, rest = self.route()
        body = self.body_json()
        if version is None:
            if rest.startswith("/admin/") and os.environ.get("PAS_ADMIN_ENABLED") != "true":
                return self.reply(404, outcome("not-found", "Not found."))
            if rest == "/admin/reset":
                with LOCK:
                    STATE["subscriptions"].clear()
                    STATE["pended"].clear()
                    STATE["responses"].clear()
                return self.reply(204)
            if rest == "/admin/pended/finalize":
                wanted = (body or {}).get("version") if isinstance(body, dict) else None
                finalized, notes = finalize(wanted)
                log(f"admin finalize {wanted}: {finalized} notifications {notes}")
                return self.reply(200, {"finalized": finalized, "notifications": notes}, "application/json")
            return self.reply(404, outcome("not-found", "unknown path"))

        if rest in ("/Claim/$submit", "/Claim/$inquire"):
            if not isinstance(body, dict) or body.get("resourceType") != "Bundle" or not body.get("entry"):
                return self.reply(400, outcome("invalid", "Request must be a non-empty PAS Bundle"))
            claim_entry = next((e for e in body["entry"] if (e.get("resource") or {}).get("resourceType") == "Claim"), None)
            if claim_entry is None:
                return self.reply(400, outcome("invalid", "Bundle contains no Claim"))
            if rest == "/Claim/$submit":
                code = decision_for(claim_entry["resource"])
                bundle = response_bundle(body, claim_entry, code)
                if code == "A4":
                    record = {"version": version, "request": body, "claim_entry": claim_entry, "done": False}
                    with LOCK:
                        STATE["pended"].append(record)
                    if AUTO_FINALIZE > 0:
                        threading.Timer(AUTO_FINALIZE, finalize, args=(version,)).start()
                return self.reply(200, bundle)
            bundle = response_bundle(body, claim_entry, "A1", inquiry=True)
            if version == "v221":
                return self.reply(200, {"resourceType": "Parameters",
                                        "parameter": [{"name": "return", "resource": bundle}]})
            return self.reply(200, bundle)

        if rest == "/Subscription":
            if not isinstance(body, dict) or body.get("resourceType") != "Subscription":
                return self.reply(400, outcome("invalid", "Body must be a Subscription"))
            sub_id = str(uuid.uuid4())
            body["id"] = sub_id
            body["status"] = "requested"
            host = self.headers.get("Host") or "localhost"
            base_url = f"http://{host}{self.prefix}/{version}/fhir"
            with LOCK:
                STATE["subscriptions"][sub_id] = {"resource": body, "version": version, "base_url": base_url,
                                                  "events": 0}
            threading.Thread(target=handshake, args=(sub_id,), daemon=True).start()
            return self.reply(201, body, headers={"Location": f"{base_url}/Subscription/{sub_id}"})
        return self.reply(404, outcome("not-found", "unknown path"))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8085)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--prefix", default="")
    args = parser.parse_args()
    Handler.prefix = args.prefix.rstrip("/")
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    log(f"listening on http://{args.host}:{args.port}{Handler.prefix} (auto-finalize {AUTO_FINALIZE}s)")
    server.serve_forever()


if __name__ == "__main__":
    main()

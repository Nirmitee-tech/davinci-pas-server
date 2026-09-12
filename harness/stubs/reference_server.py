#!/usr/bin/env python3
"""Reference PAS server: an HTTP wrapper around the executable contract model (spec/reference/pas_reference.py).

Used to prove the harness and the testdata end to end against a real Inferno PAS kit before (and alongside) the Go
and .NET servers. It implements spec/SPEC.md sections 1, 3, 4, 5, 7, 9 and 11 with in-memory state:

  {prefix}/{v221|v201}/fhir/metadata                GET
  {prefix}/{v221|v201}/fhir/Claim/$submit           POST
  {prefix}/{v221|v201}/fhir/Claim/$inquire          POST
  {prefix}/{v221|v201}/fhir/Subscription            POST
  {prefix}/{v221|v201}/fhir/Subscription/{id}       GET, PUT, DELETE
  {prefix}/{v221|v201}/fhir/Subscription/{id}/$status GET
  {prefix}/{v221|v201}/fhir/ClaimResponse/{id}      GET
  {prefix}/admin/pended/finalize                    POST  (test-only, needs PAS_ADMIN_ENABLED=true)
  {prefix}/admin/reset                              POST  (test-only, needs PAS_ADMIN_ENABLED=true)
  {prefix}/admin/notifications                      GET   (test-only, needs PAS_ADMIN_ENABLED=true)
  {prefix}/health                                   GET

Notifications (handshake and event) are delivered by a background thread with the retry contract of SPEC.md 7.3.

Usage:
  PAS_ADMIN_ENABLED=true python3 harness/stubs/reference_server.py --port 8089   (for an Inferno run)
  PAS_AUTH_TOKENS=<token> PAS_ADMIN_ENABLED=true python3 harness/stubs/reference_server.py --port 8089

The /admin routes are disabled (404) unless PAS_ADMIN_ENABLED is exactly true (spec/SPEC.md section 11).

Environment: PAS_PUBLIC_BASE_URL, PAS_AUTH_TOKENS, PAS_PENDED_AUTO_FINALIZE_SECONDS, PAS_NOTIFICATION_RETRY_SECONDS,
PAS_NOTIFICATION_MAX_SECONDS, PAS_ADMIN_ENABLED, PAS_PREFIX. Python 3 standard library only. Synthetic data only.
"""
import argparse
import json
import os
import re
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
sys.path.insert(0, str(REPO / 'spec' / 'reference'))
import pas_reference as ref  # noqa: E402

FHIR_JSON = 'application/fhir+json; charset=utf-8'


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


class State:
    def __init__(self, public_base, rules, auto_finalize, retry_seconds, max_seconds):
        self.lock = threading.RLock()
        self.public_base = public_base.rstrip('/')
        self.rules = rules
        self.auto_finalize = auto_finalize
        self.retry_seconds = retry_seconds
        self.max_seconds = max_seconds
        self.deliveries = []
        self.reset()

    def reset(self):
        with self.lock:
            self.models = {v: ref.PasModel(v, f'{self.public_base}/{v}/fhir', self.rules, live=True) for v in ('v221', 'v201')}
            self.deliveries = []

    def drain_outbox(self, version):
        with self.lock:
            model = self.models[version]
            messages, model.outbox = model.outbox, []
        for message in messages:
            threading.Thread(target=self.deliver, args=(version, message), daemon=True).start()

    def deliver(self, version, message):
        delay = 1.0 if message['kind'] == 'handshake' else 0.0
        time.sleep(delay)
        body = json.dumps(message['body']).encode()
        deadline = time.time() + self.max_seconds
        attempt = 0
        auth = message['headers'].get('Authorization', '')
        entry = {'version': version, 'kind': message['kind'], 'subscription_id': message['subscription_id'],
                 'endpoint_token': auth[7:] if auth.startswith('Bearer ') else None, 'attempts': 0,
                 'status': None, 'delivered': False, 'queued_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}
        with self.lock:
            self.deliveries.append(entry)
        while True:
            attempt += 1
            req = urllib.request.Request(message['endpoint'], data=body, method='POST', headers=message['headers'])
            try:
                with urllib.request.urlopen(req, timeout=30) as resp:
                    status = resp.status
            except urllib.error.HTTPError as err:
                status = err.code
            except (urllib.error.URLError, OSError) as err:
                status = f'network error {err}'
            ok = isinstance(status, int) and 200 <= status < 300
            with self.lock:
                entry.update(attempts=attempt, status=status if isinstance(status, int) else None, delivered=ok)
            log(f"{version} {message['kind']} for Subscription/{message['subscription_id']} attempt {attempt} -> {status}")
            if ok:
                if message['kind'] == 'handshake':
                    with self.lock:
                        sub = self.models[version].subscriptions.get(message['subscription_id'])
                        if sub:
                            sub['resource']['status'] = 'active'
                return
            if time.time() + self.retry_seconds > deadline:
                log(f"{version} {message['kind']} for Subscription/{message['subscription_id']} abandoned after {attempt} attempts")
                return
            time.sleep(self.retry_seconds)

    def finalize(self, version):
        with self.lock:
            finalized = self.models[version].finalize_pended()
            queued = len(self.models[version].outbox)
        self.drain_outbox(version)
        return finalized, queued

    def schedule_finalize(self, version):
        if self.auto_finalize <= 0:
            return
        timer = threading.Timer(self.auto_finalize, lambda: log(f'{version} auto finalize: {len(self.finalize(version)[0])} claim(s)'))
        timer.daemon = True
        timer.start()


class Handler(BaseHTTPRequestHandler):
    server_version = 'NirmiteePASReference/0.1'
    state: State = None
    prefix = ''
    tokens = []
    enable_admin = False

    def log_message(self, fmt, *args):
        log(f'{self.address_string()} {fmt % args}')

    # ------------------------------------------------------------------ helpers
    def send_json(self, status, payload, content_type=FHIR_JSON, extra=None):
        data = json.dumps(payload).encode() if payload is not None else b''
        self.send_response(status)
        if payload is not None:
            self.send_header('Content-Type', content_type)
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        if data:
            self.wfile.write(data)

    def outcome(self, status, message, code):
        self.send_json(status, ref.operation_outcome(message, code))

    def read_body(self):
        length = int(self.headers.get('Content-Length') or 0)
        raw = self.rfile.read(length) if length else b''
        if not raw:
            return None, 'empty body'
        try:
            return json.loads(raw), None
        except ValueError as err:
            return None, f'invalid JSON: {err}'

    def route(self):
        path = self.path.split('?', 1)[0]
        if self.prefix and path.startswith(self.prefix):
            path = path[len(self.prefix):]
        return path

    def authorized(self):
        if not self.tokens:
            return True
        header = self.headers.get('Authorization') or ''
        if header.startswith('Bearer ') and header[7:] in self.tokens:
            return True
        self.send_json(401, ref.operation_outcome('A valid bearer token is required.', 'security'),
                       extra={'WWW-Authenticate': 'Bearer'})
        return False

    def admin_allowed(self):
        if not self.enable_admin:
            self.outcome(404, 'Not found.', 'not-found')
            return False
        if self.headers.get('X-Forwarded-For') or self.headers.get('Forwarded'):
            self.outcome(403, 'Control endpoints are not available through a proxy.', 'forbidden')
            return False
        return True

    # ------------------------------------------------------------------ dispatch
    def handle_any(self, method):
        path = self.route()
        if path == '/health' and method == 'GET':
            return self.send_json(200, {'status': 'ok'}, 'application/json')
        if path == '/admin/notifications' and method == 'GET':
            if not self.admin_allowed():
                return None
            with self.state.lock:
                return self.send_json(200, {'notifications': list(self.state.deliveries)}, 'application/json')
        if path.startswith('/admin/'):
            if method != 'POST':
                return self.outcome(405, 'Method not allowed.', 'not-supported')
            if not self.admin_allowed():
                return None
            if path == '/admin/reset':
                self.state.reset()
                return self.send_json(204, None)
            if path == '/admin/pended/finalize':
                body, _ = self.read_body()
                versions = [body['version']] if isinstance(body, dict) and body.get('version') in ('v221', 'v201') else ['v221', 'v201']
                finalized, notifications = [], 0
                for v in versions:
                    done, queued = self.state.finalize(v)
                    finalized += [b['entry'][0]['resource']['id'] for b in done]
                    notifications += queued
                return self.send_json(200, {'finalized': finalized, 'notifications': notifications}, 'application/json')
            return self.outcome(404, 'Not found.', 'not-found')
        m = re.match(r'^/(v221|v201)/fhir(/.*)?$', path)
        if not m:
            return self.outcome(404, f'Unknown route {path}.', 'not-found')
        version, rest = m.group(1), m.group(2) or '/'
        model = self.state.models[version]
        if rest == '/metadata' and method == 'GET':
            return self.send_json(200, model.metadata())
        if not self.authorized():
            return None
        if rest in ('/Claim/$submit', '/Claim/$inquire'):
            if method != 'POST':
                return self.outcome(405, 'Method not allowed.', 'not-supported')
            body, err = self.read_body()
            if err:
                return self.outcome(400, f'The request body must be a PAS Bundle ({err}).', 'structure')
            with self.state.lock:
                if rest.endswith('submit'):
                    status, payload = model.submit(body)
                    pending = status == 200 and any(r['decision'] == 'pended' and r['rule'].get('autoFinalize')
                                                    for r in model.records[model.claim_key(body['entry'][0]['resource'])]['results'])
                else:
                    status, payload = model.inquire(body)
                    pending = False
            if pending:
                self.state.schedule_finalize(version)
            return self.send_json(status, payload)
        if rest == '/Subscription':
            if method != 'POST':
                return self.outcome(405, 'Method not allowed.', 'not-supported')
            body, err = self.read_body()
            if err:
                return self.outcome(400, f'The request body must be a Subscription ({err}).', 'structure')
            with self.state.lock:
                status, payload = model.create_subscription(body)
            if status == 201:
                self.state.drain_outbox(version)
                return self.send_json(201, payload, extra={'Location': f"{model.base}/Subscription/{payload['id']}"})
            return self.send_json(status, payload)
        m2 = re.match(r'^/Subscription/([^/]+)(/\$status)?$', rest)
        if m2:
            sid, is_status = m2.group(1), m2.group(2)
            with self.state.lock:
                sub = model.subscriptions.get(sid)
                if not sub:
                    return self.outcome(404, f'Subscription/{sid} not found.', 'not-found')
                if is_status and method == 'GET':
                    params = model.status_parameters(sub['resource'], 'query-status', sub['resource'].get('status', 'active'))
                    return self.send_json(200, params)
                if method == 'GET':
                    return self.send_json(200, sub['resource'])
                if method == 'DELETE':
                    del model.subscriptions[sid]
                    return self.send_json(204, None)
                if method == 'PUT':
                    body, err = self.read_body()
                    if err or not isinstance(body, dict) or body.get('resourceType') != 'Subscription':
                        return self.outcome(400, 'The request body must be a Subscription.', 'structure')
                    body['id'] = sid
                    sub['resource'] = body
                    return self.send_json(200, body)
            return self.outcome(405, 'Method not allowed.', 'not-supported')
        m3 = re.match(r'^/ClaimResponse/([^/]+)$', rest)
        if m3 and method == 'GET':
            with self.state.lock:
                cr = model.claim_response_by_id(m3.group(1))
            return self.send_json(200, cr) if cr else self.outcome(404, 'ClaimResponse not found.', 'not-found')
        return self.outcome(404, f'Unknown route {rest}.', 'not-found')

    def do_GET(self):
        self.handle_any('GET')

    def do_POST(self):
        self.handle_any('POST')

    def do_PUT(self):
        self.handle_any('PUT')

    def do_DELETE(self):
        self.handle_any('DELETE')


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--host', default='0.0.0.0')
    ap.add_argument('--port', type=int, default=8089)
    ap.add_argument('--public-base', default=os.environ.get('PAS_PUBLIC_BASE_URL'),
                    help='absolute root used in notifications (default http://host.docker.internal:<port>)')
    ap.add_argument('--rules', default=str(REPO / 'spec' / 'adjudication-rules.json'))
    args = ap.parse_args()
    public_base = (args.public_base or f'http://host.docker.internal:{args.port}') + os.environ.get('PAS_PREFIX', '')
    rules = json.loads(Path(args.rules).read_text())
    Handler.state = State(public_base, rules,
                          float(os.environ.get('PAS_PENDED_AUTO_FINALIZE_SECONDS', '10')),
                          float(os.environ.get('PAS_NOTIFICATION_RETRY_SECONDS', '3')),
                          float(os.environ.get('PAS_NOTIFICATION_MAX_SECONDS', '280')))
    Handler.prefix = os.environ.get('PAS_PREFIX', '')
    Handler.tokens = [t for t in (os.environ.get('PAS_AUTH_TOKENS') or '').split(',') if t]
    Handler.enable_admin = os.environ.get('PAS_ADMIN_ENABLED') == 'true'
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    log(f'reference PAS server on {args.host}:{args.port}, public base {public_base}, auth {"on" if Handler.tokens else "off"}, admin routes {"ENABLED (test-only)" if Handler.enable_admin else "disabled"}')
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == '__main__':
    main()

#!/usr/bin/env python3
"""Offline selftest for run_suite.py: no Docker, no real Inferno.

Starts stubs/fake_inferno.py and stubs/stub_pas_server.py on free localhost ports and checks:
  1. positive: run_suite.py --suite both exits 0, both Subscription waits are resolved without a human
     (handshake confirmation and admin finalize trigger), reports and a LATEST file are written;
  2. negative: with --no-trigger and auto-finalize disabled, the pended wait is not resolved and the
     harness exits non-zero.
Reports go to a temporary directory so harness/reports and LATEST.md stay reserved for real runs.
"""

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
HARNESS = HERE.parent


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_http(url, seconds=15):
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as resp:
                if resp.status == 200:
                    return
        except OSError:
            time.sleep(0.2)
    raise RuntimeError(f"{url} did not come up")


def harness(args, env=None):
    cmd = [sys.executable, str(HARNESS / "run_suite.py")] + args
    print("$ " + " ".join(cmd), flush=True)
    return subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=600)


def main():
    if not (HERE / "fixture_inputs" / "v221" / "inputs.json").exists():
        subprocess.run([sys.executable, str(HERE / "make_fixtures.py")], check=True)
    inferno_port, server_port = free_port(), free_port()
    env = dict(os.environ, PAS_ADMIN_ENABLED="true", PAS_PENDED_AUTO_FINALIZE_SECONDS="0", PAS_NOTIFY_RETRY_SECONDS="1",
               PAS_NOTIFY_MAX_SECONDS="120")
    tmp = Path(tempfile.mkdtemp(prefix="pas-harness-selftest-"))
    procs = [
        subprocess.Popen([sys.executable, str(HERE / "fake_inferno.py"), "--port", str(inferno_port)],
                         stdout=open(tmp / "fake_inferno.log", "w"), stderr=subprocess.STDOUT),
        subprocess.Popen([sys.executable, str(HERE / "stub_pas_server.py"), "--port", str(server_port)],
                         env=env, stdout=open(tmp / "stub_pas.log", "w"), stderr=subprocess.STDOUT),
    ]
    failures = []
    try:
        wait_http(f"http://127.0.0.1:{inferno_port}/api/test_suites")
        wait_http(f"http://127.0.0.1:{server_port}/health")
        common = ["--inferno", f"http://127.0.0.1:{inferno_port}",
                  "--fhir-base", f"http://127.0.0.1:{server_port}/{{suite}}/fhir",
                  "--control-url", f"http://127.0.0.1:{server_port}",
                  "--inputs", str(HERE / "fixture_inputs" / "{suite}" / "inputs.json"),
                  "--poll", "0.5", "--handshake-grace", "2"]

        pos = harness(common + ["--suite", "both", "--label", "selftest",
                                "--reports-dir", str(tmp / "reports"), "--latest", str(tmp / "LATEST.md")])
        print(pos.stdout[-4000:], pos.stderr[-2000:])
        if pos.returncode != 0:
            failures.append(f"positive run exit {pos.returncode}")
        for needle in ("confirming", "admin/pended/finalize -> HTTP 200"):
            if pos.stdout.count(needle) < 2:
                failures.append(f"expected '{needle}' for both suites")
        summaries = sorted((tmp / "reports").glob("*_selftest_*/summary.md"))
        if len(summaries) != 2:
            failures.append(f"expected 2 summaries, found {len(summaries)}")
        for summary in summaries:
            text = summary.read_text()
            if "Verdict: **PASS**" not in text:
                failures.append(f"{summary} not PASS")
            results = json.loads((summary.parent / "results.json").read_text())
            if not results or any(r.get("result") not in ("pass",) for r in results):
                failures.append(f"{summary.parent.name}: non-pass results present")
        if not (tmp / "LATEST.md").exists() or "PASS" not in (tmp / "LATEST.md").read_text():
            failures.append("LATEST.md missing or not PASS")

        neg = harness(common + ["--suite", "v221", "--label", "selftest-negative", "--groups", "1,2",
                                "--no-trigger", "--notification-timeout", "5",
                                "--reports-dir", str(tmp / "reports"), "--latest", str(tmp / "LATEST_negative.md")])
        print(neg.stdout[-2500:], neg.stderr[-1000:])
        if neg.returncode != 1:
            failures.append(f"negative run should exit 1, got {neg.returncode}")
        if "Manual step" not in neg.stdout:
            failures.append("negative run did not print the manual step")

        dry = harness(common + ["--suite", "v201", "--dry-run"])
        if dry.returncode != 0 or "pended_pa_inquire_request_payload" not in dry.stdout:
            failures.append("dry run did not list v201 inputs")
    finally:
        for proc in procs:
            proc.terminate()
        for proc in procs:
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
    print(f"selftest artifacts: {tmp}")
    if failures:
        print("SELFTEST FAIL:\n- " + "\n- ".join(failures))
        return 1
    print("SELFTEST PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())

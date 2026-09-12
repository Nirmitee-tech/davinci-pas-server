# PAS harness

Headless driver for the Inferno Da Vinci PAS Server suites (v2.2.1 and v2.0.1). Python 3 standard library.

## Local run

Prerequisites: the PAS Inferno kit up on http://localhost:8280 (compose project `inferno-pas`), the PAS server running (Go 8085 or .NET 8086), testdata in `testdata/{v221,v201}/inputs.json`.

```sh
python3 harness/run_suite.py --suite both --label go-local
python3 harness/run_suite.py --suite both --label dotnet-local \
  --fhir-base "http://host.docker.internal:8086/{suite}/fhir" --control-url http://localhost:8086
```

Useful options: `--groups 1,2` (short ids or title words), `--dry-run` (print resolved inputs),
`--bearer-token` (sent as OAuth Credentials), `--no-trigger` (rely on server auto-finalize),
`--allow-skips`, `--notification-timeout 150`.

Start the server under test with `PAS_ADMIN_ENABLED=true` for the run only: the `/admin/*` control routes are
test-only and disabled (404) by default (spec/SPEC.md section 11). Without the flag the run still works but waits
`--handshake-grace` seconds at the handshake and relies on the server's auto finalize at the pended wait.

Waits are handled automatically:
- Subscription Setup: after the server's handshake reaches Inferno, the harness opens the confirmation URL.
- Pended workflow: the harness calls `POST {control-url}/admin/pended/finalize`; the server's event
  notification resumes Inferno. If nothing arrives, the harness prints the manual step and exits 1.

Outputs: `harness/reports/<UTC>_<label>_<suite>/{session.json,results.json,summary.md}` and
`harness/LATEST.md`. Exit 0 only when every selected test passed (skips count as failures unless
`--allow-skips`), 1 on failures, 2 on setup errors.

Reference server (live baseline for the Go and .NET servers):

```sh
PAS_ADMIN_ENABLED=true python3 harness/stubs/reference_server.py --port 8089
python3 harness/run_suite.py --suite both --label reference --fhir-base "http://host.docker.internal:8089/{suite}/fhir" --control-url http://localhost:8089
```

## Offline selftest

```sh
python3 harness/stubs/selftest.py
```

Starts `stubs/fake_inferno.py` (a small imitation of the Inferno API and notification routes) and
`stubs/stub_pas_server.py` (a tiny PAS server, not the contract) on free localhost ports and checks the
positive flow for both suites, the negative flow (unresolved pended wait exits 1) and `--dry-run`.
`stubs/make_fixtures.py` regenerates the synthetic fixture inputs.

Public run on inferno.healthit.gov: see `tls_public_run.md`.

# Inferno result summaries

Markdown summaries of the Inferno Da Vinci PAS Server test suite runs behind the results table in the
[root README](../README.md). They are copied unchanged from `harness/reports/<UTC>_<label>_<suite>/summary.md`,
renamed after their report folder, except that tunnel hostnames are replaced with `<ngrok-host>`. The JSON session
and results files the harness also writes are not published here.

## How they were produced

- Test kit: [Inferno Da Vinci PAS Test Kit](https://github.com/inferno-framework/davinci-pas-test-kit) v0.15.2
  (commit `49ffd92`, inferno_core 1.4.3, subscriptions_test_kit 0.12.2), suites `davinci_pas_server_suite_v221` (PAS
  2.2.1) and `davinci_pas_server_suite_v201` (PAS STU 2.0.1).
- Driver: `harness/run_suite.py`, which creates an Inferno session, fills the inputs from `testdata/v221` or
  `testdata/v201`, runs every top-level group in order, confirms the Subscription Setup wait once the server's
  handshake was delivered, triggers the pended decision through `POST /admin/pended/finalize` so the server's event
  notification resumes the pended wait, polls each run to completion and writes the reports.
- Servers: started with `PAS_ADMIN_ENABLED=true` for the duration of the runs only, and with authentication off
  (`PAS_AUTH_TOKENS` empty), which the suites allow because they do not score OAuth.
- Known issues: `harness/known_kit_issues.json` lists no entries for this kit version. A run passes only with zero
  failures, errors and skips.
- Behavior contract: `spec/SPEC.md` and the rules in `spec/adjudication-rules.json`.
- Date: 12 September 2026.

### Local runs

The kit ran in Docker with the compose override from "Running the Inferno suites yourself" in the root README, which
publishes Inferno on `http://localhost:8280`. The servers ran natively on the same host: Go on port 8085 and .NET on
port 8086. Inferno reached them at `http://host.docker.internal:8085` and `:8086`.

```sh
python3 harness/run_suite.py --suite both --label go-local
python3 harness/run_suite.py --suite both --label dotnet-local \
  --fhir-base "http://host.docker.internal:8086/{suite}/fhir" --control-url http://localhost:8086
```

### Public runs

The server ran behind an ngrok HTTPS tunnel, started with `PAS_PUBLIC_BASE_URL=https://<ngrok-host>`, and the suites
ran on the public instance at https://inferno.healthit.gov against `https://<ngrok-host>/v221/fhir` and `/v201/fhir`.
The harness called the admin routes on localhost; through the tunnel they answer 403. The PAS server suites contain
no TLS test, so the public runs cover the same tests as the local runs. See `harness/tls_public_run.md` for the
procedure.

## Files

| File | Implementation | Suite | Run | Pass | Fail | Skip |
|---|---|---|---|---|---|---|
| [20260912T082955Z_go-local_v221.md](20260912T082955Z_go-local_v221.md) | Go | v2.2.1 | local | 84 | 0 | 0 |
| [20260912T083049Z_dotnet-local_v221.md](20260912T083049Z_dotnet-local_v221.md) | .NET | v2.2.1 | local | 84 | 0 | 0 |
| [20260912T082955Z_go-local_v201.md](20260912T082955Z_go-local_v201.md) | Go | v2.0.1 | local | 68 | 0 | 0 |
| [20260912T083049Z_dotnet-local_v201.md](20260912T083049Z_dotnet-local_v201.md) | .NET | v2.0.1 | local | 68 | 0 | 0 |
| [20260912T083140Z_go-public_v221.md](20260912T083140Z_go-public_v221.md) | Go | v2.2.1 | public, HTTPS | 84 | 0 | 0 |
| [20260912T083140Z_go-public_v201.md](20260912T083140Z_go-public_v201.md) | Go | v2.0.1 | public, HTTPS | 68 | 0 | 0 |
| [20260912T083524Z_dotnet-public_v221.md](20260912T083524Z_dotnet-public_v221.md) | .NET | v2.2.1 | public, HTTPS | 84 | 0 | 0 |
| [20260912T083524Z_dotnet-public_v201.md](20260912T083524Z_dotnet-public_v201.md) | .NET | v2.0.1 | public, HTTPS | 68 | 0 | 0 |

Each summary lists the Inferno session, the FHIR base under test, the per-group totals and any test that did not
pass. The notification token, a random value generated for each session, is replaced with `<redacted>`.

## Group totals in a passing run

| Group | v2.2.1 tests | v2.0.1 tests |
|---|---|---|
| 1 Subscription Setup | 5 | 5 |
| 2 Demonstrate Workflow Support | 30 | 18 |
| 3 Demonstrate Element Support | 47 | 43 |
| 4 Demonstrate Error Handling | 2 | 2 |
| Total | 84 | 68 |

## Reproducing

Follow "Running the Inferno suites yourself" in the [root README](../README.md) or [CONTRIBUTING.md](../CONTRIBUTING.md).
Results depend on the kit version; newer kit releases may add, change or fix tests.

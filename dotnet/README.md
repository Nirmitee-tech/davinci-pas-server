# Nirmitee Da Vinci PAS Payer Server (.NET)

An open-source Da Vinci Prior Authorization Support (PAS) payer server on ASP.NET Core minimal APIs (.NET 10). It
serves PAS 2.2.1 and STU 2.0.1 side by side and follows the behavior contract in `../spec/SPEC.md`, so it behaves the
same as the Go server. Adjudication is rule based and deterministic; all codes, identifiers and test data are
synthetic.

## Architecture

CRD, DTR and PAS are separate servers that can later be mounted into one Burden Reduction host.

```
dotnet/
  Nirmitee.DaVinci.Pas.slnx
  src/Nirmitee.DaVinci.Common/       shared building blocks (copied from the DTR server, see below)
  src/Nirmitee.DaVinci.Pas/          the PAS module as a library
    PasModule.cs                     services.AddPas(configuration) and endpoints.MapPas(prefix)
    PasOptions.cs                    options plus the PAS_* environment variables
    Rules/                           adjudication-rules.json loader and rule matching (the file is embedded)
    Claims/                          PasEngine ($submit, $inquire, finalize, Subscriptions) and ResponseBuilder
    Storage/                         store abstractions with in-memory defaults (claims, subscriptions, outbox, delivery log)
    Notifications/                   background services: rest-hook delivery with retries, pended auto finalize
    Http/                            request pipeline (auth, parsing, OperationOutcome errors, audit), external base URL
  src/Nirmitee.DaVinci.Pas.Host/     thin host: Program.cs and appsettings.json
  tests/Nirmitee.DaVinci.Pas.Tests   xUnit unit and in-memory end-to-end tests
```

The library has no fixed ports or absolute URLs. Every route is relative to the prefix given to `MapPas`, and the
external FHIR base is computed per request. A combined host mounts it like this:

```csharp
builder.Services.AddPas(builder.Configuration);
app.MapPas("/pas");   // routes become /pas/v221/fhir/..., /pas/v201/fhir/..., /pas/health
```

State lives behind `IClaimStore`, `ISubscriptionStore`, `INotificationOutbox` and `INotificationLog`. `AddPas`
registers in-memory implementations with `TryAdd`, so a host can register durable ones first. Restarting the
in-memory server clears all state.

### Nirmitee.DaVinci.Common

`src/Nirmitee.DaVinci.Common` is a copy of `dotnet/src/Nirmitee.DaVinci.Common` from the DTR server
(github.com/Nirmitee-tech/davinci-dtr-server). Its public API is unchanged so the copies can become one shared
package later. The PAS copy only adds `InternalsVisibleTo` for `Nirmitee.DaVinci.Pas.Tests`. PAS uses the JSONL audit
log, the JSON helpers and the URL rewrite rules.

## Endpoints

For each FHIR base `{B}` = `{prefix}/v221/fhir` and `{prefix}/v201/fhir`:

| Method | Path | Behavior |
|---|---|---|
| GET | `{B}/metadata` | CapabilityStatement (Claim `$submit` and `$inquire`, ClaimResponse read, Subscription create, read, update, delete with the PAS topic); never requires a token |
| POST | `{B}/Claim/$submit` | 200 PAS Response Bundle: ClaimResponse first, referenced request resources echoed verbatim, CommunicationRequest and Task when documentation is needed |
| POST | `{B}/Claim/$inquire` | 200 Inquiry Response Bundle built from the stored submission (or the rules when unknown); wrapped in Parameters `return` on `/v221`, bare Bundle on `/v201` |
| POST | `{B}/Subscription` | 201 with `status` `requested` and `Location`; a handshake follows about one second later |
| GET, PUT, DELETE | `{B}/Subscription/{id}` | read, replace (200), delete (204) |
| GET | `{B}/Subscription/{id}/$status` | searchset Bundle with the SubscriptionStatus (`query-status`) |
| GET | `{B}/ClaimResponse/{id}` | the latest ClaimResponse with that id (used after id-only notifications) |
| GET | `{prefix}/health` | status, rules version and source |

Errors are OperationOutcomes with `application/fhir+json`: 400 for Bundles that cannot be processed (not JSON, not a
collection Bundle, no entries, no Claim, unresolvable patient, insurer or provider, no items on `$submit`), 401
`security` with `WWW-Authenticate: Bearer`, 404 unknown routes and ids, 405 unsupported methods, 422 invalid
Subscriptions and 500 unexpected failures. Business problems inside a valid request are reported in
`ClaimResponse.error` with `outcome` `partial`.

### Adjudication

`../spec/adjudication-rules.json` is embedded in the library and evaluated per `Claim.item`, first match wins:
cancelled items and cancel requests (`C`), `A9270` denied (`A3`), `A9999` denied with `ClaimResponse.error`, `E0470`
pended (`A4`) then approved, `E1390` pended with CommunicationRequest and Task, `E0601` modified with `addItem` (`A6`),
anything else approved (`A1`). A later `$submit` with the same `Claim.identifier` replaces the stored request (claim
updates), and every submitted item sequence is returned. `$inquire` maps authorization numbers listed in
`replacedAuthorizations` to their replacement.

### Subscriptions and notifications

Subscriptions must use the PAS topic, a `rest-hook` channel with an absolute http(s) endpoint and `full-resource`
or `id-only` payload content. The handshake and event notifications are POSTed with `Content-Type` from
`channel.payload` and every `channel.header`. Delivery retries every `PAS_NOTIFICATION_RETRY_SECONDS` while the
answer is not 2xx, gives up after `PAS_NOTIFICATION_MAX_SECONDS` and never resends after a 2xx; no heartbeats are
sent. A delivered handshake makes the Subscription `active`. When a pended item is finalized (after
`PAS_PENDED_AUTO_FINALIZE_SECONDS`), each active Subscription on the same FHIR base whose `org-identifier` filter
matches the requesting organization gets one event notification: the updated response Bundle for `full-resource`,
the ClaimResponse URL for `id-only`.

## Configuration

Environment variables (same names as the Go server) override the `Pas` section of `appsettings.json`:

| Variable | Default | Meaning |
|---|---|---|
| `PAS_PORT` | `8086` | listen port on all interfaces, ignored when `--urls` or `ASPNETCORE_URLS` is set |
| `PAS_PREFIX` | empty | route prefix |
| `PAS_PUBLIC_BASE_URL` | unset | fixed external origin used for FHIR bases and notification references, for example `https://x.ngrok-free.app` |
| `PAS_RULES_FILE` | embedded copy of `spec/adjudication-rules.json` | rules file; relative paths are searched upward from the working directory |
| `PAS_AUTH_TOKENS` | empty | accepted bearer tokens, comma separated; empty disables authentication |
| `PAS_PENDED_AUTO_FINALIZE_SECONDS` | `10` | delay before pended items are finalized; `0` disables the timer |
| `PAS_NOTIFICATION_RETRY_SECONDS` | `3` | retry interval for handshake and event notifications |
| `PAS_NOTIFICATION_MAX_SECONDS` | `280` | stop retrying a notification after this long |
| `PAS_ADMIN_ENABLED` | unset (disabled) | test-only control routes, enabled only by the exact value `true` (see below) |
| `PAS_DATA_DIR` | `./data` | the audit log is written to `{PAS_DATA_DIR}/pas-audit/` |

Settings only available in `appsettings.json` or as `Pas__*` variables: `HandshakeDelaySeconds` (1),
`NotificationTimeoutSeconds` (15), `AuditEnabled` (true) and `NotificationUrlRewrites`, which rewrites Subscription
endpoint prefixes before delivery. That helps when the server runs in a container and the subscriber sent a
`localhost` endpoint: `Pas__NotificationUrlRewrites__0__From=http://localhost:8280`,
`Pas__NotificationUrlRewrites__0__To=http://host.docker.internal:8280`.

Without `PAS_PUBLIC_BASE_URL` the FHIR base is `{scheme}://{host}{prefix}/{version}/fhir`, where scheme and host come
from the first `X-Forwarded-Proto` and `X-Forwarded-Host` values or the connection and `Host` header.

### Security notes

- There are no default credentials. Authentication is off until `PAS_AUTH_TOKENS` is set; then every FHIR route
  except `GET metadata` requires one of the listed bearer tokens. Choose long random values and use TLS in front of
  the server.
- **Control routes are for test automation only.** `POST {prefix}/admin/pended/finalize` (decide pended claims now),
  `POST {prefix}/admin/reset` (clear all state) and `GET {prefix}/admin/notifications` (delivery log, including the
  subscriber's notification token) exist so an Inferno run can proceed without a human. They answer 404 unless the
  server starts with `PAS_ADMIN_ENABLED=true`, and even then refuse requests carrying `X-Forwarded-For` or `Forwarded`
  headers (403). Enable the flag only for the duration of a test run on a host that is not reachable from the
  internet, and restart without it afterwards. The Docker image never sets it.

## Run

```bash
export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
cd dotnet
dotnet run --project src/Nirmitee.DaVinci.Pas.Host
curl http://localhost:8086/v221/fhir/metadata
```

## Test

```bash
cd dotnet
dotnet test
```

The tests replay the synthetic testdata of both suites in Inferno order against the in-memory host and require every
response and notification (CapabilityStatement, Subscription create, handshake, approval, denial and pended
responses, event notification, v2.0.1 pended inquiry, claim updates, must support submits and inquiries, error
responses) to equal the golden examples in `../spec/examples` with the same keys in the same order, after ids, dates
and instants are masked. They also check the expected decisions, response Bundle reference closure and echoed
resources, claim update item sequences, replaced authorization numbers and the notification rules, plus request
validation, 404 and 405 handling, the Subscription lifecycle, bearer authentication, the control route flag and
proxy refusal, reset, audit redaction, forwarded headers, a public base URL, a rules file from disk, mounting under a
prefix, notification retries until 2xx without resends, giving up after the retry window, auto finalize and
Subscription filters.

## Test against Inferno

With the local Inferno PAS test kit on http://localhost:8280, start the server for the run with the control routes
enabled, then from the repository root:

```bash
PAS_ADMIN_ENABLED=true dotnet run --project dotnet/src/Nirmitee.DaVinci.Pas.Host
python3 harness/run_suite.py --suite both --label dotnet-local \
  --fhir-base "http://host.docker.internal:8086/{suite}/fhir" --control-url http://localhost:8086
```

Reports land in `harness/reports/<time>_<label>_<suite>/summary.md` and `harness/LATEST.md`. The PAS server suites
contain no TLS test. To repeat the run on the public Inferno instance, expose the server through an https tunnel
(`harness/tls_public_run.md`) and start it with `PAS_PUBLIC_BASE_URL=https://<subdomain>.ngrok-free.app`.

## Docker

```bash
# from the repository root (the build embeds spec/adjudication-rules.json)
docker build -f dotnet/Dockerfile -t nirmitee-pas-dotnet .
docker run --rm -p 8086:8086 nirmitee-pas-dotnet
```

The image is multi-stage, runs as the non-root `app` user and writes the audit log to `/data/pas-audit`.

## Audit log

Every request appends one line to `{PAS_DATA_DIR}/pas-audit/{yyyy-MM-dd}.jsonl` with the timestamp, version,
operation, method, path, status, duration, remote address, a detail note, the request body and the response body.
Every notification attempt adds a line with the endpoint, status, attempt number and outcome. `Authorization`
headers are never written, and bearer tokens inside bodies (for example Subscription `channel.header`) are replaced
by `[redacted]`.

## Known spec questions

See `SPEC-ISSUES.md`.

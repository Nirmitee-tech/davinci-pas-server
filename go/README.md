# Da Vinci PAS Payer Server (Go)

An open-source Da Vinci Prior Authorization Support (PAS) payer server written in Go with the standard library plus
`gopkg.in/yaml.v3` (used only by the shared configuration loader). It serves PAS 2.2.1 and PAS STU 2.0.1 FHIR bases
with `Claim/$submit`, `Claim/$inquire`, rest-hook Subscriptions (handshake and event notifications), Claim updates and
`ClaimResponse` read. Decisions come from a deterministic rules file. It is built to pass the Inferno Da Vinci PAS
Server test suites (`davinci_pas_server_suite_v221` and `davinci_pas_server_suite_v201`). The behavior contract
shared with the .NET implementation is `../spec/SPEC.md`; the rules are `../spec/adjudication-rules.json`. All members,
codes and identifiers are synthetic.

## Endpoints

Each version has a FHIR base: `{prefix}/v221/fhir` (PAS 2.2.1) and `{prefix}/v201/fhir` (PAS 2.0.1).

| Method and path under a FHIR base | Auth | Purpose |
|---|---|---|
| `GET /metadata` | none | CapabilityStatement (declares Subscription create, read, update, delete) |
| `POST /Claim/$submit` | Bearer when enabled | PAS Request Bundle in, PAS Response Bundle out (200) |
| `POST /Claim/$inquire` | Bearer when enabled | Inquiry Bundle in; 2.2.1 answers `Parameters` with a `return` Bundle, 2.0.1 answers the Bundle |
| `POST /Subscription` | Bearer when enabled | rest-hook Subscription on the PAS topic; 201 with `status` `requested`, then a handshake |
| `GET`, `PUT`, `DELETE /Subscription/{id}` | Bearer when enabled | read, replace (same validation), delete |
| `GET /Subscription/{id}/$status` | Bearer when enabled | SubscriptionStatus Parameters (`query-status`) |
| `GET /ClaimResponse/{id}` | Bearer when enabled | stored ClaimResponse (focus of 2.0.1 `id-only` notifications) |
| `GET {prefix}/health` | none | liveness |

FHIR responses are `application/fhir+json`. Errors are OperationOutcomes: 400 for a body that cannot be processed
(not a Bundle, wrong `Bundle.type`, no entries, first entry not a Claim, unresolvable patient, insurer or provider,
no items on `$submit`), 401 with `WWW-Authenticate: Bearer` for a missing or wrong token, 404 for unknown routes and
ids, 405 for a wrong method, 422 for an invalid Subscription and 500 for unexpected failures. Business problems inside
a valid Bundle are reported in `ClaimResponse.error` with `outcome` `partial`.

### Test-only control routes

`POST {prefix}/admin/pended/finalize`, `POST {prefix}/admin/reset` and `GET {prefix}/admin/notifications` exist so an
automated Inferno run can finish without a human: they decide pended claims on demand and expose the notification
delivery log, including the bearer token sent to the notification endpoint. They are **disabled by default** and
answer 404 like any unknown route. They are enabled only when `PAS_ADMIN_ENABLED` is exactly `true`, and even then they
refuse requests that carry `X-Forwarded-For` or `Forwarded` (403), so they are not reachable through a tunnel or reverse
proxy. Never enable them on a deployment.

## How it works

- **Adjudication** (`pas/engine.go`, `pas/rules.go`). Each `Claim.item` is matched against the rules in order: item
  cancelled, request cancelled (certificationType 3), denied, denied with `ClaimResponse.error`, pended and later
  approved, pended with a CommunicationRequest and Task, modified with an `addItem`, otherwise approved. The claim
  level decision (cancelled, pended, denied, partial, modified or approved) is derived from the items. Item review
  actions use X12 306 codes (A1, A3, A4, A6, C) in `extension-reviewAction` on `ClaimResponse.item.adjudication`.
- **Response Bundles.** The ClaimResponse is the first entry; every resource it references (except
  `ClaimResponse.request`) and every resource those reference appears exactly once, copied verbatim from the request
  with its fullUrl, id, identifier and key order unchanged. The submitted Claim is never echoed. Generated
  CommunicationRequest and Task resources use `urn:uuid` fullUrls.
- **State** (`pas/store.go`). Claims are stored by `Claim.identifier[0]` system and value per version. A later submit
  with the same identifier replaces the record (Claim updates) and is adjudicated again, returning exactly the
  submitted item sequences. `$inquire` answers from the stored record and adjudicates unknown items statelessly;
  replaced authorization numbers from the rules are returned on inquiry. `Store` is an interface with an in-memory
  default (`NewMemoryStore`), so a database-backed store can be plugged in through `Config.Store`.
- **Subscriptions and notifications** (`pas/subscriptions.go`, `pas/notifier.go`). A created Subscription is stored with
  `status` `requested`; about one second later the handshake is POSTed to `channel.endpoint` with every
  `channel.header` as an HTTP header and `Content-Type` from `channel.payload`. After a 2xx answer the Subscription
  becomes `active`. When a pended item is finalized (automatically after `PAS_PENDED_AUTO_FINALIZE_SECONDS`, or through
  the control route) the stored response is rebuilt and one event notification is queued per matching Subscription of
  that version (`org-identifier` filter against the requesting organization). 2.2.1 sends `full-resource` notifications
  carrying the updated response Bundle; 2.0.1 sends `id-only` notifications whose focus is the `ClaimResponse` URL.
  Delivery retries every `PAS_NOTIFICATION_RETRY_SECONDS` while the answer is not 2xx, stops after the first 2xx and
  gives up after `PAS_NOTIFICATION_MAX_SECONDS`. No heartbeats are sent.
- **Audit** (`internal/shared/audit`). Every request and response, and every notification attempt, is written as one
  JSON line to `{PAS_DATA_DIR}/audit/YYYY-MM-DD.jsonl`. Bearer token values are redacted, including those inside
  `Subscription.channel.header`.

## Architecture

```
cmd/pas-server/            thin host: environment config, JSON logger, audit file, http.Server, graceful shutdown
pas/                       public package: Config, LoadConfig, NewHandler(cfg) (http.Handler, error), Store
  handler.go               routing relative to the prefix, external base URL, bearer checks, control routes, audit records
  engine.go                validation, adjudication, response Bundle construction, inquiry, finalize
  subscriptions.go         Subscription validation, handshake and event notification Bundles, filters
  notifier.go              background rest-hook delivery with retries and the delivery log
  store.go                 Store interface and the in-memory implementation
  rules.go                 rules file model; adjudication-rules.json is a bundled copy of the spec file
internal/shared/           shared with the CRD and DTR servers
  auth/  fhir/  audit/  config/  logging/
```

`pas.NewHandler` keeps no global state, so a combined burden reduction host can mount the CRD, DTR and PAS handlers in
one process, each with its own `Prefix`. The returned handler also implements `io.Closer`; call `Close` on shutdown to
stop auto finalize timers and pending deliveries.

`internal/shared` is a copy of `go/internal/shared` from the DTR server
(github.com/Nirmitee-tech/davinci-dtr-server) with the same package API. PAS uses `fhir` (key order preserving JSON),
`audit`, `config`, `logging` and `auth.EqualConstantTime`. Keep the copies in sync when either side changes.

## Configuration

Settings come from defaults, then an optional YAML or JSON file (`PAS_CONFIG_FILE`), then environment variables.

| Variable | Default | Meaning |
|---|---|---|
| `PAS_PORT` | `8085` | listen port on all interfaces; `PAS_LISTEN` or the `-listen` flag set a full address |
| `PAS_PREFIX` | empty | route prefix, e.g. `/pas` |
| `PAS_PUBLIC_BASE_URL` | empty | external origin used in resource URLs and notifications, e.g. a public tunnel URL; otherwise derived from the request |
| `PAS_RULES_FILE` | `spec/adjudication-rules.json`, then `../spec/adjudication-rules.json`, then the bundled copy | adjudication rules |
| `PAS_AUTH_TOKENS` | empty | comma separated accepted bearer tokens; empty disables authentication |
| `PAS_PENDED_AUTO_FINALIZE_SECONDS` | `10` | delay before auto-finalizable pended items are decided; `0` disables |
| `PAS_NOTIFICATION_RETRY_SECONDS` | `3` | retry interval for handshake and event notifications |
| `PAS_NOTIFICATION_MAX_SECONDS` | `280` | stop retrying a notification after this long |
| `PAS_ADMIN_ENABLED` | unset | test-only control routes; enabled only by the exact value `true` |
| `PAS_DATA_DIR` | `data` | audit log directory root |
| `PAS_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `PAS_URL_REWRITES` | empty | `from|to` pairs applied to notification endpoints, e.g. when the server runs in a container |

There are no default credentials. With `PAS_AUTH_TOKENS` empty the FHIR routes are open, which suits a local
conformance run; the server logs a warning at startup. Set tokens (or place the server behind an authorization gateway)
for anything reachable by others. `GET /metadata` never requires a token.

## Run

```sh
cd go
go run ./cmd/pas-server
# FHIR bases: http://localhost:8085/v221/fhir and http://localhost:8085/v201/fhir
```

Docker (distroless, non-root, rules bundled in the binary):

```sh
docker build -t nirmitee/davinci-pas-server-go go/
docker run --rm -p 8085:8085 nirmitee/davinci-pas-server-go
```

## Tests and conformance

```sh
cd go
go test ./...
```

The tests replay `../testdata/<version>` in Inferno suite order against the handler with a local notification
endpoint and check decisions, response Bundle structure, echoed resources, notification contents and headers, retry
behavior, Claim updates, inquiry, error handling, authentication, control route protection and audit redaction.

Inferno conformance, local Inferno with the PAS test kit 0.15.2, harness `harness/run_suite.py` (report summaries in
`../docs/results/`):

| Suite | Result |
|---|---|
| `davinci_pas_server_suite_v221` | 84 pass, 0 fail, 0 skip |
| `davinci_pas_server_suite_v201` | 68 pass, 0 fail, 0 skip |

To reproduce, start the server with `PAS_ADMIN_ENABLED=true` for the duration of the run only, then:

```sh
python3 harness/run_suite.py --suite both --label go-local
```

## License

Apache-2.0.

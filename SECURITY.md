# Security Policy

## Reporting a vulnerability

Please report security issues privately through GitHub's **Report a vulnerability** button on this repository's
Security tab (private vulnerability reporting), or by email to **hello@nirmitee.io** with the subject
"Security: davinci-pas-server". Do not open a public GitHub issue, pull request or discussion for a suspected
vulnerability.

Include as much of the following as you can:

- the affected implementation (`go/`, `dotnet/` or both) and the commit or release
- a description of the issue and its impact
- steps to reproduce, using synthetic data only
- any suggested fix

We aim to acknowledge a report within five business days and to keep you informed while we investigate. Once a
fix is available we will publish an advisory and credit the reporter unless you ask us not to.

## No protected health information

Never include real patient data, member identifiers, access tokens, notification tokens, private keys or production
URLs in an issue, pull request, log excerpt or security report. Use the synthetic data in `testdata/` and
`spec/examples/` to reproduce problems. If you have accidentally posted sensitive data, report it privately the same
way so it can be removed.

## Supported versions

The project has not published a tagged release yet.

| Version | Supported |
|---|---|
| `main` branch | yes |
| latest tagged release (once releases exist) | yes |
| older releases | no |

## Test-only admin routes

`GET {prefix}/admin/notifications`, `POST {prefix}/admin/pended/finalize` and `POST {prefix}/admin/reset` exist only
so an automated Inferno run can finish without a human. When enabled they expose the bearer tokens sent to
notification endpoints, let any caller decide pended prior authorization requests and clear all state. They are
guarded as follows (`spec/SPEC.md` section 11):

- disabled by default: every `/admin/*` path answers 404 like an unknown route
- enabled only when `PAS_ADMIN_ENABLED` is exactly `true`; neither Docker image sets it
- refused with 403, even when enabled, for requests that carry `X-Forwarded-For` or `Forwarded`, so they cannot be
  reached through a tunnel or reverse proxy that adds those headers

**Never enable them on a production server or on any server reachable by others.** Set the flag only for the duration
of a conformance run and restart without it afterwards. The proxy check depends on the proxy adding forwarding
headers; a proxy or port forward that does not add them gives no such protection. A way to reach the admin routes
while they are disabled, or through a proxy while enabled, is a vulnerability; please report it.

## No default credentials

The servers ship with no default bearer tokens, passwords or keys. Authentication is off until `PAS_AUTH_TOKENS` is
set, which suits a local conformance run only; the Go server logs a warning at startup in that case. Contributions
must not add credentials that are accepted by default, sample tokens that are enabled without configuration, or admin
routes that are on by default.

## Deployment notes

This server is a reference implementation of the Da Vinci PAS payer side. Before using it with real data:

- run it behind TLS 1.2 or higher (the server itself listens on plain HTTP), and set `PAS_PUBLIC_BASE_URL` to the
  public origin so the URLs inside notifications use it
- turn on authentication: set `PAS_AUTH_TOKENS` to long random values, or place the server behind an authorization
  gateway; static bearer tokens are not a substitute for the SMART Backend Services authorization your clients may
  expect
- leave `PAS_ADMIN_ENABLED` unset
- restrict outbound network access: the server POSTs handshakes and notifications, with the headers the Subscription
  names, to whatever rest-hook endpoint an authorized client registers
- replace `spec/adjudication-rules.json` with your own decision logic; the bundled rules are synthetic test rules, not
  coverage policy
- note that claims, Subscriptions and queued notifications are held in memory, so they do not survive a restart and
  are not shared between instances; plug in a durable store through the storage interfaces
- protect the audit log directory, because audit lines contain request and response bodies (bearer tokens are
  redacted, clinical content is not)

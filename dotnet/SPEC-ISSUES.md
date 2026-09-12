# SPEC issues found while building the .NET PAS server

Contract: `../spec/SPEC.md` VERSION 1.2.0. The .NET server reproduces `spec/reference/pas_reference.py` for every
payload the Inferno suites inspect; the unit tests compare each response and notification with `spec/examples`
(same keys in the same order after masking ids and dates). The items below are places where the prose, the reference
model or the suites leave behavior open. None of them affects an Inferno test.

## 1. Section 7.1: shape of `GET Subscription/{id}/$status`

SPEC 7.1 says the route "returns the SubscriptionStatus Parameters (7.2 shape, type `query-status`)", which can be
read as a bare Parameters resource or as the history Bundle of 7.2. The Subscriptions R5 Backport IG defines
`$status` on R4 as returning a Bundle of SubscriptionStatus resources, so the .NET server answers with a `searchset`
Bundle holding one Parameters entry (`type` `query-status`, `search.mode` `match`). The route is not exercised by the
suites or by the reference model. Suggested fix: state the exact shape in 7.1.

## 2. Section 7.3 and the reference model: which Subscriptions receive an event notification

SPEC 7.3 queues "one event notification per active Subscription whose filter matches (`org-identifier=<v>` matches
any `identifier.value` of the requestor Organization)". `pas_reference.py` `finalize_pended` notifies every stored
Subscription and increments `events-since-subscription-start` for all of them, whatever their status or filter. The
.NET server follows the SPEC text: only `active` Subscriptions (handshake answered 2xx) on the same FHIR base version
whose `org-identifier` filters match (a `system|value` form is also accepted). With the single Subscription of an
Inferno session both give the same notification. Suggested fix: align the reference model or note the difference.

## 3. Section 7.1: Subscription details the SPEC leaves open

- Endpoint: the reference model only checks that `channel.endpoint` is present. The .NET server also answers 422
  when it is not an absolute `http` or `https` URL, so it never posts to other schemes. Inferno always sends an
  absolute URL.
- `PUT Subscription/{id}`: a body `id` different from the URL id is a 400 (`invalid`); a changed `channel.endpoint`
  sets `status` back to `requested` and sends a new handshake; otherwise the status is kept.
- `DELETE Subscription/{id}` answers 204 and drops queued notifications for it.

## 4. Section 4.4: Claims without `identifier` or `type`

`pas_reference.py` copies `Claim.identifier` and `Claim.type` into the ClaimResponse even when they are absent, which
serializes as JSON `null`. The .NET server omits the elements instead, since `null` is not valid FHIR JSON. All
testdata Claims carry both elements, so the examples are unaffected.

## 5. Section 11: admin flag name changed during the build (resolved in VERSION 1.2.0)

VERSION 1.1.0 described `PAS_ENABLE_ADMIN` with default `true`. VERSION 1.2.0 replaced it with `PAS_ADMIN_ENABLED`,
disabled unless the value is exactly `true`, with every `/admin/*` path answering 404 while disabled. The .NET server
implements 1.2.0 only; `PAS_ENABLE_ADMIN` is ignored.

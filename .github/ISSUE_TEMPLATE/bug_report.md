---
name: Bug report
about: Something in the Go or .NET server does not behave as spec/SPEC.md or the PAS IG describes
title: "[bug] "
labels: bug
assignees: ""
---

<!--
Do not include real patient data, member identifiers, access tokens, notification tokens or private keys.
Use the synthetic data in testdata/ and spec/examples/. Report security issues privately as described in SECURITY.md.
-->

## Implementation

- [ ] Go (`go/`)
- [ ] .NET (`dotnet/`)

Commit or release:

## What happened

## What you expected

Reference the section of `spec/SPEC.md`, the PAS IG page or the Inferno test id if relevant.

## Steps to reproduce

1.
2.
3.

Operation and FHIR base (for example `POST /v221/fhir/Claim/$submit`):

Request body (synthetic, from `testdata/` or `spec/examples/` if possible):

```json
```

Response, OperationOutcome or notification:

```json
```

## Inferno result (if applicable)

- Suite and version (v2.2.1 or v2.0.1):
- Test id and result message:

## Environment

- OS:
- Go or .NET SDK version:
- Running natively or in Docker:
- Relevant `PAS_*` settings (no secrets or tokens):

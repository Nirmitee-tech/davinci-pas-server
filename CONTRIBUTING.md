# Contributing

Thank you for helping improve the Nirmitee Da Vinci PAS Server. This repository holds two implementations of the
same behavior, one in Go (`go/`) and one in .NET (`dotnet/`). Both must stay in step.

By contributing you agree that your contributions are licensed under the Apache License, Version 2.0, as described
in section 5 of the [LICENSE](LICENSE). Please follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## The spec-first rule

`spec/SPEC.md` is the behavior contract both servers implement. Its shared inputs are the adjudication rules in
`spec/adjudication-rules.json`, the synthetic request Bundles in `testdata/` and the golden exchanges in
`spec/examples/`, which both test suites replay.

- Any change to externally visible behavior (routes, status codes, authorization, response Bundle contents,
  decisions, Subscriptions and notifications, admin routes, configuration, audit log format) starts with a change to
  `spec/SPEC.md`.
- Bump the `VERSION:` line at the top of `spec/SPEC.md` and add a changelog entry at the bottom.
- `spec/reference/pas_reference.py` is the executable reading of the contract. If you change it, the rules or the
  test data generator, run `python3 testdata/build_testdata.py` and then `python3 spec/reference/pas_reference.py` to
  regenerate `testdata/` and `spec/examples/`, and commit the results together. The reference model prints `PASS`
  when every offline check holds.
- `go/pas/adjudication-rules.json` is a bundled copy of `spec/adjudication-rules.json` and must stay identical. CI
  checks this.
- Implement the change in **both** `go/` and `dotnet/` in the same pull request, or open a tracking issue for the
  second implementation and link it.
- Where the spec and the Inferno Ruby tests disagree, the tests win. Record the finding in `dotnet/SPEC-ISSUES.md`
  (or a `go/SPEC-ISSUES.md`) and fix the spec.

Refactors, test additions and documentation fixes that do not change behavior do not need a spec change.

## Test-only admin routes

The `/admin/*` routes exist only for automated Inferno runs and are disabled unless `PAS_ADMIN_ENABLED=true`
(`spec/SPEC.md` section 11). Keep it that way: do not enable them by default in code, configuration files,
Dockerfiles, compose files or examples, and do not relax the 404-when-disabled or the 403-through-a-proxy rules.

## Known test kit issues

No Inferno PAS test kit defect currently blocks a conformant server, so `harness/known_kit_issues.json` is empty. Do
not work around a failing test by serving different content to test requests or by breaking a profile to satisfy a
test. If you find a kit defect, add an entry to `harness/known_kit_issues.json` with the evidence, record it in the
live evidence log in `spec/SPEC.md` section 17 and link the upstream issue. Remove the entry once the kit is fixed.

## Development setup

### Go

- Go 1.26 or newer (the exact version is in `go/go.mod`)

```sh
cd go
gofmt -l .
go vet ./...
go test ./...
go run ./cmd/pas-server        # listens on :8085
```

Format with `gofmt` before committing. CI fails on unformatted files.

### .NET

- .NET 10 SDK

```sh
cd dotnet
dotnet build
dotnet test
dotnet run --project src/Nirmitee.DaVinci.Pas.Host   # listens on :8086
```

Unit tests in both languages read `spec/` and `testdata/`, so run them from inside the repository. The harness and
the reference model need Python 3 and use only the standard library.

## Running the Inferno suites

The acceptance criteria are the Inferno Da Vinci PAS Server test suites (`davinci_pas_server_suite_v221` and
`davinci_pas_server_suite_v201`) with zero failures and zero skips. The test kit is not part of this repository.

Requirements: Docker with Compose 2.24.4 or newer, Python 3.

```sh
# once: clone the kit next to the code (the folder is gitignored)
git clone https://github.com/inferno-framework/davinci-pas-test-kit.git
cd davinci-pas-test-kit
# add docker-compose.override.yml (see "Running the Inferno suites yourself" in the README), then
COMPOSE_PROJECT_NAME=inferno-pas sh setup.sh
docker compose -p inferno-pas up -d        # UI on http://localhost:8280
cd ..
```

Start a server natively on the host with `PAS_ADMIN_ENABLED=true` for the duration of the run (Go on 8085 or .NET on
8086), then:

```sh
python3 harness/run_suite.py --suite both --label go-local
python3 harness/run_suite.py --suite both --label dotnet-local \
  --fhir-base "http://host.docker.internal:8086/{suite}/fhir" --control-url http://localhost:8086
```

Reports are written to `harness/reports/` (gitignored). The exit status is 0 when every test passed, 1 for failures
(including skips unless `--allow-skips`), 2 for a harness or connectivity problem and 3 when the only failures are the
entries in `harness/known_kit_issues.json`. The harness holds a lock so only one suite runs at a time against a local
kit. Restart the server without `PAS_ADMIN_ENABLED` when you are done. For a run on the public Inferno instance see
`harness/tls_public_run.md`.

The offline harness self test needs no Inferno: `python3 harness/stubs/selftest.py`.

## Data rules

- Synthetic data only. Never commit, paste or attach real patient data, member identifiers, tokens or keys.
- No default credentials: do not add bearer tokens, passwords or keys that are accepted by default.
- Do not commit audit logs (`data/`), raw harness reports or tunnel hostnames. Curated result summaries go in
  `docs/results/`.

## Pull request checklist

- [ ] Behavior changes are described in `spec/SPEC.md` (VERSION bumped, changelog entry added)
- [ ] Test data and golden examples are regenerated if the reference model, rules or generator changed
- [ ] `go/pas/adjudication-rules.json` is identical to `spec/adjudication-rules.json`
- [ ] Both implementations are updated, or a follow-up issue is linked
- [ ] `go vet ./...` and `go test ./...` pass; code is `gofmt`-clean
- [ ] `dotnet build` and `dotnet test` pass without new warnings
- [ ] Inferno suites show no failures or skips for any change that affects operations, decisions, Subscriptions,
      notifications or test data, with the report summary noted in the pull request
- [ ] Admin routes remain disabled by default
- [ ] README or configuration tables updated if settings changed
- [ ] No secrets, real PHI or machine-specific paths in code, docs or test data

## Reporting bugs and requesting features

Use the issue templates. For security issues follow [SECURITY.md](SECURITY.md) instead of opening an issue.

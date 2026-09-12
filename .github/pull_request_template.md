## Summary

<!-- What does this change and why? Link the related issue. -->

## Implementations touched

- [ ] Go (`go/`)
- [ ] .NET (`dotnet/`)
- [ ] Spec or adjudication rules (`spec/`)
- [ ] Test data, golden examples, reference model or harness
- [ ] Documentation or CI only

## Checklist

- [ ] Behavior changes are written into `spec/SPEC.md` first (VERSION bumped, changelog entry added)
- [ ] Test data and golden examples were regenerated with `python3 testdata/build_testdata.py` and
      `python3 spec/reference/pas_reference.py` if the reference model, rules or generator changed
- [ ] `go/pas/adjudication-rules.json` is identical to `spec/adjudication-rules.json`
- [ ] Both implementations are updated, or a follow-up issue is linked
- [ ] `go vet ./...` and `go test ./...` pass; code is `gofmt`-clean
- [ ] `dotnet build` and `dotnet test` pass
- [ ] Inferno suites show no failures or skips when operations, decisions, Subscriptions, notifications or test data
      changed
- [ ] The `/admin/*` routes remain disabled unless `PAS_ADMIN_ENABLED=true`
- [ ] Configuration tables in the READMEs are updated if settings changed
- [ ] No secrets, real PHI, tunnel hostnames or machine-specific paths

## Inferno results

<!-- Paste the result counts from the report summary for each suite you ran, or write "not applicable". -->

| Impl | Suite | pass | fail | skip |
|---|---|---|---|---|
| | | | | |

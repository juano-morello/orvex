# Shared golden-round seeding test support (LBT-24)

## What changed
The helpers that seed and execute golden crash/v1 rounds against PostgreSQL existed twice: in `internal/runtime/execution/helpers_integration_test.go` (LBT-12) and `cmd/crashctl/export_helpers_integration_test.go` (LBT-19). This was the open item deferred in `docs/specs/73dbe98d_audit-export-cli.md` §11 item 7. They now live in one package, `internal/persistence/postgres/pgtest/roundtest`, and both callers use it.

## Where
- `internal/persistence/postgres/pgtest/roundtest/roundtest.go`: `Registry`, `NewExecutor` (with an optional config mutator), `FundPlayer`, `HoldFor`, `SeedAdmission`.
- `internal/persistence/postgres/pgtest/roundtest/golden.go`: the `Golden` type (`Lease`, `Hash`, `UUID`), `ReadGolden`, `GoldenConfig`, `SeedGolden`, `SeedGoldenAdmissions`.
- Callers migrated: `helpers_integration_test.go` and the `close_bound`, `concurrency`, `crossround`, `e2e`, `execute`, `failure`, `ledger_audit` and `round` integration tests in `internal/runtime/execution/`; `export_helpers_integration_test.go` and `export_integration_test.go` in `cmd/crashctl/`.
- `README.md`: package list mentions `pgtest/roundtest`.
- `docs/specs/163e5d2b_shared-golden-round-seeding.md`: the spec for this work.

## Why this location
The package sits beside `pgtest`. It is importable from `execution_test` (an external test package) and from `cmd/crashctl`, which a helper inside `execution`'s own test files could not offer. Because the package is only imported by `_test.go` files, no production package depends on it. The spec has the import-cycle reasoning.

## Verify
Test assertions were migrated, not changed. Run `make test`, `make test-integration` (needs Docker), `make lint` and `make build`. This write-up did not run them.

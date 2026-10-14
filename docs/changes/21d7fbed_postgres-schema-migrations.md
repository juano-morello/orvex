# LBT-10: PostgreSQL schema and migration tooling

## What changed
Orvex now has a versioned PostgreSQL schema (ADR-005) and a migration runner. The approved spec is committed at `docs/specs/17380a6e_postgresql-schema-migrations.md`, marked approved. Decisions P-1 to P-26 are in that spec. The implementation plan is `docs/specs/21d7fbed_postgres-schema-implementation.md`.

- **Schema:** `migrations/0001`–`0006`, each with an `.up.sql` and a `.down.sql`. They cover foundation, rounds and admissions, chain and execution, snapshots, ledger and outbox. `migrations/migrations.go` embeds them as `migrations.FS`.
- **Runner:** `internal/persistence/postgres` has `migrate.go`, `errors.go`, `tx.go` and `doc.go`. It loads the embedded migrations and applies or reverts them. It uses pgx v5.11.0.
- **Test support:** `internal/persistence/postgres/pgtest` holds the Testcontainers harness (`pgtest.go`), `load.go` and `fingerprint.go`. It uses testcontainers-go v0.44.0.
- **Export:** `internal/persistence/eventstore/export.go` exports stored events. The LBT-9 codec, chain and export formats are stored as they are. The diff shows no change to them or to the golden fixtures.
- **CLI:** `cmd/crashctl/migrate.go` adds:
  - `crashctl migrate up [-to N]`
  - `crashctl migrate down -to N`
  - `crashctl migrate status`
- **Other files:** `deployments/docker/compose.yaml` gains a PostgreSQL service. `docs/architecture/persistence.md` is new. The diff also touches `.gitattributes`, `.golangci.yml`, `README.md`, `AGENTS.md`, `go.mod`, `go.sum` and `.github/workflows/ci.yml`. The CI file gets a new `integration` job.

## Using it
- Set `ORVEX_DATABASE_URL`. For the compose database that is `postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable`.
- Run `./bin/crashctl migrate up`.
- Exit codes:

|Code|Meaning|
|---|---|
|0|done|
|1|a migration failed|
|2|usage error|
|3|unsupported schema state|
|4|I/O error|

## Verifying
- `make test` runs the unit tests.
- `make test-integration` is new. It runs `go test -tags integration -count=1 ./...` against a real PostgreSQL container. It needs a Docker daemon.
- `make lint`, `make build` and `make golden-check` should still pass.
- The integration tests are `schema_integration_test.go` and `migrate_integration_test.go` in the postgres package, plus integration tests in `cmd/crashctl` and `eventstore`. They include the clean up/down migration run.

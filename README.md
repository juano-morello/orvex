# Orvex

Orvex is a deterministic, audit-first multiplayer Crash game platform. It uses simulated credits only.

Status: scaffold ([LBT-5](https://linear.app/lbtworks/issue/LBT-5/scaffold-go-module-and-repository-conventions)), the game contract ([LBT-6](https://linear.app/lbtworks/issue/LBT-6/implement-generic-game-contract-and-versioned-adapter)), the frozen Crash v1 math ([LBT-7](https://linear.app/lbtworks/issue/LBT-7/freeze-crash-mathematical-specification-and-golden-vectors)), the Crash v1 round rules ([LBT-8](https://linear.app/lbtworks/issue/LBT-8/implement-crash-decideevolve-and-lifecycle-invariants)), the canonical codec, hash chain and audit CLI ([LBT-9](https://linear.app/lbtworks/issue/LBT-9/build-canonical-event-codec-and-audit-replay-cli)), the PostgreSQL schema and migration tooling ([LBT-10](https://linear.app/lbtworks/issue/LBT-10/implement-postgresql-schema-and-migration-tooling)), the durable per-round admission sequencer ([LBT-11](https://linear.app/lbtworks/issue/LBT-11/implement-durable-per-round-postgresql-admission-sequencer)), the atomic execution commit with the simulated-credit ledger, outbox and round creation ([LBT-12](https://linear.app/lbtworks/issue/LBT-12/implement-atomic-execution-commit-ledger-and-outbox)), the audit history export CLI ([LBT-19](https://linear.app/lbtworks/issue/LBT-19/crashctl-audit-export-round-id)) and the audit ledger stage with history format v2 ([LBT-18](https://linear.app/lbtworks/issue/LBT-18/audit-ledger-stage-history-format-v2)). No service wiring, leases, outbox relay or transport exists yet.

## Layout

- `cmd/gateway`: client-facing gateway service (scaffold)
- `cmd/engine`: game execution service (scaffold)
- `cmd/crashctl`: replay/audit and operations CLI: `crashctl audit verify|replay|chain` (LBT-9), `crashctl audit export` (LBT-19), `crashctl migrate up|down|status` (LBT-10)
- `internal/game/crash/v1`: Crash v1 rules: frozen arithmetic, fairness derivation and golden vectors (ADR-004, LBT-7); round lifecycle Decide/Evolve, command authority and public projection (ADR-003, LBT-8); canonical codec (ADR-001, LBT-9)
- `internal/buildinfo`: build identifier stamped at link time
- `internal/canonjson`: Orvex Canonical JSON (RFC 8785 subset): encoder and strict decoder (LBT-9)
- `internal/audit/chain`: chain records, link hash and state hash
- `internal/audit/replay`: state replay and decision replay through the rules registry
- `internal/audit/verification`: history reader (formats v1 and v2), staged verifier with the ledger stage, golden histories
- `internal/persistence/postgres`: PostgreSQL migration runner, SQLSTATE constants and transaction options (ADR-005, LBT-10); `pgtest`: Testcontainers harness, catalog fingerprint and audit-history loader for integration tests; `pgtest/roundtest`: crash/v1 round and golden-history seeding through the executor for integration tests (LBT-24)
- `internal/persistence/eventstore`: audit history export from the database (v1 LBT-10, v2 with ledger rows LBT-18)
- `internal/runtime/admission`: durable per-round admission sequencer (Transaction A): dedupe, sequence and effective time, admission window and closure barrier, credit holds, crash/v1 admission policy (ADR-002, ADR-005, LBT-11)
- `internal/runtime/execution`: per-round command executor (Transaction B): atomic commit of the chain record, events, ledger postings, hold releases, outbox message and snapshot under a compare-and-set and fencing token; cold load by chain replay; round creation; crash/v1 execution policy (ADR-005, LBT-12)
- `internal/ledger`: pure simulated-credit posting rules and deterministic ledger IDs (ADR-004, LBT-12)
- `internal/game`: typed game contract, canonical codec contract, deterministic input envelope, versioned adapter and rules registry (ADR-003)
- `migrations`: embedded PostgreSQL schema migrations (immutable once merged)
- `deployments/docker`: local PostgreSQL for development (Docker Compose)
- `docs/adr`: architecture decision records ADR-001 to ADR-007
- `docs/architecture`: durable design documentation ([persistence](docs/architecture/persistence.md))

## Development

Prerequisites: Go 1.26, golangci-lint v2.14.0, Python 3.11 or newer (standard library only; used by `make golden-check`) and a Docker-compatible daemon (used by `make test-integration`).

- `make build`: build all binaries into `bin/` (or `make build-gateway`, `make build-engine`, `make build-crashctl`)
- `make test`: run tests with the race detector
- `make test-integration`: run the unit tests plus the PostgreSQL integration tests (Testcontainers, pinned PostgreSQL 18 image); fails without a Docker daemon. On hosts without cgo: `make test-integration GOTESTFLAGS=`
- `make lint`: run golangci-lint
- `make golden-check`: regenerate the Crash v1 golden vectors and the audit golden histories with their independent Python oracles and compare them with the committed files
- `make fuzz`: fuzz every Go fuzz target for `FUZZTIME` each (default `30s`), one target per `go test` run; `make fuzz-list` lists them, `make fuzz-one FUZZ_PKG=internal/canonjson FUZZ_TARGET=FuzzUnmarshal` runs one and `make fuzz-guard-check` checks that `fuzz-one` refuses unknown names
- `make test-repeat`: the specs' repeated PostgreSQL runs (`make test-repeat-admission`, `make test-repeat-execution`, `make test-repeat-export`); needs a Docker daemon
- CI: `.github/workflows/ci.yml` on every push and pull request; `.github/workflows/nightly.yml` nightly and on demand (every fuzz target and the repeat runs)
- `make clean`: remove build output
- `crashctl migrate up [-to N]`, `crashctl migrate down -to N`, `crashctl migrate status`: apply, revert or list schema migrations on the database named by `ORVEX_DATABASE_URL`
- `crashctl audit export [-unsettled] ROUND_ID FILE`: write a settled round's audit history file from the database named by `ORVEX_DATABASE_URL`; for a round whose secret config is not yet revealed, `-unsettled` needs the privileged path: `SELECT` on `orvex.round_secrets` and the keyring named by `ORVEX_SECRET_KEYRING_FILE`

Local PostgreSQL:

- `docker compose -f deployments/docker/compose.yaml up -d --wait`
- `export ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable'`, then `./bin/crashctl migrate up`
- `docker compose -f deployments/docker/compose.yaml down -v`

See [AGENTS.md](AGENTS.md) for contributor and agent guidance and [docs/adr/](docs/adr/) for the architectural decisions.

## Audit

`make audit ROUND_ID=<uuid>` builds `crashctl`, exports the round from the database named by `ORVEX_DATABASE_URL` with `crashctl audit export` into a new private temporary directory (the file has mode 0600), verifies it with `crashctl audit verify -expect-head` set to the head the export printed, and removes the directory.

- `AUDIT_FORMAT=NAME` passes `-format NAME`. `AUDIT_UNSETTLED=1` passes `-unsettled`: it audits a round that is not settled at its newest snapshot, and the temporary file then holds the round's unrevealed secret config. For a round whose secret config is not revealed, it needs the privileged path (LBT-21): a role with the `orvex_secret_reader` grants in `ORVEX_DATABASE_URL` and the keyring named by `ORVEX_SECRET_KEYRING_FILE`, set in the environment or on the command line (`make audit ROUND_ID=<uuid> AUDIT_UNSETTLED=1 ORVEX_SECRET_KEYRING_FILE=<path>`).
- A missing `ROUND_ID` or `ORVEX_DATABASE_URL`, or an `AUDIT_UNSETTLED` other than `1`, stops before the build with an `audit:` message. A failed step prints `audit: crashctl audit export failed with exit code N` (or `verify`); make reports `Error N` and exits 2. `N` is `crashctl`'s code: 1 failed, 2 usage or a keyring problem, 3 unsupported, 4 I/O or database, 5 round not found, 6 round not settled or its secret config not revealed.
- The expected head comes from the same database as the file, so the run checks the export end to end but is not an independent anchor (ADR-001). Keep the printed head elsewhere to anchor it.
- Local Compose database: `ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable' make audit ROUND_ID=<uuid>`.

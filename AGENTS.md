# AGENTS.md

Guidance for agents and contributors working in Orvex: a deterministic, audit-first multiplayer Crash game platform (simulated credits only).

## Architectural decisions

The Linear ADR documents are the source of record. `docs/adr/` is a verbatim mirror. An ADR amendment must update its mirror file in the same change.

| ADR | Title | Status | Scope |
|---|---|---|---|
| [ADR-001](docs/adr/0001-audit-first-deterministic-execution.md) | Audit-First Deterministic Execution | Accepted | Immutable ordered event history is authoritative; replay, hash chains, per-round consistency boundary. |
| [ADR-002](docs/adr/0002-durable-command-admission-and-timing.md) | Durable Command Admission & Timing | Accepted with amendment | Durable admission sequence and effective time; cash-out deadline eligibility. |
| [ADR-003](docs/adr/0003-game-contract-and-event-semantics.md) | Game Contract & Event Semantics | Accepted | Pure `Game[S,C,E]` contract (`Decide`/`Evolve`); Crash v1 commands, events, invariants. |
| [ADR-004](docs/adr/0004-multiplayer-crash-game-specification.md) | Multiplayer Crash Game Specification | Accepted (implementation constants pending) | Phases, apd decimal arithmetic, payouts, commit/reveal fairness, settlement. |
| [ADR-005](docs/adr/0005-postgresql-event-store-and-transaction-model.md) | PostgreSQL Event Store & Transaction Model | Accepted | System of record; admission and execution transactions; ledger; outbox; failure semantics. |
| [ADR-006](docs/adr/0006-correctness-and-verification-strategy.md) | Correctness & Verification Strategy | Accepted | Layered verification, audit verifier, failure tests, performance experiments, CI. |
| [ADR-007](docs/adr/0007-repository-architecture-and-build-strategy.md) | Repository Architecture & Build Strategy | Accepted | Go-first monorepo, single module, layout, build and CI conventions. |

## Before changing anything

Read the ADRs relevant to the area you touch before you plan or edit:

- determinism, replay, audit, event history: ADR-001, ADR-006
- command admission, timing, deadlines: ADR-002, ADR-005
- the game contract, the Crash domain, arithmetic, fairness: ADR-003, ADR-004
- PostgreSQL, transactions, ledger, outbox: ADR-005
- tests and CI: ADR-006
- repository layout, modules, build, tooling: ADR-007
- round secrets, sealing, the keyring, the reveal table: ADR-004, ADR-005 and `docs/specs/87d8d90c_round-secret-protection.md` (LBT-21)

## Flag conflicts explicitly

If a change conflicts with, or needs to deviate from, an ADR, say so explicitly in the plan, the PR description and the agent report. Cite the ADR number and section. Do not silently work around an ADR. A deliberate deviation needs an ADR amendment or a new ADR first.

## Repository conventions (ADR-007)

- One Go module (`github.com/juano-morello/orvex`, Go 1.26). No second module, no `go.work`, no Nx or Turborepo.
- `cmd/<binary>` stays thin and independent. Binaries never import each other. Shared code lives in `internal/`.
- The layout is the "Proposed layout" in ADR-007. Create directories only when they hold real code.
- Keep dependencies minimal and pinned. Justify each new module in the PR (ADR-004 names `cockroachdb/apd/v3` as the planned arithmetic dependency).
- Released historical rules versions, golden vectors and canonical serializers are immutable (ADR-001, ADR-003, ADR-007).
- Game `Decide`/`Evolve` code is pure: no I/O, wall clock or uncontrolled randomness (ADR-003).
- Merged migrations in `migrations/` are immutable; change the schema with a new migration (checksums are enforced).
- Round secret configs are sealed at rest (LBT-21). New rounds store only the AES-256-GCM form (`internal/persistence/sealing`), and the record that settles a round writes the plaintext to `orvex.round_secret_reveals`. No SQL statement, log line or error text may carry a key, keyring content or an unrevealed secret config. Tests seal with `pgtest.Keyring()`; no other key belongs in the repository.
- `crashctl` never prints any part of `ORVEX_DATABASE_URL` (LBT-21 K-12): render database errors with `describe`, never with `err.Error()`.

## Commands

- `make build`: build all binaries into `bin/`.
- `make build-gateway`, `make build-engine`, `make build-crashctl`: build one binary.
- `make test`: `go test -race ./...`. Override with `make test GOTESTFLAGS=` on hosts without cgo.
- `make test-integration`: the tests plus the PostgreSQL integration tests (`-tags integration`, Testcontainers). It needs a Docker daemon and fails without one. Override with `make test-integration GOTESTFLAGS=` on hosts without cgo. The fuzz and repeat targets take `GOTESTFLAGS` too.
- `make lint`: golangci-lint. It requires v2.14.0. The pin lives in both `Makefile` and `.github/workflows/ci.yml`; change both together.
- `make golden-check`: regenerate the Crash v1 golden vectors and the audit golden histories with the independent Python oracles (Python 3.11+, standard library only) and fail if they differ from the committed files. Never regenerate vectors or fixtures from Go output.
- `make fuzz`: run every Go fuzz target for `FUZZTIME` each (default `30s`), one `go test -fuzz` invocation per target, since Go fuzzes one target per run. `make fuzz-list` prints the targets as `<package dir>:<FuzzName>`. `make fuzz-one FUZZ_PKG=internal/game/crash/v1 FUZZ_TARGET=FuzzRoundLifecycle` runs one; it refuses (exit 2) any name not in the list, and `make fuzz-guard-check` tests that refusal without running Go. A failing input is written to `<package>/testdata/fuzz/<FuzzName>/`; reproduce it with `go test -run=<FuzzName>/<file>` and fix the cause.
- `make test-repeat`: the repeat (flakiness) runs the specs require, against PostgreSQL (needs a Docker daemon): `make test-repeat-admission` (`-count=5`, LBT-11 S-14), `make test-repeat-execution` (`-count=5`, LBT-12 E-17) and `make test-repeat-export` (`-count=3`, LBT-19).
- `make audit ROUND_ID=<uuid>`: build `crashctl`, export the round from the database named by `ORVEX_DATABASE_URL` into a private temporary directory and verify it with `crashctl audit verify -expect-head` set to the head the export printed; `AUDIT_FORMAT=NAME` and `AUDIT_UNSETTLED=1` map to `-format` and `-unsettled`. For a round whose secret config is not revealed, `AUDIT_UNSETTLED=1` needs the privileged path (LBT-21 K-10, K-13): an `orvex_secret_reader` role in `ORVEX_DATABASE_URL` and the keyring named by `ORVEX_SECRET_KEYRING_FILE`, from the environment or the make command line. A failed step names `crashctl`'s exit code (make reports `Error N` and exits 2). The `TestMakeAudit*` integration tests in `cmd/crashctl` cover it: run `make test-integration` when you change the target.
- CI: `.github/workflows/ci.yml` runs on every push and pull request. `.github/workflows/nightly.yml` runs nightly and on manual dispatch: every fuzz target (5m each by default) and the three `make test-repeat-*` suites. Pin every GitHub Action to a full commit SHA, with its version in a trailing comment.
- `make clean`: remove `bin/`.

Run `make test` and `make lint` before handing off work. When you change `migrations/`, `internal/persistence/` or `internal/runtime/`, also run `make test-integration`. When you change `internal/runtime/admission/`, `internal/runtime/execution/` or the audit export path (`cmd/crashctl` export, `internal/persistence/eventstore/`), also run the matching `make test-repeat-*` target.

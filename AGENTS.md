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

## Commands

- `make build`: build all binaries into `bin/`.
- `make build-gateway`, `make build-engine`, `make build-crashctl`: build one binary.
- `make test`: `go test -race ./...`. Override with `make test GOTESTFLAGS=` on hosts without cgo.
- `make test-integration`: the tests plus the PostgreSQL integration tests (`-tags integration`, Testcontainers). It needs a Docker daemon and fails without one. Override with `make test-integration GOTESTFLAGS=` on hosts without cgo.
- `make lint`: golangci-lint. It requires v2.14.0. The pin lives in both `Makefile` and `.github/workflows/ci.yml`; change both together.
- `make golden-check`: regenerate the Crash v1 golden vectors and the audit golden histories with the independent Python oracles (Python 3.11+, standard library only) and fail if they differ from the committed files. Never regenerate vectors or fixtures from Go output.
- `make clean`: remove `bin/`.

Run `make test` and `make lint` before handing off work. When you change `migrations/`, `internal/persistence/` or `internal/runtime/`, also run `make test-integration`.

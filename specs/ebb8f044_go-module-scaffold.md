# Plan: Scaffold Go module and repository conventions (LBT-5 / ADR-007)

## Goal

Turn the near-empty Orvex repository into a single-module Go monorepo skeleton:
`go.mod`, three independent binaries under `cmd/`, one real `internal/` package,
a Makefile (`build`, `build-<binary>`, `test`, `lint`, `clean`), a pinned
golangci-lint v2.14.0 config, a GitHub Actions CI workflow, `docs/adr/` mirroring
ADR-001…ADR-007 verbatim, `AGENTS.md`, and a short `README.md`.

No third-party Go dependencies. `go.mod` has no `require` block and there is no `go.sum`.

## Current state (verified)

- Repo root contains only `.gitignore`, `.omp/mcp.json`, `odin/odin.config.yaml`. No `specs/` until the planner adds it.
- `odin/odin.config.yaml` already expects `make test`, `make lint`, `go vet ./...` and `make build`. Do **not** edit anything under `odin/` (protected).
- Toolchain on the operator's machine: `go1.26.5`, `golangci-lint 2.14.0` (`golangci-lint version --short` prints `2.14.0`), gcc present (cgo works, so `-race` works), GNU make.
- There is no `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md` or `.editorconfig` to follow yet. This change creates `AGENTS.md`.
- Latest GitHub Action majors (checked with `git ls-remote`): `actions/checkout@v7`, `actions/setup-go@v7`, `golangci/golangci-lint-action@v9`.

## Out of scope (do not create)

- Game, runtime, persistence, transport or audit logic (LBT-6 onward). That means no `internal/game`, `internal/runtime`, and so on.
- Empty or placeholder directories and infrastructure: `proto/`, `web/`, `migrations/`, `tests/`, `deployments/`, `docs/architecture/`, `docs/experiments/`, Dockerfiles, Docker Compose, Kustomize, Buf config.
- Nx, Turborepo, a second Go module, `go.work`.
- Make targets `proto`, `dev`, `audit`, `test-integration`.
- Path-filtered CI. Only Go exists today, so add path filters when a second toolchain arrives.
- `CLAUDE.md`, `.editorconfig`, `CONTRIBUTING.md`, `docs/adr/README.md` index, and any edit to `odin/` or `.omp/`.

## Files to create / change

### 1. `go.mod` (new)

```
module github.com/juano-morello/orvex

go 1.26.0
```

No `toolchain` line and no `require`. Do not create `go.sum`; `go mod tidy` would not create one anyway, because there are no deps.

### 2. `internal/buildinfo/buildinfo.go` (new)

This is the only `internal/` package for now. It carries the build identifier that ADR-007 asks for ("reproducible build identifiers").

```go
// Package buildinfo exposes the build identifier stamped into Orvex binaries.
package buildinfo

// Version identifies the build. It defaults to "dev" and is overridden at link
// time by the Makefile:
//
//	-ldflags "-X github.com/juano-morello/orvex/internal/buildinfo.Version=<git describe>"
var Version = "dev"

// String returns the version line printed by a binary, e.g. "gateway dev".
func String(binary string) string {
	return binary + " " + Version
}
```

### 3. `cmd/gateway/main.go`, `cmd/engine/main.go`, `cmd/crashctl/main.go` (new, one each)

Each binary is its own `package main`. Each imports only the standard library and
`internal/buildinfo`, and never another `cmd/` package. Keep the three files
identical apart from the name and the package doc comment. Do **not** factor the
CLI handling into a shared package, because the binaries have to stay independent.

Package doc comments:
- gateway: `// Command gateway is the Orvex client-facing gateway service (ADR-007). It is a scaffold: no service is implemented yet.`
- engine: `// Command engine is the Orvex deterministic game execution service (ADR-007). It is a scaffold: no service is implemented yet.`
- crashctl: `// Command crashctl is the Orvex replay/audit CLI (ADR-007). It is a scaffold: no commands are implemented yet.`

Shape (shown for gateway; substitute the name):

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/juano-morello/orvex/internal/buildinfo"
)

const name = "gateway"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. Exit codes: 0 success/help, 1 not
// implemented or write failure, 2 usage error.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "%s: unexpected arguments: %v\n", name, fs.Args())
		return 2
	}
	if *showVersion {
		if _, err := fmt.Fprintln(stdout, buildinfo.String(name)); err != nil {
			return 1
		}
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "%s: not implemented yet\n", name)
	return 1
}
```

Behavior contract, which is what the tests check:

| args | exit | stdout | stderr |
|---|---|---|---|
| `-version` | 0 | `<name> <Version>\n` | empty |
| (none) | 1 | empty | contains `not implemented` |
| `extra` (positional) | 2 | empty | contains `unexpected arguments` |
| `-bogus` | 2 | empty | non-empty (flag package error + usage) |
| `-h` | 0 | empty | non-empty (usage) |

Lint pitfall: errcheck flags ignored `fmt.Fprint*` results on an `io.Writer`. Handle
them explicitly with `_, _ =` or by checking the error, as the shape above does.

### 4. Tests (new). Each behavior above has a named test

- `internal/buildinfo/buildinfo_test.go`
  - `TestStringDefaultVersion`: `String("gateway") == "gateway dev"`.
  - `TestStringInjectedVersion`: save `Version`, set it to `"v1.2.3"`, defer the restore, then assert `String("engine") == "engine v1.2.3"`. Do not use `t.Parallel()` here.
- `cmd/gateway/main_test.go`, `cmd/engine/main_test.go`, `cmd/crashctl/main_test.go` (`package main`)
  - `TestRun`: a table-driven test over the five rows of the behavior table. Use `bytes.Buffer` for stdout and stderr, then assert the exit code, the exact stdout for `-version` (`name + " " + buildinfo.Version + "\n"`), empty stdout for the other rows, and the stderr substrings listed.
  - `TestRunWriteError`: `run([]string{"-version"}, failingWriter{}, io.Discard)` returns `1`. Here `failingWriter.Write` returns an error.

Use only the standard library `testing` package, with no assertion libraries.

### 5. `.golangci.yml` (new, golangci-lint v2 format)

```yaml
version: "2"

linters:
  default: standard

formatters:
  enable:
    - gofmt
    - goimports
  settings:
    goimports:
      local-prefixes:
        - github.com/juano-morello/orvex
```

Check it with `golangci-lint config verify`, which exits 0 when the config is valid.

### 6. `Makefile` (new; recipe lines MUST be tab-indented)

```make
# Orvex build orchestration (ADR-007). Add targets (proto, dev, audit,
# test-integration) only when they become meaningful.

GO ?= go
BINARIES := gateway engine crashctl
BIN_DIR := bin
MODULE := github.com/juano-morello/orvex
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(MODULE)/internal/buildinfo.Version=$(VERSION)
GOTESTFLAGS ?= -race

# Pinned linter version. Keep in sync with .github/workflows/ci.yml.
GOLANGCI_LINT ?= golangci-lint
GOLANGCI_LINT_VERSION := v2.14.0

BUILD_TARGETS := $(addprefix build-,$(BINARIES))

.PHONY: all build $(BUILD_TARGETS) test lint clean

all: build

build: $(BUILD_TARGETS)

# Static pattern rule: plain pattern rules are skipped for .PHONY targets.
$(BUILD_TARGETS): build-%:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$* ./cmd/$*

test:
	$(GO) test $(GOTESTFLAGS) ./...

lint:
	@found="$$($(GOLANGCI_LINT) version --short 2>/dev/null)"; \
	if [ "v$$found" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) required (found: '$${found:-none}')." >&2; \
		echo "Install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)" >&2; \
		exit 1; \
	fi
	$(GOLANGCI_LINT) run ./...

clean:
	rm -rf $(BIN_DIR)
```

Key points:
- `make build` builds all three binaries into `bin/`. `make build-gateway`, `make build-engine` and `make build-crashctl` each build exactly one, which is how they build independently.
- `make lint` fails if the installed golangci-lint is not v2.14.0. CI runs the same `make lint`, so any drift between the workflow pin and the Makefile pin turns CI red.
- `GOTESTFLAGS ?= -race` follows ADR-006 (race checks on PRs). Running `make test GOTESTFLAGS=` works on hosts without cgo.

### 7. `.gitignore` (edit: append)

Keep the existing lines and append:

```

# build output
/bin/
```

### 8. `.github/workflows/ci.yml` (new)

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true

env:
  GO_VERSION: "1.26.x"
  # Keep in sync with GOLANGCI_LINT_VERSION in the Makefile (make lint enforces it).
  GOLANGCI_LINT_VERSION: v2.14.0

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with:
          go-version: ${{ env.GO_VERSION }}
          cache-dependency-path: go.mod
      - run: make test

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with:
          go-version: ${{ env.GO_VERSION }}
          cache-dependency-path: go.mod
      - uses: golangci/golangci-lint-action@v9
        with:
          version: ${{ env.GOLANGCI_LINT_VERSION }}
          install-only: true
      - run: make lint

  build:
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        binary: [gateway, engine, crashctl]
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with:
          go-version: ${{ env.GO_VERSION }}
          cache-dependency-path: go.mod
      - run: make build-${{ matrix.binary }}
```

There is no orchestrator (Nx or Turborepo), only make and go. `cache-dependency-path: go.mod` is
there because no `go.sum` exists. Without it setup-go warns about the missing cache key.

### 9. `docs/adr/0001-…0007-*.md` (new, 7 files)

One file per ADR. The file names follow the Linear document slugs:

- `docs/adr/0001-audit-first-deterministic-execution.md`
- `docs/adr/0002-durable-command-admission-and-timing.md`
- `docs/adr/0003-game-contract-and-event-semantics.md`
- `docs/adr/0004-multiplayer-crash-game-specification.md`
- `docs/adr/0005-postgresql-event-store-and-transaction-model.md`
- `docs/adr/0006-correctness-and-verification-strategy.md`
- `docs/adr/0007-repository-architecture-and-build-strategy.md`

Each file starts with `# ADR-00N: <Title>`, then a blank line, a `Source: <linear url>` line,
and a blank line, followed by the ADR body exactly as in the Linear source. Keep its
typos and escapes (`reexecuting`, `Game\[S,C,E\]`, `[github.com/cockroachdb/apd/v3](<http://github.com/cockroachdb/apd/v3>)`).
The one transformation is in ADR-007: the Linear mention tag `<issue …>LBT-5</issue>` becomes
`[LBT-5](https://linear.app/lbtworks/issue/LBT-5/scaffold-go-module-and-repository-conventions)`.

**Do not retype the ADRs.** The exact file contents are in the Appendix below, between
`<!-- BEGIN-FILE path -->` / `<!-- END-FILE -->` marker lines. Extract them mechanically
from the repo root:

```sh
mkdir -p docs/adr && awk '/^<!-- BEGIN-FILE /{f=$3; next} /^<!-- END-FILE -->$/{close(f); f=""; next} f!=""{print > f}' "/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/ebb8f044/context_handoff/plan.md"
```

Then check: `ls docs/adr` lists exactly 7 files, `grep -c '<issue' docs/adr/*.md` reports 0 for every file,
and no file contains a line starting with `<!--` or a four-backtick line.

### 10. `AGENTS.md` (new)

Required content (acceptance hinges on items a–c):

a. **ADR index**: a table of ADR-001…ADR-007, each linked to its `docs/adr/` file by relative path (for example `[ADR-001](docs/adr/0001-audit-first-deterministic-execution.md)`), with the title, status (copy each ADR's `Status:` line) and a one-line scope summary. State that the Linear documents are the source of record and `docs/adr/` is a verbatim mirror. An ADR amendment must update the mirror in the same change.
b. **Before changing anything**: read the ADRs relevant to the area being touched. Include a short routing list:
   - determinism, replay, audit, event history → ADR-001, ADR-006
   - command admission, timing, deadlines → ADR-002, ADR-005
   - the game contract, the Crash domain, arithmetic, fairness → ADR-003, ADR-004
   - PostgreSQL, transactions, ledger, outbox → ADR-005
   - tests and CI → ADR-006
   - repository layout, modules, build, tooling → ADR-007
c. **Flag conflicts explicitly**: if a change conflicts with, or needs to deviate from, an ADR, say so explicitly in the plan, the PR description and the agent report. Cite the ADR number and section. Do not silently work around an ADR. A deliberate deviation needs an ADR amendment or a new ADR first.
d. **Repository conventions** (from ADR-007):
   - one Go module (`github.com/juano-morello/orvex`, Go 1.26); no second module, no `go.work`, no Nx or Turborepo;
   - `cmd/<binary>` stays thin and independent: binaries never import each other, and shared code lives in `internal/`;
   - the layout is the "Proposed layout" in ADR-007; create directories only when they hold real code;
   - minimal, pinned dependencies: justify each new module in the PR (ADR-004 names `cockroachdb/apd/v3` as the planned arithmetic dependency);
   - historical rules versions, golden vectors and canonical serializers are immutable once released (ADR-001, ADR-003, ADR-007);
   - game `Decide`/`Evolve` code is pure: no I/O, wall clock or uncontrolled randomness (ADR-003).
e. **Commands**: `make build`, `make build-<gateway|engine|crashctl>`, `make test` (`go test -race ./...`; override with `GOTESTFLAGS=`), `make lint` (requires golangci-lint v2.14.0; the pin lives in both `Makefile` and `.github/workflows/ci.yml`, so change both together), `make clean`. Run `make test` and `make lint` before handing off work.

Keep it concise, about 60–100 lines. It is written for agents, so use plain imperative statements and no marketing prose.

### 11. `README.md` (new, short)

Include a one-paragraph description (Orvex: a deterministic, audit-first multiplayer Crash game platform; simulated credits only), the current status (a scaffold per LBT-5), a layout list of what exists today (`cmd/gateway`, `cmd/engine`, `cmd/crashctl`, `internal/buildinfo`, `docs/adr`), the make commands with the Go 1.26 and golangci-lint v2.14.0 prerequisites, and links to `AGENTS.md` and `docs/adr/`. Keep it under about 40 lines.

## Final tree (everything this change adds)

```
.github/workflows/ci.yml
.golangci.yml
AGENTS.md
Makefile
README.md
cmd/crashctl/main.go
cmd/crashctl/main_test.go
cmd/engine/main.go
cmd/engine/main_test.go
cmd/gateway/main.go
cmd/gateway/main_test.go
docs/adr/0001-audit-first-deterministic-execution.md
docs/adr/0002-durable-command-admission-and-timing.md
docs/adr/0003-game-contract-and-event-semantics.md
docs/adr/0004-multiplayer-crash-game-specification.md
docs/adr/0005-postgresql-event-store-and-transaction-model.md
docs/adr/0006-correctness-and-verification-strategy.md
docs/adr/0007-repository-architecture-and-build-strategy.md
go.mod
internal/buildinfo/buildinfo.go
internal/buildinfo/buildinfo_test.go
.gitignore (modified)
```

The planner separately adds `specs/ebb8f044_go-module-scaffold.md`. Nothing else.

## Verification (run from the repo root; judge each by exit status)

1. `go build ./... && go vet ./... && go test ./...`: all exit 0.
2. `go mod tidy -diff` exits 0, so `go.mod` is already tidy. `test ! -e go.sum` holds.
3. `make test` exits 0 and runs the tests with `-race`.
4. `make lint` exits 0. Then run `make lint GOLANGCI_LINT=/bin/false`, which must exit non-zero with the "v2.14.0 required" message, proving the pin check works.
5. `golangci-lint config verify` exits 0.
6. Run `make clean && make build-gateway`. `bin/gateway` must exist and `bin/engine` must not. Then `make clean && make build-engine`: only `bin/engine` exists. Then `make clean && make build`: all three exist.
7. `./bin/gateway -version` prints `gateway <git describe output>` (not `dev`) and exits 0. `./bin/engine` with no args exits 1. `./bin/crashctl -version` exits 0.
8. Independence: `go list -deps ./cmd/gateway | grep -c orvex/cmd/` prints `1`, and the same holds for engine.
9. `git status --porcelain --ignored` shows `bin/` only as ignored (`!! bin/`). `git status --porcelain` lists only the files in the "Final tree" section plus `specs/`.
10. ADR mirror: there are 7 files, no `<issue` tags, no leftover marker lines, and `grep -l 'ADR-00[1-7]' AGENTS.md` matches. Every `docs/adr/...` link in AGENTS.md and README.md resolves to an existing file. One way to check: `grep -o 'docs/adr/[0-9a-z-]*\.md' AGENTS.md README.md | cut -d: -f2 | sort -u | xargs ls`.
11. Optional, if network is available: `go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/ci.yml` exits 0. `go run` of a remote module does not modify `go.mod`; confirm afterwards with `git diff --exit-code go.mod`.

---

## Appendix: exact `docs/adr/` file contents

Extract these with the awk command in section 9. Every line between a `BEGIN-FILE` marker and the following `END-FILE` marker is written verbatim to the named file.

````markdown
<!-- BEGIN-FILE docs/adr/0001-audit-first-deterministic-execution.md -->
# ADR-001: Audit-First Deterministic Execution

Source: https://linear.app/lbtworks/document/adr-001-audit-first-deterministic-execution-901f8bd77d96

Status: Accepted

## Context

Orvex must explain and reproduce every committed outcome in a shared multiplayer Crash round, including after crashes and software upgrades.

## Decision

The immutable, ordered domain event history is the authoritative record; current state is a projection. Support (1) state replay by evolving recorded events, (2) decision replay by reexecuting original admitted commands and deterministic inputs with historical rules, and (3) visual playback from recorded timeline metadata. Each round is a consistency and ownership boundary. Preserve original commands, effective times, event and schema versions, rule/build identifiers, randomness commitments/reveals, state checkpoints and ledger references. Use canonical event/state encodings and per-round hash chains, with independent anchoring as a future strengthening measure.

## Invariants

Contiguous event sequence; replay yields identical canonical state hash; a bet settles at most once; every decision is attributable to a command and rules version; historical rules remain reproducible.

## Trade-offs and limitations

Event schema evolution, storage, and verifier complexity. Hash chains in the same mutable database are tamper-evident only against a separately trusted anchor. Visual replay cannot prove exactly what an individual client saw.

## Implementation notes

Define canonical encoding, verification CLI, retention and artifact versioning before claiming audit-grade behavior.
<!-- END-FILE -->
<!-- BEGIN-FILE docs/adr/0002-durable-command-admission-and-timing.md -->
# ADR-002: Durable Command Admission & Timing

Source: https://linear.app/lbtworks/document/adr-002-durable-command-admission-and-timing-be124b9ab8e6

Status: Accepted with amendment

## Context

Cash-outs near the crash deadline must have reproducible eligibility even when gateway and engine instances disagree about time or process commands late.

## Decision

Separate durable command admission from deterministic execution. Assign an authoritative per-round admission sequence and effective timestamp; initially use PostgreSQL row-lock sequencing and a database clock read after acquiring the lock. Gateway receive time is diagnostic, not authoritative. A cash-out is timing-eligible only when its durable effective admission time is strictly before the crash deadline and after round start, subject to other game invariants. Admission acknowledgment means durable receipt, not successful execution. Deduplicate retries by stable command ID. The admission sequencer is replaceable; benchmark a fenced owner-based sequencer later.

## Important semantics

Use the same serialization boundary for admission and round closure/settlement barrier. Do not equate transaction start time with lock-acquisition time; PostgreSQL now() is transaction-start time, so consider clock_timestamp() after lock acquisition. Define handling for duplicate IDs, unknown commit outcomes, and deadline equality.

## Trade-offs

Hot-round row contention and database dependency versus clear auditable ordering. Database admission time can penalize requests delayed in queues. It is a server fairness policy, not proof of client click time.

## Open validation

Benchmark contention and clock behavior; specify exact admission SQL, rejection evidence, late-command policy and failover guarantees.
<!-- END-FILE -->
<!-- BEGIN-FILE docs/adr/0003-game-contract-and-event-semantics.md -->
# ADR-003: Game Contract & Event Semantics

Source: https://linear.app/lbtworks/document/adr-003-game-contract-and-event-semantics-0c918d047ea9

Status: Accepted

## Decision

Use a minimal typed Go Game\[S,C,E\] contract with InitialState(config), Decide(state, command, deterministicInput) -> events, and Evolve(state,event) -> nextState. Decide and Evolve are pure and deterministic: no I/O, uncontrolled randomness, wall-clock access, or mutation of input state. The runtime owns admission, sequencing, persistence, ownership and transport. Use a versioned type-erased adapter at the runtime boundary, not a general plugin framework.

## Replay

State replay: initial state + ordered events through Evolve. Decision verification: historical state + original command + recorded deterministic inputs through Decide, compare canonical events. Preserve rulesVersion, schemaVersion, stateVersion separately. The public projection must not expose secret crash outcomes.

## Crash v1 domain

Shared multiplayer round aggregate; commands include PlaceBet, StartRound, CashOut, CloseRound, SettleRound; lifecycle commands require system authority. Events include BetPlaced, RoundStarted, BetCashedOut, RoundCrashed, BetLost and RoundSettled. Invalid commands produce durable terminal command results without necessarily emitting domain events.

## Invariants

One bet per player per round; no double cash-out; no cash-out at/after crash; no transitions after settlement; deterministic canonical state hash; validated event applicability.

## Consequences

Historical game adapters and canonical serializers must remain available for audit replay. Avoid premature abstractions for hypothetical games.
<!-- END-FILE -->
<!-- BEGIN-FILE docs/adr/0004-multiplayer-crash-game-specification.md -->
# ADR-004: Multiplayer Crash Game Specification

Source: https://linear.app/lbtworks/document/adr-004-multiplayer-crash-game-specification-b93b4142286c

Status: Accepted (implementation constants pending)

## Decision

Implement a shared multiplayer Crash game with phases BETTING, RUNNING, CRASHED, SETTLED. The outcome is fixed before betting opens. Use exponential multiplier growth with [github.com/cockroachdb/apd/v3](<http://github.com/cockroachdb/apd/v3>) decimal arithmetic, pinned dependency version, explicit precision and rounding, golden vectors, checked conversions and integer simulated credits. Cash-out timing uses the durable admission time from ADR-002; equality with crash deadline loses. Payout = floor(betCredits \* scaledMultiplier / scale) using checked arithmetic.

## Fairness

Generate a cryptographically random 256-bit server seed; publish a SHA-256 commitment before betting; derive a versioned crash outcome from seed and round identity; reveal after settlement for independent verification. Commitment/reveal proves consistency with the committed seed, not impartial seed selection by itself.

## Lifecycle and settlement

Lifecycle commands are trusted-only. Crash is determined by the committed deadline even if processing is delayed. Settlement waits for the durable admission barrier and resolves all earlier eligible commands. Use balanced simulated-credit ledger postings; no real-money payments.

## Open implementation parameters

Freeze exponential coefficient, time quantum, multiplier scale, apd context, rounding, max duration, crash-point probability distribution, exact seed derivation and canonical encoding before first production rule-version release. Store authoritative numeric outputs in events; retain historical rule artifacts.

## Exclusions

No real-money flows, KYC, regulatory certification, jackpots, multi-currency or cross-region active-active.
<!-- END-FILE -->
<!-- BEGIN-FILE docs/adr/0005-postgresql-event-store-and-transaction-model.md -->
# ADR-005: PostgreSQL Event Store & Transaction Model

Source: https://linear.app/lbtworks/document/adr-005-postgresql-event-store-and-transaction-model-d2b62c7cf5e2

Status: Accepted

## Decision

PostgreSQL is the authoritative system of record for rounds, immutable command admissions, terminal command results, append-only domain events, simulated-credit double-entry ledger, outbox, and eventual snapshots. It is not necessarily the permanent sequencing mechanism.

## Transaction A: admission

Serialize admission per round; check command ID deduplication and admission window; assign sequence and effective timestamp; insert immutable admission; commit before acknowledging admitted status. The round closure/barrier must coordinate through the same serialization boundary.

## Transaction B: execution

Read next admitted command and known state version; Decide and Evolve deterministically; atomically validate expected version, next processed admission sequence and ownership fencing token, then append events, one terminal result, balanced ledger postings, outbox records, and advance processed/state versions. On conflict discard computed transition and retry from authoritative state.

## Failure semantics

Before admission commit: retry may create admission. After admission commit: retry returns original admission. Before execution commit: no partial outcome. After execution commit but before response: return existing durable result. Stale owner cannot commit. Outbox delivers at least once; consumers deduplicate.

## Cross-round balances

Protect shared player accounts with account-level transactional debit/reservation checks, not only round serialization. Settlement barriers must prevent finalization while earlier admitted commands remain unresolved.

## Trade-offs

Two durable transaction boundaries and per-round lock contention; benchmark owner sequencing later. ACID alone is not a backup/replication/retention policy.

## Open validation

Exact SQL schema, isolation, deferred ledger balance enforcement, immutable audit storage, outbox retry and backup/restore tests.
<!-- END-FILE -->
<!-- BEGIN-FILE docs/adr/0006-correctness-and-verification-strategy.md -->
# ADR-006: Correctness & Verification Strategy

Source: https://linear.app/lbtworks/document/adr-006-correctness-and-verification-strategy-83ecdddbbeeb

Status: Accepted

## Decision

Define executable game, financial, distributed and audit invariants. Implement layered verification: pure unit tests, Go fuzz/property tests, golden arithmetic and replay vectors, independent audit CLI, real PostgreSQL integration tests using Testcontainers for Go, model-based concurrency testing, deterministic failpoints, process/Kubernetes chaos, and workload-aware load tests.

## Audit verifier

Validate canonical event hash chain, state replay hash, historical decision reproduction, balanced ledger, and seed commitment/reveal. Fail closed on unknown historical schema/rules artifacts. Keep visual replay separate from cryptographic verification.

## Critical failure tests

Duplicate admission, conflicting per-round commits, double cash-out, cross-round overspending, rollback on ledger failure, ambiguous commit response, stale fencing token, crash before/after commit, settlement barrier, pod termination and recovery.

## Performance experiments

Compare many independent rounds, one hot round, cash-out burst near crash, retry storms, and pod failure under load. Measure admission and execution p50/p95/p99, backlog, lock waits, WAL and connection pool saturation. Targets (10k rounds, 5k aggregate commands/s, p99 <100ms, zero observed invariant violations) are hypotheses requiring specified hardware/workload, not promises.

## CI

PR: unit, golden, fuzz smoke, PostgreSQL integration, race/static checks. Nightly/release: extended fuzz, multi-replica chaos, load and replay verification. No distributed feature is complete without a reproducible failure test. Testing alone is not formal proof; bounded model checking/TLA+ is a possible later experiment.
<!-- END-FILE -->
<!-- BEGIN-FILE docs/adr/0007-repository-architecture-and-build-strategy.md -->
# ADR-007: Repository Architecture & Build Strategy

Source: https://linear.app/lbtworks/document/adr-007-repository-architecture-and-build-strategy-8612450e0996

Status: Accepted
Date: 2026-10-14

## Context

Orvex consists of a deterministic Go game core, separately deployable Go gateway and engine services, a replay/audit CLI, shared gRPC protocol definitions, a React/Vite console, PostgreSQL migrations, integration/load/chaos tests, and architectural documentation. Audit replay requires reproducible historical rules and traceable builds.

## Decision

Use a **Go-first monorepo** in a single GitHub repository, with **one Go module** initially. Go `internal/` packages define code boundaries; independent binaries deploy separately. Use React + Vite + TypeScript with pnpm for the frontend. Use a Makefile for orchestration, Buf for protobuf, Docker Compose for local dependencies, GitHub Actions for CI, multi-stage Dockerfiles, and initially Kustomize for Kubernetes. Do not introduce Turborepo, Nx, multiple Go modules, or a Go workspace without demonstrated need.

## Proposed layout

```text
orvex/
  cmd/{gateway,engine,crashctl}/
  internal/game/{contract.go,registry.go,crash/}/
  internal/runtime/{admission,execution,ownership,sessions}/
  internal/persistence/{postgres,eventstore}/
  internal/audit/{replay,verification}/
  internal/transport/{grpc,http,websocket}/
  proto/orvex/v1/
  web/
  migrations/
  tests/{integration,load,chaos}/
  deployments/{docker,kubernetes}/
  docs/{adr,architecture,experiments}/
  AGENTS.md
  go.mod
  Makefile
  README.md
```

The layout is a starting convention, not an obligation to create empty directories.

## Reproducibility and historical rules

After the first stable rules release, retain immutable versioned Crash rules implementations (for example `internal/game/crash/v1/`), golden vectors, pinned dependency versions including `cockroachdb/apd/v3`, arithmetic configuration, canonical serialization definitions, and reproducible build identifiers. The audit verifier selects the historical rules version rather than interpreting all events with the latest implementation. One repository commit provides a useful platform-wide source reference, but immutable release artifacts and dependency provenance remain necessary for strong reproducibility.

## CI and commands

Provide `make test`, `make test-integration`, `make lint`, `make proto`, `make dev`, `make build`, and `make audit` as they become meaningful. Use path-aware CI for fast checks: Go domain/integration, frontend typecheck/UI, protobuf contract generation, migration integration. Historical rules or replay changes require full compatibility checks; releases run the complete correctness suite including distributed failure tests.

## Trade-offs

Pros: atomic cross-service/protocol changes, shared tests and ADRs, simpler onboarding, single commit provenance, no inter-repo release coordination. Costs: larger repository, path-aware CI complexity, potential coupling, and need to protect historical rules from accidental modification. Monorepo does not imply monolithic deployment.

## Consequences / follow-up

Mirror ADR-001–007 in `docs/adr/` after the repository exists. Put agent guidance in `AGENTS.md`, requiring review of relevant ADRs before changes and explicit flagging of conflicts. [LBT-5](https://linear.app/lbtworks/issue/LBT-5/scaffold-go-module-and-repository-conventions) is the initial implementation issue. Extract separate modules/repositories only if real independent release or reuse requirements arise.
<!-- END-FILE -->
````

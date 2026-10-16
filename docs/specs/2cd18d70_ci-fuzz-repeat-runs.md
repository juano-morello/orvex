# Plan: LBT-20, scheduled fuzzing and repeated concurrency runs in CI

Run `2cd18d70`. Issue: [LBT-20](https://linear.app/lbtworks/issue/LBT-20/ci-scheduled-fuzzing-and-repeated-concurrency-runs).
Base commit: `cd78841`. Governing ADRs: ADR-006 (CI section: "PR: unit, golden, fuzz smoke, PostgreSQL
integration, race/static checks. Nightly/release: extended fuzz ..."), ADR-007 (Makefile orchestration,
GitHub Actions, single module, minimal pinned dependencies).

## 0. Facts the plan relies on (verified while planning)

- **Fuzz targets: 11.** `grep -rEo --include='*_test.go' '^func Fuzz[A-Za-z0-9_]+' cmd internal` finds:
  - `internal/canonjson`: `FuzzUnmarshal`, `FuzzStringRoundTrip`
  - `internal/game`: `FuzzSampleGamePurity`
  - `internal/game/crash/v1`: `FuzzPayout`, `FuzzCrashPointFromR`, `FuzzCashOutTiming`,
    `FuzzDecodeCommand`, `FuzzDecodeEvent`, `FuzzRoundLifecycle`
  - `internal/audit/verification`: `FuzzSingleByteFlip`, `FuzzSingleByteFlipV2`
  - None is in a build-tagged file. No `TestMain` exists in these four packages (the `pgtest.Run`
    `TestMain`s are all in `//go:build integration` files), so a fuzz run starts no container.
  - Plain `make test` already runs every seed corpus: that is ADR-006's PR "fuzz smoke". PR CI is
    not changed.
- **`go test -fuzz` with a name that matches nothing exits 0** (`testing: warning: no fuzz tests to
  fuzz`). Checked locally. The Make target must therefore refuse unknown names itself (§2).
- **`-timeout` does not cut fuzzing.** `testing.M.Run` stops the alarm before `runFuzzing`, so a
  `-fuzztime` above 10m is fine. It does apply to a normal `-count=N` run, per package binary, with a
  default of 10m.
- **Spec repeat runs**, copied exactly:
  - LBT-11 S-14 (`docs/specs/5a3d52fc_admission-sequencer.md` §verification):
    `go test -race -tags integration -count=5 -run 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/`
  - LBT-12 E-17 (`docs/specs/e0df34d3_execution-commit-ledger-outbox.md`):
    `go test -race -tags integration -count=5 -run 'Conflicting|StaleCache|CrossRound|Takeover|Crash' ./internal/runtime/execution/`
  - LBT-19 (`docs/specs/73dbe98d_audit-export-cli.md`, `docs/specs/2105c14c_audit-export-implementation.md`):
    `go test -race -tags integration -count=3 -run 'TestExport' ./cmd/crashctl/ ./internal/persistence/eventstore/`
  - Spec fuzz runs: `-fuzz=FuzzRoundLifecycle -fuzztime=30s` with `-race`
    (`docs/specs/712614a2_crash-lifecycle-rules.md`), `FuzzSampleGamePurity` 15s
    (`9d7bead2_game-contract-registry.md`), `FuzzSingleByteFlipV2` 30s
    (`f4d371a8_audit-ledger-implementation.md`).
- **Measured at the base commit** on the 32-core planning host, sequential runs, all exit 0: admission
  `-count=5` 56 s, execution `-count=5` 165 s, export `-count=3` 20 s. On a 4-vCPU GitHub runner with
  `-race`, execution can come close to the 10m default timeout, so the repeat targets set
  `-timeout 60m` explicitly (§2).
- **Action SHAs**, resolved with `git ls-remote --tags` and confirmed as commits through
  `api.github.com/repos/<repo>/commits/<sha>`:

  | Action | Version | Commit SHA | Note |
  |---|---|---|---|
  | `actions/checkout` | v7.0.1 | `3d3c42e5aac5ba805825da76410c181273ba90b1` | lightweight tag, same commit as `v7` |
  | `actions/setup-go` | v7.0.0 | `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e` | lightweight tag, same commit as `v7` |
  | `golangci/golangci-lint-action` | v9.3.0 | `ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a` | **annotated** tag: this is the peeled `v9.3.0^{}` commit. Never use the tag object `d583c34f…`. Same commit as `v9^{}` |
  | `actions/upload-artifact` | v7.0.2 | `cf430e030ddbb5b0abf93d22962f4752f3646cd9` | lightweight tag, same commit as `v7`; inputs `name`, `path`, `if-no-files-found`, `retention-days` exist |

  `ci.yml` uses `@v7`, `@v7` and `@v9` today. These pins are the commits those tags point to now, so
  CI behaves the same.
- **Linter findings.** `golangci-lint run --enable gosec,errorlint --max-issues-per-linter 0 --max-same-issues 0 ./...`
  (v2.14.0, with the repo's `build-tags: [integration]`) reports **96 issues: gosec 92, errorlint 4**.
  - The default output limits (`max-same-issues: 3`, `max-issues-per-linter: 50`) hide some of
    them. Always triage with both flags set to 0.
  - §5 gives the decision for every issue. All 92 gosec issues are false positives: values bounded by
    a schema CHECK, by config validation or by the chain stage, test fixture paths, backoff jitter, and
    test constants. 3 of the errorlint issues are real and get fixed. 1 (a sentinel identity comparison
    in a test) is a false positive.

## 1. Files to change (complete list)

New:
- `.github/workflows/nightly.yml`

Edited (CI, build, docs):
- `.github/workflows/ci.yml`: SHA pins only
- `Makefile`
- `.golangci.yml`
- `README.md`
- `AGENTS.md`

Edited (errorlint fixes, with new tests):
- `internal/game/crash/v1/curve.go`
- `internal/game/crash/v1/params.go`
- `internal/game/crash/v1/params_test.go` (new test function; one existing file)
- `internal/persistence/postgres/migrate.go`
- `internal/persistence/postgres/migrate_test.go` (new test function, plus one nolint)

Edited (inline `//nolint` comments only, exactly the lines in §5.3):
- `cmd/crashctl/export.go`, `cmd/crashctl/main.go`
- `cmd/crashctl/export_helpers_integration_test.go`, `cmd/crashctl/export_integration_test.go`,
  `cmd/crashctl/export_test.go`, `cmd/crashctl/main_test.go`, `cmd/crashctl/migrate_test.go`
- `internal/audit/verification/verification.go`, `internal/audit/verification/ledger_test.go`,
  `internal/audit/verification/verification_test.go`
- `internal/game/crash/v1/codec_test.go`, `internal/game/crash/v1/golden_test.go`,
  `internal/game/crash/v1/round_fuzz_test.go`
- `internal/ledger/ids_test.go`
- `internal/persistence/eventstore/export.go`, `internal/persistence/eventstore/export_v2.go`,
  `internal/persistence/eventstore/ledger.go`, `internal/persistence/eventstore/export_integration_test.go`,
  `internal/persistence/eventstore/export_test.go`, `internal/persistence/eventstore/ledger_test.go`
- `internal/persistence/postgres/pgtest/load.go`, `internal/persistence/postgres/migrate_integration_test.go`,
  `internal/persistence/postgres/schema_integration_test.go`
- `internal/runtime/admission/retry.go`, `internal/runtime/admission/sequencer.go`,
  `internal/runtime/admission/admit_integration_test.go`, `internal/runtime/admission/barrier_integration_test.go`,
  `internal/runtime/admission/commit_integration_test.go`, `internal/runtime/admission/concurrency_integration_test.go`,
  `internal/runtime/admission/gate_test.go`, `internal/runtime/admission/helpers_integration_test.go`
- `internal/runtime/execution/commit.go`, `internal/runtime/execution/executor.go`,
  `internal/runtime/execution/load.go`, `internal/runtime/execution/retry.go`,
  `internal/runtime/execution/transition.go`, `internal/runtime/execution/concurrency_integration_test.go`,
  `internal/runtime/execution/crashv1_test.go`, `internal/runtime/execution/errors_test.go`,
  `internal/runtime/execution/execute_integration_test.go`, `internal/runtime/execution/helpers_integration_test.go`,
  `internal/runtime/execution/ledger_audit_integration_test.go`

Plus this plan's copy, `docs/specs/2cd18d70_ci-fuzz-repeat-runs.md`, which is already written.

Must NOT change: `go.mod`, `go.sum`, `migrations/`, `docs/adr/`, anything under any `testdata/`, any
existing test assertion, crash/v1 arithmetic or codec bytes, any job or trigger in `ci.yml` other than
the `uses:` pins.

## 2. Step 1: `Makefile`

Keep everything that exists. Add the variables after the `GOLANGCI_LINT_VERSION` block. Add the
targets to `.PHONY`, put the repeat targets right after `test-integration` and the fuzz targets right
after `golden-check`. Recipe lines start with a TAB.

Variables:

```make
# Fuzzing (ADR-006: nightly extended fuzz). go test -fuzz fuzzes one target
# per invocation, so every target runs alone. FUZZ_TARGETS holds each one as
# <package dir>:<FuzzName>, found in the _test.go files under cmd/ and
# internal/ (fuzz targets live in files without build tags).
FUZZTIME ?= 30s
FUZZ_TARGETS = $(shell grep -rEo --include='*_test.go' '^func Fuzz[A-Za-z0-9_]+' cmd internal | sed -E 's|/[^/]+_test\.go:func |:|' | LC_ALL=C sort)

# Repeat runs the specs require as flakiness checks, against PostgreSQL
# (Testcontainers, needs a Docker daemon): LBT-11 S-14, LBT-12 E-17, LBT-19.
# go test's default 10m timeout applies per package binary; -race on a small
# CI runner can approach it.
REPEAT_TIMEOUT ?= 60m
```

`.PHONY` line becomes:

```make
.PHONY: all build $(BUILD_TARGETS) test test-integration test-repeat test-repeat-admission test-repeat-execution test-repeat-export lint golden-check fuzz fuzz-one fuzz-list clean
```

Repeat targets (after `test-integration`):

```make
test-repeat: test-repeat-admission test-repeat-execution test-repeat-export

test-repeat-admission:
	$(GO) test $(GOTESTFLAGS) -tags integration -count=5 -timeout $(REPEAT_TIMEOUT) -run 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/

test-repeat-execution:
	$(GO) test $(GOTESTFLAGS) -tags integration -count=5 -timeout $(REPEAT_TIMEOUT) -run 'Conflicting|StaleCache|CrossRound|Takeover|Crash' ./internal/runtime/execution/

test-repeat-export:
	$(GO) test $(GOTESTFLAGS) -tags integration -count=3 -timeout $(REPEAT_TIMEOUT) -run 'TestExport' ./cmd/crashctl/ ./internal/persistence/eventstore/
```

Fuzz targets (after `golden-check`):

```make
fuzz:
	$(if $(FUZZ_TARGETS),,$(error no fuzz targets found under cmd/ or internal/))
	@failed=; for t in $(FUZZ_TARGETS); do \
		$(MAKE) --no-print-directory fuzz-one FUZZ_PKG="$${t%%:*}" FUZZ_TARGET="$${t##*:}" || failed="$$failed $$t"; \
	done; \
	if [ -n "$$failed" ]; then echo "fuzz: failing targets:$$failed" >&2; exit 1; fi

fuzz-one:
	$(if $(filter $(FUZZ_PKG):$(FUZZ_TARGET),$(FUZZ_TARGETS)),,$(error unknown fuzz target '$(FUZZ_PKG):$(FUZZ_TARGET)'; run make fuzz-list))
	$(GO) test $(GOTESTFLAGS) -run '^$$' -fuzz '^$(FUZZ_TARGET)$$' -fuzztime $(FUZZTIME) ./$(FUZZ_PKG)

fuzz-list:
	$(if $(FUZZ_TARGETS),,$(error no fuzz targets found under cmd/ or internal/))
	@printf '%s\n' $(FUZZ_TARGETS)
```

Design notes. Do not deviate from them.
- The repeat commands are the spec commands, each with `$(GOTESTFLAGS)` (default `-race`) and
  `-timeout $(REPEAT_TIMEOUT)` added. The `-run` regexes and `-count` values stay exactly as above.
- Fuzz uses `$(GOTESTFLAGS)` too. That matches the repo convention (`GOTESTFLAGS=` on hosts without
  cgo) and the spec's `-race` FuzzRoundLifecycle command.
- `fuzz` runs every target, even after one fails, and exits 1 at the end with the list of failing
  targets. `fuzz-one` refuses a name that is not in `FUZZ_TARGETS` (make exits 2), because `go test`
  would pass silently. Neither `$(error ...)` message may contain a comma: commas split `$(if ...)`
  arguments.
- The planner ran these recipes in a scratch copy (GNU Make 4.4.1, with `GO=echo` and `GO=false`).
  `fuzz-list` printed the 11 targets. `fuzz` ran 11 separate invocations, and with `GO=false` it ran
  all 11 and then exited non-zero. An unknown or empty `FUZZ_PKG:FUZZ_TARGET` exited 2.

## 3. Step 2: new `.github/workflows/nightly.yml`

Write exactly this. Keep the comments.

```yaml
name: Nightly

# Extended verification (ADR-006 "Nightly/release: extended fuzz"): every Go
# fuzz target with a bounded -fuzztime, each in its own job, and the repeat
# runs the specs require (LBT-11 S-14, LBT-12 E-17, LBT-19). PR CI (ci.yml)
# does not run these.
on:
  schedule:
    - cron: "17 3 * * *"
  workflow_dispatch:
    inputs:
      fuzztime:
        description: "go test -fuzztime for each fuzz target"
        type: choice
        options: ["1m", "5m", "15m"]
        default: "5m"

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: false

defaults:
  run:
    shell: bash

env:
  GO_VERSION: "1.26.x"
  FUZZTIME: ${{ inputs.fuzztime || '5m' }}

jobs:
  fuzz-targets:
    runs-on: ubuntu-latest
    timeout-minutes: 5
    outputs:
      targets: ${{ steps.list.outputs.targets }}
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - id: list
        run: |
          targets="$(make -s fuzz-list | jq -R -s -c 'split("\n") | map(select(. != "") | split(":") | {pkg: .[0], fn: .[1], id: ((.[0] | gsub("/"; "-")) + "-" + .[1])})')"
          echo "$targets"
          echo "targets=$targets" >> "$GITHUB_OUTPUT"

  fuzz:
    needs: fuzz-targets
    name: fuzz ${{ matrix.target.fn }} (${{ matrix.target.pkg }})
    runs-on: ubuntu-latest
    timeout-minutes: 30
    strategy:
      fail-fast: false
      matrix:
        target: ${{ fromJSON(needs.fuzz-targets.outputs.targets) }}
    env:
      FUZZ_PKG: ${{ matrix.target.pkg }}
      FUZZ_TARGET: ${{ matrix.target.fn }}
      ARTIFACT: fuzz-crasher-${{ matrix.target.id }}
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version: ${{ env.GO_VERSION }}
          cache-dependency-path: go.mod
      - name: Fuzz
        run: make fuzz-one FUZZ_PKG="$FUZZ_PKG" FUZZ_TARGET="$FUZZ_TARGET" FUZZTIME="$FUZZTIME"
      - name: Upload failing input
        if: failure()
        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
        with:
          name: ${{ env.ARTIFACT }}
          path: ${{ matrix.target.pkg }}/testdata/fuzz/${{ matrix.target.fn }}/
          if-no-files-found: warn
          retention-days: 30
      - name: Summarize the failure
        if: failure()
        run: |
          {
            echo "### ${FUZZ_TARGET} (${FUZZ_PKG}) failed"
            echo ""
            echo "If the fuzzer found a failing input, artifact \`${ARTIFACT}\` holds it."
            echo "Copy it into \`${FUZZ_PKG}/testdata/fuzz/${FUZZ_TARGET}/\` and reproduce with"
            echo "\`go test -run=${FUZZ_TARGET}/<file> ./${FUZZ_PKG}\`."
          } >> "$GITHUB_STEP_SUMMARY"

  repeat:
    name: repeat ${{ matrix.suite }}
    runs-on: ubuntu-latest
    timeout-minutes: 75
    strategy:
      fail-fast: false
      matrix:
        suite: [admission, execution, export]
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version: ${{ env.GO_VERSION }}
          cache-dependency-path: go.mod
      - run: make test-repeat-${{ matrix.suite }}
```

Why it is shaped this way:
- One job per fuzz target. A red run names the failing target, the targets run in parallel, and each
  one gets its own crasher artifact. The matrix comes from `make fuzz-list`, so the Makefile is the
  only list, and a new `Fuzz*` function is picked up with no workflow edit. An empty list makes
  `fromJSON` produce an empty matrix, which GitHub rejects, so that failure is visible too.
- `fail-fast: false`: one failure never cancels the other targets or suites.
- Bounded time. The default `-fuzztime` is 5m per target. A manual run can only choose 1m, 5m or 15m,
  and `timeout-minutes: 30` covers 15m plus the `-race` compile. Each repeat suite has `-timeout 60m`
  inside a 75-minute job.
- Failures are visible. The job fails, `go test` prints the failing input path and the `-run=` line,
  the upload step attaches `<pkg>/testdata/fuzz/<FuzzName>/`, and the summary step writes the
  reproduce instructions. GitHub notifies the owner of a scheduled workflow when it fails.
- Values from `matrix` reach `run:` through `env`. The repeat suite names are literals in the file.
- `ubuntu-latest` comes with Docker (Testcontainers), `make`, `grep`, `sed` and `jq`. The job that
  builds the list needs no Go.

## 4. Step 3: pin every action in `.github/workflows/ci.yml`

Replace only the `uses:` lines. Leave triggers, jobs, steps and `env` as they are:
- `uses: actions/checkout@v7` (4×) → `uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1`
- `uses: actions/setup-go@v7` (4×) → `uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0`
- `uses: golangci/golangci-lint-action@v9` (1×) → `uses: golangci/golangci-lint-action@ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a # v9.3.0`

Before you write them, re-resolve the SHAs:

```sh
git ls-remote --tags https://github.com/actions/checkout 'refs/tags/v7.0.1*'
git ls-remote --tags https://github.com/actions/setup-go 'refs/tags/v7.0.0*'
git ls-remote --tags https://github.com/golangci/golangci-lint-action 'refs/tags/v9.3.0*'   # use the ^{} line
git ls-remote --tags https://github.com/actions/upload-artifact 'refs/tags/v7.0.2*'
```

If a value differs from §0, stop and report it. Do not pick a different version.

## 5. Step 4: `.golangci.yml`, then every finding

### 5.1 Config

`linters` becomes:

```yaml
linters:
  default: standard
  enable:
    - errorlint
    - gosec
```

No `settings`, no `exclusions`, no presets. Path rules and `common-false-positives` are blanket
exclusions, and the issue forbids them.

Reproduce the baseline before you edit any Go file:

```sh
golangci-lint run --max-issues-per-linter 0 --max-same-issues 0 ./...
```

It must list the 96 issues in §5.2 and §5.3. If the list differs, match issues by file and source
text, not by line number. Triage any issue that is not in the tables under the same rules (a real fix
first, or else a line-level nolint with a specific reason) and name it in the report.

### 5.2 errorlint: 3 fixes, 1 nolint

| Location | Change |
|---|---|
| `internal/game/crash/v1/curve.go:32` | `fmt.Errorf("%w: tick %d: %v", ErrArithmetic, n, err)` → `fmt.Errorf("%w: tick %d: %w", ErrArithmetic, n, err)` |
| `internal/game/crash/v1/params.go:88` (`apdCheck`) | `fmt.Errorf("%w: %s: %v", ErrArithmetic, op, err)` → `fmt.Errorf("%w: %s: %w", ErrArithmetic, op, err)` |
| `internal/persistence/postgres/migrate.go:112` | `fmt.Errorf("%w: %s: %s", ErrMigrationFiles, file, err)` → `fmt.Errorf("%w: %s: %w", ErrMigrationFiles, file, err)` |
| `internal/runtime/execution/errors_test.go:26` | do not change the code. Append `//nolint:errorlint // s and ErrFailClosed are table sentinels compared by identity to skip the one err must match` |

The three fixes change no message text, because `%w` prints the same as `%v`/`%s`. What changes is
that `errors.Is` and `errors.As` now reach the cause as well as the sentinel. Nothing in the repository
calls `errors.Unwrap`. The crash/v1 edits touch error wrapping only. No arithmetic, constant, encoded
byte or golden vector changes, and crash/v1 has no release tag yet (§9).

New tests, as new functions. Do not edit any existing test function:
- `internal/game/crash/v1/params_test.go`: add `TestApdCheckWrapsCause`.
  - Set `cause := errors.New("apd failure")` and `err := apdCheck("exp", 0, cause)`.
  - Require `errors.Is(err, ErrArithmetic)` and `errors.Is(err, cause)`.
  - Require `err.Error() == "crash/v1: arithmetic failure: exp: apd failure"`.
  - The `curve.go:32` path (`f.Int64()` failing) cannot be reached through `MultiplierAtTick`, because
    `n` is bounded by `MaxTick`. It has no test. Say so in the report.
- `internal/persistence/postgres/migrate_test.go`: add `TestLoadMigrationsWrapsContentError`.
  - Set `_, err := LoadMigrations(with(validSet(), map[string]*fstest.MapFile{"0002_second.up.sql": file("")}))`.
  - Require `errors.Is(err, ErrMigrationFiles)`.
  - Require `err.Error() == "postgres: invalid migration files: 0002_second.up.sql: empty file"`.
  - Require that the content error is in the chain. Use
    `var multi interface{ Unwrap() []error }` and `errors.As(err, &multi)`, then check
    `len(multi.Unwrap()) == 2` and `multi.Unwrap()[1].Error() == "empty file"`. Use `errors.As`, not a
    type assertion: errorlint flags type assertions on errors.

### 5.3 gosec: 92 line-level nolints

How to apply them:
- Append a trailing comment to the reported line: `//nolint:gosec // <text>`. Use exactly that prefix
  (no space after `//`). Put it on the line in the table, which is the one golangci-lint reports. It
  may be the continuation line of a multi-line call. Never put a nolint on a function, block or file,
  and never use `#nosec`.
- After you edit a file, run `gofmt -w <file>`. Trailing comments in aligned key-value blocks (for
  example `sequencer.go:142-143`) get realigned, and `make lint` checks gofmt. Only the lines you
  touched and their alignment group may change.
- The planner checked this in a scratch copy: a trailing nolint on a continuation line suppresses the
  issue, and the aligned block needs gofmt.

Production code (31):

| # | Location | Rule | Comment text after `//nolint:gosec // ` |
|---|---|---|---|
| P1 | `cmd/crashctl/export.go:211` | G304 | `G304: FILE is the operator's own output path; O_EXCL and 0o600 never overwrite or widen access` |
| P2 | `cmd/crashctl/main.go:151` | G304 | `G304: FILE is the operator's own input path; audit verify, replay and chain only read it` |
| P3 | `internal/audit/verification/verification.go:494` | G115 | `G115: last = len(h.records)-1 >= 0` |
| P4 | `internal/audit/verification/verification.go:498` | G115 | `G115: last = len(h.records)-1 >= 0` |
| P5 | `internal/audit/verification/verification.go:567` | G115 | `G115: the chain stage checked seq == its record index < len(h.records)` |
| P6 | `internal/audit/verification/verification.go:571` | G115 | same text as P5 |
| P7 | `internal/audit/verification/verification.go:574` | G115 | same text as P5 |
| P8 | `internal/audit/verification/verification.go:591` | G115 | `G115: m.Sequence is a record sequence the chain stage checked against its index < len(h.records)` |
| P9 | `internal/persistence/eventstore/export.go:120` | G115 | `G115: last is a snapshot sequence scanned from bigint and checked >= 0 above` |
| P10 | `internal/persistence/eventstore/export.go:140` | G115 | same text as P9 |
| P11 | `internal/persistence/eventstore/export.go:154` | G115 | same text as P9 |
| P12 | `internal/persistence/eventstore/export_v2.go:64` | G115 | `G115: n is one round's record count (last + 1), as in ExportHistory's int(n)` |
| P13 | `internal/persistence/eventstore/export_v2.go:115` | G115 | `G115: last is a snapshot sequence scanned from bigint and checked >= 0 above` |
| P14 | `internal/persistence/eventstore/export_v2.go:134` | G115 | `G115: last is the newest snapshot sequence, a bigint checked >= 0 by readExportBase` |
| P15 | `internal/persistence/eventstore/ledger.go:69` | G115 | same text as P14 |
| P16 | `internal/persistence/postgres/pgtest/load.go:107` | G115 | `G115: a golden history sequence (< 100); a wrapped value would fail the column CHECK` |
| P17 | `internal/persistence/postgres/pgtest/load.go:144` | G115 | same text as P16 |
| P18 | `internal/persistence/postgres/pgtest/load.go:153` | G115 | same text as P16 |
| P19 | `internal/runtime/admission/sequencer.go:142` | G115 | `G115: cfg.validate() bounds LockTimeout to at most 60s (60000 ms)` |
| P20 | `internal/runtime/admission/sequencer.go:143` | G115 | `G115: cfg.validate() bounds IdleInTransactionTimeout to at most 60s (60000 ms)` |
| P21 | `internal/runtime/admission/retry.go:150` | G404 | `G404: backoff jitter needs no unpredictability; crypto/rand adds nothing` |
| P22 | `internal/runtime/execution/commit.go:65` | G115 | `G115: tr.sequence = processed_sequence + 1, a bigint round sequence` |
| P23 | `internal/runtime/execution/commit.go:95` | G115 | `G115: event sequences are bigint domain_events.event_sequence values` |
| P24 | `internal/runtime/execution/executor.go:480` | G115 | `G115: k = processed_sequence + 1, a bigint round sequence` |
| P25 | `internal/runtime/execution/load.go:56` | G115 | `G115: rounds_versions_check holds rules_version in [1, 4294967295]` |
| P26 | `internal/runtime/execution/load.go:110` | G115 | `G115: round_snapshots_sequence_check holds sequence >= 0` |
| P27 | `internal/runtime/execution/load.go:224` | G115 | `G115: k is an admission sequence, a bigint admissions.sequence value` |
| P28 | `internal/runtime/execution/retry.go:163` | G115 | `G115: k = processed_sequence + 1, a bigint round sequence` |
| P29 | `internal/runtime/execution/retry.go:165` | G115 | same text as P28 |
| P30 | `internal/runtime/execution/retry.go:197` | G404 | `G404: backoff jitter needs no unpredictability; crypto/rand adds nothing` |
| P31 | `internal/runtime/execution/transition.go:131` | G115 | `G115: seq is an event sequence stored in bigint domain_events.event_sequence` |

Why no code change for these. Every value is bounded by an invariant gosec cannot see: a schema CHECK
(`migrations/0002`–`0004`), `Config.validate()`, or `checkChain`'s `seq == index` rule. A guard would
be dead code, its error branch could not be tested, and it would mean changing concurrency-critical
code. G304 on the CLI is the command's purpose. G404 is jitter.

Test code (61):

| # | Location | Rule | Comment text after `//nolint:gosec // ` |
|---|---|---|---|
| T1 | `cmd/crashctl/export_helpers_integration_test.go:140` | G115 | `G115: seq is admitted_sequence (CHECK >= 0) + 1` |
| T2 | `cmd/crashctl/export_integration_test.go:178` | G304 | `G304: path is a file this test created under t.TempDir()` |
| T3 | `cmd/crashctl/export_integration_test.go:279` | G304 | same text as T2 |
| T4 | `cmd/crashctl/export_test.go:173` | G304 | same text as T2 |
| T5 | `cmd/crashctl/export_test.go:304` | G304 | `G304: fixed golden fixture under testdata` |
| T6 | `cmd/crashctl/main_test.go:261` | G703 | `G703: path is under t.TempDir()` |
| T7 | `cmd/crashctl/main_test.go:319` | G304 | `G304: fixed golden fixture under testdata` |
| T8 | `cmd/crashctl/main_test.go:487` | G703 | `G703: path is under t.TempDir()` |
| T9 | `cmd/crashctl/migrate_test.go:18` | G101 | `G101: dummy credentials for a port nothing listens on, not a secret` |
| T10 | `internal/audit/verification/ledger_test.go:75` | G304 | `G304: fixed golden fixture under testdata` |
| T11 | `internal/audit/verification/verification_test.go:77` | G304 | same text as T10 |
| T12 | `internal/game/crash/v1/codec_test.go:49` | G101 | `G101: public seed of the spec's codec test vector, not a credential` |
| T13 | `internal/game/crash/v1/golden_test.go:156` | G304 | `G304: fixed golden fixture under testdata` |
| T14 | `internal/game/crash/v1/golden_test.go:248` | G304 | `G304: fixed repository file at the module root` |
| T15 | `internal/game/crash/v1/round_fuzz_test.go:15` | G115 | `G115: callers pass op kinds 0 to 5` |
| T16 | `internal/game/crash/v1/round_fuzz_test.go:18` | G115 | `G115: (kind+4)%6 is in [0, 5]` |
| T17 | `internal/ledger/ids_test.go:44` | G404 | `G404: a seeded PCG keeps the test inputs reproducible; no secret is derived` |
| T18 | `internal/persistence/eventstore/export_integration_test.go:67` | G304 | `G304: fixed golden fixture under testdata` |
| T19 | `internal/persistence/eventstore/export_integration_test.go:264` | G304 | same text as T18 |
| T20 | `internal/persistence/eventstore/export_test.go:24` | G304 | same text as T18 |
| T21 | `internal/persistence/eventstore/ledger_test.go:133` | G304 | same text as T18 |
| T22 | `internal/persistence/eventstore/ledger_test.go:89` | G115 | `G115: a sequence from a golden v2 fixture (< 100)` |
| T23 | `internal/persistence/eventstore/ledger_test.go:96` | G115 | same text as T22 |
| T24 | `internal/persistence/eventstore/ledger_test.go:104` | G115 | same text as T22 |
| T25 | `internal/persistence/eventstore/ledger_test.go:109` | G115 | `G115: a posting index from a golden v2 fixture (< 100)` |
| T26 | `internal/persistence/eventstore/ledger_test.go:114` | G115 | `G115: an event sequence from a golden v2 fixture (< 100)` |
| T27 | `internal/persistence/postgres/migrate_integration_test.go:31` | G304 | `G304: fixed golden fixture under testdata` |
| T28 | `internal/persistence/postgres/migrate_integration_test.go:124` | G602 | `G602: len(a) == len(b) is checked above` |
| T29 | `internal/persistence/postgres/migrate_test.go:166` | G115 | `G115: deliberate two's-complement reinterpretation, the MigrationLockKey derivation` |
| T30 | `internal/persistence/postgres/schema_integration_test.go:158` | G115 | `G115: deliberate truncation to the low 16 bits of a test sequence` |
| T31 | `internal/persistence/postgres/schema_integration_test.go:205` | G115 | `G115: a test round's sequence, far below math.MaxInt64` |
| T32 | `internal/persistence/postgres/schema_integration_test.go:226` | G115 | same text as T31 |
| T33 | `internal/persistence/postgres/schema_integration_test.go:242` | G115 | same text as T31 |
| T34 | `internal/persistence/postgres/schema_integration_test.go:261` | G115 | same text as T31 |
| T35 | `internal/persistence/postgres/schema_integration_test.go:264` | G115 | same text as T31 |
| T36 | `internal/persistence/postgres/schema_integration_test.go:450` | G115 | `G115: ic.Records is a golden history's record count (< 100)` |
| T37 | `internal/runtime/admission/admit_integration_test.go:142` | G115 | `G115: a test round's sequence, far below math.MaxInt64` |
| T38 | `internal/runtime/admission/admit_integration_test.go:214` | G115 | same text as T37 |
| T39 | `internal/runtime/admission/admit_integration_test.go:218` | G115 | same text as T37 |
| T40 | `internal/runtime/admission/barrier_integration_test.go:96` | G115 | same text as T37 |
| T41 | `internal/runtime/admission/barrier_integration_test.go:99` | G115 | same text as T37 |
| T42 | `internal/runtime/admission/barrier_integration_test.go:298` | G115 | same text as T37 |
| T43 | `internal/runtime/admission/barrier_integration_test.go:383` | G115 | same text as T37 |
| T44 | `internal/runtime/admission/barrier_integration_test.go:386` | G115 | same text as T37 |
| T45 | `internal/runtime/admission/barrier_integration_test.go:389` | G115 | `G115: c is a test round's close sequence, far below math.MaxInt` |
| T46 | `internal/runtime/admission/barrier_integration_test.go:398` | G115 | same text as T37 |
| T47 | `internal/runtime/admission/barrier_integration_test.go:475` | G115 | `G115: r.sequence is admissions.sequence (CHECK >= 1)` |
| T48 | `internal/runtime/admission/commit_integration_test.go:173` | G115 | same text as T37 |
| T49 | `internal/runtime/admission/concurrency_integration_test.go:116` | G115 | `G115: k is a non-negative trial index` |
| T50 | `internal/runtime/admission/gate_test.go:144` | G115 | `G115: i%10 is in [0, 9]` |
| T51 | `internal/runtime/admission/helpers_integration_test.go:120` | G115 | `G115: n is a non-negative test command number` |
| T52 | `internal/runtime/admission/helpers_integration_test.go:365` | G115 | `G115: seq is admitted_sequence (CHECK >= 0) + 1` |
| T53 | `internal/runtime/admission/helpers_integration_test.go:418` | G115 | same text as T37 |
| T54 | `internal/runtime/execution/concurrency_integration_test.go:249` | G115 | `G115: seq is chain_records.sequence (CHECK >= 0)` |
| T55 | `internal/runtime/execution/crashv1_test.go:28` | G304 | `G304: fixed golden fixture under testdata` |
| T56 | `internal/runtime/execution/execute_integration_test.go:151` | G115 | `G115: seq is a ledger_transactions.sequence of this test round (>= 1)` |
| T57 | `internal/runtime/execution/execute_integration_test.go:351` | G115 | `G115: c.Sequence is a golden history sequence (< 100)` |
| T58 | `internal/runtime/execution/helpers_integration_test.go:91` | G115 | `G115: n is a non-negative test command number` |
| T59 | `internal/runtime/execution/helpers_integration_test.go:272` | G115 | `G115: seq is admitted_sequence (CHECK >= 0) + 1` |
| T60 | `internal/runtime/execution/helpers_integration_test.go:308` | G304 | `G304: fixed golden fixture under testdata` |
| T61 | `internal/runtime/execution/ledger_audit_integration_test.go:25` | G304 | same text as T60 |

"same text as X" means you copy X's comment text verbatim. After these edits,
`golangci-lint run --max-issues-per-linter 0 --max-same-issues 0 ./...` must print `0 issues.`

Rebase onto main after LBT-23 and LBT-24. LBT-24 moved the golden-round seeding helpers of T1, T59
and T60 into `internal/persistence/postgres/pgtest/roundtest`, so those comments now sit on
`roundtest.SeedAdmission`'s `return uint64(seq)` (T1 and T59, one site) and on `roundtest.ReadGolden`'s
`os.ReadFile(path)` (T60, text `G304: test support; callers pass a golden fixture under testdata`).
LBT-23 added unparsable-DSN cases to `cmd/crashctl/migrate_test.go`; their map literal carries
`//nolint:gosec // G101: dummy credentials in unparsable DSNs, not a secret`.

## 6. Step 5: docs

Each bullet below is written as `` <text> ``. Insert only `<text>`, the part between the outer double
backticks, as one Markdown list line. It keeps its own single backticks.

`AGENTS.md`, `## Commands`:
- Insert after the `make golden-check` bullet:
  - `` - `make fuzz`: run every Go fuzz target for `FUZZTIME` each (default `30s`), one `go test -fuzz` invocation per target, since Go fuzzes one target per run. `make fuzz-list` prints the targets as `<package dir>:<FuzzName>`. `make fuzz-one FUZZ_PKG=internal/game/crash/v1 FUZZ_TARGET=FuzzRoundLifecycle` runs one. A failing input is written to `<package>/testdata/fuzz/<FuzzName>/`; reproduce it with `go test -run=<FuzzName>/<file>` and fix the cause. ``
  - `` - `make test-repeat`: the repeat (flakiness) runs the specs require, against PostgreSQL (needs a Docker daemon): `make test-repeat-admission` (`-count=5`, LBT-11 S-14), `make test-repeat-execution` (`-count=5`, LBT-12 E-17) and `make test-repeat-export` (`-count=3`, LBT-19). ``
  - `` - CI: `.github/workflows/ci.yml` runs on every push and pull request. `.github/workflows/nightly.yml` runs nightly and on manual dispatch: every fuzz target (5m each by default) and the three `make test-repeat-*` suites. Pin every GitHub Action to a full commit SHA, with its version in a trailing comment. ``
- In the closing paragraph, after the `make test-integration` sentence, add: `When you change
  internal/runtime/admission/, internal/runtime/execution/ or the audit export path (cmd/crashctl
  export, internal/persistence/eventstore/), also run the matching make test-repeat-* target.` Use
  backticks on the paths and targets, the way the existing sentence does.
- In the `make test` and `make test-integration` bullets the `GOTESTFLAGS=` note stays as it is. Add
  `` The fuzz and repeat targets take `GOTESTFLAGS` too. `` to the end of the `make test-integration`
  bullet.

`README.md`, `## Development` list:
- Insert after the `make golden-check` bullet:
  - `` - `make fuzz`: fuzz every Go fuzz target for `FUZZTIME` each (default `30s`), one target per `go test` run; `make fuzz-list` lists them and `make fuzz-one FUZZ_PKG=internal/canonjson FUZZ_TARGET=FuzzUnmarshal` runs one ``
  - `` - `make test-repeat`: the specs' repeated PostgreSQL runs (`make test-repeat-admission`, `make test-repeat-execution`, `make test-repeat-export`); needs a Docker daemon ``
  - `` - CI: `.github/workflows/ci.yml` on every push and pull request; `.github/workflows/nightly.yml` nightly and on demand (every fuzz target and the repeat runs) ``

Do not touch the README status line or any other section.

## 7. Tests: what proves each behavior change

| Behavior change | Proof |
|---|---|
| `apdCheck` keeps the apd error in the chain, message unchanged | new `TestApdCheckWrapsCause` (`internal/game/crash/v1/params_test.go`); existing `TestApdCheck` still passes unchanged |
| `curve.go:32` wraps with `%w` | unreachable through `MultiplierAtTick` (tick bounded by `MaxTick`); no test, stated in the report; crash/v1 tests and `make golden-check` show no output change |
| `LoadMigrations` keeps the content error in the chain, message unchanged | new `TestLoadMigrationsWrapsContentError` (`internal/persistence/postgres/migrate_test.go`); existing `TestLoadMigrations` still passes unchanged |
| gosec and errorlint enforced | `make lint` exits 0 with `0 issues.` |
| `make fuzz-list`, `fuzz`, `fuzz-one` | §8 commands 6–9 |
| `make test-repeat*` | §8 commands 10–13 |
| nightly workflow and pins | §8 commands 14–16 |

No existing test function, assertion, fixture or golden file changes.

## 8. Verification

Rules, carried over from LBT-11 and LBT-12:
- Run every command in the foreground, one at a time, to completion. Never run two Docker or test
  commands at the same time.
- Use a tool timeout of at least 1800 s for 2, 12 and 13.
- Judge each command by its exit status alone, and wrap it in `time`.
- The report gives, for every command: the exact command, its exit status and `real` duration.

1. `time make test`: exit 0
2. `time make test-integration`: exit 0 (Docker)
3. `time make lint`: exit 0, prints `0 issues.`
4. `time make build`: exit 0
5. `time make golden-check`: exit 0
6. `time make fuzz-list`: exit 0, prints exactly the 11 lines of §0, sorted
7. `time make fuzz-one FUZZ_PKG=internal/game/crash/v1 FUZZ_TARGET=FuzzRoundLifecycle FUZZTIME=30s`:
   exit 0 (the spec's 30 s run)
8. `time make fuzz FUZZTIME=5s`: exit 0, with 11 separate `go test ... -fuzz '^<Name>$' ...` invocations
   in the output
9. `make fuzz-one FUZZ_PKG=internal/canonjson FUZZ_TARGET=FuzzNope`: exit **2**, prints
   `unknown fuzz target 'internal/canonjson:FuzzNope'`
10. `time make test-repeat-admission`: exit 0 (planner: 56 s)
11. `time make test-repeat-execution`: exit 0 (planner: 165 s)
12. `time make test-repeat-export`: exit 0 (planner: 20 s)
13. `time make test-repeat`: exit 0. It runs 10–12 again as prerequisites.
14. `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 .github/workflows/ci.yml .github/workflows/nightly.yml`:
    exit 0. This is a one-off tool run and must not change `go.mod` or `go.sum`.
15. `grep -hE '^\s*(-\s*)?uses:' .github/workflows/*.yml | grep -vE '@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+$'`:
    prints nothing. grep exits 1 when nothing matches, and that is the pass.
16. `make -s fuzz-list | jq -R -s -c 'split("\n") | map(select(. != "") | split(":") | {pkg: .[0], fn: .[1], id: ((.[0] | gsub("/"; "-")) + "-" + .[1])})'`:
    a JSON array of 11 objects, each `id` free of `/`. This is the matrix the nightly job builds.
17. `go vet ./...` and `go vet -tags integration ./...`: exit 0
18. `git diff --stat -- go.mod go.sum migrations docs/adr` prints nothing.
    `git status --porcelain --untracked-files=all | grep testdata` prints nothing (no fuzz corpus or
    crasher left behind).

If a fuzz run (7 or 8) finds a failing input, it has found a real bug. Stop. Do not commit the
`testdata/fuzz` file. Report the target, the input and the `go test -run=` line. If a repeat run fails,
report the failing test and its message. Do not weaken any test, and do not lower any `-count`.

Report `changed_files` as every path from `git status --porcelain --untracked-files=all`, by exact
path. It must equal §1.

## 9. ADR check

- **ADR-006, CI section.** PR CI keeps unit, golden, fuzz smoke (the seed corpora in `make test`),
  PostgreSQL integration, race and static checks. The nightly workflow adds the "extended fuzz" that
  ADR-006 assigns to nightly/release. Multi-replica chaos, load and replay verification belong to
  LBT-15 and LBT-25. No conflict.
- **ADR-007.** Make orchestration, GitHub Actions and one module. No new Go dependency: actionlint
  is run once through `go run …@version` and is not added to `go.mod`. No conflict.
- **ADR-001, ADR-003, ADR-007 (immutable released rules, canonical serializers).** The crash/v1 edit
  (`curve.go`, `params.go`) changes error wrapping only. Message text, arithmetic, codec bytes and
  golden vectors stay the same. The repo has no release tag, so no released artifact changes. This is
  not a deviation. The PR description must still say so, because it is a non-test edit inside
  `internal/game/crash/v1`.

## 10. Non-goals

- Load and chaos testing (LBT-15) and `make audit` (LBT-25) are out of scope.
- No change to any test assertion, to crash/v1 math, or to golden vectors or fixtures.
- No new PR CI job. No path-aware CI. Nightly runs do not persist a fuzz corpus.
- No other workflow hardening beyond SHA pins. That excludes `persist-credentials` and job
  `timeout-minutes` in `ci.yml`.
- No other linter or linter setting, and no reformatting beyond what gofmt requires on touched lines.

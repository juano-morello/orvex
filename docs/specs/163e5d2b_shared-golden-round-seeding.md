# LBT-24: shared test-support package for seeding golden rounds

- Linear: [LBT-24](https://linear.app/lbtworks/issue/LBT-24), milestone M2 · Durable Execution. Related: LBT-12, LBT-19.
- Linear acceptance: "one implementation of the shared seeding helpers; both packages use it; no test is weakened
  or removed; `make test`, `make test-integration`, `make lint` pass."
- Request: also `make build`; run every check in the foreground to completion; report each exact command with its
  exit status and duration; list every changed file by exact path; explain the package location in the report.
- Origin: `docs/specs/73dbe98d_audit-export-cli.md` §11 item 7 (LBT-19 deferred this extraction). Do not edit that
  spec: it records what was decided then.
- Governing ADRs: ADR-006 (verification strategy, real PostgreSQL tests), ADR-007 (one module, layout, minimal
  dependencies). ADR-001, ADR-003 and ADR-005 apply only as "do not change": golden histories, seeding SQL and
  transaction options stay exactly as they are.

This is a test-only refactor. Production code and test assertions do not change.

## 1. Current state (verified while planning)

- Base: `main` at `cd78841`. `go vet -tags integration ./...` exits 0 on the unmodified tree. Docker 29.8.2 is
  reachable; golangci-lint is v2.14.0. `.golangci.yml` lints with `build-tags: [integration]`.
- `internal/runtime/execution/helpers_integration_test.go` (LBT-12, `package execution_test`, 786 lines) defines
  `newExecutor`, `crashRegistry`, `fundPlayer`, `holdSpec`, `holdFor`, `seedAdmission`, `goldenRound`
  (`roundID`, `lines`, `header`, `cmds`), `readGolden(t, name)`, `goldenConfig`, `seedGolden(t, db, x, name, upTo)`,
  `seedGoldenAdmissions`, `goldenHold`, plus helpers only the execution tests use (`newDB`, `lease`, `cmdID`,
  `newRoundID`, `createRound`, `mustExec`, `inTx`, `effects`, fault injection, `faultDB`, `failingAdapter`, ...).
  `goldenDir = "../../audit/verification/testdata/golden"`.
- `cmd/crashctl/export_helpers_integration_test.go` (LBT-19, `package main`, 306 lines) has its own copies:
  `fundPlayer`, `holdSpec`, `holdFor`, `seedAdmission`, `execOrFatal`, `inExecutionTx`, `exportTestID`, and a
  `seedGolden(t, pool, file)` that builds the executor inline (`execution.New(pool, newRegistry(), DefaultConfig
  with EngineBuild "golden", execution.CrashV1Policy())`) and returns `goldenRound{roundID, lines, exec}` with
  methods `hash`, `lease`, `uuid`. Its comment says the LBT-12 helpers "cannot be imported (spec section 11 open
  item 7)". crashctl's `goldenDir = "../../internal/audit/verification/testdata/golden"` lives in `main_test.go`.
- The two `seedGolden` versions do the same thing: read the golden, decode the genesis and the header's
  `configSecret`, `CreateRound` with the genesis values, require the created genesis to equal golden line 0, fund
  every player with the sum of their S-10 holds over all command records (players sorted), then seed admissions in
  order and require each seeded sequence to equal the record's. Execution's version adds `upTo` (seed admissions
  `1..upTo`, 0 = all) and `seedGoldenAdmissions`; crashctl's always seeds all.
- `fundPlayer` and `seedAdmission` are statement-for-statement identical in both files. The only difference is the
  grant transaction ID source: execution uses `newRoundID()` (prefix `0x0e`, shared with round IDs), crashctl uses
  `exportTestID()` (same prefix, separate counter).
- Execution tests also insert `orvex.ledger_transactions` rows with `newRoundID()` IDs
  (`concurrency_integration_test.go:48`, `failure_integration_test.go:408,412`). A shared grant-ID counter with the
  same `0x0e` prefix could collide with them inside one database (the `failure` case would then fail on
  `ledger_transactions_pkey` instead of the constraint it asserts). Section 2, decision 4, avoids that.
- Import graph (`go list -tags integration`): `pgtest` imports `chain`, `verification`, `canonjson`,
  `persistence/postgres`, `migrations`. It is imported only by external test packages (`postgres_test`,
  `eventstore_test`, `admission_test`, `execution_test`) and by `cmd/crashctl` tests. In-package tests of
  `execution` and `admission` do not import `pgtest`. `pgtest` has no build tag.
- `internal/runtime/admission/helpers_integration_test.go` has a third, different `fundPlayer`/`seedAdmission`
  (`seedAdmission` encodes a `v1.Command` itself). It is not one of the two callers named by LBT-24: out of scope
  (section 9).

## 2. Decisions

1. **Location: new package `internal/persistence/postgres/pgtest/roundtest` (package `roundtest`).**
   - Not inside package `pgtest`: the helpers import `internal/runtime/execution` and `internal/runtime/admission`.
     Adding those imports to `pgtest` would put the executor into every `pgtest` user's test binary
     (`postgres_test`, `eventstore_test`, `admission_test`). It would also mean in-package tests of `admission`
     or `execution` could never import `pgtest`: that would be an import cycle. A separate package keeps the
     runtime dependency limited to its two users.
   - Under `pgtest/` rather than a new `internal/testsupport/...`: the test harness already lives at
     `internal/persistence/postgres/pgtest` (LBT-10 P-7). ADR-007's proposed layout has no `internal/testsupport`
     directory, so a sub-package of the existing harness avoids adding a top-level `internal/` directory the layout
     does not list. Go directories imply no import direction, so nesting under `pgtest/` creates no dependency on or
     from `pgtest`. `roundtest` does not import `pgtest`.
   - Cycle check: `roundtest` imports `audit/chain`, `canonjson`, `game`, `game/crash/v1`, `persistence/postgres`,
     `runtime/admission`, `runtime/execution` and pgx. No production package imports `roundtest`. Its importers
     are `execution_test` (an external test package, allowed to import a package that imports `execution`) and
     `cmd/crashctl` tests (package `main`, which nothing imports). There is no cycle. Package doc rule: in-package
     (`package execution` / `package admission`) tests must never import it.
   - Name: `roundtest` follows the `pgtest` / `httptest` `<thing>test` convention. It does not clash with any
     identifier in either caller package. `seed` would clash with execution's `func seed`.
2. **Test-only, enforced: every `roundtest` file starts with `//go:build integration`.** Verified while planning
   with a throwaway module:
   - `go build ./...`, `go vet ./...` and `go test ./...` without the tag silently skip a package whose files are
     all excluded, so `make test` and `make build` are unaffected;
   - a non-test package importing it fails the untagged build with "build constraints exclude all Go files", so no
     production package can import it;
   - `make lint` (build tag `integration`) and `make test-integration` compile and lint it.

   `pgtest` stays untagged and unchanged: its `pgtest_test.go` unit test runs in `make test`. `roundtest` is used
   only by `*_integration_test.go` files.
3. **Scope of the shared code: everything both callers duplicate for seeding and executing golden rounds.** That is
   the golden executor construction (EngineBuild `"golden"` + `execution.CrashV1Policy()`; crashctl inlines it,
   execution has `newExecutor`), the crash/v1 registry it defaults to, `FundPlayer`, `Hold`/`HoldFor`,
   `SeedAdmission`, golden reading/config decoding, `SeedGolden`, `SeedGoldenAdmissions`, and the golden round type.
   Callers use the shared helpers **directly**: no forwarding wrappers or function-variable aliases under the old
   names. Two small local adapters stay, because each binds a package-specific value: execution's
   `goldenPath(name)` (its `goldenDir`) and crashctl's `seedGolden(t, pool, file)` (its `goldenDir` and the
   production `newRegistry()`, as the LBT-19 spec §10 step 5 requires).
4. **Grant transaction IDs:** `roundtest` generates them from its own counter with first byte `0x5e` (version
   nibble `0x70` as before). That byte differs from every caller's generator (`0x0c` command IDs, `0x0e`
   round/transaction IDs, `0x0f` in execution's in-package tests), so a grant can never collide with a
   caller-inserted ledger transaction. No assertion depends on grant ID values: grants have `round_id` NULL and
   appear in no export, effect set or golden.
5. **Generic statement helpers stay local.** Execution's `mustExec`/`inTx` (used by fault injection,
   `seedMany` and failure tests) and crashctl's `execOrFatal` (used by `replicaExec` and `restrictedRole`) are not
   seeding helpers. `roundtest` gets its own unexported `mustExec`/`inTx`. crashctl's `inExecutionTx` and
   `exportTestID`/`exportTestIDs` become unused and are deleted.
6. **`Golden` carries the executor that created it** (`Executor` field), which replaces crashctl's
   `goldenRound.exec`. Execution tests keep their own executor variables and ignore the field.

## 3. ADR alignment and flags

No decision conflicts with an accepted ADR.

- ADR-007: one module, no new dependency (`roundtest` uses pgx, already required), no `go.mod`/`go.sum` change.
  The new directory holds real code. It sits inside the listed `internal/persistence/{postgres,...}` tree, next to
  the existing harness. `cmd/crashctl` changes only in test files, and binaries still import nothing from each
  other.
- **Flagged (not a conflict):** a package under `internal/persistence/` now imports `internal/runtime/execution`
  and `internal/runtime/admission`, the reverse of the production layering. ADR-007 sets no import-direction rule
  between `internal/` packages. The package is test-only (decision 2) and exists to drive the executor against
  PostgreSQL. Put this point in the PR description and the report.
- ADR-006: tests still run against real PostgreSQL through `pgtest`. No test is removed, skipped or weakened
  (section 7 checks it).
- ADR-001 / ADR-003: golden histories are only read, never regenerated or edited. ADR-005: the seeding SQL and
  `postgres.ExecutionTxOptions()` are moved verbatim.

## 4. The `roundtest` package (normative)

Two new files. Both start with `//go:build integration`, then a blank line. Use goimports grouping with the
`github.com/juano-morello/orvex` local prefix. Import crash/v1 as `crashv1`. All exported functions take
`testing.TB` (as `pgtest` does) and call `t.Helper()`. Use `context.Background()`; there is no package-level `ctx`.

### 4.1 `internal/persistence/postgres/pgtest/roundtest/roundtest.go`

Package doc comment, in substance:

> Package roundtest seeds crash/v1 rounds into a migrated Orvex database for PostgreSQL integration tests
> (LBT-24). It builds the executor that creates golden rounds, funds players, seeds admissions directly with their
> LBT-11 S-10 holds, as LBT-11 does, and creates, funds and seeds the LBT-9 golden histories through LBT-12's
> executor. It executes nothing itself. Callers drive the executor.
>
> It is test support. Every file carries //go:build integration, so no production build can import it. It imports
> internal/runtime/execution and internal/runtime/admission: only external test packages (package x_test) and cmd
> tests may import it, never the in-package tests of a package it imports.

Contents (bodies moved from `internal/runtime/execution/helpers_integration_test.go`; keep the SQL text and error
messages byte-for-byte):

| Identifier | Signature / body |
|---|---|
| `const engineBuild = "golden"` | unexported; comment: the engine build the LBT-9 golden histories record |
| `Registry` | `func Registry(t testing.TB) *game.Registry`: body of execution's `crashRegistry` |
| `NewExecutor` | `func NewExecutor(t testing.TB, db execution.DB, mutate func(*execution.Config), reg *game.Registry) *execution.Executor`: body of execution's `newExecutor` (`DefaultConfig`, `EngineBuild = engineBuild`, `mutate`, `reg == nil` → `Registry(t)`, `execution.New(db, reg, cfg, execution.CrashV1Policy())`, `t.Fatal` on error) |
| `grantIDs`, `grantID()` | `var grantIDs atomic.Uint64`; `grantID() [16]byte`: `id[0], id[6] = 0x5e, 0x70`; `binary.BigEndian.PutUint64(id[8:], grantIDs.Add(1))`. The comment states decision 4. |
| `execer`, `mustExec` | unexported; `mustExec(t testing.TB, db execer, sql string, args ...any)` = execution's `mustExec` |
| `inTx` | unexported; `inTx(t testing.TB, db *pgxpool.Pool, fn func(tx pgx.Tx))` = execution's `inTx` (`postgres.ExecutionTxOptions()`, deferred rollback, `commit: %v`) |
| `FundPlayer` | `func FundPlayer(t testing.TB, db *pgxpool.Pool, player string, credits int64)`: execution's `fundPlayer` with `txID := grantID()` |
| `Hold` | `type Hold struct { Player string; Credits int64 }` |
| `HoldFor` | `func HoldFor(cmd any) *Hold`: `admission.CrashV1Policy().Classify(cmd)`; `err != nil \|\| cl.Hold.PlayerID == ""` → nil |
| `SeedAdmission` | `func SeedAdmission(t testing.TB, db *pgxpool.Pool, roundID, commandID [16]byte, command []byte, effectiveTime int64, hold *Hold) uint64`: execution's `seedAdmission` |

Keep the existing doc comments, adapted: `FundPlayer` "creates the player's account if absent and grants it
credits from the global system account faucet, which it creates if absent", and so on.

### 4.2 `internal/persistence/postgres/pgtest/roundtest/golden.go`

```go
// Golden is an LBT-9 golden history whose round Executor created and whose
// admissions are seeded, none executed.
type Golden struct {
	RoundID [16]byte
	Header  canonjson.Object
	// Lines are the record lines 0..N, without the header.
	Lines [][]byte
	// Commands are the command records 1..N.
	Commands []chain.Command
	// Executor is the executor that created the round.
	Executor *execution.Executor
}

// Lease returns the round-creation lease (fencing token 0).
func (g Golden) Lease() execution.Lease { return execution.Lease{RoundID: g.RoundID} }

// Hash returns the link hash of golden record seq.
func (g Golden) Hash(seq int) chain.Hash { return chain.LinkHash(g.Lines[seq]) }

// UUID returns the round ID as D-11 text.
func (g Golden) UUID() string { return string(canonjson.UUID(g.RoundID)) }
```

| Identifier | Signature / body |
|---|---|
| `ReadGolden` | `func ReadGolden(t testing.TB, path string) (canonjson.Object, [][]byte)`: execution's `readGolden`, reading `path` as given (callers build it). Split with `bytes.Split(bytes.TrimSuffix(b, "\n"), "\n")`; unmarshal line 0 as the header; return `header, lines[1:]`. |
| `GoldenConfig` | `func GoldenConfig(t testing.TB, header canonjson.Object, genesisLine []byte) (chain.Genesis, crashv1.Config)`: execution's `goldenConfig` |
| `SeedGolden` | `func SeedGolden(t testing.TB, db *pgxpool.Pool, x *execution.Executor, path string, upTo int) Golden`: execution's `seedGolden`, with `ReadGolden(t, path)`, `%s` = `path` in the `CreateRound(%s)` and "genesis differs" messages, and `Executor: x` in the result. Fund all players over **all** command records (players sorted, `FundPlayer`), then `SeedGoldenAdmissions(t, db, g, 1, upTo)`. |
| `SeedGoldenAdmissions` | `func SeedGoldenAdmissions(t testing.TB, db *pgxpool.Pool, g Golden, from, to int)`: execution's `seedGoldenAdmissions` (`to == 0` → `len(g.Commands)`; `seeded sequence %d, want %d`) |
| `goldenHold` | unexported: `func goldenHold(t testing.TB, c chain.Command) *Hold`: decode with `crashv1.Adapter().DecodeCommand`, then `HoldFor` |

## 5. Migrating `internal/runtime/execution` (package `execution_test` only)

Edit only the `*_integration_test.go` files. Never touch the in-package files `crashv1_test.go`,
`executor_test.go`, `transition_test.go` or `errors_test.go`. They are `package execution` and must not import
`roundtest`.

### 5.1 `helpers_integration_test.go`

- Delete `crashRegistry`, `newExecutor`, `fundPlayer`, `holdSpec`, `holdFor`, `seedAdmission`, the
  `--- Golden histories ---` block (`goldenRound`, `readGolden`, `goldenConfig`, `seedGolden`,
  `seedGoldenAdmissions`, `goldenHold`).
- Add, in the old golden section:
  ```go
  // goldenPath returns the path of LBT-9 golden history name.
  func goldenPath(name string) string { return filepath.Join(goldenDir, name+".jsonl") }
  ```
- `seed(...)` becomes
  `return roundtest.SeedAdmission(t, db, roundID, cmdID(n), encode(t, cmd), effectiveTime, roundtest.HoldFor(cmd))`.
- `verifyExport`: `crashRegistry(t)` → `roundtest.Registry(t)`.
- Everything else stays: `mustExec`, `inTx`, `newRoundID`, `idCounter`, `lease`, effects, faults and so on. Update
  imports: add `roundtest`, and drop whatever becomes unused (`sort`, `admission`, possibly others; let the compiler
  and goimports decide).

### 5.2 Call-site renames (mechanical; counts measured on the base tree)

Apply them only in the files listed. Every `.roundID/.lines/.cmds/.header` receiver in these files is a golden
round value (`gr`, `ga`, `gb`), which was checked while planning.

| Old | New | Files (count) |
|---|---|---|
| `newExecutor(` | `roundtest.NewExecutor(` | close_bound 1, concurrency 5, crossround 2, e2e 1, execute 13, failure 17, ledger_audit 1, round 4 |
| `crashRegistry(` | `roundtest.Registry(` | crossround 1, execute 1, ledger_audit 1 (+ helpers `verifyExport`) |
| `fundPlayer(` | `roundtest.FundPlayer(` | close_bound 2, crossround 2, e2e 2, execute 5, failure 2 |
| `seedAdmission(` | `roundtest.SeedAdmission(` | close_bound 1, failure 1 |
| `holdFor(c.cmd)`; `h.player`, `h.credits` | `roundtest.HoldFor(c.cmd)`; `h.Player`, `h.Credits` | concurrency 1 (lines 65-68, in `seedMany`) |
| `seedGolden(t, P, X, NAME, N)` | `roundtest.SeedGolden(t, P, X, goldenPath(NAME), N)` | execute 5, failure 1, ledger_audit 1 |
| `seedGoldenAdmissions(` | `roundtest.SeedGoldenAdmissions(` | failure 1 |
| `readGolden(t, NAME)` | `roundtest.ReadGolden(t, goldenPath(NAME))` | execute 2, round 1 (not `readGoldenV2`) |
| `goldenConfig(` | `roundtest.GoldenConfig(` | execute 2, round 1 |
| type `goldenRound` | `roundtest.Golden` | execute 4 (`requireGoldenRecords`, `requireGoldenLedger`, `goldenP3`, `requireExportVerifies`) |
| `gr./ga./gb.roundID` | `.RoundID` | execute 22, failure 13, ledger_audit 2 |
| `gr.lines` | `gr.Lines` | execute 15 |
| `gr.cmds` | `gr.Commands` | execute 6, failure 1, ledger_audit 2 |
| `gr.header` | `gr.Header` | execute 1 |

File names above are short for `internal/runtime/execution/<name>_integration_test.go`. Add the `roundtest` import
to each file that uses it. `lease(gr.RoundID)` stays as is, using the local `lease` helper (do not switch to
`gr.Lease()`). Change nothing else on these lines: no reformatting, renamed variables or rewritten assertions.

## 6. Migrating `cmd/crashctl` (package `main`, test files only)

Do not touch `migrate_integration_test.go` (it holds `TestMain`; LBT-23 edits it), `migrate_test.go`,
`main_test.go` or `export_test.go`.

### 6.1 `cmd/crashctl/export_helpers_integration_test.go`

- Delete `goldenRound` and its `hash`/`lease`/`uuid` methods, `exportTestIDs`, `exportTestID`, `inExecutionTx`,
  `fundPlayer`, `holdSpec`, `holdFor`, `seedAdmission` and the old `seedGolden`.
- Replace the file comment with one saying the helpers are for crashctl audit export (LBT-19 spec §10 step 5), and
  that golden rounds are created, funded and seeded by `internal/persistence/postgres/pgtest/roundtest` (LBT-24),
  shared with the LBT-12 executor tests.
- Add:
  ```go
  // seedGolden creates the golden round in file (relative to goldenDir) with
  // an executor over pool built from crashctl's registry (EngineBuild
  // "golden"), funds each player with the sum of their S-10 holds, and seeds
  // every golden admission in order. It executes nothing.
  func seedGolden(t *testing.T, pool *pgxpool.Pool, file string) roundtest.Golden {
  	t.Helper()
  	reg, err := newRegistry()
  	if err != nil {
  		t.Fatal(err)
  	}
  	return roundtest.SeedGolden(t, pool, roundtest.NewExecutor(t, pool, nil, reg), filepath.Join(goldenDir, file), 0)
  }
  ```
- Keep `execOrFatal`, `replicaExec`, `exportRolePassword`, `exportRoles`, `exportGrants`, `restrictedRole` as they
  are. Fix imports: drop `bytes`, `encoding/binary`, `sort`, `chain`, `canonjson`, `crashv1`, `postgres`,
  `admission`, `execution` if unused; add `path/filepath`, `roundtest`.

### 6.2 `cmd/crashctl/export_integration_test.go`

| Old | New | Count |
|---|---|---|
| `drained(...) goldenRound` | `drained(...) roundtest.Golden` | 1 |
| `g.exec.` | `g.Executor.` | 2 |
| `g.lease()` | `g.Lease()` | 2 |
| `g.hash(` | `g.Hash(` | 11 |
| `g.uuid()` | `g.UUID()` | 19 |
| `g.lines` | `g.Lines` | 3 |
| `g.roundID` | `g.RoundID` | 1 |

Add the `roundtest` import. Change nothing else.

## 7. Tests

No behavior changes, so there are no new tests. `roundtest` gets no test file of its own: a `TestMain` would start
another PostgreSQL container, and the callers' integration tests already exercise every helper. These existing
tests are the coverage. Each must still exist, run and pass unchanged in what it asserts:

- `SeedGolden`, `ReadGolden`, `GoldenConfig`, `Golden`: `TestExecuteReproducesGoldenHistories` (all four
  goldens), `TestExecuteSnapshots`, `TestExecuteOutbox`, `TestColdLoadMatchesCache`, `TestColdLoadChecksSnapshots`,
  `TestCreateRound`, `TestExecutedGoldensExportVerifiedLedger` (this one pre-inserts player accounts, so it
  exercises `FundPlayer`'s `ON CONFLICT (player_id) DO NOTHING`), and every crashctl `TestExport*` that seeds
  goldens: `TestExportExecutedRounds`, `TestExportFollowsSnapshots`, `TestExportStdout`, `TestExportMinimalGrants`,
  `TestExportOutputFailures`.
- `SeedGoldenAdmissions` (partial `upTo`, then a later range) and `SeedAdmission` with a non-golden command:
  `TestDuplicatePayoutImpossible`.
- `FundPlayer`, `SeedAdmission`, `HoldFor`: `TestCloseBoundRefusesEarlyCloseAndSettles`,
  `TestCrossRoundBalanceProtection`, `TestCrossRoundAdmissionWaitsForExecution`, `TestAdmitAndExecuteRound`,
  `TestExecuteBetConsumesHoldAtExactBalance`, `TestExecuteRejectedBetReleasesHold`, `TestFailClosedHaltsRound`,
  `TestSettlementResolvesEarlierCommands`, the `seed()` users in `execute_` and `failure_integration_test.go`,
  and `TestConflictingPerRoundCommits` (`seedMany` → `HoldFor`).
- `NewExecutor` with `mutate` and a custom registry: the failure tests (`failingRegistry`, `faultDB`),
  `TestStaleCacheDiscarded` and `TestConflictingPerRoundCommits` (EngineBuild variants).

Guards the builder runs and reports (section 8): an identical before/after list of passing tests and subtests, no
new `t.Skip`, an identical set of `func Test*` declarations, and no remaining duplicate definitions.

## 8. Verification

Run every command in the foreground from the repo root, to completion. Record the exact command, exit status and
wall time (for example with bash `time`). Judge each by its exit status. First set
`H=/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/163e5d2b/context_handoff` in the shell. The
test-list pipelines below run with `set -o pipefail`, so a failing `go test` fails the pipeline.

0. **Before editing anything**, capture the baseline test list:
   `set -o pipefail; go test -tags integration -count=1 -v ./internal/runtime/execution/ ./cmd/crashctl/ 2>&1 | grep -E '^ *--- (PASS|FAIL|SKIP)' | sed -E 's/ \([0-9.]+s\)$//' | sort > "$H/lbt24-tests-before.txt"`
   → exit 0, and the file has no `FAIL` line. If either fails, stop and report the failing baseline.
1. `gofmt -l internal cmd`: no output.
2. `go vet ./...` → 0, and `go vet -tags integration ./...` → 0 (compiles `roundtest` and every integration test;
   catches import cycles).
3. `make build` → 0.
4. `make test` → 0.
5. `make lint` → 0.
6. `make test-integration` → 0 (Docker).
7. The step 0 command again, writing `"$H/lbt24-tests-after.txt"` (→ exit 0), then
   `diff "$H/lbt24-tests-before.txt" "$H/lbt24-tests-after.txt"` → exit 0 (identical: no test or subtest removed,
   renamed, skipped or failing).
8. `diff <(git grep -h -E '^func (Test|Benchmark|Fuzz|Example)' HEAD -- '*_test.go' | sort) <(git grep -h -E '^func (Test|Benchmark|Fuzz|Example)' -- '*_test.go' | sort)`
   → exit 0.
9. `git diff -U0 -- '*.go' | grep -E '^\+.*\bt\.Skip'` → exit 1 (no match).
10. Duplicates gone:
    `grep -rn -E 'func (fundPlayer|seedAdmission|holdFor|goldenHold|goldenConfig|readGolden|seedGolden|seedGoldenAdmissions|newExecutor|crashRegistry|inExecutionTx|exportTestID)\(|type (holdSpec|goldenRound) ' internal/runtime/execution cmd/crashctl`
    → exactly one line: `cmd/crashctl/export_helpers_integration_test.go:…:func seedGolden(`.
11. Test-only:
    - `go list -tags integration -f '{{join .Imports "\n"}}' ./... | grep -x github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest`
      → exit 1 (no non-test file imports it);
    - `go list -tags integration -deps ./cmd/... | grep -c pgtest/roundtest` → prints `0`, exit 1;
    - `go list -tags integration -f '{{.ImportPath}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./... | grep -E ' .*pgtest/roundtest'`
      → exactly two lines, starting with `github.com/juano-morello/orvex/cmd/crashctl` and
      `github.com/juano-morello/orvex/internal/runtime/execution`.
12. `git status --porcelain` lists only the files in section 10 (plus the spec copy, which the planner added).

## 9. Out of scope

- Production code of any package; test assertions; golden histories and fixtures (never regenerated).
- `internal/runtime/admission` tests, including its own `fundPlayer`/`seedAdmission` variant (a possible follow-up:
  `admission_test` could use `roundtest` too). The admission concurrency test and the other LBT-23 files:
  `internal/game/crash/v1/evolve.go` and the crashctl and postgres migrate tests.
- Makefile, `.github/workflows/ci.yml` (LBT-20), `.golangci.yml`, `go.mod`, `go.sum`, `docs/adr/`, existing specs
  (including `73dbe98d_audit-export-cli.md`).
- Generic statement helpers (`mustExec`, `inTx`, `execOrFatal`) in either caller (decision 5).
- `docs/changes/` notes (written by a later stage, not by this build).

## 10. Files

| Path | Action |
|---|---|
| `internal/persistence/postgres/pgtest/roundtest/roundtest.go` | new (§4.1) |
| `internal/persistence/postgres/pgtest/roundtest/golden.go` | new (§4.2) |
| `internal/runtime/execution/helpers_integration_test.go` | edit (§5.1) |
| `internal/runtime/execution/close_bound_integration_test.go` | edit (§5.2) |
| `internal/runtime/execution/concurrency_integration_test.go` | edit (§5.2) |
| `internal/runtime/execution/crossround_integration_test.go` | edit (§5.2) |
| `internal/runtime/execution/e2e_integration_test.go` | edit (§5.2) |
| `internal/runtime/execution/execute_integration_test.go` | edit (§5.2) |
| `internal/runtime/execution/failure_integration_test.go` | edit (§5.2) |
| `internal/runtime/execution/ledger_audit_integration_test.go` | edit (§5.2) |
| `internal/runtime/execution/round_integration_test.go` | edit (§5.2) |
| `cmd/crashctl/export_helpers_integration_test.go` | edit (§6.1) |
| `cmd/crashctl/export_integration_test.go` | edit (§6.2) |
| `README.md` | edit: in the Layout bullet for `internal/persistence/postgres`, after the `pgtest` clause, add `; pgtest/roundtest: crash/v1 round and golden-history seeding through the executor for integration tests (LBT-24)`, matching the bullet's existing style |
| `docs/specs/163e5d2b_shared-golden-round-seeding.md` | this plan (added by the planner) |

## 11. Report requirements

- Every section 8 command, exactly as run, with its exit status and duration.
- Every changed file by exact path (section 10, adjusted to what was actually changed).
- The location rationale (decision 1), the build-tag enforcement (decision 2), and the flagged reverse layering
  (section 3), stated in the report and the PR description.
- Any deviation from this plan, with the reason.

# Plan v2: close out LBT-12. Run the `-count=5` flakiness check to completion, fix what it exposes, report

Run `6f13b30a`, continuation. The normative build plan is
`docs/specs/6f13b30a_execution-ledger-implementation.md` (v1, committed in `48f9973`), together with the
approved spec `docs/specs/e0df34d3_execution-commit-ledger-outbox.md`. Everything in v1 still holds: its
§2 flags, §3 addendum design, §6 test list and §7 verification. This plan changes no design. It only
finishes the run.

## 1. State of the worktree (verified while planning)

- The previous builder attempt (attempt 3) built everything in v1. It reported every gate passing except
  one: the spec's `-count=5` flakiness check had not finished. That attempt started the check twice in
  the background (`bg_3` and `bg_1`, concurrently) and wrote its report before either one exited. No
  result was recorded, and nothing failed. `make test GOTESTFLAGS=` (v1 §7) was not run.
- `git status --porcelain --untracked-files=all` shows exactly the 57 paths in §5 below. Nothing else is
  changed.
- Protected paths are clean: `git status --porcelain` over `docs/adr internal/game internal/audit
  internal/canonjson go.mod go.sum Makefile .golangci.yml .github AGENTS.md
  internal/persistence/eventstore internal/persistence/postgres/pgtest migrations/000[1-7]_*` prints
  nothing.
- `diff <staged spec> docs/specs/e0df34d3_execution-commit-ledger-outbox.md` shows only the hunks
  `3,4c3,5`, `220,236c221,237` and `1612a1614,1638` (status line, 17 ticked boxes, addendum), as v1
  step 0 requires.
- Docker 29.8.2 is up. The host has 32 CPUs and 31 GiB of memory.
- The regex `Conflicting|StaleCache|CrossRound|Takeover|Crash` selects these tests in
  `./internal/runtime/execution/`:
  - integration: `TestConflictingPerRoundCommits`, `TestStaleCacheDiscarded`,
    `TestCrossRoundBalanceProtection`, `TestCrossRoundAdmissionWaitsForExecution`,
    `TestTakeoverWaitsForCommit`, `TestTakeoverBeforeCompareAndSet`, `TestCrashBeforeCommit`,
    `TestCrashAfterCommit`;
  - unit: `TestCrashV1PolicyDescriptor`, `TestCrashV1Checkpoint`, `TestCrashV1Final`,
    `TestCrashV1CloseOffset`.
- The previous builder measured 31.5 s without `-race` for `TestConflictingPerRoundCommits` alone (2
  variants × 10 trials). Under `-race` and five repetitions, expect the whole run to take tens of minutes
  at most. The planner started one run while planning. Its result was not back when this plan had to be
  submitted, so no duration is known yet.

## 2. Step 1: the flakiness check, to completion (the blocker)

Run from the repository root:

```sh
time go test -race -tags integration -count=5 -run 'Conflicting|StaleCache|CrossRound|Takeover|Crash' ./internal/runtime/execution/
```

Rules, all mandatory:

- **Run exactly one copy at a time.** Never start a second copy while one is running. Two concurrent
  race runs, each with its own Testcontainers PostgreSQL, compete for CPU and slow each other down, and
  the timing-sensitive tests (3 s lock timeouts, 1 s idle timeouts) can then fail for reasons unrelated
  to the code. Do not run `make test-integration` or any other Docker-backed test at the same time.
- **Wait for it to exit before you report.**
  - Call the shell with `timeout: 3600`.
  - If the tool moves the call to a background job, do not poll it and do not start another copy. Wait
    for the job's result message.
  - Do not write the build report, and do not end the turn, until that message has arrived with the
    command's exit status.
  - If the 3600 s deadline kills the command, that is a failure to investigate under step 2, not a
    reason to report.
- **Judge it by the exit status only.** Record three facts for the report: the exact command above, the
  exit status, and `time`'s `real` value.

## 3. Step 2: if it exits non-zero, fix the cause

1. From the output, identify the failing test, its subtest or variant, and its message.
2. Reproduce it alone, e.g. `go test -race -tags integration -count=5 -run '^TestName$'
   ./internal/runtime/execution/`. Increase the count if it is rare.
3. Fix the root cause, following the spec's §10 and v1's rules:
   - Never weaken an invariant, an expected value, a golden comparison or an acceptance assertion.
   - Never add sleeps to wait for locks. Poll `pg_stat_activity`, as `pgtest.WaitUntilBlocked` and the
     existing `waitLockWaiters` do.
   - Do not add `t.Parallel()`.
   - Typical causes to check first:
     - an assertion that depends on interleaving instead of the spec's interleaving-independent
       invariants (E-17 "Determinism");
     - a lock or idle timeout hit under `-race` load. Raise only the test's configured timeouts, not
       the production defaults;
     - package-level state reused across `-count` repetitions;
     - goroutines started without the closed-channel start (`together`).
4. Re-run the single test at `-count=5` until it passes. Then re-run the full step 1 command from
   scratch, under step 1's rules. **The report gives the final full run's exit status and duration**,
   and names each earlier failing run and what it exposed.
5. If the fix touches production code (`internal/runtime/execution/*.go` non-test files,
   `internal/ledger`, migrations), the unit or integration test that covers the corrected behavior must
   be named in the report. Add one if none exists.

## 4. Step 3: final gates (after step 1 has exited 0)

Run them one after another, never at the same time as the flakiness check. Each must exit 0:

```sh
make test
make test GOTESTFLAGS=
make lint
go vet ./...
go vet -tags integration ./...
make build
make golden-check
make test-integration
```

`make test GOTESTFLAGS=` was missing from the previous attempt. Run the whole list even if step 2
changed nothing, so that the report describes the final tree.

Then the unchanged-files checks (v1 §7):

```sh
git status --porcelain -- docs/adr internal/game internal/audit internal/canonjson go.mod go.sum Makefile .golangci.yml .github AGENTS.md internal/persistence/eventstore internal/persistence/postgres/pgtest migrations/0001_foundation.up.sql migrations/0001_foundation.down.sql migrations/0002_rounds_admissions.up.sql migrations/0002_rounds_admissions.down.sql migrations/0003_chain_execution.up.sql migrations/0003_chain_execution.down.sql migrations/0004_snapshots.up.sql migrations/0004_snapshots.down.sql migrations/0005_ledger.up.sql migrations/0005_ledger.down.sql migrations/0006_outbox.up.sql migrations/0006_outbox.down.sql migrations/0007_admission_window.up.sql migrations/0007_admission_window.down.sql
```

This must print nothing. `git status --porcelain -- cmd` must list only
`cmd/crashctl/migrate_integration_test.go`.

## 5. Step 4: the report

- `status: success` only when step 1's final run and every step 3 gate exited 0.
- In the summary or notes:
  - the flakiness command exactly as in step 1, its exit status and its `real` duration;
  - each gate with its exit status;
  - the v1 §2 flags for the PR, which the previous report already listed (items 1 to 8 of its notes).
- `changed_files`: every path from `git status --porcelain --untracked-files=all`, with directories
  expanded to files. Exclude the planner's own `docs/specs/6f13b30a_*` files. Today that is exactly
  these 57 paths, plus any file a step 2 fix touches:

```
README.md
cmd/crashctl/migrate_integration_test.go
docs/architecture/persistence.md
docs/specs/e0df34d3_execution-commit-ledger-outbox.md
internal/ledger/crashv1.go
internal/ledger/crashv1_test.go
internal/ledger/doc.go
internal/ledger/ids.go
internal/ledger/ids_test.go
internal/ledger/imports_test.go
internal/ledger/ledger.go
internal/persistence/postgres/admission_window_integration_test.go
internal/persistence/postgres/close_bound_integration_test.go
internal/persistence/postgres/doc.go
internal/persistence/postgres/errors.go
internal/persistence/postgres/errors_test.go
internal/persistence/postgres/execution_ledger_integration_test.go
internal/persistence/postgres/migrate_integration_test.go
internal/persistence/postgres/migrate_test.go
internal/persistence/postgres/tx.go
internal/persistence/postgres/tx_test.go
internal/runtime/admission/admit_integration_test.go
internal/runtime/admission/barrier_integration_test.go
internal/runtime/admission/doc.go
internal/runtime/admission/errors.go
internal/runtime/admission/errors_test.go
internal/runtime/admission/helpers_integration_test.go
internal/runtime/admission/role_integration_test.go
internal/runtime/admission/sequencer.go
internal/runtime/admission/sequencer_test.go
internal/runtime/execution/close_bound_integration_test.go
internal/runtime/execution/commit.go
internal/runtime/execution/concurrency_integration_test.go
internal/runtime/execution/crashv1.go
internal/runtime/execution/crashv1_test.go
internal/runtime/execution/crossround_integration_test.go
internal/runtime/execution/doc.go
internal/runtime/execution/e2e_integration_test.go
internal/runtime/execution/errors.go
internal/runtime/execution/errors_test.go
internal/runtime/execution/execute_integration_test.go
internal/runtime/execution/executor.go
internal/runtime/execution/executor_test.go
internal/runtime/execution/failure_integration_test.go
internal/runtime/execution/helpers_integration_test.go
internal/runtime/execution/load.go
internal/runtime/execution/policy.go
internal/runtime/execution/retry.go
internal/runtime/execution/retry_test.go
internal/runtime/execution/round.go
internal/runtime/execution/round_integration_test.go
internal/runtime/execution/transition.go
internal/runtime/execution/transition_test.go
migrations/0008_execution_ledger.down.sql
migrations/0008_execution_ledger.up.sql
migrations/0009_close_bound.down.sql
migrations/0009_close_bound.up.sql
```

## 6. Tests

This step changes no behavior, so no test is added or edited unless step 2 finds a defect. In that case
the test that exposed it, run at `-count=5` and then inside the full step 1 command, is the regression
test, and any new or edited test is named in the report. Every test listed in v1 §6 must still pass under
`make test-integration`.

## 7. Out of scope

Any change beyond a fix that step 2 requires: no refactors, no new features, no edits to the approved
spec copy, the ADRs, golden fixtures, migrations `0001`–`0007`, `go.mod` or `go.sum`. Everything
the spec's §13 lists as out of scope remains out of scope.

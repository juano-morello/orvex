# Plan v2: LBT-11 close-out — five-pass concurrency check, gates, changed-file list

Run `f205de56`, continuation. The implementation plan is
`docs/specs/f205de56_admission-sequencer-implementation.md` (committed in `8575b20`). The normative
spec is `docs/specs/5a3d52fc_admission-sequencer.md`, the approved copy of
`/home/juano/Workspaces/LBTWorks/Orvex/requests/lbt-11-approved-spec.md`. Both still apply unchanged.
This plan adds no requirement. It closes the one remaining review item: evidence that the spec's
five-pass concurrency check passes on the final tree (spec §10 "Verification"; plan v1 step 9), and the
exact list of changed files.

ADR alignment: no change to code or design, so no new ADR conflict. The PR-description items from plan
v1 §2 still apply (see step 5).

## 1. State at planning time (verified)

- The worktree holds exactly the 28 changed files listed in step 5 (`git status --porcelain
  --untracked-files=all` gives 28 lines), the same set as the builder's last envelope.
- Review: `context_handoff/review.md` (reviewer_2) approves 22 of 22 requirements. Both earlier
  blockers are fixed: the S-12 return contract in `internal/runtime/admission/sequencer.go`, and the
  NULL-argument cases in `TestAdmitCommandArgumentChecks`
  (`internal/persistence/postgres/admission_window_integration_test.go`).
- The files `internal/runtime/admission/{crashv1,doc,errors,gate,policy,retry,sequencer}.go` and
  `migrations/0007_admission_window.{up,down}.sql` all carry the mtime `2026-10-14 23:16:05`. That is
  after reviewer_2's runs. They were touched by tooling within one millisecond; no agent edit is
  recorded. The evidence for the hand-off must therefore come from the tree as it is now, not from the
  reviewer's run.
- Planner run on the current tree, 23:17:
  - `gofmt -l internal/ cmd/` printed nothing;
  - `go vet ./...` and `go vet -tags integration ./...` exited 0;
  - `time go test -race -tags integration -count=5 -run 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/`
    exited 0: `ok … 45.042s`, `real 0m45.642s`. `-count` disables the test cache, so these are not
    cached results;
  - `go test -tags integration -list 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/`
    selects exactly 7 tests: `TestBarrierConcurrentCloseAndCashOuts`,
    `TestAdmitConcurrentDuplicates`, `TestAdmitConcurrentDuplicatesAcrossRounds`,
    `TestAdmitHotRoundOrdering`, `TestAdmitManyRoundsIndependentSequences`,
    `TestAdmitConcurrentCrossRoundHolds`, `TestAdmitHotRoundDoesNotStarveOtherRounds`. No test in the
    package calls `t.Skip`.

The builder must still run everything itself (steps 2 to 4). The planner's run is a baseline, not the
hand-off evidence.

## 2. Rules for this round

- **Change no file unless a command in steps 3 or 4 fails.** No refactors, no reformatting, no doc
  edits, no plan or spec edits.
- Run every command in the foreground and wait for it to finish. Give the five-pass run a timeout of at
  least 1800 s. Never report while a command is still running: that omission is why this round exists.
- Judge each command by its exit status only.
- Never weaken a concurrency invariant, lower a goroutine or iteration count, drop `-race`, add
  `t.Skip`, add sleeps or widen a timeout in a test to make a run pass (spec §8 and §10).

## 3. Steps

### Step 1. Scope check (no Docker)

1. `git status --porcelain --untracked-files=all` lists exactly the 28 paths of step 5, and nothing
   else.
2. `git diff --stat main -- docs/adr internal/game internal/audit internal/canonjson go.mod go.sum Makefile .github .golangci.yml cmd/gateway cmd/engine internal/persistence/eventstore`
   prints nothing.
3. `git diff --stat main -- migrations` prints nothing (`0001`–`0006` unchanged; the two `0007` files are
   untracked and appear only in item 1).
4. `diff /home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/f205de56/context_handoff/lbt-11-approved-spec.md docs/specs/5a3d52fc_admission-sequencer.md`
   shows exactly the hunks `3,4c3,4` and `202,215c202,215`. (`diff` exits 1 when files differ; that
   is expected here. Judge by the hunk headers.)

### Step 2. Gates on the final tree

Run in this order. Each must exit 0:

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

### Step 3. The five-pass concurrency check (the open review item)

```sh
go test -tags integration -list 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/
time go test -race -tags integration -count=5 -run 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/
```

- The `-list` output must name exactly the 7 tests of §1.
- The second command must exit 0. Record three values: its exit status, the package duration that
  `go test` prints on the `ok` line, and `real` from `time`.
- Expect about 45 s on this host. A much longer run that still passes is acceptable; report it.

### Step 4. Only if step 2 or 3 fails

1. Keep the full failing output (`artifact://…`). Name the failing test, the iteration (1 to 5) and the
   failed assertion or race report.
2. Classify the failure:
   - **Environment.** The Docker daemon is unreachable, `pgtest.Run` fails, or the host ran out of
     resources (the same test passes on an immediate re-run and the output names no invariant). Re-run
     once. Report both runs, and change no file.
   - **Data race** (`WARNING: DATA RACE`). Fix the race where it lives: production code
     (`gate.go`, `sequencer.go`, `retry.go`) or the test helpers (`helpers_integration_test.go`,
     for example shared slices or maps written by goroutines without a mutex).
   - **Invariant violation.** Examples: duplicate or missing sequences, more than one row for a command
     ID, an ACK that differs from its row, a lock-waiter count above the gate, `ErrBusy` or
     `ErrOutcomeUnknown` in a test that expects none. Find the cause in
     `migrations/0007_admission_window.up.sql` (`orvex.admit_command`) or in
     `internal/runtime/admission/*.go`, and check it against S-4 to S-12 and spec §5.3. Fix the code,
     never the assertion. If the spec itself looks wrong, stop and report it instead of deviating.
3. Tests for a fix:
   - the failing test is the regression test, and it must pass in a fresh step-3 run;
   - if the cause can be reproduced without PostgreSQL, also add a unit test to the matching file
     (`internal/runtime/admission/gate_test.go`, `retry_test.go` or `sequencer_test.go`), named
     `Test<Unit><Behavior>`. Name it in the report.
4. After any fix, run steps 1 to 3 again from the start. The hand-off evidence is the clean run after
   the last change.
5. A fix may only touch files already in step 5's list. Touching any other file needs a written reason
   in the report. Migrations `0001`–`0006`, `internal/game/**`, `internal/audit/**`,
   `internal/canonjson/**` and golden fixtures stay frozen.

### Step 5. Report

In the build envelope:

- `summary`: the step-3 result as exit status, `go test` package duration and `real` time (for example
  "five-pass check: exit 0, ok 45.0s, real 45.6s, 7 tests × 5"), and the exit status of each step-2 gate.
  If step 4 ran, say what failed, the cause, the fix and the tests that cover it.
- `changed_files`: exactly these 28 paths, plus any path a step-4 fix added (each with its reason):

  ```text
  AGENTS.md
  README.md
  cmd/crashctl/migrate_integration_test.go
  docs/architecture/persistence.md
  docs/specs/5a3d52fc_admission-sequencer.md
  internal/persistence/postgres/admission_window_integration_test.go
  internal/persistence/postgres/migrate_integration_test.go
  internal/persistence/postgres/migrate_test.go
  internal/persistence/postgres/pgtest/fingerprint.go
  internal/runtime/admission/admit_integration_test.go
  internal/runtime/admission/barrier_integration_test.go
  internal/runtime/admission/commit_integration_test.go
  internal/runtime/admission/concurrency_integration_test.go
  internal/runtime/admission/crashv1.go
  internal/runtime/admission/crashv1_test.go
  internal/runtime/admission/doc.go
  internal/runtime/admission/errors.go
  internal/runtime/admission/errors_test.go
  internal/runtime/admission/gate.go
  internal/runtime/admission/gate_test.go
  internal/runtime/admission/helpers_integration_test.go
  internal/runtime/admission/policy.go
  internal/runtime/admission/retry.go
  internal/runtime/admission/retry_test.go
  internal/runtime/admission/sequencer.go
  internal/runtime/admission/sequencer_test.go
  migrations/0007_admission_window.down.sql
  migrations/0007_admission_window.up.sql
  ```

  The planner's own files are not build output and are left out of this list:
  `docs/specs/f205de56_admission-sequencer-implementation.md` (already committed) and
  `docs/specs/f205de56_admission-sequencer-implementation_v2.md` (this plan).
- `commit_message`: unchanged, `feat(admission): add durable per-round PostgreSQL admission sequencer`.
- `notes_for_next_agent`, for the PR description:
  - the approved spec's §2, including the S-5 and S-7 deviations from LBT-10;
  - plan v1 §2's five repository-forced changes: the `pgtest.Fingerprint` column position, the
    `TestEmbeddedMigrations` header regex, the other migration-count assertions, the `persistence.md`
    corrections and the README status sentence;
  - the open item for the engineer: an ambiguous COMMIT followed only by definite failures. S-12 then
    returns `ErrUnavailable`, `ErrBusy` or "not admitted", although the earlier COMMIT may have
    succeeded. Resubmitting the same command ID returns the original admission through dedupe.

## 4. Tests

There is no behavior change in this round, so no test is added or updated, unless step 4 runs. In that
case the tests are the failing test plus any unit test that step 4 item 3 requires, each named in the
report. The acceptance tests are unchanged from plan v1 §5. The repeated run covers
`TestAdmitConcurrentDuplicates`, `TestAdmitConcurrentDuplicatesAcrossRounds`,
`TestAdmitHotRoundOrdering` (gate 4 and gate 16), `TestAdmitManyRoundsIndependentSequences`,
`TestAdmitConcurrentCrossRoundHolds`, `TestAdmitHotRoundDoesNotStarveOtherRounds` and
`TestBarrierConcurrentCloseAndCashOuts`.

## 5. Out of scope

Unchanged from plan v1 §6 and spec §11: LBT-12 to LBT-15 work, wiring into `cmd/gateway` or
`cmd/engine`, a rejection-evidence table, `LISTEN`/`NOTIFY`, verifier checks of the barrier. Also out of
scope: any cleanup, refactor or doc polish of the approved implementation.

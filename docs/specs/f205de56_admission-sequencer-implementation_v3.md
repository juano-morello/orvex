# Plan v3: LBT-11 close-out — five-pass concurrency check, gates, changed-file list

Run `f205de56`, second continuation. This plan replaces plan v2
(`docs/specs/f205de56_admission-sequencer-implementation_v2.md`). Plan v2's step 1 no longer matches the
repository, and its build attempt produced no result (§1). The implementation plan
`docs/specs/f205de56_admission-sequencer-implementation.md` and the approved spec
`docs/specs/5a3d52fc_admission-sequencer.md` still apply unchanged.

This plan adds no requirement. It closes the one open review item: evidence that the spec's five-pass
concurrency check passes on the final tree (spec §10 "Verification"; plan v1 step 9). It also produces
the exact list of changed files.

ADR alignment: there is no code or design change, so there is no new ADR conflict. The PR-description
items of plan v1 §2 still apply (step 5).

## 1. State at planning time (verified 2026-10-15)

- **The implementation is committed.** Odin's `commit_plan` step after plan v2 committed the whole
  worktree in `86df4db` ("docs(spec): add LBT-11 close-out plan…"): all 28 implementation files plus
  the v2 plan. `git status --porcelain --untracked-files=all` is now empty. Plan v2's check "git status
  lists the 28 paths" is therefore obsolete; step 1 below replaces it.
- **`main` moved** from the base `cc79dcb` to `7a25d82`. The new commit, "chore(odin): give the builder
  60m per call", changes only `odin/odin.config.yaml`. Compare against the merge base with
  `main...HEAD` (three dots). A two-dot `git diff main` would show that odin change reversed.
- **Why the last build failed (environment, not code).** The builder started
  `time go test -race -tags integration -count=5 -run 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/`
  at 23:19:53 local time, with a tool timeout of 2400 s.
  - The host suspended at 23:20:21 (`journalctl`: `PM: suspend entry (s2idle)`).
  - It resumed at 09:59:57 (`PM: suspend exit`).
  - That same second, odin ended the build with `builder hit max_time 30m`, while reporting 41.3 s of
    build time.
  - The test never reported a result. Nothing points to a hang in the code.
- **Planner baseline runs.** The content was identical for both runs: HEAD `86df4db` equals the tree
  of 23:17, and nothing changed afterwards.
  - 23:17: exit 0, `ok … 45.042s`, `real 0m45.642s`.
  - 10:07 today: exit 0, `ok … 47.837s`, `real 0m48.518s`.
  - In both runs `-list` selects exactly 7 tests:
    - `TestBarrierConcurrentCloseAndCashOuts`;
    - `TestAdmitConcurrentDuplicates`;
    - `TestAdmitConcurrentDuplicatesAcrossRounds`;
    - `TestAdmitHotRoundOrdering`;
    - `TestAdmitManyRoundsIndependentSequences`;
    - `TestAdmitConcurrentCrossRoundHolds`;
    - `TestAdmitHotRoundDoesNotStarveOtherRounds`.

    No test in the package calls `t.Skip`.
- **Other checks.** No orphaned test containers are running. Reviewer_2 (`context_handoff/review.md`)
  approved 22 of 22 requirements, and its own five-pass run passed (42 s).
- **Scope against the merge base.** `git diff --name-only main...HEAD` gives 30 paths: the 28 of step 5
  plus the planner files `docs/specs/f205de56_admission-sequencer-implementation.md` and `…_v2.md`.
  After this plan is committed, `…_v3.md` makes 31.
  - The frozen-path diff (step 1 item 3) is empty.
  - Under `migrations/` only the two `0007` files differ.

The planner's runs are a baseline. The builder must still run everything itself; that run is the
hand-off evidence.

## 2. Rules for this round

- **Change no file unless a command in step 2 or 3 fails.** No refactors, reformatting, doc edits, plan
  edits or spec edits.
- **Run the five-pass check first** (step 2). It is the open item. If the session ends early, at least
  this result exists.
- Run every command in the foreground and wait for it to finish. The builder now has 60 minutes per
  call. Give no single command a timeout above 1800 s. The previous attempt used 2400 s, which is more
  than the old 30-minute cap. Never report while a command is still running.
- Do not pipe test output through `grep`. Run the command plainly and keep the full output (the tool
  saves it as `artifact://…`).
- Judge each command by its exit status only.
- Never weaken a concurrency invariant, lower a goroutine or iteration count, drop `-race`, add
  `t.Skip`, add sleeps or widen a test timeout to make a run pass (spec §8 and §10).
- The worktree is clean and the implementation is committed. Do not commit; odin commits. If you make
  no fix, the worktree stays clean.

## 3. Steps

### Step 1. Scope check (no Docker, a few seconds)

1. `git status --porcelain --untracked-files=all` prints nothing.
2. `git diff --name-only main...HEAD` prints the 28 paths of step 5 plus only planner files named
   `docs/specs/f205de56_admission-sequencer-implementation*.md`.
3. `git diff --stat main...HEAD -- docs/adr internal/game internal/audit internal/canonjson go.mod go.sum Makefile .github .golangci.yml cmd/gateway cmd/engine internal/persistence/eventstore`
   prints nothing.
4. `git diff --name-only main...HEAD -- migrations` prints exactly
   `migrations/0007_admission_window.down.sql` and `migrations/0007_admission_window.up.sql`, so
   `0001`–`0006` are unchanged.
5. `diff /home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/f205de56/context_handoff/lbt-11-approved-spec.md docs/specs/5a3d52fc_admission-sequencer.md`
   shows exactly the hunks `3,4c3,4` and `202,215c202,215`. `diff` exits 1 when files differ; judge
   this item by the hunk headers.

### Step 2. The five-pass concurrency check (the open review item)

```sh
go test -tags integration -list 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/
time go test -race -tags integration -count=5 -run 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/
```

- Use a tool timeout of 1800 s for the second command.
- The `-list` output must name exactly the 7 tests of §1.
- The second command must exit 0. Record three values: its exit status, the package duration on
  `go test`'s `ok` line, and `real` from `time`. Expect 42 to 50 s on this host. A slower run that
  passes is acceptable; report it as it is.
- If the run is interrupted without a test failure (host suspend, Docker restart, the tool's own
  timeout), that is an environment failure. Run it again once; report both attempts.

### Step 3. Gates on the final tree

Run in this order, each with a tool timeout of at most 1800 s. Each must exit 0:

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

### Step 4. Only if step 2 or 3 fails

1. Keep the full failing output (`artifact://…`). Name the failing test, the iteration (1 to 5) and the
   failed assertion or race report.
2. Classify the failure:
   - **Environment.** The Docker daemon is unreachable, `pgtest.Run` fails, the host suspended, or it
     ran out of resources: the output names no invariant, and the same test passes when re-run at once.
     Re-run once, report both runs, and change no file.
   - **Data race** (`WARNING: DATA RACE`). Fix the race where it lives: production code (`gate.go`,
     `sequencer.go`, `retry.go`) or the test helpers (`helpers_integration_test.go`, for example a
     slice or map that goroutines write without a mutex).
   - **Invariant violation.** Examples: duplicate or missing sequences, more than one row for one
     command ID, an ACK that differs from its row, more lock waiters than the gate allows, or
     `ErrBusy` / `ErrOutcomeUnknown` in a test that expects neither.
     - Find the cause in `migrations/0007_admission_window.up.sql` (`orvex.admit_command`) or in
       `internal/runtime/admission/*.go`, and check it against S-4 to S-12 and spec §5.3.
     - Fix the code, never the assertion.
     - If the spec itself looks wrong, stop and report that instead of deviating.
   - `0007` is not merged yet (it exists only on this branch), so a SQL fix edits
     `0007_admission_window.{up,down}.sql` in place. `0001`–`0006` stay frozen.
3. Tests for a fix:
   - The failing test is the regression test. It must pass in a fresh step-2 run.
   - If the cause reproduces without PostgreSQL, also add a unit test, named `Test<Unit><Behavior>`, to
     the matching file: `internal/runtime/admission/gate_test.go`, `retry_test.go` or
     `sequencer_test.go`. Name it in the report.
4. After any fix, run steps 1 to 3 again from the start. In step 1 item 1, `git status` now lists
   exactly the files you changed. The hand-off evidence is the clean run after the last change.
5. A fix may touch only files already in step 5's list. Touching any other file needs a written reason
   in the report. Migrations `0001`–`0006`, `internal/game/**`, `internal/audit/**`,
   `internal/canonjson/**` and the golden fixtures stay frozen.

### Step 5. Report

In the build envelope:

- `summary`:
  - the step-2 result: exit status, `go test` package duration and `real` time, for example
    "five-pass check: exit 0, ok 47.8s, real 48.5s, 7 tests × 5";
  - the exit status of each step-3 gate;
  - one sentence saying that the earlier attempt was cut off by a host suspend, not by a test failure;
  - if step 4 ran: what failed, the cause, the fix and the tests that cover it.
- `changed_files`: exactly these 28 paths, which are the branch's implementation changes against the
  merge base (`git diff --name-only main...HEAD` without the planner's `docs/specs/f205de56_*` files).
  Add any path a step-4 fix changed, each with its reason.

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

  Say explicitly that these files are already committed in `86df4db`, and whether this round changed
  any file (expected: none).
- `commit_message`: unchanged, `feat(admission): add durable per-round PostgreSQL admission sequencer`.
- `notes_for_next_agent`, for the PR description:
  - the approved spec's §2, including the S-5 and S-7 deviations from LBT-10;
  - plan v1 §2's five repository-forced changes:
    - the `pgtest.Fingerprint` column position;
    - the `TestEmbeddedMigrations` header regex;
    - the other migration-count assertions;
    - the `persistence.md` corrections;
    - the README status sentence;
  - the open item for the engineer: after an ambiguous COMMIT followed only by definite failures, S-12
    returns `ErrUnavailable`, `ErrBusy` or "not admitted", although the earlier COMMIT may have
    succeeded. Resubmitting the same command ID returns the original admission through dedupe;
  - a note for whoever squashes or writes the PR: the implementation landed in commit `86df4db` under a
    `docs(spec)` subject. The PR title and squash message should use the `feat(admission)` message
    above.

## 4. Tests

This round changes no behavior, so it adds or updates no test unless step 4 runs. If it does, the tests
are the failing test plus any unit test that step 4 item 3 requires, each named in the report.

The acceptance tests are unchanged from plan v1 §5. The repeated run covers:

- `TestAdmitConcurrentDuplicates`;
- `TestAdmitConcurrentDuplicatesAcrossRounds`;
- `TestAdmitHotRoundOrdering` (gate 4 and gate 16);
- `TestAdmitManyRoundsIndependentSequences`;
- `TestAdmitConcurrentCrossRoundHolds`;
- `TestAdmitHotRoundDoesNotStarveOtherRounds`;
- `TestBarrierConcurrentCloseAndCashOuts`.

## 5. Out of scope

Out of scope, unchanged from plan v1 §6 and spec §11:

- LBT-12 to LBT-15 work;
- wiring into `cmd/gateway` or `cmd/engine`;
- a rejection-evidence table;
- `LISTEN`/`NOTIFY`;
- verifier checks of the barrier.

Also out of scope: any cleanup, refactor or doc polish of the approved implementation, and any rewrite
of git history (the commit subject of `86df4db` is left to the PR squash).

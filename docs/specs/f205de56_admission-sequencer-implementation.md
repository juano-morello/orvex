# Plan: implement LBT-11, the durable per-round PostgreSQL admission sequencer

Run `f205de56`. This plan implements the **approved** spec written by odin run `5a3d52fc`
("the spec" below). The engineer approved S-1 to S-14 as written, with no edits, and accepted every point
of the spec's §2, including both deviations from LBT-10: S-5 (lock-free fast path) and S-7
(effective-time clamp).

- Source of the spec: `/home/juano/Workspaces/LBTWorks/Orvex/requests/lbt-11-approved-spec.md`. A
  byte-identical copy is staged at
  `/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/f205de56/context_handoff/lbt-11-approved-spec.md`
  (sha256 `7feeee98ab4e4f95f33e1aff63c6e66a0be906ffc1d49aedd93b86a636e6cc9f`). Step 0 adds it to the
  repo as `docs/specs/5a3d52fc_admission-sequencer.md`.
- **The spec's §4 (S-1 to S-14), §5 (SQL), §6 (Go API), §7, §8 (builder rules) and §10 steps 1 to 6
  (files, test cases) are normative.** This plan does not retype them. It lists every file and every
  test, gives the order of work, and records where the merged repository forces a change the spec did
  not foresee (§2 below). Where this plan and the spec disagree, this plan's §2 wins, because each item
  there is backed by evidence from the merged code. The spec's §0 instruction "make no repository
  changes" no longer applies.
- Linear: LBT-11 (milestone M2, blocked by LBT-10, which is merged; blocks LBT-12).
- ADRs read: ADR-001, ADR-002, ADR-003, ADR-004, ADR-005, ADR-006, ADR-007 (`docs/adr/`).

## 1. Preconditions (verified on this worktree; base `main` at `cc79dcb`)

The spec's §0 precondition holds. LBT-10 is merged with the names the spec expects:

- `migrations/0001_foundation` … `0006_outbox` (`.up.sql` and `.down.sql`) and `migrations/migrations.go`
  (`//go:embed *.sql`). The next free number is **0007**.
- `internal/persistence/postgres`: `AdmissionTxOptions()` (READ COMMITTED, read-write),
  `ExecutionTxOptions()`, `SnapshotReadTxOptions()` (REPEATABLE READ, read-only), `SQLState`,
  `IsRetryable` (`40001`, `40P01`), `CodeAppendOnly` … `CodeRecordBinding` (`OX001` to `OX008`;
  `CodeHeadRegression = "OX005"`, `CodeDownRefused = "OX003"`).
- `internal/persistence/postgres/pgtest`: `Run`, `MigratedDatabase`, `EmptyDatabase`, `CreateDatabase`,
  `Pool` (8 connections), `WaitUntilBlocked`, `LoadHistory`, `Fingerprint`, `Querier`.
- `make test-integration` = `go test $(GOTESTFLAGS) -tags integration -count=1 ./...`; the CI
  `integration` job runs it.
- Schema items the spec relies on match LBT-10 §5.2/§5.6:
  - `orvex.round_admission_heads(round_id, admitted_sequence DEFAULT 0, admitted_ref GENERATED)` with
    `round_admission_heads_forward` (`OX005`) and the deferred `round_admission_heads_head_fkey`;
  - `orvex.admissions(round_id, sequence, prev_sequence GENERATED, command_id, effective_time_unix_micro,
    command)` with `admissions_command_id_key` (global unique) and the nil-UUID check;
  - `orvex.ledger_accounts(account_id, kind, player_id, …, floor_enforced, debits_posted, credits_posted,
    held)` with `ledger_accounts_floor_check` and `ledger_accounts_player_id_key`;
  - `orvex.ledger_holds(round_id, sequence, account_id, amount)` with the AFTER INSERT trigger
    `ledger_holds_apply` (`held = held + amount`) and a foreign key to `admissions`;
  - view `orvex.command_results` and `chain_records.result_status/result_reason`.
- `go.mod` already requires `github.com/jackc/pgx/v5 v5.11.0` and
  `github.com/testcontainers/testcontainers-go v0.44.0`. **No `go.mod` or `go.sum` change.**
- crash/v1 names: `v1.Descriptor()`, `v1.Adapter()`, `v1.GameID`, `v1.RulesVersionNumber`
  (`game.RulesVersion` 1; note `v1.RulesVersion` is the *string* `"crash/v1"`, do not use it as a
  registry key), `v1.MinBetCredits`, `v1.MaxBetCredits`, `v1.AuthorityPlayer/System/None`,
  `v1.PlaceBet{Authority, Player, BetCredits}`, `v1.CashOut{Authority, Player}`,
  `v1.StartRound/CloseRound/SettleRound{Authority}`, `v1.Config{RoundID, Seed, MinBetCredits,
  MaxBetCredits, MaxBets}`, `v1.State.CrashDeadlineUnixMicro`,
  `v1.RoundCrashed.CrashDeadlineUnixMicro`, `v1.BetLost{Player, BetCredits}`.
  `chain.EncodeGenesis`, `chain.EncodeCommand`, `chain.LinkHash`, `chain.Genesis`, `chain.Command`,
  `chain.Result` exist.
- Pointers such as `&v1.CashOut{}` satisfy the `v1.Command` interface (pointer method sets include value
  methods), so `Adapter.EncodeCommand` rejects them with `game.ErrUnencodable`, not
  `game.ErrTypeMismatch` as the spec's S-10 says. Either way `Admit` returns `ErrInvalidCommand`; no
  behavior change.

## 2. ADR alignment, conflicts and deviations (copy into the PR description)

**No ADR conflict.** The two deliberate departures from LBT-10's wording are the spec's §2 items 1 (S-5)
and 2 (S-7), both approved by the engineer. Copy the spec's whole §2 into the PR description, followed by
the items below.

The merged repository forces five changes that the spec's §10 did not list. None contradicts an ADR or a
spec decision; each is the smallest change that keeps the gates green:

1. **`pgtest.Fingerprint` prints a column's position among live columns instead of the raw `attnum`
   (deviation from spec §10 step 1, "change nothing else in `pgtest`").**
   - Evidence: on the pinned `postgres:18.6-trixie` image, `ALTER TABLE … ADD COLUMN` after a
     `DROP COLUMN` of the same columns gives them new `attnum` values (probe: first up 3, 4; re-up 5, 6).
     `pgtest.Fingerprint` already skips `attisdropped` columns, but prints `' #' || a.attnum`.
   - Consequence without the fix: `TestMigrateEachStepReversible` fails at version 7 with
     `re-up 0007: fingerprint differs from the forward pass`. `0007` is the first migration that adds
     columns to an existing table; LBT-10's downs all drop whole tables.
   - Fix: in the columns query of `fingerprintQueries`, replace `a.attnum::text` with
     `(row_number() OVER (PARTITION BY a.attrelid ORDER BY a.attnum))::text`. The `WHERE` already removes
     dropped and system columns and filters whole relations, so the window numbers exactly the live
     user columns. Column order is still fingerprinted; only holes left by dropped columns disappear.
     Update the `Fingerprint` doc comment ("column (position among live columns, type, …)").
   - Alternatives rejected: editing `TestMigrateEachStepReversible` (weakens an LBT-10 invariant);
     rebuilding `round_admission_heads` in `0007` (contradicts spec §5.1).
2. **`TestEmbeddedMigrations` header regex.** Spec §5 fixes `0007`'s first line as
   `-- 0007 admission_window: <purpose> (LBT-11 spec S-7, S-8, §5)`. The LBT-10 test requires
   `(LBT-10 spec P-n…)`. Widen the regex to
   `^-- ([0-9]{4}) ([a-z][a-z0-9_]*): .+ \((?:LBT-10 spec P|LBT-11 spec S)-[0-9]+[^)]*\)$` (capture groups
   1 and 2 unchanged) and the failure message to name both forms. `docs/architecture/persistence.md` §5
   states the same rule and changes with it (item 4).
3. **Two more migration-count assertions** fall under the spec's "any other assertion of 6 as the newest
   version": `TestMigrateUpDownClean` (reverted list) and `TestMigrateCLIStdoutFailure` (`schema
   version 6`). In the latter, the down with audit data now refuses at `0007` before reverting anything,
   so the comment "A runner error after partial progress" becomes "A runner error (the down refuses at
   0007)". The assertion itself still holds: `io.WriteString` calls `Write` even for an empty report,
   so the failing writer still yields exit 4 and one line with `OX003`.
   - Accepted consequence (as the spec already accepts for `TestMigrateDownRefusesAuditData`): the
     worked-example down no longer exercises partial progress through `0006`/`0005`.
4. **`docs/architecture/persistence.md`**: besides the spec's new "Admission (LBT-11)" section, three
   in-place sentences would become false and are corrected: the §1 table row of
   `round_admission_heads`, the §4 table row "A: admission" (which says "first statement locks"; the
   S-5 fast path now runs first inside the function) and the §5 first-line rule (item 2). The Go doc
   comments in `internal/persistence/postgres/tx.go` and `doc.go` stay unchanged (spec: no other edits
   to that package); the client's first statement is still the one call that takes the lock.
5. **`README.md`**: the Status sentence ends "No runtime or transport exists yet." It becomes "No
   command execution, service wiring or transport exists yet." (exact text in step 8).

## 3. Work order

Do the steps in order. Do not run build, lint or tests until step 9, except the targeted runs a step
names. The builder may not commit (odin commits).

### Step 0. Add the approved spec to the repo

```sh
cp /home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/f205de56/context_handoff/lbt-11-approved-spec.md \
   docs/specs/5a3d52fc_admission-sequencer.md
```

Then make exactly these edits and nothing else:

- Lines 3 and 4 (the status paragraph, currently `Status: **PROPOSED, awaiting engineer approval.** …
  in a later run.`) become exactly these two lines:

  ```text
  Status: **APPROVED (2026-10-14).** The engineer approved S-1 to S-14 as written, with no edits, and
  accepted every point of section 2, including both deviations from LBT-10 (S-5 and S-7).
  ```

- Lines 202 to 215 (the 14 rows S-1 to S-14 of the §3 table): each row's final cell `| [ ] |` becomes
  `| [x] |` (for example `sed -i '202,215s/| \[ \] |$/| [x] |/' docs/specs/5a3d52fc_admission-sequencer.md`).

Check (each must hold):

- `diff <staged copy> docs/specs/5a3d52fc_admission-sequencer.md` shows exactly two hunks, `3,4c3,4`
  and `202,215c202,215`;
- on the repo copy, `grep -c '| \[x\] |$'` gives 14 and `grep -c '\[ \]'` gives 0.

### Step 1. Migration `0007_admission_window`

New files `migrations/0007_admission_window.up.sql` and `.down.sql`, per spec §5 exactly (names of
columns, constraints, trigger and functions as given there). First lines:

- up: `-- 0007 admission_window: admission window columns, effective-time conversion and admit_command (LBT-11 spec S-7, S-8, §5)`
- down: `-- 0007 admission_window: drop the admission window and admit_command, refusing when round_admission_heads holds rows (LBT-11 spec S-8, §5.4)`

Conventions (from `0001`–`0006` and `TestMigrationsCreateOnlyOrvexObjects`): schema-qualified names;
every function `LANGUAGE plpgsql` with exactly one `SET search_path = pg_catalog` clause and no other
`SET` clause (the test requires `proconfig = '{search_path=pg_catalog}'`); a `COMMENT ON` for every new
column and function naming ADR-002; LF line endings; no transaction statements on their own line.

Up file contents, in order:

1. `ALTER TABLE orvex.round_admission_heads` adding `admission_state text NOT NULL DEFAULT 'open'`,
   `started_sequence bigint`, `closed_sequence bigint`, `sealed_sequence bigint`, and the constraints
   `round_admission_heads_state_check`, `round_admission_heads_window_check`,
   `round_admission_heads_started_fkey`, `…_closed_fkey`, `…_sealed_fkey` (`NOT DEFERRABLE`; each
   `(round_id, x_sequence) REFERENCES orvex.admissions (round_id, sequence)`). The window check follows
   §5.1 with explicit `IS NOT NULL` tests and ends with "each non-null sequence between 1 and
   `admitted_sequence`".
2. `orvex.check_admission_window()` and trigger `round_admission_heads_window BEFORE UPDATE … FOR EACH
   ROW`. Rank `open` 0, `started` 1, `closed` 2, `sealed` 3 via a `CASE` that yields **NULL for any other
   value**, and raise `OX005` only when `new_rank < old_rank` is true, or when an `OLD` sequence column
   that is not null `IS DISTINCT FROM` its `NEW` value. An unknown state must reach the `CHECK`
   (`TestAdmissionWindowConstraints` expects `23514 round_admission_heads_state_check` for `bogus`). The
   trigger name sorts after `round_admission_heads_forward`, so LBT-10's trigger still fires first.
3. `orvex.unix_micro(ts timestamptz) RETURNS bigint`, `IMMUTABLE`, plpgsql,
   `RETURN (EXTRACT(EPOCH FROM ts) * 1000000)::bigint;`.
4. `orvex.admit_command(…)` with the §5.3 signature, `RETURNS TABLE (outcome text, reason text,
   sequence bigint, effective_time_unix_micro bigint)`, `VOLATILE`, steps 0 to 13 of §5.3 in order.
   - Argument checks (`RAISE … USING ERRCODE = '22023'`) run before step 0.
   - **Name clashes:** the `RETURNS TABLE` columns `sequence` and `effective_time_unix_micro` are
     PL/pgSQL variables. Qualify every column reference in every query with a table alias
     (`a.sequence`, `h.admitted_sequence`, `la.player_id`), never name a variable like an alias, and
     return rows with `RETURN QUERY SELECT …` (or assign the OUT columns and `RETURN NEXT`). `INSERT`
     column lists and `UPDATE … SET` targets are not substituted and stay unqualified.
   - Step 0 uses `PERFORM set_config(…, true)`. Verified on the pinned image: transaction-local
     settings made inside a function whose only `SET` clause is `search_path` survive the function's
     return and revert at `COMMIT`/`ROLLBACK` (probe: `lock_timeout` `1234ms` and `synchronous_commit`
     `on` inside the transaction, `0` and `off` after `COMMIT`).
   - Step 4 is the first statement after the `FOR UPDATE` in step 3; nothing reads a clock earlier.
   - No `EXCEPTION` blocks anywhere (spec §5.3, S-11).
5. `COMMENT ON` for the four columns and the three functions.

Down file per §5.4: the `OX003` `DO` block (message
`down migration refused: orvex.round_admission_heads holds rows`), then drop `orvex.admit_command`, the
trigger, `orvex.check_admission_window()`, the five constraints, the four columns and
`orvex.unix_micro`. After the down the fingerprint must equal version 6 (with the §2 item 1 fix).

### Step 2. LBT-10 test edits for the seventh migration

Exact edits; change nothing else in these files:

- `internal/persistence/postgres/migrate_test.go`, **`TestEmbeddedMigrations`**: add
  `"admission_window"` to `want`; widen the `header` regex and the error message (§2 item 2).
- `internal/persistence/postgres/migrate_integration_test.go`:
  - **`TestMigrateUpDownClean`**: reverted want `[]int{7, 6, 5, 4, 3, 2, 1}`.
  - **`TestMigrateDownRefusesAuditData`**: after `LoadHistory(worked-example)`, `MigrateDown(0)` reverts
    nothing (`versions(reverted)` is empty; update the message), the error has SQLSTATE `OX003`, the
    version is **7**, the chain-record and snapshot counts are unchanged, and the re-up applies nothing
    (`len(versions(up(...))) == 0`).
  - **`TestMigrateEachStepReversible`**: no code change; with step 3 it now proves `0007` is reversible.
- `cmd/crashctl/migrate_integration_test.go`:
  - **`TestMigrateCLI`**: add `"0007 admission_window"` to `names`; comments "six" become "seven";
    `schema version 6` becomes `schema version 7` (up and the no-op up); status check
    `len(got) != 8 || got[7] != "schema version 7"`; the no-op down is `down -to 7`; step 5 expects
    `stdout == ""` (nothing reverted, no schema version) with the same exit code, `OX003` and one
    `crashctl: migrate down: ` stderr line.
  - **`TestMigrateCLIStdoutFailure`**: `schema version 7`; the comment change of §2 item 3.

### Step 3. `pgtest.Fingerprint` column position (§2 item 1)

- `internal/persistence/postgres/pgtest/fingerprint.go`: the one-expression change and doc comment of
  §2 item 1. Nothing else in `pgtest` changes; `LoadHistory` stays as is.
- New test **`TestFingerprintStableAcrossColumnDropAndReAdd`** in
  `internal/persistence/postgres/migrate_integration_test.go` (it has the package `TestMain` and the
  `load`, `up`, `down`, `fingerprint` helpers; follow its `t.Parallel()` style), on
  `pgtest.EmptyDatabase`, with an `fstest.MapFS` set:
  `0001_base`: `CREATE TABLE public.fp_a (x int);` / `DROP TABLE public.fp_a;` and
  `0002_columns`: `ALTER TABLE public.fp_a ADD COLUMN y int, ADD COLUMN z text;` /
  `ALTER TABLE public.fp_a DROP COLUMN y, DROP COLUMN z;`.
  Up to 1 → `f1`; up to 2 → `f2`; down to 1 → equals `f1`; up to 2 → equals `f2`. It fails without the
  fix (the re-added columns get new `attnum`s).

### Step 4. Package `internal/runtime/admission` (spec §6, S-2 to S-12)

| File | Contents |
|---|---|
| `doc.go` | package doc: ADR-002, ADR-005 Transaction A, S-3 to S-8 in brief, a pointer to `docs/specs/5a3d52fc_admission-sequencer.md` and `docs/architecture/persistence.md` §12 |
| `sequencer.go` | `DB`, `Config`, `DefaultConfig`, `New`, `Sequencer`, `Request`, `Admission`, `Admission.Input`, `Admit`, `Outcome` (+ constants), `Status`, `Lookup` |
| `policy.go` | `Policy`, `Class` (+ constants), `Hold`, `Classification`; unexported helpers: class ↔ SQL text (`ordinary`/`start`/`close`/`settle`), classification validity check |
| `crashv1.go` | `CrashV1Policy()` per S-10 |
| `errors.go` | the 13 sentinels, `Reason` (+ 4 constants), `RejectedError`, `ClosureNotDueError` |
| `gate.go` | per-round gate: mutex-guarded `map[[16]byte]*gateEntry{slots chan struct{}; refs int}`, `acquire(ctx, round) (release func(), error)`; the entry is deleted when `refs` reaches 0 |
| `retry.go` | call-error and commit-error classifiers per the S-12 table, `backoff(attempt int) time.Duration` with `math/rand/v2` (`rand.N(limit + 1)`, limit `min(50ms, 5ms·2^(n−2))`), the attempt loop helpers |

Implementation notes (behavior is fixed by the spec; these settle details it leaves open):

- `New`: besides the spec's checks, a nil `db` or nil `reg` gives `ErrInvalidConfig`. Store, per
  `(game, rules version)`, the policy and the registry's adapter, so `Admit` does one map lookup.
- `Admit` validation order is spec §6 "Admit steps" 1; no DB call and no gate slot on any validation
  failure.
- The function call: `SELECT outcome, reason, sequence, effective_time_unix_micro FROM
  orvex.admit_command($1, …, $11)` via `tx.QueryRow(...).Scan(&outcome, &reason /* *string */, &seq /*
  *int64 */, &et /* *int64 */)`. Pass NULLs as typed nil pointers. Timeouts in whole milliseconds
  (`int32(cfg.LockTimeout / time.Millisecond)`).
- Roll back every non-committed transaction with a detached, bounded context
  (`context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)`, unexported constant) and ignore its
  error (`_ =`): nothing was written, and a failed rollback closes the connection, which also releases
  the head lock. This keeps a cancelled caller from holding a round's head until the idle timeout.
- `23505` on `admissions_command_id_key` (cross-round duplicate racing in another round, spec prototype
  4): resolve with a fresh `SnapshotReadTxOptions()` read of the admission by command ID (round, sequence,
  effective time, command bytes). Same round and bytes → `Admission{Duplicate: true}`; otherwise
  `ErrCommandIDReused`; no row → `ErrInternal`. Share this read with `Lookup`.
- `Lookup`: one `SnapshotReadTxOptions()` transaction, one query:
  `admissions a LEFT JOIN chain_records r USING (round_id, sequence) WHERE a.command_id = $1`. No row →
  `ErrNotFound` (a nil ID simply finds no row). `result_status` NULL → `OutcomePending`, `accepted` →
  `OutcomeAccepted`, `rejected` → `OutcomeRejected` with `Reason`; any other value → `ErrInternal`.
  Errors: `ctx` done → wraps `ctx.Err()`; non-`PgError` or classes `08`/`53`/`57` → `ErrUnavailable`;
  anything else → `ErrInternal`. No retry loop.
- Errors: lowercase, prefix `admission: `; wrap with `%w` so `errors.Is` finds the sentinel and
  `errors.As` the `*pgconn.PgError` (for example `fmt.Errorf("%w: %w", ErrInternal, err)`). On an
  ambiguous final attempt, `fmt.Errorf("%w: %w", ErrOutcomeUnknown, cause)` so both
  `ErrOutcomeUnknown` and `context.Canceled` match. No error text formats command bytes, seeds or
  deadlines (`ClosureNotDueError.Error()` names neither time).
- Unknown `reason` text on a `rejected` outcome → `ErrInternal`.
- Lint is golangci `standard` with the `integration` tag: handle every returned error (`_ =` for
  rollbacks), goimports grouping with the `github.com/juano-morello/orvex` local prefix.

### Step 5. Unit tests (`make test`, no Docker)

Internal tests (`package admission`), cases exactly as spec §10 step 3:

- `crashv1_test.go`: **`TestCrashV1PolicyDescriptor`**, **`TestCrashV1Classify`**.
- `errors_test.go`: **`TestRejectedError`**, **`TestClosureNotDueError`**.
- `retry_test.go`: **`TestClassifyCallError`**, **`TestClassifyCommitError`**, **`TestBackoffBounds`**.
- `gate_test.go`: **`TestGateBoundsPerRound`**, **`TestGateRoundsIndependent`**,
  **`TestGateCancelWhileWaiting`**, **`TestGateReleasesEntries`**.
- `sequencer_test.go` (fake `DB`; fake `pgx.Tx` embedding `pgx.Tx` and implementing `QueryRow`,
  `Commit`, `Rollback`; the fake tells admission transactions from read transactions by `TxOptions`):
  - **`TestDefaultConfig`**;
  - **`TestNewValidation`**, plus the cases nil `db` and nil registry;
  - **`TestAdmitValidation`** (the fake fails the test if `BeginTx` is called);
  - **`TestAdmitOutcomeMapping`** (`Commit` only for `admitted`);
  - **`TestAdmitRetryLoop`** (attempts counted on admission transactions only; the `23505` case
    scripts the resolve read);
  - **`TestLookupErrors`** (new, for the `Lookup` mapping above): `BeginTx` fails with a connection
    error → `ErrUnavailable`; the query fails with `42P01` → `ErrInternal`; a row with
    `result_status = 'bogus'` → `ErrInternal`; a cancelled `ctx` → wraps `context.Canceled`.

### Step 6. Integration helpers (`internal/runtime/admission/helpers_integration_test.go`)

`//go:build integration`, `package admission_test`, `func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }`,
and every helper of spec §10 step 4: `newPool`, `newSequencer`, `createRound` (with the rules-version
override option), `fundPlayer`, `seedAdmission`, `head`, `row`, `waitLockWaiters`, `waitAdvisoryWaiter`,
`installCommitHooks`, `faultDB`, `cmdID`, `workedRoundID`, `workedSeed`. Notes:

- `createRound` runs in one `postgres.ExecutionTxOptions()` transaction: `rounds` (from
  `v1.Descriptor()`, `chain = chain.Version`, `created_at_unix_micro` = the genesis `CreatedAtUnixMicro`),
  `round_secrets` (secret bytes from `v1.Adapter().EncodeConfig`), `round_admission_heads` with
  defaults, `round_execution_heads (round_id, 0, 0)`, the genesis `chain_records` row (sequence 0,
  32 zero bytes `prev_hash`, `hash = chain.LinkHash(record)`, NULL result, token 0). Tests that need the
  genesis hash read it back from `chain_records`. `schema_integration_test.go` and
  `pgtest/load.go` show the same inserts.
- `fundPlayer` mirrors `schema_integration_test.go`'s `grant`: a global system account `faucet`
  (`floor_enforced false`, created once per database, for example `INSERT … ON CONFLICT DO NOTHING` then
  select it by name), the player account (`kind 'player'`, `floor_enforced true`), one
  `ledger_transactions` row (kind `grant`, no record) and one posting faucet → player.
- Observation queries (`waitLockWaiters`, `waitAdvisoryWaiter`, `head`, `row`, counts) use a pool or
  connection separate from the sequencer's pool, so they never compete for its connections. They poll
  every 10 ms with a 10 s deadline, then `t.Fatal`; no sleeps to wait for blocking.
- `installCommitHooks` creates schema `orvex_test` in the test's own database, with the two
  `DEFERRABLE INITIALLY DEFERRED` constraint triggers `AFTER INSERT ON orvex.admissions`; build the
  trigger functions with the IDs and key as literals.
- `faultDB` injects faults only into transactions begun with `postgres.AdmissionTxOptions()` and counts
  attempts on those; read transactions (`Lookup`, the `23505` resolve) pass through untouched.
- Hold windows: the function sets `lock_timeout` (3 s by default) for the rest of the transaction, and
  it also bounds the deferred trigger's `pg_advisory_xact_lock` wait during `COMMIT`. Tests that hold
  `COMMIT` must release the advisory key promptly (well under 3 s); keep the held section to the
  assertions the spec lists.

### Step 7. Integration tests (`make test-integration`)

Every test file starts with `//go:build integration`. Cases, setups and checks are exactly those of
spec §10 steps 1 and 5. Admission integration tests do not call `t.Parallel()`; tests in
`internal/persistence/postgres` follow that package's existing `t.Parallel()` convention and helpers
(`migrated`, `loadGolden`, `requireCode`, `count`). Concurrency tests assert only interleaving-
independent invariants; goroutines start on one closed channel; unexpected errors are collected and
reported after all goroutines finish.

- `internal/persistence/postgres/admission_window_integration_test.go` (rows from
  `LoadHistory(empty-round)`, whose admissions 1 to 3 are `StartRound`, `CloseRound`, `SettleRound`):
  **`TestAdmissionWindowConstraints`**, **`TestUnixMicroExact`**, **`TestAdmitCommandArgumentChecks`**.
  - For "`sealed` with `sealed_sequence ≠ admitted_sequence`", insert admission 4 and, in the same
    transaction, set `admitted_sequence = 4` with state `sealed` and sequences `(1, 2, 3)`.
  - For "`started_sequence = 4` with `admitted_sequence = 3`", the `CHECK` fails before the foreign key
    is checked, so the expected error is `23514` `round_admission_heads_window_check`, not `23503`.
- `internal/runtime/admission/admit_integration_test.go`: **`TestAdmitBasics`**,
  **`TestAdmitRetryReturnsOriginal`**, **`TestAdmitCommandIDReused`**, **`TestAdmitRoundNotFound`**,
  **`TestAdmitRulesMismatch`**, **`TestAdmitHoldsCredits`**, **`TestAdmitRejectsNulPlayer`**,
  **`TestLookup`**, **`TestEffectiveTimeReadAfterLock`**, **`TestEffectiveTimeMonotonic`**,
  **`TestAdmitLockTimeout`**, **`TestAbandonedAdmissionReleasesLock`**,
  **`TestAdmitCommandSessionSettings`** (`ALTER DATABASE` needs the name from `current_database()`,
  quoted with `pgx.Identifier{name}.Sanitize()`), **`TestFastPathSkipsLock`**.
- `internal/runtime/admission/concurrency_integration_test.go`: **`TestAdmitConcurrentDuplicates`**,
  **`TestAdmitConcurrentDuplicatesAcrossRounds`**, **`TestAdmitHotRoundOrdering`** (subtests gate 4 and
  16), **`TestAdmitManyRoundsIndependentSequences`**, **`TestAdmitConcurrentCrossRoundHolds`**,
  **`TestAdmitHotRoundDoesNotStarveOtherRounds`** (after `waitLockWaiters(4)`, sample the lock-waiter
  count a few more times and require ≤ 4 each time).
- `internal/runtime/admission/barrier_integration_test.go`: **`TestWindowTransitions`** (all 16 cells),
  **`TestBarrierRejectsLateCommands`**, **`TestAdmitDuplicateAfterWindowClosed`**,
  **`TestBarrierSealIsFinal`**, **`TestClosureNotBefore`**, **`TestClosureNotBeforeEqualityAdmits`**,
  **`TestBarrierConcurrentCloseAndCashOuts`**, **`TestAdmissionStreamDrivesCrashRound`** (replay with
  `v1.Adapter()` `DecodeCommand`/`Decide`/`Evolve` from `InitialState`; rejection codes through
  `RejectionCode`).
- `internal/runtime/admission/commit_integration_test.go`: **`TestAdmitAckFollowsCommit`**,
  **`TestAdmitNoAckWhenCommitFails`**, **`TestAdmitAmbiguousCommitCommitted`**,
  **`TestAdmitAmbiguousCommitAborted`**, **`TestAdmitOutcomeUnknownWhenContextEnds`**,
  **`TestAdmitRetriesConnectionLossBeforeCommit`**.

If a test contradicts the spec, find which side is wrong; never weaken a concurrency invariant to make a
run pass (spec §10).

### Step 8. Documentation

- `docs/architecture/persistence.md`:
  - new section **`## 12. Admission (LBT-11)`** after §11, linking
    `../specs/5a3d52fc_admission-sequencer.md`: the window columns and their constraints
    (`round_admission_heads_state_check`, `…_window_check`, the three `NOT DEFERRABLE` foreign keys, the
    `round_admission_heads_window` trigger raising `OX005`); the S-8 transition table; the
    `admit_command` steps (fast path, lock, clock, clamp, dedupe, window, closure guard, hold, insert,
    head); lock order and the transaction-local timeouts and `synchronous_commit`; the outcome →
    Go error mapping and the S-12 retry policy; the grants the admitting role needs (spec §2 item 13);
    `orvex.unix_micro` as the only timestamp conversion;
  - §1 table, `round_admission_heads` row: Holds becomes "`admitted_sequence`, the admission window
    (§12), Transaction A's lock row"; Invariants add "window forward-only (`OX005`); sealed is final";
  - §4 table, row "A: admission (LBT-11)": Concurrency control becomes "`orvex.admit_command` (§12):
    lock-free dedupe and window read, then `round_admission_heads` `FOR UPDATE`; then
    `clock_timestamp()`, dedupe, window, holds, admission insert";
  - §5 first bullet: "The first line is `-- NNNN name: <purpose> (LBT-10 spec P-n)` or, from `0007`,
    `(LBT-11 spec S-n)`."
- `README.md`:
  - Status: the tail currently reads "… audit CLI ([LBT-9](…)) and the PostgreSQL schema and migration
    tooling ([LBT-10](…)). No runtime or transport exists yet." Change it, keeping both existing URLs,
    to "… audit CLI ([LBT-9](…)), the PostgreSQL schema and migration tooling ([LBT-10](…)) and the
    durable per-round admission sequencer
    ([LBT-11](https://linear.app/lbtworks/issue/LBT-11/implement-durable-per-round-postgresql-admission-sequencer)).
    No command execution, service wiring or transport exists yet."
  - Layout: after the `internal/persistence/eventstore` line add
    ``- `internal/runtime/admission`: durable per-round admission sequencer (Transaction A): dedupe, sequence and effective time, admission window and closure barrier, credit holds, crash/v1 admission policy (ADR-002, ADR-005, LBT-11)``.
- `AGENTS.md`, Commands, last paragraph: "When you change `migrations/`, `internal/persistence/` or
  `internal/runtime/`, also run `make test-integration`."

### Step 9. Verification (each must exit 0)

```sh
make test
make test GOTESTFLAGS=
make lint
go vet ./...
go vet -tags integration ./...
make build
make golden-check
make test-integration
go test -race -tags integration -count=5 -run 'Concurrent|HotRound|ManyRounds' ./internal/runtime/admission/
```

- The `-count=5` run is the flakiness check; all five passes must succeed.
- `git diff --stat main -- docs/adr internal/game internal/audit internal/canonjson go.mod go.sum Makefile .github`
  must be empty, `git diff --stat main -- migrations` must list only the two `0007` files, and no file
  under any `testdata/golden` changes.
- `docs/specs/5a3d52fc_admission-sequencer.md` passes the step 0 check.

## 4. Files

| File | Action |
|---|---|
| `docs/specs/5a3d52fc_admission-sequencer.md` | new: the approved spec (step 0) |
| `migrations/0007_admission_window.up.sql`, `.down.sql` | new (step 1) |
| `internal/runtime/admission/{doc,sequencer,policy,crashv1,errors,gate,retry}.go` | new (step 4) |
| `internal/runtime/admission/{crashv1,errors,retry,gate,sequencer}_test.go` | new, unit (step 5) |
| `internal/runtime/admission/{helpers,admit,concurrency,barrier,commit}_integration_test.go` | new, integration (steps 6, 7) |
| `internal/persistence/postgres/admission_window_integration_test.go` | new, integration (step 7) |
| `internal/persistence/postgres/migrate_test.go` | edit: `TestEmbeddedMigrations` (step 2) |
| `internal/persistence/postgres/migrate_integration_test.go` | edit: `TestMigrateUpDownClean`, `TestMigrateDownRefusesAuditData`; new `TestFingerprintStableAcrossColumnDropAndReAdd` (steps 2, 3) |
| `cmd/crashctl/migrate_integration_test.go` | edit: `TestMigrateCLI`, `TestMigrateCLIStdoutFailure` (step 2) |
| `internal/persistence/postgres/pgtest/fingerprint.go` | edit: column position (step 3) |
| `docs/architecture/persistence.md`, `README.md`, `AGENTS.md` | edit (step 8) |

Unchanged: `docs/adr/**`, `internal/game/**`, `internal/audit/**` (including golden histories),
`internal/canonjson/**`, migrations `0001`–`0006`, `migrations/migrations.go`, `go.mod`, `go.sum`,
`Makefile`, `.github/workflows/ci.yml`, `.golangci.yml`, `cmd/gateway`, `cmd/engine`,
`internal/persistence/postgres/*.go` (non-test), `internal/persistence/postgres/pgtest/{pgtest,load}.go`,
`internal/persistence/eventstore/**`.

## 5. LBT-11 acceptance → tests

| Acceptance | Tests |
|---|---|
| Concurrent duplicate submissions create one record | `TestAdmitConcurrentDuplicates`, `TestAdmitConcurrentDuplicatesAcrossRounds`, `TestAdmitRetryReturnsOriginal`, `TestAdmitDuplicateAfterWindowClosed` |
| Hot-round ordering is unique | `TestAdmitHotRoundOrdering`, `TestAdmitManyRoundsIndependentSequences`, `TestBarrierConcurrentCloseAndCashOuts` |
| Admission ACK follows commit | `TestAdmitAckFollowsCommit`, `TestAdmitNoAckWhenCommitFails`, `TestAdmitAmbiguousCommitCommitted`, `TestAdmitAmbiguousCommitAborted`, `TestAdmitOutcomeUnknownWhenContextEnds`, `TestAdmitRetriesConnectionLossBeforeCommit`; each concurrency test also checks every ACK against its committed row |
| Per-round locking, timeouts | `TestAdmitLockTimeout`, `TestAbandonedAdmissionReleasesLock`, `TestAdmitCommandSessionSettings`, `TestAdmitHotRoundDoesNotStarveOtherRounds`, `TestFastPathSkipsLock` |
| Effective DB time after the lock | `TestEffectiveTimeReadAfterLock`, `TestEffectiveTimeMonotonic`, `TestUnixMicroExact` |
| Unique command IDs, retry lookup | `TestAdmitCommandIDReused`, `TestAdmitRetryReturnsOriginal`, `TestLookup` |
| Deadline admission rules (equality loses) | `TestAdmissionStreamDrivesCrashRound`, `TestClosureNotBefore`, `TestClosureNotBeforeEqualityAdmits` |
| Closure barrier | `TestWindowTransitions`, `TestBarrierRejectsLateCommands`, `TestBarrierSealIsFinal`, `TestBarrierConcurrentCloseAndCashOuts`, `TestAdmissionWindowConstraints` |
| ADR-006 cross-round overspending (admission part) | `TestAdmitConcurrentCrossRoundHolds`, `TestAdmitHoldsCredits` |
| `0007` reversible, LBT-10 runner tests green | `TestMigrateEachStepReversible`, `TestMigrateUpDownClean`, `TestMigrateDownRefusesAuditData`, `TestMigrateCLI`, `TestMigrateCLIStdoutFailure`, `TestEmbeddedMigrations`, `TestFingerprintStableAcrossColumnDropAndReAdd` |

## 6. Out of scope

Everything in spec §11: Transaction B, posting rules, hold releases and the outbox (LBT-12); production
round creation and the lifecycle scheduler (LBT-12/14); gateway, gRPC, client command IDs, rate limiting
and client error mapping (LBT-13); ownership leases, failover, roles and grants (LBT-14); load tests and
tuning (LBT-15); a rejection-evidence table; `LISTEN`/`NOTIFY`; verifier checks of the barrier. No wiring
into `cmd/gateway` or `cmd/engine`. No change to the crash/v1 math, the LBT-9 codec or chain formats, any
golden fixture, or migrations `0001`–`0006`.

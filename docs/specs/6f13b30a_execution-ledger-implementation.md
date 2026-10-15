# Plan: implement LBT-12 (atomic execution commit, ledger and outbox) with the engineer's close-bound addendum

Run `6f13b30a`. This plan implements the **approved** LBT-12 spec written by odin run `e0df34d3`
("the spec" below), plus the engineer addendum of 2026-10-15 ("the addendum"). The engineer approved
E-1 to E-17 as written and accepted every point of the spec's §2.

- Spec source: `/home/juano/Workspaces/LBTWorks/Orvex/requests/lbt-12-approved-spec.md`. A byte-identical
  copy is staged at
  `/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/6f13b30a/context_handoff/lbt-12-approved-spec.md`
  (1612 lines, sha256 `0ef02fdf2a605247dae93443a0e359fa03360e5ab90c4a8894b3eeae854cf023`). Step 0 adds it
  to the repo as `docs/specs/e0df34d3_execution-commit-ledger-outbox.md`. Use the staged copy if the
  `requests/` path is outside your reach.
- **Normative:** the spec's §4 (E-1 to E-17), §5 (algorithm and SQL), §6 (migration `0008`), §7 (Go API),
  §8 (golden expectations), §10 (builder rules), §11 (acceptance tests) and §12 steps 1 to 7 (files, test
  cases, verification). This plan does not retype them. It adds the addendum's design (§3 below), gives
  the order of work, names every file and test, and records where the addendum or the merged code
  changes what the spec says (§2). **Where this plan and the spec disagree, this plan wins.** Everything
  else in the spec stands. The spec's §0 "make no repository changes" no longer applies.
- Linear: LBT-12, milestone M2 · Durable Execution. LBT-10 and LBT-11 are merged.
- ADRs read: ADR-001 to ADR-007 (`docs/adr/`). Specs read: LBT-10 (`17380a6e`), LBT-11
  (`5a3d52fc`), the LBT-11 implementation plans (`f205de56*`), the math and lifecycle specs.

## 1. Preconditions (verified on this worktree; base `main` at `8b26867`)

The spec's §0 precondition holds:

- `migrations/0001` … `0007_admission_window` exist; the next free number is **0008**. `0007`'s header
  names "LBT-11 spec S-7, S-8, §5".
- `internal/runtime/admission` has `Sequencer`, `New`, `Admit`, `Lookup`, `CrashV1Policy`, `Request`,
  `Admission`, `ClosureNotDueError`, `ErrClosureNotDue`. Holds follow LBT-11 S-10/S-11 (player
  `PlaceBet` with a non-empty player and `1 ≤ BetCredits ≤ 1,000,000,000`, inserted in the admission's
  own transaction). Nothing the spec relies on differs.
- `internal/persistence/postgres`: `AdmissionTxOptions`, `ExecutionTxOptions`, `SnapshotReadTxOptions`,
  `SQLState`, `IsRetryable`, `CodeAppendOnly` … `CodeRecordBinding` (`OX001`–`OX008`). `pgtest`: `Run`,
  `MigratedDatabase`, `EmptyDatabase`, `Pool`, `WaitUntilBlocked`, `LoadHistory`, `Fingerprint`.
- APIs the spec calls exist with the expected names: `chain.EncodeGenesis/EncodeCommand/Decode/LinkHash/
  StateHash`, `replay.Run(adapter, initial, records, mode, visit)`, `verification.Verify`,
  `verification.MaxLineBytes = 64 << 20`, `game.NewRegistry`, `(*game.Registry).Lookup`,
  `eventstore.ExportHistory`.
- `TestEmbeddedMigrations`' header regex is `\((?:LBT-10 spec P|LBT-11 spec S)-[0-9]+[^)]*\)`: it must be
  widened (step 1).
- LBT-11's `TestLookup` appends records only for `CashOut` admissions, which carry no hold. **No edit is
  needed for `OX009`** (spec §12 step 1's conditional edit does not apply).
- `AGENTS.md` already names `internal/runtime/` in the `make test-integration` hand-off line. **No
  `AGENTS.md` edit.**
- crash/v1: `v1.State.CrashDeadlineOffsetMicros` is `D` (set by `InitialState`, in `[0, 153,510,000]`),
  `RoundStarted.StartedAtUnixMicro` is `T0` = the `StartRound` effective time, `decideCloseRound` rejects
  `t < T0 + D` (equality accepts), `CashOut` at `t ≥ T0 + D` is `deadline_passed`. Golden `D` values:
  worked-example 9,410,000; instant-crash 0; empty-round 40,010,000.
- `TestMigrationsCreateOnlyOrvexObjects` pins every `orvex` function's `proconfig` to exactly
  `{search_path=pg_catalog}` and the `orvex` relation count to 15. This plan adds no table.
- `pgtest.Fingerprint` hashes `pg_get_functiondef` (body, `SECURITY DEFINER`, `SET` clauses) and
  comments, not ACLs.
- go1.26.5; `go.mod` already has pgx v5.11.0 and testcontainers-go v0.44.0. **No `go.mod`/`go.sum`
  change.**

## 2. ADR alignment, conflicts and deviations (copy into the PR description)

Copy the spec's §2 (items 1 to 20) into the PR unchanged, then add the items below. **No item
contradicts an accepted ADR.**

1. **The addendum resolves spec §2 item 14** (inherited LBT-11 open item 4). An early `CloseRound` can no
   longer close the window, so a round can no longer be stuck with its stakes in the round account.
2. **LBT-11 S-8 (changed, engineer-decided).** The closure guard no longer trusts the caller. Admitting
   `StartRound` records `close_not_before_unix_micro = T0 + D` on the round's admission head;
   `admit_command` returns `not_due` for a `close` whose effective time is earlier (equality admits).
   **Choice flagged for the engineer: `NotBeforeUnixMicro` is removed, not kept as an additional check.**
   - Removed from `admission.Request`, from `orvex.admit_command`'s parameters (11 → 10), and from
     `ClosureNotDueError` (which keeps only `EffectiveTimeUnixMicro`). The `invalid_not_before` outcome
     is retired.
   - Reasons: a caller bound can only delay closure, which has no use case; keeping it would make
     `ClosureNotDueError` ambiguous (it could not say which bound refused without revealing the stored
     one); and the scheduler then never sends the secret deadline over the wire.
   - Consequences: LBT-11 S-9's rows "a `close` before `NotBeforeUnixMicro`" (now: before the stored
     bound) and "bad `NotBeforeUnixMicro` → `ErrInvalidRequest`" (gone); LBT-11 §7's scheduler
     obligation becomes "submit `CloseRound`; on `ErrClosureNotDue`, retry at or after `T0 + D`".
   - LBT-11 S-8's rejected alternative "store the deadline on the head reveals the crash outcome to every
     role that can read the head (P-13, P-25)" is now adopted *with* that risk closed by item 3.
3. **LBT-11 §2 item 13, the admitting role's grants (changed; this is the request's "S-13 grants").**
   LBT-11 decision S-13 itself (rejection evidence) is unchanged. `orvex.admit_command` becomes
   `SECURITY DEFINER`, so the admitting role needs only:
   - `USAGE` on schema `orvex`;
   - `EXECUTE` on `orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, integer,
     integer)`;
   - `SELECT` on `admissions` and `chain_records` (`Lookup` and the `23505` resolve only).

   It must have **nothing** on `round_secrets` (unchanged) and **no `SELECT` on
   `round_admission_heads`** (which now holds the secret bound). The previous `SELECT`/`INSERT`/`UPDATE`
   rights on `rounds`, `round_admission_heads`, `admissions` (insert), `ledger_accounts` and
   `ledger_holds` are no longer needed.
4. **LBT-10 P-13 / `docs/architecture/persistence.md` §8, "migrations create no roles and grant nothing"
   (deviation, needs approval).** Migration `0009` runs `REVOKE ALL ON FUNCTION orvex.admit_command(…)
   FROM PUBLIC`. It grants nothing and creates no role. Without it, any role that LBT-14 gives `USAGE` on
   `orvex` (an auditor, the outbox relay) could admit commands with the owner's rights through the new
   `SECURITY DEFINER` function. PostgreSQL's own guidance for `SECURITY DEFINER` functions requires this
   revoke.
5. **LBT-12 E-16 and §7 (changed, engineer-decided).** `CreateRound` also writes
   `round_secrets.close_offset_micros = D`. The execution `Policy` interface gains
   `CloseOffsetMicros(initial any) (int64, error)` (§3.4). The spec's E-2 rationale and §9 (LBT-14
   scheduler passes `NotBeforeUnixMicro = T0 + D`) are superseded by item 2.
6. **Where `D` lives.** The addendum asks for "a column that only `admit_command` reads". `D` goes in a
   new nullable column of `orvex.round_secrets` (§3.1): the table is already append-only and already
   unreadable by the admitting role (P-13, P-25), and it is written in the same `INSERT` as the seed.
   The engine role, which reads `config_secret` for the cold load, should get column-level `SELECT
   (round_id, config_secret)` from LBT-14 so that only the function's owner reads the offset (documented
   in step 8). No Go code selects the column.
7. **Migration placement (allowed by the addendum).** The addendum is migration
   `0009_close_bound`, immediately after `0008_execution_ledger`. `0008` stays exactly as the spec's §6
   says (its down refuses nothing). `0009`'s down drops columns from tables that hold round data, so per
   LBT-10 P-5 (as extended by LBT-11 §2 item 11) it refuses with `OX003` when `round_admission_heads` or
   `round_secrets` holds rows. Consequence for the spec's §12 step 1 test edits: a down with audit data
   now stops at **`0009`** with nothing reverted (not "reverts `[8]`, then stops at `0007`").
8. **LBT-10 P-11 conventions.** The new function keeps `SET search_path = pg_catalog` (pinned by
   `TestMigrationsCreateOnlyOrvexObjects`). Every relation in it is schema-qualified, and PostgreSQL never
   searches `pg_temp` for functions, so the definer cannot be hijacked through `search_path`.
9. **Spec §12 "Unchanged: `internal/runtime/admission` non-test code" (changed by the addendum).**
   `sequencer.go`, `errors.go` and `doc.go` change (§3.5). Spec §10's "never edit migrations `0001` to
   `0007`" holds: `0009` drops and recreates the function `0007` created; the `0007` files are untouched.
10. **Rounds without an offset fail closed.** Rows created before `0009` (and `pgtest.LoadHistory`
    rounds) have `close_offset_micros` NULL. Admitting `StartRound` for such a round returns `corrupt`
    (`ErrInternal`), and a `close` in a `started` round without a bound returns `corrupt`. There is no
    production data, so no backfill is needed.
11. **ADR check for the addendum.** ADR-002 and ADR-005: the closure barrier still runs in the same
    serialization boundary (the head lock), now with a trusted bound. ADR-004 and the math spec §7: `D`
    and `T0 + D` stay secret until `RoundCrashed`. They are stored at rest with the seed's protection
    (P-25), never returned by `admit_command`, never in errors, the outbox, or exports. ADR-003: `Decide`
    is unchanged and still rejects `crash_not_due`, now as defence in depth. ADR-001: the admission bound
    is not part of the chain; records, replay and golden fixtures are unchanged.
12. **Spec §12 step 5 `seedWithDeadline` (fix).** Counter `0` is the all-zero seed, which `InitialState`
    rejects. Start the scan at `1` (skip any counter whose `InitialState` fails).

## 3. Addendum design (normative for this run)

### 3.1 Schema: migration `0009_close_bound`

`migrations/0009_close_bound.up.sql`. First line, exactly:
`-- 0009 close_bound: secret close offset, close bound recorded at start, SECURITY DEFINER admit_command (LBT-12 spec E-16 and engineer addendum; LBT-11 spec S-8)`

Follow LBT-10 P-11 (schema-qualified names, explicit constraint names, `LANGUAGE plpgsql`,
`SET search_path = pg_catalog`, comments naming ADRs, as `0007` does). In order:

1. `ALTER TABLE orvex.round_secrets ADD COLUMN close_offset_micros bigint, ADD CONSTRAINT
   round_secrets_close_offset_check CHECK (close_offset_micros IS NULL OR close_offset_micros >= 0);`
   `COMMENT ON COLUMN`: secret offset from the start admission's effective time before which no closing
   command is admitted (crash/v1: the crash deadline offset `D`); written at round creation, read only by
   `orvex.admit_command` (ADR-002, ADR-004). The existing append-only triggers keep it immutable.
2. `ALTER TABLE orvex.round_admission_heads ADD COLUMN close_not_before_unix_micro bigint, ADD CONSTRAINT
   round_admission_heads_close_bound_check CHECK (close_not_before_unix_micro IS NULL OR
   started_sequence IS NOT NULL);` `COMMENT ON COLUMN`: secret closure bound `T0 + D`, set once when the
   start command is admitted; a closing command with an earlier effective time is not due (ADR-002,
   ADR-004).
3. `CREATE FUNCTION orvex.check_close_bound() RETURNS trigger`: raise `OX005` (`orvex.round_admission_heads:
   close bound of round % changes`, with `OLD.round_id`; never format the bound) when
   `OLD.close_not_before_unix_micro IS NOT NULL AND NEW.close_not_before_unix_micro IS DISTINCT FROM
   OLD.close_not_before_unix_micro`; return `NEW`. `COMMENT ON FUNCTION` naming ADR-002.
   `CREATE TRIGGER round_admission_heads_close_bound BEFORE UPDATE ON orvex.round_admission_heads FOR EACH
   ROW EXECUTE FUNCTION orvex.check_close_bound();`
4. `DROP FUNCTION orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, bigint,
   integer, integer);` then `CREATE FUNCTION orvex.admit_command(p_round_id uuid, p_game text,
   p_rules_version bigint, p_command_id uuid, p_command bytea, p_class text, p_hold_player_id text,
   p_hold_credits bigint, p_lock_timeout_ms integer, p_idle_timeout_ms integer) RETURNS TABLE (outcome
   text, reason text, sequence bigint, effective_time_unix_micro bigint) LANGUAGE plpgsql VOLATILE
   SECURITY DEFINER SET search_path = pg_catalog`. The body is `0007`'s body with exactly these changes:
   - parameters: `p_not_before_unix_micro` removed; remove its argument check
     (`(p_class = 'close') <> (p_not_before_unix_micro IS NOT NULL)`);
   - `DECLARE`: remove `v_started` and `v_t0`; add `v_offset bigint;` and `v_bound bigint;`;
   - step 3 (locked read): `SELECT h.admitted_sequence, h.admission_state, h.close_not_before_unix_micro
     INTO v_admitted, v_state, v_bound … FOR UPDATE`;
   - step 8 is replaced by:

     ```sql
     -- 8. Closure bound (LBT-12 engineer addendum). The offset and the
     -- bound are secret until the crash: no outcome, message or error
     -- carries them.
     IF p_class = 'start' THEN
         SELECT s.close_offset_micros INTO v_offset
             FROM orvex.round_secrets s
             WHERE s.round_id = p_round_id;
         IF v_offset IS NULL THEN  -- no secret row, or no offset recorded
             RETURN QUERY SELECT 'corrupt'::text, NULL::text, NULL::bigint, NULL::bigint;
             RETURN;
         END IF;
         -- Saturate instead of overflowing; Decide rejects such a start anyway (D-7).
         IF v_t > 9223372036854775807 - v_offset THEN
             v_bound := 9223372036854775807;
         ELSE
             v_bound := v_t + v_offset;
         END IF;
     ELSIF p_class = 'close' THEN
         -- Step 7 admits a close only in state 'started'.
         IF v_bound IS NULL THEN
             RETURN QUERY SELECT 'corrupt'::text, NULL::text, NULL::bigint, NULL::bigint;
             RETURN;
         END IF;
         IF v_t < v_bound THEN
             RETURN QUERY SELECT 'not_due'::text, NULL::text, NULL::bigint, v_t;
             RETURN;
         END IF;
     END IF;
     ```
   - step 12's `UPDATE` adds `close_not_before_unix_micro = CASE WHEN p_class = 'start' THEN v_bound ELSE
     h.close_not_before_unix_micro END`;
   - everything else (steps 0–7, 9–11, 13, the comments, "never catches exceptions") is unchanged.
5. `REVOKE ALL ON FUNCTION orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint,
   integer, integer) FROM PUBLIC;`
6. `COMMENT ON FUNCTION orvex.admit_command(<10-arg signature>) IS 'Transaction A (SECURITY DEFINER):
   dedupe, admission window, closure bound recorded at start and enforced at close, effective time read
   after the round head lock, credit hold and admission insert; one row per call (ADR-002, ADR-004,
   ADR-005).';`

`migrations/0009_close_bound.down.sql`. First line, exactly:
`-- 0009 close_bound: restore the LBT-11 admit_command and drop the close bound, refusing when round_admission_heads or round_secrets holds rows (LBT-12 spec E-16 and engineer addendum; LBT-11 spec S-8)`

1. A `DO` block, as in `0007`'s down: raise `OX003` `down migration refused: orvex.round_admission_heads
   holds rows` if that table has a row, then the same for `orvex.round_secrets`.
2. `DROP FUNCTION orvex.admit_command(<10-arg signature>);`
3. Recreate `0007`'s function **verbatim**: copy from `CREATE FUNCTION orvex.admit_command(` through the
   closing `$$;` and the following `COMMENT ON FUNCTION … IS 'Transaction A: …';` statement, byte for
   byte, from `migrations/0007_admission_window.up.sql`. The fingerprint hashes `pg_get_functiondef` and
   comments, so any difference fails `TestMigrateEachStepReversible`. (The restored function gets the
   default ACL, as after `0007`.)
4. `DROP TRIGGER round_admission_heads_close_bound ON orvex.round_admission_heads; DROP FUNCTION
   orvex.check_close_bound();`
5. `ALTER TABLE orvex.round_admission_heads DROP CONSTRAINT round_admission_heads_close_bound_check, DROP
   COLUMN close_not_before_unix_micro;`
6. `ALTER TABLE orvex.round_secrets DROP CONSTRAINT round_secrets_close_offset_check, DROP COLUMN
   close_offset_micros;`

After the down, the fingerprint equals version 8's.

### 3.2 What never leaves the database

- `admit_command`'s outcomes carry only `outcome`, `reason`, `sequence`, `effective_time_unix_micro`
  (`not_due` carries the caller's own too-early effective time, which is not secret).
- No `RAISE` message, Go error text or `%+v` of an error formats the offset or the bound.
- `eventstore.ExportHistory` is unchanged and selects only `config_secret` from `round_secrets`.
- Outbox payloads are chain records (E-12); before `RoundCrashed` no record carries `D` or `T0 + D`.

### 3.3 Admission head and window table (doc wording)

| State | `close` |
|---|---|
| `started` | admit → `closed` if effective ≥ `close_not_before_unix_micro` (recorded when `start` was admitted), else `not_due`; a missing bound is `corrupt` |

Other cells of the S-8 table are unchanged. `start` additionally fails closed (`corrupt`) when the
round has no `close_offset_micros`.

### 3.4 Execution side (`internal/runtime/execution`)

- `Policy` (spec §7) gains:

  ```go
  // CloseOffsetMicros returns the secret offset from the start command's
  // effective time before which no closing command may be admitted, for the
  // round whose initial state is initial (crash/v1: D). It is >= 0. Errors
  // never format the value.
  CloseOffsetMicros(initial any) (int64, error)
  ```
  `CrashV1Policy`: `initial.(v1.State).CrashDeadlineOffsetMicros`; a foreign type or a negative value is
  an error (`execution: …` text without digits).
- `CreateRound` (E-16): after step 1's `InitialState` check, call `CloseOffsetMicros(initial)`; an error
  is `ErrInvalidRound`. Step 3's `round_secrets` insert becomes `INSERT INTO orvex.round_secrets
  (round_id, config_secret, close_offset_micros) VALUES ($1, $2, $3)`. `Created` and every error stay
  free of the value. Nothing else in E-16 changes (the head row is inserted with defaults; the bound stays
  NULL until `StartRound` is admitted).
- The cold load (§5.2) does not read the new column.

### 3.5 Admission side (`internal/runtime/admission`, non-test code)

- `sequencer.go`:
  - remove `Request.NotBeforeUnixMicro` and its validation in `Admit` (both `ErrInvalidRequest` branches);
  - `admitSQL`: `SELECT outcome, reason, sequence, effective_time_unix_micro FROM
    orvex.admit_command($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`; `attempt` passes `RoundID, game,
    rules, CommandID, command, class, holdPlayer, holdCredits, lockMs, idleMs` (no `notBefore`);
  - outcome `not_due` → `&ClosureNotDueError{EffectiveTimeUnixMicro: *effTime}`;
  - delete the `invalid_not_before` case (it now falls to "unknown outcome" → `ErrInternal`);
  - update the `Admit` doc comment if it mentions the bound.
- `errors.go`: `type ClosureNotDueError struct{ EffectiveTimeUnixMicro int64 }`; its doc says the effective
  time was before the round's stored closure bound (LBT-11 S-8 as amended by the LBT-12 engineer
  addendum) and that the error never carries the bound. `ErrInvalidRequest`'s doc drops the
  `NotBeforeUnixMicro` clause. `Error()` text unchanged.
- `doc.go`: the window bullet says a `CloseRound` is admitted only at an effective time at or after the
  bound `T0 + D` that admitting `StartRound` recorded from the round's secret offset; mention that
  `orvex.admit_command` is `SECURITY DEFINER`.

## 4. Files

| File | Action |
|---|---|
| `docs/specs/e0df34d3_execution-commit-ledger-outbox.md` | new: the approved spec (step 0) |
| `migrations/0008_execution_ledger.up.sql`, `.down.sql` | new, spec §6 exactly |
| `migrations/0009_close_bound.up.sql`, `.down.sql` | new, §3.1 |
| `internal/persistence/postgres/errors.go`, `tx.go`, `doc.go` | edit, spec §12 step 1 |
| `internal/persistence/postgres/tx_test.go`, `errors_test.go` | edit (unit), spec §12 step 1 |
| `internal/persistence/postgres/migrate_test.go` | edit: two names, regex (step 1) |
| `internal/persistence/postgres/migrate_integration_test.go` | edit: nine migrations, `0009` refuses (step 1) |
| `internal/persistence/postgres/admission_window_integration_test.go` | edit: 10-arg `admit_command` (step 2) |
| `internal/persistence/postgres/execution_ledger_integration_test.go` | new, spec §12 step 1 |
| `internal/persistence/postgres/close_bound_integration_test.go` | new (step 2) |
| `cmd/crashctl/migrate_integration_test.go` | edit: nine migrations, `0009` refuses (step 1) |
| `internal/runtime/admission/sequencer.go`, `errors.go`, `doc.go` | edit, §3.5 |
| `internal/runtime/admission/sequencer_test.go`, `errors_test.go` | edit (unit), step 2 |
| `internal/runtime/admission/helpers_integration_test.go`, `barrier_integration_test.go`, `admit_integration_test.go` | edit (step 2) |
| `internal/runtime/admission/role_integration_test.go` | new (step 2) |
| `internal/ledger/doc.go`, `ledger.go`, `ids.go`, `crashv1.go` + `ids_test.go`, `crashv1_test.go`, `imports_test.go` | new, spec §12 step 2 |
| `internal/runtime/execution/*.go` (spec §12 files table) | new, spec §12 steps 3–6, plus §3.4 |
| `internal/runtime/execution/close_bound_integration_test.go` | new (step 7) |
| `docs/architecture/persistence.md` | edit, spec step 7 + §3 (step 8) |
| `README.md` | edit, spec step 7 (step 8) |

Unchanged (verify at the end): `docs/adr/**`, `internal/game/**`, `internal/audit/**` (including every
golden fixture under `testdata/`), `internal/canonjson/**`, `migrations/0001`–`0007`, `go.mod`, `go.sum`,
`Makefile`, `.github/workflows/ci.yml`, `.golangci.yml`, `AGENTS.md`, `cmd/*` non-test code,
`internal/persistence/eventstore/**`, `internal/persistence/postgres/pgtest/**`, `internal/runtime/admission`
non-test files other than the three named above, `docs/changes/**`.

## 5. Work order

Follow the spec's §10 builder rules throughout (goimports grouping, `//go:build integration` first line,
one `TestMain` per package, assert SQLSTATEs / constraint names / sentinels / bytes, never message text,
no sleeps to wait for locks, no `t.Parallel()` in the execution package's concurrency tests).

### Step 0: add the approved spec

1. `cp <staged copy> docs/specs/e0df34d3_execution-commit-ledger-outbox.md`.
2. Replace lines 3–4 (the `Status: **PROPOSED, …** … in a later run.` paragraph) with exactly:

   ```
   Status: **APPROVED (2026-10-15).** The engineer approved E-1 to E-17 as written, with no edits, and
   accepted every point of section 2. The engineer addendum at the end of this document resolves section 2
   item 14.
   ```
3. In the §3 table (lines 220–236) change each `| [ ] |` to `| [x] |` (17 rows, E-1 to E-17).
4. Append, after the last line (`11. Follow-up issue: …snapshots.`), one blank line and:

   ```
   ## Engineer addendum (2026-10-15)

   Decided by the engineer on 2026-10-15. It resolves section 2 item 14, the risk inherited from LBT-11
   open item 4: a `CloseRound` admitted before `T0 + D` must not be able to close the admission window.
   It is fixed at admission, in the LBT-12 implementation:

   - `Executor.CreateRound` (E-16) stores the round's crash deadline offset `D` (microseconds, from the
     approved crash/v1 math, D-9: `D = n_c * Q`) in a column that only the `SECURITY DEFINER` function
     `orvex.admit_command` reads. The admitting role gets no grant on it, and it never leaves the database
     through admission results, exports before settlement, the outbox, or errors.
   - Admitting `StartRound` records the bound `close_not_before = T0 + D` on the round's admission head,
     where `T0` is that admission's effective time.
   - `admit_command` refuses a `CloseRound` whose effective time is earlier than that stored bound, with
     the same not-yet-due rejection class LBT-11 uses. The caller-supplied `NotBeforeUnixMicro` (LBT-11
     S-8) no longer decides closure.
   - Tests against real PostgreSQL: an early `CloseRound` is refused and the window stays open; a
     `CloseRound` at exactly `T0 + D` is admitted (closure at equality, as LBT-11 S-8 and the lifecycle
     spec item 7 state); the round then settles normally; the admitting role cannot read the stored
     offset.
   - The change goes in migration `0008` or an immediately following one. It changes LBT-11 S-8, the
     admitting role's grants (LBT-11 §2 item 13) and E-16.

   The implementation plan `docs/specs/6f13b30a_execution-ledger-implementation.md` records how it is
   built: migration `0009_close_bound`, and the removal of `NotBeforeUnixMicro`.
   ```
5. Check: `diff <staged copy> docs/specs/e0df34d3_execution-commit-ledger-outbox.md` shows only hunks
   `3,4c3,5`, `220,236c221,237` and the appended block (diff exits 1 because files differ; inspect the
   hunks, not the exit status). No other change to the spec's content.

### Step 1: migration `0008`, `postgres` additions, LBT-10 test updates (spec §12 step 1)

Do spec §12 step 1 as written, with these deltas (they already account for `0009` from step 2):

- `migrate_test.go`:
  - `want` gains `"execution_ledger", "close_bound"`;
  - widen `header` to `` `^-- ([0-9]{4}) ([a-z][a-z0-9_]*): .+ \(LBT-[0-9]+ spec [A-Z]-[0-9]+[^)]*\)$` ``
    and update the `t.Errorf` text to "(LBT-n spec X-n)".
- `migrate_integration_test.go`:
  - `TestMigrateUpDownClean`: reverted `[]int{9, 8, 7, 6, 5, 4, 3, 2, 1}`;
  - `TestMigrateDownRefusesAuditData`: comment "0009 refuses first: round_admission_heads and
    round_secrets hold the loaded round's rows"; reverted nothing ("0009 refuses"); `OX003`; version `9`;
    counts unchanged; re-up applies nothing.
- `cmd/crashctl/migrate_integration_test.go`:
  - `TestMigrateCLI`: `names` gains `"0008 execution_ledger", "0009 close_bound"`; comments "all nine" /
    "nine applied"; `schema version 9` everywhere; `status` has 10 lines with `got[9] == "schema version
    9"`; the no-op down is `-to 9`; down with data: exit `exitFailed` with `OX003`, stdout `""`, message
    "0009 refuses before any step completes";
  - `TestMigrateCLIStdoutFailure`: `schema version 9`; comment "the down refuses at 0009".
- `TestChainRecordsRequireHoldRelease` and `TestLedgerPostingsOnePerEvent` load goldens with
  `LoadHistory` (unchanged by `0009`).
- `TestMigrateEachStepReversible`, `TestMigrationsCreateOnlyOrvexObjects`: no edit; they must pass with
  `0008` and `0009` (15 relations; `proconfig` `{search_path=pg_catalog}` for every function).

### Step 2: migration `0009` and the admission changes (§3.1, §3.5)

Write `0009` (§3.1) and the §3.5 code changes, then the tests below.

**Unit (`make test`):**
- `internal/runtime/admission/sequencer_test.go`:
  - `fakeTx.QueryRow`: expect **10** arguments ("want 10");
  - `TestAdmitValidation`: delete the cases "not-before on CashOut", "close with not-before 0", "close
    with not-before -1" and the unused `closeRound` variable;
  - `TestAdmitOutcomeMapping`: `closeReq` without `NotBeforeUnixMicro`; the `not_due` check is
    `errors.As(err, &e) && e.EffectiveTimeUnixMicro == 900`; the `invalid_not_before` row becomes
    `{name: "retired invalid_not_before", req: closeReq, script: nilRow("invalid_not_before"), wantErr:
    ErrInternal}`.
- `internal/runtime/admission/errors_test.go` `TestClosureNotDueError`: construct with
  `EffectiveTimeUnixMicro` only; `errors.As` recovers it; `Error()` has no digits.

**Integration, `internal/persistence/postgres`:**
- `admission_window_integration_test.go` `TestAdmitCommandArgumentChecks`: drop `notBefore` from `args`;
  call `orvex.admit_command($1 … $10)` with 10 values; delete the cases "not-before on ordinary" and
  "close without not-before". The remaining cases still give `22023`, and the trailing counts hold.
- new `close_bound_integration_test.go` (`package postgres_test`, reuse the package's helpers
  `migrated`, `loadGolden`, `exec`, `requireStmtCode`, `mustCommit`, `newID`, `count`):
  - `TestCloseBoundColumns`:
    - a new `rounds` row plus a `round_secrets` row with `close_offset_micros = -1` → `23514`
      `round_secrets_close_offset_check`; with `0` it commits;
    - `UPDATE orvex.round_secrets SET close_offset_micros = 1` → `OX001`;
    - on the `empty-round` head (open, admitted 3): `SET close_not_before_unix_micro = 1` → `23514`
      `round_admission_heads_close_bound_check`; `SET admission_state = 'started', started_sequence = 1,
      close_not_before_unix_micro = 5` commits; then `SET close_not_before_unix_micro = 6` → `OX005`, and
      `SET close_not_before_unix_micro = NULL` → `OX005`.
  - `TestAdmitCommandCloseBoundFailsClosed` (raw `admit_command` calls in a transaction that is rolled
    back): on the loaded `empty-round` round (offset NULL, head open) a `'start'` call returns `corrupt`;
    after `mustCommit` of `admission_state = 'started', started_sequence = 1` (bound NULL) a `'close'`
    call returns `corrupt`; the head (`admitted_sequence` 3, state, bound) and the admission count are
    unchanged.
  - `TestAdmitCommandSecurityDefiner`: `pg_proc` for
    `'orvex.admit_command(uuid,text,bigint,uuid,bytea,text,text,bigint,integer,integer)'::regprocedure`
    has `prosecdef = true` and `proconfig = '{search_path=pg_catalog}'`;
    `to_regprocedure('orvex.admit_command(uuid,text,bigint,uuid,bytea,text,text,bigint,bigint,integer,integer)')`
    is NULL; `has_function_privilege('public', '<10-arg signature>', 'EXECUTE')` is false.

**Integration, `internal/runtime/admission`:**
- `helpers_integration_test.go`:
  - refactor `createRoundRules` into `createRoundWith(t, db, roundID, seed, rules, offset *int64)`;
    `createRound`/`createRoundRules` pass `&state.CrashDeadlineOffsetMicros` (the round's real `D`); the
    `round_secrets` insert writes `close_offset_micros` (NULL when `offset` is nil);
  - `seedWithOffset(t, roundID, min, max int64) [32]byte`: scan 32-byte big-endian counters from 1 until
    `v1.Rules{}.InitialState` (config as in `createRound`) gives `min ≤ D ≤ max`; fail after 100,000;
    `instantSeed(t, roundID)` = `seedWithOffset(t, roundID, 0, 0)`;
  - `closeRequest(roundID, commandID [16]byte) admission.Request` (no not-before);
  - `headRow` gains `bound *int64`; `head()` selects `close_not_before_unix_micro`.
- Rounds that close right after `StartRound` use `createRound(t, e.pool, round, instantSeed(t, round))`
  (D = 0, bound = T0): `windowAt`, `TestBarrierRejectsLateCommands`, `TestAdmitDuplicateAfterWindowClosed`,
  `TestBarrierConcurrentCloseAndCashOuts`, `TestFastPathSkipsLock`. Every `closeRequest(…, notBefore)`
  call drops the third argument (`TestWindowTransitions`, `TestBarrierSealIsFinal` and the above). Their
  assertions are otherwise unchanged.
- `admit_integration_test.go` `rawAdmit`: `orvex.admit_command($1, 'crash', 1, $2, $3, 'ordinary', NULL,
  NULL, $4, $5)`.
- `barrier_integration_test.go`, rewritten with exact effective times (S-7 clamp), round
  `workedRoundID`/`workedSeed` (`D = 9,410,000`, assert it):
  - `TestClosureNotBefore`: `seedAdmission` an ordinary `cashOut("zed")` at `x = now + 1 h`; admit
    `StartRound` → effective exactly `x` (= T0); head `started`, `bound == x + D`; admit `CloseRound` →
    `*ClosureNotDueError`, `errors.Is(err, ErrClosureNotDue)`, `EffectiveTimeUnixMicro == x`;
    `err.Error()` and `fmt.Sprintf("%+v", err)` contain neither `strconv.FormatInt(x+D, 10)` nor
    `strconv.FormatInt(D, 10)`; no row; head unchanged (`started`, `closed` nil, same `admitted`, same
    bound). **The window stays open:** `CashOut alice` is admitted (effective `x`). `seedAdmission` at
    `x + D − 1`; `CloseRound` → not due with effective exactly `x + D − 1`; no row.
  - `TestClosureNotBeforeEqualityAdmits`: same start at `T0 = x`; `seedAdmission` at `x + D`; `CloseRound`
    is admitted with effective exactly `x + D`; head `closed` at its sequence; `SettleRound` admitted;
    head `sealed`.
  - `TestAdmissionStreamDrivesCrashRound`: after step 4 (`CashOut alice`) insert an early
    `closeRequest(round, cmdID(7))` → `*ClosureNotDueError` with effective `< deadline`, no row (this
    replaces the `deadline+1` not-before step); after step 6, `closeRequest(round, cmdID(8))` is admitted
    at sequence 7 with effective exactly `deadline`; assert the head's bound `== deadline` after the
    start. Sequences, replay and every other assertion are unchanged.
  - new `TestStartWithoutCloseOffsetFailsClosed`: `createRoundWith(…, nil)`; `StartRound` → `ErrInternal`;
    no row; head `open`, `admitted` 0, bound nil.
- new `role_integration_test.go` `TestAdmittingRoleCannotReadCloseBound`:
  - as the test superuser: `CREATE ROLE <r> NOLOGIN` (`r` unique per call, e.g. `orvex_admitter_<n>`
    from the package's atomic counter; quote with `pgx.Identifier`), then exactly the documented
    admitting-role grants (one helper constant used by the test and mirrored in the docs): `GRANT USAGE ON
    SCHEMA orvex`, `GRANT EXECUTE ON FUNCTION orvex.admit_command(<10-arg>)`, `GRANT SELECT ON
    orvex.admissions, orvex.chain_records`. Cleanup (registered before the pool's): `DROP OWNED BY <r>;
    DROP ROLE <r>` on the test database.
  - a pool on `e.dsn` whose `AfterConnect` runs `SET ROLE <r>` (closed at cleanup, before the role is
    dropped); a `Sequencer` over it;
  - round `workedRoundID`/`workedSeed`, alice funded 100 (as superuser). Through the role's sequencer:
    `PlaceBet alice 10` admitted (hold placed); `seedAdmission` (superuser) at `x = now + 1 h`;
    `StartRound` admitted at `x`; an early `CloseRound` → `*ClosureNotDueError`; `seedAdmission` at
    `x + D`; `CloseRound` admitted at `x + D`; `SettleRound` admitted; `Lookup(bet)` returns pending.
  - as the role, each statement fails with `42501` (`postgres.SQLState`): `SELECT close_offset_micros
    FROM orvex.round_secrets`; `SELECT config_secret FROM orvex.round_secrets`; `SELECT
    close_not_before_unix_micro FROM orvex.round_admission_heads`; `SELECT admission_state FROM
    orvex.round_admission_heads`; a direct `INSERT INTO orvex.admissions`.
  - as the superuser: `has_column_privilege(<r>, 'orvex.round_secrets', 'close_offset_micros',
    'SELECT')` and `has_column_privilege(<r>, 'orvex.round_admission_heads',
    'close_not_before_unix_micro', 'SELECT')` are false; `has_function_privilege('public', <10-arg>,
    'EXECUTE')` is false; `has_function_privilege(<r>, <10-arg>, 'EXECUTE')` is true.
- Unchanged and must still pass: every other LBT-11 test (`TestLookup` needs no hold-release edit).

### Step 3: `internal/ledger` (spec §12 step 2)

As written: `doc.go`, `ledger.go`, `ids.go`, `crashv1.go` per §7, E-8, E-9. Unit tests:
`TestHouseAccountID`, `TestRoundAccountID`, `TestTransactionID` (the §1 literals), `TestIDsAreUUIDv8`,
`TestIDsDistinct`, `TestCrashV1Descriptor`, `TestCrashV1Postings`, `TestCrashV1PostingsRejectsBadInput`,
`TestCrashV1PostingsWorkedExample`, `TestLedgerImportsArePure`.

### Step 4: `internal/runtime/execution` code (spec §12 step 3, plus §3.4)

As written (§5, §7, E-2 to E-16), with the `Policy.CloseOffsetMicros` method and the `CreateRound`
change of §3.4. Every fake `Policy` in tests implements the new method.

### Step 5: execution unit tests (spec §12 step 4)

All of: `TestCrashV1PolicyDescriptor`, `TestCrashV1Checkpoint`, `TestCrashV1Final`, `TestFailClosedError`,
`TestClassifyStatementError`, `TestClassifyCommitError`, `TestConflictDecision`, `TestBackoffBounds`,
`TestDefaultConfig`, `TestNewValidation`, `TestExecuteNextValidation`,
`TestTransitionReproducesGoldenRecords`, `TestTransitionFailClosed`, `TestTransitionHoldChecks`,
`TestTransitionValidatesPostings`, `TestMaxRecordBytes`. Plus, in `crashv1_test.go`:
- `TestCrashV1CloseOffset`: the worked-example config (round `0190f3a8-7b3c-7d4e-8f00-000000000001`, seed
  bytes `0x00…0x1f`) gives `9,410,000`; the instant-crash golden's config (genesis config + header
  `configSecret`) gives `0`; the empty-round golden's gives `40,010,000`; a foreign state type (`struct{}{}`)
  errors, and the error text has no digits.

### Step 6: execution integration helpers (spec §12 step 5)

As written, with `seedWithDeadline` starting at counter 1 (§2 item 12).

### Step 7: execution integration tests (spec §12 step 6, plus the addendum)

Every test of spec §12 step 6, as written, with these deltas:

- `TestCreateRound`: also assert `round_secrets.close_offset_micros` = `D` for each golden genesis
  (worked-example `9,410,000`, instant-crash `0`, empty-round `40,010,000`, in-progress = its config's
  `InitialState` `D`) and `round_admission_heads.close_not_before_unix_micro IS NULL`.
- `TestAdmitAndExecuteRound`: the `CloseRound` request has no not-before. Admit it; on
  `ErrClosureNotDue`, poll the database clock until it is ≥ `T0 + D`, then resubmit the same request
  (same command ID). Everything else unchanged.

New `close_bound_integration_test.go`, `TestCloseBoundRefusesEarlyCloseAndSettles` (one database,
`pgtest.Pool`, one `Executor`, one `admission.Sequencer` with `admission.DefaultConfig()` and
`admission.CrashV1Policy()`):
1. Fresh round; seed `seedWithDeadline(t, round, 30 s, 150 s)`; `cfg = v1.Config{round, seed, 1,
   1_000_000_000, 10}`; `D` from `InitialState(cfg)`. `CreateRound`; `round_secrets.close_offset_micros
   == D`; head bound NULL, state `open`.
2. Fund alice 100, bob 200. Admit `PlaceBet alice 100` (1), `PlaceBet bob 200` (2), system `StartRound`
   (3) → `T0`; head bound `== T0 + D`. `Drain` executes 3.
3. **Early close refused, window stays open:** admit system `CloseRound` → `*admission.ClosureNotDueError`
   (`errors.Is` `admission.ErrClosureNotDue`) with `T0 ≤ EffectiveTimeUnixMicro < T0 + D`; no admission
   row for its command ID; head `started`, `closed_sequence` NULL, `admitted_sequence` 3. Poll the
   database clock until `> T0`; admit `CashOut bob` (4) → admitted; `Drain` → accepted, `BetCashedOut`
   with `PayoutCredits ≥ 200`.
4. **No leak before the crash:** with `qD = "\"" + strconv.FormatInt(D, 10) + "\""` and `qB` the same
   for `T0 + D` (canonical JSON quotes int64 values): no outbox payload (sequences 0–4) and no byte of
   `eventstore.ExportHistory`'s output contains `qD` or `qB`; neither `err.Error()` nor
   `fmt.Sprintf("%+v", err)` of step 3's error contains `strconv.FormatInt(D, 10)` or
   `strconv.FormatInt(T0+D, 10)`.
5. **Closure at equality:** `seedAdmission` (LBT-12 helper, no hold) `CashOut alice` at exactly `T0 + D`
   (5). Admit `CloseRound` (6) → effective exactly `T0 + D`; head `closed`, `closed_sequence` 6. Admit
   `SettleRound` (7); head `sealed`.
6. **Settles normally:** `Drain` executes 5, 6, 7: 5 rejected `deadline_passed` (D-9, equality loses);
   6 accepted `RoundCrashed{ClosedAtUnixMicro: T0 + D, CrashDeadlineUnixMicro: T0 + D}`; 7 accepted with
   `BetLost` for alice and `RoundSettled{TotalBetCredits: 300, TotalPayoutCredits: bob's payout}`,
   `Outcome.Final`. Alice and bob `held` 0; one release per hold (2); round account balance 0; house
   balance `= 300 − payout`; Σ `debits_posted` = Σ `credits_posted`; `ExportHistory` then
   `verification.Verify(ModeVerify)` passes.

### Step 8: docs

- Spec §12 step 7: `docs/architecture/persistence.md` section "Execution (LBT-12)" (new §13 after §12
  "Admission") and the catalogue rows for `OX009` and `ledger_postings_event_key`; `README.md` Status adds
  LBT-12, Layout adds `internal/runtime/execution` and `internal/ledger`.
- Addendum, in `docs/architecture/persistence.md`:
  - the tables catalogue row for `round_secrets` mentions `close_offset_micros`; §6 "Seed exposure" adds
    that `round_secrets.close_offset_micros` and `round_admission_heads.close_not_before_unix_micro` are
    secret until the crash, like the seed;
  - §8 grant matrix: note that `0009` revokes `PUBLIC`'s default `EXECUTE` on the `SECURITY DEFINER`
    `orvex.admit_command` (the one ACL statement in the migrations); the engine role gets column-level
    `SELECT (round_id, config_secret)` on `round_secrets` and no `SELECT` on
    `round_admission_heads.close_not_before_unix_micro`;
  - §12 "Window columns": add `close_not_before_unix_micro`, `round_admission_heads_close_bound_check`
    and trigger `round_admission_heads_close_bound` (`OX005`);
  - §12 transition table: the `started`/`close` cell per §3.3;
  - §12 `orvex.admit_command`: say it is `SECURITY DEFINER` with `PUBLIC` revoked; step 7 becomes "for
    `start`: read `round_secrets.close_offset_micros` (missing: `corrupt`) and compute the bound; for
    `close`: `not_due` if the effective time is before the head's bound (equality admits; a missing bound
    is `corrupt`)"; step 9 adds "and, for `start`, the bound";
  - §12 outcomes table: remove `invalid_not_before`;
  - §12 grants: replace the list with §2 item 3's.
- Name the migration in the new §13 together with `0008`, and cite this plan and the spec.

## 6. Tests, complete list (all mandatory)

| Package | Tests |
|---|---|
| `internal/persistence/postgres` (unit) | `TestExecutionTxOptionsWithTimeouts` (new); OX009 in every code enumeration of `errors_test.go`; `TestEmbeddedMigrations` (edit) |
| `internal/persistence/postgres` (integration) | `TestLedgerPostingsOnePerEvent`, `TestChainRecordsRequireHoldRelease`, `TestExecutionTxOptionsWithTimeoutsApplied` (new); `TestCloseBoundColumns`, `TestAdmitCommandCloseBoundFailsClosed`, `TestAdmitCommandSecurityDefiner` (new); `TestMigrateUpDownClean`, `TestMigrateDownRefusesAuditData`, `TestAdmitCommandArgumentChecks` (edit); existing `TestLedgerOneTransactionPerRecord`, `TestMigrateEachStepReversible`, `TestMigrationsCreateOnlyOrvexObjects` (pass unchanged) |
| `cmd/crashctl` (integration) | `TestMigrateCLI`, `TestMigrateCLIStdoutFailure` (edit) |
| `internal/runtime/admission` (unit) | `TestAdmitValidation`, `TestAdmitOutcomeMapping`, `TestClosureNotDueError` (edit) |
| `internal/runtime/admission` (integration) | `TestClosureNotBefore`, `TestClosureNotBeforeEqualityAdmits`, `TestAdmissionStreamDrivesCrashRound` (rewrite); `TestStartWithoutCloseOffsetFailsClosed`, `TestAdmittingRoleCannotReadCloseBound` (new); `TestWindowTransitions`, `TestBarrierRejectsLateCommands`, `TestAdmitDuplicateAfterWindowClosed`, `TestBarrierSealIsFinal`, `TestBarrierConcurrentCloseAndCashOuts`, `TestFastPathSkipsLock`, `TestAbandonedAdmissionReleasesLock`, `TestAdmitCommandSessionSettings` (helper-level edits only) |
| `internal/ledger` (unit) | `TestHouseAccountID`, `TestRoundAccountID`, `TestTransactionID`, `TestIDsAreUUIDv8`, `TestIDsDistinct`, `TestCrashV1Descriptor`, `TestCrashV1Postings`, `TestCrashV1PostingsRejectsBadInput`, `TestCrashV1PostingsWorkedExample`, `TestLedgerImportsArePure` |
| `internal/runtime/execution` (unit) | `TestCrashV1PolicyDescriptor`, `TestCrashV1Checkpoint`, `TestCrashV1Final`, `TestCrashV1CloseOffset`, `TestFailClosedError`, `TestClassifyStatementError`, `TestClassifyCommitError`, `TestConflictDecision`, `TestBackoffBounds`, `TestDefaultConfig`, `TestNewValidation`, `TestExecuteNextValidation`, `TestTransitionReproducesGoldenRecords`, `TestTransitionFailClosed`, `TestTransitionHoldChecks`, `TestTransitionValidatesPostings`, `TestMaxRecordBytes` |
| `internal/runtime/execution` (integration) | `TestCreateRound`, `TestCreateRoundIdempotent`, `TestCreateRoundConflict`, `TestExecuteReproducesGoldenHistories`, `TestExecuteIdle`, `TestExecuteBetConsumesHoldAtExactBalance`, `TestExecuteRejectedBetReleasesHold`, `TestExecuteSnapshots`, `TestExecuteOutbox`, `TestColdLoadMatchesCache`, `TestColdLoadChecksSnapshots`, `TestFailClosedHaltsRound`, `TestSettlementResolvesEarlierCommands`, `TestConflictingPerRoundCommits`, `TestStaleCacheDiscarded`, `TestInjectedFailureLeavesNoPartialEffects`, `TestLedgerFailureRollsBack`, `TestLedgerFailureRetried`, `TestCommitFailureRollsBack`, `TestStaleFencingTokenBeforeStep`, `TestTakeoverWaitsForCommit`, `TestTakeoverBeforeCompareAndSet`, `TestCrashBeforeCommit`, `TestCrashAfterCommit`, `TestDuplicatePayoutImpossible`, `TestCrossRoundBalanceProtection`, `TestCrossRoundAdmissionWaitsForExecution`, `TestAdmitAndExecuteRound`, `TestCloseBoundRefusesEarlyCloseAndSettles` |

Acceptance mapping: the spec's §11 table, plus the addendum: early close refused and window open
(`TestCloseBoundRefusesEarlyCloseAndSettles` step 3, `TestClosureNotBefore`); closure at exactly `T0 + D`
(`TestClosureNotBeforeEqualityAdmits`, step 5); settles normally (step 6); admitting role cannot read the
offset (`TestAdmittingRoleCannotReadCloseBound`).

If a golden reproduction or any test contradicts the spec or this plan, find which side is wrong. Never
edit a golden fixture, weaken an invariant, or change an expected value to match output.

## 7. Verification (each must exit 0; judge by exit status)

```sh
make test
make test GOTESTFLAGS=
make lint
go vet ./...
go vet -tags integration ./...
make build
make golden-check
make test-integration
time go test -race -tags integration -count=5 -run 'Conflicting|StaleCache|CrossRound|Takeover|Crash' ./internal/runtime/execution/
```

- The `-count=5` run is the spec's flakiness check. **Report its exit status and its wall-clock duration
  (`time`'s `real`) in the build report.** It must pass all five times.
- Unchanged-files check (both must print nothing):

  ```sh
  git status --porcelain -- docs/adr internal/game internal/audit internal/canonjson go.mod go.sum Makefile .golangci.yml .github AGENTS.md internal/persistence/eventstore internal/persistence/postgres/pgtest migrations/0001_foundation.up.sql migrations/0001_foundation.down.sql migrations/0002_rounds_admissions.up.sql migrations/0002_rounds_admissions.down.sql migrations/0003_chain_execution.up.sql migrations/0003_chain_execution.down.sql migrations/0004_snapshots.up.sql migrations/0004_snapshots.down.sql migrations/0005_ledger.up.sql migrations/0005_ledger.down.sql migrations/0006_outbox.up.sql migrations/0006_outbox.down.sql migrations/0007_admission_window.up.sql migrations/0007_admission_window.down.sql
  git status --porcelain -- cmd | grep -v '_test.go$'
  ```
  (the second command exits 1 when nothing matches; that is the passing case).
- `git status --porcelain -- internal/runtime/admission` lists only `sequencer.go`, `errors.go`,
  `doc.go`, test files and the new `role_integration_test.go`.

## 8. Out of scope

Everything the spec's §13 lists: the outbox relay, wire format and gateway (LBT-13); lease acquisition,
renewal, takeover, failover, the engine loop, the scheduler, roles and grants, snapshot `state` bytes,
halted-round operations (LBT-14); load tests, production failpoints, metrics, tuning (LBT-15); the audit
ledger stage and history format v2 (LBT-18); `crashctl audit export` (LBT-19). This run also creates no
production role and grants nothing in migrations (§2 item 4 is a revoke only), adds no backfill for
pre-`0009` rounds, and does not edit the LBT-11 spec, the ADRs or `docs/changes/`.

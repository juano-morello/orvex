# Durable per-round PostgreSQL admission sequencer (LBT-11)

Status: **APPROVED (2026-10-14).** The engineer approved S-1 to S-14 as written, with no edits, and
accepted every point of section 2, including both deviations from LBT-10 (S-5 and S-7).

- Linear: [LBT-11](https://linear.app/lbtworks/issue/LBT-11/implement-durable-per-round-postgresql-admission-sequencer),
  milestone M2 · Durable Execution. Blocked by LBT-10; blocks LBT-12.
- Linear text: "Per-round locking, unique command IDs, effective DB timestamps after lock, durable sequence,
  retry lookup, deadline admission rules and closure barrier. Acceptance: concurrent duplicate submissions
  create one record; hot-round ordering is unique; admission ACK follows commit. Reference ADR-002/005."
- Governing ADRs: ADR-002 (primary), ADR-005 (Transaction A), ADR-001, ADR-004, ADR-006, plus ADR-003 and
  ADR-007 (mirrors in `docs/adr/`).
- Approved inputs:
  - "LBT-10 spec": the approved PostgreSQL schema and migration spec (decisions P-1 to P-26). It is being
    implemented in parallel and is not on `main`. The copy read for this plan is outside the repo and is
    not copied in.
  - "LBT-9 spec": `docs/specs/8bebb70f_canonical-codec-audit-cli.md` (A-1 to A-20), merged.
  - "Math spec": `docs/specs/e96d124d_crash-v1-math-spec.md` (D-1 to D-14).
  - "Lifecycle spec": `docs/specs/712614a2_crash-lifecycle-rules.md` (LBT-8, §2 items 1 to 13).

Decision IDs are `S-n`, so they do not clash with `D-n`, `A-n` or `P-n`.

## 0. What this run delivers

- This document is the deliverable. The engineer approves its copy under `docs/specs/`.
- **Builder, this run: make no repository changes.** Do not write Go code, SQL, migrations or CI changes,
  and do not touch `go.mod`, `go.sum`, `Makefile`, `.golangci.yml`, `README.md`, `AGENTS.md` or
  `docs/adr/`. The gates must still pass on the unchanged tree: `make test`, `make lint`, `go vet ./...`,
  `make build`, `make golden-check`.
- After approval, section 10 is the implementation plan.
- **Implementation precondition: LBT-10 is merged on `main` and the worktree is based on it.** Check that
  these exist:
  - `migrations/0001_foundation.up.sql` … `migrations/0006_outbox.up.sql` and `migrations/migrations.go`;
  - `internal/persistence/postgres` with `AdmissionTxOptions`, `SnapshotReadTxOptions`, `SQLState`,
    `IsRetryable` and the `Code*` constants;
  - `internal/persistence/postgres/pgtest` with `Run`, `MigratedDatabase`, `Pool`, `WaitUntilBlocked`;
  - the `test-integration` Makefile target.

  If they do not, stop and report "blocked on LBT-10". If LBT-10's merged names differ from the LBT-10
  spec, use the merged names: this spec fixes behavior, not identifiers. If LBT-10 merged a different
  number of migrations, LBT-11's migration takes the next free number. If the merged schema differs from
  LBT-10 §5.2 or §5.6 in anything this spec relies on (the `round_admission_heads`, `admissions`,
  `ledger_accounts` and `ledger_holds` columns, `admissions_command_id_key`,
  `ledger_accounts_floor_check`, the hold trigger), stop and report.

## 1. Current state (verified while planning)

- `main` is at `f7a8aed`. Toolchain: go1.26.5. Docker Engine 29.8.2 with the pinned
  `postgres:18.6-trixie` image (LBT-10 P-1) present locally. `go.mod` requires only
  `github.com/cockroachdb/apd/v3 v3.2.3`.
- LBT-9 is merged: `internal/canonjson`, `internal/audit/{chain,replay,verification}`, the crash/v1
  `Codec`, codec methods on `game.Adapter`, and `crashctl audit`. `chain.EncodeGenesis`,
  `chain.EncodeCommand` and `chain.LinkHash` exist.
- There is no `migrations/`, `internal/persistence/` or `internal/runtime/` on `main`. LBT-10 adds the
  first two.
- The approved LBT-10 copy is headed "PROPOSED" and its boxes are unticked. This spec treats P-1 to P-26
  as approved, as the request states. If approval changed P-14, P-15, P-21 or P-23, decisions S-3, S-4,
  S-5, S-8 and S-11 need review.
- crash/v1 facts this spec relies on (`internal/game/crash/v1/decide.go`):
  - `Decide` checks, first failure wins: phase validity, settled, command type, authority, `t < 0`,
    phase, then command-specific checks.
  - Authority: `PlaceBet` and `CashOut` need `AuthorityPlayer`; `StartRound`, `CloseRound` and
    `SettleRound` need `AuthoritySystem`.
  - `CashOut`: `t <= T0` → `not_after_start`; `t >= T0 + D` → `deadline_passed` (D-9, equality loses).
  - `CloseRound`: `t < T0 + D` → `crash_not_due`; otherwise `RoundCrashed`.
  - `StartRound` needs BETTING and `t <= MaxStartMicros` (`MaxInt64 − 153,510,000`). `SettleRound` needs
    CRASHED.
  - `PlaceBet`: non-empty player, amount within the round's config limits, which lie inside
    `[v1.MinBetCredits, v1.MaxBetCredits] = [1, 1,000,000,000]` (D-7); no second bet; bet cap.
  - `D` and `T0 + D` are secret until `RoundCrashed` (math spec §7). `State.CrashDeadlineUnixMicro` holds
    `T0 + D` after `RoundStarted`.
- The golden history `worked-example.jsonl` admits, in order: bets, a `CashOut` before start, `StartRound`
  (9), cash-outs, a `CloseRound` (16, rejected `crash_not_due`), two cash-outs (17, 18), a second
  `CloseRound` (19), a cash-out (20), `SettleRound` (21) and a `PlaceBet` (22). See §2 item 8.
- Prototypes on the pinned PostgreSQL 18.6 image, in a throwaway container outside the repo, with a
  minimal head and admission table pair and a PL/pgSQL function shaped like §5.3:
  1. A VOLATILE PL/pgSQL function under READ COMMITTED that waits on `SELECT … FOR UPDATE` sees, in its
     next statement, an admission that the lock holder committed meanwhile. It returned the duplicate
     found under the lock.
  2. `set_config('lock_timeout', '300ms', true)` inside the function bounds the function's own later
     `FOR UPDATE` wait. The wait failed with `55P03`, and the session default was still `0` afterwards.
  3. `set_config('idle_in_transaction_session_timeout', '400ms', true)` inside the function: a client that
     called the function and never committed was terminated (FATAL) after 400 ms. Its insert rolled back,
     and the admission waiting behind it proceeded about 200 ms after it started.
  4. The same command ID in two rounds at once: the second insert waited on the unique index until the
     first committed, then failed with `23505`. `lock_timeout` also bounds that wait (`55P03`).
  5. Clamping to a previous effective time of `4000000000000000` (in the future) returned exactly that
     value.
  6. A test-only `DEFERRABLE INITIALLY DEFERRED` constraint trigger that calls `pg_advisory_xact_lock`
     holds `COMMIT`. While it waited, `pg_locks` showed one ungranted advisory lock and other sessions saw
     0 rows. After the lock was released, `COMMIT` returned and the row was visible.
  7. `(EXTRACT(EPOCH FROM '2025-10-09 08:53:20.123457+00'::timestamptz) * 1000000)::bigint` is
     `1760000000123457` (confirms LBT-10 §1).

## 2. ADR alignment, conflicts and flagged interpretations

No decision contradicts an accepted ADR or a math-spec (D-1 to D-14) or LBT-9 (A-1 to A-20) decision.
Two decisions depart from the wording of LBT-10 P-23 and §6 and need explicit approval (items 1 and 2).
AGENTS.md "Flag conflicts explicitly": copy this section into the implementation PR.

1. **LBT-10 P-23 and §6, "first statement locks `round_admission_heads`" (deviation, needs approval).**
   S-5 runs these steps inside the same function call, before the lock:
   - three `set_config` calls;
   - a lock-free command-ID lookup;
   - a lock-free read of `rounds` and the admission head.

   These reads see only immutable rows (`admissions`, `rounds`) and a value that only moves forward (the
   admission state). Every decision they could make is checked again under the lock. The clock is read
   only after the lock, which is P-23's stated rationale and ADR-002's requirement. The alternative,
   locking first, is in S-5.
2. **LBT-10 §6 item 5 and ADR-002 "database clock read after acquiring the lock" (interpretation, needs
   approval).** S-7 sets the effective time to `max(clock after lock, previous admission's effective time)`.
   It equals the clock reading unless the database clock stepped backwards.
3. **ADR-002 open validation "rejection evidence" (deferred).** S-13: rejections at admission write
   nothing in LBT-11.
4. **ADR-002 open validations.**
   - Closed here:
     - the exact admission SQL (§5.3, as normative behavior);
     - the late-command policy (S-8, S-9);
     - duplicate IDs and unknown commit outcomes (S-6, S-12);
     - deadline equality (S-9).
   - Not closed here:
     - benchmarking contention and clock behavior (LBT-15);
     - failover guarantees (LBT-14). S-4 states what holds whichever process admits.
5. **ADR-003 "the runtime owns admission" and "avoid premature abstractions" (interpretation).** The
   admission window must know which crash/v1 commands start, close and settle a round, and how much a bet
   reserves. That knowledge lives in a runtime-side policy for crash/v1 (S-10), selected by the registry
   descriptor. The game contract and the crash/v1 package do not change.
6. **ADR-004 "Crash is determined by the committed deadline even if processing is delayed" and D-9
   (alignment).**
   - Timing eligibility (`0 < e < D`, equality loses) stays only in `Decide`.
   - The sequencer never compares a cash-out time with the deadline (S-9).
   - The closure guard (S-8) only keeps the window from closing before `T0 + D`, using a bound that the
     trusted scheduler supplies. At equality the round closes. This matches lifecycle spec item 7, where
     `CloseRound` is accepted at `t >= T0 + D`.
7. **Lifecycle spec item 9 (alignment).** The bet cut-off stays the `StartRound` admission sequence,
   enforced by `Decide` (`wrong_phase`). The sequencer does not reject late bets.
8. **Golden histories (interpretation, no change).**
   - `worked-example` contains admissions the sequencer would refuse:
     - records 17, 18 and 20 follow an admitted `CloseRound` (16);
     - 19 is a second `CloseRound`;
     - 22 follows `SettleRound` (21).
   - The barrier is a runtime admission property, not a rules or verifier property. `Decide` and the
     verifier still accept such histories (A-7, A-13), and the goldens stay as they are (A-18, A-20).
   - A verifier check for the barrier would need admission classes in the chain (a chain v2). That is not
     proposed.
9. **LBT-9 §2 item 7 (alignment).** LBT-9 left effective-time monotonicity to LBT-11. S-7 makes effective
   times non-decreasing within a round.
10. **LBT-10 P-15, P-21 and §9 (alignment).**
    - Rejections at admission create no row.
    - Holds are placed at admission.
    - Player IDs containing U+0000 are rejected.
    - Command bytes are always codec output, because the sequencer encodes typed commands itself (S-2).
11. **LBT-10 P-5 and approved LBT-10 tests (consequence).** The down migration of `0007` refuses
    (`OX003`) when `round_admission_heads` holds rows (P-5: "future downs that drop columns follow the
    same rule"). LBT-10 tests that count migrations or walk the down path change:
    - `TestMigrateCLI`: six becomes seven;
    - `TestMigrateDownRefusesAuditData`: the down now stops at `0007`;
    - `TestEmbeddedMigrations`, only if it asserts the total count.

    §10 step 1 gives the exact edits.
12. **LBT-10 P-11 (reuse).** A window regression raises the existing `OX005` (`CodeHeadRegression`). No
    new SQLSTATE is added.
13. **LBT-10 P-13 (input to LBT-14 grants).**
    - The admitting role needs:
      - EXECUTE on `orvex.admit_command`;
      - SELECT on `rounds`, `round_admission_heads`, `admissions` and `ledger_accounts`;
      - INSERT on `admissions` and `ledger_holds`;
      - UPDATE on `round_admission_heads` and `ledger_accounts` (the latter through the hold trigger);
      - SELECT on `chain_records`, for `Lookup` only.
    - It needs nothing on `round_secrets` or `round_snapshots`, so the gateway restriction of P-13 holds.
14. **ADR-005 "commit before acknowledging admitted status" (alignment, strengthened).** S-3 turns
    `synchronous_commit` back on when a session default is `off`, so a returned `COMMIT` is flushed.
15. **AGENTS.md dependencies.** No new module. pgx v5 and testcontainers-go come from LBT-10 (P-2, P-7),
    and backoff jitter uses `math/rand/v2`.
16. **ADR-007 layout.** `internal/runtime/admission` is in the proposed layout. No new top-level
    directory.

| ADR clause | Where satisfied |
|---|---|
| ADR-002: authoritative per-round admission sequence and effective timestamp; PostgreSQL row-lock sequencing; DB clock read after the lock | S-4, S-7, §5.3 |
| ADR-002: gateway receive time is diagnostic only | S-4 (ordering) |
| ADR-002: cash-out eligible only when effective time is strictly before the deadline and after start | S-9 (left to `Decide`, D-9) |
| ADR-002: admission ACK means durable receipt, not execution | S-2, S-3 |
| ADR-002: deduplicate retries by stable command ID; duplicate IDs, unknown commit outcomes, deadline equality defined | S-6, S-12, S-9 |
| ADR-002: same serialization boundary for admission and round closure/settlement barrier | S-8 |
| ADR-002: sequencer replaceable | S-2 (API independent of the lock mechanism) |
| ADR-005 Transaction A: serialize per round; dedupe; admission window; sequence and effective time; immutable admission; commit before ACK; closure barrier through the same boundary | S-3 to S-8, §5.3 |
| ADR-005 failure semantics: before commit a retry may admit; after commit a retry returns the original | S-6, S-12 |
| ADR-005 cross-round balances: account-level reservation checks; settlement barrier | S-11, S-8 |
| ADR-004: equality with the crash deadline loses; crash fixed by the committed deadline; settlement waits for the admission barrier | S-9, S-8 |
| ADR-001: preserve original commands and effective times; per-round consistency boundary | S-3, S-7, §7 |
| ADR-006: real PostgreSQL integration tests; duplicate admission, ambiguous commit, settlement barrier, cross-round overspending | S-14, §9 |
| ADR-007: `internal/runtime/admission`, `migrations/`, `make test-integration` | S-1, S-14 |

## 3. Decision summary

Tick a box to approve, or edit the value. The implementation run uses approved values only.

| ID | Decision | Proposed value | Approve |
|---|---|---|---|
| S-1 | Package, layout, migration | `internal/runtime/admission`; migration `0007_admission_window`; no new module; library only | [x] |
| S-2 | Go API and ACK semantics | `Sequencer.Admit(ctx, Request) (Admission, error)` returns an `Admission` only after `COMMIT` succeeded or for an already committed row; `Lookup` reads the execution result separately; typed commands are encoded by the round's codec | [x] |
| S-3 | Shape of Transaction A | one explicit READ COMMITTED transaction (`AdmissionTxOptions`) calling one PL/pgSQL function `orvex.admit_command`, then `COMMIT` or `ROLLBACK`; `synchronous_commit` never `off` | [x] |
| S-4 | Per-round serialization | `SELECT … FOR UPDATE` on the round's `round_admission_heads` row; order = lock grant order; transaction-local `lock_timeout` 3 s and `idle_in_transaction_session_timeout` 1 s; at most one account row locked after the head; in-process gate of 4 in-flight transactions per round | [x] |
| S-5 | Lock-free fast path | command-ID lookup and window read before the lock; every check repeated under the lock (P-23 deviation) | [x] |
| S-6 | Command IDs, dedupe, retry lookup | global UUID key; same round and same bytes return the original admission, even after the window closed; otherwise `ErrCommandIDReused`; rejections are not remembered; unknown outcomes resolve by resubmitting the same ID | [x] |
| S-7 | Effective time | `orvex.unix_micro(clock_timestamp())` read once, first thing after the lock; clamped to at least the previous admission's effective time | [x] |
| S-8 | Admission window and closure barrier | head states `open` → `started` → `closed` → `sealed`, driven by system `StartRound`/`CloseRound`/`SettleRound` admissions; `CloseRound` needs effective time ≥ caller-supplied `NotBeforeUnixMicro` (= `T0 + D`); sealed means no more admissions, enforced by a `CHECK` | [x] |
| S-9 | Admission vs `Decide` | admission rejects only: window closed or sealed, lifecycle order, closure not due, insufficient credits, invalid requests; every game rule, including D-9 timing and deadline equality, stays in `Decide` | [x] |
| S-10 | crash/v1 admission policy | class from (command type, authority); hold = `BetCredits` for player `PlaceBet`s with a non-empty player and `1 ≤ BetCredits ≤ 1,000,000,000`; U+0000 in a player ID rejected | [x] |
| S-11 | Credit holds | lock the player account `FOR NO KEY UPDATE` after the head; `insufficient_credits` when no account or available < hold; insert `ledger_holds` after the admission | [x] |
| S-12 | Errors, SQLSTATEs, retries | mapping table in S-12; at most 3 attempts with full-jitter backoff (5 ms doubling, cap 50 ms) for `40001`/`40P01`, connection loss and ambiguous commits; never retry `55P03`, business rejections or constraint bugs | [x] |
| S-13 | Rejection evidence | none stored in LBT-11; the reason is returned to the caller only | [x] |
| S-14 | Test strategy | real PostgreSQL through `pgtest` under `make test-integration`; fault injection with a DB wrapper and a test-only deferred trigger; exact effective times through the S-7 clamp | [x] |

## 4. Decisions

### S-1. Package, layout and migration

**Proposal.**
- New package `internal/runtime/admission`, from ADR-007's `internal/runtime/{admission,…}`.
  - It imports `internal/game`, `internal/game/crash/v1` (policy only), `internal/persistence/postgres`
    (transaction options, `SQLState`, `IsRetryable`) and pgx v5 (`pgx`, `pgconn`).
- New migration pair `migrations/0007_admission_window.{up,down}.sql`. LBT-10 §9 reserves "its own
  migrations (0007 onward)".
- The package is a library. Wiring it into `cmd/gateway` (player commands) and `cmd/engine` (lifecycle
  commands) belongs to LBT-12 to LBT-14.
- No change to `internal/game/**`, `internal/audit/**`, `internal/canonjson/**` or migrations `0001` to
  `0006`. No new module.

**Rationale.** It follows ADR-007's layout and LBT-10's split: LBT-11 owns the admission head (P-14).
Binaries stay thin (AGENTS.md).

**Alternatives.**
- `internal/persistence/postgres/admission`: puts game-specific policy into the persistence layer.
- The policy inside `internal/game/crash/v1`: the rules package would learn runtime concepts, and its
  import allowlist (`TestNoIOImports`) would need widening.
- Wiring into `cmd/gateway` now: transport is LBT-13.

### S-2. Go API and ACK semantics

**Proposal.** §6 is normative. In short:
- `Admit(ctx, Request) (Admission, error)`.
  - The `Request` carries the round ID, the game and rules version, a client-chosen command ID, the
    rules version's **typed** command value, and, for a closing command only, `NotBeforeUnixMicro`.
  - The sequencer encodes the command with the round's adapter (`EncodeCommand`) and classifies it with
    the rules version's policy (S-10).
- `Admission` is the durable receipt: round ID, command ID, sequence, effective time, and `Duplicate`.
  - `Admit` builds it only after `tx.Commit` returned nil, or from a row that was already committed (a
    duplicate).
  - It carries no execution result.
- A rejection at admission is a `*RejectedError` (`errors.Is(err, ErrRejected)`) with a stable reason
  code (S-9). Every other failure wraps a sentinel error (S-12).
- `Lookup(ctx, commandID) (Status, error)` returns the admission and its execution outcome: pending, or
  accepted, or rejected with `Decide`'s reason code, read from LBT-10's `chain_records`. `ErrNotFound`
  from `Lookup` is not definitive for a command still in flight (S-6).
- `Admission.Input()` returns `game.Input{Sequence, EffectiveTimeUnixMicro}`, the value LBT-12 hands to
  `Decide`.

**Rationale.**
- ADR-002: "Admission acknowledgment means durable receipt, not successful execution."
- ADR-005: "commit before acknowledging admitted status."
- Encoding inside the sequencer makes "command bytes that are not codec output" (LBT-10 §9) impossible,
  and the same value always encodes to the same bytes, which dedupe compares (S-6).

**Alternatives.**
- `Admit` waits for the execution result: couples Transactions A and B, which ADR-002 separates, and adds
  the execution backlog to admission latency.
- Canonical bytes in the request: the sequencer would have to decode and re-encode to prove they are
  canonical, and the gateway needs the codec anyway.
- Class and hold supplied by the caller: the reserved amount could disagree with the command bytes.

### S-3. Shape of Transaction A

**Proposal.**
- Begin with `postgres.AdmissionTxOptions()` (READ COMMITTED, READ WRITE; P-23).
- Run exactly one statement: `SELECT outcome, reason, sequence, effective_time_unix_micro FROM
  orvex.admit_command(…)`.
- `COMMIT` only when `outcome = 'admitted'`. `ROLLBACK` for every other outcome, because nothing was
  written.
- `orvex.admit_command` is PL/pgSQL, `VOLATILE`, `SET search_path = pg_catalog` (P-11), created by `0007`.
  §5.3 is its normative behavior.
- Its first statements set three transaction-local settings with `set_config(…, true)`:
  - `lock_timeout`;
  - `idle_in_transaction_session_timeout`;
  - `synchronous_commit = 'on'`, only when the current value is `'off'`.

**Rationale.**
- The head lock is held from the lock statement to the end of `COMMIT`.
  - A Go-driven sequence of six to eight statements would hold it across as many network round trips.
  - The function holds it for one round trip plus the commit.
  - Hot-round throughput is bounded by lock hold time (ADR-006 "one hot round").
- One SQL object holds the order ADR-002 cares about (lock, then clock) and can be reviewed in one place.
- `VOLATILE` gives each statement a fresh snapshot. After waiting for the lock, the function therefore
  sees admissions committed meanwhile (prototype 1).
- With `synchronous_commit = off`, `COMMIT` can return before the WAL is flushed, so the ACK would precede
  durability.
  - `local`, `remote_write`, `on` and `remote_apply` all flush locally first.
  - So only `off` is overridden, and a stronger operator setting is kept.

**Alternatives.**
- Go-driven statements: plainer Go, but N round trips under the lock.
- One data-modifying CTE: one round trip without PL/pgSQL. PostgreSQL does not fix the evaluation order
  inside one statement, though, so `clock_timestamp()` could be evaluated before the row lock is granted,
  which breaks ADR-002.
- An autocommit call without `BEGIN`: saves a round trip, but the isolation then comes from the server
  default (P-23 §6 item 1).
- A pgx batch of `BEGIN`, the call and `COMMIT` in one round trip: error handling after a failed
  statement in a batch is murky. LBT-15 may measure it as an optimization.
- A procedure that commits internally: the ACK must follow a commit the client observed.

### S-4. Per-round serialization

**Proposal.**
- **Lock target.** The round's `round_admission_heads` row, locked by `SELECT … FOR UPDATE` (P-14, P-23).
  `StartRound`, `CloseRound` and `SettleRound` take the same lock through the same function (ADR-002
  "same serialization boundary").
- **Ordering.**
  - The admission sequence order is the order in which transactions are granted that lock.
  - It is total, unique (primary key), gapless (LBT-10 foreign key), and effective times are
    non-decreasing along it (S-7).
  - A caller that submits commands one after another, waiting for each ACK, gets increasing sequences.
  - Nothing else is promised: not gateway receive order (ADR-002), not client send order, not FIFO among
    concurrent waiters.
- **Lock order inside A.** First the head row, then at most one ledger account row (S-11). Never an
  execution head (LBT-10 §6 item 2).
  - Two admissions can then only wait on each other through a head or through one account, never in a
    cycle.
  - Transaction B never takes an admission head, so A and B cannot deadlock either.
- **Timeouts**, set transaction-locally by the function before the lock:
  - `lock_timeout = Config.LockTimeout` (default 3 s).
    - It bounds the head wait, the account wait and a unique-index wait on a cross-round duplicate
      (prototypes 2 and 4).
    - Expiry is `55P03`, returned as `ErrBusy`.
  - `idle_in_transaction_session_timeout = Config.IdleInTransactionTimeout` (default 1 s).
    - If the client disappears between the function's return and `COMMIT`, the server ends that session
      and releases the head lock within 1 s (prototype 3).
    - Without it, a vanished client stalls the round until TCP keepalive notices.
  - `Config.LockTimeout` must exceed `Config.IdleInTransactionTimeout`, so a waiter outlives an abandoned
    holder.
  - The caller's `ctx` bounds everything else: pool wait and network. A `ctx` that ends before `COMMIT`
    is sent leaves nothing admitted.
- **In-process gate.**
  - At most `Config.MaxInFlightPerRound` (default 4) `Admit` calls per round per `Sequencer` hold a pool
    connection at once. Further calls wait in Go and honor `ctx`.
  - Correctness never depends on the gate.
- **Processes.** Any number of processes may admit for a round at the same time. Correctness relies only
  on the database, not on ownership. LBT-14 failover does not change admission correctness.

**Rationale.**
- ADR-002 names row-lock sequencing as the initial mechanism.
- P-14 created the separate lock row so admissions and executions do not contend.
- The timeouts are measured behavior (§1).
- The gate addresses ADR-006's "connection pool saturation": without it, one hot round's waiters can take
  every pooled connection and starve the other rounds served by the same process.

**Alternatives.**
- `pg_advisory_xact_lock` on a round key: the lock is not a row, a 64-bit key derived from a UUID can
  collide, and the state it guards lives on the head row anyway.
- `UPDATE … RETURNING` as the lock statement: advances the head before the dedupe and window checks, so a
  rejection must undo a write.
- `NOWAIT` or `SKIP LOCKED`: turns every contended admission into a retry (ADR-006 "retry storms").
- SERIALIZABLE: P-23 rejects it.
- No gate, deferred to LBT-15: simpler, but one hot round can starve the pool.
- A gate of capacity 1: serializes in Go as well and adds the `BEGIN` and call round trips to the serial
  path, lowering hot-round throughput.

### S-5. Lock-free fast path

**Proposal.** Inside the function, after `set_config` and before the lock:
1. Look up the command ID in `admissions` (unique index). If a row exists, return `duplicate` or
   `conflict` without the lock (S-6).
2. Read `rounds` left-joined with `round_admission_heads`, without a lock.
   - No round row: `not_found`.
   - No head row: `corrupt`.
   - The game or rules version differs from the request: `rules_mismatch`.
   - State `sealed`: rejected `round_sealed`.
   - State `closed` and class `ordinary`: rejected `round_closed`.

Otherwise take the lock and repeat every check on the locked row (§5.3).

**Rationale.**
- It is safe: `admissions` and `rounds` rows are immutable (P-13), and the window only moves forward (S-8
  trigger). A lock-free answer can never be contradicted later.
- The gain: retries and the cash-out burst right after the crash (ADR-006 "cash-out burst near crash")
  are answered without queuing on the hot lock.
- It departs from P-23's wording ("first statement locks"). P-23's rationale, the clock read after the
  lock, holds: nothing reads the clock before the lock.

**Alternatives.**
- Lock first, the literal P-23: every retry and every late command queues behind the hot lock.
- The fast path as a separate read transaction before A: one more round trip on every admission.

### S-6. Command IDs, deduplication and retry lookup

**Proposal.**
- A command ID is a client-chosen UUID, never the nil UUID (P-12), and globally unique (P-15,
  `admissions_command_id_key`). The caller reuses it on every retry of the same command.
- **Match rule.** An existing admission with the request's command ID matches iff it has the same
  `round_id` and byte-identical canonical command bytes.
  - A match returns the original `Admission` with `Duplicate = true`.
  - Anything else returns `ErrCommandIDReused`, and nothing is written.
- Dedupe runs before the window checks. A retried admitted command gets its original admission even after
  the window closed or sealed.

| Earlier attempt with this ID | A retry with the same ID returns |
|---|---|
| admitted (committed) | the original `Admission` (same sequence and effective time), `Duplicate = true`, and no second hold |
| rejected at admission (no row) | a fresh evaluation. `round_closed` and `round_sealed` can only repeat (the window never moves back). `lifecycle_order` (for example a close before the start was admitted), `insufficient_credits` and `ErrClosureNotDue` may now admit |
| unknown outcome (ambiguous commit, connection lost, `ctx` ended during `COMMIT`) | the original `Admission` if that attempt committed, otherwise a fresh evaluation. If that attempt is still committing, the retry waits on the head lock, which is released only after the commit is visible |
| still in flight (concurrent) | the retry waits on the head lock, then returns the original (prototype 1), or admits if the first attempt rolled back |
| same ID with different bytes or a different round | `ErrCommandIDReused` |

- The sequencer retries ambiguous outcomes itself within `MaxAttempts` (S-12) before returning
  `ErrOutcomeUnknown`.
- The caller's rule: on `ErrOutcomeUnknown`, `ErrUnavailable`, `ErrBusy` or a `ctx` error, resubmit the
  same `Request` with the same ID. Never treat these as rejections.
- `Lookup(commandID)` is the read-only status API for a caller that no longer holds the request. Its
  `ErrNotFound` means "no committed admission yet", not "rejected".
- The execution outcome of an admitted command comes from `chain_records`, written by LBT-12. LBT-13 uses
  it for ADR-005 "after execution commit but before response: return existing durable result".

**Rationale.**
- ADR-002: "Deduplicate retries by stable command ID."
- ADR-005: "After admission commit: retry returns original admission."
- Comparing bytes is exact, because codec output is canonical (A-1, A-2).

**Alternatives.**
- Compare only the round: a client that sends different content under one ID would be told it was
  admitted.
- Per-round uniqueness: P-15 chose global uniqueness.
- Remember rejections: S-13.

### S-7. Effective time

**Proposal.**
- Read `clock_timestamp()` exactly once, in the first statement after the lock is granted.
- Convert with `orvex.unix_micro(ts)`, a new function computing `(EXTRACT(EPOCH FROM ts) * 1000000)::bigint`.
  The conversion is exact (prototype 7; LBT-10 §6 item 5).
- Never use `now()`, `transaction_timestamp()` or `statement_timestamp()`.
- `effective = max(clock µs, effective time of admission admitted_sequence of this round)`. The first
  admission of a round has no previous value.
- The value is stored in `admissions.effective_time_unix_micro`, returned in `Admission`, and becomes
  `game.Input.EffectiveTimeUnixMicro` in LBT-12.

**Rationale.**
- ADR-002: the database clock after the lock, not the transaction start.
- The clamp keeps sequence order and time order consistent when the database clock steps backwards, for
  example an NTP step or a failover to a host with a slower clock.
  - Every command admitted after an admitted `CloseRound` then has a time ≥ the close's time ≥ `T0 + D`.
  - That is what makes the S-8 barrier outcome-neutral.
- The clamp changes a value only when the clock went backwards. It never moves time beyond the last real
  reading.
- It costs one primary-key lookup under the lock.

**Alternatives.**
- The raw clock: simpler, but order and time can disagree, and LBT-9 explicitly left monotonicity to
  LBT-11.
- A `last_effective_time` column on the head: saves the lookup but duplicates a value already in
  `admissions`.
- Refuse admissions while the clock is behind: stalls the round on a clock step.
- Also clamp to the round's creation time: that time's clock source is round creation's (LBT-12), and
  sequence order already puts the genesis record first.

### S-8. Admission window and closure barrier

**Proposal.**
- New columns on `round_admission_heads` (§5.1):
  - `admission_state`: `open` | `started` | `closed` | `sealed`, default `open`;
  - `started_sequence`, `closed_sequence`, `sealed_sequence`: null until set, then set once.
- Policy classes (S-10): `ordinary`, `start`, `close`, `settle`.
- Transitions, decided under the head lock:

| State | `ordinary` | `start` | `close` | `settle` |
|---|---|---|---|---|
| `open` | admit | admit → `started` | rejected `lifecycle_order` | rejected `lifecycle_order` |
| `started` | admit | rejected `lifecycle_order` | admit → `closed` if effective ≥ `NotBeforeUnixMicro`, else `ErrClosureNotDue` | rejected `lifecycle_order` |
| `closed` | rejected `round_closed` | rejected `lifecycle_order` | rejected `lifecycle_order` | admit → `sealed` |
| `sealed` | rejected `round_sealed` | rejected `round_sealed` | rejected `round_sealed` | rejected `round_sealed` |

- **Closure guard.**
  - A `close` request carries `NotBeforeUnixMicro`: the absolute crash deadline `T0 + D` that the
    scheduler reads from the executed state (`State.CrashDeadlineUnixMicro`).
  - The sequencer requires `NotBeforeUnixMicro ≥ T0`, where `T0` is the effective time of admission
    `started_sequence`. Otherwise it returns `ErrInvalidRequest`.
  - It admits the close only if `effective ≥ NotBeforeUnixMicro`; equality admits. Otherwise it returns
    `*ClosureNotDueError` holding both times, and nothing is written.
- **What the barrier guarantees.**
  1. *One boundary.* `StartRound`, `CloseRound` and `SettleRound` are admitted through `Admit`, under the
     same head lock as every other command (ADR-002, ADR-005).
  2. *Settlement resolves exactly the earlier commands.* `SettleRound`'s sequence `s` is the round's last
     admission. LBT-12 executes in sequence order, so commands `1..s-1` are resolved before settlement
     (ADR-004 "settlement waits for the durable admission barrier"; ADR-005 "prevent finalization while
     earlier admitted commands remain unresolved").
  3. *Finality.* Once `sealed`, `admitted_sequence = sealed_sequence` forever, enforced by a `CHECK`
     together with the forward-only triggers.
  4. *Outcome neutrality.* With S-7, a command refused as `round_closed` would have reached `Decide` after
     an accepted `CloseRound`, with effective time ≥ `T0 + D` in phase CRASHED, and been rejected there
     anyway. The barrier never denies an eligible cash-out (ADR-002 fairness). It only saves a chain
     record per late command.
  5. *No stuck window.* A `close` is admitted only after an admitted system `StartRound`, and only at
     `t ≥ T0 + D`. `Decide` accepts a system `StartRound` in BETTING for any real clock value, so the
     admission state never runs ahead of the game phase into a state the rules refuse.
- **Database backstops** (`0007`): a state `CHECK`, a window-consistency `CHECK`, foreign keys to
  `admissions`, and a forward-only trigger (`OX005`).

**Rationale.**
- ADR-002, ADR-004 and ADR-005 as quoted above.
- The deadline is secret until the crash (math spec §7), and LBT-10 P-13 keeps `round_secrets` from the
  gateway role. The sequencer therefore cannot compute it. The trusted scheduler that issues `CloseRound`
  already holds it.

**Alternatives.**
- Close the window when `CloseRound` is *executed*: Transaction B would lock the admission head, which
  LBT-10 §6 item 2 forbids, and late commands keep producing records until then.
- Store the deadline on the head: reveals the crash outcome to every role that can read the head (P-13,
  P-25).
- No `closed` state, only `sealed`: correct, because `Decide` rejects late cash-outs as `wrong_phase`, but
  the post-crash burst writes one record per command under the hot lock.
- No closure guard: an early `CloseRound` that `Decide` rejects (`crash_not_due`) would leave the window
  closed while the round is still RUNNING. Eligible cash-outs would be refused and the round would be
  stuck.

### S-9. What admission rejects and what `Decide` decides

**Proposal.** Admission rejects only what cannot depend on `Decide`. A rejection writes nothing.

| Rejected at admission | Result |
|---|---|
| an `ordinary` command while `closed` | `*RejectedError` `round_closed` |
| any command while `sealed` | `*RejectedError` `round_sealed` |
| a lifecycle command out of order (S-8 table) | `*RejectedError` `lifecycle_order` |
| a `close` before `NotBeforeUnixMicro` | `*ClosureNotDueError` |
| a hold larger than the available credits, or no player account | `*RejectedError` `insufficient_credits` |
| nil IDs, unknown rules version, rules mismatch, bad `NotBeforeUnixMicro` | `ErrInvalidRequest`, `ErrUnknownRules` |
| unencodable command, U+0000 in a player ID | `ErrInvalidCommand` |

Everything else is admitted and decided by `Decide`, producing a chained terminal result. Notably:

| Case | `Decide` result |
|---|---|
| `CashOut` with `e = t − T0 ≤ 0` | rejected `not_after_start` |
| `CashOut` with `e ≥ D`, **including `e = D` (deadline equality loses, ADR-004, D-9)** | rejected `deadline_passed` |
| `CashOut` with `0 < e < D` | accepted, payout per D-8 |
| `PlaceBet` after an admitted `StartRound`; `CashOut` before it | rejected `wrong_phase` |
| wrong authority on any command | rejected `unauthorized` |
| empty player | rejected `invalid_command` |
| amount outside the round's limits, second bet, bet cap, no bet, second cash-out | the lifecycle spec's reason codes |
| `CloseRound` before `T0 + D` | rejected `crash_not_due` (unreachable through `Admit`, S-8) |

A `CashOut` admitted with effective time exactly `T0 + D` is rejected by `Decide`, and a `CloseRound`
admitted at exactly `T0 + D` is accepted. Both outcomes follow from the same durable effective time.

**Rationale.**
- Decisions that depend on game state must be reproducible by decision replay (ADR-001, ADR-003). A
  `Decide` rejection is chained evidence (A-7).
- The sequencer knows neither `D`, which is secret, nor the executed state.

**Alternatives.**
- Reject cash-outs with `e ≥ D` at admission: needs the secret deadline in the admission path, and the
  most disputed rejections would leave no chain record.
- Reject `PlaceBet` after `StartRound` at admission: the same outcome, but it leaves no record, and the
  saving is small because bets arrive before the start.

### S-10. crash/v1 admission policy

**Proposal.** `admission.CrashV1Policy()` returns a `Policy` whose `Descriptor()` equals `v1.Descriptor()`.

| Command | Authority | Class | Hold |
|---|---|---|---|
| `v1.PlaceBet` | player | `ordinary` | `{Player, BetCredits}` when `Player != ""` and `v1.MinBetCredits ≤ BetCredits ≤ v1.MaxBetCredits`; otherwise none |
| `v1.PlaceBet` | none or system | `ordinary` | none |
| `v1.CashOut` | any | `ordinary` | none |
| `v1.StartRound` | system | `start` | none |
| `v1.CloseRound` | system | `close` | none |
| `v1.SettleRound` | system | `settle` | none |
| `StartRound`, `CloseRound`, `SettleRound` | not system | `ordinary` | none |

- A `PlaceBet` or `CashOut` whose player contains U+0000 returns `ErrInvalidCommand`.
- nil, pointers and foreign types return `ErrInvalidCommand`. `EncodeCommand` rejects them first with
  `game.ErrTypeMismatch`.

**Rationale.**
- Every `PlaceBet` that `Decide` could accept has a hold. `Decide` requires player authority, a non-empty
  player and an amount inside the round's limits, which lie inside D-7's. LBT-12 can therefore post every
  `BetPlaced` against a hold.
- A hold for an amount that `Decide` rejects on the round's narrower limits is released at execution.
  That is harmless.
- Only system authority can drive the window. A mis-built lifecycle command gets `unauthorized` recorded
  by `Decide`.
- U+0000 cannot be stored in `ledger_accounts.player_id` (LBT-10 §2 item 12).

**Alternatives.**
- A hold for every `PlaceBet`: zero or negative amounts violate `ledger_holds.amount > 0`.
- Reject non-system lifecycle commands at admission: loses the `unauthorized` evidence.
- A policy method on `game.Adapter`: a contract change for one runtime concern.

### S-11. Credit holds at admission

**Proposal.** For a classification with a hold, under the head lock and after the window checks:
1. `SELECT account_id, credits_posted - debits_posted - held FROM orvex.ledger_accounts WHERE kind =
   'player' AND player_id = $player FOR NO KEY UPDATE`.
2. If there is no row, or the available amount is below the hold: rejected `insufficient_credits`.
3. Insert the admission, then `ledger_holds(round_id, sequence, account_id, amount)`. The hold's foreign
   key to `admissions` needs the admission first. LBT-10's hold trigger adds the amount to `held`.

- The account floor `CHECK` remains a backstop. A `23514` on `ledger_accounts_floor_check` also maps to
  `insufficient_credits` (LBT-10 §6 item 4).
- A missing account gets the same code as low credits, so the answer does not reveal whether an account
  exists. Admission never creates accounts: LBT-12 and operations do.

**Rationale.**
- P-21: credits are reserved at admission, and the account row lock protects balances across rounds
  (ADR-005).
- Checking first avoids a PL/pgSQL `EXCEPTION` block, which would cost a subtransaction per admission.
- `FOR NO KEY UPDATE` is the lock the hold trigger's `UPDATE` takes anyway. It does not conflict with the
  `KEY SHARE` locks of foreign-key checks from LBT-12's postings.

**Alternatives.**
- Insert and catch `23514` inside the function: a subtransaction per admission.
- Create a missing account at its first admission: account policy belongs to LBT-12.
- Hold in Transaction B: P-21 rejects it.

### S-12. Errors, SQLSTATEs and retry policy

**Proposal.**

| Where | Condition | Handling |
|---|---|---|
| before the DB | request or command invalid | `ErrInvalidRequest`, `ErrInvalidCommand` or `ErrUnknownRules`; no DB call |
| gate, `BeginTx`, function call | `ctx` done | return an error wrapping `ctx.Err()`: not admitted, because no `COMMIT` was sent |
| `BeginTx`, function call | not a `*pgconn.PgError` (connection loss) | retry: no `COMMIT` was sent, so nothing committed |
| `BeginTx`, function call | `PgError` of class `08`, `53` or `57` (except `57014` with `ctx` done), or `25P03` | retry, like connection loss: no `COMMIT` was sent |
| function call | `40001`, `40P01` (`postgres.IsRetryable`) | retry |
| function call | `55P03` | `ErrBusy`; no retry |
| function call | `23505` on `admissions_command_id_key` | resolve: look the ID up in a new read transaction. Same round and bytes give an `Admission` with `Duplicate = true`; otherwise `ErrCommandIDReused` |
| function call | `23514` on `ledger_accounts_floor_check` | `*RejectedError` `insufficient_credits` |
| function call | any other SQLSTATE (`23xxx`, `OXxxx`, `22023`, `42xxx`, `XX000`, …) | `ErrInternal`, wrapping the `PgError`; fail closed, no retry |
| result | an unknown `outcome` string | `ErrInternal` |
| `Commit` | nil | return the `Admission` |
| `Commit` | `40001`, `40P01` | rolled back: retry |
| `Commit` | `PgError` of class `23` or `OX`, `40002`, or `pgx.ErrTxCommitRollback` | definitely not committed: `ErrInternal` |
| `Commit` | anything else: non-`PgError`, `40003`, classes `08`, `53`, `57`, `58`, `XX`, `ctx` errors | **ambiguous**: retry if `ctx` is alive and attempts remain; otherwise `ErrOutcomeUnknown`, wrapping the cause |

- `Config.MaxAttempts` defaults to 3.
- Before attempt `n ≥ 2`, wait a uniformly random duration in `[0, min(50 ms, 5 ms · 2^(n−2))]` (full
  jitter), interruptible by `ctx`.
- After the last attempt:
  - `ErrOutcomeUnknown` if the last failure was ambiguous;
  - otherwise `ErrUnavailable`, wrapping the last error.
- Every returned error wraps exactly one sentinel. `PgError` details (SQLSTATE, constraint name) stay
  reachable through `errors.As` for logs. No error text contains command bytes, seeds or deadlines.

**Rationale.**
- The classes follow LBT-10 §6 item 4.
- Dedupe makes every retry with the same command ID safe, so the policy can retry whenever the outcome is
  not definite.
- A backend can be terminated (`57P01`) after its commit record is written, so a FATAL during `COMMIT` is
  ambiguous, not a rollback.
- Bounded, jittered retries avoid ADR-006's retry storms.
- A lock timeout has already waited `LockTimeout`, so retrying it only adds contention.

**Alternatives.**
- Retry `55P03`: amplifies contention on a hot round.
- Unbounded retries: retry storms.
- Treat every `Commit` error as definite: could report "not admitted" for a committed admission.

### S-13. Rejection evidence

**Proposal.** LBT-11 stores none.
- A rejected request writes nothing, and the caller receives the reason.
- The evidence the barrier rests on is durable: the admitted `CloseRound` and `SettleRound` rows (sequence
  and effective time) and the window columns.
- `Decide`'s rejections are chained (A-7).

**Rationale.**
- ADR-002 leaves rejection evidence open.
- The high-volume case, the post-crash cash-out burst, would add one write per request at the worst
  moment, under the hot lock or in a second transaction.
- The barrier's rejections are outcome-neutral (S-8 guarantee 4).

**Alternatives.**
- An append-only `admission_rejections(command_id, round_id, reason, at_unix_micro)` written in A under
  the lock: write amplification during the burst.
- The same table written after rollback, in a separate transaction: the evidence time is no longer taken
  under the lock.
- Structured logs only, in LBT-13: not durable.

### S-14. Test strategy

**Proposal.**
- **Real PostgreSQL.**
  - Integration tests run against real PostgreSQL through LBT-10's `pgtest` (P-7). They live in
    `*_integration_test.go` files with `//go:build integration` (P-8).
  - Each test package starts one container in `TestMain` (`pgtest.Run`). Each test uses its own migrated
    database clone (`pgtest.MigratedDatabase`).
  - They run under `make test-integration`, which is `go test -race -tags integration -count=1 ./...`
    with the default `GOTESTFLAGS`. Locally this needs Docker. In CI, LBT-10's `integration` job runs it
    on every push and pull request.
- **Determinism.**
  - Concurrency tests assert only invariants that hold under every interleaving: sets of sequences, row
    counts, monotonicity, equality of each ACK with its row. They never assert timings or which caller
    wins.
  - Goroutines start together on a closed channel.
  - Blocking is detected through `pg_stat_activity` or `pg_locks`, never with sleeps.
  - The admission integration tests do not call `t.Parallel()`.
- **Fault injection.**
  - (a) `faultDB` wraps the pool. Its transactions can fail the function call or `Commit`, either after
    or instead of the real commit.
  - (b) A test-only deferred constraint trigger in schema `orvex_test` holds or fails `COMMIT` for one
    chosen command ID (prototype 6).
- **Exact effective times.** Seed a previous admission with a future effective time. The S-7 clamp makes
  the next admission's time exactly that value.
- **Unit tests** without Docker cover validation, the policy, error classification, the retry loop (with
  a fake transaction) and the gate. `make test` runs them.

**Alternatives.**
- A test clock seam in the SQL function: production code with a test override.
- Sleeps to wait for blocking: flaky.
- Testing only through LBT-12: LBT-11's acceptance would wait for LBT-12.

## 5. Normative database behavior (migration `0007_admission_window`)

First line of each file: `-- 0007 admission_window: <purpose> (LBT-11 spec S-7, S-8, §5)`. Schema
conventions follow LBT-10 P-11: schema-qualified names, explicit constraint names, every function
`LANGUAGE plpgsql` with `SET search_path = pg_catalog`, plus a `COMMENT ON` for every new column and
function, each naming ADR-002.

### 5.1 New columns on `orvex.round_admission_heads`

| Column | Type | Constraints |
|---|---|---|
| `admission_state` | `text NOT NULL DEFAULT 'open'` | `round_admission_heads_state_check`: `IN ('open','started','closed','sealed')` |
| `started_sequence` | `bigint`, null | `round_admission_heads_started_fkey` `(round_id, started_sequence)` → `admissions (round_id, sequence)` |
| `closed_sequence` | `bigint`, null | `round_admission_heads_closed_fkey`, likewise |
| `sealed_sequence` | `bigint`, null | `round_admission_heads_sealed_fkey`, likewise |

- The three foreign keys are `NOT DEFERRABLE`. `admit_command` inserts the admission before it updates
  the head.
- `round_admission_heads_window_check`. A `CHECK` whose expression is NULL passes, so every column that
  must be set is tested with `IS NOT NULL` explicitly:
  - `open`: all three sequences null;
  - `started`: `started_sequence` not null; the other two null;
  - `closed`: `started_sequence` and `closed_sequence` not null, `closed_sequence > started_sequence`;
    `sealed_sequence` null;
  - `sealed`: all three not null, `started_sequence < closed_sequence < sealed_sequence`, and
    **`sealed_sequence = admitted_sequence`**;
  - in every state, each non-null sequence is between 1 and `admitted_sequence`.
- Trigger `round_admission_heads_window`, `BEFORE UPDATE FOR EACH ROW`, runs function
  `orvex.check_admission_window()`. It raises `OX005` (`CodeHeadRegression`) when:
  - the state rank decreases (`open` 0 < `started` 1 < `closed` 2 < `sealed` 3); or
  - a sequence column that was non-null in `OLD` differs in `NEW`.
- LBT-10's `round_admission_heads_forward` trigger and `round_admission_heads_head_fkey` stay unchanged.
- Rows that exist without these columns set, such as heads written by `pgtest.LoadHistory`, get state
  `open` with null sequences. That satisfies every constraint. `LoadHistory` is not changed.

### 5.2 `orvex.unix_micro(ts timestamptz) RETURNS bigint`

- `IMMUTABLE`. It returns `(EXTRACT(EPOCH FROM ts) * 1000000)::bigint`, which is exact at `timestamptz`'s
  microsecond resolution (prototype 7).
- It is the only conversion of a timestamp into an effective time.

### 5.3 `orvex.admit_command`

```text
orvex.admit_command(
    p_round_id              uuid,
    p_game                  text,
    p_rules_version         bigint,
    p_command_id            uuid,
    p_command               bytea,    -- the codec's canonical command bytes
    p_class                 text,     -- 'ordinary' | 'start' | 'close' | 'settle'
    p_hold_player_id        text,     -- NULL: no hold
    p_hold_credits          bigint,   -- NULL iff p_hold_player_id IS NULL; otherwise > 0
    p_not_before_unix_micro bigint,   -- not NULL iff p_class = 'close'
    p_lock_timeout_ms       integer,  -- >= 1
    p_idle_timeout_ms       integer   -- >= 1
) RETURNS TABLE (outcome text, reason text, sequence bigint, effective_time_unix_micro bigint)
VOLATILE, exactly one row
```

**Outcomes:**

| `outcome` | `reason` | `sequence` | `effective_time_unix_micro` |
|---|---|---|---|
| `admitted` | null | new sequence | its effective time |
| `duplicate` | null | original sequence | original effective time |
| `conflict` | null | null | null |
| `rejected` | `round_closed`, `round_sealed`, `lifecycle_order` or `insufficient_credits` | null | null |
| `not_due` | null | null | the effective time that was too early |
| `invalid_not_before` | null | null | null |
| `rules_mismatch` | null | null | null |
| `not_found` | null | null | null |
| `corrupt` | null | null | null |

**Argument checks.** These are caller bugs and raise SQLSTATE `22023`:
- a `p_class` outside the four values;
- hold parameters inconsistent with the rules above;
- `p_not_before_unix_micro` present for a non-close class, or missing for a close;
- a timeout below 1.

**Steps, in this order; the first that returns wins:**

0. *Settings.*
   - `set_config('lock_timeout', p_lock_timeout_ms || 'ms', true)`.
   - `set_config('idle_in_transaction_session_timeout', p_idle_timeout_ms || 'ms', true)`.
   - If `current_setting('synchronous_commit') = 'off'`, run `set_config('synchronous_commit', 'on',
     true)`.
1. *Fast dedupe, no lock.* Select `round_id`, `sequence`, `effective_time_unix_micro` and `command` from
   `admissions` where `command_id = p_command_id`.
   - Found with the same round and `command = p_command`: return `duplicate`.
   - Found otherwise: return `conflict`.
2. *Fast window, no lock.* Select `r.game`, `r.rules_version`, whether a head exists, and
   `h.admission_state` from `rounds r LEFT JOIN round_admission_heads h USING (round_id)` where
   `r.round_id = p_round_id`.
   - No row: `not_found`.
   - No head: `corrupt`.
   - `(game, rules_version) ≠ (p_game, p_rules_version)`: `rules_mismatch`.
   - State `sealed`: `rejected round_sealed`.
   - State `closed` and class `ordinary`: `rejected round_closed`.
3. *Lock.* `SELECT admitted_sequence, admission_state, started_sequence FROM round_admission_heads WHERE
   round_id = p_round_id FOR UPDATE`.
4. *Clock*, the first statement after the lock: `v_clock := orvex.unix_micro(clock_timestamp())`.
5. *Clamp.* `v_t := v_clock`. If `admitted_sequence > 0`: `v_t := greatest(v_t, effective time of
   admission (p_round_id, admitted_sequence))`.
6. *Dedupe again*, as in step 1, on a fresh snapshot.
7. *Window* per the S-8 table on the locked state. Return `rejected round_sealed`, `round_closed` or
   `lifecycle_order` as it says.
8. *Closure guard*, for class `close` only:
   - `v_t0` := effective time of admission `(p_round_id, started_sequence)`.
   - If `p_not_before_unix_micro < v_t0`: return `invalid_not_before`.
   - If `v_t < p_not_before_unix_micro`: return `not_due` with `v_t`.
9. *Hold check*, when `p_hold_player_id` is not null: S-11 step 1. No row, or available <
   `p_hold_credits`: return `rejected insufficient_credits`.
10. *Insert.* `v_seq := admitted_sequence + 1`. Insert `admissions(round_id, sequence, command_id,
    effective_time_unix_micro, command)`.
11. *Hold insert*, when there is a hold: insert `ledger_holds(round_id, sequence, account_id, amount)`.
12. *Advance the head.* `UPDATE round_admission_heads SET admitted_sequence = v_seq`, plus, by class:
    - `start`: `admission_state = 'started'`, `started_sequence = v_seq`;
    - `close`: `admission_state = 'closed'`, `closed_sequence = v_seq`;
    - `settle`: `admission_state = 'sealed'`, `sealed_sequence = v_seq`.
13. Return `admitted` with `v_seq` and `v_t`.

Steps 1 and 2 are the S-5 fast path. Steps 3 to 13 hold the head lock until the caller's `COMMIT` or
`ROLLBACK`. The function never catches exceptions.

### 5.4 Down migration

1. A `DO` block raises `OX003` (`down migration refused: orvex.round_admission_heads holds rows`) if the
   table has any row (P-5).
2. Drop `orvex.admit_command`, the trigger `round_admission_heads_window`,
   `orvex.check_admission_window()`, the five constraints, the four columns and `orvex.unix_micro`.

After the down, the catalog fingerprint equals the one at version 6 (LBT-10's
`TestMigrateEachStepReversible`).

## 6. Go API (normative; `package admission`)

Names may be refined during implementation; behavior may not.

```go
// DB is what the Sequencer needs from the database; *pgxpool.Pool implements it.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Config tunes the Sequencer. New rejects values outside the ranges (ErrInvalidConfig);
// there is no zero-means-default.
type Config struct {
	LockTimeout              time.Duration // [10ms, 60s]; must exceed IdleInTransactionTimeout
	IdleInTransactionTimeout time.Duration // [50ms, 60s]
	MaxAttempts              int           // [1, 10]
	MaxInFlightPerRound      int           // [1, 64]
}

// DefaultConfig returns {3s, 1s, 3, 4}.
func DefaultConfig() Config

// Sequencer admits commands (ADR-002, ADR-005 Transaction A). Safe for concurrent use.
type Sequencer struct{ /* unexported */ }

// New requires at least one policy, no two policies for one (game, rules version), and for each policy
// an adapter in reg whose Descriptor equals the policy's.
func New(db DB, reg *game.Registry, cfg Config, policies ...Policy) (*Sequencer, error)

type Request struct {
	RoundID      [16]byte // not the nil UUID
	Game         game.ID
	RulesVersion game.RulesVersion
	CommandID    [16]byte // not the nil UUID; reuse it on every retry of this command
	Command      any      // the rules version's command value, e.g. v1.CashOut{...}
	// NotBeforeUnixMicro is the earliest effective time at which a closing command
	// may be admitted: the absolute crash deadline T0 + D (S-8). > 0 for ClassClose,
	// 0 for every other class.
	NotBeforeUnixMicro int64
}

// Admission is a durable admission receipt. Admit returns one only after the
// admission committed (ADR-002); it says nothing about execution.
type Admission struct {
	RoundID                [16]byte
	CommandID              [16]byte
	Sequence               uint64 // >= 1
	EffectiveTimeUnixMicro int64
	// Duplicate reports that the admission already existed when Admit's final attempt ran.
	Duplicate bool
}

// Input returns the deterministic input Decide receives for this command.
func (a Admission) Input() game.Input

func (s *Sequencer) Admit(ctx context.Context, req Request) (Admission, error)

type Outcome uint8

const (
	OutcomePending  Outcome = iota + 1 // admitted, not executed yet
	OutcomeAccepted                    // Decide accepted it
	OutcomeRejected                    // Decide rejected it; Status.Reason holds the code
)

type Status struct {
	Admission Admission // Duplicate is always false
	Outcome   Outcome
	Reason    string
}

// Lookup reads the admission and execution status of commandID in one
// SnapshotReadTxOptions transaction. ErrNotFound is not definitive for a command
// still being admitted; resubmitting the same Request is.
func (s *Sequencer) Lookup(ctx context.Context, commandID [16]byte) (Status, error)

// Policy is the admission view of one rules version (S-10). Classify is pure.
type Policy interface {
	Descriptor() game.Descriptor
	Classify(command any) (Classification, error)
}

type Class uint8

const (
	ClassOrdinary Class = iota + 1
	ClassStart
	ClassClose
	ClassSettle
)

// Hold is a credit reservation; the zero value means none.
type Hold struct {
	PlayerID string
	Credits  int64 // > 0 when PlayerID != ""
}

type Classification struct {
	Class Class
	Hold  Hold
}

// CrashV1Policy returns the crash/v1 policy (S-10).
func CrashV1Policy() Policy

// Reason is the stable code of a rejection at admission (S-9). Not a Decide code.
type Reason string

const (
	ReasonRoundClosed         Reason = "round_closed"
	ReasonRoundSealed         Reason = "round_sealed"
	ReasonLifecycleOrder      Reason = "lifecycle_order"
	ReasonInsufficientCredits Reason = "insufficient_credits"
)

type RejectedError struct{ Reason Reason }

func (e *RejectedError) Error() string // "admission: rejected: " + reason
func (e *RejectedError) Unwrap() error // ErrRejected

// ClosureNotDueError: the closing command's effective time was before NotBeforeUnixMicro.
// Error() names neither time.
type ClosureNotDueError struct{ EffectiveTimeUnixMicro, NotBeforeUnixMicro int64 }

func (e *ClosureNotDueError) Error() string // "admission: round closure not due"
func (e *ClosureNotDueError) Unwrap() error // ErrClosureNotDue

var (
	ErrInvalidConfig   = errors.New("admission: invalid config")
	ErrInvalidRequest  = errors.New("admission: invalid request")
	ErrInvalidCommand  = errors.New("admission: invalid command")
	ErrUnknownRules    = errors.New("admission: no admission policy for the game rules version")
	ErrRoundNotFound   = errors.New("admission: round not found")
	ErrCommandIDReused = errors.New("admission: command id already admitted for a different command")
	ErrRejected        = errors.New("admission: rejected")
	ErrClosureNotDue   = errors.New("admission: round closure not due")
	ErrBusy            = errors.New("admission: lock wait timed out")
	ErrUnavailable     = errors.New("admission: database unavailable; command not admitted")
	ErrOutcomeUnknown  = errors.New("admission: outcome unknown; resubmit the same command id")
	ErrInternal        = errors.New("admission: internal error")
	ErrNotFound        = errors.New("admission: no admission for command id")
)
```

**`Admit` steps** (Go side):
1. Validate, first failure wins:
   - nil `RoundID` or `CommandID` → `ErrInvalidRequest`;
   - no policy for `(Game, RulesVersion)` → `ErrUnknownRules`;
   - `EncodeCommand` fails → `ErrInvalidCommand`;
   - `Classify` fails → `ErrInvalidCommand`;
   - a classification with an invalid class or an inconsistent hold → `ErrInternal` (a policy bug);
   - `NotBeforeUnixMicro ≤ 0` for `ClassClose`, or non-zero for any other class → `ErrInvalidRequest`.
2. Acquire the round's gate slot (honors `ctx`). Release it on return.
3. Run the attempt loop of S-12. Each attempt:
   - begins with `postgres.AdmissionTxOptions()`;
   - calls `orvex.admit_command` with the encoded bytes, the classification, `NotBeforeUnixMicro` (NULL
     unless `ClassClose`) and the two timeouts in whole milliseconds;
   - maps the outcome row (§5.3) as below.
4. Outcome mapping:

| `outcome` | Result |
|---|---|
| `admitted` | `COMMIT`; if it returns nil, the `Admission` (`Duplicate = false`) |
| `duplicate` | `ROLLBACK`; the `Admission` with `Duplicate = true` |
| `conflict` | `ErrCommandIDReused` |
| `rejected` | `*RejectedError` |
| `not_due` | `*ClosureNotDueError` |
| `invalid_not_before`, `rules_mismatch` | `ErrInvalidRequest` |
| `not_found` | `ErrRoundNotFound` |
| `corrupt`, anything else | `ErrInternal` |

Error strings are lowercase, without trailing punctuation, and prefixed `admission: `.

## 7. What LBT-12 consumes from admissions (not designed here)

- **The input stream.**
  - `orvex.admissions` rows of a round in sequence order, from `processed_sequence + 1` up to
    `admitted_sequence`. They are read without locks because they are immutable (LBT-10 §6 item 2).
  - Each row gives:
    - the command bytes: `DecodeCommand` with the round's adapter, copied byte for byte into the chain
      record, which LBT-10's `OX008` binding checks;
    - the `command_id`;
    - `game.Input{Sequence, EffectiveTimeUnixMicro}`, as `Admission.Input()` builds it.
- **Guarantees LBT-12 may rely on.**
  - Sequences are gapless from 1, immutable and unique.
  - Effective times are non-decreasing in sequence order.
  - Bytes are canonical codec output for the round's rules version.
  - A system `StartRound`, `CloseRound` or `SettleRound` was admitted only in window order.
  - `CloseRound` was admitted only at an effective time ≥ the `NotBeforeUnixMicro` its scheduler
    supplied.
- **Obligations.**
  - Every admitted command is executed exactly once, accepted or rejected. Nothing can "unadmit" a
    command (A-7).
  - LBT-12 never updates `round_admission_heads` (LBT-10 §6 item 2).
- **Holds.**
  - Exactly the player `PlaceBet`s with a non-empty player and an amount within D-7's limits have one
    `ledger_holds` row `(round_id, sequence, account_id)`. No other admission has one.
  - Transaction B releases the hold (`ledger_hold_releases`) whatever the result, and posts the bet from
    it on `BetPlaced`.
- **Finality.**
  - `sealed_sequence` is the round's last admission. A round is final once `processed_sequence =
    sealed_sequence`.
  - `started_sequence` and `closed_sequence` are available for observability.
- **Wake-up.** LBT-11 sends no notification. `NOTIFY` serializes the commits of all notifying
  transactions. LBT-12 decides how to discover new admissions, for example by polling
  `admitted_sequence`.
- **Results.** `Lookup` reads `chain_records.result_status` and `result_reason` (P-17), so LBT-12's
  records become visible to callers without further work.
- **Round creation** (LBT-10 §9 proposes LBT-12 as owner) inserts `round_admission_heads` with the `0007`
  defaults (`open`).
- **Scheduler** (LBT-12 or LBT-14).
  - It admits system `StartRound`, `CloseRound` and `SettleRound` through `Admit`.
  - For `CloseRound`, `NotBeforeUnixMicro` = `State.CrashDeadlineUnixMicro` from the executed state.
  - On `ErrClosureNotDue`, it waits `NotBeforeUnixMicro − EffectiveTimeUnixMicro` and resubmits with the
    same command ID.

## 8. Builder rules for the implementation run

- goimports grouping with the `github.com/juano-morello/orvex` local prefix (`.golangci.yml`).
- Integration tests are in `package admission_test` (external). Unit tests that touch unexported helpers
  (gate, retry classification, fake transactions) are in `package admission`.
- Every integration test file starts with `//go:build integration`. The package has one `TestMain` calling
  `pgtest.Run`, in `helpers_integration_test.go`.
- Tests assert SQLSTATEs (`postgres.SQLState`), `PgError.ConstraintName`, sentinel errors and reason codes,
  never message text.
- No sleeps to wait for blocking: poll `pg_stat_activity` or `pg_locks` with a 10 s deadline, then
  `t.Fatal`.
- Never edit a golden fixture, `internal/game/**`, `internal/audit/**`, `internal/canonjson/**` or
  migrations `0001` to `0006`.

## 9. LBT-11 acceptance as tests

| Acceptance (Linear and request) | Tests (§10 names the files) |
|---|---|
| Concurrent duplicate submissions create one record | `TestAdmitConcurrentDuplicates`, `TestAdmitConcurrentDuplicatesAcrossRounds`, `TestAdmitRetryReturnsOriginal`, `TestAdmitDuplicateAfterWindowClosed` |
| Hot-round ordering is unique | `TestAdmitHotRoundOrdering`, `TestAdmitManyRoundsIndependentSequences`, `TestBarrierConcurrentCloseAndCashOuts` |
| Admission ACK follows commit | `TestAdmitAckFollowsCommit`, `TestAdmitNoAckWhenCommitFails`, `TestAdmitAmbiguousCommitCommitted`, `TestAdmitAmbiguousCommitAborted`, `TestAdmitOutcomeUnknownWhenContextEnds`, `TestAdmitRetriesConnectionLossBeforeCommit`; every concurrency test also checks each ACK against its committed row |
| Per-round locking and timeouts | `TestAdmitLockTimeout`, `TestAbandonedAdmissionReleasesLock`, `TestAdmitCommandSessionSettings`, `TestAdmitHotRoundDoesNotStarveOtherRounds`, `TestFastPathSkipsLock` |
| Effective DB timestamps after the lock | `TestEffectiveTimeReadAfterLock`, `TestEffectiveTimeMonotonic`, `TestUnixMicroExact` |
| Unique command IDs and retry lookup | `TestAdmitCommandIDReused`, `TestAdmitRetryReturnsOriginal`, `TestLookup` |
| Deadline admission rules (equality loses) | `TestAdmissionStreamDrivesCrashRound`, `TestClosureNotBefore`, `TestClosureNotBeforeEqualityAdmits` |
| Closure barrier | `TestWindowTransitions`, `TestBarrierRejectsLateCommands`, `TestBarrierSealIsFinal`, `TestBarrierConcurrentCloseAndCashOuts`, `TestAdmissionWindowConstraints` |
| ADR-006 cross-round overspending (admission part) | `TestAdmitConcurrentCrossRoundHolds`, `TestAdmitHoldsCredits` |

## 10. Implementation plan (after approval)

### Files

| File | Action |
|---|---|
| `migrations/0007_admission_window.up.sql`, `.down.sql` | new, per §5 |
| `internal/runtime/admission/doc.go` | new: package doc (ADR-002, ADR-005 Transaction A; S-3 to S-8 in brief; this spec's path) |
| `internal/runtime/admission/sequencer.go` | new: `DB`, `Config`, `DefaultConfig`, `New`, `Sequencer`, `Request`, `Admission`, `Admission.Input`, `Admit`, `Outcome`, `Status`, `Lookup` |
| `internal/runtime/admission/policy.go` | new: `Policy`, `Class`, `Hold`, `Classification` |
| `internal/runtime/admission/crashv1.go` | new: `CrashV1Policy` (S-10) |
| `internal/runtime/admission/errors.go` | new: sentinels, `Reason`, `RejectedError`, `ClosureNotDueError` |
| `internal/runtime/admission/gate.go` | new: per-round in-process gate (S-4) |
| `internal/runtime/admission/retry.go` | new: call and commit error classification, backoff (S-12) |
| `internal/runtime/admission/crashv1_test.go`, `sequencer_test.go`, `retry_test.go`, `gate_test.go`, `errors_test.go` | new (unit) |
| `internal/runtime/admission/helpers_integration_test.go`, `admit_integration_test.go`, `concurrency_integration_test.go`, `barrier_integration_test.go`, `commit_integration_test.go` | new (integration) |
| `internal/persistence/postgres/admission_window_integration_test.go` | new (integration): the `0007` schema objects |
| `internal/persistence/postgres/migrate_test.go`, `migrate_integration_test.go`, `cmd/crashctl/migrate_integration_test.go` | edit: migration count (step 1) |
| `internal/persistence/postgres/pgtest/fingerprint.go` | edit only if needed (step 1) |
| `docs/architecture/persistence.md` | edit: new section "Admission (LBT-11)": window columns and constraints, `admit_command` steps, lock order and timeouts, outcome and error mapping, grants the admitting role needs (§2 item 13) |
| `README.md` | edit: Status adds LBT-11; Layout adds `internal/runtime/admission` |
| `AGENTS.md` | edit: the hand-off line that requires `make test-integration` for `migrations/` and `internal/persistence/` also names `internal/runtime/` |

Unchanged: `docs/adr/**`, `internal/game/**`, `internal/audit/**`, `internal/canonjson/**`, migrations
`0001` to `0006`, every golden fixture, `go.mod`, `go.sum`, `Makefile`, `.github/workflows/ci.yml`,
`cmd/gateway`, `cmd/engine`.

### Step 1: migration `0007` and LBT-10 test updates

- Write `0007_admission_window.{up,down}.sql` per §5, with the constraint, trigger and function names
  exactly as given.
- Update LBT-10's tests for the seventh migration:
  - `TestEmbeddedMigrations`: if it asserts the total count or the full name list, add `admission_window`
    as version 7.
  - `TestMigrateCLI`: `up` prints seven `applied` lines and `schema version 7`; `status` prints seven
    `applied` lines.
  - `TestMigrateDownRefusesAuditData`: after `LoadHistory(worked-example)`, `MigrateDown(0)` fails at
    `0007` with `OX003`, the version stays 7, and the chain-record and snapshot counts are unchanged. A new
    `up` succeeds.
  - Any other assertion of "6" as the newest version.
- If `TestMigrateEachStepReversible` fails at version 6 only because dropped columns appear in the
  fingerprint, make `pgtest.Fingerprint` skip `pg_attribute.attisdropped` columns. Change nothing else in
  `pgtest`.

`internal/persistence/postgres/admission_window_integration_test.go` (each test from
`pgtest.MigratedDatabase`; rows from `pgtest.LoadHistory(empty-round)`, whose admissions 1 to 3 are
`StartRound`, `CloseRound` and `SettleRound`):
- `TestAdmissionWindowConstraints`:
  - each of these gives `23514` `round_admission_heads_window_check`:
    - `started` with a null `started_sequence`;
    - `closed` with a null `closed_sequence`;
    - `closed` with `closed_sequence = started_sequence`;
    - `sealed` with `sealed_sequence ≠ admitted_sequence`;
    - `started_sequence = 4` with `admitted_sequence = 3`;
  - state `bogus` gives `23514` `round_admission_heads_state_check`;
  - the valid progression `open` → `started(1)` → `closed(2)` → `sealed(3)` succeeds;
  - after it:
    - state back to `closed` gives `OX005`;
    - changing `started_sequence` gives `OX005`;
    - inserting admission 4 and setting `admitted_sequence = 4` gives `23514`
      `round_admission_heads_window_check` (sealed means final).
- `TestUnixMicroExact`: `SELECT orvex.unix_micro($1)` equals Go's `UnixMicro()` for:
  - the Unix epoch;
  - `2025-10-09T08:53:20.123457Z` (`1760000000123457`);
  - `1969-12-31T23:59:59.999999Z` (`-1`);
  - `2262-01-01T00:00:00Z`.
- `TestAdmitCommandArgumentChecks`: raw calls of `orvex.admit_command` with:
  - class `other`;
  - a hold player without credits;
  - credits 0;
  - a not-before on an `ordinary` class;
  - a `close` without a not-before;
  - a lock timeout of 0.

  Each gives `22023` and writes nothing.

### Step 2: `internal/runtime/admission` code

Write the package per §6, S-4, S-10 and S-12.
- Gate: a mutex-guarded map from round ID to `{slots chan struct{}; refs int}`. An entry is deleted when
  `refs` drops to 0, so the map holds only rounds with calls in flight.
- Backoff uses `math/rand/v2`.

### Step 3: unit tests (`make test`)

- `crashv1_test.go`:
  - `TestCrashV1PolicyDescriptor`: equals `v1.Descriptor()`.
  - `TestCrashV1Classify`, a table over every S-10 row:
    - each command type with player, system and none authority;
    - `PlaceBet` amounts −1, 0, 1, 1,000,000,000 and 1,000,000,001: a hold only for 1 and 1,000,000,000
      with player authority;
    - empty player: no hold;
    - U+0000 inside a `PlaceBet` and a `CashOut` player: `ErrInvalidCommand`;
    - nil, `&v1.CashOut{}` and a foreign type: `ErrInvalidCommand`.
- `errors_test.go`:
  - `TestRejectedError`: `errors.Is(err, ErrRejected)`, `errors.As` recovers the reason, and the text is
    `admission: rejected: <reason>`.
  - `TestClosureNotDueError`: `errors.Is(err, ErrClosureNotDue)`, the fields are kept, and the text holds
    no digits.
- `retry_test.go`:
  - `TestClassifyCallError`, a table:
    - `40001` and `40P01`: retry;
    - `55P03`: busy;
    - `23505` with constraint `admissions_command_id_key`: resolve;
    - `23505` with another constraint: internal;
    - `23514` with `ledger_accounts_floor_check`: insufficient credits;
    - `OX005` and `22023`: internal;
    - a `*net.OpError`, `08006`, `53300`, `57P01` and `25P03`: retry;
    - `57014` with the `ctx` done, and `context.Canceled`: `ctx`.
  - `TestClassifyCommitError`, a table:
    - `23000`, `OX004`, `40002` and `pgx.ErrTxCommitRollback`: definite;
    - `40001` and `40P01`: retry;
    - `40003`, `57P01`, `08006`, `53300`, `XX000`, `io.ErrUnexpectedEOF` and a wrapped
      `context.Canceled`: ambiguous.
  - `TestBackoffBounds`: 1,000 samples per attempt `n = 2..6` all lie in `[0, min(50 ms, 5 ms ·
    2^(n−2))]`.
- `gate_test.go`:
  - `TestGateBoundsPerRound`: with capacity 2, a third `acquire` on the same round blocks until a
    `release`.
  - `TestGateRoundsIndependent`: a full round does not block another round.
  - `TestGateCancelWhileWaiting`: a cancelled `ctx` returns `ctx.Err()` and leaves no slot taken.
  - `TestGateReleasesEntries`: after 100 goroutines acquire and release on 10 rounds, the map is empty.
- `sequencer_test.go` (fake `DB` and a fake `pgx.Tx` that embeds the interface and implements `QueryRow`,
  `Commit` and `Rollback`):
  - `TestDefaultConfig`: `{3s, 1s, 3, 4}`.
  - `TestNewValidation` gives `ErrInvalidConfig` for:
    - each `Config` bound ±1 unit;
    - `LockTimeout ≤ IdleInTransactionTimeout`;
    - no policies;
    - two policies for one key;
    - a policy without a registered adapter;
    - a registry descriptor whose schema or state version differs from the policy's.
  - `TestAdmitValidation`: the fake DB fails the test if `BeginTx` is called.
    - `ErrInvalidRequest`: nil round ID; nil command ID; `NotBeforeUnixMicro` 5 on a `CashOut`;
      `NotBeforeUnixMicro` 0 or −1 on a system `CloseRound`.
    - `ErrUnknownRules`: game `dice`; rules version 2.
    - `ErrInvalidCommand`: `v1.PlaceBet{Authority: 7}`; a nil command.
  - `TestAdmitOutcomeMapping`: each §5.3 outcome row maps per §6; `bogus` gives `ErrInternal`. `Commit`
    is called only for `admitted`.
  - `TestAdmitRetryLoop`, scripted failures per attempt with `MaxAttempts` 3 (attempt count observed by
    the fake):
    - call `40001`, `40001`, then `admitted`: an `Admission`, 3 attempts;
    - call `40P01` three times: `ErrUnavailable`;
    - call `55P03`: `ErrBusy` after 1 attempt;
    - a connection error, then `admitted`: an `Admission`, 2 attempts;
    - commit `io.ErrUnexpectedEOF`, then `duplicate`: an `Admission` with `Duplicate = true`;
    - commit `io.ErrUnexpectedEOF` three times: `ErrOutcomeUnknown`;
    - `ctx` cancelled during backoff: wraps `context.Canceled`, no further attempt;
    - call `23505` `admissions_command_id_key`, with the resolve lookup returning another round:
      `ErrCommandIDReused`.

### Step 4: integration test helpers (`helpers_integration_test.go`)

- `TestMain`: `os.Exit(pgtest.Run(m))`.
- `newPool(t, dsn, maxConns)`: a `pgxpool.Pool` with the given `MaxConns`, closed at cleanup.
- `newSequencer(t, db, mutate func(*Config))`: `crash/v1` registry plus `CrashV1Policy()`.
- `createRound(t, db, roundID, seed) (v1.Config, v1.State)`, in one transaction:
  - config: `MinBetCredits 1`, `MaxBetCredits 1,000,000,000`, `MaxBets 100`;
  - inserts `rounds` from `v1.Descriptor()`;
  - inserts `round_secrets` (the codec's secret config);
  - inserts `round_admission_heads` with defaults and `round_execution_heads` with `(0, token 0)`;
  - inserts the genesis `chain_records` row from `chain.EncodeGenesis`: the public config, `CreatedAt` =
    now in µs, `EngineBuild` `test`, `hash = chain.LinkHash`;
  - returns the config and `InitialState`.

  An option overrides the rules version written to `rounds` and the genesis (for `TestAdmitRulesMismatch`).
- `fundPlayer(t, db, player, credits)`:
  - creates the global system account `faucet` (floor not enforced) if absent;
  - creates the player account;
  - writes one `ledger_transactions` row (kind `grant`, no record) with one posting faucet → player.
- `seedAdmission(t, db, roundID, commandID, cmd, effectiveTime)`: inserts the next admission directly with
  the given time and advances `admitted_sequence`, in one transaction. Only for `ordinary` commands.
- `head(t, db, roundID)`: reads the head's columns. `row(t, db, commandID)`: reads an admission.
- `waitLockWaiters(t, db, n)`: polls `pg_stat_activity` (this database, `wait_event_type = 'Lock'`) until
  at least `n` backends wait.
- `waitAdvisoryWaiter(t, db) (pid int32)`: polls `pg_locks` for one ungranted advisory lock.
- `installCommitHooks(t, db, holdID, failID [16]byte, key int64)`: creates schema `orvex_test` and two
  `DEFERRABLE INITIALLY DEFERRED` constraint triggers `AFTER INSERT ON orvex.admissions`:
  - one runs `pg_advisory_xact_lock(key)` when `command_id = holdID`;
  - the other raises SQLSTATE `23000` when `command_id = failID`.
- `faultDB`: wraps a pool. Per attempt, its transactions can:
  - fail `QueryRow` with a connection error without executing;
  - run the real `Commit`, then return `io.ErrUnexpectedEOF`;
  - run the real `Commit`, cancel a supplied `ctx`, and return an error wrapping `context.Canceled`.
- `cmdID(n)`: a deterministic non-nil UUID per integer.
- `workedRoundID` = `0190f3a8-7b3c-7d4e-8f00-000000000001` and `workedSeed` = bytes `00 01 … 1f`, which
  give `D = 9,410,000` µs (lifecycle spec §1).

### Step 5: integration tests (`make test-integration`)

Each test starts from its own `pgtest.MigratedDatabase`. Unless stated otherwise:
- the sequencer uses `DefaultConfig()`;
- the pool comes from `pgtest.Pool` (8 connections);
- rounds come from `createRound` with fresh round IDs.

**`concurrency_integration_test.go`.** All goroutines wait on one closed `start` channel. Any error that
is not expected fails the test after all goroutines have finished.

- **`TestAdmitConcurrentDuplicates`**:
  - Setup: 1 round; alice funded 1,000 credits; `newPool` with 16 connections.
  - 10 trials; in trial `k` (1 to 10), **64 goroutines** submit the identical `Request{PlaceBet alice 10,
    CommandID cmdID(k)}`.
  - After each trial:
    1. all 64 calls return nil error;
    2. all 64 `Admission`s are equal except `Duplicate`, and exactly one has `Duplicate = false`;
    3. `Sequence == k`, so duplicates consume no sequence;
    4. exactly one `admissions` row has the ID, and it equals the `Admission`;
    5. `admitted_sequence == k`;
    6. the round has exactly `k` hold rows, and alice's `held == 10·k`.
- **`TestAdmitConcurrentDuplicatesAcrossRounds`**:
  - Setup: rounds A and B; 16 connections.
  - 5 trials; per trial, **32 goroutines** submit `CashOut{bob}` under the same `CommandID`. Even
    goroutines target A, odd goroutines target B.
  - After each trial:
    1. exactly one `admissions` row has the ID; call its round W;
    2. every call targeting W returns the same `Admission`, exactly one with `Duplicate = false`;
    3. every call targeting the other round returns `ErrCommandIDReused`;
    4. the other round's `admitted_sequence` did not change in this trial.
- **`TestAdmitHotRoundOrdering`**, two subtests: `MaxInFlightPerRound` 4 and 16.
  - Setup: 1 round; 16 connections; **32 goroutines × 50 sequential submissions = 1,600 admissions**.
    Goroutine `g` alternates `PlaceBet{p<g>, 1}` and `CashOut{p<g>}`, with distinct command IDs. Each
    `p<g>` is funded 25 credits.
  - Checks:
    1. no error;
    2. the 1,600 returned sequences are exactly `{1..1600}`;
    3. each goroutine's sequences strictly increase in submission order;
    4. `admitted_sequence = 1600`, 1,600 rows, and effective times non-decreasing in sequence order;
    5. each `Admission` equals its row, matched by command ID;
    6. 800 hold rows, and each `p<g>` has `held = 25`;
    7. `admission_state = 'open'`.
- **`TestAdmitManyRoundsIndependentSequences`**:
  - Setup: **8 rounds × 8 goroutines × 25 `CashOut`s = 1,600 admissions**; 16 connections.
  - Checks:
    1. each round's returned sequences are `{1..200}`;
    2. each round has `admitted_sequence = 200` and non-decreasing effective times;
    3. 1,600 rows in total.
- **`TestAdmitConcurrentCrossRoundHolds`**:
  - Setup: alice funded 150; rounds A and B; **16 goroutines**, 8 per round, each `PlaceBet{alice, 100}`
    with a distinct ID.
  - Checks:
    1. exactly 1 success;
    2. 15 `*RejectedError` `insufficient_credits`;
    3. alice's `held = 100`;
    4. one hold row;
    5. the two `admitted_sequence` values sum to 1.
- **`TestAdmitHotRoundDoesNotStarveOtherRounds`**:
  - Setup: pool of 8; gate 4. A separate connection (not from the pool) holds round A's head
    `FOR UPDATE`.
  - Steps:
    1. **32 goroutines** admit to A.
    2. `waitLockWaiters(4)`; the number of lock-waiting backends is never above 4 while it is polled.
    3. An admission to round B returns within a 5 s `ctx`.
    4. The blocker rolls back.
  - Then:
    1. all 32 calls succeed;
    2. A's sequences are `{1..32}`.

**`barrier_integration_test.go`**

- **`TestWindowTransitions`**: a table over every cell of the S-8 table.
  - Each case drives a fresh round to the state through `Admit`:
    - `open`: nothing;
    - `started`: `StartRound`;
    - `closed`: plus `CloseRound` with `NotBefore = T0`;
    - `sealed`: plus `SettleRound`.
  - It then submits a command of the column's class and checks:
    - the result: `Admission`, or `*RejectedError` with the reason;
    - the head's state and sequence columns after it;
    - that a rejection wrote no row.
- **`TestBarrierRejectsLateCommands`**:
  - Setup: `started`, alice's bet admitted, then `CloseRound` admitted.
  - New `CashOut{alice}`, `PlaceBet{carol}` and a player-authority `StartRound` each give `round_closed`,
    with no row and no hold.
- **`TestAdmitDuplicateAfterWindowClosed`**: a `CashOut` admitted while `started`, then the window closed
  and sealed. Resubmitting the same `Request` returns the original `Admission` with `Duplicate = true`,
  not `round_closed` or `round_sealed`.
- **`TestBarrierSealIsFinal`**:
  - after `SettleRound`, every class gives `round_sealed`;
  - `admitted_sequence == sealed_sequence`;
  - a raw `UPDATE` that bumps `admitted_sequence` (after inserting the admission) gives `23514`
    `round_admission_heads_window_check`.
- **`TestClosureNotBefore`**:
  - `NotBefore` = now + 1 h gives `*ClosureNotDueError` with `EffectiveTimeUnixMicro < NotBeforeUnixMicro`;
    no row; state `started`.
  - `NotBefore = T0 − 1` gives `ErrInvalidRequest`.
  - `NotBefore = T0` admits.
- **`TestClosureNotBeforeEqualityAdmits`**:
  - After `StartRound`, `seedAdmission` a `CashOut` at `X` = now + 1 h.
  - `CloseRound` with `NotBefore = X + 1` gives `*ClosureNotDueError` with `EffectiveTimeUnixMicro == X`.
  - With `NotBefore = X` it is admitted with effective time exactly `X`.
- **`TestBarrierConcurrentCloseAndCashOuts`**:
  - Setup: 1 round, `StartRound` admitted, 16 connections. **32 goroutines** each submit `CashOut`s
    with fresh IDs, one after another, until one returns `round_closed`; then the goroutine stops.
    Every goroutine therefore overlaps the close.
  - After `admitted_sequence ≥ 200` (polled), the test admits `CloseRound` (`NotBefore = T0`) and records
    its sequence `c`.
  - When all goroutines are done:
    1. each goroutine got exactly one rejection, `round_closed`, on its last submission, and no other
       error;
    2. every other `CashOut` was admitted with a sequence `< c`, increasing within each goroutine;
    3. no admission has a sequence `> c`;
    4. admitted cash-outs have effective times ≤ the close's;
    5. `admitted_sequence == c`.
  - Then `SettleRound` is admitted at `c + 1`, the state is `sealed`, and there are `c + 1` rows.
- **`TestAdmissionStreamDrivesCrashRound`** (end to end with crash/v1 `Decide`, without LBT-12):
  - Setup: `workedRoundID` with `workedSeed`, so `D = 9,410,000`; alice and bob funded 1,000 each.
  - Steps:
    1. `PlaceBet alice 100` admitted at sequence 1.
    2. `PlaceBet bob 100` at 2.
    3. `StartRound` at 3; `T0` := its effective time.
    4. `CashOut alice` at 4.
    5. `seedAdmission` `CashOut zed` at exactly `T0 + D` (5).
    6. `CashOut bob` at 6: its effective time must be exactly `T0 + D`.
    7. `CloseRound` with `NotBefore = T0 + D + 1`: `*ClosureNotDueError` with effective time `T0 + D`.
    8. `CloseRound` with `NotBefore = T0 + D` at 7, effective time `T0 + D`.
    9. `CashOut alice`: `round_closed`.
    10. `SettleRound` at 8.
    11. `PlaceBet carol`: `round_sealed`.
  - Replay sequences 1 to 8 from the `admissions` rows: `DecodeCommand`, then `Decide` with
    `game.Input{sequence, effective time}`, then `Evolve`, starting from `InitialState`. Expect:
    - 1, 2, 3 accepted;
    - 4 accepted. If its `e` is not in `(0, D)`, fail with a message naming the timing: that needs a
      second admission within the same microsecond or a 9.41 s stall;
    - 5 rejected `no_bet`;
    - **6 rejected `deadline_passed` (equality loses)**;
    - **7 accepted, with `RoundCrashed.CrashDeadlineUnixMicro == T0 + D`**;
    - 8 accepted, with `BetLost{bob}`;
    - no fail-closed error;
    - two hold rows (alice and bob).

**`admit_integration_test.go`**

- `TestAdmitBasics`: fields, `Duplicate = false`, the row equals the `Admission`, and `Input()` equals
  `{Sequence, EffectiveTimeUnixMicro}`.
- `TestAdmitRetryReturnsOriginal`: a sequential resubmit gives `Duplicate = true` with the same sequence
  and time, no second hold, and an unchanged `admitted_sequence`.
- `TestAdmitCommandIDReused`: the same ID with other bytes in the same round, and the same bytes in
  another round, each give `ErrCommandIDReused` with nothing written.
- `TestAdmitRoundNotFound`: `ErrRoundNotFound`.
- `TestAdmitRulesMismatch`: a round created with rules version 2, then admitted with crash/v1, gives
  `ErrInvalidRequest`.
- `TestAdmitHoldsCredits`:
  - alice funded 100: `PlaceBet 60` is admitted with `held = 60`;
  - `PlaceBet 50` (another ID) gives `insufficient_credits`, with no row and no sequence consumed: the
    next admission gets sequence 2;
  - an unfunded player gives `insufficient_credits`;
  - amounts 0, −1 and 1,000,000,001, a system-authority `PlaceBet` and an empty player are admitted
    without a hold.
- `TestAdmitRejectsNulPlayer`: `PlaceBet` and `CashOut` with `"a\x00b"` give `ErrInvalidCommand`, and
  nothing is written.
- `TestLookup`:
  - an unknown ID gives `ErrNotFound`;
  - an admitted command gives `OutcomePending`;
  - after the test inserts the matching chain record directly:
    - built with `chain.EncodeCommand`: the admission's bytes, ID and time; `prevHash` = the genesis
      hash; result rejected `no_bet`; no events;
    - advancing `round_execution_heads` to 1 with token 0;
  - `Lookup` gives `OutcomeRejected` `no_bet`. A second record with an accepted result and empty events
    gives `OutcomeAccepted`.
- `TestEffectiveTimeReadAfterLock`:
  1. A blocker transaction holds the head `FOR UPDATE`.
  2. `Admit` starts in a goroutine.
  3. `waitLockWaiters(1)`.
  4. The blocker reads `r = orvex.unix_micro(clock_timestamp())` and commits.
  5. The admitted effective time is ≥ `r`. The admitting transaction began before `r`, so its time is not
     the transaction start (ADR-002).
- `TestEffectiveTimeMonotonic`:
  - 50 sequential admissions have non-decreasing times;
  - after `seedAdmission` at `X` = now + 1 h, the next three admissions each have effective time exactly
    `X` and consecutive sequences.
- `TestAdmitLockTimeout`:
  - Config: `LockTimeout` 100 ms, `IdleInTransactionTimeout` 50 ms. A blocker holds the head.
  - `Admit` gives `ErrBusy` within 2 s, and nothing is written.
  - After the blocker rolls back, the same `Request` is admitted at sequence 1.
- `TestAbandonedAdmissionReleasesLock`:
  1. A raw connection runs `BEGIN` and `orvex.admit_command(…, 3000, 200)`: outcome `admitted`. It never
     commits.
  2. `Admit` of another command (default config) succeeds with sequence 1.
  3. The raw connection's next statement fails: the session was terminated.
  4. The abandoned command has no row.
- `TestAdmitCommandSessionSettings`:
  - In a raw transaction after `orvex.admit_command(…, 1234, 567)`:
    - `SHOW lock_timeout` is `1234ms`;
    - `SHOW idle_in_transaction_session_timeout` is `567ms`.
  - With `ALTER DATABASE … SET synchronous_commit = off` and a new connection, `SHOW synchronous_commit`
    inside that transaction is `on`.
  - After `COMMIT`, the three session values are back to `0`, `0` and `off`.
- `TestFastPathSkipsLock`:
  - Config: `LockTimeout` 30 s. Setup: a command admitted, then the window closed. A blocker holds the
    head.
  - Each call below runs with a 2 s `ctx`:
    - resubmitting the admitted command returns `Duplicate = true`;
    - a new `CashOut` returns `round_closed`;
    - an unknown round returns `ErrRoundNotFound`.
  - A lock wait would end in a `ctx` error instead.

**`commit_integration_test.go`**

- **`TestAdmitAckFollowsCommit`**:
  1. `installCommitHooks` with `holdID = H` and advisory key `K`.
  2. The test holds `K` on its own connection (`pg_advisory_lock`).
  3. `Admit(H)` runs in a goroutine; `waitAdvisoryWaiter`.
  4. While `COMMIT` is held:
     - `Admit` has not returned (its result channel is empty);
     - another connection sees no row for `H` and `admitted_sequence = 0`;
     - `Lookup(H)` gives `ErrNotFound`.
  5. Release `K`.
  6. `Admit` returns `{Sequence 1, Duplicate false}`, and right afterwards another connection sees the row,
     equal to the `Admission`.
- **`TestAdmitNoAckWhenCommitFails`**:
  - `Admit(F)` with the fail hook returns `ErrInternal`, and `postgres.SQLState(err) == "23000"`.
  - There is no row and `admitted_sequence` is unchanged.
  - A following `Admit` of another command gets sequence 1 (no gap).
- **`TestAdmitAmbiguousCommitCommitted`**: `faultDB` runs the real `Commit` and then returns
  `io.ErrUnexpectedEOF` on attempt 1.
  - `Admit` returns `{Sequence 1, Duplicate true}`;
  - exactly one row;
  - `Lookup` agrees.
- **`TestAdmitAmbiguousCommitAborted`**:
  1. Hold `COMMIT` as in `TestAdmitAckFollowsCommit`.
  2. `pg_terminate_backend` the waiting pid from `waitAdvisoryWaiter`, then release `K`.
  3. `Admit` returns `{Sequence 1, Duplicate false}` from its retry, and there is exactly one row.
- **`TestAdmitOutcomeUnknownWhenContextEnds`**: `faultDB` runs the real `Commit`, cancels the call's
  `ctx`, and returns an error wrapping `context.Canceled`.
  - `Admit` returns an error matching both `ErrOutcomeUnknown` and `context.Canceled`.
  - The row exists.
  - A new `Admit` of the same `Request` returns `Duplicate = true` with the same sequence.
- **`TestAdmitRetriesConnectionLossBeforeCommit`**: `faultDB` fails the call on attempt 1 without
  executing it. `Admit` returns `Duplicate = false`, and there is exactly one row.

### Step 6: docs

Make the `docs/architecture/persistence.md`, `README.md` and `AGENTS.md` edits from the files table.

### Verification for the implementation run

Each must exit 0:

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

- The `-count=5` run is the flakiness check for the concurrency tests. It must pass all five times.
- `git diff --stat` must show no change to `docs/adr/`, `internal/game/`, `internal/audit/`,
  `internal/canonjson/`, migrations `0001` to `0006`, `go.mod`, `go.sum` or any golden fixture.
- If a test contradicts this spec, find which side is wrong. Never weaken a concurrency invariant to make a
  run pass.

## 11. Out of scope and open items

Out of scope:
- in this run: any Go, SQL, migration, CI or `go.mod` change;
- LBT-12:
  - Transaction B, posting rules, hold releases, the outbox;
  - production round creation;
  - the scheduler that issues lifecycle commands;
- LBT-13:
  - the gateway and gRPC transport;
  - client command-ID generation;
  - rate limiting;
  - mapping errors to client responses;
- LBT-14: ownership leases, failover, roles and grants, and `EXECUTE` on `admit_command`;
- LBT-15:
  - load tests;
  - tuning the gate capacity and timeouts;
  - measuring hot-row churn on the head and lock waits;
- a rejection-evidence table;
- `LISTEN`/`NOTIFY` wake-up;
- verifier checks of the barrier.

Open for the engineer:
1. The S-5 fast path, which departs from LBT-10 P-23's "first statement locks" (§2 item 1).
2. The S-7 clamp of effective times to the previous admission (§2 item 2).
3. No rejection evidence in LBT-11 (S-13). ADR-002's open validation stays open.
4. The S-8 closure guard trusts the scheduler's `NotBeforeUnixMicro`. A scheduler bug that passes a value
   below `T0 + D` but ≥ `T0` can still close the window early. `Decide` would then reject the close
   (`crash_not_due`) and the round would be stuck. The only alternative found exposes the secret deadline
   to the admission path.
5. Defaults: gate 4, `LockTimeout` 3 s, `IdleInTransactionTimeout` 1 s, `MaxAttempts` 3 (S-4, S-12).
   LBT-15 tunes them.
6. A missing account answers `insufficient_credits` (S-11).
7. Optional: deterministic command IDs for lifecycle commands (for example derived from the round ID and
   the command type), so that two schedulers that race dedupe instead of getting `lifecycle_order`. Not
   proposed. LBT-14 ownership makes such races rare.
8. The LBT-10 test edits caused by the seventh migration (§2 item 11, §10 step 1).

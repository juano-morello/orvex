# Atomic execution commit, ledger and outbox (LBT-12)

Status: **APPROVED (2026-10-15).** The engineer approved E-1 to E-17 as written, with no edits, and
accepted every point of section 2. The engineer addendum at the end of this document resolves section 2
item 14.

- Linear: [LBT-12](https://linear.app/lbtworks/issue/LBT-12/implement-atomic-execution-commit-ledger-and-outbox),
  milestone M2 · Durable Execution. Blocked by LBT-10 (done) and LBT-11 (in progress).
- Linear text: "Commit domain events, terminal result, balanced simulated-credit postings, state
  version/processed sequence and outbox in one transaction. Include optimistic checks and fencing.
  Acceptance: injected failure leaves no partial effects; duplicate payout impossible; cross-round balance
  protection. Reference ADR-005."
- Governing ADRs: ADR-005 (primary: Transaction B, failure semantics, cross-round balances), ADR-001,
  ADR-003, ADR-004 (settlement, balanced simulated-credit ledger), ADR-006 (critical failure tests),
  ADR-002 and ADR-007 (mirrors in `docs/adr/`).
- Approved inputs:
  - "LBT-10 spec": `docs/specs/17380a6e_postgresql-schema-migrations.md` (P-1 to P-26), merged.
  - "LBT-9 spec": `docs/specs/8bebb70f_canonical-codec-audit-cli.md` (A-1 to A-20), merged.
  - "Math spec": `docs/specs/e96d124d_crash-v1-math-spec.md` (D-1 to D-14).
  - "Lifecycle spec": `docs/specs/712614a2_crash-lifecycle-rules.md` (LBT-8, §2 items 1 to 13).
  - "LBT-11 spec": the approved admission sequencer spec (S-1 to S-14). It is being implemented in
    parallel and is not on `main`. The copy read for this plan is outside the repo and is not copied in.

Decision IDs are `E-n`, so they do not clash with `D-n`, `A-n`, `P-n` or `S-n`.

## 0. What this run delivers

- This document is the deliverable. The engineer approves its copy under `docs/specs/`.
- **Builder, this run: make no repository changes.** Do not write Go code, SQL, migrations or CI changes,
  and do not touch `go.mod`, `go.sum`, `Makefile`, `.golangci.yml`, `README.md`, `AGENTS.md` or
  `docs/adr/`. The gates must still pass on the unchanged tree: `make test`, `make lint`, `go vet ./...`,
  `make build`, `make golden-check`.
- After approval, section 12 is the implementation plan.
- **Implementation precondition: LBT-11 is merged on `main` and the worktree is based on it.** Check that
  these exist:
  - `migrations/0007_admission_window.up.sql` (or LBT-11's migration under another number);
  - `internal/runtime/admission` with `Sequencer`, `Admit`, `CrashV1Policy`, `Lookup`;
  - everything LBT-10 provides (listed in section 1).

  If LBT-11 is not merged, stop and report "blocked on LBT-11". If LBT-11's merged names differ from the
  LBT-11 spec, use the merged names: this spec fixes behavior, not identifiers. LBT-12's migration takes
  the next free number after LBT-11's (`0008` if LBT-11 merged `0007`). If the merged LBT-11 behavior
  differs from LBT-11 S-10, S-11 or §7 in anything this spec relies on (which admissions carry a hold, the
  hold amount, holds inserted with the admission), stop and report.

## 1. Current state (verified while planning)

- `main` is at `cc79dcb`. Toolchain: go1.26.5. Docker Engine 29.8.2 with the pinned
  `postgres:18.6-trixie` image. `go.mod` requires `github.com/cockroachdb/apd/v3 v3.2.3`,
  `github.com/jackc/pgx/v5 v5.11.0` and `github.com/testcontainers/testcontainers-go v0.44.0`.
- LBT-10 is merged:
  - migrations `0001` to `0006`: `rounds`, `round_secrets`, `admissions`, `round_admission_heads`,
    `chain_records` (hash `CHECK`, previous-record foreign key, `OX008` binding trigger, deferred `OX004`
    fencing trigger), `round_execution_heads` (forward-only, owner change needs a higher token),
    `domain_events`, `round_snapshots`, `ledger_accounts` (trigger-maintained totals, floor `CHECK`),
    `ledger_transactions` (unique per chain record), `ledger_postings` (two-sided), `ledger_holds`,
    `ledger_hold_releases` (need the admission's chain record), `outbox_messages` (dedupe key, `OX006`);
  - `internal/persistence/postgres`: `AdmissionTxOptions`, `ExecutionTxOptions`, `SnapshotReadTxOptions`,
    `SQLState`, `IsRetryable`, `CodeAppendOnly` … `CodeRecordBinding` (`OX001` to `OX008`);
  - `pgtest`: `Run`, `MigratedDatabase`, `Pool`, `WaitUntilBlocked`, `LoadHistory`, `Fingerprint`;
  - `eventstore.ExportHistory` (exports up to the newest snapshot, P-24).
- LBT-11 is not on `main`: no `0007`, no `internal/runtime/`.
- The game contract (`internal/game/codec.go`) has `EncodeState` but **no `DecodeState`**. Snapshot
  `state` bytes therefore cannot be turned back into a state today.
- crash/v1 facts this spec relies on:
  - `Decide` returns `*RejectionError` (13 codes) or a fail-closed `ErrInvalidState`/`ErrArithmetic`
    (lifecycle item 8). With the D-7 limits and the 10,000-bet cap, the fail-closed paths are defence in
    depth and unreachable from a valid state (lifecycle item 3).
  - Every `PlaceBet` that `Decide` accepts has player authority, a non-empty player and an amount inside
    the round's limits, which lie inside `[1, 1,000,000,000]`. LBT-11 S-10 places exactly that hold.
  - `BetCashedOut.PayoutCredits ≥ BetCredits ≥ 1`, because the multiplier is at least 1.0000x (D-3, D-8).
  - `SettleRound` emits one `BetLost` per open bet, then `RoundSettled` with `TotalBetCredits` and
    `TotalPayoutCredits` (lifecycle item 7).
  - No event carries a secret before it is due: `RoundCrashed` reveals the crash outcome, `RoundSettled`
    the seed (`events.go`; math spec §7). Commands carry none.
- LBT-9 provides `chain.EncodeGenesis`, `chain.EncodeCommand`, `chain.LinkHash`, `chain.StateHash`,
  `chain.Decode`, `replay.Run(…, replay.StateOnly, visit)` and `verification.Verify`.
- pgx v5.11.0 `TxOptions` has `BeginQuery` (`tx.go:47`), which replaces the generated `BEGIN` and runs
  through `Exec` without arguments, that is the simple protocol.
- `internal/persistence/postgres/migrate_test.go` `TestEmbeddedMigrations` requires every migration's
  first line to match `\(LBT-10 spec P-[0-9]+[^)]*\)`. LBT-11's `0007` header names "LBT-11 spec", so
  LBT-11 must already widen it; §12 step 1 covers both cases.
- Prototypes, on the pinned image in a throwaway container outside the repo, with migrations `0001` to
  `0006` applied from the repo files and the worked-example genesis, admissions 1 to 3 and a funded
  player loaded:
  1. pgx v5.11.0 `BeginTx` with `BeginQuery` = `BEGIN ISOLATION LEVEL READ COMMITTED READ WRITE; SELECT
     set_config('lock_timeout', '3000ms', true), set_config('idle_in_transaction_session_timeout',
     '1000ms', true), set_config('synchronous_commit', CASE current_setting('synchronous_commit') WHEN
     'off' THEN 'on' ELSE current_setting('synchronous_commit') END, true)`. Inside the transaction:
     `read committed`, `3s`, `1s`. After `COMMIT` the session values were back to their defaults. With a
     session default of `synchronous_commit = off` the transaction ran with `on`; with `remote_apply` it
     kept `remote_apply`.
  2. Player funded exactly 100 with a hold of 100 for admission 1. Inserting the bet posting **before**
     the hold release failed with `23514` `ledger_accounts_floor_check` (`debits 100 + held 100 > credits
     100`). Releasing first, then posting, committed with `debits_posted = 100`, `held = 0`.
  3. A deferred constraint trigger shaped like §6 (`OX009`): inserting chain record 1 without releasing
     admission 1's hold failed at `COMMIT` with `OX009` and left `processed_sequence = 0`. With the
     release in the same transaction the commit succeeded.
  4. A unique partial index `(round_id, event_sequence) WHERE event_sequence IS NOT NULL` on
     `ledger_postings`: a second posting for event 1 failed with `23505` `ledger_postings_event_key`.
  5. `INSERT … ON CONFLICT (account_id) DO NOTHING` of a system account twice: one row, and the
     `ledger_accounts_guard` trigger accepted both statements.
  6. A multi-row `INSERT … SELECT … FROM unnest(…)` into a table with a self foreign key like
     `domain_events_prev_fkey` succeeded with rows in any order (non-deferred foreign keys are checked at
     the end of the statement).
- Literal cross-check values, computed with Python's standard library for E-8 and E-9
  (`uuid8(s)` = the first 16 bytes of SHA-256 of the UTF-8 string `s`, with byte 6 = `(b6 & 0x0f) | 0x80`
  and byte 8 = `(b8 & 0x3f) | 0x80`):
  - house account `uuid8("orvex/ledger/v1/account:global/house")` =
    `4688699a-7bf4-8e07-8cec-c5f6bef86c36`;
  - worked-example round account `uuid8("orvex/ledger/v1/account:0190f3a8-7b3c-7d4e-8f00-000000000001/round_pool")`
    = `13565418-c5ee-8f13-85c6-552f9e8755bd`;
  - ledger transaction `uuid8("orvex/ledger/v1/transaction:0190f3a8-7b3c-7d4e-8f00-000000000001/<seq>")`:
    sequence 1 `862c9c4b-3cb4-847f-80eb-d0364134723c`, 12 `5687be29-0e53-89bc-886e-babaeada505f`,
    17 `4357c225-96f0-803b-a757-26ca7f5055ac`, 21 `d61426fa-cde4-8067-a6f6-163fb1aeb196`.
- Worked-example expectations under the proposed rules (§8 has the full table): six postings, seven hold
  releases, six ledger transactions, snapshots at 0, 9, 19 and 21, 23 outbox messages; the round account
  ends at 0 and the house account at −757,599,784.

## 2. ADR alignment, conflicts and flagged interpretations

No decision contradicts an accepted ADR. Several decisions depart from, extend or interpret approved
decisions and need the engineer's explicit attention (AGENTS.md "Flag conflicts explicitly"). Copy this
section into the implementation PR.

1. **LBT-10 P-23 and §6, "first statement is the compare-and-set" (deviation, needs approval).** E-6 sends
   Transaction B's `BEGIN` together with one `SELECT set_config(…)` that sets `lock_timeout`,
   `idle_in_transaction_session_timeout` and, only when it is `off`, `synchronous_commit`. It reads no
   table and takes no lock. The compare-and-set is the first statement that touches data, and B commits
   only when it affected exactly one row (§6 item 6). This is the same kind of deviation as LBT-11 §2
   item 1.
2. **LBT-10 P-23 rule 1, "every transaction states its isolation explicitly" (interpretation).** The
   executor reads immutable rows (the next admission, a player's account ID, the head after a conflict)
   with single statements outside any explicit transaction (E-4, E-7). One statement sees one snapshot
   at any isolation level. Every multi-statement read (the cold load) uses `SnapshotReadTxOptions()`.
3. **ADR-005 "read … known state version" and "retry from authoritative state" (interpretation).** E-3
   keeps the round's state in memory between commands. The authoritative state stays the chain in
   PostgreSQL: the cache is a derived copy, loaded by replaying the chain, and every commit proves it
   current through the compare-and-set on `processed_sequence` plus the previous-record foreign key on
   `prev_hash`. On any conflict the copy is discarded and reloaded.
4. **ADR-001 and LBT-10 P-20, snapshot `state` (consequence).** `round_snapshots.state` stays NULL. The
   codec has no `DecodeState`, so recovery replays from genesis, and snapshots serve as checkpoints:
   the cold load compares its replayed state hashes with them (E-3, E-13). Snapshot-based recovery needs
   a `game.Codec` contract change and is left to LBT-14.
5. **LBT-10 P-21, ledger (addition, needs approval).** Migration `0008` (E-11) adds:
   - a unique index: at most one posting per chained event (`ledger_postings_event_key`);
   - a deferred check that every hold of an executed admission is released in the record's transaction
     (`OX009`, a new code in P-11's class `OX`).
   Both strengthen P-21. The unique index replaces the non-unique `ledger_postings_event_idx` on the same
   columns.
6. **LBT-10 §6 item 4, "`23514` on `ledger_accounts_floor_check`: insufficient available credits, a
   business outcome" (interpretation).** That holds for Transaction A. In Transaction B a floor violation
   is a bug: B never increases a player's `debits_posted + held` (E-10), so E-15 classifies it as
   `ErrInternal`.
7. **LBT-10 P-22, "payload (opaque; LBT-13 defines the format)" (interpretation).** E-12 fixes what is
   stored: the exact chain record bytes. LBT-13 still defines the wire format it derives from them.
8. **LBT-9 §11, "LBT-12 (ledger): the verifier gains a ledger stage … history format v2" (deviation,
   needs approval).** This spec does not add the ledger stage, the v2 history format or oracle changes.
   E-9 makes every posting a pure, deterministic function of the chained events, with deterministic
   transaction and account IDs, so a later stage can recompute and compare them. Section 13 proposes the
   follow-up issue. Postings stay outside the hash chain (no chain v2), as LBT-9 §11 allows.
9. **Lifecycle spec item 7, "puts all loss postings in the same execution transaction as `RoundSettled`"
   (interpretation).** E-9 posts no row per `BetLost`. The stake left the player at `BetPlaced`; the
   round's single net settlement posting, written in the `RoundSettled` transaction, moves the round's
   result to the house. Per-loss postings would rewrite the same two account rows up to 10,000 times in
   one transaction.
10. **LBT-10 §9 and open item 4, LBT-11 §7, round creation (addition, needs approval).** E-16 makes
    LBT-12 the owner of production round creation, as both specs propose: round rows, secret, heads,
    genesis record, snapshot 0, the round's ledger account and the genesis outbox message, in one
    transaction.
11. **ADR-007 layout (addition, needs approval).** `internal/runtime/execution` is in the proposed layout.
    `internal/ledger` (pure posting rules and ledger IDs) is new (E-1).
12. **LBT-9 A-7 and LBT-11 §7, "every admitted command is executed exactly once" (consequence).** A
    command whose `Decide` fails closed gets no record (A-7). The round halts before it, later admissions
    stay unexecuted and its credit hold stays held until operators act (E-14).
13. **LBT-10 P-19, leases (alignment).** Transaction B checks the fencing token, not `lease_expires_at`.
    A stale owner is one whose token was superseded; an expired lease that nobody took over still holds
    the current token, and its commits are safe (E-7).
14. **LBT-11 open item 4 (inherited risk).** If a `CloseRound` is admitted before `T0 + D`, `Decide`
    rejects it (`crash_not_due`) and the window stays closed. LBT-12 executes that record faithfully; the
    round can then never settle, and the stakes stay in the round account. LBT-12 cannot repair it
    without breaking replay.
15. **LBT-11 approval status.** The LBT-11 copy is headed "PROPOSED" with unticked boxes. This spec treats
    S-1 to S-14 as approved, as the request states. If approval changed S-10, S-11, S-12 or §7, decisions
    E-4, E-10 and E-15 need review.
16. **LBT-10 and LBT-11 tests (consequence).** `0008` changes migration-count and down-path assertions:
    `TestEmbeddedMigrations`, `TestMigrateCLI`, `TestMigrateDownRefusesAuditData`. LBT-11's `TestLookup`
    changes only if it inserts a chain record for an admission that holds credits without the release.
    §12 step 1 gives the edits.
17. **ADR-006 "deterministic failpoints" (scope).** Tests inject failures with a database wrapper,
    test-only SQL objects and concurrent sessions (E-17). Production failpoints belong to LBT-15.
18. **ADR-001 "preserve … ledger references" (alignment).** Postings reference `(round_id,
    event_sequence)`, ledger transactions reference `(round_id, sequence)`, and hold releases reference
    the chain record.
19. **ADR-003 public projection (alignment).** Outbox payloads are chain record bytes, which hold no
    secret before it is due. `round_snapshots.state`, `round_secrets` and the in-memory state never reach
    the outbox (E-12).
20. **AGENTS.md dependencies.** No new module.

| ADR clause | Where satisfied |
|---|---|
| ADR-005 Transaction B: read next admitted command and known state version | E-3, E-4 |
| ADR-005: Decide and Evolve deterministically | E-5 |
| ADR-005: atomically validate expected version, next processed sequence and fencing token | E-6, E-7 |
| ADR-005: append events, one terminal result, balanced postings, outbox; advance processed/state versions | E-5, E-6, E-9, E-12, E-13 |
| ADR-005: on conflict discard the computed transition and retry from authoritative state | E-3, E-7, E-15 |
| ADR-005 failure semantics: before execution commit no partial outcome; after commit return the existing durable result; stale owner cannot commit; outbox at least once with consumer dedupe | E-6, E-7, E-12, E-15 |
| ADR-005 cross-round balances: account-level transactional debit/reservation checks; settlement barrier | E-10, E-11, E-5 (strict order) |
| ADR-001: immutable ordered history, replay, per-round chains, checkpoints, ledger references; a bet settles at most once | E-3, E-5, E-9, E-11, E-13 |
| ADR-003: pure Decide/Evolve; durable terminal results for rejections; public projection hides secrets | E-5, E-12, E-14 |
| ADR-004: authoritative numeric outputs in events; settlement after the barrier; balanced simulated-credit ledger | E-9, E-10, E-5 |
| ADR-006: real PostgreSQL integration tests; conflicting per-round commits, rollback on ledger failure, stale fencing token, crash before/after commit, double cash-out, cross-round overspending, ambiguous commit, settlement barrier | E-17, section 11 |
| ADR-007: `internal/runtime/execution`, `migrations/`, `make test-integration`, thin binaries | E-1 |

## 3. Decision summary

Tick a box to approve, or edit the value. The implementation run uses approved values only.

| ID | Decision | Proposed value | Approve |
|---|---|---|---|
| E-1 | Packages, layout, migration | `internal/runtime/execution` (executor, round creation, crash/v1 execution policy); `internal/ledger` (pure posting rules, ledger IDs); migration `0008_execution_ledger`; two small additions to `internal/persistence/postgres`; library only; no new module | [x] |
| E-2 | Go API | `Executor.ExecuteNext(ctx, Lease) (Outcome, error)` executes exactly the next admitted command of one round; `Drain`, `Forget`, `CreateRound`; `Lease{RoundID, FencingToken}` supplied by the caller (LBT-14) | [x] |
| E-3 | Authoritative state | per-round in-memory state, loaded by replaying genesis plus every chain record (state replay, snapshot hashes checked), kept between commands, proven current by the compare-and-set and the `prev_hash` foreign key, discarded on any conflict | [x] |
| E-4 | Reading the next command | one primary-key read of admission `processed + 1` joined with its holds, before Transaction B, without locks; player accounts for payouts by one lookup, memoized per round | [x] |
| E-5 | Transition and chain record | Decide, then Evolve every event, outside any transaction; LBT-9 command record from the admission's exact bytes, the cached head hash and contiguous event sequences; strict sequence order | [x] |
| E-6 | Shape of Transaction B | `BEGIN` with transaction-local timeouts; the compare-and-set alone; one pipelined batch of inserts in a fixed order; `COMMIT` | [x] |
| E-7 | Optimistic checks and fencing | compare-and-set on `processed_sequence = k−1 AND fencing_token = t`; `prev_hash` foreign key; deferred `OX004`; on conflict read the head: other token → `ErrFenced`; same record bytes → adopt; otherwise reload and retry | [x] |
| E-8 | Ledger accounts | existing player accounts; per round a `round_pool` system account; one global `house` system account; deterministic UUIDv8 IDs from SHA-256 | [x] |
| E-9 | crash/v1 posting rules | `BetPlaced`: player → round from the hold; `BetCashedOut`: round → player; `RoundSettled`: one net posting between round and house; `BetLost`, `RoundStarted`, `RoundCrashed`: none; one ledger transaction per record with postings, deterministic ID | [x] |
| E-10 | Holds and cross-round protection | release every hold of the admission in its record's transaction, whatever the result, before any posting; a `BetPlaced` consumes exactly its hold; B never raises `debits_posted + held` | [x] |
| E-11 | Migration `0008` | unique `ledger_postings_event_key` (replaces `ledger_postings_event_idx`); deferred `chain_records_holds_released` trigger raising `OX009` | [x] |
| E-12 | Outbox | one message per chain record, genesis included; `message_index` 0; topic `round.record.v1`; payload = the exact record bytes; consumers dedupe on `(round_id, sequence, message_index)` | [x] |
| E-13 | Snapshots | sequence 0 at round creation; in Transaction B for every record that emits `RoundStarted`, `RoundCrashed` or `RoundSettled`; engine-computed hash; `state` NULL | [x] |
| E-14 | Fail-closed `Decide` | no transaction, no record, no result; `*FailClosedError`; the round halts in the process until `Forget`; holds stay; no durable fault row | [x] |
| E-15 | Errors and retries | mapping table in E-15; at most 3 attempts with full-jitter backoff (5 ms doubling, cap 50 ms) for `40001`/`40P01`, connection loss and ambiguous commits; reloads count as attempts; never retry `55P03`, fencing, fail-closed or constraint bugs | [x] |
| E-16 | Round creation | `Executor.CreateRound` writes the round, secret, both heads, genesis, snapshot 0, `round_pool`, `house` (if absent) and the genesis outbox message in one transaction; idempotent by round ID and genesis bytes | [x] |
| E-17 | Test strategy | real PostgreSQL through `pgtest`; golden histories reproduced byte for byte; failures injected by a `faultDB` wrapper, test-only triggers in schema `orvex_test`, backend termination and concurrent takeovers; no production test seams | [x] |

## 4. Decisions

### E-1. Packages, layout and migration

**Proposal.**
- `internal/runtime/execution` (ADR-007 `internal/runtime/{…,execution,…}`): the executor, the cold load,
  Transaction B, round creation, the crash/v1 execution policy (snapshot cadence, finality) and the error
  types. It imports `internal/game`, `internal/game/crash/v1` (policy only), `internal/audit/chain`,
  `internal/audit/replay`, `internal/canonjson`, `internal/ledger`, `internal/persistence/postgres`,
  `internal/buildinfo` and pgx v5.
- `internal/ledger`: pure code with no I/O, clock or randomness: account references, postings, the
  `Rule` interface, the crash/v1 posting rule and the deterministic ID functions. It imports only the
  standard library, `internal/game` and `internal/game/crash/v1`.
- Migration pair `migrations/0008_execution_ledger.{up,down}.sql` (E-11), or the next free number.
- `internal/persistence/postgres` gains `ExecutionTxOptionsWithTimeouts` (E-6) and `CodeHoldUnreleased =
  "OX009"` (E-11).
- Library only. Wiring into `cmd/engine` (engine loop, leases, scheduler) is LBT-14.
- No change to `internal/game/**`, `internal/audit/**`, `internal/canonjson/**`,
  `internal/runtime/admission` (except a conditional test fix, §12 step 1), migrations `0001` to `0007`,
  `go.mod` or any golden fixture.

**Rationale.**
- ADR-007's layout and LBT-11's precedent (`internal/runtime/admission`): game-specific runtime policy
  lives next to the runtime that uses it, not in the rules package.
- The posting rule is pure and is what a later audit ledger stage must recompute (LBT-9 §11). In
  `internal/ledger` that stage can import it without importing pgx or the executor.

**Alternatives.**
- Posting rules inside `internal/runtime/execution`: no new top-level package, but the future audit stage
  would import a runtime package with database dependencies, or the code moves later.
- `internal/runtime/execution/ledger` as a pure subpackage: works, with an audit → runtime import
  direction.
- Posting rules in `internal/game/crash/v1`: the frozen rules package would learn a runtime concept, and
  its import allowlist (`TestNoIOImports`) would need widening. LBT-11 S-1 rejected the same idea for
  admission policy.

### E-2. Go API

**Proposal.** §7 is normative. In short:
- `New(db DB, reg *game.Registry, cfg Config, policies ...Policy) (*Executor, error)`. `DB` is
  `BeginTx`, `Query` and `QueryRow`; `*pgxpool.Pool` implements it.
- `ExecuteNext(ctx, lease) (Outcome, error)` executes exactly one admitted command of `lease.RoundID`,
  the one at `processed_sequence + 1`, or reports that none is pending (`Outcome.Executed == false`).
- `Drain(ctx, lease, limit) (int, error)` calls `ExecuteNext` until nothing is pending, `limit` commands
  ran, or an error.
- `Forget(roundID)` drops the round's in-memory entry, including a halt (E-14).
- `CreateRound(ctx, NewRound) (Created, error)` (E-16).
- `Lease{RoundID, FencingToken}` is the caller's claim to execute the round under that token. LBT-14
  obtains it. Until then a single engine uses the round-creation token 0 (P-19).
- `Outcome` carries the sequence, command ID, terminal result, decoded events, record bytes and hash,
  `Adopted` (the record was committed by an earlier attempt whose outcome was unknown) and `Final` (the
  policy reports the round final).
- `ExecuteNext` calls for one round serialize in the process; different rounds run in parallel.

**Rationale.**
- One command per call keeps the unit of work equal to ADR-005's Transaction B and to LBT-9's chain unit
  (A-7).
- The token comes from the caller, because ownership is LBT-14's. The executor needs only the value it
  must prove in the compare-and-set.
- `Outcome.Events` lets a scheduler (LBT-14) read `RoundStarted.StartedAtUnixMicro`, which with the
  round's config gives the `T0 + D` that LBT-11's `CloseRound` needs as `NotBeforeUnixMicro`.

**Alternatives.**
- A long-running `Run(ctx, lease)` loop with polling: mixes wake-up and lease handling into LBT-12;
  LBT-14 owns the engine loop and bounded mailboxes.
- The executor acquires leases itself: LBT-14's scope.
- Expose the in-memory state: callers could mutate a cached slice (crash/v1 `State.Bets`).

### E-3. Authoritative state: replayed chain, cached in memory

**Proposal.**
- The round's authoritative state is its chain in PostgreSQL. The executor keeps, per round, an entry
  `{state, processed, head, nextEventSequence, playerAccounts, halt}`:
  - `state` is the state after record `processed`;
  - `head` is the link hash of record `processed`;
  - `playerAccounts` memoizes player ID → account ID (E-4).
- **Cold load** (§5.2), when the entry has no state: one `SnapshotReadTxOptions` transaction reads the
  `rounds` row, the secret config, the execution head, records `0..processed` and the snapshot rows up to
  `processed`. It decodes the genesis record, calls `DecodeConfig` and `InitialState`, decodes every
  command record and its events, and folds the events with `replay.Run(…, replay.StateOnly, visit)`. At
  every snapshot sequence `visit` compares `chain.StateHash` of the replayed state with the stored
  `state_hash`. A mismatch, a decode failure or an inconsistent row is `ErrCorrupt`.
- **Hot path.** A command executes against the cached state. Nothing re-reads the state.
- **Proof of currency at commit.** The compare-and-set requires `processed_sequence = k − 1` under the
  lease token, and the new record's `prev_hash` must equal the stored hash of record `k − 1` (foreign key
  `chain_records_prev_fkey`). The cached state is a deterministic function of records `0..k−1`, whose
  hashes chain to `head`, so a commit that passes both checks was computed from the authoritative state.
- **Discard.** On a version conflict, a `prev_hash` mismatch, a fence or an internal error the entry's
  state is dropped and the next attempt cold-loads. A transient error keeps it: nothing committed, or the
  next compare-and-set finds out.
- **Eviction.** After a record that makes the round final (policy `Final`) the entry is dropped. `Forget`
  drops any entry. The cold load handles a later command for an evicted round.

**Rationale.**
- Replaying every record per command is O(n²) per round: a 10,000-bet round would decode about 5·10⁷
  records.
- ADR-001 state replay (initial state plus ordered events through `Evolve`) is exactly the cold load, and
  checking snapshot hashes on the way detects a divergence between stored history and engine state.
- The cache never needs a separate version check: the compare-and-set and the hash link make a stale
  cache fail to commit.

**Alternatives.**
- Full replay before every command: simple and stateless, O(n²).
- Load from snapshot `state` bytes: needs `DecodeState` in `game.Codec` (a contract change), and P-20 left
  the column NULL until LBT-14 decides. A natural LBT-14 optimization once recovery time matters.
- A mutable "current state" table updated in B: a second copy of the truth that could drift, and
  secret-bearing.
- Decision replay (`StateAndDecision`) in the cold load: stronger, about ten times slower; auditing is the
  verifier's job (`crashctl audit verify`).

### E-4. Reading the next admitted command

**Proposal.**
- `k = processed + 1`. Before Transaction B, one statement, no lock (LBT-10 §6 item 2: admissions are
  immutable):

  ```sql
  SELECT a.command_id, a.effective_time_unix_micro, a.command, h.account_id, h.amount, acc.player_id
  FROM orvex.admissions a
  LEFT JOIN orvex.ledger_holds h ON h.round_id = a.round_id AND h.sequence = a.sequence
  LEFT JOIN orvex.ledger_accounts acc ON acc.account_id = h.account_id
  WHERE a.round_id = $1 AND a.sequence = $2
  ORDER BY h.account_id
  ```

  - No row: nothing is pending; `ExecuteNext` returns `Outcome{Executed: false}`.
  - Otherwise one row per hold, or one row with NULL hold columns.
  - LBT-11 inserts holds in the admission's own transaction, so a visible admission comes with all its
    holds.
- The admission is kept for the next attempt while `k` is unchanged.
- **Payout accounts.** A `BetCashedOut` credits the player's account. The executor takes it from
  `playerAccounts`, filled when that player's `BetPlaced` executed. On a miss (after a cold load) it runs
  `SELECT account_id FROM orvex.ledger_accounts WHERE kind = 'player' AND player_id = $1`. No row is
  `ErrCorrupt`: the bet was placed against that account's hold.
- Wake-up is not designed here (LBT-11 §7 leaves it to LBT-12; this spec leaves it to LBT-14's engine
  loop). `ExecuteNext` is cheap when nothing is pending: one primary-key read.

**Rationale.**
- Reading before B keeps B short and lets the transition be computed with no transaction open.
- The join gives the hold and the account's player in the same snapshot, which E-10 verifies.

**Alternatives.**
- Read inside B after the compare-and-set: B stays open while `Decide` runs, and the head row stays
  locked meanwhile.
- Prefetch several admissions per read in `Drain`: saves a round trip per command on a hot round; an
  LBT-15 optimization.
- Wake-up through `LISTEN`/`NOTIFY`: LBT-11 §7 notes it serializes all notifying commits.

### E-5. Computing the transition and the chain record

**Proposal.** A pure function of `(entry, admission, adapter, policy, engine build)`, run outside any
transaction (§5.4):
1. `DecodeCommand(admission.command)`.
2. `Decide(state, command, game.Input{Sequence: k, EffectiveTimeUnixMicro: admission time})`.
   - A rejection (`RejectionCode` ok): result `rejected` with the code; no events; next state = state.
   - A fail-closed error: E-14.
   - Accepted: fold every event through `Evolve`. An `Evolve` error is fail-closed (ADR-003 "validated
     event applicability").
3. Encode each event (`EncodeEvent`); event `j` gets sequence `nextEventSequence + j`; `event_type` is the
   `type` member of the canonical event object (`canonjson.Unmarshal`; LBT-10 §2 item 12).
4. Record: `chain.EncodeCommand(chain.Command{RoundID, Sequence: k, PrevHash: head, CommandID,
   EffectiveTimeUnixMicro, EngineBuild, Payload: admission.command, Result, Events})`; `hash =
   chain.LinkHash(record)`. A record longer than 64 MiB (`MaxRecordBytes`, A-11, the DB `CHECK`) is
   fail-closed.
5. Postings: the policy's ledger rule (E-9) on the decoded events, validated and checked against the
   holds (E-10).
6. Snapshot: if the policy's `Checkpoint(events)` holds (E-13), `chain.StateHash(game, stateVersion,
   EncodeState(next))`.
7. `Final(next)`.

- The command payload is the admission's exact bytes. `OX008` checks the copy.
- Execution is strictly in admission order: command `k` executes only after `k − 1` committed. This is
  ADR-004's and ADR-005's settlement barrier: `SettleRound` at sequence `s` executes after `1..s−1`.

**Rationale.**
- Every value in the record comes from the admission, the previous record or deterministic rules, so a
  recomputation after an unknown outcome yields byte-identical bytes, which E-7's adoption relies on.
- Events carry the authoritative numeric outputs (ADR-004); postings use them and never recompute
  payouts.

**Alternatives.**
- Re-encode the command from the decoded value: equal for canonical bytes, but copying the admitted
  bytes is what LBT-9 §11 and `OX008` ask for.

### E-6. Shape of Transaction B

**Proposal.** §5.6 is normative.
1. `BeginTx` with `postgres.ExecutionTxOptionsWithTimeouts(cfg.LockTimeout, cfg.IdleInTransactionTimeout)`:
   READ COMMITTED, read-write, `BeginQuery` exactly as prototype 1, with the two durations in whole
   milliseconds.
2. The compare-and-set alone:
   `UPDATE orvex.round_execution_heads SET processed_sequence = $3 WHERE round_id = $1 AND
   processed_sequence = $2 AND fencing_token = $4` (`$2 = k − 1`, `$3 = k`). Not exactly one row: roll
   back and classify (E-7).
3. One `pgx.Batch`, in this order, omitting empty statements:
   1. `chain_records` (record, hash, `prev_hash` = cached head, result columns, `fencing_token` = lease
      token);
   2. `domain_events`, all rows in one `INSERT … SELECT … FROM unnest(…)`;
   3. `ledger_hold_releases` for every hold of the admission, ascending `account_id`;
   4. `ledger_transactions`, when there are postings;
   5. `ledger_postings`, all rows in one statement, ascending `posting_index`;
   6. `outbox_messages`, one row (E-12);
   7. `round_snapshots`, when the record is a checkpoint (E-13).
   Read every result in order. The first error rolls back and is classified.
4. `COMMIT`. The deferred checks run: `OX004` fencing, `OX009` hold releases, the head foreign key.

- Lock order (LBT-10 §6 items 2 and 3): the round's execution head first, then account rows through
  releases and postings. Each B touches at most one shared account row (one player, or `house` at
  settlement) plus its own round's `round_pool`, so B transactions cannot deadlock with each other or with
  A, which locks one admission head and at most one player row.
- `lock_timeout` bounds B's waits on account rows held by other transactions.
  `idle_in_transaction_session_timeout` releases the head and account locks within a second if the
  engine vanishes mid-transaction, so LBT-14 can take over and the player's admissions in other rounds
  are not blocked until TCP keepalive notices. `synchronous_commit` is forced to `on` only when it is
  `off`, because the outbox relay may read a commit before an asynchronous flush, and a crash would then
  lose a record that was already published.
- Round trips: the admission read, `BEGIN`, the compare-and-set, the batch, `COMMIT`.

**Rationale.**
- LBT-10 §6 item 6: "B commits only when its first statement, the compare-and-set update, affected
  exactly one row". Sending it alone means nothing is written, and no account row is locked, unless it
  succeeded.
- The batch keeps the insert phase at one round trip. The execution head is single-writer, so its lock
  hold time does not limit other rounds.
- The release-before-post order is required (prototype 2).

**Alternatives.**
- Pipeline the compare-and-set with the inserts in one batch, committing only if it reported one row:
  saves a round trip, but after a failed compare-and-set the inserts still run (and may lock a player
  row) before the rollback.
- A PL/pgSQL `execute_command(…)` function with array arguments: one round trip, but the posting and
  outbox logic moves into SQL. LBT-15 may measure it.
- No session settings (literal P-23): a vanished engine blocks the round and the player's account rows
  until TCP keepalive.
- `SET LOCAL` statements after `BEGIN`: one more round trip.

### E-7. Optimistic checks, fencing and conflicts

**Proposal.**
- **Checks.** Three, all in Transaction B:
  1. the compare-and-set: `processed_sequence = k − 1` (the expected version and next processed sequence;
     LBT-10 §2 item 4) and `fencing_token = t` (the lease);
  2. `chain_records_prev_fkey`: the record extends the stored hash of record `k − 1`, which binds the
     cached state to the stored chain (E-3);
  3. at commit, LBT-10's deferred `OX004` trigger and the head foreign key, as a backstop.
- **Conflict check** (§5.7), after the compare-and-set reported 0 rows, after `23503` on
  `chain_records_prev_fkey`, after commit-time `OX004` or `23503` on `round_execution_heads_head_fkey`, and
  after an ambiguous commit, one statement:

  ```sql
  SELECT h.processed_sequence, h.fencing_token, r.hash
  FROM orvex.round_execution_heads h
  LEFT JOIN orvex.chain_records r ON r.round_id = h.round_id AND r.sequence = $2
  WHERE h.round_id = $1
  ```

  with `$2 = k`. Then, first match wins:

  | Head | Meaning | Action |
  |---|---|---|
  | no row | the head is gone | `ErrCorrupt`, halt |
  | `fencing_token ≠ t` | ownership moved (P-19) | `ErrFenced`; drop the entry; the caller must stop driving the round |
  | `processed ≥ k` and record `k`'s hash = the computed hash | this transition is already committed (an earlier attempt, or another executor with the same token and build) | **adopt**: advance the entry as if committed; `Outcome.Adopted = true` |
  | `processed = k − 1` | nothing committed | retry with the same transition (backoff) |
  | otherwise | another transition is committed, or the cache is ahead of the database | drop the entry's state; retry (reload) |

- B never checks `lease_expires_at` (§2 item 13). Takeovers increment the token (LBT-10 trigger), so either
  the takeover waits on the head row until B commits, or B's compare-and-set finds the new token.

**Rationale.**
- ADR-005 "stale owner cannot commit" holds twice: in the compare-and-set and in the deferred trigger.
- Adopting by hash is exact: identical bytes mean an identical transition (E-5). It implements ADR-005
  "after execution commit but before response: return existing durable result" for the executor itself.
- An ambiguous commit that is still in flight is handled too: the conflict check may still see `k − 1`,
  and the retry's compare-and-set then waits on the head row, finds `k` committed and adopts.

**Alternatives.**
- Also check `lease_expires_at > now()`: ties B to the database clock and protects nothing that the token
  does not.
- Check `owner_id` in the compare-and-set: redundant, every owner change raises the token.
- Treat every conflict as fatal: an ambiguous commit would then need operator action.

### E-8. Ledger accounts

**Proposal.**
- **Player accounts**: the existing `kind = 'player'` rows, one per player ID, floor enforced, created and
  funded by operations (LBT-11 S-11: admission never creates accounts). LBT-12 creates none.
- **Round account**: per round one system account `round_pool` with `round_id` set and
  `floor_enforced = false`, created by round creation (E-16). It receives stakes and pays cash-outs during
  the round, may go negative, and ends at exactly 0 after settlement.
- **House account**: one global system account `house` (`round_id` NULL, `floor_enforced = false`),
  created by the first round creation (`ON CONFLICT (account_id) DO NOTHING`, prototype 5). It absorbs
  each round's net result once, at settlement.
- **Deterministic IDs** (`internal/ledger`; literals in section 1):
  - `HouseAccountID() = uuid8("orvex/ledger/v1/account:global/house")`;
  - `RoundAccountID(r) = uuid8("orvex/ledger/v1/account:" + uuidText(r) + "/round_pool")`;
  - `TransactionID(r, k) = uuid8("orvex/ledger/v1/transaction:" + uuidText(r) + "/" + decimal(k))`;
  - `uuid8` is RFC 9562 UUIDv8: the first 16 bytes of SHA-256, version nibble 8, variant `10`.
- The names `house` and `round_pool` are reserved for the executor.

**Rationale.**
- P-21 "hot rows": one global account per posting would serialize every bet and cash-out of every round.
  With a round account, the global row is written once per round.
- Deterministic IDs make postings a pure function of the chain (LBT-9 §11), and an executor never needs a
  lookup for system accounts.

**Alternatives.**
- One global house account for every posting: P-21 warns against it.
- Sharded house accounts (`house_00` … `house_15` by round): only if LBT-15 measures contention on
  settlement.
- Random account and transaction IDs: postings could not be recomputed exactly.
- Two round accounts (stakes, payouts): more rows, same information.

### E-9. crash/v1 posting rules

**Proposal.** `ledger.CrashV1()` returns the rule. For one executed record with decoded events
`e_0 … e_{n−1}` at event sequences `first … first+n−1`:

| Event | Posting (debit → credit, amount) | Notes |
|---|---|---|
| `BetPlaced{P, b}` | player `P` → round, `b` | `FromHold`: consumes the admission's hold (E-10) |
| `BetCashedOut{P, …, payout}` | round → player `P`, `payout` | `payout ≥ 1` (§1) |
| `RoundSettled{TotalBetCredits: B, TotalPayoutCredits: W}` | `B > W`: round → house, `B − W`; `B < W`: house → round, `W − B`; `B = W`: none | leaves the round account at 0 |
| `RoundStarted`, `RoundCrashed`, `BetLost` | none | |

- Each posting carries `event_sequence` of its event and `round_id` of its record.
- A record with at least one posting gets exactly one ledger transaction: `transaction_id =
  TransactionID(round, k)`, `round_id`, `sequence = k`, and `kind`:
  - `crash_bet` for a `BetPlaced`;
  - `crash_cash_out` for a `BetCashedOut`;
  - `crash_settlement` for a `RoundSettled` posting.
- `posting_index` is the posting's position in the rule's output, from 0.
- A record without postings has no ledger transaction (rejections, lifecycle records, an even
  settlement). Its hold releases need none (they reference the chain record).
- The executor validates the rule's output before B: amounts > 0, debit ≠ credit after resolution, every
  event sequence inside the record's range, at most one posting per event, `FromHold` only on a player
  debit. A violation is fail-closed.

**Rationale.**
- Double entry by construction (P-21): every posting moves one amount between two accounts, so
  Σ debits = Σ credits over all accounts (ADR-004 "balanced").
- A bet settles at most once (ADR-001): `Decide` emits at most one `BetCashedOut` per bet and one
  settlement per round; each event has at most one posting (E-11); each record at most one ledger
  transaction (P-21).
- `BetLost` needs no money movement: the stake moved at `BetPlaced`. The net settlement posting is one row
  instead of up to 10,000 (§2 item 9).
- The worked example (§8) checks the rule end to end: round account 0, house −757,599,784, players'
  gains summing to +757,599,784.

**Alternatives.**
- One posting per `BetLost` (round → house) plus a funding posting: up to 10,000 updates of the same two
  account rows in one transaction, with growing version chains.
- Settle each cash-out against the house immediately: the global row in every cash-out transaction.
- Posting rules recorded in the chain (chain v2 posting digest): LBT-9 §11 leaves it optional; recomputation
  from events gives the same assurance without a chain change.

### E-10. Holds and cross-round balance protection

**Proposal.**
- **Release always.** In the transaction that writes record `k`, release every `ledger_holds` row of
  admission `k`, whatever the result (LBT-11 §7). Releases come before any posting (prototype 2).
- **Consume on `BetPlaced`.** A `FromHold` posting requires, before B:
  - exactly one hold row for the admission;
  - its account is a player account whose `player_id` equals the event's player;
  - its amount equals the posting's amount (LBT-11 S-10: hold = `BetCredits`).
  Anything else is `ErrCorrupt` and halts the round. The account ID is memoized in `playerAccounts`.
- **Rejected or non-betting commands** with a hold (a `PlaceBet` rejected as `duplicate_bet`,
  `invalid_amount`, `bet_limit_reached`, `wrong_phase`, `round_settled`, …) get the release only, which
  frees the reserved credits for other rounds.
- **Why cross-round protection holds** (ADR-005):
  - Transaction A reserves credits on the player's account row under its lock: `held += b`, refused when
    `debits_posted + held > credits_posted` (LBT-11 S-11, the floor `CHECK`).
  - Transaction B, for a `BetPlaced`, runs `held −= b` and then `debits_posted += b` on the same row,
    locked by its first update until commit. The player's commitment `debits_posted + held` is unchanged,
    and no other transaction can take the freed credits in between.
  - Every other effect of B on a player raises `credits_posted` (a payout) or lowers `held` (a release).
    B never raises `debits_posted + held`, so B cannot overspend, and a floor violation in B is a bug
    (§2 item 6).
  - All updates are relative and evaluated on the newest row version under READ COMMITTED (P-23), so A
    and B in different rounds serialize on the account row, never on a round.
- **Database backstops**: the floor `CHECK`; `ledger_hold_releases_pkey` (one release per hold);
  `ledger_hold_releases_record_fkey` (only execution releases); `OX009` (no executed admission keeps a
  hold, E-11).

**Rationale.** P-21 and LBT-11 S-11 put the reservation at admission, because `Decide` is pure and cannot
see balances. B converts or frees the reservation atomically with the record that justifies it.

**Alternatives.**
- Post first, release second: fails the floor check at an exact balance (prototype 2).
- Keep holds of rejected bets until settlement: locks credits for the rest of the round with no reason.
- Debit at admission (no holds): a bet that `Decide` rejects would need a refund posting without an event.

### E-11. Migration `0008_execution_ledger`

**Proposal.** §6 is normative.
- Replace `ledger_postings_event_idx` with the unique partial index `ledger_postings_event_key ON
  orvex.ledger_postings (round_id, event_sequence) WHERE event_sequence IS NOT NULL`: at most one posting
  per chained event (prototype 4).
- Add function `orvex.check_record_holds_released()` and the deferred constraint trigger
  `chain_records_holds_released AFTER INSERT ON orvex.chain_records`: at commit, a command record whose
  admission still has a hold without a release raises `OX009` (prototype 3).
- `OX009` is `postgres.CodeHoldUnreleased`.
- The down drops the trigger and function, drops the unique index and recreates
  `ledger_postings_event_idx`. It drops no table, so it does not refuse with `OX003` (P-5).

**Rationale.**
- "Duplicate payout impossible" then holds in the database, not only in the executor: one record per
  sequence, one ledger transaction per record, one posting per event.
- A forgotten release would lock a player's credits forever; the trigger makes it impossible to commit.
- Both checks cost one index probe per row or per record.

**Alternatives.**
- Executor and tests only: correct while the executor has no bug.
- A trigger binding each posting's event to its transaction's record: stronger, more SQL; not needed for
  the acceptance.
- A unique index next to the old non-unique one: duplicate index, double write cost.

### E-12. Outbox

**Proposal.**
- One message per chain record, written in the record's transaction: genesis at round creation (E-16),
  each command record in B.
- `(round_id, sequence, message_index = 0)`, the LBT-10 dedupe key; `topic = 'round.record.v1'`
  (`execution.OutboxTopicRecord`); `payload` = the exact record bytes, as hashed.
- `v1` names the payload format: one `orvex/audit/v1` chain record. A future chain version gets a new
  topic. `message_index` values above 0 are reserved for future per-record messages.
- No secret in any payload: the genesis carries the commitment, commands carry no secret, and events
  reveal the outcome and seed only when due (§1). `round_secrets`, `round_snapshots.state` and the state
  never enter the outbox.
- Delivery is at least once (ADR-005). Consumers deduplicate on the key; per-round order is `sequence`
  (P-22: never `message_id`); a consumer can check each record's `prevHash` against the previous payload's
  link hash and detect a gap.
- Records are deterministic, so a transition recomputed after an unknown outcome carries the same
  payload under the same key, and every redelivery of a key is byte-identical.

**Rationale.**
- The chain record is already canonical, public at commit and self-verifying. It holds the command, its
  ID, effective time, result and events: what LBT-13 needs for the submitter's result and for
  spectators.
- No new format is invented in LBT-12, and the relay needs no join and no `SELECT` on `chain_records`.

**Alternatives.**
- A reference-only payload (`round_id`, `sequence`): no duplicated bytes (a 1.5 MB settlement record is
  stored twice until the relay deletes the published row), but every relay read joins `chain_records`.
- Separate result and event messages (`message_index` 0 and 1): transport concerns decided before LBT-13.
- The public projection: has no canonical encoding today, and it is a function of the records anyway.

### E-13. Snapshots

**Proposal.**
- Sequence 0 in the round-creation transaction (P-20): `chain.StateHash` of `InitialState`.
- In Transaction B, a snapshot row for record `k` when the policy's `Checkpoint(events)` holds. For
  crash/v1: the record emitted `RoundStarted`, `RoundCrashed` or `RoundSettled`. The settlement record is
  therefore always a checkpoint (P-20).
- `state_hash` from the engine's state after the record (A-10; never recomputed at export, LBT-9 §11);
  `state` NULL (§2 item 4); `engine_build = Config.EngineBuild`.

**Rationale.**
- `ExportHistory` ends at the newest snapshot (P-24). Phase checkpoints let an operator export a round at
  the end of betting and at the crash, before settlement.
- Three state encodings per round; no per-command O(state) work (A-10).

**Alternatives.**
- Every N records: bounded export lag on long rounds, and repeated full-state encodings of big rounds;
  can be added to the policy later.
- Every record: O(n²) bytes encoded per round.
- Only 0 and settlement: an unsettled round cannot be exported past genesis.

### E-14. Fail-closed `Decide` and other deterministic failures

**Proposal.**
- Fail-closed cases, all computed before B: `Decide` returns an error that `RejectionCode` does not
  recognise; `DecodeCommand`, `Evolve`, `EncodeEvent`, `chain.EncodeCommand`, `EncodeState` or the
  ledger rule fails; the rule's output is invalid; the record exceeds 64 MiB.
- Handling:
  - no transaction is started; no record, no result, no events, no postings, no outbox row (A-7);
  - `ExecuteNext` returns `*FailClosedError{RoundID, Sequence, Err}`; `errors.Is(err, ErrFailClosed)`
    holds and the cause stays reachable;
  - the entry is halted at that sequence: later calls return the same error without recomputing, until
    `Forget`;
  - admissions after it stay unexecuted (strict order), so the round cannot settle; the admission's hold
    stays held;
  - no durable fault row. The signal is the error to the caller (LBT-14 logs and alerts; LBT-15 adds
    metrics) and a stalled `processed_sequence < admitted_sequence`.
- `ErrCorrupt` (stored data contradicts an invariant: replay or snapshot mismatch, missing or mismatched
  hold, missing player account, missing head) halts the same way.
- Error text never contains state, seeds or deadlines. crash/v1 errors never format secrets (lifecycle
  spec; `cashOutAmounts`).

**Rationale.**
- A-7 and A-15: a fail-closed command has no record, and the verifier would reject a fabricated one.
- `Decide` is deterministic: retrying cannot succeed and only burns CPU.
- crash/v1 cannot reach these paths from a valid state (§1), so a durable fault table would serve an
  event that should never happen.

**Alternatives.**
- Record a synthetic rejection such as `internal_error`: decision replay fails on it (A-15).
- Skip the command and continue: breaks the gapless chain (A-7) and ADR-005's barrier.
- An append-only `execution_faults(round_id, sequence, at, engine_build, error)` table: durable evidence
  for operators; LBT-14 can add it with its runbook.

### E-15. Errors, SQLSTATEs and retries

**Proposal.**

| Where | Condition | Handling |
|---|---|---|
| before the DB | invalid lease or `limit < 1` | `ErrInvalidRequest`; no DB call |
| cold load | no `rounds` row | `ErrRoundNotFound` |
| cold load | head token ≠ lease token | `ErrFenced` |
| cold load | no policy or adapter, or descriptor ≠ `rounds` row (A-14) | `ErrUnknownRules`; halt |
| cold load | decode, replay or snapshot mismatch; missing secret or head | `ErrCorrupt`; halt |
| transition | E-14 | `*FailClosedError` or `ErrCorrupt`; halt |
| any statement | `ctx` done | an error wrapping `ctx.Err()`; the entry is kept (E-3) |
| any statement before `COMMIT` | not a `*pgconn.PgError` (connection loss), class `08`, `53`, `57` (except `57014` with `ctx` done), `25P03` | retry: nothing committed |
| any statement | `40001`, `40P01` | retry |
| any statement | `55P03` (`lock_timeout`) | `ErrBusy`; no retry |
| compare-and-set | 0 rows | conflict check (E-7) |
| batch | `23503` on `chain_records_prev_fkey` | conflict check |
| batch | any other `23xxx` (including `23514` floor, `23505` on `ledger_postings_event_key` or `ledger_transactions_record_key`), `OXxxx` (including `OX008`), `22xxx`, `42xxx`, `XX000` | `ErrInternal` wrapping the `PgError`; drop the entry; no retry |
| `COMMIT` | nil | success; advance the entry |
| `COMMIT` | `OX004`, or `23503` on `round_execution_heads_head_fkey` | conflict check |
| `COMMIT` | `40001`, `40P01` | retry |
| `COMMIT` | `OX009`, any other class `23` or `OX`, `40002`, `pgx.ErrTxCommitRollback` | definitely not committed: `ErrInternal`; drop the entry |
| `COMMIT` | anything else (non-`PgError`, `40003`, classes `08`, `53`, `57`, `58`, `XX`) | ambiguous: conflict check on the next attempt |

- `Config.MaxAttempts` defaults to 3. Retries and reloads count as attempts; an adoption ends the call
  successfully.
- Before attempt `n ≥ 2` after a transient error, wait a uniformly random duration in
  `[0, min(50 ms, 5 ms · 2^(n−2))]` (full jitter, `math/rand/v2`), interruptible by `ctx`. A reload
  retries without waiting.
- After the last attempt: `ErrUnavailable`, wrapping the last cause. Calling `ExecuteNext` again later is
  always safe: the next attempt's compare-and-set or conflict check resolves any unknown outcome.
- Every returned error wraps exactly one sentinel (`*FailClosedError` wraps `ErrFailClosed` and its
  cause). `PgError` details stay reachable through `errors.As`.
- `CreateRound` uses the same classes, with `23505` on `rounds_pkey` resolved per E-16.

**Rationale.**
- Same classes and backoff as LBT-11 S-12 and LBT-10 §6 item 4, so the two transactions behave alike
  under contention (ADR-006 "retry storms").
- B is idempotent by construction (sequence-keyed record, deterministic bytes), so retrying any
  non-definite outcome is safe.

**Alternatives.**
- Unbounded retries inside the call: hides outages from the caller and amplifies storms.
- Retry `55P03`: the wait already lasted `LockTimeout`.
- Treat ambiguous commits as failures: would make the caller handle what the conflict check resolves.

### E-16. Round creation

**Proposal.** `Executor.CreateRound(ctx, NewRound{RoundID, Game, RulesVersion, Config, CreatedAtUnixMicro})`:
1. Validate before the DB: non-nil round ID; a policy and adapter for `(Game, RulesVersion)`;
   `EncodeConfig(Config)`; `DecodeConfig(RoundID, public, secret)`; `InitialState` of both the given and
   the decoded config, whose state hashes must be equal (this catches a config whose own round ID differs
   from `RoundID`). Failures give `ErrInvalidRound` or `ErrUnknownRules`.
2. Build the genesis record (`chain.EncodeGenesis`: versions from the descriptor, public config,
   `CreatedAtUnixMicro`, `Config.EngineBuild`), its hash and the state hash of the initial state.
3. One transaction with `ExecutionTxOptionsWithTimeouts`, inserts only:
   `rounds`; `round_secrets` (the secret config bytes, P-25); `round_admission_heads` (defaults);
   `round_execution_heads` (defaults: sequence 0, token 0, no owner); genesis `chain_records` row (token 0);
   `round_snapshots` sequence 0; `ledger_accounts` `round_pool` (insert) and `house` (`ON CONFLICT
   (account_id) DO NOTHING`); `outbox_messages` `(round, 0, 0, 'round.record.v1', genesis)`. Commit.
4. `23505` on `rounds_pkey`: read the stored genesis record; equal bytes return `Created{Duplicate:
   true}`, otherwise `ErrRoundExists`. Ambiguous commits retry, and the retry resolves through the same
   path.
5. On success the executor seeds the round's entry with the initial state at sequence 0.

- `CreatedAtUnixMicro` is the scheduler's clock (it is the commitment publication time, A-8, and not a
  `Decide` input). The seed comes in `Config`: the caller draws it with `crypto/rand` (D-10). The
  scheduler that does both is LBT-14's.

**Rationale.**
- LBT-10 §9 and LBT-11 §7 propose LBT-12. Creation writes the same objects as B (genesis record, snapshot,
  outbox, the round account), so it belongs with the code that encodes them.
- Deterministic genesis bytes make creation idempotent across unknown outcomes.

**Alternatives.**
- A separate issue for round creation: tests would keep a parallel helper, and P-20's snapshot-0
  obligation would have no production writer.
- Create `round_pool` lazily in B: an extra statement in every posting transaction.
- Read `CreatedAtUnixMicro` from the database clock inside creation: golden genesis bytes could not be
  reproduced.

### E-17. Test strategy and failure injection

**Proposal.**
- **Real PostgreSQL.** Integration tests run through `pgtest` (P-7) in `*_integration_test.go` files with
  `//go:build integration` (P-8), one container per package (`pgtest.Run`), one migrated database per
  test (`pgtest.MigratedDatabase`). `make test-integration` runs them locally and in CI's `integration`
  job.
- **Byte-exact goldens.** The four LBT-9 golden histories are reproduced byte for byte: as a pure unit test
  through the transition function, and end to end through `CreateRound`, seeded admissions and `Drain`.
- **Admissions.** Most tests seed admissions directly (exact effective times and command IDs), with holds
  exactly as LBT-11 S-10 and S-11 place them. Cross-round and end-to-end tests use LBT-11's
  `admission.Sequencer`.
- **Failure injection, no production seams:**
  1. `faultDB` wraps the pool. Its transactions run hooks before the compare-and-set, before the batch,
     before `COMMIT` and after the real `COMMIT`. Hooks can return an injected error instead of the real
     call, block on a channel, terminate the backend, or run the real commit and then return
     `io.ErrUnexpectedEOF`.
  2. Test-only SQL in schema `orvex_test`, created per test database:
     - a `BEFORE INSERT` trigger on a chosen table that raises a chosen SQLSTATE (optionally with a
       constraint name) while `nextval('orvex_test.fault_budget') ≤ N`; sequences are not rolled back, so
       "fail once" survives the rollback;
     - a `DEFERRABLE INITIALLY DEFERRED` constraint trigger that raises at `COMMIT`.
  3. `pg_terminate_backend` of the executor's connection, from the test's admin connection, to model a
     process crash.
  4. A concurrent session that performs LBT-14's takeover (`UPDATE orvex.round_execution_heads SET
     fencing_token = fencing_token + 1, owner_id = 'engine-b', lease_expires_at = now() + interval '30
     seconds' WHERE round_id = $1`), synchronized with `pgtest.WaitUntilBlocked`.
- **"No partial effects"** is one helper: `effects(roundID)` captures the head, the row counts of every
  table B writes for the round (records, events, transactions, postings, releases, outbox, snapshots)
  and the totals of every account involved. A failed attempt must leave it unchanged; a successful
  retry must change it exactly as one execution does.
- **Determinism.** Concurrency tests assert only interleaving-independent invariants (sets, counts,
  totals, chain validity), start goroutines on a closed channel, detect blocking through
  `pg_stat_activity`, never sleep to wait for a lock, and do not call `t.Parallel()`.

**Alternatives.**
- Failpoint hooks in production code (`Config.hooks`): a test seam in the commit path; LBT-15 owns
  production failpoints.
- Mocks of pgx for commit tests: would not exercise the real constraints and deferred triggers.
- Testing only through the sequencer: exact effective times and golden bytes would be out of reach.

## 5. Normative execution algorithm

### 5.1 `ExecuteNext(ctx, lease)`

1. Validate the lease (non-nil `RoundID`, `FencingToken ≥ 0`); otherwise `ErrInvalidRequest`.
2. Lock the round's entry (create it if absent). A halted entry returns its stored error.
3. For `attempt = 1 … MaxAttempts`:
   1. No state in the entry: cold load (§5.2).
   2. `k = processed + 1`. Read admission `k` (§5.3) unless held from an earlier attempt. None:
      return `Outcome{Executed: false}`.
   3. Compute the transition (§5.4). Fail-closed or corrupt: halt and return.
   4. Resolve accounts (§5.5). Corrupt: halt and return.
   5. Run Transaction B (§5.6) and classify (§5.7, E-15):
      - committed or adopted: advance the entry (`state`, `processed = k`, `head = hash`,
        `nextEventSequence += n`, memoized accounts); if `Final`, drop the entry; return the `Outcome`;
      - fenced: drop the entry; return `ErrFenced`;
      - reload: drop the entry's state; continue;
      - retry: back off; continue;
      - ambiguous commit: back off; the next attempt starts with the conflict check (E-7) and adopts,
        retries or reloads per its table;
      - busy, internal, `ctx`: return the error (internal drops the entry).
4. Attempts exhausted: `ErrUnavailable`, wrapping the last cause.

### 5.2 Cold load (one `SnapshotReadTxOptions` transaction, always rolled back)

1. `SELECT r.game, r.rules_version, r.schema_version, r.state_version, s.config_secret,
   h.processed_sequence, h.fencing_token FROM orvex.rounds r LEFT JOIN orvex.round_secrets s USING
   (round_id) LEFT JOIN orvex.round_execution_heads h USING (round_id) WHERE r.round_id = $1`.
   - No row: `ErrRoundNotFound`. NULL secret or head: `ErrCorrupt`.
   - `fencing_token ≠ lease.FencingToken`: `ErrFenced`.
2. Registry adapter and policy for `(game, rules_version)`; the descriptor's schema and state versions
   must equal the row's; otherwise `ErrUnknownRules`.
3. `SELECT sequence, hash, record FROM orvex.chain_records WHERE round_id = $1 AND sequence <= $2 ORDER BY
   sequence` with `$2 = processed`. Row `i` has `sequence = i`; the count is `processed + 1`.
4. `SELECT sequence, state_hash FROM orvex.round_snapshots WHERE round_id = $1 AND sequence <= $2 ORDER BY
   sequence`.
5. Record 0: `chain.Decode` gives a `chain.Genesis` whose round ID, game and versions equal the row;
   `DecodeConfig(roundID, genesis.Config, secret)`; `InitialState`.
6. Records `1..processed`: `chain.Decode` gives a `chain.Command` with `Sequence = i` and the round ID;
   event sequences are contiguous from 1 across the round; events decode with `DecodeEvent`. Build
   `replay.Record`s and run `replay.Run(adapter, initial, records, replay.StateOnly, visit)`; `visit`
   compares `chain.StateHash(game, stateVersion, EncodeState(state))` with every snapshot at that
   sequence.
7. The entry gets `state`, `processed`, `head` = stored hash of record `processed`, and
   `nextEventSequence = 1 + number of events`.
8. Any mismatch in steps 3 to 6: `ErrCorrupt`, halt.

### 5.3 Admission read

The E-4 statement. Rows give: command ID, effective time, command bytes, and zero or more holds
`{account_id, amount, player_id}` (`player_id` NULL for a non-player account).

### 5.4 Transition (pure)

E-5 steps 1 to 7. Output: `record`, `hash`, `result`, `events` (typed), `eventBytes`, `eventTypes`,
`next`, `kind` and `postings` (with `ledger.Account` references), `releases` (the holds' account IDs,
ascending), `checkpoint` and its `stateHash`, `final`.

### 5.5 Account resolution

- `AccountRound` → `ledger.RoundAccountID(round)`; `AccountHouse` → `ledger.HouseAccountID()`.
- `AccountPlayer` with `FromHold` → the hold's account (E-10 checks).
- `AccountPlayer` otherwise → `playerAccounts`, else the E-4 lookup.
- After resolution, debit ≠ credit for every posting.

### 5.6 Transaction B statements

```sql
-- after BeginTx(ExecutionTxOptionsWithTimeouts(lock, idle))
UPDATE orvex.round_execution_heads SET processed_sequence = $3
 WHERE round_id = $1 AND processed_sequence = $2 AND fencing_token = $4;          -- must affect 1 row
-- batch:
INSERT INTO orvex.chain_records (round_id, sequence, prev_hash, hash, record, result_status, result_reason, fencing_token)
VALUES ($1, $k, $prev, $hash, $record, $status, $reason, $token);
INSERT INTO orvex.domain_events (round_id, event_sequence, sequence, event_type)
SELECT $1, s, $k, t FROM unnest($seqs::bigint[], $types::text[]) AS u(s, t);
INSERT INTO orvex.ledger_hold_releases (round_id, sequence, account_id)
SELECT $1, $k, a FROM unnest($accounts::uuid[]) AS u(a) ORDER BY a;
INSERT INTO orvex.ledger_transactions (transaction_id, kind, round_id, sequence) VALUES ($tx, $kind, $1, $k);
INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount, round_id, event_sequence)
SELECT $tx, i, d, c, a, $1, e FROM unnest($idx::int[], $deb::uuid[], $cred::uuid[], $amt::bigint[], $ev::bigint[]) AS u(i, d, c, a, e) ORDER BY i;
INSERT INTO orvex.outbox_messages (round_id, sequence, message_index, topic, payload) VALUES ($1, $k, 0, 'round.record.v1', $record);
INSERT INTO orvex.round_snapshots (round_id, sequence, state_hash, state, engine_build) VALUES ($1, $k, $stateHash, NULL, $build);
-- COMMIT
```

The SQL text is illustrative; the order, the columns written and the single-statement bulk inserts are
normative.

### 5.7 Classification

E-7's conflict check and E-15's table. The conflict check runs on the pool (a fresh connection), never on
the failed transaction.

## 6. Normative database changes (migration `0008_execution_ledger`)

First line of both files: `-- 0008 execution_ledger: <purpose> (LBT-12 spec E-10, E-11)`. LBT-10 P-11
conventions: schema-qualified names, explicit names, `LANGUAGE plpgsql` with `SET search_path =
pg_catalog`, and a `COMMENT ON` for the new function and index naming ADR-005.

**Up:**
1. `DROP INDEX orvex.ledger_postings_event_idx;`
2. `CREATE UNIQUE INDEX ledger_postings_event_key ON orvex.ledger_postings (round_id, event_sequence)
   WHERE event_sequence IS NOT NULL;`
3. `orvex.check_record_holds_released() RETURNS trigger`: if `NEW.sequence >= 1` and some
   `orvex.ledger_holds` row with `(round_id, sequence) = (NEW.round_id, NEW.sequence)` has no
   `orvex.ledger_hold_releases` row with the same `(round_id, sequence, account_id)`, raise
   `orvex.chain_records: record % of round % leaves a credit hold unreleased` with `ERRCODE = 'OX009'`;
   return NULL.
4. `CREATE CONSTRAINT TRIGGER chain_records_holds_released AFTER INSERT ON orvex.chain_records DEFERRABLE
   INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION orvex.check_record_holds_released();`

**Down:** drop the trigger and the function; `DROP INDEX orvex.ledger_postings_event_key`; recreate
`ledger_postings_event_idx` exactly as `0005` created it. After the down the catalog fingerprint equals
the one at the previous version (`TestMigrateEachStepReversible`).

At commit the deferred triggers on `chain_records` fire in name order: `chain_records_fenced` (`OX004`)
before `chain_records_holds_released` (`OX009`).

## 7. Go API (normative)

Names may be refined during implementation; behavior may not.

```go
package ledger

type AccountKind uint8

const (
	AccountPlayer AccountKind = iota + 1 // the player's account, found by player ID
	AccountRound                          // the round's "round_pool" system account
	AccountHouse                          // the global "house" system account
)

// Account references a ledger account; Player is set iff Kind == AccountPlayer.
type Account struct {
	Kind   AccountKind
	Player string
}

type Posting struct {
	Debit, Credit Account
	Amount        int64  // > 0
	EventSequence uint64 // the chained event it settles
	FromHold      bool   // the debit consumes the admission's credit hold
}

// Rule is the pure posting rule of one rules version.
type Rule interface {
	Descriptor() game.Descriptor
	// Postings returns the ledger transaction kind and the postings of one executed command record.
	// events[j] has event sequence first+j. kind is "" iff postings is empty.
	Postings(events []any, first uint64) (kind string, postings []Posting, err error)
}

const (
	HouseAccountName = "house"
	RoundAccountName = "round_pool"
)

func CrashV1() Rule
func HouseAccountID() [16]byte
func RoundAccountID(roundID [16]byte) [16]byte
func TransactionID(roundID [16]byte, sequence uint64) [16]byte
```

```go
package execution

// DB is what the Executor needs; *pgxpool.Pool implements it.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Config tunes the Executor. New rejects values outside the ranges (ErrInvalidConfig).
type Config struct {
	EngineBuild              string        // 1–128 printable ASCII bytes (A-8)
	LockTimeout              time.Duration // [10ms, 60s]; must exceed IdleInTransactionTimeout
	IdleInTransactionTimeout time.Duration // [50ms, 60s]
	MaxAttempts              int           // [1, 10]
}

// DefaultConfig returns {buildinfo.Version, 3s, 1s, 3}.
func DefaultConfig() Config

// Policy is the execution view of one rules version.
type Policy interface {
	Descriptor() game.Descriptor
	Ledger() ledger.Rule
	// Checkpoint reports whether the state after a record with these events gets a snapshot (E-13).
	Checkpoint(events []any) bool
	// Final reports whether no command can change state any more.
	Final(state any) (bool, error)
}

// CrashV1Policy: Checkpoint on RoundStarted, RoundCrashed or RoundSettled; Final in PhaseSettled.
func CrashV1Policy() Policy

const (
	OutboxTopicRecord = "round.record.v1"
	MaxRecordBytes    = 64 << 20
)

// New requires at least one policy, no two for one (game, rules version), and for each an adapter in
// reg whose Descriptor equals the policy's and its Ledger's.
func New(db DB, reg *game.Registry, cfg Config, policies ...Policy) (*Executor, error)

// Executor is safe for concurrent use; calls for one round serialize.
type Executor struct{ /* unexported */ }

// Lease is the caller's claim to execute one round (LBT-14 grants it).
type Lease struct {
	RoundID      [16]byte // not the nil UUID
	FencingToken int64    // >= 0; 0 is the round-creation token
}

type Outcome struct {
	Executed  bool         // false: no admitted command was pending
	Sequence  uint64
	CommandID [16]byte
	Result    chain.Result
	Events    []any        // decoded events of the rules version; do not mutate
	Record    []byte       // the exact chain record bytes
	Hash      chain.Hash
	Adopted   bool         // committed by an earlier attempt whose outcome was unknown
	Final     bool
}

func (e *Executor) ExecuteNext(ctx context.Context, lease Lease) (Outcome, error)
func (e *Executor) Drain(ctx context.Context, lease Lease, limit int) (executed int, err error)
func (e *Executor) Forget(roundID [16]byte)

type NewRound struct {
	RoundID            [16]byte
	Game               game.ID
	RulesVersion       game.RulesVersion
	Config             any   // the rules version's typed config, including the secret seed
	CreatedAtUnixMicro int64 // genesis createdAtUnixMicro (A-8)
}

type Created struct {
	RoundID   [16]byte
	Genesis   []byte
	Head      chain.Hash
	StateHash chain.Hash
	Duplicate bool // the identical round already existed
}

func (e *Executor) CreateRound(ctx context.Context, r NewRound) (Created, error)

// FailClosedError: the rules, codec or posting rule failed deterministically at Sequence (E-14).
type FailClosedError struct {
	RoundID  [16]byte
	Sequence uint64
	Err      error
}

func (e *FailClosedError) Error() string   // "execution: round <uuid> halted at sequence <k>: <err>"
func (e *FailClosedError) Unwrap() []error // ErrFailClosed, Err

var (
	ErrInvalidConfig  = errors.New("execution: invalid config")
	ErrInvalidRequest = errors.New("execution: invalid request")
	ErrInvalidRound   = errors.New("execution: invalid round")
	ErrUnknownRules   = errors.New("execution: no execution policy for the game rules version")
	ErrRoundNotFound  = errors.New("execution: round not found")
	ErrRoundExists    = errors.New("execution: round exists with a different genesis")
	ErrFenced         = errors.New("execution: fencing token superseded; stop executing this round")
	ErrFailClosed     = errors.New("execution: rules failed closed; round halted")
	ErrCorrupt        = errors.New("execution: stored round data violates an execution invariant")
	ErrBusy           = errors.New("execution: lock wait timed out")
	ErrUnavailable    = errors.New("execution: database unavailable; call again later")
	ErrInternal       = errors.New("execution: internal error")
)
```

```go
package postgres

// CodeHoldUnreleased: at commit, a command record's admission still holds credits (E-11).
const CodeHoldUnreleased = "OX009"

// ExecutionTxOptionsWithTimeouts returns Transaction B's options: READ COMMITTED, read-write, with a
// BeginQuery that also sets lock_timeout and idle_in_transaction_session_timeout transaction-locally (in
// whole milliseconds; both must be >= 1ms) and turns synchronous_commit on when it is off (E-6).
func ExecutionTxOptionsWithTimeouts(lock, idle time.Duration) pgx.TxOptions
```

Error strings are lowercase, without trailing punctuation, prefixed `execution: `, `ledger: ` or
`postgres: `.

## 8. crash/v1 expectations on the golden histories

Players are funded with exactly the sum of their holds, all admissions are seeded before execution, and
the executor uses `EngineBuild = "golden"`.

**worked-example** (round `0190f3a8-7b3c-7d4e-8f00-000000000001`):

| Seq | Command and result | Hold | Ledger effect |
|---|---|---|---|
| 1 | alice bets 100, accepted (event 1) | alice 100 | release; tx `862c9c4b…` `crash_bet`: alice → round 100, event 1 |
| 2 | bob bets 250, accepted (event 2) | bob 250 | release; `crash_bet`: bob → round 250, event 2 |
| 3 | player `P3` (LBT-9 §5.6) bets 1,000,000,000, accepted (event 3) | `P3` 1,000,000,000 | release; `crash_bet`: `P3` → round, event 3 |
| 4 | alice bets 50, `duplicate_bet` | alice 50 | release only |
| 5 | dave bets 0, `invalid_amount` | none | none |
| 6 | empty player, `invalid_command` | none | none |
| 7 | dave bets 10, `bet_limit_reached` | dave 10 | release only |
| 8 | alice cash-out, `wrong_phase` | none | none |
| 9 | `StartRound`, accepted (event 4) | none | none; snapshot |
| 10, 13–16, 18, 20 | rejections | none | none |
| 11 | erin bets 10, `wrong_phase` | erin 10 | release only |
| 12 | alice cash-out 134 (event 5) | none | tx `5687be29…` `crash_cash_out`: round → alice 134, event 5 |
| 17 | `P3` cash-out 1,757,600,000 (event 6) | none | tx `4357c225…` `crash_cash_out`: round → `P3`, event 6 |
| 19 | `CloseRound`, accepted (event 7) | none | none; snapshot |
| 21 | `SettleRound`: `BetLost` bob (event 8), `RoundSettled` totals 1,000,000,350 / 1,757,600,134 (event 9) | none | tx `d61426fa…` `crash_settlement`: house → round 757,599,784, event 9; snapshot |
| 22 | frank bets 10, `round_settled` (executed after a cold load, because 21 made the round final) | frank 10 | release only |

Final totals: alice credits 284, debits 100 (balance 184); bob credits 250, debits 250; `P3` credits
2,757,600,000, debits 1,000,000,000; dave, erin and frank 10 each; every `held` 0; round account credits
= debits = 1,757,600,134 (balance 0); house debits 757,599,784. Snapshots 0, 9, 19, 21; 23 outbox
messages; 9 `domain_events` rows; 6 ledger transactions; 7 releases.

**instant-crash**: alice bets 500 (event 1, `crash_bet`); cash-out 3 rejected `deadline_passed`;
settlement: `BetLost` (event 4), `RoundSettled` 500 / 0 (event 5) → round → house 500 (`crash_settlement`,
event 5). Snapshots 0, 2, 4, 5. **empty-round**: no postings (even settlement); snapshots 0, 1, 2, 3.
**in-progress**: alice bets 1,000 (event 1); cash-out 3,320 round → alice (event 3); snapshots 0, 2.

## 9. What LBT-13 and LBT-14 build on (not designed here)

**LBT-13 (gateway, gRPC protocol, outbox relay).**
- Outbox rows (E-12): topic `round.record.v1`, payload = one `orvex/audit/v1` chain record
  (`chain.Decode`), key `(round_id, sequence, 0)`, genesis at sequence 0, per-round order by `sequence`.
- The relay claims pending rows through `outbox_messages_pending_idx` with `FOR UPDATE SKIP LOCKED`,
  maintains `attempts`, `next_attempt_at`, `last_error` and `published_at` (the only mutable columns,
  `OX006`), never treats `message_id` as an order, and deletes published rows per an operations policy
  (P-26). It needs no access to `round_secrets` or `round_snapshots`.
- Consumers deduplicate on the key and may verify `prevHash` links.
- Execution acknowledgment: the record's result, read from `orvex.command_results` or
  `admission.Sequencer.Lookup` (LBT-11), implements ADR-005 "after execution commit but before response:
  return existing durable result". Admission acknowledgment stays LBT-11's.
- Payloads hold no secret before due, so they can reach clients after mapping to the wire format.

**LBT-14 (ownership leases, failover, recovery).**
- Acquire and take over through a conditional `UPDATE` of `round_execution_heads` raising
  `fencing_token`, setting `owner_id` and `lease_expires_at` (P-19 trigger). Renewals may keep the token.
- Pass `Lease{RoundID, FencingToken}` to `ExecuteNext`/`Drain`. On `ErrFenced`, stop driving the round and
  call `Forget`.
- Recovery is the cold load (E-3): genesis plus chain replay with snapshot checks. Snapshot-based recovery
  needs `DecodeState` and a decision on storing `round_snapshots.state`.
- The engine loop: find rounds with `admitted_sequence > processed_sequence`, wake-up or polling, bounded
  mailboxes, `Drain` with a limit for fairness, graceful SIGTERM (an abandoned B is rolled back within
  `IdleInTransactionTimeout`).
- The scheduler: create rounds (`CreateRound`, seed from `crypto/rand`), admit `StartRound`, `CloseRound`
  (`NotBeforeUnixMicro = T0 + D`, from `RoundStarted` in `Outcome.Events` and the config) and
  `SettleRound` through LBT-11.
- Operations for halted rounds (`*FailClosedError`, `ErrCorrupt`) and their outstanding holds.
- Grants for the engine role (P-13): `SELECT` on `rounds`, `round_secrets`, `admissions`, `ledger_holds`,
  `ledger_accounts`, `chain_records`, `round_snapshots`, `round_execution_heads`; `INSERT` on `rounds`,
  `round_secrets`, `round_admission_heads`, `round_execution_heads`, `chain_records`, `domain_events`,
  `round_snapshots`, `ledger_accounts`, `ledger_transactions`, `ledger_postings`, `ledger_hold_releases`,
  `outbox_messages`; `UPDATE` on `round_execution_heads` and `ledger_accounts` (through the ledger
  triggers).

## 10. Builder rules for the implementation run

- goimports grouping with the `github.com/juano-morello/orvex` local prefix (`.golangci.yml`).
- Integration tests are in `package execution_test` (external). Unit tests that use unexported helpers are
  in `package execution`. `internal/ledger` tests are in `package ledger`.
- Every integration test file starts with `//go:build integration`. The execution package has one
  `TestMain` calling `pgtest.Run`, in `helpers_integration_test.go`.
- Tests assert SQLSTATEs (`postgres.SQLState`), `PgError.ConstraintName`, sentinel errors, reason codes
  and bytes, never message text.
- No sleeps to wait for blocking: poll `pg_stat_activity` (`pgtest.WaitUntilBlocked`) with its 10 s
  deadline. Waiting for database time to pass (the end-to-end crash deadline) polls the database clock.
- Never edit a golden fixture, `internal/game/**`, `internal/audit/**`, `internal/canonjson/**` or
  migrations `0001` to `0007`. If a golden reproduction differs, find which side contradicts this spec or
  LBT-9; never change a fixture to match output.

## 11. LBT-12 acceptance as tests

| Acceptance | Tests (§12 names the files) |
|---|---|
| Injected failure leaves no partial effects | `TestInjectedFailureLeavesNoPartialEffects`, `TestLedgerFailureRollsBack`, `TestLedgerFailureRetried`, `TestCommitFailureRollsBack`, `TestCrashBeforeCommit`, `TestFailClosedHaltsRound` |
| Duplicate payout impossible | `TestDuplicatePayoutImpossible`, `TestConflictingPerRoundCommits`, `TestCrashAfterCommit`, `TestLedgerPostingsOnePerEvent`; LBT-10 `TestLedgerOneTransactionPerRecord` |
| Cross-round balance protection | `TestCrossRoundBalanceProtection`, `TestCrossRoundAdmissionWaitsForExecution`, `TestExecuteBetConsumesHoldAtExactBalance`, `TestChainRecordsRequireHoldRelease` |
| One transaction: events, result, postings, version, outbox | `TestExecuteReproducesGoldenHistories`, `TestTransitionReproducesGoldenRecords`, `TestExecuteOutbox`, `TestExecuteSnapshots` |
| Optimistic checks and fencing | `TestStaleFencingTokenBeforeStep`, `TestTakeoverWaitsForCommit`, `TestTakeoverBeforeCompareAndSet`, `TestStaleCacheDiscarded`, `TestConflictingPerRoundCommits` |
| Authoritative state from the chain (E-3) | `TestColdLoadMatchesCache`, `TestColdLoadChecksSnapshots`, `TestExecuteReproducesGoldenHistories` (record 22 of `worked-example` runs after a cold load) |
| ADR-006 conflicting per-round commits | `TestConflictingPerRoundCommits`, `TestStaleCacheDiscarded` |
| ADR-006 rollback on ledger failure | `TestLedgerFailureRollsBack`, `TestLedgerFailureRetried`, `TestInjectedFailureLeavesNoPartialEffects` |
| ADR-006 stale fencing token | `TestStaleFencingTokenBeforeStep`, `TestTakeoverWaitsForCommit`, `TestTakeoverBeforeCompareAndSet` |
| ADR-006 crash before/after commit, ambiguous commit | `TestCrashBeforeCommit`, `TestCrashAfterCommit` |
| ADR-006 double cash-out | `TestDuplicatePayoutImpossible` |
| ADR-006 cross-round overspending (execution part) | `TestCrossRoundBalanceProtection`, `TestCrossRoundAdmissionWaitsForExecution` |
| ADR-006 settlement barrier (execution part) | `TestSettlementResolvesEarlierCommands` |
| Round creation (E-16) | `TestCreateRound`, `TestCreateRoundIdempotent`, `TestCreateRoundConflict` |
| Export and verification of executed rounds | `TestExecuteReproducesGoldenHistories` (export passes `verification.Verify`) |

Not here: duplicate admission (LBT-11), pod termination and recovery (LBT-14), load (LBT-15).

## 12. Implementation plan (after approval)

### Files

| File | Action |
|---|---|
| `migrations/0008_execution_ledger.up.sql`, `.down.sql` | new, per §6 |
| `internal/persistence/postgres/errors.go` | edit: `CodeHoldUnreleased` |
| `internal/persistence/postgres/tx.go` | edit: `ExecutionTxOptionsWithTimeouts` |
| `internal/persistence/postgres/doc.go` | edit: `OX009`, B's options |
| `internal/persistence/postgres/tx_test.go`, `errors_test.go` | edit (unit) |
| `internal/persistence/postgres/execution_ledger_integration_test.go` | new (integration) |
| `internal/persistence/postgres/migrate_test.go`, `migrate_integration_test.go`, `cmd/crashctl/migrate_integration_test.go` | edit: eighth migration (step 1) |
| `internal/runtime/admission/*_integration_test.go` | edit only if `TestLookup` inserts a record for a held admission (step 1) |
| `internal/ledger/doc.go`, `ledger.go`, `ids.go`, `crashv1.go` | new |
| `internal/ledger/ids_test.go`, `crashv1_test.go`, `imports_test.go` | new (unit) |
| `internal/runtime/execution/doc.go` | new: package doc (ADR-005 Transaction B, E-3, E-6, E-7 in brief; this spec's path) |
| `internal/runtime/execution/executor.go` | new: `DB`, `Config`, `DefaultConfig`, `New`, `Executor`, `Lease`, `Outcome`, `ExecuteNext`, `Drain`, `Forget`, entries |
| `internal/runtime/execution/load.go` | new: cold load (§5.2) |
| `internal/runtime/execution/transition.go` | new: §5.4, §5.5 checks |
| `internal/runtime/execution/commit.go` | new: §5.6, §5.7 |
| `internal/runtime/execution/round.go` | new: `NewRound`, `Created`, `CreateRound` |
| `internal/runtime/execution/policy.go`, `crashv1.go` | new: `Policy`, `CrashV1Policy` |
| `internal/runtime/execution/errors.go`, `retry.go` | new: sentinels, `FailClosedError`, classification, backoff |
| `internal/runtime/execution/executor_test.go`, `transition_test.go`, `retry_test.go`, `crashv1_test.go`, `errors_test.go` | new (unit) |
| `internal/runtime/execution/helpers_integration_test.go`, `round_integration_test.go`, `execute_integration_test.go`, `concurrency_integration_test.go`, `failure_integration_test.go`, `crossround_integration_test.go`, `e2e_integration_test.go` | new (integration) |
| `docs/architecture/persistence.md` | edit: section "Execution (LBT-12)": B's statements and options, account model, posting table, holds, outbox topic and payload, snapshot cadence, error mapping, engine-role grants; catalogue rows for `OX009` and `ledger_postings_event_key` |
| `README.md` | edit: Status adds LBT-12; Layout adds `internal/runtime/execution` and `internal/ledger` |
| `AGENTS.md` | edit only if LBT-11 did not already add `internal/runtime/` to the `make test-integration` hand-off line |

Unchanged: `docs/adr/**`, `internal/game/**`, `internal/audit/**`, `internal/canonjson/**`,
`internal/runtime/admission` non-test code, migrations `0001` to `0007`, golden fixtures, `go.mod`,
`go.sum`, `Makefile`, `.github/workflows/ci.yml`, `cmd/*` non-test code.

### Step 1: migration `0008`, `postgres` additions, LBT-10 and LBT-11 test updates

- Write `0008_execution_ledger.{up,down}.sql` per §6, with the names exactly as given.
- `errors.go`: `CodeHoldUnreleased = "OX009"`. `tx.go`: `ExecutionTxOptionsWithTimeouts`. `doc.go`: both.
- Unit tests:
  - `tx_test.go` `TestExecutionTxOptionsWithTimeouts`: for `(3s, 1s)` the `BeginQuery` equals prototype 1's
    string exactly; for `(1500ms, 250ms)` it holds `'1500ms'` and `'250ms'`; `IsoLevel` is
    `pgx.ReadCommitted` and `AccessMode` `pgx.ReadWrite`.
  - `errors_test.go`: add `OX009` wherever the codes are enumerated.
- Migration test edits:
  - `TestEmbeddedMigrations`: append `execution_ledger` (after LBT-11's name) to `want`; if the header
    regex does not accept `(LBT-12 spec E-…)`, widen it to `\(LBT-[0-9]+ spec [A-Z]-[0-9]+[^)]*\)`.
  - `TestMigrateCLI`: `up` prints eight `applied` lines and `schema version 8`; `status` eight lines; the
    down-with-data case completes the `0008` step before the first refusing down (LBT-11's `0007`) stops
    it.
  - `TestMigrateDownRefusesAuditData`: `MigrateDown(0)` reverts `[8]`, then fails at `0007` with `OX003`;
    the version is 7; record and snapshot counts are unchanged; a new `up` applies `[8]`.
  - Any other assertion of the newest version.
- LBT-11 `TestLookup` (conditional): if it inserts a chain record for an admission that holds credits,
  insert the matching `ledger_hold_releases` row in the same transaction. Change nothing else in LBT-11.
- `execution_ledger_integration_test.go` (each test from `pgtest.MigratedDatabase`):
  - `TestLedgerPostingsOnePerEvent`: load `instant-crash`; create two accounts and a ledger transaction
    for record 1; a posting for event 1 commits; a second posting for event 1 (another index) gives
    `23505` `ledger_postings_event_key`; a ledger transaction for record 5 with a posting for event 1
    gives the same; postings with NULL `event_sequence` are unaffected (two grants commit).
  - `TestChainRecordsRequireHoldRelease`: load `worked-example`; fund a player; insert admission 23 with
    the existing helpers (`insertAdmission(admissionOf(rejectedRecord(round, 23, head22)))`) and a hold on
    it. In one transaction, advance the head to 23 and insert record 23 without the release: `COMMIT`
    fails with `OX009` and the head stays 22. The same transaction plus the release commits. Record 24 for
    an admission without a hold commits.
  - `TestExecutionTxOptionsWithTimeoutsApplied`: inside a transaction begun with the options,
    `current_setting` gives `read committed`, the two timeouts; after `COMMIT` both are back to the session
    values; with a session `SET synchronous_commit = off` the transaction shows `on`; with `remote_apply`
    it shows `remote_apply`.

### Step 2: `internal/ledger`

Write the package per §7, E-8 and E-9.

Unit tests (`make test`):
- `ids_test.go`:
  - `TestHouseAccountID`, `TestRoundAccountID`, `TestTransactionID`: equal the section 1 literals.
  - `TestIDsAreUUIDv8`: version nibble 8 and variant `10` for 1,000 random round IDs and sequences.
  - `TestIDsDistinct`: different rounds, sequences and names give different IDs.
- `crashv1_test.go`:
  - `TestCrashV1Descriptor`: equals `v1.Descriptor()`.
  - `TestCrashV1Postings`, a table: `BetPlaced` (player → round, `FromHold`, kind `crash_bet`);
    `BetCashedOut` (round → player, kind `crash_cash_out`); `RoundSettled` with `B > W`, `B < W`, `B = W`
    (kind `crash_settlement` or `""`); `BetLost` + `RoundSettled` (one posting, event sequence of
    `RoundSettled`); `RoundStarted`, `RoundCrashed` (none); event sequences offset by `first`.
  - `TestCrashV1PostingsRejectsBadInput`: a foreign event type, a `BetCashedOut` with payout 0, a
    `BetPlaced` with 0 credits each give an error.
  - `TestCrashV1PostingsWorkedExample`: decode every event of `worked-example.jsonl` with the crash/v1
    codec, apply the rule per record, apply the postings to an in-memory map of balances: §8's balances,
    round account 0, Σ balances = 0.
- `imports_test.go` `TestLedgerImportsArePure`: parse every non-test file; imports are limited to
  `crypto/sha256`, `encoding/hex`, `errors`, `fmt`, `strconv`, `internal/game`, `internal/game/crash/v1`
  (no pgx, `net`, `os`, `time`, `math/rand`).

### Step 3: `internal/runtime/execution` code

Write the package per §5, §7 and E-2 to E-16. The entry map is a mutex-guarded map from round ID to an
entry with its own mutex; `Forget` and drops delete map entries.

### Step 4: unit tests (`make test`)

- `crashv1_test.go`: `TestCrashV1PolicyDescriptor`; `TestCrashV1Checkpoint` (true for records with
  `RoundStarted`, `RoundCrashed`, `RoundSettled`; false otherwise and for no events); `TestCrashV1Final`
  (true only in `PhaseSettled`; a foreign state type errors).
- `errors_test.go`: `TestFailClosedError`: `errors.Is(err, ErrFailClosed)` and `errors.Is(err, cause)`
  hold; the text is `execution: round <uuid> halted at sequence <k>: <cause>`.
- `retry_test.go`:
  - `TestClassifyStatementError`, a table over E-15's rows: `40001`, `40P01` retry; `55P03` busy;
    `*net.OpError`, `08006`, `53300`, `57P01`, `25P03` retry; `57014` with `ctx` done and
    `context.Canceled` give `ctx`; `23503` on `chain_records_prev_fkey` gives conflict; `23514` on
    `ledger_accounts_floor_check`, `23505` on `ledger_postings_event_key`, `OX008`, `22023`, `42P01`,
    `XX000` give internal.
  - `TestClassifyCommitError`: `OX004` and `23503` on `round_execution_heads_head_fkey` give conflict;
    `40001`, `40P01` retry; `OX009`, `23000`, `40002`, `pgx.ErrTxCommitRollback` internal; `40003`,
    `57P01`, `08006`, `53300`, `XX000`, `io.ErrUnexpectedEOF`, a wrapped `context.Canceled` ambiguous.
  - `TestConflictDecision`: E-7's table (no row; other token; `processed ≥ k` with equal hash and with
    another hash; `processed = k − 1`; `processed < k − 1`).
  - `TestBackoffBounds`: 1,000 samples per attempt `n = 2..6` lie in `[0, min(50 ms, 5 ms · 2^(n−2))]`.
- `executor_test.go` (a fake `DB` that fails the test when called where noted):
  - `TestDefaultConfig`: `{buildinfo.Version, 3s, 1s, 3}`.
  - `TestNewValidation`: `ErrInvalidConfig` for each `Config` bound ±1 unit, `LockTimeout ≤
    IdleInTransactionTimeout`, an empty, 129-byte or space-containing `EngineBuild`, no policies, two
    policies for one key, a policy without an adapter, a policy whose `Ledger().Descriptor()` differs.
  - `TestExecuteNextValidation`: nil round ID, token −1 give `ErrInvalidRequest`; `Drain` with limit 0
    gives `ErrInvalidRequest`; the fake DB is never called.
- `transition_test.go` (package `execution`, no DB):
  - `TestTransitionReproducesGoldenRecords`: for each golden history, build the initial state from the
    genesis config and the header's `configSecret`, then for each command record feed the admission (its
    command bytes, ID, time and S-10 holds with synthetic account IDs) through the transition with
    `EngineBuild "golden"`: the record bytes equal the golden line, the hash equals `chain.LinkHash`,
    event types are the events' `type`s, releases, postings and checkpoints equal §8.
  - `TestTransitionFailClosed`: a `failingAdapter` (embeds the crash/v1 adapter; `Decide` returns
    `errors.New("boom")` at a chosen sequence) gives `*FailClosedError` with that sequence and no record;
    a failing `Evolve` likewise.
  - `TestTransitionHoldChecks`: a `BetPlaced` with no hold, two holds, a hold of another amount, or a hold
    on another player's account gives `ErrCorrupt`; a rejected `PlaceBet` with a hold gives a release and
    no posting; a non-player hold account on a rejected command is released.
  - `TestTransitionValidatesPostings`: a fake `Policy` whose rule returns amount 0, debit = credit, an
    event sequence outside the record, two postings for one event, or `FromHold` on a non-player debit
    gives `*FailClosedError`.
  - `TestMaxRecordBytes`: equals `verification.MaxLineBytes` and the `0003` `CHECK` value 67108864.

### Step 5: integration helpers (`helpers_integration_test.go`)

- `TestMain`: `os.Exit(pgtest.Run(m))`.
- `newExecutor(t, db, mutate func(*execution.Config), reg *game.Registry)`: default registry is crash/v1
  only; `EngineBuild "golden"` unless mutated.
- `fundPlayer(t, db, player, credits)`: the global system account `faucet` (floor not enforced) if absent,
  the player account if absent, one ledger transaction `grant` with one posting faucet → player.
- `seedAdmission(t, db, roundID, commandID, cmdBytes, effectiveTime, hold *holdSpec)`: in one transaction,
  lock the admission head, insert the next admission, insert the hold if given, advance
  `admitted_sequence`. `holdFor(cmd)` applies LBT-11 S-10 (player authority, non-empty player,
  `1 ≤ bet ≤ 1,000,000,000`).
- `seedGolden(t, db, name) goldenRound`: `CreateRound` from the golden genesis (round ID, seed from
  `configSecret`, limits, `createdAtUnixMicro`), funds each player with the sum of their holds, seeds every
  admission from the golden records, returns the golden lines.
- `effects(t, db, roundID) effectSet` (E-17) and `requireEffects(t, got, want)`.
- `installFaults(t, db)`: schema `orvex_test` with sequence `fault_budget`, table `fault_config(target
  text, sqlstate text, constraint_name text)`, a trigger function that raises per `fault_config` while
  `nextval('orvex_test.fault_budget') <= budget`, `enableFault(table, sqlstate, constraint, budget)`,
  `disableFaults()`, and `enableCommitFault(sqlstate)` (a deferred constraint trigger on
  `orvex.outbox_messages`).
- `faultDB`: wraps the pool; implements `execution.DB`; `faultTx` embeds `pgx.Tx` and runs hooks
  `beforeCAS`, `beforeBatch`, `beforeCommit`, `afterCommit` (§E-17 item 1); `backendPID(tx)`.
- `takeover(t, db, roundID, owner) (newToken int64)`.
- `cmdID(n)`, `newRoundID(t)`, `seedWithDeadline(t, roundID, min, max) [32]byte` (scan seeds `0, 1, …` as
  32-byte big-endian counters until `v1.InitialState` gives `min ≤ D ≤ max`).
- `failingAdapter` (same as the unit-test one) for `TestFailClosedHaltsRound`.

### Step 6: integration tests (`make test-integration`)

Each test starts from its own `pgtest.MigratedDatabase`, uses `pgtest.Pool` unless stated otherwise, and
fresh round IDs unless it reproduces a golden.

**`round_integration_test.go`**
- `TestCreateRound`: rows written: `rounds` (versions, chain, creation time), `round_secrets` = the codec's
  secret bytes, both heads with defaults, the genesis record equal to `chain.EncodeGenesis` of the inputs
  with token 0, snapshot 0 with the state hash of `InitialState` and NULL `state`, the `round_pool`
  account with `RoundAccountID`, the `house` account with `HouseAccountID`, one outbox row
  `(round, 0, 0, 'round.record.v1', genesis)`. The four golden genesis lines are reproduced byte for byte.
- `TestCreateRoundIdempotent`: the same `NewRound` twice: the second returns `Duplicate`, and every row
  count is unchanged. A second round reuses the existing `house` row.
- `TestCreateRoundConflict`: same round ID, another seed: `ErrRoundExists`, nothing written. A config
  whose own round ID differs from `RoundID`: `ErrInvalidRound`. Rules version 2: `ErrUnknownRules`.

**`execute_integration_test.go`**
- `TestExecuteReproducesGoldenHistories`, per golden: `seedGolden`, `Drain` (limit 100) executes every
  admission; `chain_records.record` for sequences `0..N` equal the golden lines; `command_results` match;
  ledger rows, releases, snapshots, outbox rows and final totals equal §8; Σ `debits_posted` = Σ
  `credits_posted` over all accounts; `eventstore.ExportHistory` then `verification.Verify(ModeVerify)`
  passes and the export's record lines equal the golden lines up to the newest snapshot.
- `TestExecuteIdle`: a round with no admission: `Outcome{Executed: false}` and no write.
- `TestExecuteBetConsumesHoldAtExactBalance`: alice funded exactly 100, bet 100: committed; alice
  `debits_posted 100`, `held 0`, `credits_posted 100`.
- `TestExecuteRejectedBetReleasesHold`: a round with `MaxBetCredits 100`, a bet of 150 admitted with a
  hold of 150: rejected `invalid_amount`; release only; no ledger transaction; `held` back to 0.
- `TestExecuteSnapshots`: snapshot rows exactly at 0 and the phase records, hashes equal
  `chain.StateHash` of the replayed state, `state` NULL, `engine_build "golden"`.
- `TestExecuteOutbox`: one row per record with key `(round, k, 0)`, topic, payload equal to the record;
  no payload before the `RoundCrashed` record contains `crashPointScaled`, `crashTick` or
  `crashDeadlineUnixMicro`, and none before the `RoundSettled` record contains the seed's hex.
- `TestColdLoadMatchesCache`: execute half of `worked-example`, `Forget`, execute the rest: the records
  are identical to an uninterrupted run.
- `TestColdLoadChecksSnapshots`: `pgtest.LoadHistory(worked-example)` (token 0, no pending admission):
  `ExecuteNext` returns `Executed: false`, so the cold load replayed and matched every snapshot. The same
  history with checkpoint 9's `stateHash` altered in the header (loaded into another database) gives
  `ErrCorrupt`; a second call returns it without a query (halted); nothing is written.
- `TestFailClosedHaltsRound`: registry with `failingAdapter` failing at sequence 2 of three seeded
  admissions (a bet with a hold at 2): `ExecuteNext` executes 1, then returns `*FailClosedError` at 2;
  `effects` unchanged by the failed call; the hold of 2 is still held; no outbox row for 2; a second call
  returns the same error without a database write (`faultDB` counts zero `BeginTx`); admission 3 is not
  executed; after `Forget` the error repeats.
- `TestSettlementResolvesEarlierCommands`: seeded bets, `StartRound`, cash-outs, `CloseRound`,
  `SettleRound`: records exist for every sequence in order; the settlement's `BetLost` events are exactly
  the bets without a `BetCashedOut`; the settlement posting equals `TotalBetCredits − TotalPayoutCredits`.
  With `failingAdapter` failing at a cash-out before `SettleRound`, the settlement is never executed.

**`concurrency_integration_test.go`**
- `TestConflictingPerRoundCommits`: per trial a round (`MaxBets 100`, seed from `seedWithDeadline(1 s,
  10 s)`) with 200 seeded admissions: 100 funded bets, `StartRound`, 50 cash-outs inside the deadline, 47
  rejected commands, `CloseRound` at `T0 + D`, `SettleRound`. Two `Executor` instances with the same token
  run `Drain` concurrently; 10 trials:
  - variant A, same `EngineBuild`: every sequence has exactly one record; every outcome is either
    executed or adopted; one release per hold; one posting per money event; totals as in a sequential
    run; one outbox row per record; the export verifies.
  - variant B, builds `a` and `b`: the same invariants; every record carries the build of the instance
    that committed it.
- `TestStaleCacheDiscarded`: 20 seeded admissions; instance X executes to `k = 10`; instance Y (build
  `other`) executes 11 to 15; X's `ExecuteNext` executes 16: its record's `prevHash` is the hash of Y's
  record 15, X's computed record 11 was discarded (reload), and every sequence has one record.

**`failure_integration_test.go`**
- `TestInjectedFailureLeavesNoPartialEffects`, a table over `domain_events`, `ledger_hold_releases`,
  `ledger_transactions`, `ledger_postings`, `outbox_messages`, `round_snapshots` (each with an admission
  that writes that table): a permanent fault (`P0001`) on the table makes `ExecuteNext` return
  `ErrInternal`; `effects` are unchanged; after `disableFaults()` the next call succeeds and `effects`
  change exactly as one execution.
- `TestLedgerFailureRollsBack`: fault `23514` with constraint `ledger_accounts_floor_check` on
  `ledger_postings`: `ErrInternal` (not a business rejection), nothing written, head unchanged.
- `TestLedgerFailureRetried`: fault `40P01` on `ledger_postings` with budget 1: `ExecuteNext` succeeds on
  attempt 2; one posting, one release, one record.
- `TestCommitFailureRollsBack`: `enableCommitFault('23000')`: `ErrInternal`; nothing written.
- `TestStaleFencingTokenBeforeStep`: `takeover` before the call: `ErrFenced`; nothing written; the entry
  is dropped; with the new token the call succeeds.
- `TestTakeoverWaitsForCommit`: `beforeCommit` blocks; a concurrent `takeover` blocks on the head row
  (`WaitUntilBlocked`); releasing the hook commits B; the takeover then commits; the old lease's next call
  gives `ErrFenced` and writes nothing.
- `TestTakeoverBeforeCompareAndSet`: `beforeCAS` runs a committed `takeover`: the compare-and-set reports 0
  rows; `ErrFenced`; nothing written.
- `TestCrashBeforeCommit`: `beforeCommit` terminates the executor's backend (`pg_terminate_backend`):
  (a) the same call succeeds on a later attempt, and `effects` show exactly one execution; (b) an instance
  with `MaxAttempts 1` returns `ErrUnavailable`, `effects` are unchanged, the test abandons the instance,
  and a new instance cold-loads and executes `k` once.
- `TestCrashAfterCommit`: `afterCommit` runs the real commit and returns `io.ErrUnexpectedEOF`: (a) the
  same call returns `Outcome{Adopted: true}` for `k`, `effects` show one execution, and
  `orvex.command_results` holds the result; (b) a new instance's next call executes `k+1` (or reports
  `Executed: false`), never `k` again.
- `TestDuplicatePayoutImpossible`:
  1. the `in-progress` golden seeded up to admission 2; X drains; Y cold-loads at 2 (idle); admission 3
     (alice's cash-out) is seeded; X executes it; Y (build `other`) executes: reload, then idle. One
     `crash_cash_out` transaction, one posting for event 3, alice credited once.
  2. a second cash-out admission for alice: rejected `already_cashed_out`; no posting.
  3. direct SQL: a second ledger transaction for record 3 gives `23505` `ledger_transactions_record_key`; a
     ledger transaction for record 4 with a posting for event 3 gives `23505` `ledger_postings_event_key`;
     a second release of admission 1's hold gives `23505` `ledger_hold_releases_pkey`.
  4. two instances with the same token race on a fresh round's cash-out: one posting.

**`crossround_integration_test.go`** (uses `admission.Sequencer`, LBT-11 `DefaultConfig`)
- `TestCrossRoundBalanceProtection`:
  1. alice funded 1,000; ten rounds; ten goroutines admit `PlaceBet{alice, 150}` concurrently, one per
     round: exactly six admitted, four `insufficient_credits`;
  2. concurrently, `Drain` all ten rounds and admit `PlaceBet{alice, 150}` in ten new rounds: all ten
     rejected `insufficient_credits` in every interleaving;
  3. in round 1, admit a second `PlaceBet{alice, 100}`: admitted with a hold; drain: rejected
     `duplicate_bet`, hold released; then `PlaceBet{alice, 100}` in round 11 is admitted and executed;
  4. final: alice `credits_posted 1,000`, `debits_posted 1,000`, `held 0`; one release per hold; Σ debits
     = Σ credits; no `23514` was observed by any call.
- `TestCrossRoundAdmissionWaitsForExecution`:
  - case "bet accepted": alice funded 200; round 1 bet 150 admitted. B for it pauses in `beforeCommit`
    (after the release and posting locked alice's row); an `Admit` of `PlaceBet{alice, 60}` in round 2
    blocks (wait until one backend of the database waits on a lock in `pg_stat_activity`, like LBT-11's
    `waitLockWaiters`); releasing B commits it; the admission returns `insufficient_credits`.
  - case "bet rejected": round 1 has `MaxBetCredits 100`, bet 150 admitted with its hold; B (rejected
    `invalid_amount`, release only) pauses; the round-2 admission of 60 blocks; after B commits it is
    admitted (available 200).

**`e2e_integration_test.go`**
- `TestAdmitAndExecuteRound`: `CreateRound` with `seedWithDeadline(500 ms, 2 s)`; fund alice and bob;
  admit their bets and `StartRound` through the `Sequencer`; `Drain`; poll the database clock until it is
  after `T0`; admit alice's `CashOut`; `Drain`: accepted, payout credited; admit `CloseRound` with
  `NotBeforeUnixMicro = T0 + D` (on `ErrClosureNotDue`, poll the database clock until `T0 + D` and resubmit
  the same request); `Drain`: `RoundCrashed`; admit `SettleRound`; `Drain`: settled, `Outcome.Final`;
  export verifies; holds all released; round account 0; house balance = Σ bets − Σ payouts; seven
  outbox rows.

### Step 7: docs

Make the `docs/architecture/persistence.md` and `README.md` edits from the files table, and the `AGENTS.md`
edit if needed.

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
go test -race -tags integration -count=5 -run 'Conflicting|StaleCache|CrossRound|Takeover|Crash' ./internal/runtime/execution/
```

- The `-count=5` run is the flakiness check for the concurrency and failure tests. It must pass all five
  times.
- `git diff --stat` must show no change to `docs/adr/`, `internal/game/`, `internal/audit/`,
  `internal/canonjson/`, migrations `0001` to `0007`, `go.mod`, `go.sum`, golden fixtures, or
  non-test code of `internal/runtime/admission` and `cmd/`.
- If a test contradicts this spec, find which side is wrong. Never weaken an invariant or a golden
  comparison to make a run pass.

## 13. Out of scope and open items

Out of scope:
- in this run: any Go, SQL, migration, CI or `go.mod` change;
- LBT-13: the outbox relay and its delivery loop, the wire format, the gateway and gRPC protocol;
- LBT-14: lease acquisition, renewal and takeover, failover, the engine loop and wake-up, the scheduler,
  roles and grants, snapshot `state` bytes, halted-round operations;
- LBT-15: load tests, production failpoints, metrics, tuning of timeouts and attempts, hot-row and
  record-size measurements;
- the audit ledger stage and history format v2 (section 2 item 8);
- `crashctl audit export` (LBT-10 open item 9).

Open for the engineer:
1. The `BEGIN` settings in Transaction B, a deviation from P-23's "first statement" wording (§2 item 1).
2. The validated in-memory cache as ADR-005's "known state version" (§2 item 3, E-3).
3. Migration `0008`: one posting per event and the `OX009` hold-release check (§2 item 5, E-11).
4. The net settlement posting instead of one posting per `BetLost` (§2 item 9, E-9).
5. Outbox payload = the exact chain record bytes, topic `round.record.v1` (§2 item 7, E-12).
6. Round creation owned by LBT-12, with a caller-supplied creation time (§2 item 10, E-16).
7. `internal/ledger` as a new top-level internal package (§2 item 11, E-1).
8. Snapshot cadence at phase changes only (E-13).
9. No durable fault record for fail-closed commands (E-14).
10. Follow-up issue: "Audit ledger stage (history format v2)": a verifier stage that recomputes postings
    from chained events with `internal/ledger` and compares them with exported ledger rows, plus the
    oracle and golden changes `make golden-check` needs (LBT-9 §11).
11. Follow-up issue: `crashctl audit export ROUND_ID` now that LBT-12 writes snapshots.

## Engineer addendum (2026-10-15)

Decided by the engineer on 2026-10-15. It resolves section 2 item 14, the risk inherited from LBT-11
open item 4: a `CloseRound` admitted before `T0 + D` must not be able to close the admission window.
It is fixed at admission, in the LBT-12 implementation:

+- `Executor.CreateRound` (E-16) stores the round's crash deadline offset `D` (microseconds, from the
  approved crash/v1 math, D-9: `D = n_c * Q`) in a column that only the `SECURITY DEFINER` function
  `orvex.admit_command` reads. The admitting role gets no grant on it, and it never leaves the database
  through admission results, exports before settlement, the outbox, or errors.
+- Admitting `StartRound` records the bound `close_not_before = T0 + D` on the round's admission head,
  where `T0` is that admission's effective time.
+- `admit_command` refuses a `CloseRound` whose effective time is earlier than that stored bound, with
  the same not-yet-due rejection class LBT-11 uses. The caller-supplied `NotBeforeUnixMicro` (LBT-11
  S-8) no longer decides closure.
+- Tests against real PostgreSQL: an early `CloseRound` is refused and the window stays open; a
  `CloseRound` at exactly `T0 + D` is admitted (closure at equality, as LBT-11 S-8 and the lifecycle
  spec item 7 state); the round then settles normally; the admitting role cannot read the stored
  offset.
+- The change goes in migration `0008` or an immediately following one. It changes LBT-11 S-8, the
  admitting role's grants (LBT-11 §2 item 13) and E-16.

The implementation plan `docs/specs/6f13b30a_execution-ledger-implementation.md` records how it is
built: migration `0009_close_bound`, and the removal of `NotBeforeUnixMicro`.

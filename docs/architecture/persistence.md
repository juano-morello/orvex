# Persistence: PostgreSQL schema, isolation and migrations

This document describes the PostgreSQL system of record (ADR-005) built in LBT-10. The normative design
is [the LBT-10 spec](../specs/17380a6e_postgresql-schema-migrations.md) (decisions P-1 to P-26). The
governing decisions are [ADR-001](../adr/0001-audit-first-deterministic-execution.md),
[ADR-002](../adr/0002-durable-command-admission-and-timing.md),
[ADR-005](../adr/0005-postgresql-event-store-and-transaction-model.md) and
[ADR-006](../adr/0006-correctness-and-verification-strategy.md).

The schema lives in `migrations/`, the runner and Go constants in `internal/persistence/postgres`, the
history export in `internal/persistence/eventstore` and the test harness in
`internal/persistence/postgres/pgtest`.

## 1. Tables and invariants

Every application object lives in schema `orvex`. The migration runner's bookkeeping lives in
`orvex_migrate`. `public` stays empty. All SQL is schema-qualified, and every function is
`LANGUAGE plpgsql` with `SET search_path = pg_catalog`.

| Table | Kind | Holds | Invariants |
|---|---|---|---|
| `rounds` | append-only | round identity, game, rules/schema/state versions, chain version, creation time | non-nil UUID; game `^[a-z][a-z0-9-]{0,31}$`; versions in `[1, 2^32-1]`; chain `orvex/audit/v1` |
| `round_secrets` | append-only | the codec's secret config (the seed): from `0010` only sealed with AES-256-GCM in `secret_scheme`, `secret_key_id`, `secret_nonce`, `secret_ciphertext`; legacy rows written before `0010` keep plaintext `config_secret` (§6, §16); from `0009`, the secret close offset `close_offset_micros` (§12) | one row per round; offset `>= 0`; exactly one form (`round_secrets_form_check`); a sealed form of the K-5 shape (`round_secrets_sealed_shape_check`); no new plaintext (`round_secrets_sealed_check`, `NOT VALID`) |
| `round_secret_reveals` | append-only | from `0010`: the secret config revealed by the record that finalizes the round (§13) | one per round; names an accepted command record of its round (`OX010`) |
| `admissions` | append-only | admitted commands: sequence, command ID, effective time (µs), canonical command bytes | gapless from 1 by a self foreign key; `command_id` unique across all rounds and non-nil |
| `round_admission_heads` | mutable, undeletable | `admitted_sequence`, the admission window (§12), Transaction A's lock row | forward-only (`OX005`); deferred foreign key to the last admission; window forward-only (`OX005`); sealed is final |
| `chain_records` | append-only | exact LBT-9 record bytes, link hash, previous hash, result columns, fencing token | see below |
| `round_execution_heads` | mutable, undeletable | `processed_sequence`, `fencing_token`, `owner_id`, `lease_expires_at` | forward-only; an owner change needs a higher token (`OX005`); deferred foreign key to the last record |
| `domain_events` | append-only | `(round_id, event_sequence)` → emitting record, event type | contiguous from 1 by a self foreign key |
| `round_snapshots` | append-only | engine-computed state hash per chain record, optional state bytes | references its chain record |
| `ledger_accounts` | totals mutable by triggers only | player and system accounts, `debits_posted`, `credits_posted`, `held` | floor `debits_posted + held <= credits_posted` on player accounts |
| `ledger_transactions` | append-only | one per business operation | at most one per chain record |
| `ledger_postings` | append-only | one amount moved from a debit to a credit account | `amount > 0`; distinct accounts; balanced by construction; at most one posting per chained event (`ledger_postings_event_key`, §13) |
| `ledger_holds`, `ledger_hold_releases` | append-only | reservations at admission and their releases | a release needs the admission's chain record |
| `outbox_messages` | delivery columns mutable | messages with dedupe key `(round_id, sequence, message_index)` | `OX006` guard; published rows may be deleted |

`chain_records` enforces the LBT-9 chain in the database:

- `hash = sha256('orvex/audit/v1/record:' || record)` (`chain_records_hash_check`), a second SHA-256
  implementation next to the engine's;
- `(round_id, sequence - 1, prev_hash)` references the previous record's `(round_id, sequence, hash)`
  (`chain_records_prev_fkey`), so a fork, a gap or a broken link is impossible; genesis is sequence 0
  with 32 zero bytes (`chain_records_genesis_check`);
- each command record references its admission (`chain_records_admission_fkey`);
- a record is at most 64 MiB and holds no `0x0A` byte (`chain_records_record_check`), so every row is
  one valid export line;
- the `chain_records_binding` trigger (`OX008`) compares the record's fixed prefix and suffix with byte
  templates built from the row's columns, its admission and its `rounds` row (spec §5.3);
- the deferred `chain_records_fenced` trigger (`OX004`) requires, at commit, the round's current
  fencing token and a processed sequence covering the record;
- the deferred `chain_records_holds_released` trigger (`OX009`, §13) requires, at commit, a release for
  every credit hold of the record's admission.

Relationships: `rounds` 1–1 `round_secrets`, `round_admission_heads` and `round_execution_heads`;
`round_secrets` 1–0..1 `round_secret_reveals`, which references the chain record it was revealed at;
`rounds` 1–n `admissions` 1–0..1 `chain_records` (sequence ≥ 1); exactly one genesis record per round;
each chain record has 0..n `domain_events` and 0..1 `round_snapshots` rows.

## 2. Authoritative versus derived data

- **Authoritative:** `chain_records.record`. The canonical bytes of commands, events and results live
  only there (LBT-9 A-7, A-8). `admissions.command` and `admissions.effective_time_unix_micro` are what
  was durably admitted; the binding trigger requires each record to repeat them exactly (ADR-002).
- **Derived and bound:** `chain_records.result_status`/`result_reason` and the view
  `orvex.command_results` (bound by `OX008`); `rounds` (bound to the genesis record);
  `domain_events` (an index without payload copies).
- **Engine-computed:** `round_snapshots.state_hash`, written at commit and never recomputed by replay at
  export time (LBT-9 §11).
- Columns are never the source for hashing or export. `eventstore.ExportHistory` copies record bytes
  verbatim, in sequence order, up to the newest snapshot (P-24). It also returns `RoundRecords`, the
  round's chain record count read in the same snapshot, so a caller can tell when records after the
  newest snapshot were left out. `crashctl audit export` (§14) writes the file.

## 3. Constraint catalogue

Custom SQLSTATEs, class `OX`, exported as Go constants in `internal/persistence/postgres`:

| Code | Go constant | Raised when |
|---|---|---|
| `OX001` | `CodeAppendOnly` | `UPDATE`, `DELETE` or `TRUNCATE` on an append-only table, or `DELETE`/`TRUNCATE` on a head or account table |
| `OX002` | `CodeLedgerAccount` | an account is inserted with non-zero totals, or updated other than by a ledger trigger |
| `OX003` | `CodeDownRefused` | a down migration would drop a table holding rows |
| `OX004` | `CodeNotFenced` | at commit, a chain record was not written under the round's current execution head and fencing token |
| `OX005` | `CodeHeadRegression` | an admission or execution head moves backwards, or the owner changes without a new fencing token |
| `OX006` | `CodeOutboxImmutable` | an outbox update touches a non-delivery column, changes a published row, or deletes an unpublished row |
| `OX007` | `CodeEncoding` | migrating a database whose encoding is not UTF8 |
| `OX008` | `CodeRecordBinding` | chain record bytes disagree with its columns, its admission or its round |
| `OX009` | `CodeHoldUnreleased` | at commit, a command record's admission still holds credits without a release (§13) |
| `OX010` | `CodeSecretReveal` | a reveal row names a chain record that is not an accepted command record of its round (§13, LBT-21 K-15) |

Named constraints that callers and tests rely on (standard SQLSTATEs):

| Constraint | SQLSTATE | Meaning |
|---|---|---|
| `admissions_command_id_key` | 23505 | duplicate command ID: look up the original admission |
| `admissions_prev_fkey` | 23503 | admission sequence gap |
| `round_admission_heads_head_fkey` | 23503 (commit) | admission head ahead of the admissions |
| `round_execution_heads_head_fkey` | 23503 (commit) | execution head not at an existing record; a round created without its genesis record |
| `round_execution_heads_owner_check` | 23514 | owner and lease must be both set or both null; owner is 1–128 printable ASCII |
| `chain_records_hash_check` | 23514 | link hash does not match the bytes |
| `chain_records_prev_fkey` | 23503 | record does not extend the previous record's hash |
| `chain_records_admission_fkey` | 23503 | command record without its admission |
| `chain_records_genesis_check` | 23514 | genesis with a non-zero previous hash |
| `chain_records_result_check` | 23514 | result columns of the wrong shape |
| `chain_records_record_check` | 23514 | record over 64 MiB or containing a newline |
| `domain_events_prev_fkey`, `domain_events_record_fkey` | 23503 | event gap; event of an unknown record |
| `round_snapshots_record_fkey` | 23503 | snapshot of an unknown record |
| `ledger_accounts_floor_check` | 23514 | insufficient available credits (a business outcome in A; a bug in B, §13) |
| `ledger_accounts_kind_check`, `ledger_accounts_player_id_key`, `ledger_accounts_system_name_key` | 23514, 23505 | account shape and uniqueness (a partial unique index for system names) |
| `ledger_transactions_record_key` | 23505 | a second ledger transaction for one chain record (duplicate payout) |
| `ledger_postings_event_key` | 23505 | a second posting for one chained event (duplicate payout, §13) |
| `ledger_postings_amount_check`, `ledger_postings_accounts_check` | 23514 | non-positive amount; same account on both sides |
| `ledger_postings_round_fkey`, `ledger_postings_event_fkey` | 23503 | posting round differs from its transaction's; event of another round |
| `ledger_hold_releases_record_fkey` | 23503 | release before the admission's chain record exists |
| `outbox_messages_dedupe_key` | 23505 | duplicate outbox message |
| `round_secrets_sealed_check` | 23514 | a plaintext `config_secret` in a row written from `0010` on (`NOT VALID`: legacy rows are not checked) |
| `round_secrets_form_check` | 23514 | not exactly one of `config_secret` and `secret_ciphertext` |
| `round_secrets_sealed_shape_check` | 23514 | sealed columns not all NULL or not all set; scheme other than `aes-256-gcm/v1`; key ID outside `^[a-z0-9][a-z0-9._-]{0,63}$`; nonce not 12 bytes; ciphertext under 18 bytes |
| `round_secret_reveals_pkey` | 23505 | a second reveal for one round |
| `round_secret_reveals_secret_fkey`, `round_secret_reveals_record_fkey` | 23503 | a reveal without its `round_secrets` row; a reveal of an unknown chain record |
| `round_secret_reveals_sequence_check` | 23514 | a reveal at sequence 0 (genesis) |

Order of checks on insert: `BEFORE` triggers (the binding trigger) first, then `CHECK` constraints in
name order, then unique indexes, then foreign keys, then deferred checks at commit. The binding trigger
leaves missing or malformed inputs (no round, no admission, a malformed result) to the constraints, so
those report their own SQLSTATE.

## 4. Transaction isolation and locking

| Transaction | Go options | Concurrency control |
|---|---|---|
| A: admission (LBT-11) | `AdmissionTxOptions()`: READ COMMITTED, read-write | `orvex.admit_command` (§12): lock-free dedupe and window read, then `round_admission_heads` `FOR UPDATE`; then `clock_timestamp()`, dedupe, window, holds, admission insert |
| B: execution (LBT-12) | `ExecutionTxOptionsWithTimeouts(lock, idle)`: READ COMMITTED, read-write, transaction-local timeouts in the `BEGIN` | the compare-and-set `UPDATE round_execution_heads … WHERE processed_sequence = k-1 AND fencing_token = t` is the first statement that touches data (0 rows means a conflict); then one batch of inserts; deferred `OX004`, `OX009` and head foreign keys at commit; account floors at each statement (§13) |
| Round creation (LBT-12) | `ExecutionTxOptionsWithTimeouts(lock, idle)` | inserts only; deferred foreign keys require the genesis record |
| Ownership takeover (LBT-14) | `ExecutionTxOptions()` | conditional `UPDATE` of the execution head |
| Export and multi-statement reads | `SnapshotReadTxOptions()`: REPEATABLE READ, read-only | one consistent snapshot |
| Migrations | server default, READ COMMITTED | advisory transaction lock, one file per transaction |

Rules (spec §6):

1. Every transaction states its isolation explicitly with the options functions.
2. Lock order: the round's head row first (`round_admission_heads` in A, `round_execution_heads` in B
   and takeovers), then ledger accounts through holds and postings. Never lock an execution head inside
   A or an admission head inside B. Admissions and chain records are immutable and need no locks.
3. Touch accounts in ascending `account_id` where feasible. The posting trigger updates the lower
   `account_id` first. Ledger totals are updated relatively (`held = held + amount`), so a blocked update
   re-evaluates the floor check on the newest row version.
4. Error classes:
   - `40001`, `40P01` (`IsRetryable`): retry the whole transaction from authoritative state;
   - commit-time `OX004`, or `23503` on `round_execution_heads_head_fkey`, in B: an ownership or
     version conflict; discard the computed transition and reload;
   - `23505` on `admissions_command_id_key` in A: a duplicate; return the original admission;
   - `23514` on `ledger_accounts_floor_check`: insufficient available credits;
   - any other `23xxx` or `OX` code: a bug; fail closed and do not retry.
5. Effective time in A: after the lock, `orvex.unix_micro(clock_timestamp())`
   (`orvex.unix_micro`, clamped to the previous admission's time, §12). Never `now()`, which is the
   transaction start time (ADR-002).
6. B commits only when its compare-and-set affected exactly one row. The deferred checks are a
   backstop, not the protocol.

Why READ COMMITTED: every invariant is enforced by a lock on one row, a key or foreign key, a `CHECK` on
the newest row version, or a commit-time check that takes its own lock. Under REPEATABLE READ or
SERIALIZABLE, a lock wait on a concurrently updated row ends in `40001`, so every contended admission
would retry.

## 5. Migration policy

- Files are `migrations/NNNN_name.up.sql` and `NNNN_name.down.sql`, contiguous from `0001`, each version
  with one name and both files. The first line is `-- NNNN name: <purpose> (LBT-n spec X-n)`, naming the
  spec decision: `(LBT-10 spec P-n)`, from `0007` `(LBT-11 spec S-n)`, from `0008` `(LBT-12 spec E-n)`,
  from `0010` `(LBT-21 spec K-n)`.
- A file must not be empty, hold a transaction statement on its own line (`BEGIN;`, `COMMIT;`,
  `ROLLBACK;`, `START TRANSACTION`), use `CONCURRENTLY` or start a line with a psql meta-command.
- Each step is one READ COMMITTED transaction: `pg_advisory_xact_lock(7766444933939532367)` (the first
  8 bytes of SHA-256(`orvex/migrate`)), create the bookkeeping if absent, validate it, run one file,
  write or delete the bookkeeping row, commit. A failed file leaves nothing behind.
- `orvex_migrate.schema_migrations` records the SHA-256 of both files of each applied migration. The
  runner refuses a database whose applied files differ from the build's (`ErrChecksumMismatch`), whose
  schema is newer (`ErrSchemaTooNew`) or whose bookkeeping is inconsistent (`ErrSchemaState`).
- **Merged migrations are immutable.** Change the schema with a new migration. `.gitattributes` pins
  `/migrations/*.sql` to LF so a checkout cannot change the checksums.
- Every migration has an exact down. A down refuses with `OX003` when a table it drops holds rows, so on
  a database with audit data it fails and changes nothing. Downs are for development and tests.
- `crashctl migrate up [-to N] | down -to N | status` runs the runner against `ORVEX_DATABASE_URL`.
  Exit codes: 0 done; 1 a migration failed (SQL error, `OX003`, `OX007`); 2 usage or a target on the
  wrong side of the current version; 3 schema newer than the build, checksum mismatch or inconsistent
  bookkeeping; 4 cannot connect or write.

## 6. Round secrets at rest

The normative design is [the LBT-21 spec](../specs/87d8d90c_round-secret-protection.md) (decisions K-1
to K-15); it supersedes P-25's "stored unencrypted" for every round created from `0010` on.

- **Sealed (K-1, K-5):** the codec's secret config (for crash/v1 `{"seed":"<hex>"}`) of every round
  created after `0010` is stored only sealed: AES-256-GCM in the engine, with a random 96-bit nonce and
  the additional authenticated data `orvex/round-secret/v1`, 0x00, the scheme, 0x00, the key ID, 0x00,
  the 16 round ID bytes, in `round_secrets.secret_*`. PostgreSQL never sees a key; no SQL statement
  carries a key or an unsettled plaintext secret. It is never inside a chain record. A database copy or
  a non-engine role yields an unsettled round's seed only together with the keyring (§16).
- **Revealed (K-6):** the record that finalizes the round (crash/v1: the accepted `SettleRound`, whose
  `RoundSettled` event already carries the seed) writes the plaintext to `round_secret_reveals` in the
  same Transaction B (§13). From then on the secret is public (ADR-004), and auditors read it without a
  key.
- **Not sealed, residual risk R-1 (accepted, tracked as LBT-26):** `round_secrets.close_offset_micros`
  (the crash/v1 crash deadline offset `D`) and `round_admission_heads.close_not_before_unix_micro` (the
  closure bound `T0 + D`, from `0009`) stay plaintext, because `orvex.admit_command` reads them inside
  PostgreSQL. Only grants protect them: only the owner of `orvex.admit_command` reads them (§12), and no
  admission result, error, outbox message or export carries them. A database copy still reveals the
  crash tick of a round that has not crashed.
- **Legacy rows, R-2:** rows written before `0010` keep their plaintext `config_secret`; no `UPDATE`
  touches them. The `NOT VALID` `round_secrets_sealed_check` stops new plaintext from every role; never
  `VALIDATE` it while legacy rows exist. Migration `0010` backfills the reveal of a legacy round whose
  admission window is sealed at an accepted record.
- **Snapshots, R-5:** `round_snapshots.state` stays NULL (E-13). It would hold the seed if ever written;
  any future writer must seal it.

Credits are simulated (ADR-004).

## 7. Retention

Every row of every append-only table is kept indefinitely; no deletion path exists (P-26). Published
outbox rows may be deleted; when is an operations policy (LBT-14). Any future archival must first export
and verify complete histories and needs an ADR-001-level decision.

## 8. Grant matrix (recommended, LBT-14)

Migrations create no roles and grant nothing, with one exception: `0009` revokes `PUBLIC`'s default
`EXECUTE` on the `SECURITY DEFINER` function `orvex.admit_command` (§12), so a role that LBT-14 gives
`USAGE` on `orvex` cannot admit commands with the owner's rights. LBT-14 owns roles and grants.
Recommended matrix (P-13, with the round secret grants of LBT-21 K-14, which supersede P-13's and
LBT-19 X-11's for `round_secrets`):

| Role | `round_secrets` | `round_secret_reveals` | Keyring |
|---|---|---|---|
| owner of `orvex.admit_command` | owner (reads `close_offset_micros`) | — | no |
| engine | `SELECT (round_id, config_secret, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext)` and `INSERT` | `SELECT`, `INSERT` | yes |
| gateway (admitting) | nothing | nothing | no |
| `orvex_auditor` | nothing | `SELECT` | no |
| `orvex_secret_reader` (`NOLOGIN`, break-glass) | `SELECT` on the same six columns as the engine | (via `orvex_auditor`) | operator's copy, only while in use |

For the other tables: the engine has `SELECT` and `INSERT` on the append-only tables and `SELECT`,
`INSERT`, `UPDATE` on the heads, `ledger_accounts` and `outbox_messages` (§13); the gateway has what
admission needs (§12); the auditor has what the export needs (§14). Nobody but the engine reads
`round_snapshots.state`. No role but the owner of `orvex.admit_command` reads
`round_secrets.close_offset_micros` or `round_admission_heads.close_not_before_unix_micro` (§6).

The auditor role that runs `crashctl audit export` and the grants it needs are in §14.

Triggers enforce immutability for every role except table owners and superusers, independent of grants.

## 9. ADR-001 limitation

Triggers, the hash `CHECK` and the binding checks protect against application bugs and non-owner roles.
They do not protect against the table owner or a superuser, who can disable triggers or rewrite rows
with recomputed hashes. ADR-001's separately trusted anchor (for example an externally held head hash,
`crashctl audit verify -expect-head`) remains future work.

## 10. TRUNCATE

Every table in `orvex` has a statement-level `<table>_no_truncate` trigger raising `OX001`. PostgreSQL
checks foreign keys before `BEFORE TRUNCATE` triggers run: a plain `TRUNCATE` of a table that another
table references fails with `0A000` ("cannot truncate a table referenced in a foreign key constraint")
before any trigger fires. `TRUNCATE … CASCADE` reaches the triggers and fails with `OX001`. Either way
nothing is removed. `DROP TABLE` does not fire these triggers, so down migrations still work behind
their `OX003` guard.

## 11. Local use

```sh
docker compose -f deployments/docker/compose.yaml up -d --wait
export ORVEX_DATABASE_URL='postgres://orvex:orvex@127.0.0.1:55432/orvex?sslmode=disable'
make build-crashctl
./bin/crashctl migrate up
./bin/crashctl migrate status
./bin/crashctl migrate down -to 0
docker compose -f deployments/docker/compose.yaml down -v
```

The Compose service uses the same pinned image as the tests (`pgtest.Image`,
`postgres:18.6-trixie@sha256:7493…4336`). `make test-integration` starts its own Testcontainers
PostgreSQL per test package and needs a Docker-compatible daemon; without one it fails rather than
skips.

## 12. Admission (LBT-11)

Migration `0007_admission_window` and `internal/runtime/admission` implement Transaction A, the durable
per-round admission sequencer. The normative design is
[the LBT-11 spec](../specs/5a3d52fc_admission-sequencer.md) (decisions S-1 to S-14). Migration
`0009_close_bound` amends the closure guard (S-8) per the
[LBT-12 engineer addendum](../specs/e0df34d3_execution-commit-ledger-outbox.md#engineer-addendum-2026-10-15):
the bound is stored, not supplied by the caller.

### Window columns

`round_admission_heads` gains:

| Column | Meaning |
|---|---|
| `admission_state` | `open`, `started`, `closed` or `sealed`; default `open` (`round_admission_heads_state_check`) |
| `started_sequence` | admission sequence of the admitted system `StartRound` |
| `closed_sequence` | admission sequence of the admitted system `CloseRound`, the closure barrier |
| `sealed_sequence` | admission sequence of the admitted system `SettleRound`, the round's last admission |
| `close_not_before_unix_micro` | from `0009`: the secret closure bound `T0 + D`, set once when the `start` command is admitted (`T0` its effective time, `D` the round's `round_secrets.close_offset_micros`) |

- `round_admission_heads_window_check`: `open` has no sequence; `started` has only `started_sequence`;
  `closed` adds `closed_sequence > started_sequence`; `sealed` has all three increasing and
  `sealed_sequence = admitted_sequence`; every set sequence lies in `[1, admitted_sequence]`.
- `round_admission_heads_started_fkey`, `…_closed_fkey`, `…_sealed_fkey`: `NOT DEFERRABLE` foreign keys
  to `admissions (round_id, sequence)`.
- Trigger `round_admission_heads_window` (`orvex.check_admission_window()`) raises `OX005` when the state
  rank decreases or a set sequence changes. With the window check this makes a sealed round final.
- `round_admission_heads_close_bound_check`: a bound only once `started_sequence` is set. Trigger
  `round_admission_heads_close_bound` (`orvex.check_close_bound()`) raises `OX005` when a set bound
  changes; its message never formats the bound.

Transitions under the head lock (S-8), by the class the rules version's policy assigns:

| State | `ordinary` | `start` | `close` | `settle` |
|---|---|---|---|---|
| `open` | admit | admit → `started` | `lifecycle_order` | `lifecycle_order` |
| `started` | admit | `lifecycle_order` | admit → `closed` if effective ≥ `close_not_before_unix_micro` (recorded when `start` was admitted), else `not_due`; a missing bound is `corrupt` | `lifecycle_order` |
| `closed` | `round_closed` | `lifecycle_order` | `lifecycle_order` | admit → `sealed` |
| `sealed` | `round_sealed` | `round_sealed` | `round_sealed` | `round_sealed` |

For crash/v1, system `StartRound`, `CloseRound` and `SettleRound` are `start`, `close` and `settle`;
everything else is `ordinary`. A player `PlaceBet` with a non-empty player and an amount in
`[1, 1,000,000,000]` holds its amount. A `start` additionally fails closed (`corrupt`) when the round
has no `close_offset_micros` (a round created before `0009`).

### `orvex.admit_command`

Transaction A is one `AdmissionTxOptions()` transaction that calls `orvex.admit_command` once, then
commits only on `admitted` and rolls back otherwise. From `0009` the function is `SECURITY DEFINER`
with `SET search_path = pg_catalog` and every relation schema-qualified, and `PUBLIC`'s default
`EXECUTE` on it is revoked. It takes 10 arguments (the caller's not-before bound is gone). In order:

1. sets `lock_timeout` and `idle_in_transaction_session_timeout` transaction-locally, and
   `synchronous_commit = on` if it is `off`, so an acknowledged `COMMIT` is flushed;
2. fast path, without locks: an existing command ID returns `duplicate` (same round and bytes) or
   `conflict`; an unknown round `not_found`; a game or rules mismatch `rules_mismatch`; a sealed round
   `round_sealed`; an ordinary command in a closed round `round_closed`;
3. locks the round's head `FOR UPDATE`;
4. reads the clock once: `orvex.unix_micro(clock_timestamp())`, the first statement after the lock;
5. clamps it to at least the effective time of admission `admitted_sequence`, so effective times are
   non-decreasing in sequence order even if the clock steps back;
6. repeats the dedupe on a fresh snapshot, then applies the window table;
7. for `start`: reads `round_secrets.close_offset_micros` (missing: `corrupt`) and computes the bound
   `T0 + D` (saturating); for `close`: `not_due` if the effective time is before the head's bound
   (equality admits; a missing bound is `corrupt`). Neither the offset nor the bound appears in any
   outcome or message;
8. for a hold: locks the player's account `FOR NO KEY UPDATE`; no account or too few available credits
   is `insufficient_credits`;
9. inserts the admission, then the hold (the hold trigger adds it to `held`), then advances the head
   and the window and, for `start`, the bound.

It never catches exceptions. Argument errors raise `22023`. `orvex.unix_micro` is the only conversion
of a timestamp into an effective time. Rejections at admission write nothing and are not stored (S-13).

### Locks and timeouts

- Lock order inside A: the round's admission head, then at most one ledger account. A never locks an
  execution head.
- `lock_timeout` (default 3 s) bounds the head wait, the account wait and the unique-index wait of a
  cross-round duplicate; expiry is `55P03`.
- `idle_in_transaction_session_timeout` (default 1 s) ends a session that called the function and then
  went idle, releasing the head.
- The sequencer lets at most 4 calls per round hold a pool connection at once (`MaxInFlightPerRound`).

### Outcomes and errors

| Outcome or error | Go result |
|---|---|
| `admitted` | `COMMIT`, then the `Admission` |
| `duplicate` | the original `Admission`, `Duplicate = true` |
| `conflict`, or `23505` on `admissions_command_id_key` resolving to another round or other bytes | `ErrCommandIDReused` |
| `rejected`, or `23514` on `ledger_accounts_floor_check` | `*RejectedError` with the reason |
| `not_due` | `*ClosureNotDueError` (carries only the too-early effective time) |
| `rules_mismatch` | `ErrInvalidRequest` |
| `not_found` | `ErrRoundNotFound` |
| `55P03` | `ErrBusy`, no retry |
| `40001`, `40P01`, connection loss, classes `08`, `53`, `57`, `25P03` before `COMMIT` | retry |
| ambiguous `COMMIT` failure (connection loss, `40003`, classes `08`, `53`, `57`, `58`, `XX`) | retry with the same command ID, else `ErrOutcomeUnknown` |
| `COMMIT` failure of class `23` or `OX`, `40002` | `ErrInternal` |
| any other SQLSTATE, `corrupt` or an unknown outcome | `ErrInternal` |

An `Admit` call makes at most 3 attempts (`MaxAttempts`) with full-jitter backoff (5 ms doubling, cap
50 ms). After the last attempt it returns `ErrOutcomeUnknown` if the last failure was an ambiguous
`COMMIT`, otherwise `ErrUnavailable`. On `ErrOutcomeUnknown`, `ErrUnavailable`, `ErrBusy` or a context
error, callers resubmit the same request with the same command ID. `Lookup` reads an admission and its
`chain_records` result in one `SnapshotReadTxOptions()` transaction.

### Grants the admitting role needs (input to LBT-14)

With `orvex.admit_command` `SECURITY DEFINER` (`0009`), the admitting role needs only:

- `USAGE` on schema `orvex`;
- `EXECUTE` on `orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, integer, integer)`;
- `SELECT` on `admissions` and `chain_records`, for `Lookup` and the `23505` resolve.

It must have nothing on `round_secrets` and no `SELECT` on `round_admission_heads`, which hold the
secret close offset and closure bound; nothing on `round_snapshots` or `round_secret_reveals` either. `TestAdmittingRoleCannotReadCloseBound`
(`internal/runtime/admission`) creates a role with exactly these grants and checks it.

## 13. Execution (LBT-12)

Migrations `0008_execution_ledger` and `0009_close_bound`, `internal/runtime/execution` and
`internal/ledger` implement Transaction B, the simulated-credit ledger, the outbox and production round
creation. The normative design is [the LBT-12 spec](../specs/e0df34d3_execution-commit-ledger-outbox.md)
(decisions E-1 to E-17 and the engineer addendum), built per
[the implementation plan](../specs/6f13b30a_execution-ledger-implementation.md).

### Migration `0008_execution_ledger`

- `ledger_postings_event_key`: a unique partial index on `ledger_postings (round_id, event_sequence)
  WHERE event_sequence IS NOT NULL`, replacing the non-unique `ledger_postings_event_idx`. At most one
  posting per chained event, so a bet settles at most once (`23505`).
- `chain_records_holds_released`: a deferred constraint trigger (`orvex.check_record_holds_released()`)
  that raises `OX009` at commit when a command record's admission still has a hold without a release.
  At commit the deferred triggers fire in name order: `chain_records_fenced` (`OX004`) first.
- The down drops both and recreates `ledger_postings_event_idx`; it drops no table and refuses nothing.
  `0009`'s down refuses with `OX003` while `round_admission_heads` or `round_secrets` holds rows;
  `0010`'s down refuses first while `round_secret_reveals` holds rows or `round_secrets` holds a sealed
  row.

### Transaction B

`Executor.ExecuteNext(ctx, Lease{RoundID, FencingToken})` executes exactly admission
`k = processed_sequence + 1`:

1. Before B, one statement without locks reads admission `k` joined with its holds and their accounts'
   players. Decide and Evolve run outside any transaction against the round's cached state (a replay of
   the chain, below); the LBT-9 command record is built from the admission's exact bytes.
2. `BeginTx(ExecutionTxOptionsWithTimeouts(LockTimeout, IdleInTransactionTimeout))`: the `BEGIN` sets
   `lock_timeout`, `idle_in_transaction_session_timeout` and, only when it is `off`,
   `synchronous_commit = on`, transaction-locally. It reads no table.
3. The compare-and-set alone: `UPDATE orvex.round_execution_heads SET processed_sequence = k WHERE
   round_id = $1 AND processed_sequence = k - 1 AND fencing_token = $t`. Not exactly one row: roll back
   and run the conflict check.
4. One pipelined batch, in this order: the chain record (`prev_hash` = the cached head, fencing token
   `t`); `domain_events` in one `unnest` insert; `ledger_hold_releases` for every hold of the admission,
   ascending `account_id`, before any posting; the ledger transaction and its postings when there are
   postings; one `outbox_messages` row; a `round_snapshots` row when the record is a checkpoint; last,
   on the record that first makes `Policy.Final` true (final after the record, not before it), the
   `round_secret_reveals` row with the round's plaintext secret config (LBT-21 K-6). The reveal commits
   or rolls back with the record, so an adopted record already carries its reveal.
5. `COMMIT`; the deferred `OX004`, `OX009` and head foreign keys run.

Lock order: the round's execution head, then at most one shared account row (one player, or `house`
at settlement) plus the round's own `round_pool`. B never raises a player's `debits_posted + held`: a
`BetPlaced` releases its hold (`held -= b`) and then debits it (`debits_posted += b`) on the same row
under one lock, so a floor violation in B is a bug (`ErrInternal`). Admission in another round waits on
that row lock and then sees the converted or released credits (ADR-005 cross-round balances).

**Conflict check** (on a fresh connection): `SELECT h.processed_sequence, h.fencing_token, r.hash FROM
round_execution_heads h LEFT JOIN chain_records r ON (round_id, sequence = k)`. No head: `ErrCorrupt`;
another token: `ErrFenced` (stop driving the round); record `k` committed with the computed hash: adopt
it (`Outcome.Adopted`); `processed = k - 1`: retry; otherwise reload the cache.

**State.** The authoritative state is the chain. The executor caches, per round, the state replayed from
genesis through every record (`replay.StateOnly`), checking every snapshot's `state_hash` on the way
(the cold load, one `SnapshotReadTxOptions()` transaction). The compare-and-set and
`chain_records_prev_fkey` prove the cache current at commit; any conflict drops it. The cold load takes
the secret config from the round's reveal, else the legacy plaintext `config_secret`, else it opens the
sealed form with `Config.Keyring` (LBT-21 K-8). An unknown scheme, a key ID missing from the keyring or
a failed authentication is `ErrCorrupt` wrapping `sealing.ErrScheme`, `sealing.ErrUnknownKey` or
`sealing.ErrOpen`, and halts the round; no error text holds the plaintext, a key, the nonce or the
ciphertext. The entry keeps the plaintext secret next to the state.

### Accounts and postings (crash/v1)

| Account | Row | IDs (`internal/ledger`, UUIDv8 of SHA-256) |
|---|---|---|
| player | existing `kind = 'player'` account, floor enforced | random, created by operations |
| `round_pool` | system, `round_id` set, no floor; created with the round | `uuid8("orvex/ledger/v1/account:<round>/round_pool")` |
| `house` | system, global, no floor; created by the first round (`ON CONFLICT DO NOTHING`) | `uuid8("orvex/ledger/v1/account:global/house")` |

| Event | Posting | Ledger transaction kind |
|---|---|---|
| `BetPlaced{P, b}` | player `P` → `round_pool`, `b`, consuming the admission's hold | `crash_bet` |
| `BetCashedOut{P, payout}` | `round_pool` → player `P`, `payout` | `crash_cash_out` |
| `RoundSettled{B, W}` | `B > W`: `round_pool` → `house`, `B − W`; `B < W`: `house` → `round_pool`, `W − B` | `crash_settlement` |
| `RoundStarted`, `RoundCrashed`, `BetLost` | none | |

Each record with postings has exactly one ledger transaction, `uuid8("orvex/ledger/v1/transaction:<round>/<k>")`.
Each posting carries its event's `event_sequence`. After settlement the round account is 0.

### Outbox and snapshots

- One `outbox_messages` row per chain record, genesis included: key `(round_id, sequence, 0)`, topic
  `round.record.v1`, payload the exact record bytes. No payload carries a secret before it is due.
- Snapshots (`state` NULL, `engine_build` the executor's build): sequence 0 at creation, then every
  record that emits `RoundStarted`, `RoundCrashed` or `RoundSettled`.

### Round creation

`Executor.CreateRound` writes, in one `ExecutionTxOptionsWithTimeouts` transaction: `rounds`,
`round_secrets` (the secret config sealed with `Config.Keyring`, which `New` requires, `config_secret`
NULL, and, per the addendum, `close_offset_micros = D`), both heads with
their defaults, the genesis record (token 0), snapshot 0, the `round_pool` account, the `house` account
if absent and the genesis outbox message. `23505` on `rounds_pkey` with equal genesis bytes is
`Created.Duplicate`; different bytes are `ErrRoundExists`.

### Errors

| Condition | Go result |
|---|---|
| invalid lease, Drain limit < 1 | `ErrInvalidRequest`, no database call |
| no round; head token ≠ lease | `ErrRoundNotFound`; `ErrFenced` |
| no policy, descriptor mismatch | `ErrUnknownRules`, round halted |
| decode, replay or snapshot mismatch; missing hold or player account; a sealed secret that cannot be opened | `ErrCorrupt`, round halted |
| Decide, Evolve, codec or posting rule fails | `*FailClosedError` (`ErrFailClosed`), nothing written, round halted until `Forget` |
| `40001`, `40P01`, connection loss, classes `08`, `53`, `57`, `25P03` before `COMMIT` | retry |
| `55P03` | `ErrBusy` |
| compare-and-set 0 rows, `23503` on `chain_records_prev_fkey`, commit-time `OX004` or `23503` on `round_execution_heads_head_fkey` | conflict check |
| any other `23xxx`, `OX` (including `OX010` on the reveal), `22xxx`, `42xxx`, `XX000` in the batch; `OX009`, class `23`/`OX`, `40002` at `COMMIT` | `ErrInternal`, cache dropped |
| ambiguous `COMMIT` | the next attempt starts with the conflict check |
| attempts exhausted (default 3, full-jitter backoff 5 ms doubling, cap 50 ms) | `ErrUnavailable`; calling again is safe |
| `ctx` ended (including `57014` while `ctx` is done) | an error wrapping `ctx.Err()` and the driver's error; the cache is kept |

### Grants the engine role needs (input to LBT-14)

Per LBT-21 K-14: `SELECT` on `rounds`, `round_secrets (round_id, config_secret, secret_scheme,
secret_key_id, secret_nonce, secret_ciphertext)` (column-level, not `close_offset_micros`),
`round_secret_reveals`, `admissions`, `ledger_holds`, `ledger_accounts`, `chain_records` (also read by
`orvex.check_secret_reveal()`, which runs with invoker rights), `round_snapshots`,
`round_execution_heads`; `INSERT` on `rounds`, `round_secrets`, `round_secret_reveals`,
`round_admission_heads`, `round_execution_heads`, `chain_records`, `domain_events`, `round_snapshots`,
`ledger_accounts`, `ledger_transactions`, `ledger_postings`, `ledger_hold_releases`, `outbox_messages`;
`UPDATE` on `round_execution_heads` and `ledger_accounts` (through the ledger triggers). The engine also
holds the keyring (§16).

## 14. History export CLI (LBT-19)

`crashctl audit export` writes one round's audit history file from the database named by
`ORVEX_DATABASE_URL`. The normative design is [the LBT-19 spec](../specs/73dbe98d_audit-export-cli.md)
(decisions X-1 to X-13).

```
crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE
```

- `ROUND_ID` is canonical lowercase UUID text, never the nil UUID. `FILE` `-` writes the history to
  standard output and the report to standard error. `-format` accepts only `orvex/audit-history/v1`,
  its default; a well-formed later version is exit 3.
- `ORVEX_DATABASE_URL` is read as `crashctl migrate` reads it: a 10 s connect timeout unless the DSN
  sets one. crashctl never prints any part of it (LBT-21 K-12): it prints no driver, network or server
  text, only SQLSTATE codes, constraint names and the messages of class-`OX` errors, which Orvex
  migrations author. A connect failure prints `cannot connect to the database named by
  ORVEX_DATABASE_URL`, plus `: SQLSTATE <code>` or `: timeout`; any other driver or network error
  prints `database driver error`. A final pass replaces any remaining raw DSN or password with `xxxxx`.

### Reading

- The export uses one connection and exactly one transaction, `ExportHistory`'s
  `SnapshotReadTxOptions()` (REPEATABLE READ, READ ONLY, always rolled back). The CLI issues no
  statement of its own and takes `AccessShare` locks only.
- Session parameters: `default_transaction_read_only=on`, always, overriding the DSN;
  `application_name=crashctl` and `idle_in_transaction_session_timeout=60000` unless the DSN sets them.
- The secret config (LBT-21 K-9): the round's reveal, from a statement that never names
  `round_secrets`. Without a reveal the default export is exit 6 before any replay. With `-unsettled`
  it reads `round_secrets`, the privileged path: legacy plaintext as is, or the sealed form opened with
  the keyring named by `ORVEX_SECRET_KEYRING_FILE` (`eventstore.Options{Unrevealed, Keyring}`).
- The file holds records `0..S`, where `S` is the round's newest snapshot (P-24). The report line
  `records <S+1> of <N+1> head <hex64>` gives `S` and the round's newest record `N` from the same
  snapshot. When `S < N` a `note:` line names the sequences left out. In production LBT-11 seals
  admission at `SettleRound`, so a settled round's export is complete.

### Settlement gate

Every v1 file carries `configSecret`, the seed. By default the CLI writes a file only if the round is
settled at `S`: it replays the exported bytes (`verification.Verify` in replay mode, which also checks
the seed against the commitment) and asks the round's execution policy whether the replayed state is
final (`Policy.Final`, crash/v1: `PhaseSettled`). The file's own chain then already reveals the secret
it holds (ADR-003, ADR-004). Otherwise nothing is written and the exit code is 6 (not settled), 3 (an
unsupported rules version or no policy) or 1 (the replay failed).

`-unsettled` writes the file whatever the check says, and the report then says `settled no` or
`settled unknown` with a warning. Such a file can disclose the crash point of a running round: keep it
private (R-8). For a round whose secret is not revealed, `-unsettled` needs the privileged path (LBT-21
K-10): `SELECT` on `orvex.round_secrets` (role `orvex_secret_reader`) and, for a sealed row, the keyring.
crashctl reads `ORVEX_SECRET_KEYRING_FILE` only with `-unsettled`, after the argument and `FILE` checks
and before connecting; an unset or empty variable means no keyring. A revealed round needs neither.

| Invocation | Round | Result |
|---|---|---|
| default | revealed, settled at `S` | exit 0 |
| default | revealed, not settled at `S` | exit 6, the not-settled message |
| default | not revealed | exit 6, `round <id> has no revealed secret config: it is not settled; -unsettled exports it through the privileged path` |
| `-unsettled` | revealed | exit 0, no key needed |
| `-unsettled` | not revealed, legacy plaintext | exit 0 with the privileged role; exit 4 (`42501`) without it |
| `-unsettled` | not revealed, sealed | exit 0 with role and keyring; exit 4 (`42501`) without the role; exit 2 without a keyring or when the key ID is missing; exit 1 on a failed authentication |

### Output

- The history is built in memory, so the snapshot stays short and nothing reaches disk before the
  settlement check.
- `FILE` must not exist (a file, directory or symlink, dangling or not): the CLI never overwrites. It
  checks before connecting, creates `FILE` with `O_EXCL` and mode `0600`, then writes, syncs and closes
  it. Any failure after the open, including a failed report write, removes `FILE`. Exit 0 if and only
  if `FILE` holds the complete history.
- The report's `head` is the file header's head, in exactly the form `crashctl audit verify
  -expect-head` accepts. It comes from the same database as the file, so it becomes a trust anchor only
  once recorded elsewhere at export time (ADR-001 limitation, §9).

### Exit codes

| Code | Meaning |
|---|---|
| 0 | the history was written and the report printed |
| 1 | the stored round violates an export invariant (`ErrCorrupt`, `ErrNoCheckpoint`), a sealed secret fails authentication or has an unknown scheme, or the settlement check's replay failed or `Final` returned an error |
| 2 | usage: arguments, flags, `ROUND_ID`, a malformed `-format`, `ORVEX_DATABASE_URL` unset, empty or unparsable, `FILE` exists; a keyring problem: `ORVEX_SECRET_KEYRING_FILE` unusable (one line naming the rule, never the path or key material), a sealed unrevealed round without a keyring, or a key ID missing from the keyring |
| 3 | unsupported: `-format` names a version this build does not write; the history's format, chain, game, rules, schema or state version is unknown; no settlement policy for the rules version |
| 4 | I/O: `FILE`'s directory is not usable; cannot connect; any database error (`42501`, `42P01`, `40001`, connection loss, …); cannot write `FILE`, stdout or the report |
| 5 | round not found |
| 6 | round not settled at its newest snapshot, or with no revealed secret config, and `-unsettled` was not given |

Every failure prints nothing on stdout and one stderr line `crashctl: audit export: <message>`.

### Grants the auditor role needs (input to LBT-14)

Per LBT-21 K-10 and K-14 (superseding LBT-19 X-11):

```sql
CREATE ROLE orvex_auditor NOLOGIN;
GRANT CONNECT ON DATABASE orvex TO orvex_auditor;
GRANT USAGE ON SCHEMA orvex TO orvex_auditor;
GRANT SELECT ON orvex.rounds, orvex.chain_records TO orvex_auditor;
GRANT SELECT ON orvex.round_secret_reveals TO orvex_auditor;
GRANT SELECT (round_id, sequence, state_hash) ON orvex.round_snapshots TO orvex_auditor;
-- break-glass: the privileged path of -unsettled, together with the keyring
CREATE ROLE orvex_secret_reader NOLOGIN;
GRANT SELECT (round_id, config_secret, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext)
    ON orvex.round_secrets TO orvex_secret_reader;
-- login roles: CREATE ROLE <name> LOGIN IN ROLE orvex_auditor;
-- break-glass login roles: CREATE ROLE <name> LOGIN IN ROLE orvex_auditor, orvex_secret_reader;
```

- Nothing else: no `INSERT`, `UPDATE`, `DELETE` or `TRUNCATE`; nothing on `admissions`, heads, the
  ledger, the outbox, `domain_events` or `orvex_migrate`.
- The auditor has nothing on `round_secrets`, not even a single column: it reads only the secrets that
  settlement revealed. An unsettled round has nothing it can read. The secret reader alone yields only
  ciphertext; it never gets `round_secrets.close_offset_micros` (§6) or `round_snapshots.state`.
- `TestExportMinimalGrants` (`cmd/crashctl`) proves the set is sufficient and that `round_snapshots.state`,
  `admissions` and every `round_secrets` column are denied; `TestExportDatabaseErrors` shows that a
  missing grant is exit 4; `TestExportUnsettledNeedsPrivilegedPath` walks the privileged path.

## 15. Audit export v2 (LBT-18)

`eventstore.ExportHistoryV2` writes a round as an `orvex/audit-history/v2` file, the input of the
verifier's ledger stage. The normative design is [the LBT-18 spec](../specs/ac81f88f_audit-ledger-stage.md)
(decisions L-1 to L-14), built per [the implementation plan](../specs/f4d371a8_audit-ledger-implementation.md).
`ExportHistory` is unchanged and keeps writing format v1 with the same bytes.

### What it reads

In the same `SnapshotReadTxOptions` transaction as v1, it reads the secret config (from the same
sources and under the same `eventstore.Options` as v1, LBT-21 K-9), the snapshots, the end
`S` (the newest snapshot) and the head exactly as `ExportHistory` does, then the round's ledger rows with
`sequence <= S` (spec §5.4, §8):

| v2 field | Source (round `R`, sequence `j <= S`) |
|---|---|
| header `accounts` | `ledger_accounts` rows referenced by the rows below: `account_id`, `kind`, `player_id` (player), `name` and `round_id` (system; NULL is the nil UUID) |
| line `j` `holds` | `ledger_holds` with `(round_id, sequence) = (R, j)`: `account_id`, `amount` |
| line `j` `releases` | `ledger_hold_releases` with `(round_id, sequence) = (R, j)`: `account_id` |
| line `j` `transactions` | `ledger_transactions` with `(round_id, sequence) = (R, j)`: `transaction_id`, `kind` |
| `postings` | `ledger_postings` of each transaction: `posting_index`, `debit_account_id`, `credit_account_id`, `amount`, `event_sequence` (NULL is `"0"`) |

Only accounts that a hold, release or posting of the file references are exported. Rows are sorted in Go
(`bytes.Compare` on the 16-byte IDs; postings by `posting_index`), never by SQL order. The file is the v2
header, records `0..S` verbatim and one ledger line per record `0..S`.

### What it omits (L-2)

`created_at` columns (wall clock), the account totals `debits_posted`, `credits_posted`, `held` and
`floor_enforced` (cross-round facts), and `ledger_postings.round_id` (a posting belongs to its
transaction's round). A NULL `event_sequence` is exported as `"0"`.

### Failures

The exporter copies rows faithfully: it never filters, repairs or recomputes them, so degenerate rows
reach the verifier, which rejects them. A row it cannot encode is `ErrCorrupt`: an account kind other than
`player` or `system`, a player without a non-empty `player_id`, a system account without `name`, a
negative `posting_index` or `event_sequence`, a string `canonjson` rejects, or a referenced account that is
missing. `ErrRoundNotFound` and `ErrNoCheckpoint` are returned as for v1, before anything is written.

## 16. Round secret sealing (LBT-21)

The normative design is [the LBT-21 spec](../specs/87d8d90c_round-secret-protection.md) (decisions K-1
to K-15). `internal/persistence/sealing` seals and opens round secret configs; `execution.Config.Keyring`
and `eventstore.Options.Keyring` take its `*sealing.Keyring`.

### Keyring file (K-2)

A process reads its keyring from the file named by `ORVEX_SECRET_KEYRING_FILE`. The variable holds a
path, never key material. The file is ASCII with LF or CRLF line endings:

```text
# comments and blank lines are ignored
active k2026-10
key k2026-01 <base64>
key k2026-10 <base64>
```

- Fields are separated by spaces or tabs. Exactly one `active <id>` line and 1 to 64 `key <id>
  <base64>` lines. Key IDs are unique and match `^[a-z0-9][a-z0-9._-]{0,63}$`; the active ID must be
  listed. `<base64>` is padded standard base64 (RFC 4648 §4) of exactly 32 bytes. Any other line or
  field is an error naming the line and the rule, never the line's content or a key byte.
- `sealing.LoadKeyringFile` follows symlinks (Kubernetes Secret volumes are symlinks). The target must
  be a regular file of at most 64 KiB with `perm & 0o027 == 0`: `0400`, `0440`, `0600` and `0640`
  pass; the Kubernetes default `0644` fails. Mode and size are checked on the opened file.
- The keyring is read once at process start; changing it needs a restart. In memory it keeps only key
  IDs and AEADs, and prints as `sealing.Keyring{active: "<id>", keys: <n>}`.

Generation:

```sh
printf 'active k1\nkey k1 %s\n' "$(openssl rand -base64 32)" > keyring && chmod 0400 keyring
```

### Distribution (K-14)

- The engine always holds the keyring. An operator holds it only for a break-glass `crashctl audit
  export -unsettled` of an unrevealed round. The gateway, PostgreSQL and auditors never do; CI holds only
  the fixed `pgtest` test key.
- Kubernetes: one Secret, `orvex-secret-keyring`, mounted read-only into engine pods only, with
  `defaultMode: 0400` (or `0440` with `fsGroup`), never into gateway pods. Engine pods disable core
  dumps (R-7).
- Back the keyring up separately from database backups, never in the same artifact. LBT-14 owns the
  roles of §8, the distribution and the role-matrix test.

### Rotation (K-4)

Rows are never re-encrypted: `round_secrets` stays append-only, and a secret needs protection only
until its round settles, after which its reveal serves every reader without a key.

1. Add `key <new>` to every holder's keyring, keeping the same active key. Restart, rolling.
2. Switch every keyring to `active <new>`. Restart. New rounds are sealed with `<new>`.
3. Retire `<old>` once this query returns 0, or once the operator explicitly abandons every remaining
   such round:

   ```sql
   SELECT count(*) FROM orvex.round_secrets s
   WHERE s.secret_key_id = '<old>'
     AND NOT EXISTS (SELECT 1 FROM orvex.round_secret_reveals v WHERE v.round_id = s.round_id);
   ```

Steps 1 and 2 are separate restarts: during a rolling restart, an engine still on the old keyring may
cold-load a round sealed with the new key. Rotate at least yearly and after any suspected exposure.

**Compromise:** rotate at once (steps 1 and 2 together), and treat every unrevealed round sealed with
the compromised key as exposed; what happens to those rounds is an operations decision.

**Loss:** a round whose key is lost before its reveal can never be cold-loaded (`ErrCorrupt` wrapping
`sealing.ErrUnknownKey`) or exported with `-unsettled`. Settled rounds are not affected.

### Residual risks

- **R-1:** `D` and `T0 + D` stay plaintext (§6); a database copy, or the `admit_command` owner role,
  can learn the crash tick of a round that has not crashed. Accepted; tracked as LBT-26.
- **R-2:** rows written before `0010` keep their plaintext seed.
- **R-3:** whoever compromises an engine, or holds a keyring together with a database copy, reads every
  unsettled seed.
- **R-4:** losing a key makes the unsettled rounds sealed under it unrecoverable.
- **R-5:** `round_snapshots.state` would contain the seed if it were ever written.
- **R-6:** when the reveal is written depends on the engine's `Policy.Final`; PostgreSQL checks only
  that the reveal names an accepted record (`TestComputeTransitionReveal`).
- **R-7:** the plaintext secret and the keys live in engine memory, and Go cannot reliably zero them.
- **R-8:** a file written with `-unsettled` holds the seed.

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
| `round_secrets` | append-only | the codec's secret config bytes (the seed) | one row per round |
| `admissions` | append-only | admitted commands: sequence, command ID, effective time (µs), canonical command bytes | gapless from 1 by a self foreign key; `command_id` unique across all rounds and non-nil |
| `round_admission_heads` | mutable, undeletable | `admitted_sequence`, Transaction A's lock row | forward-only (`OX005`); deferred foreign key to the last admission |
| `chain_records` | append-only | exact LBT-9 record bytes, link hash, previous hash, result columns, fencing token | see below |
| `round_execution_heads` | mutable, undeletable | `processed_sequence`, `fencing_token`, `owner_id`, `lease_expires_at` | forward-only; an owner change needs a higher token (`OX005`); deferred foreign key to the last record |
| `domain_events` | append-only | `(round_id, event_sequence)` → emitting record, event type | contiguous from 1 by a self foreign key |
| `round_snapshots` | append-only | engine-computed state hash per chain record, optional state bytes | references its chain record |
| `ledger_accounts` | totals mutable by triggers only | player and system accounts, `debits_posted`, `credits_posted`, `held` | floor `debits_posted + held <= credits_posted` on player accounts |
| `ledger_transactions` | append-only | one per business operation | at most one per chain record |
| `ledger_postings` | append-only | one amount moved from a debit to a credit account | `amount > 0`; distinct accounts; balanced by construction |
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
  fencing token and a processed sequence covering the record.

Relationships: `rounds` 1–1 `round_secrets`, `round_admission_heads` and `round_execution_heads`;
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
  verbatim, in sequence order, up to the newest snapshot (P-24).

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
| `ledger_accounts_floor_check` | 23514 | insufficient available credits (a business outcome) |
| `ledger_accounts_kind_check`, `ledger_accounts_player_id_key`, `ledger_accounts_system_name_key` | 23514, 23505 | account shape and uniqueness (a partial unique index for system names) |
| `ledger_transactions_record_key` | 23505 | a second ledger transaction for one chain record (duplicate payout) |
| `ledger_postings_amount_check`, `ledger_postings_accounts_check` | 23514 | non-positive amount; same account on both sides |
| `ledger_postings_round_fkey`, `ledger_postings_event_fkey` | 23503 | posting round differs from its transaction's; event of another round |
| `ledger_hold_releases_record_fkey` | 23503 | release before the admission's chain record exists |
| `outbox_messages_dedupe_key` | 23505 | duplicate outbox message |

Order of checks on insert: `BEFORE` triggers (the binding trigger) first, then `CHECK` constraints in
name order, then unique indexes, then foreign keys, then deferred checks at commit. The binding trigger
leaves missing or malformed inputs (no round, no admission, a malformed result) to the constraints, so
those report their own SQLSTATE.

## 4. Transaction isolation and locking

| Transaction | Go options | Concurrency control |
|---|---|---|
| A: admission (LBT-11) | `AdmissionTxOptions()`: READ COMMITTED, read-write | first statement locks `round_admission_heads` for the round; then dedupe, window, `clock_timestamp()`, admission insert, holds |
| B: execution (LBT-12) | `ExecutionTxOptions()`: READ COMMITTED, read-write | first statement is the compare-and-set `UPDATE round_execution_heads … WHERE processed_sequence = k-1 AND fencing_token = t` (0 rows means a conflict); then inserts; deferred `OX004` and head foreign keys at commit; account floors at each statement |
| Round creation | `ExecutionTxOptions()` | inserts only; deferred foreign keys require the genesis record |
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
5. Effective time in A: after the lock, `(EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint`.
   Never `now()`, which is the transaction start time (ADR-002).
6. B commits only when its compare-and-set affected exactly one row. The deferred checks are a
   backstop, not the protocol.

Why READ COMMITTED: every invariant is enforced by a lock on one row, a key or foreign key, a `CHECK` on
the newest row version, or a commit-time check that takes its own lock. Under REPEATABLE READ or
SERIALIZABLE, a lock wait on a concurrently updated row ends in `40001`, so every contended admission
would retry.

## 5. Migration policy

- Files are `migrations/NNNN_name.up.sql` and `NNNN_name.down.sql`, contiguous from `0001`, each version
  with one name and both files. The first line is `-- NNNN name: <purpose> (LBT-10 spec P-n)`.
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

## 6. Seed exposure

`round_secrets.config_secret` stores the codec's secret config (for crash/v1 `{"seed":"<hex>"}`)
verbatim and **unencrypted** (P-25). It is never inside a chain record. Before settlement, anyone who can
read `round_secrets`, `round_snapshots.state` or a database backup can predict the round's crash point.
Credits are simulated (ADR-004), and application-level encryption with key management is deferred to
its own issue.

## 7. Retention

Every row of every append-only table is kept indefinitely; no deletion path exists (P-26). Published
outbox rows may be deleted; when is an operations policy (LBT-14). Any future archival must first export
and verify complete histories and needs an ADR-001-level decision.

## 8. Grant matrix (recommended, LBT-14)

Migrations create no roles and grant nothing. LBT-14 owns roles and grants. Recommended matrix (P-13):

| Role | Append-only tables | Heads, `ledger_accounts`, `outbox_messages` | `round_secrets`, `round_snapshots.state` |
|---|---|---|---|
| application | `SELECT`, `INSERT` only | `SELECT`, `INSERT`, `UPDATE` | readable (engine recovery, export) |
| gateway (if it admits commands) | as needed for admission | as needed for admission | no `SELECT` |

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

// Package postgres holds the PostgreSQL system of record's Go side (ADR-005;
// LBT-10, docs/specs/17380a6e_postgresql-schema-migrations.md): the
// migration runner, the schema's SQLSTATE constants and the transaction
// options. docs/architecture/persistence.md is the durable description of
// the schema; this comment summarizes it.
//
// # Schema
//
// The migrations in the repository's migrations/ directory create schema
// orvex:
//
//   - rounds, round_secrets: immutable round identity and the codec's secret
//     config (the seed), sealed with AES-256-GCM from 0010; legacy rows
//     written before 0010 keep their plaintext (LBT-21);
//   - round_secret_reveals: the secret config revealed by the record that
//     finalizes a round; it must name an accepted command record (OX010);
//   - round_admission_heads: Transaction A's per-round lock row and
//     admission counter;
//   - admissions: gapless per-round admitted commands with a globally unique
//     command ID and effective time;
//   - chain_records: the authoritative LBT-9 record bytes with link hashes,
//     linked by foreign key and bound to their columns (OX008); at commit
//     every credit hold of an executed admission must be released (OX009);
//   - round_execution_heads: Transaction B's compare-and-set row: processed
//     sequence, owner and fencing token (OX004, OX005);
//   - domain_events, view command_results: derived indexes bound to the
//     record bytes;
//   - round_snapshots: engine-computed state hashes, the export checkpoints;
//   - ledger_accounts, ledger_transactions, ledger_postings, ledger_holds,
//     ledger_hold_releases: two-sided postings with trigger-maintained totals
//     and a floor check; at most one posting per chained event
//     (ledger_postings_event_key);
//   - outbox_messages: the transactional outbox.
//
// Append-only tables reject UPDATE, DELETE and TRUNCATE with OX001.
//
// # Isolation (decision P-23)
//
//	Transaction                 Options                          Concurrency control
//	A: admission                AdmissionTxOptions               lock round_admission_heads first
//	B: execution                ExecutionTxOptionsWithTimeouts   compare-and-set round_execution_heads first
//	round creation (LBT-12)     ExecutionTxOptionsWithTimeouts   inserts only
//	takeover                    ExecutionTxOptions               conditional head update
//	export, multi-statement     SnapshotReadTxOptions            one consistent snapshot
//	migrations                  server default (READ COMMITTED)  advisory transaction lock
//
// ExecutionTxOptionsWithTimeouts sets lock_timeout and
// idle_in_transaction_session_timeout transaction-locally in its BEGIN and
// turns synchronous_commit on when it is off (LBT-12 spec E-6).
//
// # Error classes
//
//   - 40001, 40P01 (IsRetryable): retry the whole transaction.
//   - OX004, or 23503 on round_execution_heads_head_fkey, at commit in B: an
//     ownership or version conflict; reload.
//   - OX009 at commit in B: an executed admission keeps a credit hold; a bug.
//   - 23505 on admissions_command_id_key in A: a duplicate; look up the
//     original admission.
//   - 23514 on ledger_accounts_floor_check: insufficient available credits
//     in A; a bug in B, which never raises a player's debits plus holds.
//   - Any other 23xxx or OX code: a bug; fail closed and do not retry.
//
// Use SQLState to read a wrapped error's SQLSTATE and compare it with the
// Code constants.
package postgres

// Package admission is the durable per-round admission sequencer (ADR-002;
// ADR-005 Transaction A; LBT-11, docs/specs/5a3d52fc_admission-sequencer.md).
// docs/architecture/persistence.md section 12 describes the database side.
//
// Sequencer.Admit encodes a typed command with the round's codec, classifies
// it with the rules version's Policy, and runs one READ COMMITTED
// transaction (postgres.AdmissionTxOptions) that calls the PL/pgSQL
// function orvex.admit_command once and then commits or rolls back. An
// Admission is returned only after COMMIT succeeded, or for a command
// already admitted under the same command ID: it is a durable receipt, not
// an execution result. Lookup reads the execution result separately.
//
// In brief (decisions S-3 to S-8 of the spec):
//
//   - The function sets transaction-local lock_timeout and
//     idle_in_transaction_session_timeout, and turns synchronous_commit on
//     when it is off, so the acknowledged COMMIT is flushed.
//   - A lock-free fast path answers duplicates, unknown rounds and closed
//     or sealed windows; every decision is checked again under the lock.
//   - The round's orvex.round_admission_heads row, locked FOR UPDATE, is the
//     per-round serialization point; sequence order is lock grant order.
//     At most one player account row is locked after it.
//   - The effective time is the database clock read once right after the
//     lock, clamped to at least the previous admission's effective time.
//   - Command IDs are globally unique. The same round and bytes return the
//     original admission (Duplicate), anything else ErrCommandIDReused.
//   - The admission window moves open -> started -> closed -> sealed on
//     admitted system StartRound, CloseRound and SettleRound commands. A
//     CloseRound is admitted only at an effective time at or after the
//     bound T0 + D that admitting StartRound recorded from the round's
//     secret close offset (LBT-12 engineer addendum); a sealed round admits
//     nothing. orvex.admit_command is SECURITY DEFINER, so the admitting
//     role never reads the offset or the bound.
//
// Errors wrap exactly one sentinel (or a context error); ambiguous commits
// and connection loss are retried with the same command ID up to
// Config.MaxAttempts times (S-12). An in-process gate bounds the calls per
// round that hold a pool connection (S-4).
package admission

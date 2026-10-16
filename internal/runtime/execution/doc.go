// Package execution is the per-round command executor: ADR-005's
// Transaction B, atomic execution commit with the simulated-credit ledger
// and the transactional outbox, and production round creation (LBT-12,
// docs/specs/e0df34d3_execution-commit-ledger-outbox.md, decisions E-1 to
// E-17, with the engineer addendum; the implementation plan is
// docs/specs/6f13b30a_execution-ledger-implementation.md).
// docs/architecture/persistence.md section 13 describes the database side.
//
// In brief:
//
//   - The authoritative state of a round is its chain in PostgreSQL. The
//     Executor caches a replayed copy per round (E-3), loaded by a cold load
//     that replays genesis plus every chain record and checks every snapshot
//     hash on the way, and drops it on any conflict.
//   - ExecuteNext executes exactly the admission at processed_sequence + 1
//     (E-4, E-5): Decide and Evolve outside any transaction, the LBT-9
//     command record from the admission's exact bytes, postings from the
//     rules version's ledger.Rule, the admission's hold releases, the
//     snapshot when the policy asks for one.
//   - Transaction B (E-6) begins with transaction-local lock and idle
//     timeouts, runs the compare-and-set on the round's execution head
//     (processed_sequence = k - 1 under the lease's fencing token) alone,
//     then one pipelined batch: record, events, hold releases, ledger
//     transaction and postings, outbox message, snapshot; then COMMIT.
//   - A conflict (E-7) reads the head once: another fencing token is
//     ErrFenced; record k committed with the computed hash is adopted;
//     otherwise the transition is retried or the cache reloaded.
//   - A Decide that fails closed writes nothing and halts the round in this
//     process (*FailClosedError, E-14) until Forget.
//   - CreateRound (E-16) writes a round with its genesis record, snapshot 0,
//     round_pool account and genesis outbox message in one transaction, and
//     stores the policy's secret close offset that orvex.admit_command
//     reads (LBT-12 engineer addendum).
//   - CreateRound stores the secret config sealed with Config.Keyring
//     (AES-256-GCM, LBT-21 K-8; internal/persistence/sealing), never in
//     plaintext.
//   - The cold load reads the round's reveal, else the legacy plaintext,
//     else opens the sealed form with Config.Keyring; a form it cannot open
//     is ErrCorrupt. The record that first makes the round final writes the
//     reveal (round_secret_reveals) as the last statement of its
//     Transaction B (LBT-21 K-6).
//
// Errors wrap exactly one sentinel or a context error (E-15). Transient
// failures are retried up to Config.MaxAttempts times with full-jitter
// backoff; calling ExecuteNext again later is always safe.
package execution

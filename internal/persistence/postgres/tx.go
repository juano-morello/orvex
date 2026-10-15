package postgres

import (
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdmissionTxOptions returns the options of Transaction A, command
// admission: READ COMMITTED, read-write. Its first statement locks the
// round's admission head (section 6).
func AdmissionTxOptions() pgx.TxOptions {
	return pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}
}

// ExecutionTxOptions returns the options of Transaction B, execution
// commit, and of round creation and ownership takeover: READ COMMITTED,
// read-write. Its first statement is the compare-and-set update of the
// round's execution head (section 6).
func ExecutionTxOptions() pgx.TxOptions {
	return pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}
}

// ExecutionTxOptionsWithTimeouts returns Transaction B's options (LBT-12
// spec E-6): READ COMMITTED, read-write, with a BeginQuery that also sets
// lock_timeout and idle_in_transaction_session_timeout transaction-locally,
// in whole milliseconds, and turns synchronous_commit on when it is off.
// Both durations must be at least 1ms; shorter values are raised to 1ms.
// The BeginQuery reads no table and takes no lock, so the compare-and-set
// stays the first statement that touches data.
func ExecutionTxOptionsWithTimeouts(lock, idle time.Duration) pgx.TxOptions {
	return pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadWrite,
		BeginQuery: "BEGIN ISOLATION LEVEL READ COMMITTED READ WRITE; SELECT " +
			"set_config('lock_timeout', '" + wholeMillis(lock) + "ms', true), " +
			"set_config('idle_in_transaction_session_timeout', '" + wholeMillis(idle) + "ms', true), " +
			"set_config('synchronous_commit', CASE current_setting('synchronous_commit') " +
			"WHEN 'off' THEN 'on' ELSE current_setting('synchronous_commit') END, true)",
	}
}

// wholeMillis formats d in whole milliseconds, at least 1.
func wholeMillis(d time.Duration) string {
	ms := d.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return strconv.FormatInt(ms, 10)
}

// SnapshotReadTxOptions returns the options of exports and other
// multi-statement reads: REPEATABLE READ, read-only, one consistent snapshot.
func SnapshotReadTxOptions() pgx.TxOptions {
	return pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
}

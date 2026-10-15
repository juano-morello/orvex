package postgres

import "github.com/jackc/pgx/v5"

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

// SnapshotReadTxOptions returns the options of exports and other
// multi-statement reads: REPEATABLE READ, read-only, one consistent snapshot.
func SnapshotReadTxOptions() pgx.TxOptions {
	return pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
}

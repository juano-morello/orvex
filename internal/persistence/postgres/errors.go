package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Custom SQLSTATEs raised by the schema (class OX, decision P-11).
const (
	// CodeAppendOnly: UPDATE, DELETE or TRUNCATE on an append-only table,
	// or DELETE or TRUNCATE on a head or account table.
	CodeAppendOnly = "OX001"
	// CodeLedgerAccount: an account inserted with non-zero totals, or
	// updated other than by a ledger trigger.
	CodeLedgerAccount = "OX002"
	// CodeDownRefused: a down migration would drop a table holding rows.
	CodeDownRefused = "OX003"
	// CodeNotFenced: at commit, a chain record was not written under the
	// round's current execution head and fencing token.
	CodeNotFenced = "OX004"
	// CodeHeadRegression: an admission or execution head moves backwards,
	// or the owner changes without a new fencing token.
	CodeHeadRegression = "OX005"
	// CodeOutboxImmutable: an outbox update touches a non-delivery column,
	// changes a published row, or deletes an unpublished row.
	CodeOutboxImmutable = "OX006"
	// CodeEncoding: migrating a database whose encoding is not UTF8.
	CodeEncoding = "OX007"
	// CodeRecordBinding: chain record bytes disagree with the record's
	// columns, its admission or its round.
	CodeRecordBinding = "OX008"
	// CodeHoldUnreleased: at commit, a command record's admission still
	// holds credits (LBT-12 spec E-11).
	CodeHoldUnreleased = "OX009"
)

// Standard SQLSTATEs that IsRetryable accepts.
const (
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
)

// SQLState returns the SQLSTATE of the *pgconn.PgError wrapped by err, or ""
// if err wraps none.
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// IsRetryable reports whether err is a serialization failure (40001) or a
// deadlock (40P01): the only errors a transaction retries from
// authoritative state (decision P-23).
func IsRetryable(err error) bool {
	switch SQLState(err) {
	case codeSerializationFailure, codeDeadlockDetected:
		return true
	default:
		return false
	}
}

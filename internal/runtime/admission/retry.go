package admission

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// SQLSTATEs and constraint names the S-12 table names.
const (
	codeUniqueViolation      = "23505"
	codeCheckViolation       = "23514"
	codeLockNotAvailable     = "55P03"
	codeIdleSessionTimeout   = "25P03"
	codeTxIntegrityViolation = "40002"

	constraintCommandIDKey = "admissions_command_id_key"
	constraintFloor        = "ledger_accounts_floor_check"
)

// callClass is the handling of an error from BeginTx or the
// orvex.admit_command call (S-12). No COMMIT was sent, so nothing
// committed.
type callClass uint8

const (
	// callInternal: any other SQLSTATE; fail closed, no retry.
	callInternal callClass = iota
	// callRetry: connection loss, classes 08, 53 and 57, 25P03, 40001 and
	// 40P01; retry.
	callRetry
	// callBusy: 55P03, the lock wait timed out; ErrBusy, no retry.
	callBusy
	// callResolve: 23505 on admissions_command_id_key; read the admission.
	callResolve
	// callInsufficientCredits: 23514 on ledger_accounts_floor_check.
	callInsufficientCredits
	// callContext: ctx ended; return an error wrapping ctx.Err().
	callContext
)

// classifyCallError classifies an error from BeginTx or the function call.
func classifyCallError(ctx context.Context, err error) callClass {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return callContext
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return callInternal
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return callRetry
	}
	switch {
	case postgres.IsRetryable(err), pgErr.Code == codeIdleSessionTimeout:
		return callRetry
	case pgErr.Code == codeLockNotAvailable:
		return callBusy
	case pgErr.Code == codeUniqueViolation && pgErr.ConstraintName == constraintCommandIDKey:
		return callResolve
	case pgErr.Code == codeCheckViolation && pgErr.ConstraintName == constraintFloor:
		return callInsufficientCredits
	case unavailableClass(pgErr.Code):
		return callRetry
	default:
		return callInternal
	}
}

// unavailableClass reports SQLSTATE classes 08 (connection), 53
// (insufficient resources) and 57 (operator intervention).
func unavailableClass(code string) bool {
	if len(code) != 5 {
		return false
	}
	switch code[:2] {
	case "08", "53", "57":
		return true
	default:
		return false
	}
}

// commitClass is the handling of an error from Commit (S-12).
type commitClass uint8

const (
	// commitAmbiguous: the commit may have happened; retry with the same
	// command ID if ctx is alive and attempts remain, else
	// ErrOutcomeUnknown.
	commitAmbiguous commitClass = iota
	// commitRetry: 40001 or 40P01, rolled back; retry.
	commitRetry
	// commitDefinite: definitely not committed (class 23 or OX, 40002,
	// pgx.ErrTxCommitRollback); ErrInternal.
	commitDefinite
)

// classifyCommitError classifies a non-nil error from Commit.
func classifyCommitError(err error) commitClass {
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return commitDefinite
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return commitAmbiguous
	}
	switch {
	case postgres.IsRetryable(err):
		return commitRetry
	case pgErr.Code == codeTxIntegrityViolation,
		len(pgErr.Code) == 5 && (pgErr.Code[:2] == "23" || pgErr.Code[:2] == "OX"):
		return commitDefinite
	default:
		return commitAmbiguous
	}
}

// Backoff before attempt n >= 2: uniform in [0, min(maxBackoff,
// baseBackoff * 2^(n-2))] (full jitter).
const (
	baseBackoff = 5 * time.Millisecond
	maxBackoff  = 50 * time.Millisecond
)

// backoffLimit returns the upper bound of the backoff before attempt n.
func backoffLimit(attempt int) time.Duration {
	if attempt < 2 {
		return 0
	}
	limit := baseBackoff
	for i := 2; i < attempt && limit < maxBackoff; i++ {
		limit *= 2
	}
	return min(limit, maxBackoff)
}

// backoff returns a random wait before attempt n.
func backoff(attempt int) time.Duration {
	limit := backoffLimit(attempt)
	if limit <= 0 {
		return 0
	}
	return rand.N(limit + 1)
}

// sleepCtx waits d or until ctx ends, returning ctx.Err() in that case.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

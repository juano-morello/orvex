package execution

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// SQLSTATEs and constraint names of the E-15 table.
const (
	codeFKViolation        = "23503"
	codeUniqueViolation    = "23505"
	codeLockNotAvailable   = "55P03"
	codeIdleSessionTimeout = "25P03"
	codeQueryCanceled      = "57014"
	codeTxIntegrity        = "40002"

	constraintPrevFkey   = "chain_records_prev_fkey"
	constraintHeadFkey   = "round_execution_heads_head_fkey"
	constraintRoundsPkey = "rounds_pkey"
)

// stmtClass is the handling of an error from a statement before COMMIT
// (E-15). No COMMIT was sent, so nothing committed.
type stmtClass uint8

const (
	// stmtInternal: a constraint bug or any other SQLSTATE; ErrInternal,
	// drop the entry, no retry.
	stmtInternal stmtClass = iota
	// stmtRetry: connection loss, classes 08, 53 and 57, 25P03, 40001 and
	// 40P01; retry.
	stmtRetry
	// stmtBusy: 55P03; ErrBusy, no retry.
	stmtBusy
	// stmtConflict: 23503 on chain_records_prev_fkey; run the conflict
	// check.
	stmtConflict
	// stmtContext: ctx ended; return an error wrapping ctx.Err().
	stmtContext
)

// classifyStatementError classifies an error from BeginTx or a statement.
func classifyStatementError(ctx context.Context, err error) stmtClass {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return stmtContext
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		if errors.Is(err, pgx.ErrNoRows) {
			return stmtInternal
		}
		return stmtRetry
	}
	switch {
	case postgres.IsRetryable(err), pgErr.Code == codeIdleSessionTimeout:
		return stmtRetry
	case pgErr.Code == codeLockNotAvailable:
		return stmtBusy
	case pgErr.Code == codeFKViolation && pgErr.ConstraintName == constraintPrevFkey:
		return stmtConflict
	case unavailableClass(pgErr.Code):
		return stmtRetry
	default:
		return stmtInternal
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

// commitClass is the handling of an error from COMMIT (E-15).
type commitClass uint8

const (
	// commitAmbiguous: the commit may have happened; the next attempt
	// starts with the conflict check.
	commitAmbiguous commitClass = iota
	// commitRetry: 40001 or 40P01, rolled back; retry.
	commitRetry
	// commitConflict: OX004 or 23503 on round_execution_heads_head_fkey;
	// run the conflict check.
	commitConflict
	// commitDefinite: definitely not committed (OX009, any other class 23
	// or OX, 40002, pgx.ErrTxCommitRollback); ErrInternal.
	commitDefinite
)

// classifyCommitError classifies a non-nil error from COMMIT.
func classifyCommitError(err error) commitClass {
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return commitDefinite
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return commitAmbiguous
	}
	switch {
	case pgErr.Code == postgres.CodeNotFenced,
		pgErr.Code == codeFKViolation && pgErr.ConstraintName == constraintHeadFkey:
		return commitConflict
	case postgres.IsRetryable(err):
		return commitRetry
	case pgErr.Code == codeTxIntegrity,
		len(pgErr.Code) == 5 && (pgErr.Code[:2] == "23" || pgErr.Code[:2] == "OX"):
		return commitDefinite
	default:
		return commitAmbiguous
	}
}

// conflictDecision is the outcome of the conflict check (E-7).
type conflictDecision uint8

const (
	// conflictCorrupt: the execution head is gone.
	conflictCorrupt conflictDecision = iota + 1
	// conflictFenced: another fencing token owns the round.
	conflictFenced
	// conflictAdopt: record k is committed with the computed hash.
	conflictAdopt
	// conflictRetry: nothing committed; retry the same transition.
	conflictRetry
	// conflictReload: another transition is committed, or the cache is
	// ahead of the database; reload.
	conflictReload
)

// headView is what the conflict check reads.
type headView struct {
	found     bool
	processed int64
	token     int64
	// recordHash is the hash of record k, nil when there is none.
	recordHash []byte
}

// decideConflict applies E-7's table, first match wins, for record k
// computed with hash under token.
func decideConflict(h headView, k uint64, hash [32]byte, token int64) conflictDecision {
	switch {
	case !h.found:
		return conflictCorrupt
	case h.token != token:
		return conflictFenced
	case h.processed >= int64(k) && h.recordHash != nil && string(h.recordHash) == string(hash[:]):
		return conflictAdopt
	case h.processed == int64(k)-1:
		return conflictRetry
	default:
		return conflictReload
	}
}

// Backoff before attempt n >= 2: uniform in [0, min(maxBackoff,
// baseBackoff * 2^(n-2))] (full jitter, E-15).
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

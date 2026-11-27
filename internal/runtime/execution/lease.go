package execution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxLeaseTTL bounds the ttl of ClaimLease.
const MaxLeaseTTL = time.Hour

// Lock before evaluating the database clock, including after waiting for an
// earlier Transaction B. A repeated claim by the same process recovers its
// live token after a lost reply without extending the lease.
const lockedLease = `WITH locked AS MATERIALIZED (
	SELECT round_id, owner_id, fencing_token, lease_expires_at
	FROM orvex.round_execution_heads WHERE round_id = $1 FOR NO KEY UPDATE
)`

const claimSQL = lockedLease + `, claimed AS (
	UPDATE orvex.round_execution_heads h SET fencing_token = l.fencing_token + 1,
		owner_id = $2, lease_expires_at = clock_timestamp() + $3::bigint * interval '1 microsecond'
	FROM locked l WHERE h.round_id = l.round_id
		AND (l.owner_id IS NULL OR l.lease_expires_at <= clock_timestamp())
	RETURNING h.fencing_token
)
SELECT COALESCE((SELECT fencing_token FROM claimed),
	CASE WHEN owner_id = $2 THEN fencing_token END) FROM locked`

const renewSQL = lockedLease + `, renewed AS (
	UPDATE orvex.round_execution_heads h
	SET lease_expires_at = clock_timestamp() + $3::bigint * interval '1 microsecond'
	FROM locked l WHERE h.round_id = l.round_id AND l.owner_id = $2
		AND l.fencing_token = $4 AND l.lease_expires_at > clock_timestamp()
	RETURNING h.fencing_token
)
SELECT (SELECT fencing_token FROM renewed) FROM locked`

// ClaimLease claims an unowned or expired round. owner identifies one process incarnation.
func (e *Executor) ClaimLease(ctx context.Context, roundID [16]byte, owner string, ttl time.Duration) (Lease, error) {
	return e.changeLease(ctx, roundID, owner, ttl, claimSQL, ErrLeaseHeld)
}

// RenewLease extends the current owner's live lease, preserving its token.
func (e *Executor) RenewLease(ctx context.Context, lease Lease, owner string, ttl time.Duration) error {
	if err := lease.validate(); err != nil {
		return err
	}
	_, err := e.changeLease(ctx, lease.RoundID, owner, ttl, renewSQL, ErrFenced, lease.FencingToken)
	return err
}

func (e *Executor) changeLease(ctx context.Context, roundID [16]byte, owner string, ttl time.Duration, query string, denied error, extra ...any) (Lease, error) {
	switch {
	case roundID == [16]byte{}:
		return Lease{}, fmt.Errorf("%w: nil round id", ErrInvalidRequest)
	case !engineBuildPattern.MatchString(owner):
		return Lease{}, fmt.Errorf("%w: owner must be 1 to 128 printable ASCII bytes without spaces", ErrInvalidRequest)
	case ttl < time.Microsecond || ttl > MaxLeaseTTL:
		return Lease{}, fmt.Errorf("%w: lease ttl outside [1µs, %v]", ErrInvalidRequest, MaxLeaseTTL)
	}
	args := append([]any{roundID, owner, ttl.Microseconds()}, extra...)
	var lastErr error
	for attempt := 1; attempt <= e.cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := e.sleep(ctx, backoff(attempt)); err != nil {
				return Lease{}, contextError(ctx, err)
			}
		}
		var token *int64
		attemptCtx, cancel := context.WithTimeout(ctx, e.cfg.LockTimeout)
		err := e.db.QueryRow(attemptCtx, query, args...).Scan(&token)
		timedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if err != nil && timedOut {
			return Lease{}, ErrUnavailable
		}
		if err == nil {
			if token == nil {
				return Lease{}, denied
			}
			return Lease{RoundID: roundID, FencingToken: *token}, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return Lease{}, ErrRoundNotFound
		}
		switch classifyStatementError(ctx, err) {
		case stmtContext:
			return Lease{}, contextError(ctx, err)
		case stmtRetry:
			lastErr = err
		case stmtBusy:
			return Lease{}, fmt.Errorf("%w: %w", ErrBusy, err)
		default:
			return Lease{}, fmt.Errorf("%w: change lease: %w", ErrInternal, err)
		}
	}
	return Lease{}, fmt.Errorf("%w: %w", ErrUnavailable, lastErr)
}

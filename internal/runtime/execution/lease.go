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

// claimSQL bumps the round's fencing token and names the new owner in one
// statement; check_execution_head accepts an owner change only together
// with a higher token.
const claimSQL = `UPDATE orvex.round_execution_heads
	SET fencing_token = fencing_token + 1, owner_id = $2,
		lease_expires_at = clock_timestamp() + $3::bigint * interval '1 microsecond'
	WHERE round_id = $1
	RETURNING fencing_token`

// ClaimLease takes the execution of round roundID over for owner and
// returns the lease of the new fencing token. One statement raises the
// round's fencing token by one, sets owner_id to owner (1 to 128 printable
// ASCII bytes without spaces) and lease_expires_at to the database clock
// plus ttl (in (0, MaxLeaseTTL]). From then on a lease of an older token is
// fenced: its next Transaction B returns ErrFenced (E-7).
//
// The claim does not wait for an earlier lease to expire, and nothing reads
// lease_expires_at yet: the last claim owns the round. Calling it again
// after an unknown outcome is safe; it raises the token again, and the
// owner holds the newest one. The round's cache is kept: it does not
// depend on the token.
func (e *Executor) ClaimLease(ctx context.Context, roundID [16]byte, owner string, ttl time.Duration) (Lease, error) {
	switch {
	case roundID == [16]byte{}:
		return Lease{}, fmt.Errorf("%w: nil round id", ErrInvalidRequest)
	case !engineBuildPattern.MatchString(owner):
		return Lease{}, fmt.Errorf("%w: owner must be 1 to 128 printable ASCII bytes without spaces", ErrInvalidRequest)
	case ttl <= 0 || ttl > MaxLeaseTTL:
		return Lease{}, fmt.Errorf("%w: lease ttl outside (0, %v]", ErrInvalidRequest, MaxLeaseTTL)
	}
	var lastErr error
	for attempt := 1; attempt <= e.cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := e.sleep(ctx, backoff(attempt)); err != nil {
				return Lease{}, contextError(ctx, err)
			}
		}
		var token int64
		err := e.db.QueryRow(ctx, claimSQL, roundID, owner, ttl.Microseconds()).Scan(&token)
		if err == nil {
			return Lease{RoundID: roundID, FencingToken: token}, nil
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
			return Lease{}, fmt.Errorf("%w: claim lease: %w", ErrInternal, err)
		}
	}
	return Lease{}, fmt.Errorf("%w: %w", ErrUnavailable, lastErr)
}

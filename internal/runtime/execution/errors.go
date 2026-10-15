package execution

import (
	"errors"
	"fmt"

	"github.com/juano-morello/orvex/internal/canonjson"
)

// Sentinel errors. Every error the Executor returns wraps exactly one of
// them, except a context error, which wraps ctx.Err() (E-15). PgError
// details stay reachable through errors.As. No error text contains state,
// seeds or deadlines (E-14).
var (
	// ErrInvalidConfig: New was given a Config outside its ranges, no
	// policy, a duplicate policy, or a policy without a matching adapter or
	// ledger rule.
	ErrInvalidConfig = errors.New("execution: invalid config")
	// ErrInvalidRequest: a nil round ID, a negative fencing token, or a
	// Drain limit below 1. No database call was made.
	ErrInvalidRequest = errors.New("execution: invalid request")
	// ErrInvalidRound: CreateRound was given a nil round ID or a config the
	// rules version rejects or that belongs to another round.
	ErrInvalidRound = errors.New("execution: invalid round")
	// ErrUnknownRules: no execution policy or adapter for the round's game
	// rules version, or its descriptor differs from the round's.
	ErrUnknownRules = errors.New("execution: no execution policy for the game rules version")
	// ErrRoundNotFound: the round does not exist.
	ErrRoundNotFound = errors.New("execution: round not found")
	// ErrRoundExists: CreateRound found the round ID with a different
	// genesis record. Nothing was written.
	ErrRoundExists = errors.New("execution: round exists with a different genesis")
	// ErrFenced: the round's fencing token is no longer the lease's; stop
	// executing this round (E-7).
	ErrFenced = errors.New("execution: fencing token superseded; stop executing this round")
	// ErrFailClosed is matched by every *FailClosedError.
	ErrFailClosed = errors.New("execution: rules failed closed; round halted")
	// ErrCorrupt: stored round data violates an execution invariant; the
	// round is halted in this process until Forget.
	ErrCorrupt = errors.New("execution: stored round data violates an execution invariant")
	// ErrBusy: a lock wait exceeded Config.LockTimeout. Nothing committed;
	// call again later.
	ErrBusy = errors.New("execution: lock wait timed out")
	// ErrUnavailable: every attempt failed transiently. Calling again later
	// is safe: the next attempt resolves any unknown outcome.
	ErrUnavailable = errors.New("execution: database unavailable; call again later")
	// ErrInternal: a definite failure that is a bug; nothing committed.
	ErrInternal = errors.New("execution: internal error")
)

// FailClosedError reports that the rules, the codec or the posting rule
// failed deterministically while computing record Sequence of round RoundID
// (E-14). Nothing was written; the round is halted until Forget.
type FailClosedError struct {
	RoundID  [16]byte
	Sequence uint64
	Err      error
}

// Error returns "execution: round <uuid> halted at sequence <k>: <err>".
func (e *FailClosedError) Error() string {
	return fmt.Sprintf("execution: round %s halted at sequence %d: %v", canonjson.UUID(e.RoundID), e.Sequence, e.Err)
}

// Unwrap makes errors.Is(err, ErrFailClosed) and errors.Is(err, cause)
// hold.
func (e *FailClosedError) Unwrap() []error { return []error{ErrFailClosed, e.Err} }

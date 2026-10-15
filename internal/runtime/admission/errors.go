package admission

import "errors"

// Sentinel errors. Every error Admit and Lookup return wraps exactly one of
// them, except a context error, which wraps ctx.Err() (S-12). PgError
// details stay reachable through errors.As. No error text contains command
// bytes, seeds or deadlines.
var (
	// ErrInvalidConfig: New was given a Config outside its ranges, no
	// policy, a duplicate policy, or a policy without a matching adapter.
	ErrInvalidConfig = errors.New("admission: invalid config")
	// ErrInvalidRequest: a nil round or command ID, a NotBeforeUnixMicro
	// that does not fit the command's class, or a request whose game or
	// rules version differs from the round's.
	ErrInvalidRequest = errors.New("admission: invalid request")
	// ErrInvalidCommand: the command cannot be encoded or classified by the
	// rules version, or a player ID contains U+0000.
	ErrInvalidCommand = errors.New("admission: invalid command")
	// ErrUnknownRules: no policy for the request's game and rules version.
	ErrUnknownRules = errors.New("admission: no admission policy for the game rules version")
	// ErrRoundNotFound: the round does not exist.
	ErrRoundNotFound = errors.New("admission: round not found")
	// ErrCommandIDReused: the command ID is already admitted for another
	// round or other command bytes (S-6). Nothing was written.
	ErrCommandIDReused = errors.New("admission: command id already admitted for a different command")
	// ErrRejected is matched by every *RejectedError.
	ErrRejected = errors.New("admission: rejected")
	// ErrClosureNotDue is matched by every *ClosureNotDueError.
	ErrClosureNotDue = errors.New("admission: round closure not due")
	// ErrBusy: the round's lock wait exceeded Config.LockTimeout. Nothing
	// was admitted; resubmit the same request.
	ErrBusy = errors.New("admission: lock wait timed out")
	// ErrUnavailable: the database failed on every attempt before COMMIT.
	// Nothing was admitted; resubmit the same request.
	ErrUnavailable = errors.New("admission: database unavailable; command not admitted")
	// ErrOutcomeUnknown: the last COMMIT's outcome is unknown. Resubmit the
	// same request with the same command ID; never treat it as a rejection.
	ErrOutcomeUnknown = errors.New("admission: outcome unknown; resubmit the same command id")
	// ErrInternal: a definite failure that is a bug or corruption; fail
	// closed.
	ErrInternal = errors.New("admission: internal error")
	// ErrNotFound: Lookup found no committed admission for the command ID.
	// It is not definitive for a command still being admitted.
	ErrNotFound = errors.New("admission: no admission for command id")
)

// Reason is the stable code of a rejection at admission (S-9). It is not a
// Decide reason code.
type Reason string

// Rejection reasons at admission.
const (
	// ReasonRoundClosed: an ordinary command after the admitted CloseRound.
	ReasonRoundClosed Reason = "round_closed"
	// ReasonRoundSealed: any command after the admitted SettleRound.
	ReasonRoundSealed Reason = "round_sealed"
	// ReasonLifecycleOrder: a lifecycle command out of window order (S-8).
	ReasonLifecycleOrder Reason = "lifecycle_order"
	// ReasonInsufficientCredits: the hold exceeds the player's available
	// credits, or the player has no account (S-11).
	ReasonInsufficientCredits Reason = "insufficient_credits"
)

// RejectedError is a rejection at admission. Nothing was written; a later
// resubmission is evaluated afresh (S-6).
type RejectedError struct{ Reason Reason }

// Error returns "admission: rejected: " followed by the reason code.
func (e *RejectedError) Error() string { return "admission: rejected: " + string(e.Reason) }

// Unwrap makes errors.Is(err, ErrRejected) hold.
func (e *RejectedError) Unwrap() error { return ErrRejected }

// ClosureNotDueError reports a closing command whose effective time was
// before NotBeforeUnixMicro (S-8). Nothing was written. Its text names
// neither time.
type ClosureNotDueError struct{ EffectiveTimeUnixMicro, NotBeforeUnixMicro int64 }

// Error returns "admission: round closure not due".
func (e *ClosureNotDueError) Error() string { return "admission: round closure not due" }

// Unwrap makes errors.Is(err, ErrClosureNotDue) hold.
func (e *ClosureNotDueError) Unwrap() error { return ErrClosureNotDue }

// reasonOf maps the reason text of a rejected outcome (§5.3).
func reasonOf(s string) (Reason, bool) {
	switch r := Reason(s); r {
	case ReasonRoundClosed, ReasonRoundSealed, ReasonLifecycleOrder, ReasonInsufficientCredits:
		return r, true
	default:
		return "", false
	}
}

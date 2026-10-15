package admission

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// DB is what the Sequencer needs from the database; *pgxpool.Pool
// implements it.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Config tunes the Sequencer. New rejects values outside the ranges
// (ErrInvalidConfig); there is no zero-means-default.
type Config struct {
	// LockTimeout bounds every lock wait inside Transaction A: the round's
	// head, the player's account and a cross-round duplicate's unique-index
	// wait. In [10ms, 60s]; it must exceed IdleInTransactionTimeout.
	LockTimeout time.Duration
	// IdleInTransactionTimeout ends a session that called
	// orvex.admit_command and then went idle, releasing the round's head.
	// In [50ms, 60s].
	IdleInTransactionTimeout time.Duration
	// MaxAttempts bounds the attempts of one Admit call (S-12). In [1, 10].
	MaxAttempts int
	// MaxInFlightPerRound bounds the Admit calls per round that hold a
	// pool connection at once (S-4). In [1, 64].
	MaxInFlightPerRound int
}

// Config bounds.
const (
	minLockTimeout  = 10 * time.Millisecond
	maxLockTimeout  = 60 * time.Second
	minIdleTimeout  = 50 * time.Millisecond
	maxIdleTimeout  = 60 * time.Second
	maxAttempts     = 10
	maxInFlight     = 64
	rollbackTimeout = 5 * time.Second
)

// DefaultConfig returns LockTimeout 3s, IdleInTransactionTimeout 1s,
// MaxAttempts 3 and MaxInFlightPerRound 4.
func DefaultConfig() Config {
	return Config{
		LockTimeout:              3 * time.Second,
		IdleInTransactionTimeout: time.Second,
		MaxAttempts:              3,
		MaxInFlightPerRound:      4,
	}
}

func (c Config) validate() error {
	switch {
	case c.LockTimeout < minLockTimeout || c.LockTimeout > maxLockTimeout:
		return fmt.Errorf("%w: LockTimeout outside [%v, %v]", ErrInvalidConfig, minLockTimeout, maxLockTimeout)
	case c.IdleInTransactionTimeout < minIdleTimeout || c.IdleInTransactionTimeout > maxIdleTimeout:
		return fmt.Errorf("%w: IdleInTransactionTimeout outside [%v, %v]", ErrInvalidConfig, minIdleTimeout, maxIdleTimeout)
	case c.LockTimeout <= c.IdleInTransactionTimeout:
		return fmt.Errorf("%w: LockTimeout must exceed IdleInTransactionTimeout", ErrInvalidConfig)
	case c.MaxAttempts < 1 || c.MaxAttempts > maxAttempts:
		return fmt.Errorf("%w: MaxAttempts outside [1, %d]", ErrInvalidConfig, maxAttempts)
	case c.MaxInFlightPerRound < 1 || c.MaxInFlightPerRound > maxInFlight:
		return fmt.Errorf("%w: MaxInFlightPerRound outside [1, %d]", ErrInvalidConfig, maxInFlight)
	default:
		return nil
	}
}

// Sequencer admits commands: ADR-002's durable admission and ADR-005's
// Transaction A. It is safe for concurrent use.
type Sequencer struct {
	db     DB
	cfg    Config
	rules  map[rulesKey]rulesEntry
	gate   *gate
	lockMs int32
	idleMs int32
	// sleep waits between attempts; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

type rulesKey struct {
	game  game.ID
	rules game.RulesVersion
}

type rulesEntry struct {
	policy  Policy
	adapter game.Adapter
}

// New requires at least one policy, no two policies for one (game, rules
// version), and for each policy an adapter in reg whose Descriptor equals
// the policy's.
func New(db DB, reg *game.Registry, cfg Config, policies ...Policy) (*Sequencer, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: nil DB", ErrInvalidConfig)
	}
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", ErrInvalidConfig)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(policies) == 0 {
		return nil, fmt.Errorf("%w: no policies", ErrInvalidConfig)
	}
	rules := make(map[rulesKey]rulesEntry, len(policies))
	for i, p := range policies {
		if p == nil {
			return nil, fmt.Errorf("%w: policy %d is nil", ErrInvalidConfig, i)
		}
		d := p.Descriptor()
		k := rulesKey{game: d.Game, rules: d.RulesVersion}
		if _, dup := rules[k]; dup {
			return nil, fmt.Errorf("%w: two policies for %q %d", ErrInvalidConfig, d.Game, d.RulesVersion)
		}
		a, err := reg.Lookup(d.Game, d.RulesVersion)
		if err != nil {
			return nil, fmt.Errorf("%w: policy %q %d: %w", ErrInvalidConfig, d.Game, d.RulesVersion, err)
		}
		if a.Descriptor() != d {
			return nil, fmt.Errorf("%w: policy %q %d: registry descriptor differs", ErrInvalidConfig, d.Game, d.RulesVersion)
		}
		rules[k] = rulesEntry{policy: p, adapter: a}
	}
	return &Sequencer{
		db:     db,
		cfg:    cfg,
		rules:  rules,
		gate:   newGate(cfg.MaxInFlightPerRound),
		lockMs: int32(cfg.LockTimeout / time.Millisecond),
		idleMs: int32(cfg.IdleInTransactionTimeout / time.Millisecond),
		sleep:  sleepCtx,
	}, nil
}

// Request is one command submitted for admission.
type Request struct {
	RoundID      [16]byte // not the nil UUID
	Game         game.ID
	RulesVersion game.RulesVersion
	CommandID    [16]byte // not the nil UUID; reuse it on every retry of this command
	Command      any      // the rules version's command value, e.g. v1.CashOut{...}
	// NotBeforeUnixMicro is the earliest effective time at which a closing
	// command may be admitted: the absolute crash deadline T0 + D (S-8).
	// > 0 for ClassClose, 0 for every other class.
	NotBeforeUnixMicro int64
}

// Admission is a durable admission receipt. Admit returns one only after
// the admission committed (ADR-002); it says nothing about execution.
type Admission struct {
	RoundID                [16]byte
	CommandID              [16]byte
	Sequence               uint64 // >= 1
	EffectiveTimeUnixMicro int64
	// Duplicate reports that the admission already existed when Admit's
	// final attempt ran.
	Duplicate bool
}

// Input returns the deterministic input Decide receives for this command.
func (a Admission) Input() game.Input {
	return game.Input{Sequence: a.Sequence, EffectiveTimeUnixMicro: a.EffectiveTimeUnixMicro}
}

// admitSQL is Transaction A's single statement (S-3).
const admitSQL = `SELECT outcome, reason, sequence, effective_time_unix_micro
	FROM orvex.admit_command($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

// Admit durably admits req.Command to round req.RoundID and returns its
// admission once COMMIT succeeded, or the original admission of a command
// already admitted under req.CommandID (Duplicate). A rejection at
// admission is a *RejectedError or *ClosureNotDueError; nothing was written.
//
// On ErrOutcomeUnknown, ErrUnavailable, ErrBusy or a context error, resubmit
// the same Request with the same command ID; never treat these as
// rejections.
func (s *Sequencer) Admit(ctx context.Context, req Request) (Admission, error) {
	if req.RoundID == ([16]byte{}) {
		return Admission{}, fmt.Errorf("%w: nil round id", ErrInvalidRequest)
	}
	if req.CommandID == ([16]byte{}) {
		return Admission{}, fmt.Errorf("%w: nil command id", ErrInvalidRequest)
	}
	re, ok := s.rules[rulesKey{game: req.Game, rules: req.RulesVersion}]
	if !ok {
		return Admission{}, fmt.Errorf("%w: %q %d", ErrUnknownRules, req.Game, req.RulesVersion)
	}
	command, err := re.adapter.EncodeCommand(req.Command)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: %w", ErrInvalidCommand, err)
	}
	cl, err := re.policy.Classify(req.Command)
	if err != nil {
		if errors.Is(err, ErrInvalidCommand) {
			return Admission{}, err
		}
		return Admission{}, fmt.Errorf("%w: %w", ErrInvalidCommand, err)
	}
	if !cl.valid() {
		return Admission{}, fmt.Errorf("%w: policy returned an invalid classification", ErrInternal)
	}
	if cl.Class == ClassClose {
		if req.NotBeforeUnixMicro <= 0 {
			return Admission{}, fmt.Errorf("%w: a closing command needs NotBeforeUnixMicro > 0", ErrInvalidRequest)
		}
	} else if req.NotBeforeUnixMicro != 0 {
		return Admission{}, fmt.Errorf("%w: NotBeforeUnixMicro is only for a closing command", ErrInvalidRequest)
	}

	release, err := s.gate.acquire(ctx, req.RoundID)
	if err != nil {
		return Admission{}, notAdmitted(err)
	}
	defer release()

	call := admitCall{req: req, command: command, cl: cl}
	var (
		lastErr error
		// ambiguous reports that the last failure was an ambiguous COMMIT
		// (S-12: ErrOutcomeUnknown only then).
		ambiguous bool
	)
	for attempt := 1; attempt <= s.cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := s.sleep(ctx, backoff(attempt)); err != nil {
				if ambiguous {
					return Admission{}, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
				}
				return Admission{}, notAdmitted(err)
			}
		}
		a, res, err := s.attempt(ctx, call)
		switch res {
		case attemptDone:
			return a, err
		case attemptAmbiguous:
			if ctx.Err() != nil {
				return Admission{}, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
			}
			lastErr, ambiguous = err, true
		default:
			lastErr, ambiguous = err, false
		}
	}
	if ambiguous {
		return Admission{}, fmt.Errorf("%w: %w", ErrOutcomeUnknown, lastErr)
	}
	return Admission{}, fmt.Errorf("%w: %w", ErrUnavailable, lastErr)
}

// notAdmitted wraps a context error: no COMMIT was sent, so nothing was
// admitted.
func notAdmitted(err error) error {
	return fmt.Errorf("admission: not admitted: %w", err)
}

type admitCall struct {
	req     Request
	command []byte
	cl      Classification
}

// attemptResult tells the attempt loop what to do next.
type attemptResult uint8

const (
	// attemptDone: return the Admission and error as they are.
	attemptDone attemptResult = iota
	// attemptRetry: nothing committed in this attempt; retry.
	attemptRetry
	// attemptAmbiguous: COMMIT's outcome is unknown; retry.
	attemptAmbiguous
)

// attempt runs one Transaction A.
func (s *Sequencer) attempt(ctx context.Context, c admitCall) (Admission, attemptResult, error) {
	tx, err := s.db.BeginTx(ctx, postgres.AdmissionTxOptions())
	if err != nil {
		return s.callFailed(ctx, c, err)
	}
	commitSent := false
	defer func() {
		if !commitSent {
			rollback(ctx, tx)
		}
	}()

	class, _ := c.cl.Class.sqlText()
	var holdPlayer *string
	var holdCredits, notBefore *int64
	if c.cl.Hold.PlayerID != "" {
		holdPlayer, holdCredits = &c.cl.Hold.PlayerID, &c.cl.Hold.Credits
	}
	if c.cl.Class == ClassClose {
		notBefore = &c.req.NotBeforeUnixMicro
	}
	var (
		outcome  string
		reason   *string
		sequence *int64
		effTime  *int64
	)
	err = tx.QueryRow(ctx, admitSQL,
		c.req.RoundID, string(c.req.Game), int64(c.req.RulesVersion), c.req.CommandID, c.command,
		class, holdPlayer, holdCredits, notBefore, s.lockMs, s.idleMs,
	).Scan(&outcome, &reason, &sequence, &effTime)
	if err != nil {
		rollback(ctx, tx)
		commitSent = true // closed; nothing more to roll back
		return s.callFailed(ctx, c, err)
	}

	admission := func(dup bool) (Admission, bool) {
		if sequence == nil || *sequence < 1 || effTime == nil {
			return Admission{}, false
		}
		return Admission{
			RoundID:                c.req.RoundID,
			CommandID:              c.req.CommandID,
			Sequence:               uint64(*sequence),
			EffectiveTimeUnixMicro: *effTime,
			Duplicate:              dup,
		}, true
	}
	internal := func(what string) (Admission, attemptResult, error) {
		return Admission{}, attemptDone, fmt.Errorf("%w: admit_command: %s", ErrInternal, what)
	}

	switch outcome {
	case "admitted":
		a, ok := admission(false)
		if !ok {
			return internal("admitted outcome without sequence or effective time")
		}
		commitSent = true
		err := tx.Commit(ctx)
		if err == nil {
			return a, attemptDone, nil
		}
		switch classifyCommitError(err) {
		case commitRetry:
			return Admission{}, attemptRetry, err
		case commitDefinite:
			return Admission{}, attemptDone, fmt.Errorf("%w: commit: %w", ErrInternal, err)
		default:
			return Admission{}, attemptAmbiguous, err
		}
	case "duplicate":
		a, ok := admission(true)
		if !ok {
			return internal("duplicate outcome without sequence or effective time")
		}
		return a, attemptDone, nil
	case "conflict":
		return Admission{}, attemptDone, ErrCommandIDReused
	case "rejected":
		if reason == nil {
			return internal("rejected outcome without reason")
		}
		r, ok := reasonOf(*reason)
		if !ok {
			return internal("unknown rejection reason")
		}
		return Admission{}, attemptDone, &RejectedError{Reason: r}
	case "not_due":
		if effTime == nil {
			return internal("not_due outcome without effective time")
		}
		return Admission{}, attemptDone, &ClosureNotDueError{
			EffectiveTimeUnixMicro: *effTime,
			NotBeforeUnixMicro:     c.req.NotBeforeUnixMicro,
		}
	case "invalid_not_before":
		return Admission{}, attemptDone, fmt.Errorf("%w: NotBeforeUnixMicro is before the round's start", ErrInvalidRequest)
	case "rules_mismatch":
		return Admission{}, attemptDone, fmt.Errorf("%w: the round's game or rules version differs", ErrInvalidRequest)
	case "not_found":
		return Admission{}, attemptDone, ErrRoundNotFound
	case "corrupt":
		return internal("round admission head missing or inconsistent")
	default:
		return internal("unknown outcome")
	}
}

// callFailed maps an error from BeginTx or the function call (S-12).
func (s *Sequencer) callFailed(ctx context.Context, c admitCall, err error) (Admission, attemptResult, error) {
	switch classifyCallError(ctx, err) {
	case callContext:
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = fmt.Errorf("%w: %w", ctxErr, err)
		}
		return Admission{}, attemptDone, notAdmitted(err)
	case callRetry:
		return Admission{}, attemptRetry, err
	case callBusy:
		return Admission{}, attemptDone, fmt.Errorf("%w: %w", ErrBusy, err)
	case callInsufficientCredits:
		// The PgError stays reachable through errors.As (S-12).
		return Admission{}, attemptDone, fmt.Errorf("%w: %w", &RejectedError{Reason: ReasonInsufficientCredits}, err)
	case callResolve:
		return s.resolve(ctx, c, err)
	default:
		return Admission{}, attemptDone, fmt.Errorf("%w: %w", ErrInternal, err)
	}
}

// resolve handles 23505 on admissions_command_id_key: another transaction
// committed an admission with this command ID meanwhile (S-6, S-12).
func (s *Sequencer) resolve(ctx context.Context, c admitCall, cause error) (Admission, attemptResult, error) {
	st, err := s.readStatus(ctx, c.req.CommandID)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		return Admission{}, attemptDone, fmt.Errorf("%w: duplicate command id without an admission: %w", ErrInternal, cause)
	case errors.Is(err, ErrInternal):
		return Admission{}, attemptDone, err
	case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = fmt.Errorf("%w: %w", ctxErr, err)
		}
		return Admission{}, attemptDone, notAdmitted(err)
	case unavailable(err):
		return Admission{}, attemptRetry, err
	default:
		return Admission{}, attemptDone, fmt.Errorf("%w: %w", ErrInternal, err)
	}
	if st.round != c.req.RoundID || string(st.command) != string(c.command) {
		// The 23505 PgError stays reachable through errors.As (S-12).
		return Admission{}, attemptDone, fmt.Errorf("%w: %w", ErrCommandIDReused, cause)
	}
	a := st.status.Admission
	a.Duplicate = true
	return a, attemptDone, nil
}

// rollback ends tx with a detached, bounded context, so that a cancelled
// caller does not keep a round's head locked. Its error is ignored: nothing
// was written, and a failed rollback closes the connection, which also
// releases the lock.
func rollback(ctx context.Context, tx pgx.Tx) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(rctx)
}

// Outcome is the execution outcome of an admitted command.
type Outcome uint8

// Execution outcomes.
const (
	OutcomePending  Outcome = iota + 1 // admitted, not executed yet
	OutcomeAccepted                    // Decide accepted it
	OutcomeRejected                    // Decide rejected it; Status.Reason holds the code
)

// Status is the admission and execution status of a command.
type Status struct {
	Admission Admission // Duplicate is always false
	Outcome   Outcome
	Reason    string
}

// lookupSQL reads an admission and its chain record's result, if executed.
const lookupSQL = `SELECT a.round_id, a.sequence, a.effective_time_unix_micro, a.command,
	       r.result_status, r.result_reason
	FROM orvex.admissions a
	LEFT JOIN orvex.chain_records r ON r.round_id = a.round_id AND r.sequence = a.sequence
	WHERE a.command_id = $1`

// Lookup reads the admission and execution status of commandID in one
// SnapshotReadTxOptions transaction. ErrNotFound is not definitive for a
// command still being admitted; resubmitting the same Request is.
func (s *Sequencer) Lookup(ctx context.Context, commandID [16]byte) (Status, error) {
	st, err := s.readStatus(ctx, commandID)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrInternal):
		return Status{}, err
	default:
		return Status{}, readFailed(ctx, err)
	}
	return st.status, nil
}

type readResult struct {
	status  Status
	round   [16]byte
	command []byte
}

// readStatus is Lookup's read, which the 23505 resolve shares. It returns
// ErrNotFound, an error wrapping ErrInternal for an inconsistent row, or the
// driver's error unchanged.
func (s *Sequencer) readStatus(ctx context.Context, commandID [16]byte) (readResult, error) {
	tx, err := s.db.BeginTx(ctx, postgres.SnapshotReadTxOptions())
	if err != nil {
		return readResult{}, err
	}
	defer rollback(ctx, tx)
	var (
		out      readResult
		sequence int64
		effTime  int64
		status   *string
		reason   *string
	)
	err = tx.QueryRow(ctx, lookupSQL, commandID).
		Scan(&out.round, &sequence, &effTime, &out.command, &status, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return readResult{}, ErrNotFound
	}
	if err != nil {
		return readResult{}, err
	}
	if sequence < 1 {
		return readResult{}, fmt.Errorf("%w: admission sequence %d", ErrInternal, sequence)
	}
	out.status.Admission = Admission{
		RoundID:                out.round,
		CommandID:              commandID,
		Sequence:               uint64(sequence),
		EffectiveTimeUnixMicro: effTime,
	}
	switch {
	case status == nil:
		out.status.Outcome = OutcomePending
	case *status == "accepted":
		out.status.Outcome = OutcomeAccepted
	case *status == "rejected" && reason != nil:
		out.status.Outcome, out.status.Reason = OutcomeRejected, *reason
	default:
		return readResult{}, fmt.Errorf("%w: unknown result status", ErrInternal)
	}
	return out, nil
}

// readFailed maps a read error: ctx done wraps ctx.Err(); connection loss
// and classes 08, 53 and 57 are ErrUnavailable; anything else ErrInternal.
func readFailed(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = fmt.Errorf("%w: %w", ctxErr, err)
		}
		return fmt.Errorf("admission: read: %w", err)
	}
	if unavailable(err) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return fmt.Errorf("%w: %w", ErrInternal, err)
}

// unavailable reports connection loss (not a PgError) or SQLSTATE classes
// 08, 53 and 57.
func unavailable(err error) bool {
	var pgErr *pgconn.PgError
	return !errors.As(err, &pgErr) || unavailableClass(pgErr.Code)
}

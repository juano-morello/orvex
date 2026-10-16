package execution

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/ledger"
)

// DB is what the Executor needs; *pgxpool.Pool implements it.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Config tunes the Executor. New rejects values outside the ranges
// (ErrInvalidConfig); there is no zero-means-default.
type Config struct {
	// EngineBuild is written to every record and snapshot: 1 to 128
	// printable ASCII bytes without spaces (A-8).
	EngineBuild string
	// LockTimeout bounds Transaction B's lock waits on account rows. In
	// [10ms, 60s]; it must exceed IdleInTransactionTimeout.
	LockTimeout time.Duration
	// IdleInTransactionTimeout ends a session that left Transaction B
	// idle, releasing its locks. In [50ms, 60s].
	IdleInTransactionTimeout time.Duration
	// MaxAttempts bounds the attempts of one ExecuteNext or CreateRound
	// call; retries and reloads count. In [1, 10].
	MaxAttempts int
}

// Config bounds.
const (
	minLockTimeout  = 10 * time.Millisecond
	maxLockTimeout  = 60 * time.Second
	minIdleTimeout  = 50 * time.Millisecond
	maxIdleTimeout  = 60 * time.Second
	maxAttempts     = 10
	rollbackTimeout = 5 * time.Second
)

// Outbox and record limits.
const (
	// OutboxTopicRecord is the topic of the one outbox message per chain
	// record; its payload is the exact record bytes (E-12).
	OutboxTopicRecord = "round.record.v1"
	// MaxRecordBytes bounds a chain record (A-11, the chain_records CHECK).
	MaxRecordBytes = 64 << 20
)

var engineBuildPattern = regexp.MustCompile(`^[!-~]{1,128}$`)

// DefaultConfig returns {buildinfo.Version, 3s, 1s, 3}.
func DefaultConfig() Config {
	return Config{
		EngineBuild:              buildinfo.Version,
		LockTimeout:              3 * time.Second,
		IdleInTransactionTimeout: time.Second,
		MaxAttempts:              3,
	}
}

func (c Config) validate() error {
	switch {
	case !engineBuildPattern.MatchString(c.EngineBuild):
		return fmt.Errorf("%w: EngineBuild must be 1 to 128 printable ASCII bytes without spaces", ErrInvalidConfig)
	case c.LockTimeout < minLockTimeout || c.LockTimeout > maxLockTimeout:
		return fmt.Errorf("%w: LockTimeout outside [%v, %v]", ErrInvalidConfig, minLockTimeout, maxLockTimeout)
	case c.IdleInTransactionTimeout < minIdleTimeout || c.IdleInTransactionTimeout > maxIdleTimeout:
		return fmt.Errorf("%w: IdleInTransactionTimeout outside [%v, %v]", ErrInvalidConfig, minIdleTimeout, maxIdleTimeout)
	case c.LockTimeout <= c.IdleInTransactionTimeout:
		return fmt.Errorf("%w: LockTimeout must exceed IdleInTransactionTimeout", ErrInvalidConfig)
	case c.MaxAttempts < 1 || c.MaxAttempts > maxAttempts:
		return fmt.Errorf("%w: MaxAttempts outside [1, %d]", ErrInvalidConfig, maxAttempts)
	default:
		return nil
	}
}

type rulesKey struct {
	game  game.ID
	rules game.RulesVersion
}

// Executor executes admitted commands: ADR-005's Transaction B. It is safe
// for concurrent use; calls for one round serialize, different rounds run
// in parallel.
type Executor struct {
	db    DB
	cfg   Config
	rules map[rulesKey]rulesEntry

	mu      sync.Mutex
	entries map[[16]byte]*entry

	// sleep waits between attempts; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// entry is the in-process cache of one round (E-3).
type entry struct {
	mu      sync.Mutex
	dropped atomic.Bool

	loaded bool
	rules  rulesEntry
	roundState
	// playerAccounts memoizes player ID -> account ID (E-4).
	playerAccounts map[string][16]byte
	// halt is the error a halted round returns until Forget (E-14).
	halt error
	// pending is the admission read for processed + 1, kept across
	// attempts.
	pending *admissionRow
}

// New requires at least one policy, no two for one (game, rules version),
// and for each an adapter in reg whose Descriptor equals the policy's and
// its Ledger's.
func New(db DB, reg *game.Registry, cfg Config, policies ...Policy) (*Executor, error) {
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
		rule := p.Ledger()
		if rule == nil || rule.Descriptor() != d {
			return nil, fmt.Errorf("%w: policy %q %d: ledger rule missing or for another rules version", ErrInvalidConfig, d.Game, d.RulesVersion)
		}
		rules[k] = rulesEntry{adapter: a, policy: p, rule: rule}
	}
	return &Executor{
		db:      db,
		cfg:     cfg,
		rules:   rules,
		entries: map[[16]byte]*entry{},
		sleep:   sleepCtx,
	}, nil
}

// Lease is the caller's claim to execute one round under a fencing token
// (LBT-14 grants it).
type Lease struct {
	RoundID      [16]byte // not the nil UUID
	FencingToken int64    // >= 0; 0 is the round-creation token
}

func (l Lease) validate() error {
	switch {
	case l.RoundID == [16]byte{}:
		return fmt.Errorf("%w: nil round id", ErrInvalidRequest)
	case l.FencingToken < 0:
		return fmt.Errorf("%w: negative fencing token", ErrInvalidRequest)
	default:
		return nil
	}
}

// Outcome is the result of one ExecuteNext call.
type Outcome struct {
	// Executed is false when no admitted command was pending.
	Executed  bool
	Sequence  uint64
	CommandID [16]byte
	Result    chain.Result
	// Events are the decoded events of the rules version; do not mutate.
	Events []any
	// Record is the exact chain record bytes.
	Record []byte
	Hash   chain.Hash
	// Adopted reports that an earlier attempt whose outcome was unknown
	// committed the record.
	Adopted bool
	// Final reports that the policy considers the round final.
	Final bool
}

// lock returns the round's entry, locked. A dropped entry is replaced.
func (e *Executor) lock(roundID [16]byte) *entry {
	for {
		e.mu.Lock()
		en := e.entries[roundID]
		if en == nil {
			en = &entry{}
			e.entries[roundID] = en
		}
		e.mu.Unlock()
		en.mu.Lock()
		if !en.dropped.Load() {
			return en
		}
		en.mu.Unlock()
	}
}

// drop removes en from the map; a later call starts afresh.
func (e *Executor) drop(roundID [16]byte, en *entry) {
	en.dropped.Store(true)
	e.mu.Lock()
	if e.entries[roundID] == en {
		delete(e.entries, roundID)
	}
	e.mu.Unlock()
}

// Forget drops the round's in-memory entry, including a halt (E-14).
func (e *Executor) Forget(roundID [16]byte) {
	e.mu.Lock()
	en := e.entries[roundID]
	delete(e.entries, roundID)
	e.mu.Unlock()
	if en != nil {
		en.dropped.Store(true)
	}
}

// halt records err as the round's halt and returns it.
func halt(en *entry, err error) error {
	en.halt = err
	en.loaded = false
	en.pending = nil
	return err
}

// ExecuteNext executes exactly the next admitted command of lease.RoundID,
// the one at processed_sequence + 1, in one Transaction B (§5.1), or reports
// that none is pending (Outcome.Executed false). On ErrUnavailable, ErrBusy
// or a context error, calling again later is safe.
func (e *Executor) ExecuteNext(ctx context.Context, lease Lease) (Outcome, error) {
	if err := lease.validate(); err != nil {
		return Outcome{}, err
	}
	en := e.lock(lease.RoundID)
	defer en.mu.Unlock()
	if en.halt != nil {
		return Outcome{}, en.halt
	}

	var (
		lastErr error
		tr      *transition
		// checkFirst: the last COMMIT was ambiguous; start with the
		// conflict check.
		checkFirst bool
		wait       bool
	)
	for attempt := 1; attempt <= e.cfg.MaxAttempts; attempt++ {
		if wait {
			if err := e.sleep(ctx, backoff(attempt)); err != nil {
				return Outcome{}, contextError(ctx, err)
			}
		}
		wait = false

		if !en.loaded {
			tr, checkFirst = nil, false
			err := e.load(ctx, en, lease)
			switch {
			case err == nil:
			case errors.Is(err, errTransient):
				lastErr, wait = err, true
				continue
			case errors.Is(err, ErrFenced), errors.Is(err, ErrRoundNotFound), errors.Is(err, ErrInternal):
				e.drop(lease.RoundID, en)
				return Outcome{}, err
			case errors.Is(err, ErrCorrupt), errors.Is(err, ErrUnknownRules):
				return Outcome{}, halt(en, err)
			default:
				return Outcome{}, err
			}
		}
		k := en.processed + 1

		if checkFirst {
			checkFirst = false
			out, next, err := e.resolveConflict(ctx, lease, en, tr)
			switch next {
			case stepDone:
				return out, err
			case stepRetry:
				lastErr, wait = err, true
				continue
			case stepReload:
				lastErr = err
				continue
			}
			// stepProceed: nothing committed; run B again below.
		}

		if en.pending == nil || en.pending.sequence != k {
			adm, ok, err := e.readAdmission(ctx, lease.RoundID, k)
			if err != nil {
				switch classifyStatementError(ctx, err) {
				case stmtContext:
					return Outcome{}, contextError(ctx, err)
				case stmtRetry:
					lastErr, wait = err, true
					continue
				case stmtBusy:
					return Outcome{}, fmt.Errorf("%w: %w", ErrBusy, err)
				default:
					e.drop(lease.RoundID, en)
					return Outcome{}, fmt.Errorf("%w: read admission: %w", ErrInternal, err)
				}
			}
			if !ok {
				return Outcome{}, nil
			}
			en.pending = &adm
			tr = nil
		}

		if tr == nil || tr.sequence != k {
			var err error
			tr, err = computeTransition(en.rules, lease.RoundID, en.roundState, *en.pending, e.cfg.EngineBuild)
			if err != nil {
				return Outcome{}, halt(en, err)
			}
		}

		accounts, err := e.resolveAccounts(ctx, lease.RoundID, en, tr)
		if err != nil {
			switch {
			case errors.Is(err, ErrCorrupt), errors.Is(err, ErrFailClosed):
				return Outcome{}, halt(en, err)
			}
			switch classifyStatementError(ctx, err) {
			case stmtContext:
				return Outcome{}, contextError(ctx, err)
			case stmtRetry:
				lastErr, wait = err, true
				continue
			case stmtBusy:
				return Outcome{}, fmt.Errorf("%w: %w", ErrBusy, err)
			default:
				e.drop(lease.RoundID, en)
				return Outcome{}, fmt.Errorf("%w: resolve accounts: %w", ErrInternal, err)
			}
		}

		res, err := e.commit(ctx, lease, en, tr, accounts)
		switch res {
		case commitOK:
			return e.advance(lease.RoundID, en, tr, false), nil
		case commitResultConflict:
			out, next, cerr := e.resolveConflict(ctx, lease, en, tr)
			switch next {
			case stepDone:
				return out, cerr
			case stepRetry, stepProceed:
				lastErr, wait = err, true
			case stepReload:
				lastErr = err
			}
		case commitResultRetry:
			lastErr, wait = err, true
		case commitResultAmbiguous:
			lastErr, wait, checkFirst = err, true, true
		case commitResultBusy:
			return Outcome{}, fmt.Errorf("%w: %w", ErrBusy, err)
		case commitResultContext:
			return Outcome{}, contextError(ctx, err)
		default:
			e.drop(lease.RoundID, en)
			return Outcome{}, fmt.Errorf("%w: %w", ErrInternal, err)
		}
	}
	return Outcome{}, fmt.Errorf("%w: %w", ErrUnavailable, lastErr)
}

// Drain calls ExecuteNext until nothing is pending, limit commands ran, or
// an error; it returns the number executed.
func (e *Executor) Drain(ctx context.Context, lease Lease, limit int) (executed int, err error) {
	if limit < 1 {
		return 0, fmt.Errorf("%w: drain limit below 1", ErrInvalidRequest)
	}
	if err := lease.validate(); err != nil {
		return 0, err
	}
	for executed < limit {
		out, err := e.ExecuteNext(ctx, lease)
		if err != nil {
			return executed, err
		}
		if !out.Executed {
			return executed, nil
		}
		executed++
	}
	return executed, nil
}

// advance moves the entry past a committed or adopted transition and
// returns its Outcome. A final round's entry is dropped (E-3).
func (e *Executor) advance(roundID [16]byte, en *entry, tr *transition, adopted bool) Outcome {
	en.state = tr.next
	en.processed = tr.sequence
	en.head = tr.hash
	en.nextEventSequence += uint64(len(tr.events))
	en.pending = nil
	for p, id := range tr.betPlayers {
		if en.playerAccounts == nil {
			en.playerAccounts = map[string][16]byte{}
		}
		en.playerAccounts[p] = id
	}
	if tr.final {
		e.drop(roundID, en)
	}
	return Outcome{
		Executed:  true,
		Sequence:  tr.sequence,
		CommandID: tr.commandID,
		Result:    tr.result,
		Events:    tr.events,
		Record:    tr.record,
		Hash:      tr.hash,
		Adopted:   adopted,
		Final:     tr.final,
	}
}

// step tells ExecuteNext what follows a conflict check.
type step uint8

const (
	// stepDone: return the Outcome and error.
	stepDone step = iota
	// stepProceed: nothing committed; run Transaction B again now.
	stepProceed
	// stepRetry: back off, then retry.
	stepRetry
	// stepReload: the cache was dropped; reload without waiting.
	stepReload
)

// resolveConflict runs the conflict check (E-7) on a fresh connection and
// applies its table.
func (e *Executor) resolveConflict(ctx context.Context, lease Lease, en *entry, tr *transition) (Outcome, step, error) {
	k := en.processed + 1
	var h headView
	var processed, token *int64
	var hash []byte
	err := e.db.QueryRow(ctx, conflictSQL, lease.RoundID, int64(k)).Scan(&processed, &token, &hash) //nolint:gosec // G115: k = processed_sequence + 1, a bigint round sequence
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		switch classifyStatementError(ctx, err) {
		case stmtContext:
			return Outcome{}, stepDone, contextError(ctx, err)
		case stmtBusy:
			return Outcome{}, stepDone, fmt.Errorf("%w: %w", ErrBusy, err)
		case stmtRetry:
			return Outcome{}, stepRetry, err
		default:
			e.drop(lease.RoundID, en)
			return Outcome{}, stepDone, fmt.Errorf("%w: conflict check: %w", ErrInternal, err)
		}
	default:
		h = headView{found: processed != nil && token != nil, recordHash: hash}
		if h.found {
			h.processed, h.token = *processed, *token
		}
	}
	switch decideConflict(h, k, tr.hash, lease.FencingToken) {
	case conflictCorrupt:
		return Outcome{}, stepDone, halt(en, fmt.Errorf("%w: the round's execution head is missing", ErrCorrupt))
	case conflictFenced:
		e.drop(lease.RoundID, en)
		return Outcome{}, stepDone, ErrFenced
	case conflictAdopt:
		return e.advance(lease.RoundID, en, tr, true), stepDone, nil
	case conflictRetry:
		return Outcome{}, stepProceed, errNothingCommitted
	default:
		en.loaded = false
		en.pending = nil
		return Outcome{}, stepReload, errReload
	}
}

var (
	errNothingCommitted = errors.New("execution: conflict check: nothing committed")
	errReload           = errors.New("execution: conflict check: another transition is committed; reloading")
	// errTransient marks a cold-load failure worth retrying.
	errTransient = errors.New("execution: transient database error")
)

// conflictSQL is E-7's single-statement conflict check.
const conflictSQL = `SELECT h.processed_sequence, h.fencing_token, r.hash
	FROM orvex.round_execution_heads h
	LEFT JOIN orvex.chain_records r ON r.round_id = h.round_id AND r.sequence = $2
	WHERE h.round_id = $1`

// contextError returns the error of a call whose ctx ended (E-15): it wraps
// ctx.Err(), so errors.Is(err, ctx.Err()) holds even when the driver's error
// does not wrap it (a 57014 PgError, a closed connection), and it keeps the
// driver's error reachable through errors.Is and errors.As.
func contextError(ctx context.Context, err error) error {
	ctxErr := ctx.Err()
	switch {
	case ctxErr == nil, errors.Is(err, ctxErr):
		return fmt.Errorf("execution: %w", err)
	default:
		return fmt.Errorf("execution: %w: %w", ctxErr, err)
	}
}

// resolveAccounts maps every posting side to an account ID (§5.5). It
// returns ErrCorrupt for a missing player account, a *FailClosedError when
// both sides resolve to one account, or a database error.
func (e *Executor) resolveAccounts(ctx context.Context, roundID [16]byte, en *entry, tr *transition) ([][2][16]byte, error) {
	out := make([][2][16]byte, len(tr.postings))
	for i, p := range tr.postings {
		for side, a := range []ledger.Account{p.Debit, p.Credit} {
			var id [16]byte
			switch {
			case a.Kind == ledger.AccountRound:
				id = ledger.RoundAccountID(roundID)
			case a.Kind == ledger.AccountHouse:
				id = ledger.HouseAccountID()
			case side == 0 && p.FromHold:
				id = p.holdAccount
			default:
				var err error
				if id, err = e.playerAccount(ctx, en, a.Player); err != nil {
					return nil, err
				}
			}
			out[i][side] = id
		}
		if out[i][0] == out[i][1] {
			return nil, &FailClosedError{RoundID: roundID, Sequence: tr.sequence,
				Err: fmt.Errorf("%w: posting %d resolves to one account on both sides", errBadPosting, i)}
		}
	}
	return out, nil
}

// playerAccount returns the account of player, memoized per round (E-4).
func (e *Executor) playerAccount(ctx context.Context, en *entry, player string) ([16]byte, error) {
	if id, ok := en.playerAccounts[player]; ok {
		return id, nil
	}
	var id [16]byte
	err := e.db.QueryRow(ctx, `SELECT account_id FROM orvex.ledger_accounts WHERE kind = 'player' AND player_id = $1`, player).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, fmt.Errorf("%w: a payout's player has no account", ErrCorrupt)
	}
	if err != nil {
		return id, err
	}
	if en.playerAccounts == nil {
		en.playerAccounts = map[string][16]byte{}
	}
	en.playerAccounts[player] = id
	return id, nil
}

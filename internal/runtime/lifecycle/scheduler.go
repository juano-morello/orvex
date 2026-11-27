package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// MaxBettingWindow bounds Config.BettingWindow.
const MaxBettingWindow = time.Hour

// leaseTTL bounds how long a standby waits after an unclean exit.
const leaseTTL = 6 * time.Second

var (
	// ErrInvalidConfig: New was given a nil dependency or a Config outside
	// its ranges.
	ErrInvalidConfig = errors.New("lifecycle: invalid config")
	// errHalted marks a round that stays unsettled until an operator acts.
	errHalted = errors.New("lifecycle: round halted")
	// errLeadershipLost: the leadership session ended.
	errLeadershipLost = errors.New("lifecycle: scheduler leadership lost")
	// errNotExecuted: every admission through a lifecycle command's own
	// executed, and the command is still pending.
	errNotExecuted = errors.New("lifecycle: admitted lifecycle command not executed yet")
)

// Config tunes the Scheduler.
type Config struct {
	// BettingWindow is the time from a round's creation to its start
	// command, in [0, MaxBettingWindow]. It is scheduling policy: Decide
	// accepts bets until the start command's sequence, whenever it runs.
	BettingWindow time.Duration
	// CreateRounds lets the scheduler create a round when no unsettled one
	// exists. Off, it only drives the unsettled rounds it finds.
	CreateRounds bool
	// Logger receives the scheduler's log lines; nil discards them.
	Logger *slog.Logger
}

// timing holds the scheduler's polling and backoff intervals.
type timing struct {
	// running and waiting are the driver's ticks while RUNNING and
	// otherwise: executing admitted player commands needs no wakeup.
	running, waiting time.Duration
	// idle is the discovery interval when there is nothing to drive or
	// create.
	idle time.Duration
	// acquire is the leadership attempt interval of a standby; watch is the
	// leader's session check interval.
	acquire, watch time.Duration
	// maxBackoff caps the wait after consecutive failures.
	maxBackoff time.Duration
}

var defaultTiming = timing{
	running:    20 * time.Millisecond,
	waiting:    100 * time.Millisecond,
	idle:       time.Second,
	acquire:    time.Second,
	watch:      time.Second,
	maxBackoff: 2 * time.Second,
}

// drainBatch bounds the commands a driver executes between two checks of
// its round's phase and the database clock.
const drainBatch = 64

// Scheduler creates and drives crash/v1 rounds while it leads.
type Scheduler struct {
	pool   *pgxpool.Pool
	x      *execution.Executor
	seq    *admission.Sequencer
	cfg    Config
	log    *slog.Logger
	owner  string
	timing timing
	// seed draws a new round's seed; tests replace it.
	seed func(roundID [16]byte) [32]byte
}

// New returns a scheduler over pool, executing with x and admitting its
// lifecycle commands with seq. Its owner ID, written to every round it
// claims, is "engine/" and 16 random hex digits.
func New(pool *pgxpool.Pool, x *execution.Executor, seq *admission.Sequencer, cfg Config) (*Scheduler, error) {
	switch {
	case pool == nil || x == nil || seq == nil:
		return nil, fmt.Errorf("%w: nil pool, executor or sequencer", ErrInvalidConfig)
	case cfg.BettingWindow < 0 || cfg.BettingWindow > MaxBettingWindow:
		return nil, fmt.Errorf("%w: BettingWindow outside [0, %v]", ErrInvalidConfig, MaxBettingWindow)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &Scheduler{
		pool: pool, x: x, seq: seq, cfg: cfg, log: log,
		owner:  "engine/" + hex.EncodeToString(b[:]),
		timing: defaultTiming,
		seed:   randomSeed,
	}, nil
}

// Run leads and schedules until ctx ends, then returns nil. While another
// session leads it waits; when its own leadership is lost it tries again.
// It returns an error wrapping execution.ErrFenced when another engine took
// over a round it was driving, and any error that is a bug.
func (s *Scheduler) Run(ctx context.Context) error {
	connConfig := s.pool.Config().ConnConfig
	for {
		l, err := s.acquire(ctx, connConfig)
		if err != nil {
			return nil
		}
		s.log.Info("scheduler leads", "owner", s.owner)
		err = s.lead(ctx, l)
		l.release()
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, errLeadershipLost):
			s.log.Warn("scheduler leadership lost; trying again", "owner", s.owner)
		default:
			return err
		}
	}
}

// acquire waits for the leadership; its only error is ctx's.
func (s *Scheduler) acquire(ctx context.Context, cfg *pgx.ConnConfig) (*leadership, error) {
	waiting := false
	for {
		l, err := tryLead(ctx, cfg)
		switch {
		case l != nil:
			return l, nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil:
			s.log.Warn("scheduler leadership attempt failed", "error", Describe(err))
		case !waiting:
			s.log.Info("another scheduler leads; waiting")
			waiting = true
		}
		if err := sleepCtx(ctx, s.timing.acquire); err != nil {
			return nil, err
		}
	}
}

// lead schedules while l holds; it returns errLeadershipLost once the
// session check fails.
func (s *Scheduler) lead(ctx context.Context, l *leadership) error {
	lctx, cancel := context.WithCancelCause(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { l.watch(lctx, s.timing.watch, func() { cancel(errLeadershipLost) }) })
	err := s.schedule(lctx, l)
	if lctx.Err() != nil && ctx.Err() == nil {
		err = context.Cause(lctx)
	}
	cancel(nil)
	wg.Wait()
	return err
}

// round is an unsettled round: its ID and the genesis created_at, which
// opens its betting window.
type round struct {
	id                 [16]byte
	createdAtUnixMicro int64
}

// discoverSQL lists the crash rounds without a reveal: the reveal is
// written by the record that settles a round (LBT-21 K-6).
const discoverSQL = `SELECT r.round_id, r.created_at_unix_micro FROM orvex.rounds r
	WHERE r.game = $1
	  AND NOT EXISTS (SELECT 1 FROM orvex.round_secret_reveals v WHERE v.round_id = r.round_id)
	ORDER BY r.created_at_unix_micro, r.round_id`

// schedule drives every unsettled round to settlement, oldest first, and
// creates a round when none is left, CreateRounds is on and no round is
// halted.
func (s *Scheduler) schedule(ctx context.Context, l *leadership) error {
	halted := map[[16]byte]bool{}
	failures := 0
	for {
		rows, err := s.pool.Query(ctx, discoverSQL, string(v1.GameID))
		var rounds []round
		if err == nil {
			rounds, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (round, error) {
				var r round
				return r, row.Scan(&r.id, &r.createdAtUnixMicro)
			})
		}
		if err != nil {
			if err := s.failed(ctx, "discover rounds", err, &failures); err != nil {
				return err
			}
			continue
		}
		failures = 0

		drove := false
		for _, r := range rounds {
			if halted[r.id] {
				continue
			}
			drove = true
			err := s.drive(ctx, r)
			if errors.Is(err, errHalted) {
				halted[r.id] = true
				s.log.Error("round halted; it blocks new rounds until an operator resolves it",
					"round", roundText(r.id), "error", Describe(err))
				continue
			}
			if err != nil {
				return err
			}
		}
		switch {
		case drove:
		case len(rounds) > 0 || !s.cfg.CreateRounds:
			if err := sleepCtx(ctx, s.timing.idle); err != nil {
				return err
			}
		default:
			if err := s.create(ctx, l); err != nil {
				return err
			}
		}
	}
}

// Limits of the rounds the scheduler creates: the widest crash/v1 allows.
const (
	roundMinBetCredits = v1.MinBetCredits
	roundMaxBetCredits = v1.MaxBetCredits
	roundMaxBets       = v1.MaxBetsPerRound
)

// create draws one round plan and sends it unchanged until CreateRound
// answers definitely; a commit whose outcome was unknown answers Duplicate
// on the retry. It checks the leadership before each attempt. If the plan
// is lost with the process, discovery adopts the round it may have
// committed.
func (s *Scheduler) create(ctx context.Context, l *leadership) error {
	var createdAt int64
	for failures := 0; ; {
		var err error
		if createdAt, err = s.now(ctx); err == nil {
			break
		}
		if err := s.failed(ctx, "read the database clock", err, &failures); err != nil {
			return err
		}
	}
	id := newRoundID()
	plan := execution.NewRound{
		RoundID: id, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config: v1.Config{
			RoundID: id, Seed: s.seed(id),
			MinBetCredits: roundMinBetCredits, MaxBetCredits: roundMaxBetCredits, MaxBets: roundMaxBets,
		},
		CreatedAtUnixMicro: createdAt,
	}
	for failures := 0; ; {
		if err := l.check(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errLeadershipLost
		}
		_, err := s.x.CreateRound(ctx, plan)
		switch {
		case err == nil:
			s.log.Info("round created", "round", roundText(id))
			return nil
		case errors.Is(err, execution.ErrRoundExists):
			s.log.Warn("round ID already holds another round; drawing a new plan", "round", roundText(id))
			return nil
		case errors.Is(err, execution.ErrInvalidRound), errors.Is(err, execution.ErrUnknownRules):
			return err
		}
		if err := s.failed(ctx, "create round", err, &failures, "round", roundText(id)); err != nil {
			return err
		}
	}
}

// now reads the database clock, the clock of admission effective times.
func (s *Scheduler) now(ctx context.Context) (int64, error) {
	var t int64
	err := s.pool.QueryRow(ctx, `SELECT orvex.unix_micro(clock_timestamp())`).Scan(&t)
	return t, err
}

// haltErrors halt the round they occur for: stored data or rules that fail
// closed, or a request the engine itself built wrong.
var haltErrors = []error{
	execution.ErrCorrupt, execution.ErrFailClosed, execution.ErrUnknownRules,
	execution.ErrRoundNotFound, execution.ErrInvalidRequest,
	admission.ErrInternal, admission.ErrInvalidRequest, admission.ErrInvalidCommand,
	admission.ErrUnknownRules, admission.ErrRoundNotFound, admission.ErrCommandIDReused,
}

// failed handles an error of one of the scheduler's loops. It returns ctx's
// error once ctx has ended, err itself when it halts the round or stops the
// scheduler, and otherwise logs err, waits out the backoff and returns nil
// for the caller to try again.
func (s *Scheduler) failed(ctx context.Context, what string, err error, failures *int, attrs ...any) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errHalted) || errors.Is(err, execution.ErrFenced) {
		return err
	}
	for _, h := range haltErrors {
		if errors.Is(err, h) {
			return fmt.Errorf("%w: %w", errHalted, err)
		}
	}
	*failures++
	s.log.Warn(what+" failed; retrying", append(attrs, "error", Describe(err))...)
	wait := s.timing.maxBackoff
	if *failures < 8 {
		wait = min(wait, 20*time.Millisecond<<*failures)
	}
	return sleepCtx(ctx, wait)
}

// randomSeed draws a crash/v1 seed with crypto/rand; the all-zero seed is
// invalid (math spec D-10).
func randomSeed([16]byte) [32]byte {
	var seed [32]byte
	for seed == ([32]byte{}) {
		_, _ = rand.Read(seed[:])
	}
	return seed
}

// newRoundID returns a random RFC 9562 UUIDv4.
func newRoundID() [16]byte {
	var id [16]byte
	_, _ = rand.Read(id[:])
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id
}

func roundText(id [16]byte) string { return string(canonjson.UUID(id)) }

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

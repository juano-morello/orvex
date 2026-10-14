package v1

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/juano-morello/orvex/internal/game"
)

// Registry keys of crash/v1. ADR-003 keeps the rules, schema and state
// versions separate. RulesVersionNumber is the numeric registry key paired
// with the textual RulesVersion.
const (
	GameID             game.ID            = "crash"
	RulesVersionNumber game.RulesVersion  = 1
	SchemaVersion      game.SchemaVersion = 1
	StateVersion       game.StateVersion  = 1
)

// Descriptor returns the registry descriptor of crash/v1.
func Descriptor() game.Descriptor {
	return game.Descriptor{
		Game:          GameID,
		RulesVersion:  RulesVersionNumber,
		SchemaVersion: SchemaVersion,
		StateVersion:  StateVersion,
	}
}

// Adapter returns the type-erased crash/v1 adapter for game.NewRegistry.
func Adapter() game.Adapter {
	return game.Adapt(Descriptor(), Rules{}, Codec{})
}

// MaxBetsPerRound is the hard crash/v1 limit on bets in one round (LBT-8;
// resolves the aggregate item of math spec D-7). Config.MaxBets may narrow
// it, never widen it.
const MaxBetsPerRound int64 = 10_000

// Phase is the lifecycle phase of a round. A round moves forward only:
// BETTING, RUNNING, CRASHED, SETTLED.
type Phase uint8

const (
	// PhaseInvalid is the zero value; it is never a valid state.
	PhaseInvalid Phase = iota
	// PhaseBetting accepts bets until StartRound.
	PhaseBetting
	// PhaseRunning accepts cash-outs before the crash deadline.
	PhaseRunning
	// PhaseCrashed has revealed the crash outcome and awaits settlement.
	PhaseCrashed
	// PhaseSettled is terminal: no command or event applies.
	PhaseSettled
)

// String returns the phase name.
func (p Phase) String() string {
	switch p {
	case PhaseBetting:
		return "BETTING"
	case PhaseRunning:
		return "RUNNING"
	case PhaseCrashed:
		return "CRASHED"
	case PhaseSettled:
		return "SETTLED"
	default:
		return "INVALID"
	}
}

// BetStatus is the status of one bet.
type BetStatus uint8

const (
	// StatusInvalid is the zero value; it is never valid.
	StatusInvalid BetStatus = iota
	// StatusOpen is a bet that has neither cashed out nor lost.
	StatusOpen
	// StatusCashedOut is a bet paid by BetCashedOut.
	StatusCashedOut
	// StatusLost is a bet lost at settlement.
	StatusLost
)

// PlayerID is an opaque, non-empty player identifier set by the runtime from
// the authenticated session.
type PlayerID string

// Config is the recorded round-creation input. The runtime generates Seed
// with crypto/rand (math spec D-10) and records it; it is secret until
// RoundSettled.
type Config struct {
	// RoundID holds the round's UUID bytes (math spec D-11); not the nil UUID.
	RoundID [16]byte
	// Seed is the round's secret server seed; not all zero.
	Seed [32]byte
	// MinBetCredits is the round's smallest bet, in [MinBetCredits, MaxBetCredits].
	MinBetCredits int64
	// MaxBetCredits is the round's largest bet, in [Config.MinBetCredits, MaxBetCredits].
	MaxBetCredits int64
	// MaxBets is the round's bet limit, in [1, MaxBetsPerRound].
	MaxBets int64
}

// Bet is one player's bet. It holds no secret. The cash-out fields are zero
// unless Status is StatusCashedOut.
type Bet struct {
	Player                  PlayerID
	BetCredits              int64
	PlacedAtUnixMicro       int64
	Status                  BetStatus
	CashedOutAtUnixMicro    int64
	CashOutTick             int64
	CashOutMultiplierScaled int64
	PayoutCredits           int64
}

// State is the authoritative, engine-internal state of one round. From
// InitialState on it holds the secret seed and crash outcome: never send it
// to clients; publish Project(state) instead (ADR-003, math spec section 7).
type State struct {
	RoundID       [16]byte
	Phase         Phase
	MinBetCredits int64
	MaxBetCredits int64
	MaxBets       int64

	// Commitment is SHA-256(Seed), public from round creation.
	Commitment [32]byte
	// Seed is secret until RoundSettled.
	Seed [32]byte

	// The crash outcome is secret until RoundCrashed (math spec section 7).
	CrashPointScaled          int64
	CrashTick                 int64
	CrashDeadlineOffsetMicros int64
	// CrashDeadlineUnixMicro is T0 + D, set by RoundStarted.
	CrashDeadlineUnixMicro int64

	// StartedAtUnixMicro is T0, set by RoundStarted.
	StartedAtUnixMicro int64
	// ClosedAtUnixMicro is set by RoundCrashed.
	ClosedAtUnixMicro int64
	// SettledAtUnixMicro is set by RoundSettled.
	SettledAtUnixMicro int64

	// Bets are in placement order, at most one per player.
	Bets               []Bet
	TotalBetCredits    int64
	TotalPayoutCredits int64
}

var (
	// ErrInvalidConfig reports a Config InitialState cannot build a round from.
	ErrInvalidConfig = errors.New("crash/v1: invalid round config")
	// ErrInvalidState reports a state no sequence of crash/v1 events builds.
	// Callers fail closed.
	ErrInvalidState = errors.New("crash/v1: invalid round state")
	// ErrEventNotApplicable reports an event Evolve cannot apply to the state
	// (ADR-003 "validated event applicability"). Replay treats it as failure.
	ErrEventNotApplicable = errors.New("crash/v1: event does not apply to round state")
	// ErrRejected is matched by every *RejectionError: a deterministic command
	// rejection recorded as a terminal command result (ADR-003).
	ErrRejected = errors.New("crash/v1: command rejected")
)

// Rules implements game.Game[State, Command, Event, Config] for crash/v1
// (ADR-003, ADR-004, LBT-8; spec docs/specs/712614a2_crash-lifecycle-rules.md).
//
// A round moves BETTING -> RUNNING -> CRASHED -> SETTLED. Players place one
// bet each while BETTING and cash out while RUNNING strictly before the
// committed crash deadline T0 + D (deadline equality loses). The trusted
// scheduler issues StartRound, CloseRound and SettleRound with
// AuthoritySystem; players issue PlaceBet and CashOut with AuthorityPlayer.
// After settlement no command or event applies.
//
// State carries the seed and crash outcome from InitialState on. Publish
// Project(state): it hides the outcome until RoundCrashed and the seed until
// RoundSettled. No error text formats a secret value.
//
// Rules holds no state; InitialState, Decide and Evolve are pure.
type Rules struct{}

// InitialState validates cfg and derives the round's commitment and crash
// outcome from the seed and round ID (math spec D-9, D-10). The round starts
// in PhaseBetting with no bets.
func (Rules) InitialState(cfg Config) (State, error) {
	switch {
	case cfg.RoundID == [16]byte{}:
		return State{}, fmt.Errorf("%w: round id is the nil uuid", ErrInvalidConfig)
	case cfg.Seed == [32]byte{}:
		return State{}, fmt.Errorf("%w: seed is all zero", ErrInvalidConfig)
	case cfg.MinBetCredits < MinBetCredits:
		return State{}, fmt.Errorf("%w: min bet %d below %d", ErrInvalidConfig, cfg.MinBetCredits, MinBetCredits)
	case cfg.MaxBetCredits > MaxBetCredits:
		return State{}, fmt.Errorf("%w: max bet %d above %d", ErrInvalidConfig, cfg.MaxBetCredits, MaxBetCredits)
	case cfg.MinBetCredits > cfg.MaxBetCredits:
		return State{}, fmt.Errorf("%w: min bet %d above max bet %d", ErrInvalidConfig, cfg.MinBetCredits, cfg.MaxBetCredits)
	case cfg.MaxBets < 1 || cfg.MaxBets > MaxBetsPerRound:
		return State{}, fmt.Errorf("%w: max bets %d not in [1, %d]", ErrInvalidConfig, cfg.MaxBets, MaxBetsPerRound)
	}

	// The inner errors of CrashTick and DeadlineOffsetMicros format the
	// secret crash point or tick, so they are dropped (math spec section 7).
	c := CrashPoint(cfg.Seed, cfg.RoundID)
	n, err := CrashTick(c)
	if err != nil {
		return State{}, fmt.Errorf("%w: derive crash outcome", ErrArithmetic)
	}
	d, err := DeadlineOffsetMicros(n)
	if err != nil {
		return State{}, fmt.Errorf("%w: derive crash outcome", ErrArithmetic)
	}

	return State{
		RoundID:                   cfg.RoundID,
		Phase:                     PhaseBetting,
		MinBetCredits:             cfg.MinBetCredits,
		MaxBetCredits:             cfg.MaxBetCredits,
		MaxBets:                   cfg.MaxBets,
		Commitment:                Commitment(cfg.Seed),
		Seed:                      cfg.Seed,
		CrashPointScaled:          c,
		CrashTick:                 n,
		CrashDeadlineOffsetMicros: d,
	}, nil
}

// addNonNeg returns a+b for non-negative operands, failing on overflow. The
// error never formats the operands: one of them may be the secret D.
func addNonNeg(a, b int64) (int64, error) {
	if a < 0 || b < 0 || b > math.MaxInt64-a {
		return 0, fmt.Errorf("%w: int64 addition overflows", ErrArithmetic)
	}
	return a + b, nil
}

// findBet returns the index of p's bet in bets, or -1.
func findBet(bets []Bet, p PlayerID) int {
	return slices.IndexFunc(bets, func(b Bet) bool { return b.Player == p })
}

// validPhase reports whether p is one of the four lifecycle phases.
func validPhase(p Phase) bool {
	return p >= PhaseBetting && p <= PhaseSettled
}

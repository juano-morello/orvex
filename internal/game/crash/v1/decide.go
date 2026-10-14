package v1

import (
	"errors"
	"fmt"

	"github.com/juano-morello/orvex/internal/game"
)

// Reason is the stable code of a deterministic command rejection, recorded
// as the terminal command result (ADR-003). The codes are frozen with the
// rules version.
type Reason string

// Rejection reasons of crash/v1.
const (
	// ReasonInvalidCommand: nil or unknown command type, or an empty player.
	ReasonInvalidCommand Reason = "invalid_command"
	// ReasonUnauthorized: the command's Authority is not the one it requires.
	ReasonUnauthorized Reason = "unauthorized"
	// ReasonTimeOutOfRange: a negative effective time, or a StartRound time
	// above MaxStartMicros (math spec D-7).
	ReasonTimeOutOfRange Reason = "time_out_of_range"
	// ReasonWrongPhase: the command does not apply in the round's phase.
	ReasonWrongPhase Reason = "wrong_phase"
	// ReasonRoundSettled: any command after settlement.
	ReasonRoundSettled Reason = "round_settled"
	// ReasonInvalidAmount: a bet outside the round's limits (math spec D-7).
	ReasonInvalidAmount Reason = "invalid_amount"
	// ReasonDuplicateBet: the player already has a bet in the round.
	ReasonDuplicateBet Reason = "duplicate_bet"
	// ReasonBetLimitReached: the round already holds MaxBets bets.
	ReasonBetLimitReached Reason = "bet_limit_reached"
	// ReasonNoBet: the player has no bet in the round.
	ReasonNoBet Reason = "no_bet"
	// ReasonAlreadyCashedOut: the player's bet has already cashed out.
	ReasonAlreadyCashedOut Reason = "already_cashed_out"
	// ReasonNotAfterStart: a cash-out at or before T0 (ADR-002, math spec D-9).
	ReasonNotAfterStart Reason = "not_after_start"
	// ReasonDeadlinePassed: a cash-out at or after T0 + D; equality loses.
	ReasonDeadlinePassed Reason = "deadline_passed"
	// ReasonCrashNotDue: CloseRound before T0 + D.
	ReasonCrashNotDue Reason = "crash_not_due"
)

// RejectionError is a deterministic command rejection. Its text is the
// reason code only, so it never carries a secret.
type RejectionError struct{ Reason Reason }

func (e *RejectionError) Error() string { return "crash/v1: command rejected: " + string(e.Reason) }

// Unwrap makes errors.Is(err, ErrRejected) hold for every rejection.
func (e *RejectionError) Unwrap() error { return ErrRejected }

// RejectionReason reports the reason of a rejection returned by Decide.
func RejectionReason(err error) (Reason, bool) {
	var r *RejectionError
	if !errors.As(err, &r) {
		return "", false
	}
	return r.Reason, true
}

func reject(r Reason) ([]Event, error) {
	return nil, &RejectionError{Reason: r}
}

// Decide validates c against s at the effective admission time
// in.EffectiveTimeUnixMicro and returns the events it produces. A
// *RejectionError (errors.Is ErrRejected) is a terminal command result
// without events. Any other error (ErrInvalidState, ErrArithmetic) means
// fail closed: no events and no terminal result (math spec D-7).
//
// Checks run in a fixed order and the first failure wins: phase validity,
// settlement, command type, authority, time range, phase, then the
// command-specific checks.
func (Rules) Decide(s State, c Command, in game.Input) ([]Event, error) {
	if !validPhase(s.Phase) {
		return nil, fmt.Errorf("%w: unknown phase", ErrInvalidState)
	}
	if s.Phase == PhaseSettled {
		return reject(ReasonRoundSettled)
	}

	var (
		have, need Authority
		phase      Phase
	)
	switch c := c.(type) {
	case PlaceBet:
		have, need, phase = c.Authority, AuthorityPlayer, PhaseBetting
	case StartRound:
		have, need, phase = c.Authority, AuthoritySystem, PhaseBetting
	case CashOut:
		have, need, phase = c.Authority, AuthorityPlayer, PhaseRunning
	case CloseRound:
		have, need, phase = c.Authority, AuthoritySystem, PhaseRunning
	case SettleRound:
		have, need, phase = c.Authority, AuthoritySystem, PhaseCrashed
	default:
		return reject(ReasonInvalidCommand)
	}
	if have != need {
		return reject(ReasonUnauthorized)
	}
	t := in.EffectiveTimeUnixMicro
	if t < 0 {
		return reject(ReasonTimeOutOfRange)
	}
	if s.Phase != phase {
		return reject(ReasonWrongPhase)
	}

	switch c := c.(type) {
	case PlaceBet:
		return decidePlaceBet(s, c, t)
	case StartRound:
		return decideStartRound(s, t)
	case CashOut:
		return decideCashOut(s, c, t)
	case CloseRound:
		return decideCloseRound(s, t)
	default: // SettleRound; the first switch admitted only the five commands.
		return decideSettleRound(s, t), nil
	}
}

func decidePlaceBet(s State, c PlaceBet, t int64) ([]Event, error) {
	if c.Player == "" {
		return reject(ReasonInvalidCommand)
	}
	if c.BetCredits < s.MinBetCredits || c.BetCredits > s.MaxBetCredits {
		return reject(ReasonInvalidAmount)
	}
	if findBet(s.Bets, c.Player) >= 0 {
		return reject(ReasonDuplicateBet)
	}
	if int64(len(s.Bets)) >= s.MaxBets {
		return reject(ReasonBetLimitReached)
	}
	if _, err := addNonNeg(s.TotalBetCredits, c.BetCredits); err != nil {
		return nil, err
	}
	return []Event{BetPlaced{Player: c.Player, BetCredits: c.BetCredits, PlacedAtUnixMicro: t}}, nil
}

func decideStartRound(s State, t int64) ([]Event, error) {
	if t > MaxStartMicros {
		return reject(ReasonTimeOutOfRange)
	}
	if _, err := addNonNeg(t, s.CrashDeadlineOffsetMicros); err != nil {
		return nil, err
	}
	return []Event{RoundStarted{StartedAtUnixMicro: t}}, nil
}

func decideCashOut(s State, c CashOut, t int64) ([]Event, error) {
	if c.Player == "" {
		return reject(ReasonInvalidCommand)
	}
	i := findBet(s.Bets, c.Player)
	if i < 0 {
		return reject(ReasonNoBet)
	}
	bet := s.Bets[i]
	switch bet.Status {
	case StatusOpen:
	case StatusCashedOut:
		return reject(ReasonAlreadyCashedOut)
	default:
		return nil, fmt.Errorf("%w: bet status %d while running", ErrInvalidState, bet.Status)
	}
	if t <= s.StartedAtUnixMicro {
		return reject(ReasonNotAfterStart)
	}
	tick, m, p, eligible, err := cashOutAmounts(s, bet.BetCredits, t)
	if err != nil {
		return nil, err
	}
	if !eligible {
		return reject(ReasonDeadlinePassed)
	}
	if _, err := addNonNeg(s.TotalPayoutCredits, p); err != nil {
		return nil, err
	}
	return []Event{BetCashedOut{
		Player:                  c.Player,
		CashedOutAtUnixMicro:    t,
		CashOutTick:             tick,
		CashOutMultiplierScaled: m,
		PayoutCredits:           p,
	}}, nil
}

// cashOutAmounts returns the tick, multiplier and payout of a cash-out of
// betCredits at effective time t, or eligible == false at or after the crash
// deadline (math spec D-8, D-9). Decide and Evolve share it.
func cashOutAmounts(s State, betCredits, t int64) (tick, m, p int64, eligible bool, err error) {
	tick, eligible, err = CashOutTiming(s.StartedAtUnixMicro, t, s.CrashTick)
	if err != nil {
		// The inner error formats the secret crash tick (math spec section 7).
		return 0, 0, 0, false, fmt.Errorf("%w: cash-out timing", ErrInvalidState)
	}
	if !eligible {
		return 0, 0, 0, false, nil
	}
	// These errors format only the public tick, bet and multiplier.
	m, err = MultiplierAtTick(tick)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("%w: cash-out payout: %w", ErrInvalidState, err)
	}
	p, err = Payout(betCredits, m)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("%w: cash-out payout: %w", ErrInvalidState, err)
	}
	return tick, m, p, true, nil
}

func decideCloseRound(s State, t int64) ([]Event, error) {
	if t < s.CrashDeadlineUnixMicro {
		return reject(ReasonCrashNotDue)
	}
	return []Event{RoundCrashed{
		ClosedAtUnixMicro:         t,
		CrashPointScaled:          s.CrashPointScaled,
		CrashTick:                 s.CrashTick,
		CrashDeadlineOffsetMicros: s.CrashDeadlineOffsetMicros,
		CrashDeadlineUnixMicro:    s.CrashDeadlineUnixMicro,
	}}, nil
}

func decideSettleRound(s State, t int64) []Event {
	events := make([]Event, 0, len(s.Bets)+1)
	for _, b := range s.Bets {
		if b.Status == StatusOpen {
			events = append(events, BetLost{Player: b.Player, BetCredits: b.BetCredits})
		}
	}
	return append(events, RoundSettled{
		SettledAtUnixMicro: t,
		BetCount:           int64(len(s.Bets)),
		TotalBetCredits:    s.TotalBetCredits,
		TotalPayoutCredits: s.TotalPayoutCredits,
		Seed:               s.Seed,
	})
}

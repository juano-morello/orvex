package v1

import (
	"fmt"
	"slices"
)

// Evolve applies e to s and returns the next state. It accepts an event only
// if Decide could have produced it from s for some valid input (ADR-003
// "validated event applicability"); otherwise it returns State{} and an error
// wrapping ErrEventNotApplicable (or ErrInvalidState / ErrArithmetic when s
// itself is broken).
//
// Evolve is copy-on-write: it never writes into s.Bets, and every returned
// state has a fresh Bets backing array, so callers can never alias the input.
func (Rules) Evolve(s State, e Event) (State, error) {
	if !validPhase(s.Phase) {
		return State{}, fmt.Errorf("%w: unknown phase", ErrInvalidState)
	}
	if s.Phase == PhaseSettled {
		return State{}, fmt.Errorf("%w: round settled", ErrEventNotApplicable)
	}

	var (
		next State
		err  error
	)
	switch e := e.(type) {
	case BetPlaced:
		next, err = evolveBetPlaced(s, e)
	case RoundStarted:
		next, err = evolveRoundStarted(s, e)
	case BetCashedOut:
		next, err = evolveBetCashedOut(s, e)
	case RoundCrashed:
		next, err = evolveRoundCrashed(s, e)
	case BetLost:
		next, err = evolveBetLost(s, e)
	case RoundSettled:
		next, err = evolveRoundSettled(s, e)
	default:
		return State{}, fmt.Errorf("%w: unknown event type %T", ErrEventNotApplicable, e)
	}
	if err != nil {
		return State{}, err
	}
	return next, nil
}

func notApplicable(event, reason string) error {
	return fmt.Errorf("%w: %s: %s", ErrEventNotApplicable, event, reason)
}

// cloneState returns a shallow copy of s with a fresh Bets backing array.
func cloneState(s State) State {
	next := s
	next.Bets = slices.Clone(s.Bets)
	return next
}

func evolveBetPlaced(s State, e BetPlaced) (State, error) {
	const name = "BetPlaced"
	switch {
	case s.Phase != PhaseBetting:
		return State{}, notApplicable(name, "phase is not BETTING")
	case e.Player == "":
		return State{}, notApplicable(name, "empty player")
	case findBet(s.Bets, e.Player) >= 0:
		return State{}, notApplicable(name, "player already has a bet")
	case e.BetCredits < s.MinBetCredits || e.BetCredits > s.MaxBetCredits:
		return State{}, notApplicable(name, "bet outside the round limits")
	case int64(len(s.Bets)) >= s.MaxBets:
		return State{}, notApplicable(name, "bet limit reached")
	case e.PlacedAtUnixMicro < 0:
		return State{}, notApplicable(name, "negative placement time")
	}
	total, err := addNonNeg(s.TotalBetCredits, e.BetCredits)
	if err != nil {
		return State{}, err
	}
	next := s
	next.Bets = make([]Bet, len(s.Bets), len(s.Bets)+1)
	copy(next.Bets, s.Bets)
	next.Bets = append(next.Bets, Bet{
		Player:            e.Player,
		BetCredits:        e.BetCredits,
		PlacedAtUnixMicro: e.PlacedAtUnixMicro,
		Status:            StatusOpen,
	})
	next.TotalBetCredits = total
	return next, nil
}

func evolveRoundStarted(s State, e RoundStarted) (State, error) {
	const name = "RoundStarted"
	switch {
	case s.Phase != PhaseBetting:
		return State{}, notApplicable(name, "phase is not BETTING")
	case e.StartedAtUnixMicro < 0 || e.StartedAtUnixMicro > MaxStartMicros:
		return State{}, notApplicable(name, "start time out of range")
	}
	deadline, err := addNonNeg(e.StartedAtUnixMicro, s.CrashDeadlineOffsetMicros)
	if err != nil {
		return State{}, err
	}
	next := cloneState(s)
	next.Phase = PhaseRunning
	next.StartedAtUnixMicro = e.StartedAtUnixMicro
	next.CrashDeadlineUnixMicro = deadline
	return next, nil
}

func evolveBetCashedOut(s State, e BetCashedOut) (State, error) {
	const name = "BetCashedOut"
	if s.Phase != PhaseRunning {
		return State{}, notApplicable(name, "phase is not RUNNING")
	}
	i := findBet(s.Bets, e.Player)
	switch {
	case i < 0:
		return State{}, notApplicable(name, "player has no bet")
	case s.Bets[i].Status != StatusOpen:
		return State{}, notApplicable(name, "bet is not open")
	case e.CashedOutAtUnixMicro <= s.StartedAtUnixMicro:
		return State{}, notApplicable(name, "cash-out not after round start")
	}
	tick, m, p, eligible, err := cashOutAmounts(s, s.Bets[i].BetCredits, e.CashedOutAtUnixMicro)
	switch {
	case err != nil:
		return State{}, err
	case !eligible:
		return State{}, notApplicable(name, "cash-out not before the crash deadline")
	case tick != e.CashOutTick:
		return State{}, notApplicable(name, "cash-out tick does not match its time")
	case m != e.CashOutMultiplierScaled:
		return State{}, notApplicable(name, "multiplier does not match its tick")
	case p != e.PayoutCredits:
		return State{}, notApplicable(name, "payout does not match bet and multiplier")
	}
	total, err := addNonNeg(s.TotalPayoutCredits, p)
	if err != nil {
		return State{}, err
	}
	next := cloneState(s)
	b := &next.Bets[i]
	b.Status = StatusCashedOut
	b.CashedOutAtUnixMicro = e.CashedOutAtUnixMicro
	b.CashOutTick = tick
	b.CashOutMultiplierScaled = m
	b.PayoutCredits = p
	next.TotalPayoutCredits = total
	return next, nil
}

func evolveRoundCrashed(s State, e RoundCrashed) (State, error) {
	const name = "RoundCrashed"
	switch {
	case s.Phase != PhaseRunning:
		return State{}, notApplicable(name, "phase is not RUNNING")
	case e.CrashPointScaled != s.CrashPointScaled ||
		e.CrashTick != s.CrashTick ||
		e.CrashDeadlineOffsetMicros != s.CrashDeadlineOffsetMicros ||
		e.CrashDeadlineUnixMicro != s.CrashDeadlineUnixMicro:
		return State{}, notApplicable(name, "crash outcome does not match")
	case e.ClosedAtUnixMicro < s.CrashDeadlineUnixMicro:
		return State{}, notApplicable(name, "closed before the crash deadline")
	}
	next := cloneState(s)
	next.Phase = PhaseCrashed
	next.ClosedAtUnixMicro = e.ClosedAtUnixMicro
	return next, nil
}

func evolveBetLost(s State, e BetLost) (State, error) {
	const name = "BetLost"
	if s.Phase != PhaseCrashed {
		return State{}, notApplicable(name, "phase is not CRASHED")
	}
	i := findBet(s.Bets, e.Player)
	switch {
	case i < 0:
		return State{}, notApplicable(name, "player has no bet")
	case s.Bets[i].Status != StatusOpen:
		return State{}, notApplicable(name, "bet is not open")
	case s.Bets[i].BetCredits != e.BetCredits:
		return State{}, notApplicable(name, "bet amount does not match")
	}
	next := cloneState(s)
	next.Bets[i].Status = StatusLost
	return next, nil
}

// evolveRoundSettled also rechecks the reveal against the state (LBT-9 A-12,
// defence in depth): the seed must open the round's commitment and derive its
// crash point. InitialState guarantees both, so only a state that InitialState
// and Evolve did not build fails them.
func evolveRoundSettled(s State, e RoundSettled) (State, error) {
	const name = "RoundSettled"
	switch {
	case s.Phase != PhaseCrashed:
		return State{}, notApplicable(name, "phase is not CRASHED")
	case slices.ContainsFunc(s.Bets, func(b Bet) bool { return b.Status == StatusOpen }):
		return State{}, notApplicable(name, "a bet is still open")
	case e.BetCount != int64(len(s.Bets)):
		return State{}, notApplicable(name, "bet count does not match")
	case e.TotalBetCredits != s.TotalBetCredits:
		return State{}, notApplicable(name, "total bet does not match")
	case e.TotalPayoutCredits != s.TotalPayoutCredits:
		return State{}, notApplicable(name, "total payout does not match")
	case e.Seed != s.Seed:
		return State{}, notApplicable(name, "seed does not match")
	case !VerifyCommitment(e.Seed, s.Commitment):
		return State{}, notApplicable(name, "commitment does not match the seed")
	case CrashPoint(e.Seed, s.RoundID) != s.CrashPointScaled:
		return State{}, notApplicable(name, "crash point does not match the seed")
	case e.SettledAtUnixMicro < 0:
		return State{}, notApplicable(name, "negative settlement time")
	}
	next := cloneState(s)
	next.Phase = PhaseSettled
	next.SettledAtUnixMicro = e.SettledAtUnixMicro
	return next, nil
}

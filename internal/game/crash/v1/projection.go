package v1

import (
	"fmt"
	"slices"
)

// Projection is the public view of a round (ADR-003 "Replay"; math spec
// section 7). The crash fields and ClosedAtUnixMicro are zero before
// PhaseCrashed; Seed and SettledAtUnixMicro are zero before PhaseSettled;
// StartedAtUnixMicro is zero in PhaseBetting. Phase says which fields are
// meaningful.
type Projection struct {
	RoundID       [16]byte
	Phase         Phase
	Commitment    [32]byte
	MinBetCredits int64
	MaxBetCredits int64
	MaxBets       int64

	StartedAtUnixMicro int64
	// Bets is a deep copy in placement order; it never aliases the state.
	Bets               []Bet
	TotalBetCredits    int64
	TotalPayoutCredits int64

	ClosedAtUnixMicro         int64
	CrashPointScaled          int64
	CrashTick                 int64
	CrashDeadlineOffsetMicros int64
	CrashDeadlineUnixMicro    int64

	SettledAtUnixMicro int64
	Seed               [32]byte
}

// Project returns the public view of s. It copies fields by a per-phase
// allow-list and never copies State wholesale, so a new State field stays
// private until it is added here deliberately.
func Project(s State) (Projection, error) {
	if !validPhase(s.Phase) {
		return Projection{}, fmt.Errorf("%w: unknown phase", ErrInvalidState)
	}
	p := Projection{
		RoundID:            s.RoundID,
		Phase:              s.Phase,
		Commitment:         s.Commitment,
		MinBetCredits:      s.MinBetCredits,
		MaxBetCredits:      s.MaxBetCredits,
		MaxBets:            s.MaxBets,
		Bets:               slices.Clone(s.Bets),
		TotalBetCredits:    s.TotalBetCredits,
		TotalPayoutCredits: s.TotalPayoutCredits,
	}
	if s.Phase >= PhaseRunning {
		p.StartedAtUnixMicro = s.StartedAtUnixMicro
	}
	if s.Phase >= PhaseCrashed {
		p.ClosedAtUnixMicro = s.ClosedAtUnixMicro
		p.CrashPointScaled = s.CrashPointScaled
		p.CrashTick = s.CrashTick
		p.CrashDeadlineOffsetMicros = s.CrashDeadlineOffsetMicros
		p.CrashDeadlineUnixMicro = s.CrashDeadlineUnixMicro
	}
	if s.Phase == PhaseSettled {
		p.SettledAtUnixMicro = s.SettledAtUnixMicro
		p.Seed = s.Seed
	}
	return p, nil
}

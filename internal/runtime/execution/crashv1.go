package execution

import (
	"errors"

	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
)

// CrashV1Policy returns the crash/v1 execution policy: the ledger.CrashV1
// posting rule; a checkpoint after every record that emits RoundStarted,
// RoundCrashed or RoundSettled; final in PhaseSettled; the close offset is
// the crash deadline offset D.
func CrashV1Policy() Policy { return crashV1Policy{} }

type crashV1Policy struct{}

var (
	errNotCrashV1State = errors.New("execution: state has the wrong type for the crash rules")
	errNegativeOffset  = errors.New("execution: crash close offset is negative")
)

func (crashV1Policy) Descriptor() game.Descriptor { return v1.Descriptor() }

func (crashV1Policy) Ledger() ledger.Rule { return ledger.CrashV1() }

func (crashV1Policy) Checkpoint(events []any) bool {
	for _, e := range events {
		switch e.(type) {
		case v1.RoundStarted, v1.RoundCrashed, v1.RoundSettled:
			return true
		}
	}
	return false
}

func (crashV1Policy) Final(state any) (bool, error) {
	s, ok := state.(v1.State)
	if !ok {
		return false, errNotCrashV1State
	}
	return s.Phase == v1.PhaseSettled, nil
}

func (crashV1Policy) CloseOffsetMicros(initial any) (int64, error) {
	s, ok := initial.(v1.State)
	if !ok {
		return 0, errNotCrashV1State
	}
	if s.CrashDeadlineOffsetMicros < 0 {
		return 0, errNegativeOffset
	}
	return s.CrashDeadlineOffsetMicros, nil
}

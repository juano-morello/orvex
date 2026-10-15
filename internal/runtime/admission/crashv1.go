package admission

import (
	"fmt"
	"strings"

	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
)

// CrashV1Policy returns the crash/v1 admission policy (S-10).
//
//   - A player-authority PlaceBet with a non-empty player and
//     v1.MinBetCredits <= BetCredits <= v1.MaxBetCredits holds BetCredits
//     for the player; every other command holds nothing.
//   - System-authority StartRound, CloseRound and SettleRound are
//     ClassStart, ClassClose and ClassSettle; with any other authority they
//     are ClassOrdinary, so Decide records them as unauthorized.
//   - A PlaceBet or CashOut whose player contains U+0000, nil, pointers and
//     foreign types return ErrInvalidCommand.
func CrashV1Policy() Policy { return crashV1Policy{} }

type crashV1Policy struct{}

func (crashV1Policy) Descriptor() game.Descriptor { return v1.Descriptor() }

func (crashV1Policy) Classify(command any) (Classification, error) {
	ordinary := Classification{Class: ClassOrdinary}
	switch c := command.(type) {
	case v1.PlaceBet:
		if hasNUL(c.Player) {
			return Classification{}, fmt.Errorf("%w: crash/v1 PlaceBet player contains U+0000", ErrInvalidCommand)
		}
		if c.Authority == v1.AuthorityPlayer && c.Player != "" &&
			c.BetCredits >= v1.MinBetCredits && c.BetCredits <= v1.MaxBetCredits {
			ordinary.Hold = Hold{PlayerID: string(c.Player), Credits: c.BetCredits}
		}
		return ordinary, nil
	case v1.CashOut:
		if hasNUL(c.Player) {
			return Classification{}, fmt.Errorf("%w: crash/v1 CashOut player contains U+0000", ErrInvalidCommand)
		}
		return ordinary, nil
	case v1.StartRound:
		return lifecycle(c.Authority, ClassStart), nil
	case v1.CloseRound:
		return lifecycle(c.Authority, ClassClose), nil
	case v1.SettleRound:
		return lifecycle(c.Authority, ClassSettle), nil
	default:
		return Classification{}, fmt.Errorf("%w: crash/v1 command of type %T", ErrInvalidCommand, command)
	}
}

// lifecycle returns class for a system-authority command and
// ClassOrdinary otherwise.
func lifecycle(a v1.Authority, class Class) Classification {
	if a != v1.AuthoritySystem {
		return Classification{Class: ClassOrdinary}
	}
	return Classification{Class: class}
}

func hasNUL(p v1.PlayerID) bool { return strings.ContainsRune(string(p), 0) }

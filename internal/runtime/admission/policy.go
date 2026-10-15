package admission

import (
	"strings"
	"unicode/utf8"

	"github.com/juano-morello/orvex/internal/game"
)

// Policy is the admission view of one rules version (S-10): which commands
// drive the admission window and how many credits a command reserves.
// Classify is pure.
type Policy interface {
	// Descriptor returns the rules version the policy classifies. New
	// requires the registry's adapter for it to have an equal descriptor.
	Descriptor() game.Descriptor
	// Classify returns the command's class and hold, or an error for a
	// command the rules version cannot admit.
	Classify(command any) (Classification, error)
}

// Class is a command's role in the admission window (S-8).
type Class uint8

// Classes of the admission window.
const (
	// ClassOrdinary is every command that does not drive the window.
	ClassOrdinary Class = iota + 1
	// ClassStart opens the round's run: open -> started.
	ClassStart
	// ClassClose is the closure barrier: started -> closed.
	ClassClose
	// ClassSettle seals the round: closed -> sealed.
	ClassSettle
)

// Hold is a credit reservation; the zero value means none.
type Hold struct {
	PlayerID string
	Credits  int64 // > 0 when PlayerID != ""
}

// Classification is a command's class and hold.
type Classification struct {
	Class Class
	Hold  Hold
}

// sqlText returns the class as orvex.admit_command's p_class.
func (c Class) sqlText() (string, bool) {
	switch c {
	case ClassOrdinary:
		return "ordinary", true
	case ClassStart:
		return "start", true
	case ClassClose:
		return "close", true
	case ClassSettle:
		return "settle", true
	default:
		return "", false
	}
}

// valid reports whether c has a known class and a consistent hold: none,
// or a storable player ID with positive credits.
func (c Classification) valid() bool {
	if _, ok := c.Class.sqlText(); !ok {
		return false
	}
	h := c.Hold
	if h.PlayerID == "" {
		return h.Credits == 0
	}
	return h.Credits > 0 && utf8.ValidString(h.PlayerID) && !strings.ContainsRune(h.PlayerID, 0)
}

package ledger

import (
	"errors"

	"github.com/juano-morello/orvex/internal/game"
)

// AccountKind says which ledger account a posting side references.
type AccountKind uint8

const (
	// AccountPlayer is the player's account, found by player ID.
	AccountPlayer AccountKind = iota + 1
	// AccountRound is the round's "round_pool" system account.
	AccountRound
	// AccountHouse is the global "house" system account.
	AccountHouse
)

// Account references a ledger account; Player is set iff Kind ==
// AccountPlayer.
type Account struct {
	Kind   AccountKind
	Player string
}

// Valid reports whether a is a well-formed reference.
func (a Account) Valid() bool {
	switch a.Kind {
	case AccountPlayer:
		return a.Player != ""
	case AccountRound, AccountHouse:
		return a.Player == ""
	default:
		return false
	}
}

// Posting moves Amount from the Debit account to the Credit account.
type Posting struct {
	Debit, Credit Account
	// Amount is > 0.
	Amount int64
	// EventSequence is the chained event the posting settles.
	EventSequence uint64
	// FromHold reports that the debit consumes the admission's credit hold.
	FromHold bool
}

// Rule is the pure posting rule of one rules version.
type Rule interface {
	Descriptor() game.Descriptor
	// Postings returns the ledger transaction kind and the postings of one
	// executed command record. events[j] has event sequence first+j. kind is
	// "" iff postings is empty.
	Postings(events []any, first uint64) (kind string, postings []Posting, err error)
}

// Reserved system account names.
const (
	HouseAccountName = "house"
	RoundAccountName = "round_pool"
)

// ErrInvalidEvents reports events a rule cannot post: a foreign event type
// or an amount outside the rule's range.
var ErrInvalidEvents = errors.New("ledger: events cannot be posted")

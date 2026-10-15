package execution

import (
	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/ledger"
)

// Policy is the execution view of one rules version: its posting rule,
// snapshot cadence, finality and secret close offset. Implementations are
// pure.
type Policy interface {
	Descriptor() game.Descriptor
	// Ledger returns the rules version's posting rule (E-9).
	Ledger() ledger.Rule
	// Checkpoint reports whether the state after a record with these
	// events gets a snapshot (E-13).
	Checkpoint(events []any) bool
	// Final reports whether no command can change state any more.
	Final(state any) (bool, error)
	// CloseOffsetMicros returns the secret offset from the start command's
	// effective time before which no closing command may be admitted, for
	// the round whose initial state is initial (crash/v1: D). It is >= 0.
	// Errors never format the value (LBT-12 engineer addendum).
	CloseOffsetMicros(initial any) (int64, error)
}

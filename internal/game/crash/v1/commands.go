package v1

// Authority is who submitted a command. The runtime sets it from the
// authenticated admission channel, never from client data (ADR-003, ADR-004:
// lifecycle commands are trusted-only). Decide enforces it, so decision
// replay reproduces an authority rejection.
type Authority uint8

const (
	// AuthorityNone is the zero value; every command carrying it is rejected.
	AuthorityNone Authority = iota
	// AuthorityPlayer is an authenticated player session. PlaceBet and
	// CashOut require it.
	AuthorityPlayer
	// AuthoritySystem is the trusted round scheduler. StartRound, CloseRound
	// and SettleRound require it.
	AuthoritySystem
)

// Command is a crash/v1 command: one of PlaceBet, StartRound, CashOut,
// CloseRound or SettleRound. It is a sealed sum type; pass commands by value.
// Pointers to command types are not commands Decide accepts.
type Command interface{ isCommand() }

// PlaceBet places the player's single bet of the round. It requires
// AuthorityPlayer and PhaseBetting.
type PlaceBet struct {
	Authority  Authority
	Player     PlayerID
	BetCredits int64
}

// StartRound closes betting and starts the multiplier curve at the command's
// effective admission time T0 (math spec D-9). It requires AuthoritySystem
// and PhaseBetting.
type StartRound struct{ Authority Authority }

// CashOut cashes out the player's open bet at the command's effective
// admission time. It requires AuthorityPlayer and PhaseRunning.
type CashOut struct {
	Authority Authority
	Player    PlayerID
}

// CloseRound records the crash once the committed crash deadline T0 + D has
// passed. It requires AuthoritySystem and PhaseRunning.
type CloseRound struct{ Authority Authority }

// SettleRound resolves every open bet as lost and reveals the seed. It
// requires AuthoritySystem and PhaseCrashed.
type SettleRound struct{ Authority Authority }

func (PlaceBet) isCommand()    {}
func (StartRound) isCommand()  {}
func (CashOut) isCommand()     {}
func (CloseRound) isCommand()  {}
func (SettleRound) isCommand() {}

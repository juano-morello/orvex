package v1

// Event is a crash/v1 event: one of BetPlaced, RoundStarted, BetCashedOut,
// RoundCrashed, BetLost or RoundSettled. It is a sealed sum type; pass events
// by value. Payloads hold only plain values, use int64 fixed units and give
// times in microseconds since the Unix epoch, UTC (math spec D-12).
//
// No event carries a secret before it is due: RoundCrashed reveals the crash
// outcome and RoundSettled the seed (math spec section 7).
type Event interface{ isEvent() }

// BetPlaced records a player's accepted bet.
type BetPlaced struct {
	Player            PlayerID
	BetCredits        int64
	PlacedAtUnixMicro int64
}

// RoundStarted records the end of betting and the round start time T0, the
// StartRound command's effective admission time (math spec D-9).
type RoundStarted struct {
	StartedAtUnixMicro int64
}

// BetCashedOut records an eligible cash-out at effective admission time
// CashedOutAtUnixMicro, its tick, multiplier and payout (math spec D-8, D-9).
type BetCashedOut struct {
	Player                  PlayerID
	CashedOutAtUnixMicro    int64
	CashOutTick             int64
	CashOutMultiplierScaled int64
	PayoutCredits           int64
}

// RoundCrashed records the crash and reveals the authoritative crash outcome
// (math spec D-12). ClosedAtUnixMicro is the CloseRound effective time, at or
// after CrashDeadlineUnixMicro.
type RoundCrashed struct {
	ClosedAtUnixMicro         int64
	CrashPointScaled          int64
	CrashTick                 int64
	CrashDeadlineOffsetMicros int64
	CrashDeadlineUnixMicro    int64
}

// BetLost records that a bet still open at settlement is lost.
type BetLost struct {
	Player     PlayerID
	BetCredits int64
}

// RoundSettled closes the round with its totals and reveals the seed (math
// spec D-10 step 7). It is the last event of a round.
type RoundSettled struct {
	SettledAtUnixMicro int64
	BetCount           int64
	TotalBetCredits    int64
	TotalPayoutCredits int64
	Seed               [32]byte
}

func (BetPlaced) isEvent()    {}
func (RoundStarted) isEvent() {}
func (BetCashedOut) isEvent() {}
func (RoundCrashed) isEvent() {}
func (BetLost) isEvent()      {}
func (RoundSettled) isEvent() {}

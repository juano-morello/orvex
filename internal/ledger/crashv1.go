package ledger

import (
	"fmt"

	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
)

// crash/v1 ledger transaction kinds (E-9).
const (
	KindCrashBet        = "crash_bet"
	KindCrashCashOut    = "crash_cash_out"
	KindCrashSettlement = "crash_settlement"
)

// CrashV1 returns the crash/v1 posting rule (E-9):
//
//	BetPlaced{P, b}         player P -> round, b, from the admission's hold
//	BetCashedOut{P, payout} round -> player P, payout
//	RoundSettled{B, W}      B > W: round -> house, B - W; B < W: house -> round, W - B
//	RoundStarted, RoundCrashed, BetLost: none
//
// The settlement posting leaves the round account at 0: the stakes entered
// it at BetPlaced and the payouts left it at BetCashedOut.
func CrashV1() Rule { return crashV1{} }

type crashV1 struct{}

func (crashV1) Descriptor() game.Descriptor { return v1.Descriptor() }

func (crashV1) Postings(events []any, first uint64) (string, []Posting, error) {
	var (
		kind     string
		postings []Posting
	)
	add := func(k string, p Posting) error {
		if kind != "" && kind != k {
			return fmt.Errorf("%w: one record posts both %s and %s", ErrInvalidEvents, kind, k)
		}
		kind = k
		postings = append(postings, p)
		return nil
	}
	for j, e := range events {
		seq := first + uint64(j)
		var err error
		switch ev := e.(type) {
		case v1.BetPlaced:
			if ev.Player == "" || ev.BetCredits < 1 {
				return "", nil, fmt.Errorf("%w: BetPlaced %d has no player or no credits", ErrInvalidEvents, j)
			}
			err = add(KindCrashBet, Posting{
				Debit:         Account{Kind: AccountPlayer, Player: string(ev.Player)},
				Credit:        Account{Kind: AccountRound},
				Amount:        ev.BetCredits,
				EventSequence: seq,
				FromHold:      true,
			})
		case v1.BetCashedOut:
			if ev.Player == "" || ev.PayoutCredits < 1 {
				return "", nil, fmt.Errorf("%w: BetCashedOut %d has no player or no payout", ErrInvalidEvents, j)
			}
			err = add(KindCrashCashOut, Posting{
				Debit:         Account{Kind: AccountRound},
				Credit:        Account{Kind: AccountPlayer, Player: string(ev.Player)},
				Amount:        ev.PayoutCredits,
				EventSequence: seq,
			})
		case v1.RoundSettled:
			b, w := ev.TotalBetCredits, ev.TotalPayoutCredits
			if b < 0 || w < 0 {
				return "", nil, fmt.Errorf("%w: RoundSettled %d has negative totals", ErrInvalidEvents, j)
			}
			switch {
			case b > w:
				err = add(KindCrashSettlement, Posting{
					Debit: Account{Kind: AccountRound}, Credit: Account{Kind: AccountHouse},
					Amount: b - w, EventSequence: seq,
				})
			case b < w:
				err = add(KindCrashSettlement, Posting{
					Debit: Account{Kind: AccountHouse}, Credit: Account{Kind: AccountRound},
					Amount: w - b, EventSequence: seq,
				})
			}
		case v1.RoundStarted, v1.RoundCrashed, v1.BetLost:
		default:
			return "", nil, fmt.Errorf("%w: event %d has type %T", ErrInvalidEvents, j, e)
		}
		if err != nil {
			return "", nil, err
		}
	}
	return kind, postings, nil
}

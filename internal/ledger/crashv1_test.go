package ledger

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
)

func TestCrashV1Descriptor(t *testing.T) {
	if got := CrashV1().Descriptor(); got != v1.Descriptor() {
		t.Errorf("Descriptor = %+v, want %+v", got, v1.Descriptor())
	}
}

func TestCrashV1Postings(t *testing.T) {
	player := func(p string) Account { return Account{Kind: AccountPlayer, Player: p} }
	round := Account{Kind: AccountRound}
	house := Account{Kind: AccountHouse}
	cases := []struct {
		name     string
		events   []any
		first    uint64
		kind     string
		postings []Posting
	}{
		{"BetPlaced", []any{v1.BetPlaced{Player: "alice", BetCredits: 100}}, 1, KindCrashBet,
			[]Posting{{Debit: player("alice"), Credit: round, Amount: 100, EventSequence: 1, FromHold: true}}},
		{"BetCashedOut", []any{v1.BetCashedOut{Player: "alice", PayoutCredits: 134}}, 5, KindCrashCashOut,
			[]Posting{{Debit: round, Credit: player("alice"), Amount: 134, EventSequence: 5}}},
		{"RoundSettled B > W", []any{v1.RoundSettled{TotalBetCredits: 500, TotalPayoutCredits: 120}}, 9, KindCrashSettlement,
			[]Posting{{Debit: round, Credit: house, Amount: 380, EventSequence: 9}}},
		{"RoundSettled B < W", []any{v1.RoundSettled{TotalBetCredits: 1_000_000_350, TotalPayoutCredits: 1_757_600_134}}, 9, KindCrashSettlement,
			[]Posting{{Debit: house, Credit: round, Amount: 757_599_784, EventSequence: 9}}},
		{"RoundSettled B = W", []any{v1.RoundSettled{TotalBetCredits: 300, TotalPayoutCredits: 300}}, 3, "", nil},
		{"BetLost and RoundSettled", []any{
			v1.BetLost{Player: "bob", BetCredits: 250},
			v1.BetLost{Player: "carol", BetCredits: 50},
			v1.RoundSettled{TotalBetCredits: 300, TotalPayoutCredits: 0},
		}, 7, KindCrashSettlement,
			[]Posting{{Debit: round, Credit: house, Amount: 300, EventSequence: 9}}},
		{"RoundStarted", []any{v1.RoundStarted{StartedAtUnixMicro: 1}}, 4, "", nil},
		{"RoundCrashed", []any{v1.RoundCrashed{ClosedAtUnixMicro: 1}}, 4, "", nil},
		{"no events", nil, 1, "", nil},
		{"offset by first", []any{v1.BetPlaced{Player: "bob", BetCredits: 7}}, 1000, KindCrashBet,
			[]Posting{{Debit: player("bob"), Credit: round, Amount: 7, EventSequence: 1000, FromHold: true}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, postings, err := CrashV1().Postings(c.events, c.first)
			if err != nil {
				t.Fatalf("Postings: %v", err)
			}
			if kind != c.kind || !reflect.DeepEqual(postings, c.postings) {
				t.Errorf("Postings = %q %+v, want %q %+v", kind, postings, c.kind, c.postings)
			}
			for _, p := range postings {
				if !p.Debit.Valid() || !p.Credit.Valid() {
					t.Errorf("posting %+v has an invalid account", p)
				}
			}
		})
	}
}

func TestCrashV1PostingsRejectsBadInput(t *testing.T) {
	for _, c := range []struct {
		name   string
		events []any
	}{
		{"foreign event", []any{struct{}{}}},
		{"pointer event", []any{&v1.BetPlaced{Player: "alice", BetCredits: 1}}},
		{"cash-out payout 0", []any{v1.BetCashedOut{Player: "alice", PayoutCredits: 0}}},
		{"bet 0 credits", []any{v1.BetPlaced{Player: "alice", BetCredits: 0}}},
		{"bet without player", []any{v1.BetPlaced{BetCredits: 5}}},
		{"negative settlement totals", []any{v1.RoundSettled{TotalBetCredits: -1}}},
		{"mixed kinds", []any{v1.BetPlaced{Player: "a", BetCredits: 1}, v1.BetCashedOut{Player: "a", PayoutCredits: 1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := CrashV1().Postings(c.events, 1); !errors.Is(err, ErrInvalidEvents) {
				t.Errorf("err = %v, want ErrInvalidEvents", err)
			}
		})
	}
}

const goldenDir = "../audit/verification/testdata/golden"

// TestCrashV1PostingsWorkedExample applies the rule to every record of the
// worked example and checks the balances of spec section 8 (without the
// players' funding grants).
func TestCrashV1PostingsWorkedExample(t *testing.T) {
	b, err := os.ReadFile(goldenDir + "/worked-example.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	adapter := v1.Adapter()
	balance := map[Account]int64{}
	kinds := map[uint64]string{}
	var p3 string // the player of record 3 (LBT-9 spec section 5.6)
	for _, line := range lines[2:] {
		rec, err := chain.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		c, ok := rec.(chain.Command)
		if !ok {
			t.Fatalf("record is %T, want a command record", rec)
		}
		events := make([]any, len(c.Events))
		for j, e := range c.Events {
			if events[j], err = adapter.DecodeEvent(e.Payload); err != nil {
				t.Fatal(err)
			}
		}
		if bp, ok := firstEvent(events).(v1.BetPlaced); ok && c.Sequence == 3 {
			p3 = string(bp.Player)
		}
		var first uint64
		if len(c.Events) > 0 {
			first = c.Events[0].Sequence
		}
		kind, postings, err := CrashV1().Postings(events, first)
		if err != nil {
			t.Fatalf("record %d: %v", c.Sequence, err)
		}
		if kind != "" {
			kinds[c.Sequence] = kind
		}
		for _, p := range postings {
			balance[p.Debit] -= p.Amount
			balance[p.Credit] += p.Amount
		}
	}
	want := map[uint64]string{1: KindCrashBet, 2: KindCrashBet, 3: KindCrashBet, 12: KindCrashCashOut, 17: KindCrashCashOut, 21: KindCrashSettlement}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("ledger transactions = %v, want %v", kinds, want)
	}
	if got := balance[Account{Kind: AccountRound}]; got != 0 {
		t.Errorf("round account balance = %d, want 0", got)
	}
	if got := balance[Account{Kind: AccountHouse}]; got != -757_599_784 {
		t.Errorf("house balance = %d, want -757599784", got)
	}
	var players, sum int64
	for a, v := range balance {
		sum += v
		if a.Kind == AccountPlayer {
			players += v
		}
	}
	if sum != 0 || players != 757_599_784 {
		t.Errorf("sum of balances %d, players' gains %d; want 0 and 757599784", sum, players)
	}
	for p, v := range map[string]int64{"alice": 34, "bob": -250, p3: 757_600_000} {
		if got := balance[Account{Kind: AccountPlayer, Player: p}]; got != v {
			t.Errorf("player %q balance = %d, want %d", p, got, v)
		}
	}
}

func firstEvent(events []any) any {
	if len(events) == 0 {
		return nil
	}
	return events[0]
}

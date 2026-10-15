package admission

import (
	"errors"
	"strconv"
	"testing"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
)

func TestCrashV1PolicyDescriptor(t *testing.T) {
	if got := CrashV1Policy().Descriptor(); got != v1.Descriptor() {
		t.Errorf("Descriptor() = %+v, want %+v", got, v1.Descriptor())
	}
}

func TestCrashV1Classify(t *testing.T) {
	ordinary := Classification{Class: ClassOrdinary}
	hold := func(p string, n int64) Classification {
		return Classification{Class: ClassOrdinary, Hold: Hold{PlayerID: p, Credits: n}}
	}
	authorities := []struct {
		name string
		a    v1.Authority
	}{{"player", v1.AuthorityPlayer}, {"system", v1.AuthoritySystem}, {"none", v1.AuthorityNone}}

	type tc struct {
		name string
		cmd  any
		want Classification
	}
	var cases []tc
	for _, au := range authorities {
		lifecycle := func(c Class) Classification {
			if au.a == v1.AuthoritySystem {
				return Classification{Class: c}
			}
			return ordinary
		}
		betWant := ordinary
		if au.a == v1.AuthorityPlayer {
			betWant = hold("alice", 10)
		}
		cases = append(cases,
			tc{"PlaceBet " + au.name, v1.PlaceBet{Authority: au.a, Player: "alice", BetCredits: 10}, betWant},
			tc{"CashOut " + au.name, v1.CashOut{Authority: au.a, Player: "alice"}, ordinary},
			tc{"StartRound " + au.name, v1.StartRound{Authority: au.a}, lifecycle(ClassStart)},
			tc{"CloseRound " + au.name, v1.CloseRound{Authority: au.a}, lifecycle(ClassClose)},
			tc{"SettleRound " + au.name, v1.SettleRound{Authority: au.a}, lifecycle(ClassSettle)},
		)
	}
	for _, amount := range []int64{-1, 0, 1, 1_000_000_000, 1_000_000_001} {
		want := ordinary
		if amount == 1 || amount == 1_000_000_000 {
			want = hold("bob", amount)
		}
		cases = append(cases, tc{"PlaceBet amount " + strconv.FormatInt(amount, 10),
			v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "bob", BetCredits: amount}, want})
	}
	cases = append(cases, tc{"PlaceBet empty player",
		v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "", BetCredits: 10}, ordinary})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CrashV1Policy().Classify(c.cmd)
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got != c.want {
				t.Errorf("Classify = %+v, want %+v", got, c.want)
			}
			if !got.valid() {
				t.Errorf("Classify = %+v, not a valid classification", got)
			}
		})
	}

	invalid := []struct {
		name string
		cmd  any
	}{
		{"PlaceBet NUL player", v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "a\x00b", BetCredits: 10}},
		{"CashOut NUL player", v1.CashOut{Authority: v1.AuthorityPlayer, Player: "a\x00b"}},
		{"nil", nil},
		{"pointer", &v1.CashOut{}},
		{"foreign type", "CashOut"},
	}
	for _, c := range invalid {
		t.Run(c.name, func(t *testing.T) {
			if _, err := CrashV1Policy().Classify(c.cmd); !errors.Is(err, ErrInvalidCommand) {
				t.Errorf("Classify err = %v, want ErrInvalidCommand", err)
			}
		})
	}
}

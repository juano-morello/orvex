package v1

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestEvolveValidatesApplicability(t *testing.T) {
	const deadline = testT0 + 9_410_000
	r := Rules{}
	evolve := func(t *testing.T, s State, e Event) State {
		t.Helper()
		next, err := r.Evolve(s, e)
		if err != nil {
			t.Fatalf("Evolve(%#v): %v", e, err)
		}
		return next
	}

	cfg := testConfig(workedSeed())
	cfg.MinBetCredits, cfg.MaxBetCredits, cfg.MaxBets = 10, 100_000, 2
	betting := evolve(t, newRound(t, cfg), BetPlaced{Player: "alice", BetCredits: 1_000, PlacedAtUnixMicro: testT0 - 2})
	full := evolve(t, betting, BetPlaced{Player: "bob", BetCredits: 1_000, PlacedAtUnixMicro: testT0 - 1})
	running := evolve(t, full, RoundStarted{StartedAtUnixMicro: testT0})
	cashed, _ := step(t, running, cashOut("alice"), testT0+1_000_000)
	crashed, _ := step(t, cashed, closeRound(), deadline)
	lost := evolve(t, crashed, BetLost{Player: "bob", BetCredits: 1_000})
	settledState := evolve(t, lost, RoundSettled{SettledAtUnixMicro: deadline + 1, BetCount: 2,
		TotalBetCredits: 2_000, TotalPayoutCredits: lost.TotalPayoutCredits, Seed: workedSeed()})

	tick := int64(200)
	m, err := MultiplierAtTick(tick)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Payout(1_000, m)
	if err != nil {
		t.Fatal(err)
	}
	goodCash := BetCashedOut{Player: "bob", CashedOutAtUnixMicro: testT0 + 2_000_000,
		CashOutTick: tick, CashOutMultiplierScaled: m, PayoutCredits: p}
	cashWith := func(f func(*BetCashedOut)) BetCashedOut { e := goodCash; f(&e); return e }
	mDeadline, err := MultiplierAtTick(941)
	if err != nil {
		t.Fatal(err)
	}

	goodCrash := RoundCrashed{ClosedAtUnixMicro: deadline, CrashPointScaled: 17_577, CrashTick: 941,
		CrashDeadlineOffsetMicros: 9_410_000, CrashDeadlineUnixMicro: deadline}
	crashWith := func(f func(*RoundCrashed)) RoundCrashed { e := goodCrash; f(&e); return e }

	goodSettle := RoundSettled{SettledAtUnixMicro: deadline + 1, BetCount: 2,
		TotalBetCredits: 2_000, TotalPayoutCredits: lost.TotalPayoutCredits, Seed: workedSeed()}
	settleWith := func(f func(*RoundSettled)) RoundSettled { e := goodSettle; f(&e); return e }

	// The unmodified templates apply, so each failing case below fails for
	// the one difference it introduces.
	evolve(t, cashed, goodCash)
	evolve(t, running, goodCrash)
	evolve(t, lost, goodSettle)
	evolve(t, crashed, BetLost{Player: "bob", BetCredits: 1_000})
	evolve(t, betting, BetPlaced{Player: "carol", BetCredits: 10, PlacedAtUnixMicro: 0})

	cases := []struct {
		name string
		s    State
		e    Event
	}{
		{"BetPlaced while running", running, BetPlaced{Player: "carol", BetCredits: 1_000, PlacedAtUnixMicro: testT0}},
		{"BetPlaced empty player", betting, BetPlaced{Player: "", BetCredits: 1_000, PlacedAtUnixMicro: testT0}},
		{"BetPlaced below min", betting, BetPlaced{Player: "carol", BetCredits: 9, PlacedAtUnixMicro: testT0}},
		{"BetPlaced above max", betting, BetPlaced{Player: "carol", BetCredits: 100_001, PlacedAtUnixMicro: testT0}},
		{"BetPlaced at bet limit", full, BetPlaced{Player: "carol", BetCredits: 1_000, PlacedAtUnixMicro: testT0}},
		{"BetPlaced duplicate player", betting, BetPlaced{Player: "alice", BetCredits: 1_000, PlacedAtUnixMicro: testT0}},
		{"BetPlaced negative time", betting, BetPlaced{Player: "carol", BetCredits: 1_000, PlacedAtUnixMicro: -1}},

		{"RoundStarted while running", running, RoundStarted{StartedAtUnixMicro: testT0 + 1}},
		{"RoundStarted negative time", full, RoundStarted{StartedAtUnixMicro: -1}},
		{"RoundStarted after max start", full, RoundStarted{StartedAtUnixMicro: MaxStartMicros + 1}},

		{"BetCashedOut while betting", full, goodCash},
		{"BetCashedOut unknown player", cashed, cashWith(func(e *BetCashedOut) { e.Player = "zoe" })},
		{"BetCashedOut already cashed out", cashed, cashWith(func(e *BetCashedOut) { e.Player = "alice" })},
		{"BetCashedOut at T0", cashed, BetCashedOut{Player: "bob", CashedOutAtUnixMicro: testT0,
			CashOutTick: 0, CashOutMultiplierScaled: Scale, PayoutCredits: 1_000}},
		{"BetCashedOut at deadline", cashed, BetCashedOut{Player: "bob", CashedOutAtUnixMicro: deadline,
			CashOutTick: 941, CashOutMultiplierScaled: mDeadline, PayoutCredits: 1_000 * mDeadline / Scale}},
		{"BetCashedOut tick off by one", cashed, cashWith(func(e *BetCashedOut) { e.CashOutTick++ })},
		{"BetCashedOut multiplier off by one", cashed, cashWith(func(e *BetCashedOut) { e.CashOutMultiplierScaled++ })},
		{"BetCashedOut payout off by one", cashed, cashWith(func(e *BetCashedOut) { e.PayoutCredits++ })},

		{"RoundCrashed while betting", full, goodCrash},
		{"RoundCrashed before deadline", running, crashWith(func(e *RoundCrashed) { e.ClosedAtUnixMicro-- })},
		{"RoundCrashed crash point off", running, crashWith(func(e *RoundCrashed) { e.CrashPointScaled++ })},
		{"RoundCrashed crash tick off", running, crashWith(func(e *RoundCrashed) { e.CrashTick++ })},
		{"RoundCrashed offset off", running, crashWith(func(e *RoundCrashed) { e.CrashDeadlineOffsetMicros++ })},
		{"RoundCrashed deadline off", running, crashWith(func(e *RoundCrashed) { e.CrashDeadlineUnixMicro++ })},

		{"BetLost while running", running, BetLost{Player: "bob", BetCredits: 1_000}},
		{"BetLost unknown player", crashed, BetLost{Player: "zoe", BetCredits: 1_000}},
		{"BetLost cashed-out player", crashed, BetLost{Player: "alice", BetCredits: 1_000}},
		{"BetLost already lost", lost, BetLost{Player: "bob", BetCredits: 1_000}},
		{"BetLost wrong amount", crashed, BetLost{Player: "bob", BetCredits: 999}},

		{"RoundSettled while running", running, goodSettle},
		{"RoundSettled with an open bet", crashed, goodSettle},
		{"RoundSettled wrong bet count", lost, settleWith(func(e *RoundSettled) { e.BetCount++ })},
		{"RoundSettled wrong total bet", lost, settleWith(func(e *RoundSettled) { e.TotalBetCredits++ })},
		{"RoundSettled wrong total payout", lost, settleWith(func(e *RoundSettled) { e.TotalPayoutCredits++ })},
		{"RoundSettled wrong seed", lost, settleWith(func(e *RoundSettled) { e.Seed[31] ^= 0x01 })},
		{"RoundSettled negative time", lost, settleWith(func(e *RoundSettled) { e.SettledAtUnixMicro = -1 })},

		{"nil event", betting, nil},
		{"pointer event", betting, &BetPlaced{Player: "carol", BetCredits: 1_000, PlacedAtUnixMicro: testT0}},
	}
	for _, e := range allEventKinds() {
		cases = append(cases, struct {
			name string
			s    State
			e    Event
		}{"settled " + reflect.TypeOf(e).Name(), settledState, e})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := takeRoundSnapshot(tc.s)
			next, err := r.Evolve(tc.s, tc.e)
			if !errors.Is(err, ErrEventNotApplicable) {
				t.Errorf("err = %v, want ErrEventNotApplicable", err)
			}
			if !reflect.DeepEqual(next, State{}) {
				t.Errorf("state = %+v, want State{}", next)
			}
			if snap.changed(tc.s) {
				t.Error("rejected event changed the input state")
			}
		})
	}
}

func TestRoundRejectsInvalidState(t *testing.T) {
	broken := newRound(t, testConfig(workedSeed()))
	broken.Phase = 99
	for name, s := range map[string]State{"zero": {}, "phase 99": broken} {
		t.Run(name, func(t *testing.T) {
			events, err := Rules{}.Decide(s, placeBet("alice", 5), inputAt(testT0))
			if events != nil || !errors.Is(err, ErrInvalidState) || errors.Is(err, ErrRejected) {
				t.Errorf("Decide = %v, %v; want ErrInvalidState and no rejection", events, err)
			}
			if next, err := (Rules{}).Evolve(s, BetPlaced{Player: "alice", BetCredits: 5}); !errors.Is(err, ErrInvalidState) ||
				!reflect.DeepEqual(next, State{}) {
				t.Errorf("Evolve = %+v, %v; want ErrInvalidState", next, err)
			}
			if p, err := Project(s); !errors.Is(err, ErrInvalidState) || !reflect.DeepEqual(p, Projection{}) {
				t.Errorf("Project = %+v, %v; want ErrInvalidState", p, err)
			}
		})
	}
}

// TestEvolveFailsClosedOnOverflow checks that Evolve rejects an event whose
// aggregate or deadline sum overflows with ErrArithmetic and State{}, without
// changing the input state.
func TestEvolveFailsClosedOnOverflow(t *testing.T) {
	betting := newRound(t, testConfig(workedSeed()))
	betting, _ = step(t, betting, placeBet("alice", 1_000), testT0-1)
	betTotal := withSpareCap(betting)
	betTotal.TotalBetCredits = math.MaxInt64 - 5

	deadline := withSpareCap(betting)
	deadline.CrashDeadlineOffsetMicros = math.MaxInt64

	running := runningRound(t, workedSeed(), "alice")
	payoutTotal := withSpareCap(running)
	payoutTotal.TotalPayoutCredits = math.MaxInt64 - 999

	cases := []struct {
		name string
		s    State
		e    Event
	}{
		{"BetPlaced bet total", betTotal, BetPlaced{Player: "bob", BetCredits: 6, PlacedAtUnixMicro: testT0}},
		{"RoundStarted deadline", deadline, RoundStarted{StartedAtUnixMicro: MaxStartMicros}},
		{"BetCashedOut payout total", payoutTotal, BetCashedOut{Player: "alice", CashedOutAtUnixMicro: testT0 + 1,
			CashOutTick: 0, CashOutMultiplierScaled: Scale, PayoutCredits: 1_000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := takeRoundSnapshot(tc.s)
			next, err := Rules{}.Evolve(tc.s, tc.e)
			if !errors.Is(err, ErrArithmetic) || errors.Is(err, ErrEventNotApplicable) {
				t.Errorf("err = %v, want ErrArithmetic", err)
			}
			if !reflect.DeepEqual(next, State{}) {
				t.Errorf("state = %+v, want State{}", next)
			}
			if snap.changed(tc.s) {
				t.Error("failed Evolve changed the input state")
			}
		})
	}
}

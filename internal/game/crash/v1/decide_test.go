package v1

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

// settledWorkedRound plays the worked example of TestRoundFullWorkedExample
// and returns the settled state and its event history.
func settledWorkedRound(t testing.TB) (State, []Event) {
	t.Helper()
	s := newRound(t, testConfig(workedSeed()))
	var history []Event
	do := func(c Command, at int64) {
		t.Helper()
		var ev []Event
		s, ev = step(t, s, c, at)
		history = append(history, ev...)
	}
	do(placeBet("alice", 1_000), testT0-4_000)
	do(placeBet("bob", 250), testT0-3_000)
	do(placeBet("carol", 1_000_000_000), testT0-2_000)
	do(placeBet("dave", 7), testT0-1_000)
	do(startRound(), testT0)
	do(cashOut("carol"), testT0+1)
	do(cashOut("alice"), testT0+9_409_999)
	do(closeRound(), testT0+9_410_000)
	do(settleRound(), testT0+10_000_000)
	return s, history
}

func wantEvents(t *testing.T, got []Event, want ...Event) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v\nwant %#v", got, want)
	}
}

func wantReason(t *testing.T, got, want Reason) {
	t.Helper()
	if got != want {
		t.Fatalf("reason = %s, want %s", got, want)
	}
}

func TestRoundFullWorkedExample(t *testing.T) {
	s := newRound(t, testConfig(workedSeed()))
	var ev []Event

	bets := []struct {
		p  PlayerID
		b  int64
		at int64
	}{
		{"alice", 1_000, testT0 - 4_000},
		{"bob", 250, testT0 - 3_000},
		{"carol", 1_000_000_000, testT0 - 2_000},
		{"dave", 7, testT0 - 1_000},
	}
	for _, b := range bets {
		s, ev = step(t, s, placeBet(b.p, b.b), b.at)
		wantEvents(t, ev, BetPlaced{Player: b.p, BetCredits: b.b, PlacedAtUnixMicro: b.at})
	}

	s, ev = step(t, s, startRound(), testT0)
	wantEvents(t, ev, RoundStarted{StartedAtUnixMicro: testT0})

	wantReason(t, rejected(t, s, cashOut("carol"), testT0), ReasonNotAfterStart)

	s, ev = step(t, s, cashOut("carol"), testT0+1)
	wantEvents(t, ev, BetCashedOut{Player: "carol", CashedOutAtUnixMicro: testT0 + 1,
		CashOutTick: 0, CashOutMultiplierScaled: 10_000, PayoutCredits: 1_000_000_000})

	s, ev = step(t, s, cashOut("alice"), testT0+9_409_999)
	wantEvents(t, ev, BetCashedOut{Player: "alice", CashedOutAtUnixMicro: testT0 + 9_409_999,
		CashOutTick: 940, CashOutMultiplierScaled: 17_576, PayoutCredits: 1_757})

	wantReason(t, rejected(t, s, cashOut("bob"), testT0+9_410_000), ReasonDeadlinePassed)
	wantReason(t, rejected(t, s, closeRound(), testT0+9_409_999), ReasonCrashNotDue)

	s, ev = step(t, s, closeRound(), testT0+9_410_000)
	wantEvents(t, ev, RoundCrashed{ClosedAtUnixMicro: testT0 + 9_410_000, CrashPointScaled: 17_577,
		CrashTick: 941, CrashDeadlineOffsetMicros: 9_410_000, CrashDeadlineUnixMicro: testT0 + 9_410_000})

	s, ev = step(t, s, settleRound(), testT0+10_000_000)
	wantEvents(t, ev,
		BetLost{Player: "bob", BetCredits: 250},
		BetLost{Player: "dave", BetCredits: 7},
		RoundSettled{SettledAtUnixMicro: testT0 + 10_000_000, BetCount: 4,
			TotalBetCredits: 1_000_001_257, TotalPayoutCredits: 1_000_001_757, Seed: workedSeed()},
	)

	if s.Phase != PhaseSettled {
		t.Errorf("phase = %v, want SETTLED", s.Phase)
	}
	wantStatus := map[PlayerID]BetStatus{"alice": StatusCashedOut, "bob": StatusLost, "carol": StatusCashedOut, "dave": StatusLost}
	for _, b := range s.Bets {
		if b.Status != wantStatus[b.Player] {
			t.Errorf("%s status = %d, want %d", b.Player, b.Status, wantStatus[b.Player])
		}
	}
	if !VerifyCommitment(s.Seed, s.Commitment) {
		t.Error("final seed does not verify against the commitment")
	}
	if final, _ := settledWorkedRound(t); !reflect.DeepEqual(final, s) {
		t.Errorf("settledWorkedRound differs from the worked example")
	}
}

// TestEvolveAppliesDecidedEvents checks the state each decided event builds.
func TestEvolveAppliesDecidedEvents(t *testing.T) {
	s := newRound(t, testConfig(workedSeed()))
	s, _ = step(t, s, placeBet("alice", 1_000), testT0-2_000)
	s, _ = step(t, s, placeBet("bob", 250), testT0-1_000)
	if s.Phase != PhaseBetting || s.TotalBetCredits != 1_250 || len(s.Bets) != 2 {
		t.Fatalf("after bets: %+v", s)
	}
	if b := s.Bets[1]; b != (Bet{Player: "bob", BetCredits: 250, PlacedAtUnixMicro: testT0 - 1_000, Status: StatusOpen}) {
		t.Errorf("bob's bet = %+v", b)
	}

	s, _ = step(t, s, startRound(), testT0)
	if s.Phase != PhaseRunning || s.StartedAtUnixMicro != testT0 || s.CrashDeadlineUnixMicro != testT0+9_410_000 {
		t.Fatalf("after start: phase %v, T0 %d, deadline %d", s.Phase, s.StartedAtUnixMicro, s.CrashDeadlineUnixMicro)
	}

	s, _ = step(t, s, cashOut("alice"), testT0+9_409_999)
	want := Bet{Player: "alice", BetCredits: 1_000, PlacedAtUnixMicro: testT0 - 2_000, Status: StatusCashedOut,
		CashedOutAtUnixMicro: testT0 + 9_409_999, CashOutTick: 940, CashOutMultiplierScaled: 17_576, PayoutCredits: 1_757}
	if s.Bets[0] != want || s.TotalPayoutCredits != 1_757 {
		t.Fatalf("after cash-out: %+v, total payout %d", s.Bets[0], s.TotalPayoutCredits)
	}

	s, _ = step(t, s, closeRound(), testT0+9_500_000)
	if s.Phase != PhaseCrashed || s.ClosedAtUnixMicro != testT0+9_500_000 {
		t.Fatalf("after close: phase %v, closed %d", s.Phase, s.ClosedAtUnixMicro)
	}

	s, _ = step(t, s, settleRound(), testT0+9_600_000)
	if s.Phase != PhaseSettled || s.SettledAtUnixMicro != testT0+9_600_000 || s.Bets[1].Status != StatusLost {
		t.Fatalf("after settle: %+v", s)
	}
	if s.TotalBetCredits != 1_250 || s.TotalPayoutCredits != 1_757 {
		t.Errorf("totals = %d/%d", s.TotalBetCredits, s.TotalPayoutCredits)
	}
}

func TestRoundOneBetPerPlayer(t *testing.T) {
	s := newRound(t, testConfig(workedSeed()))
	s, _ = step(t, s, placeBet("alice", 1_000), testT0-2_000)

	snap := takeRoundSnapshot(s)
	wantReason(t, rejected(t, s, placeBet("alice", 5_000), testT0-1_500), ReasonDuplicateBet)
	if snap.changed(s) {
		t.Fatal("rejected duplicate bet changed the state")
	}

	s, _ = step(t, s, placeBet("bob", 500), testT0-1_000)

	forged := BetPlaced{Player: "alice", BetCredits: 2_000, PlacedAtUnixMicro: testT0 - 900}
	if next, err := (Rules{}).Evolve(s, forged); !errors.Is(err, ErrEventNotApplicable) || !reflect.DeepEqual(next, State{}) {
		t.Fatalf("forged second BetPlaced = %v, %v", next, err)
	}

	s, _ = step(t, s, startRound(), testT0)
	s, _ = step(t, s, closeRound(), testT0+9_410_000)
	s, ev := step(t, s, settleRound(), testT0+9_420_000)

	alice, lost := 0, 0
	for _, b := range s.Bets {
		if b.Player == "alice" {
			alice++
		}
	}
	for _, e := range ev {
		if l, ok := e.(BetLost); ok && l.Player == "alice" {
			lost++
		}
	}
	if alice != 1 || lost > 1 {
		t.Errorf("alice has %d bets and %d BetLost events, want 1 and at most 1", alice, lost)
	}
}

func TestRoundNoDoubleCashOut(t *testing.T) {
	s := runningRound(t, workedSeed(), "alice", "bob")
	s, ev := step(t, s, cashOut("alice"), testT0+1_000_000)
	payout := ev[0].(BetCashedOut).PayoutCredits

	wantReason(t, rejected(t, s, cashOut("alice"), testT0+2_000_000), ReasonAlreadyCashedOut)
	wantReason(t, rejected(t, s, cashOut("alice"), testT0+9_410_000), ReasonAlreadyCashedOut)

	tick := int64(150)
	m, err := MultiplierAtTick(tick)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Payout(1_000, m)
	if err != nil {
		t.Fatal(err)
	}
	forged := BetCashedOut{Player: "alice", CashedOutAtUnixMicro: testT0 + 1_500_000,
		CashOutTick: tick, CashOutMultiplierScaled: m, PayoutCredits: p}
	if _, err := (Rules{}).Evolve(s, forged); !errors.Is(err, ErrEventNotApplicable) {
		t.Fatalf("forged second BetCashedOut = %v, want ErrEventNotApplicable", err)
	}
	if s.TotalPayoutCredits != payout {
		t.Errorf("total payout = %d, want the single payout %d", s.TotalPayoutCredits, payout)
	}

	s, _ = step(t, s, closeRound(), testT0+9_410_000)
	_, ev = step(t, s, settleRound(), testT0+9_420_000)
	for _, e := range ev {
		if l, ok := e.(BetLost); ok && l.Player == "alice" {
			t.Errorf("settlement emitted BetLost for cashed-out alice")
		}
	}
}

func TestRoundDeadlineEqualityLoses(t *testing.T) {
	for _, o := range []roundOutcome{workedOutcome(), longOutcome()} {
		t.Run(o.name, func(t *testing.T) {
			s := runningRound(t, o.seed, "alice", "bob")
			deadline := testT0 + o.d

			s, ev := step(t, s, cashOut("alice"), deadline-1)
			if got := ev[0].(BetCashedOut); got.CashOutTick != o.n-1 || got.CashOutMultiplierScaled >= o.c {
				t.Errorf("cash-out at T0+D-1 = %+v, want tick %d below C", got, o.n-1)
			}
			wantReason(t, rejected(t, s, cashOut("bob"), deadline), ReasonDeadlinePassed)

			m, err := MultiplierAtTick(o.n)
			if err != nil {
				t.Fatal(err)
			}
			p, err := Payout(1_000, m)
			if err != nil {
				t.Fatal(err)
			}
			forged := BetCashedOut{Player: "bob", CashedOutAtUnixMicro: deadline,
				CashOutTick: o.n, CashOutMultiplierScaled: m, PayoutCredits: p}
			if _, err := (Rules{}).Evolve(s, forged); !errors.Is(err, ErrEventNotApplicable) {
				t.Errorf("forged cash-out at the deadline = %v, want ErrEventNotApplicable", err)
			}

			s, _ = step(t, s, closeRound(), deadline)
			_, ev = step(t, s, settleRound(), deadline+1)
			if ev[0] != (BetLost{Player: "bob", BetCredits: 1_000}) {
				t.Errorf("settlement = %#v, want bob's BetLost first", ev)
			}
		})
	}
}

func TestRoundNoCashOutAtOrAfterCrash(t *testing.T) {
	o := workedOutcome()
	s := runningRound(t, o.seed, "alice", "bob")
	deadline := testT0 + o.d
	wantReason(t, rejected(t, s, cashOut("alice"), deadline), ReasonDeadlinePassed)
	wantReason(t, rejected(t, s, cashOut("alice"), deadline+1), ReasonDeadlinePassed)
	s, _ = step(t, s, closeRound(), deadline)
	wantReason(t, rejected(t, s, cashOut("alice"), deadline+2), ReasonWrongPhase)
	s, _ = step(t, s, settleRound(), deadline+3)
	wantReason(t, rejected(t, s, cashOut("alice"), deadline+4), ReasonRoundSettled)

	t.Run("instant", func(t *testing.T) {
		s := runningRound(t, instantOutcome().seed, "alice", "bob")
		wantReason(t, rejected(t, s, cashOut("alice"), testT0), ReasonNotAfterStart)
		wantReason(t, rejected(t, s, cashOut("alice"), testT0+1), ReasonDeadlinePassed)
		s, _ = step(t, s, closeRound(), testT0)
		s, ev := step(t, s, settleRound(), testT0+1)
		if len(ev) != 3 || s.TotalPayoutCredits != 0 {
			t.Errorf("settlement = %#v, total payout %d; want every bet lost", ev, s.TotalPayoutCredits)
		}
		for _, b := range s.Bets {
			if b.Status != StatusLost {
				t.Errorf("%s status = %d, want lost", b.Player, b.Status)
			}
		}
	})

	t.Run("multiplier below crash point", func(t *testing.T) {
		for _, o := range []roundOutcome{workedOutcome(), longOutcome(), instantOutcome()} {
			s := runningRound(t, o.seed, "alice")
			for _, e := range []int64{1, o.d / 2, o.d - TickMicros, o.d - TickMicros + 1, o.d - 1, o.d, o.d + 1} {
				events, _ := Rules{}.Decide(s, cashOut("alice"), inputAt(testT0+e))
				for _, ev := range events {
					if c, ok := ev.(BetCashedOut); ok && c.CashOutMultiplierScaled >= o.c {
						t.Errorf("%s: cash-out at offset %d has multiplier %d >= C %d", o.name, e, c.CashOutMultiplierScaled, o.c)
					}
				}
			}
		}
	})
}

func TestRoundCloseWaitsForDeadline(t *testing.T) {
	o := workedOutcome()
	s := runningRound(t, o.seed, "alice")
	deadline := testT0 + o.d
	wantReason(t, rejected(t, s, closeRound(), deadline-1), ReasonCrashNotDue)
	_, ev := step(t, s, closeRound(), deadline+5_000_000)
	wantEvents(t, ev, RoundCrashed{ClosedAtUnixMicro: deadline + 5_000_000, CrashPointScaled: o.c,
		CrashTick: o.n, CrashDeadlineOffsetMicros: o.d, CrashDeadlineUnixMicro: deadline})
}

func TestRoundLifecycleAuthority(t *testing.T) {
	betting := newRound(t, testConfig(workedSeed()))
	running := runningRound(t, workedSeed(), "alice")
	crashed, _ := step(t, running, closeRound(), testT0+9_410_000)

	cases := []struct {
		name  string
		s     State
		at    int64
		build func(Authority) Command
		right Authority
	}{
		{"StartRound", betting, testT0, func(a Authority) Command { return StartRound{Authority: a} }, AuthoritySystem},
		{"CloseRound", running, testT0 + 9_410_000, func(a Authority) Command { return CloseRound{Authority: a} }, AuthoritySystem},
		{"SettleRound", crashed, testT0 + 9_420_000, func(a Authority) Command { return SettleRound{Authority: a} }, AuthoritySystem},
		{"PlaceBet", betting, testT0 - 1, func(a Authority) Command { return PlaceBet{Authority: a, Player: "zoe", BetCredits: 5} }, AuthorityPlayer},
		{"CashOut", running, testT0 + 1, func(a Authority) Command { return CashOut{Authority: a, Player: "alice"} }, AuthorityPlayer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, a := range []Authority{AuthorityNone, AuthorityPlayer, AuthoritySystem, 3, 255} {
				if a == tc.right {
					continue
				}
				wantReason(t, rejected(t, tc.s, tc.build(a), tc.at), ReasonUnauthorized)
			}
			step(t, tc.s, tc.build(tc.right), tc.at)
		})
	}
}

var allReasons = []Reason{
	ReasonInvalidCommand, ReasonUnauthorized, ReasonTimeOutOfRange, ReasonWrongPhase,
	ReasonRoundSettled, ReasonInvalidAmount, ReasonDuplicateBet, ReasonBetLimitReached,
	ReasonNoBet, ReasonAlreadyCashedOut, ReasonNotAfterStart, ReasonDeadlinePassed,
	ReasonCrashNotDue,
}

func TestRoundRejectionReasons(t *testing.T) {
	betting := func(t *testing.T) State { return newRound(t, testConfig(workedSeed())) }
	limited := func(t *testing.T) State {
		cfg := testConfig(workedSeed())
		cfg.MinBetCredits, cfg.MaxBetCredits = 10, 100
		return newRound(t, cfg)
	}
	full := func(t *testing.T) State {
		cfg := testConfig(workedSeed())
		cfg.MaxBets = 2
		s := newRound(t, cfg)
		s, _ = step(t, s, placeBet("alice", 1_000), testT0-2)
		s, _ = step(t, s, placeBet("bob", 1_000), testT0-1)
		return s
	}
	running := func(t *testing.T) State { return runningRound(t, workedSeed(), "alice", "bob") }
	cashedOut := func(t *testing.T) State {
		s, _ := step(t, running(t), cashOut("alice"), testT0+1)
		return s
	}
	crashed := func(t *testing.T) State {
		s, _ := step(t, running(t), closeRound(), testT0+9_410_000)
		return s
	}
	settled := func(t *testing.T) State {
		s, _ := step(t, crashed(t), settleRound(), testT0+9_420_000)
		return s
	}

	const deadline = testT0 + 9_410_000
	cases := []struct {
		name  string
		state func(*testing.T) State
		cmd   Command
		at    int64
		want  Reason
	}{
		{"nil command", betting, nil, testT0, ReasonInvalidCommand},
		{"pointer command", betting, &PlaceBet{Authority: AuthorityPlayer, Player: "alice", BetCredits: 5}, testT0, ReasonInvalidCommand},
		{"pointer lifecycle command", running, &CloseRound{Authority: AuthoritySystem}, deadline, ReasonInvalidCommand},
		{"bet with empty player", betting, placeBet("", 5), testT0, ReasonInvalidCommand},
		{"cash-out with empty player", running, cashOut(""), testT0 + 1, ReasonInvalidCommand},
		{"wrong authority", betting, PlaceBet{Authority: AuthoritySystem, Player: "alice", BetCredits: 5}, testT0, ReasonUnauthorized},
		{"wrong authority before phase", running, PlaceBet{Authority: AuthorityNone, Player: "zoe", BetCredits: 5}, testT0, ReasonUnauthorized},
		{"negative time bet", betting, placeBet("alice", 5), -1, ReasonTimeOutOfRange},
		{"negative time cash-out", running, cashOut("alice"), -1, ReasonTimeOutOfRange},
		{"negative time before phase", crashed, placeBet("zoe", 5), -1, ReasonTimeOutOfRange},
		{"start after max start", betting, startRound(), MaxStartMicros + 1, ReasonTimeOutOfRange},
		{"start at max int64", betting, startRound(), math.MaxInt64, ReasonTimeOutOfRange},
		{"bet while running", running, placeBet("zoe", 5), testT0 + 1, ReasonWrongPhase},
		{"bet while crashed", crashed, placeBet("zoe", 5), testT0 + 1, ReasonWrongPhase},
		{"start while running", running, startRound(), testT0 + 1, ReasonWrongPhase},
		{"start while crashed", crashed, startRound(), testT0 + 1, ReasonWrongPhase},
		{"cash-out while betting", betting, cashOut("alice"), testT0, ReasonWrongPhase},
		{"cash-out while crashed", crashed, cashOut("alice"), deadline, ReasonWrongPhase},
		{"close while betting", betting, closeRound(), deadline, ReasonWrongPhase},
		{"close while crashed", crashed, closeRound(), deadline, ReasonWrongPhase},
		{"settle while betting", betting, settleRound(), deadline, ReasonWrongPhase},
		{"settle while running", running, settleRound(), deadline, ReasonWrongPhase},
		{"bet 0", limited, placeBet("alice", 0), testT0, ReasonInvalidAmount},
		{"bet below min", limited, placeBet("alice", 9), testT0, ReasonInvalidAmount},
		{"bet above max", limited, placeBet("alice", 101), testT0, ReasonInvalidAmount},
		{"bet negative", limited, placeBet("alice", -10), testT0, ReasonInvalidAmount},
		{"existing bettor while running", running, placeBet("alice", 5), testT0 + 1, ReasonWrongPhase},
		{"duplicate bet while betting", full, placeBet("alice", 5), testT0, ReasonDuplicateBet},
		{"bet limit reached", full, placeBet("carol", 5), testT0, ReasonBetLimitReached},
		{"cash-out with no bet", running, cashOut("zoe"), testT0 + 1, ReasonNoBet},
		{"cash-out twice", cashedOut, cashOut("alice"), testT0 + 2, ReasonAlreadyCashedOut},
		{"cash-out at T0", running, cashOut("alice"), testT0, ReasonNotAfterStart},
		{"cash-out before T0", running, cashOut("alice"), testT0 - 1, ReasonNotAfterStart},
		{"cash-out at deadline", running, cashOut("alice"), deadline, ReasonDeadlinePassed},
		{"cash-out at max int64", running, cashOut("alice"), math.MaxInt64, ReasonDeadlinePassed},
		{"close before deadline", running, closeRound(), deadline - 1, ReasonCrashNotDue},
		{"settled nil", settled, nil, deadline, ReasonRoundSettled},
		{"settled bet", settled, placeBet("zoe", 5), deadline, ReasonRoundSettled},
		{"settled start", settled, startRound(), deadline, ReasonRoundSettled},
		{"settled cash-out", settled, cashOut("bob"), deadline, ReasonRoundSettled},
		{"settled close", settled, closeRound(), deadline, ReasonRoundSettled},
		{"settled settle", settled, settleRound(), deadline, ReasonRoundSettled},
		{"settled wrong authority", settled, StartRound{}, -1, ReasonRoundSettled},
	}

	covered := map[Reason]bool{}
	for _, tc := range cases {
		covered[tc.want] = true
		t.Run(tc.name, func(t *testing.T) {
			s := tc.state(t)
			snap := takeRoundSnapshot(s)
			wantReason(t, rejected(t, s, tc.cmd, tc.at), tc.want)
			if snap.changed(s) {
				t.Error("rejection changed the state")
			}
		})
	}
	for _, r := range allReasons {
		if !covered[r] {
			t.Errorf("reason %s has no rejection case", r)
		}
	}

	t.Run("boundaries accepted", func(t *testing.T) {
		step(t, betting(t), startRound(), MaxStartMicros)
		step(t, betting(t), startRound(), 0)
		step(t, limited(t), placeBet("alice", 10), testT0)
		step(t, limited(t), placeBet("alice", 100), testT0)
		step(t, running(t), closeRound(), deadline)
		step(t, running(t), cashOut("alice"), deadline-1)
	})
}

func TestRoundSettleResolvesOpenBets(t *testing.T) {
	s := runningRound(t, workedSeed(), "alice", "bob", "carol")
	s, ev := step(t, s, cashOut("bob"), testT0+1_000_000)
	payout := ev[0].(BetCashedOut).PayoutCredits
	s, _ = step(t, s, closeRound(), testT0+9_410_000)
	_, ev = step(t, s, settleRound(), testT0+9_500_000)
	wantEvents(t, ev,
		BetLost{Player: "alice", BetCredits: 1_000},
		BetLost{Player: "carol", BetCredits: 1_000},
		RoundSettled{SettledAtUnixMicro: testT0 + 9_500_000, BetCount: 3,
			TotalBetCredits: 3_000, TotalPayoutCredits: payout, Seed: workedSeed()},
	)
}

// allEventKinds returns one well-formed event of every type, each built to
// look plausible for the worked round.
func allEventKinds() []Event {
	return []Event{
		BetPlaced{Player: "zoe", BetCredits: 5, PlacedAtUnixMicro: testT0},
		RoundStarted{StartedAtUnixMicro: testT0},
		BetCashedOut{Player: "bob", CashedOutAtUnixMicro: testT0 + 1, CashOutTick: 0,
			CashOutMultiplierScaled: Scale, PayoutCredits: 250},
		RoundCrashed{ClosedAtUnixMicro: testT0 + 9_410_000, CrashPointScaled: 17_577, CrashTick: 941,
			CrashDeadlineOffsetMicros: 9_410_000, CrashDeadlineUnixMicro: testT0 + 9_410_000},
		BetLost{Player: "bob", BetCredits: 250},
		RoundSettled{SettledAtUnixMicro: testT0 + 10_000_000, BetCount: 4,
			TotalBetCredits: 1_000_001_257, TotalPayoutCredits: 1_000_001_757, Seed: workedSeed()},
	}
}

// allCommandKinds returns every command type with every authority, plus nil.
func allCommandKinds() []Command {
	cmds := []Command{nil}
	for _, a := range []Authority{AuthorityNone, AuthorityPlayer, AuthoritySystem} {
		cmds = append(cmds,
			PlaceBet{Authority: a, Player: "zoe", BetCredits: 5},
			PlaceBet{Authority: a, Player: "alice", BetCredits: 1_000},
			StartRound{Authority: a},
			CashOut{Authority: a, Player: "bob"},
			CashOut{Authority: a, Player: "alice"},
			CloseRound{Authority: a},
			SettleRound{Authority: a},
		)
	}
	return cmds
}

func TestRoundSettledStateImmutable(t *testing.T) {
	s, history := settledWorkedRound(t)
	s = withSpareCap(s)
	snap := takeRoundSnapshot(s)
	proj, err := Project(s)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range allCommandKinds() {
		for _, at := range []int64{-1, 0, testT0, testT0 + 1, testT0 + 9_410_000, testT0 + 20_000_000, math.MaxInt64} {
			events, err := Rules{}.Decide(s, c, inputAt(at))
			if events != nil || err == nil {
				t.Errorf("Decide(%#v at %d) on a settled round = %#v, %v", c, at, events, err)
			}
		}
	}
	for _, e := range append(allEventKinds(), history...) {
		if next, err := (Rules{}).Evolve(s, e); err == nil {
			t.Errorf("Evolve(%#v) on a settled round = %+v", e, next)
		}
	}

	if snap.changed(s) {
		t.Fatal("settled state changed")
	}
	if got, err := Project(s); err != nil || !reflect.DeepEqual(got, proj) {
		t.Fatalf("projection changed: %+v, %v", got, err)
	}
}

func TestRoundNoTransitionsAfterSettlement(t *testing.T) {
	s, history := settledWorkedRound(t)
	for _, c := range allCommandKinds() {
		for _, at := range []int64{-1, testT0 + 20_000_000} {
			wantReason(t, rejected(t, s, c, at), ReasonRoundSettled)
		}
	}
	for _, e := range append(allEventKinds(), history...) {
		next, err := Rules{}.Evolve(s, e)
		if !errors.Is(err, ErrEventNotApplicable) || !reflect.DeepEqual(next, State{}) {
			t.Errorf("Evolve(%#v) on a settled round = %+v, %v; want ErrEventNotApplicable", e, next, err)
		}
	}
}

// TestRoundDecideFailsClosedOnOverflow checks that Decide turns an
// overflowing aggregate or deadline sum into ErrArithmetic: no events, no
// terminal rejection and an unchanged input state (math spec D-7).
func TestRoundDecideFailsClosedOnOverflow(t *testing.T) {
	betting := newRound(t, testConfig(workedSeed()))
	betting, _ = step(t, betting, placeBet("alice", 1_000), testT0-1)
	betTotal := betting
	betTotal.TotalBetCredits = math.MaxInt64 - 5

	deadline := withSpareCap(betting)
	deadline.CrashDeadlineOffsetMicros = math.MaxInt64

	running := runningRound(t, workedSeed(), "alice")
	payoutTotal := running
	payoutTotal.TotalPayoutCredits = math.MaxInt64 - 999

	cases := []struct {
		name string
		s    State
		c    Command
		at   int64
	}{
		{"bet total", withSpareCap(betTotal), placeBet("bob", 6), testT0},
		{"start deadline", deadline, startRound(), MaxStartMicros},
		{"start deadline at zero", deadline, startRound(), 1},
		{"payout total", withSpareCap(payoutTotal), cashOut("alice"), testT0 + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := takeRoundSnapshot(tc.s)
			events, err := Rules{}.Decide(tc.s, tc.c, inputAt(tc.at))
			if events != nil || !errors.Is(err, ErrArithmetic) || errors.Is(err, ErrRejected) {
				t.Errorf("Decide = %#v, %v; want ErrArithmetic without events or rejection", events, err)
			}
			if _, ok := RejectionReason(err); ok {
				t.Errorf("overflow reported as a terminal rejection: %v", err)
			}
			if snap.changed(tc.s) {
				t.Error("failed Decide changed the input state")
			}
		})
	}

	// One credit less fits exactly, so the guards fire on overflow only.
	betFits := betTotal
	betFits.TotalBetCredits = math.MaxInt64 - 6
	if _, ev := step(t, betFits, placeBet("bob", 6), testT0); len(ev) != 1 {
		t.Errorf("bet filling int64 exactly = %#v", ev)
	}
	payoutFits := payoutTotal
	payoutFits.TotalPayoutCredits = math.MaxInt64 - 1_000
	if next, _ := step(t, payoutFits, cashOut("alice"), testT0+1); next.TotalPayoutCredits != math.MaxInt64 {
		t.Errorf("payout total = %d, want MaxInt64", next.TotalPayoutCredits)
	}
}

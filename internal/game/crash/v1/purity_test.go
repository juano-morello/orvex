package v1

import (
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/juano-morello/orvex/internal/game"
)

type roundGame = game.Game[State, Command, Event, Config]

// erasedRules runs Rules through the type-erased Adapter, so every purity
// check covers both the typed rules and the runtime boundary.
type erasedRules struct{ a game.Adapter }

func (e erasedRules) InitialState(cfg Config) (State, error) {
	s, err := e.a.InitialState(cfg)
	return roundStateOf(s), err
}

func (e erasedRules) Decide(s State, c Command, in game.Input) ([]Event, error) {
	out, err := e.a.Decide(s, c, in)
	if out == nil {
		return nil, err
	}
	events := make([]Event, len(out))
	for i, ev := range out {
		events[i] = ev.(Event)
	}
	return events, err
}

func (e erasedRules) Evolve(s State, ev Event) (State, error) {
	next, err := e.a.Evolve(s, ev)
	return roundStateOf(next), err
}

func roundStateOf(v any) State {
	if v == nil {
		return State{}
	}
	return v.(State)
}

func rulesUnderTest() []struct {
	name string
	g    roundGame
} {
	return []struct {
		name string
		g    roundGame
	}{
		{"typed", Rules{}},
		{"adapter", erasedRules{Adapter()}},
	}
}

// roundSnapshot is a deep copy of a State plus the full backing array of its
// Bets, so writes past len (append aliasing) are caught too.
type roundSnapshot struct {
	state   State
	backing []Bet
}

func takeRoundSnapshot(s State) roundSnapshot {
	cp := s
	cp.Bets = slices.Clone(s.Bets)
	return roundSnapshot{state: cp, backing: slices.Clone(s.Bets[:cap(s.Bets)])}
}

func (snap roundSnapshot) changed(s State) bool {
	return !reflect.DeepEqual(snap.state, s) || !slices.Equal(snap.backing, s.Bets[:cap(s.Bets)])
}

// withSpareCap re-allocates s.Bets with spare capacity, so an append into the
// caller's backing array is observable.
func withSpareCap(s State) State {
	b := make([]Bet, len(s.Bets), len(s.Bets)+8)
	copy(b, s.Bets)
	s.Bets = b
	return s
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// roundPurityViolation runs Decide twice, then Evolve twice for every
// produced event, all against the same s. After each Evolve it overwrites the
// returned bets, so a returned state that aliases s is caught. It returns a
// description of the first non-deterministic result or mutation of s, or ""
// if every call was pure.
func roundPurityViolation(g roundGame, s State, c Command, in game.Input) string {
	snap := takeRoundSnapshot(s)

	ev1, err1 := g.Decide(s, c, in)
	if snap.changed(s) {
		return "first Decide mutated the caller's state"
	}
	ev2, err2 := g.Decide(s, c, in)
	if snap.changed(s) {
		return "second Decide mutated the caller's state"
	}
	if !reflect.DeepEqual(ev1, ev2) {
		return fmt.Sprintf("Decide events differ: %#v vs %#v", ev1, ev2)
	}
	if errText(err1) != errText(err2) {
		return fmt.Sprintf("Decide errors differ: %v vs %v", err1, err2)
	}

	for i, ev := range ev1 {
		next1, eerr1 := g.Evolve(s, ev)
		if snap.changed(s) {
			return fmt.Sprintf("first Evolve of event %d mutated the caller's state", i)
		}
		next2, eerr2 := g.Evolve(s, ev)
		if snap.changed(s) {
			return fmt.Sprintf("second Evolve of event %d mutated the caller's state", i)
		}
		if !reflect.DeepEqual(next1, next2) {
			return fmt.Sprintf("Evolve of event %d states differ: %#v vs %#v", i, next1, next2)
		}
		if errText(eerr1) != errText(eerr2) {
			return fmt.Sprintf("Evolve of event %d errors differ: %v vs %v", i, eerr1, eerr2)
		}
		for _, next := range []State{next1, next2} {
			for j := range next.Bets {
				next.Bets[j] = Bet{Player: "overwritten", BetCredits: -1}
			}
			if snap.changed(s) {
				return fmt.Sprintf("state returned by Evolve of event %d aliases the caller's bets", i)
			}
		}
	}
	return ""
}

// purityStates returns one state per phase, the betting and running ones
// with bets, each with spare Bets capacity.
func purityStates(t *testing.T) map[string]State {
	fresh := newRound(t, testConfig(workedSeed()))
	betting, _ := step(t, fresh, placeBet("alice", 1_000), testT0-2_000)
	betting, _ = step(t, betting, placeBet("bob", 250), testT0-1_000)
	running, _ := step(t, betting, startRound(), testT0)
	running, _ = step(t, running, cashOut("alice"), testT0+1_000_000)
	crashed, _ := step(t, running, closeRound(), testT0+9_410_000)
	settled, _ := step(t, crashed, settleRound(), testT0+9_420_000)
	states := map[string]State{
		"fresh betting": fresh,
		"betting":       betting,
		"running":       running,
		"crashed":       crashed,
		"settled":       settled,
	}
	for k, s := range states {
		states[k] = withSpareCap(s)
	}
	return states
}

func TestRulesPurity(t *testing.T) {
	const d = 9_410_000
	cmds := []Command{nil}
	for _, a := range []Authority{AuthorityPlayer, AuthoritySystem} {
		cmds = append(cmds,
			PlaceBet{Authority: a, Player: "zoe", BetCredits: 5},
			StartRound{Authority: a},
			CashOut{Authority: a, Player: "bob"},
			CloseRound{Authority: a},
			SettleRound{Authority: a},
		)
	}
	times := []int64{testT0 - 1, testT0 + 1, testT0 + d - 1, testT0 + d, testT0 + d + 1}

	for name, s := range purityStates(t) {
		for _, c := range cmds {
			for _, at := range times {
				for _, r := range rulesUnderTest() {
					in := game.Input{Sequence: 3, EffectiveTimeUnixMicro: at}
					if v := roundPurityViolation(r.g, s, c, in); v != "" {
						t.Errorf("%s/%#v/%d/%s: %s", name, c, at, r.name, v)
					}
				}
			}
		}
	}
}

type evolveWritesBet struct{ Rules }

func (evolveWritesBet) Evolve(s State, e Event) (State, error) {
	s.Bets[0].Status = StatusLost
	return Rules{}.Evolve(s, e)
}

type evolveAppendsSpare struct{ Rules }

func (evolveAppendsSpare) Evolve(s State, _ Event) (State, error) {
	s.Bets = append(s.Bets, Bet{Player: "leak"})
	return s, nil
}

type evolveSharesBets struct{ Rules }

func (evolveSharesBets) Evolve(s State, e Event) (State, error) {
	next, err := Rules{}.Evolve(s, e)
	next.Bets = s.Bets
	return next, err
}

// TestRoundPurityCheckDetectsMutation is the negative control for
// TestRulesPurity: leaky rules must be caught.
func TestRoundPurityCheckDetectsMutation(t *testing.T) {
	running := purityStates(t)["running"]
	cases := []struct {
		name string
		g    roundGame
	}{
		{"evolve writes a bet", evolveWritesBet{}},
		{"evolve appends into spare capacity", evolveAppendsSpare{}},
		{"evolve returns shared bets", evolveSharesBets{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v := roundPurityViolation(tc.g, running, cashOut("bob"), inputAt(testT0+2_000_000)); v == "" {
				t.Fatal("roundPurityViolation did not detect the mutation")
			}
		})
	}
}

func TestRulesConcurrentUse(t *testing.T) {
	shared := purityStates(t)["running"]
	snap := takeRoundSnapshot(shared)
	cmd := cashOut("bob")
	in := inputAt(testT0 + 2_000_000)

	g := Rules{}
	wantEvents, err := g.Decide(shared, cmd, in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	wantNext, err := g.Evolve(shared, wantEvents[0])
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	const workers = 8
	events := make([][]Event, workers)
	nexts := make([]State, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev, err := g.Decide(shared, cmd, in)
			if err != nil {
				errs[i] = err
				return
			}
			events[i] = ev
			nexts[i], errs[i] = g.Evolve(shared, ev[0])
		}()
	}
	wg.Wait()

	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if !reflect.DeepEqual(events[i], wantEvents) {
			t.Errorf("worker %d events = %#v, want %#v", i, events[i], wantEvents)
		}
		if !reflect.DeepEqual(nexts[i], wantNext) {
			t.Errorf("worker %d next state = %#v, want %#v", i, nexts[i], wantNext)
		}
	}
	if snap.changed(shared) {
		t.Fatalf("shared state changed: %#v", shared)
	}
}

func TestRoundErrorsDoNotLeakSecrets(t *testing.T) {
	seed := workedSeed()
	const deadline = testT0 + 9_410_000
	var errs []error
	collect := func(_ any, err error) {
		if err == nil {
			t.Fatal("expected an error")
		}
		errs = append(errs, err)
	}

	r := Rules{}
	betting := newRound(t, testConfig(seed))
	running := runningRound(t, seed, "alice", "bob")
	cashed, _ := step(t, running, cashOut("alice"), testT0+1)
	crashed, _ := step(t, cashed, closeRound(), deadline)
	settled, _ := step(t, crashed, settleRound(), deadline+1)

	// Rejections.
	collect(r.Decide(betting, cashOut("alice"), inputAt(testT0)))
	collect(r.Decide(betting, placeBet("", 5), inputAt(testT0)))
	collect(r.Decide(betting, startRound(), inputAt(-1)))
	collect(r.Decide(running, cashOut("alice"), inputAt(testT0)))
	collect(r.Decide(running, cashOut("alice"), inputAt(deadline)))
	collect(r.Decide(running, cashOut("zoe"), inputAt(deadline)))
	collect(r.Decide(running, closeRound(), inputAt(deadline-1)))
	collect(r.Decide(running, StartRound{}, inputAt(deadline)))
	collect(r.Decide(cashed, cashOut("alice"), inputAt(deadline)))
	collect(r.Decide(settled, closeRound(), inputAt(deadline)))

	// Evolve errors on forged events.
	crash := RoundCrashed{ClosedAtUnixMicro: deadline, CrashPointScaled: 17_577, CrashTick: 941,
		CrashDeadlineOffsetMicros: 9_410_000, CrashDeadlineUnixMicro: deadline}
	for _, f := range []func(*RoundCrashed){
		func(e *RoundCrashed) { e.ClosedAtUnixMicro-- },
		func(e *RoundCrashed) { e.CrashPointScaled++ },
		func(e *RoundCrashed) { e.CrashTick++ },
		func(e *RoundCrashed) { e.CrashDeadlineOffsetMicros++ },
		func(e *RoundCrashed) { e.CrashDeadlineUnixMicro++ },
	} {
		e := crash
		f(&e)
		collect(r.Evolve(running, e))
	}
	// Resolve bob's loss first, so the forged RoundSettled differs from an
	// applicable one only in its seed and reaches the seed guard.
	lost, err := r.Evolve(crashed, BetLost{Player: "bob", BetCredits: 1_000})
	if err != nil {
		t.Fatalf("Evolve(BetLost): %v", err)
	}
	goodSettle := RoundSettled{SettledAtUnixMicro: deadline + 1, BetCount: 2,
		TotalBetCredits: 2_000, TotalPayoutCredits: lost.TotalPayoutCredits, Seed: seed}
	if _, err := r.Evolve(lost, goodSettle); err != nil {
		t.Fatalf("Evolve(RoundSettled) with the right seed: %v", err)
	}
	wrongSeed := goodSettle
	wrongSeed.Seed[0] ^= 1
	_, seedErr := r.Evolve(lost, wrongSeed)
	if !errors.Is(seedErr, ErrEventNotApplicable) || !strings.Contains(errText(seedErr), "seed does not match") {
		t.Fatalf("Evolve(RoundSettled) with a wrong seed = %v, want the seed-mismatch error", seedErr)
	}
	collect(nil, seedErr)
	badCommitment := lost
	badCommitment.Commitment[0] ^= 1
	_, commitErr := r.Evolve(badCommitment, goodSettle)
	if !errors.Is(commitErr, ErrEventNotApplicable) || !strings.Contains(errText(commitErr), "commitment does not match the seed") {
		t.Fatalf("Evolve(RoundSettled) on a state with a wrong commitment = %v, want the commitment-mismatch error", commitErr)
	}
	collect(nil, commitErr)
	badCrash := lost
	badCrash.CrashPointScaled++
	_, crashErr := r.Evolve(badCrash, goodSettle)
	if !errors.Is(crashErr, ErrEventNotApplicable) || !strings.Contains(errText(crashErr), "crash point does not match the seed") {
		t.Fatalf("Evolve(RoundSettled) on a state with a wrong crash point = %v, want the crash-point-mismatch error", crashErr)
	}
	collect(nil, crashErr)
	m, err := MultiplierAtTick(941)
	if err != nil {
		t.Fatal(err)
	}
	collect(r.Evolve(running, BetCashedOut{Player: "bob", CashedOutAtUnixMicro: deadline,
		CashOutTick: 941, CashOutMultiplierScaled: m, PayoutCredits: 1_000 * m / Scale}))
	collect(r.Evolve(settled, crash))

	// Broken states carrying the worked secrets.
	broken := settled
	broken.Phase = 99
	collect(Project(broken))
	collect(r.Decide(broken, closeRound(), inputAt(deadline)))
	collect(r.Evolve(broken, crash))
	brokenRunning := running
	brokenRunning.StartedAtUnixMicro = -1
	collect(r.Decide(brokenRunning, cashOut("alice"), inputAt(deadline-1)))
	brokenRunning = running
	brokenRunning.CrashTick = MaxTick + 1
	collect(r.Decide(brokenRunning, cashOut("alice"), inputAt(testT0+1)))

	secrets := []string{
		"17577",
		"9410000",
		strconv.FormatInt(deadline, 10),
		hex.EncodeToString(seed[:]),
	}
	for _, err := range errs {
		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q leaks %s", err, secret)
			}
		}
	}
}

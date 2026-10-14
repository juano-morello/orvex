package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/game"
)

// testT0 is the round start time used by the lifecycle tests.
const testT0 int64 = 1_760_000_000_000_000

// Indexes of lifecycleSeed with known outcomes (plan section 1, computed by an
// independent Python script): an instant crash (C = 1.0000x, D = 0) and a long
// round (C = 19.6616x).
const (
	instantSeedIndex = 58
	longSeedIndex    = 6
)

// workedRoundID is the math spec worked-example round ID
// 0190f3a8-7b3c-7d4e-8f00-000000000001.
var workedRoundID = [16]byte{0x01, 0x90, 0xf3, 0xa8, 0x7b, 0x3c, 0x7d, 0x4e, 0x8f, 0x00, 0, 0, 0, 0, 0, 0x01}

// workedSeed returns the math spec D-10 worked-example seed: bytes 0 to 31.
func workedSeed() [32]byte {
	var s [32]byte
	for i := range s {
		s[i] = byte(i)
	}
	return s
}

func lifecycleSeed(i int) [32]byte {
	return sha256.Sum256([]byte("orvex/crash/v1/lifecycle-test/" + strconv.Itoa(i)))
}

// roundOutcome is an expected crash outcome of a test seed with workedRoundID.
type roundOutcome struct {
	name    string
	seed    [32]byte
	c, n, d int64
}

func workedOutcome() roundOutcome {
	return roundOutcome{"worked", workedSeed(), 17_577, 941, 9_410_000}
}

func instantOutcome() roundOutcome {
	return roundOutcome{"instant", lifecycleSeed(instantSeedIndex), 10_000, 0, 0}
}

func longOutcome() roundOutcome {
	return roundOutcome{"long", lifecycleSeed(longSeedIndex), 196_616, 4_965, 49_650_000}
}

func testConfig(seed [32]byte) Config {
	return Config{
		RoundID:       workedRoundID,
		Seed:          seed,
		MinBetCredits: 1,
		MaxBetCredits: MaxBetCredits,
		MaxBets:       100,
	}
}

func newRound(t testing.TB, cfg Config) State {
	t.Helper()
	s, err := Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatalf("InitialState: %v", err)
	}
	return s
}

func placeBet(p PlayerID, credits int64) Command {
	return PlaceBet{Authority: AuthorityPlayer, Player: p, BetCredits: credits}
}
func cashOut(p PlayerID) Command { return CashOut{Authority: AuthorityPlayer, Player: p} }
func startRound() Command        { return StartRound{Authority: AuthoritySystem} }
func closeRound() Command        { return CloseRound{Authority: AuthoritySystem} }
func settleRound() Command       { return SettleRound{Authority: AuthoritySystem} }

func inputAt(at int64) game.Input {
	return game.Input{Sequence: 1, EffectiveTimeUnixMicro: at}
}

// step decides c at time at and evolves every produced event, failing the
// test on any error.
func step(t testing.TB, s State, c Command, at int64) (State, []Event) {
	t.Helper()
	events, err := Rules{}.Decide(s, c, inputAt(at))
	if err != nil {
		t.Fatalf("Decide(%#v at %d): %v", c, at, err)
	}
	if len(events) == 0 {
		t.Fatalf("Decide(%#v at %d) returned no events", c, at)
	}
	for _, e := range events {
		s, err = Rules{}.Evolve(s, e)
		if err != nil {
			t.Fatalf("Evolve(%#v): %v", e, err)
		}
	}
	return s, events
}

// rejected decides c at time at and returns the rejection reason, failing the
// test unless Decide rejected the command without events.
func rejected(t testing.TB, s State, c Command, at int64) Reason {
	t.Helper()
	events, err := Rules{}.Decide(s, c, inputAt(at))
	if events != nil {
		t.Fatalf("Decide(%#v at %d) = %#v, want no events", c, at, events)
	}
	r, ok := RejectionReason(err)
	if !ok || !errors.Is(err, ErrRejected) {
		t.Fatalf("Decide(%#v at %d) error = %v, want a rejection", c, at, err)
	}
	return r
}

// runningRound places a 1,000-credit bet for each player and starts the round
// at testT0.
func runningRound(t testing.TB, seed [32]byte, players ...PlayerID) State {
	t.Helper()
	s := newRound(t, testConfig(seed))
	for _, p := range players {
		s, _ = step(t, s, placeBet(p, 1_000), testT0-1_000)
	}
	s, _ = step(t, s, startRound(), testT0)
	return s
}

func TestInitialStateDerivesOutcome(t *testing.T) {
	for _, o := range []roundOutcome{workedOutcome(), instantOutcome(), longOutcome()} {
		t.Run(o.name, func(t *testing.T) {
			cfg := testConfig(o.seed)
			s := newRound(t, cfg)
			if s.Phase != PhaseBetting || s.Seed != o.seed || s.RoundID != workedRoundID {
				t.Errorf("phase/seed/round = %v/%x/%x", s.Phase, s.Seed, s.RoundID)
			}
			if s.MinBetCredits != cfg.MinBetCredits || s.MaxBetCredits != cfg.MaxBetCredits || s.MaxBets != cfg.MaxBets {
				t.Errorf("limits = %d/%d/%d, want config's", s.MinBetCredits, s.MaxBetCredits, s.MaxBets)
			}
			if s.Commitment != Commitment(o.seed) {
				t.Errorf("commitment = %x, want SHA-256(seed)", s.Commitment)
			}
			if s.CrashPointScaled != o.c || s.CrashTick != o.n || s.CrashDeadlineOffsetMicros != o.d {
				t.Errorf("outcome = C %d, n_c %d, D %d; want %d, %d, %d",
					s.CrashPointScaled, s.CrashTick, s.CrashDeadlineOffsetMicros, o.c, o.n, o.d)
			}
			if len(s.Bets) != 0 || s.TotalBetCredits != 0 || s.TotalPayoutCredits != 0 {
				t.Errorf("bets/totals = %v/%d/%d, want empty", s.Bets, s.TotalBetCredits, s.TotalPayoutCredits)
			}
			if s.StartedAtUnixMicro != 0 || s.CrashDeadlineUnixMicro != 0 || s.ClosedAtUnixMicro != 0 || s.SettledAtUnixMicro != 0 {
				t.Errorf("times not zero: %+v", s)
			}
		})
	}
	s := newRound(t, testConfig(workedSeed()))
	if got := hex.EncodeToString(s.Commitment[:]); got != "630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd" {
		t.Errorf("worked commitment = %s", got)
	}
}

func TestInitialStateRejectsInvalidConfig(t *testing.T) {
	base := testConfig(workedSeed())
	with := func(f func(*Config)) Config {
		c := base
		f(&c)
		return c
	}
	invalid := map[string]Config{
		"nil round id":      with(func(c *Config) { c.RoundID = [16]byte{} }),
		"zero seed":         with(func(c *Config) { c.Seed = [32]byte{} }),
		"min bet 0":         with(func(c *Config) { c.MinBetCredits = 0 }),
		"min bet -1":        with(func(c *Config) { c.MinBetCredits = -1 }),
		"max bet above cap": with(func(c *Config) { c.MaxBetCredits = MaxBetCredits + 1 }),
		"min above max":     with(func(c *Config) { c.MinBetCredits, c.MaxBetCredits = 10, 9 }),
		"max bets 0":        with(func(c *Config) { c.MaxBets = 0 }),
		"max bets -1":       with(func(c *Config) { c.MaxBets = -1 }),
		"max bets above":    with(func(c *Config) { c.MaxBets = MaxBetsPerRound + 1 }),
	}
	for name, cfg := range invalid {
		t.Run(name, func(t *testing.T) {
			s, err := Rules{}.InitialState(cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("err = %v, want ErrInvalidConfig", err)
			}
			if !reflect.DeepEqual(s, State{}) {
				t.Errorf("state = %+v, want State{}", s)
			}
		})
	}
	valid := map[string]Config{
		"max bets at cap":   with(func(c *Config) { c.MaxBets = MaxBetsPerRound }),
		"min = max = 1":     with(func(c *Config) { c.MinBetCredits, c.MaxBetCredits = 1, 1 }),
		"min = max = limit": with(func(c *Config) { c.MinBetCredits, c.MaxBetCredits = MaxBetCredits, MaxBetCredits }),
	}
	for name, cfg := range valid {
		t.Run(name, func(t *testing.T) {
			if _, err := (Rules{}).InitialState(cfg); err != nil {
				t.Errorf("InitialState: %v", err)
			}
		})
	}
}

func TestAdapterRegistration(t *testing.T) {
	want := game.Descriptor{Game: "crash", RulesVersion: 1, SchemaVersion: 1, StateVersion: 1}
	if Descriptor() != want {
		t.Fatalf("Descriptor() = %+v, want %+v", Descriptor(), want)
	}
	reg, err := game.NewRegistry(Adapter())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	a, err := reg.Lookup("crash", 1)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if a.Descriptor() != want {
		t.Errorf("adapter descriptor = %+v, want %+v", a.Descriptor(), want)
	}

	s0, err := a.InitialState(testConfig(workedSeed()))
	if err != nil {
		t.Fatalf("InitialState: %v", err)
	}
	events, err := a.Decide(s0, placeBet("alice", 1_000), inputAt(testT0))
	if err != nil || len(events) != 1 {
		t.Fatalf("Decide = %v, %v", events, err)
	}
	s1, err := a.Evolve(s0, events[0])
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	if bets := s1.(State).Bets; len(bets) != 1 || bets[0].Player != "alice" {
		t.Errorf("bets = %+v, want alice's bet", bets)
	}

	if _, err := a.Decide(s1, placeBet("alice", 5), inputAt(testT0)); err == nil {
		t.Error("duplicate bet accepted through the adapter")
	} else if r, ok := RejectionReason(err); !ok || r != ReasonDuplicateBet {
		t.Errorf("duplicate bet = %v, want %s", err, ReasonDuplicateBet)
	}

	for _, bad := range []any{struct{}{}, nil} {
		if _, err := a.Decide(bad, placeBet("bob", 1), inputAt(testT0)); !errors.Is(err, game.ErrTypeMismatch) {
			t.Errorf("Decide(state %#v) = %v, want ErrTypeMismatch", bad, err)
		}
	}
	if _, err := reg.Lookup("crash", 2); !errors.Is(err, game.ErrUnknownRules) {
		t.Errorf("Lookup(crash, 2) = %v, want ErrUnknownRules", err)
	}

	// The adapter carries the canonical codec (LBT-9, A-6).
	b, err := a.EncodeEvent(RoundStarted{StartedAtUnixMicro: 1760000000000000})
	if want := `{"startedAtUnixMicro":"1760000000000000","type":"RoundStarted"}`; err != nil || string(b) != want {
		t.Errorf("EncodeEvent(RoundStarted) = %s, %v; want %s", b, err, want)
	}
	if _, err := a.EncodeEvent(struct{}{}); !errors.Is(err, game.ErrTypeMismatch) {
		t.Errorf("EncodeEvent(struct{}{}) = %v, want ErrTypeMismatch", err)
	}
}

func TestRoundAddNonNeg(t *testing.T) {
	const a int64 = 1_000
	ok := []struct{ a, b, want int64 }{
		{0, 0, 0},
		{a, math.MaxInt64 - a, math.MaxInt64},
		{math.MaxInt64, 0, math.MaxInt64},
		{0, math.MaxInt64, math.MaxInt64},
	}
	for _, c := range ok {
		if got, err := addNonNeg(c.a, c.b); err != nil || got != c.want {
			t.Errorf("addNonNeg(%d, %d) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
	}
	fail := []struct{ a, b int64 }{
		{a, math.MaxInt64 - a + 1},
		{math.MaxInt64, 1},
		{1, math.MaxInt64},
		{math.MaxInt64, math.MaxInt64},
		{-1, 0},
		{0, -1},
		{-1, 2},
		{math.MinInt64, math.MaxInt64},
	}
	for _, c := range fail {
		got, err := addNonNeg(c.a, c.b)
		if !errors.Is(err, ErrArithmetic) || got != 0 {
			t.Errorf("addNonNeg(%d, %d) = %d, %v; want ErrArithmetic", c.a, c.b, got, err)
			continue
		}
		// One operand may be the secret D, so the text names neither.
		for _, op := range []int64{c.a, c.b} {
			if s := strconv.FormatInt(op, 10); op > 9 && strings.Contains(err.Error(), s) {
				t.Errorf("addNonNeg(%d, %d) error %q formats operand %s", c.a, c.b, err, s)
			}
		}
	}
}

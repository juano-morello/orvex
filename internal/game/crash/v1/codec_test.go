package v1

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
)

var _ game.Codec[State, Command, Event, Config] = Codec{}

// p3 is the escaping player of spec section 5.6: space, quotes, snowman,
// backslash, U+0001 and U+1F600.
const p3 PlayerID = "zoë \"☃\"\\\u0001😀"

// workedCodecConfig is the config of the LBT-9 worked example (spec section
// 8.2): maxBets is 3, unlike testConfig.
func workedCodecConfig() Config {
	return Config{
		RoundID:       workedRoundID,
		Seed:          workedSeed(),
		MinBetCredits: 1,
		MaxBetCredits: 1_000_000_000,
		MaxBets:       3,
	}
}

// codecVectorLines are the payload lines of spec section 5.6, typed in from
// the spec.
const (
	vecPlaceBet     = `{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"}`
	vecStartRound   = `{"authority":"system","type":"StartRound"}`
	vecCashOut      = `{"authority":"player","player":"alice","type":"CashOut"}`
	vecCloseRound   = `{"authority":"system","type":"CloseRound"}`
	vecSettleRound  = `{"authority":"system","type":"SettleRound"}`
	vecBetPlaced    = `{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"}`
	vecRoundStarted = `{"startedAtUnixMicro":"1760000000000000","type":"RoundStarted"}`
	vecBetCashedOut = `{"cashOutMultiplierScaled":"13498","cashOutTick":"500","cashedOutAtUnixMicro":"1760000005000000","payoutCredits":"134","player":"alice","type":"BetCashedOut"}`
	vecRoundCrashed = `{"closedAtUnixMicro":"1760000009410000","crashDeadlineOffsetMicros":"9410000","crashDeadlineUnixMicro":"1760000009410000","crashPointScaled":"17577","crashTick":"941","type":"RoundCrashed"}`
	vecBetLost      = `{"betCredits":"250","player":"bob","type":"BetLost"}`
	vecRoundSettled = `{"betCount":"3","seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f","settledAtUnixMicro":"1760000010000000","totalBetCredits":"1000000350","totalPayoutCredits":"1757600134","type":"RoundSettled"}`
	vecConfig       = `{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1"}`
	vecConfigSecret = `{"seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"}`
	vecBet          = `{"betCredits":"100","cashOutMultiplierScaled":"13498","cashOutTick":"500","cashedOutAtUnixMicro":"1760000005000000","payoutCredits":"134","placedAtUnixMicro":"1759999980000000","player":"alice","status":"cashed_out"}`
	vecResult       = `{"reason":"duplicate_bet","status":"rejected"}`
	vecP3Hex        = "7b22617574686f72697479223a22706c61796572222c2262657443726564697473223a2231303030303030303030222c22706c61796572223a227a6fc3ab205c22e298835c225c5c5c7530303031f09f9880222c2274797065223a22506c616365426574227d"
	vecInitialState = `{"bets":[],"closedAtUnixMicro":"0","commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","crashDeadlineOffsetMicros":"9410000","crashDeadlineUnixMicro":"0","crashPointScaled":"17577","crashTick":"941","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1","phase":"BETTING","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001","seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f","settledAtUnixMicro":"0","startedAtUnixMicro":"0","totalBetCredits":"0","totalPayoutCredits":"0"}`
)

func TestCodecVectors(t *testing.T) {
	c := Codec{}
	cfg := workedCodecConfig()

	commands := []struct {
		c    Command
		want string
	}{
		{placeBet("alice", 100), vecPlaceBet},
		{startRound(), vecStartRound},
		{cashOut("alice"), vecCashOut},
		{closeRound(), vecCloseRound},
		{settleRound(), vecSettleRound},
	}
	for _, tc := range commands {
		b, err := c.EncodeCommand(tc.c)
		if err != nil || string(b) != tc.want {
			t.Errorf("EncodeCommand(%#v) = %s, %v\nwant %s", tc.c, b, err, tc.want)
		}
	}

	events := []struct {
		e    Event
		want string
	}{
		{BetPlaced{Player: "alice", BetCredits: 100, PlacedAtUnixMicro: testT0 - 20_000_000}, vecBetPlaced},
		{RoundStarted{StartedAtUnixMicro: testT0}, vecRoundStarted},
		{BetCashedOut{Player: "alice", CashedOutAtUnixMicro: testT0 + 5_000_000, CashOutTick: 500,
			CashOutMultiplierScaled: 13_498, PayoutCredits: 134}, vecBetCashedOut},
		{RoundCrashed{ClosedAtUnixMicro: testT0 + 9_410_000, CrashPointScaled: 17_577, CrashTick: 941,
			CrashDeadlineOffsetMicros: 9_410_000, CrashDeadlineUnixMicro: testT0 + 9_410_000}, vecRoundCrashed},
		{BetLost{Player: "bob", BetCredits: 250}, vecBetLost},
		{RoundSettled{SettledAtUnixMicro: testT0 + 10_000_000, BetCount: 3, TotalBetCredits: 1_000_000_350,
			TotalPayoutCredits: 1_757_600_134, Seed: workedSeed()}, vecRoundSettled},
	}
	for _, tc := range events {
		b, err := c.EncodeEvent(tc.e)
		if err != nil || string(b) != tc.want {
			t.Errorf("EncodeEvent(%#v) = %s, %v\nwant %s", tc.e, b, err, tc.want)
		}
	}

	pub, sec, err := c.EncodeConfig(cfg)
	if err != nil || string(pub) != vecConfig || string(sec) != vecConfigSecret {
		t.Errorf("EncodeConfig = %s, %s, %v\nwant %s, %s", pub, sec, err, vecConfig, vecConfigSecret)
	}

	b, err := c.EncodeCommand(PlaceBet{Authority: AuthorityPlayer, Player: p3, BetCredits: 1_000_000_000})
	if err != nil || hex.EncodeToString(b) != vecP3Hex {
		t.Errorf("EncodeCommand(P3) = %x, %v\nwant %s", b, err, vecP3Hex)
	}

	s, err := Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatalf("InitialState: %v", err)
	}
	b, err = c.EncodeState(s)
	if err != nil || string(b) != vecInitialState {
		t.Errorf("EncodeState(InitialState) = %s, %v\nwant %s", b, err, vecInitialState)
	}

	// The state bet object, extracted from a state holding that bet.
	s.Bets = []Bet{{Player: "alice", BetCredits: 100, PlacedAtUnixMicro: testT0 - 20_000_000, Status: StatusCashedOut,
		CashedOutAtUnixMicro: testT0 + 5_000_000, CashOutTick: 500, CashOutMultiplierScaled: 13_498, PayoutCredits: 134}}
	b, err = c.EncodeState(s)
	if err != nil {
		t.Fatalf("EncodeState with a bet: %v", err)
	}
	if !strings.Contains(string(b), `"bets":[`+vecBet+`]`) {
		t.Errorf("EncodeState bets = %s\nwant bets [%s]", b, vecBet)
	}

	// The rejected result object of a record, built from RejectionCode.
	code, ok := c.RejectionCode(&RejectionError{Reason: ReasonDuplicateBet})
	if !ok {
		t.Fatal("RejectionCode(duplicate_bet) not ok")
	}
	b, err = canonjson.Marshal(canonjson.Object{"reason": canonjson.String(code), "status": canonjson.String("rejected")})
	if err != nil || string(b) != vecResult {
		t.Errorf("result = %s, %v; want %s", b, err, vecResult)
	}
}

func roundTripCommands() []Command {
	return []Command{
		PlaceBet{Authority: AuthorityPlayer, Player: "alice", BetCredits: 100},
		PlaceBet{Authority: AuthorityNone, Player: "", BetCredits: math.MinInt64},
		PlaceBet{Authority: AuthoritySystem, Player: p3, BetCredits: math.MaxInt64},
		PlaceBet{},
		StartRound{},
		StartRound{Authority: AuthoritySystem},
		CashOut{Authority: AuthorityPlayer, Player: p3},
		CashOut{},
		CloseRound{Authority: AuthorityPlayer},
		SettleRound{Authority: AuthoritySystem},
	}
}

func roundTripEvents() []Event {
	var seed [32]byte
	return []Event{
		BetPlaced{Player: "alice", BetCredits: 100, PlacedAtUnixMicro: math.MinInt64},
		BetPlaced{},
		RoundStarted{StartedAtUnixMicro: math.MaxInt64},
		RoundStarted{},
		BetCashedOut{Player: p3, CashedOutAtUnixMicro: -1, CashOutTick: math.MaxInt64, CashOutMultiplierScaled: math.MinInt64, PayoutCredits: 7},
		BetCashedOut{},
		RoundCrashed{ClosedAtUnixMicro: math.MaxInt64, CrashPointScaled: 1, CrashTick: 2, CrashDeadlineOffsetMicros: 3, CrashDeadlineUnixMicro: math.MinInt64},
		RoundCrashed{},
		BetLost{Player: "", BetCredits: -5},
		BetLost{Player: p3, BetCredits: math.MaxInt64},
		RoundSettled{SettledAtUnixMicro: 1, BetCount: 2, TotalBetCredits: 3, TotalPayoutCredits: 4, Seed: workedSeed()},
		RoundSettled{Seed: seed},
	}
}

// codecStates returns valid states in all four phases with bets in all
// three statuses.
func codecStates(t *testing.T) map[string]State {
	t.Helper()
	s := newRound(t, workedCodecConfig())
	betting, _ := step(t, s, placeBet("alice", 100), testT0-20_000_000)
	betting, _ = step(t, betting, placeBet(p3, 1_000_000_000), testT0-18_000_000)
	running, _ := step(t, betting, startRound(), testT0)
	cashed, _ := step(t, running, cashOut("alice"), testT0+5_000_000)
	crashed, _ := step(t, cashed, closeRound(), testT0+9_410_000)
	settled, _ := step(t, crashed, settleRound(), testT0+10_000_000)
	return map[string]State{
		"fresh": s, "betting": betting, "running": running, "cashed": cashed,
		"crashed": crashed, "settled": settled,
	}
}

func TestCodecRoundTrip(t *testing.T) {
	c := Codec{}
	for _, cmd := range roundTripCommands() {
		b, err := c.EncodeCommand(cmd)
		if err != nil {
			t.Fatalf("EncodeCommand(%#v): %v", cmd, err)
		}
		got, err := c.DecodeCommand(b)
		if err != nil || !reflect.DeepEqual(got, cmd) {
			t.Fatalf("DecodeCommand(%s) = %#v, %v; want %#v", b, got, err, cmd)
		}
		again, err := c.EncodeCommand(got)
		if err != nil || string(again) != string(b) {
			t.Fatalf("re-encoded command = %s, %v; want %s", again, err, b)
		}
	}
	for _, e := range roundTripEvents() {
		b, err := c.EncodeEvent(e)
		if err != nil {
			t.Fatalf("EncodeEvent(%#v): %v", e, err)
		}
		got, err := c.DecodeEvent(b)
		if err != nil || !reflect.DeepEqual(got, e) {
			t.Fatalf("DecodeEvent(%s) = %#v, %v; want %#v", b, got, err, e)
		}
		again, err := c.EncodeEvent(got)
		if err != nil || string(again) != string(b) {
			t.Fatalf("re-encoded event = %s, %v; want %s", again, err, b)
		}
	}

	var zeroSeed [32]byte
	for _, cfg := range []Config{
		workedCodecConfig(),
		{RoundID: workedRoundID, Seed: zeroSeed, MinBetCredits: math.MinInt64, MaxBetCredits: math.MaxInt64, MaxBets: 0},
		{},
	} {
		pub, sec, err := c.EncodeConfig(cfg)
		if err != nil {
			t.Fatalf("EncodeConfig(%+v): %v", cfg, err)
		}
		got, err := c.DecodeConfig(cfg.RoundID, pub, sec)
		if err != nil || got != cfg {
			t.Fatalf("DecodeConfig(%s, %s) = %+v, %v; want %+v", pub, sec, got, err, cfg)
		}
	}

	seen := map[string]string{}
	for name, s := range codecStates(t) {
		b, err := c.EncodeState(s)
		if err != nil {
			t.Fatalf("EncodeState(%s): %v", name, err)
		}
		o, err := canonjson.Unmarshal(b)
		if err != nil {
			t.Fatalf("EncodeState(%s) is not OCJ: %v", name, err)
		}
		again, err := canonjson.Marshal(o)
		if err != nil || string(again) != string(b) {
			t.Fatalf("EncodeState(%s) does not round-trip", name)
		}
		if other, dup := seen[string(b)]; dup {
			t.Fatalf("states %s and %s encode the same", name, other)
		}
		seen[string(b)] = name
		if b2, _ := c.EncodeState(s); string(b2) != string(b) {
			t.Fatalf("EncodeState(%s) is not deterministic", name)
		}
	}

	// The settled state holds bets in all three statuses.
	settled := codecStates(t)["settled"]
	statuses := map[BetStatus]bool{}
	for _, b := range settled.Bets {
		statuses[b.Status] = true
	}
	open := codecStates(t)["betting"]
	statuses[open.Bets[0].Status] = true
	if !statuses[StatusOpen] || !statuses[StatusCashedOut] || !statuses[StatusLost] {
		t.Fatalf("test states lack a bet status: %v", statuses)
	}
}

func TestCodecRejectsMalformed(t *testing.T) {
	c := Codec{}
	commands := []string{
		`{"authority":"player","player":"alice","type":"PlaceBet"}`,                             // missing betCredits
		`{"authority":"player","betCredits":"1","note":"x","player":"alice","type":"PlaceBet"}`, // unknown key
		`{"authority":"system"}`,                                                        // missing type
		`{"authority":"system","type":"StopRound"}`,                                     // unknown type
		`{"authority":"system","type":["x"]}`,                                           // type not a string
		`{"authority":"admin","type":"StartRound"}`,                                     // unknown authority
		`{"authority":["x"],"type":"StartRound"}`,                                       // authority not a string
		`{"authority":"system","type":"startRound"}`,                                    // case matters
		`{"authority":"player","betCredits":"0100","player":"alice","type":"PlaceBet"}`, // non-canonical int
		`{"authority":"player","betCredits":"-0","player":"alice","type":"PlaceBet"}`,
		`{"authority":"player","betCredits":"9223372036854775808","player":"alice","type":"PlaceBet"}`,
		`{"authority":"player","betCredits":{},"player":"alice","type":"PlaceBet"}`,
		`{"authority":"player","player":[],"type":"CashOut"}`,
		`{"authority":"system", "type":"StartRound"}`, // whitespace
		`{"type":"StartRound","authority":"system"}`,  // unsorted
		``,
		`[]`,
	}
	for _, in := range commands {
		if got, err := c.DecodeCommand([]byte(in)); !errors.Is(err, game.ErrMalformed) || got != nil {
			t.Errorf("DecodeCommand(%s) = %#v, %v; want ErrMalformed", in, got, err)
		}
	}

	events := []string{
		`{"startedAtUnixMicro":"1","type":"RoundBegun"}`,
		`{"type":"RoundStarted"}`,
		`{"startedAtUnixMicro":"1","startedAtUnixMicroX":"1","type":"RoundStarted"}`,
		`{"startedAtUnixMicro":"+1","type":"RoundStarted"}`,
		`{"betCredits":"250","player":"bob","type":"BetLost","x":"y"}`,
		`{"betCount":"3","seed":"000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F","settledAtUnixMicro":"1","totalBetCredits":"1","totalPayoutCredits":"1","type":"RoundSettled"}`, // uppercase hex
		`{"betCount":"3","seed":"0001","settledAtUnixMicro":"1","totalBetCredits":"1","totalPayoutCredits":"1","type":"RoundSettled"}`,                                                             // short hex
		`{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"} `,
	}
	for _, in := range events {
		if got, err := c.DecodeEvent([]byte(in)); !errors.Is(err, game.ErrMalformed) || got != nil {
			t.Errorf("DecodeEvent(%s) = %#v, %v; want ErrMalformed", in, got, err)
		}
	}

	configs := []struct{ pub, sec string }{
		{`{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","maxBetCredits":"1000000000","maxBets":"3"}`, vecConfigSecret},
		{vecConfig, `{"seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f","x":"y"}`},
		{vecConfig, `{}`},
		{strings.Replace(vecConfig, `"maxBets":"3"`, `"maxBets":"03"`, 1), vecConfigSecret},
		{vecConfig, `{"seed":"000102"}`},
		{vecConfig, ` ` + vecConfigSecret},
		{`{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1","phase":"betting"}`, vecConfigSecret},
	}
	for _, tc := range configs {
		if _, err := c.DecodeConfig(workedRoundID, []byte(tc.pub), []byte(tc.sec)); !errors.Is(err, game.ErrMalformed) {
			t.Errorf("DecodeConfig(%s, %s) = %v; want ErrMalformed", tc.pub, tc.sec, err)
		}
	}
	// Uppercase commitment is a syntax failure before any commitment check.
	upper := strings.Replace(vecConfig, "630dcd29", "630DCD29", 1)
	if _, err := c.DecodeConfig(workedRoundID, []byte(upper), []byte(vecConfigSecret)); !errors.Is(err, game.ErrMalformed) || errors.Is(err, game.ErrRevealMismatch) {
		t.Errorf("DecodeConfig(uppercase commitment) = %v; want ErrMalformed only", err)
	}

	// Missing and unknown keys per type (spec section 10 step 3), derived
	// from the valid section 5.6 vectors: every required non-type key is
	// removed in turn, and an unknown key is added at each end of the
	// sorted key order.
	for _, vec := range []string{vecPlaceBet, vecStartRound, vecCashOut, vecCloseRound, vecSettleRound} {
		if _, err := c.DecodeCommand([]byte(vec)); err != nil {
			t.Fatalf("DecodeCommand(%s) valid vector: %v", vec, err)
		}
		for _, in := range keyVariants(t, vec) {
			if got, err := c.DecodeCommand([]byte(in)); !errors.Is(err, game.ErrMalformed) || got != nil {
				t.Errorf("DecodeCommand(%s) = %#v, %v; want ErrMalformed", in, got, err)
			}
		}
	}
	for _, vec := range []string{vecBetPlaced, vecRoundStarted, vecBetCashedOut, vecRoundCrashed, vecBetLost, vecRoundSettled} {
		if _, err := c.DecodeEvent([]byte(vec)); err != nil {
			t.Fatalf("DecodeEvent(%s) valid vector: %v", vec, err)
		}
		for _, in := range keyVariants(t, vec) {
			if got, err := c.DecodeEvent([]byte(in)); !errors.Is(err, game.ErrMalformed) || got != nil {
				t.Errorf("DecodeEvent(%s) = %#v, %v; want ErrMalformed", in, got, err)
			}
		}
	}
}

// keyVariants returns canonical encodings of the payload vec with each
// non-type key removed in turn, then with an unknown key added first and
// last in key order.
func keyVariants(t *testing.T, vec string) []string {
	t.Helper()
	o, err := canonjson.Unmarshal([]byte(vec))
	if err != nil {
		t.Fatalf("Unmarshal(%s): %v", vec, err)
	}
	var out []string
	add := func(m canonjson.Object) {
		b, err := canonjson.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", m, err)
		}
		out = append(out, string(b))
	}
	clone := func() canonjson.Object {
		m := make(canonjson.Object, len(o)+1)
		for k, v := range o {
			m[k] = v
		}
		return m
	}
	removed := 0
	for k := range o {
		if k == "type" {
			continue
		}
		m := clone()
		delete(m, k)
		add(m)
		removed++
	}
	if removed == 0 {
		t.Fatalf("vector %s has no non-type key to remove", vec)
	}
	for _, k := range []string{"aaa", "zzz"} {
		m := clone()
		m[k] = canonjson.String("x")
		add(m)
	}
	return out
}

func TestCodecRejectsUnencodable(t *testing.T) {
	c := Codec{}
	commands := []Command{
		PlaceBet{Authority: 3, Player: "alice", BetCredits: 1},
		StartRound{Authority: 255},
		PlaceBet{Authority: AuthorityPlayer, Player: "\xff", BetCredits: 1},
		CashOut{Authority: AuthorityPlayer, Player: "a\xc0\xafb"},
		nil,
		&PlaceBet{},
		&StartRound{Authority: AuthoritySystem},
	}
	for _, cmd := range commands {
		if b, err := c.EncodeCommand(cmd); !errors.Is(err, game.ErrUnencodable) || b != nil {
			t.Errorf("EncodeCommand(%#v) = %s, %v; want ErrUnencodable", cmd, b, err)
		}
	}
	events := []Event{nil, &RoundStarted{}, &BetLost{}, BetLost{Player: "\xed\xa0\x80"}}
	for _, e := range events {
		if b, err := c.EncodeEvent(e); !errors.Is(err, game.ErrUnencodable) || b != nil {
			t.Errorf("EncodeEvent(%#v) = %s, %v; want ErrUnencodable", e, b, err)
		}
	}

	base := newRound(t, workedCodecConfig())
	bad := []State{}
	for _, p := range []Phase{PhaseInvalid, 5, 255} {
		s := base
		s.Phase = p
		bad = append(bad, s)
	}
	for _, st := range []BetStatus{StatusInvalid, 4} {
		s := base
		s.Bets = []Bet{{Player: "alice", BetCredits: 1, Status: st}}
		bad = append(bad, s)
	}
	s := base
	s.Bets = []Bet{{Player: "\xff", Status: StatusOpen}}
	bad = append(bad, s)
	for _, s := range bad {
		if b, err := c.EncodeState(s); !errors.Is(err, game.ErrUnencodable) || b != nil {
			t.Errorf("EncodeState(phase %d, bets %+v) = %s, %v; want ErrUnencodable", s.Phase, s.Bets, b, err)
		}
	}
	if _, _, err := c.EncodeConfig(Config{}); err != nil {
		t.Errorf("EncodeConfig(zero) = %v; every config is encodable", err)
	}
}

func TestDecodeConfigRevealMismatch(t *testing.T) {
	c := Codec{}
	cfg := workedCodecConfig()
	pub, _, err := c.EncodeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	other := cfg
	other.Seed[31] ^= 1
	_, sec, err := c.EncodeConfig(other)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.DecodeConfig(cfg.RoundID, pub, sec)
	if !errors.Is(err, game.ErrRevealMismatch) || got != (Config{}) {
		t.Fatalf("DecodeConfig(mismatched seed) = %+v, %v; want ErrRevealMismatch", got, err)
	}
	if errors.Is(err, game.ErrMalformed) {
		t.Errorf("reveal mismatch also matches ErrMalformed: %v", err)
	}
	for _, seed := range [][32]byte{cfg.Seed, other.Seed} {
		if h := hex.EncodeToString(seed[:]); strings.Contains(err.Error(), h) || strings.Contains(err.Error(), h[:16]) {
			t.Errorf("reveal mismatch error formats a seed: %v", err)
		}
	}
	// The matching secret decodes.
	_, sec, _ = c.EncodeConfig(cfg)
	if got, err := c.DecodeConfig(cfg.RoundID, pub, sec); err != nil || got != cfg {
		t.Errorf("DecodeConfig(matching) = %+v, %v; want %+v", got, err, cfg)
	}
}

// mutateField changes v, a settable field, to a different valid value.
func mutateField(t *testing.T, name string, v reflect.Value) {
	t.Helper()
	switch v.Interface().(type) {
	case Phase:
		v.Set(reflect.ValueOf(PhaseSettled))
	case BetStatus:
		v.Set(reflect.ValueOf(StatusLost))
	case int64:
		v.SetInt(v.Int() + 1)
	case PlayerID:
		v.SetString(v.String() + "x")
	case [16]byte, [32]byte:
		v.Index(0).SetUint(v.Index(0).Uint() ^ 0xff)
	case []Bet:
		v.Set(reflect.Append(v, reflect.ValueOf(Bet{Player: "zed", Status: StatusOpen})))
	default:
		t.Fatalf("field %s has type %s; teach mutateField and EncodeState about it", name, v.Type())
	}
}

func TestEncodeStateCoversEveryField(t *testing.T) {
	if n := reflect.TypeOf(State{}).NumField(); n != 17 {
		t.Fatalf("State has %d fields; want 17 (a new field needs a codec change and a stateVersion bump)", n)
	}
	if n := reflect.TypeOf(Bet{}).NumField(); n != 8 {
		t.Fatalf("Bet has %d fields; want 8 (a new field needs a codec change and a stateVersion bump)", n)
	}
	c := Codec{}
	base := codecStates(t)["cashed"]
	base.ClosedAtUnixMicro = 11
	base.SettledAtUnixMicro = 12
	baseBytes, err := c.EncodeState(base)
	if err != nil {
		t.Fatal(err)
	}

	st := reflect.TypeOf(State{})
	for i := range st.NumField() {
		s := base
		s.Bets = append([]Bet(nil), base.Bets...)
		mutateField(t, st.Field(i).Name, reflect.ValueOf(&s).Elem().Field(i))
		b, err := c.EncodeState(s)
		if err != nil {
			t.Fatalf("State.%s changed: EncodeState: %v", st.Field(i).Name, err)
		}
		if string(b) == string(baseBytes) {
			t.Errorf("State.%s is not covered by EncodeState", st.Field(i).Name)
		}
	}

	bt := reflect.TypeOf(Bet{})
	for i := range bt.NumField() {
		s := base
		s.Bets = append([]Bet(nil), base.Bets...)
		if bt.Field(i).Name == "Status" {
			s.Bets[0].Status = StatusOpen // base bet 0 is cashed out
		} else {
			mutateField(t, bt.Field(i).Name, reflect.ValueOf(&s.Bets[0]).Elem().Field(i))
		}
		b, err := c.EncodeState(s)
		if err != nil {
			t.Fatalf("Bet.%s changed: EncodeState: %v", bt.Field(i).Name, err)
		}
		if string(b) == string(baseBytes) {
			t.Errorf("Bet.%s is not covered by EncodeState", bt.Field(i).Name)
		}
	}
}

func TestRejectionCode(t *testing.T) {
	c := Codec{}
	reasons := []Reason{
		ReasonInvalidCommand, ReasonUnauthorized, ReasonTimeOutOfRange, ReasonWrongPhase,
		ReasonRoundSettled, ReasonInvalidAmount, ReasonDuplicateBet, ReasonBetLimitReached,
		ReasonNoBet, ReasonAlreadyCashedOut, ReasonNotAfterStart, ReasonDeadlinePassed,
		ReasonCrashNotDue,
	}
	want := []string{
		"invalid_command", "unauthorized", "time_out_of_range", "wrong_phase",
		"round_settled", "invalid_amount", "duplicate_bet", "bet_limit_reached",
		"no_bet", "already_cashed_out", "not_after_start", "deadline_passed",
		"crash_not_due",
	}
	for i, r := range reasons {
		for _, err := range []error{&RejectionError{Reason: r}, fmt.Errorf("wrapped: %w", &RejectionError{Reason: r})} {
			if code, ok := c.RejectionCode(err); !ok || code != want[i] {
				t.Errorf("RejectionCode(%v) = %q, %v; want %q", err, code, ok, want[i])
			}
		}
	}
	for _, err := range []error{ErrInvalidState, ErrArithmetic, fmt.Errorf("x: %w", ErrInvalidState), errors.New("x"), ErrRejected, nil} {
		if code, ok := c.RejectionCode(err); ok || code != "" {
			t.Errorf("RejectionCode(%v) = %q, %v; want fail closed", err, code, ok)
		}
	}
}

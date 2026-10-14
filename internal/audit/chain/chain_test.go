package chain_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
)

// Literals of spec section 5.6, typed in from the spec.
const (
	genesisLine = `{"chain":"orvex/audit/v1","config":{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1"},"createdAtUnixMicro":"1759999970000000","engineBuild":"golden","game":"crash","kind":"genesis","prevHash":"0000000000000000000000000000000000000000000000000000000000000000","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001","rulesVersion":"1","schemaVersion":"1","sequence":"0","stateVersion":"1"}`
	hash0       = "cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284"
	record1Line = `{"command":{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"},"commandId":"0190f3a8-7b3c-7d4e-8f01-000000000001","effectiveTimeUnixMicro":"1759999980000000","engineBuild":"golden","events":[{"event":{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"},"eventSequence":"1"}],"kind":"command","prevHash":"cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284","result":{"status":"accepted"},"roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001","sequence":"1"}`
	hash1       = "6cbb649e00d7ac798be564e2a2f90910ffd69caa9f95207e9ee2f9e72afcf061"
	stateHash0  = "1c98ea460213ad0d5900a92fbf37cc3078fa9e822d2065a65037b667734de7e7"

	configLine    = `{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1"}`
	placeBetLine  = `{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"}`
	betPlacedLine = `{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"}`
	t0            = int64(1_760_000_000_000_000)
)

var (
	roundID   = [16]byte{0x01, 0x90, 0xf3, 0xa8, 0x7b, 0x3c, 0x7d, 0x4e, 0x8f, 0x00, 0, 0, 0, 0, 0, 0x01}
	commandID = [16]byte{0x01, 0x90, 0xf3, 0xa8, 0x7b, 0x3c, 0x7d, 0x4e, 0x8f, 0x01, 0, 0, 0, 0, 0, 0x01}
)

func workedSeed() [32]byte {
	var s [32]byte
	for i := range s {
		s[i] = byte(i)
	}
	return s
}

func workedGenesis() chain.Genesis {
	return chain.Genesis{
		RoundID:            roundID,
		Game:               "crash",
		RulesVersion:       1,
		SchemaVersion:      1,
		StateVersion:       1,
		Config:             []byte(configLine),
		CreatedAtUnixMicro: t0 - 30_000_000,
		EngineBuild:        "golden",
	}
}

func workedRecord1(prev chain.Hash) chain.Command {
	return chain.Command{
		RoundID:                roundID,
		Sequence:               1,
		PrevHash:               prev,
		CommandID:              commandID,
		EffectiveTimeUnixMicro: t0 - 20_000_000,
		EngineBuild:            "golden",
		Payload:                []byte(placeBetLine),
		Events:                 []chain.Event{{Sequence: 1, Payload: []byte(betPlacedLine)}},
	}
}

func TestWorkedExampleGenesis(t *testing.T) {
	b, err := chain.EncodeGenesis(workedGenesis())
	if err != nil || string(b) != genesisLine {
		t.Fatalf("EncodeGenesis = %s, %v\nwant %s", b, err, genesisLine)
	}
	if h := chain.LinkHash(b); h.String() != hash0 {
		t.Errorf("LinkHash(genesis) = %s; want %s", h, hash0)
	}
	if h := chain.LinkHash([]byte(genesisLine)); h.String() != hash0 {
		t.Errorf("LinkHash(literal genesis) = %s; want %s", h, hash0)
	}
}

func TestWorkedExampleRecord1(t *testing.T) {
	prev := chain.LinkHash([]byte(genesisLine))
	b, err := chain.EncodeCommand(workedRecord1(prev))
	if err != nil || string(b) != record1Line {
		t.Fatalf("EncodeCommand = %s, %v\nwant %s", b, err, record1Line)
	}
	if h := chain.LinkHash([]byte(record1Line)); h.String() != hash1 {
		t.Errorf("LinkHash(record 1) = %s; want %s", h, hash1)
	}
	c := workedRecord1(prev)
	if in := c.Input(); in.Sequence != 1 || in.EffectiveTimeUnixMicro != t0-20_000_000 {
		t.Errorf("Input() = %+v", in)
	}
}

func TestWorkedExampleInitialStateHash(t *testing.T) {
	cfg := v1.Config{RoundID: roundID, Seed: workedSeed(), MinBetCredits: 1, MaxBetCredits: 1_000_000_000, MaxBets: 3}
	s, err := v1.Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := v1.Codec{}.EncodeState(s)
	if err != nil {
		t.Fatal(err)
	}
	h, err := chain.StateHash(v1.GameID, v1.StateVersion, b)
	if err != nil || h.String() != stateHash0 {
		t.Fatalf("StateHash = %s, %v; want %s", h, err, stateHash0)
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	got, err := chain.Decode([]byte(genesisLine))
	if err != nil {
		t.Fatalf("Decode(genesis): %v", err)
	}
	if want := workedGenesis(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Decode(genesis) = %#v\nwant %#v", got, want)
	}
	again, err := chain.EncodeGenesis(got.(chain.Genesis))
	if err != nil || string(again) != genesisLine {
		t.Fatalf("re-encoded genesis = %s, %v", again, err)
	}

	got, err = chain.Decode([]byte(record1Line))
	if err != nil {
		t.Fatalf("Decode(record 1): %v", err)
	}
	if want := workedRecord1(chain.LinkHash([]byte(genesisLine))); !reflect.DeepEqual(got, want) {
		t.Fatalf("Decode(record 1) = %#v\nwant %#v", got, want)
	}

	rejected := chain.Command{
		RoundID: roundID, Sequence: 4, CommandID: commandID, EffectiveTimeUnixMicro: -1,
		EngineBuild: strings.Repeat("~", 128), Payload: []byte(placeBetLine),
		Result: chain.Result{Rejected: true, Reason: "duplicate_bet"},
		Events: []chain.Event{},
	}
	b, err := chain.EncodeCommand(rejected)
	if err != nil {
		t.Fatalf("EncodeCommand(rejected): %v", err)
	}
	if !strings.Contains(string(b), `"events":[],`) || !strings.Contains(string(b), `"result":{"reason":"duplicate_bet","status":"rejected"}`) {
		t.Fatalf("rejected record = %s", b)
	}
	got, err = chain.Decode(b)
	if err != nil || !reflect.DeepEqual(got, rejected) {
		t.Fatalf("Decode(rejected) = %#v, %v\nwant %#v", got, err, rejected)
	}
}

// replace returns line with the first occurrence of old replaced by new,
// failing if old is absent.
func replace(t *testing.T, line, old, new string) string {
	t.Helper()
	if !strings.Contains(line, old) {
		t.Fatalf("%q not in %s", old, line)
	}
	return strings.Replace(line, old, new, 1)
}

func TestDecodeRejects(t *testing.T) {
	g, r := genesisLine, record1Line
	cases := map[string]string{
		"genesis missing key":        replace(t, g, `"createdAtUnixMicro":"1759999970000000",`, ``),
		"genesis unknown key":        replace(t, g, `"kind":"genesis",`, `"kind":"genesis","note":"x",`),
		"command missing key":        replace(t, r, `"engineBuild":"golden",`, ``),
		"command unknown key":        replace(t, r, `"kind":"command",`, `"kind":"command","note":"x",`),
		"kind admission":             replace(t, r, `"kind":"command"`, `"kind":"admission"`),
		"kind missing":               replace(t, r, `"kind":"command",`, ``),
		"kind not a string":          replace(t, r, `"kind":"command"`, `"kind":[]`),
		"sequence leading zero":      replace(t, r, `"sequence":"1"`, `"sequence":"01"`),
		"genesis sequence negative":  replace(t, g, `"sequence":"0"`, `"sequence":"-1"`),
		"uppercase prevHash":         replace(t, r, `"prevHash":"cfdd`, `"prevHash":"CFDD`),
		"short prevHash":             replace(t, r, `"prevHash":"cfdd`, `"prevHash":"cfd`),
		"bad roundId":                replace(t, r, `"roundId":"0190f3a8-7b3c`, `"roundId":"0190f3a87-b3c`),
		"uppercase commandId":        replace(t, r, `"commandId":"0190f3a8`, `"commandId":"0190F3A8`),
		"engineBuild empty":          replace(t, g, `"engineBuild":"golden"`, `"engineBuild":""`),
		"engineBuild space":          replace(t, g, `"engineBuild":"golden"`, `"engineBuild":"gol den"`),
		"engineBuild 129 bytes":      replace(t, r, `"engineBuild":"golden"`, `"engineBuild":"`+strings.Repeat("a", 129)+`"`),
		"engineBuild non-ASCII":      replace(t, r, `"engineBuild":"golden"`, `"engineBuild":"goldén"`),
		"game Crash":                 replace(t, g, `"game":"crash"`, `"game":"Crash"`),
		"game too long":              replace(t, g, `"game":"crash"`, `"game":"`+strings.Repeat("a", 33)+`"`),
		"rulesVersion 0":             replace(t, g, `"rulesVersion":"1"`, `"rulesVersion":"0"`),
		"stateVersion 2^32":          replace(t, g, `"stateVersion":"1"`, `"stateVersion":"4294967296"`),
		"accepted with reason":       replace(t, r, `"result":{"status":"accepted"}`, `"result":{"reason":"no_bet","status":"accepted"}`),
		"rejected without reason":    replace(t, r, `"result":{"status":"accepted"}`, `"result":{"status":"rejected"}`),
		"reason Bad":                 replace(t, r, `"result":{"status":"accepted"}`, `"result":{"reason":"Bad","status":"rejected"}`),
		"status unknown":             replace(t, r, `"result":{"status":"accepted"}`, `"result":{"status":"pending"}`),
		"eventSequence 0":            replace(t, r, `"eventSequence":"1"`, `"eventSequence":"0"`),
		"event item unknown key":     replace(t, r, `"eventSequence":"1"}`, `"eventSequence":"1","x":"y"}`),
		"event not an object":        replace(t, r, `"event":{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"}`, `"event":"x"`),
		"events not an array":        replace(t, r, `"events":[{"event":{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"},"eventSequence":"1"}]`, `"events":{}`),
		"command not an object":      replace(t, r, `"command":{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"}`, `"command":"x"`),
		"config not an object":       replace(t, g, `"config":{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1"}`, `"config":[]`),
		"effective time leading +":   replace(t, r, `"effectiveTimeUnixMicro":"1759999980000000"`, `"effectiveTimeUnixMicro":"+1759999980000000"`),
		"non-canonical whitespace":   replace(t, r, `"kind":"command"`, `"kind": "command"`),
		"trailing newline":           r + "\n",
		"genesis with no chain":      replace(t, g, `"chain":"orvex/audit/v1",`, ``),
		"command with current chain": replace(t, r, `"command":{`, `"chain":"orvex/audit/v1","command":{`),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := chain.Decode([]byte(line)); err == nil {
				t.Fatalf("Decode accepted %s as %#v", line, got)
			} else if errors.Is(err, chain.ErrUnsupportedChain) {
				t.Fatalf("Decode = %v; want a syntax error, not ErrUnsupportedChain", err)
			}
		})
	}
}

func TestEncodeRejectsUndecodable(t *testing.T) {
	bad := []chain.Genesis{}
	for _, f := range []func(*chain.Genesis){
		func(g *chain.Genesis) { g.Game = "Crash" },
		func(g *chain.Genesis) { g.Game = "" },
		func(g *chain.Genesis) { g.RulesVersion = 0 },
		func(g *chain.Genesis) { g.StateVersion = 0 },
		func(g *chain.Genesis) { g.EngineBuild = "" },
		func(g *chain.Genesis) { g.EngineBuild = "a b" },
		func(g *chain.Genesis) { g.Config = []byte(`{ }`) },
		func(g *chain.Genesis) { g.Config = nil },
	} {
		g := workedGenesis()
		f(&g)
		bad = append(bad, g)
	}
	for _, g := range bad {
		if b, err := chain.EncodeGenesis(g); err == nil {
			t.Errorf("EncodeGenesis(%+v) = %s; want an error", g, b)
		}
	}
	var prev chain.Hash
	for _, f := range []func(*chain.Command){
		func(c *chain.Command) { c.EngineBuild = strings.Repeat("a", 129) },
		func(c *chain.Command) { c.Result = chain.Result{Reason: "no_bet"} },
		func(c *chain.Command) { c.Result = chain.Result{Rejected: true} },
		func(c *chain.Command) { c.Result = chain.Result{Rejected: true, Reason: "No_bet"} },
		func(c *chain.Command) { c.Payload = []byte(`[]`) },
		func(c *chain.Command) { c.Events[0].Sequence = 0 },
		func(c *chain.Command) { c.Events[0].Payload = []byte(`{"b":"1","a":"2"}`) },
	} {
		c := workedRecord1(prev)
		f(&c)
		if b, err := chain.EncodeCommand(c); err == nil {
			t.Errorf("EncodeCommand(%+v) = %s; want an error", c, b)
		}
	}
}

func TestDecodeUnsupportedChain(t *testing.T) {
	for _, v := range []string{`"orvex/audit/v2"`, `""`, `"orvex/audit/V1"`, `[]`, `{}`} {
		line := replace(t, genesisLine, `"chain":"orvex/audit/v1"`, `"chain":`+v)
		if _, err := chain.Decode([]byte(line)); !errors.Is(err, chain.ErrUnsupportedChain) {
			t.Errorf("Decode(chain %s) = %v; want ErrUnsupportedChain", v, err)
		}
	}
	// The chain check comes before the kind and key-set checks.
	line := replace(t, genesisLine, `"chain":"orvex/audit/v1"`, `"chain":"orvex/audit/v2"`)
	line = replace(t, line, `"kind":"genesis",`, `"kind":"future",`)
	line = replace(t, line, `"stateVersion":"1"}`, `"stateVersion":"1","zfuture":"x"}`)
	if _, err := chain.Decode([]byte(line)); !errors.Is(err, chain.ErrUnsupportedChain) {
		t.Errorf("Decode(v2 with new keys) = %v; want ErrUnsupportedChain", err)
	}
}

func TestStateHashBindsVersion(t *testing.T) {
	state := []byte(`{"a":"1"}`)
	h := func(g game.ID, v game.StateVersion) chain.Hash {
		t.Helper()
		out, err := chain.StateHash(g, v, state)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	base := h("crash", 1)
	if base == h("crash", 2) {
		t.Error("state version 1 and 2 hash the same")
	}
	if base == h("dice", 1) {
		t.Error("games crash and dice hash the same")
	}
	if base != h("crash", 1) {
		t.Error("StateHash is not deterministic")
	}
	other, err := chain.StateHash(v1.GameID, 1, []byte(`{"a":"2"}`))
	if err != nil || other == base {
		t.Errorf("different states hash the same (%v)", err)
	}
	for _, bad := range []string{``, `{ }`, `[]`, `{"a":1}`} {
		if _, err := chain.StateHash(v1.GameID, 1, []byte(bad)); !errors.Is(err, canonjson.ErrNotCanonical) {
			t.Errorf("StateHash(%q) = %v; want ErrNotCanonical", bad, err)
		}
	}
}

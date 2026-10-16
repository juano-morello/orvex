package execution

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
)

const goldenDir = "../../audit/verification/testdata/golden"

// golden is a decoded golden history.
type golden struct {
	header  canonjson.Object
	lines   [][]byte
	genesis chain.Genesis
	config  v1.Config
	cmds    []chain.Command
}

func loadGolden(t *testing.T, name string) golden {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name+".jsonl")) //nolint:gosec // G304: fixed golden fixture under testdata
	if err != nil {
		t.Fatal(err)
	}
	all := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	g := golden{lines: all[1:]}
	if g.header, err = canonjson.Unmarshal(all[0]); err != nil {
		t.Fatal(err)
	}
	rec, err := chain.Decode(g.lines[0])
	if err != nil {
		t.Fatal(err)
	}
	g.genesis = rec.(chain.Genesis)
	so, err := canonjson.Obj(g.header["configSecret"])
	if err != nil {
		t.Fatal(err)
	}
	secret, err := canonjson.Marshal(so)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := v1.Adapter().DecodeConfig(g.genesis.RoundID, g.genesis.Config, secret)
	if err != nil {
		t.Fatal(err)
	}
	g.config = cfg.(v1.Config)
	for _, line := range g.lines[1:] {
		rec, err := chain.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		g.cmds = append(g.cmds, rec.(chain.Command))
	}
	return g
}

func TestCrashV1PolicyDescriptor(t *testing.T) {
	p := CrashV1Policy()
	if p.Descriptor() != v1.Descriptor() || p.Ledger().Descriptor() != v1.Descriptor() {
		t.Errorf("descriptors = %+v / %+v, want %+v", p.Descriptor(), p.Ledger().Descriptor(), v1.Descriptor())
	}
}

func TestCrashV1Checkpoint(t *testing.T) {
	p := CrashV1Policy()
	for _, c := range []struct {
		events []any
		want   bool
	}{
		{nil, false},
		{[]any{}, false},
		{[]any{v1.BetPlaced{Player: "a", BetCredits: 1}}, false},
		{[]any{v1.BetCashedOut{Player: "a", PayoutCredits: 1}}, false},
		{[]any{v1.BetLost{Player: "a", BetCredits: 1}}, false},
		{[]any{v1.RoundStarted{}}, true},
		{[]any{v1.RoundCrashed{}}, true},
		{[]any{v1.BetLost{Player: "a", BetCredits: 1}, v1.RoundSettled{}}, true},
	} {
		if got := p.Checkpoint(c.events); got != c.want {
			t.Errorf("Checkpoint(%+v) = %v, want %v", c.events, got, c.want)
		}
	}
}

func TestCrashV1Final(t *testing.T) {
	p := CrashV1Policy()
	for _, c := range []struct {
		phase v1.Phase
		want  bool
	}{{v1.PhaseBetting, false}, {v1.PhaseRunning, false}, {v1.PhaseCrashed, false}, {v1.PhaseSettled, true}} {
		got, err := p.Final(v1.State{Phase: c.phase})
		if err != nil || got != c.want {
			t.Errorf("Final(%s) = %v, %v; want %v", c.phase, got, err, c.want)
		}
	}
	if _, err := p.Final(struct{}{}); err == nil {
		t.Error("Final of a foreign state type succeeded")
	}
}

func TestCrashV1CloseOffset(t *testing.T) {
	p := CrashV1Policy()
	worked := v1.Config{MinBetCredits: 1, MaxBetCredits: 1_000_000_000, MaxBets: 10_000}
	worked.RoundID = loadGolden(t, "worked-example").genesis.RoundID
	if canonjson.UUID(worked.RoundID) != "0190f3a8-7b3c-7d4e-8f00-000000000001" {
		t.Fatalf("worked round = %s", canonjson.UUID(worked.RoundID))
	}
	for i := range worked.Seed {
		worked.Seed[i] = byte(i)
	}
	configs := map[string]v1.Config{
		"worked-example literal": worked,
		"worked-example":         loadGolden(t, "worked-example").config,
		"instant-crash":          loadGolden(t, "instant-crash").config,
		"empty-round":            loadGolden(t, "empty-round").config,
	}
	want := map[string]int64{"worked-example literal": 9_410_000, "worked-example": 9_410_000, "instant-crash": 0, "empty-round": 40_010_000}
	for name, cfg := range configs {
		s, err := v1.Rules{}.InitialState(cfg)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.CloseOffsetMicros(s)
		if err != nil || got != want[name] {
			t.Errorf("%s: CloseOffsetMicros = %d, %v; want %d", name, got, err, want[name])
		}
	}
	for _, bad := range []any{struct{}{}, v1.State{CrashDeadlineOffsetMicros: -1}} {
		_, err := p.CloseOffsetMicros(bad)
		if err == nil {
			t.Errorf("CloseOffsetMicros(%T) succeeded", bad)
			continue
		}
		if strings.ContainsAny(err.Error(), "0123456789") {
			t.Errorf("error %q contains digits", err)
		}
	}
}

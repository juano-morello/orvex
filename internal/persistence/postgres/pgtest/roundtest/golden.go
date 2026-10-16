//go:build integration

package roundtest

import (
	"bytes"
	"context"
	"os"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	crashv1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// Golden is an LBT-9 golden history whose round Executor created and whose
// admissions are seeded, none executed.
type Golden struct {
	RoundID [16]byte
	Header  canonjson.Object
	// Lines are the record lines 0..N, without the header.
	Lines [][]byte
	// Commands are the command records 1..N.
	Commands []chain.Command
	// Executor is the executor that created the round.
	Executor *execution.Executor
}

// Lease returns the round-creation lease (fencing token 0).
func (g Golden) Lease() execution.Lease { return execution.Lease{RoundID: g.RoundID} }

// Hash returns the link hash of golden record seq.
func (g Golden) Hash(seq int) chain.Hash { return chain.LinkHash(g.Lines[seq]) }

// UUID returns the round ID as D-11 text.
func (g Golden) UUID() string { return string(canonjson.UUID(g.RoundID)) }

// ReadGolden reads the golden history at path and returns its header and
// its record lines 0..N.
func ReadGolden(t testing.TB, path string) (canonjson.Object, [][]byte) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test support; callers pass a golden fixture under testdata
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	header, err := canonjson.Unmarshal(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	return header, lines[1:]
}

// GoldenConfig returns the golden round's genesis and typed config.
func GoldenConfig(t testing.TB, header canonjson.Object, genesisLine []byte) (chain.Genesis, crashv1.Config) {
	t.Helper()
	rec, err := chain.Decode(genesisLine)
	if err != nil {
		t.Fatal(err)
	}
	g := rec.(chain.Genesis)
	so, err := canonjson.Obj(header["configSecret"])
	if err != nil {
		t.Fatal(err)
	}
	secret, err := canonjson.Marshal(so)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := crashv1.Adapter().DecodeConfig(g.RoundID, g.Config, secret)
	if err != nil {
		t.Fatal(err)
	}
	return g, cfg.(crashv1.Config)
}

// SeedGolden creates the golden round at path with x, funds each player with
// the sum of their holds, and seeds every admission from the golden records,
// up to admission upTo (0: all).
func SeedGolden(t testing.TB, db *pgxpool.Pool, x *execution.Executor, path string, upTo int) Golden {
	t.Helper()
	header, lines := ReadGolden(t, path)
	g, cfg := GoldenConfig(t, header, lines[0])
	created, err := x.CreateRound(context.Background(), execution.NewRound{
		RoundID: g.RoundID, Game: g.Game, RulesVersion: g.RulesVersion, Config: cfg, CreatedAtUnixMicro: g.CreatedAtUnixMicro,
	})
	if err != nil {
		t.Fatalf("CreateRound(%s): %v", path, err)
	}
	if !bytes.Equal(created.Genesis, lines[0]) {
		t.Fatalf("%s genesis differs:\n got %s\nwant %s", path, created.Genesis, lines[0])
	}
	gr := Golden{RoundID: g.RoundID, Header: header, Lines: lines, Executor: x}
	funds := map[string]int64{}
	for _, line := range lines[1:] {
		rec, err := chain.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		c := rec.(chain.Command)
		gr.Commands = append(gr.Commands, c)
		if h := goldenHold(t, c); h != nil {
			funds[h.Player] += h.Credits
		}
	}
	players := make([]string, 0, len(funds))
	for p := range funds {
		players = append(players, p)
	}
	sort.Strings(players)
	for _, p := range players {
		FundPlayer(t, db, p, funds[p])
	}
	SeedGoldenAdmissions(t, db, gr, 1, upTo)
	return gr
}

// SeedGoldenAdmissions seeds golden admissions from..to (to 0: all).
func SeedGoldenAdmissions(t testing.TB, db *pgxpool.Pool, g Golden, from, to int) {
	t.Helper()
	if to == 0 {
		to = len(g.Commands)
	}
	for i := from; i <= to; i++ {
		c := g.Commands[i-1]
		if seq := SeedAdmission(t, db, g.RoundID, c.CommandID, c.Payload, c.EffectiveTimeUnixMicro, goldenHold(t, c)); seq != c.Sequence {
			t.Fatalf("seeded sequence %d, want %d", seq, c.Sequence)
		}
	}
}

// goldenHold returns the S-10 hold of golden command record c.
func goldenHold(t testing.TB, c chain.Command) *Hold {
	t.Helper()
	cmd, err := crashv1.Adapter().DecodeCommand(c.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return HoldFor(cmd)
}

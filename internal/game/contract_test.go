package game_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/juano-morello/orvex/internal/game"
)

type tallyGame = game.Game[tallyState, tallyCommand, tallyEvent, tallyConfig]

// erased runs the typed contract through the type-erased Adapter, so every
// purity check covers both the typed game and the runtime boundary.
type erased struct{ a game.Adapter }

var _ tallyGame = erased{}

func (e erased) InitialState(cfg tallyConfig) (tallyState, error) {
	s, err := e.a.InitialState(cfg)
	return stateOf(s), err
}

func (e erased) Decide(s tallyState, c tallyCommand, in game.Input) ([]tallyEvent, error) {
	out, err := e.a.Decide(s, c, in)
	if out == nil {
		return nil, err
	}
	events := make([]tallyEvent, len(out))
	for i, ev := range out {
		events[i] = ev.(tallyEvent)
	}
	return events, err
}

func (e erased) Evolve(s tallyState, ev tallyEvent) (tallyState, error) {
	next, err := e.a.Evolve(s, ev)
	return stateOf(next), err
}

func stateOf(v any) tallyState {
	if v == nil {
		return tallyState{}
	}
	return v.(tallyState)
}

// mustBeComparable keeps Input comparable and free of reference fields at
// compile time.
func mustBeComparable[T comparable]() {}

var _ = mustBeComparable[game.Input]

// stateSnapshot is a deep copy of a tallyState plus the full backing array
// of its History, so writes past len (append aliasing) are caught too.
type stateSnapshot struct {
	state   tallyState
	backing []uint64
}

func takeSnapshot(s tallyState) stateSnapshot {
	cp := s
	cp.Scores = maps.Clone(s.Scores)
	cp.History = slices.Clone(s.History)
	return stateSnapshot{state: cp, backing: slices.Clone(s.History[:cap(s.History)])}
}

func (snap stateSnapshot) changed(s tallyState) bool {
	return !reflect.DeepEqual(snap.state, s) || !slices.Equal(snap.backing, s.History[:cap(s.History)])
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// purityViolation runs Decide twice, then Evolve twice for every produced
// event, all against the same s. It returns a description of the first
// non-deterministic result or mutation of s, or "" if every call was pure.
func purityViolation(g tallyGame, s tallyState, c tallyCommand, in game.Input) string {
	snap := takeSnapshot(s)

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
	}
	return ""
}

func gamesUnderTest() []struct {
	name string
	g    tallyGame
} {
	return []struct {
		name string
		g    tallyGame
	}{
		{"typed", tally{}},
		{"adapter", erased{game.Adapt(sampleDescriptor(1), tally{})}},
	}
}

func TestSampleGamePurity(t *testing.T) {
	closedState := baseState()
	closedState.Closed = true

	in := game.Input{Sequence: 7, EffectiveTimeUnixMicro: 1_700_000_000_000_000}
	cases := []struct {
		name  string
		state tallyState
		cmd   tallyCommand
	}{
		{"accepted score", baseState(), score{Player: "alice", Points: 5}},
		{"rejected over limit", baseState(), score{Player: "bob", Points: 1_000}},
		{"rejected zero points", baseState(), score{Player: "bob", Points: 0}},
		{"accepted close", baseState(), closeTally{}},
		{"score after close", closedState, score{Player: "alice", Points: 1}},
	}
	for _, tc := range cases {
		for _, gut := range gamesUnderTest() {
			t.Run(tc.name+"/"+gut.name, func(t *testing.T) {
				if v := purityViolation(gut.g, tc.state, tc.cmd, in); v != "" {
					t.Fatal(v)
				}
			})
		}
	}
}

type decideWritesMap struct{ tally }

func (decideWritesMap) Decide(s tallyState, c tallyCommand, in game.Input) ([]tallyEvent, error) {
	s.Scores["leak"]++
	return tally{}.Decide(s, c, in)
}

type evolveWritesMap struct{ tally }

func (evolveWritesMap) Evolve(s tallyState, e tallyEvent) (tallyState, error) {
	s.Scores["leak"]++
	return tally{}.Evolve(s, e)
}

type evolveAppendsShared struct{ tally }

func (evolveAppendsShared) Evolve(s tallyState, e tallyEvent) (tallyState, error) {
	// Writes into spare capacity of the caller's backing array: invisible
	// to a len-bounded comparison, caught by the full-capacity check.
	s.History = append(s.History, 99)
	return s, nil
}

// TestPurityCheckDetectsMutation is the negative control for
// TestSampleGamePurity: leaky games must be caught.
func TestPurityCheckDetectsMutation(t *testing.T) {
	cases := []struct {
		name string
		g    tallyGame
	}{
		{"decide writes map", decideWritesMap{}},
		{"evolve writes map", evolveWritesMap{}},
		{"evolve appends into shared backing array", evolveAppendsShared{}},
	}
	in := game.Input{Sequence: 7, EffectiveTimeUnixMicro: 1_000}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v := purityViolation(tc.g, baseState(), score{Player: "alice", Points: 5}, in); v == "" {
				t.Fatal("purityViolation did not detect the mutation")
			}
		})
	}
}

func TestSampleGameConcurrentUse(t *testing.T) {
	shared := baseState()
	snap := takeSnapshot(shared)
	cmd := score{Player: "carol", Points: 3}
	in := game.Input{Sequence: 9, EffectiveTimeUnixMicro: 9_000}

	g := tally{}
	wantEvents, err := g.Decide(shared, cmd, in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	wantNext, err := g.Evolve(shared, wantEvents[0])
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	const workers = 8
	events := make([][]tallyEvent, workers)
	nexts := make([]tallyState, workers)
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

func FuzzSampleGamePurity(f *testing.F) {
	f.Add("alice", 5, uint64(1), int64(1_000), false)
	f.Add("bob", 85, uint64(2), int64(2_000), false)
	f.Add("bob", 86, uint64(3), int64(3_000), false)
	f.Add("carol", 0, uint64(4), int64(4_000), false)
	f.Add("carol", -1, uint64(5), int64(5_000), false)
	f.Add("", 1, uint64(6), int64(6_000), true)
	f.Add("dave", math.MaxInt, uint64(0), int64(0), false)
	f.Add("eve", math.MinInt, uint64(math.MaxUint64), int64(math.MaxInt64), false)
	f.Add("frank", 1, uint64(math.MaxUint64), int64(math.MinInt64), true)

	f.Fuzz(func(t *testing.T, player string, points int, seq uint64, at int64, closeIt bool) {
		var cmd tallyCommand = score{Player: player, Points: points}
		if closeIt {
			cmd = closeTally{}
		}
		in := game.Input{Sequence: seq, EffectiveTimeUnixMicro: at}
		for _, gut := range gamesUnderTest() {
			if v := purityViolation(gut.g, baseState(), cmd, in); v != "" {
				t.Fatalf("%s: %s", gut.name, v)
			}
		}
	})
}

// TestNoIOImports guards the "pure functions have no I/O" rule statically:
// the package's production code and the sample game may import only
// packages that cannot do I/O, read a clock, draw randomness or reflect.
func TestNoIOImports(t *testing.T) {
	allowed := map[string]bool{
		"errors": true,
		"fmt":    true,
		"maps":   true,
		"slices": true,
		"github.com/juano-morello/orvex/internal/game": true,
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") && name != "sample_game_test.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked[name] = true

		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s: %v", name, imp.Path.Value, err)
			}
			if !allowed[path] {
				t.Errorf("%s imports %q, which is not on the pure-code allowlist", name, path)
			}
			if imp.Name != nil {
				t.Errorf("%s renames import %q; named, blank and dot imports are not allowed", name, path)
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "fmt" && sel.Sel.Name != "Errorf" {
				t.Errorf("%s: fmt.%s is not allowed; only fmt.Errorf", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}

	for _, want := range []string{"contract.go", "registry.go", "sample_game_test.go"} {
		if !checked[want] {
			t.Errorf("%s was not checked", want)
		}
	}
}

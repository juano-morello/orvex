package game_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/juano-morello/orvex/internal/game"
)

func TestAdapterMatchesTypedGame(t *testing.T) {
	typed := tally{}
	a := game.Adapt(sampleDescriptor(1), tally{})
	viaAdapter := erased{a}

	if got := a.Descriptor(); got != sampleDescriptor(1) {
		t.Fatalf("Descriptor() = %+v, want %+v", got, sampleDescriptor(1))
	}

	s1, err1 := typed.InitialState(tallyConfig{MaxPoints: 20})
	s2, err2 := viaAdapter.InitialState(tallyConfig{MaxPoints: 20})
	if err1 != nil || err2 != nil {
		t.Fatalf("InitialState errors: typed %v, adapter %v", err1, err2)
	}
	if !reflect.DeepEqual(s1, s2) {
		t.Fatalf("InitialState: typed %#v, adapter %#v", s1, s2)
	}

	steps := []struct {
		name    string
		cmd     tallyCommand
		in      game.Input
		wantErr error
		want    []tallyEvent
	}{
		{
			name: "score",
			cmd:  score{Player: "alice", Points: 5},
			in:   game.Input{Sequence: 1, EffectiveTimeUnixMicro: 1_000},
			want: []tallyEvent{scored{Player: "alice", Points: 5, Seq: 1, AtUnixMicro: 1_000}},
		},
		{
			name:    "over limit",
			cmd:     score{Player: "bob", Points: 50},
			in:      game.Input{Sequence: 2, EffectiveTimeUnixMicro: 2_000},
			wantErr: errOverLimit,
		},
		{
			name: "close",
			cmd:  closeTally{},
			in:   game.Input{Sequence: 3, EffectiveTimeUnixMicro: 3_000},
			want: []tallyEvent{closed{Seq: 3, AtUnixMicro: 3_000}},
		},
	}
	for _, st := range steps {
		ev1, err1 := typed.Decide(s1, st.cmd, st.in)
		ev2, err2 := viaAdapter.Decide(s2, st.cmd, st.in)
		if !errors.Is(err1, st.wantErr) || !errors.Is(err2, st.wantErr) {
			t.Fatalf("%s: Decide errors typed %v, adapter %v, want %v", st.name, err1, err2, st.wantErr)
		}
		if !reflect.DeepEqual(ev1, st.want) || !reflect.DeepEqual(ev2, st.want) {
			t.Fatalf("%s: Decide events typed %#v, adapter %#v, want %#v", st.name, ev1, ev2, st.want)
		}
		for i := range ev1 {
			s1, err1 = typed.Evolve(s1, ev1[i])
			s2, err2 = viaAdapter.Evolve(s2, ev2[i])
			if err1 != nil || err2 != nil {
				t.Fatalf("%s: Evolve errors typed %v, adapter %v", st.name, err1, err2)
			}
			if !reflect.DeepEqual(s1, s2) {
				t.Fatalf("%s: Evolve typed %#v, adapter %#v", st.name, s1, s2)
			}
		}
	}

	want := tallyState{MaxPoints: 20, Total: 5, Closed: true, Scores: map[string]int{"alice": 5}, History: []uint64{1, 3}}
	if !reflect.DeepEqual(s2, want) {
		t.Fatalf("final state = %#v, want %#v", s2, want)
	}
}

func TestAdapterRejectsWrongTypes(t *testing.T) {
	a := game.Adapt(sampleDescriptor(1), tally{})
	in := game.Input{Sequence: 1, EffectiveTimeUnixMicro: 1}

	cases := []struct {
		name string
		call func() (any, error)
	}{
		{"InitialState string", func() (any, error) { return a.InitialState("x") }},
		{"InitialState nil", func() (any, error) { return a.InitialState(nil) }},
		{"Decide bad state", func() (any, error) { return a.Decide(1, score{}, in) }},
		{"Decide nil command", func() (any, error) { return a.Decide(baseState(), nil, in) }},
		{"Decide event as command", func() (any, error) { return a.Decide(baseState(), scored{}, in) }},
		{"Evolve nil state", func() (any, error) { return a.Evolve(nil, scored{}) }},
		{"Evolve nil event", func() (any, error) { return a.Evolve(baseState(), nil) }},
		{"Evolve command as event", func() (any, error) { return a.Evolve(baseState(), score{}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.call()
			if !errors.Is(err, game.ErrTypeMismatch) {
				t.Fatalf("err = %v, want ErrTypeMismatch", err)
			}
			if !isNil(got) {
				t.Fatalf("result = %#v, want nil", got)
			}
		})
	}
}

// isNil reports whether v is a nil interface or a nil []any.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	events, ok := v.([]any)
	return ok && events == nil
}

// sloppy returns results alongside errors; the adapter must drop them.
type sloppy struct{ tally }

func (sloppy) Decide(tallyState, tallyCommand, game.Input) ([]tallyEvent, error) {
	return []tallyEvent{closed{}}, errClosed
}

func (sloppy) Evolve(tallyState, tallyEvent) (tallyState, error) {
	return baseState(), errClosed
}

func TestAdapterGameErrors(t *testing.T) {
	a := game.Adapt(sampleDescriptor(1), tally{})
	in := game.Input{Sequence: 1, EffectiveTimeUnixMicro: 1}

	events, err := a.Decide(baseState(), score{Player: "bob", Points: 1_000}, in)
	if !errors.Is(err, errOverLimit) || errors.Is(err, game.ErrTypeMismatch) {
		t.Fatalf("over limit: err = %v, want errOverLimit only", err)
	}
	if events != nil {
		t.Fatalf("over limit: events = %#v, want nil", events)
	}

	s, err := a.InitialState(tallyConfig{})
	if !errors.Is(err, errInvalidConfig) {
		t.Fatalf("InitialState: err = %v, want errInvalidConfig", err)
	}
	if s != nil {
		t.Fatalf("InitialState: state = %#v, want nil", s)
	}

	sa := game.Adapt(sampleDescriptor(1), sloppy{})
	events, err = sa.Decide(baseState(), closeTally{}, in)
	if !errors.Is(err, errClosed) || events != nil {
		t.Fatalf("sloppy Decide = %#v, %v; want nil, errClosed", events, err)
	}
	next, err := sa.Evolve(baseState(), closed{})
	if !errors.Is(err, errClosed) || next != nil {
		t.Fatalf("sloppy Evolve = %#v, %v; want nil, errClosed", next, err)
	}
}

var (
	v1 = game.Descriptor{Game: "tally", RulesVersion: 1, SchemaVersion: 1, StateVersion: 1}
	v2 = game.Descriptor{Game: "tally", RulesVersion: 2, SchemaVersion: 1, StateVersion: 2}
)

func newTestRegistry(t *testing.T) *game.Registry {
	t.Helper()
	r, err := game.NewRegistry(game.Adapt(v1, tally{}), game.Adapt(v2, tally{}))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func TestRegistryLookup(t *testing.T) {
	r := newTestRegistry(t)
	for _, want := range []game.Descriptor{v1, v2} {
		a, err := r.Lookup(want.Game, want.RulesVersion)
		if err != nil {
			t.Fatalf("Lookup(%q, %d): %v", want.Game, want.RulesVersion, err)
		}
		if got := a.Descriptor(); got != want {
			t.Fatalf("Lookup(%q, %d).Descriptor() = %+v, want %+v", want.Game, want.RulesVersion, got, want)
		}
	}
}

func TestRegistryLookupFailsClosed(t *testing.T) {
	r := newTestRegistry(t)
	empty, err := game.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry(): %v", err)
	}

	cases := []struct {
		name  string
		r     *game.Registry
		game  game.ID
		rules game.RulesVersion
	}{
		{"unregistered rules version", r, "tally", 3},
		{"unknown game", r, "crash", 1},
		{"zero values", r, "", 0},
		{"empty registry", empty, "tally", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := tc.r.Lookup(tc.game, tc.rules)
			if !errors.Is(err, game.ErrUnknownRules) {
				t.Fatalf("err = %v, want ErrUnknownRules", err)
			}
			if a != nil {
				t.Fatalf("adapter = %+v, want nil (no fallback; got descriptor %+v)", a, a.Descriptor())
			}
		})
	}
}

func TestNewRegistryRejectsInvalid(t *testing.T) {
	with := func(f func(*game.Descriptor)) game.Adapter {
		d := v1
		f(&d)
		return game.Adapt(d, tally{})
	}
	cases := []struct {
		name     string
		adapters []game.Adapter
		want     error
	}{
		{"nil adapter", []game.Adapter{nil}, game.ErrInvalidRegistration},
		{"empty game", []game.Adapter{with(func(d *game.Descriptor) { d.Game = "" })}, game.ErrInvalidRegistration},
		{"zero rules version", []game.Adapter{with(func(d *game.Descriptor) { d.RulesVersion = 0 })}, game.ErrInvalidRegistration},
		{"zero schema version", []game.Adapter{with(func(d *game.Descriptor) { d.SchemaVersion = 0 })}, game.ErrInvalidRegistration},
		{"zero state version", []game.Adapter{with(func(d *game.Descriptor) { d.StateVersion = 0 })}, game.ErrInvalidRegistration},
		{
			"duplicate rules version",
			[]game.Adapter{
				game.Adapt(v1, tally{}),
				with(func(d *game.Descriptor) { d.SchemaVersion = 2; d.StateVersion = 2 }),
			},
			game.ErrDuplicateRules,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := game.NewRegistry(tc.adapters...)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if r != nil {
				t.Fatalf("registry = %+v, want nil", r)
			}
		})
	}
}

func TestRegistryConcurrentLookup(t *testing.T) {
	r := newTestRegistry(t)
	const workers, rounds = 16, 200
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				for _, want := range []game.Descriptor{v1, v2} {
					a, err := r.Lookup(want.Game, want.RulesVersion)
					if err != nil {
						errs <- err
						return
					}
					if a.Descriptor() != want {
						errs <- errors.New("descriptor mismatch")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

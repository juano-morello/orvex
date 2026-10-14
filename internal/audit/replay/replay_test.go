package replay_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/replay"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
)

const t0 = int64(1_760_000_000_000_000)

func testConfig() v1.Config {
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i)
	}
	return v1.Config{
		RoundID:       [16]byte{0x01, 0x90, 0xf3, 0xa8, 0x7b, 0x3c, 0x7d, 0x4e, 0x8f, 0x00, 0, 0, 0, 0, 0, 0x01},
		Seed:          seed,
		MinBetCredits: 1,
		MaxBetCredits: 1_000_000_000,
		MaxBets:       3,
	}
}

type step struct {
	c  v1.Command
	at int64
}

// script is a short round: an accepted bet, a duplicate (rejected), the
// start, an accepted cash-out and a cash-out without a bet (rejected).
var script = []step{
	{v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 100}, t0 - 2_000_000},
	{v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 50}, t0 - 1_000_000},
	{v1.StartRound{Authority: v1.AuthoritySystem}, t0},
	{v1.CashOut{Authority: v1.AuthorityPlayer, Player: "alice"}, t0 + 1_000_000},
	{v1.CashOut{Authority: v1.AuthorityPlayer, Player: "bob"}, t0 + 2_000_000},
}

// buildRound runs script through the crash/v1 rules and codec and returns
// the initial state and the records, as the engine would write them.
func buildRound(t *testing.T) (any, []replay.Record) {
	t.Helper()
	a := v1.Adapter()
	initial, err := a.InitialState(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := initial
	var records []replay.Record
	for i, st := range script {
		in := game.Input{Sequence: uint64(i + 1), EffectiveTimeUnixMicro: st.at}
		r := replay.Record{Sequence: in.Sequence, Input: in, Command: st.c}
		events, err := a.Decide(s, st.c, in)
		if err != nil {
			code, ok := a.RejectionCode(err)
			if !ok {
				t.Fatalf("step %d fails closed: %v", i+1, err)
			}
			r.Result = chain.Result{Rejected: true, Reason: code}
		}
		for _, e := range events {
			b, err := a.EncodeEvent(e)
			if err != nil {
				t.Fatal(err)
			}
			if s, err = a.Evolve(s, e); err != nil {
				t.Fatal(err)
			}
			r.Events = append(r.Events, e)
			r.EventBytes = append(r.EventBytes, b)
		}
		records = append(records, r)
	}
	if !records[1].Result.Rejected || records[3].Result.Rejected || !records[4].Result.Rejected {
		t.Fatalf("script outcomes changed: %+v", records)
	}
	return initial, records
}

func TestRunReproducesRound(t *testing.T) {
	initial, records := buildRound(t)
	for _, mode := range []replay.Mode{replay.StateOnly, replay.StateAndDecision} {
		var seen []uint64
		var last any
		err := replay.Run(v1.Adapter(), initial, records, mode, func(seq uint64, s any) error {
			seen = append(seen, seq)
			last = s
			return nil
		})
		if err != nil {
			t.Fatalf("mode %d: %v", mode, err)
		}
		if want := []uint64{0, 1, 2, 3, 4, 5}; !slices.Equal(seen, want) {
			t.Errorf("mode %d: visited %v; want %v", mode, seen, want)
		}
		s := last.(v1.State)
		if s.Phase != v1.PhaseRunning || len(s.Bets) != 1 || s.Bets[0].Status != v1.StatusCashedOut {
			t.Errorf("mode %d: final state %+v", mode, s)
		}
	}

	stop := errors.New("stop")
	calls := 0
	err := replay.Run(v1.Adapter(), initial, records, replay.StateAndDecision, func(seq uint64, _ any) error {
		calls++
		if seq == 2 {
			return stop
		}
		return nil
	})
	if err != stop || calls != 3 { //nolint:errorlint // a visit error is returned as is
		t.Errorf("Run with a failing visit = %v after %d calls; want stop after 3", err, calls)
	}
	if err := replay.Run(v1.Adapter(), initial, records, replay.StateOnly, nil); err != nil {
		t.Errorf("Run with nil visit: %v", err)
	}
	if err := replay.Run(v1.Adapter(), initial, records, 0, nil); err == nil {
		t.Error("Run accepted mode 0")
	}
}

func encode(t *testing.T, e v1.Event) []byte {
	t.Helper()
	b, err := v1.Codec{}.EncodeEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecisionMismatch(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(t *testing.T, rs []replay.Record)
		seq       uint64
		stateOnly *uint64 // nil: StateOnly passes; else the state mismatch sequence
	}{
		{
			name: "changed event bytes",
			mutate: func(t *testing.T, rs []replay.Record) {
				e := v1.BetPlaced{Player: "alice", BetCredits: 100, PlacedAtUnixMicro: t0 - 1_999_999}
				rs[0].Events[0], rs[0].EventBytes[0] = e, encode(t, e)
			},
			seq: 1,
		},
		{
			name: "recorded rejected but Decide accepts",
			mutate: func(_ *testing.T, rs []replay.Record) {
				rs[3].Result = chain.Result{Rejected: true, Reason: "no_bet"}
				rs[3].Events, rs[3].EventBytes = nil, nil
			},
			seq: 4,
		},
		{
			name: "recorded accepted but Decide rejects",
			mutate: func(t *testing.T, rs []replay.Record) {
				e := v1.BetPlaced{Player: "alice", BetCredits: 50, PlacedAtUnixMicro: t0 - 1_000_000}
				rs[1].Result = chain.Result{}
				rs[1].Events, rs[1].EventBytes = []any{e}, [][]byte{encode(t, e)}
			},
			seq:       2,
			stateOnly: new(uint64(2)),
		},
		{
			name: "different reason",
			mutate: func(_ *testing.T, rs []replay.Record) {
				rs[4].Result.Reason = "wrong_phase"
			},
			seq: 5,
		},
		{
			name: "event count differs",
			mutate: func(_ *testing.T, rs []replay.Record) {
				rs[3].Events, rs[3].EventBytes = nil, nil
			},
			seq: 4,
		},
		{
			name: "extra event",
			mutate: func(_ *testing.T, rs []replay.Record) {
				rs[3].Events = append(rs[3].Events, rs[3].Events[0])
				rs[3].EventBytes = append(rs[3].EventBytes, rs[3].EventBytes[0])
			},
			seq:       4,
			stateOnly: new(uint64(4)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			initial, records := buildRound(t)
			tc.mutate(t, records)

			err := replay.Run(v1.Adapter(), initial, records, replay.StateAndDecision, nil)
			var m *replay.Mismatch
			if !errors.As(err, &m) || !m.Decision || m.Sequence != tc.seq {
				t.Fatalf("StateAndDecision: %v; want a decision mismatch at %d", err, tc.seq)
			}

			err = replay.Run(v1.Adapter(), initial, records, replay.StateOnly, nil)
			if tc.stateOnly == nil {
				if err != nil {
					t.Fatalf("StateOnly: %v; want success", err)
				}
				return
			}
			if !errors.As(err, &m) || m.Decision || m.Sequence != *tc.stateOnly {
				t.Fatalf("StateOnly: %v; want a state mismatch at %d", err, *tc.stateOnly)
			}
		})
	}
}

// failClosed wraps the crash/v1 adapter with a Decide that fails closed.
type failClosed struct{ game.Adapter }

func (failClosed) Decide(any, any, game.Input) ([]any, error) {
	return nil, v1.ErrInvalidState
}

func TestFailClosedDecideIsMismatch(t *testing.T) {
	initial, records := buildRound(t)
	err := replay.Run(failClosed{v1.Adapter()}, initial, records, replay.StateAndDecision, nil)
	var m *replay.Mismatch
	if !errors.As(err, &m) || !m.Decision || m.Sequence != 1 || !errors.Is(err, v1.ErrInvalidState) {
		t.Fatalf("Run = %v; want a decision mismatch at 1 wrapping ErrInvalidState", err)
	}
	// State replay alone does not call Decide.
	if err := replay.Run(failClosed{v1.Adapter()}, initial, records, replay.StateOnly, nil); err != nil {
		t.Fatalf("StateOnly = %v", err)
	}
}

func TestEvolveErrorIsStateMismatch(t *testing.T) {
	initial, records := buildRound(t)
	co := records[3].Events[0].(v1.BetCashedOut)
	co.PayoutCredits++
	records[3].Events[0], records[3].EventBytes[0] = co, encode(t, co)

	err := replay.Run(v1.Adapter(), initial, records, replay.StateOnly, nil)
	var m *replay.Mismatch
	if !errors.As(err, &m) || m.Decision || m.Sequence != 4 || !errors.Is(err, v1.ErrEventNotApplicable) {
		t.Fatalf("StateOnly = %v; want a state mismatch at 4 wrapping ErrEventNotApplicable", err)
	}
	if got := m.Error(); got == "" || !errors.Is(m, v1.ErrEventNotApplicable) {
		t.Fatalf("Mismatch.Error() = %q", got)
	}
	// In StateAndDecision mode the forged event fails decision replay first.
	err = replay.Run(v1.Adapter(), initial, records, replay.StateAndDecision, nil)
	if !errors.As(err, &m) || !m.Decision || m.Sequence != 4 || !errors.Is(err, replay.ErrEventDiffers) {
		t.Fatalf("StateAndDecision = %v; want a decision mismatch at 4", err)
	}
}

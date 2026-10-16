package execution

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

func crashRules() rulesEntry {
	return rulesEntry{adapter: v1.Adapter(), policy: CrashV1Policy(), rule: ledger.CrashV1()}
}

// accountOf is a synthetic account ID per player.
func accountOf(player string) [16]byte {
	sum := sha256.Sum256([]byte("account:" + player))
	var id [16]byte
	copy(id[:], sum[:])
	return id
}

// holdsFor applies LBT-11 S-10 to command bytes.
func holdsFor(t *testing.T, command []byte) []holdRow {
	t.Helper()
	cmd, err := v1.Adapter().DecodeCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := admission.CrashV1Policy().Classify(cmd)
	if err != nil || cl.Hold.PlayerID == "" {
		return nil
	}
	return []holdRow{{accountID: accountOf(cl.Hold.PlayerID), amount: cl.Hold.Credits, player: cl.Hold.PlayerID}}
}

// goldenTransitions is spec section 8 per golden history: the ledger
// transaction kind per record and the checkpoint records.
var goldenTransitions = map[string]struct {
	kinds       map[uint64]string
	checkpoints []uint64
	releases    int
}{
	"worked-example": {map[uint64]string{1: "crash_bet", 2: "crash_bet", 3: "crash_bet", 12: "crash_cash_out", 17: "crash_cash_out", 21: "crash_settlement"}, []uint64{9, 19, 21}, 7},
	"instant-crash":  {map[uint64]string{1: "crash_bet", 5: "crash_settlement"}, []uint64{2, 4, 5}, 1},
	"empty-round":    {map[uint64]string{}, []uint64{1, 2, 3}, 0},
	"in-progress":    {map[uint64]string{1: "crash_bet", 3: "crash_cash_out"}, []uint64{2}, 1},
}

func TestTransitionReproducesGoldenRecords(t *testing.T) {
	for name, want := range goldenTransitions {
		t.Run(name, func(t *testing.T) {
			g := loadGolden(t, name)
			initial, err := v1.Adapter().InitialState(g.config)
			if err != nil {
				t.Fatal(err)
			}
			st := roundState{state: initial, head: chain.LinkHash(g.lines[0]), nextEventSequence: 1}
			kinds := map[uint64]string{}
			var checkpoints []uint64
			releases := 0
			for i, c := range g.cmds {
				adm := admissionRow{sequence: c.Sequence, commandID: c.CommandID, effectiveTime: c.EffectiveTimeUnixMicro,
					command: c.Payload, holds: holdsFor(t, c.Payload)}
				tr, err := computeTransition(crashRules(), g.genesis.RoundID, st, adm, "golden")
				if err != nil {
					t.Fatalf("record %d: %v", c.Sequence, err)
				}
				line := g.lines[i+1]
				if !bytes.Equal(tr.record, line) {
					t.Fatalf("record %d differs:\n got %s\nwant %s", c.Sequence, tr.record, line)
				}
				if tr.hash != chain.LinkHash(line) || tr.result != c.Result {
					t.Errorf("record %d: hash or result differs", c.Sequence)
				}
				var types []string
				for _, e := range c.Events {
					o, err := canonjson.Unmarshal(e.Payload)
					if err != nil {
						t.Fatal(err)
					}
					typ, _ := canonjson.Text(o["type"])
					types = append(types, typ)
				}
				if !slices.Equal(tr.eventTypes, types) {
					t.Errorf("record %d event types %v, want %v", c.Sequence, tr.eventTypes, types)
				}
				var wantReleases [][16]byte
				for _, h := range adm.holds {
					wantReleases = append(wantReleases, h.accountID)
				}
				if !reflect.DeepEqual(tr.releases, wantReleases) {
					t.Errorf("record %d releases %x, want %x", c.Sequence, tr.releases, wantReleases)
				}
				releases += len(tr.releases)
				if tr.kind != "" {
					kinds[c.Sequence] = tr.kind
				}
				for _, p := range tr.postings {
					if p.FromHold && p.holdAccount != adm.holds[0].accountID {
						t.Errorf("record %d: the bet does not consume its hold", c.Sequence)
					}
				}
				if tr.checkpoint {
					checkpoints = append(checkpoints, c.Sequence)
				}
				st = roundState{state: tr.next, processed: c.Sequence, head: tr.hash, nextEventSequence: st.nextEventSequence + uint64(len(tr.events))}
			}
			if !reflect.DeepEqual(kinds, want.kinds) {
				t.Errorf("ledger transactions %v, want %v", kinds, want.kinds)
			}
			if !slices.Equal(checkpoints, want.checkpoints) {
				t.Errorf("checkpoints %v, want %v", checkpoints, want.checkpoints)
			}
			if releases != want.releases {
				t.Errorf("%d releases, want %d", releases, want.releases)
			}
		})
	}
}

// TestComputeTransitionReveal: the reveal is set exactly on the record that
// first makes the round final (LBT-21 K-6, R-6): worked-example's accepted
// SettleRound at 21, never at 22 (final before and after), at a rejected
// record or at an earlier record.
func TestComputeTransitionReveal(t *testing.T) {
	g := loadGolden(t, "worked-example")
	initial, err := v1.Adapter().InitialState(g.config)
	if err != nil {
		t.Fatal(err)
	}
	st := roundState{state: initial, head: chain.LinkHash(g.lines[0]), nextEventSequence: 1}
	var reveals, finals []uint64
	for _, c := range g.cmds {
		adm := admissionRow{sequence: c.Sequence, commandID: c.CommandID, effectiveTime: c.EffectiveTimeUnixMicro,
			command: c.Payload, holds: holdsFor(t, c.Payload)}
		tr, err := computeTransition(crashRules(), g.genesis.RoundID, st, adm, "golden")
		if err != nil {
			t.Fatalf("record %d: %v", c.Sequence, err)
		}
		if tr.reveal {
			reveals = append(reveals, c.Sequence)
			if tr.result.Rejected {
				t.Errorf("record %d: a rejected record reveals", c.Sequence)
			}
		}
		if tr.final {
			finals = append(finals, c.Sequence)
		}
		st = roundState{state: tr.next, processed: c.Sequence, head: tr.hash, nextEventSequence: st.nextEventSequence + uint64(len(tr.events))}
	}
	if !slices.Equal(reveals, []uint64{21}) {
		t.Errorf("reveals at %v, want [21]", reveals)
	}
	if !slices.Equal(finals, []uint64{21, 22}) {
		t.Errorf("final at %v, want [21 22]", finals)
	}

	// A Final error for the state before the record fails closed.
	adm := betAdmission(t, "alice", 100, []holdRow{{accountID: accountOf("alice"), amount: 100, player: "alice"}})
	before := freshState(t, 1_000_000_000)
	r := crashRules()
	r.policy = finalErrPolicy{Policy: CrashV1Policy(), bad: before.state}
	tr, err := computeTransition(r, testRound, before, adm, "golden")
	var fc *FailClosedError
	if tr != nil || !errors.As(err, &fc) || fc.Sequence != 1 || !errors.Is(err, ErrFailClosed) {
		t.Errorf("Final error before the record: transition %v, err %v; want FailClosedError at 1", tr != nil, err)
	}
}

// finalErrPolicy is the crash/v1 policy whose Final fails for state bad.
type finalErrPolicy struct {
	Policy
	bad any
}

func (p finalErrPolicy) Final(s any) (bool, error) {
	if reflect.DeepEqual(s, p.bad) {
		return false, errors.New("boom")
	}
	return p.Policy.Final(s)
}

// failingAdapter is the crash/v1 adapter with a Decide that fails closed
// at failDecide and an Evolve that always fails when failEvolve is set.
type failingAdapter struct {
	game.Adapter
	failDecide uint64
	failEvolve bool
}

func (a failingAdapter) Decide(state, command any, in game.Input) ([]any, error) {
	if in.Sequence == a.failDecide {
		return nil, errors.New("boom")
	}
	return a.Adapter.Decide(state, command, in)
}

func (a failingAdapter) Evolve(state, event any) (any, error) {
	if a.failEvolve {
		return nil, errors.New("boom")
	}
	return a.Adapter.Evolve(state, event)
}

var testRound = [16]byte{0x0f, 0, 0, 0, 0, 0, 0x70, 0, 0x80, 0, 0, 0, 0, 0, 0, 1}

// freshState returns the initial state of a test round in betting phase.
func freshState(t *testing.T, maxBet int64) roundState {
	t.Helper()
	var seed [32]byte
	seed[0] = 0xab
	s, err := v1.Rules{}.InitialState(v1.Config{RoundID: testRound, Seed: seed, MinBetCredits: 1, MaxBetCredits: maxBet, MaxBets: 100})
	if err != nil {
		t.Fatal(err)
	}
	return roundState{state: s, nextEventSequence: 1}
}

func betAdmission(t *testing.T, player string, credits int64, holds []holdRow) admissionRow {
	t.Helper()
	b, err := v1.Adapter().EncodeCommand(v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: v1.PlayerID(player), BetCredits: credits})
	if err != nil {
		t.Fatal(err)
	}
	return admissionRow{sequence: 1, commandID: [16]byte{0x0c, 1}, effectiveTime: 1_000, command: b, holds: holds}
}

func TestTransitionFailClosed(t *testing.T) {
	adm := betAdmission(t, "alice", 100, []holdRow{{accountID: accountOf("alice"), amount: 100, player: "alice"}})
	for name, a := range map[string]failingAdapter{
		"decide": {Adapter: v1.Adapter(), failDecide: 1},
		"evolve": {Adapter: v1.Adapter(), failEvolve: true},
	} {
		r := crashRules()
		r.adapter = a
		tr, err := computeTransition(r, testRound, freshState(t, 1_000_000_000), adm, "golden")
		var fc *FailClosedError
		if tr != nil || !errors.As(err, &fc) || fc.Sequence != 1 || fc.RoundID != testRound || !errors.Is(err, ErrFailClosed) {
			t.Errorf("%s: transition %v, err %v; want FailClosedError at 1", name, tr != nil, err)
		}
	}
	// Undecodable command bytes fail closed too.
	bad := adm
	bad.command = []byte(`{"type":"Nope"}`)
	if _, err := computeTransition(crashRules(), testRound, freshState(t, 1_000_000_000), bad, "golden"); !errors.Is(err, ErrFailClosed) {
		t.Errorf("undecodable command: err = %v, want ErrFailClosed", err)
	}
}

func TestTransitionHoldChecks(t *testing.T) {
	alice := holdRow{accountID: accountOf("alice"), amount: 100, player: "alice"}
	corrupt := map[string][]holdRow{
		"no hold":              nil,
		"two holds":            {alice, {accountID: accountOf("alice2"), amount: 100, player: "alice"}},
		"another amount":       {{accountID: alice.accountID, amount: 99, player: "alice"}},
		"another player":       {{accountID: accountOf("bob"), amount: 100, player: "bob"}},
		"a non-player account": {{accountID: accountOf("pot"), amount: 100}},
	}
	for name, holds := range corrupt {
		_, err := computeTransition(crashRules(), testRound, freshState(t, 1_000_000_000), betAdmission(t, "alice", 100, holds), "golden")
		if !errors.Is(err, ErrCorrupt) || errors.Is(err, ErrFailClosed) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}

	// A rejected bet with a hold: release only.
	tr, err := computeTransition(crashRules(), testRound, freshState(t, 50), betAdmission(t, "alice", 100, []holdRow{alice}), "golden")
	if err != nil || !tr.result.Rejected || tr.result.Reason != string(v1.ReasonInvalidAmount) {
		t.Fatalf("rejected bet = %+v, %v", tr, err)
	}
	if len(tr.postings) != 0 || tr.kind != "" || !reflect.DeepEqual(tr.releases, [][16]byte{alice.accountID}) {
		t.Errorf("rejected bet: postings %v kind %q releases %x; want a release only", tr.postings, tr.kind, tr.releases)
	}
	// A non-player hold account on a rejected command is released.
	pot := holdRow{accountID: accountOf("pot"), amount: 100}
	tr, err = computeTransition(crashRules(), testRound, freshState(t, 50), betAdmission(t, "alice", 100, []holdRow{pot}), "golden")
	if err != nil || !reflect.DeepEqual(tr.releases, [][16]byte{pot.accountID}) {
		t.Errorf("non-player hold: %v, %v; want released", tr, err)
	}
	// Releases are ascending.
	holds := []holdRow{{accountID: [16]byte{9}, amount: 1}, {accountID: [16]byte{3}, amount: 1}}
	tr, err = computeTransition(crashRules(), testRound, freshState(t, 50), betAdmission(t, "alice", 100, holds), "golden")
	if err != nil || !reflect.DeepEqual(tr.releases, [][16]byte{{3}, {9}}) {
		t.Errorf("releases %x, %v; want ascending", tr.releases, err)
	}
}

// scriptedRule returns fixed postings for any record.
type scriptedRule struct {
	kind     string
	postings []ledger.Posting
}

func (scriptedRule) Descriptor() game.Descriptor { return v1.Descriptor() }

func (r scriptedRule) Postings([]any, uint64) (string, []ledger.Posting, error) {
	return r.kind, r.postings, nil
}

func TestTransitionValidatesPostings(t *testing.T) {
	player := ledger.Account{Kind: ledger.AccountPlayer, Player: "alice"}
	round := ledger.Account{Kind: ledger.AccountRound}
	house := ledger.Account{Kind: ledger.AccountHouse}
	ok := ledger.Posting{Debit: player, Credit: round, Amount: 100, EventSequence: 1, FromHold: true}
	cases := map[string]scriptedRule{
		"amount 0":               {"x", []ledger.Posting{{Debit: player, Credit: round, Amount: 0, EventSequence: 1, FromHold: true}}},
		"debit = credit":         {"x", []ledger.Posting{{Debit: round, Credit: round, Amount: 1, EventSequence: 1}}},
		"event outside":          {"x", []ledger.Posting{{Debit: player, Credit: round, Amount: 100, EventSequence: 2, FromHold: true}}},
		"event before":           {"x", []ledger.Posting{{Debit: player, Credit: round, Amount: 100, EventSequence: 0, FromHold: true}}},
		"two for one event":      {"x", []ledger.Posting{ok, {Debit: round, Credit: house, Amount: 1, EventSequence: 1}}},
		"FromHold on non-player": {"x", []ledger.Posting{{Debit: house, Credit: round, Amount: 100, EventSequence: 1, FromHold: true}}},
		"kind without postings":  {"x", nil},
		"postings without kind":  {"", []ledger.Posting{ok}},
		"invalid account":        {"x", []ledger.Posting{{Debit: ledger.Account{Kind: ledger.AccountPlayer}, Credit: round, Amount: 1, EventSequence: 1}}},
	}
	adm := betAdmission(t, "alice", 100, []holdRow{{accountID: accountOf("alice"), amount: 100, player: "alice"}})
	for name, rule := range cases {
		r := crashRules()
		r.rule = rule
		_, err := computeTransition(r, testRound, freshState(t, 1_000_000_000), adm, "golden")
		var fc *FailClosedError
		if !errors.As(err, &fc) || fc.Sequence != 1 {
			t.Errorf("%s: err = %v, want FailClosedError", name, err)
		}
	}
	r := crashRules()
	r.rule = scriptedRule{"x", []ledger.Posting{ok}}
	if _, err := computeTransition(r, testRound, freshState(t, 1_000_000_000), adm, "golden"); err != nil {
		t.Errorf("valid scripted posting: %v", err)
	}
}

func TestMaxRecordBytes(t *testing.T) {
	if MaxRecordBytes != verification.MaxLineBytes {
		t.Errorf("MaxRecordBytes = %d, want verification.MaxLineBytes = %d", MaxRecordBytes, verification.MaxLineBytes)
	}
	if MaxRecordBytes != 67108864 {
		t.Errorf("MaxRecordBytes = %d, want 67108864 (the 0003 chain_records_record_check)", MaxRecordBytes)
	}
}

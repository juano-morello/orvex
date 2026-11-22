package engineapi

import (
	"bytes"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/runtime/sessions"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

func mustUUID(t *testing.T, s string) [16]byte {
	t.Helper()
	id, err := canonjson.ParseUUID(canonjson.String(s))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestCommandIDGolden pins the derivation. The vectors come from an
// independent computation (Python hashlib) of the documented construction.
func TestCommandIDGolden(t *testing.T) {
	client := mustUUID(t, "5d1f0c2e-7a3b-4c4d-9e8f-0a1b2c3d4ea1")
	for player, want := range map[string]string{
		"p_ana": "6c06205d-cf89-8697-9e4b-724502dcef40",
		"p_bo":  "8135e8dd-6a79-8e58-8ef7-a78b6f5318c3",
	} {
		if got := string(canonjson.UUID(commandID(sessions.PlayerID(player), client))); got != want {
			t.Errorf("commandID(%q) = %s, want %s", player, got, want)
		}
	}
}

var testRound = [16]byte{0x0b, 0x7e, 0x6a, 0x1c, 0x3c, 0x2f, 0x4e, 0x0a, 0x9a, 0x51, 0x6d, 0x2f, 0x0c, 0x1e, 0x4b, 0x77}

var testSeed = [32]byte{0xab, 0xcd, 31: 0xef}

func genesisRecord(t *testing.T, round [16]byte, g chainGenesisOverride) []byte {
	t.Helper()
	public, _, err := v1.Codec{}.EncodeConfig(v1.Config{RoundID: round, Seed: testSeed, MinBetCredits: 1, MaxBetCredits: 5000, MaxBets: 100})
	if err != nil {
		t.Fatal(err)
	}
	gen := chain.Genesis{
		RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		SchemaVersion: v1.SchemaVersion, StateVersion: v1.StateVersion,
		Config: public, CreatedAtUnixMicro: 1_763_800_000_000_000, EngineBuild: "test",
	}
	if g != nil {
		g(&gen)
	}
	b, err := chain.EncodeGenesis(gen)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type chainGenesisOverride func(*chain.Genesis)

func commandRecord(t *testing.T, round [16]byte, seq uint64, result chain.Result, events ...v1.Event) []byte {
	t.Helper()
	payload, err := v1.Codec{}.EncodeCommand(v1.CashOut{Authority: v1.AuthorityPlayer, Player: "p_ana"})
	if err != nil {
		t.Fatal(err)
	}
	c := chain.Command{
		RoundID: round, Sequence: seq, CommandID: [16]byte{0xc0}, EffectiveTimeUnixMicro: 1_763_800_006_210_000,
		EngineBuild: "test", Payload: payload, Result: result,
	}
	for i, e := range events {
		b, err := v1.Codec{}.EncodeEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		c.Events = append(c.Events, chain.Event{Sequence: uint64(i) + 1, Payload: b})
	}
	b, err := chain.EncodeCommand(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPublicRecordGenesis(t *testing.T) {
	got, err := publicRecord(testRound, eventstore.StoredRecord{Sequence: 0, Record: genesisRecord(t, testRound, nil)})
	if err != nil {
		t.Fatal(err)
	}
	commitment := v1.Commitment(testSeed)
	want := &orvexv1.SubscribeRoundResponse{
		Sequence:               0,
		EffectiveTimeUnixMicro: 1_763_800_000_000_000,
		Body: &orvexv1.SubscribeRoundResponse_Genesis{Genesis: &orvexv1.Genesis{
			Commitment: commitment[:], MinBetCredits: 1, MaxBetCredits: 5000, MaxBets: 100,
		}},
	}
	if !proto.Equal(got, want) {
		t.Errorf("publicRecord = %v, want %v", got, want)
	}
	if b, _ := proto.Marshal(got); bytes.Contains(b, testSeed[:]) {
		t.Error("the genesis frame carries the seed")
	}
}

func TestPublicRecordCommand(t *testing.T) {
	cashed := v1.BetCashedOut{Player: "p_ana", CashedOutAtUnixMicro: 1_763_800_006_210_000, CashOutTick: 12, CashOutMultiplierScaled: 25000, PayoutCredits: 250}
	settled := v1.RoundSettled{SettledAtUnixMicro: 9, BetCount: 2, TotalBetCredits: 300, TotalPayoutCredits: 250, Seed: testSeed}
	got, err := publicRecord(testRound, eventstore.StoredRecord{Sequence: 5, Record: commandRecord(t, testRound, 5, chain.Result{}, cashed, settled)})
	if err != nil {
		t.Fatal(err)
	}
	want := &orvexv1.SubscribeRoundResponse{
		Sequence:               5,
		EffectiveTimeUnixMicro: 1_763_800_006_210_000,
		Body: &orvexv1.SubscribeRoundResponse_Events{Events: &orvexv1.Events{Events: []*orvexv1.CrashEvent{
			{Event: &orvexv1.CrashEvent_BetCashedOut{BetCashedOut: &orvexv1.BetCashedOut{
				PlayerId: "p_ana", CashedOutAtUnixMicro: 1_763_800_006_210_000, CashOutTick: 12, CashOutMultiplierScaled: 25000, PayoutCredits: 250,
			}}},
			{Event: &orvexv1.CrashEvent_RoundSettled{RoundSettled: &orvexv1.RoundSettled{
				SettledAtUnixMicro: 9, BetCount: 2, TotalBetCredits: 300, TotalPayoutCredits: 250, Seed: testSeed[:],
			}}},
		}}},
	}
	if !proto.Equal(got, want) {
		t.Errorf("publicRecord = %v, want %v", got, want)
	}
}

func TestPublicRecordRejectedHasNoEvents(t *testing.T) {
	got, err := publicRecord(testRound, eventstore.StoredRecord{Sequence: 3, Record: commandRecord(t, testRound, 3, chain.Result{Rejected: true, Reason: "no_open_bet"})})
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := got.GetBody().(*orvexv1.SubscribeRoundResponse_Events)
	if !ok || len(ev.Events.GetEvents()) != 0 || got.GetSequence() != 3 {
		t.Errorf("publicRecord = %v, want sequence 3 with no events", got)
	}
	if b, _ := proto.Marshal(got); bytes.Contains(b, []byte("no_open_bet")) {
		t.Error("the frame carries the rejection reason")
	}
}

func TestPublicRecordRefusesMismatches(t *testing.T) {
	other := [16]byte{0x01}
	for name, r := range map[string]eventstore.StoredRecord{
		"genesis of another round":  {Sequence: 0, Record: genesisRecord(t, other, nil)},
		"genesis at sequence 1":     {Sequence: 1, Record: genesisRecord(t, testRound, nil)},
		"genesis of another game":   {Sequence: 0, Record: genesisRecord(t, testRound, func(g *chain.Genesis) { g.Game = "dice" })},
		"command at another seq":    {Sequence: 4, Record: commandRecord(t, testRound, 5, chain.Result{})},
		"command of another round":  {Sequence: 5, Record: commandRecord(t, other, 5, chain.Result{})},
		"not a record":              {Sequence: 5, Record: []byte(`{}`)},
		"genesis with a bad config": {Sequence: 0, Record: genesisRecord(t, testRound, func(g *chain.Genesis) { g.Config = []byte(`{"commitment":"00"}`) })},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := publicRecord(testRound, r); !errors.Is(err, errBadRecord) {
				t.Errorf("publicRecord = %v, want errBadRecord", err)
			}
		})
	}
}

//go:build integration && acceptance

package firstround

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
)

// An engine-created round has a random crash point: it may crash at once
// or run to the crash/v1 cap, so every wait allows the longest round.
const (
	smokeWindow = 4 * time.Second
	smokeWait   = smokeWindow + time.Duration(v1.MaxDurationMicros)*time.Microsecond + time.Minute
)

func parseRoundID(t *testing.T, s string) [16]byte {
	t.Helper()
	id, err := canonjson.ParseUUID(canonjson.String(s))
	if err != nil {
		t.Fatalf("round id %q: %v", s, err)
	}
	return id
}

// betInCurrentRound bets in the current round, moving to the next one if
// betting there already closed.
func betInCurrentRound(t *testing.T, p *player, credits int64) (string, receipt) {
	t.Helper()
	closed := map[string]bool{}
	for range 3 {
		round := p.waitCurrentRound(t, smokeWait, func(id string) bool { return id != "" && !closed[id] })
		status, b := p.submit(t, round, placeBetBody(t, newUUID(t), credits))
		if status == http.StatusAccepted {
			return round, decodeReceipt(t, p.name, round, b)
		}
		var e wireError
		_ = json.Unmarshal(b, &e)
		if status != http.StatusConflict || e.Code != "round_closed" {
			t.Fatalf("%s: bet in round %s = %d %s, want 202, or 409 round_closed", p.name, round, status, b)
		}
		closed[round] = true
	}
	t.Fatal("betting had closed in 3 rounds in a row before a bet reached the gateway; widen smokeWindow")
	return "", receipt{}
}

// TestEngineCreatedRoundSurvivesRestart starts the engine on an empty
// database, so the engine creates the round. alice bets, the engine is
// restarted on the same address while the round is unsettled, and the new
// engine process finishes the round and creates the next one. alice tries
// to cash out; a refusal or a lost bet is a valid outcome, since the round
// may crash at once.
func TestEngineCreatedRoundSurvivesRestart(t *testing.T) {
	p := newPlatform(t, "alice")
	p.grant("smoke-alice-1", "alice", 1000)
	engine := p.startEngine(engineConfig{createRounds: true, bettingWindow: smokeWindow})
	gw := p.startGateway(engine.addr)
	alice := p.player("alice", gw)

	round, bet := betInCurrentRound(t, alice, 100)
	id := parseRoundID(t, round)
	requireAdmission(t, p.obs, id, bet, v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 100})
	stream := watch(t, alice, round)
	stream.waitFor(t, "the genesis frame", time.Minute, func(s streamState) bool { return len(s.frames) > 0 })
	if p.obs.settled(id) {
		t.Fatalf("round %s settled before the restart; widen smokeWindow", round)
	}

	requireCleanStop(t, engine)
	restarted := p.startEngine(engineConfig{createRounds: true, bettingWindow: smokeWindow, addr: engine.addr})

	stream.waitEvent(t, "RoundStarted", smokeWait)
	cashOutID := newUUID(t)
	status, b := alice.submit(t, round, cashOutBody(t, cashOutID))
	switch status {
	case http.StatusAccepted:
		decodeReceipt(t, alice.name, round, b)
	case http.StatusConflict:
		var e wireError
		_ = json.Unmarshal(b, &e)
		if e.Code != "round_closed" && e.Code != "round_sealed" {
			t.Fatalf("cash-out = %d %s, want 202, or 409 round_closed or round_sealed", status, b)
		}
	default:
		t.Fatalf("cash-out = %d %s, want 202 or 409", status, b)
	}

	frames := stream.waitEnd(t, smokeWait)
	records := p.obs.records(id)
	requireFramesMatch(t, "alice", frames, records)
	if g := records[0].Genesis; g == nil || g.Game != v1.GameID {
		t.Fatalf("record 0 of round %s is %+v, want a crash genesis", round, records[0])
	}
	var payout int64
	outcomes := 0
	for _, rec := range records[1:] {
		for _, e := range rec.Events {
			switch e := e.(type) {
			case v1.BetCashedOut:
				outcomes++
				payout = e.PayoutCredits
			case v1.BetLost:
				outcomes++
			}
		}
	}
	if outcomes != 1 {
		t.Errorf("alice's bet has %d outcomes in the round, want 1", outcomes)
	}
	if status == http.StatusAccepted {
		code, s := alice.lookup(t, cashOutID)
		want := "rejected"
		if payout > 0 {
			want = "accepted"
		}
		if code != http.StatusOK || s.Status != want {
			t.Errorf("lookup of the cash-out = %d %+v, want %s", code, s, want)
		}
	}
	if !p.obs.settled(id) || p.obs.admissionState(id) != "sealed" {
		t.Errorf("round revealed %v, admission state %q; want revealed and sealed", p.obs.settled(id), p.obs.admissionState(id))
	}
	p.obs.requirePlayer("alice", 1000-100+payout, 0)
	p.obs.requireSettledLedger([][16]byte{id}, 100-payout)
	p.auditV2(id, records)

	next := alice.waitCurrentRound(t, time.Minute, func(s string) bool { return s != "" && s != round })
	nextStream := watch(t, alice, next)
	nextStream.waitFor(t, "the genesis frame", time.Minute, func(s streamState) bool { return len(s.frames) > 0 })

	requireCleanStop(t, gw)
	requireCleanStop(t, restarted)
	p.requireNoSecretsInOutput()
	reconnects, repeats := stream.stats()
	t.Logf("round %s paid %d; the stream reconnected %d times across the restart with %d repeated frames; next round %s",
		round, payout, reconnects, repeats, next)
}

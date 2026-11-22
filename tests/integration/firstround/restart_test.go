//go:build integration && acceptance

package firstround

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// betAttempt is one clientCommandId the smoke bet used.
type betAttempt struct {
	round, commandID string
	// submits counts identical requests; more than one means a delivery
	// was uncertain.
	submits int
	// outcome is "accepted", or why the round took no bet: wrong_phase
	// (admitted behind StartRound, so the rules rejected it), round_closed
	// or round_sealed.
	outcome string
}

const maxSmokeAttempts = 3

// placeSmokeBet bets credits for p and waits until execution accepts the
// bet. It tries first, unless empty, then each new current round. A round
// past its betting window refuses the bet at admission (409 round_closed
// or round_sealed), or admits it behind StartRound and the rules reject it
// (wrong_phase). The next attempt then waits for a different round and
// draws a fresh clientCommandId, because the old one names a command of the
// old round. An uncertain delivery keeps the clientCommandId.
func placeSmokeBet(t *testing.T, p *player, credits int64, first string, timeout time.Duration) (string, receipt, []betAttempt) {
	t.Helper()
	tried := map[string]bool{}
	var attempts []betAttempt
	for len(attempts) < maxSmokeAttempts {
		round := first
		first = ""
		if round == "" {
			round = p.waitCurrentRound(t, timeout, func(id string) bool { return id != "" && !tried[id] })
		}
		tried[round] = true
		a := betAttempt{round: round, commandID: newUUID(t)}
		status, b, n := p.submitDefinite(t, round, placeBetBody(t, a.commandID, credits), timeout)
		a.submits = n
		switch status {
		case http.StatusAccepted:
			r := decodeReceipt(t, p.name, round, b)
			s := p.awaitOutcome(t, a.commandID, timeout)
			if s.Sequence != r.Sequence || s.RoundID != round {
				t.Fatalf("%s: bet receipt %+v, lookup %+v: want the same admission", p.name, r, s)
			}
			if s.Status == "accepted" {
				a.outcome = s.Status
				return round, r, append(attempts, a)
			}
			if s.Reason != string(v1.ReasonWrongPhase) {
				t.Fatalf("%s: bet in round %s rejected with %q, want accepted or %s", p.name, round, s.Reason, v1.ReasonWrongPhase)
			}
			a.outcome = s.Reason
		case http.StatusConflict:
			a.outcome = errorCode(b)
			if a.outcome != "round_closed" && a.outcome != "round_sealed" {
				t.Fatalf("%s: bet in round %s = %d %s, want 202, or 409 round_closed or round_sealed", p.name, round, status, b)
			}
		default:
			t.Fatalf("%s: bet in round %s = %d %s, want 202 or 409", p.name, round, status, b)
		}
		attempts = append(attempts, a)
	}
	t.Fatalf("%s: %d rounds in a row took no bet: %+v; widen smokeWindow", p.name, len(attempts), attempts)
	return "", receipt{}, nil
}

// TestEngineCreatedRoundSurvivesRestart starts the engine on an empty
// database, so the engine creates the rounds. alice first bets in a round
// that already started, so the bet misses, and the smoke bet moves to the
// next round with a new clientCommandId. Then the engine is restarted on
// the same address while that round is unsettled, and the new engine
// process finishes the round and creates the next one. alice tries to
// cash out; a refusal or a lost bet is a valid outcome, since the round may
// crash at once. No seed of the three rounds may reach a service log from
// start through shutdown, which a test-only decoy that prints one late
// proves the check can see.
func TestEngineCreatedRoundSurvivesRestart(t *testing.T) {
	p := newPlatform(t, "alice")
	p.grant("smoke-alice-1", "alice", 1000)
	engine := p.startEngine(engineConfig{createRounds: true, bettingWindow: smokeWindow})
	gw := p.startGateway(engine.addr)
	alice := p.player("alice", gw)

	missed := alice.waitCurrentRound(t, smokeWait, func(id string) bool { return id != "" })
	missedStream := watch(t, alice, missed)
	missedStream.waitEvent(t, "RoundStarted", smokeWait)
	round, bet, attempts := placeSmokeBet(t, alice, 100, missed, smokeWait)
	if len(attempts) < 2 || attempts[0].round != missed || attempts[0].outcome == "accepted" {
		t.Fatalf("smoke bet attempts %+v, want a miss in the started round %s first", attempts, missed)
	}
	for i, a := range attempts[1:] {
		if prev := attempts[i]; a.commandID == prev.commandID || a.round == prev.round {
			t.Fatalf("smoke bet attempts %+v: attempt %d reuses the round or the clientCommandId of the one before", attempts, i+1)
		}
	}
	missedID := parseRoundID(t, missed)
	requireFramesMatch(t, "alice", missedStream.waitEnd(t, smokeWait), p.obs.records(missedID))

	id := parseRoundID(t, round)
	requireAdmission(t, p.obs, id, bet, v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 100})
	stream := watch(t, alice, round)
	stream.waitFor(t, "the genesis frame", time.Minute, func(s streamState) bool { return len(s.frames) > 0 })
	if p.obs.settled(id) {
		t.Fatalf("round %s settled before the restart; widen smokeWindow", round)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	decoyFile := filepath.Join(t.TempDir(), "decoy")
	decoy, err := launch("decoy", exe, nil, []string{helperEnv + "=decoy", decoyFileEnv + "=" + decoyFile}, readyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = decoy.stop() })

	requireCleanStop(t, engine)
	restarted := p.startEngine(engineConfig{createRounds: true, bettingWindow: smokeWindow, addr: engine.addr})

	stream.waitEvent(t, "RoundStarted", smokeWait)
	cashOutID := newUUID(t)
	status, b, _ := alice.submitDefinite(t, round, cashOutBody(t, cashOutID), smokeWait)
	switch status {
	case http.StatusAccepted:
		decodeReceipt(t, alice.name, round, b)
	case http.StatusConflict:
		if e := errorCode(b); e != "round_closed" && e != "round_sealed" {
			t.Fatalf("cash-out = %d %s, want 202, or 409 round_closed or round_sealed", status, b)
		}
	default:
		t.Fatalf("cash-out = %d %s, want 202 or 409", status, b)
	}

	frames := stream.waitEnd(t, smokeWait)
	records := p.obs.records(id)
	requireFramesMatch(t, "alice", frames, records)
	requireFinalCursorStops(t, alice, round, frames[len(frames)-1].Sequence)
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
	p.obs.requireSettledLedger([][16]byte{missedID, id}, 100-payout)
	_, export := p.auditV2(id, records)

	next := alice.waitCurrentRound(t, time.Minute, func(s string) bool { return s != "" && s != round })
	nextStream := watch(t, alice, next)
	nextStream.waitFor(t, "the genesis frame", time.Minute, func(s streamState) bool { return len(s.frames) > 0 })

	seed := seedSecret("the seed of round "+round, p.obs.seed(id))
	settledFrame, _ := streamState{frames: frames}.event("RoundSettled")
	if got := findLeaks([]output{{"the RoundSettled frame", settledFrame.Data}, {"the v2 export", string(export)}}, []secret{seed}); len(got) != 2 {
		t.Errorf("seed reveals %q, want the RoundSettled frame and the v2 export: the documented public reveal", got)
	}
	unrevealed := seedSecret("the unrevealed seed of round "+next, p.obs.seed(parseRoundID(t, next)))
	seeds := []secret{seedSecret("the seed of round "+missed, p.obs.seed(missedID)), seed, unrevealed}

	requireCleanStop(t, gw)
	requireCleanStop(t, restarted)
	if err := os.WriteFile(decoyFile, []byte("lifecycle: shutting down seed="+unrevealed.forms[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, err := decoy.stop(); err != nil || code != 0 {
		t.Fatalf("decoy stop = %d, %v", code, err)
	}
	if got := findLeaks([]output{{"decoy", decoy.log.String()}}, seeds); len(got) != 1 || !strings.Contains(got[0], unrevealed.name) {
		t.Fatalf("leak check of a log that printed the unrevealed seed at shutdown found %q, want that seed", got)
	}
	p.requireNoSecretsInOutput(seeds...)
	reconnects, repeats := stream.stats()
	t.Logf("missed round %s (%+v); round %s paid %d; the stream reconnected %d times across the restart with %d repeated frames; next round %s",
		missed, attempts[0], round, payout, reconnects, repeats, next)
}

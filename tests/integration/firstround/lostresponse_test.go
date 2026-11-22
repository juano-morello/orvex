//go:build integration && acceptance

package firstround

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// The controlled round: bets fit easily in the betting window, and the
// crash deadline D leaves seconds between RoundStarted reaching a client
// and the crash, so the cash-out is eligible on every run.
const (
	controlledWindow = 5 * time.Second
	controlledMinD   = 4 * time.Second
	controlledMaxD   = 10 * time.Second
	controlledWait   = controlledWindow + controlledMaxD + time.Minute
	// cashOutAfter puts the cash-out where the multiplier pays more than
	// the stake, about 1.03x, far before the crash deadline.
	cashOutAfter = 500 * time.Millisecond
)

type controlledRound struct {
	id   [16]byte
	text string
	seed [32]byte
	// d is the crash deadline offset D in microseconds.
	d int64
}

// createControlledRound creates the round in the test process, sealed with
// the test keyring exactly as the engine seals its own, with a seed whose D
// lies in [minD, maxD]. The services never receive a seed; the engine
// adopts the round from the database.
func createControlledRound(t *testing.T, p *platform, minD, maxD time.Duration) controlledRound {
	t.Helper()
	id := randomID(t)
	seed := roundtest.SeedWithDeadline(t, id, minD, maxD)
	cfg := v1.Config{RoundID: id, Seed: seed, MinBetCredits: 1, MaxBetCredits: 1_000_000_000, MaxBets: 100}
	state, err := v1.Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	x := roundtest.NewExecutor(t, p.obs.db, nil, nil)
	if _, err := x.CreateRound(context.Background(), execution.NewRound{
		RoundID: id, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config: cfg, CreatedAtUnixMicro: p.obs.now(),
	}); err != nil {
		t.Fatalf("CreateRound: %v", err)
	}
	return controlledRound{id: id, text: string(canonjson.UUID(id)), seed: seed, d: state.CrashDeadlineOffsetMicros}
}

// requireAdmission requires the receipt to name a stored admission of want
// at the receipt's sequence and effective time.
func requireAdmission(t *testing.T, o observer, round [16]byte, r receipt, want v1.Command) {
	t.Helper()
	for _, a := range o.admissions(round) {
		if a.Sequence != int64(r.Sequence) {
			continue
		}
		if a.Command != want || a.EffectiveTime != int64(r.EffectiveTimeUnixMicro) {
			t.Fatalf("admission %d is %+v at %d, receipt says %+v at %d", a.Sequence, a.Command, a.EffectiveTime, want, r.EffectiveTimeUnixMicro)
		}
		return
	}
	t.Fatalf("no admission at sequence %d for receipt %+v", r.Sequence, r)
}

func countAdmissions(as []admitted, want v1.Command) int {
	n := 0
	for _, a := range as {
		if a.Command == want {
			n++
		}
	}
	return n
}

// requireFramesMatch requires a client's stream to hold every chain record
// of the round, in order, with the same events.
func requireFramesMatch(t *testing.T, who string, frames []frame, records []storedRecord) {
	t.Helper()
	if len(frames) != len(records) {
		t.Fatalf("%s saw %d frames, the round has %d records; frames:\n%s", who, len(frames), len(records), describeFrames(frames))
	}
	for i, f := range frames {
		if want := records[i].types(); !slices.Equal(f.types(), want) {
			t.Errorf("%s frame %d events %v, record %d events %v", who, f.Sequence, f.types(), records[i].Sequence, want)
		}
	}
	if last := frames[len(frames)-1]; !last.has("RoundSettled") {
		t.Errorf("%s's last frame %d has events %v, want RoundSettled before end", who, last.Sequence, last.types())
	}
}

// TestLostCashOutResponsePaysOnce plays a two-player round through the
// gateway. alice's cash-out is admitted, its response is lost on the way
// to her, and she resubmits the identical request after settlement. The
// retry returns the original admission, the payout lands exactly once, and
// the settled round verifies as an explicit orvex/audit-history/v2 export.
// Along the way it checks that identity comes only from the bearer token.
func TestLostCashOutResponsePaysOnce(t *testing.T) {
	p := newPlatform(t, "alice", "bob")
	p.grant("acceptance-alice-1", "alice", 1000)
	p.grant("acceptance-bob-1", "bob", 1000)

	engine := p.startEngine(engineConfig{createRounds: false, bettingWindow: controlledWindow})
	gw := p.startGateway(engine.addr)
	alice, bob := p.player("alice", gw), p.player("bob", gw)
	cashOutID := newUUID(t)
	proxy := newLossyProxy(t, alice.base, cashOutID)
	aliceViaProxy := alice.via(proxy.url)

	r := createControlledRound(t, p, controlledMinD, controlledMaxD)
	alice.waitCurrentRound(t, 30*time.Second, func(id string) bool { return id == r.text })
	aliceStream, bobStream := watch(t, alice, r.text), watch(t, bob, r.text)

	betID := newUUID(t)
	aliceBet := aliceViaProxy.admit(t, r.text, placeBetBody(t, betID, 100))
	bobBet := bob.admit(t, r.text, placeBetBody(t, betID, 200))
	if aliceBet.Duplicate || bobBet.Duplicate || aliceBet.Sequence == bobBet.Sequence {
		t.Fatalf("one clientCommandId from two players: alice %+v, bob %+v; want two separate admissions", aliceBet, bobBet)
	}
	requireAdmission(t, p.obs, r.id, aliceBet, v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 100})
	requireAdmission(t, p.obs, r.id, bobBet, v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "bob", BetCredits: 200})
	for _, h := range []struct {
		r    receipt
		want int64
	}{{aliceBet, 100}, {bobBet, 200}} {
		if got := p.obs.int(`SELECT coalesce(sum(amount), 0) FROM orvex.ledger_holds WHERE round_id = $1 AND sequence = $2`, r.id, int64(h.r.Sequence)); got != h.want {
			t.Errorf("hold of the bet at sequence %d is %d, want %d", h.r.Sequence, got, h.want)
		}
	}

	anonymous := newPlayer("anonymous", "", alice.base)
	anonymous.requireRefused(t, r.text, placeBetBody(t, newUUID(t), 100), http.StatusUnauthorized, "")
	stranger := newPlayer("stranger", randomHex(t), alice.base)
	stranger.requireRefused(t, r.text, placeBetBody(t, newUUID(t), 100), http.StatusUnauthorized, "")
	impersonation := []byte(fmt.Sprintf(`{"clientCommandId":%q,"placeBet":{"betCredits":"100"},"player":"bob"}`, newUUID(t)))
	alice.requireRefused(t, r.text, impersonation, http.StatusBadRequest, "")
	if n := len(p.obs.admissions(r.id)); n != 2 {
		t.Fatalf("%d admissions after the refused requests, want only the 2 bets", n)
	}
	for _, c := range []struct {
		p    *player
		want receipt
	}{{alice, aliceBet}, {bob, bobBet}} {
		if status, s := c.p.lookup(t, betID); status != http.StatusOK || s.Sequence != c.want.Sequence {
			t.Errorf("%s: lookup of the shared clientCommandId = %d %+v, want 200 with sequence %d", c.p.name, status, s, c.want.Sequence)
		}
	}

	aliceStream.waitEvent(t, "RoundStarted", controlledWait)
	var t0 int64
	for _, rec := range p.obs.records(r.id) {
		if _, ok := rec.Command.(v1.StartRound); ok {
			t0 = rec.Record.EffectiveTimeUnixMicro
		}
	}
	if t0 == 0 {
		t.Fatal("RoundStarted streamed but no StartRound record is stored")
	}
	preReveal := engine.log.String() + gw.log.String()
	if p.obs.settled(r.id) {
		t.Fatal("round settled right after it started")
	}
	p.obs.waitClock(t0 + cashOutAfter.Microseconds())

	cashOut := cashOutBody(t, cashOutID)
	if status, b, err := aliceViaProxy.call(http.MethodPost, "/v1/rounds/"+r.text+"/commands", cashOut); err == nil {
		t.Fatalf("cash-out answered %d %s through the lossy proxy; want the response lost", status, b)
	}
	lostBody, forwarded := proxy.lostResponse()
	if lostBody == nil || forwarded != 1 {
		t.Fatalf("proxy forwarded %d cash-out requests and lost %q; want one 202 lost", forwarded, lostBody)
	}
	lost := decodeReceipt(t, "lost response", r.text, lostBody)
	if lost.Duplicate {
		t.Fatalf("first cash-out receipt %+v is a duplicate", lost)
	}
	aliceCashOut := v1.CashOut{Authority: v1.AuthorityPlayer, Player: "alice"}
	requireAdmission(t, p.obs, r.id, lost, aliceCashOut)
	if e := int64(lost.EffectiveTimeUnixMicro) - t0; e <= 0 || e >= r.d {
		t.Fatalf("cash-out effective %dµs after T0 with D = %dµs: the controlled round left no eligibility margin", e, r.d)
	}

	bobStream.waitEvent(t, "RoundSettled", controlledWait)
	retry := aliceViaProxy.admit(t, r.text, cashOut)
	if !retry.Duplicate || retry.Sequence != lost.Sequence || retry.EffectiveTimeUnixMicro != lost.EffectiveTimeUnixMicro {
		t.Fatalf("retry after settlement = %+v, want the lost admission %+v marked duplicate", retry, lost)
	}
	if _, n := proxy.lostResponse(); n != 2 {
		t.Errorf("proxy forwarded %d cash-out requests, want the lost one and one retry", n)
	}
	if n := countAdmissions(p.obs.admissions(r.id), aliceCashOut); n != 1 {
		t.Fatalf("%d cash-out admissions for alice, want 1", n)
	}
	aliceViaProxy.requireRefused(t, r.text, placeBetBody(t, cashOutID, 100), http.StatusConflict, "command_id_reused")

	status, outcome := alice.lookup(t, cashOutID)
	if status != http.StatusOK || outcome.Status != "accepted" || outcome.Sequence != lost.Sequence {
		t.Fatalf("lookup of the cash-out = %d %+v, want 200 accepted at sequence %d", status, outcome, lost.Sequence)
	}
	var payout int64
	for _, e := range outcome.Events {
		if e.Type == "BetCashedOut" && e.PlayerID == "alice" {
			payout = int64(e.PayoutCredits)
		}
	}
	if payout <= 100 {
		t.Fatalf("lookup events %+v, want alice's BetCashedOut paying more than her bet of 100", outcome.Events)
	}
	if status, _ := bob.lookup(t, cashOutID); status != http.StatusNotFound {
		t.Errorf("bob's lookup of alice's cash-out = %d, want 404", status)
	}

	aliceFrames := aliceStream.waitEnd(t, controlledWait)
	bobFrames := bobStream.waitEnd(t, controlledWait)
	records := p.obs.records(r.id)
	requireFramesMatch(t, "alice", aliceFrames, records)
	requireFramesMatch(t, "bob", bobFrames, records)
	commitment := v1.Commitment(r.seed)
	if !strings.Contains(aliceFrames[0].Data, hex.EncodeToString(commitment[:])) {
		t.Errorf("genesis frame %s lacks the commitment %x", aliceFrames[0].Data, commitment)
	}
	seedHex := hex.EncodeToString(r.seed[:])
	for _, f := range aliceFrames {
		if f.has("RoundSettled") {
			if !strings.Contains(f.Data, seedHex) {
				t.Errorf("RoundSettled frame %s does not reveal the seed", f.Data)
			}
			break
		}
		if strings.Contains(f.Data, seedHex) {
			t.Errorf("frame %d carries the seed before settlement", f.Sequence)
		}
	}
	for _, f := range aliceFrames {
		for _, e := range f.Events {
			if e.Type == "BetCashedOut" && (e.PlayerID != "alice" || int64(e.PayoutCredits) != payout || f.Sequence != int64(lost.Sequence)) {
				t.Errorf("frame %d BetCashedOut %+v, want alice paid %d at sequence %d", f.Sequence, e, payout, lost.Sequence)
			}
		}
	}

	var cashedOut []v1.BetCashedOut
	var betsLost []v1.BetLost
	lifecycle := map[string]int{}
	for _, rec := range records[1:] {
		if rec.Record.Result.Rejected {
			t.Errorf("record %d %+v rejected: %s", rec.Sequence, rec.Command, rec.Record.Result.Reason)
		}
		switch rec.Command.(type) {
		case v1.StartRound, v1.CloseRound, v1.SettleRound:
			lifecycle[fmt.Sprintf("%T", rec.Command)]++
		}
		for _, e := range rec.Events {
			switch e := e.(type) {
			case v1.BetCashedOut:
				cashedOut = append(cashedOut, e)
				if rec.Sequence != int64(lost.Sequence) || e.CashedOutAtUnixMicro != int64(lost.EffectiveTimeUnixMicro) {
					t.Errorf("BetCashedOut at record %d effective %d, want the admission at %d effective %d", rec.Sequence, e.CashedOutAtUnixMicro, lost.Sequence, lost.EffectiveTimeUnixMicro)
				}
			case v1.BetLost:
				betsLost = append(betsLost, e)
			}
		}
	}
	if len(cashedOut) != 1 || cashedOut[0].Player != "alice" || cashedOut[0].PayoutCredits != payout {
		t.Errorf("BetCashedOut events %+v, want exactly one paying alice %d", cashedOut, payout)
	}
	if len(betsLost) != 1 || betsLost[0] != (v1.BetLost{Player: "bob", BetCredits: 200}) {
		t.Errorf("BetLost events %+v, want bob's 200", betsLost)
	}
	for _, c := range []string{"v1.StartRound", "v1.CloseRound", "v1.SettleRound"} {
		if lifecycle[c] != 1 {
			t.Errorf("%d %s records, want 1", lifecycle[c], c)
		}
	}
	admissions := p.obs.admissions(r.id)
	if len(admissions) != len(records)-1 || len(admissions) != 6 {
		t.Errorf("%d admissions and %d command records, want 6 of each: 2 bets, 1 cash-out, start, close, settle", len(admissions), len(records)-1)
	}
	if got := p.obs.payoutsTo(r.id, "alice"); !slices.Equal(got, []int64{payout}) {
		t.Errorf("round postings crediting alice %v, want exactly one of %d", got, payout)
	}
	if got := p.obs.payoutsTo(r.id, "bob"); len(got) != 0 {
		t.Errorf("round postings crediting bob %v, want none", got)
	}
	if !p.obs.settled(r.id) || p.obs.admissionState(r.id) != "sealed" {
		t.Errorf("round revealed %v, admission state %q; want revealed and sealed", p.obs.settled(r.id), p.obs.admissionState(r.id))
	}
	p.obs.requirePlayer("alice", 1000-100+payout, 0)
	p.obs.requirePlayer("bob", 1000-200, 0)
	p.obs.requireSettledLedger([][16]byte{r.id}, 300-payout)

	report := p.auditV2(r.id, records)
	if want := "decision-replay ok commands 6 accepted 6 rejected 0 "; !strings.Contains(report, want) {
		t.Errorf("verify report:\n%s\nwant %q", report, want)
	}

	requireCleanStop(t, gw)
	requireCleanStop(t, engine)
	p.requireNoSecretsInOutput()
	if strings.Contains(preReveal, seedHex) {
		t.Error("a service logged the round's seed before settlement")
	}
	reconnects, repeats := aliceStream.stats()
	t.Logf("round %s: T0 %d, D %dµs, cash-out at +%dµs paid %d; alice's stream reconnected %d times, %d repeated frames",
		r.text, t0, r.d, int64(lost.EffectiveTimeUnixMicro)-t0, payout, reconnects, repeats)
}

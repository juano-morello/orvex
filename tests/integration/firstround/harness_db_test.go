//go:build integration && acceptance

package firstround

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// TestHarnessObserverAndAudit plays a controlled round with the in-process
// sequencer and executor, then runs the scenarios' database checks and the
// v2 audit on it: the observer and the auditor role work before any
// service exists.
func TestHarnessObserverAndAudit(t *testing.T) {
	ctx := context.Background()
	p := newPlatform(t, "alice", "bob")
	roundtest.FundPlayer(t, p.obs.db, "alice", 1000)
	roundtest.FundPlayer(t, p.obs.db, "bob", 1000)
	r := createControlledRound(t, p, 500*time.Millisecond, 2*time.Second)
	if p.obs.admissionState(r.id) != "open" || p.obs.settled(r.id) {
		t.Fatalf("new round: state %q, revealed %v", p.obs.admissionState(r.id), p.obs.settled(r.id))
	}

	s, err := admission.New(p.obs.db, roundtest.Registry(t), admission.DefaultConfig(), admission.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	x := roundtest.NewExecutor(t, p.obs.db, nil, nil)
	admit := func(cmd v1.Command) receipt {
		t.Helper()
		req := admission.Request{RoundID: r.id, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, CommandID: randomID(t), Command: cmd}
		for {
			a, err := s.Admit(ctx, req)
			if errors.Is(err, admission.ErrClosureNotDue) {
				p.obs.waitClock(p.obs.now() + 10_000)
				continue
			}
			if err != nil {
				t.Fatalf("Admit %+v: %v", cmd, err)
			}
			if _, err := x.Drain(ctx, execution.Lease{RoundID: r.id}, 100); err != nil {
				t.Fatalf("Drain: %v", err)
			}
			return receipt{RoundID: r.text, Sequence: wireInt(a.Sequence), EffectiveTimeUnixMicro: wireInt(a.EffectiveTimeUnixMicro)} //nolint:gosec // G115: admission sequences are small
		}
	}
	bet := admit(v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 100})
	requireAdmission(t, p.obs, r.id, bet, v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 100})
	admit(v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "bob", BetCredits: 200})
	start := admit(v1.StartRound{Authority: v1.AuthoritySystem})
	p.obs.waitClock(int64(start.EffectiveTimeUnixMicro) + 1)
	cashOut := admit(v1.CashOut{Authority: v1.AuthorityPlayer, Player: "alice"})
	admit(v1.CloseRound{Authority: v1.AuthoritySystem})
	admit(v1.SettleRound{Authority: v1.AuthoritySystem})

	records := p.obs.records(r.id)
	if len(records) != 7 || records[0].Genesis == nil || !slices.Equal(records[cashOut.Sequence].types(), []string{"BetCashedOut"}) {
		t.Fatalf("records %+v", records)
	}
	payout := records[cashOut.Sequence].Events[0].(v1.BetCashedOut).PayoutCredits
	if got := p.obs.payoutsTo(r.id, "alice"); !slices.Equal(got, []int64{payout}) {
		t.Errorf("payouts to alice %v, want [%d]", got, payout)
	}
	if n := countAdmissions(p.obs.admissions(r.id), v1.CashOut{Authority: v1.AuthorityPlayer, Player: "alice"}); n != 1 {
		t.Errorf("%d cash-out admissions", n)
	}
	if !p.obs.settled(r.id) || p.obs.admissionState(r.id) != "sealed" {
		t.Errorf("settled round: revealed %v, state %q", p.obs.settled(r.id), p.obs.admissionState(r.id))
	}
	p.obs.requirePlayer("alice", 1000-100+payout, 0)
	p.obs.requirePlayer("bob", 800, 0)
	p.obs.requireSettledLedger([][16]byte{r.id}, 300-payout)
	p.auditV2(r.id, records)
	p.requireNoSecretsInOutput()
}

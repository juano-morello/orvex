//go:build integration

package execution_test

import (
	"errors"
	"testing"
	"time"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// mustAdmit admits req and fails the test on error.
func mustAdmit(t *testing.T, s *admission.Sequencer, req admission.Request) admission.Admission {
	t.Helper()
	a, err := s.Admit(ctx, req)
	if err != nil {
		t.Fatalf("Admit %T: %v", req.Command, err)
	}
	return a
}

// mustDrain drains the round and requires n executions.
func mustDrain(t *testing.T, x *execution.Executor, round [16]byte, n int) {
	t.Helper()
	if got, err := x.Drain(ctx, lease(round), 100); err != nil || got != n {
		t.Fatalf("Drain = %d, %v; want %d", got, err, n)
	}
}

func TestAdmitAndExecuteRound(t *testing.T) {
	_, pool := newDB(t)
	x := newExecutor(t, pool, nil, nil)
	s := newSequencer(t, pool)
	round := newRoundID()
	cfg := createRound(t, x, round, seedWithDeadline(t, round, 500*time.Millisecond, 2*time.Second), 1_000_000_000, 100)
	initial, err := v1.Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := initial.CrashDeadlineOffsetMicros
	fundPlayer(t, pool, "alice", 100)
	fundPlayer(t, pool, "bob", 200)

	mustAdmit(t, s, request(round, cmdID(1), placeBet("alice", 100)))
	mustAdmit(t, s, request(round, cmdID(2), placeBet("bob", 200)))
	start := mustAdmit(t, s, request(round, cmdID(3), startRound))
	mustDrain(t, x, round, 3)
	t0 := start.EffectiveTimeUnixMicro

	waitDBClock(t, pool, t0+1)
	mustAdmit(t, s, request(round, cmdID(4), cashOut("alice")))
	out, err := x.ExecuteNext(ctx, lease(round))
	if err != nil || out.Result.Rejected {
		t.Fatalf("cash-out = %+v, %v; want accepted", out, err)
	}
	payout := out.Events[0].(v1.BetCashedOut).PayoutCredits
	if got := playerAccount(t, pool, "alice"); got.credits != 100+payout {
		t.Errorf("alice credits %d, want %d", got.credits, 100+payout)
	}

	// CloseRound: resubmit the same request until T0 + D is due.
	closeReq := request(round, cmdID(5), closeRound)
	if _, err := s.Admit(ctx, closeReq); err != nil {
		if !errors.Is(err, admission.ErrClosureNotDue) {
			t.Fatalf("CloseRound: %v", err)
		}
		waitDBClock(t, pool, t0+d)
		mustAdmit(t, s, closeReq)
	}
	out, err = x.ExecuteNext(ctx, lease(round))
	if err != nil || out.Result.Rejected {
		t.Fatalf("CloseRound = %+v, %v; want accepted", out, err)
	}
	if _, ok := out.Events[0].(v1.RoundCrashed); !ok {
		t.Fatalf("CloseRound events = %+v, want RoundCrashed", out.Events)
	}

	mustAdmit(t, s, request(round, cmdID(6), settleRound))
	out, err = x.ExecuteNext(ctx, lease(round))
	if err != nil || out.Result.Rejected || !out.Final {
		t.Fatalf("SettleRound = %+v, %v; want accepted and final", out, err)
	}

	if _, err := verifyExport(t, exportHistory(t, pool, round)); err != nil {
		t.Errorf("Verify(export): %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_accounts WHERE held <> 0`); n != 0 {
		t.Errorf("%d accounts still hold credits", n)
	}
	if got := accountByID(t, pool, ledger.RoundAccountID(round)); got.balance() != 0 {
		t.Errorf("round account balance %d, want 0", got.balance())
	}
	if got := accountByID(t, pool, ledger.HouseAccountID()); got.balance() != 300-payout {
		t.Errorf("house balance %d, want Σ bets − Σ payouts = %d", got.balance(), 300-payout)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.outbox_messages WHERE round_id = $1`, round); n != 7 {
		t.Errorf("%d outbox rows, want 7", n)
	}
	requireBalanced(t, pool)
}

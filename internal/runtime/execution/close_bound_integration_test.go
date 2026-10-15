//go:build integration

package execution_test

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

// TestCloseBoundRefusesEarlyCloseAndSettles is the engineer addendum's
// acceptance through CreateRound, the sequencer and the executor: an early
// CloseRound is refused and the window stays open, nothing leaks the
// offset, a CloseRound at exactly T0 + D is admitted, and the round settles.
func TestCloseBoundRefusesEarlyCloseAndSettles(t *testing.T) {
	_, pool := newDB(t)
	x := newExecutor(t, pool, nil, nil)
	s := newSequencer(t, pool)

	// 1. The round stores D; no bound yet.
	round := newRoundID()
	cfg := createRound(t, x, round, seedWithDeadline(t, round, 30*time.Second, 150*time.Second), 1_000_000_000, 10)
	initial, err := v1.Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := initial.CrashDeadlineOffsetMicros
	var offset *int64
	if err := pool.QueryRow(ctx, `SELECT close_offset_micros FROM orvex.round_secrets WHERE round_id = $1`, round).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	if offset == nil || *offset != d {
		t.Fatalf("close_offset_micros = %v, want D = %d", offset, d)
	}
	type head struct {
		admitted       int64
		state          string
		closed, sealed *int64
		bound          *int64
	}
	readHead := func() head {
		t.Helper()
		var h head
		if err := pool.QueryRow(ctx, `SELECT admitted_sequence, admission_state, closed_sequence, sealed_sequence, close_not_before_unix_micro
			FROM orvex.round_admission_heads WHERE round_id = $1`, round).Scan(&h.admitted, &h.state, &h.closed, &h.sealed, &h.bound); err != nil {
			t.Fatal(err)
		}
		return h
	}
	if h := readHead(); h.state != "open" || h.bound != nil {
		t.Fatalf("head = %+v, want open without a bound", h)
	}

	// 2. Bets and the start; the bound is T0 + D.
	fundPlayer(t, pool, "alice", 100)
	fundPlayer(t, pool, "bob", 200)
	mustAdmit(t, s, request(round, cmdID(1), placeBet("alice", 100)))
	mustAdmit(t, s, request(round, cmdID(2), placeBet("bob", 200)))
	start := mustAdmit(t, s, request(round, cmdID(3), startRound))
	t0 := start.EffectiveTimeUnixMicro
	if h := readHead(); h.bound == nil || *h.bound != t0+d {
		t.Fatalf("closure bound = %v, want T0 + D = %d", h.bound, t0+d)
	}
	mustDrain(t, x, round, 3)

	// 3. An early CloseRound is refused; the window stays open.
	_, closeErr := s.Admit(ctx, request(round, cmdID(90), closeRound))
	var nd *admission.ClosureNotDueError
	if !errors.As(closeErr, &nd) || !errors.Is(closeErr, admission.ErrClosureNotDue) {
		t.Fatalf("early CloseRound: err = %v, want ClosureNotDueError", closeErr)
	}
	if nd.EffectiveTimeUnixMicro < t0 || nd.EffectiveTimeUnixMicro >= t0+d {
		t.Errorf("not-due effective time %d outside [T0 %d, T0 + D)", nd.EffectiveTimeUnixMicro, t0)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.admissions WHERE command_id = $1`, cmdID(90)); n != 0 {
		t.Error("the refused CloseRound has an admission row")
	}
	if h := readHead(); h.state != "started" || h.closed != nil || h.admitted != 3 {
		t.Errorf("head = %+v, want started, admitted 3, not closed", h)
	}
	waitDBClock(t, pool, t0+1)
	mustAdmit(t, s, request(round, cmdID(4), cashOut("bob")))
	out, err := x.ExecuteNext(ctx, lease(round))
	if err != nil || out.Result.Rejected {
		t.Fatalf("CashOut bob = %+v, %v; want accepted", out, err)
	}
	bobPayout := out.Events[0].(v1.BetCashedOut).PayoutCredits
	if bobPayout < 200 {
		t.Errorf("bob's payout %d, want >= 200", bobPayout)
	}

	// 4. Nothing reveals D or T0 + D before the crash.
	qD := `"` + strconv.FormatInt(d, 10) + `"`
	qB := `"` + strconv.FormatInt(t0+d, 10) + `"`
	rows, err := pool.Query(ctx, `SELECT sequence, payload FROM orvex.outbox_messages WHERE round_id = $1 ORDER BY sequence`, round)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var seq int64
		var payload []byte
		if err := rows.Scan(&seq, &payload); err != nil {
			t.Fatal(err)
		}
		n++
		if bytes.Contains(payload, []byte(qD)) || bytes.Contains(payload, []byte(qB)) {
			t.Errorf("outbox payload %d reveals the close offset or bound", seq)
		}
	}
	rows.Close()
	if n != 5 {
		t.Errorf("%d outbox messages before the crash, want 5 (sequences 0-4)", n)
	}
	export := exportHistory(t, pool, round)
	if bytes.Contains(export, []byte(qD)) || bytes.Contains(export, []byte(qB)) {
		t.Error("the export before settlement reveals the close offset or bound")
	}
	for _, text := range []string{closeErr.Error(), fmt.Sprintf("%+v", closeErr)} {
		if strings.Contains(text, strconv.FormatInt(d, 10)) || strings.Contains(text, strconv.FormatInt(t0+d, 10)) {
			t.Errorf("error text %q reveals the close offset or bound", text)
		}
	}

	// 5. Closure at exactly T0 + D.
	if seq := seedAdmission(t, pool, round, cmdID(5), encode(t, cashOut("alice")), t0+d, nil); seq != 5 {
		t.Fatalf("seeded sequence %d, want 5", seq)
	}
	closed := mustAdmit(t, s, request(round, cmdID(6), closeRound))
	if closed.Sequence != 6 || closed.EffectiveTimeUnixMicro != t0+d {
		t.Fatalf("CloseRound = %+v, want sequence 6 at exactly T0 + D = %d", closed, t0+d)
	}
	if h := readHead(); h.state != "closed" || h.closed == nil || *h.closed != 6 {
		t.Errorf("head = %+v, want closed at 6", h)
	}
	if settle := mustAdmit(t, s, request(round, cmdID(7), settleRound)); settle.Sequence != 7 {
		t.Fatalf("SettleRound sequence %d, want 7", settle.Sequence)
	}
	if h := readHead(); h.state != "sealed" {
		t.Errorf("state = %s, want sealed", h.state)
	}

	// 6. The round settles normally.
	out, err = x.ExecuteNext(ctx, lease(round))
	if err != nil || !out.Result.Rejected || out.Result.Reason != string(v1.ReasonDeadlinePassed) {
		t.Fatalf("CashOut alice at T0 + D = %+v, %v; want rejected deadline_passed", out, err)
	}
	out, err = x.ExecuteNext(ctx, lease(round))
	if err != nil || out.Result.Rejected || len(out.Events) != 1 {
		t.Fatalf("CloseRound = %+v, %v; want accepted", out, err)
	}
	rc, ok := out.Events[0].(v1.RoundCrashed)
	if !ok || rc.ClosedAtUnixMicro != t0+d || rc.CrashDeadlineUnixMicro != t0+d {
		t.Errorf("RoundCrashed = %+v, want closed at and deadline T0 + D = %d", out.Events[0], t0+d)
	}
	out, err = x.ExecuteNext(ctx, lease(round))
	if err != nil || out.Result.Rejected || !out.Final || len(out.Events) != 2 {
		t.Fatalf("SettleRound = %+v, %v; want accepted and final", out, err)
	}
	if bl, ok := out.Events[0].(v1.BetLost); !ok || bl.Player != "alice" {
		t.Errorf("settlement event 0 = %+v, want BetLost alice", out.Events[0])
	}
	rs, ok := out.Events[1].(v1.RoundSettled)
	if !ok || rs.TotalBetCredits != 300 || rs.TotalPayoutCredits != bobPayout {
		t.Errorf("RoundSettled = %+v, want totals 300 / %d", out.Events[1], bobPayout)
	}
	for _, p := range []string{"alice", "bob"} {
		if a := playerAccount(t, pool, p); a.held != 0 {
			t.Errorf("%s held %d, want 0", p, a.held)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_hold_releases WHERE round_id = $1`, round); n != 2 {
		t.Errorf("%d releases, want 2 (one per hold)", n)
	}
	if got := accountByID(t, pool, ledger.RoundAccountID(round)); got.balance() != 0 {
		t.Errorf("round account balance %d, want 0", got.balance())
	}
	if got := accountByID(t, pool, ledger.HouseAccountID()); got.balance() != 300-bobPayout {
		t.Errorf("house balance %d, want %d", got.balance(), 300-bobPayout)
	}
	requireBalanced(t, pool)
	if _, err := verifyExport(t, exportHistory(t, pool, round)); err != nil {
		t.Errorf("Verify(export): %v", err)
	}
}

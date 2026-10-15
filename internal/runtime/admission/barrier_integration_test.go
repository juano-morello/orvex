//go:build integration

package admission_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

// windowAt drives a fresh round to state through Admit and returns the
// round and the start's effective time T0 (0 when open).
func windowAt(t *testing.T, e env, s *admission.Sequencer, state string) (round [16]byte, t0 int64) {
	t.Helper()
	round = newRoundID()
	createRound(t, e.pool, round, testSeed)
	if state == "open" {
		return round, 0
	}
	t0 = mustAdmit(t, s, request(round, freshID(), startRound)).EffectiveTimeUnixMicro
	if state == "started" {
		return round, t0
	}
	mustAdmit(t, s, closeRequest(round, freshID(), t0))
	if state == "closed" {
		return round, t0
	}
	mustAdmit(t, s, request(round, freshID(), settleRound))
	return round, t0
}

func TestWindowTransitions(t *testing.T) {
	type expect struct {
		state  string           // state after the command
		reason admission.Reason // "" means admitted
	}
	admit := func(state string) expect { return expect{state: state} }
	reject := func(state string, r admission.Reason) expect { return expect{state: state, reason: r} }
	classes := []string{"ordinary", "start", "close", "settle"}
	table := map[string][4]expect{
		"open": {admit("open"), admit("started"),
			reject("open", admission.ReasonLifecycleOrder), reject("open", admission.ReasonLifecycleOrder)},
		"started": {admit("started"), reject("started", admission.ReasonLifecycleOrder),
			admit("closed"), reject("started", admission.ReasonLifecycleOrder)},
		"closed": {reject("closed", admission.ReasonRoundClosed), reject("closed", admission.ReasonLifecycleOrder),
			reject("closed", admission.ReasonLifecycleOrder), admit("sealed")},
		"sealed": {reject("sealed", admission.ReasonRoundSealed), reject("sealed", admission.ReasonRoundSealed),
			reject("sealed", admission.ReasonRoundSealed), reject("sealed", admission.ReasonRoundSealed)},
	}
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	for _, from := range []string{"open", "started", "closed", "sealed"} {
		for ci, class := range classes {
			want := table[from][ci]
			t.Run(from+"/"+class, func(t *testing.T) {
				round, t0 := windowAt(t, e, s, from)
				before := head(t, e.obs, round)
				id := freshID()
				req := request(round, id, cashOut("alice"))
				switch class {
				case "start":
					req.Command = startRound
				case "close":
					req = closeRequest(round, id, max(t0, 1))
				case "settle":
					req.Command = settleRound
				}
				a, err := s.Admit(ctx, req)
				after := head(t, e.obs, round)
				if after.state != want.state {
					t.Errorf("state %s -> %s, want %s", from, after.state, want.state)
				}
				if want.reason != "" {
					requireRejected(t, err, want.reason)
					requireNoRow(t, e.obs, id)
					if after.admitted != before.admitted || !equalPtr(after.started, before.started) ||
						!equalPtr(after.closed, before.closed) || !equalPtr(after.sealed, before.sealed) {
						t.Errorf("head changed by a rejection: %+v -> %+v", before, after)
					}
					return
				}
				if err != nil {
					t.Fatalf("Admit: %v", err)
				}
				requireRow(t, e.obs, a)
				if after.admitted != int64(a.Sequence) {
					t.Errorf("admitted_sequence = %d, want %d", after.admitted, a.Sequence)
				}
				seq := int64(a.Sequence)
				wantStarted, wantClosed, wantSealed := before.started, before.closed, before.sealed
				switch class {
				case "start":
					wantStarted = &seq
				case "close":
					wantClosed = &seq
				case "settle":
					wantSealed = &seq
				}
				if !equalPtr(after.started, wantStarted) || !equalPtr(after.closed, wantClosed) || !equalPtr(after.sealed, wantSealed) {
					t.Errorf("window sequences = %v %v %v, want %v %v %v",
						show(after.started), show(after.closed), show(after.sealed),
						show(wantStarted), show(wantClosed), show(wantSealed))
				}
			})
		}
	}
}

func equalPtr(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func show(p *int64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

func TestBarrierRejectsLateCommands(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	fundPlayer(t, e.pool, "alice", 100)
	fundPlayer(t, e.pool, "carol", 100)

	mustAdmit(t, s, request(round, cmdID(1), placeBet("alice", 10)))
	start := mustAdmit(t, s, request(round, cmdID(2), startRound))
	mustAdmit(t, s, closeRequest(round, cmdID(3), start.EffectiveTimeUnixMicro))
	late := []v1.Command{cashOut("alice"), placeBet("carol", 10), v1.StartRound{Authority: v1.AuthorityPlayer}}
	for i, cmd := range late {
		_, err := s.Admit(ctx, request(round, cmdID(10+i), cmd))
		requireRejected(t, err, admission.ReasonRoundClosed)
		requireNoRow(t, e.obs, cmdID(10+i))
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.ledger_holds WHERE round_id = $1`, round); n != 1 {
		t.Errorf("%d hold rows, want 1 (alice's bet)", n)
	}
	if h := held(t, e.obs, "carol"); h != 0 {
		t.Errorf("carol held %d, want 0", h)
	}
	if h := head(t, e.obs, round); h.admitted != 3 || h.state != "closed" {
		t.Errorf("head = %+v, want admitted 3, closed", h)
	}
}

func TestAdmitDuplicateAfterWindowClosed(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)

	start := mustAdmit(t, s, request(round, cmdID(1), startRound))
	req := request(round, cmdID(2), cashOut("alice"))
	first := mustAdmit(t, s, req)
	mustAdmit(t, s, closeRequest(round, cmdID(3), start.EffectiveTimeUnixMicro))
	again := mustAdmit(t, s, req)
	mustAdmit(t, s, request(round, cmdID(4), settleRound))
	sealed := mustAdmit(t, s, req)
	for _, a := range []admission.Admission{again, sealed} {
		if !a.Duplicate || a.Sequence != first.Sequence || a.EffectiveTimeUnixMicro != first.EffectiveTimeUnixMicro {
			t.Errorf("resubmit = %+v, want the original %+v as duplicate", a, first)
		}
	}
}

func TestBarrierSealIsFinal(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round, t0 := windowAt(t, e, s, "sealed")
	for _, req := range []admission.Request{
		request(round, freshID(), cashOut("alice")),
		request(round, freshID(), startRound),
		closeRequest(round, freshID(), t0),
		request(round, freshID(), settleRound),
	} {
		_, err := s.Admit(ctx, req)
		requireRejected(t, err, admission.ReasonRoundSealed)
		requireNoRow(t, e.obs, req.CommandID)
	}
	h := head(t, e.obs, round)
	if h.state != "sealed" || h.sealed == nil || *h.sealed != h.admitted {
		t.Fatalf("head = %+v, want sealed with sealed_sequence = admitted_sequence", h)
	}

	// A raw bump of admitted_sequence after an inserted admission violates
	// the window check.
	b, err := v1.Adapter().EncodeCommand(cashOut("raw"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.BeginTx(ctx, postgres.ExecutionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
		VALUES ($1, $2, $3, $4, $5)`, round, h.admitted+1, cmdID(500), time.Now().UnixMicro(), b); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `UPDATE orvex.round_admission_heads SET admitted_sequence = $2 WHERE round_id = $1`, round, h.admitted+1)
	if code := postgres.SQLState(err); code != "23514" || constraintOf(err) != "round_admission_heads_window_check" {
		t.Errorf("err = %v, want 23514 round_admission_heads_window_check", err)
	}
}

func TestClosureNotBefore(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round, t0 := windowAt(t, e, s, "started")
	next := 1

	notBefore := time.Now().Add(time.Hour).UnixMicro()
	_, err := s.Admit(ctx, closeRequest(round, cmdID(next), notBefore))
	var nd *admission.ClosureNotDueError
	if !errors.As(err, &nd) || !errors.Is(err, admission.ErrClosureNotDue) {
		t.Fatalf("err = %v, want ClosureNotDueError", err)
	}
	if nd.NotBeforeUnixMicro != notBefore || nd.EffectiveTimeUnixMicro >= notBefore || nd.EffectiveTimeUnixMicro < t0 {
		t.Errorf("ClosureNotDueError = %+v, want effective time in [T0 %d, not-before %d)", *nd, t0, notBefore)
	}
	requireNoRow(t, e.obs, cmdID(next))
	if h := head(t, e.obs, round); h.state != "started" {
		t.Errorf("state = %s, want started", h.state)
	}

	if _, err := s.Admit(ctx, closeRequest(round, cmdID(next+1), t0-1)); !errors.Is(err, admission.ErrInvalidRequest) {
		t.Errorf("not-before T0-1: err = %v, want ErrInvalidRequest", err)
	}
	requireNoRow(t, e.obs, cmdID(next+1))

	a := mustAdmit(t, s, closeRequest(round, cmdID(next+2), t0))
	requireRow(t, e.obs, a)
	if h := head(t, e.obs, round); h.state != "closed" || h.closed == nil || *h.closed != int64(a.Sequence) {
		t.Errorf("head = %+v, want closed at %d", h, a.Sequence)
	}
}

func TestClosureNotBeforeEqualityAdmits(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round, _ := windowAt(t, e, s, "started")
	next := 1
	x := time.Now().Add(time.Hour).UnixMicro()
	seedAdmission(t, e.pool, round, cmdID(next), cashOut("zed"), x)

	_, err := s.Admit(ctx, closeRequest(round, cmdID(next+1), x+1))
	var nd *admission.ClosureNotDueError
	if !errors.As(err, &nd) || nd.EffectiveTimeUnixMicro != x || nd.NotBeforeUnixMicro != x+1 {
		t.Fatalf("err = %v (%+v), want ClosureNotDueError with effective time %d", err, nd, x)
	}
	a := mustAdmit(t, s, closeRequest(round, cmdID(next+2), x))
	if a.EffectiveTimeUnixMicro != x {
		t.Errorf("close effective time = %d, want exactly %d", a.EffectiveTimeUnixMicro, x)
	}
	requireRow(t, e.obs, a)
}

func TestBarrierConcurrentCloseAndCashOuts(t *testing.T) {
	e := newEnv(t, 16)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	start := mustAdmit(t, s, request(round, cmdID(1), startRound))

	const goroutines = 32
	acks := make([][]admission.Admission, goroutines)
	rejections := make([]int, goroutines)
	lastRejected := make([]bool, goroutines)
	var errs errorList
	done := make(chan struct{})
	go func() {
		defer close(done)
		together(goroutines, func(g int) {
			for i := 0; ; i++ {
				a, err := s.Admit(ctx, request(round, cmdID(100_000+g*10_000+i), cashOut(fmt.Sprintf("p%d", g))))
				var re *admission.RejectedError
				switch {
				case err == nil:
					acks[g] = append(acks[g], a)
				case errors.As(err, &re) && re.Reason == admission.ReasonRoundClosed:
					rejections[g]++
					lastRejected[g] = true
					return
				default:
					errs.add("goroutine %d submission %d: %v", g, i, err)
					return
				}
				if i >= 9_999 {
					errs.add("goroutine %d: no round_closed after %d submissions", g, i+1)
					return
				}
			}
		})
	}()

	poll(t, "admitted_sequence >= 200", func() bool { return head(t, e.obs, round).admitted >= 200 })
	closed := mustAdmit(t, s, closeRequest(round, cmdID(2), start.EffectiveTimeUnixMicro))
	c := closed.Sequence
	<-done
	errs.report(t)

	for g := range goroutines {
		if rejections[g] != 1 || !lastRejected[g] {
			t.Errorf("goroutine %d: %d rejections, last rejected %v; want exactly one round_closed, last", g, rejections[g], lastRejected[g])
		}
		for i, a := range acks[g] {
			if a.Sequence >= c {
				t.Errorf("goroutine %d: cash-out admitted at %d, not before the close at %d", g, a.Sequence, c)
			}
			if i > 0 && a.Sequence <= acks[g][i-1].Sequence {
				t.Errorf("goroutine %d: sequence %d after %d", g, a.Sequence, acks[g][i-1].Sequence)
			}
			if a.EffectiveTimeUnixMicro > closed.EffectiveTimeUnixMicro {
				t.Errorf("goroutine %d: cash-out time %d after the close's %d", g, a.EffectiveTimeUnixMicro, closed.EffectiveTimeUnixMicro)
			}
			requireRow(t, e.obs, a)
		}
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions WHERE round_id = $1 AND sequence > $2`, round, int64(c)); n != 0 {
		t.Errorf("%d admissions after the close", n)
	}
	if h := head(t, e.obs, round); h.admitted != int64(c) || h.state != "closed" {
		t.Errorf("head = %+v, want admitted %d, closed", h, c)
	}
	requireGaplessMonotonic(t, e.obs, round, int(c))

	settled := mustAdmit(t, s, request(round, cmdID(3), settleRound))
	if settled.Sequence != c+1 {
		t.Errorf("SettleRound sequence = %d, want %d", settled.Sequence, c+1)
	}
	if h := head(t, e.obs, round); h.state != "sealed" {
		t.Errorf("state = %s, want sealed", h.state)
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions WHERE round_id = $1`, round); n != int64(c+1) {
		t.Errorf("%d admissions, want %d", n, c+1)
	}
}

func TestAdmissionStreamDrivesCrashRound(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	_, state := createRound(t, e.pool, workedRoundID, workedSeed)
	d := state.CrashDeadlineOffsetMicros
	if d != 9_410_000 {
		t.Fatalf("D = %d, want 9410000 for the worked round", d)
	}
	fundPlayer(t, e.pool, "alice", 1000)
	fundPlayer(t, e.pool, "bob", 1000)
	round := workedRoundID
	expectSeq := func(a admission.Admission, seq uint64) {
		t.Helper()
		if a.Sequence != seq || a.Duplicate {
			t.Fatalf("Admission = %+v, want sequence %d", a, seq)
		}
	}

	expectSeq(mustAdmit(t, s, request(round, cmdID(1), placeBet("alice", 100))), 1)
	expectSeq(mustAdmit(t, s, request(round, cmdID(2), placeBet("bob", 100))), 2)
	start := mustAdmit(t, s, request(round, cmdID(3), startRound))
	expectSeq(start, 3)
	t0 := start.EffectiveTimeUnixMicro
	deadline := t0 + d
	expectSeq(mustAdmit(t, s, request(round, cmdID(4), cashOut("alice"))), 4)
	if seq := seedAdmission(t, e.pool, round, cmdID(5), cashOut("zed"), deadline); seq != 5 {
		t.Fatalf("seeded sequence %d, want 5", seq)
	}
	bob := mustAdmit(t, s, request(round, cmdID(6), cashOut("bob")))
	expectSeq(bob, 6)
	if bob.EffectiveTimeUnixMicro != deadline {
		t.Fatalf("CashOut bob effective time = %d, want exactly T0 + D = %d", bob.EffectiveTimeUnixMicro, deadline)
	}
	_, err := s.Admit(ctx, closeRequest(round, cmdID(7), deadline+1))
	var nd *admission.ClosureNotDueError
	if !errors.As(err, &nd) || nd.EffectiveTimeUnixMicro != deadline {
		t.Fatalf("CloseRound at T0+D+1: err = %v, want ClosureNotDueError at %d", err, deadline)
	}
	closed := mustAdmit(t, s, closeRequest(round, cmdID(8), deadline))
	expectSeq(closed, 7)
	if closed.EffectiveTimeUnixMicro != deadline {
		t.Fatalf("CloseRound effective time = %d, want %d", closed.EffectiveTimeUnixMicro, deadline)
	}
	_, err = s.Admit(ctx, request(round, cmdID(9), cashOut("alice")))
	requireRejected(t, err, admission.ReasonRoundClosed)
	expectSeq(mustAdmit(t, s, request(round, cmdID(10), settleRound)), 8)
	_, err = s.Admit(ctx, request(round, cmdID(11), placeBet("carol", 10)))
	requireRejected(t, err, admission.ReasonRoundSealed)

	// Replay the admission stream through crash/v1 Decide and Evolve.
	adapter := v1.Adapter()
	var st any = state
	type result struct {
		accepted bool
		reason   string
		events   []any
	}
	results := map[int64]result{}
	rows := roundRows(t, e.obs, round)
	if len(rows) != 8 {
		t.Fatalf("%d admissions, want 8", len(rows))
	}
	for _, r := range rows {
		cmd, err := adapter.DecodeCommand(r.command)
		if err != nil {
			t.Fatalf("decode %d: %v", r.sequence, err)
		}
		events, err := adapter.Decide(st, cmd, game.Input{Sequence: uint64(r.sequence), EffectiveTimeUnixMicro: r.effTime})
		if err != nil {
			code, ok := adapter.RejectionCode(err)
			if !ok {
				t.Fatalf("sequence %d: fail-closed error %v", r.sequence, err)
			}
			results[r.sequence] = result{reason: code}
			continue
		}
		for _, ev := range events {
			if st, err = adapter.Evolve(st, ev); err != nil {
				t.Fatalf("sequence %d: Evolve: %v", r.sequence, err)
			}
		}
		results[r.sequence] = result{accepted: true, events: events}
	}

	for _, seq := range []int64{1, 2, 3} {
		if !results[seq].accepted {
			t.Errorf("sequence %d rejected %s, want accepted", seq, results[seq].reason)
		}
	}
	if !results[4].accepted {
		e4 := rows[3].effTime - t0
		t.Errorf("sequence 4 (CashOut alice) rejected %s with e = %d µs: the test needs e in (0, D); e <= 0 means two admissions in one microsecond, e >= D a 9.41 s stall",
			results[4].reason, e4)
	}
	if r := results[5]; r.accepted || r.reason != string(v1.ReasonNoBet) {
		t.Errorf("sequence 5 = %+v, want rejected no_bet", r)
	}
	if r := results[6]; r.accepted || r.reason != string(v1.ReasonDeadlinePassed) {
		t.Errorf("sequence 6 = %+v, want rejected deadline_passed (equality loses)", r)
	}
	r7 := results[7]
	if !r7.accepted {
		t.Errorf("sequence 7 (CloseRound) rejected %s, want accepted", r7.reason)
	} else {
		found := false
		for _, ev := range r7.events {
			if rc, ok := ev.(v1.RoundCrashed); ok {
				found = true
				if rc.CrashDeadlineUnixMicro != deadline {
					t.Errorf("RoundCrashed.CrashDeadlineUnixMicro = %d, want %d", rc.CrashDeadlineUnixMicro, deadline)
				}
			}
		}
		if !found {
			t.Error("sequence 7 has no RoundCrashed event")
		}
	}
	r8 := results[8]
	if !r8.accepted {
		t.Errorf("sequence 8 (SettleRound) rejected %s, want accepted", r8.reason)
	} else {
		found := false
		for _, ev := range r8.events {
			if bl, ok := ev.(v1.BetLost); ok && bl.Player == "bob" {
				found = true
			}
		}
		if !found {
			t.Error("sequence 8 has no BetLost{bob}")
		}
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.ledger_holds WHERE round_id = $1`, round); n != 2 {
		t.Errorf("%d hold rows, want 2 (alice and bob)", n)
	}
}

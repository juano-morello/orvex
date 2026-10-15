//go:build integration

package admission_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

func TestAdmitBasics(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)

	before := time.Now().UnixMicro()
	a := mustAdmit(t, s, request(round, cmdID(1), cashOut("alice")))
	if a.RoundID != round || a.CommandID != cmdID(1) || a.Sequence != 1 || a.Duplicate {
		t.Errorf("Admission = %+v, want round, command ID, sequence 1, not duplicate", a)
	}
	if a.EffectiveTimeUnixMicro < before-time.Minute.Microseconds() || a.EffectiveTimeUnixMicro > time.Now().UnixMicro()+time.Minute.Microseconds() {
		t.Errorf("effective time %d is not near the clock", a.EffectiveTimeUnixMicro)
	}
	requireRow(t, e.obs, a)
	r, _ := row(t, e.obs, a.CommandID)
	want, err := v1.Adapter().EncodeCommand(cashOut("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if string(r.command) != string(want) {
		t.Errorf("command bytes = %s, want the codec's %s", r.command, want)
	}
	if got := a.Input(); got != (game.Input{Sequence: 1, EffectiveTimeUnixMicro: a.EffectiveTimeUnixMicro}) {
		t.Errorf("Input() = %+v", got)
	}
	if h := head(t, e.obs, round); h.admitted != 1 || h.state != "open" {
		t.Errorf("head = %+v, want admitted 1, open", h)
	}
}

func TestAdmitRetryReturnsOriginal(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	fundPlayer(t, e.pool, "alice", 100)

	req := request(round, cmdID(1), placeBet("alice", 10))
	first := mustAdmit(t, s, req)
	again := mustAdmit(t, s, req)
	if !again.Duplicate || again.Sequence != first.Sequence || again.EffectiveTimeUnixMicro != first.EffectiveTimeUnixMicro ||
		again.RoundID != first.RoundID || again.CommandID != first.CommandID {
		t.Errorf("resubmit = %+v, want the original %+v with Duplicate", again, first)
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.ledger_holds WHERE round_id = $1`, round); n != 1 {
		t.Errorf("%d hold rows, want 1", n)
	}
	if h := held(t, e.obs, "alice"); h != 10 {
		t.Errorf("alice held %d, want 10", h)
	}
	if h := head(t, e.obs, round); h.admitted != 1 {
		t.Errorf("admitted_sequence = %d, want 1", h.admitted)
	}
}

func TestAdmitCommandIDReused(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	roundA, roundB := newRoundID(), newRoundID()
	createRound(t, e.pool, roundA, testSeed)
	createRound(t, e.pool, roundB, testSeed)
	fundPlayer(t, e.pool, "alice", 100)

	a := mustAdmit(t, s, request(roundA, cmdID(1), cashOut("alice")))
	if _, err := s.Admit(ctx, request(roundA, cmdID(1), placeBet("alice", 5))); !errors.Is(err, admission.ErrCommandIDReused) {
		t.Errorf("same ID, other bytes: err = %v, want ErrCommandIDReused", err)
	}
	if _, err := s.Admit(ctx, request(roundB, cmdID(1), cashOut("alice"))); !errors.Is(err, admission.ErrCommandIDReused) {
		t.Errorf("same bytes, other round: err = %v, want ErrCommandIDReused", err)
	}
	requireRow(t, e.obs, a)
	if h := head(t, e.obs, roundA); h.admitted != 1 {
		t.Errorf("round A admitted_sequence = %d, want 1", h.admitted)
	}
	if h := head(t, e.obs, roundB); h.admitted != 0 {
		t.Errorf("round B admitted_sequence = %d, want 0", h.admitted)
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.ledger_holds`); n != 0 {
		t.Errorf("%d hold rows, want 0", n)
	}
}

func TestAdmitRoundNotFound(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	if _, err := s.Admit(ctx, request(newRoundID(), cmdID(1), cashOut("alice"))); !errors.Is(err, admission.ErrRoundNotFound) {
		t.Errorf("err = %v, want ErrRoundNotFound", err)
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions`); n != 0 {
		t.Errorf("%d admissions, want 0", n)
	}
}

func TestAdmitRulesMismatch(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRoundRules(t, e.pool, round, testSeed, 2)
	if _, err := s.Admit(ctx, request(round, cmdID(1), cashOut("alice"))); !errors.Is(err, admission.ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
	requireNoRow(t, e.obs, cmdID(1))
}

func TestAdmitHoldsCredits(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	fundPlayer(t, e.pool, "alice", 100)
	holds := func() int64 {
		return count(t, e.obs, `SELECT count(*) FROM orvex.ledger_holds WHERE round_id = $1`, round)
	}

	a := mustAdmit(t, s, request(round, cmdID(1), placeBet("alice", 60)))
	if h := held(t, e.obs, "alice"); h != 60 {
		t.Errorf("alice held %d, want 60", h)
	}
	var amount int64
	if err := e.obs.QueryRow(ctx, `SELECT h.amount FROM orvex.ledger_holds h
		JOIN orvex.ledger_accounts la ON la.account_id = h.account_id
		WHERE h.round_id = $1 AND h.sequence = $2 AND la.player_id = 'alice'`, round, int64(a.Sequence)).Scan(&amount); err != nil || amount != 60 {
		t.Errorf("hold of admission %d = %d (%v), want 60 on alice's account", a.Sequence, amount, err)
	}

	_, err := s.Admit(ctx, request(round, cmdID(2), placeBet("alice", 50)))
	requireRejected(t, err, admission.ReasonInsufficientCredits)
	requireNoRow(t, e.obs, cmdID(2))
	_, err = s.Admit(ctx, request(round, cmdID(3), placeBet("unfunded", 1)))
	requireRejected(t, err, admission.ReasonInsufficientCredits)
	requireNoRow(t, e.obs, cmdID(3))
	if next := mustAdmit(t, s, request(round, cmdID(4), cashOut("alice"))); next.Sequence != 2 {
		t.Errorf("next admission sequence = %d, want 2: a rejection consumes no sequence", next.Sequence)
	}

	noHold := []v1.Command{
		placeBet("alice", 0),
		placeBet("alice", -1),
		placeBet("alice", 1_000_000_001),
		v1.PlaceBet{Authority: v1.AuthoritySystem, Player: "alice", BetCredits: 10},
		placeBet("", 10),
	}
	for i, cmd := range noHold {
		mustAdmit(t, s, request(round, cmdID(10+i), cmd))
	}
	if n := holds(); n != 1 {
		t.Errorf("%d hold rows, want 1", n)
	}
	if h := held(t, e.obs, "alice"); h != 60 {
		t.Errorf("alice held %d, want 60", h)
	}
}

func TestAdmitRejectsNulPlayer(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	for i, cmd := range []v1.Command{placeBet("a\x00b", 10), cashOut("a\x00b")} {
		if _, err := s.Admit(ctx, request(round, cmdID(i+1), cmd)); !errors.Is(err, admission.ErrInvalidCommand) {
			t.Errorf("%T: err = %v, want ErrInvalidCommand", cmd, err)
		}
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions`); n != 0 {
		t.Errorf("%d admissions, want 0", n)
	}
	if h := head(t, e.obs, round); h.admitted != 0 {
		t.Errorf("admitted_sequence = %d, want 0", h.admitted)
	}
}

// appendRecord writes the chain record of admission a with result r and no
// events, and advances the execution head to it, as LBT-12 would.
func appendRecord(t *testing.T, e env, a admission.Admission, prev chain.Hash, r chain.Result) chain.Hash {
	t.Helper()
	ar, ok := row(t, e.obs, a.CommandID)
	if !ok {
		t.Fatal("no admission row")
	}
	record, err := chain.EncodeCommand(chain.Command{
		RoundID: a.RoundID, Sequence: a.Sequence, PrevHash: prev, CommandID: a.CommandID,
		EffectiveTimeUnixMicro: a.EffectiveTimeUnixMicro, EngineBuild: "test", Payload: ar.command, Result: r,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := chain.LinkHash(record)
	status, reason := "accepted", (*string)(nil)
	if r.Rejected {
		status, reason = "rejected", &r.Reason
	}
	inTx(t, e.pool, func(tx pgx.Tx) {
		mustExec(t, tx, `UPDATE orvex.round_execution_heads SET processed_sequence = $2 WHERE round_id = $1`,
			a.RoundID, int64(a.Sequence))
		mustExec(t, tx, `INSERT INTO orvex.chain_records
			(round_id, sequence, prev_hash, hash, record, result_status, result_reason, fencing_token)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 0)`,
			a.RoundID, int64(a.Sequence), prev[:], hash[:], record, status, reason)
	})
	return hash
}

func TestLookup(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)

	if _, err := s.Lookup(ctx, cmdID(99)); !errors.Is(err, admission.ErrNotFound) {
		t.Errorf("unknown ID: err = %v, want ErrNotFound", err)
	}
	a := mustAdmit(t, s, request(round, cmdID(1), cashOut("alice")))
	st, err := s.Lookup(ctx, a.CommandID)
	if err != nil || st != (admission.Status{Admission: a, Outcome: admission.OutcomePending}) {
		t.Errorf("Lookup = %+v, %v; want pending %+v", st, err, a)
	}

	h1 := appendRecord(t, e, a, genesisHash(t, e.obs, round), chain.Result{Rejected: true, Reason: "no_bet"})
	st, err = s.Lookup(ctx, a.CommandID)
	if err != nil || st != (admission.Status{Admission: a, Outcome: admission.OutcomeRejected, Reason: "no_bet"}) {
		t.Errorf("Lookup = %+v, %v; want rejected no_bet", st, err)
	}

	b := mustAdmit(t, s, request(round, cmdID(2), cashOut("bob")))
	appendRecord(t, e, b, h1, chain.Result{})
	st, err = s.Lookup(ctx, b.CommandID)
	if err != nil || st != (admission.Status{Admission: b, Outcome: admission.OutcomeAccepted}) {
		t.Errorf("Lookup = %+v, %v; want accepted %+v", st, err, b)
	}
}

// holdHead locks round's admission head FOR UPDATE in a transaction on its
// own connection and returns that transaction.
func holdHead(t *testing.T, e env, round [16]byte) pgx.Tx {
	t.Helper()
	conn := connect(t, e.dsn)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `SELECT 1 FROM orvex.round_admission_heads WHERE round_id = $1 FOR UPDATE`, round); err != nil {
		t.Fatal(err)
	}
	return tx
}

type admitResult struct {
	a   admission.Admission
	err error
}

func admitAsync(s *admission.Sequencer, c context.Context, req admission.Request) <-chan admitResult {
	out := make(chan admitResult, 1)
	go func() {
		a, err := s.Admit(c, req)
		out <- admitResult{a, err}
	}()
	return out
}

func await(t *testing.T, ch <-chan admitResult) admitResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("Admit did not return within 30s")
		return admitResult{}
	}
}

func TestEffectiveTimeReadAfterLock(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)

	blocker := holdHead(t, e, round)
	res := admitAsync(s, ctx, request(round, cmdID(1), cashOut("alice")))
	waitLockWaiters(t, e.obs, 1)
	var r int64
	if err := blocker.QueryRow(ctx, `SELECT orvex.unix_micro(clock_timestamp())`).Scan(&r); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := await(t, res)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.a.EffectiveTimeUnixMicro < r {
		t.Errorf("effective time %d is before the blocker's release clock %d: not read after the lock", got.a.EffectiveTimeUnixMicro, r)
	}
	requireRow(t, e.obs, got.a)
}

func TestEffectiveTimeMonotonic(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)

	var prev int64
	for i := range 50 {
		a := mustAdmit(t, s, request(round, cmdID(i+1), cashOut("alice")))
		if a.Sequence != uint64(i+1) || a.EffectiveTimeUnixMicro < prev {
			t.Errorf("admission %d = %+v after time %d", i+1, a, prev)
		}
		prev = a.EffectiveTimeUnixMicro
	}
	x := time.Now().Add(time.Hour).UnixMicro()
	seq := seedAdmission(t, e.pool, round, cmdID(100), cashOut("zed"), x)
	for i := range 3 {
		a := mustAdmit(t, s, request(round, cmdID(101+i), cashOut("alice")))
		if a.EffectiveTimeUnixMicro != x || a.Sequence != seq+uint64(i)+1 {
			t.Errorf("admission after the seeded one = %+v, want time %d, sequence %d", a, x, seq+uint64(i)+1)
		}
	}
}

func TestAdmitLockTimeout(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, func(c *admission.Config) {
		c.LockTimeout, c.IdleInTransactionTimeout = 100*time.Millisecond, 50*time.Millisecond
	})
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)

	blocker := holdHead(t, e, round)
	req := request(round, cmdID(1), cashOut("alice"))
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := s.Admit(c, req)
	if !errors.Is(err, admission.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if code := postgres.SQLState(err); code != "55P03" {
		t.Errorf("SQLSTATE = %q, want 55P03", code)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("ErrBusy after %v, want within 2s", d)
	}
	requireNoRow(t, e.obs, cmdID(1))
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if a := mustAdmit(t, s, req); a.Sequence != 1 || a.Duplicate {
		t.Errorf("after the blocker: %+v, want sequence 1", a)
	}
}

// rawAdmit calls orvex.admit_command for an ordinary CashOut on q.
func rawAdmit(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, round, id [16]byte, lockMs, idleMs int32) string {
	t.Helper()
	b, err := v1.Adapter().EncodeCommand(cashOut("raw"))
	if err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := q.QueryRow(ctx, `SELECT outcome FROM orvex.admit_command($1, 'crash', 1, $2, $3, 'ordinary', NULL, NULL, $4, $5)`,
		round, id, b, lockMs, idleMs).Scan(&outcome); err != nil {
		t.Fatalf("admit_command: %v", err)
	}
	return outcome
}

func TestAbandonedAdmissionReleasesLock(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)

	raw := connect(t, e.dsn)
	tx, err := raw.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome := rawAdmit(t, tx, round, cmdID(1), 3000, 200); outcome != "admitted" {
		t.Fatalf("abandoned admit_command outcome = %q, want admitted", outcome)
	}
	// The raw session never commits; the server ends it after 200 ms idle.
	a := mustAdmit(t, s, request(round, cmdID(2), cashOut("alice")))
	if a.Sequence != 1 || a.Duplicate {
		t.Errorf("Admission = %+v, want sequence 1", a)
	}
	if _, err := tx.Exec(ctx, `SELECT 1`); err == nil {
		t.Error("the abandoned session's next statement succeeded, want its session terminated")
	}
	requireNoRow(t, e.obs, cmdID(1))
	requireRow(t, e.obs, a)
}

func TestAdmitCommandSessionSettings(t *testing.T) {
	e := newEnv(t, 0)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	show := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}, name string) string {
		t.Helper()
		var v string
		if err := q.QueryRow(ctx, "SHOW "+name).Scan(&v); err != nil {
			t.Fatalf("SHOW %s: %v", name, err)
		}
		return v
	}

	raw := connect(t, e.dsn)
	tx, err := raw.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome := rawAdmit(t, tx, round, cmdID(1), 1234, 567); outcome != "admitted" {
		t.Fatalf("outcome = %q, want admitted", outcome)
	}
	if v := show(tx, "lock_timeout"); v != "1234ms" {
		t.Errorf("lock_timeout = %q, want 1234ms", v)
	}
	if v := show(tx, "idle_in_transaction_session_timeout"); v != "567ms" {
		t.Errorf("idle_in_transaction_session_timeout = %q, want 567ms", v)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var name string
	if err := e.obs.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := e.obs.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" SET synchronous_commit = off"); err != nil {
		t.Fatal(err)
	}
	fresh := connect(t, e.dsn)
	if v := show(fresh, "synchronous_commit"); v != "off" {
		t.Fatalf("new session synchronous_commit = %q, want off", v)
	}
	tx, err = fresh.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome := rawAdmit(t, tx, round, cmdID(2), 1234, 567); outcome != "admitted" {
		t.Fatalf("outcome = %q, want admitted", outcome)
	}
	if v := show(tx, "synchronous_commit"); v != "on" {
		t.Errorf("synchronous_commit inside the admission = %q, want on", v)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for setting, want := range map[string]string{
		"lock_timeout":                        "0",
		"idle_in_transaction_session_timeout": "0",
		"synchronous_commit":                  "off",
	} {
		if v := show(fresh, setting); v != want {
			t.Errorf("after COMMIT %s = %q, want %q", setting, v, want)
		}
	}
}

func TestFastPathSkipsLock(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, func(c *admission.Config) { c.LockTimeout = 30 * time.Second })
	round := newRoundID()
	createRound(t, e.pool, round, instantSeed(t, round))

	mustAdmit(t, s, request(round, cmdID(1), startRound))
	admitted := request(round, cmdID(2), cashOut("alice"))
	first := mustAdmit(t, s, admitted)
	mustAdmit(t, s, closeRequest(round, cmdID(3)))

	holdHead(t, e, round)
	within := func(name string, fn func(c context.Context)) {
		t.Helper()
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		fn(c)
		if c.Err() != nil {
			t.Errorf("%s: ctx ended, so the call waited on the head lock", name)
		}
	}
	within("resubmit", func(c context.Context) {
		a, err := s.Admit(c, admitted)
		if err != nil || !a.Duplicate || a.Sequence != first.Sequence || a.EffectiveTimeUnixMicro != first.EffectiveTimeUnixMicro {
			t.Errorf("resubmit = %+v, %v; want the original %+v as duplicate", a, err, first)
		}
	})
	within("late cash-out", func(c context.Context) {
		_, err := s.Admit(c, request(round, cmdID(4), cashOut("bob")))
		requireRejected(t, err, admission.ReasonRoundClosed)
	})
	within("unknown round", func(c context.Context) {
		if _, err := s.Admit(c, request(newRoundID(), cmdID(5), cashOut("bob"))); !errors.Is(err, admission.ErrRoundNotFound) {
			t.Errorf("err = %v, want ErrRoundNotFound", err)
		}
	})
}

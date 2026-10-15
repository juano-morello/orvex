//go:build integration

package admission_test

import (
	"context"
	"errors"
	"testing"

	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

// commitKey is the advisory lock key the hold hook waits on.
const commitKey int64 = 0x0ec11

// holdCommits takes commitKey on its own connection, so the hold hook's
// COMMIT waits, and returns the function that releases it. Release
// promptly: the admission's lock_timeout also bounds the hook's wait.
func holdCommits(t *testing.T, e env) (release func()) {
	t.Helper()
	conn := connect(t, e.dsn)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, commitKey); err != nil {
		t.Fatal(err)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, commitKey); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdmitAckFollowsCommit(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	h := cmdID(1)
	installCommitHooks(t, e.obs, h, cmdID(999), commitKey)

	release := holdCommits(t, e)
	defer release()
	res := admitAsync(s, ctx, request(round, h, cashOut("alice")))
	waitAdvisoryWaiter(t, e.obs)

	// COMMIT is held: no acknowledgment, nothing visible.
	select {
	case r := <-res:
		t.Fatalf("Admit returned %+v, %v while its COMMIT is held", r.a, r.err)
	default:
	}
	requireNoRow(t, e.obs, h)
	if hd := head(t, e.obs, round); hd.admitted != 0 {
		t.Errorf("admitted_sequence = %d while COMMIT is held, want 0", hd.admitted)
	}
	if _, err := s.Lookup(ctx, h); !errors.Is(err, admission.ErrNotFound) {
		t.Errorf("Lookup while COMMIT is held: err = %v, want ErrNotFound", err)
	}
	release()

	got := await(t, res)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.a.Sequence != 1 || got.a.Duplicate {
		t.Errorf("Admission = %+v, want sequence 1, not duplicate", got.a)
	}
	requireRow(t, e.obs, got.a)
}

func TestAdmitNoAckWhenCommitFails(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	f := cmdID(1)
	installCommitHooks(t, e.obs, cmdID(999), f, commitKey)

	_, err := s.Admit(ctx, request(round, f, cashOut("alice")))
	if !errors.Is(err, admission.ErrInternal) || postgres.SQLState(err) != "23000" {
		t.Fatalf("err = %v, want ErrInternal with SQLSTATE 23000", err)
	}
	requireNoRow(t, e.obs, f)
	if hd := head(t, e.obs, round); hd.admitted != 0 {
		t.Errorf("admitted_sequence = %d, want 0", hd.admitted)
	}
	if a := mustAdmit(t, s, request(round, cmdID(2), cashOut("bob"))); a.Sequence != 1 {
		t.Errorf("next admission sequence = %d, want 1 (no gap)", a.Sequence)
	}
}

func TestAdmitAmbiguousCommitCommitted(t *testing.T) {
	e := newEnv(t, 0)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	fdb := &faultDB{db: e.pool, faults: []fault{faultCommitThenEOF}}
	s := newSequencer(t, fdb, nil)

	a := mustAdmit(t, s, request(round, cmdID(1), cashOut("alice")))
	if a.Sequence != 1 || !a.Duplicate {
		t.Errorf("Admission = %+v, want sequence 1 from the retry, duplicate", a)
	}
	if n := fdb.admissionAttempts(); n != 2 {
		t.Errorf("%d admission attempts, want 2", n)
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions WHERE round_id = $1`, round); n != 1 {
		t.Errorf("%d admissions, want 1", n)
	}
	requireRow(t, e.obs, a)
	st, err := s.Lookup(ctx, a.CommandID)
	want := a
	want.Duplicate = false
	if err != nil || st.Admission != want || st.Outcome != admission.OutcomePending {
		t.Errorf("Lookup = %+v, %v; want pending %+v", st, err, want)
	}
}

func TestAdmitAmbiguousCommitAborted(t *testing.T) {
	e := newEnv(t, 0)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	h := cmdID(1)
	installCommitHooks(t, e.obs, h, cmdID(999), commitKey)

	release := holdCommits(t, e)
	defer release()
	res := admitAsync(s, ctx, request(round, h, cashOut("alice")))
	pid := waitAdvisoryWaiter(t, e.obs)
	var terminated bool
	if err := e.obs.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v", pid, terminated, err)
	}
	release()

	got := await(t, res)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.a.Sequence != 1 || got.a.Duplicate {
		t.Errorf("Admission = %+v, want sequence 1 admitted by the retry", got.a)
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions WHERE round_id = $1`, round); n != 1 {
		t.Errorf("%d admissions, want 1", n)
	}
	requireRow(t, e.obs, got.a)
}

func TestAdmitOutcomeUnknownWhenContextEnds(t *testing.T) {
	e := newEnv(t, 0)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	fdb := &faultDB{db: e.pool, faults: []fault{faultCommitThenCancel}, cancel: cancel}
	s := newSequencer(t, fdb, nil)

	req := request(round, cmdID(1), cashOut("alice"))
	_, err := s.Admit(c, req)
	if !errors.Is(err, admission.ErrOutcomeUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrOutcomeUnknown and context.Canceled", err)
	}
	r, ok := row(t, e.obs, cmdID(1))
	if !ok {
		t.Fatal("no admission row: the commit happened")
	}
	a := mustAdmit(t, s, req)
	if !a.Duplicate || int64(a.Sequence) != r.sequence || a.EffectiveTimeUnixMicro != r.effTime {
		t.Errorf("resubmit = %+v, want the committed row (sequence %d) as duplicate", a, r.sequence)
	}
}

func TestAdmitRetriesConnectionLossBeforeCommit(t *testing.T) {
	e := newEnv(t, 0)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	fdb := &faultDB{db: e.pool, faults: []fault{faultCallConnLost}}
	s := newSequencer(t, fdb, nil)

	a := mustAdmit(t, s, request(round, cmdID(1), cashOut("alice")))
	if a.Duplicate || a.Sequence != 1 {
		t.Errorf("Admission = %+v, want sequence 1, not duplicate", a)
	}
	if n := fdb.admissionAttempts(); n != 2 {
		t.Errorf("%d admission attempts, want 2", n)
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions WHERE round_id = $1`, round); n != 1 {
		t.Errorf("%d admissions, want 1", n)
	}
	requireRow(t, e.obs, a)
}

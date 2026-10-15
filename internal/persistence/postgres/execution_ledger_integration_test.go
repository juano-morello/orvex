//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// Tests of migration 0008's schema objects and of Transaction B's options
// (LBT-12 spec E-6, E-11, §12 step 1).

func TestLedgerPostingsOnePerEvent(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "instant-crash")
	pot := mustAccount(t, pool, system("pot", &l.RoundID))
	house := mustAccount(t, pool, system("house", nil))

	var bet [16]byte
	mustCommit(t, pool, func(tx pgx.Tx) error {
		var err error
		if bet, err = ledgerTx(tx, "crash_bet", &l.RoundID, new(int64(1))); err != nil {
			return err
		}
		return post(tx, bet, 0, house, pot, 500, &l.RoundID, new(int64(1)))
	})
	requireStmtCode(t, pool, func(tx pgx.Tx) error {
		return post(tx, bet, 1, house, pot, 500, &l.RoundID, new(int64(1)))
	}, codeUniqueViolation, "ledger_postings_event_key")
	requireStmtCode(t, pool, func(tx pgx.Tx) error {
		settle, err := ledgerTx(tx, "crash_settlement", &l.RoundID, new(int64(5)))
		if err != nil {
			return err
		}
		return post(tx, settle, 0, pot, house, 500, &l.RoundID, new(int64(1)))
	}, codeUniqueViolation, "ledger_postings_event_key")

	// Postings without an event are unaffected.
	grant(t, pool, house, pot, 10)
	grant(t, pool, house, pot, 10)
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_postings`); n != 3 {
		t.Errorf("%d postings, want 3", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'orvex' AND indexname = 'ledger_postings_event_idx'`); n != 0 {
		t.Error("ledger_postings_event_idx still exists next to ledger_postings_event_key")
	}
}

func TestChainRecordsRequireHoldRelease(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	faucet := mustAccount(t, pool, system("faucet", nil))
	alice := mustAccount(t, pool, player("alice"))
	grant(t, pool, faucet, alice, 1000)

	r23 := rejectedRecord(l.RoundID, 23, l.Head)
	mustCommit(t, pool, func(tx pgx.Tx) error {
		if err := insertAdmission(tx, admissionOf(r23)); err != nil {
			return err
		}
		if err := hold(tx, l.RoundID, 23, alice, 100); err != nil {
			return err
		}
		return exec(tx, `UPDATE orvex.round_admission_heads SET admitted_sequence = 23 WHERE round_id = $1`, l.RoundID)
	})
	execute23 := func(withRelease bool) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			if err := exec(tx, `UPDATE orvex.round_execution_heads SET processed_sequence = 23 WHERE round_id = $1`, l.RoundID); err != nil {
				return err
			}
			if err := insertRecord(tx, rowOf(r23, 0)); err != nil {
				return err
			}
			if withRelease {
				return release(tx, l.RoundID, 23, alice)
			}
			return nil
		}
	}

	requireCommitCode(t, pool, execute23(false), postgres.CodeHoldUnreleased, "")
	if n := count(t, pool, `SELECT processed_sequence FROM orvex.round_execution_heads WHERE round_id = $1`, l.RoundID); n != 22 {
		t.Errorf("processed_sequence = %d after the refused commit, want 22", n)
	}
	if got := totalsOf(t, pool, alice); got != (totals{credits: 1000, held: 100}) {
		t.Errorf("alice totals = %+v, want the hold kept", got)
	}

	mustCommit(t, pool, execute23(true))
	if got := totalsOf(t, pool, alice); got != (totals{credits: 1000}) {
		t.Errorf("alice totals = %+v after the release, want no hold", got)
	}

	// An admission without a hold needs no release.
	mustCommit(t, pool, func(tx pgx.Tx) error {
		appendRejected(t, tx, l.RoundID, 24, chain.LinkHash(r23.bytes()), 0)
		return nil
	})
	if n := count(t, pool, `SELECT processed_sequence FROM orvex.round_execution_heads WHERE round_id = $1`, l.RoundID); n != 24 {
		t.Errorf("processed_sequence = %d, want 24", n)
	}
}

func TestExecutionTxOptionsWithTimeoutsApplied(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	setting := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}, name string) string {
		t.Helper()
		var v string
		if err := q.QueryRow(ctx, `SELECT current_setting($1)`, name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	sessionLock := setting(conn, "lock_timeout")
	sessionIdle := setting(conn, "idle_in_transaction_session_timeout")

	opts := postgres.ExecutionTxOptionsWithTimeouts(3*time.Second, time.Second)
	tx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"transaction_isolation":               "read committed",
		"transaction_read_only":               "off",
		"lock_timeout":                        "3s",
		"idle_in_transaction_session_timeout": "1s",
	} {
		if got := setting(tx, name); got != want {
			t.Errorf("%s = %q inside B, want %q", name, got, want)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := setting(conn, "lock_timeout"); got != sessionLock {
		t.Errorf("lock_timeout = %q after COMMIT, want the session's %q", got, sessionLock)
	}
	if got := setting(conn, "idle_in_transaction_session_timeout"); got != sessionIdle {
		t.Errorf("idle_in_transaction_session_timeout = %q after COMMIT, want the session's %q", got, sessionIdle)
	}

	for _, c := range []struct{ session, want string }{{"off", "on"}, {"remote_apply", "remote_apply"}, {"local", "local"}} {
		if _, err := conn.Exec(ctx, `SELECT set_config('synchronous_commit', $1, false)`, c.session); err != nil {
			t.Fatal(err)
		}
		tx, err := conn.BeginTx(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := setting(tx, "synchronous_commit"); got != c.want {
			t.Errorf("session synchronous_commit %s: %q inside B, want %q", c.session, got, c.want)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if got := setting(conn, "synchronous_commit"); got != c.session {
			t.Errorf("synchronous_commit = %q after COMMIT, want the session's %q", got, c.session)
		}
	}
}

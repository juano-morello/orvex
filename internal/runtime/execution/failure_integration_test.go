//go:build integration

package execution_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// betRound creates a round with alice funded 100 and two seeded
// admissions: her bet of 100 (1) and StartRound (2).
func betRound(t *testing.T, pool *pgxpool.Pool, x *execution.Executor) [16]byte {
	t.Helper()
	round := newRoundID()
	createRound(t, x, round, testSeed, 1_000_000_000, 100)
	roundtest.FundPlayer(t, pool, "alice", 100)
	seed(t, pool, round, 1, placeBet("alice", 100), 1_000)
	seed(t, pool, round, 2, startRound, 2_000)
	return round
}

// oneBet returns e after one execution of betRound's bet: one record,
// event, ledger transaction, posting, release and outbox message; alice's
// hold of 100 converted into a debit; the round account credited 100.
func oneBet(e effectSet, round [16]byte) effectSet {
	e = e.clone()
	e.processed++
	e.records++
	e.events++
	e.transactions++
	e.postings++
	e.releases++
	e.outbox++
	alice := e.accounts[playerKey("alice")]
	alice.debits += 100
	alice.held -= 100
	e.accounts[playerKey("alice")] = alice
	pot := e.accounts[roundPoolKey(round)]
	pot.credits += 100
	e.accounts[roundPoolKey(round)] = pot
	return e
}

// oneStart returns e after one execution of betRound's StartRound: one
// record, event, outbox message and snapshot; no account changes.
func oneStart(e effectSet, _ [16]byte) effectSet {
	e = e.clone()
	e.processed++
	e.records++
	e.events++
	e.outbox++
	e.snapshots++
	return e
}

func TestInjectedFailureLeavesNoPartialEffects(t *testing.T) {
	for _, table := range []string{"domain_events", "ledger_hold_releases", "ledger_transactions", "ledger_postings", "outbox_messages", "round_snapshots"} {
		t.Run(table, func(t *testing.T) {
			_, pool := newDB(t)
			installFaults(t, pool)
			x := roundtest.NewExecutor(t, pool, nil, nil)
			round := betRound(t, pool, x)
			after := oneBet
			if table == "round_snapshots" {
				// The start writes the snapshot; execute the bet first.
				if _, err := x.ExecuteNext(ctx, lease(round)); err != nil {
					t.Fatal(err)
				}
				after = oneStart
			}
			before := effects(t, pool, round)
			enableFault(t, pool, table, "P0001", "", 1_000_000)
			if _, err := x.ExecuteNext(ctx, lease(round)); !errors.Is(err, execution.ErrInternal) || postgres.SQLState(err) != "P0001" {
				t.Fatalf("ExecuteNext = %v, want ErrInternal wrapping P0001", err)
			}
			requireEffects(t, effects(t, pool, round), before)
			disableFaults(t, pool)
			if out, err := x.ExecuteNext(ctx, lease(round)); err != nil || !out.Executed {
				t.Fatalf("ExecuteNext after the fault = %+v, %v", out, err)
			}
			requireEffects(t, effects(t, pool, round), after(before, round))
		})
	}
}

func TestLedgerFailureRollsBack(t *testing.T) {
	_, pool := newDB(t)
	installFaults(t, pool)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := betRound(t, pool, x)
	before := effects(t, pool, round)
	enableFault(t, pool, "ledger_postings", "23514", "ledger_accounts_floor_check", 1_000_000)
	_, err := x.ExecuteNext(ctx, lease(round))
	if !errors.Is(err, execution.ErrInternal) || postgres.SQLState(err) != "23514" {
		t.Fatalf("ExecuteNext = %v, want ErrInternal wrapping 23514 (a bug in B, not a business rejection)", err)
	}
	requireEffects(t, effects(t, pool, round), before)
}

func TestLedgerFailureRetried(t *testing.T) {
	_, pool := newDB(t)
	installFaults(t, pool)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := betRound(t, pool, x)
	before := effects(t, pool, round)
	enableFault(t, pool, "ledger_postings", "40P01", "", 1)
	out, err := x.ExecuteNext(ctx, lease(round))
	if err != nil || !out.Executed || out.Sequence != 1 {
		t.Fatalf("ExecuteNext = %+v, %v; want success on attempt 2", out, err)
	}
	if n := count(t, pool, `SELECT last_value FROM orvex_test.fault_budget`); n < 2 {
		t.Errorf("fault budget sequence at %d: the fault did not fire before the retry", n)
	}
	requireEffects(t, effects(t, pool, round), oneBet(before, round))
}

func TestCommitFailureRollsBack(t *testing.T) {
	_, pool := newDB(t)
	installFaults(t, pool)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := betRound(t, pool, x)
	before := effects(t, pool, round)
	enableCommitFault(t, pool, "23000")
	if _, err := x.ExecuteNext(ctx, lease(round)); !errors.Is(err, execution.ErrInternal) || postgres.SQLState(err) != "23000" {
		t.Fatalf("ExecuteNext = %v, want ErrInternal wrapping 23000", err)
	}
	requireEffects(t, effects(t, pool, round), before)
}

func TestStaleFencingTokenBeforeStep(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := betRound(t, pool, x)
	before := effects(t, pool, round)
	token := takeover(t, pool, round, "engine-b")
	if _, err := x.ExecuteNext(ctx, lease(round)); !errors.Is(err, execution.ErrFenced) {
		t.Fatalf("ExecuteNext with the old token = %v, want ErrFenced", err)
	}
	requireEffects(t, effects(t, pool, round), before)
	out, err := x.ExecuteNext(ctx, execution.Lease{RoundID: round, FencingToken: token})
	if err != nil || out.Sequence != 1 {
		t.Fatalf("ExecuteNext with the new token = %+v, %v", out, err)
	}
	var recordToken int64
	if err := pool.QueryRow(ctx, `SELECT fencing_token FROM orvex.chain_records WHERE round_id = $1 AND sequence = 1`, round).Scan(&recordToken); err != nil {
		t.Fatal(err)
	}
	if recordToken != token {
		t.Errorf("record fencing token %d, want %d", recordToken, token)
	}
}

func TestTakeoverWaitsForCommit(t *testing.T) {
	_, pool := newDB(t)
	fdb := &faultDB{pool: pool}
	x := roundtest.NewExecutor(t, fdb, nil, nil)
	round := betRound(t, pool, x)

	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fdb.set(func(f *faultDB) {
		f.beforeCommit = func(pgx.Tx) error {
			once.Do(func() { close(paused) })
			<-release
			return nil
		}
	})
	type result struct {
		out execution.Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := x.ExecuteNext(ctx, lease(round))
		done <- result{out, err}
	}()
	<-paused

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	pid := conn.Conn().PgConn().PID()
	newToken := make(chan int64, 1)
	go func() { newToken <- takeoverConn(conn.Conn(), round) }()
	pgtest.WaitUntilBlocked(t, pool, pid)
	close(release)
	r := <-done
	if r.err != nil || r.out.Sequence != 1 {
		t.Fatalf("ExecuteNext = %+v, %v; want sequence 1 committed before the takeover", r.out, r.err)
	}
	if tok := <-newToken; tok != 1 {
		t.Fatalf("takeover token %d, want 1", tok)
	}
	fdb.set(func(f *faultDB) { f.beforeCommit = nil })
	before := effects(t, pool, round)
	if _, err := x.ExecuteNext(ctx, lease(round)); !errors.Is(err, execution.ErrFenced) {
		t.Fatalf("old lease after the takeover = %v, want ErrFenced", err)
	}
	requireEffects(t, effects(t, pool, round), before)
}

// takeoverConn runs the takeover on conn and returns the new token, or -1.
func takeoverConn(conn *pgx.Conn, round [16]byte) int64 {
	var token int64
	if err := conn.QueryRow(ctx, `UPDATE orvex.round_execution_heads
		SET fencing_token = fencing_token + 1, owner_id = 'engine-b', lease_expires_at = now() + interval '30 seconds'
		WHERE round_id = $1 RETURNING fencing_token`, round).Scan(&token); err != nil {
		return -1
	}
	return token
}

func TestTakeoverBeforeCompareAndSet(t *testing.T) {
	_, pool := newDB(t)
	fdb := &faultDB{pool: pool}
	x := roundtest.NewExecutor(t, fdb, nil, nil)
	round := betRound(t, pool, x)
	before := effects(t, pool, round)
	var once sync.Once
	fdb.set(func(f *faultDB) {
		f.beforeCAS = func(pgx.Tx) error {
			once.Do(func() { takeover(t, pool, round, "engine-b") })
			return nil
		}
	})
	if _, err := x.ExecuteNext(ctx, lease(round)); !errors.Is(err, execution.ErrFenced) {
		t.Fatalf("ExecuteNext = %v, want ErrFenced", err)
	}
	requireEffects(t, effects(t, pool, round), before)
	if n := count(t, pool, `SELECT fencing_token FROM orvex.round_execution_heads WHERE round_id = $1`, round); n != 1 {
		t.Errorf("fencing token %d, want 1", n)
	}
}

// terminateBeforeCommit makes the first COMMIT of fdb's transactions
// terminate the executor's backend first (a process crash before COMMIT).
func terminateBeforeCommit(t *testing.T, fdb *faultDB, admin *pgxpool.Pool) {
	t.Helper()
	var once sync.Once
	fdb.set(func(f *faultDB) {
		f.beforeCommit = func(tx pgx.Tx) error {
			once.Do(func() {
				pid := backendPID(tx)
				var ok bool
				// Wait (up to 10 s) until the backend has exited.
				if err := admin.QueryRow(ctx, `SELECT pg_terminate_backend($1, 10000)`, int64(pid)).Scan(&ok); err != nil || !ok {
					t.Errorf("pg_terminate_backend(%d) = %v, %v", pid, ok, err)
				}
			})
			return nil
		}
	})
}

func TestCrashBeforeCommit(t *testing.T) {
	t.Run("same call retries", func(t *testing.T) {
		_, pool := newDB(t)
		fdb := &faultDB{pool: pool}
		x := roundtest.NewExecutor(t, fdb, nil, nil)
		round := betRound(t, pool, x)
		before := effects(t, pool, round)
		terminateBeforeCommit(t, fdb, pool)
		out, err := x.ExecuteNext(ctx, lease(round))
		if err != nil || out.Sequence != 1 {
			t.Fatalf("ExecuteNext = %+v, %v; want success on a later attempt", out, err)
		}
		requireEffects(t, effects(t, pool, round), oneBet(before, round))
	})
	t.Run("abandoned instance", func(t *testing.T) {
		_, pool := newDB(t)
		fdb := &faultDB{pool: pool}
		x := roundtest.NewExecutor(t, fdb, func(c *execution.Config) { c.MaxAttempts = 1 }, nil)
		round := betRound(t, pool, x)
		before := effects(t, pool, round)
		terminateBeforeCommit(t, fdb, pool)
		if _, err := x.ExecuteNext(ctx, lease(round)); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("ExecuteNext = %v, want ErrUnavailable", err)
		}
		requireEffects(t, effects(t, pool, round), before)
		// A new instance cold-loads and executes 1 once.
		y := roundtest.NewExecutor(t, pool, nil, nil)
		out, err := y.ExecuteNext(ctx, lease(round))
		if err != nil || out.Sequence != 1 || out.Adopted {
			t.Fatalf("new instance = %+v, %v", out, err)
		}
		requireEffects(t, effects(t, pool, round), oneBet(before, round))
	})
}

func TestCrashAfterCommit(t *testing.T) {
	// setup returns an executor with maxAttempts whose first COMMIT
	// succeeds and then reports io.ErrUnexpectedEOF.
	setup := func(t *testing.T, maxAttempts int) (*pgxpool.Pool, *execution.Executor, [16]byte, effectSet) {
		t.Helper()
		_, pool := newDB(t)
		fdb := &faultDB{pool: pool}
		x := roundtest.NewExecutor(t, fdb, func(c *execution.Config) { c.MaxAttempts = maxAttempts }, nil)
		round := betRound(t, pool, x)
		before := effects(t, pool, round)
		var once sync.Once
		fdb.set(func(f *faultDB) {
			f.afterCommit = func(pgx.Tx) error {
				var err error
				once.Do(func() { err = io.ErrUnexpectedEOF })
				return err
			}
		})
		return pool, x, round, before
	}
	t.Run("same call adopts", func(t *testing.T) {
		pool, x, round, before := setup(t, 3)
		out, err := x.ExecuteNext(ctx, lease(round))
		if err != nil || !out.Adopted || out.Sequence != 1 || !out.Executed {
			t.Fatalf("ExecuteNext = %+v, %v; want Adopted sequence 1", out, err)
		}
		requireEffects(t, effects(t, pool, round), oneBet(before, round))
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM orvex.command_results WHERE command_id = $1`, cmdID(1)).Scan(&status); err != nil || status != "accepted" {
			t.Errorf("command_results = %q, %v; want accepted", status, err)
		}
	})
	t.Run("new instance continues", func(t *testing.T) {
		pool, crashing, round, before := setup(t, 1)
		if _, err := crashing.ExecuteNext(ctx, lease(round)); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("crashing instance = %v, want ErrUnavailable", err)
		}
		requireEffects(t, effects(t, pool, round), oneBet(before, round))
		y := roundtest.NewExecutor(t, pool, nil, nil)
		out, err := y.ExecuteNext(ctx, lease(round))
		if err != nil || !out.Executed || out.Sequence != 2 {
			t.Fatalf("new instance = %+v, %v; want sequence 2, never 1 again", out, err)
		}
		if n := count(t, pool, `SELECT count(*) FROM orvex.chain_records WHERE round_id = $1 AND sequence = 1`, round); n != 1 {
			t.Errorf("%d records at 1", n)
		}
	})
	t.Run("settle record adopted", func(t *testing.T) {
		// The ambiguous COMMIT is the one of the record that finalizes the
		// round: the adopted record carries its reveal, exactly once
		// (LBT-21 K-8).
		_, pool := newDB(t)
		fdb := &faultDB{pool: pool}
		x := roundtest.NewExecutor(t, fdb, nil, nil)
		gr := roundtest.SeedGolden(t, pool, x, goldenPath("worked-example"), 0)
		if n, err := x.Drain(ctx, lease(gr.RoundID), 20); err != nil || n != 20 {
			t.Fatalf("Drain = %d, %v; want 20", n, err)
		}
		var once sync.Once
		fdb.set(func(f *faultDB) {
			f.afterCommit = func(pgx.Tx) error {
				var err error
				once.Do(func() { err = io.ErrUnexpectedEOF })
				return err
			}
		})
		out, err := x.ExecuteNext(ctx, lease(gr.RoundID))
		if err != nil || !out.Adopted || out.Sequence != 21 || !out.Final {
			t.Fatalf("ExecuteNext = %+v, %v; want Adopted, final, sequence 21", out, err)
		}
		if n := count(t, pool, `SELECT count(*) FROM orvex.round_secret_reveals WHERE round_id = $1 AND sequence = 21`, gr.RoundID); n != 1 {
			t.Errorf("%d reveals at 21, want 1", n)
		}
	})
}

// TestRevealAtomicWithSettleRecord: the reveal commits or rolls back with
// the record that finalizes the round (LBT-21 K-6, ADR-006 failure test).
func TestRevealAtomicWithSettleRecord(t *testing.T) {
	_, pool := newDB(t)
	installFaults(t, pool)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	gr := roundtest.SeedGolden(t, pool, x, goldenPath("worked-example"), 0)
	if n, err := x.Drain(ctx, lease(gr.RoundID), 20); err != nil || n != 20 {
		t.Fatalf("Drain = %d, %v; want 20", n, err)
	}
	before := effects(t, pool, gr.RoundID)
	revealCount := func() int64 {
		return count(t, pool, `SELECT count(*) FROM orvex.round_secret_reveals WHERE round_id = $1`, gr.RoundID)
	}
	for _, f := range []struct{ table, constraint string }{
		{"round_secret_reveals", "round_secret_reveals_config_secret_check"},
		{"ledger_postings", "ledger_accounts_floor_check"},
	} {
		t.Run(f.table, func(t *testing.T) {
			enableFault(t, pool, f.table, "23514", f.constraint, 1_000_000)
			_, err := x.ExecuteNext(ctx, lease(gr.RoundID))
			if !errors.Is(err, execution.ErrInternal) || postgres.SQLState(err) != "23514" {
				t.Fatalf("ExecuteNext = %v, want ErrInternal wrapping 23514", err)
			}
			requireEffects(t, effects(t, pool, gr.RoundID), before)
			if n := revealCount(); n != 0 {
				t.Errorf("%d reveals after the failed settle record, want 0", n)
			}
			disableFaults(t, pool)
		})
	}
	out, err := x.ExecuteNext(ctx, lease(gr.RoundID))
	if err != nil || out.Sequence != 21 || !out.Final {
		t.Fatalf("ExecuteNext after the faults = %+v, %v; want final record 21", out, err)
	}
	if n := revealCount(); n != 1 {
		t.Errorf("%d reveals after record 21, want 1", n)
	}
}

func TestDuplicatePayoutImpossible(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	y := roundtest.NewExecutor(t, pool, func(c *execution.Config) { c.EngineBuild = "other" }, nil)

	// 1. The in-progress golden up to admission 2; Y caches the state at 2;
	// X executes alice's cash-out; Y reloads and finds nothing pending.
	gr := roundtest.SeedGolden(t, pool, x, goldenPath("in-progress"), 2)
	if n, err := x.Drain(ctx, lease(gr.RoundID), 10); err != nil || n != 2 {
		t.Fatalf("X Drain = %d, %v", n, err)
	}
	if out, err := y.ExecuteNext(ctx, lease(gr.RoundID)); err != nil || out.Executed {
		t.Fatalf("Y idle = %+v, %v", out, err)
	}
	roundtest.SeedGoldenAdmissions(t, pool, gr, 3, 3)
	if out, err := x.ExecuteNext(ctx, lease(gr.RoundID)); err != nil || out.Sequence != 3 || out.Result.Rejected {
		t.Fatalf("X cash-out = %+v, %v", out, err)
	}
	if out, err := y.ExecuteNext(ctx, lease(gr.RoundID)); err != nil || out.Executed {
		t.Fatalf("Y after X's cash-out = %+v, %v; want a reload, then nothing pending", out, err)
	}
	payouts := func() int64 {
		return count(t, pool, `SELECT count(*) FROM orvex.ledger_transactions WHERE round_id = $1 AND kind = 'crash_cash_out'`, gr.RoundID)
	}
	if n := payouts(); n != 1 {
		t.Errorf("%d cash-out transactions, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_postings WHERE round_id = $1 AND event_sequence = 3`, gr.RoundID); n != 1 {
		t.Errorf("%d postings for event 3, want 1", n)
	}
	alice := playerAccount(t, pool, "alice")
	if alice.credits != 1000+3320 {
		t.Errorf("alice credits %d, want 4320 (credited once)", alice.credits)
	}

	// 2. A second cash-out is rejected and posts nothing.
	third := gr.Commands[2]
	roundtest.SeedAdmission(t, pool, gr.RoundID, cmdID(4), encode(t, cashOut("alice")), third.EffectiveTimeUnixMicro, nil)
	out, err := x.ExecuteNext(ctx, lease(gr.RoundID))
	if err != nil || !out.Result.Rejected || out.Result.Reason != string(v1.ReasonAlreadyCashedOut) {
		t.Fatalf("second cash-out = %+v, %v; want rejected already_cashed_out", out, err)
	}
	if n := payouts(); n != 1 {
		t.Errorf("%d cash-out transactions after the repeat, want 1", n)
	}

	// 3. The database refuses a second payout on its own.
	pot := ledger.RoundAccountID(gr.RoundID)
	var aliceID [16]byte
	if err := pool.QueryRow(ctx, `SELECT account_id FROM orvex.ledger_accounts WHERE player_id = 'alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	requireStmtError(t, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO orvex.ledger_transactions (transaction_id, kind, round_id, sequence) VALUES ($1, 'crash_cash_out', $2, 3)`,
			newRoundID(), gr.RoundID)
		return err
	}, "23505", "ledger_transactions_record_key")
	requireStmtError(t, pool, func(tx pgx.Tx) error {
		id := newRoundID()
		if _, err := tx.Exec(ctx, `INSERT INTO orvex.ledger_transactions (transaction_id, kind, round_id, sequence) VALUES ($1, 'crash_cash_out', $2, 4)`,
			id, gr.RoundID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO orvex.ledger_postings
			(transaction_id, posting_index, debit_account_id, credit_account_id, amount, round_id, event_sequence)
			VALUES ($1, 0, $2, $3, 3320, $4, 3)`, id, pot, aliceID, gr.RoundID)
		return err
	}, "23505", "ledger_postings_event_key")
	requireStmtError(t, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO orvex.ledger_hold_releases (round_id, sequence, account_id) VALUES ($1, 1, $2)`, gr.RoundID, aliceID)
		return err
	}, "23505", "ledger_hold_releases_pkey")

	// 4. Two instances with the same token race on a fresh round's
	// cash-out: one posting.
	round := newRoundID()
	a := roundtest.NewExecutor(t, pool, nil, nil)
	b := roundtest.NewExecutor(t, pool, nil, nil)
	createRound(t, a, round, seedWithDeadline(t, round, 30_000_000_000, 150_000_000_000), 1_000_000_000, 100)
	roundtest.FundPlayer(t, pool, "zara", 100)
	seed(t, pool, round, 1, placeBet("zara", 100), 1_000)
	seed(t, pool, round, 2, startRound, 2_000)
	if n, err := a.Drain(ctx, lease(round), 10); err != nil || n != 2 {
		t.Fatalf("A Drain = %d, %v", n, err)
	}
	if out, err := b.ExecuteNext(ctx, lease(round)); err != nil || out.Executed {
		t.Fatalf("B idle = %+v, %v", out, err)
	}
	seed(t, pool, round, 3, cashOut("zara"), 3_000)
	var wg sync.WaitGroup
	outs := make([]execution.Outcome, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, ex := range []*execution.Executor{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outs[i], errs[i] = ex.ExecuteNext(ctx, lease(round))
		}()
	}
	close(start)
	wg.Wait()
	committed := 0
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("instance %d: %v", i, errs[i])
		}
		if outs[i].Executed && !outs[i].Adopted {
			committed++
		}
	}
	if committed != 1 {
		t.Errorf("%d instances committed the cash-out, want 1 (outcomes %+v / %+v)", committed, outs[0], outs[1])
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_postings WHERE round_id = $1 AND event_sequence = 3`, round); n != 1 {
		t.Errorf("%d postings for the raced cash-out, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.chain_records WHERE round_id = $1`, round); n != 4 {
		t.Errorf("%d records, want 4", n)
	}
}

// requireStmtError runs fn in a transaction that is rolled back and
// requires its error to carry sqlstate and constraint.
func requireStmtError(t *testing.T, db *pgxpool.Pool, fn func(tx pgx.Tx) error, sqlstate, constraint string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.ExecutionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = fn(tx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlstate || pgErr.ConstraintName != constraint {
		t.Errorf("err = %v, want SQLSTATE %s on %s", err, sqlstate, constraint)
	}
}

// TestContextEndedKeepsEntry: the context ends inside Transaction B and the
// driver reports 57014 (query canceled), which does not wrap the context
// error. ExecuteNext returns an error wrapping ctx.Err() and the PgError,
// writes nothing, and keeps the round's cached state: the next call runs
// Transaction B without a cold load (E-15).
func TestContextEndedKeepsEntry(t *testing.T) {
	_, pool := newDB(t)
	fdb := &faultDB{pool: pool}
	x := roundtest.NewExecutor(t, fdb, nil, nil)
	round := betRound(t, pool, x)
	before := effects(t, pool, round)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	fdb.set(func(f *faultDB) {
		f.beforeCAS = func(pgx.Tx) error {
			var err error
			once.Do(func() {
				cancel()
				err = &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}
			})
			return err
		}
	})
	_, err := x.ExecuteNext(cctx, lease(round))
	if !errors.Is(err, context.Canceled) || postgres.SQLState(err) != "57014" {
		t.Fatalf("ExecuteNext = %v; want an error wrapping context.Canceled and the 57014 PgError", err)
	}
	requireEffects(t, effects(t, pool, round), before)

	begins := fdb.begins.Load()
	out, err := x.ExecuteNext(ctx, lease(round))
	if err != nil || out.Sequence != 1 || out.Adopted {
		t.Fatalf("ExecuteNext after the context error = %+v, %v", out, err)
	}
	if n := fdb.begins.Load() - begins; n != 1 {
		t.Errorf("%d transactions for the next call, want 1: the entry was dropped and cold-loaded", n)
	}
	requireEffects(t, effects(t, pool, round), oneBet(before, round))
}

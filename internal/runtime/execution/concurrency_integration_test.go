//go:build integration

package execution_test

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// conflictRoundID is the round of every TestConflictingPerRoundCommits
// trial; each trial has its own database.
var conflictRoundID = [16]byte{0x0e, 0, 0, 0, 0, 0, 0x70, 0, 0xc0, 0xff, 0xee, 0, 0, 0, 0, 0x01}

// seededCommand is one admission of a bulk seed.
type seededCommand struct {
	cmd v1.Command
	at  int64
}

// seedMany funds players and seeds commands with their S-10 holds in one
// transaction, as consecutive admissions.
func seedMany(t *testing.T, db *pgxpool.Pool, roundID [16]byte, funds map[string]int64, cmds []seededCommand) {
	t.Helper()
	inTx(t, db, func(tx pgx.Tx) {
		if len(funds) > 0 {
			players := make([]string, 0, len(funds))
			amounts := make([]int64, 0, len(funds))
			for p, c := range funds {
				players, amounts = append(players, p), append(amounts, c)
			}
			mustExec(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
				SELECT gen_random_uuid(), 'system', 'faucet', false
				WHERE NOT EXISTS (SELECT 1 FROM orvex.ledger_accounts WHERE kind = 'system' AND name = 'faucet' AND round_id IS NULL)`)
			mustExec(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced)
				SELECT gen_random_uuid(), 'player', p, true FROM unnest($1::text[]) AS u(p) ON CONFLICT (player_id) DO NOTHING`, players)
			txID := newRoundID()
			mustExec(t, tx, `INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`, txID)
			mustExec(t, tx, `INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount)
				SELECT $1, (u.n - 1)::int, f.account_id, p.account_id, u.a
				FROM unnest($2::text[], $3::bigint[]) WITH ORDINALITY AS u(p, a, n)
				JOIN orvex.ledger_accounts p ON p.player_id = u.p
				CROSS JOIN orvex.ledger_accounts f
				WHERE f.kind = 'system' AND f.name = 'faucet' AND f.round_id IS NULL`, txID, players, amounts)
		}
		var seq int64
		if err := tx.QueryRow(ctx, `SELECT admitted_sequence FROM orvex.round_admission_heads WHERE round_id = $1 FOR UPDATE`, roundID).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		for i, c := range cmds {
			seq++
			mustExec(t, tx, `INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
				VALUES ($1, $2, $3, $4, $5)`, roundID, seq, cmdID(100_000+i), c.at, encode(t, c.cmd))
			if h := holdFor(c.cmd); h != nil {
				mustExec(t, tx, `INSERT INTO orvex.ledger_holds (round_id, sequence, account_id, amount)
					SELECT $1, $2, account_id, $4 FROM orvex.ledger_accounts WHERE kind = 'player' AND player_id = $3`,
					roundID, seq, h.player, h.credits)
			}
		}
		mustExec(t, tx, `UPDATE orvex.round_admission_heads SET admitted_sequence = $2 WHERE round_id = $1`, roundID, seq)
	})
}

// conflictRound creates the 200-admission round of
// TestConflictingPerRoundCommits with x: 100 funded bets, StartRound, 50
// cash-outs inside the deadline, 47 rejected commands (25 wrong_phase bets
// with holds, 22 repeated cash-outs), CloseRound at T0 + D, SettleRound.
func conflictRound(t *testing.T, pool *pgxpool.Pool, x *execution.Executor, seed [32]byte) {
	t.Helper()
	cfg := createRound(t, x, conflictRoundID, seed, 1_000_000_000, 100)
	s, err := v1.Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := s.CrashDeadlineOffsetMicros
	const t0 = int64(1_760_000_000_000_000)
	funds := map[string]int64{}
	var cmds []seededCommand
	player := func(i int) string { return fmt.Sprintf("p%02d", i) }
	for i := range 100 {
		funds[player(i)] = 200
		cmds = append(cmds, seededCommand{placeBet(player(i), int64(10+i)), t0 - 1000 + int64(i)})
	}
	cmds = append(cmds, seededCommand{startRound, t0})
	for i := range 50 {
		cmds = append(cmds, seededCommand{cashOut(player(i)), t0 + 1 + int64(i)})
	}
	for i := range 25 {
		cmds = append(cmds, seededCommand{placeBet(player(50+i), 5), t0 + 100 + int64(i)})
	}
	for i := range 22 {
		cmds = append(cmds, seededCommand{cashOut(player(i)), t0 + 200 + int64(i)})
	}
	cmds = append(cmds, seededCommand{closeRound, t0 + d}, seededCommand{settleRound, t0 + d + 1})
	if len(cmds) != 200 {
		t.Fatalf("%d commands, want 200", len(cmds))
	}
	seedMany(t, pool, conflictRoundID, funds, cmds)
}

// runUntilIdle calls ExecuteNext until nothing is pending. ErrUnavailable
// (attempts exhausted by the other instance's commits) is retried, as the
// caller is told to (E-15). It returns every outcome.
func runUntilIdle(x *execution.Executor, l execution.Lease) ([]execution.Outcome, error) {
	var outs []execution.Outcome
	for {
		out, err := x.ExecuteNext(ctx, l)
		if errors.Is(err, execution.ErrUnavailable) {
			continue
		}
		if err != nil {
			return outs, err
		}
		if !out.Executed {
			return outs, nil
		}
		outs = append(outs, out)
	}
}

// totalsSnapshot returns every account's totals keyed by account ID, minus
// the faucet's random ID, plus the players by name.
func totalsSnapshot(t *testing.T, pool *pgxpool.Pool) map[string]account {
	t.Helper()
	out := map[string]account{}
	rows, err := pool.Query(ctx, `SELECT coalesce(player_id, name), debits_posted, credits_posted, held FROM orvex.ledger_accounts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var a account
		if err := rows.Scan(&k, &a.debits, &a.credits, &a.held); err != nil {
			t.Fatal(err)
		}
		out[k] = a
	}
	return out
}

func TestConflictingPerRoundCommits(t *testing.T) {
	seed := seedWithDeadline(t, conflictRoundID, time.Second, 10*time.Second)

	// The sequential reference run.
	_, refPool := newDB(t)
	ref := newExecutor(t, refPool, nil, nil)
	conflictRound(t, refPool, ref, seed)
	if n, err := ref.Drain(ctx, lease(conflictRoundID), 1000); err != nil || n != 200 {
		t.Fatalf("reference Drain = %d, %v", n, err)
	}
	wantTotals := totalsSnapshot(t, refPool)
	wantEffects := effects(t, refPool, conflictRoundID)

	for _, variant := range []struct {
		name   string
		builds [2]string
	}{{"same build", [2]string{"golden", "golden"}}, {"two builds", [2]string{"a", "b"}}} {
		t.Run(variant.name, func(t *testing.T) {
			for trial := range 10 {
				_, pool := newDB(t)
				xs := [2]*execution.Executor{
					newExecutor(t, pool, func(c *execution.Config) { c.EngineBuild = variant.builds[0] }, nil),
					newExecutor(t, pool, func(c *execution.Config) { c.EngineBuild = variant.builds[1] }, nil),
				}
				conflictRound(t, pool, xs[0], seed)
				var (
					outs [2][]execution.Outcome
					errs [2]error
					wg   sync.WaitGroup
				)
				start := make(chan struct{})
				for i := range 2 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						outs[i], errs[i] = runUntilIdle(xs[i], lease(conflictRoundID))
					}()
				}
				close(start)
				wg.Wait()
				for i, err := range errs {
					if err != nil {
						t.Fatalf("trial %d: instance %d: %v", trial, i, err)
					}
				}
				requireConflictInvariants(t, pool, outs, variant.builds, wantTotals, wantEffects)
			}
		})
	}
}

// requireConflictInvariants checks the interleaving-independent invariants
// of two instances that drained one round.
func requireConflictInvariants(t *testing.T, pool *pgxpool.Pool, outs [2][]execution.Outcome, builds [2]string,
	wantTotals map[string]account, wantEffects effectSet) {
	t.Helper()
	got := effects(t, pool, conflictRoundID)
	requireEffects(t, got, wantEffects)
	gotTotals := totalsSnapshot(t, pool)
	if !reflect.DeepEqual(gotTotals, wantTotals) {
		t.Errorf("account totals differ from the sequential run")
	}
	// Every sequence has exactly one record; its committer's build is in it.
	committed := map[uint64]int{}
	for i, list := range outs {
		for _, o := range list {
			if !o.Executed {
				t.Errorf("instance %d: outcome not executed", i)
			}
			if !o.Adopted {
				if prev, dup := committed[o.Sequence]; dup && prev != i {
					t.Errorf("sequence %d committed by both instances", o.Sequence)
				}
				committed[o.Sequence] = i
			}
		}
	}
	rows, err := pool.Query(ctx, `SELECT sequence, record FROM orvex.chain_records WHERE round_id = $1 AND sequence > 0 ORDER BY sequence`, conflictRoundID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var seq int64
		var rec []byte
		if err := rows.Scan(&seq, &rec); err != nil {
			t.Fatal(err)
		}
		n++
		r, err := chain.Decode(rec)
		if err != nil {
			t.Fatal(err)
		}
		build := r.(chain.Command).EngineBuild
		if who, ok := committed[uint64(seq)]; ok && builds[who] != build { //nolint:gosec // G115: seq is chain_records.sequence (CHECK >= 0)
			t.Errorf("record %d carries build %q, its committer is %q", seq, build, builds[who])
		}
		if build != builds[0] && build != builds[1] {
			t.Errorf("record %d carries build %q", seq, build)
		}
	}
	if n != 200 {
		t.Errorf("%d command records, want 200", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_hold_releases WHERE round_id = $1`, conflictRoundID); n != 125 {
		t.Errorf("%d releases, want one per hold (125)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM (SELECT round_id, event_sequence FROM orvex.ledger_postings
		WHERE round_id = $1 GROUP BY 1, 2 HAVING count(*) > 1) d`, conflictRoundID); n != 0 {
		t.Errorf("%d events with more than one posting", n)
	}
	requireBalanced(t, pool)
	b := exportHistory(t, pool, conflictRoundID)
	if _, err := verifyExport(t, b); err != nil {
		t.Errorf("Verify(export): %v", err)
	}
}

func TestStaleCacheDiscarded(t *testing.T) {
	_, pool := newDB(t)
	x := newExecutor(t, pool, nil, nil)
	y := newExecutor(t, pool, func(c *execution.Config) { c.EngineBuild = "other" }, nil)
	round := newRoundID()
	createRound(t, x, round, testSeed, 1_000_000_000, 100)
	funds := map[string]int64{}
	var cmds []seededCommand
	for i := range 20 {
		p := fmt.Sprintf("s%02d", i)
		funds[p] = 100
		cmds = append(cmds, seededCommand{placeBet(p, int64(1+i)), 1_000 + int64(i)})
	}
	seedMany(t, pool, round, funds, cmds)

	if n, err := x.Drain(ctx, lease(round), 10); err != nil || n != 10 {
		t.Fatalf("X Drain(10) = %d, %v", n, err)
	}
	if n, err := y.Drain(ctx, lease(round), 5); err != nil || n != 5 {
		t.Fatalf("Y Drain(5) = %d, %v", n, err)
	}
	out, err := x.ExecuteNext(ctx, lease(round))
	if err != nil || out.Sequence != 16 || out.Adopted {
		t.Fatalf("X ExecuteNext = %+v, %v; want sequence 16 after discarding its stale record 11", out, err)
	}
	var hash15 []byte
	if err := pool.QueryRow(ctx, `SELECT hash FROM orvex.chain_records WHERE round_id = $1 AND sequence = 15`, round).Scan(&hash15); err != nil {
		t.Fatal(err)
	}
	rec, err := chain.Decode(out.Record)
	if err != nil {
		t.Fatal(err)
	}
	if prev := rec.(chain.Command).PrevHash; string(prev[:]) != string(hash15) {
		t.Error("record 16's prevHash is not Y's record 15")
	}
	builds := map[int64]string{}
	rows, err := pool.Query(ctx, `SELECT sequence, record FROM orvex.chain_records WHERE round_id = $1 AND sequence > 0 ORDER BY sequence`, round)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int64
		var b []byte
		if err := rows.Scan(&seq, &b); err != nil {
			t.Fatal(err)
		}
		r, _ := chain.Decode(b)
		builds[seq] = r.(chain.Command).EngineBuild
	}
	rows.Close()
	if len(builds) != 16 {
		t.Fatalf("%d command records, want 16", len(builds))
	}
	for seq, b := range builds {
		want := "golden"
		if seq >= 11 && seq <= 15 {
			want = "other"
		}
		if b != want {
			t.Errorf("record %d build %q, want %q", seq, b, want)
		}
	}
	if got := accountByID(t, pool, ledger.RoundAccountID(round)); got.credits != int64(16*17/2) {
		t.Errorf("round account credits %d, want %d", got.credits, 16*17/2)
	}
}

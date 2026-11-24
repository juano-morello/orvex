//go:build integration

package grant_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/runtime/grant"
)

// Integration tests of grant.Apply against real PostgreSQL. Each test uses
// its own migrated database. Blocking is detected through pg_stat_activity,
// never with sleeps.

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var ctx = context.Background()

func newDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.MigratedDatabase(t)
	return dsn, pgtest.Pool(t, dsn)
}

func mustParse(t *testing.T, key, player string, credits int64) grant.Grant {
	t.Helper()
	g, err := grant.Parse(key, player, fmt.Sprint(credits))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func mustApply(t *testing.T, db grant.DB, g grant.Grant, want grant.Status) grant.Result {
	t.Helper()
	res, err := grant.Apply(ctx, db, g)
	if err != nil {
		t.Fatalf("Apply(%s): %v", g.Key(), err)
	}
	if res.Status != want || res.TransactionID != ledger.GrantTransactionID(g.Key()) {
		t.Fatalf("Apply(%s) = %+v, want status %v and transaction %x", g.Key(), res, want, ledger.GrantTransactionID(g.Key()))
	}
	return res
}

func mustExec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func count(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// ledgerDump returns every ledger account, transaction and posting, so
// that two dumps are equal iff nothing was written in between.
func ledgerDump(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	var out string
	if err := db.QueryRow(ctx, `SELECT concat_ws(E'\n--\n',
		(SELECT string_agg(format('%s %s %s %s %s %s %s %s', account_id, kind, player_id, name, floor_enforced,
			debits_posted, credits_posted, held), E'\n' ORDER BY account_id) FROM orvex.ledger_accounts),
		(SELECT string_agg(format('%s %s %s %s', transaction_id, kind, round_id, sequence), E'\n' ORDER BY transaction_id)
			FROM orvex.ledger_transactions),
		(SELECT string_agg(format('%s %s %s %s %s', transaction_id, posting_index, debit_account_id, credit_account_id, amount),
			E'\n' ORDER BY transaction_id, posting_index) FROM orvex.ledger_postings))`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

type account struct {
	ID                    [16]byte
	Debits, Credits, Held int64
}

// playerAccount returns player's account; ok is false if it has none.
func playerAccount(t *testing.T, db *pgxpool.Pool, player string) (a account, ok bool) {
	t.Helper()
	err := db.QueryRow(ctx, `SELECT account_id, debits_posted, credits_posted, held FROM orvex.ledger_accounts
		WHERE kind = 'player' AND player_id = $1 AND floor_enforced`, player).Scan(&a.ID, &a.Debits, &a.Credits, &a.Held)
	if errors.Is(err, pgx.ErrNoRows) {
		return account{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return a, true
}

// faucetAccount returns the one global faucet account.
func faucetAccount(t *testing.T, db *pgxpool.Pool) account {
	t.Helper()
	if n := count(t, db, `SELECT count(*) FROM orvex.ledger_accounts WHERE name = 'faucet'`); n != 1 {
		t.Fatalf("%d faucet accounts, want 1", n)
	}
	var a account
	if err := db.QueryRow(ctx, `SELECT account_id, debits_posted, credits_posted, held FROM orvex.ledger_accounts
		WHERE kind = 'system' AND name = 'faucet' AND round_id IS NULL AND NOT floor_enforced`).
		Scan(&a.ID, &a.Debits, &a.Credits, &a.Held); err != nil {
		t.Fatal(err)
	}
	return a
}

// requireBalanced fails unless every account total adds up: Σ debits = Σ
// credits.
func requireBalanced(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	var debits, credits int64
	if err := db.QueryRow(ctx, `SELECT coalesce(sum(debits_posted), 0), coalesce(sum(credits_posted), 0)
		FROM orvex.ledger_accounts`).Scan(&debits, &credits); err != nil {
		t.Fatal(err)
	}
	if debits != credits {
		t.Errorf("Σ debits %d != Σ credits %d", debits, credits)
	}
}

func requireCredits(t *testing.T, db *pgxpool.Pool, player string, credits int64) account {
	t.Helper()
	a, ok := playerAccount(t, db, player)
	if !ok || a.Credits != credits || a.Debits != 0 || a.Held != 0 {
		t.Fatalf("%s account %+v (exists %v), want credits %d, debits 0, held 0", player, a, ok, credits)
	}
	return a
}

func TestApplyNewPlayer(t *testing.T) {
	_, pool := newDB(t)
	res := mustApply(t, pool, mustParse(t, "alice-1", "alice", 500), grant.Applied)

	alice := requireCredits(t, pool, "alice", 500)
	if res.AccountID != alice.ID {
		t.Errorf("AccountID %x, want alice's account %x", res.AccountID, alice.ID)
	}
	faucet := faucetAccount(t, pool)
	if faucet != (account{ID: ledger.FaucetAccountID(), Debits: 500}) {
		t.Errorf("faucet %+v, want ID %x, debits 500 and nothing else", faucet, ledger.FaucetAccountID())
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_transactions
		WHERE transaction_id = $1 AND kind = 'grant' AND round_id IS NULL AND sequence IS NULL`, res.TransactionID); n != 1 {
		t.Errorf("%d grant transaction rows, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_postings
		WHERE transaction_id = $1 AND posting_index = 0 AND debit_account_id = $2 AND credit_account_id = $3
		AND amount = 500 AND round_id IS NULL AND event_sequence IS NULL`, res.TransactionID, faucet.ID, alice.ID); n != 1 {
		t.Errorf("%d matching postings, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_postings`); n != 1 {
		t.Errorf("%d postings, want 1", n)
	}
	requireBalanced(t, pool)
}

func TestApplyAgainChangesNothing(t *testing.T) {
	_, pool := newDB(t)
	g := mustParse(t, "alice-1", "alice", 500)
	first := mustApply(t, pool, g, grant.Applied)
	before := ledgerDump(t, pool)
	for range 3 {
		again := mustApply(t, pool, g, grant.Existing)
		if again.AccountID != first.AccountID {
			t.Errorf("rerun AccountID %x, want %x", again.AccountID, first.AccountID)
		}
	}
	if after := ledgerDump(t, pool); after != before {
		t.Errorf("rerun wrote:\nbefore\n%s\nafter\n%s", before, after)
	}
	requireCredits(t, pool, "alice", 500)
}

func TestApplyGrantsAdd(t *testing.T) {
	_, pool := newDB(t)
	a := mustApply(t, pool, mustParse(t, "alice-1", "alice", 500), grant.Applied)
	b := mustApply(t, pool, mustParse(t, "alice-2", "alice", 250), grant.Applied)
	if a.AccountID != b.AccountID {
		t.Errorf("second grant credited account %x, want %x", b.AccountID, a.AccountID)
	}
	mustApply(t, pool, mustParse(t, "bob-1", "bob", 100), grant.Applied)
	requireCredits(t, pool, "alice", 750)
	requireCredits(t, pool, "bob", 100)
	if f := faucetAccount(t, pool); f.Debits != 850 || f.Credits != 0 {
		t.Errorf("faucet %+v, want debits 850, credits 0", f)
	}
	requireBalanced(t, pool)
}

func TestApplyKeyReused(t *testing.T) {
	_, pool := newDB(t)
	mustApply(t, pool, mustParse(t, "alice-1", "alice", 500), grant.Applied)
	before := ledgerDump(t, pool)
	for _, g := range []grant.Grant{
		mustParse(t, "alice-1", "alice", 501),
		mustParse(t, "alice-1", "carol", 500),
		mustParse(t, "alice-1", "carol", 7),
	} {
		_, err := grant.Apply(ctx, pool, g)
		var reused *grant.KeyReusedError
		if !errors.As(err, &reused) || *reused != (grant.KeyReusedError{Key: "alice-1", Player: "alice", Credits: 500}) {
			t.Errorf("Apply(%s, %s, %d) = %v, want the stored alice, 500", g.Key(), g.Player(), g.Credits(), err)
		}
	}
	if after := ledgerDump(t, pool); after != before {
		t.Errorf("a reused grant key wrote:\nbefore\n%s\nafter\n%s", before, after)
	}
	if _, ok := playerAccount(t, pool, "carol"); ok {
		t.Error("a reused grant key created carol's account")
	}
}

// applyAll applies grants concurrently, all released at once, and returns
// each call's result and error.
func applyAll(t *testing.T, db grant.DB, grants []grant.Grant) ([]grant.Result, []error) {
	t.Helper()
	results := make([]grant.Result, len(grants))
	errs := make([]error, len(grants))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, g := range grants {
		wg.Go(func() {
			<-start
			results[i], errs[i] = grant.Apply(ctx, db, g)
		})
	}
	close(start)
	wg.Wait()
	return results, errs
}

func TestApplyConcurrentSameGrant(t *testing.T) {
	_, pool := newDB(t)
	g := mustParse(t, "alice-1", "alice", 500)
	grants := []grant.Grant{g, g, g, g, g, g, g, g}
	results, errs := applyAll(t, pool, grants)
	statuses := map[grant.Status]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		statuses[results[i].Status]++
	}
	if statuses[grant.Applied] != 1 || statuses[grant.Existing] != 7 {
		t.Errorf("statuses %v, want 1 applied and 7 existing", statuses)
	}
	requireCredits(t, pool, "alice", 500)
	if f := faucetAccount(t, pool); f.Debits != 500 {
		t.Errorf("faucet debits %d, want 500", f.Debits)
	}
	requireBalanced(t, pool)
}

func TestApplyConcurrentSameKeyOtherPayloads(t *testing.T) {
	_, pool := newDB(t)
	payloads := []grant.Grant{mustParse(t, "k", "bob", 100), mustParse(t, "k", "carol", 200)}
	var grants []grant.Grant
	for i := range 8 {
		grants = append(grants, payloads[i%2])
	}
	results, errs := applyAll(t, pool, grants)
	winner := -1
	for i, err := range errs {
		if err == nil && results[i].Status == grant.Applied {
			if winner >= 0 {
				t.Fatalf("calls %d and %d both applied", winner, i)
			}
			winner = i
		}
	}
	if winner < 0 {
		t.Fatalf("no call applied: %v", errs)
	}
	w := grants[winner]
	for i, err := range errs {
		if i == winner {
			continue
		}
		var reused *grant.KeyReusedError
		switch {
		case grants[i] == w:
			if err != nil || results[i].Status != grant.Existing {
				t.Errorf("call %d (same payload as the winner) = %+v, %v; want existing", i, results[i], err)
			}
		case !errors.As(err, &reused) || reused.Player != w.Player() || reused.Credits != w.Credits():
			t.Errorf("call %d (other payload) = %v, want KeyReusedError naming %s, %d", i, err, w.Player(), w.Credits())
		}
	}
	requireCredits(t, pool, w.Player(), w.Credits())
	loser := payloads[0]
	if loser == w {
		loser = payloads[1]
	}
	if _, ok := playerAccount(t, pool, loser.Player()); ok {
		t.Errorf("the losing payload created %s's account", loser.Player())
	}
	requireBalanced(t, pool)
}

func TestApplyConcurrentKeysOnePlayer(t *testing.T) {
	_, pool := newDB(t)
	var grants []grant.Grant
	for i := range 8 {
		grants = append(grants, mustParse(t, fmt.Sprintf("dave-%d", i), "dave", int64(i+1)))
	}
	results, errs := applyAll(t, pool, grants)
	for i, err := range errs {
		if err != nil || results[i].Status != grant.Applied {
			t.Errorf("call %d = %+v, %v; want applied", i, results[i], err)
		}
	}
	dave := requireCredits(t, pool, "dave", 36)
	for i, res := range results {
		if res.AccountID != dave.ID {
			t.Errorf("call %d credited %x, want dave's one account %x", i, res.AccountID, dave.ID)
		}
	}
	if f := faucetAccount(t, pool); f.Debits != 36 {
		t.Errorf("faucet debits %d, want 36", f.Debits)
	}
	requireBalanced(t, pool)
}

// lockAccount locks player's account FOR NO KEY UPDATE, as admission's hold
// check does, in a transaction on its own connection, and returns it.
func lockAccount(t *testing.T, dsn, player string) pgx.Tx {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id [16]byte
	if err := tx.QueryRow(ctx, `SELECT account_id FROM orvex.ledger_accounts WHERE player_id = $1 FOR NO KEY UPDATE`,
		player).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return tx
}

// waitLockWaiter polls pg_stat_activity until a backend of this database
// waits on a lock.
func waitLockWaiter(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for count(t, db, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no backend blocked on a lock within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApplyWaitsForAccountLock(t *testing.T) {
	dsn, pool := newDB(t)
	mustApply(t, pool, mustParse(t, "erin-1", "erin", 100), grant.Applied)
	lock := lockAccount(t, dsn, "erin")

	done := make(chan error, 1)
	go func() {
		_, err := grant.Apply(ctx, pool, mustParse(t, "erin-2", "erin", 50))
		done <- err
	}()
	waitLockWaiter(t, pool)
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Apply after the lock was released: %v", err)
	}
	requireCredits(t, pool, "erin", 150)
}

func TestApplyLockTimeout(t *testing.T) {
	dsn, pool := newDB(t)
	mustApply(t, pool, mustParse(t, "erin-1", "erin", 100), grant.Applied)
	before := ledgerDump(t, pool)
	lock := lockAccount(t, dsn, "erin")

	g := mustParse(t, "erin-2", "erin", 50)
	if _, err := grant.Apply(ctx, pool, g); postgres.SQLState(err) != "55P03" {
		t.Fatalf("Apply under a held account lock = %v, want SQLSTATE 55P03", err)
	}
	if after := ledgerDump(t, pool); after != before {
		t.Errorf("a lock timeout wrote:\nbefore\n%s\nafter\n%s", before, after)
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	mustApply(t, pool, g, grant.Applied)
	requireCredits(t, pool, "erin", 150)
}

// commitHookDB begins transactions on pool whose Commit runs commit.
type commitHookDB struct {
	pool   *pgxpool.Pool
	commit func(ctx context.Context, tx pgx.Tx) error
}

func (d commitHookDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return hookTx{Tx: tx, commit: d.commit}, nil
}

type hookTx struct {
	pgx.Tx
	commit func(ctx context.Context, tx pgx.Tx) error
}

func (tx hookTx) Commit(ctx context.Context) error { return tx.commit(ctx, tx.Tx) }

func TestApplyCancelledBeforeCommit(t *testing.T) {
	_, pool := newDB(t)
	before := ledgerDump(t, pool)
	g := mustParse(t, "alice-1", "alice", 500)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	db := commitHookDB{pool: pool, commit: func(ctx context.Context, tx pgx.Tx) error {
		cancel()
		return tx.Commit(ctx)
	}}
	if _, err := grant.Apply(cctx, db, g); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply cancelled before COMMIT = %v, want context.Canceled", err)
	}
	if after := ledgerDump(t, pool); after != before {
		t.Errorf("a cancelled grant persisted:\n%s", after)
	}
	mustApply(t, pool, g, grant.Applied)
	requireCredits(t, pool, "alice", 500)
}

func TestApplyCommitResponseLost(t *testing.T) {
	_, pool := newDB(t)
	g := mustParse(t, "alice-1", "alice", 500)
	errLost := errors.New("connection reset after COMMIT")
	db := commitHookDB{pool: pool, commit: func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return errLost
	}}
	if _, err := grant.Apply(ctx, db, g); !errors.Is(err, errLost) {
		t.Fatalf("Apply = %v, want the lost commit response", err)
	}
	mustApply(t, pool, g, grant.Existing)
	requireCredits(t, pool, "alice", 500)
	requireBalanced(t, pool)
}

func TestApplyCorruptStoredGrant(t *testing.T) {
	txID := ledger.GrantTransactionID("k")
	for _, c := range []struct {
		name string
		seed []string
	}{
		{"no posting", []string{
			`INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`,
		}},
		{"other kind", []string{
			`INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'crash_bet')`,
			`INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount)
				SELECT $1, 0, f.account_id, p.account_id, 5 FROM orvex.ledger_accounts f, orvex.ledger_accounts p
				WHERE f.name = 'faucet' AND p.player_id = 'alice'`,
		}},
		{"debit not the faucet", []string{
			`INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`,
			`INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount)
				SELECT $1, 0, h.account_id, p.account_id, 5 FROM orvex.ledger_accounts h, orvex.ledger_accounts p
				WHERE h.name = 'house' AND p.player_id = 'alice'`,
		}},
		{"two postings", []string{
			`INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`,
			`INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount)
				SELECT $1, i, f.account_id, p.account_id, 5 FROM orvex.ledger_accounts f, orvex.ledger_accounts p,
				generate_series(0, 1) i WHERE f.name = 'faucet' AND p.player_id = 'alice'`,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, pool := newDB(t)
			mustApply(t, pool, mustParse(t, "alice-1", "alice", 500), grant.Applied)
			mustExec(t, pool, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
				VALUES ($1, 'system', 'house', false)`, ledger.HouseAccountID())
			for _, sql := range c.seed {
				mustExec(t, pool, sql, txID)
			}
			before := ledgerDump(t, pool)
			for _, g := range []grant.Grant{mustParse(t, "k", "alice", 5), mustParse(t, "k", "zoe", 5)} {
				if _, err := grant.Apply(ctx, pool, g); !errors.Is(err, grant.ErrCorrupt) {
					t.Errorf("Apply(%s, %s) = %v, want ErrCorrupt", g.Key(), g.Player(), err)
				}
			}
			if after := ledgerDump(t, pool); after != before {
				t.Errorf("a corrupt stored grant led to writes:\nbefore\n%s\nafter\n%s", before, after)
			}
		})
	}
}

func TestApplyKeepsLegacyFaucet(t *testing.T) {
	_, pool := newDB(t)
	var legacy [16]byte
	if err := pool.QueryRow(ctx, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
		VALUES (gen_random_uuid(), 'system', 'faucet', false) RETURNING account_id`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	mustApply(t, pool, mustParse(t, "alice-1", "alice", 500), grant.Applied)
	if f := faucetAccount(t, pool); f != (account{ID: legacy, Debits: 500}) {
		t.Errorf("faucet %+v, want the legacy faucet %x with debits 500", f, legacy)
	}
	requireCredits(t, pool, "alice", 500)
	requireBalanced(t, pool)
}

// legacyPlayers are IDs the former rules accepted (1 to 256 bytes of UTF-8
// without control characters) that sessions.ParsePlayerID refuses, so the
// gateway can never authenticate them.
var legacyPlayers = []string{"Alice", "Bob Smith ✓", "ünï", "a.b", "al:ice", strings.Repeat("x", 65), strings.Repeat("é", 128)}

// seedLegacyGrant stores a grant the way the former Apply did, whatever the
// spelling of the player: the transaction row, the accounts if absent, one
// faucet-to-player posting.
func seedLegacyGrant(t *testing.T, db *pgxpool.Pool, key, player string, credits int64) {
	t.Helper()
	txID := ledger.GrantTransactionID(key)
	mustExec(t, db, `INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`, txID)
	mustExec(t, db, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
		SELECT $1::uuid, 'system', 'faucet', false
		WHERE NOT EXISTS (SELECT 1 FROM orvex.ledger_accounts WHERE kind = 'system' AND name = 'faucet' AND round_id IS NULL)`,
		ledger.FaucetAccountID())
	mustExec(t, db, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced)
		SELECT gen_random_uuid(), 'player', $1::text, true
		WHERE NOT EXISTS (SELECT 1 FROM orvex.ledger_accounts WHERE player_id = $1::text)`, player)
	mustExec(t, db, `INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount)
		SELECT $1, 0, f.account_id, p.account_id, $3
		FROM orvex.ledger_accounts f, orvex.ledger_accounts p
		WHERE f.kind = 'system' AND f.name = 'faucet' AND p.player_id = $2`, txID, player, credits)
}

func TestApplyRefusesNewUnplayablePlayer(t *testing.T) {
	_, pool := newDB(t)
	mustApply(t, pool, mustParse(t, "alice-1", "alice", 500), grant.Applied)
	before := ledgerDump(t, pool)
	for i, p := range legacyPlayers {
		g := mustParse(t, fmt.Sprintf("new-%d", i), p, 5)
		if _, err := grant.Apply(ctx, pool, g); !errors.Is(err, grant.ErrInvalid) {
			t.Errorf("Apply(%q) = %v, want ErrInvalid", p, err)
		}
		if _, ok := playerAccount(t, pool, p); ok {
			t.Errorf("a refused grant created the account of %q", p)
		}
	}
	if after := ledgerDump(t, pool); after != before {
		t.Errorf("a refused grant wrote:\nbefore\n%s\nafter\n%s", before, after)
	}
	// The same key still funds a playable ID: nothing was reserved. So does
	// the longest playable ID, which the validator takes from the gateway.
	mustApply(t, pool, mustParse(t, "new-0", "alice", 5), grant.Applied)
	mustApply(t, pool, mustParse(t, "new-max", strings.Repeat("a", 64), 5), grant.Applied)
}

// TestApplyRefusesUnplayableOnEmptyLedger: not even the faucet is created.
func TestApplyRefusesUnplayableOnEmptyLedger(t *testing.T) {
	_, pool := newDB(t)
	if _, err := grant.Apply(ctx, pool, mustParse(t, "k", "Alice", 5)); !errors.Is(err, grant.ErrInvalid) {
		t.Fatalf("Apply = %v, want ErrInvalid", err)
	}
	if n := count(t, pool, `SELECT (SELECT count(*) FROM orvex.ledger_accounts) + (SELECT count(*) FROM orvex.ledger_transactions)
		+ (SELECT count(*) FROM orvex.ledger_postings)`); n != 0 {
		t.Errorf("%d ledger rows after a refused grant, want 0", n)
	}
}

func TestApplyLegacyGrantRetries(t *testing.T) {
	_, pool := newDB(t)
	for i, p := range legacyPlayers {
		seedLegacyGrant(t, pool, fmt.Sprintf("legacy-%d", i), p, 500)
	}
	// A playable account beside them, so ID spellings can be told apart.
	mustApply(t, pool, mustParse(t, "alice-1", "alice", 7), grant.Applied)
	before := ledgerDump(t, pool)

	for i, p := range legacyPlayers {
		key := fmt.Sprintf("legacy-%d", i)
		first, err := grant.Apply(ctx, pool, mustParse(t, key, p, 500))
		if err != nil || first.Status != grant.Existing {
			t.Fatalf("retry of %s for %q = %+v, %v; want existing", key, p, first, err)
		}
		a, ok := playerAccount(t, pool, p)
		if !ok || first.AccountID != a.ID {
			t.Errorf("retry of %s answered account %x, want the legacy account of %q", key, first.AccountID, p)
		}
		for _, other := range []grant.Grant{
			mustParse(t, key, p, 501),                                       // other credits
			mustParse(t, key, "alice", 500),                                 // a playable player
			mustParse(t, key, legacyPlayers[(i+1)%len(legacyPlayers)], 500), // another legacy spelling
			mustParse(t, key, strings.ToLower(p), 500),                      // the folded spelling
		} {
			if strings.ToLower(other.Player()) == p && other.Credits() == 500 {
				continue // lowercase of an already-lower ID is the same payload
			}
			_, err := grant.Apply(ctx, pool, other)
			var reused *grant.KeyReusedError
			if !errors.As(err, &reused) || *reused != (grant.KeyReusedError{Key: key, Player: p, Credits: 500}) {
				t.Errorf("Apply(%s, %q, %d) = %v, want KeyReusedError naming %q, 500", key, other.Player(), other.Credits(), err, p)
			}
		}
	}
	if after := ledgerDump(t, pool); after != before {
		t.Errorf("legacy retries wrote:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// TestApplyLegacySpellingNeverFolded: an old grant to "Alice" and a new
// grant to "alice" are two accounts; a new grant to "Alice" is refused.
func TestApplyLegacySpellingNeverFolded(t *testing.T) {
	_, pool := newDB(t)
	seedLegacyGrant(t, pool, "legacy", "Alice", 500)
	if _, err := grant.Apply(ctx, pool, mustParse(t, "again", "Alice", 5)); !errors.Is(err, grant.ErrInvalid) {
		t.Fatalf("a new grant to the undeliverable spelling = %v, want ErrInvalid", err)
	}
	mustApply(t, pool, mustParse(t, "fresh", "alice", 9), grant.Applied)
	upper, _ := playerAccount(t, pool, "Alice")
	if upper.Credits != 500 {
		t.Errorf("Alice credits %d, want 500", upper.Credits)
	}
	requireCredits(t, pool, "alice", 9)
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_accounts WHERE kind = 'player'`); n != 2 {
		t.Errorf("%d player accounts, want 2", n)
	}
	mustApply(t, pool, mustParse(t, "legacy", "Alice", 500), grant.Existing)
}

// TestApplyConcurrentLegacyAndUnplayable: concurrent callers of a stored
// legacy key and of a new unsupported one, released at once.
func TestApplyConcurrentLegacyAndUnplayable(t *testing.T) {
	_, pool := newDB(t)
	seedLegacyGrant(t, pool, "legacy", "Bob Smith", 500)
	before := ledgerDump(t, pool)
	var grants []grant.Grant
	for i := range 12 {
		switch i % 3 {
		case 0:
			grants = append(grants, mustParse(t, "legacy", "Bob Smith", 500))
		case 1:
			grants = append(grants, mustParse(t, "legacy", "Bob Smith", 501))
		default:
			grants = append(grants, mustParse(t, "brand-new", "Bob Smith", 500))
		}
	}
	results, errs := applyAll(t, pool, grants)
	for i, err := range errs {
		var reused *grant.KeyReusedError
		switch i % 3 {
		case 0:
			if err != nil || results[i].Status != grant.Existing {
				t.Errorf("call %d = %+v, %v; want existing", i, results[i], err)
			}
		case 1:
			if !errors.As(err, &reused) {
				t.Errorf("call %d = %v, want KeyReusedError", i, err)
			}
		default:
			if !errors.Is(err, grant.ErrInvalid) {
				t.Errorf("call %d = %v, want ErrInvalid", i, err)
			}
		}
	}
	if after := ledgerDump(t, pool); after != before {
		t.Errorf("concurrent calls wrote:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// TestApplyConcurrentPlayableAndUnplayableSameKey: one new key raced by a
// playable payload and an unsupported one. The playable payload is stored
// once; the unsupported one never writes, and answers ErrInvalid if it ran
// first and KeyReusedError if it ran after.
func TestApplyConcurrentPlayableAndUnplayableSameKey(t *testing.T) {
	_, pool := newDB(t)
	var grants []grant.Grant
	for i := range 10 {
		if i%2 == 0 {
			grants = append(grants, mustParse(t, "k", "alice", 100))
		} else {
			grants = append(grants, mustParse(t, "k", "Alice", 100))
		}
	}
	results, errs := applyAll(t, pool, grants)
	applied := 0
	for i, err := range errs {
		var reused *grant.KeyReusedError
		switch {
		case i%2 == 0 && err == nil && results[i].Status == grant.Applied:
			applied++
		case i%2 == 0 && err == nil && results[i].Status == grant.Existing:
		case i%2 == 1 && (errors.Is(err, grant.ErrInvalid) || errors.As(err, &reused)):
		default:
			t.Errorf("call %d (%q) = %+v, %v", i, grants[i].Player(), results[i], err)
		}
	}
	if applied != 1 {
		t.Errorf("%d calls applied, want 1", applied)
	}
	requireCredits(t, pool, "alice", 100)
	if _, ok := playerAccount(t, pool, "Alice"); ok {
		t.Error("the unsupported payload created an account")
	}
	requireBalanced(t, pool)
}

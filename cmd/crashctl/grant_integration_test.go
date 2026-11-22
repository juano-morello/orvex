//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	crashv1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/execution"
	"github.com/juano-morello/orvex/internal/runtime/grant"
)

// Integration tests of crashctl ledger grant against real PostgreSQL. Each
// test uses its own migrated database and sets ORVEX_DATABASE_URL, so none
// is parallel.

// ledgerOperatorGrants is the least privilege ledger grant needs: SELECT
// and INSERT on the three ledger tables, and UPDATE of the two posted
// totals, which the posting trigger updates as the invoking role. Nothing
// on rounds, secrets, admissions, heads, holds or the outbox. %[1]s is the
// role, %[2]s the database.
var ledgerOperatorGrants = []string{
	`GRANT CONNECT ON DATABASE %[2]s TO %[1]s`,
	`GRANT USAGE ON SCHEMA orvex TO %[1]s`,
	`GRANT SELECT, INSERT ON orvex.ledger_accounts, orvex.ledger_transactions, orvex.ledger_postings TO %[1]s`,
	`GRANT UPDATE (debits_posted, credits_posted) ON orvex.ledger_accounts TO %[1]s`,
}

type ledgerAccount struct {
	ID                    [16]byte
	Debits, Credits, Held int64
}

// ledgerAccountOf returns player's account; ok is false if it has none.
func ledgerAccountOf(t *testing.T, pool *pgxpool.Pool, player string) (a ledgerAccount, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT account_id, debits_posted, credits_posted, held
		FROM orvex.ledger_accounts WHERE player_id = $1`, player).Scan(&a.ID, &a.Debits, &a.Credits, &a.Held)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledgerAccount{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return a, true
}

// ledgerRows returns every ledger account, transaction and posting, so that
// two calls are equal iff nothing was written in between.
func ledgerRows(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(), `SELECT concat_ws(E'\n--\n',
		(SELECT string_agg(format('%s %s %s %s %s %s %s', account_id, kind, player_id, name, debits_posted, credits_posted, held),
			E'\n' ORDER BY account_id) FROM orvex.ledger_accounts),
		(SELECT string_agg(format('%s %s %s', transaction_id, kind, round_id), E'\n' ORDER BY transaction_id)
			FROM orvex.ledger_transactions),
		(SELECT string_agg(format('%s %s %s %s %s', transaction_id, posting_index, debit_account_id, credit_account_id, amount),
			E'\n' ORDER BY transaction_id, posting_index) FROM orvex.ledger_postings))`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// wantGrantReport is ledger grant's report for a grant stored with
// transaction ledger.GrantTransactionID(key) to account.
func wantGrantReport(key, player string, account [16]byte, credits int64, status string) string {
	return fmt.Sprintf("grant %s\ntransaction %s\nplayer %s\naccount %s\ncredits %d\nstatus %s\nok\n",
		key, canonjson.UUID(ledger.GrantTransactionID(key)), player, canonjson.UUID(account), credits, status)
}

// requireGranted runs ledger grant and requires exit 0 with the report of
// status.
func requireGranted(t *testing.T, pool *pgxpool.Pool, key, player string, credits int64, status string) {
	t.Helper()
	code, stdout, stderr := grantCmd(key, player, fmt.Sprint(credits))
	if code != exitOK || stderr != "" {
		t.Fatalf("ledger grant %s %s %d: exit %d, stderr %q; want 0", key, player, credits, code, stderr)
	}
	a, ok := ledgerAccountOf(t, pool, player)
	if !ok {
		t.Fatalf("ledger grant %s: %s has no account", key, player)
	}
	if want := wantGrantReport(key, player, a.ID, credits, status); stdout != want {
		t.Errorf("report =\n%s\nwant\n%s", stdout, want)
	}
}

func TestGrantCLI(t *testing.T) {
	_, pool := exportDatabase(t)
	requireGranted(t, pool, "alice-1", "alice", 500, "applied")
	before := ledgerRows(t, pool)
	requireGranted(t, pool, "alice-1", "alice", 500, "existing")
	if after := ledgerRows(t, pool); after != before {
		t.Errorf("rerun wrote:\nbefore\n%s\nafter\n%s", before, after)
	}
	if a, _ := ledgerAccountOf(t, pool, "alice"); a.Credits != 500 || a.Debits != 0 {
		t.Errorf("alice %+v, want credits 500, debits 0", a)
	}

	const reused = "crashctl: ledger grant: grant key alice-1 was used for player \"alice\", credits 500; " +
		"choose a new grant key for another grant\n"
	for _, args := range [][]string{{"alice-1", "alice", "501"}, {"alice-1", "bob", "500"}} {
		code, stdout, stderr := grantCmd(args...)
		if code != exitGrantKeyReused || stdout != "" || stderr != reused {
			t.Errorf("ledger grant %v: exit %d, stdout %q, stderr %q; want %d, empty and %q",
				args, code, stdout, stderr, exitGrantKeyReused, reused)
		}
	}
	if after := ledgerRows(t, pool); after != before {
		t.Errorf("a reused grant key wrote:\nbefore\n%s\nafter\n%s", before, after)
	}
	if _, ok := ledgerAccountOf(t, pool, "bob"); ok {
		t.Error("a reused grant key created bob's account")
	}

	requireGranted(t, pool, "alice-2", "alice", 250, "applied")
	requireGranted(t, pool, "bob-1", "Bob Smith ✓", 100, "applied")
	if a, _ := ledgerAccountOf(t, pool, "alice"); a.Credits != 750 {
		t.Errorf("alice credits %d after two grants, want 750", a.Credits)
	}
}

func TestGrantCLICorruptStoredGrant(t *testing.T) {
	_, pool := exportDatabase(t)
	execOrFatal(t, pool, `INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`,
		ledger.GrantTransactionID("alice-1"))
	before := ledgerRows(t, pool)
	code, stdout, stderr := grantCmd("alice-1", "alice", "500")
	want := "crashctl: ledger grant: grant: the stored ledger transaction with the grant's ID is not a faucet-to-player grant: " +
		"transaction " + string(canonjson.UUID(ledger.GrantTransactionID("alice-1"))) + "\n"
	if code != exitFailed || stdout != "" || stderr != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want %d, empty and %q", code, stdout, stderr, exitFailed, want)
	}
	if after := ledgerRows(t, pool); after != before {
		t.Errorf("a corrupt stored grant led to writes:\n%s", after)
	}
}

func TestGrantCLIReportWriteFailure(t *testing.T) {
	_, pool := exportDatabase(t)
	var stderr bytes.Buffer
	code := run([]string{"ledger", "grant", "alice-1", "alice", "500"}, strings.NewReader(""), failingWriter{}, &stderr)
	if want := "crashctl: ledger grant: write report: write failed; the grant is applied\n"; code != exitIO || stderr.String() != want {
		t.Errorf("exit %d, stderr %q; want %d and %q", code, stderr.String(), exitIO, want)
	}
	requireGranted(t, pool, "alice-1", "alice", 500, "existing")
}

func TestGrantCLIRoles(t *testing.T) {
	dsn, pool := exportDatabase(t)
	database := databaseName(t, dsn)
	var hostPort string
	withURL(t, dsn, func(u *url.URL) { hostPort = u.Host })
	before := ledgerRows(t, pool)

	tests := []struct {
		name   string
		grants []string
	}{
		{"read only", ledgerOperatorGrants[:2:2]},
		{"no UPDATE of the totals", ledgerOperatorGrants[:3:3]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grants := append(tt.grants, `GRANT SELECT ON orvex.ledger_accounts, orvex.ledger_transactions, orvex.ledger_postings TO %[1]s`)
			role := fmt.Sprintf("orvex_export_%d", exportRoles.Add(1))
			roleDSN := loginRole(t, dsn, role, exportRolePassword, grants)
			t.Setenv(databaseURLEnv, roleDSN)
			code, stdout, stderr := grantCmd("alice-1", "alice", "500")
			if code != exitIO || stdout != "" || !strings.Contains(stderr, "SQLSTATE 42501; rerunning the same command is safe\n") {
				t.Errorf("exit %d, stdout %q, stderr %q; want %d with SQLSTATE 42501", code, stdout, stderr, exitIO)
			}
			requireOneGrantLine(t, stderr)
			for _, s := range []string{role, exportRolePassword, database, hostPort, roleDSN} {
				if strings.Contains(stderr, s) {
					t.Errorf("stderr %q contains %q", stderr, s)
				}
			}
			if after := ledgerRows(t, pool); after != before {
				t.Errorf("a refused grant wrote:\n%s", after)
			}
		})
	}

	t.Run("least privilege", func(t *testing.T) {
		t.Setenv(databaseURLEnv, restrictedRole(t, dsn, ledgerOperatorGrants))
		requireGranted(t, pool, "alice-1", "alice", 500, "applied")
		requireGranted(t, pool, "alice-1", "alice", 500, "existing")
		requireGranted(t, pool, "alice-2", "alice", 1, "applied")
		if code, _, stderr := grantCmd("alice-1", "alice", "9"); code != exitGrantKeyReused {
			t.Errorf("reused grant key: exit %d, stderr %q; want %d", code, stderr, exitGrantKeyReused)
		}
	})
}

// TestGrantFundsRound funds the worked-example round's players with ledger
// grant, plays it, and audits it: the round replays to the golden head, its
// v2 export verifies with the ledger stage and carries neither the faucet
// nor a grant, each balance is its grants plus its round postings, and a
// later grant leaves both exports byte-identical.
func TestGrantFundsRound(t *testing.T) {
	ctx := context.Background()
	_, pool := exportDatabase(t)
	reg, err := newRegistry()
	if err != nil {
		t.Fatal(err)
	}
	x := roundtest.NewExecutor(t, pool, nil, reg)
	header, lines := roundtest.ReadGolden(t, filepath.Join(goldenDir, "worked-example.jsonl"))
	genesis, cfg := roundtest.GoldenConfig(t, header, lines[0])
	if _, err := x.CreateRound(ctx, execution.NewRound{
		RoundID: genesis.RoundID, Game: genesis.Game, RulesVersion: genesis.RulesVersion, Config: cfg,
		CreatedAtUnixMicro: genesis.CreatedAtUnixMicro,
	}); err != nil {
		t.Fatal(err)
	}
	g := roundtest.Golden{RoundID: genesis.RoundID, Header: header, Lines: lines, Executor: x}
	holds := map[string]int64{}
	for _, line := range lines[1:] {
		rec, err := chain.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		c := rec.(chain.Command)
		g.Commands = append(g.Commands, c)
		cmd, err := crashv1.Adapter().DecodeCommand(c.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if h := roundtest.HoldFor(cmd); h != nil {
			holds[h.Player] += h.Credits
		}
	}
	var players []string
	for p := range holds {
		players = append(players, p)
	}
	sort.Strings(players)

	// ledger grant refuses a player ID with a control character, which
	// crash/v1 accepts: the worked example's P3 holds U+0001 (LBT-9 §5.6).
	// The test helper funds it the same way, from the faucet outside the
	// round.
	granted := map[string]int64{}
	var keys []string
	for i, p := range players {
		granted[p] += holds[p]
		key := fmt.Sprintf("stake-%d", i)
		if _, err := grant.Parse(key, p, fmt.Sprint(holds[p])); err != nil {
			if code, _, stderr := grantCmd(key, p, fmt.Sprint(holds[p])); code != exitUsage {
				t.Fatalf("ledger grant %q: exit %d, stderr %q; want %d", p, code, stderr, exitUsage)
			}
			roundtest.FundPlayer(t, pool, p, holds[p])
			continue
		}
		requireGranted(t, pool, key, p, holds[p], "applied")
		keys = append(keys, key)
	}
	if len(keys) < 2 {
		t.Fatalf("ledger grant funded %d players, want at least two", len(keys))
	}
	roundtest.SeedGoldenAdmissions(t, pool, g, 1, 0)

	// A grant while the round's holds are open adds to what is available.
	requireGranted(t, pool, "alice-bonus", "alice", 1000, "applied")
	granted["alice"] += 1000
	if a, _ := ledgerAccountOf(t, pool, "alice"); a.Held != holds["alice"] || a.Credits-a.Debits-a.Held != 1000 {
		t.Errorf("alice with open holds %+v, want held %d and 1000 available", a, holds["alice"])
	}

	if _, err := x.Drain(ctx, g.Lease(), 100); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	head := g.Hash(21).String()
	dir := t.TempDir()
	v1Path := filepath.Join(dir, "round.jsonl")
	if code, stdout, stderr := exportCmd(t, g.UUID(), v1Path); code != exitOK ||
		stdout != wantExportReport(g.UUID(), 21, 22, head, settledYes) {
		t.Fatalf("audit export: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	verifyFile(t, v1Path, 22, head)

	exportV2 := func() []byte {
		t.Helper()
		code, history, report := exportCmd(t, "-format", "orvex/audit-history/v2", g.UUID(), "-")
		if code != exitOK {
			t.Fatalf("audit export v2: exit %d, report %q", code, report)
		}
		return []byte(history)
	}
	v2 := exportV2()
	v2Path := filepath.Join(dir, "round-v2.jsonl")
	if err := os.WriteFile(v2Path, v2, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runAuditCmd(t, strings.NewReader(""), "verify", "-expect-head", head, v2Path)
	if code != exitOK || !strings.Contains(stdout, "\nledger ok ") {
		t.Fatalf("audit verify v2: exit %d, stdout %q, stderr %q; want 0 with the ledger stage", code, stdout, stderr)
	}
	if bytes.Contains(v2, []byte(canonjson.UUID(ledger.FaucetAccountID()))) {
		t.Error("the v2 export names the faucet account")
	}
	for _, k := range append(keys, "alice-bonus") {
		if bytes.Contains(v2, []byte(canonjson.UUID(ledger.GrantTransactionID(k)))) {
			t.Errorf("the v2 export names grant %s", k)
		}
	}

	for _, p := range players {
		a, _ := ledgerAccountOf(t, pool, p)
		var roundNet int64
		if err := pool.QueryRow(ctx, `SELECT coalesce(sum(CASE WHEN p.credit_account_id = $2 THEN p.amount ELSE -p.amount END), 0)
			FROM orvex.ledger_postings p JOIN orvex.ledger_transactions t ON t.transaction_id = p.transaction_id
			WHERE t.round_id = $1 AND $2 IN (p.debit_account_id, p.credit_account_id)`, g.RoundID, a.ID).Scan(&roundNet); err != nil {
			t.Fatal(err)
		}
		if a.Held != 0 || a.Credits-a.Debits != granted[p]+roundNet {
			t.Errorf("%s %+v: want held 0 and balance granted %d + round net %d", p, a, granted[p], roundNet)
		}
	}

	requireGranted(t, pool, "bob-after", "bob", 77, "applied")
	if !bytes.Equal(exportV2(), v2) {
		t.Error("a grant after settlement changed the round's v2 export")
	}
	again := filepath.Join(dir, "again.jsonl")
	if code, _, stderr := exportCmd(t, g.UUID(), again); code != exitOK {
		t.Fatalf("audit export after the grant: exit %d, stderr %q", code, stderr)
	}
	first, err := os.ReadFile(v1Path) //nolint:gosec // G304: a file this test wrote under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(again) //nolint:gosec // G304: a file this test wrote under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("a grant after settlement changed the round's v1 export")
	}
}

// TestGrantCLILockTimeout: a held account lock fails the grant with exit 4
// after the lock timeout, and the rerun applies it.
func TestGrantCLILockTimeout(t *testing.T) {
	ctx := context.Background()
	dsn, pool := exportDatabase(t)
	requireGranted(t, pool, "alice-1", "alice", 500, "applied")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	execOrFatal(t, tx, `SELECT 1 FROM orvex.ledger_accounts WHERE player_id = 'alice' FOR UPDATE`)
	before := ledgerRows(t, pool)

	code, stdout, stderr := grantCmd("alice-2", "alice", "5")
	if want := "crashctl: ledger grant: grant: insert posting: SQLSTATE 55P03; rerunning the same command is safe\n"; code != exitIO ||
		stdout != "" || stderr != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want %d and %q", code, stdout, stderr, exitIO, want)
	}
	if after := ledgerRows(t, pool); after != before {
		t.Errorf("a lock timeout wrote:\n%s", after)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	requireGranted(t, pool, "alice-2", "alice", 5, "applied")
}

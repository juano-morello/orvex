//go:build integration

package execution_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// Integration tests of the executor (LBT-12 spec E-17, §11, §12 steps 5
// and 6). Each test uses its own migrated database. Concurrency tests do
// not call t.Parallel(); blocking is detected through pg_stat_activity,
// never with sleeps.

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var ctx = context.Background()

const goldenDir = "../../audit/verification/testdata/golden"

// newDB returns a fresh migrated database's DSN and a pool on it.
func newDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.MigratedDatabase(t)
	return dsn, pgtest.Pool(t, dsn)
}

// crashRegistry returns a registry holding crash/v1 only.
func crashRegistry(t *testing.T) *game.Registry {
	t.Helper()
	reg, err := game.NewRegistry(v1.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// newExecutor returns an executor over db with EngineBuild "golden" unless
// mutate changes it; reg nil means crash/v1 only.
func newExecutor(t *testing.T, db execution.DB, mutate func(*execution.Config), reg *game.Registry) *execution.Executor {
	t.Helper()
	cfg := execution.DefaultConfig()
	cfg.EngineBuild = "golden"
	if mutate != nil {
		mutate(&cfg)
	}
	if reg == nil {
		reg = crashRegistry(t)
	}
	x, err := execution.New(db, reg, cfg, execution.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func lease(roundID [16]byte) execution.Lease { return execution.Lease{RoundID: roundID} }

var idCounter atomic.Uint64

// cmdID returns a deterministic non-nil UUID for n.
func cmdID(n int) [16]byte {
	var id [16]byte
	id[0], id[6] = 0x0c, 0x70
	binary.BigEndian.PutUint64(id[8:], uint64(n))
	return id
}

// newRoundID returns a fresh non-nil round UUID.
func newRoundID() [16]byte {
	var id [16]byte
	id[0], id[6] = 0x0e, 0x70
	binary.BigEndian.PutUint64(id[8:], idCounter.Add(1))
	return id
}

// roundConfig is the config of a fresh test round.
func roundConfig(roundID [16]byte, seed [32]byte, maxBet, maxBets int64) v1.Config {
	return v1.Config{RoundID: roundID, Seed: seed, MinBetCredits: 1, MaxBetCredits: maxBet, MaxBets: maxBets}
}

// seedWithDeadline scans 32-byte big-endian counters from 1 (the all-zero
// seed is invalid) until crash/v1 gives min <= D <= max µs.
func seedWithDeadline(t *testing.T, roundID [16]byte, minD, maxD time.Duration) [32]byte {
	t.Helper()
	for n := uint64(1); n <= 100_000; n++ {
		var seed [32]byte
		binary.BigEndian.PutUint64(seed[24:], n)
		s, err := v1.Rules{}.InitialState(roundConfig(roundID, seed, 1_000_000_000, 100))
		if err != nil {
			continue
		}
		if d := s.CrashDeadlineOffsetMicros; d >= minD.Microseconds() && d <= maxD.Microseconds() {
			return seed
		}
	}
	t.Fatalf("no seed with D in [%v, %v]", minD, maxD)
	return [32]byte{}
}

// testSeed is a valid seed for rounds whose timing does not matter.
var testSeed = func() [32]byte {
	var s [32]byte
	for i := range s {
		s[i] = 0xab
	}
	return s
}()

// createRound creates a crash/v1 round with x and returns its config.
func createRound(t *testing.T, x *execution.Executor, roundID [16]byte, seed [32]byte, maxBet, maxBets int64) v1.Config {
	t.Helper()
	cfg := roundConfig(roundID, seed, maxBet, maxBets)
	if _, err := x.CreateRound(ctx, execution.NewRound{
		RoundID: roundID, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config: cfg, CreatedAtUnixMicro: time.Now().UnixMicro(),
	}); err != nil {
		t.Fatalf("CreateRound: %v", err)
	}
	return cfg
}

func mustExec(t *testing.T, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// inTx runs fn in one transaction and commits.
func inTx(t *testing.T, db *pgxpool.Pool, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.ExecutionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fn(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// fundPlayer creates the player's account if absent and grants it credits
// from the global system account faucet, which it creates if absent.
func fundPlayer(t *testing.T, db *pgxpool.Pool, player string, credits int64) {
	t.Helper()
	inTx(t, db, func(tx pgx.Tx) {
		mustExec(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
			SELECT gen_random_uuid(), 'system', 'faucet', false
			WHERE NOT EXISTS (SELECT 1 FROM orvex.ledger_accounts WHERE kind = 'system' AND name = 'faucet' AND round_id IS NULL)`)
		mustExec(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced)
			VALUES (gen_random_uuid(), 'player', $1, true) ON CONFLICT (player_id) DO NOTHING`, player)
		txID := newRoundID()
		mustExec(t, tx, `INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`, txID)
		mustExec(t, tx, `INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount)
			SELECT $1, 0, f.account_id, p.account_id, $3
			FROM orvex.ledger_accounts f, orvex.ledger_accounts p
			WHERE f.kind = 'system' AND f.name = 'faucet' AND f.round_id IS NULL AND p.player_id = $2`, txID, player, credits)
	})
}

// account is a ledger account's totals.
type account struct{ debits, credits, held int64 }

func (a account) balance() int64 { return a.credits - a.debits }

func playerAccount(t *testing.T, db *pgxpool.Pool, player string) account {
	t.Helper()
	var a account
	if err := db.QueryRow(ctx, `SELECT debits_posted, credits_posted, held FROM orvex.ledger_accounts WHERE player_id = $1`, player).
		Scan(&a.debits, &a.credits, &a.held); err != nil {
		t.Fatalf("account of %q: %v", player, err)
	}
	return a
}

func accountByID(t *testing.T, db *pgxpool.Pool, id [16]byte) account {
	t.Helper()
	var a account
	if err := db.QueryRow(ctx, `SELECT debits_posted, credits_posted, held FROM orvex.ledger_accounts WHERE account_id = $1`, id).
		Scan(&a.debits, &a.credits, &a.held); err != nil {
		t.Fatalf("account %x: %v", id, err)
	}
	return a
}

// count runs a count query.
func count(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// holdSpec is a credit hold placed with a seeded admission.
type holdSpec struct {
	player  string
	credits int64
}

// holdFor applies LBT-11 S-10 to cmd: the hold a player PlaceBet carries.
func holdFor(cmd any) *holdSpec {
	cl, err := admission.CrashV1Policy().Classify(cmd)
	if err != nil || cl.Hold.PlayerID == "" {
		return nil
	}
	return &holdSpec{player: cl.Hold.PlayerID, credits: cl.Hold.Credits}
}

// encode encodes a crash/v1 command.
func encode(t *testing.T, cmd v1.Command) []byte {
	t.Helper()
	b, err := v1.Adapter().EncodeCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seedAdmission inserts the next admission directly, as LBT-11 does: under
// the admission head lock, with the hold (if any) in the same transaction.
// It returns the sequence.
func seedAdmission(t *testing.T, db *pgxpool.Pool, roundID, commandID [16]byte, command []byte, effectiveTime int64, hold *holdSpec) uint64 {
	t.Helper()
	var seq int64
	inTx(t, db, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT admitted_sequence FROM orvex.round_admission_heads WHERE round_id = $1 FOR UPDATE`,
			roundID).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		seq++
		mustExec(t, tx, `INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
			VALUES ($1, $2, $3, $4, $5)`, roundID, seq, commandID, effectiveTime, command)
		if hold != nil {
			mustExec(t, tx, `INSERT INTO orvex.ledger_holds (round_id, sequence, account_id, amount)
				SELECT $1, $2, account_id, $4 FROM orvex.ledger_accounts WHERE kind = 'player' AND player_id = $3`,
				roundID, seq, hold.player, hold.credits)
		}
		mustExec(t, tx, `UPDATE orvex.round_admission_heads SET admitted_sequence = $2 WHERE round_id = $1`, roundID, seq)
	})
	return uint64(seq)
}

// seed seeds cmd with its S-10 hold.
func seed(t *testing.T, db *pgxpool.Pool, roundID [16]byte, n int, cmd v1.Command, effectiveTime int64) uint64 {
	t.Helper()
	return seedAdmission(t, db, roundID, cmdID(n), encode(t, cmd), effectiveTime, holdFor(cmd))
}

func placeBet(player string, credits int64) v1.PlaceBet {
	return v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: v1.PlayerID(player), BetCredits: credits}
}

func cashOut(player string) v1.CashOut {
	return v1.CashOut{Authority: v1.AuthorityPlayer, Player: v1.PlayerID(player)}
}

var (
	startRound  = v1.StartRound{Authority: v1.AuthoritySystem}
	closeRound  = v1.CloseRound{Authority: v1.AuthoritySystem}
	settleRound = v1.SettleRound{Authority: v1.AuthoritySystem}
)

// --- Golden histories ---

// goldenRound is a golden history seeded into a database.
type goldenRound struct {
	roundID [16]byte
	// lines are the record lines 0..N (without the header).
	lines  [][]byte
	header canonjson.Object
	cmds   []chain.Command
}

func readGolden(t *testing.T, name string) (canonjson.Object, [][]byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	header, err := canonjson.Unmarshal(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	return header, lines[1:]
}

// goldenConfig returns the golden round's genesis and typed config.
func goldenConfig(t *testing.T, header canonjson.Object, genesisLine []byte) (chain.Genesis, v1.Config) {
	t.Helper()
	rec, err := chain.Decode(genesisLine)
	if err != nil {
		t.Fatal(err)
	}
	g := rec.(chain.Genesis)
	so, err := canonjson.Obj(header["configSecret"])
	if err != nil {
		t.Fatal(err)
	}
	secret, err := canonjson.Marshal(so)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := v1.Adapter().DecodeConfig(g.RoundID, g.Config, secret)
	if err != nil {
		t.Fatal(err)
	}
	return g, cfg.(v1.Config)
}

// seedGolden creates the golden round with x, funds each player with the
// sum of their holds, and seeds every admission from the golden records,
// up to admission upTo (0: all).
func seedGolden(t *testing.T, db *pgxpool.Pool, x *execution.Executor, name string, upTo int) goldenRound {
	t.Helper()
	header, lines := readGolden(t, name)
	g, cfg := goldenConfig(t, header, lines[0])
	created, err := x.CreateRound(ctx, execution.NewRound{
		RoundID: g.RoundID, Game: g.Game, RulesVersion: g.RulesVersion, Config: cfg, CreatedAtUnixMicro: g.CreatedAtUnixMicro,
	})
	if err != nil {
		t.Fatalf("CreateRound(%s): %v", name, err)
	}
	if !bytes.Equal(created.Genesis, lines[0]) {
		t.Fatalf("%s genesis differs:\n got %s\nwant %s", name, created.Genesis, lines[0])
	}
	gr := goldenRound{roundID: g.RoundID, lines: lines, header: header}
	funds := map[string]int64{}
	for _, line := range lines[1:] {
		rec, err := chain.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		c := rec.(chain.Command)
		gr.cmds = append(gr.cmds, c)
		if h := goldenHold(t, c); h != nil {
			funds[h.player] += h.credits
		}
	}
	players := make([]string, 0, len(funds))
	for p := range funds {
		players = append(players, p)
	}
	sort.Strings(players)
	for _, p := range players {
		fundPlayer(t, db, p, funds[p])
	}
	seedGoldenAdmissions(t, db, gr, 1, upTo)
	return gr
}

// seedGoldenAdmissions seeds golden admissions from..to (to 0: all).
func seedGoldenAdmissions(t *testing.T, db *pgxpool.Pool, gr goldenRound, from, to int) {
	t.Helper()
	if to == 0 {
		to = len(gr.cmds)
	}
	for i := from; i <= to; i++ {
		c := gr.cmds[i-1]
		if seq := seedAdmission(t, db, gr.roundID, c.CommandID, c.Payload, c.EffectiveTimeUnixMicro, goldenHold(t, c)); seq != c.Sequence {
			t.Fatalf("seeded sequence %d, want %d", seq, c.Sequence)
		}
	}
}

func goldenHold(t *testing.T, c chain.Command) *holdSpec {
	t.Helper()
	cmd, err := v1.Adapter().DecodeCommand(c.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return holdFor(cmd)
}

// --- Effects ---

// effectSet captures everything Transaction B writes for a round (E-17):
// the head, the round's row counts and the totals of every ledger account,
// keyed by playerKey or the system account's name and round.
type effectSet struct {
	processed, records, events, transactions, postings, releases, outbox, snapshots int64
	accounts                                                                        map[string]account
}

// playerKey and roundPoolKey are effectSet.accounts keys: player accounts
// by player ID, system accounts by name and round (or "global").
func playerKey(player string) string { return "player:" + player }

func roundPoolKey(roundID [16]byte) string {
	return "system:round_pool:" + string(canonjson.UUID(roundID))
}

func effects(t *testing.T, db *pgxpool.Pool, roundID [16]byte) effectSet {
	t.Helper()
	var s effectSet
	if err := db.QueryRow(ctx, `SELECT
		(SELECT processed_sequence FROM orvex.round_execution_heads WHERE round_id = $1),
		(SELECT count(*) FROM orvex.chain_records WHERE round_id = $1),
		(SELECT count(*) FROM orvex.domain_events WHERE round_id = $1),
		(SELECT count(*) FROM orvex.ledger_transactions WHERE round_id = $1),
		(SELECT count(*) FROM orvex.ledger_postings WHERE round_id = $1),
		(SELECT count(*) FROM orvex.ledger_hold_releases WHERE round_id = $1),
		(SELECT count(*) FROM orvex.outbox_messages WHERE round_id = $1),
		(SELECT count(*) FROM orvex.round_snapshots WHERE round_id = $1)`, roundID).
		Scan(&s.processed, &s.records, &s.events, &s.transactions, &s.postings, &s.releases, &s.outbox, &s.snapshots); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx, `SELECT
		CASE WHEN kind = 'player' THEN 'player:' || player_id
			ELSE 'system:' || name || ':' || coalesce(round_id::text, 'global') END,
		debits_posted, credits_posted, held
		FROM orvex.ledger_accounts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	s.accounts = map[string]account{}
	for rows.Next() {
		var k string
		var a account
		if err := rows.Scan(&k, &a.debits, &a.credits, &a.held); err != nil {
			t.Fatal(err)
		}
		s.accounts[k] = a
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return s
}

// clone returns a deep copy of e.
func (e effectSet) clone() effectSet {
	out := e
	out.accounts = make(map[string]account, len(e.accounts))
	for k, v := range e.accounts {
		out.accounts[k] = v
	}
	return out
}

func requireEffects(t *testing.T, got, want effectSet) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effects = %+v\nwant      %+v", got, want)
	}
}

// --- Failure injection (E-17 item 2) ---

// installFaults creates schema orvex_test: a fault budget sequence, a
// fault_config table and the fault trigger function.
func installFaults(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	mustExec(t, db, `CREATE SCHEMA orvex_test`)
	mustExec(t, db, `CREATE SEQUENCE orvex_test.fault_budget`)
	mustExec(t, db, `CREATE TABLE orvex_test.fault_config (
		target text PRIMARY KEY, sqlstate text NOT NULL, constraint_name text, budget bigint NOT NULL)`)
	mustExec(t, db, `CREATE FUNCTION orvex_test.raise_fault() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
	c orvex_test.fault_config%ROWTYPE;
BEGIN
	SELECT * INTO c FROM orvex_test.fault_config WHERE target = TG_TABLE_NAME;
	IF FOUND AND nextval('orvex_test.fault_budget') <= c.budget THEN
		IF c.constraint_name IS NULL THEN
			RAISE EXCEPTION 'injected fault' USING ERRCODE = c.sqlstate;
		END IF;
		RAISE EXCEPTION 'injected fault' USING ERRCODE = c.sqlstate, CONSTRAINT = c.constraint_name;
	END IF;
	IF TG_WHEN = 'AFTER' THEN
		RETURN NULL;
	END IF;
	RETURN NEW;
END
$$`)
}

// enableFault makes inserts into orvex.<table> raise sqlstate (with
// constraint unless empty) for the next budget inserts.
func enableFault(t *testing.T, db *pgxpool.Pool, table, sqlstate, constraint string, budget int64) {
	t.Helper()
	var c *string
	if constraint != "" {
		c = &constraint
	}
	mustExec(t, db, `SELECT setval('orvex_test.fault_budget', 1, false)`)
	mustExec(t, db, `INSERT INTO orvex_test.fault_config (target, sqlstate, constraint_name, budget) VALUES ($1, $2, $3, $4)
		ON CONFLICT (target) DO UPDATE SET sqlstate = EXCLUDED.sqlstate, constraint_name = EXCLUDED.constraint_name, budget = EXCLUDED.budget`,
		table, sqlstate, c, budget)
	mustExec(t, db, fmt.Sprintf(`CREATE OR REPLACE TRIGGER orvex_test_fault BEFORE INSERT ON orvex.%s
		FOR EACH ROW EXECUTE FUNCTION orvex_test.raise_fault()`, pgx.Identifier{table}.Sanitize()))
}

// enableCommitFault makes every COMMIT that inserted an outbox message fail
// with sqlstate.
func enableCommitFault(t *testing.T, db *pgxpool.Pool, sqlstate string) {
	t.Helper()
	mustExec(t, db, `SELECT setval('orvex_test.fault_budget', 1, false)`)
	mustExec(t, db, `INSERT INTO orvex_test.fault_config (target, sqlstate, constraint_name, budget) VALUES ('outbox_messages', $1, NULL, 1000000)
		ON CONFLICT (target) DO UPDATE SET sqlstate = EXCLUDED.sqlstate, constraint_name = NULL, budget = EXCLUDED.budget`, sqlstate)
	mustExec(t, db, `CREATE CONSTRAINT TRIGGER orvex_test_commit_fault AFTER INSERT ON orvex.outbox_messages
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION orvex_test.raise_fault()`)
}

// disableFaults removes every injected fault.
func disableFaults(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	mustExec(t, db, `DELETE FROM orvex_test.fault_config`)
}

// --- faultDB (E-17 item 1) ---

// hook runs at a point of Transaction B. It may return an error to inject
// instead of the real call.
type hook func(tx pgx.Tx) error

// faultDB wraps a pool; transactions begun with Transaction B's options run
// the hooks. It counts BeginTx calls.
type faultDB struct {
	pool                                 *pgxpool.Pool
	mu                                   sync.Mutex
	beforeCAS, beforeBatch, beforeCommit hook
	afterCommit                          hook
	begins                               atomic.Int64
	commits                              atomic.Int64
}

func (f *faultDB) set(fn func(f *faultDB)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *faultDB) hooks() (cas, batch, before, after hook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beforeCAS, f.beforeBatch, f.beforeCommit, f.afterCommit
}

func (f *faultDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	f.begins.Add(1)
	tx, err := f.pool.BeginTx(ctx, opts)
	if err != nil || opts.AccessMode != pgx.ReadWrite {
		return tx, err
	}
	return &faultTx{Tx: tx, db: f}, nil
}

func (f *faultDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return f.pool.Query(ctx, sql, args...)
}

func (f *faultDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return f.pool.QueryRow(ctx, sql, args...)
}

type faultTx struct {
	pgx.Tx
	db *faultDB
}

func (tx *faultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if cas, _, _, _ := tx.db.hooks(); cas != nil {
		if err := cas(tx.Tx); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *faultTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	if _, batch, _, _ := tx.db.hooks(); batch != nil {
		if err := batch(tx.Tx); err != nil {
			return errBatch{err: err}
		}
	}
	return tx.Tx.SendBatch(ctx, b)
}

func (tx *faultTx) Commit(ctx context.Context) error {
	_, _, before, after := tx.db.hooks()
	if before != nil {
		if err := before(tx.Tx); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
	}
	err := tx.Tx.Commit(ctx)
	if err == nil {
		tx.db.commits.Add(1)
	}
	if err == nil && after != nil {
		return after(tx.Tx)
	}
	return err
}

type errBatch struct{ err error }

func (b errBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, b.err }
func (b errBatch) Query() (pgx.Rows, error)         { return nil, b.err }
func (b errBatch) QueryRow() pgx.Row                { return errRow(b) }
func (b errBatch) Close() error                     { return b.err }

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// backendPID returns the PID of tx's backend.
func backendPID(tx pgx.Tx) uint32 { return tx.Conn().PgConn().PID() }

// takeover performs LBT-14's ownership takeover and returns the new token.
func takeover(t *testing.T, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, roundID [16]byte, owner string) int64 {
	t.Helper()
	var token int64
	if err := db.QueryRow(ctx, `UPDATE orvex.round_execution_heads
		SET fencing_token = fencing_token + 1, owner_id = $2, lease_expires_at = now() + interval '30 seconds'
		WHERE round_id = $1 RETURNING fencing_token`, roundID, owner).Scan(&token); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	return token
}

// --- failingAdapter ---

// failingAdapter is the crash/v1 adapter whose Decide fails closed at
// failDecide and whose Evolve fails at failEvolve (0: never).
type failingAdapter struct {
	game.Adapter
	failDecide, failEvolve uint64
	evolving               *atomic.Uint64
}

var errBoom = errors.New("boom")

func newFailingAdapter(failDecide, failEvolve uint64) failingAdapter {
	return failingAdapter{Adapter: v1.Adapter(), failDecide: failDecide, failEvolve: failEvolve, evolving: new(atomic.Uint64)}
}

func (a failingAdapter) Decide(state, command any, in game.Input) ([]any, error) {
	if in.Sequence == a.failDecide {
		return nil, errBoom
	}
	a.evolving.Store(in.Sequence)
	return a.Adapter.Decide(state, command, in)
}

func (a failingAdapter) Evolve(state, event any) (any, error) {
	if a.failEvolve != 0 && a.evolving.Load() == a.failEvolve {
		return nil, errBoom
	}
	return a.Adapter.Evolve(state, event)
}

func failingRegistry(t *testing.T, a failingAdapter) *game.Registry {
	t.Helper()
	reg, err := game.NewRegistry(a)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// --- Misc ---

// dbNowMicro returns the database clock in µs.
func dbNowMicro(t *testing.T, db *pgxpool.Pool) int64 {
	t.Helper()
	var now int64
	if err := db.QueryRow(ctx, `SELECT orvex.unix_micro(clock_timestamp())`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// waitDBClock polls the database clock until it is at least at.
func waitDBClock(t *testing.T, db *pgxpool.Pool, at int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for dbNowMicro(t, db) < at {
		if time.Now().After(deadline) {
			t.Fatalf("database clock did not reach %d", at)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// exportHistory exports the round and returns the file.
func exportHistory(t *testing.T, db *pgxpool.Pool, roundID [16]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := eventstore.ExportHistory(ctx, db, roundID, &buf); err != nil {
		t.Fatalf("ExportHistory: %v", err)
	}
	return buf.Bytes()
}

// loadHistory loads a history with pgtest.LoadHistory (token 0, every
// header checkpoint) and returns its round ID.
func loadHistory(t *testing.T, db *pgxpool.Pool, history []byte) [16]byte {
	t.Helper()
	return pgtest.LoadHistory(t, db, history, nil).RoundID
}

func canonMarshal(o canonjson.Object) ([]byte, error) { return canonjson.Marshal(o) }

// alterCheckpoint returns a copy of a history header whose checkpoint at
// seq has a different state hash.
func alterCheckpoint(t *testing.T, header canonjson.Object, seq uint64) canonjson.Object {
	t.Helper()
	list, err := canonjson.List(header["checkpoints"])
	if err != nil {
		t.Fatal(err)
	}
	out := canonjson.Object{}
	for k, v := range header {
		out[k] = v
	}
	altered := make(canonjson.Array, len(list))
	found := false
	for i, v := range list {
		o, err := canonjson.Obj(v)
		if err != nil {
			t.Fatal(err)
		}
		cp := canonjson.Object{}
		for k, v := range o {
			cp[k] = v
		}
		if s, _ := canonjson.ParseUint(o["sequence"]); s == seq {
			h, err := canonjson.ParseHex(o["stateHash"], 32)
			if err != nil {
				t.Fatal(err)
			}
			h[0] ^= 0xff
			cp["stateHash"] = canonjson.Hex(h)
			found = true
		}
		altered[i] = cp
	}
	if !found {
		t.Fatalf("no checkpoint %d in the header", seq)
	}
	out["checkpoints"] = altered
	return out
}

// verifyExport verifies an exported history with decision replay.
func verifyExport(t *testing.T, b []byte) (verification.Report, error) {
	t.Helper()
	return verification.Verify(bytes.NewReader(b), verification.Options{Mode: verification.ModeVerify, Registry: crashRegistry(t)})
}

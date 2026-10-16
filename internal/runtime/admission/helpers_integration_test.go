//go:build integration

package admission_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

// Integration tests of the admission sequencer (LBT-11 spec S-14, §10
// steps 4 and 5). Each test uses its own migrated database. Tests do not
// call t.Parallel(). Blocking is detected through pg_stat_activity or
// pg_locks, never with sleeps.

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var ctx = context.Background()

// pollDeadline bounds every wait for a database condition.
const pollDeadline = 10 * time.Second

// env is a test database with the sequencer's pool and a separate
// observation pool, so observation never competes for the sequencer's
// connections.
type env struct {
	dsn  string
	pool *pgxpool.Pool
	obs  *pgxpool.Pool
}

// newEnv migrates a fresh database. maxConns 0 uses pgtest.Pool (8
// connections) for the sequencer's pool.
func newEnv(t *testing.T, maxConns int32) env {
	t.Helper()
	dsn := pgtest.MigratedDatabase(t)
	e := env{dsn: dsn, obs: newPool(t, dsn, 4)}
	if maxConns == 0 {
		e.pool = pgtest.Pool(t, dsn)
	} else {
		e.pool = newPool(t, dsn, maxConns)
	}
	return e
}

// newPool opens a pool with at most maxConns connections, closed at
// cleanup.
func newPool(t *testing.T, dsn string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// connect opens one connection outside every pool, closed at cleanup.
func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// newSequencer returns a crash/v1 sequencer over db with DefaultConfig
// after mutate.
func newSequencer(t *testing.T, db admission.DB, mutate func(*admission.Config)) *admission.Sequencer {
	t.Helper()
	reg, err := game.NewRegistry(v1.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	cfg := admission.DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := admission.New(db, reg, cfg, admission.CrashV1Policy())
	if err != nil {
		t.Fatalf("admission.New: %v", err)
	}
	return s
}

var idCounter atomic.Uint64

// cmdID returns a deterministic non-nil UUID for n.
func cmdID(n int) [16]byte {
	var id [16]byte
	id[0], id[6] = 0x0c, 0x70
	binary.BigEndian.PutUint64(id[8:], uint64(n)) //nolint:gosec // G115: n is a non-negative test command number
	return id
}

var freshCounter atomic.Int64

// freshID returns a command ID that no cmdID(n) with n < 1e9 returns.
func freshID() [16]byte { return cmdID(int(1_000_000_000 + freshCounter.Add(1))) }

// constraintOf returns the constraint name of the PgError err wraps.
func constraintOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// newRoundID returns a fresh non-nil round UUID.
func newRoundID() [16]byte {
	var id [16]byte
	id[0], id[6] = 0x0a, 0x70
	binary.BigEndian.PutUint64(id[8:], idCounter.Add(1))
	return id
}

// workedRoundID and workedSeed give D = 9,410,000 µs (lifecycle spec §1).
var (
	workedRoundID = mustUUID("0190f3a8-7b3c-7d4e-8f00-000000000001")
	workedSeed    = func() [32]byte {
		var s [32]byte
		for i := range s {
			s[i] = byte(i)
		}
		return s
	}()
	testSeed = func() [32]byte {
		var s [32]byte
		for i := range s {
			s[i] = 0xab
		}
		return s
	}()
)

func mustUUID(s string) [16]byte {
	id, err := canonjson.ParseUUID(canonjson.String(s))
	if err != nil {
		panic(err)
	}
	return id
}

// createRound creates a crash/v1 round as round creation would: rounds,
// round_secrets with the round's secret close offset D, both heads with
// their defaults, and the genesis chain record, in one execution
// transaction. It returns the round's config and initial state.
func createRound(t *testing.T, db *pgxpool.Pool, roundID [16]byte, seed [32]byte) (v1.Config, v1.State) {
	t.Helper()
	return createRoundRules(t, db, roundID, seed, v1.RulesVersionNumber)
}

// createRoundRules is createRound with the rules version written to rounds
// and the genesis record overridden.
func createRoundRules(t *testing.T, db *pgxpool.Pool, roundID [16]byte, seed [32]byte, rules game.RulesVersion) (v1.Config, v1.State) {
	t.Helper()
	_, state := roundConfig(t, roundID, seed)
	return createRoundWith(t, db, roundID, seed, rules, &state.CrashDeadlineOffsetMicros)
}

// roundConfig returns the config createRound uses and its initial state.
func roundConfig(t *testing.T, roundID [16]byte, seed [32]byte) (v1.Config, v1.State) {
	t.Helper()
	cfg := v1.Config{RoundID: roundID, Seed: seed, MinBetCredits: 1, MaxBetCredits: 1_000_000_000, MaxBets: 100}
	state, err := v1.Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, state
}

// createRoundWith is createRoundRules with round_secrets.close_offset_micros
// set to *offset, or NULL when offset is nil (a round created before
// migration 0009).
func createRoundWith(t *testing.T, db *pgxpool.Pool, roundID [16]byte, seed [32]byte, rules game.RulesVersion, offset *int64) (v1.Config, v1.State) {
	t.Helper()
	cfg, state := roundConfig(t, roundID, seed)
	public, secret, err := v1.Adapter().EncodeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := v1.Descriptor()
	g := chain.Genesis{
		RoundID: roundID, Game: d.Game, RulesVersion: rules, SchemaVersion: d.SchemaVersion, StateVersion: d.StateVersion,
		Config: public, CreatedAtUnixMicro: time.Now().UnixMicro(), EngineBuild: "test",
	}
	record, err := chain.EncodeGenesis(g)
	if err != nil {
		t.Fatal(err)
	}
	hash := chain.LinkHash(record)
	inTx(t, db, func(tx pgx.Tx) {
		mustExec(t, tx, `INSERT INTO orvex.rounds
			(round_id, game, rules_version, schema_version, state_version, chain, created_at_unix_micro)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			roundID, string(g.Game), int64(rules), int64(g.SchemaVersion), int64(g.StateVersion), chain.Version, g.CreatedAtUnixMicro)
		mustExec(t, tx, `INSERT INTO orvex.round_secrets (round_id, config_secret, close_offset_micros) VALUES ($1, $2, $3)`,
			roundID, secret, offset)
		mustExec(t, tx, `INSERT INTO orvex.round_admission_heads (round_id) VALUES ($1)`, roundID)
		mustExec(t, tx, `INSERT INTO orvex.round_execution_heads (round_id, processed_sequence, fencing_token)
			VALUES ($1, 0, 0)`, roundID)
		mustExec(t, tx, `INSERT INTO orvex.chain_records
			(round_id, sequence, prev_hash, hash, record, result_status, result_reason, fencing_token)
			VALUES ($1, 0, $2, $3, $4, NULL, NULL, 0)`, roundID, make([]byte, 32), hash[:], record)
	})
	return cfg, state
}

// seedWithOffset scans 32-byte big-endian counters from 1 for a seed whose
// round (config as in createRound) has min <= D <= max. Counter 0 is the
// all-zero seed, which InitialState rejects.
func seedWithOffset(t *testing.T, roundID [16]byte, minD, maxD int64) [32]byte {
	t.Helper()
	for n := uint64(1); n <= 100_000; n++ {
		var seed [32]byte
		binary.BigEndian.PutUint64(seed[24:], n)
		cfg := v1.Config{RoundID: roundID, Seed: seed, MinBetCredits: 1, MaxBetCredits: 1_000_000_000, MaxBets: 100}
		state, err := v1.Rules{}.InitialState(cfg)
		if err != nil {
			continue
		}
		if d := state.CrashDeadlineOffsetMicros; d >= minD && d <= maxD {
			return seed
		}
	}
	t.Fatalf("no seed with D in [%d, %d] within 100000 counters", minD, maxD)
	return [32]byte{}
}

// instantSeed returns a seed whose round crashes instantly (D = 0), so its
// closure bound equals the start's effective time.
func instantSeed(t *testing.T, roundID [16]byte) [32]byte {
	t.Helper()
	return seedWithOffset(t, roundID, 0, 0)
}

// genesisHash reads the round's genesis link hash.
func genesisHash(t *testing.T, db *pgxpool.Pool, roundID [16]byte) chain.Hash {
	t.Helper()
	var b []byte
	if err := db.QueryRow(ctx, `SELECT hash FROM orvex.chain_records WHERE round_id = $1 AND sequence = 0`, roundID).Scan(&b); err != nil {
		t.Fatal(err)
	}
	var h chain.Hash
	copy(h[:], b)
	return h
}

func mustExec(t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
}

// inTx runs fn in one execution transaction and commits.
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

// fundPlayer creates player's account and grants it credits from the
// global system account faucet, which it creates if absent.
func fundPlayer(t *testing.T, db *pgxpool.Pool, player string, credits int64) {
	t.Helper()
	inTx(t, db, func(tx pgx.Tx) {
		var id [16]byte
		id[0], id[6] = 0x0f, 0x70
		binary.BigEndian.PutUint64(id[8:], idCounter.Add(1))
		mustExec(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
			VALUES ($1, 'system', 'faucet', false) ON CONFLICT DO NOTHING`, id)
		var faucet [16]byte
		if err := tx.QueryRow(ctx, `SELECT account_id FROM orvex.ledger_accounts
			WHERE kind = 'system' AND name = 'faucet' AND round_id IS NULL`).Scan(&faucet); err != nil {
			t.Fatal(err)
		}
		account, txID := newRoundID(), newRoundID()
		account[0], txID[0] = 0x0e, 0x0d
		mustExec(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced)
			VALUES ($1, 'player', $2, true)`, account, player)
		mustExec(t, tx, `INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`, txID)
		mustExec(t, tx, `INSERT INTO orvex.ledger_postings
			(transaction_id, posting_index, debit_account_id, credit_account_id, amount)
			VALUES ($1, 0, $2, $3, $4)`, txID, faucet, account, credits)
	})
}

// held returns the player's held credits.
func held(t *testing.T, db *pgxpool.Pool, player string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(ctx, `SELECT held FROM orvex.ledger_accounts WHERE kind = 'player' AND player_id = $1`, player).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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

// seedAdmission inserts the next admission of an ordinary command directly,
// with the given effective time, and advances admitted_sequence, in one
// transaction. It returns the sequence.
func seedAdmission(t *testing.T, db *pgxpool.Pool, roundID, commandID [16]byte, cmd v1.Command, effectiveTime int64) uint64 {
	t.Helper()
	b, err := v1.Adapter().EncodeCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	inTx(t, db, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT admitted_sequence FROM orvex.round_admission_heads WHERE round_id = $1 FOR UPDATE`,
			roundID).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		seq++
		mustExec(t, tx, `INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
			VALUES ($1, $2, $3, $4, $5)`, roundID, seq, commandID, effectiveTime, b)
		mustExec(t, tx, `UPDATE orvex.round_admission_heads SET admitted_sequence = $2 WHERE round_id = $1`, roundID, seq)
	})
	return uint64(seq) //nolint:gosec // G115: seq is admitted_sequence (CHECK >= 0) + 1
}

// headRow is a round's admission head.
type headRow struct {
	admitted                int64
	state                   string
	started, closed, sealed *int64
	bound                   *int64 // close_not_before_unix_micro
}

func head(t *testing.T, db *pgxpool.Pool, roundID [16]byte) headRow {
	t.Helper()
	var h headRow
	if err := db.QueryRow(ctx, `SELECT admitted_sequence, admission_state, started_sequence, closed_sequence, sealed_sequence,
		close_not_before_unix_micro
		FROM orvex.round_admission_heads WHERE round_id = $1`, roundID).
		Scan(&h.admitted, &h.state, &h.started, &h.closed, &h.sealed, &h.bound); err != nil {
		t.Fatal(err)
	}
	return h
}

// admissionRow is one orvex.admissions row.
type admissionRow struct {
	roundID  [16]byte
	sequence int64
	effTime  int64
	command  []byte
}

// row reads the admission of commandID; ok is false if there is none.
func row(t *testing.T, db *pgxpool.Pool, commandID [16]byte) (r admissionRow, ok bool) {
	t.Helper()
	err := db.QueryRow(ctx, `SELECT round_id, sequence, effective_time_unix_micro, command
		FROM orvex.admissions WHERE command_id = $1`, commandID).Scan(&r.roundID, &r.sequence, &r.effTime, &r.command)
	if errors.Is(err, pgx.ErrNoRows) {
		return admissionRow{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return r, true
}

// requireRow requires a's committed row to equal a.
func requireRow(t *testing.T, db *pgxpool.Pool, a admission.Admission) {
	t.Helper()
	r, ok := row(t, db, a.CommandID)
	if !ok {
		t.Errorf("no admission row for acknowledged command %x", a.CommandID)
		return
	}
	if r.roundID != a.RoundID || r.sequence != int64(a.Sequence) || r.effTime != a.EffectiveTimeUnixMicro { //nolint:gosec // G115: a test round's sequence, far below math.MaxInt64
		t.Errorf("row = round %x seq %d time %d, Admission = %+v", r.roundID, r.sequence, r.effTime, a)
	}
}

// requireNoRow requires that commandID has no admission.
func requireNoRow(t *testing.T, db *pgxpool.Pool, commandID [16]byte) {
	t.Helper()
	if r, ok := row(t, db, commandID); ok {
		t.Errorf("command %x has an admission row: %+v", commandID, r)
	}
}

// requireRejected requires err to be a *RejectedError with reason.
func requireRejected(t *testing.T, err error, reason admission.Reason) {
	t.Helper()
	var re *admission.RejectedError
	if !errors.As(err, &re) || re.Reason != reason || !errors.Is(err, admission.ErrRejected) {
		t.Errorf("err = %v, want RejectedError %s", err, reason)
	}
}

// poll calls cond every 10 ms until it holds, failing the test after
// pollDeadline.
func poll(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(pollDeadline)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached within %v", what, pollDeadline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lockWaiters returns the number of this database's backends waiting on a
// lock.
func lockWaiters(t *testing.T, db *pgxpool.Pool) int64 {
	t.Helper()
	return count(t, db, `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'`)
}

// waitLockWaiters polls pg_stat_activity until at least n backends of this
// database wait on a lock.
func waitLockWaiters(t *testing.T, db *pgxpool.Pool, n int64) {
	t.Helper()
	poll(t, fmt.Sprintf("%d lock waiters", n), func() bool { return lockWaiters(t, db) >= n })
}

// waitAdvisoryWaiter polls pg_locks until exactly one backend of this
// database waits for an advisory lock, and returns its pid.
func waitAdvisoryWaiter(t *testing.T, db *pgxpool.Pool) int32 {
	t.Helper()
	var pid int32
	poll(t, "one advisory lock waiter", func() bool {
		rows, err := db.Query(ctx, `SELECT l.pid FROM pg_locks l
			JOIN pg_database d ON d.oid = l.database
			WHERE l.locktype = 'advisory' AND NOT l.granted AND d.datname = current_database()`)
		if err != nil {
			t.Fatal(err)
		}
		pids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
		if err != nil {
			t.Fatal(err)
		}
		if len(pids) == 1 {
			pid = pids[0]
			return true
		}
		return false
	})
	return pid
}

// installCommitHooks creates schema orvex_test with two deferred constraint
// triggers on orvex.admissions: one holds COMMIT of the admission of holdID
// on advisory lock key, the other fails COMMIT of failID with SQLSTATE
// 23000. Test-only (S-14).
func installCommitHooks(t *testing.T, db *pgxpool.Pool, holdID, failID [16]byte, key int64) {
	t.Helper()
	uuid := func(id [16]byte) string { return string(canonjson.UUID(id)) }
	for _, sql := range []string{
		`CREATE SCHEMA orvex_test`,
		fmt.Sprintf(`CREATE FUNCTION orvex_test.hold_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.command_id = '%s'::uuid THEN
        PERFORM pg_advisory_xact_lock(%d);
    END IF;
    RETURN NULL;
END
$$`, uuid(holdID), key),
		fmt.Sprintf(`CREATE FUNCTION orvex_test.fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.command_id = '%s'::uuid THEN
        RAISE EXCEPTION 'orvex_test: commit of the admission fails' USING ERRCODE = '23000';
    END IF;
    RETURN NULL;
END
$$`, uuid(failID)),
		`CREATE CONSTRAINT TRIGGER admissions_hold_commit AFTER INSERT ON orvex.admissions
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION orvex_test.hold_commit()`,
		`CREATE CONSTRAINT TRIGGER admissions_fail_commit AFTER INSERT ON orvex.admissions
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION orvex_test.fail_commit()`,
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatalf("install commit hooks: %v", err)
		}
	}
}

// fault is an injected failure of one admission transaction.
type fault uint8

const (
	faultNone fault = iota
	// faultCallConnLost fails the function call with a connection error
	// without executing it.
	faultCallConnLost
	// faultCommitThenEOF runs the real Commit, then returns
	// io.ErrUnexpectedEOF.
	faultCommitThenEOF
	// faultCommitThenCancel runs the real Commit, cancels the supplied
	// ctx and returns an error wrapping context.Canceled.
	faultCommitThenCancel
)

// faultDB wraps a pool. It injects faults only into transactions begun
// with postgres.AdmissionTxOptions(), the k-th such transaction getting
// faults[k-1], and counts them; read transactions pass through untouched.
type faultDB struct {
	db     admission.DB
	faults []fault
	cancel context.CancelFunc

	mu       sync.Mutex
	attempts int
}

func (f *faultDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	if opts != postgres.AdmissionTxOptions() {
		return f.db.BeginTx(ctx, opts)
	}
	f.mu.Lock()
	n := f.attempts
	f.attempts++
	fl := faultNone
	if n < len(f.faults) {
		fl = f.faults[n]
	}
	f.mu.Unlock()
	tx, err := f.db.BeginTx(ctx, opts)
	if err != nil || fl == faultNone {
		return tx, err
	}
	return &faultTx{Tx: tx, fault: fl, cancel: f.cancel}, nil
}

func (f *faultDB) admissionAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

type faultTx struct {
	pgx.Tx
	fault  fault
	cancel context.CancelFunc
}

func (tx *faultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.fault == faultCallConnLost {
		return errRow{err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer (injected)")}}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *faultTx) Commit(ctx context.Context) error {
	switch tx.fault {
	case faultCommitThenEOF:
		if err := tx.Tx.Commit(ctx); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	case faultCommitThenCancel:
		if err := tx.Tx.Commit(ctx); err != nil {
			return err
		}
		tx.cancel()
		return fmt.Errorf("commit (injected): %w", context.Canceled)
	default:
		return tx.Tx.Commit(ctx)
	}
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// Request builders.

func request(roundID, commandID [16]byte, cmd v1.Command) admission.Request {
	return admission.Request{RoundID: roundID, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, CommandID: commandID, Command: cmd}
}

func closeRequest(roundID, commandID [16]byte) admission.Request {
	return request(roundID, commandID, v1.CloseRound{Authority: v1.AuthoritySystem})
}

func placeBet(player string, credits int64) v1.PlaceBet {
	return v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: v1.PlayerID(player), BetCredits: credits}
}

func cashOut(player string) v1.CashOut {
	return v1.CashOut{Authority: v1.AuthorityPlayer, Player: v1.PlayerID(player)}
}

var (
	startRound  = v1.StartRound{Authority: v1.AuthoritySystem}
	settleRound = v1.SettleRound{Authority: v1.AuthoritySystem}
)

// mustAdmit admits req and fails the test on error.
func mustAdmit(t *testing.T, s *admission.Sequencer, req admission.Request) admission.Admission {
	t.Helper()
	a, err := s.Admit(ctx, req)
	if err != nil {
		t.Fatalf("Admit(%T): %v", req.Command, err)
	}
	return a
}

// errorList collects unexpected errors from goroutines, reported after
// they all finished.
type errorList struct {
	mu   sync.Mutex
	errs []string
}

func (l *errorList) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, fmt.Sprintf(format, args...))
}

func (l *errorList) report(t *testing.T) {
	t.Helper()
	for i, e := range l.errs {
		if i == 20 {
			t.Errorf("... and %d more", len(l.errs)-20)
			break
		}
		t.Error(e)
	}
	if len(l.errs) > 0 {
		t.FailNow()
	}
}

// together runs fn(0..n-1) in n goroutines that all start on one closed
// channel, and waits for them.
func together(n int, fn func(i int)) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			fn(i)
		})
	}
	close(start)
	wg.Wait()
}

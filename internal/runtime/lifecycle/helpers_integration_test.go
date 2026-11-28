//go:build integration

package lifecycle

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/verification"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// Integration tests of the scheduler against PostgreSQL (ADR-006). Each
// test uses its own migrated database and drives real rounds; seeds are
// chosen so that crash deadlines are short.

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var ctx = context.Background()

// testTiming keeps the tests fast; the logic is the production one.
var testTiming = timing{
	running:    5 * time.Millisecond,
	waiting:    10 * time.Millisecond,
	idle:       20 * time.Millisecond,
	maxBackoff: 50 * time.Millisecond,
}

// syncBuffer is a log sink safe for the scheduler's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is one test database with a sequencer and a log sink.
type env struct {
	t    *testing.T
	dsn  string
	pool *pgxpool.Pool
	seq  *admission.Sequencer
	logs *syncBuffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := pgtest.MigratedDatabase(t)
	pool := pgtest.Pool(t, dsn)
	seq, err := admission.New(pool, roundtest.Registry(t), admission.DefaultConfig(), admission.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, dsn: dsn, pool: pool, seq: seq, logs: &syncBuffer{}}
}

// scheduler returns a scheduler with its own executor over db (nil: the
// pool), the test timing and short-deadline seeds.
func (e *env) scheduler(cfg Config, db execution.DB, mutate func(*execution.Config)) *Scheduler {
	e.t.Helper()
	if db == nil {
		db = e.pool
	}
	x := roundtest.NewExecutor(e.t, db, mutate, nil)
	cfg.Logger = slog.New(slog.NewTextHandler(e.logs, nil))
	s, err := New(e.pool, x, e.seq, cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	s.timing = testTiming
	s.seed = func(id [16]byte) [32]byte { return roundtest.SeedWithDeadline(e.t, id, time.Second, 2*time.Second) }
	return s
}

// running is a scheduler's Run in a goroutine.
type running struct {
	cancel context.CancelFunc
	done   chan error
}

func run(s *Scheduler) *running {
	c, cancel := context.WithCancel(ctx)
	r := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- s.Run(c) }()
	return r
}

// stop cancels Run and returns its error.
func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return within 30s of its cancellation")
		return nil
	}
}

// startRun runs s and stops it at cleanup, failing on an error.
func startRun(t *testing.T, s *Scheduler) *running {
	t.Helper()
	r := run(s)
	t.Cleanup(func() {
		if err := r.stop(t); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return r
}

// waitFor polls cond every 10ms for up to a minute.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) count(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *env) rounds() int64 { return e.count(`SELECT count(*) FROM orvex.rounds`) }

func (e *env) settled(round [16]byte) bool {
	return e.count(`SELECT count(*) FROM orvex.round_secret_reveals WHERE round_id = $1`, round) == 1
}

func (e *env) hasEvent(round [16]byte, typ string) bool {
	return e.count(`SELECT count(*) FROM orvex.domain_events WHERE round_id = $1 AND event_type = $2`, round, typ) > 0
}

// firstRound returns the oldest round's ID.
func (e *env) firstRound() [16]byte {
	e.t.Helper()
	var id [16]byte
	if err := e.pool.QueryRow(ctx, `SELECT round_id FROM orvex.rounds ORDER BY created_at_unix_micro LIMIT 1`).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// head returns the round's fencing token and owner.
func (e *env) head(round [16]byte) (int64, string) {
	e.t.Helper()
	var (
		token int64
		owner *string
	)
	if err := e.pool.QueryRow(ctx, `SELECT fencing_token, owner_id FROM orvex.round_execution_heads WHERE round_id = $1`, round).
		Scan(&token, &owner); err != nil {
		e.t.Fatal(err)
	}
	if owner == nil {
		return token, ""
	}
	return token, *owner
}

// admissions returns the round's admitted command IDs and their results in
// sequence order ("" while not executed).
func (e *env) admissions(round [16]byte) ([][16]byte, []string) {
	e.t.Helper()
	rows, err := e.pool.Query(ctx, `SELECT a.command_id, coalesce(r.result_status, '') FROM orvex.admissions a
		LEFT JOIN orvex.chain_records r USING (round_id, sequence) WHERE a.round_id = $1 ORDER BY a.sequence`, round)
	if err != nil {
		e.t.Fatal(err)
	}
	var (
		ids      [][16]byte
		statuses []string
	)
	for rows.Next() {
		var (
			id     [16]byte
			status string
		)
		if err := rows.Scan(&id, &status); err != nil {
			e.t.Fatal(err)
		}
		ids, statuses = append(ids, id), append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return ids, statuses
}

// requireLifecycle requires the round's admissions to be players player
// commands and, in this order, exactly one start, close and settle under
// the engine's command IDs, every admission accepted.
func (e *env) requireLifecycle(round [16]byte, players int) {
	e.t.Helper()
	ids, statuses := e.admissions(round)
	want := []string{"start", "close", "settle"}
	if len(ids) != players+len(want) {
		e.t.Fatalf("%d admissions, want %d player commands and %v", len(ids), players, want)
	}
	var got []string
	for _, id := range ids {
		for _, name := range want {
			if id == commandID(round, name) {
				got = append(got, name)
			}
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		e.t.Errorf("lifecycle admissions %v, want %v", got, want)
	}
	for i, s := range statuses {
		if s != "accepted" {
			e.t.Errorf("admission %d result %q, want accepted", i+1, s)
		}
	}
}

// requireAudit exports the settled round and verifies it.
func (e *env) requireAudit(round [16]byte) {
	e.t.Helper()
	var buf bytes.Buffer
	if _, err := eventstore.ExportHistory(ctx, e.pool, round, &buf, eventstore.Options{}); err != nil {
		e.t.Fatalf("ExportHistory: %v", err)
	}
	if _, err := verification.Verify(&buf, verification.Options{Mode: verification.ModeVerify, Registry: roundtest.Registry(e.t)}); err != nil {
		e.t.Fatalf("Verify: %v", err)
	}
}

// createRound creates a round as an earlier engine would have, with token
// 0, created at createdAt.
func (e *env) createRound(x *execution.Executor, createdAt int64) [16]byte {
	e.t.Helper()
	id := newRoundID()
	if _, err := x.CreateRound(ctx, execution.NewRound{
		RoundID: id, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config: v1.Config{
			RoundID: id, Seed: roundtest.SeedWithDeadline(e.t, id, time.Second, 2*time.Second),
			MinBetCredits: roundMinBetCredits, MaxBetCredits: roundMaxBetCredits, MaxBets: roundMaxBets,
		},
		CreatedAtUnixMicro: createdAt,
	}); err != nil {
		e.t.Fatalf("CreateRound: %v", err)
	}
	return id
}

func (e *env) dbNow() int64 {
	e.t.Helper()
	var n int64
	if err := e.pool.QueryRow(ctx, `SELECT orvex.unix_micro(clock_timestamp())`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// admit admits a command and fails the test on error.
func (e *env) admit(round, commandID [16]byte, cmd v1.Command) admission.Admission {
	e.t.Helper()
	a, err := e.seq.Admit(ctx, admission.Request{
		RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, CommandID: commandID, Command: cmd,
	})
	if err != nil {
		e.t.Fatalf("Admit %T: %v", cmd, err)
	}
	return a
}

// playerCommandID returns a non-nil command ID for test player commands.
func playerCommandID(n uint64) [16]byte {
	var id [16]byte
	id[0], id[6] = 0x0c, 0x70
	binary.BigEndian.PutUint64(id[8:], n)
	return id
}

// lostCommitDB wraps a pool; its read-write transactions run afterCommit
// after a successful COMMIT, which may replace the result: a commit whose
// response was lost.
type lostCommitDB struct {
	*pgxpool.Pool
	afterCommit func() error
}

func (d *lostCommitDB) BeginTx(c context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.Pool.BeginTx(c, opts)
	if err != nil || opts.AccessMode != pgx.ReadWrite {
		return tx, err
	}
	return &lostCommitTx{Tx: tx, db: d}, nil
}

type lostCommitTx struct {
	pgx.Tx
	db *lostCommitDB
}

func (tx *lostCommitTx) Commit(c context.Context) error {
	if err := tx.Tx.Commit(c); err != nil {
		return err
	}
	return tx.db.afterCommit()
}

// loseFirstCommit returns an afterCommit that reports the first commit as
// a lost connection and runs then before returning.
func loseFirstCommit(then func()) func() error {
	var once sync.Once
	return func() error {
		err := error(nil)
		once.Do(func() {
			then()
			err = io.ErrUnexpectedEOF
		})
		return err
	}
}

// occurrences counts the log lines containing msg.
func occurrences(logs, msg string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, msg) {
			n++
		}
	}
	return n
}

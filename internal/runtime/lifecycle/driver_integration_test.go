//go:build integration

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// TestDueStepsAdmittedAtBatchBoundary: after every command the scheduler
// executes, a feeder admits one more, so the round's backlog never empties.
// The start, due at creation, and the close, due at the crash deadline, are
// each admitted after at most one batch, behind every command admitted
// before them. While the start waits behind that backlog one commit response
// is lost: that failure is retried once, and the wait itself is none. The
// round settles without a halt and passes the audit verifier.
func TestDueStepsAdmittedAtBatchBoundary(t *testing.T) {
	e := newEnv(t)
	const bets = drainBatch
	for i := range bets {
		roundtest.FundPlayer(t, e.pool, fmt.Sprintf("p%d", i), 100)
	}
	created := e.dbNow()
	id := e.createRound(roundtest.NewExecutor(t, e.pool, nil, nil), created)
	for i := range bets {
		e.admit(id, playerCommandID(uint64(i+1)), v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: v1.PlayerID(fmt.Sprintf("p%d", i)), BetCredits: 1})
	}

	f := &feeder{e: e, round: id, next: bets + 1}
	db := &lostCommitDB{Pool: e.pool, afterCommit: f.committed}
	s := e.scheduler(Config{BettingWindow: 0}, db, func(c *execution.Config) { c.MaxAttempts = 1 })
	c, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := s.drive(c, round{id: id, createdAtUnixMicro: created}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	t.Logf("%d commands executed before the start's admission, %d past the crash deadline before the close's; %d fed",
		f.beforeStart, f.pastDeadline, f.next-bets-1)

	if f.beforeStart > drainBatch {
		t.Errorf("%d commands executed while the due start was not admitted, want at most one batch of %d", f.beforeStart, drainBatch)
	}
	if f.pastDeadline > drainBatch {
		t.Errorf("%d commands executed past the crash deadline while the close was not admitted, want at most one batch of %d", f.pastDeadline, drainBatch)
	}
	if !f.closedRefused {
		t.Error("the feeder stopped before the close was admitted: the close was not admitted under a backlog")
	}
	if !f.lost {
		t.Error("no commit response was lost while the start waited behind earlier admissions")
	}
	if start := e.count(`SELECT started_sequence FROM orvex.round_admission_heads WHERE round_id = $1`, id); start <= bets {
		t.Errorf("start admitted at sequence %d, want after the %d bets admitted before it", start, bets)
	}
	if n := e.count(`SELECT count(*) FROM orvex.domain_events WHERE round_id = $1 AND event_type = 'BetPlaced'`, id); n != bets {
		t.Errorf("%d bets placed, want all %d admitted before the start", n, bets)
	}
	e.requireLifecycleOrder(id)
	logs := e.logs.String()
	if n := occurrences(logs, "drive round failed; retrying"); n != 1 {
		t.Errorf("%d drive retries logged, want 1 for the lost commit:\n%s", n, logs)
	}
	if strings.Contains(logs, "halted") {
		t.Errorf("the round halted:\n%s", logs)
	}
	e.requireAudit(id)
}

// feeder is the afterCommit of TestDueStepsAdmittedAtBatchBoundary. After
// each Transaction B it reads the round's heads, counts the commands
// executed while a due step was not admitted, reports one commit while the
// start waits as lost, and admits one more cash-out until the close refuses
// it.
type feeder struct {
	e     *env
	round [16]byte
	// next is the number of the next fed command's ID.
	next uint64
	// beforeStart counts the commands executed before the start was
	// admitted, and pastDeadline those executed at or after the crash
	// deadline, after the start, before the close was admitted.
	beforeStart, pastDeadline int
	// lost: one commit response was reported lost.
	lost bool
	// closedRefused: a fed command was refused because the close was
	// admitted.
	closedRefused bool
	// stopped: the feeder admits nothing more.
	stopped bool
}

const feederHeadsSQL = `SELECT a.started_sequence, a.closed_sequence, a.close_not_before_unix_micro,
		x.processed_sequence, orvex.unix_micro(clock_timestamp())
	FROM orvex.round_admission_heads a JOIN orvex.round_execution_heads x USING (round_id)
	WHERE a.round_id = $1`

func (f *feeder) committed() error {
	var (
		started, closed, bound *int64
		processed, now         int64
	)
	if err := f.e.pool.QueryRow(ctx, feederHeadsSQL, f.round).Scan(&started, &closed, &bound, &processed, &now); err != nil {
		f.e.t.Errorf("read the round's heads: %v", err)
		f.stopped = true
		return nil
	}
	var lost error
	switch {
	case started == nil:
		f.beforeStart++
	case processed < *started:
		if !f.lost {
			f.lost, lost = true, io.ErrUnexpectedEOF
		}
	case closed == nil && now >= *bound:
		f.pastDeadline++
	}
	if f.beforeStart > drainBatch || f.pastDeadline > drainBatch {
		f.stopped = true
	}
	if f.stopped {
		return lost
	}
	_, err := f.e.seq.Admit(ctx, admission.Request{
		RoundID: f.round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		CommandID: playerCommandID(f.next), Command: v1.CashOut{Authority: v1.AuthorityPlayer, Player: "feeder"},
	})
	f.next++
	refused, ok := errors.AsType[*admission.RejectedError](err)
	switch {
	case ok && refused.Reason == admission.ReasonRoundClosed:
		f.closedRefused, f.stopped = true, true
	case err != nil:
		f.e.t.Errorf("feed: %v", err)
		f.stopped = true
	}
	return lost
}

// requireLifecycleOrder requires every admission of the settled round to
// have executed, and the engine's start, close and settle to be accepted in
// this order, the settle last.
func (e *env) requireLifecycleOrder(round [16]byte) {
	e.t.Helper()
	ids, statuses := e.admissions(round)
	var got []string
	for i, id := range ids {
		if statuses[i] == "" {
			e.t.Errorf("admission %d not executed", i+1)
		}
		for _, name := range []string{"start", "close", "settle"} {
			if id == commandID(round, name) {
				got = append(got, name)
				if statuses[i] != "accepted" {
					e.t.Errorf("%s result %q, want accepted", name, statuses[i])
				}
			}
		}
	}
	if strings.Join(got, ",") != "start,close,settle" {
		e.t.Errorf("lifecycle admissions %v, want start, close, settle", got)
	}
	if len(ids) == 0 || ids[len(ids)-1] != commandID(round, "settle") {
		e.t.Error("the settle is not the round's last admission")
	}
}

// loadCounter wraps an executor's DB and counts its read-only
// transactions, which are its cold loads (E-3).
type loadCounter struct {
	execution.DB
	loads atomic.Int64
}

func (d *loadCounter) BeginTx(c context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	if opts.AccessMode == pgx.ReadOnly {
		d.loads.Add(1)
	}
	return d.DB.BeginTx(c, opts)
}

// requireEvicted requires s's executor to hold no state of the settled
// round: reading it is one cold load.
func (e *env) requireEvicted(s *Scheduler, db *loadCounter, round [16]byte) {
	e.t.Helper()
	token, _ := e.head(round)
	before := db.loads.Load()
	got, err := s.x.State(ctx, execution.Lease{RoundID: round, FencingToken: token})
	if err != nil {
		e.t.Fatalf("State: %v", err)
	}
	if got.(v1.State).Phase != v1.PhaseSettled {
		e.t.Errorf("phase %v, want SETTLED", got.(v1.State).Phase)
	}
	if n := db.loads.Load() - before; n != 1 {
		e.t.Errorf("reading the settled round took %d cold loads, want 1: the executor kept its state", n)
	}
}

// TestSettledRoundsLeaveNoState: the scheduler drives rounds its executor
// created, one after another, to settlement. It never replays one: their
// states stay cached until their final records, and nothing reloads them
// after. No settled round stays in memory. The oldest still exports,
// verifies and replays from the database while the executor is up.
func TestSettledRoundsLeaveNoState(t *testing.T) {
	e := newEnv(t)
	roundtest.FundPlayer(t, e.pool, "alice", 1_000)
	db := &loadCounter{DB: e.pool}
	s := e.scheduler(Config{}, db, nil)
	c, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var ids [][16]byte
	for i := range 3 {
		created := e.dbNow()
		id := e.createRound(s.x, created)
		e.admit(id, playerCommandID(uint64(i+1)), v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 10})
		if err := s.drive(c, round{id: id, createdAtUnixMicro: created}); err != nil {
			t.Fatalf("drive round %d: %v", i+1, err)
		}
		if !e.settled(id) {
			t.Fatalf("round %d did not settle", i+1)
		}
		e.requireLifecycle(id, 1)
		ids = append(ids, id)
	}
	if n := db.loads.Load(); n != 0 {
		t.Errorf("%d cold loads driving %d rounds the executor created, want 0", n, len(ids))
	}
	for _, id := range ids {
		e.requireEvicted(s, db, id)
	}

	e.requireAudit(ids[0])
	tail, err := eventstore.ReadTail(ctx, e.pool, ids[0], 0, 1_000)
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	if !tail.Final || uint64(len(tail.Records)) != tail.Head+1 {
		t.Errorf("replay of the oldest round: %d records, head %d, final %t; want every record through a final head",
			len(tail.Records), tail.Head, tail.Final)
	}
}

// TestSettledBeforeClaimIsReleased: another engine settled the round after
// this one listed it. The driver's cold load finds it settled; the drive
// ends without a halt and keeps none of its state.
func TestSettledBeforeClaimIsReleased(t *testing.T) {
	e := newEnv(t)
	c, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	first := e.scheduler(Config{}, nil, nil)
	created := e.dbNow()
	id := e.createRound(first.x, created)
	if err := first.drive(c, round{id: id, createdAtUnixMicro: created}); err != nil {
		t.Fatalf("first drive: %v", err)
	}

	// The next claimant can adopt the already settled round once the first lease expires.
	if _, err := e.pool.Exec(c, "UPDATE orvex.round_execution_heads SET lease_expires_at=clock_timestamp() WHERE round_id=$1", id); err != nil {
		t.Fatal(err)
	}
	db := &loadCounter{DB: e.pool}
	s := e.scheduler(Config{}, db, nil)
	if err := s.drive(c, round{id: id, createdAtUnixMicro: created}); err != nil {
		t.Fatalf("drive of the settled round: %v", err)
	}
	if n := db.loads.Load(); n != 1 {
		t.Errorf("%d cold loads driving the settled round, want 1", n)
	}
	e.requireEvicted(s, db, id)
	e.requireLifecycle(id, 0)
	if strings.Contains(e.logs.String(), "halted") {
		t.Errorf("a round halted:\n%s", e.logs)
	}
}

// TestRefusedStepWaitsForEarlierAdmissions: a start admitted under another
// command ID, behind more than one batch of cash-outs, gets the scheduler's
// own start refused at admission after its first batch. The scheduler goes
// on executing every admission before the refusal, finds the round started,
// and closes and settles it instead of halting.
func TestRefusedStepWaitsForEarlierAdmissions(t *testing.T) {
	e := newEnv(t)
	s := e.scheduler(Config{}, nil, nil)
	created := e.dbNow()
	id := e.createRound(s.x, created)
	const backlog = 2 * drainBatch
	for i := range backlog {
		e.admit(id, playerCommandID(uint64(i+1)), v1.CashOut{Authority: v1.AuthorityPlayer, Player: "feeder"})
	}
	foreign := playerCommandID(backlog + 1)
	e.admit(id, foreign, v1.StartRound{Authority: v1.AuthoritySystem})
	c, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := s.drive(c, round{id: id, createdAtUnixMicro: created}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if !e.settled(id) {
		t.Fatal("the round did not settle")
	}
	if _, err := e.seq.Lookup(ctx, commandID(id, "start")); !errors.Is(err, admission.ErrNotFound) {
		t.Errorf("the engine's start: Lookup = %v, want ErrNotFound", err)
	}
	ids, statuses := e.admissions(id)
	if len(ids) != backlog+3 {
		t.Fatalf("%d admissions, want %d cash-outs, the foreign start, close and settle", len(ids), backlog)
	}
	for i, want := range [][16]byte{foreign, commandID(id, "close"), commandID(id, "settle")} {
		if got, status := ids[backlog+i], statuses[backlog+i]; got != want || status != "accepted" {
			t.Errorf("admission %d: %x %q, want %x accepted", backlog+i+1, got, status, want)
		}
	}
	if strings.Contains(e.logs.String(), "halted") {
		t.Errorf("the round halted:\n%s", e.logs)
	}
}

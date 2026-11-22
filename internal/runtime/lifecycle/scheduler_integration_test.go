//go:build integration

package lifecycle

import (
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// TestSchedulerPlaysRounds: with nobody but a player, the scheduler creates
// a round, executes the player's bet and cash-out, starts, closes and
// settles it, and creates the next one. The settled round passes the audit
// verifier, and no log line carries its seed or crash outcome.
func TestSchedulerPlaysRounds(t *testing.T) {
	e := newEnv(t)
	roundtest.FundPlayer(t, e.pool, "alice", 1_000)
	s := e.scheduler(Config{BettingWindow: time.Second, CreateRounds: true}, nil, nil)
	startRun(t, s)

	waitFor(t, "a round", func() bool { return e.rounds() == 1 })
	round := e.firstRound()
	e.admit(round, playerCommandID(1), v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 100})
	waitFor(t, "RoundStarted", func() bool { return e.hasEvent(round, "RoundStarted") })
	e.admit(round, playerCommandID(2), v1.CashOut{Authority: v1.AuthorityPlayer, Player: "alice"})
	waitFor(t, "the round to settle", func() bool { return e.settled(round) })
	waitFor(t, "the next round", func() bool { return e.rounds() == 2 })

	e.requireLifecycle(round, 2)
	if !e.hasEvent(round, "BetCashedOut") {
		t.Error("alice's cash-out did not pay")
	}
	if token, owner := e.head(round); token != 1 || owner != s.owner {
		t.Errorf("head token %d owner %q, want 1 and %q", token, owner, s.owner)
	}
	if n := e.count(`SELECT count(*) FROM orvex.rounds r WHERE NOT EXISTS
		(SELECT 1 FROM orvex.round_secret_reveals v WHERE v.round_id = r.round_id)`); n != 1 {
		t.Errorf("%d unsettled rounds, want 1", n)
	}
	e.requireAudit(round)

	y := roundtest.NewExecutor(t, e.pool, nil, nil)
	got, err := y.State(ctx, execution.Lease{RoundID: round, FencingToken: 1})
	if err != nil {
		t.Fatal(err)
	}
	st := got.(v1.State)
	logs := e.logs.String()
	for name, secret := range map[string]string{
		"seed":           hex.EncodeToString(st.Seed[:]),
		"deadline":       strconv.FormatInt(st.CrashDeadlineUnixMicro, 10),
		"offset":         strconv.FormatInt(st.CrashDeadlineOffsetMicros, 10),
		"crash point":    strconv.FormatInt(st.CrashPointScaled, 10),
		"database URL":   e.dsn,
		"database login": "orvex:orvex@",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs carry the round's %s", name)
		}
	}
	for _, line := range []string{"round created", "round claimed", "phase=RUNNING", "phase=CRASHED", "phase=SETTLED"} {
		if !strings.Contains(logs, line) {
			t.Errorf("the logs lack %q", line)
		}
	}
}

// TestCreateRetriesSamePlan: the first CreateRound commits but its response
// is lost. The scheduler sends the same plan again, which is Duplicate:
// one round, never a second seed.
func TestCreateRetriesSamePlan(t *testing.T) {
	e := newEnv(t)
	db := &lostCommitDB{Pool: e.pool, afterCommit: loseFirstCommit(func() {})}
	s := e.scheduler(Config{BettingWindow: time.Hour, CreateRounds: true}, db, func(c *execution.Config) { c.MaxAttempts = 1 })
	startRun(t, s)

	waitFor(t, "the round to be claimed", func() bool { return strings.Contains(e.logs.String(), "round claimed") })
	time.Sleep(10 * testTiming.idle)
	logs := e.logs.String()
	if n := e.rounds(); n != 1 {
		t.Fatalf("%d rounds, want 1", n)
	}
	if n := occurrences(logs, "create round failed; retrying"); n != 1 {
		t.Errorf("%d create retries logged, want 1", n)
	}
	if n := occurrences(logs, "round created"); n != 1 {
		t.Errorf("%d creations logged, want 1", n)
	}
}

// TestLostPlanAdoptedOnRestart: the engine dies right after the creation
// committed, before it learned so. The next engine finds the round by
// discovery and adopts it instead of creating another.
func TestLostPlanAdoptedOnRestart(t *testing.T) {
	e := newEnv(t)
	var first *running
	db := &lostCommitDB{Pool: e.pool}
	db.afterCommit = loseFirstCommit(func() { first.cancel() })
	a := e.scheduler(Config{BettingWindow: time.Hour, CreateRounds: true}, db, func(c *execution.Config) { c.MaxAttempts = 1 })
	first = run(a)
	if err := <-first.done; err != nil {
		t.Fatalf("first engine Run: %v", err)
	}
	if n := e.rounds(); n != 1 {
		t.Fatalf("%d rounds after the lost commit, want 1", n)
	}
	round := e.firstRound()
	if token, _ := e.head(round); token != 0 {
		t.Fatalf("token %d before the restart, want 0", token)
	}

	b := e.scheduler(Config{BettingWindow: time.Hour, CreateRounds: true}, nil, nil)
	startRun(t, b)
	waitFor(t, "the round to be adopted", func() bool { _, owner := e.head(round); return owner == b.owner })
	time.Sleep(10 * testTiming.idle)
	if n := e.rounds(); n != 1 {
		t.Errorf("%d rounds after the restart, want 1", n)
	}
	if token, _ := e.head(round); token != 1 {
		t.Errorf("token %d, want 1", token)
	}
}

// TestRestartAdoptsEachPhase: an earlier engine left the round in each
// lifecycle state, a command admitted or executed. The new engine claims
// it, admits each remaining step under the same command IDs, so the ones
// already admitted are Duplicate, and settles it once.
func TestRestartAdoptsEachPhase(t *testing.T) {
	type upTo uint8
	const (
		startAdmitted upTo = iota
		started
		closeAdmitted
		crashed
		settleAdmitted
	)
	for name, left := range map[string]upTo{
		"start admitted":  startAdmitted,
		"running":         started,
		"close admitted":  closeAdmitted,
		"crashed":         crashed,
		"settle admitted": settleAdmitted,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			old := roundtest.NewExecutor(t, e.pool, nil, nil)
			round := e.createRound(old, e.dbNow())
			drain := func() {
				t.Helper()
				if _, err := old.Drain(ctx, execution.Lease{RoundID: round}, 10); err != nil {
					t.Fatal(err)
				}
			}
			e.admit(round, commandID(round, "start"), steps[v1.PhaseBetting].command)
			if left >= started {
				drain()
				got, err := old.State(ctx, execution.Lease{RoundID: round})
				if err != nil {
					t.Fatal(err)
				}
				deadline := got.(v1.State).CrashDeadlineUnixMicro
				waitFor(t, "the crash deadline", func() bool { return e.dbNow() >= deadline })
			}
			if left >= closeAdmitted {
				e.admit(round, commandID(round, "close"), steps[v1.PhaseRunning].command)
			}
			if left >= crashed {
				drain()
			}
			if left >= settleAdmitted {
				e.admit(round, commandID(round, "settle"), steps[v1.PhaseCrashed].command)
			}

			s := e.scheduler(Config{CreateRounds: false}, nil, nil)
			startRun(t, s)
			waitFor(t, "the round to settle", func() bool { return e.settled(round) })
			e.requireLifecycle(round, 0)
			if token, owner := e.head(round); token != 1 || owner != s.owner {
				t.Errorf("head token %d owner %q, want 1 and %q", token, owner, s.owner)
			}
			if strings.Contains(e.logs.String(), "halted") {
				t.Errorf("a round halted:\n%s", e.logs)
			}
			e.requireAudit(round)
		})
	}
}

// TestStandbyTakesOverMidRound: a second engine waits while the first
// leads. When the first stops mid-round, the second adopts the round,
// finishes it exactly once and creates the next.
func TestStandbyTakesOverMidRound(t *testing.T) {
	e := newEnv(t)
	a := e.scheduler(Config{BettingWindow: 200 * time.Millisecond, CreateRounds: true}, nil, nil)
	first := run(a)
	waitFor(t, "a round", func() bool { return e.rounds() == 1 })
	round := e.firstRound()
	waitFor(t, "RoundStarted", func() bool { return e.hasEvent(round, "RoundStarted") })

	b := e.scheduler(Config{BettingWindow: 200 * time.Millisecond, CreateRounds: true}, nil, nil)
	startRun(t, b)
	waitFor(t, "the standby to wait", func() bool { return strings.Contains(e.logs.String(), "another scheduler leads; waiting") })
	if n := e.rounds(); n != 1 {
		t.Fatalf("%d rounds with two engines, want 1", n)
	}
	if err := first.stop(t); err != nil {
		t.Fatalf("first engine Run: %v", err)
	}
	if e.settled(round) {
		t.Fatal("the round settled before the takeover; the test proves nothing")
	}

	waitFor(t, "the round to settle", func() bool { return e.settled(round) })
	waitFor(t, "the next round", func() bool { return e.rounds() == 2 })
	e.requireLifecycle(round, 0)
	if token, owner := e.head(round); token != 2 || owner != b.owner {
		t.Errorf("head token %d owner %q, want 2 and the standby's %q", token, owner, b.owner)
	}
	if n := e.count(`SELECT count(*) FROM orvex.domain_events WHERE round_id = $1 AND event_type = 'RoundSettled'`, round); n != 1 {
		t.Errorf("%d RoundSettled events, want 1", n)
	}
}

// TestFencedSchedulerStops: when another engine claims a round this one is
// driving, this one's next execution is fenced and Run returns ErrFenced.
func TestFencedSchedulerStops(t *testing.T) {
	e := newEnv(t)
	roundtest.FundPlayer(t, e.pool, "alice", 1_000)
	s := e.scheduler(Config{BettingWindow: time.Hour, CreateRounds: true}, nil, nil)
	r := run(s)
	waitFor(t, "the round to be claimed", func() bool { return strings.Contains(e.logs.String(), "round claimed") })
	round := e.firstRound()

	other := roundtest.NewExecutor(t, e.pool, nil, nil)
	if _, err := other.ClaimLease(ctx, round, "engine/other", time.Minute); err != nil {
		t.Fatal(err)
	}
	e.admit(round, playerCommandID(1), v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "alice", BetCredits: 100})
	select {
	case err := <-r.done:
		if !errors.Is(err, execution.ErrFenced) {
			t.Fatalf("Run = %v, want ErrFenced", err)
		}
	case <-time.After(30 * time.Second):
		r.cancel()
		t.Fatal("the fenced scheduler kept running")
	}
	if n := e.count(`SELECT processed_sequence FROM orvex.round_execution_heads WHERE round_id = $1`, round); n != 0 {
		t.Errorf("processed sequence %d, want 0: the fenced engine executed", n)
	}
}

// TestHaltedRoundBlocksCreation: a round whose sealed secret does not open
// halts. It is reported once, and the scheduler creates no round while it
// is unsettled.
func TestHaltedRoundBlocksCreation(t *testing.T) {
	e := newEnv(t)
	round := e.createRound(roundtest.NewExecutor(t, e.pool, nil, nil), e.dbNow())
	pgtest.ReplicaExec(t, e.pool, `UPDATE orvex.round_secrets
		SET secret_ciphertext = set_byte(secret_ciphertext, 0, get_byte(secret_ciphertext, 0) # 1) WHERE round_id = $1`, round)

	s := e.scheduler(Config{BettingWindow: 0, CreateRounds: true}, nil, nil)
	startRun(t, s)
	waitFor(t, "the halt report", func() bool { return strings.Contains(e.logs.String(), "round halted") })
	time.Sleep(10 * testTiming.idle)
	if n := e.rounds(); n != 1 {
		t.Errorf("%d rounds, want 1: a halted round blocks creation", n)
	}
	logs := e.logs.String()
	if n := occurrences(logs, "round halted"); n != 1 {
		t.Errorf("the halt is reported %d times, want once", n)
	}
	if !strings.Contains(logs, execution.ErrCorrupt.Error()) {
		t.Errorf("the halt report lacks its class:\n%s", logs)
	}
}

// TestCreateRoundsOff: with creation off, the scheduler creates nothing,
// adopts a round that appears and settles it, and then creates nothing
// either.
func TestCreateRoundsOff(t *testing.T) {
	e := newEnv(t)
	s := e.scheduler(Config{BettingWindow: 100 * time.Millisecond, CreateRounds: false}, nil, nil)
	startRun(t, s)
	time.Sleep(10 * testTiming.idle)
	if n := e.rounds(); n != 0 {
		t.Fatalf("%d rounds with creation off, want 0", n)
	}

	round := e.createRound(roundtest.NewExecutor(t, e.pool, nil, nil), e.dbNow())
	waitFor(t, "the round to settle", func() bool { return e.settled(round) })
	time.Sleep(10 * testTiming.idle)
	if n := e.rounds(); n != 1 {
		t.Errorf("%d rounds after the settlement, want 1", n)
	}
	e.requireLifecycle(round, 0)
}

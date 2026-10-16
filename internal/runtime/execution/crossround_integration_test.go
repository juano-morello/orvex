//go:build integration

package execution_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// newSequencer returns an LBT-11 sequencer with DefaultConfig over db.
func newSequencer(t *testing.T, db admission.DB) *admission.Sequencer {
	t.Helper()
	s, err := admission.New(db, roundtest.Registry(t), admission.DefaultConfig(), admission.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func request(roundID, commandID [16]byte, cmd v1.Command) admission.Request {
	return admission.Request{RoundID: roundID, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, CommandID: commandID, Command: cmd}
}

// together runs fn(0..n-1) in n goroutines that start on one closed
// channel, and waits for them.
func together(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}

func isInsufficient(err error) bool {
	var re *admission.RejectedError
	return errors.As(err, &re) && re.Reason == admission.ReasonInsufficientCredits
}

func TestCrossRoundBalanceProtection(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	s := newSequencer(t, pool)
	roundtest.FundPlayer(t, pool, "alice", 1000)
	rounds := make([][16]byte, 20)
	for i := range rounds {
		rounds[i] = newRoundID()
		createRound(t, x, rounds[i], testSeed, 1_000_000_000, 100)
	}
	var observed []error
	var mu sync.Mutex
	observe := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, err)
	}

	// 1. Ten concurrent bets of 150 against 1,000: six admitted.
	admitErrs := make([]error, 10)
	together(10, func(i int) {
		_, admitErrs[i] = s.Admit(ctx, request(rounds[i], cmdID(i+1), placeBet("alice", 150)))
		observe(admitErrs[i])
	})
	var admittedRounds []int
	insufficient := 0
	for i, err := range admitErrs {
		switch {
		case err == nil:
			admittedRounds = append(admittedRounds, i)
		case isInsufficient(err):
			insufficient++
		default:
			t.Fatalf("round %d: %v", i, err)
		}
	}
	if len(admittedRounds) != 6 || insufficient != 4 {
		t.Fatalf("%d admitted, %d insufficient_credits; want 6 and 4", len(admittedRounds), insufficient)
	}

	// 2. Executing the bets frees no credits for other rounds.
	stepErrs := make([]error, 20)
	together(20, func(i int) {
		if i < 10 {
			_, stepErrs[i] = x.Drain(ctx, lease(rounds[i]), 10)
		} else {
			_, stepErrs[i] = s.Admit(ctx, request(rounds[i], cmdID(100+i), placeBet("alice", 150)))
		}
		observe(stepErrs[i])
	})
	for i, err := range stepErrs {
		if i < 10 && err != nil {
			t.Errorf("Drain round %d: %v", i, err)
		}
		if i >= 10 && !isInsufficient(err) {
			t.Errorf("round %d: err = %v, want insufficient_credits", i, err)
		}
	}
	if got := playerAccount(t, pool, "alice"); got != (account{debits: 900, credits: 1000}) {
		t.Fatalf("alice after the bets = %+v, want debits 900, held 0", got)
	}

	// 3. A duplicate bet holds credits until its rejection releases them.
	r1 := rounds[admittedRounds[0]]
	if _, err := s.Admit(ctx, request(r1, cmdID(200), placeBet("alice", 100))); err != nil {
		t.Fatalf("second bet in round 1: %v", err)
	}
	if got := playerAccount(t, pool, "alice"); got.held != 100 {
		t.Errorf("alice held %d, want 100", got.held)
	}
	out, err := x.ExecuteNext(ctx, lease(r1))
	if err != nil || !out.Result.Rejected || out.Result.Reason != string(v1.ReasonDuplicateBet) {
		t.Fatalf("second bet = %+v, %v; want rejected duplicate_bet", out, err)
	}
	if _, err := s.Admit(ctx, request(rounds[10], cmdID(201), placeBet("alice", 100))); err != nil {
		t.Fatalf("bet in round 11: %v", err)
	}
	if out, err := x.ExecuteNext(ctx, lease(rounds[10])); err != nil || out.Result.Rejected {
		t.Fatalf("bet in round 11 = %+v, %v; want accepted", out, err)
	}

	// 4. Final state.
	if got := playerAccount(t, pool, "alice"); got != (account{debits: 1000, credits: 1000}) {
		t.Errorf("alice = %+v, want credits 1000, debits 1000, held 0", got)
	}
	if h, r := count(t, pool, `SELECT count(*) FROM orvex.ledger_holds`), count(t, pool, `SELECT count(*) FROM orvex.ledger_hold_releases`); h != 8 || r != 8 {
		t.Errorf("%d holds, %d releases; want 8 and 8", h, r)
	}
	requireBalanced(t, pool)
	for _, err := range observed {
		if postgres.SQLState(err) == "23514" {
			t.Errorf("a call observed 23514: %v", err)
		}
	}
}

// waitLockWaiters polls pg_stat_activity until at least one backend of
// this database waits on a lock.
func waitLockWaiters(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for count(t, db, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no backend blocked on a lock within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCrossRoundAdmissionWaitsForExecution(t *testing.T) {
	for _, c := range []struct {
		name     string
		maxBet   int64
		rejected bool
	}{{"bet accepted", 1_000_000_000, false}, {"bet rejected", 100, true}} {
		t.Run(c.name, func(t *testing.T) {
			_, pool := newDB(t)
			fdb := &faultDB{pool: pool}
			x := roundtest.NewExecutor(t, fdb, nil, nil)
			s := newSequencer(t, pool)
			roundtest.FundPlayer(t, pool, "alice", 200)
			r1, r2 := newRoundID(), newRoundID()
			createRound(t, x, r1, testSeed, c.maxBet, 100)
			createRound(t, x, r2, testSeed, 1_000_000_000, 100)
			if _, err := s.Admit(ctx, request(r1, cmdID(1), placeBet("alice", 150))); err != nil {
				t.Fatal(err)
			}

			paused, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			fdb.set(func(f *faultDB) {
				f.beforeCommit = func(pgx.Tx) error {
					once.Do(func() { close(paused) })
					<-release
					return nil
				}
			})
			type execResult struct {
				out execution.Outcome
				err error
			}
			executed := make(chan execResult, 1)
			go func() {
				out, err := x.ExecuteNext(ctx, lease(r1))
				executed <- execResult{out, err}
			}()
			<-paused
			admitted := make(chan error, 1)
			go func() {
				_, err := s.Admit(ctx, request(r2, cmdID(2), placeBet("alice", 60)))
				admitted <- err
			}()
			waitLockWaiters(t, pool)
			select {
			case err := <-admitted:
				t.Fatalf("the round-2 admission finished while B held alice's row: %v", err)
			default:
			}
			close(release)
			r := <-executed
			if r.err != nil || r.out.Result.Rejected != c.rejected {
				t.Fatalf("ExecuteNext = %+v, %v; want rejected %v", r.out, r.err, c.rejected)
			}
			err := <-admitted
			if c.rejected {
				if err != nil {
					t.Errorf("round-2 admission = %v, want admitted (available 200)", err)
				}
			} else if !isInsufficient(err) {
				t.Errorf("round-2 admission = %v, want insufficient_credits", err)
			}
		})
	}
}

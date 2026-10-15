//go:build integration

package admission_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/runtime/admission"
)

// Concurrency tests assert only invariants that hold under every
// interleaving: sets of sequences, row counts, monotonicity and the
// equality of each acknowledgment with its committed row. They never
// assert timings or which caller wins.

// roundRows returns a round's admissions in sequence order.
func roundRows(t *testing.T, db *pgxpool.Pool, round [16]byte) []admissionRow {
	t.Helper()
	rows, err := db.Query(ctx, `SELECT round_id, sequence, effective_time_unix_micro, command
		FROM orvex.admissions WHERE round_id = $1 ORDER BY sequence`, round)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (admissionRow, error) {
		var a admissionRow
		err := r.Scan(&a.roundID, &a.sequence, &a.effTime, &a.command)
		return a, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// requireGaplessMonotonic requires the round's rows to be sequences 1..n
// with non-decreasing effective times.
func requireGaplessMonotonic(t *testing.T, db *pgxpool.Pool, round [16]byte, n int) {
	t.Helper()
	rows := roundRows(t, db, round)
	if len(rows) != n {
		t.Errorf("round has %d admissions, want %d", len(rows), n)
	}
	for i, r := range rows {
		if r.sequence != int64(i+1) {
			t.Errorf("admission %d has sequence %d", i+1, r.sequence)
			return
		}
		if i > 0 && r.effTime < rows[i-1].effTime {
			t.Errorf("effective time decreases at sequence %d: %d < %d", r.sequence, r.effTime, rows[i-1].effTime)
		}
	}
}

// requireSequenceSet requires got to be exactly {1..n}.
func requireSequenceSet(t *testing.T, got []uint64, n int) {
	t.Helper()
	s := slices.Clone(got)
	slices.Sort(s)
	if len(s) != n {
		t.Errorf("%d sequences, want %d", len(s), n)
		return
	}
	for i, v := range s {
		if v != uint64(i+1) {
			t.Errorf("sequences are not exactly {1..%d}: position %d holds %d", n, i+1, v)
			return
		}
	}
}

func TestAdmitConcurrentDuplicates(t *testing.T) {
	e := newEnv(t, 16)
	s := newSequencer(t, e.pool, nil)
	round := newRoundID()
	createRound(t, e.pool, round, testSeed)
	fundPlayer(t, e.pool, "alice", 1000)

	const goroutines = 64
	for k := 1; k <= 10; k++ {
		req := request(round, cmdID(k), placeBet("alice", 10))
		results := make([]admission.Admission, goroutines)
		var errs errorList
		together(goroutines, func(i int) {
			a, err := s.Admit(ctx, req)
			if err != nil {
				errs.add("trial %d goroutine %d: %v", k, i, err)
				return
			}
			results[i] = a
		})
		errs.report(t)

		fresh := 0
		for i, a := range results {
			if !a.Duplicate {
				fresh++
			}
			b := a
			b.Duplicate = results[0].Duplicate
			if b != results[0] {
				t.Errorf("trial %d: goroutine %d got %+v, goroutine 0 got %+v", k, i, a, results[0])
			}
		}
		if fresh != 1 {
			t.Errorf("trial %d: %d admissions with Duplicate = false, want 1", k, fresh)
		}
		if results[0].Sequence != uint64(k) {
			t.Errorf("trial %d: sequence %d, want %d: duplicates consumed sequences", k, results[0].Sequence, k)
		}
		if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions WHERE command_id = $1`, cmdID(k)); n != 1 {
			t.Errorf("trial %d: %d rows with the command ID, want 1", k, n)
		}
		requireRow(t, e.obs, results[0])
		if h := head(t, e.obs, round); h.admitted != int64(k) {
			t.Errorf("trial %d: admitted_sequence = %d, want %d", k, h.admitted, k)
		}
		if n := count(t, e.obs, `SELECT count(*) FROM orvex.ledger_holds WHERE round_id = $1`, round); n != int64(k) {
			t.Errorf("trial %d: %d hold rows, want %d", k, n, k)
		}
		if h := held(t, e.obs, "alice"); h != int64(10*k) {
			t.Errorf("trial %d: alice held %d, want %d", k, h, 10*k)
		}
	}
}

func TestAdmitConcurrentDuplicatesAcrossRounds(t *testing.T) {
	e := newEnv(t, 16)
	s := newSequencer(t, e.pool, nil)
	rounds := [2][16]byte{newRoundID(), newRoundID()}
	createRound(t, e.pool, rounds[0], testSeed)
	createRound(t, e.pool, rounds[1], testSeed)

	const goroutines = 32
	for k := 1; k <= 5; k++ {
		before := [2]int64{head(t, e.obs, rounds[0]).admitted, head(t, e.obs, rounds[1]).admitted}
		id := cmdID(k)
		results := make([]admission.Admission, goroutines)
		errsOf := make([]error, goroutines)
		together(goroutines, func(i int) {
			results[i], errsOf[i] = s.Admit(ctx, request(rounds[i%2], id, cashOut("bob")))
		})

		r, ok := row(t, e.obs, id)
		if !ok {
			t.Fatalf("trial %d: no admission row for the command ID", k)
		}
		if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions WHERE command_id = $1`, id); n != 1 {
			t.Errorf("trial %d: %d rows with the command ID, want 1", k, n)
		}
		w := 0
		if r.roundID == rounds[1] {
			w = 1
		}
		var winner *admission.Admission
		fresh := 0
		for i := range goroutines {
			if i%2 != w {
				if !errors.Is(errsOf[i], admission.ErrCommandIDReused) {
					t.Errorf("trial %d: goroutine %d (other round): err = %v, want ErrCommandIDReused", k, i, errsOf[i])
				}
				continue
			}
			if errsOf[i] != nil {
				t.Errorf("trial %d: goroutine %d (winning round): %v", k, i, errsOf[i])
				continue
			}
			a := results[i]
			if !a.Duplicate {
				fresh++
			}
			a.Duplicate = false
			if winner == nil {
				winner = &a
				requireRow(t, e.obs, a)
			} else if a != *winner {
				t.Errorf("trial %d: goroutine %d got %+v, want %+v", k, i, a, *winner)
			}
		}
		if fresh != 1 {
			t.Errorf("trial %d: %d admissions with Duplicate = false, want 1", k, fresh)
		}
		if after := head(t, e.obs, rounds[1-w]).admitted; after != before[1-w] {
			t.Errorf("trial %d: the other round's admitted_sequence moved from %d to %d", k, before[1-w], after)
		}
	}
}

func TestAdmitHotRoundOrdering(t *testing.T) {
	for _, gate := range []int{4, 16} {
		t.Run(fmt.Sprintf("gate %d", gate), func(t *testing.T) {
			e := newEnv(t, 16)
			s := newSequencer(t, e.pool, func(c *admission.Config) { c.MaxInFlightPerRound = gate })
			round := newRoundID()
			createRound(t, e.pool, round, testSeed)
			const goroutines, perGoroutine = 32, 50
			player := func(g int) string { return fmt.Sprintf("p%d", g) }
			for g := range goroutines {
				fundPlayer(t, e.pool, player(g), 25)
			}

			acks := make([][]admission.Admission, goroutines)
			var errs errorList
			together(goroutines, func(g int) {
				for i := range perGoroutine {
					req := request(round, cmdID(g*1000+i+1), cashOut(player(g)))
					if i%2 == 0 {
						req.Command = placeBet(player(g), 1)
					}
					a, err := s.Admit(ctx, req)
					if err != nil {
						errs.add("goroutine %d submission %d: %v", g, i, err)
						return
					}
					acks[g] = append(acks[g], a)
				}
			})
			errs.report(t)

			var all []uint64
			for g, as := range acks {
				for i, a := range as {
					all = append(all, a.Sequence)
					if i > 0 && a.Sequence <= as[i-1].Sequence {
						t.Errorf("goroutine %d: sequence %d after %d", g, a.Sequence, as[i-1].Sequence)
					}
					if a.Duplicate {
						t.Errorf("goroutine %d: %+v is a duplicate", g, a)
					}
					requireRow(t, e.obs, a)
				}
			}
			const total = goroutines * perGoroutine
			requireSequenceSet(t, all, total)
			if h := head(t, e.obs, round); h.admitted != total || h.state != "open" {
				t.Errorf("head = %+v, want admitted %d, open", h, total)
			}
			requireGaplessMonotonic(t, e.obs, round, total)
			if n := count(t, e.obs, `SELECT count(*) FROM orvex.ledger_holds WHERE round_id = $1`, round); n != total/2 {
				t.Errorf("%d hold rows, want %d", n, total/2)
			}
			for g := range goroutines {
				if h := held(t, e.obs, player(g)); h != 25 {
					t.Errorf("%s held %d, want 25", player(g), h)
				}
			}
		})
	}
}

func TestAdmitManyRoundsIndependentSequences(t *testing.T) {
	e := newEnv(t, 16)
	s := newSequencer(t, e.pool, nil)
	const rounds, perRound, each = 8, 8, 25
	ids := make([][16]byte, rounds)
	for r := range ids {
		ids[r] = newRoundID()
		createRound(t, e.pool, ids[r], testSeed)
	}
	acks := make([][]admission.Admission, rounds*perRound)
	var errs errorList
	together(rounds*perRound, func(g int) {
		r := g % rounds
		for i := range each {
			a, err := s.Admit(ctx, request(ids[r], cmdID(g*1000+i+1), cashOut(fmt.Sprintf("p%d", g))))
			if err != nil {
				errs.add("goroutine %d submission %d: %v", g, i, err)
				return
			}
			acks[g] = append(acks[g], a)
		}
	})
	errs.report(t)

	byRound := make([][]uint64, rounds)
	for g, as := range acks {
		for _, a := range as {
			if a.RoundID != ids[g%rounds] {
				t.Errorf("goroutine %d: admission for round %x", g, a.RoundID)
			}
			byRound[g%rounds] = append(byRound[g%rounds], a.Sequence)
			requireRow(t, e.obs, a)
		}
	}
	for r := range rounds {
		requireSequenceSet(t, byRound[r], perRound*each)
		if h := head(t, e.obs, ids[r]); h.admitted != perRound*each {
			t.Errorf("round %d: admitted_sequence = %d, want %d", r, h.admitted, perRound*each)
		}
		requireGaplessMonotonic(t, e.obs, ids[r], perRound*each)
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.admissions`); n != rounds*perRound*each {
		t.Errorf("%d admissions, want %d", n, rounds*perRound*each)
	}
}

func TestAdmitConcurrentCrossRoundHolds(t *testing.T) {
	e := newEnv(t, 16)
	s := newSequencer(t, e.pool, nil)
	rounds := [2][16]byte{newRoundID(), newRoundID()}
	createRound(t, e.pool, rounds[0], testSeed)
	createRound(t, e.pool, rounds[1], testSeed)
	fundPlayer(t, e.pool, "alice", 150)

	const goroutines = 16
	results := make([]admission.Admission, goroutines)
	errsOf := make([]error, goroutines)
	together(goroutines, func(i int) {
		results[i], errsOf[i] = s.Admit(ctx, request(rounds[i%2], cmdID(i+1), placeBet("alice", 100)))
	})
	ok, rejected := 0, 0
	for i, err := range errsOf {
		var re *admission.RejectedError
		switch {
		case err == nil:
			ok++
			requireRow(t, e.obs, results[i])
		case errors.As(err, &re) && re.Reason == admission.ReasonInsufficientCredits:
			rejected++
			requireNoRow(t, e.obs, cmdID(i+1))
		default:
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	if ok != 1 || rejected != goroutines-1 {
		t.Errorf("%d admitted and %d insufficient_credits, want 1 and %d", ok, rejected, goroutines-1)
	}
	if h := held(t, e.obs, "alice"); h != 100 {
		t.Errorf("alice held %d, want 100", h)
	}
	if n := count(t, e.obs, `SELECT count(*) FROM orvex.ledger_holds`); n != 1 {
		t.Errorf("%d hold rows, want 1", n)
	}
	if sum := head(t, e.obs, rounds[0]).admitted + head(t, e.obs, rounds[1]).admitted; sum != 1 {
		t.Errorf("admitted sequences sum to %d, want 1", sum)
	}
}

func TestAdmitHotRoundDoesNotStarveOtherRounds(t *testing.T) {
	e := newEnv(t, 8)
	s := newSequencer(t, e.pool, func(c *admission.Config) { c.MaxInFlightPerRound = 4 })
	roundA, roundB := newRoundID(), newRoundID()
	createRound(t, e.pool, roundA, testSeed)
	createRound(t, e.pool, roundB, testSeed)

	blocker := holdHead(t, e, roundA)
	const goroutines = 32
	acks := make([]admission.Admission, goroutines)
	errsOf := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Go(func() {
		together(goroutines, func(i int) {
			acks[i], errsOf[i] = s.Admit(ctx, request(roundA, cmdID(i+1), cashOut(fmt.Sprintf("p%d", i))))
		})
	})

	waitLockWaiters(t, e.obs, 4)
	for range 5 {
		if n := lockWaiters(t, e.obs); n > 4 {
			t.Errorf("%d backends wait on a lock, want at most 4 (the round's gate)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	b, err := s.Admit(c, request(roundB, cmdID(1000), cashOut("other")))
	cancel()
	if err != nil {
		t.Errorf("round B admission while round A is blocked: %v", err)
	} else {
		requireRow(t, e.obs, b)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	var seqs []uint64
	for i, err := range errsOf {
		if err != nil {
			t.Errorf("round A goroutine %d: %v", i, err)
			continue
		}
		seqs = append(seqs, acks[i].Sequence)
		requireRow(t, e.obs, acks[i])
	}
	requireSequenceSet(t, seqs, goroutines)
}

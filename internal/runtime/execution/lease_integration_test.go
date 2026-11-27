//go:build integration

package execution_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

func TestClaimLeaseTakeover(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := betRound(t, pool, x)

	a, err := x.ClaimLease(ctx, round, "fixture", time.Minute)
	if err != nil || a != (execution.Lease{RoundID: round, FencingToken: 1}) {
		t.Fatalf("ClaimLease a = %+v, %v; want token 1", a, err)
	}
	var (
		owner   string
		expires bool
	)
	if err := pool.QueryRow(ctx, `SELECT owner_id, lease_expires_at > clock_timestamp()
		FROM orvex.round_execution_heads WHERE round_id = $1`, round).Scan(&owner, &expires); err != nil {
		t.Fatal(err)
	}
	if owner != "fixture" || !expires {
		t.Errorf("head owner %q, lease in the future %v; want fixture, true", owner, expires)
	}
	if _, err := x.ExecuteNext(ctx, execution.Lease{RoundID: round}); !errors.Is(err, execution.ErrFenced) {
		t.Fatalf("token 0 after the claim: err = %v, want ErrFenced", err)
	}
	if out, err := x.ExecuteNext(ctx, a); err != nil || out.Sequence != 1 {
		t.Fatalf("token 1 = %+v, %v; want sequence 1", out, err)
	}

	y := roundtest.NewExecutor(t, pool, nil, nil)
	if _, err := y.ClaimLease(ctx, round, "engine-b", time.Minute); !errors.Is(err, execution.ErrLeaseHeld) {
		t.Fatalf("live takeover: %v", err)
	}
	if err := y.RenewLease(ctx, a, "engine-b", time.Minute); !errors.Is(err, execution.ErrFenced) {
		t.Fatalf("renewing another owner's lease: %v", err)
	}
	if err := x.RenewLease(ctx, a, "fixture", time.Minute); err != nil {
		t.Fatal(err)
	}
	expireLease(t, pool, round)
	if err := x.RenewLease(ctx, a, "fixture", time.Minute); !errors.Is(err, execution.ErrFenced) {
		t.Fatalf("expired renewal: %v", err)
	}
	before := effects(t, pool, round)
	if _, err := x.ExecuteNext(ctx, a); !errors.Is(err, execution.ErrFenced) {
		t.Fatalf("expired write: %v", err)
	}
	requireEffects(t, effects(t, pool, round), before)
	b, err := y.ClaimLease(ctx, round, "engine-b", time.Minute)
	if err != nil || b.FencingToken != 2 {
		t.Fatalf("ClaimLease b = %+v, %v; want token 2", b, err)
	}
	if _, err := x.ExecuteNext(ctx, a); !errors.Is(err, execution.ErrFenced) {
		t.Fatalf("token 1 after the second claim: err = %v, want ErrFenced", err)
	}
	if out, err := y.ExecuteNext(ctx, b); err != nil || out.Sequence != 2 {
		t.Fatalf("token 2 = %+v, %v; want sequence 2", out, err)
	}

	if _, err := x.ClaimLease(ctx, newRoundID(), "fixture", time.Minute); !errors.Is(err, execution.ErrRoundNotFound) {
		t.Errorf("unknown round: err = %v, want ErrRoundNotFound", err)
	}
}

func TestTakeoverHasOneWinner(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := betRound(t, pool, x)
	expireLease(t, pool, round)
	start := make(chan struct{})
	type claim struct {
		lease execution.Lease
		err   error
	}
	results := make(chan claim, 2)
	for _, owner := range []string{"engine-b", "engine-c"} {
		go func() { <-start; l, err := x.ClaimLease(ctx, round, owner, time.Minute); results <- claim{l, err} }()
	}
	close(start)
	wins := 0
	for range 2 {
		r := <-results
		switch {
		case r.err == nil && r.lease.FencingToken == 2:
			wins++
		case errors.Is(r.err, execution.ErrLeaseHeld):
		default:
			t.Fatalf("unexpected claim %+v: %v", r.lease, r.err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d winners, want one", wins)
	}
}

// TestStateFollowsExecution: State returns the cached state as execution
// advances, and a fresh executor's cold load returns the same state.
func TestStateFollowsExecution(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := betRound(t, pool, x)

	requirePhase := func(x *execution.Executor, l execution.Lease, want v1.Phase) v1.State {
		t.Helper()
		got, err := x.State(ctx, l)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		s, ok := got.(v1.State)
		if !ok || s.Phase != want {
			t.Fatalf("State phase = %v (%T), want %v", s.Phase, got, want)
		}
		return s
	}
	requirePhase(x, lease(round), v1.PhaseBetting)
	mustDrain(t, x, round, 2)
	cached := requirePhase(x, lease(round), v1.PhaseRunning)
	if cached.StartedAtUnixMicro != 2_000 || len(cached.Bets) != 1 {
		t.Errorf("cached state started %d with %d bets, want 2000 and 1", cached.StartedAtUnixMicro, len(cached.Bets))
	}

	y := roundtest.NewExecutor(t, pool, nil, nil)
	if cold := requirePhase(y, lease(round), v1.PhaseRunning); !reflect.DeepEqual(cold, cached) {
		t.Error("cold-loaded state differs from the cached state")
	}

	if _, err := x.ClaimLease(ctx, round, "fixture", time.Minute); err != nil {
		t.Fatal(err)
	}
	z := roundtest.NewExecutor(t, pool, nil, nil)
	if _, err := z.State(ctx, execution.Lease{RoundID: round}); !errors.Is(err, execution.ErrFenced) {
		t.Errorf("cold load under a superseded token: err = %v, want ErrFenced", err)
	}
}

// TestStateHaltsCorruptRound: a sealed secret that does not open halts the
// round; State keeps returning the halt.
func TestStateHaltsCorruptRound(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := newRoundID()
	createRound(t, x, round, testSeed, 1_000_000_000, 100)
	pgtest.ReplicaExec(t, pool, `UPDATE orvex.round_secrets
		SET secret_ciphertext = set_byte(secret_ciphertext, 0, get_byte(secret_ciphertext, 0) # 1) WHERE round_id = $1`, round)

	y := roundtest.NewExecutor(t, pool, nil, nil)
	for i := range 2 {
		if _, err := y.State(ctx, lease(round)); !errors.Is(err, execution.ErrCorrupt) {
			t.Fatalf("State call %d: err = %v, want ErrCorrupt", i+1, err)
		}
	}
}

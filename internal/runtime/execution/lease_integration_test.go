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

// TestClaimLeaseTakeover: each claim raises the fencing token by one and
// names its owner; the previous lease is fenced at its next Transaction B,
// and the newest lease executes.
func TestClaimLeaseTakeover(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := betRound(t, pool, x)

	a, err := x.ClaimLease(ctx, round, "engine-a", time.Minute)
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
	if owner != "engine-a" || !expires {
		t.Errorf("head owner %q, lease in the future %v; want engine-a, true", owner, expires)
	}
	if _, err := x.ExecuteNext(ctx, lease(round)); !errors.Is(err, execution.ErrFenced) {
		t.Fatalf("token 0 after the claim: err = %v, want ErrFenced", err)
	}
	if out, err := x.ExecuteNext(ctx, a); err != nil || out.Sequence != 1 {
		t.Fatalf("token 1 = %+v, %v; want sequence 1", out, err)
	}

	y := roundtest.NewExecutor(t, pool, nil, nil)
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

	if _, err := x.ClaimLease(ctx, newRoundID(), "engine-a", time.Minute); !errors.Is(err, execution.ErrRoundNotFound) {
		t.Errorf("unknown round: err = %v, want ErrRoundNotFound", err)
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

	if _, err := x.ClaimLease(ctx, round, "engine-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	z := roundtest.NewExecutor(t, pool, nil, nil)
	if _, err := z.State(ctx, lease(round)); !errors.Is(err, execution.ErrFenced) {
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

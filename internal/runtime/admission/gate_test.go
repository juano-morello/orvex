package admission

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// size returns the number of rounds in the gate's map.
func (g *gate) size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.rounds)
}

func round(n byte) [16]byte { return [16]byte{0: 0x01, 15: n} }

func mustAcquire(t *testing.T, g *gate, r [16]byte) func() {
	t.Helper()
	release, err := g.acquire(context.Background(), r)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return release
}

// acquireAsync acquires in a goroutine and reports the release function
// once it has a slot.
func acquireAsync(g *gate, ctx context.Context, r [16]byte) (<-chan func(), <-chan error) {
	got := make(chan func(), 1)
	errc := make(chan error, 1)
	go func() {
		release, err := g.acquire(ctx, r)
		if err != nil {
			errc <- err
			return
		}
		got <- release
	}()
	return got, errc
}

// waitRefs polls until round r has n acquirers.
func waitRefs(t *testing.T, g *gate, r [16]byte, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		g.mu.Lock()
		e := g.rounds[r]
		refs := 0
		if e != nil {
			refs = e.refs
		}
		g.mu.Unlock()
		if refs == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("round has %d acquirers, want %d", refs, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGateBoundsPerRound(t *testing.T) {
	g := newGate(2)
	r := round(1)
	r1 := mustAcquire(t, g, r)
	r2 := mustAcquire(t, g, r)
	got, errc := acquireAsync(g, context.Background(), r)
	waitRefs(t, g, r, 3)
	select {
	case <-got:
		t.Fatal("third acquire succeeded while two slots are held")
	case err := <-errc:
		t.Fatalf("third acquire: %v", err)
	default:
	}
	r1()
	r1() // a second call is a no-op
	select {
	case r3 := <-got:
		r3()
	case <-time.After(10 * time.Second):
		t.Fatal("third acquire did not proceed after a release")
	}
	r2()
	if n := g.size(); n != 0 {
		t.Errorf("gate holds %d rounds, want 0", n)
	}
}

func TestGateRoundsIndependent(t *testing.T) {
	g := newGate(1)
	ra := mustAcquire(t, g, round(1))
	defer ra()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rb, err := g.acquire(ctx, round(2))
	if err != nil {
		t.Fatalf("acquire on another round: %v", err)
	}
	rb()
}

func TestGateCancelWhileWaiting(t *testing.T) {
	g := newGate(1)
	r := round(1)
	held := mustAcquire(t, g, r)
	ctx, cancel := context.WithCancel(context.Background())
	got, errc := acquireAsync(g, ctx, r)
	waitRefs(t, g, r, 2)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("acquire err = %v, want context.Canceled", err)
		}
	case <-got:
		t.Fatal("cancelled acquire got a slot")
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled acquire did not return")
	}
	waitRefs(t, g, r, 1)
	held()
	// The cancelled waiter took no slot: the next acquire proceeds at once.
	mustAcquire(t, g, r)()
	if _, err := g.acquire(ctx, r); !errors.Is(err, context.Canceled) {
		t.Errorf("acquire with a done ctx: err = %v, want context.Canceled", err)
	}
	if n := g.size(); n != 0 {
		t.Errorf("gate holds %d rounds, want 0", n)
	}
}

func TestGateReleasesEntries(t *testing.T) {
	g := newGate(2)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := range 100 {
		wg.Go(func() {
			release, err := g.acquire(context.Background(), round(byte(i%10)))
			if err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Millisecond)
			release()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("acquire: %v", err)
	}
	if n := g.size(); n != 0 {
		t.Errorf("gate holds %d rounds after every release, want 0", n)
	}
}

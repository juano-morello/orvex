package admission

import (
	"context"
	"sync"
)

// gate bounds the Admit calls per round that hold a pool connection at once
// (S-4), so one hot round cannot take every pooled connection. Correctness
// never depends on it. The map holds only rounds with calls in flight.
type gate struct {
	capacity int

	mu     sync.Mutex
	rounds map[[16]byte]*gateEntry
}

type gateEntry struct {
	slots chan struct{}
	refs  int // acquirers holding or waiting for a slot
}

func newGate(capacity int) *gate {
	return &gate{capacity: capacity, rounds: make(map[[16]byte]*gateEntry)}
}

// acquire waits for one of round's slots or for ctx to end. On success it
// returns a release function, which is safe to call more than once; on
// failure it returns ctx.Err() and holds no slot.
func (g *gate) acquire(ctx context.Context, round [16]byte) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	e := g.rounds[round]
	if e == nil {
		e = &gateEntry{slots: make(chan struct{}, g.capacity)}
		g.rounds[round] = e
	}
	e.refs++
	g.mu.Unlock()

	select {
	case e.slots <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-e.slots
				g.unref(round, e)
			})
		}, nil
	case <-ctx.Done():
		g.unref(round, e)
		return nil, ctx.Err()
	}
}

func (g *gate) unref(round [16]byte, e *gateEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(g.rounds, round)
	}
}

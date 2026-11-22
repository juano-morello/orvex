//go:build integration && acceptance

package firstround

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseEvent is one dispatched Server-Sent Event.
type sseEvent struct {
	id    string
	hasID bool
	event string
	data  string
}

// readSSE parses r as text/event-stream and calls emit for each event. It
// returns emit's first error, the read error, or nil at EOF.
func readSSE(r io.Reader, emit func(sseEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	var ev sseEvent
	var data []string
	pending := false
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if pending {
				ev.data = strings.Join(data, "\n")
				if err := emit(ev); err != nil {
					return err
				}
			}
			ev, data, pending = sseEvent{}, nil, false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			ev.id, ev.hasID, pending = value, true, true
		case "event":
			ev.event, pending = value, true
		case "data":
			data, pending = append(data, value), true
		}
	}
	return sc.Err()
}

// frame is one record frame of a round stream.
type frame struct {
	Sequence int64
	Data     string
	Events   []wireEvent
}

func (f frame) has(typ string) bool {
	return slices.ContainsFunc(f.Events, func(e wireEvent) bool { return e.Type == typ })
}

func (f frame) types() []string {
	out := make([]string, len(f.Events))
	for i, e := range f.Events {
		out[i] = e.Type
	}
	return out
}

// streamState is what a watcher has seen so far: record frames 0..n gapless,
// and whether the server sent "end".
type streamState struct {
	frames []frame
	ended  bool
}

func (s streamState) event(typ string) (frame, bool) {
	for _, f := range s.frames {
		if f.has(typ) {
			return f, true
		}
	}
	return frame{}, false
}

// violation is a stream that breaks the delivery contract; the watcher
// stops on it instead of reconnecting.
type violation struct{ msg string }

func (v violation) Error() string { return v.msg }

func violationf(format string, args ...any) error { return violation{fmt.Sprintf(format, args...)} }

var errEnded = errors.New("end of round stream")

// watcher follows one round's stream from genesis like a client: on a
// dropped connection it reconnects with Last-Event-ID and drops frames it
// already has. Within one connection frames must be strictly increasing;
// overall they must be gapless from sequence 0.
type watcher struct {
	p     *player
	round string

	mu         sync.Mutex
	frames     []frame
	ended      bool
	failure    error
	lastErr    error
	reconnects int
	repeats    int
	changed    chan struct{}

	cancel context.CancelFunc
	done   chan struct{}
}

const reconnectDelay = 100 * time.Millisecond

func watch(t *testing.T, p *player, round string) *watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{p: p, round: round, changed: make(chan struct{}), cancel: cancel, done: make(chan struct{})}
	go w.run(ctx)
	t.Cleanup(w.close)
	return w
}

func (w *watcher) close() {
	w.cancel()
	<-w.done
}

func (w *watcher) run(ctx context.Context) {
	defer close(w.done)
	for {
		err := w.session(ctx)
		w.mu.Lock()
		var v violation
		switch {
		case errors.Is(err, errEnded):
			w.ended = true
		case errors.As(err, &v):
			w.failure = err
		default:
			w.lastErr = err
			w.reconnects++
		}
		finished := w.ended || w.failure != nil
		close(w.changed)
		w.changed = make(chan struct{})
		w.mu.Unlock()
		if finished {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func (w *watcher) cursor() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.frames)) - 1
}

func (w *watcher) session(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.p.base+"/v1/rounds/"+w.round+"/stream", nil)
	if err != nil {
		return violationf("stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.p.token)
	req.Header.Set("Accept", "text/event-stream")
	cursor := w.cursor()
	if cursor >= 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
	}
	resp, err := w.p.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		// The cursor is the round's last record: nothing more will come.
		if cursor < 0 {
			return violationf("stream answered 204 to a subscription from genesis")
		}
		return errEnded
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("stream answered %d %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return violationf("stream Content-Type %q, want text/event-stream", ct)
	}
	prev := int64(-1)
	return readSSE(resp.Body, func(ev sseEvent) error {
		if ev.event == "end" {
			return errEnded
		}
		f, err := parseFrame(ev)
		if err != nil {
			return err
		}
		if f.Sequence <= prev {
			return violationf("frame %d after frame %d on one connection", f.Sequence, prev)
		}
		prev = f.Sequence
		return w.accept(f)
	})
}

func parseFrame(ev sseEvent) (frame, error) {
	if !ev.hasID {
		return frame{}, violationf("record frame without id: event %q data %s", ev.event, ev.data)
	}
	seq, err := strconv.ParseInt(ev.id, 10, 64)
	if err != nil || seq < 0 {
		return frame{}, violationf("frame id %q is not a sequence", ev.id)
	}
	var rec struct {
		Sequence *wireInt    `json:"sequence"`
		Events   []wireEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(ev.data), &rec); err != nil {
		return frame{}, violationf("frame %d data %s: %v", seq, ev.data, err)
	}
	if rec.Sequence != nil && int64(*rec.Sequence) != seq {
		return frame{}, violationf("frame id %d carries sequence %d", seq, *rec.Sequence)
	}
	return frame{Sequence: seq, Data: ev.data, Events: rec.Events}, nil
}

func (w *watcher) accept(f frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	next := int64(len(w.frames))
	switch {
	case f.Sequence < next:
		w.repeats++
		return nil
	case f.Sequence > next:
		return violationf("gap: frame %d after %d", f.Sequence, next-1)
	}
	w.frames = append(w.frames, f)
	close(w.changed)
	w.changed = make(chan struct{})
	return nil
}

// waitFor blocks until done holds for the stream state, and fails the test
// if the stream breaks the contract, ends first, or timeout passes.
func (w *watcher) waitFor(t *testing.T, what string, timeout time.Duration, done func(streamState) bool) streamState {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		w.mu.Lock()
		s := streamState{frames: slices.Clone(w.frames), ended: w.ended}
		failure, lastErr, ch := w.failure, w.lastErr, w.changed
		w.mu.Unlock()
		if done(s) {
			return s
		}
		if failure != nil {
			t.Fatalf("%s's stream of round %s: %v", w.p.name, w.round, failure)
		}
		if s.ended {
			t.Fatalf("%s's stream of round %s ended before %s; frames:\n%s", w.p.name, w.round, what, describeFrames(s.frames))
		}
		select {
		case <-ch:
		case <-timer.C:
			t.Fatalf("%s's stream of round %s: no %s within %v; last connection error: %v; frames:\n%s",
				w.p.name, w.round, what, timeout, lastErr, describeFrames(s.frames))
		}
	}
}

func (w *watcher) waitEvent(t *testing.T, typ string, timeout time.Duration) frame {
	t.Helper()
	s := w.waitFor(t, typ, timeout, func(s streamState) bool { _, ok := s.event(typ); return ok })
	f, _ := s.event(typ)
	return f
}

func (w *watcher) waitEnd(t *testing.T, timeout time.Duration) []frame {
	t.Helper()
	return w.waitFor(t, `"end"`, timeout, func(s streamState) bool { return s.ended }).frames
}

func (w *watcher) stats() (reconnects, repeats int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reconnects, w.repeats
}

// requireFinalCursorStops subscribes to round with Last-Event-ID last, the
// round's final record, and requires the stream to stop without a frame:
// 204 under the current contract, a 200 stream whose first event is "end"
// under the baseline one.
func requireFinalCursorStops(t *testing.T, p *player, round string, last int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/v1/rounds/"+round+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", strconv.FormatInt(last, 10))
	resp, err := p.http.Do(req)
	if err != nil {
		t.Fatalf("subscribe to round %s at its final cursor %d: %v", round, last, err)
	}
	defer func() { _ = resp.Body.Close() }()
	c := contract(t)
	switch {
	case c.finalNoContent && resp.StatusCode == http.StatusNoContent:
		return
	case !c.finalNoContent && resp.StatusCode == http.StatusOK:
		var first sseEvent
		_ = readSSE(resp.Body, func(ev sseEvent) error { first = ev; return errEnded })
		if first.event != "end" {
			t.Fatalf("subscription at the final cursor %d of round %s: first event %+v, want end", last, round, first)
		}
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Fatalf("subscription at the final cursor %d of round %s = %d %s, want the %s contract's stop (204: %v)",
			last, round, resp.StatusCode, b, c.name, c.finalNoContent)
	}
}

func describeFrames(fs []frame) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "  %d %v\n", f.Sequence, f.types())
	}
	return b.String()
}

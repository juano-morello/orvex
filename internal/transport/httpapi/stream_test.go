package httpapi

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

const streamPath = "/v1/rounds/" + roundText + "/stream"

// TestStreamStalledReaderIsolated runs a reader that stops reading beside
// a healthy one on the same engine connection. The healthy reader gets
// every frame and the end; closing the stalled reader ends its engine
// subscription, and it resumes after the last frame it read.
func TestStreamStalledReaderIsolated(t *testing.T) {
	const total = 4096
	player := strings.Repeat("p", 4096)
	var (
		mu       sync.Mutex
		released = map[uint64]chan struct{}{}
		stalled  atomic.Bool
	)
	ts, _ := newGateway(t, &fakeEngine{subscribe: func(r *orvexv1.SubscribeRoundRequest, s grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse]) error {
		mu.Lock()
		done := make(chan struct{})
		released[r.GetAfterSequence()] = done
		mu.Unlock()
		defer close(done)
		if err := s.SendHeader(nil); err != nil {
			return err
		}
		for seq := r.GetAfterSequence() + 1; seq <= total; seq++ {
			if err := s.Send(&orvexv1.SubscribeRoundResponse{Sequence: seq, Body: &orvexv1.SubscribeRoundResponse_Events{Events: &orvexv1.Events{Events: []*orvexv1.CrashEvent{
				{Event: &orvexv1.CrashEvent_BetPlaced{BetPlaced: &orvexv1.BetPlaced{PlayerId: player, BetCredits: 1}}},
			}}}}); err != nil {
				return err
			}
		}
		if r.GetAfterSequence() == 0 && stalled.Load() {
			<-s.Context().Done()
			return s.Context().Err()
		}
		return nil
	}})

	stalled.Store(true)
	slow, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(slow, "GET %s?after=0 HTTP/1.1\r\nHost: x\r\n\r\n", streamPath); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(slow)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stalled reader's response = %v, %v", resp, err)
	}
	var lastID string
	for lastID == "" {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if id, ok := strings.CutPrefix(line, "id: "); ok {
			lastID = strings.TrimSpace(id)
		}
	}

	healthy := make(chan response, 1)
	go func() { healthy <- do(t, ts, "GET", streamPath+"?after=1000", "", "") }()
	select {
	case got := <-healthy:
		if got.status != 200 || strings.Count(got.body, "event: record\n") != total-1000 || !strings.HasSuffix(got.body, "event: end\ndata: {}\n") {
			t.Fatalf("healthy reader = %d with %d records, want %d records then end", got.status, strings.Count(got.body, "event: record\n"), total-1000)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the healthy reader did not finish beside the stalled one")
	}

	_ = slow.Close()
	mu.Lock()
	done := released[0]
	mu.Unlock()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("closing the stalled reader did not end its engine subscription")
	}
	stalled.Store(false)
	resumed := do(t, ts, "GET", streamPath, "", "", "Last-Event-ID", lastID)
	if !strings.HasPrefix(resumed.body, "retry: 1000\n\nid: "+next(t, lastID)+"\n") || !strings.HasSuffix(resumed.body, "event: end\ndata: {}\n") {
		t.Errorf("resumed after %s: %.80q...%q, want the next record first and end last", lastID, resumed.body, resumed.body[max(0, len(resumed.body)-40):])
	}
}

func next(t *testing.T, id string) string {
	t.Helper()
	n, ok := parseUint(id)
	if !ok {
		t.Fatalf("frame id %q", id)
	}
	return fmt.Sprint(n + 1)
}

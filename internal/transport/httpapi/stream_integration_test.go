//go:build integration

package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/sessions"
	"github.com/juano-morello/orvex/internal/transport/engineapi"
	"github.com/juano-morello/orvex/internal/transport/httpapi"
)

// engine runs engineapi.Serve on one loopback address, and can stop and
// start again there while the gateway keeps running.
type engine struct {
	addr string
	pool *pgxpool.Pool
	seq  *admission.Sequencer
	stop func()
}

func (e *engine) start(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- engineapi.Serve(sctx, ln, e.pool, e.seq, serviceToken) }()
	e.stop = func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
		e.stop = func() {}
	}
}

// newRestartableStack is newStack with an engine the test can restart.
func newRestartableStack(t *testing.T) (*stack, *engine) {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	reg := roundtest.Registry(t)
	seq, err := admission.New(pool, reg, admission.DefaultConfig(), admission.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &engine{addr: ln.Addr().String(), pool: pool, seq: seq}
	_ = ln.Close()
	e.start(t)
	client, err := engineapi.Dial(e.addr, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	players, err := sessions.ParsePlayers([]byte(`{"` + aliceToken + `":"alice"}`))
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(client, players, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(api)
	t.Cleanup(func() {
		api.CloseStreams()
		ts.Close()
		_ = client.Close()
		e.stop()
	})
	return &stack{pool: pool, x: roundtest.NewExecutor(t, pool, nil, reg), seq: seq, url: ts.URL}, e
}

type rawResponse struct {
	status int
	header http.Header
	body   string
}

// get reads a whole stream response.
func (s *stack) get(t *testing.T, round [16]byte, lastEventID string) rawResponse {
	t.Helper()
	req, err := http.NewRequest("GET", s.url+"/v1/rounds/"+uuidText(round)+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", lastEventID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rawResponse{resp.StatusCode, resp.Header, string(b)}
}

func ids(events []sse) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.id
		if ev.event == "end" {
			out[i] = "end"
		}
	}
	return out
}

func checkIDs(t *testing.T, what string, events []sse, want ...string) {
	t.Helper()
	got := ids(events)
	if len(got) != len(want) {
		t.Fatalf("%s: events %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: events %v, want %v", what, got, want)
		}
	}
}

// TestStreamFollowsARoundAcrossAnEngineRestart follows a round as native
// EventSource does: after a response that ends without "end" it
// reconnects with Last-Event-ID, and it stops for good on any status but
// 200. While the engine is down the gateway must answer 200 with only the
// reconnection time.
func TestStreamFollowsARoundAcrossAnEngineRestart(t *testing.T) {
	s, e := newRestartableStack(t)
	round, _, d := s.createRound(t, 0, 100*time.Millisecond)
	roundtest.FundPlayer(t, s.pool, "alice", 1000)

	code, live := s.openStream(t, round, "", "")
	if code != 200 {
		t.Fatalf("stream = %d", code)
	}
	if r := s.submit(t, aliceToken, round, randomUUID(t), placeBet("100")); r.status != 202 {
		t.Fatalf("bet = %d %v", r.status, r.body)
	}
	s.drain(t, round, 1)
	var before []sse
	for len(before) < 2 {
		ev, ok := <-live
		if !ok {
			t.Fatalf("stream ended after %v", ids(before))
		}
		before = append(before, ev)
	}
	e.stop()
	before = append(before, collect(t, live)...)
	checkIDs(t, "stream until the engine stopped", before, "0", "1")

	for down := time.Now(); time.Since(down) < 1200*time.Millisecond; time.Sleep(100 * time.Millisecond) {
		if r := s.get(t, round, "1"); r.status != 200 || r.header.Get("Content-Type") != "text/event-stream" || r.body != "retry: 1000\n\n" {
			t.Fatalf("stream with the engine down = %d %v %q, want 200 text/event-stream with only the retry time", r.status, r.header, r.body)
		}
	}
	start, err := s.system(t, round, v1.StartRound{Authority: v1.AuthoritySystem})
	if err != nil {
		t.Fatal(err)
	}
	s.drain(t, round, 1)
	e.start(t)

	var resumed <-chan sse
	var first sse
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stream did not resume after the engine restarted")
		}
		code, events := s.openStream(t, round, "", "1")
		if code != 200 {
			t.Fatalf("reconnect after the restart = %d, want 200", code)
		}
		ev, ok := <-events
		if ok {
			resumed, first = events, ev
			break
		}
	}
	if code, atHead := s.openStream(t, round, "", "2"); code != 200 {
		t.Fatalf("stream at the live head = %d, want 200", code)
	} else {
		s.waitDBClock(t, start.EffectiveTimeUnixMicro+d)
		if _, err := s.system(t, round, v1.CloseRound{Authority: v1.AuthoritySystem}); err != nil {
			t.Fatal(err)
		}
		s.drain(t, round, 1)
		if _, err := s.system(t, round, v1.SettleRound{Authority: v1.AuthoritySystem}); err != nil {
			t.Fatal(err)
		}
		s.drain(t, round, 1)
		checkIDs(t, "stream from the live head", collect(t, atHead), "3", "4", "end")
	}
	after := append([]sse{first}, collect(t, resumed)...)
	checkIDs(t, "stream resumed with Last-Event-ID 1", after, "2", "3", "4", "end")

	if r := s.get(t, round, "4"); r.status != 204 || r.body != "" {
		t.Errorf("reconnect at the last record = %d %q, want 204", r.status, r.body)
	}

	_, replay := s.openStream(t, round, "", "")
	all := collect(t, replay)
	checkIDs(t, "replay from genesis", all, "0", "1", "2", "3", "4", "end")
	followed := slices.Concat(before, after)
	for i := range all {
		if all[i] != followed[i] {
			t.Errorf("event %d followed across the restart = %+v, replayed %+v", i, followed[i], all[i])
		}
	}
	var committed []string
	rows, err := s.pool.Query(ctx, `SELECT sequence FROM orvex.chain_records WHERE round_id = $1 ORDER BY sequence`, round)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		committed = append(committed, strconv.FormatInt(seq, 10))
	}
	if rows.Err() != nil || len(committed) != len(all)-1 {
		t.Fatalf("committed sequences %v, %v; want one per record frame", committed, rows.Err())
	}
	for i, seq := range committed {
		if all[i].id != seq {
			t.Errorf("frame %d has id %s, committed sequence %s", i, all[i].id, seq)
		}
	}
}

//go:build integration

package httpapi_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
	"github.com/juano-morello/orvex/internal/runtime/sessions"
	"github.com/juano-morello/orvex/internal/transport/engineapi"
	"github.com/juano-morello/orvex/internal/transport/httpapi"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var ctx = context.Background()

const (
	serviceToken = "integration-service-token-0123456789abcdef"
	aliceToken   = "alice-token-0001"
	bobToken     = "bob-token-000001"
	carolToken   = "carol-token-0001"
	daveToken    = "dave-token-00001"
)

// stack is the whole path a player uses: an httptest server running the
// gateway API, a gRPC client, engineapi.Serve on a loopback port, and a
// migrated database. The test plays the scheduler and the execution
// driver itself.
type stack struct {
	pool   *pgxpool.Pool
	x      *execution.Executor
	seq    *admission.Sequencer
	url    string
	lossy  *lossyEngine
	faults *faultDB
}

// lossyEngine loses the response of the next SubmitCommand after the
// engine handled it, as a dropped connection would.
type lossyEngine struct {
	orvexv1.EngineServiceClient
	loseNext atomic.Bool
}

func (l *lossyEngine) SubmitCommand(ctx context.Context, req *orvexv1.SubmitCommandRequest, opts ...grpc.CallOption) (*orvexv1.SubmitCommandResponse, error) {
	resp, err := l.EngineServiceClient.SubmitCommand(ctx, req, opts...)
	if err == nil && l.loseNext.CompareAndSwap(true, false) {
		return nil, status.Error(codes.DeadlineExceeded, "response lost")
	}
	return resp, err
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	reg := roundtest.Registry(t)
	x := roundtest.NewExecutor(t, pool, nil, reg)
	faults := &faultDB{db: pool}
	seq, err := admission.New(faults, reg, admission.DefaultConfig(), admission.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- engineapi.Serve(sctx, ln, pool, seq, serviceToken) }()
	engine, err := engineapi.Dial(ln.Addr().String(), serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	players, err := sessions.ParsePlayers([]byte(`{"` + aliceToken + `":"alice","` + bobToken + `":"bob","` + carolToken + `":"carol","` + daveToken + `":"dave"}`))
	if err != nil {
		t.Fatal(err)
	}
	lossy := &lossyEngine{EngineServiceClient: engine}
	api := httpapi.New(lossy, players, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(api)
	t.Cleanup(func() {
		api.CloseStreams()
		ts.Close()
		_ = engine.Close()
		stop()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return &stack{pool: pool, x: x, seq: seq, url: ts.URL, lossy: lossy, faults: faults}
}

func randomUUID(t *testing.T) [16]byte {
	t.Helper()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	id[6], id[8] = id[6]&0x0f|0x40, id[8]&0x3f|0x80
	return id
}

func uuidText(id [16]byte) string { return string(canonjson.UUID(id)) }

// createRound creates a crash/v1 round whose crash deadline offset D lies
// in [minD, maxD] and returns its ID, seed and D in microseconds.
func (s *stack) createRound(t *testing.T, minD, maxD time.Duration) ([16]byte, [32]byte, int64) {
	t.Helper()
	round := randomUUID(t)
	for n := uint64(1); n <= 100_000; n++ {
		var seed [32]byte
		binary.BigEndian.PutUint64(seed[24:], n)
		cfg := v1.Config{RoundID: round, Seed: seed, MinBetCredits: 1, MaxBetCredits: 1_000_000_000, MaxBets: 100}
		st, err := v1.Rules{}.InitialState(cfg)
		if err != nil || st.CrashDeadlineOffsetMicros < minD.Microseconds() || st.CrashDeadlineOffsetMicros > maxD.Microseconds() {
			continue
		}
		if _, err := s.x.CreateRound(ctx, execution.NewRound{
			RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
			Config: cfg, CreatedAtUnixMicro: time.Now().UnixMicro(),
		}); err != nil {
			t.Fatalf("CreateRound: %v", err)
		}
		return round, seed, st.CrashDeadlineOffsetMicros
	}
	t.Fatalf("no seed with D in [%v, %v]", minD, maxD)
	return round, [32]byte{}, 0
}

// system admits a lifecycle command as the scheduler would.
func (s *stack) system(t *testing.T, round [16]byte, cmd v1.Command) (admission.Admission, error) {
	t.Helper()
	return s.seq.Admit(ctx, admission.Request{
		RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		CommandID: randomUUID(t), Command: cmd,
	})
}

func (s *stack) drain(t *testing.T, round [16]byte, want int) {
	t.Helper()
	if got, err := s.x.Drain(ctx, roundtest.Claim(t, s.x, round), 100); err != nil || got != want {
		t.Fatalf("Drain = %d, %v; want %d", got, err, want)
	}
}

func (s *stack) dbNow(t *testing.T) int64 {
	t.Helper()
	var now int64
	if err := s.pool.QueryRow(ctx, `SELECT orvex.unix_micro(clock_timestamp())`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func (s *stack) waitDBClock(t *testing.T, at int64) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); s.dbNow(t) < at; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("database clock did not reach %d", at)
		}
	}
}

type reply struct {
	status int
	body   map[string]any
}

func (s *stack) call(t *testing.T, method, path, token, body string) reply {
	t.Helper()
	req, err := http.NewRequest(method, s.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := reply{status: resp.StatusCode}
	if err := json.NewDecoder(resp.Body).Decode(&r.body); err != nil {
		t.Fatalf("%s %s: decode: %v", method, path, err)
	}
	return r
}

func (s *stack) submit(t *testing.T, token string, round, client [16]byte, command string) reply {
	t.Helper()
	return s.call(t, "POST", "/v1/rounds/"+uuidText(round)+"/commands", token,
		`{"clientCommandId":"`+uuidText(client)+`",`+command+`}`)
}

func placeBet(credits string) string { return `"placeBet":{"betCredits":"` + credits + `"}` }

const cashOut = `"cashOut":{}`

// sse is one Server-Sent Event.
type sse struct{ id, event, data string }

// openStream starts a stream request and returns its events, closed when
// the response ends.
func (s *stack) openStream(t *testing.T, round [16]byte, query, lastEventID string) (int, <-chan sse) {
	t.Helper()
	req, err := http.NewRequest("GET", s.url+"/v1/rounds/"+uuidText(round)+"/stream"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan sse, 64)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		r := bufio.NewReader(resp.Body)
		var ev sse
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
				if ev.event != "" {
					out <- ev
				}
				ev = sse{}
			case strings.HasPrefix(line, "id: "):
				ev.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[6:]
			}
		}
	}()
	return resp.StatusCode, out
}

// collect reads events until the stream ends or a minute passes.
func collect(t *testing.T, events <-chan sse) []sse {
	t.Helper()
	var all []sse
	timeout := time.After(time.Minute)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return all
			}
			all = append(all, ev)
		case <-timeout:
			t.Fatalf("stream did not end; got %d events", len(all))
		}
	}
}

type frame struct {
	Sequence string                       `json:"sequence"`
	Genesis  map[string]string            `json:"genesis"`
	Events   []map[string]json.RawMessage `json:"events"`
}

func parseFrame(t *testing.T, ev sse) frame {
	t.Helper()
	var f frame
	if err := json.Unmarshal([]byte(ev.data), &f); err != nil {
		t.Fatalf("frame %s: %v", ev.id, err)
	}
	if f.Sequence != ev.id {
		t.Fatalf("frame id %s holds sequence %s", ev.id, f.Sequence)
	}
	return f
}

func eventType(e map[string]json.RawMessage) string {
	var s string
	_ = json.Unmarshal(e["type"], &s)
	return s
}

// TestPlayableRoundEndToEnd plays one round through the gateway: bets,
// a duplicate submission, a cash-out whose response is lost after it
// committed, lookups and the live stream from genesis to its end, then
// resumption and resubmission after settlement.
func TestPlayableRoundEndToEnd(t *testing.T) {
	s := newStack(t)
	round, seed, d := s.createRound(t, time.Second, 3*time.Second)
	roundtest.FundPlayer(t, s.pool, "alice", 1000)
	roundtest.FundPlayer(t, s.pool, "bob", 1000)

	if r := s.call(t, "GET", "/v1/rounds/current?game=crash", "", ""); r.status != 200 || r.body["roundId"] != uuidText(round) {
		t.Fatalf("current round = %d %v, want %s", r.status, r.body, uuidText(round))
	}
	code, live := s.openStream(t, round, "", "")
	if code != 200 {
		t.Fatalf("stream = %d", code)
	}

	shared, cash, again := randomUUID(t), randomUUID(t), randomUUID(t)
	bet := s.submit(t, aliceToken, round, shared, placeBet("100"))
	if bet.status != 202 || bet.body["sequence"] != "1" || bet.body["duplicate"] != false || bet.body["clientCommandId"] != uuidText(shared) {
		t.Fatalf("alice's bet = %d %v", bet.status, bet.body)
	}
	if r := s.submit(t, bobToken, round, shared, placeBet("200")); r.status != 202 || r.body["sequence"] != "2" {
		t.Fatalf("bob's bet under the same client id = %d %v, want its own admission", r.status, r.body)
	}
	dup := s.submit(t, aliceToken, round, shared, placeBet("100"))
	if dup.status != 202 || dup.body["duplicate"] != true || dup.body["sequence"] != "1" ||
		dup.body["effectiveTimeUnixMicro"] != bet.body["effectiveTimeUnixMicro"] {
		t.Fatalf("resubmitted bet = %d %v, want the original admission", dup.status, dup.body)
	}
	if r := s.submit(t, aliceToken, round, shared, cashOut); r.status != 409 || r.body["code"] != "command_id_reused" {
		t.Fatalf("client id reused for another command = %d %v", r.status, r.body)
	}
	if r := s.submit(t, carolToken, round, randomUUID(t), placeBet("5")); r.status != 409 || r.body["code"] != "insufficient_credits" {
		t.Fatalf("unfunded bet = %d %v", r.status, r.body)
	}
	s.drain(t, round, 2)

	start, err := s.system(t, round, v1.StartRound{Authority: v1.AuthoritySystem})
	if err != nil {
		t.Fatal(err)
	}
	s.drain(t, round, 1)
	s.waitDBClock(t, start.EffectiveTimeUnixMicro+1)
	// Duplicate and malformed notifications are only wakeups: the stream
	// stays gapless and duplicate-free.
	for _, p := range []string{uuidText(round) + ":1", uuidText(round) + ":1", "garbage"} {
		if _, err := s.pool.Exec(ctx, `SELECT pg_notify('orvex_round_records', $1)`, p); err != nil {
			t.Fatal(err)
		}
	}
	s.lossy.loseNext.Store(true)
	if r := s.submit(t, aliceToken, round, cash, cashOut); r.status != 503 || r.body["code"] != "outcome_unknown" {
		t.Fatalf("cash-out with its response lost = %d %v, want 503 outcome_unknown", r.status, r.body)
	}
	cashed := s.submit(t, aliceToken, round, cash, cashOut)
	if cashed.status != 202 || cashed.body["sequence"] != "4" || cashed.body["duplicate"] != true {
		t.Fatalf("cash-out resubmitted after the lost response = %d %v, want the committed admission 4", cashed.status, cashed.body)
	}
	if r := s.submit(t, aliceToken, round, again, cashOut); r.status != 202 || r.body["sequence"] != "5" {
		t.Fatalf("second cash-out = %d %v", r.status, r.body)
	}
	s.drain(t, round, 2)

	look := s.call(t, "GET", "/v1/commands/"+uuidText(cash), aliceToken, "")
	events, _ := look.body["events"].([]any)
	if look.status != 200 || look.body["status"] != "accepted" || len(events) != 1 {
		t.Fatalf("lookup of the cash-out = %d %v", look.status, look.body)
	}
	payout, _ := events[0].(map[string]any)["payoutCredits"].(string)
	if events[0].(map[string]any)["type"] != "BetCashedOut" || payout == "" || payout == "0" {
		t.Fatalf("cash-out events = %v, want BetCashedOut with a payout", events)
	}
	if r := s.call(t, "GET", "/v1/commands/"+uuidText(again), aliceToken, ""); r.status != 200 || r.body["status"] != "rejected" || r.body["reason"] == "" {
		t.Errorf("lookup of the second cash-out = %d %v, want rejected with a reason", r.status, r.body)
	}
	if r := s.call(t, "GET", "/v1/commands/"+uuidText(shared), bobToken, ""); r.status != 200 || r.body["sequence"] != "2" {
		t.Errorf("bob's lookup of the shared client id = %d %v, want his own bet", r.status, r.body)
	}
	if r := s.call(t, "GET", "/v1/commands/"+uuidText(cash), bobToken, ""); r.status != 404 {
		t.Errorf("bob's lookup of alice's client id = %d %v, want 404", r.status, r.body)
	}

	if _, err := s.system(t, round, v1.CloseRound{Authority: v1.AuthoritySystem}); err != nil {
		if !errors.Is(err, admission.ErrClosureNotDue) {
			t.Fatal(err)
		}
		s.waitDBClock(t, start.EffectiveTimeUnixMicro+d)
		if _, err := s.system(t, round, v1.CloseRound{Authority: v1.AuthoritySystem}); err != nil {
			t.Fatal(err)
		}
	}
	s.drain(t, round, 1)
	if _, err := s.system(t, round, v1.SettleRound{Authority: v1.AuthoritySystem}); err != nil {
		t.Fatal(err)
	}
	s.drain(t, round, 1)

	all := collect(t, live)
	if len(all) != 9 || all[8].event != "end" {
		t.Fatalf("stream events %v, want records 0..7 then end", all)
	}
	seedHex := hex.EncodeToString(seed[:])
	commitment := v1.Commitment(seed)
	for i, ev := range all[:8] {
		f := parseFrame(t, ev)
		if ev.event != "record" || ev.id != string(rune('0'+i)) {
			t.Fatalf("event %d = %+v, want record %d", i, ev, i)
		}
		if i < 7 && strings.Contains(ev.data, seedHex) {
			t.Errorf("frame %d reveals the seed before settlement", i)
		}
		switch i {
		case 0:
			if f.Genesis["commitment"] != hex.EncodeToString(commitment[:]) || f.Genesis["maxBets"] != "100" {
				t.Errorf("genesis = %v", f.Genesis)
			}
		case 4:
			if len(f.Events) != 1 || eventType(f.Events[0]) != "BetCashedOut" || !strings.Contains(ev.data, `"payoutCredits":"`+payout+`"`) {
				t.Errorf("frame 4 = %s, want alice's cash-out", ev.data)
			}
		case 5:
			if f.Events == nil || len(f.Events) != 0 {
				t.Errorf("frame 5 = %s, want a rejected command with no events", ev.data)
			}
		case 7:
			if last := f.Events[len(f.Events)-1]; eventType(last) != "RoundSettled" || !strings.Contains(ev.data, seedHex) {
				t.Errorf("frame 7 = %s, want RoundSettled with the seed", ev.data)
			}
		}
	}

	again2 := s.submit(t, aliceToken, round, cash, cashOut)
	if again2.status != 202 || again2.body["duplicate"] != true || again2.body["sequence"] != "4" ||
		again2.body["effectiveTimeUnixMicro"] != cashed.body["effectiveTimeUnixMicro"] {
		t.Errorf("cash-out resubmitted after settlement = %d %v, want the original admission", again2.status, again2.body)
	}
	if r := s.submit(t, bobToken, round, randomUUID(t), placeBet("1")); r.status != 409 || r.body["code"] != "round_sealed" {
		t.Errorf("bet after settlement = %d %v, want round_sealed", r.status, r.body)
	}

	_, resumed := s.openStream(t, round, "?after=1", "4")
	if got := collect(t, resumed); len(got) != 4 || got[0].id != "5" || got[2].id != "7" || got[3].event != "end" {
		t.Errorf("resumed from Last-Event-ID 4: %v, want 5..7 then end", got)
	}
	if code, done := s.openStream(t, round, "?after=7", ""); code != 204 {
		t.Errorf("stream after the last record = %d, want 204", code)
	} else if got := collect(t, done); len(got) != 0 {
		t.Errorf("stream after the last record: %v, want no events", got)
	}
	if r := s.call(t, "GET", "/v1/rounds/"+uuidText(round)+"/stream?after=8", "", ""); r.status != 400 || r.body["code"] != "invalid_cursor" {
		t.Errorf("cursor above the head = %d %v", r.status, r.body)
	}
	if r := s.call(t, "GET", "/v1/rounds/"+uuidText(randomUUID(t))+"/stream", "", ""); r.status != 404 || r.body["code"] != "round_not_found" {
		t.Errorf("unknown round = %d %v", r.status, r.body)
	}

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var pending int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM orvex.outbox_messages WHERE published_at IS NULL`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d outbox messages still unpublished", pending)
		}
	}
}

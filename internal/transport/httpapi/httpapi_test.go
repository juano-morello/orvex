package httpapi

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/juano-morello/orvex/internal/runtime/sessions"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

const (
	aliceToken = "alice-token-0001"
	roundText  = "0b7e6a1c-3c2f-4e0a-9a51-6d2f0c1e4b77"
	clientText = "5d1f0c2e-7a3b-4c4d-9e8f-0a1b2c3d4ea1"
)

var (
	roundBytes  = []byte{0x0b, 0x7e, 0x6a, 0x1c, 0x3c, 0x2f, 0x4e, 0x0a, 0x9a, 0x51, 0x6d, 0x2f, 0x0c, 0x1e, 0x4b, 0x77}
	clientBytes = []byte{0x5d, 0x1f, 0x0c, 0x2e, 0x7a, 0x3b, 0x4c, 0x4d, 0x9e, 0x8f, 0x0a, 0x1b, 0x2c, 0x3d, 0x4e, 0xa1}
)

// fakeEngine answers with the functions a test sets and records requests.
type fakeEngine struct {
	orvexv1.UnimplementedEngineServiceServer
	mu        sync.Mutex
	submits   []*orvexv1.SubmitCommandRequest
	lookups   []*orvexv1.LookupCommandRequest
	subs      []*orvexv1.SubscribeRoundRequest
	submit    func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error)
	lookup    func(*orvexv1.LookupCommandRequest) (*orvexv1.LookupCommandResponse, error)
	current   func() (*orvexv1.GetCurrentRoundResponse, error)
	subscribe func(*orvexv1.SubscribeRoundRequest, grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse]) error
}

func (f *fakeEngine) SubmitCommand(_ context.Context, r *orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
	f.mu.Lock()
	f.submits = append(f.submits, r)
	f.mu.Unlock()
	return f.submit(r)
}

func (f *fakeEngine) LookupCommand(_ context.Context, r *orvexv1.LookupCommandRequest) (*orvexv1.LookupCommandResponse, error) {
	f.mu.Lock()
	f.lookups = append(f.lookups, r)
	f.mu.Unlock()
	return f.lookup(r)
}

func (f *fakeEngine) GetCurrentRound(context.Context, *orvexv1.GetCurrentRoundRequest) (*orvexv1.GetCurrentRoundResponse, error) {
	return f.current()
}

func (f *fakeEngine) SubscribeRound(r *orvexv1.SubscribeRoundRequest, s grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse]) error {
	f.mu.Lock()
	f.subs = append(f.subs, r)
	f.mu.Unlock()
	return f.subscribe(r, s)
}

func newGateway(t *testing.T, engine *fakeEngine) (*httptest.Server, *Handler) {
	t.Helper()
	ln := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	orvexv1.RegisterEngineServiceServer(srv, engine)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	players, err := sessions.ParsePlayers([]byte(`{"` + aliceToken + `": "alice"}`))
	if err != nil {
		t.Fatal(err)
	}
	h := New(orvexv1.NewEngineServiceClient(conn), players, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, h
}

type response struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, ts *httptest.Server, method, path, token, body string, header ...string) response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{resp.StatusCode, resp.Header, strings.TrimSuffix(string(b), "\n")}
}

func admitted(seq uint64, dup bool) func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
	return func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
		return &orvexv1.SubmitCommandResponse{Result: &orvexv1.SubmitCommandResponse_Admitted{Admitted: &orvexv1.Admission{
			RoundId: roundBytes, Sequence: seq, EffectiveTimeUnixMicro: 1_763_800_006_210_000, Duplicate: dup,
		}}}, nil
	}
}

const cashOutBody = `{"clientCommandId":"` + clientText + `","cashOut":{}}`

func TestSubmitAdmitted(t *testing.T) {
	for _, dup := range []bool{false, true} {
		engine := &fakeEngine{submit: admitted(5, dup)}
		ts, _ := newGateway(t, engine)
		got := do(t, ts, "POST", "/v1/rounds/"+roundText+"/commands", aliceToken,
			`{"clientCommandId":"`+clientText+`","placeBet":{"betCredits":"100"}}`)
		want := `{"clientCommandId":"` + clientText + `","roundId":"` + roundText +
			`","sequence":"5","effectiveTimeUnixMicro":"1763800006210000","duplicate":` + strconv.FormatBool(dup) + `,"status":"admitted"}`
		if got.status != http.StatusAccepted || got.body != want {
			t.Fatalf("POST = %d %s, want 202 %s", got.status, got.body, want)
		}
		req := engine.submits[0]
		if req.GetPrincipal().GetPlayerId() != "alice" || req.GetPlaceBet().GetBetCredits() != 100 ||
			string(req.GetRoundId()) != string(roundBytes) || string(req.GetClientCommandId()) != string(clientBytes) {
			t.Errorf("engine got %v, want alice's 100-credit bet", req)
		}
	}
}

func TestSubmitRequiresAToken(t *testing.T) {
	engine := &fakeEngine{submit: admitted(1, false)}
	ts, _ := newGateway(t, engine)
	for name, token := range map[string]string{"none": "", "unknown": "mallory-token-001"} {
		got := do(t, ts, "POST", "/v1/rounds/"+roundText+"/commands", token, cashOutBody)
		if got.status != http.StatusUnauthorized || got.body != `{"code":"unauthenticated"}` || got.header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: POST = %d %s, want 401 with WWW-Authenticate", name, got.status, got.body)
		}
	}
	got := do(t, ts, "GET", "/v1/commands/"+clientText, "", "")
	if got.status != http.StatusUnauthorized {
		t.Errorf("lookup without a token: %d, want 401", got.status)
	}
	if len(engine.submits) != 0 {
		t.Error("an unauthenticated submit reached the engine")
	}
}

func TestSubmitRefusesMalformedBodies(t *testing.T) {
	engine := &fakeEngine{submit: admitted(1, false)}
	ts, _ := newGateway(t, engine)
	id := `"clientCommandId":"` + clientText + `"`
	for name, body := range map[string]string{
		"names a player":         `{` + id + `,"player":"bob","cashOut":{}}`,
		"names an authority":     `{` + id + `,"cashOut":{},"authority":"system"}`,
		"lifecycle command":      `{` + id + `,"startRound":{}}`,
		"both commands":          `{` + id + `,"cashOut":{},"placeBet":{"betCredits":"1"}}`,
		"no command":             `{` + id + `}`,
		"numeric bet":            `{` + id + `,"placeBet":{"betCredits":100}}`,
		"zero bet":               `{` + id + `,"placeBet":{"betCredits":"0"}}`,
		"negative bet":           `{` + id + `,"placeBet":{"betCredits":"-1"}}`,
		"leading zero bet":       `{` + id + `,"placeBet":{"betCredits":"0100"}}`,
		"bet above int64":        `{` + id + `,"placeBet":{"betCredits":"9223372036854775808"}}`,
		"uppercase uuid":         `{"clientCommandId":"` + strings.ToUpper(clientText) + `","cashOut":{}}`,
		"nil uuid":               `{"clientCommandId":"00000000-0000-0000-0000-000000000000","cashOut":{}}`,
		"trailing data":          cashOutBody + `{}`,
		"trailing brace":         cashOutBody + `}`,
		"not json":               `cashOut`,
		"array":                  `[` + cashOutBody + `]`,
		"repeated id":            `{` + id + `,` + id + `,"cashOut":{}}`,
		"repeated id differs":    `{"clientCommandId":"00000000-0000-0000-0000-000000000000",` + id + `,"cashOut":{}}`,
		"repeated cash-out":      `{` + id + `,"cashOut":{},"cashOut":{}}`,
		"repeated bet":           `{` + id + `,"placeBet":{"betCredits":"1"},"placeBet":{"betCredits":"2"}}`,
		"repeated credits":       `{` + id + `,"placeBet":{"betCredits":"1","betCredits":"2"}}`,
		"mixed-case id key":      `{"ClientCommandId":"` + clientText + `","cashOut":{}}`,
		"lowercase id key":       `{"clientcommandid":"` + clientText + `","cashOut":{}}`,
		"mixed-case command":     `{` + id + `,"CashOut":{}}`,
		"mixed-case credits":     `{` + id + `,"placeBet":{"BetCredits":"1"}}`,
		"unknown cash-out field": `{` + id + `,"cashOut":{"player":"bob"}}`,
		"unknown bet field":      `{` + id + `,"placeBet":{"betCredits":"1","player":"bob"}}`,
		"bet without credits":    `{` + id + `,"placeBet":{}}`,
		"null cash-out":          `{` + id + `,"placeBet":{"betCredits":"1"},"cashOut":null}`,
		"null bet":               `{` + id + `,"placeBet":null,"cashOut":{}}`,
		"cash-out not object":    `{` + id + `,"cashOut":true}`,
		"cash-out array":         `{` + id + `,"cashOut":[]}`,
		"numeric id":             `{"clientCommandId":7,"cashOut":{}}`,
		"null id":                `{"clientCommandId":null,"cashOut":{}}`,
		"null credits":           `{` + id + `,"placeBet":{"betCredits":null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := do(t, ts, "POST", "/v1/rounds/"+roundText+"/commands", aliceToken, body)
			if got.status != http.StatusBadRequest || got.body != `{"code":"invalid_request"}` {
				t.Errorf("POST = %d %s, want 400 invalid_request", got.status, got.body)
			}
		})
	}
	if got := do(t, ts, "POST", "/v1/rounds/not-a-uuid/commands", aliceToken, cashOutBody); got.status != http.StatusBadRequest {
		t.Errorf("bad round id: %d, want 400", got.status)
	}
	if len(engine.submits) != 0 {
		t.Errorf("%d malformed submits reached the engine", len(engine.submits))
	}
}

func TestSubmitRefusesOversizedBodies(t *testing.T) {
	engine := &fakeEngine{submit: admitted(1, false)}
	ts, _ := newGateway(t, engine)
	path := "/v1/rounds/" + roundText + "/commands"
	padded := `{"clientCommandId":"` + clientText + `","cashOut":{}` + strings.Repeat(" ", maxBodyBytes) + `}`
	if got := do(t, ts, "POST", path, aliceToken, padded); got.status != http.StatusRequestEntityTooLarge || got.body != `{"code":"request_too_large"}` {
		t.Errorf("oversized POST = %d %s, want 413 request_too_large", got.status, got.body)
	}
	if got := do(t, ts, "POST", path, "", padded); got.status != http.StatusUnauthorized {
		t.Errorf("oversized POST without a token = %d %s, want 401", got.status, got.body)
	}
	if len(engine.submits) != 0 {
		t.Errorf("%d oversized submits reached the engine", len(engine.submits))
	}
	fits := `{"clientCommandId":"` + clientText + `","cashOut":{}}`
	fits += strings.Repeat(" ", maxBodyBytes-len(fits))
	if got := do(t, ts, "POST", path, aliceToken, fits); got.status != http.StatusAccepted {
		t.Errorf("POST of exactly %d bytes = %d %s, want 202", maxBodyBytes, got.status, got.body)
	}
}

func TestRoutes(t *testing.T) {
	engine := &fakeEngine{current: func() (*orvexv1.GetCurrentRoundResponse, error) {
		return &orvexv1.GetCurrentRoundResponse{RoundId: roundBytes, Game: "crash", RulesVersion: 1}, nil
	}}
	ts, _ := newGateway(t, engine)
	for _, tc := range []struct{ path, allow string }{
		{"/v1/rounds/current", "GET, HEAD"},
		{"/v1/rounds/" + roundText + "/commands", "POST"},
		{"/v1/commands/" + clientText, "GET, HEAD"},
		{"/v1/rounds/" + roundText + "/stream", "GET, HEAD"},
	} {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			if strings.Contains(tc.allow, method) {
				continue
			}
			got := do(t, ts, method, tc.path, aliceToken, cashOutBody)
			if got.status != http.StatusMethodNotAllowed || got.header.Get("Allow") != tc.allow || got.body != `{"code":"method_not_allowed"}` {
				t.Errorf("%s %s = %d Allow %q %s, want 405 Allow %q method_not_allowed", method, tc.path, got.status, got.header.Get("Allow"), got.body, tc.allow)
			}
		}
	}
	for _, path := range []string{"/", "/v1", "/v1/rounds", "/v1/rounds/current/", "/v1/rounds/" + roundText, "/v1/rounds/" + roundText + "/commands/x", "/v1/commands", "/v2/rounds/current"} {
		for _, method := range []string{"GET", "POST"} {
			if got := do(t, ts, method, path, aliceToken, cashOutBody); got.status != http.StatusNotFound || got.body != `{"code":"not_found"}` {
				t.Errorf("%s %s = %d %s, want 404 not_found", method, path, got.status, got.body)
			}
		}
	}
	if got := do(t, ts, "HEAD", "/v1/rounds/current", "", ""); got.status != http.StatusOK {
		t.Errorf("HEAD /v1/rounds/current = %d, want 200", got.status)
	}
	if len(engine.submits)+len(engine.lookups)+len(engine.subs) != 0 {
		t.Errorf("a refused route reached the engine: %d submits, %d lookups, %d subscriptions", len(engine.submits), len(engine.lookups), len(engine.subs))
	}
}

func TestSubmitMapsEngineResults(t *testing.T) {
	rejected := func(r orvexv1.RejectionReason) func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
		return func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
			return &orvexv1.SubmitCommandResponse{Result: &orvexv1.SubmitCommandResponse_Rejected{Rejected: &orvexv1.Rejection{Reason: r}}}, nil
		}
	}
	retry := func(r orvexv1.RetryReason) func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
		return func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
			return &orvexv1.SubmitCommandResponse{Result: &orvexv1.SubmitCommandResponse_Retry{Retry: &orvexv1.Retry{Reason: r}}}, nil
		}
	}
	failing := func(c codes.Code) func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
		return func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
			return nil, status.Error(c, "engine says no")
		}
	}
	for name, tc := range map[string]struct {
		submit func(*orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error)
		status int
		code   string
	}{
		"round not found":      {rejected(orvexv1.RejectionReason_REJECTION_REASON_ROUND_NOT_FOUND), 404, "round_not_found"},
		"round closed":         {rejected(orvexv1.RejectionReason_REJECTION_REASON_ROUND_CLOSED), 409, "round_closed"},
		"round sealed":         {rejected(orvexv1.RejectionReason_REJECTION_REASON_ROUND_SEALED), 409, "round_sealed"},
		"insufficient credits": {rejected(orvexv1.RejectionReason_REJECTION_REASON_INSUFFICIENT_CREDITS), 409, "insufficient_credits"},
		"command id reused":    {rejected(orvexv1.RejectionReason_REJECTION_REASON_COMMAND_ID_REUSED), 409, "command_id_reused"},
		"unknown rejection":    {rejected(orvexv1.RejectionReason_REJECTION_REASON_UNSPECIFIED), 500, "internal"},
		"not admitted":         {retry(orvexv1.RetryReason_RETRY_REASON_NOT_ADMITTED), 503, "not_admitted"},
		"outcome unknown":      {retry(orvexv1.RetryReason_RETRY_REASON_OUTCOME_UNKNOWN), 503, "outcome_unknown"},
		"lost response":        {failing(codes.DeadlineExceeded), 503, "outcome_unknown"},
		"engine unreachable":   {failing(codes.Unavailable), 503, "outcome_unknown"},
		"invalid argument":     {failing(codes.InvalidArgument), 400, "invalid_request"},
		"engine bug":           {failing(codes.Internal), 500, "internal"},
		"token refused":        {failing(codes.Unauthenticated), 500, "internal"},
	} {
		t.Run(name, func(t *testing.T) {
			ts, _ := newGateway(t, &fakeEngine{submit: tc.submit})
			got := do(t, ts, "POST", "/v1/rounds/"+roundText+"/commands", aliceToken, cashOutBody)
			if got.status != tc.status || got.body != `{"code":"`+tc.code+`"}` {
				t.Errorf("POST = %d %s, want %d %s", got.status, got.body, tc.status, tc.code)
			}
			if tc.status == 503 && got.header.Get("Retry-After") == "" {
				t.Error("503 without Retry-After")
			}
		})
	}
}

func TestLookup(t *testing.T) {
	engine := &fakeEngine{lookup: func(r *orvexv1.LookupCommandRequest) (*orvexv1.LookupCommandResponse, error) {
		if r.GetPrincipal().GetPlayerId() != "alice" {
			return nil, status.Error(codes.NotFound, "")
		}
		return &orvexv1.LookupCommandResponse{
			Admission: &orvexv1.Admission{RoundId: roundBytes, Sequence: 5, EffectiveTimeUnixMicro: 1_763_800_006_210_000},
			Outcome:   orvexv1.Outcome_OUTCOME_ACCEPTED,
			Events: []*orvexv1.CrashEvent{{Event: &orvexv1.CrashEvent_BetCashedOut{BetCashedOut: &orvexv1.BetCashedOut{
				PlayerId: "alice", CashedOutAtUnixMicro: 1_763_800_006_210_000, CashOutTick: 12, CashOutMultiplierScaled: 25000, PayoutCredits: 250,
			}}}},
		}, nil
	}}
	ts, _ := newGateway(t, engine)
	got := do(t, ts, "GET", "/v1/commands/"+clientText, aliceToken, "")
	want := `{"clientCommandId":"` + clientText + `","roundId":"` + roundText + `","sequence":"5","effectiveTimeUnixMicro":"1763800006210000",` +
		`"status":"accepted","events":[{"type":"BetCashedOut","playerId":"alice","cashedOutAtUnixMicro":"1763800006210000",` +
		`"cashOutTick":"12","cashOutMultiplierScaled":"25000","payoutCredits":"250"}]}`
	if got.status != http.StatusOK || got.body != want {
		t.Errorf("GET = %d %s\nwant 200 %s", got.status, got.body, want)
	}

	engine.lookup = func(*orvexv1.LookupCommandRequest) (*orvexv1.LookupCommandResponse, error) {
		return &orvexv1.LookupCommandResponse{
			Admission: &orvexv1.Admission{RoundId: roundBytes, Sequence: 3, EffectiveTimeUnixMicro: 1},
			Outcome:   orvexv1.Outcome_OUTCOME_REJECTED, Reason: "no_open_bet",
		}, nil
	}
	got = do(t, ts, "GET", "/v1/commands/"+clientText, aliceToken, "")
	if !strings.Contains(got.body, `"status":"rejected","reason":"no_open_bet","events":[]`) {
		t.Errorf("rejected lookup = %s", got.body)
	}
	engine.lookup = func(*orvexv1.LookupCommandRequest) (*orvexv1.LookupCommandResponse, error) {
		return nil, status.Error(codes.NotFound, "")
	}
	if got := do(t, ts, "GET", "/v1/commands/"+clientText, aliceToken, ""); got.status != 404 || got.body != `{"code":"command_not_found"}` {
		t.Errorf("unknown lookup = %d %s", got.status, got.body)
	}
}

func TestCurrentRound(t *testing.T) {
	engine := &fakeEngine{current: func() (*orvexv1.GetCurrentRoundResponse, error) {
		return &orvexv1.GetCurrentRoundResponse{RoundId: roundBytes, Game: "crash", RulesVersion: 1}, nil
	}}
	ts, _ := newGateway(t, engine)
	for _, path := range []string{"/v1/rounds/current", "/v1/rounds/current?game=crash"} {
		got := do(t, ts, "GET", path, "", "")
		if want := `{"roundId":"` + roundText + `","game":"crash","rulesVersion":"1"}`; got.status != 200 || got.body != want {
			t.Errorf("GET %s = %d %s, want 200 %s", path, got.status, got.body, want)
		}
	}
	if got := do(t, ts, "GET", "/v1/rounds/current?game=dice", "", ""); got.status != 404 || got.body != `{"code":"unknown_game"}` {
		t.Errorf("unknown game = %d %s", got.status, got.body)
	}
	engine.current = func() (*orvexv1.GetCurrentRoundResponse, error) { return nil, status.Error(codes.NotFound, "") }
	if got := do(t, ts, "GET", "/v1/rounds/current", "", ""); got.status != 404 || got.body != `{"code":"no_current_round"}` {
		t.Errorf("no round = %d %s", got.status, got.body)
	}
	engine.current = func() (*orvexv1.GetCurrentRoundResponse, error) { return nil, status.Error(codes.Unavailable, "") }
	if got := do(t, ts, "GET", "/v1/rounds/current", "", ""); got.status != 503 || got.body != `{"code":"unavailable"}` {
		t.Errorf("engine down = %d %s", got.status, got.body)
	}
}

func frames() []*orvexv1.SubscribeRoundResponse {
	commitment := make([]byte, 32)
	commitment[0] = 0x9f
	return []*orvexv1.SubscribeRoundResponse{
		{Sequence: 0, EffectiveTimeUnixMicro: 10, Body: &orvexv1.SubscribeRoundResponse_Genesis{Genesis: &orvexv1.Genesis{
			Commitment: commitment, MinBetCredits: 1, MaxBetCredits: 1_000_000_000, MaxBets: 1000,
		}}},
		{Sequence: 1, EffectiveTimeUnixMicro: 11, Body: &orvexv1.SubscribeRoundResponse_Events{Events: &orvexv1.Events{Events: []*orvexv1.CrashEvent{
			{Event: &orvexv1.CrashEvent_BetPlaced{BetPlaced: &orvexv1.BetPlaced{PlayerId: "alice", BetCredits: 100, PlacedAtUnixMicro: 11}}},
		}}}},
		{Sequence: 2, EffectiveTimeUnixMicro: 12, Body: &orvexv1.SubscribeRoundResponse_Events{Events: &orvexv1.Events{}}},
	}
}

func sendAll(fs []*orvexv1.SubscribeRoundResponse) func(*orvexv1.SubscribeRoundRequest, grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse]) error {
	return func(_ *orvexv1.SubscribeRoundRequest, s grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse]) error {
		if err := s.SendHeader(nil); err != nil {
			return err
		}
		for _, f := range fs {
			if err := s.Send(f); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestStreamRelaysRecordsThenEnd(t *testing.T) {
	ts, _ := newGateway(t, &fakeEngine{subscribe: sendAll(frames())})
	got := do(t, ts, "GET", "/v1/rounds/"+roundText+"/stream", "", "")
	want := "retry: 1000\n\n" +
		"id: 0\nevent: record\ndata: {\"sequence\":\"0\",\"effectiveTimeUnixMicro\":\"10\",\"genesis\":{\"commitment\":\"9f" + strings.Repeat("00", 31) +
		"\",\"minBetCredits\":\"1\",\"maxBetCredits\":\"1000000000\",\"maxBets\":\"1000\"}}\n\n" +
		"id: 1\nevent: record\ndata: {\"sequence\":\"1\",\"effectiveTimeUnixMicro\":\"11\",\"events\":[{\"type\":\"BetPlaced\",\"playerId\":\"alice\",\"betCredits\":\"100\",\"placedAtUnixMicro\":\"11\"}]}\n\n" +
		"id: 2\nevent: record\ndata: {\"sequence\":\"2\",\"effectiveTimeUnixMicro\":\"12\",\"events\":[]}\n\n" +
		"event: end\ndata: {}\n"
	if got.status != 200 || got.body != want || got.header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("stream = %d %q\nwant 200 %q", got.status, got.body, want)
	}
}

func TestStreamCursor(t *testing.T) {
	engine := &fakeEngine{subscribe: sendAll(nil)}
	ts, _ := newGateway(t, engine)
	path := "/v1/rounds/" + roundText + "/stream"
	do(t, ts, "GET", path+"?after=2", "", "", "Last-Event-ID", "4")
	do(t, ts, "GET", path+"?after=2", "", "")
	do(t, ts, "GET", path, "", "")
	if len(engine.subs) != 3 || engine.subs[0].GetAfterSequence() != 4 || engine.subs[1].GetAfterSequence() != 2 || engine.subs[2].AfterSequence != nil {
		t.Errorf("engine cursors %v, want 4 (Last-Event-ID wins), 2, none", engine.subs)
	}
	for _, bad := range []string{"-1", "01", "x", "18446744073709551616"} {
		if got := do(t, ts, "GET", path+"?after="+bad, "", ""); got.status != 400 || got.body != `{"code":"invalid_cursor"}` {
			t.Errorf("after=%s: %d %s, want 400 invalid_cursor", bad, got.status, got.body)
		}
	}
}

func TestStreamRefusalsBeforeAnyRecord(t *testing.T) {
	for name, tc := range map[string]struct {
		code   codes.Code
		status int
		body   string
	}{
		"unknown round":   {codes.NotFound, 404, `{"code":"round_not_found"}`},
		"cursor too high": {codes.FailedPrecondition, 400, `{"code":"invalid_cursor"}`},
		"bad request":     {codes.InvalidArgument, 400, `{"code":"invalid_request"}`},
		"engine fault":    {codes.Internal, 500, `{"code":"internal"}`},
	} {
		t.Run(name, func(t *testing.T) {
			ts, _ := newGateway(t, &fakeEngine{subscribe: func(*orvexv1.SubscribeRoundRequest, grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse]) error {
				return status.Error(tc.code, "no")
			}})
			if got := do(t, ts, "GET", "/v1/rounds/"+roundText+"/stream", "", ""); got.status != tc.status || got.body != tc.body {
				t.Errorf("stream = %d %s, want %d %s", got.status, got.body, tc.status, tc.body)
			}
		})
	}
}

func TestStreamFailureMidwaySendsNoEnd(t *testing.T) {
	ts, _ := newGateway(t, &fakeEngine{subscribe: func(r *orvexv1.SubscribeRoundRequest, s grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse]) error {
		if err := sendAll(frames()[:1])(r, s); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, "engine shutting down")
	}})
	got := do(t, ts, "GET", "/v1/rounds/"+roundText+"/stream", "", "")
	if got.status != 200 || !strings.Contains(got.body, "id: 0\n") || strings.Contains(got.body, "event: end") {
		t.Errorf("stream = %d %q, want frame 0 and no end", got.status, got.body)
	}
}

func TestStreamPingsAndCloseStreams(t *testing.T) {
	started := make(chan struct{})
	ts, h := newGateway(t, &fakeEngine{subscribe: func(_ *orvexv1.SubscribeRoundRequest, s grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse]) error {
		if err := s.SendHeader(nil); err != nil {
			return err
		}
		close(started)
		<-s.Context().Done()
		return s.Context().Err()
	}})
	h.pingInterval = 10 * time.Millisecond
	resp, err := ts.Client().Get(ts.URL + "/v1/rounds/" + roundText + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	<-started
	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read before a ping: %v", err)
		}
		if line == ": ping\n" {
			break
		}
	}
	h.CloseStreams()
	if rest, err := io.ReadAll(r); err != nil || strings.Contains(string(rest), "event: end") {
		t.Errorf("after CloseStreams: %q, %v; want the stream closed without end", rest, err)
	}
}

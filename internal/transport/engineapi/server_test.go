package engineapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/relay"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

type fakeAdmitter struct {
	requests  []admission.Request
	admission admission.Admission
	err       error
	lookups   [][16]byte
	status    admission.Status
	lookupErr error
}

func (f *fakeAdmitter) Admit(_ context.Context, req admission.Request) (admission.Admission, error) {
	f.requests = append(f.requests, req)
	return f.admission, f.err
}

func (f *fakeAdmitter) Lookup(_ context.Context, id [16]byte) (admission.Status, error) {
	f.lookups = append(f.lookups, id)
	return f.status, f.lookupErr
}

func newTestServer(a admitter) *server {
	return &server{admit: a, wakeups: relay.NewWakeups(), done: make(chan struct{}), log: slog.New(slog.DiscardHandler)}
}

var clientID = [16]byte{0x5d, 0x1f, 15: 0xa1}

func submitRequest(command any) *orvexv1.SubmitCommandRequest {
	req := &orvexv1.SubmitCommandRequest{
		Principal:       &orvexv1.Principal{PlayerId: "p_ana"},
		ClientCommandId: clientID[:],
		RoundId:         testRound[:],
	}
	switch c := command.(type) {
	case *orvexv1.PlaceBet:
		req.Command = &orvexv1.SubmitCommandRequest_PlaceBet{PlaceBet: c}
	case *orvexv1.CashOut:
		req.Command = &orvexv1.SubmitCommandRequest_CashOut{CashOut: c}
	}
	return req
}

func TestSubmitBuildsThePrincipalsCommand(t *testing.T) {
	for name, tc := range map[string]struct {
		command any
		want    v1.Command
	}{
		"place bet": {&orvexv1.PlaceBet{BetCredits: 100}, v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "p_ana", BetCredits: 100}},
		"cash out":  {&orvexv1.CashOut{}, v1.CashOut{Authority: v1.AuthorityPlayer, Player: "p_ana"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAdmitter{admission: admission.Admission{RoundID: testRound, Sequence: 1, EffectiveTimeUnixMicro: 7}}
			if _, err := newTestServer(fake).SubmitCommand(context.Background(), submitRequest(tc.command)); err != nil {
				t.Fatal(err)
			}
			want := admission.Request{
				RoundID: testRound, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
				CommandID: commandID("p_ana", clientID), Command: tc.want,
			}
			if len(fake.requests) != 1 || fake.requests[0] != want {
				t.Errorf("Admit got %+v, want %+v", fake.requests, want)
			}
		})
	}
}

func TestSubmitMapsAdmissionResults(t *testing.T) {
	admitted := func(dup bool) *orvexv1.SubmitCommandResponse {
		return &orvexv1.SubmitCommandResponse{Result: &orvexv1.SubmitCommandResponse_Admitted{Admitted: &orvexv1.Admission{
			RoundId: testRound[:], Sequence: 5, EffectiveTimeUnixMicro: 1_763_800_006_210_000, Duplicate: dup,
		}}}
	}
	rejected := func(r orvexv1.RejectionReason) *orvexv1.SubmitCommandResponse {
		return &orvexv1.SubmitCommandResponse{Result: &orvexv1.SubmitCommandResponse_Rejected{Rejected: &orvexv1.Rejection{Reason: r}}}
	}
	retry := func(r orvexv1.RetryReason) *orvexv1.SubmitCommandResponse {
		return &orvexv1.SubmitCommandResponse{Result: &orvexv1.SubmitCommandResponse_Retry{Retry: &orvexv1.Retry{Reason: r}}}
	}
	notAdmitted := fmt.Errorf("admission: not admitted: %w", context.DeadlineExceeded)
	for name, tc := range map[string]struct {
		dup  bool
		err  error
		want *orvexv1.SubmitCommandResponse
		code codes.Code
	}{
		"admitted":             {want: admitted(false)},
		"duplicate":            {dup: true, want: admitted(true)},
		"round closed":         {err: &admission.RejectedError{Reason: admission.ReasonRoundClosed}, want: rejected(orvexv1.RejectionReason_REJECTION_REASON_ROUND_CLOSED)},
		"round sealed":         {err: &admission.RejectedError{Reason: admission.ReasonRoundSealed}, want: rejected(orvexv1.RejectionReason_REJECTION_REASON_ROUND_SEALED)},
		"insufficient credits": {err: fmt.Errorf("%w: %w", &admission.RejectedError{Reason: admission.ReasonInsufficientCredits}, errors.New("pg")), want: rejected(orvexv1.RejectionReason_REJECTION_REASON_INSUFFICIENT_CREDITS)},
		"command id reused":    {err: fmt.Errorf("%w: %w", admission.ErrCommandIDReused, errors.New("23505")), want: rejected(orvexv1.RejectionReason_REJECTION_REASON_COMMAND_ID_REUSED)},
		"round not found":      {err: admission.ErrRoundNotFound, want: rejected(orvexv1.RejectionReason_REJECTION_REASON_ROUND_NOT_FOUND)},
		"busy":                 {err: fmt.Errorf("%w: lock", admission.ErrBusy), want: retry(orvexv1.RetryReason_RETRY_REASON_NOT_ADMITTED)},
		"unavailable":          {err: fmt.Errorf("%w: down", admission.ErrUnavailable), want: retry(orvexv1.RetryReason_RETRY_REASON_NOT_ADMITTED)},
		"context before send":  {err: notAdmitted, want: retry(orvexv1.RetryReason_RETRY_REASON_NOT_ADMITTED)},
		"outcome unknown":      {err: fmt.Errorf("%w: %w", admission.ErrOutcomeUnknown, context.Canceled), want: retry(orvexv1.RetryReason_RETRY_REASON_OUTCOME_UNKNOWN)},
		"invalid command":      {err: fmt.Errorf("%w: nul", admission.ErrInvalidCommand), code: codes.InvalidArgument},
		"rules mismatch":       {err: fmt.Errorf("%w: rules", admission.ErrInvalidRequest), code: codes.InvalidArgument},
		"lifecycle order":      {err: &admission.RejectedError{Reason: admission.ReasonLifecycleOrder}, code: codes.Internal},
		"closure not due":      {err: &admission.ClosureNotDueError{}, code: codes.Internal},
		"internal":             {err: fmt.Errorf("%w: corrupt", admission.ErrInternal), code: codes.Internal},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAdmitter{err: tc.err}
			if tc.err == nil {
				fake.admission = admission.Admission{RoundID: testRound, Sequence: 5, EffectiveTimeUnixMicro: 1_763_800_006_210_000, Duplicate: tc.dup}
			}
			got, err := newTestServer(fake).SubmitCommand(context.Background(), submitRequest(&orvexv1.CashOut{}))
			if status.Code(err) != tc.code {
				t.Fatalf("SubmitCommand error %v, want code %v", err, tc.code)
			}
			if tc.want != nil && !proto.Equal(got, tc.want) {
				t.Errorf("SubmitCommand = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSubmitRefusesMalformedRequests(t *testing.T) {
	for name, mutate := range map[string]func(*orvexv1.SubmitCommandRequest){
		"no principal":          func(r *orvexv1.SubmitCommandRequest) { r.Principal = nil },
		"invalid player":        func(r *orvexv1.SubmitCommandRequest) { r.Principal.PlayerId = "P ana" },
		"player with nul":       func(r *orvexv1.SubmitCommandRequest) { r.Principal.PlayerId = "p\x00ana" },
		"short client id":       func(r *orvexv1.SubmitCommandRequest) { r.ClientCommandId = clientID[:15] },
		"nil client id":         func(r *orvexv1.SubmitCommandRequest) { r.ClientCommandId = make([]byte, 16) },
		"nil round id":          func(r *orvexv1.SubmitCommandRequest) { r.RoundId = make([]byte, 16) },
		"no command":            func(r *orvexv1.SubmitCommandRequest) { r.Command = nil },
		"place bet without bet": func(r *orvexv1.SubmitCommandRequest) { r.Command = &orvexv1.SubmitCommandRequest_PlaceBet{} },
	} {
		t.Run(name, func(t *testing.T) {
			req := submitRequest(&orvexv1.CashOut{})
			mutate(req)
			fake := &fakeAdmitter{}
			_, err := newTestServer(fake).SubmitCommand(context.Background(), req)
			if name == "place bet without bet" {
				// A nil PlaceBet reads as a zero bet, which Decide rejects; it
				// still carries player authority.
				if err != nil || fake.requests[0].Command != (v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: "p_ana"}) {
					t.Errorf("SubmitCommand = %v, %+v", err, fake.requests)
				}
				return
			}
			if status.Code(err) != codes.InvalidArgument || len(fake.requests) != 0 {
				t.Errorf("SubmitCommand error %v after %d admits; want InvalidArgument and none", err, len(fake.requests))
			}
		})
	}
}

func TestLookupUsesThePrincipalsCommandID(t *testing.T) {
	fake := &fakeAdmitter{status: admission.Status{
		Admission: admission.Admission{RoundID: testRound, Sequence: 4, EffectiveTimeUnixMicro: 9},
		Outcome:   admission.OutcomeRejected, Reason: "no_open_bet",
	}}
	s := newTestServer(fake)
	got, err := s.LookupCommand(context.Background(), &orvexv1.LookupCommandRequest{
		Principal: &orvexv1.Principal{PlayerId: "p_bo"}, ClientCommandId: clientID[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	want := &orvexv1.LookupCommandResponse{
		Admission: &orvexv1.Admission{RoundId: testRound[:], Sequence: 4, EffectiveTimeUnixMicro: 9},
		Outcome:   orvexv1.Outcome_OUTCOME_REJECTED, Reason: "no_open_bet",
	}
	if !proto.Equal(got, want) {
		t.Errorf("LookupCommand = %v, want %v", got, want)
	}
	if len(fake.lookups) != 1 || fake.lookups[0] != commandID("p_bo", clientID) {
		t.Errorf("Lookup ids %x, want p_bo's derived id", fake.lookups)
	}

	fake.lookupErr = admission.ErrNotFound
	if _, err := s.LookupCommand(context.Background(), &orvexv1.LookupCommandRequest{
		Principal: &orvexv1.Principal{PlayerId: "p_bo"}, ClientCommandId: clientID[:],
	}); status.Code(err) != codes.NotFound {
		t.Errorf("LookupCommand of an unknown command: %v, want NotFound", err)
	}
	fake.lookupErr = fmt.Errorf("%w: down", admission.ErrUnavailable)
	if _, err := s.LookupCommand(context.Background(), &orvexv1.LookupCommandRequest{
		Principal: &orvexv1.Principal{PlayerId: "p_bo"}, ClientCommandId: clientID[:],
	}); status.Code(err) != codes.Unavailable {
		t.Errorf("LookupCommand with the database down: %v, want Unavailable", err)
	}
}

// startBufServer serves s behind tokenAuth for token over an in-memory
// listener and returns a dialer for it.
func startBufServer(t *testing.T, s *server, token string) func(creds grpc.DialOption) orvexv1.EngineServiceClient {
	t.Helper()
	ln := bufconn.Listen(1 << 20)
	auth := newTokenAuth(token)
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(auth.unary), grpc.ChainStreamInterceptor(auth.stream))
	orvexv1.RegisterEngineServiceServer(srv, s)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return func(creds grpc.DialOption) orvexv1.EngineServiceClient {
		opts := []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		}
		if creds != nil {
			opts = append(opts, creds)
		}
		conn, err := grpc.NewClient("passthrough:///bufnet", opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return orvexv1.NewEngineServiceClient(conn)
	}
}

func TestServiceTokenAuth(t *testing.T) {
	token := strings.Repeat("t", 40)
	fake := &fakeAdmitter{admission: admission.Admission{RoundID: testRound, Sequence: 1}}
	dial := startBufServer(t, newTestServer(fake), token)
	ctx := context.Background()
	req := submitRequest(&orvexv1.CashOut{})

	if _, err := dial(grpc.WithPerRPCCredentials(serviceToken(token))).SubmitCommand(ctx, req); err != nil {
		t.Errorf("with the token: %v", err)
	}
	for name, creds := range map[string]grpc.DialOption{
		"no token":     nil,
		"wrong token":  grpc.WithPerRPCCredentials(serviceToken(strings.Repeat("u", 40))),
		"token prefix": grpc.WithPerRPCCredentials(serviceToken(token[:39])),
	} {
		c := dial(creds)
		if _, err := c.SubmitCommand(ctx, req); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: SubmitCommand = %v, want Unauthenticated", name, err)
		}
		sub, err := c.SubscribeRound(ctx, &orvexv1.SubscribeRoundRequest{RoundId: testRound[:]})
		if err == nil {
			_, err = sub.Recv()
		}
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: SubscribeRound = %v, want Unauthenticated", name, err)
		}
	}
	if len(fake.requests) != 1 {
		t.Errorf("%d admits, want 1: an unauthenticated call reached the handler", len(fake.requests))
	}
}

func TestDialRequiresLoopback(t *testing.T) {
	token := strings.Repeat("t", 40)
	for _, addr := range []string{"127.0.0.1:7001", "localhost:7001", "[::1]:7001", "127.0.0.2:7001"} {
		c, err := Dial(addr, token)
		if err != nil {
			t.Errorf("Dial(%q): %v", addr, err)
			continue
		}
		_ = c.Close()
	}
	for _, addr := range []string{"10.0.0.1:7001", "engine:7001", "0.0.0.0:7001", "127.0.0.1", ""} {
		if c, err := Dial(addr, token); err == nil {
			_ = c.Close()
			t.Errorf("Dial(%q) succeeded, want an error", addr)
		}
	}
	if _, err := Dial("127.0.0.1:7001", "short"); err == nil {
		t.Error("Dial with a short token succeeded")
	}
}

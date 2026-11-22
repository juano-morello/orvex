// Package engineapi is the gateway-to-engine gRPC boundary
// (proto/orvex/v1/engine.proto): the engine-side server Serve and the
// gateway-side client Dial.
//
// The engine trusts a request's principal only because the caller
// presented the shared gateway service token. It builds every command
// itself with player authority and the principal as the player, so no
// caller can submit a lifecycle command or act for another player, and it
// derives the admission command ID from the principal and the client's
// command ID. Subscribers read the public projection of chain records from
// the database; the relay's wakeups only shorten the wait. No path here
// reads a round's secret config or the keyring (LBT-21).
package engineapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/juano-morello/orvex/internal/audit/chain"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/relay"
	"github.com/juano-morello/orvex/internal/runtime/sessions"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

// Server tuning.
const (
	// pollInterval bounds a subscriber's wait when no wakeup arrives.
	pollInterval = time.Second
	// batchLimit is the most records one subscriber read returns.
	batchLimit = 256
	// shutdownGrace is how long Serve waits for unary calls to finish.
	shutdownGrace = 5 * time.Second
	maxRecvBytes  = 64 << 10
)

// Serve serves EngineService on listener until ctx is done, then stops
// and returns nil; it returns the server's error if serving fails first.
// It also runs the outbox relay and the LISTEN loop that wake subscribers
// (internal/runtime/relay). Every call must carry gatewayToken, which
// must be a service token as sessions.LoadServiceTokenFile returns. The
// connection is plaintext: bind listener to a loopback address.
func Serve(ctx context.Context, listener net.Listener, pool *pgxpool.Pool, sequencer *admission.Sequencer, gatewayToken string) error {
	if listener == nil || pool == nil || sequencer == nil {
		return errors.New("engineapi: nil listener, pool or sequencer")
	}
	if !sessions.ValidServiceToken(gatewayToken) {
		return fmt.Errorf("engineapi: the gateway token is not %d to 512 bytes of printable ASCII without spaces",
			sessions.MinServiceTokenBytes)
	}
	log := slog.Default()
	ctx, cancel := context.WithCancel(ctx)
	wakeups := relay.NewWakeups()
	var wg sync.WaitGroup
	wg.Go(func() { relay.Listen(ctx, pool.Config().ConnConfig, wakeups, log) })
	wg.Go(func() { relay.RunPublisher(ctx, pool, log) })

	auth := newTokenAuth(gatewayToken)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(auth.unary),
		grpc.ChainStreamInterceptor(auth.stream),
		grpc.MaxRecvMsgSize(maxRecvBytes),
	)
	orvexv1.RegisterEngineServiceServer(srv, &server{
		admit:   sequencer,
		db:      pool,
		wakeups: wakeups,
		done:    ctx.Done(),
		log:     log,
	})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()

	var err error
	select {
	case <-ctx.Done():
		stopped := make(chan struct{})
		go func() { srv.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(shutdownGrace):
			srv.Stop()
			<-stopped
		}
		<-served
	case err = <-served:
		srv.Stop()
	}
	cancel()
	wg.Wait()
	return err
}

// admitter is what the server needs from *admission.Sequencer.
type admitter interface {
	Admit(ctx context.Context, req admission.Request) (admission.Admission, error)
	Lookup(ctx context.Context, commandID [16]byte) (admission.Status, error)
}

type server struct {
	orvexv1.UnimplementedEngineServiceServer
	admit   admitter
	db      eventstore.TxBeginner
	wakeups *relay.Wakeups
	// done closes when Serve is stopping; open subscriptions end.
	done <-chan struct{}
	log  *slog.Logger
}

func (s *server) GetCurrentRound(ctx context.Context, _ *orvexv1.GetCurrentRoundRequest) (*orvexv1.GetCurrentRoundResponse, error) {
	round, err := eventstore.CurrentRound(ctx, s.db, v1.GameID, v1.RulesVersionNumber)
	if errors.Is(err, eventstore.ErrRoundNotFound) {
		return nil, status.Error(codes.NotFound, "no round")
	}
	if err != nil {
		return nil, s.readError(ctx, "current round", err)
	}
	return &orvexv1.GetCurrentRoundResponse{
		RoundId:      round[:],
		Game:         string(v1.GameID),
		RulesVersion: uint32(v1.RulesVersionNumber),
	}, nil
}

func (s *server) SubmitCommand(ctx context.Context, req *orvexv1.SubmitCommandRequest) (*orvexv1.SubmitCommandResponse, error) {
	player, err := principal(req.GetPrincipal())
	if err != nil {
		return nil, err
	}
	client, err := uuidField("client_command_id", req.GetClientCommandId())
	if err != nil {
		return nil, err
	}
	round, err := uuidField("round_id", req.GetRoundId())
	if err != nil {
		return nil, err
	}
	var command v1.Command
	switch c := req.GetCommand().(type) {
	case *orvexv1.SubmitCommandRequest_PlaceBet:
		command = v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: v1.PlayerID(player), BetCredits: c.PlaceBet.GetBetCredits()}
	case *orvexv1.SubmitCommandRequest_CashOut:
		command = v1.CashOut{Authority: v1.AuthorityPlayer, Player: v1.PlayerID(player)}
	default:
		return nil, status.Error(codes.InvalidArgument, "command: want place_bet or cash_out")
	}
	a, err := s.admit.Admit(ctx, admission.Request{
		RoundID:      round,
		Game:         v1.GameID,
		RulesVersion: v1.RulesVersionNumber,
		CommandID:    commandID(player, client),
		Command:      command,
	})
	return s.submitResult(a, err)
}

// submitResult maps Admit's result (S-12). Every outcome a caller can act
// on travels in the response; a status error is a bad request or a bug.
//
// S-12 classifies only Admit's last attempt, and an earlier attempt's
// COMMIT may have succeeded without its reply. So ErrBusy, ErrUnavailable
// and a context error map to OUTCOME_UNKNOWN, because none of them proves
// that the submission admitted nothing. RETRY_REASON_NOT_ADMITTED is
// reserved for such a proof, and no path here has one.
func (s *server) submitResult(a admission.Admission, err error) (*orvexv1.SubmitCommandResponse, error) {
	rejected := func(r orvexv1.RejectionReason) (*orvexv1.SubmitCommandResponse, error) {
		return &orvexv1.SubmitCommandResponse{Result: &orvexv1.SubmitCommandResponse_Rejected{
			Rejected: &orvexv1.Rejection{Reason: r},
		}}, nil
	}
	retry := func(r orvexv1.RetryReason) (*orvexv1.SubmitCommandResponse, error) {
		return &orvexv1.SubmitCommandResponse{Result: &orvexv1.SubmitCommandResponse_Retry{
			Retry: &orvexv1.Retry{Reason: r},
		}}, nil
	}
	var rej *admission.RejectedError
	switch {
	case err == nil:
		return &orvexv1.SubmitCommandResponse{Result: &orvexv1.SubmitCommandResponse_Admitted{
			Admitted: admissionProto(a),
		}}, nil
	case errors.As(err, &rej):
		switch rej.Reason {
		case admission.ReasonRoundClosed:
			return rejected(orvexv1.RejectionReason_REJECTION_REASON_ROUND_CLOSED)
		case admission.ReasonRoundSealed:
			return rejected(orvexv1.RejectionReason_REJECTION_REASON_ROUND_SEALED)
		case admission.ReasonInsufficientCredits:
			return rejected(orvexv1.RejectionReason_REJECTION_REASON_INSUFFICIENT_CREDITS)
		}
	case errors.Is(err, admission.ErrRoundNotFound):
		return rejected(orvexv1.RejectionReason_REJECTION_REASON_ROUND_NOT_FOUND)
	case errors.Is(err, admission.ErrCommandIDReused):
		return rejected(orvexv1.RejectionReason_REJECTION_REASON_COMMAND_ID_REUSED)
	case errors.Is(err, admission.ErrOutcomeUnknown), errors.Is(err, admission.ErrBusy), errors.Is(err, admission.ErrUnavailable),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return retry(orvexv1.RetryReason_RETRY_REASON_OUTCOME_UNKNOWN)
	case errors.Is(err, admission.ErrInvalidCommand), errors.Is(err, admission.ErrInvalidRequest):
		return nil, status.Error(codes.InvalidArgument, "command not admissible to this round")
	}
	s.log.Error("engineapi: submit failed", "error", err.Error())
	return nil, status.Error(codes.Internal, "internal error")
}

func (s *server) LookupCommand(ctx context.Context, req *orvexv1.LookupCommandRequest) (*orvexv1.LookupCommandResponse, error) {
	player, err := principal(req.GetPrincipal())
	if err != nil {
		return nil, err
	}
	client, err := uuidField("client_command_id", req.GetClientCommandId())
	if err != nil {
		return nil, err
	}
	id := commandID(player, client)
	st, err := s.admit.Lookup(ctx, id)
	if errors.Is(err, admission.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "no admission for the command")
	}
	if err != nil {
		return nil, s.readError(ctx, "lookup", err)
	}
	out := &orvexv1.LookupCommandResponse{Admission: admissionProto(st.Admission)}
	switch st.Outcome {
	case admission.OutcomePending:
		out.Outcome = orvexv1.Outcome_OUTCOME_PENDING
	case admission.OutcomeRejected:
		out.Outcome, out.Reason = orvexv1.Outcome_OUTCOME_REJECTED, st.Reason
	case admission.OutcomeAccepted:
		out.Outcome = orvexv1.Outcome_OUTCOME_ACCEPTED
		if out.Events, err = s.commandEvents(ctx, st.Admission, id); err != nil {
			return nil, err
		}
	default:
		s.log.Error("engineapi: lookup returned an unknown outcome")
		return nil, status.Error(codes.Internal, "internal error")
	}
	return out, nil
}

// commandEvents returns the events of the executed command a, whose record
// must name command ID id.
func (s *server) commandEvents(ctx context.Context, a admission.Admission, id [16]byte) ([]*orvexv1.CrashEvent, error) {
	b, err := eventstore.ReadRecord(ctx, s.db, a.RoundID, a.Sequence)
	if err != nil {
		return nil, s.readError(ctx, "command record", err)
	}
	rec, err := chain.Decode(b)
	c, ok := rec.(chain.Command)
	if err != nil || !ok || c.CommandID != id || c.Sequence != a.Sequence || c.Result.Rejected {
		s.log.Error("engineapi: executed command record does not match its admission", "sequence", a.Sequence)
		return nil, status.Error(codes.Internal, "internal error")
	}
	events, err := publicEvents(c)
	if err != nil {
		s.log.Error("engineapi: map command events", "error", err.Error())
		return nil, status.Error(codes.Internal, "internal error")
	}
	return events, nil
}

func (s *server) SubscribeRound(req *orvexv1.SubscribeRoundRequest, stream grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse]) error {
	round, err := uuidField("round_id", req.GetRoundId())
	if err != nil {
		return err
	}
	ctx := stream.Context()
	var next uint64
	if req.AfterSequence != nil {
		if req.GetAfterSequence() >= math.MaxInt64 {
			return status.Error(codes.FailedPrecondition, "invalid cursor")
		}
		next = req.GetAfterSequence() + 1
	}
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	for first := true; ; first = false {
		// Wait before the read: a commit during the read still wakes us.
		wake, release := s.wakeups.Wait(round)
		var idle bool
		next, idle, err = s.deliver(stream, round, next, first)
		if err == nil && idle {
			select {
			case <-wake:
			case <-poll.C:
			case <-ctx.Done():
				err = status.FromContextError(ctx.Err()).Err()
			case <-s.done:
				err = status.Error(codes.Unavailable, "engine shutting down")
			}
		}
		release()
		if errors.Is(err, errRoundComplete) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// errRoundComplete ends a subscription that delivered the round's last
// record.
var errRoundComplete = errors.New("engineapi: round complete")

// deliver sends one batch of the round's records from sequence next and
// returns the sequence after the last one sent. idle reports that the
// batch reached the execution head, so the caller should wait for more.
// On the first batch it checks the round and the cursor and sends the
// stream headers.
func (s *server) deliver(stream grpc.ServerStreamingServer[orvexv1.SubscribeRoundResponse], round [16]byte, next uint64, first bool) (uint64, bool, error) {
	ctx := stream.Context()
	tail, err := eventstore.ReadTail(ctx, s.db, round, next, batchLimit)
	switch {
	case errors.Is(err, eventstore.ErrRoundNotFound):
		return next, false, status.Error(codes.NotFound, "round not found")
	case err != nil:
		return next, false, s.readError(ctx, "subscribe", err)
	}
	if first {
		if tail.Game != v1.GameID || tail.RulesVersion != v1.RulesVersionNumber {
			return next, false, status.Error(codes.NotFound, "round not found")
		}
		if next > tail.Head+1 {
			return next, false, status.Error(codes.FailedPrecondition, "invalid cursor")
		}
		// The headers tell the gateway the subscription is accepted before
		// any record exists to send.
		if err := stream.SendHeader(metadata.MD{}); err != nil {
			return next, false, err
		}
	}
	for _, r := range tail.Records {
		frame, err := publicRecord(round, r)
		if err != nil {
			s.log.Error("engineapi: map record", "error", err.Error())
			return next, false, status.Error(codes.Internal, "internal error")
		}
		if err := stream.Send(frame); err != nil {
			return next, false, err
		}
		next = r.Sequence + 1
	}
	if tail.Final && next == tail.Head+1 {
		return next, false, errRoundComplete
	}
	return next, len(tail.Records) < batchLimit, nil
}

// readError maps a database read failure: a context error to its status,
// connection loss and SQLSTATE classes 08, 53 and 57 to UNAVAILABLE, and
// anything else to INTERNAL.
func (s *server) readError(ctx context.Context, what string, err error) error {
	switch {
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case errors.Is(err, admission.ErrUnavailable):
		return status.Error(codes.Unavailable, "database unavailable")
	case errors.Is(err, admission.ErrInternal), errors.Is(err, eventstore.ErrCorrupt), errors.Is(err, eventstore.ErrRecordNotFound):
	default:
		if code := postgres.SQLState(err); code == "" || unavailableClass(code) {
			return status.Error(codes.Unavailable, "database unavailable")
		}
	}
	s.log.Error("engineapi: read failed", "read", what, "error", relay.Describe(err))
	return status.Error(codes.Internal, "internal error")
}

func unavailableClass(code string) bool {
	switch code[:2] {
	case "08", "53", "57":
		return true
	default:
		return false
	}
}

func admissionProto(a admission.Admission) *orvexv1.Admission {
	return &orvexv1.Admission{
		RoundId:                a.RoundID[:],
		Sequence:               a.Sequence,
		EffectiveTimeUnixMicro: a.EffectiveTimeUnixMicro,
		Duplicate:              a.Duplicate,
	}
}

func principal(p *orvexv1.Principal) (sessions.PlayerID, error) {
	id, ok := sessions.ParsePlayerID(p.GetPlayerId())
	if !ok {
		return "", status.Error(codes.InvalidArgument, "principal: invalid player id")
	}
	return id, nil
}

func uuidField(name string, b []byte) ([16]byte, error) {
	var id [16]byte
	if len(b) != len(id) || [16]byte(b) == id {
		return id, status.Errorf(codes.InvalidArgument, "%s: want a non-nil 16-byte UUID", name)
	}
	return [16]byte(b), nil
}

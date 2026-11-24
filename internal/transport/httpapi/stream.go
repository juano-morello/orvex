package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/juano-morello/orvex/internal/canonjson"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

// stream relays a round's records as Server-Sent Events:
//
//	id: <sequence>
//	event: record
//	data: {"sequence":"5","effectiveTimeUnixMicro":"...","events":[...]}
//
// The genesis frame carries "genesis" instead of "events". After the
// round's last record comes "event: end" and the response ends. The
// cursor is the last sequence the client holds, from Last-Event-ID or,
// without one, ?after=N. A cursor at the last record of a complete round
// gets 204, which ends EventSource. Across reconnects delivery is at least
// once: drop frames with sequence <= cursor.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	round, ok := parseUUID(r.PathValue("roundId"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	req := &orvexv1.SubscribeRoundRequest{RoundId: round[:]}
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	if cursor != "" {
		after, ok := parseUint(cursor)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_cursor")
			return
		}
		req.AfterSequence = &after
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(h.streams, cancel)()
	sub, err := h.engine.SubscribeRound(ctx, req)
	if err == nil {
		err = accepted(sub)
	}
	if err != nil {
		h.notStreamed(w, r, err)
		return
	}

	frames := make(chan received, writeBatchFrames)
	go func() {
		for {
			f, err := sub.Recv()
			select {
			case frames <- received{f, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	write := eventStream(w)
	if !write(retryField) {
		return
	}
	ping := time.NewTicker(h.pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if !write(": ping\n\n") {
				return
			}
		case got := <-frames:
			// Write every frame already received in one write and flush.
			var b strings.Builder
			ended := h.render(&b, got)
		batch:
			for !ended && b.Len() < writeBatchBytes {
				select {
				case got = <-frames:
					ended = h.render(&b, got)
				default:
					break batch
				}
			}
			if !write(b.String()) || ended {
				return
			}
		}
	}
}

// received is one result of the subscription's Recv.
type received struct {
	frame *orvexv1.SubscribeRoundResponse
	err   error
}

// A write carries the frames already received, up to writeBatchFrames
// frames or about writeBatchBytes.
const (
	writeBatchFrames = 64
	writeBatchBytes  = 64 << 10
)

// render appends the event for got to b and reports whether the response
// ends after it: at the round's end, which gets "end", or on a failure,
// which ends it without "end".
func (h *Handler) render(b *strings.Builder, got received) bool {
	if errors.Is(got.err, io.EOF) {
		b.WriteString("event: end\ndata: {}\n\n")
		return true
	}
	if got.err != nil {
		if code := status.Code(got.err); code != codes.Canceled {
			h.log.Warn("gateway: subscription ended", "code", code.String())
		}
		return true
	}
	data, err := frameJSON(got.frame)
	if err != nil {
		h.log.Error("gateway: engine sent a malformed record", "sequence", got.frame.GetSequence())
		return true
	}
	fmt.Fprintf(b, "id: %d\nevent: record\ndata: %s\n\n", got.frame.GetSequence(), data)
	return false
}

// retryField sets EventSource's reconnection time.
const retryField = "retry: 1000\n\n"

// accepted waits for the engine to accept the subscription, which it
// signals with headers before any record, and otherwise returns the
// stream's status.
func accepted(sub grpc.ServerStreamingClient[orvexv1.SubscribeRoundResponse]) error {
	if md, err := sub.Header(); err == nil && md != nil {
		return nil
	}
	_, err := sub.Recv()
	return err
}

// notStreamed answers a subscription the engine did not accept.
// EventSource stops for good on any status but 200, and reconnects with
// the same Last-Event-ID when a 200 stream ends. So a failure that may
// pass gets a stream that carries only the reconnection time and ends.
func (h *Handler) notStreamed(w http.ResponseWriter, r *http.Request, err error) {
	switch status.Code(err) {
	case codes.OutOfRange:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case codes.NotFound:
		writeError(w, http.StatusNotFound, "round_not_found")
	case codes.FailedPrecondition:
		writeError(w, http.StatusBadRequest, "invalid_cursor")
	case codes.InvalidArgument, codes.Internal, codes.Unauthenticated, codes.PermissionDenied, codes.Unimplemented:
		h.readFailed(w, "subscribe", err)
	default:
		if r.Context().Err() == nil {
			eventStream(w)(retryField)
		}
	}
}

// eventStream starts a 200 event stream and returns a function that writes
// and flushes s, reporting whether it could.
func eventStream(w http.ResponseWriter) func(s string) bool {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return func(s string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
}

// genesisJSON is the round's public config.
type genesisJSON struct {
	Commitment    string `json:"commitment"`
	MinBetCredits int64  `json:"minBetCredits,string"`
	MaxBetCredits int64  `json:"maxBetCredits,string"`
	MaxBets       int64  `json:"maxBets,string"`
}

// frameJSON renders one record frame.
func frameJSON(f *orvexv1.SubscribeRoundResponse) ([]byte, error) {
	switch body := f.GetBody().(type) {
	case *orvexv1.SubscribeRoundResponse_Genesis:
		g := body.Genesis
		if len(g.GetCommitment()) != 32 {
			return nil, errEvent
		}
		return json.Marshal(struct {
			Sequence               uint64      `json:"sequence,string"`
			EffectiveTimeUnixMicro int64       `json:"effectiveTimeUnixMicro,string"`
			Genesis                genesisJSON `json:"genesis"`
		}{f.GetSequence(), f.GetEffectiveTimeUnixMicro(), genesisJSON{
			Commitment:    string(canonjson.Hex(g.GetCommitment())),
			MinBetCredits: g.GetMinBetCredits(),
			MaxBetCredits: g.GetMaxBetCredits(),
			MaxBets:       g.GetMaxBets(),
		}})
	case *orvexv1.SubscribeRoundResponse_Events:
		events, err := eventsJSON(body.Events.GetEvents())
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Sequence               uint64 `json:"sequence,string"`
			EffectiveTimeUnixMicro int64  `json:"effectiveTimeUnixMicro,string"`
			Events                 []any  `json:"events"`
		}{f.GetSequence(), f.GetEffectiveTimeUnixMicro(), events})
	default:
		return nil, errEvent
	}
}

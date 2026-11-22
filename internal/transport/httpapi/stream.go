package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

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
// round's last record comes "event: end" and the response ends; a client
// must not reconnect to that round. The cursor is the last sequence the
// client holds, from Last-Event-ID or, without one, ?after=N. Across
// reconnects delivery is at least once: drop frames with sequence <=
// cursor.
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
	if err != nil {
		h.readFailed(w, "subscribe", err)
		return
	}
	// The engine sends headers once it accepts the subscription; without
	// them the stream failed and Recv returns the status.
	if md, err := sub.Header(); err != nil || md == nil {
		_, err := sub.Recv()
		switch status.Code(err) {
		case codes.NotFound:
			writeError(w, http.StatusNotFound, "round_not_found")
		case codes.FailedPrecondition:
			writeError(w, http.StatusBadRequest, "invalid_cursor")
		default:
			h.readFailed(w, "subscribe", err)
		}
		return
	}

	type received struct {
		frame *orvexv1.SubscribeRoundResponse
		err   error
	}
	frames := make(chan received)
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

	rc := http.NewResponseController(w)
	write := func(s string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if !write("retry: 1000\n\n") {
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
			if errors.Is(got.err, io.EOF) {
				write("event: end\ndata: {}\n\n")
				return
			}
			if got.err != nil {
				if code := status.Code(got.err); code != codes.Canceled {
					h.log.Warn("gateway: subscription ended", "code", code.String())
				}
				return
			}
			data, err := frameJSON(got.frame)
			if err != nil {
				h.log.Error("gateway: engine sent a malformed record", "sequence", got.frame.GetSequence())
				return
			}
			if !write(fmt.Sprintf("id: %d\nevent: record\ndata: %s\n\n", got.frame.GetSequence(), data)) {
				return
			}
		}
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

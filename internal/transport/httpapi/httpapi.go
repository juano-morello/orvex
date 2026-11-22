// Package httpapi is the gateway's external HTTP API. It authenticates
// players by bearer token, turns JSON requests into EngineService calls for
// that player, and relays a round's public records as Server-Sent Events.
//
//	GET  /v1/rounds/current[?game=crash]      the newest round (public)
//	POST /v1/rounds/{roundId}/commands        submit placeBet or cashOut
//	GET  /v1/commands/{clientCommandId}       the player's command status
//	GET  /v1/rounds/{roundId}/stream[?after=N] the round's records (public)
//
// JSON is camelCase and every 64-bit integer is a decimal string. A request
// body never names a player: the token does. Errors are {"code": "..."};
// every 503 means "resubmit the identical request". The gateway adds no
// retries of its own and sends no time to the engine (ADR-002).
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/runtime/sessions"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

// Authenticator maps a bearer token to a player; *sessions.Players
// implements it.
type Authenticator interface {
	Authenticate(token string) (sessions.PlayerID, bool)
}

// Timeouts.
const (
	submitTimeout = 15 * time.Second
	readTimeout   = 5 * time.Second
	writeTimeout  = 10 * time.Second
	maxBodyBytes  = 4 << 10
)

// Handler serves the API. Create it with New.
type Handler struct {
	engine orvexv1.EngineServiceClient
	auth   Authenticator
	log    *slog.Logger
	mux    *http.ServeMux
	// streams ends every open stream when cancelled; see CloseStreams.
	streams      context.Context
	closeStreams context.CancelFunc
	// pingInterval spaces SSE comment lines on an idle stream.
	pingInterval time.Duration
}

// New returns the API over engine, authenticating players with auth.
func New(engine orvexv1.EngineServiceClient, auth Authenticator, log *slog.Logger) *Handler {
	h := &Handler{engine: engine, auth: auth, log: log, mux: http.NewServeMux(), pingInterval: 15 * time.Second}
	h.streams, h.closeStreams = context.WithCancel(context.Background())
	for _, rt := range []struct {
		method, path string
		serve        http.HandlerFunc
	}{
		{http.MethodGet, "/v1/rounds/current", h.currentRound},
		{http.MethodPost, "/v1/rounds/{roundId}/commands", h.authenticated(h.submit)},
		{http.MethodGet, "/v1/commands/{clientCommandId}", h.authenticated(h.lookup)},
		{http.MethodGet, "/v1/rounds/{roundId}/stream", h.stream},
	} {
		h.mux.HandleFunc(rt.method+" "+rt.path, rt.serve)
		h.mux.HandleFunc(rt.path, methodNotAllowed(rt.method))
	}
	h.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusNotFound, "not_found") })
	return h
}

// methodNotAllowed answers 405 on a known path. ServeMux serves HEAD with
// a GET route's handler, so a GET route allows HEAD too.
func methodNotAllowed(method string) http.HandlerFunc {
	allow := method
	if method == http.MethodGet {
		allow = "GET, HEAD"
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// CloseStreams ends every open and future event stream, so that an
// http.Server's Shutdown, which waits for active requests, can finish.
// Register it with the server's RegisterOnShutdown.
func (h *Handler) CloseStreams() { h.closeStreams() }

// authenticated runs next for the player of the request's bearer token,
// or answers 401.
func (h *Handler) authenticated(next func(http.ResponseWriter, *http.Request, sessions.PlayerID)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if ok && strings.EqualFold(scheme, "Bearer") && token != "" {
			if player, ok := h.auth.Authenticate(token); ok {
				next(w, r, player)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="orvex"`)
		writeError(w, http.StatusUnauthorized, "unauthenticated")
	}
}

func (h *Handler) currentRound(w http.ResponseWriter, r *http.Request) {
	if g := r.URL.Query().Get("game"); g != "" && g != "crash" {
		writeError(w, http.StatusNotFound, "unknown_game")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	resp, err := h.engine.GetCurrentRound(ctx, &orvexv1.GetCurrentRoundRequest{})
	if status.Code(err) == codes.NotFound {
		writeError(w, http.StatusNotFound, "no_current_round")
		return
	}
	if err != nil {
		h.readFailed(w, "current round", err)
		return
	}
	round, ok := uuidText(resp.GetRoundId())
	if !ok {
		h.internal(w, "current round: engine returned a malformed round id")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		RoundID      string `json:"roundId"`
		Game         string `json:"game"`
		RulesVersion uint32 `json:"rulesVersion,string"`
	}{round, resp.GetGame(), resp.GetRulesVersion()})
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request, player sessions.PlayerID) {
	round, ok := parseUUID(r.PathValue("roundId"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	req := &orvexv1.SubmitCommandRequest{
		Principal: &orvexv1.Principal{PlayerId: string(player)},
		RoundId:   round[:],
	}
	clientText, ok := parseSubmission(b, req)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), submitTimeout)
	defer cancel()
	resp, err := h.engine.SubmitCommand(ctx, req)
	if err != nil {
		switch code := status.Code(err); code {
		case codes.InvalidArgument:
			writeError(w, http.StatusBadRequest, "invalid_request")
		case codes.Internal, codes.Unauthenticated, codes.PermissionDenied, codes.Unimplemented:
			h.internal(w, "submit: engine answered "+code.String())
		default:
			// The call may have reached the engine and committed.
			writeRetry(w, "outcome_unknown")
		}
		return
	}
	switch res := resp.GetResult().(type) {
	case *orvexv1.SubmitCommandResponse_Admitted:
		a, ok := admissionOf(res.Admitted)
		if !ok {
			h.internal(w, "submit: engine returned a malformed admission")
			return
		}
		writeJSON(w, http.StatusAccepted, struct {
			ClientCommandID string `json:"clientCommandId"`
			admissionJSON
			Duplicate bool   `json:"duplicate"`
			Status    string `json:"status"`
		}{clientText, a, res.Admitted.GetDuplicate(), "admitted"})
	case *orvexv1.SubmitCommandResponse_Rejected:
		code, ok := rejectionCodes[res.Rejected.GetReason()]
		if !ok {
			h.internal(w, "submit: engine returned an unknown rejection")
			return
		}
		writeError(w, code.status, code.code)
	case *orvexv1.SubmitCommandResponse_Retry:
		switch res.Retry.GetReason() {
		case orvexv1.RetryReason_RETRY_REASON_NOT_ADMITTED:
			writeRetry(w, "not_admitted")
		default:
			writeRetry(w, "outcome_unknown")
		}
	default:
		h.internal(w, "submit: engine returned no result")
	}
}

type httpCode struct {
	status int
	code   string
}

var rejectionCodes = map[orvexv1.RejectionReason]httpCode{
	orvexv1.RejectionReason_REJECTION_REASON_ROUND_NOT_FOUND:      {http.StatusNotFound, "round_not_found"},
	orvexv1.RejectionReason_REJECTION_REASON_ROUND_CLOSED:         {http.StatusConflict, "round_closed"},
	orvexv1.RejectionReason_REJECTION_REASON_ROUND_SEALED:         {http.StatusConflict, "round_sealed"},
	orvexv1.RejectionReason_REJECTION_REASON_INSUFFICIENT_CREDITS: {http.StatusConflict, "insufficient_credits"},
	orvexv1.RejectionReason_REJECTION_REASON_COMMAND_ID_REUSED:    {http.StatusConflict, "command_id_reused"},
}

var outcomeText = map[orvexv1.Outcome]string{
	orvexv1.Outcome_OUTCOME_PENDING:  "pending",
	orvexv1.Outcome_OUTCOME_ACCEPTED: "accepted",
	orvexv1.Outcome_OUTCOME_REJECTED: "rejected",
}

func (h *Handler) lookup(w http.ResponseWriter, r *http.Request, player sessions.PlayerID) {
	text := r.PathValue("clientCommandId")
	client, ok := parseUUID(text)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	resp, err := h.engine.LookupCommand(ctx, &orvexv1.LookupCommandRequest{
		Principal:       &orvexv1.Principal{PlayerId: string(player)},
		ClientCommandId: client[:],
	})
	if status.Code(err) == codes.NotFound {
		writeError(w, http.StatusNotFound, "command_not_found")
		return
	}
	if err != nil {
		h.readFailed(w, "lookup", err)
		return
	}
	a, ok := admissionOf(resp.GetAdmission())
	outcome, known := outcomeText[resp.GetOutcome()]
	events, err := eventsJSON(resp.GetEvents())
	if !ok || !known || err != nil {
		h.internal(w, "lookup: engine returned a malformed status")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ClientCommandID string `json:"clientCommandId"`
		admissionJSON
		Status string `json:"status"`
		Reason string `json:"reason,omitempty"`
		Events []any  `json:"events"`
	}{text, a, outcome, resp.GetReason(), events})
}

// admissionJSON is an admission receipt's public fields.
type admissionJSON struct {
	RoundID                string `json:"roundId"`
	Sequence               uint64 `json:"sequence,string"`
	EffectiveTimeUnixMicro int64  `json:"effectiveTimeUnixMicro,string"`
}

func admissionOf(a *orvexv1.Admission) (admissionJSON, bool) {
	round, ok := uuidText(a.GetRoundId())
	if !ok || a.GetSequence() == 0 {
		return admissionJSON{}, false
	}
	return admissionJSON{RoundID: round, Sequence: a.GetSequence(), EffectiveTimeUnixMicro: a.GetEffectiveTimeUnixMicro()}, true
}

// readFailed answers a failed read call: 400 for a request the engine
// refused, 500 for an engine or configuration fault, else 503.
func (h *Handler) readFailed(w http.ResponseWriter, call string, err error) {
	switch code := status.Code(err); code {
	case codes.InvalidArgument:
		writeError(w, http.StatusBadRequest, "invalid_request")
	case codes.Internal, codes.Unauthenticated, codes.PermissionDenied, codes.Unimplemented:
		h.internal(w, call+": engine answered "+code.String())
	default:
		writeRetry(w, "unavailable")
	}
}

func (h *Handler) internal(w http.ResponseWriter, msg string) {
	h.log.Error("gateway: " + msg)
	writeError(w, http.StatusInternalServerError, "internal")
}

// parseSubmission parses a submission body into req's client command ID
// and command, and returns the client command ID's text. The body is
// {"clientCommandId":"<uuid>"} with exactly one of
// "placeBet":{"betCredits":"<n>"} and "cashOut":{}. Keys match byte for
// byte and appear at most once, so no key names a player and none
// overrides another.
func parseSubmission(b []byte, req *orvexv1.SubmitCommandRequest) (string, bool) {
	var (
		clientText, credits string
		bet, cash           bool
	)
	dec := json.NewDecoder(bytes.NewReader(b))
	err := object(dec, fields{
		"clientCommandId": stringField(&clientText),
		"placeBet": func(dec *json.Decoder) error {
			bet = true
			return object(dec, fields{"betCredits": stringField(&credits)})
		},
		"cashOut": func(dec *json.Decoder) error {
			cash = true
			return object(dec, nil)
		},
	})
	if err != nil {
		return "", false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}
	client, ok := parseUUID(clientText)
	if !ok {
		return "", false
	}
	switch {
	case bet && !cash:
		n, ok := parsePositive(credits)
		if !ok {
			return "", false
		}
		req.Command = &orvexv1.SubmitCommandRequest_PlaceBet{PlaceBet: &orvexv1.PlaceBet{BetCredits: n}}
	case cash && !bet:
		req.Command = &orvexv1.SubmitCommandRequest_CashOut{CashOut: &orvexv1.CashOut{}}
	default:
		return "", false
	}
	req.ClientCommandId = client[:]
	return clientText, true
}

// fields maps each key an object may hold to the parser of its value.
type fields map[string]func(*json.Decoder) error

var errBody = errors.New("httpapi: malformed request body")

// object decodes one JSON object whose keys are all in fs, each at most
// once.
func object(dec *json.Decoder, fs fields) error {
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errBody
	}
	seen := make(map[string]bool, len(fs))
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return errBody
		}
		key, _ := t.(string)
		parse, known := fs[key]
		if !known || seen[key] {
			return errBody
		}
		seen[key] = true
		if err := parse(dec); err != nil {
			return err
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return errBody
	}
	return nil
}

func stringField(out *string) func(*json.Decoder) error {
	return func(dec *json.Decoder) error {
		t, err := dec.Token()
		s, ok := t.(string)
		if err != nil || !ok {
			return errBody
		}
		*out = s
		return nil
	}
}

// parseUUID parses canonical lowercase UUID text and refuses the nil UUID.
func parseUUID(s string) ([16]byte, bool) {
	id, err := canonjson.ParseUUID(canonjson.String(s))
	return id, err == nil && id != [16]byte{}
}

func uuidText(b []byte) (string, bool) {
	if len(b) != 16 {
		return "", false
	}
	return string(canonjson.UUID([16]byte(b))), true
}

// parseUint parses canonical decimal text: no sign, no leading zero.
func parseUint(s string) (uint64, bool) {
	if s == "" || len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// parsePositive parses canonical decimal text of an int64 >= 1.
func parsePositive(s string) (int64, bool) {
	n, ok := parseUint(s)
	if !ok || n < 1 || n > 1<<63-1 {
		return 0, false
	}
	return int64(n), true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = http.StatusInternalServerError, []byte(`{"code":"internal"}`)
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeTimeout))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, code int, errCode string) {
	writeJSON(w, code, struct {
		Code string `json:"code"`
	}{errCode})
}

// writeRetry answers 503: resubmit the identical request.
func writeRetry(w http.ResponseWriter, errCode string) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, errCode)
}

// errEvent reports an event the gateway cannot render.
var errEvent = errors.New("httpapi: malformed event")

func eventsJSON(events []*orvexv1.CrashEvent) ([]any, error) {
	out := make([]any, 0, len(events))
	for _, e := range events {
		j, err := eventJSON(e)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// eventJSON renders a crash/v1 event as JSON with its type name.
func eventJSON(e *orvexv1.CrashEvent) (any, error) {
	switch ev := e.GetEvent().(type) {
	case *orvexv1.CrashEvent_BetPlaced:
		return struct {
			Type              string `json:"type"`
			PlayerID          string `json:"playerId"`
			BetCredits        int64  `json:"betCredits,string"`
			PlacedAtUnixMicro int64  `json:"placedAtUnixMicro,string"`
		}{"BetPlaced", ev.BetPlaced.GetPlayerId(), ev.BetPlaced.GetBetCredits(), ev.BetPlaced.GetPlacedAtUnixMicro()}, nil
	case *orvexv1.CrashEvent_RoundStarted:
		return struct {
			Type               string `json:"type"`
			StartedAtUnixMicro int64  `json:"startedAtUnixMicro,string"`
		}{"RoundStarted", ev.RoundStarted.GetStartedAtUnixMicro()}, nil
	case *orvexv1.CrashEvent_BetCashedOut:
		c := ev.BetCashedOut
		return struct {
			Type                    string `json:"type"`
			PlayerID                string `json:"playerId"`
			CashedOutAtUnixMicro    int64  `json:"cashedOutAtUnixMicro,string"`
			CashOutTick             int64  `json:"cashOutTick,string"`
			CashOutMultiplierScaled int64  `json:"cashOutMultiplierScaled,string"`
			PayoutCredits           int64  `json:"payoutCredits,string"`
		}{"BetCashedOut", c.GetPlayerId(), c.GetCashedOutAtUnixMicro(), c.GetCashOutTick(),
			c.GetCashOutMultiplierScaled(), c.GetPayoutCredits()}, nil
	case *orvexv1.CrashEvent_RoundCrashed:
		c := ev.RoundCrashed
		return struct {
			Type                      string `json:"type"`
			ClosedAtUnixMicro         int64  `json:"closedAtUnixMicro,string"`
			CrashPointScaled          int64  `json:"crashPointScaled,string"`
			CrashTick                 int64  `json:"crashTick,string"`
			CrashDeadlineOffsetMicros int64  `json:"crashDeadlineOffsetMicros,string"`
			CrashDeadlineUnixMicro    int64  `json:"crashDeadlineUnixMicro,string"`
		}{"RoundCrashed", c.GetClosedAtUnixMicro(), c.GetCrashPointScaled(), c.GetCrashTick(),
			c.GetCrashDeadlineOffsetMicros(), c.GetCrashDeadlineUnixMicro()}, nil
	case *orvexv1.CrashEvent_BetLost:
		return struct {
			Type       string `json:"type"`
			PlayerID   string `json:"playerId"`
			BetCredits int64  `json:"betCredits,string"`
		}{"BetLost", ev.BetLost.GetPlayerId(), ev.BetLost.GetBetCredits()}, nil
	case *orvexv1.CrashEvent_RoundSettled:
		c := ev.RoundSettled
		if len(c.GetSeed()) != 32 {
			return nil, errEvent
		}
		return struct {
			Type               string `json:"type"`
			SettledAtUnixMicro int64  `json:"settledAtUnixMicro,string"`
			BetCount           int64  `json:"betCount,string"`
			TotalBetCredits    int64  `json:"totalBetCredits,string"`
			TotalPayoutCredits int64  `json:"totalPayoutCredits,string"`
			Seed               string `json:"seed"`
		}{"RoundSettled", c.GetSettledAtUnixMicro(), c.GetBetCount(), c.GetTotalBetCredits(),
			c.GetTotalPayoutCredits(), string(canonjson.Hex(c.GetSeed()))}, nil
	default:
		return nil, fmt.Errorf("%w: %T", errEvent, ev)
	}
}

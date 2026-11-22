//go:build integration && acceptance

package firstround

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/juano-morello/orvex/internal/canonjson"
)

// The public gateway interface the test drives, from the first playable
// gateway design: int64 values are JSON strings (a JSON number is accepted
// too), commands carry a client-chosen clientCommandId, and the player is
// the bearer token's, never a body field.

const requestTimeout = 15 * time.Second

// wireInt is an int64 sent as a decimal JSON string.
type wireInt int64

func (n *wireInt) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("integer %s: %w", b, err)
	}
	*n = wireInt(v)
	return nil
}

// receipt is the 202 body of an admitted command: its durable admission.
type receipt struct {
	ClientCommandID        string  `json:"clientCommandId"`
	RoundID                string  `json:"roundId"`
	Sequence               wireInt `json:"sequence"`
	EffectiveTimeUnixMicro wireInt `json:"effectiveTimeUnixMicro"`
	Duplicate              bool    `json:"duplicate"`
}

// commandStatus is the body of GET /v1/commands/{clientCommandId}.
type commandStatus struct {
	receipt
	Status string      `json:"status"`
	Reason string      `json:"reason"`
	Events []wireEvent `json:"events"`
}

// wireEvent holds the event fields the test reads; the rest are ignored.
type wireEvent struct {
	Type          string  `json:"type"`
	Player        string  `json:"player"`
	BetCredits    wireInt `json:"betCredits"`
	PayoutCredits wireInt `json:"payoutCredits"`
}

type wireError struct {
	Code string `json:"code"`
}

type commandBody struct {
	ClientCommandID string    `json:"clientCommandId"`
	PlaceBet        *placeBet `json:"placeBet,omitempty"`
	CashOut         *struct{} `json:"cashOut,omitempty"`
}

type placeBet struct {
	BetCredits string `json:"betCredits"`
}

func placeBetBody(t *testing.T, clientID string, credits int64) []byte {
	return mustJSON(t, commandBody{ClientCommandID: clientID, PlaceBet: &placeBet{BetCredits: strconv.FormatInt(credits, 10)}})
}

func cashOutBody(t *testing.T, clientID string) []byte {
	return mustJSON(t, commandBody{ClientCommandID: clientID, CashOut: &struct{}{}})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newUUID returns a random version 4 UUID in text form.
func newUUID(t *testing.T) string {
	t.Helper()
	id := randomID(t)
	return string(canonjson.UUID(id))
}

func randomID(t *testing.T) [16]byte {
	t.Helper()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id
}

// player is one public client: a bearer token and a base URL, the gateway's
// or a proxy's.
type player struct {
	name  string
	token string
	base  string
	http  *http.Client
}

func newPlayer(name, token, base string) *player {
	return &player{name: name, token: token, base: base, http: &http.Client{Transport: &http.Transport{}}}
}

// via returns the same player talking to base.
func (p *player) via(base string) *player {
	return &player{name: p.name, token: p.token, base: base, http: &http.Client{Transport: &http.Transport{}}}
}

// call sends one request with the player's token. A transport error is
// returned, not fatal: the lost-response step expects one.
func (p *player) call(method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, r)
	if err != nil {
		return 0, nil, err
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (p *player) mustCall(t *testing.T, method, path string, body []byte) (int, []byte) {
	t.Helper()
	status, b, err := p.call(method, path, body)
	if err != nil {
		t.Fatalf("%s: %s %s: %v", p.name, method, path, err)
	}
	return status, b
}

func (p *player) submit(t *testing.T, round string, body []byte) (int, []byte) {
	t.Helper()
	return p.mustCall(t, http.MethodPost, "/v1/rounds/"+round+"/commands", body)
}

// admit submits body and requires 202 with a receipt for round.
func (p *player) admit(t *testing.T, round string, body []byte) receipt {
	t.Helper()
	status, b := p.submit(t, round, body)
	if status != http.StatusAccepted {
		t.Fatalf("%s: submit %s = %d %s, want 202", p.name, body, status, b)
	}
	return decodeReceipt(t, p.name, round, b)
}

func decodeReceipt(t *testing.T, who, round string, b []byte) receipt {
	t.Helper()
	var r receipt
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("%s: receipt %s: %v", who, b, err)
	}
	if r.RoundID != round || r.Sequence < 1 || r.EffectiveTimeUnixMicro <= 0 {
		t.Fatalf("%s: receipt %s, want round %s, a sequence >= 1 and an effective time", who, b, round)
	}
	return r
}

// requireRefused requires submit to answer status with error code code (any
// code when code is empty).
func (p *player) requireRefused(t *testing.T, round string, body []byte, status int, code string) {
	t.Helper()
	got, b := p.submit(t, round, body)
	var e wireError
	_ = json.Unmarshal(b, &e)
	if got != status || (code != "" && e.Code != code) {
		t.Fatalf("%s: submit %s = %d %s, want %d %q", p.name, body, got, b, status, code)
	}
}

// lookup returns the status code and body of the player's command clientID.
func (p *player) lookup(t *testing.T, clientID string) (int, commandStatus) {
	t.Helper()
	status, b := p.mustCall(t, http.MethodGet, "/v1/commands/"+clientID, nil)
	var s commandStatus
	if status == http.StatusOK {
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatalf("%s: lookup %s: %s: %v", p.name, clientID, b, err)
		}
	}
	return status, s
}

// waitCurrentRound polls GET /v1/rounds/current until it names a round
// accept takes.
func (p *player) waitCurrentRound(t *testing.T, timeout time.Duration, accept func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		status, b, err := p.call(http.MethodGet, "/v1/rounds/current", nil)
		if err == nil && status == http.StatusOK {
			var cur struct {
				RoundID string `json:"roundId"`
			}
			if err := json.Unmarshal(b, &cur); err != nil {
				t.Fatalf("current round %s: %v", b, err)
			}
			if accept(cur.RoundID) {
				return cur.RoundID
			}
		}
		last = fmt.Sprintf("%d %s %v", status, b, err)
		if time.Now().After(deadline) {
			t.Fatalf("%s: GET /v1/rounds/current did not name the expected round within %v; last answer: %s", p.name, timeout, last)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

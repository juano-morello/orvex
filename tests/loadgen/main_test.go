package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProtocol(t *testing.T) {
	requests := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/rounds/round/commands" || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s %v", r.Method, r.URL.Path, r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"sequence":"42","effectiveTimeUnixMicro":"123456"}`))
	}))
	defer server.Close()
	anchor := time.Now().Add(30 * time.Millisecond)
	phases := []message{{Type: "ping", ID: "p", ControllerTime: 123}, {Type: "phase", ID: "bet", Anchor: anchor, BaseURL: server.URL, Round: "round", Attempts: []attempt{{Player: "alice", Token: "token", ClientID: "id1", Kind: "bet", BetCredits: 100}}}, {Type: "phase", ID: "cash", Anchor: anchor, BaseURL: server.URL, Round: "round", Attempts: []attempt{{Player: "alice", Token: "token", ClientID: "id2", Kind: "cashout"}}}}
	var input, output bytes.Buffer
	enc := json.NewEncoder(&input)
	for _, p := range phases {
		if err := enc.Encode(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(&input, &output); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&output)
	var pong map[string]any
	if err := dec.Decode(&pong); err != nil || pong["type"] != "pong" || pong["controller_time"] != float64(123) {
		t.Fatalf("pong %v: %v", pong, err)
	}
	for _, phase := range phases[1:] {
		var got result
		if err := dec.Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.PhaseID != phase.ID || got.Seq != 42 || got.EffectiveUS != 123456 || got.HTTPStatus != 202 || got.ScheduledUS != anchor.UnixMicro() || got.SentUS < got.ScheduledUS || got.ReceivedUS < got.SentUS {
			t.Fatalf("result %+v", got)
		}
		var done map[string]any
		if err := dec.Decode(&done); err != nil || done["type"] != "phase_done" {
			t.Fatalf("done %v: %v", done, err)
		}
	}
	bet, cash := <-requests, <-requests
	if bet["clientCommandId"] != "id1" || bet["placeBet"].(map[string]any)["betCredits"] != "100" || cash["clientCommandId"] != "id2" || cash["cashOut"] == nil {
		t.Fatalf("bodies %v %v", bet, cash)
	}
}
func TestBadPhase(t *testing.T) {
	if err := run(strings.NewReader(`{"type":"phase"}`), &bytes.Buffer{}); err == nil {
		t.Fatal("accepted invalid phase")
	}
}

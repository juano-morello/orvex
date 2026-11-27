//go:build integration && acceptance && spectatorload

package firstround

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type remoteAttempt struct {
	Player     string `json:"player"`
	Token      string `json:"token"`
	ClientID   string `json:"client_id"`
	Kind       string `json:"kind"`
	OffsetUS   int64  `json:"offset_us"`
	BetCredits int64  `json:"bet_credits"`
}
type remoteMessage struct {
	Type           string          `json:"type"`
	ID             string          `json:"id"`
	ControllerTime int64           `json:"controller_time,omitempty"`
	Anchor         time.Time       `json:"anchor,omitempty"`
	BaseURL        string          `json:"base_url,omitempty"`
	Round          string          `json:"round,omitempty"`
	Attempts       []remoteAttempt `json:"attempts,omitempty"`
}
type remoteReply struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	PhaseID        string `json:"phase_id"`
	ControllerTime int64  `json:"controller_time"`
	ReceivedAt     int64  `json:"received_at"`
	SentAt         int64  `json:"sent_at"`
	Count          int    `json:"count"`
	Player         string `json:"player"`
	ClientID       string `json:"client_id"`
	Kind           string `json:"kind"`
	ScheduledUS    int64  `json:"scheduled_us"`
	SentUS         int64  `json:"sent_us"`
	ReceivedUS     int64  `json:"received_us"`
	Seq            int64  `json:"seq"`
	EffectiveUS    int64  `json:"effective_us"`
	HTTPStatus     int    `json:"http_status"`
	Error          string `json:"error"`
}
type burstRemote struct {
	enc      *json.Encoder
	dec      *json.Decoder
	offsetUS int64
	rttUS    int64
}

func startBurstRemote(t *testing.T, launcher string) *burstRemote {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, launcher) //nolint:gosec // operator-provided launcher
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard // launcher diagnostics may contain credentials
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start remote launcher: %v", err)
	}
	t.Cleanup(func() { _ = in.Close(); cancel(); _ = cmd.Wait() })
	remote := &burstRemote{enc: json.NewEncoder(in), dec: json.NewDecoder(out)}
	// Use the least-delay exchange to reduce asymmetry error.
	for i := 0; i < 8; i++ {
		id := fmt.Sprint(i)
		sent := time.Now().UnixMicro()
		if err := remote.enc.Encode(remoteMessage{Type: "ping", ID: id, ControllerTime: sent}); err != nil {
			t.Fatal("remote ping write failed")
		}
		var reply remoteReply
		if err := remote.dec.Decode(&reply); err != nil {
			t.Fatal("remote ping read failed")
		}
		received := time.Now().UnixMicro()
		if reply.Type != "pong" || reply.ID != id || reply.ControllerTime != sent || reply.SentAt < reply.ReceivedAt {
			t.Fatal("invalid remote ping response")
		}
		rtt := received - sent - (reply.SentAt - reply.ReceivedAt)
		if rtt < 0 {
			t.Fatal("invalid remote clock exchange")
		}
		if i == 0 || rtt < remote.rttUS {
			remote.rttUS = rtt
			remote.offsetUS = ((reply.ReceivedAt - sent) + (reply.SentAt - received)) / 2
		}
	}
	return remote
}
func (r *burstRemote) phase(t *testing.T, id, base, round string, anchor time.Time, items []remoteAttempt) []burstAttempt {
	t.Helper()
	msg := remoteMessage{Type: "phase", ID: id, Anchor: anchor.Add(time.Duration(r.offsetUS) * time.Microsecond), BaseURL: base, Round: round, Attempts: items}
	if err := r.enc.Encode(msg); err != nil {
		t.Fatal("remote phase write failed")
	}
	results := make([]burstAttempt, 0, len(items))
	pending := make(map[string]bool, len(items))
	for _, item := range items {
		pending[item.Player+"/"+item.ClientID+"/"+item.Kind] = true
	}
	for {
		var reply remoteReply
		if err := r.dec.Decode(&reply); err != nil {
			t.Fatal("remote phase read failed")
		}
		if reply.Type == "phase_done" {
			if reply.ID != id || reply.Count != len(items) || len(results) != len(items) {
				t.Fatal("invalid remote phase completion")
			}
			break
		}
		if reply.Type != "result" || reply.PhaseID != id || len(results) >= len(items) {
			t.Fatal("invalid remote result")
		}
		key := reply.Player + "/" + reply.ClientID + "/" + reply.Kind
		if !pending[key] {
			t.Fatal("unexpected or duplicate remote result")
		}
		delete(pending, key)
		for _, item := range items {
			if item.Token != "" && strings.Contains(reply.Error, item.Token) {
				reply.Error = "redacted remote error"
				break
			}
		}
		convert := func(v int64) int64 {
			if v == 0 {
				return 0
			}
			return v - r.offsetUS
		}
		results = append(results, burstAttempt{Player: reply.Player, ClientID: reply.ClientID, Kind: reply.Kind, ScheduledUS: convert(reply.ScheduledUS), SentUS: convert(reply.SentUS), ReceivedUS: convert(reply.ReceivedUS), Seq: reply.Seq, EffectiveUS: reply.EffectiveUS, HTTPStatus: reply.HTTPStatus, Error: reply.Error, Provenance: "remote", ClockOffsetUS: r.offsetUS, ClockRTTUS: r.rttUS})
	}
	return results
}

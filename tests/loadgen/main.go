// loadgen executes controller-provided gateway requests over stdin/stdout JSONL.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const requestTimeout = 15 * time.Second

type message struct {
	Type           string    `json:"type"`
	ID             string    `json:"id"`
	ControllerTime int64     `json:"controller_time"`
	Anchor         time.Time `json:"anchor"`
	BaseURL        string    `json:"base_url"`
	Round          string    `json:"round"`
	Attempts       []attempt `json:"attempts"`
}
type attempt struct {
	Player     string `json:"player"`
	Token      string `json:"token"`
	ClientID   string `json:"client_id"`
	Kind       string `json:"kind"`
	OffsetUS   int64  `json:"offset_us"`
	BetCredits int64  `json:"bet_credits"`
}
type result struct {
	Type        string `json:"type"`
	PhaseID     string `json:"phase_id"`
	Player      string `json:"player"`
	ClientID    string `json:"client_id"`
	Kind        string `json:"kind"`
	ScheduledUS int64  `json:"scheduled_us"`
	SentUS      int64  `json:"sent_us"`
	ReceivedUS  int64  `json:"received_us"`
	Seq         int64  `json:"seq"`
	EffectiveUS int64  `json:"effective_us"`
	HTTPStatus  int    `json:"http_status"`
	Error       string `json:"error"`
}
type wireInt int64

func (n *wireInt) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*n = wireInt(v)
	return err
}
func submit(client *http.Client, m message, a attempt, due time.Time) result {
	r := result{Type: "result", PhaseID: m.ID, Player: a.Player, ClientID: a.ClientID, Kind: a.Kind, ScheduledUS: due.UnixMicro()}
	body := map[string]any{"clientCommandId": a.ClientID}
	if a.Kind == "bet" {
		body["placeBet"] = map[string]string{"betCredits": strconv.FormatInt(a.BetCredits, 10)}
	} else {
		body["cashOut"] = struct{}{}
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.BaseURL, "/")+"/v1/rounds/"+m.Round+"/commands", bytes.NewReader(b))
	if err != nil {
		r.Error = "request: " + err.Error()
		return r
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	r.SentUS = time.Now().UnixMicro()
	resp, err := client.Do(req)
	if err != nil {
		r.ReceivedUS = time.Now().UnixMicro()
		r.Error = "transport"
		return r
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	r.ReceivedUS = time.Now().UnixMicro()
	r.HTTPStatus = resp.StatusCode
	if err != nil {
		r.Error = "transport"
		return r
	}
	if resp.StatusCode != http.StatusAccepted {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(b, &e)
		r.Error = e.Code
		return r
	}
	var receipt struct {
		Sequence  wireInt `json:"sequence"`
		Effective wireInt `json:"effectiveTimeUnixMicro"`
	}
	if json.Unmarshal(b, &receipt) != nil || receipt.Sequence <= 0 || receipt.Effective <= 0 {
		r.Error = "malformed receipt"
		return r
	}
	r.Seq = int64(receipt.Sequence)
	r.EffectiveUS = int64(receipt.Effective)
	return r
}
func run(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	enc := json.NewEncoder(out)
	clients := make(map[string]*http.Client)
	phases := 0
	for scanner.Scan() {
		received := time.Now().UnixMicro()
		var m message
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		switch m.Type {
		case "ping":
			if err := enc.Encode(map[string]any{"type": "pong", "id": m.ID, "controller_time": m.ControllerTime, "received_at": received, "sent_at": time.Now().UnixMicro()}); err != nil {
				return err
			}
		case "phase":
			phases++
			if phases > 2 {
				return errors.New("at most two phases permitted")
			}
			if m.ID == "" || m.Anchor.IsZero() || m.BaseURL == "" || m.Round == "" || len(m.Attempts) > 10000 {
				return errors.New("invalid phase")
			}
			now := time.Now()
			for _, a := range m.Attempts {
				due := m.Anchor.Add(time.Duration(a.OffsetUS) * time.Microsecond)
				if a.OffsetUS < -3600000000 || a.OffsetUS > 3600000000 || due.After(now.Add(time.Minute)) || (a.Kind != "bet" && a.Kind != "cashout" && a.Kind != "retry") {
					return errors.New("invalid attempt schedule or kind")
				}
			}
			for _, a := range m.Attempts {
				if clients[a.Player] == nil {
					clients[a.Player] = &http.Client{Transport: &http.Transport{}}
				}
			}
			results := make([]result, len(m.Attempts))
			var wg sync.WaitGroup
			for i, a := range m.Attempts {
				wg.Add(1)
				go func(i int, a attempt) {
					defer wg.Done()
					due := m.Anchor.Add(time.Duration(a.OffsetUS) * time.Microsecond)
					if d := time.Until(due); d > 0 {
						time.Sleep(d)
					}
					results[i] = submit(clients[a.Player], m, a, due)
				}(i, a)
			}
			wg.Wait()
			for _, r := range results {
				if err := enc.Encode(r); err != nil {
					return err
				}
			}
			if err := enc.Encode(map[string]any{"type": "phase_done", "id": m.ID, "count": len(results)}); err != nil {
				return err
			}
		default:
			return errors.New("unknown message type")
		}
	}
	return scanner.Err()
}
func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

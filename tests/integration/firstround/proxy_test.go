//go:build integration && acceptance

package firstround

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// lossyProxy forwards every request to target. The first time the gateway
// answers 202 to the command clientID names, the proxy reads the whole
// response and closes the client's connection without sending any of it:
// the receipt is lost after the admission committed. Every later request
// for clientID is forwarded.
type lossyProxy struct {
	url      string
	target   string
	clientID string
	upstream *http.Client

	mu       sync.Mutex
	requests int
	lost     []byte
}

func newLossyProxy(t *testing.T, target, clientID string) *lossyProxy {
	t.Helper()
	p := &lossyProxy{target: target, clientID: clientID, upstream: &http.Client{Transport: &http.Transport{}}}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

func (p *lossyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.target+r.URL.RequestURI(), bytes.NewReader(body)) //nolint:gosec // G704: target is the gateway this test started
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out.Header = r.Header.Clone()
	resp, err := p.upstream.Do(out) //nolint:gosec // G704: target is the gateway this test started
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	selected := r.Method == http.MethodPost && commandID(body) == p.clientID
	p.mu.Lock()
	if selected {
		p.requests++
	}
	lose := selected && p.lost == nil && resp.StatusCode == http.StatusAccepted
	if lose {
		p.lost = respBody
	}
	p.mu.Unlock()
	if lose {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func commandID(body []byte) string {
	var b struct {
		ClientCommandID string `json:"clientCommandId"`
	}
	_ = json.Unmarshal(body, &b)
	return b.ClientCommandID
}

// lostResponse returns the dropped response body, nil if none was dropped,
// and how many requests for clientID the proxy forwarded.
func (p *lossyProxy) lostResponse() ([]byte, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lost, p.requests
}

//go:build integration && acceptance

package firstround

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Self-tests of the harness. They run without the services, so a scenario
// failure points at a service, not at the proxy, the stream reader or the
// process supervisor.

const helperEnv = "ORVEX_ACCEPTANCE_HELPER"

// runHelper is the child process of TestHarnessSupervisor: the test binary
// re-executed with helperEnv set, never an Orvex service.
func runHelper(mode string) int {
	switch mode {
	case "ready":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		fmt.Println("starting")
		fmt.Println("ready " + ln.Addr().String())
		<-term
		_ = ln.Close()
		return 0
	case "exit":
		fmt.Fprintln(os.Stderr, "helper: no configuration")
		return 3
	default:
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		<-term
		return 0
	}
}

func TestHarnessSupervisor(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("ready", func(t *testing.T) {
		s, err := launch("helper", exe, nil, []string{helperEnv + "=ready"}, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("tcp", s.addr)
		if err != nil {
			t.Fatalf("dial the ready address %s: %v", s.addr, err)
		}
		_ = conn.Close()
		if code, err := s.stop(); err != nil || code != 0 {
			t.Fatalf("stop = %d, %v; want exit 0 on SIGTERM", code, err)
		}
		if code, err := s.stop(); err != nil || code != 0 {
			t.Fatalf("second stop = %d, %v; want the same exit 0", code, err)
		}
		if out := s.log.String(); !strings.Contains(out, "stdout: starting\n") {
			t.Errorf("log %q lacks the line before ready", out)
		}
	})
	t.Run("exits-before-ready", func(t *testing.T) {
		_, err := launch("helper", exe, nil, []string{helperEnv + "=exit"}, 30*time.Second)
		if err == nil || !strings.Contains(err.Error(), "exited with status 3") || !strings.Contains(err.Error(), "helper: no configuration") {
			t.Fatalf("launch error %v, want the exit status and the child's stderr", err)
		}
	})
	t.Run("never-ready", func(t *testing.T) {
		_, err := launch("helper", exe, nil, []string{helperEnv + "=mute"}, 300*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "no \"ready <address>\" line") {
			t.Fatalf("launch error %v, want a readiness timeout", err)
		}
	})
}

func TestHarnessLossyProxy(t *testing.T) {
	var mu sync.Mutex
	served := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		served[commandID(b)]++
		n := served[commandID(b)]
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"n":%d}`, n)
	}))
	defer upstream.Close()
	proxy := newLossyProxy(t, upstream.URL, "lost-id")
	c := newPlayer("client", "tok", proxy.url)

	if status, b, err := c.call(http.MethodPost, "/x", []byte(`{"clientCommandId":"other"}`)); err != nil || status != http.StatusAccepted || string(b) != `{"n":1}` {
		t.Fatalf("unselected request = %d %s %v, want it forwarded", status, b, err)
	}
	if status, b, err := c.call(http.MethodPost, "/x", []byte(`{"clientCommandId":"lost-id"}`)); err == nil {
		t.Fatalf("selected request = %d %s, want a transport error", status, b)
	}
	if lost, n := proxy.lostResponse(); string(lost) != `{"n":1}` || n != 1 {
		t.Fatalf("lost %q after %d requests, want the upstream's first response", lost, n)
	}
	if status, b, err := c.call(http.MethodPost, "/x", []byte(`{"clientCommandId":"lost-id"}`)); err != nil || string(b) != `{"n":2}` {
		t.Fatalf("retry = %d %s %v, want the upstream's second response", status, b, err)
	}
	if _, n := proxy.lostResponse(); n != 2 {
		t.Errorf("proxy counted %d selected requests, want 2", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if served["lost-id"] != 2 || served["other"] != 1 {
		t.Errorf("upstream served %v, want lost-id twice and other once", served)
	}
}

// sseServer serves one round stream from scripted connections. Each
// connection gets the next script; the script sees the Last-Event-ID.
func sseServer(t *testing.T, scripts ...func(lastID string) string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := len(seen)
		seen = append(seen, r.Header.Get("Authorization")+" "+r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		if r.URL.Path != "/v1/rounds/R/stream" || n >= len(scripts) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, scripts[n](r.Header.Get("Last-Event-ID")))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func recordFrame(seq int, events string) string {
	return fmt.Sprintf("id: %d\nevent: record\ndata: {\"sequence\":\"%d\",\ndata: \"events\":[%s]}\n\n", seq, seq, events)
}

func TestHarnessStreamResumes(t *testing.T) {
	srv, seen := sseServer(t,
		func(string) string {
			return recordFrame(0, "") + recordFrame(1, `{"type":"BetPlaced","player":"alice","betCredits":"100"}`) + recordFrame(2, "")
		},
		func(string) string {
			return ": ping\n\n" + recordFrame(1, "") + recordFrame(2, "") + recordFrame(3, `{"type":"RoundSettled"}`) + "event: end\ndata: {}\n\n"
		},
	)
	w := watch(t, newPlayer("alice", "tok", srv.URL), "R")
	frames := w.waitEnd(t, 10*time.Second)
	if len(frames) != 4 || !frames[1].has("BetPlaced") || int64(frames[1].Events[0].BetCredits) != 100 || !frames[3].has("RoundSettled") {
		t.Fatalf("frames:\n%s", describeFrames(frames))
	}
	if reconnects, repeats := w.stats(); reconnects != 1 || repeats != 2 {
		t.Errorf("reconnects %d repeats %d, want 1 and 2", reconnects, repeats)
	}
	if got := strings.Join(*seen, ","); got != "Bearer tok ,Bearer tok 2" {
		t.Errorf("requests %q, want the token on both and Last-Event-ID 2 on the reconnect", got)
	}
}

func TestHarnessStreamRejectsGap(t *testing.T) {
	srv, _ := sseServer(t, func(string) string { return recordFrame(0, "") + recordFrame(2, "") })
	w := watch(t, newPlayer("alice", "tok", srv.URL), "R")
	select {
	case <-w.done:
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not stop on a gap")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failure == nil || !strings.Contains(w.failure.Error(), "gap") {
		t.Fatalf("failure %v, want a gap violation", w.failure)
	}
}

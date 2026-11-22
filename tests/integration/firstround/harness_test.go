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
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Self-tests of the harness. They run without the services, so a scenario
// failure points at a service, not at the proxy, the stream reader or the
// process supervisor.

const (
	helperEnv = "ORVEX_ACCEPTANCE_HELPER"
	// decoyFileEnv names the file a decoy helper prints when it stops.
	decoyFileEnv = "ORVEX_ACCEPTANCE_DECOY_FILE"
)

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
	case "decoy":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		fmt.Println("ready " + ln.Addr().String())
		<-term
		_ = ln.Close()
		b, err := os.ReadFile(os.Getenv(decoyFileEnv)) //nolint:gosec // G703: the file the parent test named
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_, _ = os.Stderr.Write(b)
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
			return recordFrame(0, "") + recordFrame(1, `{"type":"BetPlaced","playerId":"alice","betCredits":"100"}`) + recordFrame(2, "")
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

// TestHarnessSmokeBet drives placeSmokeBet against a scripted gateway. The
// first round is sealed, the second loses the response to the first
// submission and then rejects the admitted bet with wrong_phase, and the
// third answers 503 once before it accepts. A new round must get a fresh
// clientCommandId and an uncertain delivery the same one.
func TestHarnessSmokeBet(t *testing.T) {
	receiptStatus := contract(t).receiptStatus
	var (
		mu       sync.Mutex
		submits  []string
		current  int
		lookups  = map[string]int{}
		idOf     = map[string]string{}
		perRound = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		admitted := func(round, seq string) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprintf(w, `{"roundId":%q,"sequence":%q,"effectiveTimeUnixMicro":"1000","duplicate":false,"status":%q}`, round, seq, receiptStatus)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/rounds/current":
			current++
			round := "R1"
			if current > 2 {
				round = "R2"
			}
			_, _ = fmt.Fprintf(w, `{"roundId":%q}`, round)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			round := strings.Split(r.URL.Path, "/")[3]
			b, _ := io.ReadAll(r.Body)
			id := commandID(b)
			submits = append(submits, round+" "+id)
			idOf[round] = id
			perRound[round]++
			switch n := perRound[round]; {
			case round == "R0":
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"code":"round_sealed"}`)
			case round == "R1" && n == 1:
				if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
					_ = conn.Close()
				}
			case round == "R1":
				admitted("R1", "5")
			case round == "R2" && n == 1:
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"code":"not_admitted"}`)
			default:
				admitted("R2", "3")
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/commands/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/commands/")
			lookups[id]++
			switch {
			case id == idOf["R1"] && lookups[id] == 1:
				_, _ = io.WriteString(w, `{"roundId":"R1","sequence":"5","status":"pending","events":[]}`)
			case id == idOf["R1"]:
				_, _ = io.WriteString(w, `{"roundId":"R1","sequence":"5","status":"rejected","reason":"wrong_phase","events":[]}`)
			case id == idOf["R2"]:
				_, _ = io.WriteString(w, `{"roundId":"R2","sequence":"3","status":"accepted","events":[{"type":"BetPlaced","playerId":"alice","betCredits":"100"}]}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	round, r, attempts := placeSmokeBet(t, newPlayer("alice", "tok", srv.URL), 100, "R0", 30*time.Second)
	if round != "R2" || r.Sequence != 3 {
		t.Fatalf("smoke bet landed in %s at sequence %d, want R2 at 3", round, r.Sequence)
	}
	mu.Lock()
	defer mu.Unlock()
	a, b, c := idOf["R0"], idOf["R1"], idOf["R2"]
	if a == b || b == c || a == c {
		t.Fatalf("clientCommandIds %s %s %s, want a fresh one per round", a, b, c)
	}
	want := []betAttempt{{"R0", a, 1, "round_sealed"}, {"R1", b, 2, "wrong_phase"}, {"R2", c, 2, "accepted"}}
	if !slices.Equal(attempts, want) {
		t.Errorf("attempts %+v, want %+v", attempts, want)
	}
	if got, want := strings.Join(submits, ","), strings.Join([]string{"R0 " + a, "R1 " + b, "R1 " + b, "R2 " + c, "R2 " + c}, ","); got != want {
		t.Errorf("submissions %s, want %s: the identical request after an uncertain delivery", got, want)
	}
}

// TestHarnessLeakDetector is the negative control of the secret check: a
// test-only process prints a seed only while it stops, after any snapshot
// of its log taken while it ran, in each form the check looks for, and
// without a final newline.
func TestHarnessLeakDetector(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var seed [32]byte
	a, b := randomID(t), randomID(t)
	copy(seed[:16], a[:])
	copy(seed[16:], b[:])
	s := seedSecret("the seed", seed)
	for i, form := range s.forms {
		file := writeFile(t, t.TempDir(), "decoy", "round started; seed "+form)
		decoy, err := launch("decoy", exe, nil, []string{helperEnv + "=decoy", decoyFileEnv + "=" + file}, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		running := decoy.log.String()
		if code, err := decoy.stop(); err != nil || code != 0 {
			t.Fatalf("decoy stop = %d, %v", code, err)
		}
		if got := findLeaks([]output{{"running", running}}, []secret{s}); got != nil {
			t.Fatalf("form %d: the decoy printed the seed before it stopped: %q", i, got)
		}
		if got := findLeaks([]output{{"decoy", decoy.log.String()}}, []secret{s}); !slices.Equal(got, []string{"decoy carries the seed"}) {
			t.Errorf("form %d (%s): leaks %q in the complete log %q, want the seed", i, form, got, decoy.log.String())
		}
	}
	if got := findLeaks([]output{{"clean", "stdout: ready 127.0.0.1:1\n"}}, []secret{s}); got != nil {
		t.Errorf("clean log leaks %q", got)
	}
}

// TestHarnessFinalCursor checks the client's side of a resumed stream at a
// round's last record: the watcher stops on 204, and requireFinalCursorStops
// takes each contract's stop.
func TestHarnessFinalCursor(t *testing.T) {
	var mu sync.Mutex
	var lastIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		n := len(lastIDs)
		mu.Unlock()
		switch {
		case r.URL.Path == "/v1/rounds/R/stream" && n == 1:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, recordFrame(0, "")+recordFrame(1, `{"type":"RoundSettled"}`))
		case r.URL.Path == "/v1/rounds/R/stream":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/rounds/B/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "retry: 1000\n\nevent: end\ndata: {}\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := newPlayer("alice", "tok", srv.URL)

	frames := watch(t, p, "R").waitEnd(t, 10*time.Second)
	if len(frames) != 2 {
		t.Fatalf("frames:\n%s", describeFrames(frames))
	}
	mu.Lock()
	if got := strings.Join(lastIDs, ","); got != ",1" {
		t.Errorf("Last-Event-IDs %q, want none then 1", got)
	}
	mu.Unlock()
	t.Run("current", func(t *testing.T) {
		t.Setenv(wireContractEnv, "")
		requireFinalCursorStops(t, p, "R", 1)
	})
	t.Run("baseline", func(t *testing.T) {
		t.Setenv(wireContractEnv, baselineContract.name)
		requireFinalCursorStops(t, p, "B", 1)
	})
}

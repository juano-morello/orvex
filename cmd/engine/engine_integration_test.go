//go:build integration

package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// syncBuffer collects stderr from the engine's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// served is what the fake engine API server was given.
type served struct {
	pool  *pgxpool.Pool
	seq   *admission.Sequencer
	token string
}

// greeter is a fake engine API server: it reports its arguments, greets
// every connection on ln with "engine-api", and returns nil once ctx ends.
func greeter(got chan<- served) serveFunc {
	return func(ctx context.Context, ln net.Listener, pool *pgxpool.Pool, seq *admission.Sequencer, token string) error {
		got <- served{pool: pool, seq: seq, token: token}
		go func() {
			<-ctx.Done()
			_ = ln.Close()
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			_, _ = io.WriteString(conn, "engine-api\n")
			_ = conn.Close()
		}
	}
}

// engine is one engine process run in-process.
type engine struct {
	cancel context.CancelFunc
	code   chan int
	addr   string
	stdout *io.PipeReader
	stderr *syncBuffer
}

// startEngine runs the engine with env and serve, and waits for its ready
// line.
func startEngine(t *testing.T, env map[string]string, serve serveFunc) *engine {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r, w := io.Pipe()
	e := &engine{cancel: cancel, code: make(chan int, 1), stdout: r, stderr: &syncBuffer{}}
	go func() {
		code := run(ctx, nil, lookupIn(env), w, e.stderr, serve)
		_ = w.Close()
		e.code <- code
	}()
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		cancel()
		t.Fatalf("no ready line: %v; stderr:\n%s", err, e.stderr)
	}
	addr, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "ready ")
	if !ok {
		cancel()
		t.Fatalf("first stdout line %q, want ready <address>", line)
	}
	e.addr = addr
	go func() { _, _ = io.Copy(io.Discard, r) }()
	return e
}

// stop cancels the engine and returns its exit code.
func (e *engine) stop(t *testing.T) int {
	t.Helper()
	e.cancel()
	select {
	case code := <-e.code:
		return code
	case <-time.After(30 * time.Second):
		t.Fatal("the engine did not stop within 30s")
		return -1
	}
}

func dbEnv(t *testing.T, dsn string) map[string]string {
	t.Helper()
	return map[string]string{
		databaseURLEnv:   dsn,
		keyringFileEnv:   pgtest.KeyringFile(t),
		addrEnv:          "127.0.0.1:0",
		tokenFileEnv:     writeFile(t, "token", testToken+"\n", 0o600),
		bettingWindowEnv: "1h",
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestEngineServesAndAdoptsAfterRestart: the engine listens on an ephemeral
// port, hands its listener, pool, sequencer and the gateway token to the
// engine API server, creates a round and claims it. Restarted, it adopts
// the same round instead of creating another. No output carries the
// database URL or the token.
func TestEngineServesAndAdoptsAfterRestart(t *testing.T) {
	dsn := pgtest.MigratedDatabase(t)
	pool := pgtest.Pool(t, dsn)
	env := dbEnv(t, dsn)
	var outputs []string

	for restart, wantToken := range []int64{1, 2} {
		got := make(chan served, 1)
		e := startEngine(t, env, greeter(got))
		if !strings.HasPrefix(e.addr, "127.0.0.1:") || strings.HasSuffix(e.addr, ":0") {
			t.Errorf("ready address %q, want 127.0.0.1 and the bound port", e.addr)
		}
		s := <-got
		if s.pool == nil || s.seq == nil || s.token != testToken {
			t.Errorf("serve got pool %v, sequencer %v, token %q; want both and the token file's token", s.pool != nil, s.seq != nil, s.token)
		}
		conn, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatalf("dial %s: %v", e.addr, err)
		}
		greeting, err := bufio.NewReader(conn).ReadString('\n')
		_ = conn.Close()
		if err != nil || greeting != "engine-api\n" {
			t.Errorf("greeting %q, %v; want the engine API server's", greeting, err)
		}

		waitFor(t, "the round's claim", func() bool {
			return count(t, pool, `SELECT count(*) FROM orvex.round_execution_heads
				WHERE owner_id IS NOT NULL AND fencing_token = $1`, wantToken) == 1
		})
		time.Sleep(200 * time.Millisecond)
		if n := count(t, pool, `SELECT count(*) FROM orvex.rounds`); n != 1 {
			t.Errorf("run %d: %d rounds, want 1", restart+1, n)
		}
		if code := e.stop(t); code != exitOK {
			t.Errorf("run %d: exit code %d, want 0; stderr:\n%s", restart+1, code, e.stderr)
		}
		outputs = append(outputs, e.stderr.String())
	}
	if !strings.Contains(outputs[0], "round created") || strings.Contains(outputs[1], "round created") {
		t.Errorf("the first run must create the round and the restart must not:\n%s\n---\n%s", outputs[0], outputs[1])
	}
	for _, out := range outputs {
		for _, secret := range []string{dsn, "orvex:orvex@", testToken} {
			if strings.Contains(out, secret) {
				t.Errorf("stderr carries a secret:\n%s", out)
			}
		}
	}
}

// TestEngineCreateRoundsOff: with ORVEX_CREATE_ROUNDS=false the engine
// leads and creates nothing.
func TestEngineCreateRoundsOff(t *testing.T) {
	dsn := pgtest.MigratedDatabase(t)
	pool := pgtest.Pool(t, dsn)
	env := dbEnv(t, dsn)
	env[createRoundsEnv] = "false"
	e := startEngine(t, env, greeter(make(chan served, 1)))
	waitFor(t, "the scheduler to lead", func() bool { return strings.Contains(e.stderr.String(), "scheduler leads") })
	time.Sleep(300 * time.Millisecond)
	if n := count(t, pool, `SELECT count(*) FROM orvex.rounds`); n != 0 {
		t.Errorf("%d rounds, want 0", n)
	}
	if code := e.stop(t); code != exitOK {
		t.Errorf("exit code %d, want 0", code)
	}
}

// TestEngineServerFailureStopsEngine: when the engine API server fails,
// the engine stops the scheduler and exits 1, naming the failed part.
func TestEngineServerFailureStopsEngine(t *testing.T) {
	dsn := pgtest.MigratedDatabase(t)
	e := startEngine(t, dbEnv(t, dsn), func(context.Context, net.Listener, *pgxpool.Pool, *admission.Sequencer, string) error {
		return errors.New("listener broke")
	})
	select {
	case code := <-e.code:
		if code != exitFailed {
			t.Errorf("exit code %d, want 1", code)
		}
	case <-time.After(30 * time.Second):
		e.cancel()
		t.Fatal("the engine kept running after its server failed")
	}
	if out := e.stderr.String(); !strings.Contains(out, `component="engine API" error="listener broke"`) {
		t.Errorf("stderr lacks the failure:\n%s", out)
	}
}

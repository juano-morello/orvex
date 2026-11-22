package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juano-morello/orvex/internal/buildinfo"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// secretFile writes content to a new 0600 file and returns its path.
func secretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func validEnv(t *testing.T) map[string]string {
	return map[string]string{
		"ORVEX_GATEWAY_ADDR":       "127.0.0.1:0",
		"ORVEX_ENGINE_ADDR":        "127.0.0.1:1",
		"ORVEX_GATEWAY_TOKEN_FILE": secretFile(t, strings.Repeat("s", 40)+"\n"),
		"ORVEX_PLAYERS_FILE":       secretFile(t, `{"alice-token-0001": "alice"}`),
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string
		wantStderr string // substring; empty means stderr must be empty
		stderrAny  bool   // stderr must be non-empty, content unchecked
	}{
		{name: "version", args: []string{"-version"}, wantCode: 0, wantStdout: name + " " + buildinfo.Version + "\n"},
		{name: "no config", args: nil, wantCode: 2, wantStderr: "set ORVEX_GATEWAY_TOKEN_FILE and ORVEX_PLAYERS_FILE"},
		{name: "positional", args: []string{"extra"}, wantCode: 2, wantStderr: "unexpected arguments"},
		{name: "unknown flag", args: []string{"-bogus"}, wantCode: 2, stderrAny: true},
		{name: "help", args: []string{"-h"}, wantCode: 0, stderrAny: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, envOf(tt.env), &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			switch {
			case tt.stderrAny:
				if stderr.Len() == 0 {
					t.Error("stderr is empty, want output")
				}
			case tt.wantStderr == "":
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want empty", stderr.String())
				}
			case !strings.Contains(stderr.String(), tt.wantStderr):
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestRunWriteError(t *testing.T) {
	if code := run(context.Background(), []string{"-version"}, envOf(nil), failingWriter{}, io.Discard); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunRefusesBadConfig(t *testing.T) {
	token := strings.Repeat("s", 40)
	for name, tc := range map[string]struct {
		key, value, wantStderr string
	}{
		"short token":          {"ORVEX_GATEWAY_TOKEN_FILE", "", "ORVEX_GATEWAY_TOKEN_FILE"},
		"bad players file":     {"ORVEX_PLAYERS_FILE", `{"alice-token-0001": "Alice"}`, "ORVEX_PLAYERS_FILE"},
		"remote engine":        {"ORVEX_ENGINE_ADDR", "10.1.2.3:7001", "loopback"},
		"unusable listen addr": {"ORVEX_GATEWAY_ADDR", "127.0.0.1:99999", "listen"},
	} {
		t.Run(name, func(t *testing.T) {
			env := validEnv(t)
			switch tc.key {
			case "ORVEX_GATEWAY_TOKEN_FILE":
				env[tc.key] = secretFile(t, token[:20])
			case "ORVEX_PLAYERS_FILE":
				env[tc.key] = secretFile(t, tc.value)
			default:
				env[tc.key] = tc.value
			}
			var stderr bytes.Buffer
			code := run(context.Background(), nil, envOf(env), io.Discard, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("exit %d, stderr %q; want failure naming %q", code, stderr.String(), tc.wantStderr)
			}
			if strings.Contains(stderr.String(), token[:20]) || strings.Contains(stderr.String(), "alice-token") {
				t.Errorf("stderr %q quotes a secret", stderr.String())
			}
		})
	}
}

// TestRunServesUntilCancelled starts the gateway with no engine running:
// it prints its address, answers 503 while the engine is unreachable and
// exits 0 when its context ends.
func TestRunServesUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdoutR, stdoutW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, nil, envOf(validEnv(t)), stdoutW, &stderr)
		_ = stdoutW.Close()
	}()
	line, err := bufio.NewReader(stdoutR).ReadString('\n')
	if err != nil {
		t.Fatalf("read ready line: %v (stderr %q)", err, stderr.String())
	}
	addr, ok := strings.CutPrefix(strings.TrimSpace(line), "ready ")
	if !ok || strings.HasSuffix(addr, ":0") {
		t.Fatalf("stdout line %q, want ready <address>", line)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + addr + "/v1/rounds/current")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET current with no engine = %d, want 503", resp.StatusCode)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("gateway did not stop after cancel")
	}
}

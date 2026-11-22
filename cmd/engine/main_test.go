package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// noServe fails the test if the engine gets as far as serving.
func noServe(t *testing.T) serveFunc {
	return func(context.Context, net.Listener, *pgxpool.Pool, *admission.Sequencer, string) error {
		t.Error("serve called")
		return nil
	}
}

func lookupIn(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string // substring; empty means stderr must be empty
		stderrAny  bool   // stderr must be non-empty, content unchecked
	}{
		{name: "version", args: []string{"-version"}, wantCode: 0, wantStdout: name + " " + buildinfo.Version + "\n"},
		{name: "positional", args: []string{"extra"}, wantCode: 2, wantStderr: "unexpected arguments"},
		{name: "unknown flag", args: []string{"-bogus"}, wantCode: 2, stderrAny: true},
		{name: "help", args: []string{"-h"}, wantCode: 0, wantStderr: "ORVEX_ENGINE_ADDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, lookupIn(nil), &stdout, &stderr, noServe(t))
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
	if code := run(context.Background(), []string{"-version"}, lookupIn(nil), failingWriter{}, io.Discard, noServe(t)); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunWithoutServer(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), nil, lookupIn(nil), io.Discard, &stderr, nil); code != 1 || !strings.Contains(stderr.String(), "not linked") {
		t.Errorf("exit code = %d, stderr %q; want 1 and not linked", code, stderr.String())
	}
}

// writeFile writes content to a new file with mode perm and returns its
// path.
func writeFile(t *testing.T, name, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	chmod(t, path, perm)
	return path
}

// chmod sets the mode exactly, whatever the umask.
func chmod(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

const (
	testDSN   = "postgres://engine:s3cret-pass@127.0.0.1:1/orvex?sslmode=disable&connect_timeout=2" //nolint:gosec // G101: a fake password the tests require never to be printed
	testToken = "gateway-token-0123456789"
)

// validEnv returns an environment that passes every check before the
// database connection; its database is unreachable.
func validEnv(t *testing.T) map[string]string {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	return map[string]string{
		databaseURLEnv: testDSN,
		keyringFileEnv: writeFile(t, "keyring", "active k1\nkey k1 "+key+"\n", 0o600),
		addrEnv:        "127.0.0.1:0",
		tokenFileEnv:   writeFile(t, "token", testToken+"\n", 0o600),
	}
}

// TestRunRejectsConfig: each invalid variable stops the engine before it
// connects, naming the variable and never echoing a secret.
func TestRunRejectsConfig(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(t *testing.T, env map[string]string)
		want string
	}{
		"no database URL":  {func(_ *testing.T, e map[string]string) { delete(e, databaseURLEnv) }, "ORVEX_DATABASE_URL is not set"},
		"bad database URL": {func(_ *testing.T, e map[string]string) { e[databaseURLEnv] = "postgres://u:s3cret-pass@[::1" }, "invalid ORVEX_DATABASE_URL"},
		"no keyring":       {func(_ *testing.T, e map[string]string) { delete(e, keyringFileEnv) }, "ORVEX_SECRET_KEYRING_FILE is not set"},
		"keyring too open": {func(t *testing.T, e map[string]string) { chmod(t, e[keyringFileEnv], 0o644) }, "ORVEX_SECRET_KEYRING_FILE: permissions too open"},
		"no address":       {func(_ *testing.T, e map[string]string) { e[addrEnv] = "" }, "ORVEX_ENGINE_ADDR is not set"},
		"no token file":    {func(_ *testing.T, e map[string]string) { delete(e, tokenFileEnv) }, "ORVEX_GATEWAY_TOKEN_FILE is not set"},
		"token too open":   {func(t *testing.T, e map[string]string) { chmod(t, e[tokenFileEnv], 0o604) }, "ORVEX_GATEWAY_TOKEN_FILE: permissions too open"},
		"token missing": {func(t *testing.T, e map[string]string) {
			e[tokenFileEnv] = filepath.Join(t.TempDir(), "absent")
		}, "ORVEX_GATEWAY_TOKEN_FILE: unreadable"},
		"token empty": {func(t *testing.T, e map[string]string) {
			e[tokenFileEnv] = writeFile(t, "empty", " \n", 0o600)
		}, "ORVEX_GATEWAY_TOKEN_FILE: empty"},
		"token with space": {func(t *testing.T, e map[string]string) {
			e[tokenFileEnv] = writeFile(t, "spaced", "two words\n", 0o600)
		}, "ORVEX_GATEWAY_TOKEN_FILE: the token must be printable ASCII"},
		"token directory": {func(t *testing.T, e map[string]string) { e[tokenFileEnv] = t.TempDir() }, "ORVEX_GATEWAY_TOKEN_FILE: not a regular file"},
		"bad window":      {func(_ *testing.T, e map[string]string) { e[bettingWindowEnv] = "ten" }, "ORVEX_BETTING_WINDOW is not a Go duration"},
		"negative window": {func(_ *testing.T, e map[string]string) { e[bettingWindowEnv] = "-1s" }, "ORVEX_BETTING_WINDOW is outside"},
		"long window":     {func(_ *testing.T, e map[string]string) { e[bettingWindowEnv] = "2h" }, "ORVEX_BETTING_WINDOW is outside"},
		"bad create flag": {func(_ *testing.T, e map[string]string) { e[createRoundsEnv] = "sometimes" }, "ORVEX_CREATE_ROUNDS is not true or false"},
	} {
		t.Run(name, func(t *testing.T) {
			env := validEnv(t)
			c.edit(t, env)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), nil, lookupIn(env), &stdout, &stderr, noServe(t))
			if code != exitUsage || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("exit code %d, stderr %q; want 2 and %q", code, stderr.String(), c.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			requireNoSecrets(t, stderr.String())
		})
	}
}

// TestRunUnreachableDatabase: a database that refuses the connection fails
// the engine without printing the URL, its host or its password.
func TestRunUnreachableDatabase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code := run(ctx, nil, lookupIn(validEnv(t)), &stdout, &stderr, noServe(t))
	if code != exitFailed || !strings.Contains(stderr.String(), "cannot connect to the database named by ORVEX_DATABASE_URL") {
		t.Errorf("exit code %d, stderr %q; want 1 and cannot connect", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want no ready line", stdout.String())
	}
	requireNoSecrets(t, stderr.String())
	if strings.Contains(stderr.String(), "127.0.0.1:1") || strings.Contains(stderr.String(), "engine@") {
		t.Errorf("stderr %q names the database host or user", stderr.String())
	}
}

func requireNoSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range []string{"s3cret-pass", testToken} {
		if strings.Contains(out, s) {
			t.Errorf("output %q carries a secret", out)
		}
	}
}

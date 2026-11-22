package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/runtime/grant"
)

func grantCmd(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"ledger", "grant"}, args...), strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// requireOneGrantLine fails unless stderr is exactly one crashctl: ledger
// grant: line.
func requireOneGrantLine(t *testing.T, stderr string) {
	t.Helper()
	if !strings.HasPrefix(stderr, "crashctl: ledger grant: ") || strings.Count(stderr, "\n") != 1 ||
		!strings.HasSuffix(stderr, "\n") {
		t.Errorf("stderr = %q, want one crashctl: ledger grant: line", stderr)
	}
}

// TestGrantUsage: every malformed invocation exits 2 before connecting. The
// database URL names a port nothing listens on, so a connection attempt
// would surface as a connect error.
func TestGrantUsage(t *testing.T) {
	t.Setenv(databaseURLEnv, unreachableDSN)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", []string{"ledger"}, "usage: crashctl"},
		{"unknown subcommand", []string{"ledger", "transfer"}, `crashctl: ledger: unknown subcommand "transfer"`},
		{"no operands", []string{"ledger", "grant"}, "want GRANT_KEY, PLAYER_ID and CREDITS, got 0 arguments"},
		{"two operands", []string{"ledger", "grant", "k", "alice"}, "got 2 arguments"},
		{"four operands", []string{"ledger", "grant", "k", "alice", "5", "extra"}, "got 4 arguments"},
		{"unknown flag", []string{"ledger", "grant", "-force", "k", "alice", "5"}, "flag provided but not defined"},
		{"bad grant key", []string{"ledger", "grant", "Alice 1", "alice", "5"}, "invalid grant: the grant key must be"},
		{"empty player", []string{"ledger", "grant", "k", "", "5"}, "invalid grant: the player ID must be"},
		{"control character in player", []string{"ledger", "grant", "k", "ali\nce", "5"}, "the player ID must be"},
		{"zero credits", []string{"ledger", "grant", "k", "alice", "0"}, "invalid grant: credits must be"},
		{"negative credits", []string{"ledger", "grant", "k", "alice", "-1"}, "credits must be"},
		{"leading zero", []string{"ledger", "grant", "k", "alice", "01"}, "credits must be"},
		{"signed credits", []string{"ledger", "grant", "k", "alice", "+5"}, "credits must be"},
		{"over the cap", []string{"ledger", "grant", "k", "alice", "1000000000001"}, "from 1 to 1000000000000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, strings.NewReader(""), &stdout, &stderr)
			if code != exitUsage || stdout.Len() != 0 {
				t.Errorf("exit code %d, stdout %q; want %d and empty", code, stdout.String(), exitUsage)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.want)
			}
			if strings.Contains(stderr.String(), "cannot connect") || strings.Contains(stderr.String(), "rerun") {
				t.Errorf("stderr = %q, want no connection attempt", stderr.String())
			}
		})
	}
}

func TestGrantHelp(t *testing.T) {
	code, stdout, stderr := grantCmd("-h")
	if code != exitOK || stdout != "" || stderr != grantUsage {
		t.Errorf("exit code %d, stdout %q, stderr %q; want 0, empty and the grant usage", code, stdout, stderr)
	}
}

func TestGrantDatabaseURL(t *testing.T) {
	for _, c := range []struct {
		name string
		dsn  string
		code int
		want string
	}{
		{"unset", "", exitUsage, "crashctl: ledger grant: ORVEX_DATABASE_URL is not set\n"},
		{"invalid", "postgres://u:p@127.0.0.1:1/db?connect_timeout=x", exitUsage, "crashctl: ledger grant: invalid ORVEX_DATABASE_URL\n"},
		{"unreachable", unreachableDSN, exitIO,
			"crashctl: ledger grant: cannot connect to the database named by ORVEX_DATABASE_URL; rerunning the same command is safe\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(databaseURLEnv, c.dsn)
			code, stdout, stderr := grantCmd("alice-1", "alice", "500")
			if code != c.code || stdout != "" || stderr != c.want {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d, empty and %q", code, stdout, stderr, c.code, c.want)
			}
		})
	}
}

// TestGrantNeverPrintsDatabaseURL: no output of ledger grant, whatever the
// connection failure, holds any part of ORVEX_DATABASE_URL (LBT-21 K-12).
func TestGrantNeverPrintsDatabaseURL(t *testing.T) {
	for name, dsn := range sentinelDSNs {
		t.Run(name, func(t *testing.T) {
			t.Setenv(databaseURLEnv, dsn)
			code, stdout, stderr := grantCmd("alice-1", "alice", "500")
			if code != exitUsage && code != exitIO {
				t.Errorf("exit code = %d, want %d or %d (stderr %q)", code, exitUsage, exitIO, stderr)
			}
			requireOneGrantLine(t, stderr)
			requireNoSentinel(t, "stdout", stdout, dsn)
			requireNoSentinel(t, "stderr", stderr, dsn)
		})
	}
}

func TestGrantConnConfig(t *testing.T) {
	for dsn, want := range map[string]string{
		"postgres://u:p@127.0.0.1:1/db":                    "crashctl",
		"postgres://u:p@127.0.0.1:1/db?application_name=x": "x",
	} {
		t.Setenv(databaseURLEnv, dsn)
		cfg, err := databaseConfig()
		if err != nil {
			t.Fatal(err)
		}
		grantConnConfig(cfg)
		if got := cfg.RuntimeParams["application_name"]; got != want {
			t.Errorf("%s: application_name %q, want %q", dsn, got, want)
		}
	}
}

func TestGrantReport(t *testing.T) {
	g, err := grant.Parse("alice-1", "alice", "500")
	if err != nil {
		t.Fatal(err)
	}
	res := grant.Result{
		TransactionID: ledger.GrantTransactionID("alice-1"),
		AccountID:     [16]byte{0x01, 0x90, 15: 0x07},
		Status:        grant.Existing,
	}
	want := "grant alice-1\n" +
		"transaction 53ec0f7c-e218-87e8-93af-ece6f536e6be\n" +
		"player alice\n" +
		"account 01900000-0000-0000-0000-000000000007\n" +
		"credits 500\n" +
		"status existing\n" +
		"ok\n"
	if got := string(grantReport(g, res)); got != want {
		t.Errorf("report\n%s\nwant\n%s", got, want)
	}
}

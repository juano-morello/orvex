package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

// unreachableDSN names a port nothing listens on, so a run that gets past
// argument checking exits 4 rather than 2.
const unreachableDSN = "postgres://u:p@127.0.0.1:1/db?connect_timeout=2" //nolint:gosec // G101: dummy credentials for a port nothing listens on, not a secret

func runMigrateCmd(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"migrate"}, args...), strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMigrateUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no subcommand", nil},
		{"unknown subcommand", []string{"sideways"}},
		{"up extra", []string{"up", "extra"}},
		{"status extra", []string{"status", "extra"}},
		{"status with -to", []string{"status", "-to", "1"}},
		{"down without -to", []string{"down"}},
		{"-to -1", []string{"down", "-to", "-1"}},
		{"-to x", []string{"down", "-to", "x"}},
		{"-to 99", []string{"up", "-to", "99"}},
		{"-to leading zero", []string{"up", "-to", "01"}},
		{"-to plus sign", []string{"up", "-to", "+1"}},
		{"-to empty", []string{"down", "-to", ""}},
		{"unknown flag", []string{"up", "-bogus"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(databaseURLEnv, unreachableDSN)
			code, stdout, stderr := runMigrateCmd(tt.args...)
			if code != exitUsage {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, exitUsage, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if stderr == "" {
				t.Error("stderr is empty")
			}
		})
	}

	t.Run("ORVEX_DATABASE_URL unset", func(t *testing.T) {
		t.Setenv(databaseURLEnv, "") // restores the variable at cleanup
		if err := os.Unsetenv(databaseURLEnv); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := runMigrateCmd("status")
		if code != exitUsage || !strings.Contains(stderr, databaseURLEnv) {
			t.Errorf("exit code = %d, stderr = %q; want %d naming %s", code, stderr, exitUsage, databaseURLEnv)
		}
	})
	t.Run("ORVEX_DATABASE_URL empty", func(t *testing.T) {
		t.Setenv(databaseURLEnv, "")
		if code, _, _ := runMigrateCmd("status"); code != exitUsage {
			t.Errorf("exit code = %d, want %d", code, exitUsage)
		}
	})
	t.Run("ORVEX_DATABASE_URL unparsable", func(t *testing.T) {
		for name, dsn := range map[string]string{ //nolint:gosec // G101: dummy credentials in unparsable DSNs, not a secret
			"syntax":                 "::bad",
			"bad parameter":          "postgres://u:p@127.0.0.1:1/db?connect_timeout=x",
			"bad sslmode":            "postgres://u:p@127.0.0.1:1/db?sslmode=bogus",
			"unterminated key=value": "host='unterminated",
		} {
			t.Run(name, func(t *testing.T) {
				for _, sub := range [][]string{{"up"}, {"down", "-to", "0"}, {"status"}} {
					t.Run(sub[0], func(t *testing.T) {
						t.Setenv(databaseURLEnv, dsn)
						code, stdout, stderr := runMigrateCmd(sub...)
						if code != exitUsage {
							t.Errorf("exit code = %d, want %d", code, exitUsage)
						}
						if stdout != "" {
							t.Errorf("stdout = %q, want empty", stdout)
						}
						if want := "crashctl: migrate " + sub[0] + ": invalid ORVEX_DATABASE_URL\n"; stderr != want {
							t.Errorf("stderr = %q, want %q", stderr, want)
						}
					})
				}
			})
		}
	})
}

func TestMigrateConnectError(t *testing.T) {
	for name, dsn := range map[string]string{
		"url":       unreachableDSN,
		"key=value": "host=127.0.0.1 port=1 user=u password=p dbname=db connect_timeout=2",
	} {
		t.Run(name, func(t *testing.T) {
			for _, sub := range [][]string{{"up"}, {"down", "-to", "0"}, {"status"}} {
				t.Run(sub[0], func(t *testing.T) {
					t.Setenv(databaseURLEnv, dsn)
					code, stdout, stderr := runMigrateCmd(sub...)
					if code != exitIO {
						t.Errorf("exit code = %d, want %d (stderr %q)", code, exitIO, stderr)
					}
					if stdout != "" {
						t.Errorf("stdout = %q, want empty", stdout)
					}
					if !strings.HasPrefix(stderr, "crashctl: migrate "+sub[0]+": ") || strings.Count(stderr, "\n") != 1 {
						t.Errorf("stderr = %q, want one crashctl: migrate %s: line", stderr, sub[0])
					}
				})
			}
		})
	}
}

// TestMigrateNeverPrintsDatabaseURL: no output of crashctl migrate holds
// any part of ORVEX_DATABASE_URL (LBT-21 K-12).
func TestMigrateNeverPrintsDatabaseURL(t *testing.T) {
	for name, dsn := range sentinelDSNs {
		t.Run(name, func(t *testing.T) {
			t.Setenv(databaseURLEnv, dsn)
			for _, sub := range [][]string{{"up"}, {"down", "-to", "0"}, {"status"}} {
				code, stdout, stderr := runMigrateCmd(sub...)
				if code != exitUsage && code != exitIO {
					t.Errorf("%v: exit code = %d, want %d or %d (stderr %q)", sub, code, exitUsage, exitIO, stderr)
				}
				if !strings.HasPrefix(stderr, "crashctl: migrate "+sub[0]+": ") || strings.Count(stderr, "\n") != 1 {
					t.Errorf("%v: stderr = %q, want one crashctl: migrate %s: line", sub, stderr, sub[0])
				}
				requireNoSentinel(t, "stdout", stdout, dsn)
				requireNoSentinel(t, "stderr", stderr, dsn)
			}
		})
	}
}

// TestMigrateIgnoresKeyringEnv: crashctl migrate never reads
// ORVEX_SECRET_KEYRING_FILE (LBT-21 K-13).
func TestMigrateIgnoresKeyringEnv(t *testing.T) {
	t.Setenv(databaseURLEnv, unreachableDSN)
	t.Setenv(sealing.KeyringFileEnv, writeKeyringFile(t, "active k1\n", 0o644))
	code, stdout, stderr := runMigrateCmd("status")
	if code != exitIO || stdout != "" {
		t.Errorf("exit code = %d, stdout %q; want %d (connect) and empty", code, stdout, exitIO)
	}
	if strings.Contains(stderr, sealing.KeyringFileEnv) || strings.Contains(stderr, "keyring") {
		t.Errorf("stderr = %q mentions the keyring", stderr)
	}
}

func TestMigrateExitCodes(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{postgres.ErrTarget, exitUsage},
		{postgres.ErrWrongDirection, exitUsage},
		{postgres.ErrSchemaTooNew, exitUnsupported},
		{postgres.ErrChecksumMismatch, exitUnsupported},
		{postgres.ErrSchemaState, exitUnsupported},
		{postgres.ErrMigrationFiles, exitFailed},
		{&pgconn.PgError{Code: postgres.CodeDownRefused}, exitFailed},
		{&pgconn.PgError{Code: postgres.CodeEncoding}, exitFailed},
		{errors.New("boom"), exitFailed},
	}
	for _, tt := range tests {
		err := tt.err
		if err != nil {
			err = fmt.Errorf("postgres: migration 0004 snapshots down: %w", err)
		}
		if got := migrateExitCode(err); got != tt.want {
			t.Errorf("migrateExitCode(%v) = %d, want %d", err, got, tt.want)
		}
	}
}

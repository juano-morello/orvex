//go:build integration

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/buildinfo"
)

// Integration tests of make audit (LBT-25), run as an operator runs it,
// against rounds created, seeded and executed through
// internal/persistence/postgres/pgtest/roundtest in real PostgreSQL. Each run
// builds crashctl into a per-test BIN_DIR, so the repository's bin/ is
// untouched. None is parallel: they set ORVEX_DATABASE_URL through
// exportDatabase. None sleeps.

// makeEnvDrop names the variables removed from the inherited environment.
// make test-integration exports its flags, its level, a possible jobserver
// and (through the Makefile's export line) empty audit variables to go test;
// they must not leak into the inner make.
var makeEnvDrop = map[string]bool{
	"MAKEFLAGS": true, "MFLAGS": true, "MAKELEVEL": true, "MAKEOVERRIDES": true, "GNUMAKEFLAGS": true,
	"MAKE_TERMOUT": true, "MAKE_TERMERR": true,
	"ROUND_ID": true, "AUDIT_FORMAT": true, "AUDIT_UNSETTLED": true,
	databaseURLEnv: true, "TMPDIR": true, "GOTMPDIR": true,
}

// makeAuditResult is one make audit run: make's exit status and its streams.
type makeAuditResult struct {
	code           int
	stdout, stderr string
}

// makeAudit runs make audit BIN_DIR=binDir VERSION=<test binary's version>
// args... in the repository root with TMPDIR=tmp, the inherited environment
// minus makeEnvDrop, and env. VERSION makes the built crashctl print the
// version wantExportReport expects, not git describe's.
func makeAudit(t *testing.T, binDir, tmp string, env []string, args ...string) makeAuditResult {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Makefile")); err != nil {
		t.Fatalf("repository root %s has no Makefile: %v", root, err)
	}
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Fatalf("make audit tests need make on PATH: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), makePath, append([]string{"--no-print-directory", "audit", "BIN_DIR=" + binDir, "VERSION=" + buildinfo.Version}, args...)...) //nolint:gosec // G204: the make path and arguments are fixed by this test
	cmd.Dir = root
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !makeEnvDrop[name] {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "TMPDIR="+tmp, "GOTMPDIR="+t.TempDir())
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := makeAuditResult{}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("make audit: %v", err)
		}
		res.code = exitErr.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

// requireEmptyDir fails unless dir is empty: the temporary directory was
// removed.
func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %v after the run, want it empty", dir, names)
	}
}

// requireMakeFailure requires make to exit 2 after a recipe exit code, and
// stderr to contain every want.
func requireMakeFailure(t *testing.T, res makeAuditResult, code int, want ...string) {
	t.Helper()
	ok := res.code == 2 && strings.Contains(res.stderr, "] Error "+strconv.Itoa(code)+"\n")
	for _, w := range want {
		ok = ok && strings.Contains(res.stderr, w)
	}
	if !ok {
		t.Errorf("make exit %d, want 2 with Error %d and %q\nstdout:\n%s\nstderr:\n%s", res.code, code, want, res.stdout, res.stderr)
	}
}

// requireMakeSuccess requires make to exit 0 with nothing from make or the
// recipe on stderr.
func requireMakeSuccess(t *testing.T, res makeAuditResult) {
	t.Helper()
	if res.code != 0 {
		t.Fatalf("make exit %d, want 0\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if strings.Contains(res.stderr, "audit: ") || strings.Contains(res.stderr, "Error") {
		t.Errorf("stderr = %q, want no audit: message and no make error", res.stderr)
	}
}

// requireVerified requires stdout to hold the export report, then the
// verifier's chain line, and to end with the target's final line.
func requireVerified(t *testing.T, stdout, exportReport, round string, records int, head string) {
	t.Helper()
	chainLine := fmt.Sprintf("chain ok records %d head %s\n", records, head)
	exportAt := strings.Index(stdout, exportReport)
	verifierAt := strings.Index(stdout, "verifier ")
	if exportAt < 0 || !strings.Contains(stdout, chainLine) || verifierAt < exportAt {
		t.Errorf("stdout = %q, want the export report %q, then the verifier report with %q", stdout, exportReport, chainLine)
	}
	if want := "ok\naudit: round " + round + " verified, head " + head + "\n"; !strings.HasSuffix(stdout, want) {
		t.Errorf("stdout = %q, want it to end with %q", stdout, want)
	}
}

func TestMakeAuditMissingVariables(t *testing.T) {
	const roundUsage = "audit: ROUND_ID is not set; usage: make audit ROUND_ID=<uuid> [AUDIT_FORMAT=NAME] [AUDIT_UNSETTLED=1]\n"
	const urlUsage = "audit: ORVEX_DATABASE_URL is not set; it names the database to export from\n"
	unreachable := databaseURLEnv + "=" + unreachableDSN
	tests := []struct {
		name string
		env  []string
		args []string
		want string
	}{
		{"no-round-id", []string{unreachable}, nil, roundUsage},
		{"empty-round-id", []string{unreachable}, []string{"ROUND_ID="}, roundUsage},
		{"no-database-url", nil, []string{"ROUND_ID=" + exportRoundID}, urlUsage},
		{"empty-database-url", []string{databaseURLEnv + "="}, []string{"ROUND_ID=" + exportRoundID}, urlUsage},
		{"bad-audit-unsettled", []string{unreachable}, []string{"ROUND_ID=" + exportRoundID, "AUDIT_UNSETTLED=yes"},
			"audit: AUDIT_UNSETTLED must be 1 or empty, got 'yes'\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binDir, tmp := t.TempDir(), t.TempDir()
			res := makeAudit(t, binDir, tmp, tc.env, tc.args...)
			requireMakeFailure(t, res, exitUsage, tc.want)
			if res.stdout != "" {
				t.Errorf("stdout = %q, want nothing built or exported", res.stdout)
			}
			requireNoFile(t, filepath.Join(binDir, "crashctl"))
			requireEmptyDir(t, tmp)
		})
	}
}

func TestMakeAuditSettledRounds(t *testing.T) {
	binDir := t.TempDir()
	for _, r := range executedRounds {
		if !r.settled {
			continue
		}
		t.Run(strings.TrimSuffix(r.file, ".jsonl"), func(t *testing.T) {
			dsn, pool := exportDatabase(t)
			g := drained(t, pool, r.file)
			env := []string{databaseURLEnv + "=" + dsn}
			var args []string
			if r.file == "empty-round.jsonl" {
				// ROUND_ID from the environment, not the command line.
				env = append(env, "ROUND_ID="+g.UUID())
			} else {
				args = []string{"ROUND_ID=" + g.UUID()}
			}
			tmp := t.TempDir()
			res := makeAudit(t, binDir, tmp, env, args...)
			requireMakeSuccess(t, res)
			requireVerified(t, res.stdout, wantExportReport(g.UUID(), r.s, r.n, r.head, settledYes), g.UUID(), r.s+1, r.head)
			requireEmptyDir(t, tmp)
			if _, err := os.Stat(filepath.Join(binDir, "crashctl")); err != nil {
				t.Errorf("crashctl not built into BIN_DIR: %v", err)
			}
		})
	}
}

func TestMakeAuditUnsettled(t *testing.T) {
	binDir := t.TempDir()
	var r executedRound
	for _, er := range executedRounds {
		if er.file == "in-progress.jsonl" {
			r = er
		}
	}
	if r.file == "" {
		t.Fatal("executedRounds has no in-progress.jsonl")
	}
	dsn, pool := exportDatabase(t)
	g := drained(t, pool, r.file)
	env := []string{databaseURLEnv + "=" + dsn}

	t.Run("default", func(t *testing.T) {
		tmp := t.TempDir()
		res := makeAudit(t, binDir, tmp, env, "ROUND_ID="+g.UUID())
		requireMakeFailure(t, res, exitNotSettled, notSettledMessage(g.UUID(), r.s, r.n),
			"audit: crashctl audit export failed with exit code 6\n")
		if strings.Contains(res.stdout, "\nrecords ") {
			t.Errorf("stdout = %q, want no export report", res.stdout)
		}
		requireEmptyDir(t, tmp)
	})
	t.Run("audit-unsettled", func(t *testing.T) {
		tmp := t.TempDir()
		res := makeAudit(t, binDir, tmp, env, "ROUND_ID="+g.UUID(), "AUDIT_UNSETTLED=1")
		requireMakeSuccess(t, res)
		requireVerified(t, res.stdout, wantExportReport(g.UUID(), r.s, r.n, r.head, settledNo), g.UUID(), r.s+1, r.head)
		requireEmptyDir(t, tmp)
	})
}

func TestMakeAuditFormat(t *testing.T) {
	binDir := t.TempDir()
	r := executedRounds[0]
	if r.file != "worked-example.jsonl" {
		t.Fatalf("executedRounds[0] is %s, want worked-example.jsonl", r.file)
	}
	dsn, pool := exportDatabase(t)
	g := drained(t, pool, r.file)
	env := []string{databaseURLEnv + "=" + dsn}

	t.Run("v1", func(t *testing.T) {
		tmp := t.TempDir()
		res := makeAudit(t, binDir, tmp, env, "ROUND_ID="+g.UUID(), "AUDIT_FORMAT="+verification.Format)
		requireMakeSuccess(t, res)
		requireVerified(t, res.stdout, wantExportReport(g.UUID(), r.s, r.n, r.head, settledYes), g.UUID(), r.s+1, r.head)
		requireEmptyDir(t, tmp)
	})
	t.Run("unsupported", func(t *testing.T) {
		tmp := t.TempDir()
		res := makeAudit(t, binDir, tmp, env, "ROUND_ID="+g.UUID(), "AUDIT_FORMAT=orvex/audit-history/v99")
		requireMakeFailure(t, res, exitUnsupported, "crashctl: audit export: format orvex/audit-history/v99 unsupported",
			"audit: crashctl audit export failed with exit code 3\n")
		requireEmptyDir(t, tmp)
	})
}

func TestMakeAuditFailures(t *testing.T) {
	binDir := t.TempDir()
	unreachable := []string{databaseURLEnv + "=" + unreachableDSN}

	t.Run("unknown-round", func(t *testing.T) {
		dsn, _ := exportDatabase(t)
		const unknown = "0190f3a8-7b3c-7d4e-8f00-0000000000ff"
		tmp := t.TempDir()
		res := makeAudit(t, binDir, tmp, []string{databaseURLEnv + "=" + dsn}, "ROUND_ID="+unknown)
		requireMakeFailure(t, res, exitNotFound, "crashctl: audit export: round "+unknown+" not found\n",
			"audit: crashctl audit export failed with exit code 5\n")
		requireEmptyDir(t, tmp)
	})
	t.Run("malformed-round-id", func(t *testing.T) {
		tmp := t.TempDir()
		res := makeAudit(t, binDir, tmp, unreachable, "ROUND_ID=not-a-uuid")
		requireMakeFailure(t, res, exitUsage, "ROUND_ID must be a canonical lowercase UUID other than the nil UUID",
			"audit: crashctl audit export failed with exit code 2\n")
		requireEmptyDir(t, tmp)
	})
	t.Run("unreachable-database", func(t *testing.T) {
		tmp := t.TempDir()
		res := makeAudit(t, binDir, tmp, unreachable, "ROUND_ID="+exportRoundID)
		requireMakeFailure(t, res, exitIO, "audit: crashctl audit export failed with exit code 4\n")
		if strings.Contains(res.stdout, "u:p@") || strings.Contains(res.stderr, "u:p@") {
			t.Errorf("output names the DSN credentials\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
		}
		requireEmptyDir(t, tmp)
	})
	t.Run("verify-fails", func(t *testing.T) {
		dsn, pool := exportDatabase(t)
		g := drained(t, pool, "worked-example.jsonl")
		replicaExec(t, pool, `UPDATE orvex.round_secrets SET config_secret = $2 WHERE round_id = $1`,
			g.RoundID, []byte(`{"seed":"`+strings.Repeat("f", 64)+`"}`))
		tmp := t.TempDir()
		// -unsettled: the export still writes the file (settled unknown).
		res := makeAudit(t, binDir, tmp, []string{databaseURLEnv + "=" + dsn}, "ROUND_ID="+g.UUID(), "AUDIT_UNSETTLED=1")
		requireMakeFailure(t, res, exitFailed, "fairness failed", "audit: crashctl audit verify failed with exit code 1\n")
		if !strings.Contains(res.stdout, "\nsettled unknown\n") {
			t.Errorf("stdout = %q, want the export report with settled unknown", res.stdout)
		}
		requireEmptyDir(t, tmp)
	})
}

//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/migrations"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var statusApplied = regexp.MustCompile(`^[0-9]{4} [a-z][a-z0-9_]* applied [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z by \S+$`)

func lines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func TestMigrateCLI(t *testing.T) {
	dsn := pgtest.EmptyDatabase(t)
	t.Setenv(databaseURLEnv, dsn)
	names := []string{"0001 foundation", "0002 rounds_admissions", "0003 chain_execution", "0004 snapshots", "0005 ledger", "0006 outbox", "0007 admission_window", "0008 execution_ledger", "0009 close_bound"}

	// 1. up applies all nine.
	code, stdout, stderr := runMigrateCmd("up")
	if code != exitOK || stderr != "" {
		t.Fatalf("up: exit %d, stderr %q", code, stderr)
	}
	var want []string
	for _, n := range names {
		want = append(want, "applied "+n)
	}
	want = append(want, "schema version 9")
	if got := lines(stdout); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("up stdout:\n%s\nwant:\n%s", stdout, strings.Join(want, "\n"))
	}

	// 2. status lists nine applied migrations.
	code, stdout, stderr = runMigrateCmd("status")
	if code != exitOK || stderr != "" {
		t.Fatalf("status: exit %d, stderr %q", code, stderr)
	}
	got := lines(stdout)
	if len(got) != 10 || got[9] != "schema version 9" {
		t.Fatalf("status stdout:\n%s", stdout)
	}
	for i, n := range names {
		if !strings.HasPrefix(got[i], n+" applied ") || !statusApplied.MatchString(got[i]) {
			t.Errorf("status line %d = %q, want %q applied <RFC 3339 UTC> by <build>", i, got[i], n)
		}
	}

	// 3. down -to 0 reverts everything.
	code, stdout, stderr = runMigrateCmd("down", "-to", "0")
	if code != exitOK || stderr != "" {
		t.Fatalf("down: exit %d, stderr %q", code, stderr)
	}
	want = nil
	for i := len(names) - 1; i >= 0; i-- {
		want = append(want, "reverted "+names[i])
	}
	want = append(want, "schema version 0")
	if strings.Join(lines(stdout), "\n") != strings.Join(want, "\n") {
		t.Errorf("down stdout:\n%s", stdout)
	}
	code, stdout, _ = runMigrateCmd("status")
	if code != exitOK || !strings.Contains(stdout, "0001 foundation pending\n") || !strings.HasSuffix(stdout, "schema version 0\n") {
		t.Errorf("status after down: exit %d, stdout:\n%s", code, stdout)
	}

	// 4. up again.
	if code, _, stderr = runMigrateCmd("up"); code != exitOK {
		t.Fatalf("second up: exit %d, stderr %q", code, stderr)
	}
	if code, stdout, _ = runMigrateCmd("up"); code != exitOK || stdout != "schema version 9\n" {
		t.Errorf("no-op up: exit %d, stdout %q", code, stdout)
	}
	if code, _, stderr = runMigrateCmd("down", "-to", "9"); code != exitOK || stderr != "" {
		t.Errorf("no-op down: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr = runMigrateCmd("up", "-to", "3"); code != exitUsage || !strings.Contains(stderr, "other side") {
		t.Errorf("up below the current version: exit %d, stderr %q; want %d", code, stderr, exitUsage)
	}

	// 5. down refuses with audit data.
	pool := pgtest.Pool(t, dsn)
	history, err := os.ReadFile(filepath.Join(goldenDir, "worked-example.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	pgtest.LoadHistory(t, pool, history, nil)
	code, stdout, stderr = runMigrateCmd("down", "-to", "0")
	if code != exitFailed || !strings.Contains(stderr, "OX003") {
		t.Errorf("down with data: exit %d, stderr %q; want %d with OX003", code, stderr, exitFailed)
	}
	if strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "crashctl: migrate down: ") {
		t.Errorf("down with data: stderr %q, want one crashctl: migrate down: line", stderr)
	}
	if stdout != "" {
		t.Errorf("down with data: stdout %q, want nothing: 0009 refuses before any step completes", stdout)
	}

	// 6. a bookkeeping row from a newer build fails closed.
	if _, err := pool.Exec(context.Background(), `INSERT INTO orvex_migrate.schema_migrations
		(version, name, up_sha256, down_sha256, applied_by) VALUES (99, 'future', $1, $1, 'test')`, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"status", "up"} {
		code, stdout, stderr = runMigrateCmd(sub)
		if code != exitUnsupported {
			t.Errorf("%s with version 99: exit %d, stderr %q; want %d", sub, code, stderr, exitUnsupported)
		}
		if strings.Contains(stdout, "schema version") {
			t.Errorf("%s with version 99: stdout %q has a schema version line", sub, stdout)
		}
	}
	if strings.Contains(stderr, "orvex:orvex@") || strings.Contains(stderr, dsn) {
		t.Errorf("stderr %q contains the DSN", stderr)
	}
}

// multilineFailingWriter fails every write with a multi-line error, as some
// writers' errors are.
type multilineFailingWriter struct{}

func (multilineFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("disk\nfull:\n  sector 7")
}

// runMigrateFailingStdout runs crashctl migrate with a stdout that always
// fails and requires exit 4 and exactly one flattened stderr line that names
// the writer error. It returns that line.
func runMigrateFailingStdout(t *testing.T, args ...string) string {
	t.Helper()
	var stderr bytes.Buffer
	code := run(append([]string{"migrate"}, args...), strings.NewReader(""), multilineFailingWriter{}, &stderr)
	if code != exitIO {
		t.Errorf("migrate %v with failing stdout: exit %d, want %d (stderr %q)", args, code, exitIO, stderr.String())
	}
	line := stderr.String()
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Errorf("migrate %v with failing stdout: stderr %q, want exactly one line", args, line)
	}
	if !strings.HasPrefix(line, "crashctl: migrate "+args[0]+": ") {
		t.Errorf("migrate %v with failing stdout: stderr %q, want the crashctl: migrate %s: prefix", args, line, args[0])
	}
	if !strings.Contains(line, "write stdout: disk full: sector 7") {
		t.Errorf("migrate %v with failing stdout: stderr %q, want the flattened writer error", args, line)
	}
	return line
}

func TestMigrateCLIStdoutFailure(t *testing.T) {
	dsn := pgtest.EmptyDatabase(t)
	t.Setenv(databaseURLEnv, dsn)
	pool := pgtest.Pool(t, dsn)

	// Successful runs whose report cannot be written.
	runMigrateFailingStdout(t, "up")
	if code, stdout, _ := runMigrateCmd("status"); code != exitOK || !strings.HasSuffix(stdout, "schema version 9\n") {
		t.Fatalf("up with failing stdout did not migrate: exit %d, stdout %q", code, stdout)
	}
	runMigrateFailingStdout(t, "status")

	// A runner error (the down refuses at 0009), with a failing stdout: one
	// line carries both errors.
	history, err := os.ReadFile(filepath.Join(goldenDir, "worked-example.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	pgtest.LoadHistory(t, pool, history, nil)
	if line := runMigrateFailingStdout(t, "down", "-to", "0"); !strings.Contains(line, "OX003") {
		t.Errorf("down with data and failing stdout: stderr %q, want OX003", line)
	}

	// A runner error with nothing completed (inconsistent bookkeeping).
	if _, err := pool.Exec(context.Background(), `INSERT INTO orvex_migrate.schema_migrations
		(version, name, up_sha256, down_sha256, applied_by) VALUES (99, 'future', $1, $1, 'test')`, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if line := runMigrateFailingStdout(t, "status"); !strings.Contains(line, "bookkeeping is inconsistent") {
		t.Errorf("status with version 99 and failing stdout: stderr %q, want the runner error", line)
	}
}

// TestMigrateCLIExitCodes runs crashctl migrate end to end against real
// PostgreSQL for the LBT-10 P-6 exit-code rows no other test exercises
// through run: 1 a migration failed, 2 a target on the wrong side, 3 a schema
// that differs from or is newer than this build, 4 a database that cannot be
// reached. Stderr is matched on SQLSTATEs and runner phrases only.
func TestMigrateCLIExitCodes(t *testing.T) {
	ms, err := postgres.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	requireLine := func(t *testing.T, stderr, sub, want string) {
		t.Helper()
		if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
			t.Errorf("%s: stderr %q, want exactly one line", sub, stderr)
		}
		if !strings.HasPrefix(stderr, "crashctl: migrate "+sub+": ") {
			t.Errorf("%s: stderr %q, want the crashctl: migrate %s: prefix", sub, stderr, sub)
		}
		if !strings.Contains(stderr, want) {
			t.Errorf("%s: stderr %q, want it to contain %q", sub, stderr, want)
		}
	}
	requireStatusVersion := func(t *testing.T, v string) string {
		t.Helper()
		code, stdout, stderr := runMigrateCmd("status")
		if code != exitOK || !strings.HasSuffix(stdout, "schema version "+v+"\n") {
			t.Errorf("status: exit %d, stdout %q, stderr %q; want 0 ending with schema version %s", code, stdout, stderr, v)
		}
		return stdout
	}
	mustUp := func(t *testing.T, args ...string) {
		t.Helper()
		if code, _, stderr := runMigrateCmd(append([]string{"up"}, args...)...); code != exitOK {
			t.Fatalf("up %v: exit %d, stderr %q", args, code, stderr)
		}
	}

	t.Run("1 sql error", func(t *testing.T) {
		dsn := pgtest.EmptyDatabase(t)
		t.Setenv(databaseURLEnv, dsn)
		mustUp(t, "-to", "1")
		if _, err := pgtest.Pool(t, dsn).Exec(ctx, `CREATE TABLE orvex.chain_records (x int)`); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := runMigrateCmd("up")
		if code != exitFailed {
			t.Errorf("up: exit %d, stderr %q; want %d", code, stderr, exitFailed)
		}
		if stdout != "applied 0002 rounds_admissions\n" {
			t.Errorf("up: stdout %q, want only the committed 0002 step", stdout)
		}
		requireLine(t, stderr, "up", "42P07")
		if !strings.Contains(stderr, "0003 chain_execution up") {
			t.Errorf("up: stderr %q, want it to name 0003 chain_execution up", stderr)
		}
		if out := requireStatusVersion(t, "2"); !strings.Contains(out, "0003 chain_execution pending\n") {
			t.Errorf("status: stdout %q, want 0003 chain_execution pending", out)
		}
	})

	t.Run("1 encoding", func(t *testing.T) {
		t.Setenv(databaseURLEnv, pgtest.CreateDatabase(t, "ENCODING 'SQL_ASCII' TEMPLATE template0"))
		code, stdout, stderr := runMigrateCmd("up")
		if code != exitFailed || stdout != "" {
			t.Errorf("up: exit %d, stdout %q, stderr %q; want %d and no stdout", code, stdout, stderr, exitFailed)
		}
		requireLine(t, stderr, "up", postgres.CodeEncoding)
		requireStatusVersion(t, "0")
	})

	t.Run("2 down target above current", func(t *testing.T) {
		t.Setenv(databaseURLEnv, pgtest.EmptyDatabase(t))
		mustUp(t, "-to", "2")
		code, stdout, stderr := runMigrateCmd("down", "-to", "5")
		if code != exitUsage || stdout != "" {
			t.Errorf("down -to 5: exit %d, stdout %q, stderr %q; want %d and no stdout", code, stdout, stderr, exitUsage)
		}
		requireLine(t, stderr, "down", "other side")
		requireStatusVersion(t, "2")
	})

	unsupported := func(t *testing.T, want string) {
		t.Helper()
		for _, args := range [][]string{{"status"}, {"up"}, {"down", "-to", "0"}} {
			code, stdout, stderr := runMigrateCmd(args...)
			if code != exitUnsupported || stdout != "" {
				t.Errorf("%v: exit %d, stdout %q, stderr %q; want %d and no stdout", args, code, stdout, stderr, exitUnsupported)
			}
			requireLine(t, stderr, args[0], want)
		}
	}

	t.Run("3 checksum mismatch", func(t *testing.T) {
		dsn := pgtest.EmptyDatabase(t)
		t.Setenv(databaseURLEnv, dsn)
		mustUp(t)
		if _, err := pgtest.Pool(t, dsn).Exec(ctx,
			`UPDATE orvex_migrate.schema_migrations SET up_sha256 = $1 WHERE version = 1`, make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
		unsupported(t, "differs from this build")
	})

	t.Run("3 schema newer than build", func(t *testing.T) {
		dsn := pgtest.EmptyDatabase(t)
		t.Setenv(databaseURLEnv, dsn)
		mustUp(t)
		// Contiguous versions, so the runner reports ErrSchemaTooNew rather
		// than ErrSchemaState.
		if _, err := pgtest.Pool(t, dsn).Exec(ctx, `INSERT INTO orvex_migrate.schema_migrations
			(version, name, up_sha256, down_sha256, applied_by) VALUES ($1, 'future', $2, $2, 'test')`,
			len(ms)+1, make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
		unsupported(t, "newer than this build")
	})

	unreachable := func(t *testing.T, edit func(*url.URL), want string) {
		t.Helper()
		u, err := url.Parse(pgtest.EmptyDatabase(t))
		if err != nil {
			t.Fatal(err)
		}
		edit(u)
		t.Setenv(databaseURLEnv, u.String())
		for _, sub := range []string{"status", "up"} {
			code, stdout, stderr := runMigrateCmd(sub)
			if code != exitIO || stdout != "" {
				t.Errorf("%s: exit %d, stdout %q, stderr %q; want %d and no stdout", sub, code, stdout, stderr, exitIO)
			}
			requireLine(t, stderr, sub, want)
		}
	}

	t.Run("4 database does not exist", func(t *testing.T) {
		unreachable(t, func(u *url.URL) { u.Path = "/no_such_database" }, "3D000")
	})

	t.Run("4 wrong password", func(t *testing.T) {
		unreachable(t, func(u *url.URL) { u.User = url.UserPassword(u.User.Username(), "wrong-password") }, "28P01")
	})
}

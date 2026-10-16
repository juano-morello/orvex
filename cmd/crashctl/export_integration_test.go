//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
)

// Integration tests of crashctl audit export (LBT-19 spec section 10 step
// 6) against real PostgreSQL. Each test uses its own migrated database and
// sets ORVEX_DATABASE_URL, so none is parallel. None sleeps.

// executedRound is a spec section 8 row: a golden history created, seeded
// and executed by LBT-12's executor.
type executedRound struct {
	file      string
	snapshots []uint64
	s, n      int
	head      string // link hash of record S
	settled   bool
	// manifestHead is the golden file's head (record N's link hash).
	manifestHead string
}

var executedRounds = []executedRound{
	{"worked-example.jsonl", []uint64{0, 9, 19, 21}, 21, 22,
		"3959ed4ca0f2651071b0a325ffae6f5951d132e2e922ca8669974a0031d7fd40", true,
		"7cf8cc4b1807099cdd1f16cbc0c790f1a877f30b696ebc0de751997d73ac3da7"},
	{"instant-crash.jsonl", []uint64{0, 2, 4, 5}, 5, 5,
		"feea2c86497a5af3450c768129b71626619ae594c5f933b41ca7613e1a6d64da", true,
		"feea2c86497a5af3450c768129b71626619ae594c5f933b41ca7613e1a6d64da"},
	{"empty-round.jsonl", []uint64{0, 1, 2, 3}, 3, 3,
		"700d572f193400c59c64b24e3989d8a4477b9791a689ecfdfc0e47de35229bd6", true,
		"700d572f193400c59c64b24e3989d8a4477b9791a689ecfdfc0e47de35229bd6"},
	{"in-progress.jsonl", []uint64{0, 2}, 2, 3,
		"d0df1ee21f9abf0bcbc75ef3f3f05911e081c0e9ef9e6bdd9e336f01c9d703d3", false,
		"f041d95d5bb917ce0f0d51d30c47738c1b97932bebd26ac7f117f98a279c46f6"},
}

// exportDatabase returns a fresh migrated database's DSN and a pool on it,
// and points ORVEX_DATABASE_URL at it.
func exportDatabase(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.MigratedDatabase(t)
	t.Setenv(databaseURLEnv, dsn)
	return dsn, pgtest.Pool(t, dsn)
}

// drained seeds the golden round in file and executes every admission.
func drained(t *testing.T, pool *pgxpool.Pool, file string) roundtest.Golden {
	t.Helper()
	g := seedGolden(t, pool, file)
	if _, err := g.Executor.Drain(context.Background(), g.Lease(), 100); err != nil {
		t.Fatalf("Drain(%s): %v", file, err)
	}
	return g
}

// wantExportReport is the spec section 6 report.
func wantExportReport(round string, s, n int, head string, settled settledVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exporter %s\nformat %s\nround %s\n", buildinfo.String("crashctl"), verification.Format, round)
	fmt.Fprintf(&b, "records %d of %d head %s\n", s+1, n+1, head)
	if s < n {
		fmt.Fprintf(&b, "note: the file ends at the newest snapshot, sequence %d; sequences %d to %d are not exported\n", s, s+1, n)
	}
	fmt.Fprintf(&b, "settled %s\n", settled)
	if settled != settledYes {
		b.WriteString("warning: the file holds the round's secret config before settlement; keep it private\n")
	}
	b.WriteString("ok\n")
	return b.String()
}

// notSettledMessage is the spec section 6 exit 6 line.
func notSettledMessage(round string, s, n int) string {
	return fmt.Sprintf("crashctl: audit export: round %s is not settled at its newest snapshot, sequence %d of %d: "+
		"the file would disclose its unrevealed secret config; -unsettled exports it anyway\n", round, s, n)
}

// headerCheckpoints returns the checkpoint sequences of a history header.
func headerCheckpoints(t *testing.T, headerLine []byte) []uint64 {
	t.Helper()
	header, err := canonjson.Unmarshal(headerLine)
	if err != nil {
		t.Fatal(err)
	}
	list, err := canonjson.List(header["checkpoints"])
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, v := range list {
		o, err := canonjson.Obj(v)
		if err != nil {
			t.Fatal(err)
		}
		seq, err := canonjson.ParseUint(o["sequence"])
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

// verifyFile runs crashctl audit verify -expect-head head on path and
// requires exit 0 with the head in the chain line.
func verifyFile(t *testing.T, path string, records int, head string) {
	t.Helper()
	code, stdout, stderr := runAuditCmd(t, strings.NewReader(""), "verify", "-expect-head", head, path)
	if code != exitOK {
		t.Fatalf("audit verify -expect-head %s %s: exit %d, stderr %q", head, path, code, stderr)
	}
	if want := fmt.Sprintf("chain ok records %d head %s\n", records, head); !strings.Contains(stdout, want) {
		t.Errorf("audit verify stdout = %q, want %q", stdout, want)
	}
}

func TestExportExecutedRounds(t *testing.T) {
	for _, r := range executedRounds {
		t.Run(strings.TrimSuffix(r.file, ".jsonl"), func(t *testing.T) {
			_, pool := exportDatabase(t)
			g := drained(t, pool, r.file)
			if got := g.Hash(r.s).String(); got != r.head {
				t.Fatalf("golden record %d link hash = %s, spec section 8 says %s", r.s, got, r.head)
			}
			if got := g.Hash(r.n).String(); got != r.manifestHead || len(g.Lines) != r.n+1 {
				t.Fatalf("golden has %d records, head %s; spec section 8 says %d, %s", len(g.Lines), got, r.n+1, r.manifestHead)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "round.jsonl")
			code, stdout, stderr := exportCmd(t, g.UUID(), path)
			verdict := settledYes
			if !r.settled {
				if code != exitNotSettled || stdout != "" {
					t.Fatalf("exit %d, stdout %q; want %d and empty", code, stdout, exitNotSettled)
				}
				if want := notSettledMessage(g.UUID(), r.s, r.n); stderr != want {
					t.Errorf("stderr = %q, want %q", stderr, want)
				}
				requireNoFile(t, path)
				verdict = settledNo
				path = filepath.Join(dir, "unsettled.jsonl")
				code, stdout, stderr = exportCmd(t, "-unsettled", g.UUID(), path)
			}
			if code != exitOK || stderr != "" {
				t.Fatalf("exit %d, stderr %q; want 0 and empty", code, stderr)
			}
			if want := wantExportReport(g.UUID(), r.s, r.n, r.head, verdict); stdout != want {
				t.Errorf("report =\n%s\nwant\n%s", stdout, want)
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
			}
			file, err := os.ReadFile(path) //nolint:gosec // G304: path is a file this test created under t.TempDir()
			if err != nil {
				t.Fatal(err)
			}
			lines := historyLines(file)
			if got := headerCheckpoints(t, lines[0]); fmt.Sprint(got) != fmt.Sprint(r.snapshots) {
				t.Errorf("header checkpoints = %v, want %v", got, r.snapshots)
			}
			if got, want := bytes.Join(lines[1:], []byte("\n")), bytes.Join(g.Lines[:r.s+1], []byte("\n")); !bytes.Equal(got, want) {
				t.Errorf("records differ from golden records 0..%d", r.s)
			}
			var direct bytes.Buffer
			if _, err := eventstore.ExportHistory(context.Background(), pool, g.RoundID, &direct); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(file, direct.Bytes()) {
				t.Error("file differs from eventstore.ExportHistory")
			}

			verifyFile(t, path, r.s+1, r.head)
			if r.s < r.n {
				code, _, stderr := runAuditCmd(t, strings.NewReader(""), "verify", "-expect-head", r.manifestHead, path)
				if code != exitFailed || !strings.Contains(stderr, fmt.Sprintf("chain failed at sequence %d", r.s)) {
					t.Errorf("audit verify -expect-head <manifest head>: exit %d, stderr %q; want 1, chain failed at sequence %d",
						code, stderr, r.s)
				}
			}
		})
	}
}

func TestExportFollowsSnapshots(t *testing.T) {
	_, pool := exportDatabase(t)
	g := seedGolden(t, pool, "worked-example.jsonl")
	// Spec section 8: the heads come from the golden bytes and must equal
	// the literals computed during planning.
	for seq, want := range map[int]string{
		0:  "cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284",
		9:  "507826d44f608a6733aa3c579967ce4f4179546298372b8134a390c20aaa9bc0",
		19: "f0829a16ccf2511037175c4e0f8851285da331bef1172423c4b1fd732bcc9645",
		21: "3959ed4ca0f2651071b0a325ffae6f5951d132e2e922ca8669974a0031d7fd40",
		22: "7cf8cc4b1807099cdd1f16cbc0c790f1a877f30b696ebc0de751997d73ac3da7",
	} {
		if got := g.Hash(seq).String(); got != want {
			t.Fatalf("golden worked-example record %d link hash = %s, spec section 8 says %s", seq, got, want)
		}
	}
	dir := t.TempDir()
	s := 0
	for k := 0; k <= 22; k++ {
		if k > 0 {
			out, err := g.Executor.ExecuteNext(context.Background(), g.Lease())
			if err != nil || !out.Executed || out.Sequence != uint64(k) {
				t.Fatalf("ExecuteNext: %+v, %v; want sequence %d executed", out, err, k)
			}
		}
		for _, cp := range []int{0, 9, 19, 21} {
			if cp <= k {
				s = cp
			}
		}

		path := filepath.Join(dir, fmt.Sprintf("k%02d.jsonl", k))
		code, stdout, stderr := exportCmd(t, g.UUID(), path)
		if k < 21 {
			if code != exitNotSettled || stdout != "" || stderr != notSettledMessage(g.UUID(), s, k) {
				t.Errorf("k=%d: exit %d, stdout %q, stderr %q; want %d naming sequence %d of %d", k, code, stdout, stderr,
					exitNotSettled, s, k)
			}
			requireNoFile(t, path)
		} else if code != exitOK || stdout != wantExportReport(g.UUID(), s, k, g.Hash(s).String(), settledYes) {
			t.Errorf("k=%d: exit %d, stdout %q, stderr %q; want 0 and the settled report", k, code, stdout, stderr)
		}

		upath := filepath.Join(dir, fmt.Sprintf("k%02d-unsettled.jsonl", k))
		verdict := settledNo
		if k >= 21 {
			verdict = settledYes
		}
		code, stdout, stderr = exportCmd(t, "-unsettled", g.UUID(), upath)
		if code != exitOK || stderr != "" {
			t.Fatalf("k=%d -unsettled: exit %d, stderr %q", k, code, stderr)
		}
		if want := wantExportReport(g.UUID(), s, k, g.Hash(s).String(), verdict); stdout != want {
			t.Errorf("k=%d -unsettled: report =\n%s\nwant\n%s", k, stdout, want)
		}
		if hasNote := strings.Contains(stdout, "note: "); hasNote != (s < k) {
			t.Errorf("k=%d: note present %v, want %v", k, hasNote, s < k)
		}
		verifyFile(t, upath, s+1, g.Hash(s).String())
	}
}

func TestExportStdout(t *testing.T) {
	t.Run("settled", func(t *testing.T) {
		_, pool := exportDatabase(t)
		g := drained(t, pool, "worked-example.jsonl")
		path := filepath.Join(t.TempDir(), "round.jsonl")
		if code, _, stderr := exportCmd(t, g.UUID(), path); code != exitOK {
			t.Fatalf("path export: exit %d, stderr %q", code, stderr)
		}
		file, err := os.ReadFile(path) //nolint:gosec // G304: path is a file this test created under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := exportCmd(t, g.UUID(), "-")
		if code != exitOK {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if stdout != string(file) {
			t.Error("stdout differs from the path export")
		}
		if want := wantExportReport(g.UUID(), 21, 22, g.Hash(21).String(), settledYes); stderr != want {
			t.Errorf("stderr =\n%s\nwant\n%s", stderr, want)
		}
		code, vout, verr := runAuditCmd(t, strings.NewReader(stdout), "verify", "-expect-head", g.Hash(21).String(), "-")
		if code != exitOK || !strings.Contains(vout, "chain ok records 22 head "+g.Hash(21).String()) {
			t.Errorf("audit verify -: exit %d, stdout %q, stderr %q", code, vout, verr)
		}
	})
	t.Run("not settled", func(t *testing.T) {
		_, pool := exportDatabase(t)
		g := drained(t, pool, "in-progress.jsonl")
		code, stdout, stderr := exportCmd(t, g.UUID(), "-")
		if code != exitNotSettled || stdout != "" || stderr != notSettledMessage(g.UUID(), 2, 3) {
			t.Errorf("exit %d, stdout %q, stderr %q; want %d, nothing on stdout", code, stdout, stderr, exitNotSettled)
		}
	})
}

func TestExportUnknownRound(t *testing.T) {
	_, pool := exportDatabase(t)
	pgtest.LoadHistory(t, pool, readGoldenFile(t, "worked-example.jsonl"), nil)
	const unknown = "0190f3a8-7b3c-7d4e-8f00-0000000000ff"
	path := filepath.Join(t.TempDir(), "round.jsonl")
	code, stdout, stderr := exportCmd(t, unknown, path)
	if code != exitNotFound || stdout != "" {
		t.Errorf("exit %d, stdout %q; want %d and empty", code, stdout, exitNotFound)
	}
	if want := "crashctl: audit export: round " + unknown + " not found\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	requireNoFile(t, path)
}

func TestExportNoSnapshot(t *testing.T) {
	_, pool := exportDatabase(t)
	l := pgtest.LoadHistory(t, pool, readGoldenFile(t, "worked-example.jsonl"), []uint64{})
	path := filepath.Join(t.TempDir(), "round.jsonl")
	code, stdout, stderr := exportCmd(t, string(canonjson.UUID(l.RoundID)), path)
	if code != exitFailed || stdout != "" || !strings.Contains(stderr, "has no snapshot") {
		t.Errorf("exit %d, stdout %q, stderr %q; want %d naming the missing snapshot", code, stdout, stderr, exitFailed)
	}
	requireOneExportLine(t, stderr)
	requireNoFile(t, path)
}

func TestExportCorruptRound(t *testing.T) {
	t.Run("secret deleted", func(t *testing.T) {
		_, pool := exportDatabase(t)
		l := pgtest.LoadHistory(t, pool, readGoldenFile(t, "worked-example.jsonl"), nil)
		replicaExec(t, pool, `DELETE FROM orvex.round_secrets WHERE round_id = $1`, l.RoundID)
		path := filepath.Join(t.TempDir(), "round.jsonl")
		code, stdout, stderr := exportCmd(t, string(canonjson.UUID(l.RoundID)), path)
		if code != exitFailed || stdout != "" || !strings.Contains(stderr, "violates an export invariant") {
			t.Errorf("exit %d, stdout %q, stderr %q; want %d naming the export invariant", code, stdout, stderr, exitFailed)
		}
		requireOneExportLine(t, stderr)
		requireNoFile(t, path)
	})
	t.Run("secret replaced", func(t *testing.T) {
		_, pool := exportDatabase(t)
		l := pgtest.LoadHistory(t, pool, readGoldenFile(t, "worked-example.jsonl"), nil)
		replicaExec(t, pool, `UPDATE orvex.round_secrets SET config_secret = $2 WHERE round_id = $1`,
			l.RoundID, []byte(`{"seed":"`+strings.Repeat("f", 64)+`"}`))
		round := string(canonjson.UUID(l.RoundID))
		dir := t.TempDir()
		path := filepath.Join(dir, "round.jsonl")
		code, stdout, stderr := exportCmd(t, round, path)
		if code != exitFailed || stdout != "" || !strings.Contains(stderr, "fairness failed at sequence 0") ||
			!strings.Contains(stderr, "-unsettled") {
			t.Errorf("exit %d, stdout %q, stderr %q; want %d, a fairness failure and the -unsettled hint", code, stdout, stderr, exitFailed)
		}
		requireOneExportLine(t, stderr)
		requireNoFile(t, path)

		code, stdout, stderr = exportCmd(t, "-unsettled", round, path)
		if code != exitOK || stderr != "" {
			t.Fatalf("-unsettled: exit %d, stderr %q", code, stderr)
		}
		if !strings.Contains(stdout, "\nsettled unknown\nwarning: the file holds the round's secret config before settlement; keep it private\nok\n") {
			t.Errorf("report = %q, want settled unknown and the warning", stdout)
		}
		code, _, stderr = runAuditCmd(t, strings.NewReader(""), "verify", path)
		if code != exitFailed || !strings.Contains(stderr, "fairness failed") {
			t.Errorf("audit verify: exit %d, stderr %q; want 1 at stage fairness", code, stderr)
		}
	})
}

func TestExportUnsupportedRules(t *testing.T) {
	_, pool := exportDatabase(t)
	l := pgtest.LoadHistory(t, pool, readGoldenFile(t, "tamper/rules-version-2.jsonl"), nil)
	round := string(canonjson.UUID(l.RoundID))
	path := filepath.Join(t.TempDir(), "round.jsonl")
	code, stdout, stderr := exportCmd(t, round, path)
	if code != exitUnsupported || stdout != "" || !strings.Contains(stderr, "rules unsupported at sequence 0") {
		t.Errorf("exit %d, stdout %q, stderr %q; want %d naming the unsupported rules", code, stdout, stderr, exitUnsupported)
	}
	requireOneExportLine(t, stderr)
	requireNoFile(t, path)

	code, stdout, stderr = exportCmd(t, "-unsettled", round, path)
	if code != exitOK || stderr != "" || !strings.Contains(stdout, "\nsettled unknown\n") {
		t.Fatalf("-unsettled: exit %d, stdout %q, stderr %q; want 0 and settled unknown", code, stdout, stderr)
	}
	if code, _, stderr := runAuditCmd(t, strings.NewReader(""), "verify", path); code != exitUnsupported {
		t.Errorf("audit verify: exit %d, stderr %q; want %d", code, stderr, exitUnsupported)
	}
}

func TestExportDatabaseErrors(t *testing.T) {
	t.Run("no schema", func(t *testing.T) {
		t.Setenv(databaseURLEnv, pgtest.EmptyDatabase(t))
		path := filepath.Join(t.TempDir(), "round.jsonl")
		code, stdout, stderr := exportCmd(t, exportRoundID, path)
		if code != exitIO || stdout != "" || !strings.Contains(stderr, "42P01") {
			t.Errorf("exit %d, stdout %q, stderr %q; want %d with 42P01", code, stdout, stderr, exitIO)
		}
		requireOneExportLine(t, stderr)
		requireNoFile(t, path)
	})
	t.Run("missing grant", func(t *testing.T) {
		dsn, pool := exportDatabase(t)
		g := drained(t, pool, "worked-example.jsonl")
		var grants []string
		for _, s := range exportGrants {
			if !strings.Contains(s, "round_secrets") {
				grants = append(grants, s)
			}
		}
		if len(grants) != len(exportGrants)-1 {
			t.Fatalf("grants without round_secrets: %v", grants)
		}
		t.Setenv(databaseURLEnv, restrictedRole(t, dsn, grants))
		path := filepath.Join(t.TempDir(), "round.jsonl")
		code, stdout, stderr := exportCmd(t, g.UUID(), path)
		if code != exitIO || stdout != "" || !strings.Contains(stderr, "42501") {
			t.Errorf("exit %d, stdout %q, stderr %q; want %d with 42501", code, stdout, stderr, exitIO)
		}
		if strings.Contains(stdout, exportRolePassword) || strings.Contains(stderr, exportRolePassword) {
			t.Errorf("output contains the password: stdout %q, stderr %q", stdout, stderr)
		}
		requireOneExportLine(t, stderr)
		requireNoFile(t, path)
	})
}

func TestExportMinimalGrants(t *testing.T) {
	dsn, pool := exportDatabase(t)
	g := drained(t, pool, "worked-example.jsonl")
	roleDSN := restrictedRole(t, dsn, exportGrants)
	t.Setenv(databaseURLEnv, roleDSN)

	path := filepath.Join(t.TempDir(), "round.jsonl")
	code, stdout, stderr := exportCmd(t, g.UUID(), path)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	if want := wantExportReport(g.UUID(), 21, 22, g.Hash(21).String(), settledYes); stdout != want {
		t.Errorf("report =\n%s\nwant\n%s", stdout, want)
	}
	verifyFile(t, path, 22, g.Hash(21).String())

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, roleDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, sql := range []string{
		`SELECT state FROM orvex.round_snapshots LIMIT 1`,
		`SELECT 1 FROM orvex.admissions LIMIT 1`,
		`SELECT close_offset_micros FROM orvex.round_secrets LIMIT 1`,
	} {
		var v any
		err := conn.QueryRow(ctx, sql).Scan(&v)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s as the export role: %v, want SQLSTATE 42501", sql, err)
		}
	}
}

func TestExportOutputFailures(t *testing.T) {
	_, pool := exportDatabase(t)
	g := drained(t, pool, "worked-example.jsonl")

	t.Run("stdout", func(t *testing.T) {
		var stderr bytes.Buffer
		code := run([]string{"audit", "export", g.UUID(), "-"}, strings.NewReader(""), multilineFailingWriter{}, &stderr)
		if code != exitIO {
			t.Errorf("exit %d, want %d", code, exitIO)
		}
		requireOneExportLine(t, stderr.String())
		if !strings.Contains(stderr.String(), "write stdout: disk full: sector 7") {
			t.Errorf("stderr = %q, want the flattened writer error", stderr.String())
		}
	})
	t.Run("report", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "round.jsonl")
		var stderr bytes.Buffer
		code := run([]string{"audit", "export", g.UUID(), path}, strings.NewReader(""), multilineFailingWriter{}, &stderr)
		if code != exitIO {
			t.Errorf("exit %d, want %d", code, exitIO)
		}
		requireOneExportLine(t, stderr.String())
		if !strings.Contains(stderr.String(), "write report: disk full: sector 7") {
			t.Errorf("stderr = %q, want the report write error", stderr.String())
		}
		requireNoFile(t, path)
	})
}

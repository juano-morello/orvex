package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/canonjson"
	crashv1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// exportRoundID is a well-formed round ID, the worked-example golden round's.
const exportRoundID = "0190f3a8-7b3c-7d4e-8f00-000000000001"

// exportCmd runs crashctl audit export with args and returns its exit code,
// stdout and stderr.
func exportCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"audit", "export"}, args...), strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// requireNoFile fails unless nothing exists at path.
func requireNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists after the run (Lstat: %v), want no file", path, err)
	}
}

// requireOneExportLine fails unless stderr is exactly one line with the
// crashctl: audit export: prefix.
func requireOneExportLine(t *testing.T, stderr string) {
	t.Helper()
	if !strings.HasPrefix(stderr, "crashctl: audit export: ") || strings.Count(stderr, "\n") != 1 ||
		!strings.HasSuffix(stderr, "\n") {
		t.Errorf("stderr = %q, want one crashctl: audit export: line", stderr)
	}
}

func TestExportUsage(t *testing.T) {
	file := filepath.Join(t.TempDir(), "out.jsonl")
	tests := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"one positional", []string{exportRoundID}},
		{"three positionals", []string{exportRoundID, file, "extra"}},
		{"flag after positionals", []string{exportRoundID, file, "-unsettled"}},
		{"unknown flag", []string{"-bogus", exportRoundID, file}},
		{"uppercase ROUND_ID", []string{strings.ToUpper(exportRoundID), file}},
		{"braced ROUND_ID", []string{"{" + exportRoundID + "}", file}},
		{"urn ROUND_ID", []string{"urn:uuid:" + exportRoundID, file}},
		{"unhyphenated ROUND_ID", []string{strings.ReplaceAll(exportRoundID, "-", ""), file}},
		{"35-character ROUND_ID", []string{exportRoundID[:35], file}},
		{"empty ROUND_ID", []string{"", file}},
		{"nil ROUND_ID", []string{"00000000-0000-0000-0000-000000000000", file}},
		{"empty -format", []string{"-format", "", exportRoundID, file}},
		{"short -format", []string{"-format", "v1", exportRoundID, file}},
		{"leading-zero -format", []string{"-format", "orvex/audit-history/v01", exportRoundID, file}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(databaseURLEnv, unreachableDSN)
			code, stdout, stderr := exportCmd(t, tt.args...)
			if code != exitUsage {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, exitUsage, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if stderr == "" {
				t.Error("stderr is empty")
			}
			requireNoFile(t, file)
		})
	}

	for name, value := range map[string]*string{"unset": nil, "empty": new(""), "unparsable": new("::bad")} {
		t.Run("ORVEX_DATABASE_URL "+name, func(t *testing.T) {
			t.Setenv(databaseURLEnv, "") // restores the variable at cleanup
			if value == nil {
				if err := os.Unsetenv(databaseURLEnv); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv(databaseURLEnv, *value)
			}
			code, stdout, stderr := exportCmd(t, exportRoundID, file)
			if code != exitUsage || stdout != "" || !strings.Contains(stderr, databaseURLEnv) {
				t.Errorf("exit code = %d, stdout %q, stderr %q; want %d naming %s", code, stdout, stderr, exitUsage, databaseURLEnv)
			}
			if name == "unparsable" {
				if want := "crashctl: audit export: invalid ORVEX_DATABASE_URL\n"; stderr != want {
					t.Errorf("stderr = %q, want %q", stderr, want)
				}
			}
			requireNoFile(t, file)
		})
	}

	for _, help := range []string{"-h", "-help"} {
		t.Run(help, func(t *testing.T) {
			t.Setenv(databaseURLEnv, unreachableDSN)
			code, stdout, stderr := exportCmd(t, help)
			if code != exitOK || stdout != "" {
				t.Errorf("exit code = %d, stdout %q; want %d and empty", code, stdout, exitOK)
			}
			if !strings.Contains(stderr, "usage: crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE") {
				t.Errorf("stderr = %q, want the usage", stderr)
			}
		})
	}
}

func TestExportFormatUnsupported(t *testing.T) {
	t.Setenv(databaseURLEnv, unreachableDSN)
	file := filepath.Join(t.TempDir(), "out.jsonl")
	code, stdout, stderr := exportCmd(t, "-format", "orvex/audit-history/v2", exportRoundID, file)
	if code != exitUnsupported || stdout != "" {
		t.Errorf("exit code = %d, stdout %q; want %d and empty", code, stdout, exitUnsupported)
	}
	want := "crashctl: audit export: format orvex/audit-history/v2 unsupported: this build writes orvex/audit-history/v1\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	requireNoFile(t, file)
}

func TestExportFileExists(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.jsonl")
	if err := os.WriteFile(existing, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.jsonl")
	link := filepath.Join(dir, "dangling.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"file": existing, "directory": subdir, "dangling symlink": link} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(databaseURLEnv, unreachableDSN)
			code, stdout, stderr := exportCmd(t, exportRoundID, path)
			if code != exitUsage || stdout != "" {
				t.Errorf("exit code = %d, stdout %q; want %d and empty (stderr %q)", code, stdout, exitUsage, stderr)
			}
			requireOneExportLine(t, stderr)
			if !strings.Contains(stderr, "exists; refusing to overwrite") {
				t.Errorf("stderr = %q, want the refusal", stderr)
			}
		})
	}
	if b, err := os.ReadFile(existing); err != nil || string(b) != "keep me\n" { //nolint:gosec // G304: path is a file this test created under t.TempDir()
		t.Errorf("existing file = %q, %v; want it unchanged", b, err)
	}
	requireNoFile(t, target)
}

func TestExportMissingDirectory(t *testing.T) {
	// A password that occurs in no path keeps the directory readable after
	// masking.
	t.Setenv(databaseURLEnv, "postgres://u:sentinel-pw@127.0.0.1:1/db?connect_timeout=2")
	dir := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(dir, "out.jsonl")
	code, stdout, stderr := exportCmd(t, exportRoundID, file)
	if code != exitIO || stdout != "" {
		t.Errorf("exit code = %d, stdout %q; want %d and empty", code, stdout, exitIO)
	}
	requireOneExportLine(t, stderr)
	if !strings.Contains(stderr, "directory "+dir+" is not usable") {
		t.Errorf("stderr = %q, want it to name directory %s", stderr, dir)
	}
	if strings.Contains(stderr, "connect") || strings.Contains(stderr, "127.0.0.1") {
		t.Errorf("stderr = %q, want no connection error", stderr)
	}
	requireNoFile(t, file)
}

func TestExportConnectError(t *testing.T) {
	for _, target := range []string{"path", "-"} {
		t.Run(target, func(t *testing.T) {
			t.Setenv(databaseURLEnv, unreachableDSN)
			file := filepath.Join(t.TempDir(), "out.jsonl")
			arg := file
			if target == "-" {
				arg = "-"
			}
			code, stdout, stderr := exportCmd(t, exportRoundID, arg)
			if code != exitIO || stdout != "" {
				t.Errorf("exit code = %d, stdout %q; want %d and empty (stderr %q)", code, stdout, exitIO, stderr)
			}
			requireOneExportLine(t, stderr)
			requireNoFile(t, file)
		})
	}
}

func TestExportNeverPrintsPassword(t *testing.T) {
	const secret = "sentinel-pw"
	for name, dsn := range map[string]string{
		"unparsable":  "postgres://u:" + secret + "@127.0.0.1:1/db?connect_timeout=x",
		"unreachable": "postgres://u:" + secret + "@127.0.0.1:1/db?connect_timeout=2",
		"key=value":   "host=127.0.0.1 port=1 user=u password=" + secret + " dbname=db connect_timeout=2",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(databaseURLEnv, dsn)
			for _, target := range []string{filepath.Join(t.TempDir(), "out.jsonl"), "-"} {
				code, stdout, stderr := exportCmd(t, exportRoundID, target)
				if code != exitUsage && code != exitIO {
					t.Errorf("%s: exit code = %d, want %d or %d", target, code, exitUsage, exitIO)
				}
				if strings.Contains(stderr, secret) || strings.Contains(stdout, secret) {
					t.Errorf("%s: output contains the password: stdout %q, stderr %q", target, stdout, stderr)
				}
			}
		})
	}
}

func TestExportConnConfig(t *testing.T) {
	tests := []struct {
		dsn                     string
		readOnly, appName, idle string
		connectTimeout          time.Duration
	}{
		{"postgres://u:p@127.0.0.1:1/db", "on", "crashctl", "60000", 10 * time.Second},
		{"postgres://u:p@127.0.0.1:1/db?default_transaction_read_only=off&application_name=x&idle_in_transaction_session_timeout=5000&connect_timeout=3",
			"on", "x", "5000", 3 * time.Second},
	}
	for _, tt := range tests {
		t.Setenv(databaseURLEnv, tt.dsn)
		cfg, err := databaseConfig()
		if err != nil {
			t.Fatalf("databaseConfig(%s): %v", tt.dsn, err)
		}
		exportConnConfig(cfg)
		p := cfg.RuntimeParams
		if p["default_transaction_read_only"] != tt.readOnly || p["application_name"] != tt.appName ||
			p["idle_in_transaction_session_timeout"] != tt.idle || cfg.ConnectTimeout != tt.connectTimeout {
			t.Errorf("%s: runtime params %v, connect timeout %v; want read only %s, application %s, idle %s, timeout %v",
				tt.dsn, p, cfg.ConnectTimeout, tt.readOnly, tt.appName, tt.idle, tt.connectTimeout)
		}
	}
}

func TestExportExitCodes(t *testing.T) {
	wrapped := func(err error) error { return fmt.Errorf("eventstore: read secret: %w", err) }
	finalErr := errors.New("execution: state has the wrong type for the crash rules")
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, exitOK},
		{"round not found", eventstore.ErrRoundNotFound, exitNotFound},
		{"no checkpoint", fmt.Errorf("x: %w", eventstore.ErrNoCheckpoint), exitFailed},
		{"corrupt", fmt.Errorf("%w: snapshot 3", eventstore.ErrCorrupt), exitFailed},
		{"42501", wrapped(&pgconn.PgError{Code: "42501"}), exitIO},
		{"42P01", wrapped(&pgconn.PgError{Code: "42P01"}), exitIO},
		{"40001", wrapped(&pgconn.PgError{Code: "40001"}), exitIO},
		{"08006", wrapped(&pgconn.PgError{Code: "08006"}), exitIO},
		{"unexpected EOF", wrapped(io.ErrUnexpectedEOF), exitIO},
		{"unsupported failure", &verification.Failure{Stage: verification.StageRules, Unsupported: true, Err: errors.New("x")}, exitUnsupported},
		{"unsupported failure in check", fmt.Errorf("%w: %w", errSettlementCheck,
			&verification.Failure{Stage: verification.StageRules, Unsupported: true, Err: errors.New("x")}), exitUnsupported},
		{"no policy", fmt.Errorf("%w: %w", errSettlementCheck, fmt.Errorf("%w for crash rules 9", errNoPolicy)), exitUnsupported},
		{"format unsupported", errFormatUnsupported, exitUnsupported},
		{"failure in check", fmt.Errorf("%w: %w", errSettlementCheck,
			&verification.Failure{Stage: verification.StageFairness, Err: errors.New("x")}), exitFailed},
		{"final error", fmt.Errorf("%w: %w", errSettlementCheck, finalErr), exitFailed},
		{"not settled", errNotSettled, exitNotSettled},
		{"output exists", errOutputExists, exitUsage},
	}
	for _, tt := range tests {
		if got := exportExitCode(tt.err); got != tt.want {
			t.Errorf("%s: exportExitCode(%v) = %d, want %d", tt.name, tt.err, got, tt.want)
		}
	}
}

// readGoldenFile returns a golden history file.
func readGoldenFile(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, file)) //nolint:gosec // G304: fixed golden fixture under testdata
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// historyLines splits a history file into its header and record lines.
func historyLines(b []byte) [][]byte {
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

// finalErrPolicy is crash/v1's policy with a failing Final.
type finalErrPolicy struct{ execution.Policy }

var errFinalBoom = errors.New("execution: final boom")

func (finalErrPolicy) Final(any) (bool, error) { return false, errFinalBoom }

func TestSettlement(t *testing.T) {
	reg, err := newRegistry()
	if err != nil {
		t.Fatal(err)
	}
	m := loadManifest(t)
	var worked *struct {
		sequences []uint64
		hashes    []string
	}
	for _, h := range m.Histories {
		if h.Name == "worked-example" {
			worked = &struct {
				sequences []uint64
				hashes    []string
			}{}
			for _, cp := range h.Checkpoints {
				worked.sequences = append(worked.sequences, cp.Sequence)
				worked.hashes = append(worked.hashes, cp.StateHash)
			}
		}
	}
	if worked == nil || len(worked.sequences) != 3 || worked.sequences[0] != 0 || worked.sequences[1] != 9 {
		t.Fatalf("worked-example manifest checkpoints = %+v, want 0, 9, 22", worked)
	}

	// worked-example cut after record 9 (RoundStarted): a valid history that
	// is not settled.
	lines := historyLines(readGoldenFile(t, "worked-example.jsonl"))
	header, err := canonjson.Unmarshal(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	head := chain.LinkHash(lines[10])
	if want := "507826d44f608a6733aa3c579967ce4f4179546298372b8134a390c20aaa9bc0"; head.String() != want {
		t.Fatalf("worked-example record 9 link hash = %s, want %s", head, want)
	}
	header["head"] = canonjson.Hex(head[:])
	header["checkpoints"] = canonjson.Array{
		canonjson.Object{"sequence": canonjson.Uint(0), "stateHash": canonjson.String(worked.hashes[0])},
		canonjson.Object{"sequence": canonjson.Uint(9), "stateHash": canonjson.String(worked.hashes[1])},
	}
	cutHeader, err := canonjson.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	cut := append(append(cutHeader, '\n'), bytes.Join(lines[1:11], []byte("\n"))...)
	cut = append(cut, '\n')

	tests := []struct {
		name      string
		history   []byte
		policies  []execution.Policy
		want      settledVerdict
		stage     verification.Stage // a *Failure at this stage, if set
		unsup     bool
		wantError error // errors.Is target, if set
	}{
		{name: "worked-example", history: readGoldenFile(t, "worked-example.jsonl"), policies: settlementPolicies(), want: settledYes},
		{name: "in-progress", history: readGoldenFile(t, "in-progress.jsonl"), policies: settlementPolicies(), want: settledNo},
		{name: "worked-example cut at 9", history: cut, policies: settlementPolicies(), want: settledNo},
		{name: "rules version 2", history: readGoldenFile(t, "tamper/rules-version-2.jsonl"), policies: settlementPolicies(),
			want: settledUnknown, stage: verification.StageRules, unsup: true},
		{name: "commitment edited", history: readGoldenFile(t, "tamper/commitment-edited.jsonl"), policies: settlementPolicies(),
			want: settledUnknown, stage: verification.StageFairness},
		{name: "no policy", history: readGoldenFile(t, "worked-example.jsonl"), policies: nil,
			want: settledUnknown, wantError: errNoPolicy},
		{name: "final error", history: readGoldenFile(t, "worked-example.jsonl"),
			policies: []execution.Policy{finalErrPolicy{execution.CrashV1Policy()}}, want: settledUnknown, wantError: errFinalBoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := settlement(tt.history, reg, tt.policies)
			if got != tt.want {
				t.Fatalf("settlement = %s (%v), want %s", got, err, tt.want)
			}
			if tt.want != settledUnknown {
				if err != nil {
					t.Fatalf("settlement error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, errSettlementCheck) {
				t.Errorf("settlement error = %v, want it to wrap errSettlementCheck", err)
			}
			if tt.stage != "" {
				var f *verification.Failure
				if !errors.As(err, &f) || f.Stage != tt.stage || f.Unsupported != tt.unsup {
					t.Errorf("settlement error = %v, want a *Failure at %s (unsupported %v)", err, tt.stage, tt.unsup)
				}
			}
			if tt.wantError != nil && !errors.Is(err, tt.wantError) {
				t.Errorf("settlement error = %v, want it to wrap %v", err, tt.wantError)
			}
		})
	}

	_, err = settlement(readGoldenFile(t, "worked-example.jsonl"), reg, nil)
	d := crashv1.Descriptor()
	if got, want := settlementReason(err), fmt.Sprintf("no settlement policy for %s rules %d", d.Game, d.RulesVersion); got != want {
		t.Errorf("settlementReason = %q, want %q", got, want)
	}
}

func TestSettlementPoliciesCoverRegistry(t *testing.T) {
	reg, err := newRegistry()
	if err != nil {
		t.Fatal(err)
	}
	policies := settlementPolicies()
	seen := map[any]bool{}
	for _, p := range policies {
		d := p.Descriptor()
		if seen[d] {
			t.Errorf("two policies for %+v", d)
		}
		seen[d] = true
		a, err := reg.Lookup(d.Game, d.RulesVersion)
		if err != nil {
			t.Errorf("policy %+v has no adapter: %v", d, err)
			continue
		}
		if a.Descriptor() != d {
			t.Errorf("adapter descriptor %+v, policy %+v", a.Descriptor(), d)
		}
	}
	want := crashv1.Adapter().Descriptor()
	n := 0
	for _, p := range policies {
		if p.Descriptor() == want {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d policies for %+v, want exactly 1", n, want)
	}
}

func TestExportReport(t *testing.T) {
	var roundID [16]byte
	copy(roundID[:], []byte{0x01, 0x90, 0xf3, 0xa8, 0x7b, 0x3c, 0x7d, 0x4e, 0x8f, 0, 0, 0, 0, 0, 0, 0x01})
	var head chain.Hash
	for i := range head {
		head[i] = byte(i)
	}
	const hexHead = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	exporterLine := "exporter " + buildinfo.String("crashctl") + "\n"
	const note = "note: the file ends at the newest snapshot, sequence 21; sequences 22 to 22 are not exported\n"
	const warning = "warning: the file holds the round's secret config before settlement; keep it private\n"
	tests := []struct {
		name    string
		exp     eventstore.Export
		settled settledVerdict
		want    string
	}{
		{"settled complete", eventstore.Export{RoundID: roundID, Records: 6, RoundRecords: 6, Head: head}, settledYes,
			exporterLine + "format orvex/audit-history/v1\nround " + exportRoundID + "\nrecords 6 of 6 head " + hexHead +
				"\nsettled yes\nok\n"},
		{"settled partial", eventstore.Export{RoundID: roundID, Records: 22, RoundRecords: 23, Head: head}, settledYes,
			exporterLine + "format orvex/audit-history/v1\nround " + exportRoundID + "\nrecords 22 of 23 head " + hexHead +
				"\n" + note + "settled yes\nok\n"},
		{"not settled partial", eventstore.Export{RoundID: roundID, Records: 22, RoundRecords: 23, Head: head}, settledNo,
			exporterLine + "format orvex/audit-history/v1\nround " + exportRoundID + "\nrecords 22 of 23 head " + hexHead +
				"\n" + note + "settled no\n" + warning + "ok\n"},
		{"unknown complete", eventstore.Export{RoundID: roundID, Records: 23, RoundRecords: 23, Head: head}, settledUnknown,
			exporterLine + "format orvex/audit-history/v1\nround " + exportRoundID + "\nrecords 23 of 23 head " + hexHead +
				"\nsettled unknown\n" + warning + "ok\n"},
	}
	for _, tt := range tests {
		if got := string(exportReport(verification.Format, tt.exp, tt.settled)); got != tt.want {
			t.Errorf("%s: report =\n%s\nwant\n%s", tt.name, got, tt.want)
		}
	}
}

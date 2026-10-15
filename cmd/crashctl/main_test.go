package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/buildinfo"
)

const goldenDir = "../../internal/audit/verification/testdata/golden"

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

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
		{name: "no args", args: nil, wantCode: 2, wantStderr: "usage: crashctl"},
		{name: "positional", args: []string{"extra"}, wantCode: 2, wantStderr: "unexpected arguments"},
		{name: "unknown flag", args: []string{"-bogus"}, wantCode: 2, stderrAny: true},
		{name: "help", args: []string{"-h"}, wantCode: 0, stderrAny: true},
		{name: "usage lists migrate", args: nil, wantCode: 2, wantStderr: "crashctl migrate up [-to N]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, strings.NewReader(""), &stdout, &stderr)
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
	if code := run([]string{"-version"}, strings.NewReader(""), failingWriter{}, io.Discard); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// manifest is the part of the golden manifest.json these tests use.
type manifest struct {
	Histories []struct {
		Name        string `json:"name"`
		File        string `json:"file"`
		Records     int    `json:"records"`
		Commands    int    `json:"commands"`
		Accepted    int    `json:"accepted"`
		Rejected    int    `json:"rejected"`
		Events      int    `json:"events"`
		Head        string `json:"head"`
		Checkpoints []struct {
			Sequence  uint64 `json:"sequence"`
			StateHash string `json:"stateHash"`
		} `json:"checkpoints"`
	} `json:"histories"`
	Tamper []struct {
		Name   string `json:"name"`
		File   string `json:"file"`
		Checks []struct {
			Command  string   `json:"command"`
			Args     []string `json:"args"`
			Exit     int      `json:"exit"`
			Stage    *string  `json:"stage"`
			Sequence *string  `json:"sequence"`
		} `json:"checks"`
	} `json:"tamper"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Histories) != 4 || len(m.Tamper) != 26 {
		t.Fatalf("manifest has %d histories and %d tamper cases", len(m.Histories), len(m.Tamper))
	}
	return m
}

func runAuditCmd(t *testing.T, stdin io.Reader, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"audit"}, args...), stdin, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAuditGolden(t *testing.T) {
	m := loadManifest(t)
	for _, h := range m.Histories {
		path := filepath.Join(goldenDir, h.File)
		for _, sub := range []string{"verify", "replay", "chain"} {
			t.Run(h.Name+"/"+sub, func(t *testing.T) {
				code, stdout, stderr := runAuditCmd(t, nil, sub, path)
				if code != 0 || stderr != "" {
					t.Fatalf("exit %d, stderr %q", code, stderr)
				}
				lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
				if lines[0] != "verifier "+buildinfo.String(name) || lines[len(lines)-1] != "ok" {
					t.Errorf("stdout = %q", stdout)
				}
				wantChain := "chain ok records " + strconv.Itoa(h.Records) + " head " + h.Head
				if !strings.Contains(stdout, "\n"+wantChain+"\n") {
					t.Errorf("stdout lacks %q:\n%s", wantChain, stdout)
				}
				if got := strings.Contains(stdout, "decision-replay ok"); got != (sub == "verify") {
					t.Errorf("decision-replay line present = %v for %s", got, sub)
				}
				if got := strings.Contains(stdout, "state-replay ok"); got != (sub != "chain") {
					t.Errorf("state-replay lines present = %v for %s", got, sub)
				}
			})
		}
	}

	// The full verify report of the worked example (spec section 7).
	h := m.Histories[0]
	code, stdout, _ := runAuditCmd(t, nil, "verify", filepath.Join(goldenDir, h.File))
	want := "verifier " + buildinfo.String(name) + "\n" +
		"format orvex/audit-history/v1 chain orvex/audit/v1\n" +
		"round 0190f3a8-7b3c-7d4e-8f00-000000000001\n" +
		"rules crash 1 schema 1 state 1\n" +
		"chain ok records 23 head " + h.Head + "\n" +
		"fairness ok\n" +
		"decision-replay ok commands 22 accepted 8 rejected 14 events 9\n"
	for _, cp := range h.Checkpoints {
		want += "state-replay ok sequence " + strconv.FormatUint(cp.Sequence, 10) + " state " + cp.StateHash + "\n"
	}
	want += "ok\n"
	if code != 0 || stdout != want {
		t.Errorf("verify worked-example: exit %d, stdout\n%s\nwant\n%s", code, stdout, want)
	}
}

func TestAuditReplayStateHash(t *testing.T) {
	m := loadManifest(t)
	h := m.Histories[0]
	code, stdout, stderr := runAuditCmd(t, nil, "replay", filepath.Join(goldenDir, h.File))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var got []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "state-replay ") {
			got = append(got, line)
		}
	}
	var want []string
	for _, cp := range h.Checkpoints {
		want = append(want, "state-replay ok sequence "+strconv.FormatUint(cp.Sequence, 10)+" state "+cp.StateHash)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") || len(want) != 3 {
		t.Fatalf("state-replay lines = %q; want %q", got, want)
	}
}

// TestAuditFailures walks every manifest tamper check through the CLI.
func TestAuditFailures(t *testing.T) {
	m := loadManifest(t)
	for _, tc := range m.Tamper {
		path := filepath.Join(goldenDir, tc.File)
		for _, c := range tc.Checks {
			t.Run(tc.Name+"/"+c.Command+strings.Join(c.Args, ""), func(t *testing.T) {
				args := append(append([]string{c.Command}, c.Args...), path)
				code, stdout, stderr := runAuditCmd(t, nil, args...)
				if code != c.Exit {
					t.Fatalf("exit %d, want %d; stderr %q", code, c.Exit, stderr)
				}
				if c.Exit == 0 {
					if !strings.HasSuffix(stdout, "\nok\n") || stderr != "" {
						t.Fatalf("stdout %q, stderr %q", stdout, stderr)
					}
					return
				}
				verdict := "failed"
				if c.Exit == 3 {
					verdict = "unsupported"
				}
				where := "header"
				if *c.Sequence != "header" {
					where = "sequence " + *c.Sequence
				}
				want := "crashctl: audit " + c.Command + ": " + *c.Stage + " " + verdict + " at " + where + ": "
				if stdout != "" || !strings.HasPrefix(stderr, want) || strings.Count(stderr, "\n") != 1 {
					t.Fatalf("stdout %q, stderr %q; want one stderr line starting %q", stdout, stderr, want)
				}
			})
		}
	}

	// Named cases of spec section 10 step 8.
	code, _, stderr := runAuditCmd(t, nil, "verify", filepath.Join(goldenDir, "tamper/payout-edited.jsonl"))
	if code != 1 || !strings.Contains(stderr, "decision-replay failed at sequence 12") {
		t.Errorf("R3: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = runAuditCmd(t, nil, "verify", filepath.Join(goldenDir, "tamper/rules-version-2.jsonl"))
	if code != 3 || !strings.Contains(stderr, "rules unsupported at sequence 0") {
		t.Errorf("R15: exit %d, stderr %q", code, stderr)
	}
	r26 := filepath.Join(goldenDir, "tamper/delete-rejection.jsonl")
	if code, _, stderr = runAuditCmd(t, nil, "verify", r26); code != 0 {
		t.Errorf("R26 without -expect-head: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = runAuditCmd(t, nil, "verify", "-expect-head", m.Histories[0].Head, r26)
	if code != 1 || !strings.Contains(stderr, "chain failed at sequence 21") {
		t.Errorf("R26 with -expect-head: exit %d, stderr %q", code, stderr)
	}

	// B1 through a temp file: one payout digit changed without rehashing.
	data, err := os.ReadFile(filepath.Join(goldenDir, "worked-example.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	old := []byte(`"payoutCredits":"134"`)
	if bytes.Count(lines[13], old) != 1 {
		t.Fatal("record 12 payout not found")
	}
	lines[13] = bytes.Replace(lines[13], old, []byte(`"payoutCredits":"135"`), 1)
	b1 := filepath.Join(t.TempDir(), "b1.jsonl")
	if err := os.WriteFile(b1, bytes.Join(lines, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runAuditCmd(t, nil, "verify", b1)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "chain failed at sequence 13") {
		t.Errorf("B1: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestAuditUsage(t *testing.T) {
	golden := filepath.Join(goldenDir, "worked-example.jsonl")
	head := strings.Repeat("ab", 32)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"audit alone", []string{"audit"}, 2},
		{"unknown subcommand", []string{"audit", "check", golden}, 2},
		{"no file", []string{"audit", "verify"}, 2},
		{"two files", []string{"audit", "verify", golden, golden}, 2},
		{"expect-head not hex", []string{"audit", "verify", "-expect-head", strings.Repeat("zz", 32), golden}, 2},
		{"expect-head uppercase", []string{"audit", "verify", "-expect-head", strings.ToUpper(head), golden}, 2},
		{"expect-head short", []string{"audit", "chain", "-expect-head", head[:62], golden}, 2},
		{"expect-head empty", []string{"audit", "replay", "-expect-head", "", golden}, 2},
		{"expect-head missing value", []string{"audit", "verify", "-expect-head"}, 2},
		{"unknown flag", []string{"audit", "verify", "-bogus", golden}, 2},
		{"flag after FILE", []string{"audit", "verify", golden, "-expect-head", head}, 2},
		{"version with audit", []string{"-version", "audit", "verify", golden}, 2},
		{"subcommand help", []string{"audit", "verify", "-h"}, 0},
		{"subcommand -help", []string{"audit", "chain", "-help"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, strings.NewReader(""), &stdout, &stderr)
			if code != tc.want {
				t.Fatalf("exit %d, want %d; stderr %q", code, tc.want, stderr.String())
			}
			if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Errorf("stdout %q, stderr %q; want usage on stderr only", stdout.String(), stderr.String())
			}
		})
	}
}

func TestAuditIO(t *testing.T) {
	golden := filepath.Join(goldenDir, "empty-round.jsonl")
	code, stdout, stderr := runAuditCmd(t, nil, "verify", filepath.Join(t.TempDir(), "missing.jsonl"))
	if code != 4 || stdout != "" || !strings.HasPrefix(stderr, "crashctl: audit verify: ") {
		t.Errorf("missing file: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	var stderrBuf bytes.Buffer
	if code := run([]string{"audit", "verify", golden}, nil, failingWriter{}, &stderrBuf); code != 4 {
		t.Errorf("failing stdout: exit %d, want 4; stderr %q", code, stderrBuf.String())
	}

	data, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"verify", "replay", "chain"} {
		code, stdout, stderr = runAuditCmd(t, bytes.NewReader(data), sub, "-")
		if code != 0 || !strings.HasSuffix(stdout, "\nok\n") || stderr != "" {
			t.Errorf("%s from stdin: exit %d, stdout %q, stderr %q", sub, code, stdout, stderr)
		}
	}
	code, stdout, stderr = runAuditCmd(t, failingReader{}, "chain", "-")
	if code != 4 || stdout != "" || !strings.Contains(stderr, "read failed") {
		t.Errorf("failing stdin: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

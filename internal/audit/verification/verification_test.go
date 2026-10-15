package verification_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
)

const goldenDir = "testdata/golden"

// manifest mirrors testdata/golden/manifest.json (spec section 8.3).
type manifest struct {
	Format    string `json:"format"`
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
		Name        string       `json:"name"`
		File        string       `json:"file"`
		Base        string       `json:"base"`
		Description string       `json:"description"`
		Checks      []checkEntry `json:"checks"`
	} `json:"tamper"`
}

type checkEntry struct {
	Command  string   `json:"command"`
	Args     []string `json:"args"`
	Exit     int      `json:"exit"`
	Stage    *string  `json:"stage"`
	Sequence *string  `json:"sequence"`
}

func loadManifest(t testing.TB) manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	if m.Format != verification.Format || len(m.Histories) != 4 || len(m.Tamper) != 26 {
		t.Fatalf("manifest: format %q, %d histories, %d tamper cases", m.Format, len(m.Histories), len(m.Tamper))
	}
	return m
}

func readGolden(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func registry(t testing.TB) *game.Registry {
	t.Helper()
	r, err := game.NewRegistry(v1.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func parseHash(t testing.TB, s string) chain.Hash {
	t.Helper()
	var h chain.Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(h) || strings.ToLower(s) != s {
		t.Fatalf("bad hash %q", s)
	}
	copy(h[:], b)
	return h
}

func modeOf(t testing.TB, command string) verification.Mode {
	t.Helper()
	switch command {
	case "chain":
		return verification.ModeChain
	case "replay":
		return verification.ModeReplay
	case "verify":
		return verification.ModeVerify
	}
	t.Fatalf("unknown command %q", command)
	return 0
}

func options(t testing.TB, command string, args []string) verification.Options {
	t.Helper()
	opt := verification.Options{Mode: modeOf(t, command), Registry: registry(t)}
	switch len(args) {
	case 0:
	case 2:
		if args[0] != "-expect-head" {
			t.Fatalf("unknown args %v", args)
		}
		h := parseHash(t, args[1])
		opt.ExpectHead = &h
	default:
		t.Fatalf("unknown args %v", args)
	}
	return opt
}

// wantOutcome checks err against a manifest-style expectation: exit 0 is
// success, 1 a failure, 3 an unsupported failure; seq is a decimal or
// "header".
func wantOutcome(t testing.TB, err error, exit int, stage, seq string) {
	t.Helper()
	if exit == 0 {
		if err != nil {
			t.Fatalf("got %v; want success", err)
		}
		return
	}
	var f *verification.Failure
	if !errors.As(err, &f) {
		t.Fatalf("got %v (%T); want a *Failure", err, err)
	}
	wantSeq := verification.HeaderSequence
	if seq != "header" {
		n, perr := strconv.ParseInt(seq, 10, 64)
		if perr != nil {
			t.Fatalf("bad expected sequence %q", seq)
		}
		wantSeq = n
	}
	if string(f.Stage) != stage || f.Sequence != wantSeq || f.Unsupported != (exit == 3) {
		t.Fatalf("got %v (stage %s, sequence %d, unsupported %v); want exit %d, %s at %s",
			f, f.Stage, f.Sequence, f.Unsupported, exit, stage, seq)
	}
	if exit != 1 && exit != 3 {
		t.Fatalf("bad expected exit %d", exit)
	}
}

func TestGoldenHistories(t *testing.T) {
	m := loadManifest(t)
	for _, h := range m.Histories {
		data := readGolden(t, h.File)
		for _, command := range []string{"chain", "replay", "verify"} {
			t.Run(h.Name+"/"+command, func(t *testing.T) {
				rep, err := verification.Verify(bytes.NewReader(data), options(t, command, nil))
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				if rep.Format != "orvex/audit-history/v1" || rep.Chain != "orvex/audit/v1" {
					t.Errorf("format %q chain %q", rep.Format, rep.Chain)
				}
				if rep.Records != h.Records || rep.Commands != h.Commands || rep.Accepted != h.Accepted ||
					rep.Rejected != h.Rejected || rep.Events != h.Events {
					t.Errorf("counts = %d/%d/%d/%d/%d; manifest %d/%d/%d/%d/%d",
						rep.Records, rep.Commands, rep.Accepted, rep.Rejected, rep.Events,
						h.Records, h.Commands, h.Accepted, h.Rejected, h.Events)
				}
				if rep.Head.String() != h.Head {
					t.Errorf("head = %s; manifest %s", rep.Head, h.Head)
				}
				if command == "chain" {
					if rep.Descriptor != (game.Descriptor{}) || len(rep.Checkpoints) != 0 {
						t.Errorf("chain mode report has descriptor %+v, checkpoints %v", rep.Descriptor, rep.Checkpoints)
					}
					return
				}
				if rep.Descriptor != v1.Descriptor() {
					t.Errorf("descriptor = %+v", rep.Descriptor)
				}
				if len(rep.Checkpoints) != len(h.Checkpoints) {
					t.Fatalf("checkpoints = %v; manifest %v", rep.Checkpoints, h.Checkpoints)
				}
				for i, cp := range h.Checkpoints {
					got := rep.Checkpoints[i]
					if got.Sequence != cp.Sequence || got.StateHash.String() != cp.StateHash {
						t.Errorf("checkpoint %d = %d %s; manifest %d %s", i, got.Sequence, got.StateHash, cp.Sequence, cp.StateHash)
					}
				}
			})
		}
	}
}

func TestTamperFixtures(t *testing.T) {
	m := loadManifest(t)
	for _, tc := range m.Tamper {
		data := readGolden(t, tc.File)
		for i, c := range tc.Checks {
			t.Run(tc.Name+"/"+strconv.Itoa(i)+"-"+c.Command, func(t *testing.T) {
				_, err := verification.Verify(bytes.NewReader(data), options(t, c.Command, c.Args))
				stage, seq := "", ""
				if c.Stage != nil {
					stage = *c.Stage
				}
				if c.Sequence != nil {
					seq = *c.Sequence
				}
				wantOutcome(t, err, c.Exit, stage, seq)
			})
		}
	}
}

// goldenLines splits a history into its lines, each with its '\n'. Line
// index i+1 holds sequence i; index 0 is the header.
func goldenLines(t testing.TB, data []byte) [][]byte {
	t.Helper()
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines[len(lines)-1]) != 0 {
		t.Fatal("history does not end with a newline")
	}
	return lines[:len(lines)-1]
}

func join(lines [][]byte) []byte {
	return bytes.Join(lines, nil)
}

func clone(lines [][]byte) [][]byte {
	out := make([][]byte, len(lines))
	for i, l := range lines {
		out[i] = bytes.Clone(l)
	}
	return out
}

// replaceOnce replaces the single occurrence of old in line.
func replaceOnce(t testing.TB, line []byte, old, new string) []byte {
	t.Helper()
	if n := bytes.Count(line, []byte(old)); n != 1 {
		t.Fatalf("%q occurs %d times in %s", old, n, line)
	}
	return bytes.Replace(line, []byte(old), []byte(new), 1)
}

// flipHexDigit replaces the hex digit at i with a different lowercase hex
// digit, keeping the line well-formed.
func flipHexDigit(t testing.TB, line []byte, i int) []byte {
	t.Helper()
	c := line[i]
	if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
		t.Fatalf("byte %q at %d is not a lowercase hex digit", c, i)
	}
	out := bytes.Clone(line)
	if c == '0' {
		out[i] = '1'
	} else {
		out[i] = '0'
	}
	return out
}

// valueStart returns the offset of the first character of the string value
// that follows prefix in line.
func valueStart(t testing.TB, line []byte, prefix string) int {
	t.Helper()
	if n := bytes.Count(line, []byte(prefix)); n != 1 {
		t.Fatalf("%q occurs %d times", prefix, n)
	}
	return bytes.Index(line, []byte(prefix)) + len(prefix)
}

func TestByteMutations(t *testing.T) {
	m := loadManifest(t)
	worked := m.Histories[0]
	if worked.Name != "worked-example" {
		t.Fatalf("first history is %s", worked.Name)
	}
	base := goldenLines(t, readGolden(t, worked.File))
	if len(base) != 24 {
		t.Fatalf("worked example has %d lines; want 24", len(base))
	}
	rec := func(seq int) int { return seq + 1 } // line index of a sequence
	var cp9, cp22 string
	for _, cp := range worked.Checkpoints {
		switch cp.Sequence {
		case 9:
			cp9 = cp.StateHash
		case 22:
			cp22 = cp.StateHash
		}
	}

	cases := []struct {
		name   string
		mutate func(l [][]byte) [][]byte
		exit   int
		stage  string
		seq    string
	}{
		{"B1-payout-digit", func(l [][]byte) [][]byte {
			l[rec(12)] = replaceOnce(t, l[rec(12)], `"payoutCredits":"134"`, `"payoutCredits":"135"`)
			return l
		}, 1, "chain", "13"},
		{"B2-last-player", func(l [][]byte) [][]byte {
			l[rec(22)] = replaceOnce(t, l[rec(22)], `"frank"`, `"frans"`)
			return l
		}, 1, "chain", "22"},
		{"B3-swap-4-5", func(l [][]byte) [][]byte {
			l[rec(4)], l[rec(5)] = l[rec(5)], l[rec(4)]
			return l
		}, 1, "chain", "4"},
		{"B4-delete-13", func(l [][]byte) [][]byte {
			return append(l[:rec(13)], l[rec(13)+1:]...)
		}, 1, "chain", "13"},
		{"B5-duplicate-13", func(l [][]byte) [][]byte {
			out := append([][]byte{}, l[:rec(13)+1]...)
			out = append(out, bytes.Clone(l[rec(13)]))
			return append(out, l[rec(13)+1:]...)
		}, 1, "chain", "14"},
		{"B6-delete-last", func(l [][]byte) [][]byte {
			return l[:len(l)-1]
		}, 1, "chain", "21"},
		{"B7-head-digit", func(l [][]byte) [][]byte {
			l[0] = flipHexDigit(t, l[0], valueStart(t, l[0], `"head":"`))
			return l
		}, 1, "chain", "22"},
		{"B8-seed-last-digit", func(l [][]byte) [][]byte {
			l[0] = flipHexDigit(t, l[0], valueStart(t, l[0], `"configSecret":{"seed":"`)+63)
			return l
		}, 1, "fairness", "0"},
		{"B9-final-checkpoint", func(l [][]byte) [][]byte {
			l[0] = flipHexDigit(t, l[0], valueStart(t, l[0], `"sequence":"22","stateHash":"`))
			if !bytes.Contains(base[0], []byte(cp22)) {
				t.Fatal("checkpoint 22 hash not in header")
			}
			return l
		}, 1, "state-replay", "22"},
		{"B10-checkpoint-9", func(l [][]byte) [][]byte {
			l[0] = flipHexDigit(t, l[0], valueStart(t, l[0], `"sequence":"9","stateHash":"`))
			if !bytes.Contains(base[0], []byte(cp9)) {
				t.Fatal("checkpoint 9 hash not in header")
			}
			return l
		}, 1, "state-replay", "9"},
		{"B11-format-v3", func(l [][]byte) [][]byte {
			l[0] = replaceOnce(t, l[0], `"format":"orvex/audit-history/v1"`, `"format":"orvex/audit-history/v3"`)
			return l
		}, 3, "format", "header"},
		{"B12-space-after-colon", func(l [][]byte) [][]byte {
			i := bytes.Index(l[rec(5)], []byte(`":"`))
			out := append(bytes.Clone(l[rec(5)][:i+2]), ' ')
			l[rec(5)] = append(out, l[rec(5)][i+2:]...)
			return l
		}, 1, "format", "5"},
		{"B13-crlf", func(l [][]byte) [][]byte {
			for i := range l {
				l[i] = bytes.ReplaceAll(l[i], []byte("\n"), []byte("\r\n"))
			}
			return l
		}, 1, "format", "header"},
		{"B14-no-final-newline", func(l [][]byte) [][]byte {
			last := l[len(l)-1]
			l[len(l)-1] = last[:len(last)-1]
			return l
		}, 1, "format", "22"},
		{"B15-header-extra-key", func(l [][]byte) [][]byte {
			h := l[0]
			if !bytes.HasSuffix(h, []byte("\"}\n")) {
				t.Fatalf("header ends with %q", h[len(h)-3:])
			}
			out := append(bytes.Clone(h[:len(h)-2]), []byte(`,"note":"x"`)...)
			l[0] = append(out, "}\n"...)
			return l
		}, 1, "format", "header"},
		{"B16-empty", func([][]byte) [][]byte {
			return nil
		}, 1, "format", "header"},
		{"B16-header-only", func(l [][]byte) [][]byte {
			return l[:1]
		}, 1, "format", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := join(tc.mutate(clone(base)))
			if bytes.Equal(data, join(base)) {
				t.Fatal("mutation changed nothing")
			}
			_, err := verification.Verify(bytes.NewReader(data), options(t, "verify", nil))
			wantOutcome(t, err, tc.exit, tc.stage, tc.seq)
		})
	}
}

// TestEverySingleByteFlipFails is B17: no single-byte change of a golden
// history verifies.
func TestEverySingleByteFlipFails(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive byte flips skipped under -short")
	}
	data := readGolden(t, "empty-round.jsonl")
	opt := options(t, "verify", nil)
	for i := range data {
		mutated := bytes.Clone(data)
		mutated[i] ^= 0x01
		_, err := verification.Verify(bytes.NewReader(mutated), opt)
		var f *verification.Failure
		if !errors.As(err, &f) {
			t.Fatalf("flip at offset %d: err = %v; want a *Failure", i, err)
		}
	}
}

func TestExpectHead(t *testing.T) {
	m := loadManifest(t)
	h := m.Histories[0]
	data := readGolden(t, h.File)
	for _, command := range []string{"chain", "replay", "verify"} {
		_, err := verification.Verify(bytes.NewReader(data), options(t, command, []string{"-expect-head", h.Head}))
		wantOutcome(t, err, 0, "", "")

		other := m.Histories[1].Head
		_, err = verification.Verify(bytes.NewReader(data), options(t, command, []string{"-expect-head", other}))
		wantOutcome(t, err, 1, "chain", strconv.Itoa(h.Records-1))
	}
}

// endless yields 'a' bytes until it has produced limit bytes, then fails
// with errEndless.
type endless struct{ n, limit int }

var errEndless = errors.New("endless reader drained")

func (e *endless) Read(p []byte) (int, error) {
	if e.n >= e.limit {
		return 0, errEndless
	}
	n := min(len(p), e.limit-e.n)
	for i := range n {
		p[i] = 'a'
	}
	e.n += n
	return n, nil
}

func TestLineLimit(t *testing.T) {
	r := &endless{limit: verification.MaxLineBytes + 1<<20}
	_, err := verification.Verify(r, verification.Options{Mode: verification.ModeChain})
	if errors.Is(err, errEndless) {
		t.Fatal("Verify read past the line limit")
	}
	wantOutcome(t, err, 1, "format", "header")
	if !strings.Contains(err.Error(), "line limit") {
		t.Errorf("error %q does not mention the line limit", err)
	}

	// A record line over the limit fails at its position.
	header := goldenLines(t, readGolden(t, "empty-round.jsonl"))[0]
	r2 := io.MultiReader(bytes.NewReader(header), &endless{limit: verification.MaxLineBytes + 1<<20})
	_, err = verification.Verify(r2, verification.Options{Mode: verification.ModeChain})
	if errors.Is(err, errEndless) {
		t.Fatal("Verify read past the line limit")
	}
	wantOutcome(t, err, 1, "format", "0")
}

func TestChainModeNeedsNoRegistry(t *testing.T) {
	data := readGolden(t, "worked-example.jsonl")
	if _, err := verification.Verify(bytes.NewReader(data), verification.Options{Mode: verification.ModeChain}); err != nil {
		t.Fatalf("chain mode without a registry: %v", err)
	}
	for _, mode := range []verification.Mode{verification.ModeReplay, verification.ModeVerify} {
		_, err := verification.Verify(bytes.NewReader(data), verification.Options{Mode: mode})
		var f *verification.Failure
		if err == nil || errors.As(err, &f) {
			t.Errorf("mode %d without a registry = %v; want a plain error", mode, err)
		}
	}
	if _, err := verification.Verify(bytes.NewReader(data), verification.Options{}); err == nil {
		t.Error("zero mode accepted")
	}
	// A registry without the history's rules version fails closed.
	empty, err := game.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	_, err = verification.Verify(bytes.NewReader(data), verification.Options{Mode: verification.ModeReplay, Registry: empty})
	wantOutcome(t, err, 3, "rules", "0")
}

// failingReader returns data, then err.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestReadError(t *testing.T) {
	data := readGolden(t, "worked-example.jsonl")
	boom := errors.New("disk on fire")
	for _, cut := range []int{0, 10, len(data) / 2} {
		_, err := verification.Verify(&failingReader{data: data[:cut], err: boom}, options(t, "verify", nil))
		var f *verification.Failure
		if !errors.Is(err, boom) || errors.As(err, &f) {
			t.Errorf("cut %d: err = %v; want the read error, not a *Failure", cut, err)
		}
	}
}

func TestFailureError(t *testing.T) {
	cases := []struct {
		f    verification.Failure
		want string
	}{
		{verification.Failure{Stage: verification.StageChain, Sequence: 13, Err: errors.New("x")}, "chain failed at sequence 13: x"},
		{verification.Failure{Stage: verification.StageRules, Sequence: 0, Unsupported: true, Err: errors.New("y")}, "rules unsupported at sequence 0: y"},
		{verification.Failure{Stage: verification.StageFormat, Sequence: verification.HeaderSequence, Err: errors.New("z")}, "format failed at header: z"},
	}
	for _, tc := range cases {
		if got := tc.f.Error(); got != tc.want {
			t.Errorf("Error() = %q; want %q", got, tc.want)
		}
		if !errors.Is(&tc.f, tc.f.Err) {
			t.Errorf("Unwrap does not expose %v", tc.f.Err)
		}
	}
}

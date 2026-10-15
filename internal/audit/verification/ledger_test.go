package verification_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
)

const goldenV2Dir = "testdata/golden-v2"

// manifestV2 mirrors testdata/golden-v2/manifest.json (LBT-18 section 9.3).
type manifestV2 struct {
	Format    string `json:"format"`
	Histories []struct {
		Name        string `json:"name"`
		File        string `json:"file"`
		Base        string `json:"base"`
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
		Accounts     int `json:"accounts"`
		Transactions int `json:"transactions"`
		Postings     int `json:"postings"`
		Holds        int `json:"holds"`
		Releases     int `json:"releases"`
	} `json:"histories"`
	Tamper []struct {
		Name        string       `json:"name"`
		File        string       `json:"file"`
		Base        string       `json:"base"`
		Description string       `json:"description"`
		Checks      []checkEntry `json:"checks"`
	} `json:"tamper"`
}

func loadManifestV2(t testing.TB) manifestV2 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenV2Dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m manifestV2
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("golden-v2 manifest.json: %v", err)
	}
	if m.Format != verification.FormatV2 || len(m.Histories) != 4 || len(m.Tamper) != 26 {
		t.Fatalf("golden-v2 manifest: format %q, %d histories, %d tamper cases", m.Format, len(m.Histories), len(m.Tamper))
	}
	return m
}

func readGoldenV2(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenV2Dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func ledgerRules() []ledger.Rule {
	return []ledger.Rule{ledger.CrashV1()}
}

func optionsV2(t testing.TB, command string, args []string) verification.Options {
	t.Helper()
	opt := options(t, command, args)
	opt.LedgerRules = ledgerRules()
	return opt
}

func TestGoldenHistoriesV2(t *testing.T) {
	m := loadManifestV2(t)
	for _, h := range m.Histories {
		data := readGoldenV2(t, h.File)
		for _, command := range []string{"chain", "replay", "verify"} {
			t.Run(h.Name+"/"+command, func(t *testing.T) {
				rep, err := verification.Verify(bytes.NewReader(data), optionsV2(t, command, nil))
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				if rep.Format != verification.FormatV2 || rep.Chain != "orvex/audit/v1" {
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
				if command != "verify" && rep.Ledger != nil {
					t.Errorf("%s mode report has ledger %+v", command, rep.Ledger)
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
				if command != "verify" {
					return
				}
				want := verification.LedgerReport{Accounts: h.Accounts, Transactions: h.Transactions,
					Postings: h.Postings, Holds: h.Holds, Releases: h.Releases}
				if rep.Ledger == nil || *rep.Ledger != want {
					t.Errorf("ledger = %+v; manifest %+v", rep.Ledger, want)
				}
			})
		}
	}
}

// TestV2RecordsEqualV1: a v2 history carries its v1 base's record lines and
// header fields unchanged, plus one ledger line per record.
func TestV2RecordsEqualV1(t *testing.T) {
	m := loadManifestV2(t)
	for _, h := range m.Histories {
		t.Run(h.Name, func(t *testing.T) {
			v2 := goldenLines(t, readGoldenV2(t, h.File))
			base := goldenLines(t, readGolden(t, h.Base))
			n := len(base) - 1 // records
			if n != h.Records || len(v2) != 1+2*n {
				t.Fatalf("v2 has %d lines, v1 base %d; want 1+2*%d", len(v2), len(base), h.Records)
			}
			for i := 1; i <= n; i++ {
				if !bytes.Equal(v2[i], base[i]) {
					t.Errorf("record line %d differs from the v1 base", i-1)
				}
			}
			for j := range n {
				o, err := canonjson.Unmarshal(bytes.TrimSuffix(v2[1+n+j], []byte("\n")))
				if err != nil {
					t.Fatalf("ledger line %d: %v", j, err)
				}
				if o["kind"] != canonjson.String("ledger") || o["sequence"] != canonjson.Uint(uint64(j)) {
					t.Errorf("ledger line %d has kind %v sequence %v", j, o["kind"], o["sequence"])
				}
			}
			hv2, err := canonjson.Unmarshal(bytes.TrimSuffix(v2[0], []byte("\n")))
			if err != nil {
				t.Fatal(err)
			}
			hv1, err := canonjson.Unmarshal(bytes.TrimSuffix(base[0], []byte("\n")))
			if err != nil {
				t.Fatal(err)
			}
			if hv2["format"] != canonjson.String(verification.FormatV2) || hv1["format"] != canonjson.String(verification.Format) {
				t.Fatalf("formats %v / %v", hv2["format"], hv1["format"])
			}
			delete(hv2, "accounts")
			delete(hv2, "format")
			delete(hv1, "format")
			if !reflect.DeepEqual(hv2, hv1) {
				t.Errorf("v2 header fields differ from the v1 base:\n%v\n%v", hv2, hv1)
			}
		})
	}
}

// Spec section 5.5 literals, typed in.
const (
	litWorkedHeader = `{"accounts":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","kind":"player","player":"alice"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000002","kind":"player","player":"bob"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000003","kind":"player","player":"zoë \"☃\"\\\u0001😀"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000004","kind":"player","player":"dave"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000005","kind":"player","player":"erin"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000006","kind":"player","player":"frank"},{"accountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","kind":"system","name":"round_pool","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001"},{"accountId":"4688699a-7bf4-8e07-8cec-c5f6bef86c36","kind":"system","name":"house","roundId":"00000000-0000-0000-0000-000000000000"}],"checkpoints":[{"sequence":"0","stateHash":"1c98ea460213ad0d5900a92fbf37cc3078fa9e822d2065a65037b667734de7e7"},{"sequence":"9","stateHash":"bbf9bc3861a0bdcfccf201b185f4f08a9da2c8c61a53142a5caf13b1edd9f90f"},{"sequence":"22","stateHash":"ba0cff56c3e9473736d3ef45dfc2d9cd4d4cf0f31ea6f600af9ada84125bb02a"}],"configSecret":{"seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"},"format":"orvex/audit-history/v2","head":"7cf8cc4b1807099cdd1f16cbc0c790f1a877f30b696ebc0de751997d73ac3da7"}`

	litInProgressHeader = `{"accounts":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","kind":"player","player":"alice"},{"accountId":"209fda50-25f4-857b-bbbe-df4491ff3cbe","kind":"system","name":"round_pool","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000004"}],"checkpoints":[{"sequence":"0","stateHash":"9eafd3962055c7ef130563c7d46870628fc38a8b85e4070816540edec91dde09"},{"sequence":"3","stateHash":"559fa7527d33184bbc9fe6842a1c1c6fb26620e77f62f52101a06a2972c57e28"}],"configSecret":{"seed":"8d28ecf551864a1db9dccebdf0b4a358e13c61332beff3a0dd67f69760eb995e"},"format":"orvex/audit-history/v2","head":"f041d95d5bb917ce0f0d51d30c47738c1b97932bebd26ac7f117f98a279c46f6"}`

	litInstantLedger5 = `{"holds":[],"kind":"ledger","releases":[],"sequence":"5","transactions":[{"kind":"crash_settlement","postings":[{"amount":"500","creditAccountId":"4688699a-7bf4-8e07-8cec-c5f6bef86c36","debitAccountId":"aff26097-23f5-858c-90c3-a6a5925eab88","eventSequence":"5","postingIndex":"0"}],"transactionId":"0e9dca48-8900-8844-8924-75b55ff85b7e"}]}`
)

var litWorkedLedger = map[int]string{
	0:  `{"holds":[],"kind":"ledger","releases":[],"sequence":"0","transactions":[]}`,
	1:  `{"holds":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","amount":"100"}],"kind":"ledger","releases":["0190f3a8-7b3c-7d4e-8fa0-000000000001"],"sequence":"1","transactions":[{"kind":"crash_bet","postings":[{"amount":"100","creditAccountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","debitAccountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","eventSequence":"1","postingIndex":"0"}],"transactionId":"862c9c4b-3cb4-847f-80eb-d0364134723c"}]}`,
	4:  `{"holds":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","amount":"50"}],"kind":"ledger","releases":["0190f3a8-7b3c-7d4e-8fa0-000000000001"],"sequence":"4","transactions":[]}`,
	12: `{"holds":[],"kind":"ledger","releases":[],"sequence":"12","transactions":[{"kind":"crash_cash_out","postings":[{"amount":"134","creditAccountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","debitAccountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","eventSequence":"5","postingIndex":"0"}],"transactionId":"5687be29-0e53-89bc-886e-babaeada505f"}]}`,
	21: `{"holds":[],"kind":"ledger","releases":[],"sequence":"21","transactions":[{"kind":"crash_settlement","postings":[{"amount":"757599784","creditAccountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","debitAccountId":"4688699a-7bf4-8e07-8cec-c5f6bef86c36","eventSequence":"9","postingIndex":"0"}],"transactionId":"d61426fa-cde4-8067-a6f6-163fb1aeb196"}]}`,
	22: `{"holds":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000006","amount":"10"}],"kind":"ledger","releases":["0190f3a8-7b3c-7d4e-8fa0-000000000006"],"sequence":"22","transactions":[]}`,
}

func roundID(t testing.TB, s string) [16]byte {
	t.Helper()
	id, err := canonjson.ParseUUID(canonjson.String(s))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestV2Literals(t *testing.T) {
	worked := goldenLines(t, readGoldenV2(t, "worked-example.jsonl"))
	instant := goldenLines(t, readGoldenV2(t, "instant-crash.jsonl"))
	inProgress := goldenLines(t, readGoldenV2(t, "in-progress.jsonl"))
	if len(worked) != 47 || len(instant) != 13 || len(inProgress) != 9 {
		t.Fatalf("line counts %d/%d/%d", len(worked), len(instant), len(inProgress))
	}
	eq := func(name string, got []byte, want string) {
		t.Helper()
		if string(got) != want+"\n" {
			t.Errorf("%s:\n got %s\nwant %s", name, got, want)
		}
	}
	eq("worked-example header", worked[0], litWorkedHeader)
	eq("in-progress header", inProgress[0], litInProgressHeader)
	for j, lit := range litWorkedLedger {
		eq("worked-example ledger line "+strconv.Itoa(j), worked[24+j], lit)
	}
	eq("instant-crash ledger line 5", instant[7+5], litInstantLedger5)

	r1 := roundID(t, "0190f3a8-7b3c-7d4e-8f00-000000000001")
	r2 := roundID(t, "0190f3a8-7b3c-7d4e-8f00-000000000002")
	r3 := roundID(t, "0190f3a8-7b3c-7d4e-8f00-000000000003")
	r4 := roundID(t, "0190f3a8-7b3c-7d4e-8f00-000000000004")
	ids := []struct {
		got  [16]byte
		want string
	}{
		{ledger.HouseAccountID(), "4688699a-7bf4-8e07-8cec-c5f6bef86c36"},
		{ledger.RoundAccountID(r1), "13565418-c5ee-8f13-85c6-552f9e8755bd"},
		{ledger.RoundAccountID(r2), "aff26097-23f5-858c-90c3-a6a5925eab88"},
		{ledger.RoundAccountID(r3), "0bcd27f6-b696-839e-b6a5-dd7a1eb9e3cc"},
		{ledger.RoundAccountID(r4), "209fda50-25f4-857b-bbbe-df4491ff3cbe"},
		{ledger.TransactionID(r1, 1), "862c9c4b-3cb4-847f-80eb-d0364134723c"},
		{ledger.TransactionID(r1, 2), "f6dcb55d-533f-8465-b3fd-ac2d043c0c10"},
		{ledger.TransactionID(r1, 3), "7c84957e-9dc3-88c2-ba7f-ba3b6d2ed8b5"},
		{ledger.TransactionID(r1, 12), "5687be29-0e53-89bc-886e-babaeada505f"},
		{ledger.TransactionID(r1, 17), "4357c225-96f0-803b-a757-26ca7f5055ac"},
		{ledger.TransactionID(r1, 21), "d61426fa-cde4-8067-a6f6-163fb1aeb196"},
		{ledger.TransactionID(r2, 1), "a839c9f2-fad9-8876-b49f-e366cbb47340"},
		{ledger.TransactionID(r2, 5), "0e9dca48-8900-8844-8924-75b55ff85b7e"},
		{ledger.TransactionID(r4, 1), "5786a0a2-da6c-887e-a42d-9979ea3a9a33"},
		{ledger.TransactionID(r4, 3), "caff4675-e4f6-8624-a11e-fe7ef6095573"},
	}
	for i, c := range ids {
		if got := string(canonjson.UUID(c.got)); got != c.want {
			t.Errorf("ID %d = %s; want %s", i, got, c.want)
		}
	}
}

func TestTamperFixturesV2(t *testing.T) {
	m := loadManifestV2(t)
	for _, tc := range m.Tamper {
		data := readGoldenV2(t, tc.File)
		for i, c := range tc.Checks {
			t.Run(tc.Name+"/"+strconv.Itoa(i)+"-"+c.Command, func(t *testing.T) {
				_, err := verification.Verify(bytes.NewReader(data), optionsV2(t, c.Command, c.Args))
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

// TestByteMutationsV2 is M1 to M7 of LBT-18 section 9.5.
func TestByteMutationsV2(t *testing.T) {
	base := goldenLines(t, readGoldenV2(t, "worked-example.jsonl"))
	if len(base) != 47 {
		t.Fatalf("worked example v2 has %d lines; want 47", len(base))
	}
	baseV1 := goldenLines(t, readGolden(t, "worked-example.jsonl"))
	const firstLedger = 24 // line index of ledger line 0

	cases := []struct {
		name   string
		base   [][]byte
		mutate func(l [][]byte) [][]byte
		exit   int
		stage  string
		seq    string
	}{
		{"M1-format-v3", base, func(l [][]byte) [][]byte {
			l[0] = replaceOnce(t, l[0], `"format":"orvex/audit-history/v2"`, `"format":"orvex/audit-history/v3"`)
			return l
		}, 3, "format", "header"},
		{"M2-v1-relabelled-v2", baseV1, func(l [][]byte) [][]byte {
			l[0] = replaceOnce(t, l[0], `"format":"orvex/audit-history/v1"`, `"format":"orvex/audit-history/v2"`)
			return l
		}, 1, "format", "header"},
		{"M3-delete-ledger-0", base, func(l [][]byte) [][]byte {
			return append(l[:firstLedger], l[firstLedger+1:]...)
		}, 1, "format", "0"},
		{"M4-duplicate-last", base, func(l [][]byte) [][]byte {
			return append(l, bytes.Clone(l[len(l)-1]))
		}, 1, "format", "23"},
		{"M5-ledger-0-after-genesis", base, func(l [][]byte) [][]byte {
			out := append([][]byte{}, l[:2]...)
			out = append(out, l[firstLedger])
			out = append(out, l[2:firstLedger]...)
			return append(out, l[firstLedger+1:]...)
		}, 1, "format", "1"},
		{"M6-no-final-newline", base, func(l [][]byte) [][]byte {
			last := l[len(l)-1]
			l[len(l)-1] = last[:len(last)-1]
			return l
		}, 1, "format", "22"},
		{"M7-head-digit", base, func(l [][]byte) [][]byte {
			l[0] = flipHexDigit(t, l[0], valueStart(t, l[0], `"head":"`))
			return l
		}, 1, "chain", "22"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := join(tc.mutate(clone(tc.base)))
			if bytes.Equal(data, join(tc.base)) {
				t.Fatal("mutation changed nothing")
			}
			_, err := verification.Verify(bytes.NewReader(data), optionsV2(t, "verify", nil))
			wantOutcome(t, err, tc.exit, tc.stage, tc.seq)
		})
	}
}

// TestEverySingleByteFlipFailsV2: no single-byte change of the
// instant-crash v2 history verifies (LBT-18 L-12).
func TestEverySingleByteFlipFailsV2(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive byte flips skipped under -short")
	}
	data := readGoldenV2(t, "instant-crash.jsonl")
	opt := optionsV2(t, "verify", nil)
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

func TestLedgerRuleRequired(t *testing.T) {
	data := readGoldenV2(t, "worked-example.jsonl")
	_, err := verification.Verify(bytes.NewReader(data), options(t, "verify", nil))
	wantOutcome(t, err, 3, "rules", "0")
	for _, command := range []string{"chain", "replay"} {
		rep, err := verification.Verify(bytes.NewReader(data), options(t, command, nil))
		wantOutcome(t, err, 0, "", "")
		if rep.Ledger != nil {
			t.Errorf("%s: ledger %+v", command, rep.Ledger)
		}
	}
	rep, err := verification.Verify(bytes.NewReader(readGolden(t, "worked-example.jsonl")), options(t, "verify", nil))
	wantOutcome(t, err, 0, "", "")
	if rep.Ledger != nil {
		t.Errorf("v1 verify: ledger %+v", rep.Ledger)
	}
}

// stateV2Rule is crash/v1's rule reporting state version 2.
type stateV2Rule struct{ ledger.Rule }

func (stateV2Rule) Descriptor() game.Descriptor {
	d := v1.Descriptor()
	d.StateVersion = 2
	return d
}

func TestLedgerRuleDescriptorMismatch(t *testing.T) {
	opt := options(t, "verify", nil)
	opt.LedgerRules = []ledger.Rule{stateV2Rule{ledger.CrashV1()}}
	_, err := verification.Verify(bytes.NewReader(readGoldenV2(t, "worked-example.jsonl")), opt)
	wantOutcome(t, err, 3, "rules", "0")
}

// countingReader counts Read calls.
type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

func TestLedgerRulesInvalidOptions(t *testing.T) {
	data := readGoldenV2(t, "worked-example.jsonl")
	invalid := map[string][]ledger.Rule{
		"nil":       {nil},
		"duplicate": {ledger.CrashV1(), ledger.CrashV1()},
	}
	for name, rules := range invalid {
		for _, command := range []string{"chain", "replay", "verify"} {
			t.Run(name+"/"+command, func(t *testing.T) {
				opt := options(t, command, nil)
				opt.LedgerRules = rules
				r := &countingReader{r: bytes.NewReader(data)}
				_, err := verification.Verify(r, opt)
				var f *verification.Failure
				if err == nil || errors.As(err, &f) {
					t.Fatalf("err = %v; want a plain error", err)
				}
				if r.reads != 0 {
					t.Errorf("Verify read the input %d times before rejecting the options", r.reads)
				}
			})
		}
	}
}

// record1Rule wraps crash/v1's rule and changes its result for record 1 of
// the worked example (first event 1, one event).
type record1Rule struct {
	ledger.Rule
	zeroAmount bool
}

var errRecord1 = errors.New("record 1 cannot be posted")

func (r record1Rule) Postings(events []any, first uint64) (string, []ledger.Posting, error) {
	kind, ps, err := r.Rule.Postings(events, first)
	if first != 1 || len(events) != 1 || err != nil {
		return kind, ps, err
	}
	if !r.zeroAmount {
		return "", nil, errRecord1
	}
	ps[0].Amount = 0
	return kind, ps, nil
}

func TestLedgerRuleErrorIsLedgerFailure(t *testing.T) {
	data := readGoldenV2(t, "worked-example.jsonl")
	for _, zero := range []bool{false, true} {
		opt := options(t, "verify", nil)
		opt.LedgerRules = []ledger.Rule{record1Rule{Rule: ledger.CrashV1(), zeroAmount: zero}}
		_, err := verification.Verify(bytes.NewReader(data), opt)
		wantOutcome(t, err, 1, "ledger", "1")
	}
}

func TestV1UnaffectedByLedgerRules(t *testing.T) {
	m := loadManifest(t)
	for _, h := range m.Histories {
		data := readGolden(t, h.File)
		for _, command := range []string{"chain", "replay", "verify"} {
			t.Run(h.Name+"/"+command, func(t *testing.T) {
				without, err := verification.Verify(bytes.NewReader(data), options(t, command, nil))
				if err != nil {
					t.Fatal(err)
				}
				with, err := verification.Verify(bytes.NewReader(data), optionsV2(t, command, nil))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(without, with) {
					t.Errorf("reports differ:\n%+v\n%+v", without, with)
				}
				if with.Ledger != nil || with.Format != verification.Format {
					t.Errorf("v1 report has ledger %+v, format %q", with.Ledger, with.Format)
				}
			})
		}
	}
}

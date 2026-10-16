package eventstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
)

const goldenV2Dir = "../../audit/verification/testdata/golden-v2"

// v2Encodable are the golden-v2 files whose header and ledger lines the
// exporter must reproduce: the four histories, plus tamper fixtures with
// several transactions or postings per line and a degenerate amount, which
// the exporter copies faithfully.
var v2Encodable = []string{
	"worked-example.jsonl", "instant-crash.jsonl", "empty-round.jsonl", "in-progress.jsonl",
	"tamper/event-posted-twice.jsonl", "tamper/posting-extra.jsonl", "tamper/unbalanced-negative-amount.jsonl",
}

// must turns a (value, error) pair into a function of the test that fails
// it on the error: must(f())(t).
func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// parseV2Header returns the header fields of a v2 history and its account
// table as stored rows, in reverse order.
func parseV2Header(t *testing.T, line []byte) (chain.Hash, []Checkpoint, []byte, []accountRow) {
	t.Helper()
	o := must(canonjson.Unmarshal(line))(t)
	var head chain.Hash
	copy(head[:], must(canonjson.ParseHex(o["head"], len(head)))(t))
	secret := must(canonjson.Marshal(must(canonjson.Obj(o["configSecret"]))(t)))(t)
	var cps []Checkpoint
	for _, v := range must(canonjson.List(o["checkpoints"]))(t) {
		co := must(canonjson.Obj(v))(t)
		cp := Checkpoint{Sequence: must(canonjson.ParseUint(co["sequence"]))(t)}
		copy(cp.StateHash[:], must(canonjson.ParseHex(co["stateHash"], len(cp.StateHash)))(t))
		cps = append(cps, cp)
	}
	var accounts []accountRow
	for _, v := range must(canonjson.List(o["accounts"]))(t) {
		ao := must(canonjson.Obj(v))(t)
		a := accountRow{
			id:   must(canonjson.ParseUUID(ao["accountId"]))(t),
			kind: must(canonjson.Text(ao["kind"]))(t),
		}
		switch a.kind {
		case "player":
			p := must(canonjson.Text(ao["player"]))(t)
			a.playerID = &p
		case "system":
			n := must(canonjson.Text(ao["name"]))(t)
			a.name = &n
			if rid := must(canonjson.ParseUUID(ao["roundId"]))(t); rid != ([16]byte{}) {
				a.roundID = &rid // the nil UUID stands for a NULL round_id
			}
		default:
			t.Fatalf("account kind %q", a.kind)
		}
		accounts = append(accounts, a)
	}
	slices.Reverse(accounts)
	return head, cps, secret, accounts
}

// parseV2LedgerLine returns a ledger line as stored rows, every list in
// reverse order.
func parseV2LedgerLine(t *testing.T, line []byte) (uint64, []holdRow, []releaseRow, []transactionRow) {
	t.Helper()
	o := must(canonjson.Unmarshal(line))(t)
	seq := must(canonjson.ParseUint(o["sequence"]))(t)
	var holds []holdRow
	for _, v := range must(canonjson.List(o["holds"]))(t) {
		ho := must(canonjson.Obj(v))(t)
		holds = append(holds, holdRow{
			sequence:  int64(seq), //nolint:gosec // G115: a sequence from a golden v2 fixture (< 100)
			accountID: must(canonjson.ParseUUID(ho["accountId"]))(t),
			amount:    must(canonjson.ParseInt(ho["amount"]))(t),
		})
	}
	var releases []releaseRow
	for _, v := range must(canonjson.List(o["releases"]))(t) {
		releases = append(releases, releaseRow{sequence: int64(seq), accountID: must(canonjson.ParseUUID(v))(t)}) //nolint:gosec // G115: a sequence from a golden v2 fixture (< 100)
	}
	var txs []transactionRow
	for _, v := range must(canonjson.List(o["transactions"]))(t) {
		to := must(canonjson.Obj(v))(t)
		tr := transactionRow{
			id:       must(canonjson.ParseUUID(to["transactionId"]))(t),
			kind:     must(canonjson.Text(to["kind"]))(t),
			sequence: int64(seq), //nolint:gosec // G115: a sequence from a golden v2 fixture (< 100)
		}
		for _, pv := range must(canonjson.List(to["postings"]))(t) {
			po := must(canonjson.Obj(pv))(t)
			p := postingRow{
				index:  int64(must(canonjson.ParseUint(po["postingIndex"]))(t)), //nolint:gosec // G115: a posting index from a golden v2 fixture (< 100)
				debit:  must(canonjson.ParseUUID(po["debitAccountId"]))(t),
				credit: must(canonjson.ParseUUID(po["creditAccountId"]))(t),
				amount: must(canonjson.ParseInt(po["amount"]))(t),
			}
			if ev := int64(must(canonjson.ParseUint(po["eventSequence"]))(t)); ev != 0 { //nolint:gosec // G115: an event sequence from a golden v2 fixture (< 100)
				p.eventSequence = &ev // "0" stands for a NULL event_sequence
			}
			tr.postings = append(tr.postings, p)
		}
		slices.Reverse(tr.postings)
		txs = append(txs, tr)
	}
	slices.Reverse(holds)
	slices.Reverse(releases)
	slices.Reverse(txs)
	return seq, holds, releases, txs
}

// TestEncodeLedgerMatchesGoldenV2 rebuilds each v2 header and ledger line
// from unsorted stored rows and requires the oracle's exact bytes.
func TestEncodeLedgerMatchesGoldenV2(t *testing.T) {
	for _, name := range v2Encodable {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(goldenV2Dir, name)) //nolint:gosec // G304: fixed golden fixture under testdata
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
			if len(lines)%2 != 1 {
				t.Fatalf("%d lines", len(lines))
			}
			records := (len(lines) - 1) / 2

			head, cps, secret, accounts := parseV2Header(t, lines[0])
			got, err := encodeHeaderV2(head, cps, secret, accounts)
			if err != nil {
				t.Fatalf("encodeHeaderV2: %v", err)
			}
			if !bytes.Equal(got, lines[0]) {
				t.Errorf("encodeHeaderV2 =\n%s\nwant\n%s", got, lines[0])
			}
			for j := range records {
				line := lines[1+records+j]
				seq, holds, releases, txs := parseV2LedgerLine(t, line)
				if seq != uint64(j) {
					t.Fatalf("ledger line %d has sequence %d", j, seq)
				}
				got, err := encodeLedgerLine(seq, holds, releases, txs)
				if err != nil {
					t.Fatalf("encodeLedgerLine %d: %v", j, err)
				}
				if !bytes.Equal(got, line) {
					t.Errorf("encodeLedgerLine %d =\n%s\nwant\n%s", j, got, line)
				}
			}
		})
	}
}

func TestEncodeLedgerNullEventSequence(t *testing.T) {
	tx := transactionRow{id: [16]byte{1}, kind: "grant", postings: []postingRow{{debit: [16]byte{2}, credit: [16]byte{3}, amount: 5}}}
	got, err := encodeLedgerLine(1, nil, nil, []transactionRow{tx})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"eventSequence":"0"`)) {
		t.Errorf("encodeLedgerLine = %s; want eventSequence 0", got)
	}
}

func TestEncodeLedgerRejectsCorrupt(t *testing.T) {
	var head chain.Hash
	secret := []byte(`{"seed":"00"}`)
	str := func(s string) *string { return &s }
	neg := int64(-1)
	headerCases := map[string]accountRow{
		"kind bank":          {id: [16]byte{1}, kind: "bank"},
		"player null id":     {id: [16]byte{1}, kind: "player"},
		"player empty id":    {id: [16]byte{1}, kind: "player", playerID: str("")},
		"system null name":   {id: [16]byte{1}, kind: "system"},
		"player invalid utf": {id: [16]byte{1}, kind: "player", playerID: str("\xff")},
	}
	for name, a := range headerCases {
		if _, err := encodeHeaderV2(head, nil, secret, []accountRow{a}); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v; want ErrCorrupt", name, err)
		}
	}
	lineCases := map[string]postingRow{
		"posting index -1":  {index: -1, debit: [16]byte{2}, credit: [16]byte{3}, amount: 1},
		"event sequence -1": {debit: [16]byte{2}, credit: [16]byte{3}, amount: 1, eventSequence: &neg},
	}
	for name, p := range lineCases {
		tx := transactionRow{id: [16]byte{1}, kind: "crash_bet", postings: []postingRow{p}}
		if _, err := encodeLedgerLine(1, nil, nil, []transactionRow{tx}); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v; want ErrCorrupt", name, err)
		}
	}
}

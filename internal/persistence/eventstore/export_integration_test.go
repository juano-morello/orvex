//go:build integration

package eventstore_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	crashv1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

const goldenDir = "../../audit/verification/testdata/golden"

var ctx = context.Background()

type manifest struct {
	Format    string `json:"format"`
	Histories []struct {
		Name        string `json:"name"`
		File        string `json:"file"`
		Records     int    `json:"records"`
		Head        string `json:"head"`
		Checkpoints []struct {
			Sequence  uint64 `json:"sequence"`
			StateHash string `json:"stateHash"`
		} `json:"checkpoints"`
	} `json:"histories"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Histories) != 4 {
		t.Fatalf("manifest lists %d histories, want 4", len(m.Histories))
	}
	return m
}

func readGolden(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, file))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hash(t *testing.T, s string) chain.Hash {
	t.Helper()
	var h chain.Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(h) {
		t.Fatalf("bad hash %q", s)
	}
	copy(h[:], b)
	return h
}

func verify(t *testing.T, history []byte) verification.Report {
	t.Helper()
	reg, err := game.NewRegistry(crashv1.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := verification.Verify(bytes.NewReader(history), verification.Options{Mode: verification.ModeVerify, Registry: reg})
	if err != nil {
		t.Fatalf("verification.Verify: %v", err)
	}
	return rep
}

func TestExportGoldenHistories(t *testing.T) {
	t.Parallel()
	m := loadManifest(t)
	if m.Format != verification.Format {
		t.Fatalf("manifest format %q, want %q", m.Format, verification.Format)
	}
	for _, h := range m.Histories {
		t.Run(h.Name, func(t *testing.T) {
			t.Parallel()
			pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
			golden := readGolden(t, h.File)
			l := pgtest.LoadHistory(t, pool, golden, nil)

			var out bytes.Buffer
			exp, err := eventstore.ExportHistory(ctx, pool, l.RoundID, &out)
			if err != nil {
				t.Fatalf("ExportHistory: %v", err)
			}
			if !bytes.Equal(out.Bytes(), golden) {
				t.Errorf("export differs from %s:\n%s", h.File, out.Bytes())
			}
			rep := verify(t, out.Bytes())
			if rep.Format != verification.Format {
				t.Errorf("report format = %q", rep.Format)
			}
			if exp.RoundID != l.RoundID || exp.Records != h.Records || exp.Head != hash(t, h.Head) || rep.Head != exp.Head {
				t.Errorf("export = {round %x, records %d, head %s}, want {%x, %d, %s}",
					exp.RoundID, exp.Records, exp.Head, l.RoundID, h.Records, h.Head)
			}
			if len(exp.Checkpoints) != len(h.Checkpoints) {
				t.Fatalf("export has %d checkpoints, want %d", len(exp.Checkpoints), len(h.Checkpoints))
			}
			for i, cp := range h.Checkpoints {
				got := exp.Checkpoints[i]
				if got.Sequence != cp.Sequence || got.StateHash != hash(t, cp.StateHash) {
					t.Errorf("checkpoint %d = {%d, %s}, want {%d, %s}", i, got.Sequence, got.StateHash, cp.Sequence, cp.StateHash)
				}
			}
		})
	}
}

func TestExportStopsAtLatestSnapshot(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	golden := readGolden(t, "worked-example.jsonl")
	l := pgtest.LoadHistory(t, pool, golden, []uint64{0, 9})

	var out bytes.Buffer
	exp, err := eventstore.ExportHistory(ctx, pool, l.RoundID, &out)
	if err != nil {
		t.Fatalf("ExportHistory: %v", err)
	}
	if exp.Records != 10 || exp.Head != l.Hashes[9] {
		t.Errorf("export = {records %d, head %s}, want {10, %s}", exp.Records, exp.Head, l.Hashes[9])
	}
	if len(exp.Checkpoints) != 2 || exp.Checkpoints[0].Sequence != 0 || exp.Checkpoints[1].Sequence != 9 {
		t.Errorf("checkpoints = %+v, want sequences 0 and 9", exp.Checkpoints)
	}
	gotLines := bytes.SplitAfter(out.Bytes(), []byte("\n"))
	goldenLines := bytes.SplitAfter(golden, []byte("\n"))
	// Header, records 0..9, and the empty remainder after the final newline.
	if len(gotLines) != 12 {
		t.Fatalf("export has %d lines, want 11", len(gotLines)-1)
	}
	if want := []byte(`"head":"` + l.Hashes[9].String() + `"`); !bytes.Contains(gotLines[0], want) {
		t.Errorf("header %s does not name head %s", gotLines[0], l.Hashes[9])
	}
	if !bytes.Equal(bytes.Join(gotLines[1:], nil), bytes.Join(goldenLines[1:11], nil)) {
		t.Error("records differ from golden lines 2 to 11")
	}
	rep := verify(t, out.Bytes())
	if rep.Records != 10 || rep.Head != l.Hashes[9] {
		t.Errorf("report = {records %d, head %s}", rep.Records, rep.Head)
	}
}

func TestExportRoundNotFound(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	pgtest.LoadHistory(t, pool, readGolden(t, "worked-example.jsonl"), nil)
	var out bytes.Buffer
	unknown := [16]byte{0x01, 0x90, 0xf3, 0xa8, 15: 0x99}
	if _, err := eventstore.ExportHistory(ctx, pool, unknown, &out); !errors.Is(err, eventstore.ErrRoundNotFound) {
		t.Errorf("err = %v, want ErrRoundNotFound", err)
	}
	if out.Len() != 0 {
		t.Errorf("wrote %d bytes, want none", out.Len())
	}
	var outV2 bytes.Buffer
	if _, err := eventstore.ExportHistoryV2(ctx, pool, unknown, &outV2); !errors.Is(err, eventstore.ErrRoundNotFound) {
		t.Errorf("ExportHistoryV2: err = %v, want ErrRoundNotFound", err)
	}
	if outV2.Len() != 0 {
		t.Errorf("ExportHistoryV2 wrote %d bytes, want none", outV2.Len())
	}
}

func TestExportNoCheckpoint(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	l := pgtest.LoadHistory(t, pool, readGolden(t, "worked-example.jsonl"), []uint64{})
	var out bytes.Buffer
	if _, err := eventstore.ExportHistory(ctx, pool, l.RoundID, &out); !errors.Is(err, eventstore.ErrNoCheckpoint) {
		t.Errorf("err = %v, want ErrNoCheckpoint", err)
	}
	if out.Len() != 0 {
		t.Errorf("wrote %d bytes, want none", out.Len())
	}
	var outV2 bytes.Buffer
	if _, err := eventstore.ExportHistoryV2(ctx, pool, l.RoundID, &outV2); !errors.Is(err, eventstore.ErrNoCheckpoint) {
		t.Errorf("ExportHistoryV2: err = %v, want ErrNoCheckpoint", err)
	}
	if outV2.Len() != 0 {
		t.Errorf("ExportHistoryV2 wrote %d bytes, want none", outV2.Len())
	}
}

// ---------------------------------------------------------------- format v2 (LBT-18)

const goldenV2Dir = "../../audit/verification/testdata/golden-v2"

type manifestV2 struct {
	Format    string `json:"format"`
	Histories []struct {
		Name        string `json:"name"`
		File        string `json:"file"`
		Base        string `json:"base"`
		Records     int    `json:"records"`
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
}

func loadManifestV2(t *testing.T) manifestV2 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenV2Dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifestV2
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Format != verification.FormatV2 || len(m.Histories) != 4 {
		t.Fatalf("golden-v2 manifest: format %q, %d histories", m.Format, len(m.Histories))
	}
	return m
}

func readGoldenV2(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenV2Dir, file))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// verifyV2 runs ModeVerify with crash/v1's rules and posting rule.
func verifyV2(t *testing.T, history []byte) (verification.Report, error) {
	t.Helper()
	reg, err := game.NewRegistry(crashv1.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	return verification.Verify(bytes.NewReader(history), verification.Options{
		Mode: verification.ModeVerify, Registry: reg, LedgerRules: []ledger.Rule{ledger.CrashV1()},
	})
}

func exportV2(t *testing.T, pool *pgxpool.Pool, roundID [16]byte) ([]byte, eventstore.Export) {
	t.Helper()
	var out bytes.Buffer
	exp, err := eventstore.ExportHistoryV2(ctx, pool, roundID, &out)
	if err != nil {
		t.Fatalf("ExportHistoryV2: %v", err)
	}
	return out.Bytes(), exp
}

func TestExportV2GoldenHistories(t *testing.T) {
	t.Parallel()
	m := loadManifestV2(t)
	for _, h := range m.Histories {
		t.Run(h.Name, func(t *testing.T) {
			t.Parallel()
			pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
			golden := readGoldenV2(t, h.File)
			l := pgtest.LoadHistory(t, pool, golden, nil)

			out, exp := exportV2(t, pool, l.RoundID)
			if !bytes.Equal(out, golden) {
				t.Errorf("export differs from golden-v2/%s:\n%s", h.File, out)
			}
			rep, err := verifyV2(t, out)
			if err != nil {
				t.Fatalf("verification.Verify: %v", err)
			}
			want := verification.LedgerReport{Accounts: h.Accounts, Transactions: h.Transactions,
				Postings: h.Postings, Holds: h.Holds, Releases: h.Releases}
			if rep.Format != verification.FormatV2 || rep.Ledger == nil || *rep.Ledger != want {
				t.Errorf("report format %q ledger %+v, want %+v", rep.Format, rep.Ledger, want)
			}
			if exp.RoundID != l.RoundID || exp.Records != h.Records || exp.Head != hash(t, h.Head) || rep.Head != exp.Head {
				t.Errorf("export = {round %x, records %d, head %s}, want {%x, %d, %s}",
					exp.RoundID, exp.Records, exp.Head, l.RoundID, h.Records, h.Head)
			}
			if len(exp.Checkpoints) != len(h.Checkpoints) {
				t.Fatalf("export has %d checkpoints, want %d", len(exp.Checkpoints), len(h.Checkpoints))
			}
			for i, cp := range h.Checkpoints {
				got := exp.Checkpoints[i]
				if got.Sequence != cp.Sequence || got.StateHash != hash(t, cp.StateHash) {
					t.Errorf("checkpoint %d = {%d, %s}, want {%d, %s}", i, got.Sequence, got.StateHash, cp.Sequence, cp.StateHash)
				}
			}
		})
	}
}

func TestExportV1UnchangedWithLedgerRows(t *testing.T) {
	t.Parallel()
	m := loadManifestV2(t)
	for _, h := range m.Histories {
		t.Run(h.Name, func(t *testing.T) {
			t.Parallel()
			pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
			l := pgtest.LoadHistory(t, pool, readGoldenV2(t, h.File), nil)
			var out bytes.Buffer
			if _, err := eventstore.ExportHistory(ctx, pool, l.RoundID, &out); err != nil {
				t.Fatalf("ExportHistory: %v", err)
			}
			if !bytes.Equal(out.Bytes(), readGolden(t, h.Base)) {
				t.Errorf("v1 export differs from golden/%s:\n%s", h.Base, out.Bytes())
			}
		})
	}
}

func TestExportV2StopsAtLatestSnapshot(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	golden := readGoldenV2(t, "worked-example.jsonl")
	l := pgtest.LoadHistory(t, pool, golden, []uint64{0, 9})

	out, exp := exportV2(t, pool, l.RoundID)
	if exp.Records != 10 || exp.Head != l.Hashes[9] {
		t.Errorf("export = {records %d, head %s}, want {10, %s}", exp.Records, exp.Head, l.Hashes[9])
	}
	got := bytes.SplitAfter(out, []byte("\n"))
	want := bytes.SplitAfter(golden, []byte("\n"))
	// Header, records 0..9, ledger lines 0..9 and the empty remainder.
	if len(got) != 22 {
		t.Fatalf("export has %d lines, want 21", len(got)-1)
	}
	if !bytes.Equal(bytes.Join(got[1:11], nil), bytes.Join(want[1:11], nil)) {
		t.Error("records differ from golden records 0..9")
	}
	if !bytes.Equal(bytes.Join(got[11:21], nil), bytes.Join(want[24:34], nil)) {
		t.Error("ledger lines differ from golden ledger lines 0..9")
	}
	hdr, err := canonjson.Unmarshal(bytes.TrimSuffix(got[0], []byte("\n")))
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := canonjson.List(hdr["accounts"])
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, v := range accounts {
		o, err := canonjson.Obj(v)
		if err != nil {
			t.Fatal(err)
		}
		id, err := canonjson.Text(o["accountId"])
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// alice, bob, P3, dave and the round account: no erin, frank or house.
	wantIDs := []string{
		"0190f3a8-7b3c-7d4e-8fa0-000000000001", "0190f3a8-7b3c-7d4e-8fa0-000000000002",
		"0190f3a8-7b3c-7d4e-8fa0-000000000003", "0190f3a8-7b3c-7d4e-8fa0-000000000004",
		"13565418-c5ee-8f13-85c6-552f9e8755bd",
	}
	if !slices.Equal(ids, wantIDs) {
		t.Errorf("accounts = %v, want %v", ids, wantIDs)
	}
	rep, err := verifyV2(t, out)
	if err != nil {
		t.Fatalf("verification.Verify: %v", err)
	}
	if wantLedger := (verification.LedgerReport{Accounts: 5, Transactions: 3, Postings: 3, Holds: 5, Releases: 5}); rep.Ledger == nil || *rep.Ledger != wantLedger {
		t.Errorf("ledger = %+v, want %+v", rep.Ledger, wantLedger)
	}
}

// TestExportV2SharedAccounts: two rounds sharing alice's and the house
// account each export only their own rows and referenced accounts.
func TestExportV2SharedAccounts(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	var loaded [][16]byte
	files := []string{"worked-example.jsonl", "instant-crash.jsonl"}
	for _, f := range files {
		loaded = append(loaded, pgtest.LoadHistory(t, pool, readGoldenV2(t, f), nil).RoundID)
	}
	for i, f := range files {
		out, _ := exportV2(t, pool, loaded[i])
		if !bytes.Equal(out, readGoldenV2(t, f)) {
			t.Errorf("export of %s differs from its golden:\n%s", f, out)
		}
	}
}

// TestExportV2WithoutLedgerRowsFailsVerification: a round without ledger
// rows exports empty ledger lines, and the bet at record 1 has no hold.
func TestExportV2WithoutLedgerRowsFailsVerification(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	l := pgtest.LoadHistory(t, pool, readGolden(t, "worked-example.jsonl"), nil)
	out, _ := exportV2(t, pool, l.RoundID)
	lines := bytes.SplitAfter(out, []byte("\n"))
	if len(lines) != 48 {
		t.Fatalf("export has %d lines, want 47", len(lines)-1)
	}
	if !bytes.Contains(lines[0], []byte(`"accounts":[]`)) {
		t.Errorf("header %s has accounts", lines[0])
	}
	for j := range 23 {
		want := `{"holds":[],"kind":"ledger","releases":[],"sequence":"` + strconv.Itoa(j) + `","transactions":[]}` + "\n"
		if string(lines[24+j]) != want {
			t.Errorf("ledger line %d = %s", j, lines[24+j])
		}
	}
	_, err := verifyV2(t, out)
	var f *verification.Failure
	if !errors.As(err, &f) || f.Stage != verification.StageLedger || f.Sequence != 1 || f.Unsupported {
		t.Errorf("verify = %v, want a ledger failure at sequence 1", err)
	}
}

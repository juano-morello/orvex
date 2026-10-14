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
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/game"
	crashv1 "github.com/juano-morello/orvex/internal/game/crash/v1"
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
}

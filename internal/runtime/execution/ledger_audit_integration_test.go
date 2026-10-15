//go:build integration

package execution_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
)

const goldenV2Dir = "../../audit/verification/testdata/golden-v2"

// readGoldenV2 returns a golden v2 history's lines without their '\n'.
func readGoldenV2(t *testing.T, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenV2Dir, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

func exportHistoryV2(t *testing.T, db *pgxpool.Pool, roundID [16]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := eventstore.ExportHistoryV2(ctx, db, roundID, &buf); err != nil {
		t.Fatalf("ExportHistoryV2: %v", err)
	}
	return buf.Bytes()
}

// verifyExportV2 verifies an exported v2 history with decision replay and
// the ledger stage.
func verifyExportV2(t *testing.T, b []byte) (verification.Report, error) {
	t.Helper()
	return verification.Verify(bytes.NewReader(b), verification.Options{
		Mode: verification.ModeVerify, Registry: crashRegistry(t), LedgerRules: []ledger.Rule{ledger.CrashV1()},
	})
}

func mustObject(t *testing.T, line []byte) canonjson.Object {
	t.Helper()
	o, err := canonjson.Unmarshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func mustList(t *testing.T, v canonjson.Value) canonjson.Array {
	t.Helper()
	a, err := canonjson.List(v)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustObj(t *testing.T, v canonjson.Value) canonjson.Object {
	t.Helper()
	o, err := canonjson.Obj(v)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// referencedAccounts returns the account IDs (as UUID text) that the given
// ledger lines reference.
func referencedAccounts(t *testing.T, lines [][]byte) map[canonjson.Value]bool {
	t.Helper()
	refs := map[canonjson.Value]bool{}
	for _, line := range lines {
		o := mustObject(t, line)
		for _, h := range mustList(t, o["holds"]) {
			refs[mustObj(t, h)["accountId"]] = true
		}
		for _, r := range mustList(t, o["releases"]) {
			refs[r] = true
		}
		for _, tx := range mustList(t, o["transactions"]) {
			for _, p := range mustList(t, mustObj(t, tx)["postings"]) {
				po := mustObj(t, p)
				refs[po["debitAccountId"]] = true
				refs[po["creditAccountId"]] = true
			}
		}
	}
	return refs
}

// TestExecutedGoldensExportVerifiedLedger runs each golden through the
// executor with the golden player account IDs, exports it as format v2 and
// verifies it with the ledger stage: the executor's ledger rows export to
// the oracle's ledger lines up to the export's end S (LBT-18 L-14).
func TestExecutedGoldensExportVerifiedLedger(t *testing.T) {
	cases := []struct {
		name   string
		last   int // S, the newest snapshot (E-13)
		ledger verification.LedgerReport
	}{
		{"worked-example", 21, verification.LedgerReport{Accounts: 7, Transactions: 6, Postings: 6, Holds: 6, Releases: 6}},
		{"instant-crash", 5, verification.LedgerReport{Accounts: 3, Transactions: 2, Postings: 2, Holds: 1, Releases: 1}},
		{"empty-round", 3, verification.LedgerReport{}},
		{"in-progress", 2, verification.LedgerReport{Accounts: 2, Transactions: 1, Postings: 1, Holds: 1, Releases: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, pool := newDB(t)
			x := newExecutor(t, pool, nil, nil)

			golden := readGoldenV2(t, tc.name)
			n := (len(golden) - 1) / 2 // records
			goldenHeader := mustObject(t, golden[0])
			for _, v := range mustList(t, goldenHeader["accounts"]) {
				a := mustObj(t, v)
				if a["kind"] != canonjson.String("player") {
					continue
				}
				id, err := canonjson.ParseUUID(a["accountId"])
				if err != nil {
					t.Fatal(err)
				}
				player, err := canonjson.Text(a["player"])
				if err != nil {
					t.Fatal(err)
				}
				mustExec(t, pool, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced)
					VALUES ($1, 'player', $2, true)`, id, player)
			}

			gr := seedGolden(t, pool, x, tc.name, 0)
			executed, err := x.Drain(ctx, lease(gr.roundID), 100)
			if err != nil {
				t.Fatalf("Drain: %v", err)
			}
			if executed != len(gr.cmds) {
				t.Fatalf("Drain executed %d, want %d", executed, len(gr.cmds))
			}

			out := exportHistoryV2(t, pool, gr.roundID)
			rep, err := verifyExportV2(t, out)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if rep.Records != tc.last+1 {
				t.Fatalf("export ends at %d, want S = %d", rep.Records-1, tc.last)
			}
			if rep.Ledger == nil || *rep.Ledger != tc.ledger {
				t.Errorf("ledger = %+v, want %+v", rep.Ledger, tc.ledger)
			}

			lines := bytes.Split(bytes.TrimSuffix(out, []byte("\n")), []byte("\n"))
			s := tc.last
			if len(lines) != 1+2*(s+1) {
				t.Fatalf("export has %d lines, want %d", len(lines), 1+2*(s+1))
			}
			for i := 0; i <= s; i++ {
				if !bytes.Equal(lines[1+i], golden[1+i]) {
					t.Errorf("record %d differs from the golden:\n got %s\nwant %s", i, lines[1+i], golden[1+i])
				}
				if got, want := lines[2+s+i], golden[1+n+i]; !bytes.Equal(got, want) {
					t.Errorf("ledger line %d differs from the golden:\n got %s\nwant %s", i, got, want)
				}
			}

			refs := referencedAccounts(t, golden[1+n:1+n+s+1])
			var want canonjson.Array
			for _, v := range mustList(t, goldenHeader["accounts"]) {
				if refs[mustObj(t, v)["accountId"]] {
					want = append(want, v)
				}
			}
			if got := mustList(t, mustObject(t, lines[0])["accounts"]); !reflect.DeepEqual(got, want) && (len(got) != 0 || len(want) != 0) {
				t.Errorf("accounts = %v, want %v", got, want)
			}
		})
	}
}

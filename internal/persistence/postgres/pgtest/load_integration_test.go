//go:build integration

package pgtest_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

const goldenDir = "../../../audit/verification/testdata/golden"

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name+".jsonl")) //nolint:gosec // G304: a golden fixture under testdata
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// settledThenFails returns worked-example with record 22, the rejected
// PlaceBet after settlement, rewritten as an accepted record carrying a
// BetPlaced event, built in memory (no fixture changes). The chain links
// and binds, but its replay fails at 22: Evolve rejects any event once the
// round is settled.
func settledThenFails(t *testing.T) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSuffix(readGolden(t, "worked-example"), []byte("\n")), []byte("\n"))
	records := lines[1:]
	command := func(k int) chain.Command {
		t.Helper()
		rec, err := chain.Decode(records[k])
		if err != nil {
			t.Fatal(err)
		}
		c, ok := rec.(chain.Command)
		if !ok {
			t.Fatalf("record %d is not a command record", k)
		}
		return c
	}
	var next uint64 = 1
	for k := 1; k < 22; k++ {
		for _, e := range command(k).Events {
			next = e.Sequence + 1
		}
	}
	betPlaced := command(1).Events[0].Payload
	if !bytes.Contains(betPlaced, []byte(`"BetPlaced"`)) {
		t.Fatalf("record 1's first event %s is not BetPlaced", betPlaced)
	}
	c := command(22)
	if !c.Result.Rejected || len(c.Events) != 0 {
		t.Fatalf("record 22 = %+v, want a rejected record without events", c.Result)
	}
	c.Result = chain.Result{}
	c.Events = []chain.Event{{Sequence: next, Payload: betPlaced}}
	b, err := chain.EncodeCommand(c)
	if err != nil {
		t.Fatal(err)
	}
	records[22] = b
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

// TestLoadHistorySealsAndReveals: LoadHistory stores the secret config only
// sealed with the test keyring and writes the reveal at the record that
// finalizes a crash/v1 round, and no reveal when any part of the history
// fails to replay, even after that record (LBT-21 K-5, K-6).
func TestLoadHistorySealsAndReveals(t *testing.T) {
	ctx := context.Background()
	golden := func(name string) func(*testing.T) []byte {
		return func(t *testing.T) []byte { return readGolden(t, name) }
	}
	tests := []struct {
		name     string
		history  func(*testing.T) []byte
		revealed bool
		sequence uint64
	}{
		{"worked-example", golden("worked-example"), true, 21},
		{"in-progress", golden("in-progress"), false, 0},
		{"tamper/rules-version-2", golden("tamper/rules-version-2"), false, 0},
		{"settled, then a replay failure", settledThenFails, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The tamper fixtures share worked-example's round ID.
			pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
			l := pgtest.LoadHistory(t, pool, tt.history(t), nil)
			if l.Revealed != tt.revealed || l.RevealSequence != tt.sequence {
				t.Errorf("Revealed %v at %d, want %v at %d", l.Revealed, l.RevealSequence, tt.revealed, tt.sequence)
			}
			var (
				plain, nonce, ciphertext []byte
				scheme, keyID            *string
			)
			if err := pool.QueryRow(ctx, `SELECT config_secret, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext
				FROM orvex.round_secrets WHERE round_id = $1`, l.RoundID).Scan(&plain, &scheme, &keyID, &nonce, &ciphertext); err != nil {
				t.Fatal(err)
			}
			if plain != nil || scheme == nil || keyID == nil || *keyID != pgtest.KeyID {
				t.Fatalf("round_secrets: plaintext %q, scheme %v, key id %v; want sealed under %s", plain, scheme, keyID, pgtest.KeyID)
			}
			if bytes.Contains(ciphertext, l.Secret) {
				t.Error("the ciphertext contains the secret config")
			}
			opened, err := pgtest.Keyring().Open(l.RoundID, sealing.Sealed{Scheme: *scheme, KeyID: *keyID, Nonce: nonce, Ciphertext: ciphertext})
			if err != nil || !bytes.Equal(opened, l.Secret) {
				t.Errorf("Open = %q, %v; want %q", opened, err, l.Secret)
			}

			var seq int64
			var revealed []byte
			n := 0
			rows, err := pool.Query(ctx, `SELECT sequence, config_secret FROM orvex.round_secret_reveals WHERE round_id = $1`, l.RoundID)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				if err := rows.Scan(&seq, &revealed); err != nil {
					t.Fatal(err)
				}
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			switch {
			case !tt.revealed && n != 0:
				t.Errorf("%d reveals, want none", n)
			case tt.revealed && (n != 1 || uint64(seq) != tt.sequence || !bytes.Equal(revealed, l.Secret)): //nolint:gosec // G115: a golden history sequence (< 100)
				t.Errorf("%d reveals, at %d = %q; want one at %d = %q", n, seq, revealed, tt.sequence, l.Secret)
			}
		})
	}
}

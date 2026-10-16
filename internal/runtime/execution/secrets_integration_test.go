//go:build integration

package execution_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/persistence/sealing"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// Round secret sealing at rest (LBT-21,
// docs/specs/87d8d90c_round-secret-protection.md, K-5 to K-8).

// randomSeedWithDeadline draws seeds from crypto/rand until crash/v1 gives
// min <= D <= max µs.
func randomSeedWithDeadline(t *testing.T, roundID [16]byte, minD, maxD time.Duration) [32]byte {
	t.Helper()
	for range 10_000 {
		var seed [32]byte
		if _, err := rand.Read(seed[:]); err != nil {
			t.Fatal(err)
		}
		s, err := v1.Rules{}.InitialState(roundConfig(roundID, seed, 1_000_000_000, 100))
		if err != nil {
			continue
		}
		if d := s.CrashDeadlineOffsetMicros; d >= minD.Microseconds() && d <= maxD.Microseconds() {
			return seed
		}
	}
	t.Fatalf("no random seed with D in [%v, %v]", minD, maxD)
	return [32]byte{}
}

// tablesHolding counts, per table of schemas orvex and orvex_migrate, the
// rows whose text form contains the seed's lowercase hex or, for bytea
// printed as hex, the hex of that hex text's ASCII bytes. It returns the
// tables with a match.
func tablesHolding(t *testing.T, pool *pgxpool.Pool, seed [32]byte) []string {
	t.Helper()
	seedHex := hex.EncodeToString(seed[:])
	asciiHex := hex.EncodeToString([]byte(seedHex))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL bytea_output = 'hex'`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `SELECT schemaname, tablename FROM pg_tables
		WHERE schemaname IN ('orvex', 'orvex_migrate') ORDER BY schemaname, tablename`)
	if err != nil {
		t.Fatal(err)
	}
	type table struct{ Schema, Name string }
	tables, err := pgx.CollectRows(rows, pgx.RowToStructByPos[table])
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) < 16 {
		t.Fatalf("scanned %d tables, want every table of orvex and orvex_migrate", len(tables))
	}
	var holding []string
	for _, tb := range tables {
		var n int64
		q := `SELECT count(*) FROM ` + pgx.Identifier{tb.Schema, tb.Name}.Sanitize() +
			` t WHERE strpos(t::text, $1) > 0 OR strpos(t::text, $2) > 0`
		if err := tx.QueryRow(ctx, q, seedHex, asciiHex).Scan(&n); err != nil {
			t.Fatalf("%s.%s: %v", tb.Schema, tb.Name, err)
		}
		if n > 0 {
			holding = append(holding, tb.Name)
		}
	}
	sort.Strings(holding)
	return holding
}

// TestSeedNotStoredInPlaintext: a round created and executed up to a
// cash-out stores its random seed nowhere in plaintext; once settled, the
// seed is public in the reveal, the RoundSettled record and its outbox
// message only (LBT-21 acceptance 1).
func TestSeedNotStoredInPlaintext(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	s := newSequencer(t, pool)
	round := newRoundID()
	seedBytes := randomSeedWithDeadline(t, round, 500*time.Millisecond, 2*time.Second)
	cfg := createRound(t, x, round, seedBytes, 1_000_000_000, 100)
	initial, err := v1.Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := initial.CrashDeadlineOffsetMicros
	roundtest.FundPlayer(t, pool, "alice", 100)
	roundtest.FundPlayer(t, pool, "bob", 200)

	mustAdmit(t, s, request(round, cmdID(1), placeBet("alice", 100)))
	mustAdmit(t, s, request(round, cmdID(2), placeBet("bob", 200)))
	start := mustAdmit(t, s, request(round, cmdID(3), startRound))
	mustDrain(t, x, round, 3)
	t0 := start.EffectiveTimeUnixMicro
	waitDBClock(t, pool, t0+1)
	mustAdmit(t, s, request(round, cmdID(4), cashOut("alice")))
	mustDrain(t, x, round, 1)

	if got := tablesHolding(t, pool, seedBytes); len(got) != 0 {
		t.Fatalf("the unsettled round's seed is stored in plaintext in %v", got)
	}

	closeReq := request(round, cmdID(5), closeRound)
	if _, err := s.Admit(ctx, closeReq); err != nil {
		if !errors.Is(err, admission.ErrClosureNotDue) {
			t.Fatalf("CloseRound: %v", err)
		}
		waitDBClock(t, pool, t0+d)
		mustAdmit(t, s, closeReq)
	}
	mustDrain(t, x, round, 1)
	if got := tablesHolding(t, pool, seedBytes); len(got) != 0 {
		t.Fatalf("the crashed, unsettled round's seed is stored in plaintext in %v", got)
	}
	mustAdmit(t, s, request(round, cmdID(6), settleRound))
	mustDrain(t, x, round, 1)
	if got, want := tablesHolding(t, pool, seedBytes), []string{"chain_records", "outbox_messages", "round_secret_reveals"}; !equalStrings(got, want) {
		t.Errorf("after settlement the seed is in %v, want exactly %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// goldenSecret returns the canonical secret config of a golden header.
func goldenSecret(t *testing.T, header canonjson.Object) []byte {
	t.Helper()
	o, err := canonjson.Obj(header["configSecret"])
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonjson.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// reveals returns the round's reveal rows as sequence -> secret.
func reveals(t *testing.T, pool *pgxpool.Pool, round [16]byte) map[int64][]byte {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT sequence, config_secret FROM orvex.round_secret_reveals WHERE round_id = $1`, round)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64][]byte{}
	for rows.Next() {
		var seq int64
		var b []byte
		if err := rows.Scan(&seq, &b); err != nil {
			t.Fatal(err)
		}
		out[seq] = b
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRevealWrittenAtSettlement: the record that settles the round, and only
// it, writes the reveal (LBT-21 K-6).
func TestRevealWrittenAtSettlement(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	gr := roundtest.SeedGolden(t, pool, x, goldenPath("worked-example"), 0)
	secret := goldenSecret(t, gr.Header)
	for k := 1; k <= len(gr.Commands); k++ {
		out, err := x.ExecuteNext(ctx, lease(gr.RoundID))
		if err != nil || out.Sequence != uint64(k) { //nolint:gosec // G115: k is a golden sequence (< 100)
			t.Fatalf("ExecuteNext %d = %+v, %v", k, out, err)
		}
		got := reveals(t, pool, gr.RoundID)
		switch {
		case k < 21 && len(got) != 0:
			t.Fatalf("after record %d: %d reveals, want none", k, len(got))
		case k >= 21 && (len(got) != 1 || !bytes.Equal(got[21], secret)):
			t.Fatalf("after record %d: reveals %v, want one at 21 = %s", k, got, secret)
		}
	}
	if len(gr.Commands) != 22 {
		t.Errorf("worked-example has %d command records, want 22", len(gr.Commands))
	}
}

// TestColdLoadOpensSealedSecret: a cold load opens the sealed secret.
func TestColdLoadOpensSealedSecret(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	gr := roundtest.SeedGolden(t, pool, x, goldenPath("worked-example"), 0)
	mustDrain10 := func() {
		if n, err := x.Drain(ctx, lease(gr.RoundID), 10); err != nil || n != 10 {
			t.Fatalf("Drain = %d, %v; want 10", n, err)
		}
	}
	mustDrain10()
	x.Forget(gr.RoundID)
	out, err := x.ExecuteNext(ctx, lease(gr.RoundID))
	if err != nil || out.Sequence != 11 || !bytes.Equal(out.Record, gr.Lines[11]) {
		t.Fatalf("ExecuteNext after Forget = %+v, %v; want golden record 11", out, err)
	}
}

// otherKeyring holds only the key ID "other".
func otherKeyring(t *testing.T) *sealing.Keyring {
	t.Helper()
	k, err := sealing.NewKeyring("other", map[string][]byte{"other": bytes.Repeat([]byte{0x6f}, sealing.KeySize)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestColdLoadPrefersReveal: after settlement the reveal serves the cold
// load, so an executor without the sealing key continues the round.
func TestColdLoadPrefersReveal(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	gr := roundtest.SeedGolden(t, pool, x, goldenPath("worked-example"), 0)
	if n, err := x.Drain(ctx, lease(gr.RoundID), 21); err != nil || n != 21 {
		t.Fatalf("Drain = %d, %v; want 21", n, err)
	}
	other := otherKeyring(t)
	y := roundtest.NewExecutor(t, pool, func(c *execution.Config) { c.Keyring = other }, nil)
	out, err := y.ExecuteNext(ctx, lease(gr.RoundID))
	if err != nil || out.Sequence != 22 || !bytes.Equal(out.Record, gr.Lines[22]) {
		t.Fatalf("ExecuteNext without the sealing key = %+v, %v; want golden record 22", out, err)
	}
}

// drainedInProgress seeds and drains the in-progress golden round with a
// pgtest-keyring executor: sealed, unsettled, no reveal.
func drainedInProgress(t *testing.T, pool *pgxpool.Pool) (roundtest.Golden, [32]byte) {
	t.Helper()
	x := roundtest.NewExecutor(t, pool, nil, nil)
	gr := roundtest.SeedGolden(t, pool, x, goldenPath("in-progress"), 0)
	if n, err := x.Drain(ctx, lease(gr.RoundID), 10); err != nil || n != len(gr.Commands) {
		t.Fatalf("Drain = %d, %v; want %d", n, err, len(gr.Commands))
	}
	_, cfg := roundtest.GoldenConfig(t, gr.Header, gr.Lines[0])
	return gr, cfg.Seed
}

// TestColdLoadUnknownKey: an executor whose keyring lacks the sealing key
// halts an unrevealed round with ErrCorrupt wrapping sealing.ErrUnknownKey,
// and no error text holds the seed.
func TestColdLoadUnknownKey(t *testing.T) {
	_, pool := newDB(t)
	gr, seedBytes := drainedInProgress(t, pool)
	other := otherKeyring(t)
	y := roundtest.NewExecutor(t, pool, func(c *execution.Config) { c.Keyring = other }, nil)
	_, err := y.ExecuteNext(ctx, lease(gr.RoundID))
	if !errors.Is(err, execution.ErrCorrupt) || !errors.Is(err, sealing.ErrUnknownKey) {
		t.Fatalf("ExecuteNext = %v, want ErrCorrupt wrapping sealing.ErrUnknownKey", err)
	}
	if strings.Contains(err.Error(), hex.EncodeToString(seedBytes[:])) {
		t.Errorf("error %q holds the seed", err)
	}
	if _, again := y.ExecuteNext(ctx, lease(gr.RoundID)); !errors.Is(again, execution.ErrCorrupt) || !errors.Is(again, sealing.ErrUnknownKey) {
		t.Errorf("second ExecuteNext = %v, want the same halt", again)
	}
}

// TestColdLoadTamperedSecret: a flipped ciphertext byte fails
// authentication at cold load.
func TestColdLoadTamperedSecret(t *testing.T) {
	_, pool := newDB(t)
	gr, seedBytes := drainedInProgress(t, pool)
	pgtest.ReplicaExec(t, pool, `UPDATE orvex.round_secrets
		SET secret_ciphertext = set_byte(secret_ciphertext, 0, get_byte(secret_ciphertext, 0) # 1) WHERE round_id = $1`, gr.RoundID)
	z := roundtest.NewExecutor(t, pool, nil, nil)
	_, err := z.ExecuteNext(ctx, lease(gr.RoundID))
	if !errors.Is(err, execution.ErrCorrupt) || !errors.Is(err, sealing.ErrOpen) {
		t.Fatalf("ExecuteNext = %v, want ErrCorrupt wrapping sealing.ErrOpen", err)
	}
	if strings.Contains(err.Error(), hex.EncodeToString(seedBytes[:])) {
		t.Errorf("error %q holds the seed", err)
	}
}

// TestLegacySecretRoundCompletes: a round written before migration 0010,
// with its plaintext secret config, settles after the migration, and its
// reveal equals the legacy plaintext (LBT-21 K-11).
func TestLegacySecretRoundCompletes(t *testing.T) {
	dsn := pgtest.EmptyDatabase(t)
	pgtest.MigrateUpTo(t, dsn, 9)
	pool := pgtest.Pool(t, dsn)
	path := filepath.Join(goldenV2Dir, "in-progress.jsonl")
	history, err := os.ReadFile(path) //nolint:gosec // G304: fixed golden fixture under testdata
	if err != nil {
		t.Fatal(err)
	}
	l := pgtest.LoadLegacyHistory(t, pool, history, nil)
	mustExec(t, pool, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, round_id, floor_enforced)
		VALUES ($1, 'system', 'house', NULL, false)`, ledger.HouseAccountID())
	pgtest.MigrateUpTo(t, dsn, 10)

	header, lines := roundtest.ReadGolden(t, path)
	_, cfg := roundtest.GoldenConfig(t, header, lines[0])
	initial, err := v1.Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var t0 int64 = -1
	for _, line := range lines[1:l.Records] {
		rec, err := chain.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		c := rec.(chain.Command)
		cmd, err := v1.Adapter().DecodeCommand(c.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cmd.(v1.StartRound); ok {
			t0 = c.EffectiveTimeUnixMicro
		}
	}
	if t0 < 0 {
		t.Fatal("in-progress has no StartRound record")
	}
	closeAt := t0 + initial.CrashDeadlineOffsetMicros
	seed(t, pool, l.RoundID, 901, closeRound, closeAt)
	seed(t, pool, l.RoundID, 902, settleRound, closeAt)

	x := roundtest.NewExecutor(t, pool, nil, nil)
	var last execution.Outcome
	for range 2 {
		out, err := x.ExecuteNext(ctx, lease(l.RoundID))
		if err != nil || !out.Executed || out.Result.Rejected {
			t.Fatalf("ExecuteNext = %+v, %v; want accepted", out, err)
		}
		last = out
	}
	if !last.Final {
		t.Errorf("SettleRound outcome %+v is not final", last)
	}
	got := reveals(t, pool, l.RoundID)
	if len(got) != 1 || !bytes.Equal(got[int64(last.Sequence)], l.Secret) { //nolint:gosec // G115: a golden sequence (< 100)
		t.Errorf("reveals %v, want one at %d = the legacy plaintext %s", got, last.Sequence, l.Secret)
	}
}

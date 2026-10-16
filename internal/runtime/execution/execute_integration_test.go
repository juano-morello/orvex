//go:build integration

package execution_test

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// goldenExpectation is spec section 8 for one golden history.
type goldenExpectation struct {
	transactions map[int64]string
	snapshots    []int64
	releases     int64
	players      map[string]account
	round        account
	house        account
	final        bool
}

var goldenExpectations = map[string]goldenExpectation{
	"worked-example": {
		transactions: map[int64]string{1: "crash_bet", 2: "crash_bet", 3: "crash_bet", 12: "crash_cash_out", 17: "crash_cash_out", 21: "crash_settlement"},
		snapshots:    []int64{0, 9, 19, 21},
		releases:     7,
		players: map[string]account{
			"alice": {debits: 100, credits: 284},
			"bob":   {debits: 250, credits: 250},
			"dave":  {credits: 10},
			"erin":  {credits: 10},
			"frank": {credits: 10},
		},
		round: account{debits: 1_757_600_134, credits: 1_757_600_134},
		house: account{debits: 757_599_784},
		final: true,
	},
	"instant-crash": {
		transactions: map[int64]string{1: "crash_bet", 5: "crash_settlement"},
		snapshots:    []int64{0, 2, 4, 5},
		releases:     1,
		players:      map[string]account{"alice": {debits: 500, credits: 500}},
		round:        account{debits: 500, credits: 500},
		house:        account{credits: 500},
		final:        true,
	},
	"empty-round": {
		transactions: map[int64]string{},
		snapshots:    []int64{0, 1, 2, 3},
		final:        true,
	},
	"in-progress": {
		transactions: map[int64]string{1: "crash_bet", 3: "crash_cash_out"},
		snapshots:    []int64{0, 2},
		releases:     1,
		players:      map[string]account{"alice": {debits: 1000, credits: 1000 + 3320}},
		round:        account{debits: 3320, credits: 1000},
	},
}

func TestExecuteReproducesGoldenHistories(t *testing.T) {
	for name, want := range goldenExpectations {
		t.Run(name, func(t *testing.T) {
			_, pool := newDB(t)
			x := newExecutor(t, pool, nil, nil)
			gr := seedGolden(t, pool, x, name, 0)
			n, err := x.Drain(ctx, lease(gr.roundID), 100)
			if err != nil {
				t.Fatalf("Drain: %v", err)
			}
			if n != len(gr.cmds) {
				t.Fatalf("Drain executed %d, want %d", n, len(gr.cmds))
			}
			requireGoldenRecords(t, pool, gr)
			requireGoldenLedger(t, pool, gr, want)
			requireExportVerifies(t, pool, gr, want.snapshots[len(want.snapshots)-1])
		})
	}
}

// requireGoldenRecords compares records, results and events with the
// golden history.
func requireGoldenRecords(t *testing.T, pool *pgxpool.Pool, gr goldenRound) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT sequence, record FROM orvex.chain_records WHERE round_id = $1 ORDER BY sequence`, gr.roundID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var seq int64
		var rec []byte
		if err := rows.Scan(&seq, &rec); err != nil {
			t.Fatal(err)
		}
		if seq != int64(i) || i >= len(gr.lines) || !bytes.Equal(rec, gr.lines[i]) {
			t.Errorf("record %d differs from the golden line:\n got %s\nwant %s", seq, rec, gr.lines[min(i, len(gr.lines)-1)])
		}
		i++
	}
	if i != len(gr.lines) {
		t.Errorf("%d records, want %d", i, len(gr.lines))
	}
	for _, c := range gr.cmds {
		var status string
		var reason *string
		if err := pool.QueryRow(ctx, `SELECT status, reason FROM orvex.command_results WHERE command_id = $1`, c.CommandID).
			Scan(&status, &reason); err != nil {
			t.Fatalf("command_results %d: %v", c.Sequence, err)
		}
		if c.Result.Rejected != (status == "rejected") || (reason != nil) != c.Result.Rejected || (reason != nil && *reason != c.Result.Reason) {
			t.Errorf("command_results %d = %s %v, want %+v", c.Sequence, status, reason, c.Result)
		}
	}
	var events int64
	for _, c := range gr.cmds {
		events += int64(len(c.Events))
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.domain_events WHERE round_id = $1`, gr.roundID); n != events {
		t.Errorf("%d domain events, want %d", n, events)
	}
}

// requireGoldenLedger checks ledger rows, releases, snapshots, outbox rows
// and final totals against spec section 8.
func requireGoldenLedger(t *testing.T, pool *pgxpool.Pool, gr goldenRound, want goldenExpectation) {
	t.Helper()
	kinds := map[int64]string{}
	rows, err := pool.Query(ctx, `SELECT transaction_id, sequence, kind FROM orvex.ledger_transactions WHERE round_id = $1`, gr.roundID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id [16]byte
		var seq int64
		var kind string
		if err := rows.Scan(&id, &seq, &kind); err != nil {
			t.Fatal(err)
		}
		if id != ledger.TransactionID(gr.roundID, uint64(seq)) { //nolint:gosec // G115: seq is a ledger_transactions.sequence of this test round (>= 1)
			t.Errorf("ledger transaction of record %d has a non-deterministic ID", seq)
		}
		kinds[seq] = kind
	}
	rows.Close()
	if !reflect.DeepEqual(kinds, want.transactions) {
		t.Errorf("ledger transactions = %v, want %v", kinds, want.transactions)
	}
	// One posting per money event, at the event of its record.
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_postings p
		JOIN orvex.ledger_transactions x USING (transaction_id)
		JOIN orvex.domain_events d ON d.round_id = p.round_id AND d.event_sequence = p.event_sequence
		WHERE x.round_id = $1 AND d.sequence = x.sequence`, gr.roundID); n != int64(len(want.transactions)) {
		t.Errorf("%d postings bound to their record's events, want %d", n, len(want.transactions))
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_hold_releases WHERE round_id = $1`, gr.roundID); n != want.releases {
		t.Errorf("%d releases, want %d", n, want.releases)
	}
	var snaps []int64
	rows, err = pool.Query(ctx, `SELECT sequence FROM orvex.round_snapshots WHERE round_id = $1 AND state IS NULL AND engine_build = 'golden' ORDER BY sequence`, gr.roundID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, s)
	}
	rows.Close()
	if !reflect.DeepEqual(snaps, want.snapshots) {
		t.Errorf("snapshots = %v, want %v", snaps, want.snapshots)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.outbox_messages o JOIN orvex.chain_records r USING (round_id, sequence)
		WHERE o.round_id = $1 AND o.message_index = 0 AND o.topic = 'round.record.v1' AND o.payload = r.record`, gr.roundID); n != int64(len(gr.lines)) {
		t.Errorf("%d outbox messages equal to their records, want %d", n, len(gr.lines))
	}
	for p, a := range want.players {
		if got := playerAccount(t, pool, p); got != a {
			t.Errorf("%s = %+v, want %+v", p, got, a)
		}
	}
	if name := "worked-example"; gr.roundID == mustGoldenRound(t, name) {
		p3 := string(goldenP3(t, gr))
		if got := playerAccount(t, pool, p3); got != (account{debits: 1_000_000_000, credits: 2_757_600_000}) {
			t.Errorf("P3 = %+v", got)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_accounts WHERE kind = 'player' AND held <> 0`); n != 0 {
		t.Errorf("%d player accounts still hold credits", n)
	}
	if got := accountByID(t, pool, ledger.RoundAccountID(gr.roundID)); got != want.round {
		t.Errorf("round account = %+v, want %+v", got, want.round)
	}
	if got := accountByID(t, pool, ledger.HouseAccountID()); got != want.house {
		t.Errorf("house account = %+v, want %+v", got, want.house)
	}
	requireBalanced(t, pool)
}

// requireBalanced requires Σ debits_posted = Σ credits_posted over all
// accounts.
func requireBalanced(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var debits, credits int64
	if err := pool.QueryRow(ctx, `SELECT coalesce(sum(debits_posted), 0), coalesce(sum(credits_posted), 0) FROM orvex.ledger_accounts`).
		Scan(&debits, &credits); err != nil {
		t.Fatal(err)
	}
	if debits != credits {
		t.Errorf("Σ debits %d != Σ credits %d", debits, credits)
	}
}

func mustGoldenRound(t *testing.T, name string) [16]byte {
	t.Helper()
	header, lines := readGolden(t, name)
	g, _ := goldenConfig(t, header, lines[0])
	return g.RoundID
}

// goldenP3 returns the player of worked-example record 3 (LBT-9 §5.6).
func goldenP3(t *testing.T, gr goldenRound) v1.PlayerID {
	t.Helper()
	cmd, err := v1.Adapter().DecodeCommand(gr.cmds[2].Payload)
	if err != nil {
		t.Fatal(err)
	}
	return cmd.(v1.PlaceBet).Player
}

// requireExportVerifies exports the round, verifies it with decision replay
// and compares its record lines with the golden lines 0..newest.
func requireExportVerifies(t *testing.T, pool *pgxpool.Pool, gr goldenRound, newest int64) {
	t.Helper()
	b := exportHistory(t, pool, gr.roundID)
	if _, err := verification.Verify(bytes.NewReader(b), verification.Options{Mode: verification.ModeVerify, Registry: crashRegistry(t)}); err != nil {
		t.Fatalf("Verify(export): %v", err)
	}
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))[1:]
	if int64(len(lines)) != newest+1 {
		t.Fatalf("export has %d records, want %d", len(lines), newest+1)
	}
	for i, l := range lines {
		if !bytes.Equal(l, gr.lines[i]) {
			t.Errorf("export record %d differs from the golden line", i)
		}
	}
}

func TestExecuteIdle(t *testing.T) {
	_, pool := newDB(t)
	x := newExecutor(t, pool, nil, nil)
	round := newRoundID()
	createRound(t, x, round, testSeed, 1_000_000_000, 100)
	before := effects(t, pool, round)
	out, err := x.ExecuteNext(ctx, lease(round))
	if err != nil || out.Executed {
		t.Fatalf("ExecuteNext = %+v, %v; want not executed", out, err)
	}
	x.Forget(round)
	if out, err := x.ExecuteNext(ctx, lease(round)); err != nil || out.Executed {
		t.Fatalf("ExecuteNext after a cold load = %+v, %v; want not executed", out, err)
	}
	requireEffects(t, effects(t, pool, round), before)
}

func TestExecuteBetConsumesHoldAtExactBalance(t *testing.T) {
	_, pool := newDB(t)
	x := newExecutor(t, pool, nil, nil)
	round := newRoundID()
	createRound(t, x, round, testSeed, 1_000_000_000, 100)
	fundPlayer(t, pool, "alice", 100)
	seed(t, pool, round, 1, placeBet("alice", 100), 1_000)
	if got := playerAccount(t, pool, "alice"); got != (account{credits: 100, held: 100}) {
		t.Fatalf("alice before = %+v", got)
	}
	out, err := x.ExecuteNext(ctx, lease(round))
	if err != nil || !out.Executed || out.Result.Rejected {
		t.Fatalf("ExecuteNext = %+v, %v; want accepted", out, err)
	}
	if got := playerAccount(t, pool, "alice"); got != (account{debits: 100, credits: 100}) {
		t.Errorf("alice after = %+v, want debits 100, credits 100, held 0", got)
	}
	if got := accountByID(t, pool, ledger.RoundAccountID(round)); got != (account{credits: 100}) {
		t.Errorf("round account = %+v", got)
	}
}

func TestExecuteRejectedBetReleasesHold(t *testing.T) {
	_, pool := newDB(t)
	x := newExecutor(t, pool, nil, nil)
	round := newRoundID()
	createRound(t, x, round, testSeed, 100, 100)
	fundPlayer(t, pool, "alice", 200)
	seed(t, pool, round, 1, placeBet("alice", 150), 1_000)
	out, err := x.ExecuteNext(ctx, lease(round))
	if err != nil || !out.Result.Rejected || out.Result.Reason != string(v1.ReasonInvalidAmount) {
		t.Fatalf("ExecuteNext = %+v, %v; want rejected invalid_amount", out, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_hold_releases WHERE round_id = $1`, round); n != 1 {
		t.Errorf("%d releases, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_transactions WHERE round_id = $1`, round); n != 0 {
		t.Errorf("%d ledger transactions, want 0", n)
	}
	if got := playerAccount(t, pool, "alice"); got != (account{credits: 200}) {
		t.Errorf("alice = %+v, want held 0", got)
	}
}

func TestExecuteSnapshots(t *testing.T) {
	_, pool := newDB(t)
	x := newExecutor(t, pool, nil, nil)
	gr := seedGolden(t, pool, x, "worked-example", 0)
	if _, err := x.Drain(ctx, lease(gr.roundID), 100); err != nil {
		t.Fatal(err)
	}
	// Replay the golden events and compare the state hash at every
	// snapshot.
	a := v1.Adapter()
	_, cfg := goldenConfig(t, gr.header, gr.lines[0])
	var s any
	s, err := a.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[int64]chain.Hash{0: mustStateHash(t, s)}
	for _, c := range gr.cmds {
		for _, ev := range c.Events {
			e, err := a.DecodeEvent(ev.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if s, err = a.Evolve(s, e); err != nil {
				t.Fatal(err)
			}
		}
		hashes[int64(c.Sequence)] = mustStateHash(t, s) //nolint:gosec // G115: c.Sequence is a golden history sequence (< 100)
	}
	rows, err := pool.Query(ctx, `SELECT sequence, state_hash, state IS NULL, engine_build FROM orvex.round_snapshots WHERE round_id = $1 ORDER BY sequence`, gr.roundID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var seq int64
		var h []byte
		var null bool
		var build string
		if err := rows.Scan(&seq, &h, &null, &build); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
		want := hashes[seq]
		if !bytes.Equal(h, want[:]) || !null || build != "golden" {
			t.Errorf("snapshot %d: hash %x null %v build %q; want %x, NULL, golden", seq, h, null, build, want)
		}
	}
	if !reflect.DeepEqual(seqs, []int64{0, 9, 19, 21}) {
		t.Errorf("snapshots at %v, want [0 9 19 21]", seqs)
	}
}

func mustStateHash(t *testing.T, s any) chain.Hash {
	t.Helper()
	b, err := v1.Adapter().EncodeState(s)
	if err != nil {
		t.Fatal(err)
	}
	h, err := chain.StateHash(v1.GameID, v1.StateVersion, b)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestExecuteOutbox(t *testing.T) {
	_, pool := newDB(t)
	x := newExecutor(t, pool, nil, nil)
	gr := seedGolden(t, pool, x, "worked-example", 0)
	if _, err := x.Drain(ctx, lease(gr.roundID), 100); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT sequence, message_index, topic, payload FROM orvex.outbox_messages WHERE round_id = $1 ORDER BY sequence`, gr.roundID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	crashed, settled := int64(19), int64(21)
	secretKeys := []string{`"crashPointScaled"`, `"crashTick"`, `"crashDeadlineUnixMicro"`}
	seedHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	i := int64(0)
	for rows.Next() {
		var seq int64
		var idx int32
		var topic string
		var payload []byte
		if err := rows.Scan(&seq, &idx, &topic, &payload); err != nil {
			t.Fatal(err)
		}
		if seq != i || idx != 0 || topic != execution.OutboxTopicRecord || !bytes.Equal(payload, gr.lines[i]) {
			t.Errorf("outbox %d = (%d, %d, %q), payload equal %v", i, seq, idx, topic, bytes.Equal(payload, gr.lines[min(int(i), len(gr.lines)-1)]))
		}
		if seq < crashed {
			for _, k := range secretKeys {
				if strings.Contains(string(payload), k) {
					t.Errorf("outbox %d before the crash carries %s", seq, k)
				}
			}
		}
		if seq < settled && strings.Contains(string(payload), seedHex) {
			t.Errorf("outbox %d before settlement carries the seed", seq)
		}
		i++
	}
	if i != int64(len(gr.lines)) || i != 23 {
		t.Errorf("%d outbox messages, want %d (23)", i, len(gr.lines))
	}
}

func TestColdLoadMatchesCache(t *testing.T) {
	// Uninterrupted run.
	_, poolA := newDB(t)
	xa := newExecutor(t, poolA, nil, nil)
	ga := seedGolden(t, poolA, xa, "worked-example", 0)
	if _, err := xa.Drain(ctx, lease(ga.roundID), 100); err != nil {
		t.Fatal(err)
	}
	// Half, Forget, the rest.
	_, poolB := newDB(t)
	xb := newExecutor(t, poolB, nil, nil)
	gb := seedGolden(t, poolB, xb, "worked-example", 0)
	if n, err := xb.Drain(ctx, lease(gb.roundID), 11); err != nil || n != 11 {
		t.Fatalf("Drain(11) = %d, %v", n, err)
	}
	xb.Forget(gb.roundID)
	if _, err := xb.Drain(ctx, lease(gb.roundID), 100); err != nil {
		t.Fatal(err)
	}
	records := func(pool *pgxpool.Pool, roundID [16]byte) [][]byte {
		var out [][]byte
		rows, err := pool.Query(ctx, `SELECT record FROM orvex.chain_records WHERE round_id = $1 ORDER BY sequence`, roundID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var r []byte
			if err := rows.Scan(&r); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	if a, b := records(poolA, ga.roundID), records(poolB, gb.roundID); !reflect.DeepEqual(a, b) || len(a) != 23 {
		t.Errorf("records after a cold load differ from an uninterrupted run (%d vs %d)", len(a), len(b))
	}
}

func TestColdLoadChecksSnapshots(t *testing.T) {
	header, lines := readGolden(t, "worked-example")
	history := func(alter bool) []byte {
		h := header
		if alter {
			h = alterCheckpoint(t, header, 9)
		}
		hb, err := canonMarshal(h)
		if err != nil {
			t.Fatal(err)
		}
		return append(append(hb, '\n'), append(bytes.Join(lines, []byte("\n")), '\n')...)
	}

	_, pool := newDB(t)
	l := loadHistory(t, pool, history(false))
	x := newExecutor(t, pool, nil, nil)
	if out, err := x.ExecuteNext(ctx, lease(l)); err != nil || out.Executed {
		t.Fatalf("ExecuteNext = %+v, %v; want not executed after a matching cold load", out, err)
	}

	_, pool2 := newDB(t)
	l2 := loadHistory(t, pool2, history(true))
	fdb := &faultDB{pool: pool2}
	x2 := newExecutor(t, fdb, nil, nil)
	before := effects(t, pool2, l2)
	if _, err := x2.ExecuteNext(ctx, lease(l2)); !errors.Is(err, execution.ErrCorrupt) {
		t.Fatalf("ExecuteNext = %v, want ErrCorrupt", err)
	}
	begins := fdb.begins.Load()
	if _, err := x2.ExecuteNext(ctx, lease(l2)); !errors.Is(err, execution.ErrCorrupt) {
		t.Fatalf("second ExecuteNext = %v, want ErrCorrupt", err)
	}
	if fdb.begins.Load() != begins {
		t.Error("a halted round queried the database")
	}
	requireEffects(t, effects(t, pool2, l2), before)
}

func TestFailClosedHaltsRound(t *testing.T) {
	_, pool := newDB(t)
	fdb := &faultDB{pool: pool}
	fa := newFailingAdapter(2, 0)
	x := newExecutor(t, fdb, nil, failingRegistry(t, fa))
	round := newRoundID()
	createRound(t, x, round, testSeed, 1_000_000_000, 100)
	fundPlayer(t, pool, "alice", 100)
	fundPlayer(t, pool, "bob", 100)
	seed(t, pool, round, 1, placeBet("alice", 10), 1_000)
	seed(t, pool, round, 2, placeBet("bob", 20), 1_001)
	seed(t, pool, round, 3, placeBet("carol", 0), 1_002)

	if out, err := x.ExecuteNext(ctx, lease(round)); err != nil || out.Sequence != 1 {
		t.Fatalf("ExecuteNext 1 = %+v, %v", out, err)
	}
	before := effects(t, pool, round)
	_, err := x.ExecuteNext(ctx, lease(round))
	var fc *execution.FailClosedError
	if !errors.As(err, &fc) || fc.Sequence != 2 || fc.RoundID != round || !errors.Is(err, execution.ErrFailClosed) || !errors.Is(err, errBoom) {
		t.Fatalf("ExecuteNext 2 = %v, want FailClosedError at 2", err)
	}
	requireEffects(t, effects(t, pool, round), before)
	if got := playerAccount(t, pool, "bob"); got.held != 20 {
		t.Errorf("bob held %d, want 20", got.held)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.outbox_messages WHERE round_id = $1 AND sequence = 2`, round); n != 0 {
		t.Errorf("outbox row for 2 exists")
	}
	begins := fdb.begins.Load()
	if _, err := x.ExecuteNext(ctx, lease(round)); !errors.As(err, &fc) || fc.Sequence != 2 {
		t.Fatalf("halted ExecuteNext = %v", err)
	}
	if fdb.begins.Load() != begins {
		t.Error("a halted round began a transaction")
	}
	x.Forget(round)
	if _, err := x.ExecuteNext(ctx, lease(round)); !errors.As(err, &fc) || fc.Sequence != 2 {
		t.Fatalf("ExecuteNext after Forget = %v, want the failure again", err)
	}
	if n := count(t, pool, `SELECT processed_sequence FROM orvex.round_execution_heads WHERE round_id = $1`, round); n != 1 {
		t.Errorf("processed_sequence = %d, want 1", n)
	}
}

func TestSettlementResolvesEarlierCommands(t *testing.T) {
	run := func(t *testing.T, x *execution.Executor, pool *pgxpool.Pool, round [16]byte) {
		t.Helper()
		_, err := x.Drain(ctx, lease(round), 100)
		if err != nil {
			var fc *execution.FailClosedError
			if !errors.As(err, &fc) {
				t.Fatalf("Drain: %v", err)
			}
		}
	}
	setup := func(t *testing.T, x *execution.Executor, pool *pgxpool.Pool) ([16]byte, int64) {
		t.Helper()
		round := newRoundID()
		seed30 := seedWithDeadline(t, round, 30_000_000_000, 150_000_000_000)
		cfg := createRound(t, x, round, seed30, 1_000_000_000, 100)
		s, err := v1.Rules{}.InitialState(cfg)
		if err != nil {
			t.Fatal(err)
		}
		d := s.CrashDeadlineOffsetMicros
		for _, p := range []string{"alice", "bob", "carol"} {
			fundPlayer(t, pool, p, 1000)
		}
		t0 := int64(1_760_000_000_000_000)
		seed(t, pool, round, 1, placeBet("alice", 100), t0-3)
		seed(t, pool, round, 2, placeBet("bob", 200), t0-2)
		seed(t, pool, round, 3, placeBet("carol", 300), t0-1)
		seed(t, pool, round, 4, startRound, t0)
		seed(t, pool, round, 5, cashOut("alice"), t0+d/2)
		seed(t, pool, round, 6, cashOut("carol"), t0+d/2+1)
		seed(t, pool, round, 7, closeRound, t0+d)
		seed(t, pool, round, 8, settleRound, t0+d+1)
		return round, d
	}
	t.Run("settles", func(t *testing.T) {
		_, pool := newDB(t)
		x := newExecutor(t, pool, nil, nil)
		round, _ := setup(t, x, pool)
		run(t, x, pool, round)
		if n := count(t, pool, `SELECT processed_sequence FROM orvex.round_execution_heads WHERE round_id = $1`, round); n != 8 {
			t.Fatalf("processed %d, want 8", n)
		}
		// Decode every persisted record of the round.
		placed := map[string]int64{}
		cashedOut := map[string]int64{}
		var (
			settlement []chain.Event
			records    int
		)
		rows, err := pool.Query(ctx, `SELECT sequence, record FROM orvex.chain_records WHERE round_id = $1 AND sequence > 0 ORDER BY sequence`, round)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var seq int64
			var b []byte
			if err := rows.Scan(&seq, &b); err != nil {
				t.Fatal(err)
			}
			records++
			rec, err := chain.Decode(b)
			if err != nil {
				t.Fatal(err)
			}
			c := rec.(chain.Command)
			if c.Sequence != uint64(records) {
				t.Fatalf("record %d at position %d", c.Sequence, records)
			}
			if seq == 8 {
				settlement = c.Events
				continue
			}
			for _, ev := range c.Events {
				e, err := v1.Adapter().DecodeEvent(ev.Payload)
				if err != nil {
					t.Fatal(err)
				}
				switch e := e.(type) {
				case v1.BetPlaced:
					placed[string(e.Player)] = e.BetCredits
				case v1.BetCashedOut:
					cashedOut[string(e.Player)] = e.PayoutCredits
				}
			}
		}
		rows.Close()
		if records != 8 {
			t.Fatalf("%d command records, want 8", records)
		}
		if !reflect.DeepEqual(placed, map[string]int64{"alice": 100, "bob": 200, "carol": 300}) {
			t.Errorf("bets = %v", placed)
		}
		if len(cashedOut) != 2 || cashedOut["alice"] < 100 || cashedOut["carol"] < 300 {
			t.Fatalf("cash-outs = %v, want alice and carol", cashedOut)
		}
		var (
			lost      = map[string]int64{}
			settled   v1.RoundSettled
			settledAt uint64
		)
		for i, ev := range settlement {
			e, err := v1.Adapter().DecodeEvent(ev.Payload)
			if err != nil {
				t.Fatal(err)
			}
			switch e := e.(type) {
			case v1.BetLost:
				lost[string(e.Player)] = e.BetCredits
			case v1.RoundSettled:
				if i != len(settlement)-1 {
					t.Errorf("RoundSettled is event %d of %d, want the last", i, len(settlement))
				}
				settled, settledAt = e, ev.Sequence
			default:
				t.Errorf("settlement event %d is %T", i, e)
			}
		}
		// BetLost events are exactly the bets without a cash-out.
		wantLost := map[string]int64{}
		for p, b := range placed {
			if _, ok := cashedOut[p]; !ok {
				wantLost[p] = b
			}
		}
		if !reflect.DeepEqual(lost, wantLost) || !reflect.DeepEqual(lost, map[string]int64{"bob": 200}) {
			t.Errorf("BetLost = %v, want %v (bob only)", lost, wantLost)
		}
		payouts := cashedOut["alice"] + cashedOut["carol"]
		if settledAt == 0 || settled.TotalBetCredits != 600 || settled.TotalPayoutCredits != payouts || settled.BetCount != 3 {
			t.Fatalf("RoundSettled = %+v at event %d, want totals 600 / %d", settled, settledAt, payouts)
		}
		// The settlement posting equals TotalBetCredits - TotalPayoutCredits.
		net := settled.TotalBetCredits - settled.TotalPayoutCredits
		var (
			amount        int64
			eventSeq      int64
			debit, credit [16]byte
			kind          string
		)
		if err := pool.QueryRow(ctx, `SELECT p.amount, p.event_sequence, p.debit_account_id, p.credit_account_id, x.kind
			FROM orvex.ledger_postings p JOIN orvex.ledger_transactions x USING (transaction_id)
			WHERE x.round_id = $1 AND x.sequence = 8`, round).Scan(&amount, &eventSeq, &debit, &credit, &kind); err != nil {
			t.Fatalf("settlement posting: %v", err)
		}
		if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_postings p JOIN orvex.ledger_transactions x USING (transaction_id)
			WHERE x.round_id = $1 AND x.sequence = 8`, round); n != 1 {
			t.Errorf("%d settlement postings, want 1", n)
		}
		pot, house := ledger.RoundAccountID(round), ledger.HouseAccountID()
		wantDebit, wantCredit, wantAmount := pot, house, net
		if net < 0 {
			wantDebit, wantCredit, wantAmount = house, pot, -net
		}
		if kind != "crash_settlement" || eventSeq != int64(settledAt) || amount != wantAmount || debit != wantDebit || credit != wantCredit {
			t.Errorf("settlement posting = %s %d at event %d (%x -> %x), want crash_settlement %d at event %d (%x -> %x)",
				kind, amount, eventSeq, debit, credit, wantAmount, settledAt, wantDebit, wantCredit)
		}
		if got := accountByID(t, pool, house); got.balance() != net {
			t.Errorf("house balance %d, want TotalBetCredits - TotalPayoutCredits = %d", got.balance(), net)
		}
		if got := accountByID(t, pool, pot); got.balance() != 0 {
			t.Errorf("round account balance %d, want 0", got.balance())
		}
		requireBalanced(t, pool)
	})
	t.Run("blocked by a fail-closed cash-out", func(t *testing.T) {
		_, pool := newDB(t)
		x := newExecutor(t, pool, nil, failingRegistry(t, newFailingAdapter(6, 0)))
		round, _ := setup(t, x, pool)
		run(t, x, pool, round)
		if n := count(t, pool, `SELECT processed_sequence FROM orvex.round_execution_heads WHERE round_id = $1`, round); n != 5 {
			t.Errorf("processed %d, want 5: the settlement must not run past the failed cash-out", n)
		}
	})
}

//go:build integration

package execution_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// goldenOffsets are the golden rounds' crash deadline offsets D.
var goldenOffsets = map[string]int64{"worked-example": 9_410_000, "instant-crash": 0, "empty-round": 40_010_000}

func TestCreateRound(t *testing.T) {
	for _, name := range []string{"worked-example", "instant-crash", "empty-round", "in-progress"} {
		t.Run(name, func(t *testing.T) {
			_, pool := newDB(t)
			x := roundtest.NewExecutor(t, pool, nil, nil)
			header, lines := roundtest.ReadGolden(t, goldenPath(name))
			g, cfg := roundtest.GoldenConfig(t, header, lines[0])
			created, err := x.CreateRound(ctx, execution.NewRound{
				RoundID: g.RoundID, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, Config: cfg, CreatedAtUnixMicro: g.CreatedAtUnixMicro,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(created.Genesis, lines[0]) || created.Head != chain.LinkHash(lines[0]) || created.Duplicate {
				t.Errorf("Created = %+v; genesis equal %v", created, bytes.Equal(created.Genesis, lines[0]))
			}
			initial, err := v1.Rules{}.InitialState(cfg)
			if err != nil {
				t.Fatal(err)
			}
			wantOffset := initial.CrashDeadlineOffsetMicros
			if d, ok := goldenOffsets[name]; ok && d != wantOffset {
				t.Fatalf("D = %d, want %d", wantOffset, d)
			}
			requireCreatedRows(t, pool, g, created, cfg, wantOffset)
		})
	}
}

// requireCreatedRows checks every row CreateRound writes (E-16).
func requireCreatedRows(t *testing.T, pool *pgxpool.Pool, g chain.Genesis, created execution.Created, cfg v1.Config, offset int64) {
	t.Helper()
	round := g.RoundID
	var (
		game                  string
		rules, schema, stateV int64
		chainV                string
		createdAt             int64
		secret                []byte
		closeOffset           *int64
		admitted              int64
		admissionState        string
		bound                 *int64
		processed, token      int64
		owner                 *string
		record, hash, prev    []byte
		status, reason        *string
		recordToken           int64
		snapHash              []byte
		snapNull              bool
		snapBuild             string
		outSeq, outIdx        int64
		outTopic              string
		outPayload            []byte
	)
	if err := pool.QueryRow(ctx, `SELECT game, rules_version, schema_version, state_version, chain, created_at_unix_micro
		FROM orvex.rounds WHERE round_id = $1`, round).Scan(&game, &rules, &schema, &stateV, &chainV, &createdAt); err != nil {
		t.Fatal(err)
	}
	if game != "crash" || rules != 1 || schema != 1 || stateV != 1 || chainV != chain.Version || createdAt != g.CreatedAtUnixMicro {
		t.Errorf("rounds row = %s %d %d %d %s %d", game, rules, schema, stateV, chainV, createdAt)
	}
	if err := pool.QueryRow(ctx, `SELECT config_secret, close_offset_micros FROM orvex.round_secrets WHERE round_id = $1`, round).
		Scan(&secret, &closeOffset); err != nil {
		t.Fatal(err)
	}
	_, wantSecret, err := v1.Adapter().EncodeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secret, wantSecret) {
		t.Errorf("config_secret = %s, want %s", secret, wantSecret)
	}
	if closeOffset == nil || *closeOffset != offset {
		t.Errorf("close_offset_micros = %v, want D = %d", closeOffset, offset)
	}
	if err := pool.QueryRow(ctx, `SELECT admitted_sequence, admission_state, close_not_before_unix_micro
		FROM orvex.round_admission_heads WHERE round_id = $1`, round).Scan(&admitted, &admissionState, &bound); err != nil {
		t.Fatal(err)
	}
	if admitted != 0 || admissionState != "open" || bound != nil {
		t.Errorf("admission head = %d %s %v, want 0 open NULL", admitted, admissionState, bound)
	}
	if err := pool.QueryRow(ctx, `SELECT processed_sequence, fencing_token, owner_id FROM orvex.round_execution_heads WHERE round_id = $1`, round).
		Scan(&processed, &token, &owner); err != nil {
		t.Fatal(err)
	}
	if processed != 0 || token != 0 || owner != nil {
		t.Errorf("execution head = %d %d %v, want 0 0 NULL", processed, token, owner)
	}
	if err := pool.QueryRow(ctx, `SELECT record, hash, prev_hash, result_status, result_reason, fencing_token
		FROM orvex.chain_records WHERE round_id = $1 AND sequence = 0`, round).
		Scan(&record, &hash, &prev, &status, &reason, &recordToken); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(record, created.Genesis) || !bytes.Equal(hash, created.Head[:]) || !bytes.Equal(prev, make([]byte, 32)) ||
		status != nil || reason != nil || recordToken != 0 {
		t.Error("genesis chain record differs")
	}
	if err := pool.QueryRow(ctx, `SELECT state_hash, state IS NULL, engine_build FROM orvex.round_snapshots WHERE round_id = $1 AND sequence = 0`, round).
		Scan(&snapHash, &snapNull, &snapBuild); err != nil {
		t.Fatal(err)
	}
	initial, err := v1.Rules{}.InitialState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := mustStateHash(t, initial)
	if !bytes.Equal(snapHash, wantHash[:]) || created.StateHash != wantHash || !snapNull || snapBuild != "golden" {
		t.Errorf("snapshot 0 = %x null %v build %q, want %x", snapHash, snapNull, snapBuild, wantHash)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.round_snapshots WHERE round_id = $1`, round); n != 1 {
		t.Errorf("%d snapshots, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_accounts WHERE account_id = $1 AND kind = 'system'
		AND name = 'round_pool' AND round_id = $2 AND NOT floor_enforced`, ledger.RoundAccountID(round), round); n != 1 {
		t.Error("round_pool account missing")
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_accounts WHERE account_id = $1 AND kind = 'system'
		AND name = 'house' AND round_id IS NULL AND NOT floor_enforced`, ledger.HouseAccountID()); n != 1 {
		t.Error("house account missing")
	}
	if err := pool.QueryRow(ctx, `SELECT sequence, message_index, topic, payload FROM orvex.outbox_messages WHERE round_id = $1`, round).
		Scan(&outSeq, &outIdx, &outTopic, &outPayload); err != nil {
		t.Fatal(err)
	}
	if outSeq != 0 || outIdx != 0 || outTopic != execution.OutboxTopicRecord || !bytes.Equal(outPayload, created.Genesis) {
		t.Errorf("outbox = (%d, %d, %q)", outSeq, outIdx, outTopic)
	}
}

// tableCounts counts the rows of every table round creation writes.
func tableCounts(t *testing.T, pool *pgxpool.Pool) [9]int64 {
	t.Helper()
	var c [9]int64
	for i, table := range []string{"rounds", "round_secrets", "round_admission_heads", "round_execution_heads", "chain_records",
		"round_snapshots", "ledger_accounts", "outbox_messages", "admissions"} {
		c[i] = count(t, pool, `SELECT count(*) FROM orvex.`+table)
	}
	return c
}

func TestCreateRoundIdempotent(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := newRoundID()
	r := execution.NewRound{RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config: roundConfig(round, testSeed, 1_000_000_000, 100), CreatedAtUnixMicro: 1_760_000_000_000_000}
	first, err := x.CreateRound(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	counts := tableCounts(t, pool)
	again, err := x.CreateRound(ctx, r)
	if err != nil || !again.Duplicate || !bytes.Equal(again.Genesis, first.Genesis) || again.Head != first.Head {
		t.Fatalf("second CreateRound = %+v, %v; want Duplicate", again, err)
	}
	if got := tableCounts(t, pool); got != counts {
		t.Errorf("row counts changed by a duplicate: %v -> %v", counts, got)
	}
	// Another executor (no cache) resolves the duplicate the same way.
	if again, err := roundtest.NewExecutor(t, pool, nil, nil).CreateRound(ctx, r); err != nil || !again.Duplicate {
		t.Errorf("CreateRound from another executor = %+v, %v", again, err)
	}
	// A second round reuses the house account.
	other := newRoundID()
	createRound(t, x, other, testSeed, 1_000_000_000, 100)
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_accounts WHERE name = 'house'`); n != 1 {
		t.Errorf("%d house accounts, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_accounts WHERE name = 'round_pool'`); n != 2 {
		t.Errorf("%d round_pool accounts, want 2", n)
	}
}

func TestCreateRoundConflict(t *testing.T) {
	_, pool := newDB(t)
	x := roundtest.NewExecutor(t, pool, nil, nil)
	round := newRoundID()
	createRound(t, x, round, testSeed, 1_000_000_000, 100)
	counts := tableCounts(t, pool)

	var otherSeed [32]byte
	otherSeed[31] = 7
	_, err := x.CreateRound(ctx, execution.NewRound{RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config: roundConfig(round, otherSeed, 1_000_000_000, 100), CreatedAtUnixMicro: time.Now().UnixMicro()})
	if !errors.Is(err, execution.ErrRoundExists) {
		t.Errorf("another seed: err = %v, want ErrRoundExists", err)
	}

	fresh := newRoundID()
	_, err = x.CreateRound(ctx, execution.NewRound{RoundID: fresh, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config: roundConfig(newRoundID(), testSeed, 1_000_000_000, 100), CreatedAtUnixMicro: time.Now().UnixMicro()})
	if !errors.Is(err, execution.ErrInvalidRound) {
		t.Errorf("config of another round: err = %v, want ErrInvalidRound", err)
	}
	_, err = x.CreateRound(ctx, execution.NewRound{RoundID: fresh, Game: v1.GameID, RulesVersion: 2,
		Config: roundConfig(fresh, testSeed, 1_000_000_000, 100), CreatedAtUnixMicro: time.Now().UnixMicro()})
	if !errors.Is(err, execution.ErrUnknownRules) {
		t.Errorf("rules version 2: err = %v, want ErrUnknownRules", err)
	}
	_, err = x.CreateRound(ctx, execution.NewRound{RoundID: [16]byte{}, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config: roundConfig([16]byte{}, testSeed, 1_000_000_000, 100), CreatedAtUnixMicro: time.Now().UnixMicro()})
	if !errors.Is(err, execution.ErrInvalidRound) {
		t.Errorf("nil round id: err = %v, want ErrInvalidRound", err)
	}
	if got := tableCounts(t, pool); got != counts {
		t.Errorf("row counts changed: %v -> %v", counts, got)
	}
}

package pgtest

import (
	"bytes"
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// Loaded describes a history inserted by LoadHistory.
type Loaded struct {
	RoundID [16]byte
	// Head is the link hash of the last record.
	Head    chain.Hash
	Records int
	// CommandIDs holds the command ID of record k at index k-1.
	CommandIDs [][16]byte
	// Hashes holds the link hash of record k at index k.
	Hashes []chain.Hash
}

type checkpoint struct {
	sequence  uint64
	stateHash []byte
}

// LoadHistory inserts an LBT-9 history file into a migrated database
// exactly as round creation, admission and execution would (spec section
// 7.3), in one READ COMMITTED transaction. checkpoints selects which of the
// header's checkpoints become snapshot rows: nil selects all, an empty
// slice none; a sequence missing from the header fails the test. Any error,
// including one at commit, fails the test.
func LoadHistory(t testing.TB, db *pgxpool.Pool, history []byte, checkpoints []uint64) Loaded {
	t.Helper()
	lines := bytes.Split(bytes.TrimSuffix(history, []byte("\n")), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("pgtest: history has %d lines, want a header and a genesis record", len(lines))
	}
	secret, cps := parseHeader(t, lines[0])
	selected := selectCheckpoints(t, cps, checkpoints)

	records := lines[1:]
	g, ok := decode(t, records[0]).(chain.Genesis)
	if !ok {
		t.Fatal("pgtest: record 0 is not a genesis record")
	}
	n := int64(len(records) - 1)
	out := Loaded{RoundID: g.RoundID, Records: len(records)}

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, postgres.ExecutionTxOptions())
	if err != nil {
		t.Fatalf("pgtest: begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("pgtest: load %s: %v", what, err)
		}
	}

	exec("round", `INSERT INTO orvex.rounds
		(round_id, game, rules_version, schema_version, state_version, chain, created_at_unix_micro)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		g.RoundID, string(g.Game), int64(g.RulesVersion), int64(g.SchemaVersion), int64(g.StateVersion),
		chain.Version, g.CreatedAtUnixMicro)
	exec("secret", `INSERT INTO orvex.round_secrets (round_id, config_secret) VALUES ($1, $2)`, g.RoundID, secret)
	exec("admission head", `INSERT INTO orvex.round_admission_heads (round_id, admitted_sequence) VALUES ($1, $2)`,
		g.RoundID, n)
	exec("execution head", `INSERT INTO orvex.round_execution_heads (round_id, processed_sequence, fencing_token)
		VALUES ($1, $2, 0)`, g.RoundID, n)

	cmds := make([]chain.Command, 0, n)
	for i, line := range records[1:] {
		c, ok := decode(t, line).(chain.Command)
		if !ok {
			t.Fatalf("pgtest: record %d is not a command record", i+1)
		}
		cmds = append(cmds, c)
		out.CommandIDs = append(out.CommandIDs, c.CommandID)
		exec("admission", `INSERT INTO orvex.admissions
			(round_id, sequence, command_id, effective_time_unix_micro, command)
			VALUES ($1, $2, $3, $4, $5)`,
			c.RoundID, int64(c.Sequence), c.CommandID, c.EffectiveTimeUnixMicro, c.Payload)
	}

	for i, line := range records {
		h := chain.LinkHash(line)
		out.Hashes = append(out.Hashes, h)
		var prev chain.Hash
		var status, reason *string
		if i > 0 {
			c := cmds[i-1]
			prev = c.PrevHash
			status = new("accepted")
			if c.Result.Rejected {
				status, reason = new("rejected"), new(c.Result.Reason)
			}
		}
		exec("chain record", `INSERT INTO orvex.chain_records
			(round_id, sequence, prev_hash, hash, record, result_status, result_reason, fencing_token)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 0)`,
			g.RoundID, int64(i), prev[:], h[:], line, status, reason)
	}
	out.Head = out.Hashes[len(out.Hashes)-1]

	for _, c := range cmds {
		for _, e := range c.Events {
			o, err := canonjson.Unmarshal(e.Payload)
			if err != nil {
				t.Fatalf("pgtest: event %d: %v", e.Sequence, err)
			}
			typ, err := canonjson.Text(o["type"])
			if err != nil {
				t.Fatalf("pgtest: event %d type: %v", e.Sequence, err)
			}
			exec("domain event", `INSERT INTO orvex.domain_events (round_id, event_sequence, sequence, event_type)
				VALUES ($1, $2, $3, $4)`, c.RoundID, int64(e.Sequence), int64(c.Sequence), typ)
		}
	}

	for _, cp := range selected {
		exec("snapshot", `INSERT INTO orvex.round_snapshots (round_id, sequence, state_hash, state, engine_build)
			VALUES ($1, $2, $3, NULL, 'golden')`, g.RoundID, int64(cp.sequence), cp.stateHash)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("pgtest: load commit: %v", err)
	}
	return out
}

func decode(t testing.TB, line []byte) any {
	t.Helper()
	rec, err := chain.Decode(line)
	if err != nil {
		t.Fatalf("pgtest: decode record: %v", err)
	}
	return rec
}

// parseHeader returns the canonical secret config bytes and the checkpoints
// of a history header line.
func parseHeader(t testing.TB, line []byte) ([]byte, []checkpoint) {
	t.Helper()
	o, err := canonjson.Unmarshal(line)
	if err != nil {
		t.Fatalf("pgtest: history header: %v", err)
	}
	so, err := canonjson.Obj(o["configSecret"])
	if err != nil {
		t.Fatalf("pgtest: history header configSecret: %v", err)
	}
	secret, err := canonjson.Marshal(so)
	if err != nil {
		t.Fatalf("pgtest: history header configSecret: %v", err)
	}
	list, err := canonjson.List(o["checkpoints"])
	if err != nil {
		t.Fatalf("pgtest: history header checkpoints: %v", err)
	}
	cps := make([]checkpoint, 0, len(list))
	for i, v := range list {
		co, err := canonjson.Obj(v)
		if err != nil {
			t.Fatalf("pgtest: checkpoint %d: %v", i, err)
		}
		seq, err := canonjson.ParseUint(co["sequence"])
		if err != nil {
			t.Fatalf("pgtest: checkpoint %d sequence: %v", i, err)
		}
		h, err := canonjson.ParseHex(co["stateHash"], 32)
		if err != nil {
			t.Fatalf("pgtest: checkpoint %d stateHash: %v", i, err)
		}
		cps = append(cps, checkpoint{sequence: seq, stateHash: h})
	}
	return secret, cps
}

func selectCheckpoints(t testing.TB, cps []checkpoint, want []uint64) []checkpoint {
	t.Helper()
	if want == nil {
		return cps
	}
	out := make([]checkpoint, 0, len(want))
	for _, seq := range want {
		found := false
		for _, cp := range cps {
			if cp.sequence == seq {
				out = append(out, cp)
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("pgtest: checkpoint %d is not in the history header", seq)
		}
	}
	return out
}

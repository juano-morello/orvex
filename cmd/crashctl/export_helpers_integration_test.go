//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/url"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	crashv1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// Integration helpers for crashctl audit export (LBT-19 spec section 10
// step 5). seedGolden, fundPlayer, seedAdmission and holdFor are adapted
// from internal/runtime/execution/helpers_integration_test.go, which is in
// package execution_test and cannot be imported (spec section 11 open item
// 7).

// goldenRound is a golden history whose round was created by LBT-12's
// executor and whose admissions are seeded, none executed.
type goldenRound struct {
	roundID [16]byte
	// lines are the golden record lines 0..N, without the header.
	lines [][]byte
	exec  *execution.Executor
}

// hash returns the link hash of golden record seq.
func (g goldenRound) hash(seq int) chain.Hash { return chain.LinkHash(g.lines[seq]) }

// lease returns the round-creation lease (fencing token 0).
func (g goldenRound) lease() execution.Lease { return execution.Lease{RoundID: g.roundID} }

// uuid returns the round ID as D-11 text.
func (g goldenRound) uuid() string { return string(canonjson.UUID(g.roundID)) }

var exportTestIDs atomic.Uint64

// exportTestID returns a fresh non-nil UUID.
func exportTestID() [16]byte {
	var id [16]byte
	id[0], id[6] = 0x0e, 0x70
	binary.BigEndian.PutUint64(id[8:], exportTestIDs.Add(1))
	return id
}

func execOrFatal(t *testing.T, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// inExecutionTx runs fn in one transaction and commits.
func inExecutionTx(t *testing.T, db *pgxpool.Pool, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, postgres.ExecutionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fn(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// fundPlayer creates the player's account if absent and grants it credits
// from the global system account faucet, which it creates if absent.
func fundPlayer(t *testing.T, db *pgxpool.Pool, player string, credits int64) {
	t.Helper()
	inExecutionTx(t, db, func(tx pgx.Tx) {
		execOrFatal(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
			SELECT gen_random_uuid(), 'system', 'faucet', false
			WHERE NOT EXISTS (SELECT 1 FROM orvex.ledger_accounts WHERE kind = 'system' AND name = 'faucet' AND round_id IS NULL)`)
		execOrFatal(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced)
			VALUES (gen_random_uuid(), 'player', $1, true) ON CONFLICT (player_id) DO NOTHING`, player)
		txID := exportTestID()
		execOrFatal(t, tx, `INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`, txID)
		execOrFatal(t, tx, `INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount)
			SELECT $1, 0, f.account_id, p.account_id, $3
			FROM orvex.ledger_accounts f, orvex.ledger_accounts p
			WHERE f.kind = 'system' AND f.name = 'faucet' AND f.round_id IS NULL AND p.player_id = $2`, txID, player, credits)
	})
}

// holdSpec is a credit hold placed with a seeded admission.
type holdSpec struct {
	player  string
	credits int64
}

// holdFor applies LBT-11 S-10 to cmd: the hold a player PlaceBet carries.
func holdFor(cmd any) *holdSpec {
	cl, err := admission.CrashV1Policy().Classify(cmd)
	if err != nil || cl.Hold.PlayerID == "" {
		return nil
	}
	return &holdSpec{player: cl.Hold.PlayerID, credits: cl.Hold.Credits}
}

// seedAdmission inserts the next admission directly, as LBT-11 does: under
// the admission head lock, with the hold (if any) in the same transaction.
// It returns the sequence.
func seedAdmission(t *testing.T, db *pgxpool.Pool, roundID, commandID [16]byte, command []byte, effectiveTime int64, hold *holdSpec) uint64 {
	t.Helper()
	var seq int64
	inExecutionTx(t, db, func(tx pgx.Tx) {
		if err := tx.QueryRow(context.Background(), `SELECT admitted_sequence FROM orvex.round_admission_heads WHERE round_id = $1 FOR UPDATE`,
			roundID).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		seq++
		execOrFatal(t, tx, `INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
			VALUES ($1, $2, $3, $4, $5)`, roundID, seq, commandID, effectiveTime, command)
		if hold != nil {
			execOrFatal(t, tx, `INSERT INTO orvex.ledger_holds (round_id, sequence, account_id, amount)
				SELECT $1, $2, account_id, $4 FROM orvex.ledger_accounts WHERE kind = 'player' AND player_id = $3`,
				roundID, seq, hold.player, hold.credits)
		}
		execOrFatal(t, tx, `UPDATE orvex.round_admission_heads SET admitted_sequence = $2 WHERE round_id = $1`, roundID, seq)
	})
	return uint64(seq) //nolint:gosec // G115: seq is admitted_sequence (CHECK >= 0) + 1
}

// seedGolden creates the golden round in file with LBT-12's executor
// (EngineBuild "golden"), funds each player with the sum of their S-10
// holds, and seeds every golden admission in order. It executes nothing.
func seedGolden(t *testing.T, pool *pgxpool.Pool, file string) goldenRound {
	t.Helper()
	all := historyLines(readGoldenFile(t, file))
	header, err := canonjson.Unmarshal(all[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := all[1:]
	rec, err := chain.Decode(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	g := rec.(chain.Genesis)
	so, err := canonjson.Obj(header["configSecret"])
	if err != nil {
		t.Fatal(err)
	}
	secret, err := canonjson.Marshal(so)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := crashv1.Adapter().DecodeConfig(g.RoundID, g.Config, secret)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := newRegistry()
	if err != nil {
		t.Fatal(err)
	}
	xcfg := execution.DefaultConfig()
	xcfg.EngineBuild = "golden"
	x, err := execution.New(pool, reg, xcfg, execution.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	created, err := x.CreateRound(context.Background(), execution.NewRound{
		RoundID: g.RoundID, Game: g.Game, RulesVersion: g.RulesVersion, Config: cfg, CreatedAtUnixMicro: g.CreatedAtUnixMicro,
	})
	if err != nil {
		t.Fatalf("CreateRound(%s): %v", file, err)
	}
	if !bytes.Equal(created.Genesis, lines[0]) {
		t.Fatalf("%s genesis differs:\n got %s\nwant %s", file, created.Genesis, lines[0])
	}

	var cmds []chain.Command
	holds := map[uint64]*holdSpec{}
	funds := map[string]int64{}
	for _, line := range lines[1:] {
		rec, err := chain.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		c := rec.(chain.Command)
		cmds = append(cmds, c)
		cmd, err := crashv1.Adapter().DecodeCommand(c.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if h := holdFor(cmd); h != nil {
			holds[c.Sequence] = h
			funds[h.player] += h.credits
		}
	}
	players := make([]string, 0, len(funds))
	for p := range funds {
		players = append(players, p)
	}
	sort.Strings(players)
	for _, p := range players {
		fundPlayer(t, pool, p, funds[p])
	}
	for _, c := range cmds {
		if seq := seedAdmission(t, pool, g.RoundID, c.CommandID, c.Payload, c.EffectiveTimeUnixMicro, holds[c.Sequence]); seq != c.Sequence {
			t.Fatalf("seeded sequence %d, want %d", seq, c.Sequence)
		}
	}
	return goldenRound{roundID: g.RoundID, lines: lines, exec: x}
}

// replicaExec runs sql in one committed transaction with
// session_replication_role = replica, so the append-only triggers and
// foreign keys do not fire: a superuser corrupting stored data.
func replicaExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	execOrFatal(t, tx, `SET LOCAL session_replication_role = replica`)
	execOrFatal(t, tx, sql, args...)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// exportRolePassword is every restricted test role's password.
const exportRolePassword = "sentinel-pw"

var exportRoles atomic.Int64

// exportGrants is the auditor grant set (spec X-11 and section 7, with a
// column-level round_secrets grant: migration 0009 added the secret
// close_offset_micros column, which only orvex.admit_command reads). %[1]s is
// the role, %[2]s the database.
var exportGrants = []string{
	`GRANT CONNECT ON DATABASE %[2]s TO %[1]s`,
	`GRANT USAGE ON SCHEMA orvex TO %[1]s`,
	`GRANT SELECT ON orvex.rounds, orvex.chain_records TO %[1]s`,
	`GRANT SELECT (round_id, config_secret) ON orvex.round_secrets TO %[1]s`,
	`GRANT SELECT (round_id, sequence, state_hash) ON orvex.round_snapshots TO %[1]s`,
}

// restrictedRole creates a LOGIN role orvex_export_<n> with password
// exportRolePassword, runs grants in superDSN's database, and returns
// superDSN with that role's credentials. At cleanup it runs DROP OWNED BY in
// that database, then DROP ROLE; call it after creating the database so the
// cleanup runs before the database is dropped.
func restrictedRole(t *testing.T, superDSN string, grants []string) (roleDSN string) {
	t.Helper()
	ctx := context.Background()
	role := fmt.Sprintf("orvex_export_%d", exportRoles.Add(1))
	quotedRole := pgx.Identifier{role}.Sanitize()

	conn, err := pgx.Connect(ctx, superDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var database string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	execOrFatal(t, conn, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, quotedRole, exportRolePassword))
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, superDSN)
		if err != nil {
			t.Errorf("drop role %s: %v", role, err)
			return
		}
		defer func() { _ = c.Close(ctx) }()
		for _, sql := range []string{`DROP OWNED BY ` + quotedRole, `DROP ROLE ` + quotedRole} {
			if _, err := c.Exec(ctx, sql); err != nil {
				t.Errorf("%s: %v", sql, err)
			}
		}
	})
	for _, g := range grants {
		execOrFatal(t, conn, fmt.Sprintf(g, quotedRole, pgx.Identifier{database}.Sanitize()))
	}

	u, err := url.Parse(superDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, exportRolePassword)
	return u.String()
}

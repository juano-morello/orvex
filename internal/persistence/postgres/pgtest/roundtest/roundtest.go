//go:build integration

// Package roundtest seeds crash/v1 rounds into a migrated Orvex database for
// PostgreSQL integration tests (LBT-24). It builds the executor that creates
// golden rounds, funds players, seeds admissions directly with their LBT-11
// S-10 holds, as LBT-11 does, and creates, funds and seeds the LBT-9 golden
// histories through LBT-12's executor. It executes nothing itself. Callers
// drive the executor.
//
// It is test support. Every file carries //go:build integration, so no
// production build can import it. It imports internal/runtime/execution and
// internal/runtime/admission: only external test packages (package x_test)
// and cmd tests may import it, never the in-package tests of a package it
// imports.
package roundtest

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/game"
	crashv1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// engineBuild is the engine build the LBT-9 golden histories record.
const engineBuild = "golden"

// Registry returns a registry holding crash/v1 only.
func Registry(t testing.TB) *game.Registry {
	t.Helper()
	reg, err := game.NewRegistry(crashv1.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// NewExecutor returns an executor over db with EngineBuild "golden" unless
// mutate changes it; reg nil means crash/v1 only.
func NewExecutor(t testing.TB, db execution.DB, mutate func(*execution.Config), reg *game.Registry) *execution.Executor {
	t.Helper()
	cfg := execution.DefaultConfig()
	cfg.EngineBuild = engineBuild
	if mutate != nil {
		mutate(&cfg)
	}
	if reg == nil {
		reg = Registry(t)
	}
	x, err := execution.New(db, reg, cfg, execution.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	return x
}

var grantIDs atomic.Uint64

// grantID returns a fresh non-nil grant transaction UUID. Its first byte,
// 0x5e, differs from every test ID generator of its callers (0x0c command
// IDs, 0x0e round and ledger transaction IDs), so a grant never collides
// with a ledger transaction a caller inserts.
func grantID() [16]byte {
	var id [16]byte
	id[0], id[6] = 0x5e, 0x70
	binary.BigEndian.PutUint64(id[8:], grantIDs.Add(1))
	return id
}

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func mustExec(t testing.TB, db execer, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// inTx runs fn in one transaction and commits.
func inTx(t testing.TB, db *pgxpool.Pool, fn func(tx pgx.Tx)) {
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

// FundPlayer creates the player's account if absent and grants it credits
// from the global system account faucet, which it creates if absent.
func FundPlayer(t testing.TB, db *pgxpool.Pool, player string, credits int64) {
	t.Helper()
	inTx(t, db, func(tx pgx.Tx) {
		mustExec(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
			SELECT gen_random_uuid(), 'system', 'faucet', false
			WHERE NOT EXISTS (SELECT 1 FROM orvex.ledger_accounts WHERE kind = 'system' AND name = 'faucet' AND round_id IS NULL)`)
		mustExec(t, tx, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced)
			VALUES (gen_random_uuid(), 'player', $1, true) ON CONFLICT (player_id) DO NOTHING`, player)
		txID := grantID()
		mustExec(t, tx, `INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, 'grant')`, txID)
		mustExec(t, tx, `INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount)
			SELECT $1, 0, f.account_id, p.account_id, $3
			FROM orvex.ledger_accounts f, orvex.ledger_accounts p
			WHERE f.kind = 'system' AND f.name = 'faucet' AND f.round_id IS NULL AND p.player_id = $2`, txID, player, credits)
	})
}

// Hold is a credit hold placed with a seeded admission.
type Hold struct {
	Player  string
	Credits int64
}

// HoldFor applies LBT-11 S-10 to cmd: the hold a player PlaceBet carries.
func HoldFor(cmd any) *Hold {
	cl, err := admission.CrashV1Policy().Classify(cmd)
	if err != nil || cl.Hold.PlayerID == "" {
		return nil
	}
	return &Hold{Player: cl.Hold.PlayerID, Credits: cl.Hold.Credits}
}

// SeedAdmission inserts the next admission directly, as LBT-11 does: under
// the admission head lock, with the hold (if any) in the same transaction.
// It returns the sequence.
func SeedAdmission(t testing.TB, db *pgxpool.Pool, roundID, commandID [16]byte, command []byte, effectiveTime int64, hold *Hold) uint64 {
	t.Helper()
	var seq int64
	inTx(t, db, func(tx pgx.Tx) {
		if err := tx.QueryRow(context.Background(), `SELECT admitted_sequence FROM orvex.round_admission_heads WHERE round_id = $1 FOR UPDATE`,
			roundID).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		seq++
		mustExec(t, tx, `INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
			VALUES ($1, $2, $3, $4, $5)`, roundID, seq, commandID, effectiveTime, command)
		if hold != nil {
			mustExec(t, tx, `INSERT INTO orvex.ledger_holds (round_id, sequence, account_id, amount)
				SELECT $1, $2, account_id, $4 FROM orvex.ledger_accounts WHERE kind = 'player' AND player_id = $3`,
				roundID, seq, hold.Player, hold.Credits)
		}
		mustExec(t, tx, `UPDATE orvex.round_admission_heads SET admitted_sequence = $2 WHERE round_id = $1`, roundID, seq)
	})
	return uint64(seq)
}

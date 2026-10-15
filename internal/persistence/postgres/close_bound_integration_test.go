//go:build integration

package postgres_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// Tests of migration 0009's schema objects (LBT-12 engineer addendum): the
// secret close offset, the closure bound recorded at start and the
// SECURITY DEFINER admit_command.

const admitCommandSignature = "orvex.admit_command(uuid,text,bigint,uuid,bytea,text,text,bigint,integer,integer)"

func TestCloseBoundColumns(t *testing.T) {
	t.Parallel()
	pool := migrated(t)

	// round_secrets.close_offset_micros: >= 0 and immutable.
	round := newID()
	insertRound := func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO orvex.rounds
			(round_id, game, rules_version, schema_version, state_version, chain, created_at_unix_micro)
			VALUES ($1, 'crash', 1, 1, 1, 'orvex/audit/v1', $2)`, round, baseTimeUnixMicro)
	}
	insertSecret := func(offset int64) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			if err := insertRound(tx); err != nil {
				return err
			}
			return exec(tx, `INSERT INTO orvex.round_secrets (round_id, config_secret, close_offset_micros) VALUES ($1, $2, $3)`,
				round, []byte(`{}`), offset)
		}
	}
	requireStmtCode(t, pool, insertSecret(-1), codeCheckViolation, "round_secrets_close_offset_check")
	mustCommit(t, pool, insertSecret(0))
	requireCode(t, exec(pool, `UPDATE orvex.round_secrets SET close_offset_micros = 1 WHERE round_id = $1`, round),
		postgres.CodeAppendOnly, "")

	// round_admission_heads.close_not_before_unix_micro: only once started,
	// set once.
	l := loadGolden(t, pool, "empty-round")
	setHead := func(set string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE orvex.round_admission_heads SET `+set+` WHERE round_id = $1`, l.RoundID)
		}
	}
	requireStmtCode(t, pool, setHead(`close_not_before_unix_micro = 1`), codeCheckViolation, "round_admission_heads_close_bound_check")
	mustCommit(t, pool, setHead(`admission_state = 'started', started_sequence = 1, close_not_before_unix_micro = 5`))
	requireStmtCode(t, pool, setHead(`close_not_before_unix_micro = 6`), postgres.CodeHeadRegression, "")
	requireStmtCode(t, pool, setHead(`close_not_before_unix_micro = NULL`), postgres.CodeHeadRegression, "")
	if n := count(t, pool, `SELECT close_not_before_unix_micro FROM orvex.round_admission_heads WHERE round_id = $1`, l.RoundID); n != 5 {
		t.Errorf("close_not_before_unix_micro = %d, want 5", n)
	}
}

// rawAdmitOutcome calls admit_command for a system lifecycle command of
// class in a transaction that is rolled back, and returns the outcome.
func rawAdmitOutcome(t *testing.T, pool *pgxpool.Pool, roundID [16]byte, class, command string) string {
	t.Helper()
	tx, err := pool.BeginTx(ctx, postgres.AdmissionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var outcome string
	if err := tx.QueryRow(ctx, `SELECT outcome FROM orvex.admit_command($1, 'crash', 1, $2, $3, $4, NULL, NULL, 3000, 1000)`,
		roundID, newID(), []byte(command), class).Scan(&outcome); err != nil {
		t.Fatalf("admit_command(%s): %v", class, err)
	}
	return outcome
}

func TestAdmitCommandCloseBoundFailsClosed(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	// LoadHistory writes no close offset, as for a round created before 0009.
	l := loadGolden(t, pool, "empty-round")
	type headState struct {
		admitted int64
		state    string
		bound    *int64
	}
	readHead := func() headState {
		t.Helper()
		var h headState
		if err := pool.QueryRow(ctx, `SELECT admitted_sequence, admission_state, close_not_before_unix_micro
			FROM orvex.round_admission_heads WHERE round_id = $1`, l.RoundID).Scan(&h.admitted, &h.state, &h.bound); err != nil {
			t.Fatal(err)
		}
		return h
	}

	if got := rawAdmitOutcome(t, pool, l.RoundID, "start", `{"authority":"system","type":"StartRound"}`); got != "corrupt" {
		t.Errorf("start without a close offset: outcome %q, want corrupt", got)
	}
	if h := readHead(); h.admitted != 3 || h.state != "open" || h.bound != nil {
		t.Errorf("head = %+v, want admitted 3, open, no bound", h)
	}

	mustCommit(t, pool, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE orvex.round_admission_heads SET admission_state = 'started', started_sequence = 1 WHERE round_id = $1`, l.RoundID)
	})
	if got := rawAdmitOutcome(t, pool, l.RoundID, "close", `{"authority":"system","type":"CloseRound"}`); got != "corrupt" {
		t.Errorf("close without a closure bound: outcome %q, want corrupt", got)
	}
	if h := readHead(); h.admitted != 3 || h.state != "started" || h.bound != nil {
		t.Errorf("head = %+v, want admitted 3, started, no bound", h)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.admissions WHERE round_id = $1`, l.RoundID); n != 3 {
		t.Errorf("%d admissions, want 3", n)
	}
}

func TestAdmitCommandSecurityDefiner(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	var (
		secdef bool
		config []string
	)
	if err := pool.QueryRow(ctx, `SELECT prosecdef, proconfig FROM pg_proc WHERE oid = $1::regprocedure`, admitCommandSignature).
		Scan(&secdef, &config); err != nil {
		t.Fatal(err)
	}
	if !secdef {
		t.Error("orvex.admit_command is not SECURITY DEFINER")
	}
	if len(config) != 1 || config[0] != "search_path=pg_catalog" {
		t.Errorf("proconfig = %v, want [search_path=pg_catalog]", config)
	}
	var old *string
	if err := pool.QueryRow(ctx, `SELECT to_regprocedure('orvex.admit_command(uuid,text,bigint,uuid,bytea,text,text,bigint,bigint,integer,integer)')::text`).
		Scan(&old); err != nil {
		t.Fatal(err)
	}
	if old != nil {
		t.Errorf("the 11-argument admit_command still exists: %s", *old)
	}
	var public bool
	if err := pool.QueryRow(ctx, `SELECT has_function_privilege('public', $1, 'EXECUTE')`, admitCommandSignature).Scan(&public); err != nil {
		t.Fatal(err)
	}
	if public {
		t.Error("PUBLIC may execute orvex.admit_command")
	}
}

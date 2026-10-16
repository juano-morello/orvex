//go:build integration

package postgres_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
)

// Schema tests (LBT-10 spec section 11, step 4). Each negative case changes
// exactly one input so that the row satisfies every other constraint, and
// asserts the SQLSTATE and constraint name, never message text.

const (
	codeFKViolation     = "23503"
	codeUniqueViolation = "23505"
	codeCheckViolation  = "23514"
	codeFeatureNotSupp  = "0A000"
	placeBet            = `{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"}`
	baseTimeUnixMicro   = int64(1760000100000000)
)

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// requireCode requires err to wrap a PgError with sqlstate and, unless
// constraint is empty, that constraint name.
func requireCode(t *testing.T, err error, sqlstate, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("err = %v, want SQLSTATE %s %s", err, sqlstate, constraint)
		return
	}
	if pgErr.Code != sqlstate || (constraint != "" && pgErr.ConstraintName != constraint) {
		t.Errorf("err = SQLSTATE %s constraint %q (%s), want SQLSTATE %s constraint %q",
			pgErr.Code, pgErr.ConstraintName, pgErr.Message, sqlstate, constraint)
	}
}

func exec(db execer, sql string, args ...any) error {
	_, err := db.Exec(ctx, sql, args...)
	return err
}

func mustExec(t *testing.T, db execer, sql string, args ...any) {
	t.Helper()
	if err := exec(db, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// inTx runs fn in an execution transaction and commits. It returns fn's
// error (after a rollback) or else the commit's.
func inTx(t *testing.T, db *pgxpool.Pool, fn func(tx pgx.Tx) error) (stmtErr, commitErr error) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.ExecutionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err, nil
	}
	return nil, tx.Commit(ctx)
}

func mustCommit(t *testing.T, db *pgxpool.Pool, fn func(tx pgx.Tx) error) {
	t.Helper()
	stmtErr, commitErr := inTx(t, db, fn)
	if stmtErr != nil || commitErr != nil {
		t.Fatalf("transaction failed: statement %v, commit %v", stmtErr, commitErr)
	}
}

// requireCommitCode requires every statement of fn to succeed and the
// commit to fail with sqlstate and constraint.
func requireCommitCode(t *testing.T, db *pgxpool.Pool, fn func(tx pgx.Tx) error, sqlstate, constraint string) {
	t.Helper()
	stmtErr, commitErr := inTx(t, db, fn)
	if stmtErr != nil {
		t.Errorf("statement failed before commit: %v", stmtErr)
		return
	}
	requireCode(t, commitErr, sqlstate, constraint)
}

// requireStmtCode runs fn in its own transaction and requires its
// statement error to carry sqlstate and constraint.
func requireStmtCode(t *testing.T, db *pgxpool.Pool, fn func(tx pgx.Tx) error, sqlstate, constraint string) {
	t.Helper()
	stmtErr, commitErr := inTx(t, db, fn)
	if stmtErr == nil {
		t.Errorf("statements succeeded (commit: %v), want SQLSTATE %s %s", commitErr, sqlstate, constraint)
		return
	}
	requireCode(t, stmtErr, sqlstate, constraint)
}

// requireTruncateRejected truncates orvex.<table> with CASCADE, which
// reaches the BEFORE TRUNCATE triggers even when other tables reference it
// (a plain TRUNCATE of a referenced table fails with 0A000 first), and
// requires OX001 and an unchanged row count.
func requireTruncateRejected(t *testing.T, db *pgxpool.Pool, table string) {
	t.Helper()
	before := count(t, db, "SELECT count(*) FROM orvex."+table)
	requireCode(t, exec(db, "TRUNCATE orvex."+table+" CASCADE"), postgres.CodeAppendOnly, "")
	if after := count(t, db, "SELECT count(*) FROM orvex."+table); after != before {
		t.Errorf("orvex.%s holds %d rows after TRUNCATE, want %d", table, after, before)
	}
}

func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return pgtest.Pool(t, pgtest.MigratedDatabase(t))
}

func loadGolden(t *testing.T, db *pgxpool.Pool, name string) pgtest.Loaded {
	t.Helper()
	return pgtest.LoadHistory(t, db, readGolden(t, name), nil)
}

var idCounter atomic.Uint64

// newID returns a fresh non-nil UUID.
func newID() [16]byte {
	var id [16]byte
	id[0], id[6] = 0x0b, 0x70
	binary.BigEndian.PutUint64(id[8:], idCounter.Add(1))
	return id
}

func uuidText(id [16]byte) string { return string(canonjson.UUID(id)) }

// commandIDFor derives the command ID test helpers admit as command k of a
// round. It keeps the round ID's last byte, so IDs differ across rounds.
func commandIDFor(roundID [16]byte, k uint64) [16]byte {
	id := roundID
	id[8] = 0xee
	id[12], id[13] = byte(k>>8), byte(k) //nolint:gosec // G115: deliberate truncation to the low 16 bits of a test sequence
	return id
}

// rec holds the fields of a command record built from the spec section
// 5.3 byte templates.
type rec struct {
	roundID     [16]byte
	sequence    uint64
	prevHash    chain.Hash
	command     string
	commandID   [16]byte
	effective   int64
	engineBuild string
	events      string // the events array's contents
	status      string
	reason      string
}

func (r rec) result() string {
	if r.status == "accepted" {
		return `{"status":"accepted"}`
	}
	return `{"reason":"` + r.reason + `","status":"` + r.status + `"}`
}

func (r rec) bytes() []byte {
	return []byte(`{"command":` + r.command +
		`,"commandId":"` + uuidText(r.commandID) +
		`","effectiveTimeUnixMicro":"` + strconv.FormatInt(r.effective, 10) +
		`","engineBuild":"` + r.engineBuild +
		`","events":[` + r.events +
		`],"kind":"command","prevHash":"` + r.prevHash.String() +
		`","result":` + r.result() +
		`,"roundId":"` + uuidText(r.roundID) +
		`","sequence":"` + strconv.FormatUint(r.sequence, 10) + `"}`)
}

// rejectedRecord returns the well-formed rejected record k that
// appendRejected writes.
func rejectedRecord(roundID [16]byte, k uint64, prev chain.Hash) rec {
	return rec{
		roundID:     roundID,
		sequence:    k,
		prevHash:    prev,
		command:     placeBet,
		commandID:   commandIDFor(roundID, k),
		effective:   baseTimeUnixMicro + int64(k), //nolint:gosec // G115: a test round's sequence, far below math.MaxInt64
		engineBuild: "test",
		status:      "rejected",
		reason:      "round_settled",
	}
}

type admission struct {
	roundID   [16]byte
	sequence  uint64
	commandID [16]byte
	effective int64
	command   string
}

func admissionOf(r rec) admission {
	return admission{roundID: r.roundID, sequence: r.sequence, commandID: r.commandID, effective: r.effective, command: r.command}
}

func insertAdmission(db execer, a admission) error {
	return exec(db, `INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
		VALUES ($1, $2, $3, $4, $5)`, a.roundID, int64(a.sequence), a.commandID, a.effective, []byte(a.command)) //nolint:gosec // G115: a test round's sequence, far below math.MaxInt64
}

type recordRow struct {
	roundID  [16]byte
	sequence int64
	prevHash chain.Hash
	record   []byte
	hash     *chain.Hash // nil: chain.LinkHash(record)
	status   *string
	reason   *string
	token    int64
}

// rowOf returns the row whose columns agree with r's bytes.
func rowOf(r rec, token int64) recordRow {
	row := recordRow{roundID: r.roundID, sequence: int64(r.sequence), prevHash: r.prevHash, record: r.bytes(), status: new(r.status), token: token} //nolint:gosec // G115: a test round's sequence, far below math.MaxInt64
	if r.status != "accepted" {
		row.reason = new(r.reason)
	}
	return row
}

func insertRecord(db execer, r recordRow) error {
	h := chain.LinkHash(r.record)
	if r.hash != nil {
		h = *r.hash
	}
	return exec(db, `INSERT INTO orvex.chain_records
		(round_id, sequence, prev_hash, hash, record, result_status, result_reason, fencing_token)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.roundID, r.sequence, r.prevHash[:], h[:], r.record, r.status, r.reason, r.token)
}

func advanceHeads(db execer, roundID [16]byte, k uint64) error {
	if err := exec(db, `UPDATE orvex.round_admission_heads SET admitted_sequence = $2 WHERE round_id = $1`, roundID, int64(k)); err != nil { //nolint:gosec // G115: a test round's sequence, far below math.MaxInt64
		return err
	}
	return exec(db, `UPDATE orvex.round_execution_heads SET processed_sequence = $2 WHERE round_id = $1`, roundID, int64(k)) //nolint:gosec // G115: a test round's sequence, far below math.MaxInt64
}

// insertRejected inserts admission k and the well-formed rejected record k
// with the given fencing token, without advancing the heads.
func insertRejected(t *testing.T, db execer, roundID [16]byte, k uint64, prev chain.Hash, token int64) chain.Hash {
	t.Helper()
	r := rejectedRecord(roundID, k, prev)
	want, err := chain.EncodeCommand(chain.Command{
		RoundID: roundID, Sequence: k, PrevHash: prev, CommandID: r.commandID,
		EffectiveTimeUnixMicro: r.effective, EngineBuild: r.engineBuild, Payload: []byte(r.command),
		Result: chain.Result{Rejected: true, Reason: r.reason},
	})
	if err != nil || string(want) != string(r.bytes()) {
		t.Fatalf("section 5.3 template differs from chain.EncodeCommand (%v):\n%s\n%s", err, r.bytes(), want)
	}
	if err := insertAdmission(db, admissionOf(r)); err != nil {
		t.Fatalf("insert admission %d: %v", k, err)
	}
	if err := insertRecord(db, rowOf(r, token)); err != nil {
		t.Fatalf("insert record %d: %v", k, err)
	}
	return chain.LinkHash(r.bytes())
}

// appendRejected inserts admission k and a well-formed rejected record k
// (events [], reason round_settled, engineBuild test) under token, advances
// both heads to k and returns the record's link hash.
func appendRejected(t *testing.T, db execer, roundID [16]byte, k uint64, prev chain.Hash, token int64) chain.Hash {
	t.Helper()
	h := insertRejected(t, db, roundID, k, prev, token)
	if err := advanceHeads(db, roundID, k); err != nil {
		t.Fatalf("advance heads to %d: %v", k, err)
	}
	return h
}

type roundRow struct {
	id                   [16]byte
	game                 string
	rules, schema, state int64
	chain                string
	createdAtUnixMicro   int64
}

func roundOf(g chain.Genesis) roundRow {
	return roundRow{
		id: g.RoundID, game: string(g.Game),
		rules: int64(g.RulesVersion), schema: int64(g.SchemaVersion), state: int64(g.StateVersion),
		chain: chain.Version, createdAtUnixMicro: g.CreatedAtUnixMicro,
	}
}

func insertRound(db execer, r roundRow) error {
	return exec(db, `INSERT INTO orvex.rounds
		(round_id, game, rules_version, schema_version, state_version, chain, created_at_unix_micro)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		r.id, r.game, r.rules, r.schema, r.state, r.chain, r.createdAtUnixMicro)
}

func newGenesis() chain.Genesis {
	return chain.Genesis{
		RoundID: newID(), Game: "crash", RulesVersion: 1, SchemaVersion: 1, StateVersion: 1,
		Config: []byte(`{"commitment":"00"}`), CreatedAtUnixMicro: baseTimeUnixMicro, EngineBuild: "test",
	}
}

// insertRoundRows inserts the rounds row from g (after edit), its secret and
// both heads at 0.
func insertRoundRows(t *testing.T, db execer, g chain.Genesis, edit func(*roundRow)) {
	t.Helper()
	r := roundOf(g)
	if edit != nil {
		edit(&r)
	}
	if err := insertRound(db, r); err != nil {
		t.Fatalf("insert round: %v", err)
	}
	mustExec(t, db, `INSERT INTO orvex.round_secrets (round_id, config_secret) VALUES ($1, $2)`,
		g.RoundID, []byte(`{"seed":"`+strings.Repeat("ab", 32)+`"}`))
	mustExec(t, db, `INSERT INTO orvex.round_admission_heads (round_id) VALUES ($1)`, g.RoundID)
	mustExec(t, db, `INSERT INTO orvex.round_execution_heads (round_id) VALUES ($1)`, g.RoundID)
}

func genesisRow(t *testing.T, g chain.Genesis) recordRow {
	t.Helper()
	b, err := chain.EncodeGenesis(g)
	if err != nil {
		t.Fatal(err)
	}
	return recordRow{roundID: g.RoundID, sequence: 0, record: b}
}

// createRound inserts the rounds row from g (after edit), the secret, both
// heads at 0 and the genesis record from chain.EncodeGenesis(g) under token
// 0. It returns the genesis insert's error.
func createRound(t *testing.T, db execer, g chain.Genesis, edit func(*roundRow)) error {
	t.Helper()
	insertRoundRows(t, db, g, edit)
	return insertRecord(db, genesisRow(t, g))
}

// --- Rounds and admissions ---

func TestRoundsConstraints(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	g := newGenesis()
	base := roundOf(g)
	cases := []struct {
		name       string
		edit       func(*roundRow)
		constraint string
	}{
		{"nil uuid", func(r *roundRow) { r.id = [16]byte{} }, "rounds_round_id_check"},
		{"game Crash", func(r *roundRow) { r.game = "Crash" }, "rounds_game_check"},
		{"game of 33 characters", func(r *roundRow) { r.game = strings.Repeat("a", 33) }, "rounds_game_check"},
		{"version 0", func(r *roundRow) { r.rules = 0 }, "rounds_versions_check"},
		{"version 4294967296", func(r *roundRow) { r.schema = 4294967296 }, "rounds_versions_check"},
		{"state version 0", func(r *roundRow) { r.state = 0 }, "rounds_versions_check"},
		{"chain v2", func(r *roundRow) { r.chain = "orvex/audit/v2" }, "rounds_chain_check"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base
			c.edit(&r)
			requireCode(t, insertRound(pool, r), codeCheckViolation, c.constraint)
		})
	}
	r := base
	r.game = strings.Repeat("a", 32)
	r.rules, r.schema, r.state = 4294967295, 1, 4294967295
	if err := insertRound(pool, r); err != nil {
		t.Fatalf("valid round: %v", err)
	}
	requireCode(t, exec(pool, `UPDATE orvex.rounds SET created_at_unix_micro = 0 WHERE round_id = $1`, r.id), postgres.CodeAppendOnly, "")
	requireCode(t, exec(pool, `DELETE FROM orvex.rounds WHERE round_id = $1`, r.id), postgres.CodeAppendOnly, "")
}

func TestRoundCreationRequiresGenesis(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	g := newGenesis()
	requireCommitCode(t, pool, func(tx pgx.Tx) error {
		insertRoundRows(t, tx, g, nil)
		return nil
	}, codeFKViolation, "round_execution_heads_head_fkey")
	if n := count(t, pool, "SELECT count(*) FROM orvex.rounds"); n != 0 {
		t.Errorf("rounds holds %d rows after the failed commit, want 0", n)
	}
	mustCommit(t, pool, func(tx pgx.Tx) error { return createRound(t, tx, g, nil) })
}

func TestRoundSecretsAppendOnly(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	requireCode(t, exec(pool, `UPDATE orvex.round_secrets SET config_secret = '\x7b7d'::bytea WHERE round_id = $1`, l.RoundID), postgres.CodeAppendOnly, "")
	requireCode(t, exec(pool, `DELETE FROM orvex.round_secrets WHERE round_id = $1`, l.RoundID), postgres.CodeAppendOnly, "")
	requireCode(t, exec(pool, `TRUNCATE orvex.round_secrets`), postgres.CodeAppendOnly, "")
	if n := count(t, pool, "SELECT count(*) FROM orvex.round_secrets"); n != 1 {
		t.Errorf("round_secrets holds %d rows, want 1", n)
	}
}

func TestAdmissionsGapless(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	g := newGenesis()
	mustCommit(t, pool, func(tx pgx.Tx) error { return createRound(t, tx, g, nil) })
	a := admissionOf(rejectedRecord(g.RoundID, 2, chain.Hash{}))
	requireCode(t, insertAdmission(pool, a), codeFKViolation, "admissions_prev_fkey")
	a = admissionOf(rejectedRecord(g.RoundID, 0, chain.Hash{}))
	requireCode(t, insertAdmission(pool, a), codeCheckViolation, "admissions_sequence_check")
	for k := uint64(1); k <= 2; k++ {
		if err := insertAdmission(pool, admissionOf(rejectedRecord(g.RoundID, k, chain.Hash{}))); err != nil {
			t.Fatalf("admission %d: %v", k, err)
		}
	}
}

func TestAdmissionsCommandIDGloballyUnique(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	wx := loadGolden(t, pool, "worked-example")
	ic := loadGolden(t, pool, "instant-crash")
	a := admissionOf(rejectedRecord(ic.RoundID, uint64(ic.Records), chain.Hash{})) //nolint:gosec // G115: ic.Records is a golden history's record count (< 100)
	a.commandID = wx.CommandIDs[0]
	requireCode(t, insertAdmission(pool, a), codeUniqueViolation, "admissions_command_id_key")
	a.commandID = [16]byte{}
	requireCode(t, insertAdmission(pool, a), codeCheckViolation, "admissions_command_id_check")
}

func TestAdmissionsAppendOnly(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	requireCode(t, exec(pool, `UPDATE orvex.admissions SET effective_time_unix_micro = effective_time_unix_micro + 1
		WHERE round_id = $1 AND sequence = 1`, l.RoundID), postgres.CodeAppendOnly, "")
	requireCode(t, exec(pool, `DELETE FROM orvex.admissions WHERE round_id = $1 AND sequence = 22`, l.RoundID), postgres.CodeAppendOnly, "")
	requireTruncateRejected(t, pool, "admissions")
}

func TestRoundAdmissionHeads(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	ic := loadGolden(t, pool, "instant-crash")
	requireCommitCode(t, pool, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE orvex.round_admission_heads SET admitted_sequence = 23 WHERE round_id = $1`, l.RoundID)
	}, codeFKViolation, "round_admission_heads_head_fkey")
	requireCode(t, exec(pool, `UPDATE orvex.round_admission_heads SET admitted_sequence = 21 WHERE round_id = $1`, l.RoundID),
		postgres.CodeHeadRegression, "")
	requireCode(t, exec(pool, `UPDATE orvex.round_admission_heads SET round_id = $2 WHERE round_id = $1`, l.RoundID, ic.RoundID),
		postgres.CodeHeadRegression, "")
	requireCode(t, exec(pool, `DELETE FROM orvex.round_admission_heads WHERE round_id = $1`, l.RoundID), postgres.CodeAppendOnly, "")
	// A forward move with its admission commits.
	mustCommit(t, pool, func(tx pgx.Tx) error {
		if err := insertAdmission(tx, admissionOf(rejectedRecord(l.RoundID, 23, chain.Hash{}))); err != nil {
			return err
		}
		return exec(tx, `UPDATE orvex.round_admission_heads SET admitted_sequence = 23 WHERE round_id = $1`, l.RoundID)
	})
}

// --- Chain records ---

func TestChainRecordsHashChecked(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	r := rejectedRecord(l.RoundID, 23, l.Hashes[22])
	if err := insertAdmission(pool, admissionOf(r)); err != nil {
		t.Fatal(err)
	}
	row := rowOf(r, 0)
	h := chain.LinkHash(row.record)
	row.hash = &h
	tampered := strings.Replace(string(row.record), `"engineBuild":"test"`, `"engineBuild":"tesu"`, 1)
	if tampered == string(row.record) {
		t.Fatal("engineBuild not found in the record")
	}
	row.record = []byte(tampered)
	requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, row) }, codeCheckViolation, "chain_records_hash_check")
	// The untampered record commits.
	mustCommit(t, pool, func(tx pgx.Tx) error {
		if err := insertRecord(tx, rowOf(r, 0)); err != nil {
			return err
		}
		return advanceHeads(tx, l.RoundID, 23)
	})
}

func TestChainRecordsLinked(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")

	t.Run("wrong prev_hash", func(t *testing.T) {
		requireStmtCode(t, pool, func(tx pgx.Tx) error {
			r := rejectedRecord(l.RoundID, 23, l.Hashes[21])
			if err := insertAdmission(tx, admissionOf(r)); err != nil {
				return err
			}
			return insertRecord(tx, rowOf(r, 0))
		}, codeFKViolation, "chain_records_prev_fkey")
	})
	t.Run("gap", func(t *testing.T) {
		requireStmtCode(t, pool, func(tx pgx.Tx) error {
			if err := insertAdmission(tx, admissionOf(rejectedRecord(l.RoundID, 23, chain.Hash{}))); err != nil {
				return err
			}
			r := rejectedRecord(l.RoundID, 24, l.Hashes[22])
			if err := insertAdmission(tx, admissionOf(r)); err != nil {
				return err
			}
			return insertRecord(tx, rowOf(r, 0))
		}, codeFKViolation, "chain_records_prev_fkey")
	})
	t.Run("fork", func(t *testing.T) {
		// A second record 22 bound to admission 22 with a different result.
		var a admission
		var command []byte
		err := pool.QueryRow(ctx, `SELECT command_id, effective_time_unix_micro, command FROM orvex.admissions
			WHERE round_id = $1 AND sequence = 22`, l.RoundID).Scan(&a.commandID, &a.effective, &command)
		if err != nil {
			t.Fatal(err)
		}
		r := rec{
			roundID: l.RoundID, sequence: 22, prevHash: l.Hashes[21], command: string(command),
			commandID: a.commandID, effective: a.effective, engineBuild: "fork",
			status: "rejected", reason: "forked",
		}
		requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, rowOf(r, 0)) }, codeUniqueViolation, "")
	})
	t.Run("non-zero genesis prev_hash", func(t *testing.T) {
		g := newGenesis()
		requireStmtCode(t, pool, func(tx pgx.Tx) error {
			insertRoundRows(t, tx, g, nil)
			row := genesisRow(t, g) // the bytes keep the zero prevHash
			for i := range row.prevHash {
				row.prevHash[i] = 1
			}
			return insertRecord(tx, row)
		}, codeCheckViolation, "chain_records_genesis_check")
	})
}

func TestChainRecordsRequireAdmission(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	r := rejectedRecord(l.RoundID, 23, l.Hashes[22])
	requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, rowOf(r, 0)) },
		codeFKViolation, "chain_records_admission_fkey")
}

func TestChainRecordsBindAdmissionAndColumns(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	ic := loadGolden(t, pool, "instant-crash")
	good := rejectedRecord(l.RoundID, 23, l.Hashes[22])
	if err := insertAdmission(pool, admissionOf(good)); err != nil {
		t.Fatal(err)
	}
	accepted := good
	accepted.status, accepted.reason = "accepted", ""

	cases := []struct {
		name string
		row  func() recordRow
	}{
		{"effective time off by one", func() recordRow {
			r := good
			r.effective++
			return rowOf(r, 0)
		}},
		{"command bytes differ in one byte", func() recordRow {
			r := good
			r.command = strings.Replace(r.command, `"100"`, `"101"`, 1)
			return rowOf(r, 0)
		}},
		{"another command id", func() recordRow {
			r := good
			r.commandID = newID()
			return rowOf(r, 0)
		}},
		{"accepted column, rejected bytes", func() recordRow {
			row := rowOf(good, 0)
			row.status, row.reason = new("accepted"), nil
			return row
		}},
		{"rejected column, accepted bytes", func() recordRow {
			row := rowOf(accepted, 0)
			row.status, row.reason = new("rejected"), new("round_settled")
			return row
		}},
		{"different reason", func() recordRow {
			row := rowOf(good, 0)
			row.reason = new("round_closed")
			return row
		}},
		{"round_id in the bytes differs", func() recordRow {
			r := good
			r.roundID = ic.RoundID
			row := rowOf(r, 0)
			row.roundID = l.RoundID
			return row
		}},
		{"sequence in the bytes differs", func() recordRow {
			r := good
			r.sequence = 24
			row := rowOf(r, 0)
			row.sequence = 23
			return row
		}},
		{"prevHash in the bytes differs", func() recordRow {
			r := good
			r.prevHash = l.Hashes[21]
			row := rowOf(r, 0)
			row.prevHash = l.Hashes[22]
			return row
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, c.row()) }, postgres.CodeRecordBinding, "")
		})
	}
	genesisCases := []struct {
		name string
		edit func(*roundRow)
	}{
		{"genesis rules version differs from rounds", func(r *roundRow) { r.rules = 2 }},
		{"genesis schema version differs from rounds", func(r *roundRow) { r.schema = 2 }},
		{"genesis state version differs from rounds", func(r *roundRow) { r.state = 2 }},
		{"genesis game differs from rounds", func(r *roundRow) { r.game = "crush" }},
	}
	for _, c := range genesisCases {
		t.Run(c.name, func(t *testing.T) {
			g := newGenesis()
			requireStmtCode(t, pool, func(tx pgx.Tx) error { return createRound(t, tx, g, c.edit) }, postgres.CodeRecordBinding, "")
		})
	}
	// The well-formed record binds.
	mustCommit(t, pool, func(tx pgx.Tx) error {
		if err := insertRecord(tx, rowOf(good, 0)); err != nil {
			return err
		}
		return advanceHeads(tx, l.RoundID, 23)
	})
}

func TestChainRecordsResultShape(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	good := rejectedRecord(l.RoundID, 23, l.Hashes[22])
	if err := insertAdmission(pool, admissionOf(good)); err != nil {
		t.Fatal(err)
	}
	const constraint = "chain_records_result_check"

	t.Run("genesis with a result", func(t *testing.T) {
		g := newGenesis()
		requireStmtCode(t, pool, func(tx pgx.Tx) error {
			insertRoundRows(t, tx, g, nil)
			row := genesisRow(t, g)
			row.status = new("accepted")
			return insertRecord(tx, row)
		}, codeCheckViolation, constraint)
	})
	t.Run("rejected without a reason", func(t *testing.T) {
		row := rowOf(good, 0)
		row.reason = nil
		requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, row) }, codeCheckViolation, constraint)
	})
	t.Run("accepted with a reason", func(t *testing.T) {
		r := good
		r.status = "accepted"
		row := rowOf(r, 0)
		row.reason = new("round_settled")
		requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, row) }, codeCheckViolation, constraint)
	})
	t.Run("reason Bad", func(t *testing.T) {
		r := good
		r.reason = "Bad"
		requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, rowOf(r, 0)) }, codeCheckViolation, constraint)
	})
	t.Run("unknown status", func(t *testing.T) {
		r := good
		r.status = "pending"
		requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, rowOf(r, 0)) }, codeCheckViolation, constraint)
	})
}

func TestChainRecordsRecordCheck(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	good := rejectedRecord(l.RoundID, 23, l.Hashes[22])
	if err := insertAdmission(pool, admissionOf(good)); err != nil {
		t.Fatal(err)
	}
	const constraint = "chain_records_record_check"

	t.Run("newline in engineBuild", func(t *testing.T) {
		r := good
		r.engineBuild = "te\nst"
		requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, rowOf(r, 0)) }, codeCheckViolation, constraint)
	})
	t.Run("64 MiB + 1", func(t *testing.T) {
		r := good
		r.engineBuild = ""
		pad := 64<<20 + 1 - len(r.bytes())
		r.engineBuild = strings.Repeat("a", pad)
		if n := len(r.bytes()); n != 64<<20+1 {
			t.Fatalf("record is %d bytes", n)
		}
		requireStmtCode(t, pool, func(tx pgx.Tx) error { return insertRecord(tx, rowOf(r, 0)) }, codeCheckViolation, constraint)
	})
}

func TestChainRecordsAppendOnly(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	requireCode(t, exec(pool, `UPDATE orvex.chain_records SET fencing_token = 1 WHERE round_id = $1 AND sequence = 22`, l.RoundID),
		postgres.CodeAppendOnly, "")
	requireCode(t, exec(pool, `DELETE FROM orvex.chain_records WHERE round_id = $1 AND sequence = 22`, l.RoundID),
		postgres.CodeAppendOnly, "")
	// Other tables reference chain_records, so a plain TRUNCATE fails before
	// any trigger runs; CASCADE reaches the trigger.
	requireCode(t, exec(pool, `TRUNCATE orvex.chain_records`), codeFeatureNotSupp, "")
	requireTruncateRejected(t, pool, "chain_records")
}

// --- Execution heads and fencing ---

func TestExecutionCompareAndSet(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	const cas = `UPDATE orvex.round_execution_heads SET processed_sequence = 23
		WHERE round_id = $1 AND processed_sequence = 22 AND fencing_token = 0`

	tx1, err := pool.BeginTx(ctx, postgres.ExecutionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	tag, err := tx1.Exec(ctx, cas, l.RoundID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("first compare-and-set: %v, %d rows", err, tag.RowsAffected())
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx2, err := conn.BeginTx(ctx, postgres.ExecutionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	type result struct {
		rows int64
		err  error
	}
	done := make(chan result, 1)
	go func() {
		tag, err := tx2.Exec(ctx, cas, l.RoundID)
		done <- result{tag.RowsAffected(), err}
	}()
	pgtest.WaitUntilBlocked(t, pool, conn.Conn().PgConn().PID())

	insertRejected(t, tx1, l.RoundID, 23, l.Hashes[22], 0)
	if err := exec(tx1, `UPDATE orvex.round_admission_heads SET admitted_sequence = 23 WHERE round_id = $1`, l.RoundID); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	res := <-done
	if res.err != nil || res.rows != 0 {
		t.Errorf("second compare-and-set: %v, %d rows; want 0 rows", res.err, res.rows)
	}
	if n := count(t, pool, `SELECT processed_sequence FROM orvex.round_execution_heads WHERE round_id = $1`, l.RoundID); n != 23 {
		t.Errorf("processed_sequence = %d, want 23", n)
	}
}

func TestChainRecordsRequireCurrentFencingToken(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	mustExec(t, pool, `UPDATE orvex.round_execution_heads
		SET fencing_token = 1, owner_id = 'engine-b', lease_expires_at = now() + interval '1 minute'
		WHERE round_id = $1`, l.RoundID)

	requireCommitCode(t, pool, func(tx pgx.Tx) error {
		appendRejected(t, tx, l.RoundID, 23, l.Hashes[22], 0)
		return nil
	}, postgres.CodeNotFenced, "")
	requireCommitCode(t, pool, func(tx pgx.Tx) error {
		insertRejected(t, tx, l.RoundID, 23, l.Hashes[22], 1)
		return nil
	}, postgres.CodeNotFenced, "")
	mustCommit(t, pool, func(tx pgx.Tx) error {
		appendRejected(t, tx, l.RoundID, 23, l.Hashes[22], 1)
		return nil
	})
}

func TestExecutionHeadsForwardOnly(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	ic := loadGolden(t, pool, "instant-crash")
	update := func(set string, args ...any) error {
		return exec(pool, `UPDATE orvex.round_execution_heads SET `+set+` WHERE round_id = $1`, append([]any{l.RoundID}, args...)...)
	}
	if err := update(`fencing_token = 1, owner_id = 'engine-a', lease_expires_at = now() + interval '1 minute'`); err != nil {
		t.Fatalf("first takeover: %v", err)
	}
	for _, c := range []struct{ name, set string }{
		{"lower processed_sequence", `processed_sequence = 21`},
		{"lower token", `fencing_token = 0`},
		{"owner change with the same token", `owner_id = 'engine-b'`},
		{"owner release without a token increase", `owner_id = NULL, lease_expires_at = NULL`},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireCode(t, update(c.set), postgres.CodeHeadRegression, "")
		})
	}
	requireCode(t, update(`round_id = $2`, ic.RoundID), postgres.CodeHeadRegression, "")
	requireCode(t, update(`lease_expires_at = NULL`), codeCheckViolation, "round_execution_heads_owner_check")
	if err := update(`fencing_token = 2, owner_id = 'engine-b', lease_expires_at = now() + interval '1 minute'`); err != nil {
		t.Errorf("takeover with token + 1: %v", err)
	}
	if err := update(`lease_expires_at = now() + interval '2 minutes'`); err != nil {
		t.Errorf("lease renewal: %v", err)
	}
	if err := update(`fencing_token = 3, owner_id = NULL, lease_expires_at = NULL`); err != nil {
		t.Errorf("release with a new token: %v", err)
	}
	requireCode(t, exec(pool, `DELETE FROM orvex.round_execution_heads WHERE round_id = $1`, l.RoundID), postgres.CodeAppendOnly, "")
}

// --- Results, events and snapshots ---

type manifest struct {
	Histories []struct {
		Name     string `json:"name"`
		Accepted int    `json:"accepted"`
		Rejected int    `json:"rejected"`
		Commands int    `json:"commands"`
	} `json:"histories"`
}

func TestCommandResultsView(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	b, err := os.ReadFile(filepath.Join(goldenDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var accepted, rejected, commands int
	for _, h := range m.Histories {
		if h.Name == "worked-example" {
			accepted, rejected, commands = h.Accepted, h.Rejected, h.Commands
		}
	}
	if commands != 22 {
		t.Fatalf("manifest worked-example commands = %d, want 22", commands)
	}
	check := func() {
		t.Helper()
		if n := count(t, pool, `SELECT count(*) FROM orvex.command_results WHERE round_id = $1`, l.RoundID); n != commands {
			t.Errorf("command_results rows = %d, want %d", n, commands)
		}
		if n := count(t, pool, `SELECT count(*) FROM orvex.command_results WHERE round_id = $1 AND status = 'accepted' AND reason IS NULL`, l.RoundID); n != accepted {
			t.Errorf("accepted = %d, want %d", n, accepted)
		}
		if n := count(t, pool, `SELECT count(*) FROM orvex.command_results WHERE round_id = $1 AND status = 'rejected' AND reason IS NOT NULL`, l.RoundID); n != rejected {
			t.Errorf("rejected = %d, want %d", n, rejected)
		}
	}
	check()
	if n := count(t, pool, `SELECT count(*) FROM orvex.command_results r JOIN orvex.admissions a USING (round_id, sequence)
		WHERE round_id = $1 AND r.command_id = a.command_id AND r.effective_time_unix_micro = a.effective_time_unix_micro`, l.RoundID); n != commands {
		t.Errorf("rows matching their admission = %d, want %d", n, commands)
	}
	// A pending admission has no result.
	if err := insertAdmission(pool, admissionOf(rejectedRecord(l.RoundID, 23, chain.Hash{}))); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestDomainEventsContiguous(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	insertEvent := func(eventSeq, seq int64, typ string) error {
		return exec(pool, `INSERT INTO orvex.domain_events (round_id, event_sequence, sequence, event_type)
			VALUES ($1, $2, $3, $4)`, l.RoundID, eventSeq, seq, typ)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.domain_events WHERE round_id = $1`, l.RoundID); n != 9 {
		t.Fatalf("domain_events = %d, want 9", n)
	}
	requireCode(t, insertEvent(11, 22, "BetPlaced"), codeFKViolation, "domain_events_prev_fkey")
	requireCode(t, insertEvent(10, 23, "BetPlaced"), codeFKViolation, "domain_events_record_fkey")
	requireCode(t, insertEvent(10, 22, "betPlaced"), codeCheckViolation, "domain_events_event_type_check")
	requireCode(t, exec(pool, `UPDATE orvex.domain_events SET event_type = 'Other' WHERE round_id = $1 AND event_sequence = 1`, l.RoundID),
		postgres.CodeAppendOnly, "")
	requireCode(t, exec(pool, `DELETE FROM orvex.domain_events WHERE round_id = $1 AND event_sequence = 9`, l.RoundID),
		postgres.CodeAppendOnly, "")
	if err := insertEvent(10, 22, "BetPlaced"); err != nil {
		t.Errorf("contiguous event: %v", err)
	}
}

func TestRoundSnapshots(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	insertSnapshot := func(seq int64, hash []byte, build string) error {
		return exec(pool, `INSERT INTO orvex.round_snapshots (round_id, sequence, state_hash, engine_build)
			VALUES ($1, $2, $3, $4)`, l.RoundID, seq, hash, build)
	}
	hash := make([]byte, 32)
	requireCode(t, insertSnapshot(23, hash, "test"), codeFKViolation, "round_snapshots_record_fkey")
	requireCode(t, insertSnapshot(5, hash[:31], "test"), codeCheckViolation, "round_snapshots_state_hash_check")
	requireCode(t, insertSnapshot(5, hash, ""), codeCheckViolation, "round_snapshots_engine_build_check")
	requireCode(t, exec(pool, `UPDATE orvex.round_snapshots SET engine_build = 'x' WHERE round_id = $1 AND sequence = 9`, l.RoundID),
		postgres.CodeAppendOnly, "")
	requireCode(t, exec(pool, `DELETE FROM orvex.round_snapshots WHERE round_id = $1 AND sequence = 9`, l.RoundID),
		postgres.CodeAppendOnly, "")
	if err := insertSnapshot(5, hash, "test"); err != nil {
		t.Errorf("valid snapshot: %v", err)
	}
}

// --- Ledger ---

type account struct {
	id       [16]byte
	kind     string
	playerID *string
	name     *string
	roundID  *[16]byte
	floor    bool
}

func player(id string) account {
	return account{id: newID(), kind: "player", playerID: new(id), floor: true}
}

func system(name string, roundID *[16]byte) account {
	return account{id: newID(), kind: "system", name: new(name), roundID: roundID}
}

func insertAccount(db execer, a account) error {
	return exec(db, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, name, round_id, floor_enforced)
		VALUES ($1, $2, $3, $4, $5, $6)`, a.id, a.kind, a.playerID, a.name, a.roundID, a.floor)
}

func mustAccount(t *testing.T, db execer, a account) account {
	t.Helper()
	if err := insertAccount(db, a); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return a
}

// ledgerTx inserts a ledger transaction, optionally referencing the chain
// record (roundID, seq), and returns its ID.
func ledgerTx(db execer, kind string, roundID *[16]byte, seq *int64) ([16]byte, error) {
	id := newID()
	return id, exec(db, `INSERT INTO orvex.ledger_transactions (transaction_id, kind, round_id, sequence)
		VALUES ($1, $2, $3, $4)`, id, kind, roundID, seq)
}

func post(db execer, txID [16]byte, index int, debit, credit account, amount int64, roundID *[16]byte, eventSeq *int64) error {
	return exec(db, `INSERT INTO orvex.ledger_postings
		(transaction_id, posting_index, debit_account_id, credit_account_id, amount, round_id, event_sequence)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, txID, index, debit.id, credit.id, amount, roundID, eventSeq)
}

// grant moves amount from the faucet to a account in one committed ledger
// transaction.
func grant(t *testing.T, db *pgxpool.Pool, faucet, to account, amount int64) {
	t.Helper()
	mustCommit(t, db, func(tx pgx.Tx) error {
		id, err := ledgerTx(tx, "grant", nil, nil)
		if err != nil {
			return err
		}
		return post(tx, id, 0, faucet, to, amount, nil, nil)
	})
}

type totals struct{ debits, credits, held int64 }

func totalsOf(t *testing.T, db *pgxpool.Pool, a account) totals {
	t.Helper()
	var tt totals
	if err := db.QueryRow(ctx, `SELECT debits_posted, credits_posted, held FROM orvex.ledger_accounts WHERE account_id = $1`, a.id).
		Scan(&tt.debits, &tt.credits, &tt.held); err != nil {
		t.Fatal(err)
	}
	return tt
}

func hold(db execer, roundID [16]byte, seq int64, a account, amount int64) error {
	return exec(db, `INSERT INTO orvex.ledger_holds (round_id, sequence, account_id, amount) VALUES ($1, $2, $3, $4)`,
		roundID, seq, a.id, amount)
}

func release(db execer, roundID [16]byte, seq int64, a account) error {
	return exec(db, `INSERT INTO orvex.ledger_hold_releases (round_id, sequence, account_id) VALUES ($1, $2, $3)`,
		roundID, seq, a.id)
}

func TestLedgerAccounts(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")

	withName := player("bob")
	withName.name = new("pot")
	requireCode(t, insertAccount(pool, withName), codeCheckViolation, "ledger_accounts_kind_check")
	noName := system("pot", nil)
	noName.name = nil
	requireCode(t, insertAccount(pool, noName), codeCheckViolation, "ledger_accounts_kind_check")
	noFloor := player("bob")
	noFloor.floor = false
	requireCode(t, insertAccount(pool, noFloor), codeCheckViolation, "ledger_accounts_kind_check")
	emptyPlayer := player("")
	requireCode(t, insertAccount(pool, emptyPlayer), codeCheckViolation, "ledger_accounts_kind_check")

	alice := mustAccount(t, pool, player("alice"))
	requireCode(t, insertAccount(pool, player("alice")), codeUniqueViolation, "ledger_accounts_player_id_key")
	mustAccount(t, pool, system("pot", nil))
	requireCode(t, insertAccount(pool, system("pot", nil)), codeUniqueViolation, "ledger_accounts_system_name_key")
	scoped := mustAccount(t, pool, system("pot", &l.RoundID))
	requireCode(t, insertAccount(pool, system("pot", &l.RoundID)), codeUniqueViolation, "ledger_accounts_system_name_key")
	mustAccount(t, pool, player("bob")) // two players do not collide on (NULL, NULL)

	requireCode(t, exec(pool, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced, credits_posted)
		VALUES ($1, 'player', 'carol', true, 5)`, newID()), postgres.CodeLedgerAccount, "")
	requireCode(t, exec(pool, `UPDATE orvex.ledger_accounts SET credits_posted = 1000 WHERE account_id = $1`, alice.id),
		postgres.CodeLedgerAccount, "")
	requireCode(t, exec(pool, `UPDATE orvex.ledger_accounts SET name = 'other' WHERE account_id = $1`, scoped.id),
		postgres.CodeLedgerAccount, "")
	requireCode(t, exec(pool, `DELETE FROM orvex.ledger_accounts WHERE account_id = $1`, alice.id), postgres.CodeAppendOnly, "")
}

func TestLedgerPostingsMaintainTotals(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	faucet := mustAccount(t, pool, system("faucet", nil))
	alice := mustAccount(t, pool, player("alice"))
	pot := mustAccount(t, pool, system("pot", &l.RoundID))

	grant(t, pool, faucet, alice, 1000)
	mustCommit(t, pool, func(tx pgx.Tx) error {
		id, err := ledgerTx(tx, "bet", &l.RoundID, new(int64(1)))
		if err != nil {
			return err
		}
		return post(tx, id, 0, alice, pot, 100, &l.RoundID, new(int64(1)))
	})

	for _, c := range []struct {
		name string
		a    account
		want totals
	}{
		{"faucet", faucet, totals{debits: 1000}},
		{"alice", alice, totals{debits: 100, credits: 1000}},
		{"pot", pot, totals{credits: 100}},
	} {
		if got := totalsOf(t, pool, c.a); got != c.want {
			t.Errorf("%s totals = %+v, want %+v", c.name, got, c.want)
		}
	}
	var debits, credits, amounts int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT sum(debits_posted) FROM orvex.ledger_accounts),
		(SELECT sum(credits_posted) FROM orvex.ledger_accounts), (SELECT sum(amount) FROM orvex.ledger_postings)`).
		Scan(&debits, &credits, &amounts); err != nil {
		t.Fatal(err)
	}
	if debits != 1100 || credits != 1100 || amounts != 1100 {
		t.Errorf("sum debits %d, credits %d, amounts %d; want 1100 each", debits, credits, amounts)
	}
}

func TestLedgerFloorEnforced(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	faucet := mustAccount(t, pool, system("faucet", nil))
	alice := mustAccount(t, pool, player("alice"))
	pot := mustAccount(t, pool, system("pot", &l.RoundID))
	grant(t, pool, faucet, alice, 1000)

	requireStmtCode(t, pool, func(tx pgx.Tx) error {
		id, err := ledgerTx(tx, "bet", &l.RoundID, new(int64(1)))
		if err != nil {
			return err
		}
		return post(tx, id, 0, alice, pot, 1001, nil, nil)
	}, codeCheckViolation, "ledger_accounts_floor_check")
	if got := totalsOf(t, pool, alice); got != (totals{credits: 1000}) {
		t.Errorf("alice totals = %+v after the refused overspend", got)
	}
	// A system account may go negative.
	if got := totalsOf(t, pool, faucet); got.credits-got.debits != -1000 {
		t.Errorf("faucet balance = %d, want -1000", got.credits-got.debits)
	}
	grant(t, pool, faucet, pot, 5000)
	if got := totalsOf(t, pool, faucet); got.credits-got.debits != -6000 {
		t.Errorf("faucet balance = %d, want -6000", got.credits-got.debits)
	}
}

func TestLedgerPostingValidation(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	wx := loadGolden(t, pool, "worked-example")
	ic := loadGolden(t, pool, "instant-crash")
	faucet := mustAccount(t, pool, system("faucet", nil))
	alice := mustAccount(t, pool, player("alice"))
	pot := mustAccount(t, pool, system("pot", &ic.RoundID))
	grant(t, pool, faucet, alice, 1000)

	var icTx [16]byte
	mustCommit(t, pool, func(tx pgx.Tx) error {
		var err error
		icTx, err = ledgerTx(tx, "bet", &ic.RoundID, new(int64(1)))
		return err
	})
	cases := []struct {
		name       string
		posting    func(tx pgx.Tx) error
		code       string
		constraint string
	}{
		{"amount 0", func(tx pgx.Tx) error { return post(tx, icTx, 0, alice, pot, 0, nil, nil) },
			codeCheckViolation, "ledger_postings_amount_check"},
		{"amount -1", func(tx pgx.Tx) error { return post(tx, icTx, 0, alice, pot, -1, nil, nil) },
			codeCheckViolation, "ledger_postings_amount_check"},
		{"same account on both sides", func(tx pgx.Tx) error { return post(tx, icTx, 0, alice, alice, 10, nil, nil) },
			codeCheckViolation, "ledger_postings_accounts_check"},
		{"event without a round", func(tx pgx.Tx) error { return post(tx, icTx, 0, alice, pot, 10, nil, new(int64(1))) },
			codeCheckViolation, "ledger_postings_event_check"},
		{"event_sequence of another round", func(tx pgx.Tx) error { return post(tx, icTx, 0, alice, pot, 10, &ic.RoundID, new(int64(9))) },
			codeFKViolation, "ledger_postings_event_fkey"},
		{"round_id different from the transaction's", func(tx pgx.Tx) error { return post(tx, icTx, 0, alice, pot, 10, &wx.RoundID, nil) },
			codeFKViolation, "ledger_postings_round_fkey"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireStmtCode(t, pool, c.posting, c.code, c.constraint)
		})
	}
	mustCommit(t, pool, func(tx pgx.Tx) error { return post(tx, icTx, 0, alice, pot, 10, &ic.RoundID, new(int64(1))) })
}

func TestLedgerOneTransactionPerRecord(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	if _, err := ledgerTx(pool, "settle", &l.RoundID, new(int64(22))); err != nil {
		t.Fatal(err)
	}
	_, err := ledgerTx(pool, "settle", &l.RoundID, new(int64(22)))
	requireCode(t, err, codeUniqueViolation, "ledger_transactions_record_key")
	_, err = ledgerTx(pool, "settle", &l.RoundID, new(int64(23)))
	requireCode(t, err, codeFKViolation, "ledger_transactions_record_fkey")
	_, err = ledgerTx(pool, "settle", &l.RoundID, nil)
	requireCode(t, err, codeCheckViolation, "ledger_transactions_record_check")
}

func TestLedgerHolds(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	faucet := mustAccount(t, pool, system("faucet", nil))
	alice := mustAccount(t, pool, player("alice"))
	grant(t, pool, faucet, alice, 1000)

	if err := hold(pool, l.RoundID, 1, alice, 300); err != nil {
		t.Fatal(err)
	}
	if got := totalsOf(t, pool, alice); got != (totals{credits: 1000, held: 300}) {
		t.Errorf("alice totals after a hold = %+v", got)
	}
	requireCode(t, hold(pool, l.RoundID, 2, alice, 701), codeCheckViolation, "ledger_accounts_floor_check")
	requireCode(t, hold(pool, l.RoundID, 2, alice, 0), codeCheckViolation, "ledger_holds_amount_check")
	if err := release(pool, l.RoundID, 1, alice); err != nil {
		t.Fatal(err)
	}
	if got := totalsOf(t, pool, alice); got != (totals{credits: 1000}) {
		t.Errorf("alice totals after the release = %+v", got)
	}
	requireCode(t, release(pool, l.RoundID, 1, alice), codeUniqueViolation, "ledger_hold_releases_pkey")
	requireCode(t, release(pool, l.RoundID, 2, alice), codeFKViolation, "ledger_hold_releases_hold_fkey")

	if err := insertAdmission(pool, admissionOf(rejectedRecord(l.RoundID, 23, chain.Hash{}))); err != nil {
		t.Fatal(err)
	}
	if err := hold(pool, l.RoundID, 23, alice, 1000); err != nil {
		t.Fatal(err)
	}
	requireCode(t, release(pool, l.RoundID, 23, alice), codeFKViolation, "ledger_hold_releases_record_fkey")
	if got := totalsOf(t, pool, alice); got != (totals{credits: 1000, held: 1000}) {
		t.Errorf("alice totals = %+v, want the pending hold kept", got)
	}
}

func TestLedgerConcurrentCrossRoundHolds(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	wx := loadGolden(t, pool, "worked-example")
	ic := loadGolden(t, pool, "instant-crash")
	faucet := mustAccount(t, pool, system("faucet", nil))
	alice := mustAccount(t, pool, player("alice"))
	grant(t, pool, faucet, alice, 150)

	tx1, err := pool.BeginTx(ctx, postgres.AdmissionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	if err := hold(tx1, wx.RoundID, 1, alice, 100); err != nil {
		t.Fatal(err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx2, err := conn.BeginTx(ctx, postgres.AdmissionTxOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	done := make(chan error, 1)
	go func() { done <- hold(tx2, ic.RoundID, 1, alice, 100) }()
	pgtest.WaitUntilBlocked(t, pool, conn.Conn().PgConn().PID())

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("first hold commit: %v", err)
	}
	requireCode(t, <-done, codeCheckViolation, "ledger_accounts_floor_check")
	_ = tx2.Rollback(ctx)
	if got := totalsOf(t, pool, alice); got.held != 100 {
		t.Errorf("held = %d, want 100", got.held)
	}
}

func TestLedgerAppendOnly(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	faucet := mustAccount(t, pool, system("faucet", nil))
	alice := mustAccount(t, pool, player("alice"))
	grant(t, pool, faucet, alice, 1000)
	if err := hold(pool, l.RoundID, 1, alice, 100); err != nil {
		t.Fatal(err)
	}
	if err := release(pool, l.RoundID, 1, alice); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ table, set string }{
		{"ledger_transactions", "kind = 'other'"},
		{"ledger_postings", "amount = amount + 1"},
		{"ledger_holds", "amount = amount + 1"},
		{"ledger_hold_releases", "created_at = now()"},
	} {
		t.Run(c.table, func(t *testing.T) {
			if n := count(t, pool, "SELECT count(*) FROM orvex."+c.table); n == 0 {
				t.Fatalf("orvex.%s is empty", c.table)
			}
			requireCode(t, exec(pool, "UPDATE orvex."+c.table+" SET "+c.set), postgres.CodeAppendOnly, "")
			requireCode(t, exec(pool, "DELETE FROM orvex."+c.table), postgres.CodeAppendOnly, "")
			requireTruncateRejected(t, pool, c.table)
		})
	}
}

// --- Outbox and truncation ---

func TestOutbox(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "worked-example")
	insertMessage := func(seq int64, index int) (int64, error) {
		var id int64
		err := pool.QueryRow(ctx, `INSERT INTO orvex.outbox_messages (round_id, sequence, message_index, topic, payload)
			VALUES ($1, $2, $3, 'round.events', '\x7b7d'::bytea) RETURNING message_id`, l.RoundID, seq, index).Scan(&id)
		return id, err
	}
	a, err := insertMessage(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := insertMessage(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = insertMessage(1, 0)
	requireCode(t, err, codeUniqueViolation, "outbox_messages_dedupe_key")
	_, err = insertMessage(23, 0)
	requireCode(t, err, codeFKViolation, "outbox_messages_record_fkey")

	update := func(id int64, set string) error {
		return exec(pool, `UPDATE orvex.outbox_messages SET `+set+` WHERE message_id = $1`, id)
	}
	for _, set := range []string{
		"attempts = attempts + 1",
		"next_attempt_at = now() + interval '1 second'",
		"last_error = 'timeout'",
	} {
		if err := update(a, set); err != nil {
			t.Errorf("delivery update %q: %v", set, err)
		}
	}
	for _, set := range []string{
		`payload = '\x5b5d'::bytea`,
		"topic = 'other'",
		"attempts = 0",
		"message_index = 9",
		"created_at = now() - interval '1 day'",
	} {
		requireCode(t, update(a, set), postgres.CodeOutboxImmutable, "")
	}
	requireCode(t, exec(pool, `DELETE FROM orvex.outbox_messages WHERE message_id = $1`, a), postgres.CodeOutboxImmutable, "")
	if err := update(a, "published_at = now()"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	requireCode(t, update(a, "last_error = NULL"), postgres.CodeOutboxImmutable, "")
	requireCode(t, update(a, "attempts = attempts + 1"), postgres.CodeOutboxImmutable, "")
	requireCode(t, exec(pool, `TRUNCATE orvex.outbox_messages`), postgres.CodeAppendOnly, "")
	if err := exec(pool, `DELETE FROM orvex.outbox_messages WHERE message_id = $1`, a); err != nil {
		t.Errorf("delete a published message: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.outbox_messages WHERE message_id = $1`, b); n != 1 {
		t.Errorf("unpublished message %d gone", b)
	}
}

// TestTruncateRejectedEverywhere requires a BEFORE TRUNCATE trigger named
// <table>_no_truncate on every table in schema orvex and that TRUNCATE ...
// CASCADE of each fails with OX001.
func TestTruncateRejectedEverywhere(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	loadGolden(t, pool, "worked-example")
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'orvex' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 14 {
		t.Errorf("orvex holds %d tables, want 14", len(tables))
	}
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			// tgtype: bit 1 ROW, bit 2 BEFORE, bit 6 TRUNCATE.
			n := count(t, pool, `SELECT count(*) FROM pg_trigger g
				JOIN pg_class c ON c.oid = g.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
				JOIN pg_proc p ON p.oid = g.tgfoid
				WHERE n.nspname = 'orvex' AND c.relname = $1 AND g.tgname = $1 || '_no_truncate'
				  AND g.tgtype & 1 = 0 AND g.tgtype & 2 = 2 AND g.tgtype & 32 = 32
				  AND p.proname = 'forbid_mutation'`, table)
			if n != 1 {
				t.Errorf("orvex.%s has no statement-level BEFORE TRUNCATE trigger %s_no_truncate", table, table)
			}
			requireTruncateRejected(t, pool, table)
		})
	}
}

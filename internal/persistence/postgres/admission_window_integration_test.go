//go:build integration

package postgres_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// Tests of migration 0007's schema objects (LBT-11 spec §5, §10 step 1).
// Rows come from the empty-round golden history, whose admissions 1 to 3
// are StartRound, CloseRound and SettleRound; LoadHistory leaves the head
// open with admitted_sequence 3.

const (
	windowCheck = "round_admission_heads_window_check"
	stateCheck  = "round_admission_heads_state_check"
)

func TestAdmissionWindowConstraints(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "empty-round")
	round := l.RoundID
	setHead := func(set string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE orvex.round_admission_heads SET `+set+` WHERE round_id = $1`, round)
		}
	}
	// insertAdmission4 appends admission 4 so that admitted_sequence can
	// move to 4 in the same transaction.
	insertAdmission4 := func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
			VALUES ($1, 4, $2, $3, $4)`, round, newID(), baseTimeUnixMicro, []byte(placeBet))
	}

	var state string
	if err := pool.QueryRow(ctx, `SELECT admission_state FROM orvex.round_admission_heads WHERE round_id = $1`, round).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "open" {
		t.Fatalf("LoadHistory head state = %q, want open", state)
	}

	invalid := []struct {
		name       string
		fn         func(tx pgx.Tx) error
		constraint string
	}{
		{"started without started_sequence", setHead(`admission_state = 'started'`), windowCheck},
		{"closed without closed_sequence", setHead(`admission_state = 'closed', started_sequence = 1`), windowCheck},
		{"closed_sequence = started_sequence", setHead(`admission_state = 'closed', started_sequence = 1, closed_sequence = 1`), windowCheck},
		{"sealed_sequence below admitted_sequence", func(tx pgx.Tx) error {
			if err := insertAdmission4(tx); err != nil {
				return err
			}
			return setHead(`admitted_sequence = 4, admission_state = 'sealed',
				started_sequence = 1, closed_sequence = 2, sealed_sequence = 3`)(tx)
		}, windowCheck},
		{"started_sequence above admitted_sequence", setHead(`admission_state = 'started', started_sequence = 4`), windowCheck},
		{"unknown state", setHead(`admission_state = 'bogus'`), stateCheck},
	}
	for _, c := range invalid {
		t.Run(c.name, func(t *testing.T) {
			requireStmtCode(t, pool, c.fn, codeCheckViolation, c.constraint)
		})
	}

	// The valid progression.
	mustCommit(t, pool, setHead(`admission_state = 'started', started_sequence = 1`))
	mustCommit(t, pool, setHead(`admission_state = 'closed', closed_sequence = 2`))
	mustCommit(t, pool, setHead(`admission_state = 'sealed', sealed_sequence = 3`))

	// Forward only, and sealed is final.
	requireStmtCode(t, pool, setHead(`admission_state = 'closed', sealed_sequence = NULL`), postgres.CodeHeadRegression, "")
	requireStmtCode(t, pool, setHead(`admission_state = 'closed'`), postgres.CodeHeadRegression, "")
	requireStmtCode(t, pool, setHead(`started_sequence = 2`), postgres.CodeHeadRegression, "")
	requireStmtCode(t, pool, func(tx pgx.Tx) error {
		if err := insertAdmission4(tx); err != nil {
			return err
		}
		return setHead(`admitted_sequence = 4`)(tx)
	}, codeCheckViolation, windowCheck)

	var admitted, sealed int64
	if err := pool.QueryRow(ctx, `SELECT admitted_sequence, sealed_sequence FROM orvex.round_admission_heads WHERE round_id = $1`, round).
		Scan(&admitted, &sealed); err != nil {
		t.Fatal(err)
	}
	if admitted != 3 || sealed != 3 {
		t.Errorf("head = admitted %d sealed %d, want 3 and 3", admitted, sealed)
	}
}

func TestUnixMicroExact(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	for _, s := range []string{
		"1970-01-01T00:00:00Z",
		"2025-10-09T08:53:20.123457Z",
		"1969-12-31T23:59:59.999999Z",
		"2262-01-01T00:00:00Z",
	} {
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		var got int64
		if err := pool.QueryRow(ctx, `SELECT orvex.unix_micro($1)`, ts).Scan(&got); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if want := ts.UnixMicro(); got != want {
			t.Errorf("unix_micro(%s) = %d, want %d", s, got, want)
		}
	}
	var got int64
	if err := pool.QueryRow(ctx, `SELECT orvex.unix_micro('2025-10-09 08:53:20.123457+00'::timestamptz)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 1760000000123457 {
		t.Errorf("unix_micro(2025-10-09 08:53:20.123457+00) = %d, want 1760000000123457", got)
	}
}

func TestAdmitCommandArgumentChecks(t *testing.T) {
	t.Parallel()
	pool := migrated(t)
	l := loadGolden(t, pool, "empty-round")
	type args struct {
		roundID, game, rules, commandID, command any // nil passes NULL
		class                                    string
		holdPlayer                               *string
		holdCredits                              *int64
		lockMs                                   int32
		idleMs                                   int32
	}
	valid := args{
		roundID: l.RoundID, game: "crash", rules: int64(1),
		command: []byte(`{"authority":"player","player":"alice","type":"CashOut"}`),
		class:   "ordinary", lockMs: 3000, idleMs: 1000,
	}
	cases := []struct {
		name string
		edit func(*args)
	}{
		{"NULL round id", func(a *args) { a.roundID = nil }},
		{"NULL game", func(a *args) { a.game = nil }},
		{"NULL rules version", func(a *args) { a.rules = nil }},
		{"NULL command id", func(a *args) { a.commandID = nil }},
		{"NULL command", func(a *args) { a.command = nil }},
		{"class other", func(a *args) { a.class = "other" }},
		{"hold player without credits", func(a *args) { a.holdPlayer = new("alice") }},
		{"hold credits without player", func(a *args) { a.holdCredits = new(int64(5)) }},
		{"hold credits 0", func(a *args) { a.holdPlayer, a.holdCredits = new("alice"), new(int64(0)) }},
		{"lock timeout 0", func(a *args) { a.lockMs = 0 }},
		{"idle timeout 0", func(a *args) { a.idleMs = 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := valid
			a.commandID = newID()
			c.edit(&a)
			requireStmtCode(t, pool, func(tx pgx.Tx) error {
				return exec(tx, `SELECT * FROM orvex.admit_command($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
					a.roundID, a.game, a.rules, a.commandID, a.command,
					a.class, a.holdPlayer, a.holdCredits, a.lockMs, a.idleMs)
			}, "22023", "")
		})
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.admissions`); n != 3 {
		t.Errorf("database holds %d admissions, want 3", n)
	}
	if n := count(t, pool, `SELECT admitted_sequence FROM orvex.round_admission_heads WHERE round_id = $1`, l.RoundID); n != 3 {
		t.Errorf("admitted_sequence = %d, want 3", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM orvex.ledger_holds`); n != 0 {
		t.Errorf("database holds %d holds, want 0", n)
	}
}

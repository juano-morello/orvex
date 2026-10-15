//go:build integration

package admission_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

// admittingRoleGrants are the admitting role's grants once orvex.admit_command
// is SECURITY DEFINER (LBT-12 engineer addendum; docs/architecture/persistence.md
// section 12 "Grants" mirrors this list). %s is the quoted role name.
var admittingRoleGrants = []string{
	`GRANT USAGE ON SCHEMA orvex TO %s`,
	`GRANT EXECUTE ON FUNCTION orvex.admit_command(uuid, text, bigint, uuid, bytea, text, text, bigint, integer, integer) TO %s`,
	`GRANT SELECT ON orvex.admissions, orvex.chain_records TO %s`,
}

// admittingRole creates a NOLOGIN role with exactly admittingRoleGrants in
// e's database and returns its name and a pool whose sessions SET ROLE to
// it. Cleanup closes the pool, then drops what the role owns and the role.
func admittingRole(t *testing.T, e env) (string, *pgxpool.Pool) {
	t.Helper()
	name := fmt.Sprintf("orvex_admitter_%d", idCounter.Add(1))
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := e.obs.Exec(ctx, "CREATE ROLE "+quoted+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		if _, err := e.obs.Exec(c, "DROP OWNED BY "+quoted); err != nil {
			t.Errorf("drop owned by %s: %v", name, err)
		}
		if _, err := e.obs.Exec(c, "DROP ROLE "+quoted); err != nil {
			t.Errorf("drop role %s: %v", name, err)
		}
	})
	for _, g := range admittingRoleGrants {
		if _, err := e.obs.Exec(ctx, fmt.Sprintf(g, quoted)); err != nil {
			t.Fatalf("%s: %v", g, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	cfg.AfterConnect = func(c context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(c, "SET ROLE "+quoted)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return name, pool
}

func TestAdmittingRoleCannotReadCloseBound(t *testing.T) {
	e := newEnv(t, 0)
	name, rolePool := admittingRole(t, e)
	s := newSequencer(t, rolePool, nil)
	_, state := createRound(t, e.pool, workedRoundID, workedSeed)
	d := state.CrashDeadlineOffsetMicros
	if d != 9_410_000 {
		t.Fatalf("D = %d, want 9410000 for the worked round", d)
	}
	round := workedRoundID
	fundPlayer(t, e.pool, "alice", 100)

	// The whole window, through the role's sequencer.
	bet := mustAdmit(t, s, request(round, cmdID(1), placeBet("alice", 10)))
	if h := held(t, e.obs, "alice"); h != 10 {
		t.Errorf("alice held %d, want 10", h)
	}
	x := bet.EffectiveTimeUnixMicro + 3_600_000_000
	seedAdmission(t, e.pool, round, cmdID(2), cashOut("zed"), x)
	start := mustAdmit(t, s, request(round, cmdID(3), startRound))
	if start.EffectiveTimeUnixMicro != x {
		t.Fatalf("StartRound effective time = %d, want %d", start.EffectiveTimeUnixMicro, x)
	}
	_, err := s.Admit(ctx, closeRequest(round, cmdID(4)))
	var nd *admission.ClosureNotDueError
	if !errors.As(err, &nd) || nd.EffectiveTimeUnixMicro != x {
		t.Fatalf("early CloseRound: err = %v, want ClosureNotDueError at %d", err, x)
	}
	seedAdmission(t, e.pool, round, cmdID(5), cashOut("zed"), x+d)
	closed := mustAdmit(t, s, closeRequest(round, cmdID(6)))
	if closed.EffectiveTimeUnixMicro != x+d {
		t.Errorf("CloseRound effective time = %d, want T0 + D = %d", closed.EffectiveTimeUnixMicro, x+d)
	}
	mustAdmit(t, s, request(round, cmdID(7), settleRound))
	st, err := s.Lookup(ctx, cmdID(1))
	if err != nil || st.Admission.Sequence != bet.Sequence || st.Outcome != admission.OutcomePending {
		t.Errorf("Lookup(bet) = %+v, %v; want pending at %d", st, err, bet.Sequence)
	}

	// The role reads neither the offset nor the bound, and writes nothing
	// directly.
	b, err := v1.Adapter().EncodeCommand(cashOut("mallory"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT close_offset_micros FROM orvex.round_secrets`, nil},
		{`SELECT config_secret FROM orvex.round_secrets`, nil},
		{`SELECT close_not_before_unix_micro FROM orvex.round_admission_heads`, nil},
		{`SELECT admission_state FROM orvex.round_admission_heads`, nil},
		{`INSERT INTO orvex.admissions (round_id, sequence, command_id, effective_time_unix_micro, command)
			VALUES ($1, 99, $2, 1, $3)`, []any{round, cmdID(99), b}},
	} {
		_, err := rolePool.Exec(ctx, q.sql, q.args...)
		if code := postgres.SQLState(err); code != "42501" {
			t.Errorf("as %s, %q: err = %v, want SQLSTATE 42501", name, q.sql, err)
		}
	}

	for _, c := range []struct {
		sql  string
		args []any
		want bool
	}{
		{`SELECT has_column_privilege($1, 'orvex.round_secrets', 'close_offset_micros', 'SELECT')`, []any{name}, false},
		{`SELECT has_column_privilege($1, 'orvex.round_admission_heads', 'close_not_before_unix_micro', 'SELECT')`, []any{name}, false},
		{`SELECT has_function_privilege('public', $1, 'EXECUTE')`, []any{admitCommandSignature}, false},
		{`SELECT has_function_privilege($1, $2, 'EXECUTE')`, []any{name, admitCommandSignature}, true},
	} {
		var got bool
		if err := e.obs.QueryRow(ctx, c.sql, c.args...).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s %v = %v, want %v", c.sql, c.args, got, c.want)
		}
	}
}

const admitCommandSignature = "orvex.admit_command(uuid,text,bigint,uuid,bytea,text,text,bigint,integer,integer)"

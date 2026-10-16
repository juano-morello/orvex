//go:build integration

package main

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
)

// Integration helpers for crashctl audit export (LBT-19 spec section 10
// step 5). Golden rounds are created, funded and seeded by
// internal/persistence/postgres/pgtest/roundtest (LBT-24), shared with the
// LBT-12 executor tests.

func execOrFatal(t *testing.T, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// seedGolden creates the golden round in file (relative to goldenDir) with
// an executor over pool built from crashctl's registry (EngineBuild
// "golden"), funds each player with the sum of their S-10 holds, and seeds
// every golden admission in order. It executes nothing.
func seedGolden(t *testing.T, pool *pgxpool.Pool, file string) roundtest.Golden {
	t.Helper()
	reg, err := newRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return roundtest.SeedGolden(t, pool, roundtest.NewExecutor(t, pool, nil, reg), filepath.Join(goldenDir, file), 0)
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

// exportGrants is the orvex_auditor grant set of LBT-21 K-14 (superseding
// LBT-19 X-11): nothing on round_secrets, SELECT on round_secret_reveals,
// the reveals of settled rounds. %[1]s is the role, %[2]s the database.
var exportGrants = []string{
	`GRANT CONNECT ON DATABASE %[2]s TO %[1]s`,
	`GRANT USAGE ON SCHEMA orvex TO %[1]s`,
	`GRANT SELECT ON orvex.rounds, orvex.chain_records TO %[1]s`,
	`GRANT SELECT ON orvex.round_secret_reveals TO %[1]s`,
	`GRANT SELECT (round_id, sequence, state_hash) ON orvex.round_snapshots TO %[1]s`,
}

// secretReaderGrants is the orvex_secret_reader grant set of LBT-21 K-10:
// the sealed and legacy secret columns of round_secrets, never
// close_offset_micros. Granting exportGrants and secretReaderGrants to one
// login role models membership in both orvex_auditor and
// orvex_secret_reader.
var secretReaderGrants = []string{
	`GRANT SELECT (round_id, config_secret, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext) ON orvex.round_secrets TO %[1]s`,
}

// restrictedRole creates a LOGIN role orvex_export_<n> with password
// exportRolePassword, runs grants in superDSN's database, and returns
// superDSN with that role's credentials (loginRole).
func restrictedRole(t *testing.T, superDSN string, grants []string) (roleDSN string) {
	t.Helper()
	return loginRole(t, superDSN, fmt.Sprintf("orvex_export_%d", exportRoles.Add(1)), exportRolePassword, grants)
}

// loginRole creates LOGIN role role with password, runs grants in
// superDSN's database, and returns superDSN with that role's credentials.
// At cleanup it runs DROP OWNED BY in that database, then DROP ROLE; call it
// after creating the database so the cleanup runs before the database is
// dropped.
func loginRole(t *testing.T, superDSN, role, password string, grants []string) (roleDSN string) {
	t.Helper()
	ctx := context.Background()
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
	execOrFatal(t, conn, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, quotedRole, password))
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
	u.User = url.UserPassword(role, password)
	return u.String()
}

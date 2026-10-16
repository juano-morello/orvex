//go:build integration

package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

// sentinelRolePassword is the password of every sentinel login role.
const sentinelRolePassword = "sentinel_pw_q2" //nolint:gosec // G101: a test role's password in a throwaway container, not a secret

// databaseName returns the name of the database dsn names.
func databaseName(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var name string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

// withURL returns dsn edited by edit.
func withURL(t *testing.T, dsn string, edit func(*url.URL)) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	edit(u)
	return u.String()
}

// TestNeverPrintsDatabaseURLAgainstServer: against a real server, whatever
// fails, crashctl prints no part of ORVEX_DATABASE_URL: not the role, the
// password, the database, the server's host:port or the DSN, while the
// SQLSTATE still says what failed (LBT-21 K-12, acceptance 3).
func TestNeverPrintsDatabaseURLAgainstServer(t *testing.T) {
	dsn, pool := exportDatabase(t)
	worked := drained(t, pool, "worked-example.jsonl")
	open := drained(t, pool, "in-progress.jsonl")
	database := databaseName(t, dsn)
	var hostPort string
	withURL(t, dsn, func(u *url.URL) { hostPort = u.Host })

	newRole := func(superDSN string, grants []string) (string, string) {
		role := fmt.Sprintf("sentinel_role_q2_%d", exportRoles.Add(1))
		return role, loginRole(t, superDSN, role, sentinelRolePassword, grants)
	}
	var auditorGrants []string
	for _, g := range exportGrants {
		if !strings.Contains(g, "round_secret_reveals") {
			auditorGrants = append(auditorGrants, g)
		}
	}
	connectRole, connectDSN := newRole(dsn, exportGrants)
	noConnectRole, noConnectDSN := newRole(dsn, nil)
	auditorRole, auditorDSN := newRole(dsn, auditorGrants)
	emptyDSN := pgtest.EmptyDatabase(t)
	emptyDatabase := databaseName(t, emptyDSN)
	creatorRole, creatorDSN := newRole(emptyDSN, nil)
	execOrFatal(t, pool, fmt.Sprintf(`REVOKE CONNECT ON DATABASE %s FROM PUBLIC`, pgx.Identifier{database}.Sanitize()))

	type run func() (int, string, string)
	exportRun := func(args ...string) run {
		return func() (int, string, string) { return exportCmd(t, args...) }
	}
	migrateRun := func(args ...string) run {
		return func() (int, string, string) { return runMigrateCmd(args...) }
	}
	file := func() string { return t.TempDir() + "/round.jsonl" }

	tests := []struct {
		name     string
		dsn      string
		role     string
		keyring  bool
		run      run
		wantCode int
		want     string
	}{
		{"wrong password, export", withURL(t, connectDSN, func(u *url.URL) { u.User = url.UserPassword(connectRole, "sentinel_wrong_q2") }),
			connectRole, false, exportRun(worked.UUID(), file()), exitIO, "SQLSTATE 28P01"},
		{"wrong password, migrate", withURL(t, connectDSN, func(u *url.URL) { u.User = url.UserPassword(connectRole, "sentinel_wrong_q2") }),
			connectRole, false, migrateRun("status"), exitIO, "SQLSTATE 28P01"},
		{"unknown database", withURL(t, connectDSN, func(u *url.URL) { u.Path = "/sentinel_db_q2_missing" }),
			connectRole, false, exportRun(worked.UUID(), file()), exitIO, "SQLSTATE 3D000"},
		{"no CONNECT", noConnectDSN, noConnectRole, false, exportRun(worked.UUID(), file()), exitIO, "SQLSTATE 42501"},
		{"migrate up without CREATE", creatorDSN, creatorRole, false, migrateRun("up"), exitFailed, "SQLSTATE 42501"},
		{"auditor without the reveal grant", auditorDSN, auditorRole, false, exportRun(worked.UUID(), file()), exitIO,
			"eventstore: read revealed secret: SQLSTATE 42501"},
		{"auditor -unsettled", auditorDSN, auditorRole, true, exportRun("-unsettled", open.UUID(), file()), exitIO, "SQLSTATE 42501"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(databaseURLEnv, tt.dsn)
			t.Setenv(sealing.KeyringFileEnv, "")
			if tt.keyring {
				t.Setenv(sealing.KeyringFileEnv, pgtest.KeyringFile(t))
			}
			code, stdout, stderr := tt.run()
			if code != tt.wantCode || !strings.Contains(stderr, tt.want) {
				t.Errorf("exit %d, stderr %q; want %d with %q", code, stderr, tt.wantCode, tt.want)
			}
			if strings.Count(stderr, "\n") != 1 {
				t.Errorf("stderr %q, want one line", stderr)
			}
			out := stdout + stderr
			for _, s := range []string{tt.role, sentinelRolePassword, "sentinel_wrong_q2", "sentinel_db_q2", database, emptyDatabase, hostPort, tt.dsn} {
				if strings.Contains(out, s) {
					t.Errorf("output %q contains %q", out, s)
				}
			}
		})
	}
}

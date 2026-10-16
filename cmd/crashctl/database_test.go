package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

// sentinelDSN is a URL whose every DSN-derived value is a sentinel no
// message may contain (LBT-21 K-12); 127.0.0.1:1 refuses connections.
const sentinelDSN = "postgres://sentinel_user_q1:sentinel_pw_q1@127.0.0.1:1/sentinel_db_q1?application_name=sentinel_app_q1&connect_timeout=2" //nolint:gosec // G101: dummy credentials for a port nothing listens on, not a secret

// sentinelDSNs are the DSN forms the never-prints tests run every
// subcommand against.
var sentinelDSNs = map[string]string{ //nolint:gosec // G101: dummy credentials for hosts and ports nothing serves, not a secret
	"url refused":       sentinelDSN,
	"key=value refused": "host=127.0.0.1 port=1 user=sentinel_user_q1 password=sentinel_pw_q1 dbname=sentinel_db_q1 application_name=sentinel_app_q1 connect_timeout=2",
	"url dns failure":   "postgres://sentinel_user_q1:sentinel_pw_q1@sentinel-host-q1.invalid:5432/sentinel_db_q1?application_name=sentinel_app_q1&connect_timeout=2",
	"key=value dns":     "host=sentinel-host-q1.invalid port=5432 user=sentinel_user_q1 password=sentinel_pw_q1 dbname=sentinel_db_q1 connect_timeout=2",
	"missing root cert": "postgres://sentinel_user_q1:sentinel_pw_q1@127.0.0.1:1/sentinel_db_q1?sslmode=verify-full&sslrootcert=/sentinel_q1/root.crt&connect_timeout=2",
	"unparsable":        "postgres://sentinel_user_q1:sentinel_pw_q1@127.0.0.1:1/sentinel_db_q1?connect_timeout=x",
}

// requireNoSentinel fails if out holds any DSN-derived sentinel, the raw
// DSN or the refused host:port.
func requireNoSentinel(t *testing.T, what, out, dsn string) {
	t.Helper()
	for _, s := range []string{"sentinel", "127.0.0.1:1", dsn} {
		if strings.Contains(out, s) {
			t.Errorf("%s %q contains %q", what, out, s)
		}
	}
}

// opaqueError is an error of a trusted package (crashctl itself) whose text
// does not contain its child's.
type opaqueError struct{ err error }

func (e opaqueError) Error() string { return "crashctl test: opaque failure" }
func (e opaqueError) Unwrap() error { return e.err }

// opaqueError2 is a trusted error type whose text repeats its child's.
type opaqueError2 struct{ err error }

func (e opaqueError2) Error() string {
	return "crashctl test: " + e.err.Error() + " then " + e.err.Error()
}
func (e opaqueError2) Unwrap() error { return e.err }

// refusedConnectError returns a real *pgconn.ConnectError from a refused
// connect to the sentinel DSN.
func refusedConnectError(t *testing.T) error {
	t.Helper()
	conn, err := pgconn.Connect(context.Background(), sentinelDSN)
	if err == nil {
		_ = conn.Close(context.Background())
		t.Fatal("connect to 127.0.0.1:1 succeeded")
	}
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("connect error %T is not a *pgconn.ConnectError", err)
	}
	return err
}

func TestDescribe(t *testing.T) {
	cfg, err := pgx.ParseConfig(sentinelDSN)
	if err != nil {
		t.Fatal(err)
	}
	denied := &pgconn.PgError{Severity: "ERROR", Code: "42501", Message: "permission denied for database sentinel_db_q1",
		Detail: "user sentinel_user_q1", Hint: "host sentinel-host-q1"}
	check := &pgconn.PgError{Code: "23514", Message: `new row for relation "round_secrets" violates check constraint`,
		ConstraintName: "round_secrets_sealed_check", TableName: "round_secrets", SchemaName: "orvex"}
	refused := &pgconn.PgError{Code: "OX003", Message: "down migration refused: orvex.round_secret_reveals holds rows"}
	opErr := &net.OpError{Op: "read", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		Err: &net.DNSError{Err: "no such host", Name: "sentinel-host-q1.invalid:5432"}}
	timeout := &net.DNSError{Err: "i/o timeout", Name: "sentinel-host-q1.invalid", IsTimeout: true}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"42501", denied, "SQLSTATE 42501"},
		{"23514 with a constraint", check, "SQLSTATE 23514 constraint round_secrets_sealed_check"},
		{"class OX keeps its message", fmt.Errorf("postgres: migration 0010 sealed_secrets down: %w", refused),
			"postgres: migration 0010 sealed_secrets down: down migration refused: orvex.round_secret_reveals holds rows (SQLSTATE OX003)"},
		{"wrapped server error", fmt.Errorf("eventstore: read records: %w", denied), "eventstore: read records: SQLSTATE 42501"},
		{"two wrapped errors", fmt.Errorf("%w: %w", errors.New("postgres: migration 0010 sealed_secrets up"), check),
			"postgres: migration 0010 sealed_secrets up: SQLSTATE 23514 constraint round_secrets_sealed_check"},
		{"sentinels kept", fmt.Errorf("%w: secret config: %w", eventstore.ErrCorrupt, sealing.ErrOpen),
			"eventstore: stored round data violates an export invariant: secret config: sealing: sealed secret failed authentication"},
		{"network error", fmt.Errorf("eventstore: read records: %w", opErr), "eventstore: read records: database driver error"},
		{"network timeout", fmt.Errorf("eventstore: read records: %w", timeout), "eventstore: read records: database driver error: timeout"},
		{"connect error", fmt.Errorf("eventstore: begin: %w", refusedConnectError(t)), "eventstore: begin: database driver error"},
		{"driver wrapper around a server error", fmt.Errorf("eventstore: read revealed secret: %w",
			&url.Error{Op: "prepare", URL: sentinelDSN, Err: &pgconn.PgError{Code: "42P01", Message: `relation "orvex.rounds" does not exist`}}),
			"eventstore: read revealed secret: SQLSTATE 42P01"},
		{"parent omits its child", opaqueError{fmt.Errorf("eventstore: read revealed secret: %w", denied)},
			"eventstore: read revealed secret: SQLSTATE 42501"},
		{"joined errors", errors.Join(errors.New("first"), denied), "first\nSQLSTATE 42501"},
		{"child text repeated by %v", fmt.Errorf("eventstore: read records: %w (%v)", denied, denied), //nolint:errorlint // K-12 regression: the child's text appears twice, once unwrapped
			"eventstore: read records: SQLSTATE 42501 (SQLSTATE 42501)"},
		{"child text repeated by indexed %w", fmt.Errorf("eventstore: read records: %[1]w; again: %[1]w", denied),
			"eventstore: read records: SQLSTATE 42501; again: SQLSTATE 42501"},
		{"parent text holding a child's text twice, unwrapped", opaqueError2{err: denied},
			"crashctl test: SQLSTATE 42501 then SQLSTATE 42501"},
		{"step d: raw DSN", errors.New("leak " + sentinelDSN), "leak xxxxx"},
		{"step d: password", errors.New("leak sentinel_pw_q1 here"), "leak xxxxx here"},
		{"one line", fmt.Errorf("a\nb:\n  %w", denied), "a b: SQLSTATE 42501"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describe(cfg, tt.err)
			want := oneLine(tt.want)
			if got != want {
				t.Errorf("describe = %q, want %q", got, want)
			}
			requireNoSentinel(t, "describe", got, sentinelDSN)
		})
	}
}

func TestConnectFailureMessage(t *testing.T) {
	const base = "cannot connect to the database named by ORVEX_DATABASE_URL"
	cfg, err := pgx.ParseConfig(sentinelDSN)
	if err != nil {
		t.Fatal(err)
	}
	_, refused := pgx.ConnectConfig(context.Background(), cfg)
	if refused == nil {
		t.Fatal("connect to 127.0.0.1:1 succeeded")
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"refused", refused, base},
		{"server error", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "28P01", Message: `password authentication failed for user "sentinel_user_q1"`}), base + ": SQLSTATE 28P01"},
		{"timeout", fmt.Errorf("connect: %w", &net.DNSError{Err: "i/o timeout", Name: "sentinel-host-q1.invalid", IsTimeout: true}), base + ": timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectFailure(tt.err); got != tt.want {
				t.Errorf("connectFailure = %q, want %q", got, tt.want)
			}
		})
	}
}

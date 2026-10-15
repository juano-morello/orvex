package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Migration is one numbered schema change: an up file and the down file
// that reverts it, with the SHA-256 checksums of their exact bytes.
type Migration struct {
	Version              int
	Name                 string
	Up, Down             string
	UpSHA256, DownSHA256 [32]byte
}

// Beginner starts a transaction with the server's default options;
// *pgx.Conn and *pgxpool.Pool implement it.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// MigrationState describes one known migration in a database.
type MigrationState struct {
	Version   int
	Name      string
	Applied   bool
	AppliedAt time.Time
	AppliedBy string
}

// MigrationLockKey is the pg_advisory_xact_lock key every migration step
// takes: the first 8 bytes of SHA-256("orvex/migrate") as a big-endian int64.
const MigrationLockKey int64 = 7766444933939532367

// Errors returned by the runner (section 8). Callers test them with
// errors.Is.
var (
	ErrMigrationFiles   = errors.New("postgres: invalid migration files")
	ErrTarget           = errors.New("postgres: migration target out of range")
	ErrWrongDirection   = errors.New("postgres: migration target is on the other side of the current version")
	ErrSchemaTooNew     = errors.New("postgres: database schema is newer than this build")
	ErrChecksumMismatch = errors.New("postgres: applied migration differs from this build")
	ErrSchemaState      = errors.New("postgres: migration bookkeeping is inconsistent")
)

var (
	fileName = regexp.MustCompile(`^([0-9]{4})_([a-z][a-z0-9_]*)\.(up|down)\.sql$`)
	// txLine matches a transaction statement on its own line. A PL/pgSQL
	// BEGIN without a semicolon does not match.
	txLine = regexp.MustCompile(`(?i)^\s*((begin|commit|rollback)\s*;|start\s+transaction\b)`)
)

// bookkeepingDDL creates the runner's bookkeeping. It runs after the
// advisory lock, so concurrent runners create it exactly once.
const bookkeepingDDL = `CREATE SCHEMA IF NOT EXISTS orvex_migrate;
CREATE TABLE IF NOT EXISTS orvex_migrate.schema_migrations (
    version     integer     NOT NULL,
    name        text        NOT NULL,
    up_sha256   bytea       NOT NULL,
    down_sha256 bytea       NOT NULL,
    applied_at  timestamptz NOT NULL DEFAULT now(),
    applied_by  text        NOT NULL,
    CONSTRAINT schema_migrations_pkey PRIMARY KEY (version),
    CONSTRAINT schema_migrations_version_check CHECK (version >= 1),
    CONSTRAINT schema_migrations_up_sha256_check CHECK (octet_length(up_sha256) = 32),
    CONSTRAINT schema_migrations_down_sha256_check CHECK (octet_length(down_sha256) = 32)
)`

// LoadMigrations reads the migrations at the root of fsys (decision P-4).
// Entries that are not regular .sql files are ignored. Every .sql file must
// be named NNNN_name.up.sql or NNNN_name.down.sql; versions are contiguous
// from 0001 and each has one name and both files; a file must not be empty,
// hold a transaction statement on its own line, use CONCURRENTLY or start a
// line with a psql meta-command. Any violation wraps ErrMigrationFiles.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMigrationFiles, err)
	}
	byVersion := map[int]*Migration{}
	seen := map[int][2]bool{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		file := e.Name()
		m := fileName.FindStringSubmatch(file)
		if m == nil {
			return nil, fmt.Errorf("%w: %s: name does not match NNNN_name.{up,down}.sql", ErrMigrationFiles, file)
		}
		version, _ := strconv.Atoi(m[1])
		if version == 0 {
			return nil, fmt.Errorf("%w: %s: version 0000", ErrMigrationFiles, file)
		}
		body, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrMigrationFiles, file, err)
		}
		if err := checkContent(body); err != nil {
			return nil, fmt.Errorf("%w: %s: %s", ErrMigrationFiles, file, err)
		}
		mig, ok := byVersion[version]
		if !ok {
			mig = &Migration{Version: version, Name: m[2]}
			byVersion[version] = mig
		} else if mig.Name != m[2] {
			return nil, fmt.Errorf("%w: %s: version %04d already named %q", ErrMigrationFiles, file, version, mig.Name)
		}
		have := seen[version]
		if m[3] == "up" {
			mig.Up, mig.UpSHA256, have[0] = string(body), sha256.Sum256(body), true
		} else {
			mig.Down, mig.DownSHA256, have[1] = string(body), sha256.Sum256(body), true
		}
		seen[version] = have
	}
	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("%w: %04d_%s: versions are not contiguous from 0001", ErrMigrationFiles, m.Version, m.Name)
		}
		have := seen[m.Version]
		if !have[0] {
			return nil, fmt.Errorf("%w: %04d_%s.up.sql: missing", ErrMigrationFiles, m.Version, m.Name)
		}
		if !have[1] {
			return nil, fmt.Errorf("%w: %04d_%s.down.sql: missing", ErrMigrationFiles, m.Version, m.Name)
		}
	}
	return out, nil
}

// checkContent applies the P-4 content rules to one file.
func checkContent(body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("empty file")
	}
	if bytes.Contains(bytes.ToUpper(body), []byte("CONCURRENTLY")) {
		return errors.New("uses CONCURRENTLY, which cannot run in a transaction")
	}
	for n, line := range strings.Split(string(body), "\n") {
		if txLine.MatchString(line) {
			return fmt.Errorf("line %d: transaction statement", n+1)
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), `\`) {
			return fmt.Errorf("line %d: psql meta-command", n+1)
		}
	}
	return nil
}

// MigrateUp applies migrations until the database is at version target and
// returns the migrations it applied, in order. Each step is one transaction
// under the migration advisory lock (decision P-4). On error it returns the
// steps already committed together with the error.
func MigrateUp(ctx context.Context, db Beginner, ms []Migration, target int, appliedBy string) (applied []Migration, err error) {
	if target < 0 || target > len(ms) {
		return nil, fmt.Errorf("%w: %d not in [0, %d]", ErrTarget, target, len(ms))
	}
	for {
		m, done, err := step(ctx, db, ms, target, true, appliedBy)
		if err != nil {
			return applied, err
		}
		if done {
			return applied, nil
		}
		applied = append(applied, m)
	}
}

// MigrateDown reverts migrations until the database is at version target
// and returns the migrations it reverted, newest first. On error it returns
// the steps already committed together with the error.
func MigrateDown(ctx context.Context, db Beginner, ms []Migration, target int) (reverted []Migration, err error) {
	if target < 0 || target > len(ms) {
		return nil, fmt.Errorf("%w: %d not in [0, %d]", ErrTarget, target, len(ms))
	}
	for {
		m, done, err := step(ctx, db, ms, target, false, "")
		if err != nil {
			return reverted, err
		}
		if done {
			return reverted, nil
		}
		reverted = append(reverted, m)
	}
}

// step runs one migration transaction. done reports that the database was
// already at target, in which case nothing changed.
func step(ctx context.Context, db Beginner, ms []Migration, target int, up bool, appliedBy string) (m Migration, done bool, err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return Migration{}, false, fmt.Errorf("postgres: migration: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", MigrationLockKey); err != nil {
		return Migration{}, false, fmt.Errorf("postgres: migration: lock: %w", err)
	}
	if _, err := tx.Exec(ctx, bookkeepingDDL); err != nil {
		return Migration{}, false, fmt.Errorf("postgres: migration: bookkeeping: %w", err)
	}
	states, err := readStates(ctx, tx, ms)
	if err != nil {
		return Migration{}, false, err
	}
	v := len(states)
	switch {
	case v == target:
		if err := tx.Commit(ctx); err != nil {
			return Migration{}, false, fmt.Errorf("postgres: migration: commit: %w", err)
		}
		return Migration{}, true, nil
	case up && target < v, !up && target > v:
		return Migration{}, false, fmt.Errorf("%w: at version %d, target %d", ErrWrongDirection, v, target)
	}
	dir := "down"
	if up {
		dir = "up"
		m = ms[v]
	} else {
		m = ms[v-1]
	}
	fail := func(err error) error {
		return fmt.Errorf("postgres: migration %04d %s %s: %w", m.Version, m.Name, dir, err)
	}
	if up {
		if _, err := tx.Exec(ctx, m.Up); err != nil {
			return Migration{}, false, fail(err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO orvex_migrate.schema_migrations (version, name, up_sha256, down_sha256, applied_by)
			 VALUES ($1, $2, $3, $4, $5)`,
			m.Version, m.Name, m.UpSHA256[:], m.DownSHA256[:], appliedBy); err != nil {
			return Migration{}, false, fail(err)
		}
	} else {
		if _, err := tx.Exec(ctx, m.Down); err != nil {
			return Migration{}, false, fail(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM orvex_migrate.schema_migrations WHERE version = $1`, m.Version); err != nil {
			return Migration{}, false, fail(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Migration{}, false, fail(err)
	}
	return m, false, nil
}

// readStates reads and validates the bookkeeping rows: versions exactly
// 1..v (ErrSchemaState), v within this build (ErrSchemaTooNew), and each
// row's name and checksums equal to the build's (ErrChecksumMismatch).
func readStates(ctx context.Context, tx pgx.Tx, ms []Migration) ([]MigrationState, error) {
	rows, err := tx.Query(ctx,
		`SELECT version, name, up_sha256, down_sha256, applied_at, applied_by
		 FROM orvex_migrate.schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("postgres: migration: read bookkeeping: %w", err)
	}
	type row struct {
		state    MigrationState
		up, down []byte
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.state.Version, &r.state.Name, &r.up, &r.down, &r.state.AppliedAt, &r.state.AppliedBy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: migration: read bookkeeping: %w", err)
		}
		r.state.Applied = true
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: migration: read bookkeeping: %w", err)
	}
	for i, r := range got {
		if r.state.Version != i+1 {
			return nil, fmt.Errorf("%w: applied versions are not 1..%d", ErrSchemaState, len(got))
		}
	}
	if len(got) > len(ms) {
		return nil, fmt.Errorf("%w: database at version %d, this build knows %d", ErrSchemaTooNew, len(got), len(ms))
	}
	states := make([]MigrationState, len(got))
	for i, r := range got {
		m := ms[i]
		if r.state.Name != m.Name || !bytes.Equal(r.up, m.UpSHA256[:]) || !bytes.Equal(r.down, m.DownSHA256[:]) {
			return nil, fmt.Errorf("%w: %04d %s", ErrChecksumMismatch, m.Version, m.Name)
		}
		states[i] = r.state
	}
	return states, nil
}

// MigrationStatus returns the state of every migration in ms. It takes no
// lock and reads in one REPEATABLE READ READ ONLY transaction. A database
// without bookkeeping is at version 0; otherwise the rows are validated as
// in a migration step.
func MigrationStatus(ctx context.Context, db Beginner, ms []Migration) ([]MigrationState, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: migration status: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
		return nil, fmt.Errorf("postgres: migration status: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT to_regclass('orvex_migrate.schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return nil, fmt.Errorf("postgres: migration status: %w", err)
	}
	var applied []MigrationState
	if exists {
		if applied, err = readStates(ctx, tx, ms); err != nil {
			return nil, err
		}
	}
	out := make([]MigrationState, len(ms))
	for i, m := range ms {
		if i < len(applied) {
			out[i] = applied[i]
			continue
		}
		out[i] = MigrationState{Version: m.Version, Name: m.Name}
	}
	return out, nil
}

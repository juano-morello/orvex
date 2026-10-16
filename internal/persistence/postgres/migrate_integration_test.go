//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/migrations"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

const goldenDir = "../../audit/verification/testdata/golden"

var ctx = context.Background()

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name+".jsonl")) //nolint:gosec // G304: fixed golden fixture under testdata
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func embedded(t *testing.T) []postgres.Migration {
	t.Helper()
	ms, err := postgres.LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	return ms
}

func load(t *testing.T, fsys fstest.MapFS) []postgres.Migration {
	t.Helper()
	ms, err := postgres.LoadMigrations(fsys)
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	return ms
}

// fingerprint excludes the runner's bookkeeping schema.
func fingerprint(t *testing.T, db pgtest.Querier) string {
	t.Helper()
	fp, err := pgtest.Fingerprint(ctx, db, "orvex_migrate")
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// schemaVersion returns the number of bookkeeping rows, or -1 if the
// bookkeeping table does not exist.
func schemaVersion(t *testing.T, db *pgxpool.Pool) int {
	t.Helper()
	var exists bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('orvex_migrate.schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		return -1
	}
	var n int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM orvex_migrate.schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func count(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func up(t *testing.T, db postgres.Beginner, ms []postgres.Migration, target int) []postgres.Migration {
	t.Helper()
	applied, err := postgres.MigrateUp(ctx, db, ms, target, "test")
	if err != nil {
		t.Fatalf("MigrateUp(%d): %v", target, err)
	}
	return applied
}

func down(t *testing.T, db postgres.Beginner, ms []postgres.Migration, target int) []postgres.Migration {
	t.Helper()
	reverted, err := postgres.MigrateDown(ctx, db, ms, target)
	if err != nil {
		t.Fatalf("MigrateDown(%d): %v", target, err)
	}
	return reverted
}

func versions(ms []postgres.Migration) []int {
	out := make([]int, len(ms))
	for i, m := range ms {
		out[i] = m.Version
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] { //nolint:gosec // G602: len(a) == len(b) is checked above
			return false
		}
	}
	return true
}

func TestMigrateUpDownClean(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
	ms := embedded(t)

	f0 := fingerprint(t, pool)
	applied := up(t, pool, ms, len(ms))
	if len(applied) != len(ms) {
		t.Fatalf("applied %d migrations, want %d", len(applied), len(ms))
	}
	f1 := fingerprint(t, pool)
	if f1 == f0 {
		t.Fatal("fingerprint unchanged by migrating up")
	}
	reverted := down(t, pool, ms, 0)
	if want := []int{9, 8, 7, 6, 5, 4, 3, 2, 1}; !equalInts(versions(reverted), want) {
		t.Errorf("reverted %v, want %v", versions(reverted), want)
	}
	if f2 := fingerprint(t, pool); f2 != f0 {
		t.Errorf("fingerprint after down differs from the pristine database:\n--- pristine\n%s\n--- after down\n%s", f0, f2)
	}
	if v := schemaVersion(t, pool); v != 0 {
		t.Errorf("schema_migrations has %d rows after down, want 0", v)
	}
	up(t, pool, ms, len(ms))
	if f3 := fingerprint(t, pool); f3 != f1 {
		t.Errorf("fingerprint after the second up differs from the first:\n--- first\n%s\n--- second\n%s", f1, f3)
	}
}

func TestMigrateEachStepReversible(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
	ms := embedded(t)
	fps := []string{fingerprint(t, pool)}
	for v := 1; v <= len(ms); v++ {
		up(t, pool, ms, v)
		fps = append(fps, fingerprint(t, pool))
		if fps[v] == fps[v-1] {
			t.Errorf("migration %04d changed nothing", v)
		}
		if got := versions(down(t, pool, ms, v-1)); !equalInts(got, []int{v}) {
			t.Errorf("down to %d reverted %v, want [%d]", v-1, got, v)
		}
		if fp := fingerprint(t, pool); fp != fps[v-1] {
			t.Errorf("down %04d: fingerprint differs from version %d:\n--- want\n%s\n--- got\n%s", v, v-1, fps[v-1], fp)
		}
		if got := versions(up(t, pool, ms, v)); !equalInts(got, []int{v}) {
			t.Errorf("up to %d applied %v, want [%d]", v, got, v)
		}
		if fp := fingerprint(t, pool); fp != fps[v] {
			t.Errorf("re-up %04d: fingerprint differs from the forward pass", v)
		}
	}
}

func TestMigrateUpIdempotent(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
	ms := embedded(t)
	up(t, pool, ms, len(ms))
	f := fingerprint(t, pool)
	if again := up(t, pool, ms, len(ms)); len(again) != 0 {
		t.Errorf("second up applied %v, want nothing", versions(again))
	}
	if reverted := down(t, pool, ms, len(ms)); len(reverted) != 0 {
		t.Errorf("down to the current version reverted %v, want nothing", versions(reverted))
	}
	if fingerprint(t, pool) != f {
		t.Error("fingerprint changed by a no-op up")
	}
}

func TestMigrateConcurrentRunners(t *testing.T) {
	t.Parallel()
	dsn := pgtest.EmptyDatabase(t)
	pool := pgtest.Pool(t, dsn)
	ms := embedded(t)

	const runners = 4
	conns := make([]*pgx.Conn, runners)
	for i := range conns {
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close(ctx) })
		conns[i] = c
	}
	results := make([][]postgres.Migration, runners)
	errs := make([]error, runners)
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = postgres.MigrateUp(ctx, conns[i], ms, len(ms), "runner")
		}()
	}
	wg.Wait()

	seen := map[int]int{}
	for i := range runners {
		if errs[i] != nil {
			t.Errorf("runner %d: %v", i, errs[i])
		}
		for _, m := range results[i] {
			seen[m.Version]++
		}
	}
	for _, m := range ms {
		if seen[m.Version] != 1 {
			t.Errorf("migration %04d applied %d times, want 1", m.Version, seen[m.Version])
		}
	}
	if len(seen) != len(ms) {
		t.Errorf("applied versions %v, want exactly 1..%d", seen, len(ms))
	}
	if v := schemaVersion(t, pool); v != len(ms) {
		t.Errorf("bookkeeping has %d rows, want %d", v, len(ms))
	}
}

// scratchSet is a two-migration set for runner tests that need to tamper
// with files.
func scratchSet() fstest.MapFS {
	return fstest.MapFS{
		"0001_first.up.sql":    {Data: []byte("CREATE TABLE public.runner_a (x int);\n")},
		"0001_first.down.sql":  {Data: []byte("DROP TABLE public.runner_a;\n")},
		"0002_second.up.sql":   {Data: []byte("CREATE TABLE public.runner_b (x int);\n")},
		"0002_second.down.sql": {Data: []byte("DROP TABLE public.runner_b;\n")},
	}
}

func TestMigrateChecksumMismatch(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"0001_first.up.sql", "0001_first.down.sql"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
			fsys := scratchSet()
			up(t, pool, load(t, fsys), 2)
			f := fingerprint(t, pool)

			data := append([]byte(nil), fsys[file].Data...)
			data[len(data)-2] = ' ' // one byte: the ';' before the final newline
			fsys[file] = &fstest.MapFile{Data: data}
			ms := load(t, fsys)

			if _, err := postgres.MigrateUp(ctx, pool, ms, 2, "test"); !errors.Is(err, postgres.ErrChecksumMismatch) {
				t.Errorf("MigrateUp err = %v, want ErrChecksumMismatch", err)
			}
			if _, err := postgres.MigrateDown(ctx, pool, ms, 0); !errors.Is(err, postgres.ErrChecksumMismatch) {
				t.Errorf("MigrateDown err = %v, want ErrChecksumMismatch", err)
			}
			if _, err := postgres.MigrationStatus(ctx, pool, ms); !errors.Is(err, postgres.ErrChecksumMismatch) {
				t.Errorf("MigrationStatus err = %v, want ErrChecksumMismatch", err)
			}
			if v := schemaVersion(t, pool); v != 2 {
				t.Errorf("version = %d, want 2", v)
			}
			if fingerprint(t, pool) != f {
				t.Error("fingerprint changed")
			}
		})
	}
}

func TestMigrateSchemaTooNew(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
	ms := load(t, scratchSet())
	up(t, pool, ms, 2)
	older := ms[:1]
	if _, err := postgres.MigrateUp(ctx, pool, older, 1, "test"); !errors.Is(err, postgres.ErrSchemaTooNew) {
		t.Errorf("MigrateUp err = %v, want ErrSchemaTooNew", err)
	}
	if _, err := postgres.MigrateDown(ctx, pool, older, 0); !errors.Is(err, postgres.ErrSchemaTooNew) {
		t.Errorf("MigrateDown err = %v, want ErrSchemaTooNew", err)
	}
	if _, err := postgres.MigrationStatus(ctx, pool, older); !errors.Is(err, postgres.ErrSchemaTooNew) {
		t.Errorf("MigrationStatus err = %v, want ErrSchemaTooNew", err)
	}
	if v := schemaVersion(t, pool); v != 2 {
		t.Errorf("version = %d, want 2", v)
	}
}

func TestMigrateWrongDirection(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
	ms := embedded(t)
	up(t, pool, ms, 3)
	if applied, err := postgres.MigrateUp(ctx, pool, ms, 2, "test"); !errors.Is(err, postgres.ErrWrongDirection) || len(applied) != 0 {
		t.Errorf("MigrateUp(2) = %v, %v; want nothing, ErrWrongDirection", versions(applied), err)
	}
	if reverted, err := postgres.MigrateDown(ctx, pool, ms, 5); !errors.Is(err, postgres.ErrWrongDirection) || len(reverted) != 0 {
		t.Errorf("MigrateDown(5) = %v, %v; want nothing, ErrWrongDirection", versions(reverted), err)
	}
	if v := schemaVersion(t, pool); v != 3 {
		t.Errorf("version = %d, want 3", v)
	}
}

func TestMigrateTargetRange(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
	ms := embedded(t)
	for _, target := range []int{-1, len(ms) + 1} {
		if _, err := postgres.MigrateUp(ctx, pool, ms, target, "test"); !errors.Is(err, postgres.ErrTarget) {
			t.Errorf("MigrateUp(%d) err = %v, want ErrTarget", target, err)
		}
		if _, err := postgres.MigrateDown(ctx, pool, ms, target); !errors.Is(err, postgres.ErrTarget) {
			t.Errorf("MigrateDown(%d) err = %v, want ErrTarget", target, err)
		}
	}
	if v := schemaVersion(t, pool); v != -1 {
		t.Errorf("bookkeeping exists (%d rows), want none", v)
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
	fsys := scratchSet()
	fsys["0002_second.up.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE public.runner_b (x int);\nSELECT 1/0;\n")}
	ms := load(t, fsys)

	applied, err := postgres.MigrateUp(ctx, pool, ms, 2, "test")
	if got := versions(applied); !equalInts(got, []int{1}) {
		t.Errorf("applied %v, want [1]", got)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22012" {
		t.Fatalf("err = %v, want a wrapped PgError 22012", err)
	}
	if v := schemaVersion(t, pool); v != 1 {
		t.Errorf("version = %d, want 1", v)
	}
	if n := count(t, pool, "SELECT count(*) FROM pg_class WHERE oid = to_regclass('public.runner_b')"); n != 0 {
		t.Error("table public.runner_b exists after the failed migration")
	}
}

func TestMigrateDownRefusesAuditData(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	ms := embedded(t)
	pgtest.LoadHistory(t, pool, readGolden(t, "worked-example"), nil)
	records := count(t, pool, "SELECT count(*) FROM orvex.chain_records")
	snapshots := count(t, pool, "SELECT count(*) FROM orvex.round_snapshots")

	// 0009 refuses first: round_admission_heads and round_secrets hold the
	// loaded round's rows.
	reverted, err := postgres.MigrateDown(ctx, pool, ms, 0)
	if got := versions(reverted); len(got) != 0 {
		t.Errorf("reverted %v, want nothing (0009 refuses)", got)
	}
	if code := postgres.SQLState(err); code != postgres.CodeDownRefused {
		t.Fatalf("err = %v, want SQLSTATE %s", err, postgres.CodeDownRefused)
	}
	if v := schemaVersion(t, pool); v != 9 {
		t.Errorf("version = %d, want 9", v)
	}
	if n := count(t, pool, "SELECT count(*) FROM orvex.chain_records"); n != records {
		t.Errorf("chain records = %d, want %d", n, records)
	}
	if n := count(t, pool, "SELECT count(*) FROM orvex.round_snapshots"); n != snapshots {
		t.Errorf("snapshots = %d, want %d", n, snapshots)
	}
	if got := versions(up(t, pool, ms, len(ms))); len(got) != 0 {
		t.Errorf("re-up applied %v, want nothing", got)
	}
}

// TestFingerprintStableAcrossColumnDropAndReAdd: re-adding dropped columns
// gives them new attnums; the fingerprint numbers live columns by position,
// so a down and re-up of a column migration fingerprints as before.
func TestFingerprintStableAcrossColumnDropAndReAdd(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
	ms := load(t, fstest.MapFS{
		"0001_base.up.sql":      {Data: []byte("CREATE TABLE public.fp_a (x int);\n")},
		"0001_base.down.sql":    {Data: []byte("DROP TABLE public.fp_a;\n")},
		"0002_columns.up.sql":   {Data: []byte("ALTER TABLE public.fp_a ADD COLUMN y int, ADD COLUMN z text;\n")},
		"0002_columns.down.sql": {Data: []byte("ALTER TABLE public.fp_a DROP COLUMN y, DROP COLUMN z;\n")},
	})
	up(t, pool, ms, 1)
	f1 := fingerprint(t, pool)
	up(t, pool, ms, 2)
	f2 := fingerprint(t, pool)
	if f2 == f1 {
		t.Fatal("fingerprint unchanged by adding columns")
	}
	down(t, pool, ms, 1)
	if fp := fingerprint(t, pool); fp != f1 {
		t.Errorf("fingerprint after dropping the columns differs:\n--- want\n%s\n--- got\n%s", f1, fp)
	}
	up(t, pool, ms, 2)
	if fp := fingerprint(t, pool); fp != f2 {
		t.Errorf("fingerprint after re-adding the columns differs:\n--- want\n%s\n--- got\n%s", f2, fp)
	}
}

func TestMigrateRequiresUTF8(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.CreateDatabase(t, "ENCODING 'SQL_ASCII' TEMPLATE template0"))
	ms := embedded(t)
	applied, err := postgres.MigrateUp(ctx, pool, ms, len(ms), "test")
	if len(applied) != 0 {
		t.Errorf("applied %v, want nothing", versions(applied))
	}
	if code := postgres.SQLState(err); code != postgres.CodeEncoding {
		t.Fatalf("err = %v, want SQLSTATE %s", err, postgres.CodeEncoding)
	}
	states, err := postgres.MigrationStatus(ctx, pool, ms)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.Applied {
			t.Errorf("%04d %s applied, want pending", s.Version, s.Name)
		}
	}
}

func TestMigrationStatus(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.EmptyDatabase(t))
	ms := embedded(t)

	states, err := postgres.MigrationStatus(ctx, pool, ms)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != len(ms) {
		t.Fatalf("got %d states, want %d", len(states), len(ms))
	}
	for i, s := range states {
		if s.Version != ms[i].Version || s.Name != ms[i].Name || s.Applied {
			t.Errorf("state %d = %+v, want %04d %s pending", i, s, ms[i].Version, ms[i].Name)
		}
	}
	if v := schemaVersion(t, pool); v != -1 {
		t.Error("MigrationStatus created bookkeeping")
	}

	if _, err := postgres.MigrateUp(ctx, pool, ms, 2, "status-test"); err != nil {
		t.Fatal(err)
	}
	states, err = postgres.MigrationStatus(ctx, pool, ms)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range states {
		wantApplied := i < 2
		if s.Version != ms[i].Version || s.Name != ms[i].Name || s.Applied != wantApplied {
			t.Errorf("state %d = %+v, want %04d %s applied=%v", i, s, ms[i].Version, ms[i].Name, wantApplied)
		}
		if wantApplied && (s.AppliedBy != "status-test" || s.AppliedAt.IsZero()) {
			t.Errorf("state %d = %+v, want AppliedBy status-test and a non-zero AppliedAt", i, s)
		}
		if !wantApplied && (s.AppliedBy != "" || !s.AppliedAt.IsZero()) {
			t.Errorf("pending state %d = %+v has applied fields", i, s)
		}
	}
}

func TestMigrationsCreateOnlyOrvexObjects(t *testing.T) {
	t.Parallel()
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	checks := []struct {
		what, sql string
	}{
		{"relations in public", `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public'`},
		{"functions in public", `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'public'`},
		{"types in public", `SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
			WHERE n.nspname = 'public'`},
		{"orvex tables and views without a comment", `SELECT count(*) FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'orvex' AND c.relkind IN ('r', 'p', 'v', 'm')
			  AND obj_description(c.oid, 'pg_class') IS NULL`},
		{"orvex schema without a comment", `SELECT count(*) FROM pg_namespace
			WHERE nspname = 'orvex' AND obj_description(oid, 'pg_namespace') IS NULL`},
		{"orvex functions without a pinned search_path or not plpgsql", `SELECT count(*) FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			JOIN pg_language l ON l.oid = p.prolang
			WHERE n.nspname = 'orvex'
			  AND (p.proconfig IS DISTINCT FROM '{search_path=pg_catalog}'::text[] OR l.lanname <> 'plpgsql')`},
	}
	for _, c := range checks {
		if n := count(t, pool, c.sql); n != 0 {
			t.Errorf("%s: %d, want 0", c.what, n)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'orvex' AND c.relkind IN ('r', 'v')`); n != 15 {
		t.Errorf("orvex holds %d tables and views, want 15", n)
	}
}

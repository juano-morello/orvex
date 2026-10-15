// Package pgtest is the PostgreSQL integration-test harness (ADR-006;
// decision P-7 of docs/specs/17380a6e_postgresql-schema-migrations.md). It
// starts one Testcontainers PostgreSQL container per test binary and hands
// each test its own database: empty, or cloned from a template migrated once
// to the newest version. It also fingerprints catalogs and loads LBT-9
// audit histories into the schema.
//
// Integration test files carry //go:build integration and declare
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
//
// Without a reachable Docker-compatible daemon Run fails; it never skips.
package pgtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/migrations"
)

// Image is the pinned PostgreSQL image (decision P-1). The Compose file in
// deployments/docker repeats it; TestComposeImageMatchesPinnedImage keeps the
// two equal.
const Image = "postgres:18.6-trixie@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336"

const (
	user          = "orvex"
	password      = "orvex"
	adminDatabase = "orvex"
	templateName  = "orvex_template"
	startTimeout  = 3 * time.Minute
	opTimeout     = time.Minute
)

// server is the container of the running test binary, set by Run.
var server struct {
	hostPort string
	admin    *pgxpool.Pool
}

var (
	dbCounter atomic.Int64

	templateOnce sync.Once
	templateErr  error
)

// Run starts one container for the test binary, runs m, terminates the
// container and returns the exit code. Use:
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
//
// Without a reachable Docker daemon it prints the reason and returns 1.
func Run(m *testing.M) int {
	return run(m, startContainer, os.Stderr)
}

// starter starts PostgreSQL and returns its host:port and a function that
// stops it.
type starter func(ctx context.Context) (hostPort string, stop func() error, err error)

// run is Run with the container start and stderr injectable, so that the
// failure path is testable without depending on the host's Docker state.
func run(m interface{ Run() int }, start starter, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	hostPort, stop, err := start(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pgtest: integration tests need a Docker-compatible daemon: %v\n", err)
		return 1
	}
	defer func() {
		if err := stop(); err != nil {
			_, _ = fmt.Fprintf(stderr, "pgtest: terminate container: %v\n", err)
		}
	}()
	admin, err := pgxpool.New(ctx, dsn(hostPort, adminDatabase))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pgtest: admin pool: %v\n", err)
		return 1
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "pgtest: admin connection: %v\n", err)
		return 1
	}
	server.hostPort, server.admin = hostPort, admin
	return m.Run()
}

// startContainer starts the pinned image with Testcontainers.
func startContainer(ctx context.Context) (string, func() error, error) {
	ctr, err := testcontainers.Run(ctx, Image,
		testcontainers.WithEnv(map[string]string{
			"POSTGRES_USER":     user,
			"POSTGRES_PASSWORD": password,
			"POSTGRES_DB":       adminDatabase,
		}),
		testcontainers.WithCmd("postgres", "-c", "fsync=off"),
		testcontainers.WithTmpfs(map[string]string{"/var/lib/postgresql": "rw"}),
		testcontainers.WithExposedPorts("5432/tcp"),
		testcontainers.WithWaitStrategy(wait.ForAll(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			wait.ForListeningPort("5432/tcp"),
		).WithStartupTimeoutDefault(startTimeout)),
	)
	stop := func() error { return testcontainers.TerminateContainer(ctr) }
	if err != nil {
		_ = stop()
		return "", nil, err
	}
	hostPort, err := ctr.PortEndpoint(ctx, "5432/tcp", "")
	if err != nil {
		_ = stop()
		return "", nil, fmt.Errorf("container endpoint: %w", err)
	}
	return hostPort, stop, nil
}

func dsn(hostPort, database string) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", user, password, hostPort, database)
}

func adminPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if server.admin == nil {
		t.Fatal("pgtest: Run was not called; declare func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }")
	}
	return server.admin
}

// CreateDatabase creates a fresh database with the given CREATE DATABASE
// options (for example "ENCODING 'SQL_ASCII' TEMPLATE template0"), drops it
// WITH (FORCE) at cleanup and returns its DSN.
func CreateDatabase(t testing.TB, options string) (dsn string) {
	t.Helper()
	admin := adminPool(t)
	name := fmt.Sprintf("orvex_test_%d", dbCounter.Add(1))
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" "+options); err != nil {
		t.Fatalf("pgtest: create database %s: %v", name, err)
	}
	t.Cleanup(func() { dropDatabase(t, name) })
	return databaseDSN(name)
}

func databaseDSN(name string) string { return dsn(server.hostPort, name) }

func dropDatabase(t testing.TB, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := server.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Errorf("pgtest: drop database %s: %v", name, err)
	}
}

// EmptyDatabase returns a fresh database, dropped WITH (FORCE) at cleanup.
func EmptyDatabase(t testing.TB) (dsn string) {
	t.Helper()
	return CreateDatabase(t, "")
}

// MigratedDatabase returns a clone of a template database migrated once to
// the newest version, dropped WITH (FORCE) at cleanup.
func MigratedDatabase(t testing.TB) (dsn string) {
	t.Helper()
	admin := adminPool(t)
	templateOnce.Do(func() { templateErr = createTemplate(admin) })
	if templateErr != nil {
		t.Fatalf("pgtest: migrated template: %v", templateErr)
	}
	return CreateDatabase(t, "TEMPLATE "+templateName)
}

func createTemplate(admin *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateName); err != nil {
		return err
	}
	ms, err := postgres.LoadMigrations(migrations.FS)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, databaseDSN(templateName))
	if err != nil {
		return err
	}
	_, err = postgres.MigrateUp(ctx, conn, ms, len(ms), "pgtest")
	if cerr := conn.Close(ctx); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, err = admin.Exec(ctx, "ALTER DATABASE "+templateName+" ALLOW_CONNECTIONS false")
	return err
}

// Pool opens a pool of at most 8 connections to dsn, closed at cleanup.
func Pool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgtest: pool config: %v", err)
	}
	cfg.MaxConns = 8
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgtest: pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// WaitUntilBlocked polls pg_stat_activity until backend pid waits on a
// lock. It fails the test after 10 seconds.
func WaitUntilBlocked(t testing.TB, db *pgxpool.Pool, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waitType *string
		err := db.QueryRow(context.Background(),
			"SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1", int64(pid)).Scan(&waitType)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("pgtest: pg_stat_activity: %v", err)
		}
		if waitType != nil && *waitType == "Lock" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pgtest: backend %d did not block on a lock within 10s", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

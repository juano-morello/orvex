package postgres

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/juano-morello/orvex/migrations"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// validSet returns a valid two-migration set.
func validSet() fstest.MapFS {
	return fstest.MapFS{
		"0001_first.up.sql":    file("CREATE TABLE a (x int);\n"),
		"0001_first.down.sql":  file("DROP TABLE a;\n"),
		"0002_second.up.sql":   file("CREATE TABLE b (x int);\n"),
		"0002_second.down.sql": file("DROP TABLE b;\n"),
	}
}

func with(base fstest.MapFS, edits map[string]*fstest.MapFile) fstest.MapFS {
	out := fstest.MapFS{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range edits {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func TestLoadMigrations(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		fsys := with(validSet(), map[string]*fstest.MapFile{
			"README.md": file("ignored"),
			"sub/x.sql": file("ignored: not at the root"),
		})
		ms, err := LoadMigrations(fsys)
		if err != nil {
			t.Fatalf("LoadMigrations: %v", err)
		}
		if len(ms) != 2 {
			t.Fatalf("got %d migrations, want 2", len(ms))
		}
		for i, want := range []string{"first", "second"} {
			m := ms[i]
			if m.Version != i+1 || m.Name != want {
				t.Errorf("ms[%d] = %04d %s, want %04d %s", i, m.Version, m.Name, i+1, want)
			}
			up := fsys[m.fileName("up")].Data
			down := fsys[m.fileName("down")].Data
			if m.Up != string(up) || m.Down != string(down) {
				t.Errorf("ms[%d]: file contents differ", i)
			}
			if m.UpSHA256 != sha256.Sum256(up) || m.DownSHA256 != sha256.Sum256(down) {
				t.Errorf("ms[%d]: checksums differ from sha256 of the bytes", i)
			}
		}
	})

	t.Run("plpgsql BEGIN allowed", func(t *testing.T) {
		fsys := with(validSet(), map[string]*fstest.MapFile{
			"0002_second.up.sql": file("DO $$\nBEGIN\n    PERFORM 1;\nEND\n$$;\n"),
		})
		if _, err := LoadMigrations(fsys); err != nil {
			t.Fatalf("LoadMigrations: %v", err)
		}
	})

	rejected := []struct {
		name  string
		edits map[string]*fstest.MapFile
	}{
		{"missing down", map[string]*fstest.MapFile{"0002_second.down.sql": nil}},
		{"missing up", map[string]*fstest.MapFile{"0002_second.up.sql": nil}},
		{"gap", map[string]*fstest.MapFile{
			"0002_second.up.sql": nil, "0002_second.down.sql": nil,
			"0003_third.up.sql": file("SELECT 1;"), "0003_third.down.sql": file("SELECT 1;"),
		}},
		{"does not start at 0001", map[string]*fstest.MapFile{
			"0001_first.up.sql": nil, "0001_first.down.sql": nil,
		}},
		{"version 0000", map[string]*fstest.MapFile{
			"0000_zero.up.sql": file("SELECT 1;"), "0000_zero.down.sql": file("SELECT 1;"),
		}},
		{"duplicate version with two names", map[string]*fstest.MapFile{
			"0002_other.up.sql": file("SELECT 1;"), "0002_other.down.sql": file("SELECT 1;"),
		}},
		{"bad name 1_a", map[string]*fstest.MapFile{"1_a.up.sql": file("SELECT 1;")}},
		{"bad name 0001_A", map[string]*fstest.MapFile{"0001_A.up.sql": file("SELECT 1;")}},
		{"bad name 0001-a", map[string]*fstest.MapFile{"0001-a.up.sql": file("SELECT 1;")}},
		{"bad name 0001_a.sql", map[string]*fstest.MapFile{"0001_a.sql": file("SELECT 1;")}},
		{"empty file", map[string]*fstest.MapFile{"0002_second.up.sql": file("")}},
		{"blank file", map[string]*fstest.MapFile{"0002_second.down.sql": file(" \n\t\n")}},
		{"BEGIN;", map[string]*fstest.MapFile{"0002_second.up.sql": file("BEGIN;\nCREATE TABLE b (x int);\n")}},
		{"commit;", map[string]*fstest.MapFile{"0002_second.up.sql": file("CREATE TABLE b (x int);\ncommit;\n")}},
		{"ROLLBACK;", map[string]*fstest.MapFile{"0002_second.down.sql": file("DROP TABLE b;\n  ROLLBACK;\n")}},
		{"START TRANSACTION", map[string]*fstest.MapFile{"0002_second.up.sql": file("START TRANSACTION;\nCREATE TABLE b (x int);\n")}},
		{"CONCURRENTLY", map[string]*fstest.MapFile{"0002_second.up.sql": file("CREATE INDEX CONCURRENTLY b_x ON b (x);\n")}},
		{"psql meta-command", map[string]*fstest.MapFile{"0002_second.up.sql": file("\\set ON_ERROR_STOP 1\nCREATE TABLE b (x int);\n")}},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadMigrations(with(validSet(), tt.edits))
			if !errors.Is(err, ErrMigrationFiles) {
				t.Fatalf("err = %v, want ErrMigrationFiles", err)
			}
		})
	}
}

// TestLoadMigrationsWrapsContentError keeps the content error in the chain
// next to ErrMigrationFiles, with the message unchanged.
func TestLoadMigrationsWrapsContentError(t *testing.T) {
	_, err := LoadMigrations(with(validSet(), map[string]*fstest.MapFile{"0002_second.up.sql": file("")}))
	if !errors.Is(err, ErrMigrationFiles) {
		t.Fatalf("LoadMigrations(empty file) = %v; want ErrMigrationFiles", err)
	}
	if want := "postgres: invalid migration files: 0002_second.up.sql: empty file"; err.Error() != want {
		t.Errorf("LoadMigrations(empty file) = %q; want %q", err, want)
	}
	var multi interface{ Unwrap() []error }
	if !errors.As(err, &multi) {
		t.Fatalf("LoadMigrations(empty file) = %v; want a multi-wrapped error", err)
	}
	if causes := multi.Unwrap(); len(causes) != 2 || causes[1].Error() != "empty file" {
		t.Errorf("LoadMigrations(empty file) wraps %v; want [ErrMigrationFiles, empty file]", causes)
	}
}

func (m Migration) fileName(dir string) string {
	return fmt.Sprintf("%04d_%s.%s.sql", m.Version, m.Name, dir)
}

var header = regexp.MustCompile(`^-- ([0-9]{4}) ([a-z][a-z0-9_]*): .+ \(LBT-[0-9]+ spec [A-Z]-[0-9]+[^)]*\)$`)

func TestEmbeddedMigrations(t *testing.T) {
	ms, err := LoadMigrations(migrations.FS)
	if err != nil {
		t.Fatalf("LoadMigrations(migrations.FS): %v", err)
	}
	want := []string{"foundation", "rounds_admissions", "chain_execution", "snapshots", "ledger", "outbox", "admission_window", "execution_ledger", "close_bound", "sealed_secrets"}
	if len(ms) < len(want) {
		t.Fatalf("got %d migrations, want at least %d", len(ms), len(want))
	}
	for i, m := range ms {
		if m.Version != i+1 {
			t.Errorf("ms[%d].Version = %d, want %d", i, m.Version, i+1)
		}
		if i < len(want) && m.Name != want[i] {
			t.Errorf("ms[%d].Name = %q, want %q", i, m.Name, want[i])
		}
		for _, body := range []string{m.Up, m.Down} {
			first, _, _ := strings.Cut(body, "\n")
			h := header.FindStringSubmatch(first)
			if h == nil || h[1] != fmt.Sprintf("%04d", m.Version) || h[2] != m.Name {
				t.Errorf("%04d %s: first line %q is not \"-- %04d %s: <purpose> (LBT-n spec X-n)\"", m.Version, m.Name, first, m.Version, m.Name)
			}
		}
	}
	// Every embedded file belongs to a migration.
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2*len(ms) {
		t.Errorf("migrations.FS holds %d files, want %d", len(entries), 2*len(ms))
	}
}

func TestMigrationLockKey(t *testing.T) {
	sum := sha256.Sum256([]byte("orvex/migrate"))
	if want := int64(binary.BigEndian.Uint64(sum[:8])); MigrationLockKey != want { //nolint:gosec // G115: deliberate two's-complement reinterpretation, the MigrationLockKey derivation
		t.Errorf("MigrationLockKey = %d, want %d", MigrationLockKey, want)
	}
}

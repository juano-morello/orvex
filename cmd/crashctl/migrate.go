package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/migrations"
)

var canonicalTarget = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// runMigrate implements crashctl migrate up|down|status (LBT-10 spec P-6).
// Exit codes: 0 done, 1 a migration failed, 2 usage, 3 unsupported schema
// state, 4 I/O.
func runMigrate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	}
	sub := args[0]
	switch sub {
	case "up", "down", "status":
	default:
		_, _ = fmt.Fprintf(stderr, "%s: migrate: unknown subcommand %q\n%s", name, sub, usage)
		return exitUsage
	}
	prefix := name + ": migrate " + sub + ": "

	ms, err := postgres.LoadMigrations(migrations.FS)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, prefix+oneLine(err.Error()))
		return exitFailed
	}

	fs := flag.NewFlagSet(name+" migrate "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, usage) }
	var to *string
	if sub != "status" {
		to = fs.String("to", "", "target schema `version` N")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "%sunexpected arguments: %v\n", prefix, fs.Args())
		return exitUsage
	}
	target := len(ms)
	if to != nil {
		toSet := false
		fs.Visit(func(f *flag.Flag) { toSet = toSet || f.Name == "to" })
		switch {
		case toSet:
			n, err := strconv.Atoi(*to)
			if !canonicalTarget.MatchString(*to) || err != nil || n > len(ms) {
				_, _ = fmt.Fprintf(stderr, "%s-to must be a version from 0 to %d\n", prefix, len(ms))
				return exitUsage
			}
			target = n
		case sub == "down":
			_, _ = fmt.Fprintf(stderr, "%s-to N is required\n", prefix)
			return exitUsage
		}
	}

	cfg, err := databaseConfig()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, prefix+err.Error())
		return exitUsage
	}
	// fail prints one stderr line; the password never appears in it.
	fail := func(err error) {
		_, _ = fmt.Fprintln(stderr, prefix+mask(cfg, err))
	}

	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		fail(err)
		return exitIO
	}
	defer func() { _ = conn.Close(ctx) }()

	var out strings.Builder
	var version int
	switch sub {
	case "up":
		applied, merr := postgres.MigrateUp(ctx, conn, ms, target, buildinfo.Version)
		for _, m := range applied {
			fmt.Fprintf(&out, "applied %04d %s\n", m.Version, m.Name)
		}
		err, version = merr, target
	case "down":
		reverted, merr := postgres.MigrateDown(ctx, conn, ms, target)
		for _, m := range reverted {
			fmt.Fprintf(&out, "reverted %04d %s\n", m.Version, m.Name)
		}
		err, version = merr, target
	case "status":
		var states []postgres.MigrationState
		states, err = postgres.MigrationStatus(ctx, conn, ms)
		for _, s := range states {
			if s.Applied {
				version = s.Version
				fmt.Fprintf(&out, "%04d %s applied %s by %s\n", s.Version, s.Name,
					s.AppliedAt.UTC().Format(time.RFC3339), s.AppliedBy)
			} else {
				fmt.Fprintf(&out, "%04d %s pending\n", s.Version, s.Name)
			}
		}
	}
	if err == nil {
		fmt.Fprintf(&out, "schema version %d\n", version)
	}
	if _, werr := io.WriteString(stdout, out.String()); werr != nil {
		// One stderr line (P-6), naming the runner error too if there was one.
		if err != nil {
			fail(fmt.Errorf("%w; write stdout: %w", err, werr))
		} else {
			fail(fmt.Errorf("write stdout: %w", werr))
		}
		return exitIO
	}
	if err != nil {
		fail(err)
		return migrateExitCode(err)
	}
	return exitOK
}

// migrateExitCode maps a runner error to the P-6 exit code.
func migrateExitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, postgres.ErrTarget), errors.Is(err, postgres.ErrWrongDirection):
		return exitUsage
	case errors.Is(err, postgres.ErrSchemaTooNew), errors.Is(err, postgres.ErrChecksumMismatch),
		errors.Is(err, postgres.ErrSchemaState):
		return exitUnsupported
	default:
		return exitFailed
	}
}

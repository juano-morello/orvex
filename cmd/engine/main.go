// Command engine is the Orvex deterministic game execution service
// (ADR-007). It drives crash/v1 rounds without a human through
// internal/runtime/lifecycle: it creates, starts, closes and settles them,
// and adopts the rounds an earlier engine left unsettled. It serves the
// engine API to the gateway on ORVEX_ENGINE_ADDR and prints
// "ready <address>" once it listens and reaches the database. SIGINT and
// SIGTERM stop it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/sealing"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
	"github.com/juano-morello/orvex/internal/runtime/lifecycle"
	"github.com/juano-morello/orvex/internal/runtime/sessions"
	"github.com/juano-morello/orvex/internal/transport/engineapi"
)

const name = "engine"

const usage = `usage: engine [-version]
Environment:
  ORVEX_DATABASE_URL         the database (a PostgreSQL URL or key=value connection string)
  ORVEX_SECRET_KEYRING_FILE  the round secret keyring (LBT-21)
  ORVEX_ENGINE_ADDR          the engine API loopback listen address, such as 127.0.0.1:7001
  ORVEX_GATEWAY_TOKEN_FILE   the file holding the token the gateway presents
  ORVEX_BETTING_WINDOW       time from a round's creation to its start, a Go duration (default 10s)
  ORVEX_CREATE_ROUNDS        create rounds when none is unsettled (default true); false only drives the unsettled ones
`

// Exit codes.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// serveFunc serves the engine API on ln until ctx ends, admitting the
// gateway's commands with seq and accepting only callers that present
// gatewayToken. After ctx ends it returns nil or ctx's error.
type serveFunc func(ctx context.Context, ln net.Listener, pool *pgxpool.Pool, seq *admission.Sequencer, gatewayToken string) error

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr, engineapi.Serve)
	stop()
	os.Exit(code)
}

// run is the testable entry point: it reads the environment through
// lookup and serves the engine API with serve.
func run(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer, serve serveFunc) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, usage) }
	showVersion := fs.Bool("version", false, "print the build version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "%s: unexpected arguments: %v\n", name, fs.Args())
		return exitUsage
	}
	if *showVersion {
		if _, err := fmt.Fprintln(stdout, buildinfo.String(name)); err != nil {
			return exitFailed
		}
		return exitOK
	}
	fail := func(code int, msg string) int {
		_, _ = fmt.Fprintf(stderr, "%s: %s\n", name, msg)
		return code
	}

	cfg, err := loadConfig(lookup)
	if err != nil {
		return fail(exitUsage, err.Error())
	}
	keyring, err := sealing.LoadKeyringFile(cfg.keyringFile)
	if err != nil {
		reason := "unreadable"
		if ke, ok := errors.AsType[*sealing.KeyringError](err); ok {
			reason = ke.Reason
		}
		return fail(exitUsage, keyringFileEnv+": "+reason)
	}
	token, err := sessions.LoadServiceTokenFile(cfg.tokenFile)
	if err != nil {
		return fail(exitUsage, tokenFileEnv+": "+err.Error())
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg.pool)
	if err != nil {
		return fail(exitFailed, "cannot connect to the database named by "+databaseURLEnv+": "+lifecycle.Describe(err))
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fail(exitFailed, "cannot connect to the database named by "+databaseURLEnv+": "+lifecycle.Describe(err))
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	seq, sched, err := wire(pool, keyring, cfg, log)
	if err != nil {
		return fail(exitFailed, err.Error())
	}

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fail(exitFailed, "listen on "+addrEnv+": "+err.Error())
	}
	defer func() { _ = ln.Close() }()
	if _, err := fmt.Fprintf(stdout, "ready %s\n", ln.Addr()); err != nil {
		return exitFailed
	}
	log.Info("engine ready", "addr", ln.Addr().String(),
		"betting_window", cfg.bettingWindow.String(), "create_rounds", cfg.createRounds)
	return runAll(ctx, log,
		component{"engine API", func(ctx context.Context) error { return serve(ctx, ln, pool, seq, token) }},
		component{"scheduler", sched.Run},
	)
}

// wire builds the crash/v1 sequencer, executor and scheduler over pool.
// Its errors are bugs and carry no database text.
func wire(pool *pgxpool.Pool, keyring *sealing.Keyring, cfg config, log *slog.Logger) (*admission.Sequencer, *lifecycle.Scheduler, error) {
	reg, err := game.NewRegistry(v1.Adapter())
	if err != nil {
		return nil, nil, err
	}
	seq, err := admission.New(pool, reg, admission.DefaultConfig(), admission.CrashV1Policy())
	if err != nil {
		return nil, nil, err
	}
	xcfg := execution.DefaultConfig()
	xcfg.Keyring = keyring
	x, err := execution.New(pool, reg, xcfg, execution.CrashV1Policy())
	if err != nil {
		return nil, nil, err
	}
	sched, err := lifecycle.New(pool, x, seq, lifecycle.Config{
		BettingWindow: cfg.bettingWindow, CreateRounds: cfg.createRounds, Logger: log,
	})
	if err != nil {
		return nil, nil, err
	}
	return seq, sched, nil
}

// component is one of the engine's long-running parts.
type component struct {
	name string
	run  func(context.Context) error
}

// runAll runs parts until ctx ends or one of them returns, then stops the
// others and waits for them. A part that returns before ctx ends, or with
// an error other than ctx's, fails the engine.
func runAll(parent context.Context, log *slog.Logger, parts ...component) int {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type result struct {
		name string
		err  error
	}
	done := make(chan result, len(parts))
	for _, p := range parts {
		go func() { done <- result{p.name, p.run(ctx)} }()
	}
	code := exitOK
	for range parts {
		r := <-done
		stopping := ctx.Err() != nil
		cancel()
		switch {
		case r.err != nil && (!stopping || !errors.Is(r.err, context.Canceled)):
			log.Error("engine component failed", "component", r.name, "error", lifecycle.Describe(r.err))
			code = exitFailed
		case !stopping:
			log.Error("engine component stopped", "component", r.name)
			code = exitFailed
		}
	}
	log.Info("engine stopped")
	return code
}

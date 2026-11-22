// Command gateway is the Orvex client-facing gateway (ADR-007). It
// authenticates players by bearer token, submits their commands to the
// engine over gRPC and relays round records as Server-Sent Events
// (internal/transport/httpapi).
//
// Configuration comes from the environment:
//
//	ORVEX_GATEWAY_ADDR        listen address, default 127.0.0.1:8080
//	ORVEX_ENGINE_ADDR         engine gRPC address, default 127.0.0.1:7001;
//	                          a loopback host, since the connection is plaintext
//	ORVEX_GATEWAY_TOKEN_FILE  the shared gateway-to-engine service token
//	ORVEX_PLAYERS_FILE        JSON object mapping bearer tokens to player IDs
//
// Once listening it prints "ready <address>" on stdout. SIGINT or SIGTERM
// stops it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/runtime/sessions"
	"github.com/juano-morello/orvex/internal/transport/engineapi"
	"github.com/juano-morello/orvex/internal/transport/httpapi"
)

const name = "gateway"

// Defaults bind to loopback only.
const (
	defaultAddr       = "127.0.0.1:8080"
	defaultEngineAddr = "127.0.0.1:7001"
	shutdownTimeout   = 10 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the testable entry point. It serves until ctx is done. Exit
// codes: 0 stopped, help or version; 1 serving or write failure; 2 usage
// or configuration error.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "%s: unexpected arguments: %v\n", name, fs.Args())
		return 2
	}
	if *showVersion {
		if _, err := fmt.Fprintln(stdout, buildinfo.String(name)); err != nil {
			return 1
		}
		return 0
	}
	fail := func(code int, format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, name+": "+format+"\n", a...)
		return code
	}

	env := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	tokenFile, playersFile := getenv("ORVEX_GATEWAY_TOKEN_FILE"), getenv("ORVEX_PLAYERS_FILE")
	if tokenFile == "" || playersFile == "" {
		return fail(2, "set ORVEX_GATEWAY_TOKEN_FILE and ORVEX_PLAYERS_FILE")
	}
	token, err := sessions.LoadServiceTokenFile(tokenFile)
	if err != nil {
		return fail(2, "ORVEX_GATEWAY_TOKEN_FILE: %v", err)
	}
	players, err := sessions.LoadPlayersFile(playersFile)
	if err != nil {
		return fail(2, "ORVEX_PLAYERS_FILE: %v", err)
	}
	engine, err := engineapi.Dial(env("ORVEX_ENGINE_ADDR", defaultEngineAddr), token)
	if err != nil {
		return fail(2, "ORVEX_ENGINE_ADDR: %v", err)
	}
	defer func() { _ = engine.Close() }()

	ln, err := net.Listen("tcp", env("ORVEX_GATEWAY_ADDR", defaultAddr))
	if err != nil {
		return fail(1, "listen: %v", err)
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	api := httpapi.New(engine, players, log)
	srv := &http.Server{
		Handler:           api,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	srv.RegisterOnShutdown(api.CloseStreams)
	if _, err := fmt.Fprintf(stdout, "ready %s\n", ln.Addr()); err != nil {
		_ = ln.Close()
		return 1
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return fail(1, "serve: %v", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	<-served
	return 0
}

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/runtime/grant"
)

const grantUsage = "usage: crashctl ledger grant GRANT_KEY PLAYER_ID CREDITS\n"

// rerunSafe ends the stderr line of every database failure: a grant is
// stored whole or not at all, and a rerun with the same grant key stores it
// at most once.
const rerunSafe = "; rerunning the same command is safe"

// runLedger implements crashctl ledger grant.
func runLedger(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	}
	if args[0] != "grant" {
		_, _ = fmt.Fprintf(stderr, "%s: ledger: unknown subcommand %q\n%s", name, args[0], usage)
		return exitUsage
	}
	return runGrant(args[1:], stdout, stderr)
}

// runGrant implements crashctl ledger grant GRANT_KEY PLAYER_ID CREDITS:
// it credits CREDITS from the global faucet account to PLAYER_ID's account
// in one ledger transaction whose ID the grant key names (package grant).
// Exit codes: 0 the grant is stored (applied now or existing), 1 the stored
// transaction with the grant's ID is not a grant, 2 usage, 4 any database
// error, 7 the grant key names a grant to another player or of other
// credits. Operands are validated before connecting. Database errors print
// under the K-12 rule: no part of ORVEX_DATABASE_URL ever appears in the
// output.
func runGrant(args []string, stdout, stderr io.Writer) int {
	prefix := name + ": ledger grant: "

	flags := flag.NewFlagSet(name+" ledger grant", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = fmt.Fprint(stderr, grantUsage) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if flags.NArg() != 3 {
		_, _ = fmt.Fprintf(stderr, "%swant GRANT_KEY, PLAYER_ID and CREDITS, got %d arguments\n%s", prefix, flags.NArg(), grantUsage)
		return exitUsage
	}
	g, err := grant.Parse(flags.Arg(0), flags.Arg(1), flags.Arg(2))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, prefix+err.Error())
		return exitUsage
	}

	cfg, err := databaseConfig()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, prefix+err.Error())
		return exitUsage
	}
	grantConnConfig(cfg)

	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, prefix+redact(cfg, connectFailure(err))+rerunSafe)
		return exitIO
	}
	res, err := grant.Apply(ctx, conn, g)
	_ = conn.Close(ctx)
	var reused *grant.KeyReusedError
	switch {
	case errors.As(err, &reused):
		_, _ = fmt.Fprintln(stderr, prefix+redact(cfg, fmt.Sprintf(
			"grant key %s was used for player %q, credits %d; choose a new grant key for another grant",
			reused.Key, reused.Player, reused.Credits)))
		return exitGrantKeyReused
	case errors.Is(err, grant.ErrCorrupt):
		_, _ = fmt.Fprintln(stderr, prefix+describe(cfg, err))
		return exitFailed
	case err != nil:
		_, _ = fmt.Fprintln(stderr, prefix+describe(cfg, err)+rerunSafe)
		return exitIO
	}

	if _, err := stdout.Write(grantReport(g, res)); err != nil {
		_, _ = fmt.Fprintln(stderr, prefix+redact(cfg, "write report: "+err.Error()+"; the grant is "+res.Status.String()))
		return exitIO
	}
	return exitOK
}

// grantConnConfig names the session crashctl unless the DSN sets
// application_name.
func grantConnConfig(cfg *pgx.ConnConfig) {
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.RuntimeParams["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = name
	}
}

// grantReport formats the success report. It is the same on every rerun
// except for status.
func grantReport(g grant.Grant, res grant.Result) []byte {
	var b bytes.Buffer
	_, _ = fmt.Fprintf(&b, "grant %s\n", g.Key())
	_, _ = fmt.Fprintf(&b, "transaction %s\n", canonjson.UUID(res.TransactionID))
	_, _ = fmt.Fprintf(&b, "player %s\n", g.Player())
	_, _ = fmt.Fprintf(&b, "account %s\n", canonjson.UUID(res.AccountID))
	_, _ = fmt.Fprintf(&b, "credits %d\n", g.Credits())
	_, _ = fmt.Fprintf(&b, "status %s\n", res.Status)
	b.WriteString("ok\n")
	return b.Bytes()
}

// Command crashctl is the Orvex replay/audit and operations CLI (ADR-007).
// It verifies exported audit histories: crashctl audit verify|replay|chain
// (ADR-001, ADR-006; decision A-16 and section 7 of
// docs/specs/8bebb70f_canonical-codec-audit-cli.md), exports a round's
// history from PostgreSQL: crashctl audit export (LBT-19,
// docs/specs/73dbe98d_audit-export-cli.md), and migrates the PostgreSQL
// schema: crashctl migrate up|down|status (ADR-005; decision P-6 of
// docs/specs/17380a6e_postgresql-schema-migrations.md).
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	crashv1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

const name = "crashctl"

const usage = `usage: crashctl [-version]
       crashctl audit verify [-expect-head HEX] FILE
       crashctl audit replay [-expect-head HEX] FILE
       crashctl audit chain  [-expect-head HEX] FILE
       crashctl audit export [-unsettled] [-format NAME] ROUND_ID FILE
       crashctl migrate up [-to N]
       crashctl migrate down -to N
       crashctl migrate status
FILE "-" reads standard input (export: writes standard output).
ORVEX_DATABASE_URL names the database for migrate and export (a PostgreSQL URL or key=value connection string).
`

// Exit codes (spec section 7; 5 and 6: LBT-19 X-9).
const (
	exitOK          = 0
	exitFailed      = 1
	exitUsage       = 2
	exitUnsupported = 3
	exitIO          = 4
	exitNotFound    = 5
	exitNotSettled  = 6
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is the testable entry point. Exit codes: 0 success or help, 1
// verification failed (or -version could not write), 2 usage error, 3
// unsupported artifact, 4 I/O error; crashctl audit export adds 5 round not
// found and 6 round not settled.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	switch {
	case *showVersion && fs.NArg() == 0:
		if _, err := fmt.Fprintln(stdout, buildinfo.String(name)); err != nil {
			return exitFailed
		}
		return exitOK
	case fs.NArg() == 0:
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	case !*showVersion && fs.Arg(0) == "audit":
		return runAudit(fs.Args()[1:], stdin, stdout, stderr)
	case !*showVersion && fs.Arg(0) == "migrate":
		return runMigrate(fs.Args()[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "%s: unexpected arguments: %v\n", name, fs.Args())
		return exitUsage
	}
}

func runAudit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	}
	sub := args[0]
	if sub == "export" {
		return runExport(args[1:], stdout, stderr)
	}
	var mode verification.Mode
	switch sub {
	case "verify":
		mode = verification.ModeVerify
	case "replay":
		mode = verification.ModeReplay
	case "chain":
		mode = verification.ModeChain
	default:
		_, _ = fmt.Fprintf(stderr, "%s: audit: unknown subcommand %q\n%s", name, sub, usage)
		return exitUsage
	}
	prefix := name + ": audit " + sub + ": "

	fs := flag.NewFlagSet(name+" audit "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "usage: %s audit %s [-expect-head HEX] FILE\n", name, sub)
		fs.PrintDefaults()
	}
	expectHead := fs.String("expect-head", "", "fail unless the history head equals this `HEX` value (64 lowercase hex characters)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	opt := verification.Options{Mode: mode}
	headSet := false
	fs.Visit(func(f *flag.Flag) { headSet = headSet || f.Name == "expect-head" })
	if headSet {
		b, err := canonjson.ParseHex(canonjson.String(*expectHead), len(chain.Hash{}))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s-expect-head must be 64 lowercase hex characters\n", prefix)
			return exitUsage
		}
		var h chain.Hash
		copy(h[:], b)
		opt.ExpectHead = &h
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintf(stderr, "%swant exactly one FILE, got %d arguments\n", prefix, fs.NArg())
		fs.Usage()
		return exitUsage
	}

	in := stdin
	if path := fs.Arg(0); path != "-" {
		f, err := os.Open(path)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s%v\n", prefix, err)
			return exitIO
		}
		defer func() { _ = f.Close() }()
		in = f
	}

	if mode != verification.ModeChain {
		reg, err := newRegistry()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s%v\n", prefix, err)
			return exitFailed
		}
		opt.Registry = reg
	}

	rep, err := verification.Verify(in, opt)
	if err != nil {
		var f *verification.Failure
		if !errors.As(err, &f) {
			_, _ = fmt.Fprintf(stderr, "%s%v\n", prefix, err)
			return exitIO
		}
		_, _ = fmt.Fprintf(stderr, "%s%v\n", prefix, f)
		if f.Unsupported {
			return exitUnsupported
		}
		return exitFailed
	}

	if _, err := stdout.Write(report(rep, mode)); err != nil {
		_, _ = fmt.Fprintf(stderr, "%swrite report: %v\n", prefix, err)
		return exitIO
	}
	return exitOK
}

// newRegistry returns the rules versions crashctl replays, shared by audit
// and export.
func newRegistry() (*game.Registry, error) {
	return game.NewRegistry(crashv1.Adapter())
}

// settlementPolicies returns the execution policies crashctl audit export
// asks whether a round is settled (LBT-19 X-7), one per rules version in
// newRegistry. Historical policies are added with their adapters.
func settlementPolicies() []execution.Policy {
	return []execution.Policy{execution.CrashV1Policy()}
}

// report formats the success report of spec section 7.
func report(rep verification.Report, mode verification.Mode) []byte {
	var b bytes.Buffer
	_, _ = fmt.Fprintf(&b, "verifier %s\n", buildinfo.String(name))
	_, _ = fmt.Fprintf(&b, "format %s chain %s\n", rep.Format, rep.Chain)
	_, _ = fmt.Fprintf(&b, "round %s\n", canonjson.UUID(rep.RoundID))
	d := rep.Descriptor
	if mode != verification.ModeChain {
		_, _ = fmt.Fprintf(&b, "rules %s %d schema %d state %d\n", d.Game, d.RulesVersion, d.SchemaVersion, d.StateVersion)
	}
	_, _ = fmt.Fprintf(&b, "chain ok records %d head %s\n", rep.Records, rep.Head)
	if mode != verification.ModeChain {
		_, _ = fmt.Fprintf(&b, "fairness ok\n")
	}
	if mode == verification.ModeVerify {
		_, _ = fmt.Fprintf(&b, "decision-replay ok commands %d accepted %d rejected %d events %d\n",
			rep.Commands, rep.Accepted, rep.Rejected, rep.Events)
	}
	for _, cp := range rep.Checkpoints {
		_, _ = fmt.Fprintf(&b, "state-replay ok sequence %d state %s\n", cp.Sequence, cp.StateHash)
	}
	b.WriteString("ok\n")
	return b.Bytes()
}

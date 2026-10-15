package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// settledVerdict is the result of the settlement check (LBT-19 X-7).
type settledVerdict int

const (
	settledYes settledVerdict = iota
	settledNo
	settledUnknown
)

func (v settledVerdict) String() string {
	switch v {
	case settledYes:
		return "yes"
	case settledNo:
		return "no"
	default:
		return "unknown"
	}
}

// exporter writes one round's history in one format.
type exporter func(ctx context.Context, db eventstore.TxBeginner, roundID [16]byte, w io.Writer) (eventstore.Export, error)

// exporters maps -format values to writers (X-10); LBT-18 adds v2 here.
var exporters = map[string]exporter{verification.Format: eventstore.ExportHistory}

// historyFormatName matches a well-formed history format name: one this
// build does not write is unsupported (exit 3), anything else a usage error.
var historyFormatName = regexp.MustCompile(`^orvex/audit-history/v[1-9][0-9]*$`)

// Errors that classify export failures for exportExitCode.
var (
	errOutputExists      = errors.New("crashctl: output file exists")
	errFormatUnsupported = errors.New("crashctl: history format unsupported")
	errNotSettled        = errors.New("crashctl: round not settled")
	errNoPolicy          = errors.New("crashctl: no settlement policy")
	errSettlementCheck   = errors.New("crashctl: settlement check failed")
)

// runExport implements crashctl audit export [-unsettled] [-format NAME]
// ROUND_ID FILE (LBT-19, docs/specs/73dbe98d_audit-export-cli.md section 5).
// Exit codes: 0 written, 1 export or settlement check failed, 2 usage, 3
// unsupported, 4 I/O (every database error), 5 round not found, 6 round not
// settled.
func runExport(args []string, stdout, stderr io.Writer) int {
	prefix := name + ": audit export: "

	flags := flag.NewFlagSet(name+" audit export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "usage: %s audit export [-unsettled] [-format NAME] ROUND_ID FILE\n", name)
		flags.PrintDefaults()
	}
	unsettled := flags.Bool("unsettled", false, "write the history even if the round is not settled at its newest snapshot (the file then discloses its secret config)")
	format := flags.String("format", verification.Format, "history format `NAME` to write")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if flags.NArg() != 2 {
		_, _ = fmt.Fprintf(stderr, "%swant ROUND_ID and FILE, got %d arguments\n", prefix, flags.NArg())
		flags.Usage()
		return exitUsage
	}
	roundID, err := canonjson.ParseUUID(canonjson.String(flags.Arg(0)))
	if err != nil || roundID == [16]byte{} {
		_, _ = fmt.Fprintf(stderr, "%sROUND_ID must be a canonical lowercase UUID other than the nil UUID\n", prefix)
		return exitUsage
	}
	round := string(canonjson.UUID(roundID))
	path := flags.Arg(1)

	export, ok := exporters[*format]
	if !ok {
		if historyFormatName.MatchString(*format) {
			_, _ = fmt.Fprintf(stderr, "%sformat %s unsupported: this build writes %s\n", prefix, *format, verification.Format)
			return exportExitCode(errFormatUnsupported)
		}
		_, _ = fmt.Fprintf(stderr, "%s-format must be a history format such as %s\n", prefix, verification.Format)
		return exitUsage
	}

	cfg, err := databaseConfig()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, prefix+err.Error())
		return exitUsage
	}
	exportConnConfig(cfg)
	// fail prints one masked stderr line and returns err's exit code.
	fail := func(err error, msg string) int {
		_, _ = fmt.Fprintln(stderr, prefix+mask(cfg, errors.New(msg)))
		return exportExitCode(err)
	}

	if path != "-" {
		if code, ok := checkOutputPath(path, fail); !ok {
			return code
		}
	}

	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fail(err, err.Error())
	}
	var buf bytes.Buffer
	exp, err := export(ctx, conn, roundID, &buf)
	_ = conn.Close(ctx)
	switch {
	case errors.Is(err, eventstore.ErrRoundNotFound):
		return fail(err, "round "+round+" not found")
	case errors.Is(err, eventstore.ErrNoCheckpoint):
		return fail(err, "round "+round+" has no snapshot to end an export at")
	case err != nil:
		return fail(err, err.Error())
	}

	reg, err := newRegistry()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, prefix+mask(cfg, err))
		return exitFailed
	}
	settled, err := settlement(buf.Bytes(), reg, settlementPolicies())
	if !*unsettled {
		switch settled {
		case settledYes:
		case settledNo:
			return fail(errNotSettled, fmt.Sprintf("round %s is not settled at its newest snapshot, sequence %d of %d: "+
				"the file would disclose its unrevealed secret config; -unsettled exports it anyway",
				round, exp.Records-1, exp.RoundRecords-1))
		default:
			return fail(err, fmt.Sprintf("cannot establish that round %s is settled: %s; -unsettled exports it anyway",
				round, settlementReason(err)))
		}
	}

	report := exportReport(*format, exp, settled)
	if path == "-" {
		if _, err := stdout.Write(buf.Bytes()); err != nil {
			return fail(err, "write stdout: "+err.Error())
		}
		if _, err := stderr.Write(report); err != nil {
			return fail(err, "write report: "+err.Error())
		}
		return exitOK
	}
	if code, ok := writeOutputFile(path, buf.Bytes(), fail); !ok {
		return code
	}
	if _, err := stdout.Write(report); err != nil {
		_ = os.Remove(path)
		return fail(err, "write report: "+err.Error())
	}
	return exitOK
}

// checkOutputPath runs the X-3 pre-checks on FILE before connecting: it must
// not exist (as anything, a dangling symlink included) and its directory
// must. On failure it returns fail's code and false.
func checkOutputPath(path string, fail func(error, string) int) (int, bool) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return fail(errOutputExists, path+" exists; refusing to overwrite"), false
	case !errors.Is(err, fs.ErrNotExist):
		return fail(err, err.Error()), false
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	switch {
	case err != nil:
		return fail(err, fmt.Sprintf("%s: directory %s is not usable: %v", path, dir, err)), false
	case !info.IsDir():
		err := errors.New("not a directory")
		return fail(err, fmt.Sprintf("%s: directory %s is not usable: %v", path, dir, err)), false
	}
	return exitOK, true
}

// writeOutputFile creates FILE exclusively with mode 0600 and writes, syncs
// and closes it; on any failure after the open it removes FILE. On failure
// it returns fail's code and false.
func writeOutputFile(path string, data []byte, fail func(error, string) int) (int, bool) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case errors.Is(err, fs.ErrExist):
		return fail(errOutputExists, path+" exists; refusing to overwrite"), false
	case err != nil:
		return fail(err, err.Error()), false
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fail(err, "write "+path+": "+err.Error()), false
	}
	return exitOK, true
}

// exportConnConfig applies the X-4 session parameters: a read-only session
// always, and application_name and idle_in_transaction_session_timeout unless
// the DSN sets them.
func exportConnConfig(cfg *pgx.ConnConfig) {
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	if _, ok := cfg.RuntimeParams["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = name
	}
	if _, ok := cfg.RuntimeParams["idle_in_transaction_session_timeout"]; !ok {
		cfg.RuntimeParams["idle_in_transaction_session_timeout"] = "60000"
	}
}

// settlement reports whether the history is settled at its last record
// (X-7): it replays the history and asks the policy for its rules version
// whether the replayed state is final. An unknown verdict comes with an
// error wrapping errSettlementCheck and the cause: a *verification.Failure,
// errNoPolicy, the Final error, or another Verify error.
func settlement(history []byte, reg *game.Registry, policies []execution.Policy) (settledVerdict, error) {
	rep, err := verification.Verify(bytes.NewReader(history), verification.Options{Mode: verification.ModeReplay, Registry: reg})
	if err != nil {
		return settledUnknown, fmt.Errorf("%w: %w", errSettlementCheck, err)
	}
	d := rep.Descriptor
	for _, p := range policies {
		if p.Descriptor() != d {
			continue
		}
		final, err := p.Final(rep.State)
		switch {
		case err != nil:
			return settledUnknown, fmt.Errorf("%w: %w", errSettlementCheck, err)
		case final:
			return settledYes, nil
		default:
			return settledNo, nil
		}
	}
	return settledUnknown, fmt.Errorf("%w: %w", errSettlementCheck,
		fmt.Errorf("%w for %s rules %d", errNoPolicy, d.Game, d.RulesVersion))
}

// settlementReason returns the cause of a settlement check error as the
// stderr message names it: the verification failure, "no settlement policy
// for <game> rules <n>", or the Final error.
func settlementReason(err error) string {
	var f *verification.Failure
	if errors.As(err, &f) {
		return f.Error()
	}
	msg := strings.TrimPrefix(err.Error(), errSettlementCheck.Error()+": ")
	return strings.TrimPrefix(msg, name+": ")
}

// exportExitCode maps an export error to its X-9 exit code.
func exportExitCode(err error) int {
	var f *verification.Failure
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errOutputExists):
		return exitUsage
	case errors.Is(err, errFormatUnsupported):
		return exitUnsupported
	case errors.Is(err, errNotSettled):
		return exitNotSettled
	case errors.Is(err, eventstore.ErrRoundNotFound):
		return exitNotFound
	case errors.Is(err, eventstore.ErrNoCheckpoint), errors.Is(err, eventstore.ErrCorrupt):
		return exitFailed
	case errors.As(err, &f):
		if f.Unsupported {
			return exitUnsupported
		}
		return exitFailed
	case errors.Is(err, errNoPolicy):
		return exitUnsupported
	case errors.Is(err, errSettlementCheck):
		return exitFailed
	default:
		return exitIO
	}
}

// exportReport formats the success report of spec section 6.
func exportReport(format string, exp eventstore.Export, settled settledVerdict) []byte {
	var b bytes.Buffer
	_, _ = fmt.Fprintf(&b, "exporter %s\n", buildinfo.String(name))
	_, _ = fmt.Fprintf(&b, "format %s\n", format)
	_, _ = fmt.Fprintf(&b, "round %s\n", canonjson.UUID(exp.RoundID))
	_, _ = fmt.Fprintf(&b, "records %d of %d head %s\n", exp.Records, exp.RoundRecords, exp.Head)
	if exp.Records < exp.RoundRecords {
		_, _ = fmt.Fprintf(&b, "note: the file ends at the newest snapshot, sequence %d; sequences %d to %d are not exported\n",
			exp.Records-1, exp.Records, exp.RoundRecords-1)
	}
	_, _ = fmt.Fprintf(&b, "settled %s\n", settled)
	if settled != settledYes {
		b.WriteString("warning: the file holds the round's secret config before settlement; keep it private\n")
	}
	b.WriteString("ok\n")
	return b.Bytes()
}

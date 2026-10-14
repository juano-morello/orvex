// Command crashctl is the Orvex replay/audit CLI (ADR-007). It is a scaffold: no commands are implemented yet.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/juano-morello/orvex/internal/buildinfo"
)

const name = "crashctl"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. Exit codes: 0 success/help, 1 not
// implemented or write failure, 2 usage error.
func run(args []string, stdout, stderr io.Writer) int {
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
	_, _ = fmt.Fprintf(stderr, "%s: not implemented yet\n", name)
	return 1
}

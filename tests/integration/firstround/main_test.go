//go:build integration && acceptance

// Package firstround is the black-box acceptance test of the first playable
// round (ADR-006 critical failure tests). It builds the real engine, gateway
// and crashctl binaries, runs them as child processes against PostgreSQL
// and plays rounds through the gateway's public HTTP and Server-Sent Events
// interface. The test writes the database only to create login roles and
// the controlled round, and reads it to check what a client cannot see:
// admissions, chain records and the ledger.
//
// The services are never stubbed and a scenario never skips: without a
// Docker daemon or a build of the services it fails. The TestHarness*
// self-tests check the proxy, the stream reader, the smoke bet, the secret
// check, the process supervisor and the database checks without them.
package firstround

import (
	"os"
	"testing"

	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
	code := pgtest.Run(m)
	removeBinaries()
	os.Exit(code)
}

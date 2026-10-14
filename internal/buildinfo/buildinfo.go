// Package buildinfo exposes the build identifier stamped into Orvex binaries.
package buildinfo

// Version identifies the build. It defaults to "dev" and is overridden at link
// time by the Makefile:
//
//	-ldflags "-X github.com/juano-morello/orvex/internal/buildinfo.Version=<git describe>"
var Version = "dev"

// String returns the version line printed by a binary, e.g. "gateway dev".
func String(binary string) string {
	return binary + " " + Version
}

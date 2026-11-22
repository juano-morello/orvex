package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/persistence/sealing"
	"github.com/juano-morello/orvex/internal/runtime/lifecycle"
)

// Environment variables the engine reads.
const (
	databaseURLEnv   = "ORVEX_DATABASE_URL"
	keyringFileEnv   = sealing.KeyringFileEnv
	addrEnv          = "ORVEX_ENGINE_ADDR"
	tokenFileEnv     = "ORVEX_GATEWAY_TOKEN_FILE"
	bettingWindowEnv = "ORVEX_BETTING_WINDOW"
	createRoundsEnv  = "ORVEX_CREATE_ROUNDS"
)

// Defaults of the optional variables, and the connect timeout unless the
// database URL sets connect_timeout.
const (
	defaultBettingWindow  = 10 * time.Second
	defaultConnectTimeout = 10 * time.Second
	maxTokenFileBytes     = 4 << 10
)

// config is the engine's validated environment.
type config struct {
	pool          *pgxpool.Config
	keyringFile   string
	addr          string
	tokenFile     string
	bettingWindow time.Duration
	createRounds  bool
}

// loadConfig reads the environment through lookup. No error carries a
// variable's value: the database URL can hold a password, and the files are
// secrets.
func loadConfig(lookup func(string) (string, bool)) (config, error) {
	required := func(name string) (string, error) {
		v, ok := lookup(name)
		if !ok || v == "" {
			return "", errors.New(name + " is not set")
		}
		return v, nil
	}
	var c config
	dsn, err := required(databaseURLEnv)
	if err != nil {
		return config{}, err
	}
	if c.pool, err = pgxpool.ParseConfig(dsn); err != nil {
		return config{}, errors.New("invalid " + databaseURLEnv)
	}
	if c.pool.ConnConfig.ConnectTimeout == 0 {
		c.pool.ConnConfig.ConnectTimeout = defaultConnectTimeout
	}
	if c.keyringFile, err = required(keyringFileEnv); err != nil {
		return config{}, err
	}
	if c.addr, err = required(addrEnv); err != nil {
		return config{}, err
	}
	if c.tokenFile, err = required(tokenFileEnv); err != nil {
		return config{}, err
	}

	c.bettingWindow = defaultBettingWindow
	if v, ok := lookup(bettingWindowEnv); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, errors.New(bettingWindowEnv + " is not a Go duration such as 10s")
		}
		if d < 0 || d > lifecycle.MaxBettingWindow {
			return config{}, fmt.Errorf("%s is outside [0s, %v]", bettingWindowEnv, lifecycle.MaxBettingWindow)
		}
		c.bettingWindow = d
	}
	c.createRounds = true
	if v, ok := lookup(createRoundsEnv); ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return config{}, errors.New(createRoundsEnv + " is not true or false")
		}
		c.createRounds = b
	}
	return c, nil
}

// readToken reads the gateway token file: a regular file of at most 4 KiB
// that grants no group write and nothing to others (the keyring rule,
// LBT-21 K-2), holding one token of printable ASCII without spaces;
// surrounding whitespace is ignored. Errors name the variable and the rule,
// never the path or the content.
func readToken(path string) (string, error) {
	fail := func(reason string) (string, error) {
		return "", errors.New(tokenFileEnv + ": " + reason)
	}
	// Stat before opening, so that a FIFO or a device never blocks the open.
	before, err := os.Stat(path)
	if err != nil {
		return fail("unreadable")
	}
	if !before.Mode().IsRegular() {
		return fail("not a regular file")
	}
	f, err := os.Open(path) //nolint:gosec // G304: ORVEX_GATEWAY_TOKEN_FILE names the operator's own token file; mode and size are checked on the opened file
	if err != nil {
		return fail("unreadable")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	switch {
	case err != nil:
		return fail("unreadable")
	case !os.SameFile(before, info) || !info.Mode().IsRegular():
		return fail("not a regular file")
	case info.Mode().Perm()&0o027 != 0:
		return fail(fmt.Sprintf("permissions too open (%04o)", uint32(info.Mode().Perm())))
	case info.Size() > maxTokenFileBytes:
		return fail("larger than 4 KiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFileBytes+1))
	if err != nil || len(b) > maxTokenFileBytes {
		return fail("unreadable")
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return fail("empty")
	}
	for i := range len(token) {
		if token[i] < '!' || token[i] > '~' {
			return fail("the token must be printable ASCII without spaces")
		}
	}
	return token, nil
}

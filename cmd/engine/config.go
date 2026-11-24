package main

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/persistence/sealing"
	"github.com/juano-morello/orvex/internal/runtime/lifecycle"
	"github.com/juano-morello/orvex/internal/transport/engineapi"
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
	if err := engineapi.CheckAddress(c.addr); err != nil {
		return config{}, fmt.Errorf("%s: %w", addrEnv, err)
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

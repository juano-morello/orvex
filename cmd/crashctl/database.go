package main

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// databaseURLEnv names the database crashctl migrate and crashctl audit
// export connect to.
const databaseURLEnv = "ORVEX_DATABASE_URL"

// defaultConnectTimeout applies unless the DSN sets connect_timeout.
const defaultConnectTimeout = 10 * time.Second

// Configuration errors returned by databaseConfig; callers print them after
// their prefix and exit 2 (LBT-10 P-6, LBT-19 X-4).
var (
	errDatabaseURLUnset   = errors.New(databaseURLEnv + " is not set")
	errDatabaseURLInvalid = errors.New("invalid " + databaseURLEnv)
)

// databaseConfig reads ORVEX_DATABASE_URL (unset or empty: errDatabaseURLUnset;
// unparsable: errDatabaseURLInvalid, never the pgx error, which can echo the
// DSN) and applies defaultConnectTimeout unless the DSN sets connect_timeout.
func databaseConfig() (*pgx.ConnConfig, error) {
	dsn, ok := os.LookupEnv(databaseURLEnv)
	if !ok || dsn == "" {
		return nil, errDatabaseURLUnset
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errDatabaseURLInvalid
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = defaultConnectTimeout
	}
	return cfg, nil
}

// mask returns err as one line with cfg's password, if any, replaced by xxxxx.
func mask(cfg *pgx.ConnConfig, err error) string {
	msg := oneLine(err.Error())
	if cfg != nil && cfg.Password != "" {
		msg = strings.ReplaceAll(msg, cfg.Password, "xxxxx")
	}
	return msg
}

// oneLine collapses whitespace so that a multi-line driver error prints as
// one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

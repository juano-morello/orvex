package main

import (
	"errors"
	"net"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// The database URL rule (LBT-21 K-12,
// docs/specs/87d8d90c_round-secret-protection.md): crashctl never prints
// text produced outside Orvex once it has read ORVEX_DATABASE_URL. Driver,
// network and TLS errors, and the server's message, detail and hint, can
// carry the user, the database, hosts, ports or resolved addresses; only
// SQLSTATE codes, constraint names and the messages of class-OX errors,
// which Orvex migrations author, are printed.

// driverError replaces a foreign error node and its whole subtree.
const driverError = "database driver error"

// sqlStatePattern and constraintPattern bound what a server-sent code and
// constraint name may contribute to a message.
var (
	sqlStatePattern   = regexp.MustCompile(`^[0-9A-Z]{5}$`)
	constraintPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// connectFailure renders a pgx.ConnectConfig error (K-12 step b): a fixed
// message, plus the SQLSTATE when the server answered with an error, or
// "timeout" for a timeout. Nothing else.
func connectFailure(err error) string {
	msg := "cannot connect to the database named by " + databaseURLEnv
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr):
		return msg + ": " + sqlState(pgErr)
	case isTimeout(err):
		return msg + ": timeout"
	default:
		return msg
	}
}

// isTimeout reports a pgconn or network timeout anywhere in err's chain.
func isTimeout(err error) bool {
	if pgconn.Timeout(err) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// sqlState renders a server error as "SQLSTATE <code>".
func sqlState(e *pgconn.PgError) string {
	if !sqlStatePattern.MatchString(e.Code) {
		return "SQLSTATE ?????"
	}
	return "SQLSTATE " + e.Code
}

// renderPgError renders a server error (K-12 step c): SQLSTATE and
// constraint name only, or for class OX the Orvex-authored message too.
func renderPgError(e *pgconn.PgError) string {
	code := sqlState(e)
	if sqlStatePattern.MatchString(e.Code) && strings.HasPrefix(e.Code, "OX") {
		return oneLine(e.Message) + " (" + code + ")"
	}
	if constraintPattern.MatchString(e.ConstraintName) {
		return code + " constraint " + e.ConstraintName
	}
	return code
}

// trustedPackage reports whether an error type defined in package pkg
// produces only Orvex-authored text: the Orvex module, crashctl itself, or
// the standard library's plain wrappers.
func trustedPackage(pkg string) bool {
	switch pkg {
	case "main", "fmt", "errors", "io/fs", "context":
		return true
	}
	const module = "github.com/juano-morello/orvex"
	return pkg == module || strings.HasPrefix(pkg, module+"/")
}

// errorPackage returns the package path of err's concrete type, looking
// through one pointer.
func errorPackage(err error) string {
	t := reflect.TypeOf(err)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath()
}

// children returns err's direct wrapped errors.
func children(err error) []error {
	switch u := err.(type) { //nolint:errorlint // K-12 renders each node of the wrap tree itself
	case interface{ Unwrap() error }:
		if c := u.Unwrap(); c != nil {
			return []error{c}
		}
	case interface{ Unwrap() []error }:
		var out []error
		for _, c := range u.Unwrap() {
			if c != nil {
				out = append(out, c)
			}
		}
		return out
	}
	return nil
}

// render applies K-12 step c to err, node by node over its wrap tree. A
// *pgconn.PgError renders as renderPgError. A node of a trusted package
// keeps its own text with every occurrence of each child's text replaced by
// that child's rendering (a child's text may appear more than once, as with
// fmt.Errorf("%w (%v)", e, e)); if some child's text does not occur in it,
// the node renders as its children's renderings joined by ": ". Any other node
// renders as "database driver error" (": timeout" added for a timeout), and
// its subtree is dropped, except that a driver wrapper around a server
// error (pgconn.PrepareError) renders as that server error: SQLSTATE and
// constraint only, never the wrapper's text.
func render(err error) string {
	if err == nil {
		return ""
	}
	if pgErr, ok := err.(*pgconn.PgError); ok { //nolint:errorlint // K-12 renders each node of the wrap tree itself
		return renderPgError(pgErr)
	}
	if !trustedPackage(errorPackage(err)) {
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr):
			return renderPgError(pgErr)
		case isTimeout(err):
			return driverError + ": timeout"
		default:
			return driverError
		}
	}
	text := err.Error()
	var (
		pairs    [][2]string
		rendered []string
		inline   = true
	)
	for _, c := range children(err) {
		ct := c.Error()
		if ct == "" {
			continue
		}
		r := render(c)
		rendered = append(rendered, r)
		pairs = append(pairs, [2]string{ct, r})
		if !strings.Contains(text, ct) {
			inline = false
		}
	}
	if !inline {
		return strings.Join(rendered, ": ")
	}
	if len(pairs) == 0 {
		return text
	}
	// One left-to-right pass; at each position the longest child text wins,
	// so a child whose text contains another child's is replaced whole and
	// no rendering is rescanned.
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i][0]) > len(pairs[j][0]) })
	args := make([]string, 0, 2*len(pairs))
	for _, p := range pairs {
		args = append(args, p[0], p[1])
	}
	return strings.NewReplacer(args...).Replace(text)
}

// redact applies K-12 step d to s: one line, with any remaining occurrence
// of the raw DSN and of the password replaced by xxxxx.
func redact(cfg *pgx.ConnConfig, s string) string {
	s = oneLine(s)
	if cfg == nil {
		return s
	}
	if dsn := cfg.ConnString(); dsn != "" {
		s = strings.ReplaceAll(s, dsn, "xxxxx")
	}
	if cfg.Password != "" {
		s = strings.ReplaceAll(s, cfg.Password, "xxxxx")
	}
	return s
}

// describe renders err for one stderr line under the K-12 rule.
func describe(cfg *pgx.ConnConfig, err error) string {
	return redact(cfg, render(err))
}

// oneLine collapses whitespace so that a multi-line error prints as one
// line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

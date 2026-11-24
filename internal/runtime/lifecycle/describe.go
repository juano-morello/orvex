package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// describedSentinels are the error classes Describe names, in this order.
var describedSentinels = []error{
	ErrInvalidConfig, errHalted, errLeadershipLost,
	execution.ErrFenced, execution.ErrFailClosed, execution.ErrCorrupt, execution.ErrUnknownRules,
	execution.ErrRoundNotFound, execution.ErrRoundExists, execution.ErrInvalidRound, execution.ErrInvalidRequest,
	execution.ErrBusy, execution.ErrUnavailable, execution.ErrInternal,
	admission.ErrOutcomeUnknown, admission.ErrUnavailable, admission.ErrBusy, admission.ErrInternal,
	admission.ErrInvalidRequest, admission.ErrInvalidCommand, admission.ErrUnknownRules, admission.ErrRoundNotFound,
	admission.ErrCommandIDReused, admission.ErrClosureNotDue, admission.ErrNotFound,
	context.Canceled, context.DeadlineExceeded,
}

var sqlStatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// Describe renders err for one log line. An error with no node from the
// database driver keeps its own text. Otherwise the driver's text can name
// the database host, port, user and name, so the line is only the Orvex
// error classes err wraps, an admission rejection's reason, and the
// SQLSTATE, or "database driver error".
func Describe(err error) string {
	if err == nil {
		return ""
	}
	if !fromDriver(err) {
		return strings.Join(strings.Fields(err.Error()), " ")
	}
	var parts []string
	for _, s := range describedSentinels {
		if errors.Is(err, s) {
			parts = append(parts, s.Error())
		}
	}
	if refused, ok := errors.AsType[*admission.RejectedError](err); ok {
		parts = append(parts, refused.Error())
	}
	switch pgErr, ok := errors.AsType[*pgconn.PgError](err); {
	case ok && sqlStatePattern.MatchString(pgErr.Code):
		parts = append(parts, "SQLSTATE "+pgErr.Code)
	case pgconn.Timeout(err):
		parts = append(parts, "database driver error: timeout")
	default:
		parts = append(parts, "database driver error")
	}
	return strings.Join(parts, ": ")
}

// fromDriver reports whether any node of err's wrap tree has a type
// defined by pgx or its packages.
func fromDriver(err error) bool {
	if err == nil {
		return false
	}
	t := reflect.TypeOf(err)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if strings.HasPrefix(t.PkgPath(), "github.com/jackc/") {
		return true
	}
	switch u := err.(type) { //nolint:errorlint // walks each node of the wrap tree itself
	case interface{ Unwrap() error }:
		return fromDriver(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, c := range u.Unwrap() {
			if fromDriver(c) {
				return true
			}
		}
	}
	return false
}

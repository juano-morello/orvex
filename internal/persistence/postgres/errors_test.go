package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSQLState(t *testing.T) {
	pgErr := &pgconn.PgError{Code: CodeAppendOnly}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"pg error", pgErr, CodeAppendOnly},
		{"wrapped once", fmt.Errorf("insert: %w", pgErr), CodeAppendOnly},
		{"wrapped twice", fmt.Errorf("step: %w", fmt.Errorf("insert: %w", pgErr)), CodeAppendOnly},
		{"plain error", errors.New("boom"), ""},
		{"nil", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SQLState(tt.err); got != tt.want {
				t.Errorf("SQLState = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock", fmt.Errorf("commit: %w", &pgconn.PgError{Code: "40P01"}), true},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"append-only", &pgconn.PgError{Code: CodeAppendOnly}, false},
		{"hold unreleased", &pgconn.PgError{Code: CodeHoldUnreleased}, false},
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryable(tt.err); got != tt.want {
				t.Errorf("IsRetryable = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCodes pins the class OX SQLSTATEs the migrations raise.
func TestCodes(t *testing.T) {
	codes := []struct{ got, want string }{
		{CodeAppendOnly, "OX001"},
		{CodeLedgerAccount, "OX002"},
		{CodeDownRefused, "OX003"},
		{CodeNotFenced, "OX004"},
		{CodeHeadRegression, "OX005"},
		{CodeOutboxImmutable, "OX006"},
		{CodeEncoding, "OX007"},
		{CodeRecordBinding, "OX008"},
		{CodeHoldUnreleased, "OX009"},
		{CodeSecretReveal, "OX010"},
	}
	for _, c := range codes {
		if c.got != c.want {
			t.Errorf("code = %q, want %q", c.got, c.want)
		}
	}
}

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

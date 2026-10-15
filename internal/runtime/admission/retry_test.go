package admission

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func pgErr(code, constraint string) error {
	return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code, ConstraintName: constraint})
}

func TestClassifyCallError(t *testing.T) {
	live := context.Background()
	done, cancel := context.WithCancel(context.Background())
	cancel()
	netErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want callClass
	}{
		{"40001", live, pgErr("40001", ""), callRetry},
		{"40P01", live, pgErr("40P01", ""), callRetry},
		{"55P03", live, pgErr("55P03", ""), callBusy},
		{"23505 command id key", live, pgErr("23505", "admissions_command_id_key"), callResolve},
		{"23505 other constraint", live, pgErr("23505", "admissions_pkey"), callInternal},
		{"23514 floor", live, pgErr("23514", "ledger_accounts_floor_check"), callInsufficientCredits},
		{"23514 other constraint", live, pgErr("23514", "ledger_holds_amount_check"), callInternal},
		{"OX005", live, pgErr("OX005", ""), callInternal},
		{"22023", live, pgErr("22023", ""), callInternal},
		{"net.OpError", live, netErr, callRetry},
		{"08006", live, pgErr("08006", ""), callRetry},
		{"53300", live, pgErr("53300", ""), callRetry},
		{"57P01", live, pgErr("57P01", ""), callRetry},
		{"25P03", live, pgErr("25P03", ""), callRetry},
		{"57014 with ctx done", done, pgErr("57014", ""), callContext},
		{"57014 with ctx alive", live, pgErr("57014", ""), callRetry},
		{"context.Canceled", live, fmt.Errorf("query: %w", context.Canceled), callContext},
		{"context.Canceled with ctx done", done, context.Canceled, callContext},
		{"no rows", live, pgx.ErrNoRows, callInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyCallError(c.ctx, c.err); got != c.want {
				t.Errorf("classifyCallError = %d, want %d", got, c.want)
			}
		})
	}
}

func TestClassifyCommitError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want commitClass
	}{
		{"23000", pgErr("23000", ""), commitDefinite},
		{"OX004", pgErr("OX004", ""), commitDefinite},
		{"40002", pgErr("40002", ""), commitDefinite},
		{"ErrTxCommitRollback", pgx.ErrTxCommitRollback, commitDefinite},
		{"40001", pgErr("40001", ""), commitRetry},
		{"40P01", pgErr("40P01", ""), commitRetry},
		{"40003", pgErr("40003", ""), commitAmbiguous},
		{"57P01", pgErr("57P01", ""), commitAmbiguous},
		{"08006", pgErr("08006", ""), commitAmbiguous},
		{"53300", pgErr("53300", ""), commitAmbiguous},
		{"XX000", pgErr("XX000", ""), commitAmbiguous},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, commitAmbiguous},
		{"wrapped context.Canceled", fmt.Errorf("commit: %w", context.Canceled), commitAmbiguous},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyCommitError(c.err); got != c.want {
				t.Errorf("classifyCommitError = %d, want %d", got, c.want)
			}
		})
	}
}

func TestBackoffBounds(t *testing.T) {
	if got := backoff(1); got != 0 {
		t.Errorf("backoff(1) = %v, want 0", got)
	}
	for n := 2; n <= 6; n++ {
		limit := min(50*time.Millisecond, 5*time.Millisecond<<(n-2))
		if got := backoffLimit(n); got != limit {
			t.Errorf("backoffLimit(%d) = %v, want %v", n, got, limit)
		}
		for range 1000 {
			if d := backoff(n); d < 0 || d > limit {
				t.Fatalf("backoff(%d) = %v, outside [0, %v]", n, d, limit)
			}
		}
	}
	if got := backoffLimit(100); got != 50*time.Millisecond {
		t.Errorf("backoffLimit(100) = %v, want 50ms", got)
	}
}

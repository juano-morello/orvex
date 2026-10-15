package execution

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
	return fmt.Errorf("statement: %w", &pgconn.PgError{Code: code, ConstraintName: constraint})
}

func TestClassifyStatementError(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want stmtClass
	}{
		{"40001", live, pgErr("40001", ""), stmtRetry},
		{"40P01", live, pgErr("40P01", ""), stmtRetry},
		{"55P03", live, pgErr("55P03", ""), stmtBusy},
		{"net.OpError", live, &net.OpError{Op: "read", Err: errors.New("reset")}, stmtRetry},
		{"08006", live, pgErr("08006", ""), stmtRetry},
		{"53300", live, pgErr("53300", ""), stmtRetry},
		{"57P01", live, pgErr("57P01", ""), stmtRetry},
		{"25P03", live, pgErr("25P03", ""), stmtRetry},
		{"57014 with ctx done", done, pgErr("57014", ""), stmtContext},
		{"context.Canceled", live, fmt.Errorf("x: %w", context.Canceled), stmtContext},
		{"23503 prev_fkey", live, pgErr("23503", "chain_records_prev_fkey"), stmtConflict},
		{"23503 other", live, pgErr("23503", "domain_events_prev_fkey"), stmtInternal},
		{"23514 floor", live, pgErr("23514", "ledger_accounts_floor_check"), stmtInternal},
		{"23505 event key", live, pgErr("23505", "ledger_postings_event_key"), stmtInternal},
		{"OX008", live, pgErr("OX008", ""), stmtInternal},
		{"22023", live, pgErr("22023", ""), stmtInternal},
		{"42P01", live, pgErr("42P01", ""), stmtInternal},
		{"XX000", live, pgErr("XX000", ""), stmtInternal},
		{"no rows", live, pgx.ErrNoRows, stmtInternal},
	}
	for _, c := range cases {
		if got := classifyStatementError(c.ctx, c.err); got != c.want {
			t.Errorf("%s: class %d, want %d", c.name, got, c.want)
		}
	}
}

func TestClassifyCommitError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want commitClass
	}{
		{"OX004", pgErr("OX004", ""), commitConflict},
		{"23503 head_fkey", pgErr("23503", "round_execution_heads_head_fkey"), commitConflict},
		{"40001", pgErr("40001", ""), commitRetry},
		{"40P01", pgErr("40P01", ""), commitRetry},
		{"OX009", pgErr("OX009", ""), commitDefinite},
		{"23000", pgErr("23000", ""), commitDefinite},
		{"40002", pgErr("40002", ""), commitDefinite},
		{"ErrTxCommitRollback", pgx.ErrTxCommitRollback, commitDefinite},
		{"40003", pgErr("40003", ""), commitAmbiguous},
		{"57P01", pgErr("57P01", ""), commitAmbiguous},
		{"08006", pgErr("08006", ""), commitAmbiguous},
		{"53300", pgErr("53300", ""), commitAmbiguous},
		{"XX000", pgErr("XX000", ""), commitAmbiguous},
		{"unexpected EOF", io.ErrUnexpectedEOF, commitAmbiguous},
		{"context.Canceled", fmt.Errorf("commit: %w", context.Canceled), commitAmbiguous},
	}
	for _, c := range cases {
		if got := classifyCommitError(c.err); got != c.want {
			t.Errorf("%s: class %d, want %d", c.name, got, c.want)
		}
	}
}

func TestConflictDecision(t *testing.T) {
	const k, token = 5, 2
	hash := [32]byte{1}
	other := [32]byte{2}
	cases := []struct {
		name string
		head headView
		want conflictDecision
	}{
		{"no row", headView{}, conflictCorrupt},
		{"other token", headView{found: true, processed: k, token: token + 1, recordHash: hash[:]}, conflictFenced},
		{"committed with the computed hash", headView{found: true, processed: k, token: token, recordHash: hash[:]}, conflictAdopt},
		{"committed further with the computed hash", headView{found: true, processed: k + 3, token: token, recordHash: hash[:]}, conflictAdopt},
		{"committed with another hash", headView{found: true, processed: k, token: token, recordHash: other[:]}, conflictReload},
		{"nothing committed", headView{found: true, processed: k - 1, token: token}, conflictRetry},
		{"database behind the cache", headView{found: true, processed: k - 3, token: token}, conflictReload},
	}
	for _, c := range cases {
		if got := decideConflict(c.head, k, hash, token); got != c.want {
			t.Errorf("%s: decision %d, want %d", c.name, got, c.want)
		}
	}
}

func TestBackoffBounds(t *testing.T) {
	for n := 2; n <= 6; n++ {
		limit := min(50*time.Millisecond, 5*time.Millisecond<<(n-2))
		if got := backoffLimit(n); got != limit {
			t.Errorf("backoffLimit(%d) = %v, want %v", n, got, limit)
		}
		for range 1000 {
			if d := backoff(n); d < 0 || d > limit {
				t.Fatalf("backoff(%d) = %v outside [0, %v]", n, d, limit)
			}
		}
	}
	if backoff(1) != 0 {
		t.Error("backoff before the first attempt is not 0")
	}
}

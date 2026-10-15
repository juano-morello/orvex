package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestTxOptions(t *testing.T) {
	tests := []struct {
		name string
		got  pgx.TxOptions
		want pgx.TxOptions
	}{
		{"admission", AdmissionTxOptions(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}},
		{"execution", ExecutionTxOptions(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}},
		{"snapshot read", SnapshotReadTxOptions(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: options = %+v, want %+v", tt.name, tt.got, tt.want)
		}
	}
}

func TestExecutionTxOptionsWithTimeouts(t *testing.T) {
	const prototype = "BEGIN ISOLATION LEVEL READ COMMITTED READ WRITE; SELECT set_config('lock_timeout', '3000ms', true), set_config('idle_in_transaction_session_timeout', '1000ms', true), set_config('synchronous_commit', CASE current_setting('synchronous_commit') WHEN 'off' THEN 'on' ELSE current_setting('synchronous_commit') END, true)"
	got := ExecutionTxOptionsWithTimeouts(3*time.Second, time.Second)
	if got.BeginQuery != prototype {
		t.Errorf("BeginQuery = %q, want %q", got.BeginQuery, prototype)
	}
	if got.IsoLevel != pgx.ReadCommitted || got.AccessMode != pgx.ReadWrite {
		t.Errorf("options = %+v, want READ COMMITTED, READ WRITE", got)
	}

	other := ExecutionTxOptionsWithTimeouts(1500*time.Millisecond, 250*time.Millisecond)
	for _, want := range []string{"'1500ms'", "'250ms'"} {
		if !strings.Contains(other.BeginQuery, want) {
			t.Errorf("BeginQuery %q lacks %s", other.BeginQuery, want)
		}
	}
	if other.IsoLevel != pgx.ReadCommitted || other.AccessMode != pgx.ReadWrite {
		t.Errorf("options = %+v, want READ COMMITTED, READ WRITE", other)
	}
}

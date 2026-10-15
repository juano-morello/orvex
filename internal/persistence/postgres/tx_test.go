package postgres

import (
	"testing"

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

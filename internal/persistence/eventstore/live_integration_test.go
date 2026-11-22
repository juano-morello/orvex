//go:build integration

package eventstore_test

import (
	"errors"
	"testing"

	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
)

func TestReadTailDetectsMissingLastRecord(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.MigratedDatabase(t))
	round := pgtest.LoadHistory(t, pool, readGolden(t, "worked-example.jsonl"), nil)
	last := uint64(round.Records - 1) //nolint:gosec // G115: the fixture contains 23 records
	tail, err := eventstore.ReadTail(ctx, pool, round.RoundID, last+1, 1)
	if err != nil || len(tail.Records) != 0 {
		t.Fatalf("read past the last record: %d records, %v", len(tail.Records), err)
	}
	pgtest.ReplicaExec(t, pool, `DELETE FROM orvex.chain_records WHERE round_id = $1 AND sequence = $2`, round.RoundID, last)
	if _, err := eventstore.ReadTail(ctx, pool, round.RoundID, last, 1); !errors.Is(err, eventstore.ErrCorrupt) {
		t.Fatalf("read at a missing final record: %v, want ErrCorrupt", err)
	}
}

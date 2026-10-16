package eventstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// ExportHistoryV2 writes round roundID as an "orvex/audit-history/v2" file:
// ExportHistory's header fields, records 0..S verbatim, then the round's
// ledger rows of sequences 0..S as one ledger line per record (LBT-18 L-9).
// opts selects the secret config's source as for ExportHistory (LBT-21
// K-9). On error, w may hold a partial file the caller must discard.
func ExportHistoryV2(ctx context.Context, db TxBeginner, roundID [16]byte, w io.Writer, opts Options) (Export, error) {
	tx, err := db.BeginTx(ctx, postgres.SnapshotReadTxOptions())
	if err != nil {
		return Export{}, fmt.Errorf("eventstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	secret, cps, head, err := readExportBase(ctx, tx, roundID, opts)
	if err != nil {
		return Export{}, err
	}
	last := cps[len(cps)-1].Sequence

	rows, err := readLedger(ctx, tx, roundID, last)
	if err != nil {
		return Export{}, err
	}
	header, err := encodeHeaderV2(head, cps, secret, rows.accounts)
	if err != nil {
		return Export{}, err
	}
	bw := bufio.NewWriter(w)
	if err := writeLine(bw, header); err != nil {
		return Export{}, err
	}
	n, err := writeRecords(ctx, tx, bw, roundID, last)
	if err != nil {
		return Export{}, err
	}
	for j := uint64(0); j <= last; j++ {
		s := int64(j)
		line, err := encodeLedgerLine(j, rows.holds[s], rows.releases[s], rows.transactions[s])
		if err != nil {
			return Export{}, err
		}
		if err := writeLine(bw, line); err != nil {
			return Export{}, err
		}
	}
	if err := bw.Flush(); err != nil {
		return Export{}, fmt.Errorf("eventstore: write: %w", err)
	}
	return Export{RoundID: roundID, Records: int(n), Head: head, Checkpoints: cps}, nil //nolint:gosec // G115: n is one round's record count (last + 1), as in ExportHistory's int(n)
}

// readExportBase reads the round's secret config (readSecret), its
// snapshots as checkpoints and the head at the newest snapshot, with
// ExportHistory's queries and checks (LBT-10 section 7.2 steps 1 to 4).
func readExportBase(ctx context.Context, tx pgx.Tx, roundID [16]byte, opts Options) ([]byte, []Checkpoint, chain.Hash, error) {
	var none chain.Hash
	secret, err := readSecret(ctx, tx, roundID, opts)
	if err != nil {
		return nil, nil, none, err
	}

	rows, err := tx.Query(ctx, `SELECT sequence, state_hash FROM orvex.round_snapshots
		WHERE round_id = $1 ORDER BY sequence`, roundID)
	if err != nil {
		return nil, nil, none, fmt.Errorf("eventstore: read snapshots: %w", err)
	}
	var cps []Checkpoint
	for rows.Next() {
		var seq int64
		var h []byte
		if err := rows.Scan(&seq, &h); err != nil {
			rows.Close()
			return nil, nil, none, fmt.Errorf("eventstore: read snapshots: %w", err)
		}
		if seq < 0 || len(h) != len(chain.Hash{}) {
			rows.Close()
			return nil, nil, none, fmt.Errorf("%w: snapshot %d", ErrCorrupt, seq)
		}
		cp := Checkpoint{Sequence: uint64(seq)}
		copy(cp.StateHash[:], h)
		cps = append(cps, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, none, fmt.Errorf("eventstore: read snapshots: %w", err)
	}
	if len(cps) == 0 {
		return nil, nil, none, ErrNoCheckpoint
	}
	last := cps[len(cps)-1].Sequence

	var headBytes []byte
	err = tx.QueryRow(ctx, `SELECT hash FROM orvex.chain_records WHERE round_id = $1 AND sequence = $2`,
		roundID, int64(last)).Scan(&headBytes) //nolint:gosec // G115: last is a snapshot sequence scanned from bigint and checked >= 0 above
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil, none, fmt.Errorf("%w: no chain record at snapshot %d", ErrCorrupt, last)
	case err != nil:
		return nil, nil, none, fmt.Errorf("eventstore: read head: %w", err)
	case len(headBytes) != len(chain.Hash{}):
		return nil, nil, none, fmt.Errorf("%w: head hash length %d", ErrCorrupt, len(headBytes))
	}
	var head chain.Hash
	copy(head[:], headBytes)
	return secret, cps, head, nil
}

// writeRecords writes records 0..last verbatim, one per line, with
// ExportHistory's checks (LBT-10 section 7.2 step 6), and returns their
// count.
func writeRecords(ctx context.Context, tx pgx.Tx, bw *bufio.Writer, roundID [16]byte, last uint64) (uint64, error) {
	rows, err := tx.Query(ctx, `SELECT sequence, record FROM orvex.chain_records
		WHERE round_id = $1 AND sequence <= $2 ORDER BY sequence`, roundID, int64(last)) //nolint:gosec // G115: last is the newest snapshot sequence, a bigint checked >= 0 by readExportBase
	if err != nil {
		return 0, fmt.Errorf("eventstore: read records: %w", err)
	}
	defer rows.Close()
	var n uint64
	for rows.Next() {
		var seq int64
		var record []byte
		if err := rows.Scan(&seq, &record); err != nil {
			return 0, fmt.Errorf("eventstore: read records: %w", err)
		}
		switch {
		case seq < 0 || uint64(seq) != n:
			return 0, fmt.Errorf("%w: record %d where %d was expected", ErrCorrupt, seq, n)
		case len(record) > verification.MaxLineBytes:
			return 0, fmt.Errorf("%w: record %d exceeds %d bytes", ErrCorrupt, seq, verification.MaxLineBytes)
		case bytes.IndexByte(record, '\n') >= 0:
			return 0, fmt.Errorf("%w: record %d contains a newline", ErrCorrupt, seq)
		}
		if err := writeLine(bw, record); err != nil {
			return 0, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("eventstore: read records: %w", err)
	}
	if n != last+1 {
		return 0, fmt.Errorf("%w: %d records up to snapshot %d", ErrCorrupt, n, last)
	}
	return n, nil
}

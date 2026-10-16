// Package eventstore reads round histories out of the PostgreSQL system of
// record (ADR-001, ADR-005; decision P-24 and section 7 of
// docs/specs/17380a6e_postgresql-schema-migrations.md).
//
// ExportHistory writes a round as an LBT-9 audit history file: the stored
// chain record bytes copied verbatim, never re-encoded from columns, with a
// header built from engine-written snapshot rows and the round's secret
// config.
//
// ExportHistoryV2 writes format v2: the same file plus the round's ledger
// rows (LBT-18, docs/specs/ac81f88f_audit-ledger-stage.md).
//
// Both read the round's secret config from orvex.round_secret_reveals, the
// secret the record that settled the round revealed, which is all an
// auditor's grants reach (LBT-21, docs/specs/87d8d90c_round-secret-protection.md,
// K-9 and K-10). Options.Unrevealed selects the privileged path for a round
// with no reveal: it reads orvex.round_secrets, the legacy plaintext as is
// or the sealed form opened with Options.Keyring.
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
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

// TxBeginner starts a transaction with options; *pgx.Conn and
// *pgxpool.Pool implement it.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Checkpoint is a snapshot sequence and the engine-computed state hash after
// it.
type Checkpoint struct {
	Sequence  uint64
	StateHash chain.Hash
}

// Export describes a written history.
type Export struct {
	RoundID [16]byte
	// Records is S + 1, where S is the newest snapshot's sequence.
	Records int
	// RoundRecords is N + 1, the round's chain records in the same snapshot;
	// Records ≤ RoundRecords.
	RoundRecords int
	Head         chain.Hash
	Checkpoints  []Checkpoint
}

// Options selects where ExportHistory and ExportHistoryV2 read the round's
// secret config (LBT-21 K-9). The zero Options reads revealed secrets only.
type Options struct {
	// Unrevealed reads round_secrets when the round has no reveal: the
	// privileged path (SELECT on round_secrets, and Keyring for a sealed
	// row).
	Unrevealed bool
	// Keyring opens a sealed secret config; nil opens none.
	Keyring *sealing.Keyring
}

// Errors returned by ExportHistory; test them with errors.Is.
var (
	ErrRoundNotFound = errors.New("eventstore: round not found")
	ErrNoCheckpoint  = errors.New("eventstore: round has no snapshot to end an export at")
	ErrCorrupt       = errors.New("eventstore: stored round data violates an export invariant")
	// ErrNotRevealed: the round has no reveal and Options.Unrevealed is
	// false. Nothing is written.
	ErrNotRevealed = errors.New("eventstore: round has no revealed secret config")
	// ErrKeyringRequired: the privileged path found a sealed secret config
	// and Options.Keyring is nil. Nothing is written.
	ErrKeyringRequired = errors.New("eventstore: the round's secret config is sealed and no keyring was given")
)

// ExportHistory writes round roundID as an "orvex/audit-history/v1" file
// (LBT-9 A-11) to w. It reads one REPEATABLE READ READ ONLY snapshot and
// ends the file at the round's newest snapshot S: records 0..S verbatim,
// with every snapshot as a checkpoint. It also reports, from the same
// snapshot, how many chain records the round has (Export.RoundRecords), so
// callers can tell when records after S were left out; the written bytes do
// not depend on that count. opts selects the secret config's source
// (readSecret). On error, w may hold a partial file the caller must
// discard.
func ExportHistory(ctx context.Context, db TxBeginner, roundID [16]byte, w io.Writer, opts Options) (Export, error) {
	tx, err := db.BeginTx(ctx, postgres.SnapshotReadTxOptions())
	if err != nil {
		return Export{}, fmt.Errorf("eventstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	secret, err := readSecret(ctx, tx, roundID, opts)
	if err != nil {
		return Export{}, err
	}

	rows, err := tx.Query(ctx, `SELECT sequence, state_hash FROM orvex.round_snapshots
		WHERE round_id = $1 ORDER BY sequence`, roundID)
	if err != nil {
		return Export{}, fmt.Errorf("eventstore: read snapshots: %w", err)
	}
	var cps []Checkpoint
	for rows.Next() {
		var seq int64
		var h []byte
		if err := rows.Scan(&seq, &h); err != nil {
			rows.Close()
			return Export{}, fmt.Errorf("eventstore: read snapshots: %w", err)
		}
		if seq < 0 || len(h) != len(chain.Hash{}) {
			rows.Close()
			return Export{}, fmt.Errorf("%w: snapshot %d", ErrCorrupt, seq)
		}
		cp := Checkpoint{Sequence: uint64(seq)}
		copy(cp.StateHash[:], h)
		cps = append(cps, cp)
	}
	if err := rows.Err(); err != nil {
		return Export{}, fmt.Errorf("eventstore: read snapshots: %w", err)
	}
	if len(cps) == 0 {
		return Export{}, ErrNoCheckpoint
	}
	last := cps[len(cps)-1].Sequence

	var headBytes []byte
	err = tx.QueryRow(ctx, `SELECT hash FROM orvex.chain_records WHERE round_id = $1 AND sequence = $2`,
		roundID, int64(last)).Scan(&headBytes) //nolint:gosec // G115: last is a snapshot sequence scanned from bigint and checked >= 0 above
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Export{}, fmt.Errorf("%w: no chain record at snapshot %d", ErrCorrupt, last)
	case err != nil:
		return Export{}, fmt.Errorf("eventstore: read head: %w", err)
	case len(headBytes) != len(chain.Hash{}):
		return Export{}, fmt.Errorf("%w: head hash length %d", ErrCorrupt, len(headBytes))
	}
	var head chain.Hash
	copy(head[:], headBytes)

	var newest *int64
	err = tx.QueryRow(ctx, `SELECT max(sequence) FROM orvex.chain_records WHERE round_id = $1`,
		roundID).Scan(&newest)
	switch {
	case err != nil:
		return Export{}, fmt.Errorf("eventstore: read record count: %w", err)
	case newest == nil:
		return Export{}, fmt.Errorf("%w: round has no chain records", ErrCorrupt)
	case *newest < int64(last): //nolint:gosec // G115: last is a snapshot sequence scanned from bigint and checked >= 0 above
		return Export{}, fmt.Errorf("%w: newest record %d before snapshot %d", ErrCorrupt, *newest, last)
	}

	header, err := encodeHeader(head, cps, secret)
	if err != nil {
		return Export{}, err
	}
	bw := bufio.NewWriter(w)
	if err := writeLine(bw, header); err != nil {
		return Export{}, err
	}

	rows, err = tx.Query(ctx, `SELECT sequence, record FROM orvex.chain_records
		WHERE round_id = $1 AND sequence <= $2 ORDER BY sequence`, roundID, int64(last)) //nolint:gosec // G115: last is a snapshot sequence scanned from bigint and checked >= 0 above
	if err != nil {
		return Export{}, fmt.Errorf("eventstore: read records: %w", err)
	}
	defer rows.Close()
	var n uint64
	for rows.Next() {
		var seq int64
		var record []byte
		if err := rows.Scan(&seq, &record); err != nil {
			return Export{}, fmt.Errorf("eventstore: read records: %w", err)
		}
		switch {
		case seq < 0 || uint64(seq) != n:
			return Export{}, fmt.Errorf("%w: record %d where %d was expected", ErrCorrupt, seq, n)
		case len(record) > verification.MaxLineBytes:
			return Export{}, fmt.Errorf("%w: record %d exceeds %d bytes", ErrCorrupt, seq, verification.MaxLineBytes)
		case bytes.IndexByte(record, '\n') >= 0:
			return Export{}, fmt.Errorf("%w: record %d contains a newline", ErrCorrupt, seq)
		}
		if err := writeLine(bw, record); err != nil {
			return Export{}, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return Export{}, fmt.Errorf("eventstore: read records: %w", err)
	}
	if n != last+1 {
		return Export{}, fmt.Errorf("%w: %d records up to snapshot %d", ErrCorrupt, n, last)
	}
	if err := bw.Flush(); err != nil {
		return Export{}, fmt.Errorf("eventstore: write: %w", err)
	}
	return Export{
		RoundID:      roundID,
		Records:      int(n),
		RoundRecords: int(*newest) + 1,
		Head:         head,
		Checkpoints:  cps,
	}, nil
}

func writeLine(w *bufio.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("eventstore: write: %w", err)
	}
	if err := w.WriteByte('\n'); err != nil {
		return fmt.Errorf("eventstore: write: %w", err)
	}
	return nil
}

// encodeHeader builds the history header line (spec section 7.2 step 5).
// configSecret must be a canonical OCJ object; otherwise it returns an error
// wrapping ErrCorrupt.
func encodeHeader(head chain.Hash, cps []Checkpoint, configSecret []byte) ([]byte, error) {
	secret, err := canonjson.Unmarshal(configSecret)
	if err != nil {
		return nil, fmt.Errorf("%w: secret config: %w", ErrCorrupt, err)
	}
	list := make(canonjson.Array, len(cps))
	for i, cp := range cps {
		list[i] = canonjson.Object{
			"sequence":  canonjson.Uint(cp.Sequence),
			"stateHash": canonjson.Hex(cp.StateHash[:]),
		}
	}
	b, err := canonjson.Marshal(canonjson.Object{
		"checkpoints":  list,
		"configSecret": secret,
		"format":       canonjson.String(verification.Format),
		"head":         canonjson.Hex(head[:]),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: header: %w", ErrCorrupt, err)
	}
	return b, nil
}

// readSecret reads the round's secret config in tx (LBT-21 K-9):
//
//  1. the reveal, from a statement that never names round_secrets, so the
//     auditor's grants suffice; no round is ErrRoundNotFound;
//  2. no reveal and opts.Unrevealed false: ErrNotRevealed;
//  3. no reveal and opts.Unrevealed true: round_secrets, the privileged
//     path. No row is ErrCorrupt; legacy plaintext is used as is; a sealed
//     row needs opts.Keyring (else ErrKeyringRequired) and an Open failure
//     is ErrCorrupt wrapping the sealing error.
//
// No error carries the secret, a key, the nonce or the ciphertext.
func readSecret(ctx context.Context, tx pgx.Tx, roundID [16]byte, opts Options) ([]byte, error) {
	var revealed []byte
	err := tx.QueryRow(ctx, `SELECT v.config_secret FROM orvex.rounds r
		LEFT JOIN orvex.round_secret_reveals v USING (round_id) WHERE r.round_id = $1`, roundID).Scan(&revealed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrRoundNotFound
	case err != nil:
		return nil, fmt.Errorf("eventstore: read revealed secret: %w", err)
	case revealed != nil:
		return revealed, nil
	case !opts.Unrevealed:
		return nil, ErrNotRevealed
	}

	var (
		legacy, nonce, ciphertext []byte
		scheme, keyID             *string
	)
	err = tx.QueryRow(ctx, `SELECT config_secret, secret_scheme, secret_key_id, secret_nonce, secret_ciphertext
		FROM orvex.round_secrets WHERE round_id = $1`, roundID).Scan(&legacy, &scheme, &keyID, &nonce, &ciphertext)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("%w: round has no secret config", ErrCorrupt)
	case err != nil:
		return nil, fmt.Errorf("eventstore: read sealed secret: %w", err)
	case legacy != nil:
		return legacy, nil
	case ciphertext == nil:
		return nil, fmt.Errorf("%w: round has no secret config", ErrCorrupt)
	case opts.Keyring == nil:
		return nil, ErrKeyringRequired
	}
	sealed := sealing.Sealed{Nonce: nonce, Ciphertext: ciphertext}
	if scheme != nil {
		sealed.Scheme = *scheme
	}
	if keyID != nil {
		sealed.KeyID = *keyID
	}
	secret, err := opts.Keyring.Open(roundID, sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: secret config: %w", ErrCorrupt, err)
	}
	return secret, nil
}

package eventstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// The live reads below serve round subscribers and command lookups. None
// reads orvex.round_secrets or orvex.round_secret_reveals (LBT-21).

// ErrRecordNotFound: ReadRecord found no chain record at the sequence.
var ErrRecordNotFound = errors.New("eventstore: chain record not found")

// CurrentRound returns the newest round of g and rules version rv by
// creation time, or ErrRoundNotFound.
func CurrentRound(ctx context.Context, db TxBeginner, g game.ID, rv game.RulesVersion) ([16]byte, error) {
	var id [16]byte
	err := inSnapshot(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT round_id FROM orvex.rounds
			WHERE game = $1 AND rules_version = $2
			ORDER BY created_at_unix_micro DESC, round_id DESC LIMIT 1`, string(g), int64(rv)).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return id, ErrRoundNotFound
	}
	return id, err
}

// StoredRecord is one chain record's stored bytes.
type StoredRecord struct {
	Sequence uint64
	Record   []byte
}

// Tail is a read of a round's chain records from one snapshot.
type Tail struct {
	Game         game.ID
	RulesVersion game.RulesVersion
	// Head is the round's execution head: the highest executed sequence.
	Head uint64
	// Final reports that the round admits nothing more and Head is its
	// last admission, so no record after Head will ever exist.
	Final bool
	// Records are the records from the requested sequence on, gapless and
	// in order, at most the requested limit.
	Records []StoredRecord
}

// ReadTail reads round's heads and up to limit records with sequence >=
// from in one REPEATABLE READ READ ONLY snapshot. ErrRoundNotFound for an
// unknown round; a gap in the records is ErrCorrupt.
func ReadTail(ctx context.Context, db TxBeginner, round [16]byte, from uint64, limit int) (Tail, error) {
	var t Tail
	if from > maxSequence || limit < 1 {
		return t, fmt.Errorf("eventstore: read tail: invalid range")
	}
	err := inSnapshot(ctx, db, func(tx pgx.Tx) error {
		var (
			gameID string
			rules  int64
			head   int64
			sealed *int64
		)
		err := tx.QueryRow(ctx, `SELECT r.game, r.rules_version, e.processed_sequence, a.sealed_sequence
			FROM orvex.rounds r
			JOIN orvex.round_execution_heads e ON e.round_id = r.round_id
			JOIN orvex.round_admission_heads a ON a.round_id = r.round_id
			WHERE r.round_id = $1`, round).Scan(&gameID, &rules, &head, &sealed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRoundNotFound
		}
		if err != nil {
			return err
		}
		if head < 0 || rules < 1 || rules > 1<<32-1 {
			return fmt.Errorf("%w: round heads", ErrCorrupt)
		}
		t.Game, t.RulesVersion, t.Head = game.ID(gameID), game.RulesVersion(rules), uint64(head)
		t.Final = sealed != nil && *sealed == head
		rows, err := tx.Query(ctx, `SELECT sequence, record FROM orvex.chain_records
			WHERE round_id = $1 AND sequence >= $2 ORDER BY sequence LIMIT $3`, round, int64(from), limit) //nolint:gosec // G115: from <= maxSequence
		if err != nil {
			return err
		}
		defer rows.Close()
		next := from
		for rows.Next() {
			var seq int64
			var record []byte
			if err := rows.Scan(&seq, &record); err != nil {
				return err
			}
			if seq < 0 || uint64(seq) != next {
				return fmt.Errorf("%w: record %d where %d was expected", ErrCorrupt, seq, next)
			}
			if next > t.Head {
				return fmt.Errorf("%w: record %d past execution head %d", ErrCorrupt, next, t.Head)
			}
			t.Records = append(t.Records, StoredRecord{Sequence: next, Record: record})
			next++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(t.Records) < limit && next <= t.Head {
			return fmt.Errorf("%w: missing record %d before end of batch", ErrCorrupt, next)
		}
		return nil
	})
	if err != nil {
		return Tail{}, err
	}
	return t, nil
}

// ReadRecord returns the chain record of round at sequence, or
// ErrRecordNotFound.
func ReadRecord(ctx context.Context, db TxBeginner, round [16]byte, sequence uint64) ([]byte, error) {
	if sequence > maxSequence {
		return nil, ErrRecordNotFound
	}
	var record []byte
	err := inSnapshot(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT record FROM orvex.chain_records WHERE round_id = $1 AND sequence = $2`,
			round, int64(sequence)).Scan(&record) //nolint:gosec // G115: sequence <= maxSequence
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRecordNotFound
	}
	return record, err
}

// maxSequence is the largest sequence a bigint column holds.
const maxSequence = 1<<63 - 1

// inSnapshot runs fn in a REPEATABLE READ READ ONLY transaction and
// returns fn's error, or the driver's error unchanged.
func inSnapshot(ctx context.Context, db TxBeginner, fn func(pgx.Tx) error) error {
	tx, err := db.BeginTx(ctx, postgres.SnapshotReadTxOptions())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

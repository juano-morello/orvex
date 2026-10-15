package execution

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/replay"
	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// load is the cold load (§5.2): one REPEATABLE READ READ ONLY transaction,
// always rolled back, that replays the round's chain into en. Errors wrap
// ErrRoundNotFound, ErrFenced, ErrUnknownRules, ErrCorrupt, ErrInternal,
// errTransient or a context error.
func (e *Executor) load(ctx context.Context, en *entry, lease Lease) error {
	tx, err := e.db.BeginTx(ctx, postgres.SnapshotReadTxOptions())
	if err != nil {
		return loadDBError(ctx, err)
	}
	defer rollback(tx)

	// 1. Round, secret and execution head.
	var (
		gameID                  string
		rulesV, schemaV, stateV int64
		secret                  []byte
		processed, token        *int64
	)
	err = tx.QueryRow(ctx, `SELECT r.game, r.rules_version, r.schema_version, r.state_version, s.config_secret,
		h.processed_sequence, h.fencing_token
		FROM orvex.rounds r
		LEFT JOIN orvex.round_secrets s USING (round_id)
		LEFT JOIN orvex.round_execution_heads h USING (round_id)
		WHERE r.round_id = $1`, lease.RoundID).
		Scan(&gameID, &rulesV, &schemaV, &stateV, &secret, &processed, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRoundNotFound
	}
	if err != nil {
		return loadDBError(ctx, err)
	}
	if secret == nil || processed == nil || token == nil || *processed < 0 {
		return fmt.Errorf("%w: the round has no secret config or execution head", ErrCorrupt)
	}
	if *token != lease.FencingToken {
		return ErrFenced
	}

	// 2. Rules.
	r, ok := e.rules[rulesKey{game: game.ID(gameID), rules: game.RulesVersion(rulesV)}]
	if !ok {
		return fmt.Errorf("%w: %q %d", ErrUnknownRules, gameID, rulesV)
	}
	desc := r.adapter.Descriptor()
	if int64(desc.SchemaVersion) != schemaV || int64(desc.StateVersion) != stateV {
		return fmt.Errorf("%w: %q %d: the round's schema or state version differs", ErrUnknownRules, gameID, rulesV)
	}

	// 3. Records 0..processed.
	type stored struct {
		hash   []byte
		record []byte
	}
	var records []stored
	rows, err := tx.Query(ctx, `SELECT sequence, hash, record FROM orvex.chain_records
		WHERE round_id = $1 AND sequence <= $2 ORDER BY sequence`, lease.RoundID, *processed)
	if err != nil {
		return loadDBError(ctx, err)
	}
	for rows.Next() {
		var seq int64
		var s stored
		if err := rows.Scan(&seq, &s.hash, &s.record); err != nil {
			rows.Close()
			return loadDBError(ctx, err)
		}
		if seq != int64(len(records)) || len(s.hash) != len(chain.Hash{}) {
			rows.Close()
			return fmt.Errorf("%w: chain record %d is missing or malformed", ErrCorrupt, len(records))
		}
		records = append(records, s)
	}
	if err := rows.Err(); err != nil {
		return loadDBError(ctx, err)
	}
	if int64(len(records)) != *processed+1 {
		return fmt.Errorf("%w: %d chain records for processed sequence %d", ErrCorrupt, len(records), *processed)
	}

	// 4. Snapshots up to processed.
	snapshots := map[uint64][]byte{}
	rows, err = tx.Query(ctx, `SELECT sequence, state_hash FROM orvex.round_snapshots
		WHERE round_id = $1 AND sequence <= $2 ORDER BY sequence`, lease.RoundID, *processed)
	if err != nil {
		return loadDBError(ctx, err)
	}
	for rows.Next() {
		var seq int64
		var h []byte
		if err := rows.Scan(&seq, &h); err != nil {
			rows.Close()
			return loadDBError(ctx, err)
		}
		snapshots[uint64(seq)] = h
	}
	if err := rows.Err(); err != nil {
		return loadDBError(ctx, err)
	}

	// 5. Genesis, config, initial state.
	rec, err := chain.Decode(records[0].record)
	if err != nil {
		return fmt.Errorf("%w: genesis: %w", ErrCorrupt, err)
	}
	g, ok := rec.(chain.Genesis)
	if !ok || g.RoundID != lease.RoundID || g.Game != desc.Game || g.RulesVersion != desc.RulesVersion ||
		g.SchemaVersion != desc.SchemaVersion || g.StateVersion != desc.StateVersion {
		return fmt.Errorf("%w: the genesis record disagrees with the round", ErrCorrupt)
	}
	cfg, err := r.adapter.DecodeConfig(lease.RoundID, g.Config, secret)
	if err != nil {
		return fmt.Errorf("%w: config: %w", ErrCorrupt, err)
	}
	initial, err := r.adapter.InitialState(cfg)
	if err != nil {
		return fmt.Errorf("%w: initial state: %w", ErrCorrupt, err)
	}

	// 6. Command records, then state replay with snapshot checks.
	nextEvent := uint64(1)
	replayed := make([]replay.Record, 0, len(records)-1)
	for i := 1; i < len(records); i++ {
		rec, err := chain.Decode(records[i].record)
		if err != nil {
			return fmt.Errorf("%w: record %d: %w", ErrCorrupt, i, err)
		}
		c, ok := rec.(chain.Command)
		if !ok || c.RoundID != lease.RoundID || c.Sequence != uint64(i) || string(c.PrevHash[:]) != string(records[i-1].hash) {
			return fmt.Errorf("%w: record %d disagrees with its position", ErrCorrupt, i)
		}
		rr := replay.Record{Sequence: c.Sequence, Input: c.Input(), Result: c.Result}
		for _, ev := range c.Events {
			if ev.Sequence != nextEvent {
				return fmt.Errorf("%w: record %d: event sequence gap", ErrCorrupt, i)
			}
			nextEvent++
			d, err := r.adapter.DecodeEvent(ev.Payload)
			if err != nil {
				return fmt.Errorf("%w: record %d: event: %w", ErrCorrupt, i, err)
			}
			rr.Events = append(rr.Events, d)
			rr.EventBytes = append(rr.EventBytes, ev.Payload)
		}
		replayed = append(replayed, rr)
	}
	var final any
	err = replay.Run(r.adapter, initial, replayed, replay.StateOnly, func(seq uint64, s any) error {
		final = s
		want, ok := snapshots[seq]
		if !ok {
			return nil
		}
		b, err := r.adapter.EncodeState(s)
		if err != nil {
			return err
		}
		got, err := chain.StateHash(desc.Game, desc.StateVersion, b)
		if err != nil {
			return err
		}
		if string(got[:]) != string(want) {
			return errSnapshotMismatch
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: replay: %w", ErrCorrupt, err)
	}

	// 7. The entry.
	en.rules = r
	en.state = final
	en.processed = uint64(*processed)
	copy(en.head[:], records[len(records)-1].hash)
	en.nextEventSequence = nextEvent
	en.pending = nil
	en.loaded = true
	return nil
}

var errSnapshotMismatch = errors.New("execution: replayed state hash differs from a snapshot")

// loadDBError classifies a database error of the cold load.
func loadDBError(ctx context.Context, err error) error {
	switch classifyStatementError(ctx, err) {
	case stmtContext:
		return contextError(ctx, err)
	case stmtRetry:
		return fmt.Errorf("%w: cold load: %w", errTransient, err)
	case stmtBusy:
		return fmt.Errorf("%w: cold load: %w", ErrBusy, err)
	default:
		return fmt.Errorf("%w: cold load: %w", ErrInternal, err)
	}
}

// admissionSQL is E-4's single read of admission k with its holds.
const admissionSQL = `SELECT a.command_id, a.effective_time_unix_micro, a.command, h.account_id, h.amount, acc.player_id
	FROM orvex.admissions a
	LEFT JOIN orvex.ledger_holds h ON h.round_id = a.round_id AND h.sequence = a.sequence
	LEFT JOIN orvex.ledger_accounts acc ON acc.account_id = h.account_id
	WHERE a.round_id = $1 AND a.sequence = $2
	ORDER BY h.account_id`

// readAdmission reads admission k of the round (E-4); ok is false when it
// does not exist yet.
func (e *Executor) readAdmission(ctx context.Context, roundID [16]byte, k uint64) (admissionRow, bool, error) {
	rows, err := e.db.Query(ctx, admissionSQL, roundID, int64(k))
	if err != nil {
		return admissionRow{}, false, err
	}
	defer rows.Close()
	adm := admissionRow{sequence: k}
	found := false
	for rows.Next() {
		var (
			account pgtype.UUID
			amount  *int64
			player  *string
		)
		if err := rows.Scan(&adm.commandID, &adm.effectiveTime, &adm.command, &account, &amount, &player); err != nil {
			return admissionRow{}, false, err
		}
		found = true
		if account.Valid {
			h := holdRow{accountID: account.Bytes}
			if amount != nil {
				h.amount = *amount
			}
			if player != nil {
				h.player = *player
			}
			adm.holds = append(adm.holds, h)
		}
	}
	if err := rows.Err(); err != nil {
		return admissionRow{}, false, err
	}
	return adm, found, nil
}

// rollback ends tx, bounded by its own timeout so that a done ctx still
// releases the connection.
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(ctx)
}

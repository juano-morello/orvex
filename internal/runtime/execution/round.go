package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// NewRound is a round to create (E-16).
type NewRound struct {
	RoundID      [16]byte
	Game         game.ID
	RulesVersion game.RulesVersion
	// Config is the rules version's typed config, including the secret
	// seed, which the caller draws with crypto/rand (D-10).
	Config any
	// CreatedAtUnixMicro is the genesis createdAtUnixMicro (A-8), the
	// scheduler's clock.
	CreatedAtUnixMicro int64
}

// Created describes a created round.
type Created struct {
	RoundID   [16]byte
	Genesis   []byte
	Head      chain.Hash
	StateHash chain.Hash
	// Duplicate reports that the identical round already existed.
	Duplicate bool
}

// CreateRound writes the round, its secret config and secret close offset,
// both heads, the genesis record, snapshot 0, the round's round_pool
// account, the house account (if absent) and the genesis outbox message in
// one transaction (E-16 and the LBT-12 engineer addendum). It is idempotent
// by round ID and genesis bytes: the identical round returns Duplicate, a
// different one ErrRoundExists. Neither Created nor any error carries the
// close offset.
func (e *Executor) CreateRound(ctx context.Context, r NewRound) (Created, error) {
	// 1. Validate before the database.
	if r.RoundID == [16]byte{} {
		return Created{}, fmt.Errorf("%w: nil round id", ErrInvalidRound)
	}
	re, ok := e.rules[rulesKey{game: r.Game, rules: r.RulesVersion}]
	if !ok {
		return Created{}, fmt.Errorf("%w: %q %d", ErrUnknownRules, r.Game, r.RulesVersion)
	}
	a := re.adapter
	desc := a.Descriptor()
	public, secret, err := a.EncodeConfig(r.Config)
	if err != nil {
		return Created{}, fmt.Errorf("%w: encode config: %w", ErrInvalidRound, err)
	}
	decoded, err := a.DecodeConfig(r.RoundID, public, secret)
	if err != nil {
		return Created{}, fmt.Errorf("%w: decode config: %w", ErrInvalidRound, err)
	}
	initial, err := a.InitialState(r.Config)
	if err != nil {
		return Created{}, fmt.Errorf("%w: initial state: %w", ErrInvalidRound, err)
	}
	stateHash, err := stateHashOf(a, initial)
	if err != nil {
		return Created{}, fmt.Errorf("%w: %w", ErrInvalidRound, err)
	}
	fromDecoded, err := a.InitialState(decoded)
	if err != nil {
		return Created{}, fmt.Errorf("%w: initial state of the decoded config: %w", ErrInvalidRound, err)
	}
	if h, err := stateHashOf(a, fromDecoded); err != nil || h != stateHash {
		return Created{}, fmt.Errorf("%w: the config does not belong to the round", ErrInvalidRound)
	}
	offset, err := re.policy.CloseOffsetMicros(initial)
	if err != nil {
		return Created{}, fmt.Errorf("%w: close offset: %w", ErrInvalidRound, err)
	}

	// 2. Genesis.
	genesis, err := chain.EncodeGenesis(chain.Genesis{
		RoundID: r.RoundID, Game: desc.Game, RulesVersion: desc.RulesVersion,
		SchemaVersion: desc.SchemaVersion, StateVersion: desc.StateVersion,
		Config: public, CreatedAtUnixMicro: r.CreatedAtUnixMicro, EngineBuild: e.cfg.EngineBuild,
	})
	if err != nil {
		return Created{}, fmt.Errorf("%w: genesis: %w", ErrInvalidRound, err)
	}
	out := Created{RoundID: r.RoundID, Genesis: genesis, Head: chain.LinkHash(genesis), StateHash: stateHash}

	// 3-4. One transaction, retried per E-15.
	var lastErr error
	for attempt := 1; attempt <= e.cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := e.sleep(ctx, backoff(attempt)); err != nil {
				return Created{}, contextError(ctx, err)
			}
		}
		res, err := e.insertRound(ctx, r, re, secret, offset, out)
		switch res {
		case createOK:
			// 5. Seed the entry, unless this process already holds one.
			en := e.lock(r.RoundID)
			if !en.loaded && en.halt == nil {
				en.rules = re
				en.roundState = roundState{state: initial, processed: 0, head: out.Head, nextEventSequence: 1}
				en.loaded = true
			}
			en.mu.Unlock()
			return out, nil
		case createExists:
			stored, err := e.storedGenesis(ctx, r.RoundID)
			switch {
			case err == nil && bytes.Equal(stored, genesis):
				out.Duplicate = true
				return out, nil
			case err == nil || errors.Is(err, pgx.ErrNoRows):
				return Created{}, ErrRoundExists
			}
			lastErr = err
			switch classifyStatementError(ctx, err) {
			case stmtContext:
				return Created{}, contextError(ctx, err)
			case stmtRetry:
				continue
			default:
				return Created{}, fmt.Errorf("%w: read genesis: %w", ErrInternal, err)
			}
		case createRetry:
			lastErr = err
		case createBusy:
			return Created{}, fmt.Errorf("%w: %w", ErrBusy, err)
		case createContext:
			return Created{}, contextError(ctx, err)
		default:
			return Created{}, fmt.Errorf("%w: create round: %w", ErrInternal, err)
		}
	}
	return Created{}, fmt.Errorf("%w: %w", ErrUnavailable, lastErr)
}

type createResult uint8

const (
	createOK createResult = iota
	createExists
	createRetry
	createBusy
	createContext
	createInternal
)

// insertRound runs the round-creation transaction (E-16 step 3).
func (e *Executor) insertRound(ctx context.Context, r NewRound, re rulesEntry, secret []byte, offset int64, c Created) (createResult, error) {
	tx, err := e.db.BeginTx(ctx, postgres.ExecutionTxOptionsWithTimeouts(e.cfg.LockTimeout, e.cfg.IdleInTransactionTimeout))
	if err != nil {
		return createStatementResult(ctx, err), err
	}
	committing := false
	defer func() {
		if !committing {
			rollback(tx)
		}
	}()
	desc := re.adapter.Descriptor()
	house, pool := ledger.HouseAccountID(), ledger.RoundAccountID(r.RoundID)
	b := &pgx.Batch{}
	b.Queue(`INSERT INTO orvex.rounds
		(round_id, game, rules_version, schema_version, state_version, chain, created_at_unix_micro)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		r.RoundID, string(desc.Game), int64(desc.RulesVersion), int64(desc.SchemaVersion), int64(desc.StateVersion),
		chain.Version, r.CreatedAtUnixMicro)
	b.Queue(`INSERT INTO orvex.round_secrets (round_id, config_secret, close_offset_micros) VALUES ($1, $2, $3)`,
		r.RoundID, secret, offset)
	b.Queue(`INSERT INTO orvex.round_admission_heads (round_id) VALUES ($1)`, r.RoundID)
	b.Queue(`INSERT INTO orvex.round_execution_heads (round_id) VALUES ($1)`, r.RoundID)
	b.Queue(`INSERT INTO orvex.chain_records
		(round_id, sequence, prev_hash, hash, record, result_status, result_reason, fencing_token)
		VALUES ($1, 0, $2, $3, $4, NULL, NULL, 0)`, r.RoundID, make([]byte, len(chain.Hash{})), c.Head[:], c.Genesis)
	b.Queue(insertSnapshotSQL, r.RoundID, int64(0), c.StateHash[:], e.cfg.EngineBuild)
	b.Queue(`INSERT INTO orvex.ledger_accounts (account_id, kind, name, round_id, floor_enforced)
		VALUES ($1, 'system', $2, $3, false)`, pool, ledger.RoundAccountName, r.RoundID)
	b.Queue(`INSERT INTO orvex.ledger_accounts (account_id, kind, name, round_id, floor_enforced)
		VALUES ($1, 'system', $2, NULL, false) ON CONFLICT (account_id) DO NOTHING`, house, ledger.HouseAccountName)
	b.Queue(insertOutboxSQL, r.RoundID, int64(0), OutboxTopicRecord, c.Genesis)

	br := tx.SendBatch(ctx, b)
	for range b.Len() {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			if isRoundsPkey(err) {
				return createExists, err
			}
			return createStatementResult(ctx, err), err
		}
	}
	if err := br.Close(); err != nil {
		return createStatementResult(ctx, err), err
	}
	committing = true
	if err := tx.Commit(ctx); err != nil {
		switch classifyCommitError(err) {
		case commitRetry:
			return createRetry, err
		case commitDefinite, commitConflict:
			if isRoundsPkey(err) {
				return createExists, err
			}
			return createInternal, err
		default:
			// Ambiguous: the retry resolves it through rounds_pkey.
			return createRetry, err
		}
	}
	return createOK, nil
}

func createStatementResult(ctx context.Context, err error) createResult {
	switch classifyStatementError(ctx, err) {
	case stmtRetry:
		return createRetry
	case stmtBusy:
		return createBusy
	case stmtContext:
		return createContext
	default:
		return createInternal
	}
}

func isRoundsPkey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == codeUniqueViolation && pgErr.ConstraintName == constraintRoundsPkey
}

// storedGenesis reads the round's genesis record.
func (e *Executor) storedGenesis(ctx context.Context, roundID [16]byte) ([]byte, error) {
	var b []byte
	err := e.db.QueryRow(ctx, `SELECT record FROM orvex.chain_records WHERE round_id = $1 AND sequence = 0`, roundID).Scan(&b)
	return b, err
}

// stateHashOf returns the state hash of s under a's rules version.
func stateHashOf(a game.Adapter, s any) (chain.Hash, error) {
	b, err := a.EncodeState(s)
	if err != nil {
		return chain.Hash{}, fmt.Errorf("encode state: %w", err)
	}
	d := a.Descriptor()
	return chain.StateHash(d.Game, d.StateVersion, b)
}

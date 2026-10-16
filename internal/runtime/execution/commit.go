package execution

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// Transaction B's statements (§5.6). The compare-and-set runs alone; the
// inserts run in one pipelined batch in this order.
const (
	casSQL = `UPDATE orvex.round_execution_heads SET processed_sequence = $3
		WHERE round_id = $1 AND processed_sequence = $2 AND fencing_token = $4`
	insertRecordSQL = `INSERT INTO orvex.chain_records
		(round_id, sequence, prev_hash, hash, record, result_status, result_reason, fencing_token)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	insertEventsSQL = `INSERT INTO orvex.domain_events (round_id, event_sequence, sequence, event_type)
		SELECT $1, u.s, $2, u.t FROM unnest($3::bigint[], $4::text[]) AS u(s, t)`
	insertReleasesSQL = `INSERT INTO orvex.ledger_hold_releases (round_id, sequence, account_id)
		SELECT $1, $2, u.a FROM unnest($3::uuid[]) AS u(a) ORDER BY u.a`
	insertLedgerTxSQL = `INSERT INTO orvex.ledger_transactions (transaction_id, kind, round_id, sequence)
		VALUES ($1, $2, $3, $4)`
	insertPostingsSQL = `INSERT INTO orvex.ledger_postings
		(transaction_id, posting_index, debit_account_id, credit_account_id, amount, round_id, event_sequence)
		SELECT $1, u.i, u.d, u.c, u.a, $2, u.e
		FROM unnest($3::int[], $4::uuid[], $5::uuid[], $6::bigint[], $7::bigint[]) AS u(i, d, c, a, e)
		ORDER BY u.i`
	insertOutboxSQL = `INSERT INTO orvex.outbox_messages (round_id, sequence, message_index, topic, payload)
		VALUES ($1, $2, 0, $3, $4)`
	insertSnapshotSQL = `INSERT INTO orvex.round_snapshots (round_id, sequence, state_hash, state, engine_build)
		VALUES ($1, $2, $3, NULL, $4)`
)

// commitResult is how one Transaction B ended (§5.7).
type commitResult uint8

const (
	commitOK commitResult = iota
	commitResultConflict
	commitResultRetry
	commitResultAmbiguous
	commitResultBusy
	commitResultContext
	commitResultInternal
)

// commit runs Transaction B for tr (E-6) and classifies its outcome (E-15).
// accounts holds the resolved debit and credit account of each posting.
func (e *Executor) commit(ctx context.Context, lease Lease, en *entry, tr *transition, accounts [][2][16]byte) (commitResult, error) {
	tx, err := e.db.BeginTx(ctx, postgres.ExecutionTxOptionsWithTimeouts(e.cfg.LockTimeout, e.cfg.IdleInTransactionTimeout))
	if err != nil {
		return statementResult(ctx, err), err
	}
	committing := false
	defer func() {
		if !committing {
			rollback(tx)
		}
	}()

	k := int64(tr.sequence) //nolint:gosec // G115: tr.sequence = processed_sequence + 1, a bigint round sequence
	tag, err := tx.Exec(ctx, casSQL, lease.RoundID, k-1, k, lease.FencingToken)
	if err != nil {
		return statementResult(ctx, err), err
	}
	if tag.RowsAffected() != 1 {
		return commitResultConflict, errNothingCommitted
	}

	b := &pgx.Batch{}
	var status, reason *string
	if tr.result.Rejected {
		status, reason = new("rejected"), new(tr.result.Reason)
	} else {
		status = new("accepted")
	}
	b.Queue(insertRecordSQL, lease.RoundID, k, en.head[:], tr.hash[:], tr.record, status, reason, lease.FencingToken)
	if len(tr.eventSeqs) > 0 {
		b.Queue(insertEventsSQL, lease.RoundID, k, tr.eventSeqs, tr.eventTypes)
	}
	if len(tr.releases) > 0 {
		b.Queue(insertReleasesSQL, lease.RoundID, k, uuids(tr.releases))
	}
	if len(tr.postings) > 0 {
		txID := ledger.TransactionID(lease.RoundID, tr.sequence)
		b.Queue(insertLedgerTxSQL, txID, tr.kind, lease.RoundID, k)
		n := len(tr.postings)
		idx, amounts, events := make([]int32, n), make([]int64, n), make([]int64, n)
		debits, credits := make([][16]byte, n), make([][16]byte, n)
		for i, p := range tr.postings {
			idx[i], amounts[i], events[i] = int32(i), p.Amount, int64(p.EventSequence) //nolint:gosec // G115: event sequences are bigint domain_events.event_sequence values
			debits[i], credits[i] = accounts[i][0], accounts[i][1]
		}
		b.Queue(insertPostingsSQL, txID, lease.RoundID, idx, uuids(debits), uuids(credits), amounts, events)
	}
	b.Queue(insertOutboxSQL, lease.RoundID, k, OutboxTopicRecord, tr.record)
	if tr.checkpoint {
		b.Queue(insertSnapshotSQL, lease.RoundID, k, tr.stateHash[:], e.cfg.EngineBuild)
	}

	br := tx.SendBatch(ctx, b)
	for range b.Len() {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return statementResult(ctx, err), err
		}
	}
	if err := br.Close(); err != nil {
		return statementResult(ctx, err), err
	}

	committing = true
	if err := tx.Commit(ctx); err != nil {
		switch classifyCommitError(err) {
		case commitConflict:
			return commitResultConflict, err
		case commitRetry:
			return commitResultRetry, err
		case commitDefinite:
			return commitResultInternal, err
		default:
			return commitResultAmbiguous, err
		}
	}
	return commitOK, nil
}

// statementResult maps a statement error to a commit result.
func statementResult(ctx context.Context, err error) commitResult {
	switch classifyStatementError(ctx, err) {
	case stmtRetry:
		return commitResultRetry
	case stmtBusy:
		return commitResultBusy
	case stmtConflict:
		return commitResultConflict
	case stmtContext:
		return commitResultContext
	default:
		return commitResultInternal
	}
}

// uuids converts account IDs for a uuid[] parameter.
func uuids(ids [][16]byte) []pgtype.UUID {
	out := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		out[i] = pgtype.UUID{Bytes: id, Valid: true}
	}
	return out
}

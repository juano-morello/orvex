package eventstore

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/canonjson"
)

// Ledger rows as stored, NULLs kept (LBT-18 section 5.4).
type (
	accountRow struct {
		id       [16]byte
		kind     string
		playerID *string
		name     *string
		roundID  *[16]byte
	}
	holdRow struct {
		sequence  int64
		accountID [16]byte
		amount    int64
	}
	releaseRow struct {
		sequence  int64
		accountID [16]byte
	}
	transactionRow struct {
		id       [16]byte
		kind     string
		sequence int64
		postings []postingRow
	}
	postingRow struct {
		index         int64
		debit, credit [16]byte
		amount        int64
		eventSequence *int64
	}
)

// ledgerRows holds a round's ledger rows of sequences 0..S, grouped by
// sequence, and the accounts they reference.
type ledgerRows struct {
	accounts     []accountRow
	holds        map[int64][]holdRow
	releases     map[int64][]releaseRow
	transactions map[int64][]transactionRow
}

// readLedger reads round roundID's holds, releases, transactions and
// postings with sequence <= last, and every account they reference (LBT-18
// section 8 step 2). It copies the rows and never filters or repairs them.
func readLedger(ctx context.Context, tx pgx.Tx, roundID [16]byte, last uint64) (ledgerRows, error) {
	out := ledgerRows{
		holds:        map[int64][]holdRow{},
		releases:     map[int64][]releaseRow{},
		transactions: map[int64][]transactionRow{},
	}
	refs := map[[16]byte]bool{}
	bound := int64(last)

	rows, err := tx.Query(ctx, `SELECT sequence, account_id, amount FROM orvex.ledger_holds
		WHERE round_id = $1 AND sequence <= $2`, roundID, bound)
	if err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read holds: %w", err)
	}
	for rows.Next() {
		var h holdRow
		if err := rows.Scan(&h.sequence, &h.accountID, &h.amount); err != nil {
			rows.Close()
			return ledgerRows{}, fmt.Errorf("eventstore: read holds: %w", err)
		}
		if h.sequence < 0 {
			rows.Close()
			return ledgerRows{}, fmt.Errorf("%w: hold at sequence %d", ErrCorrupt, h.sequence)
		}
		out.holds[h.sequence] = append(out.holds[h.sequence], h)
		refs[h.accountID] = true
	}
	if err := rows.Err(); err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read holds: %w", err)
	}

	rows, err = tx.Query(ctx, `SELECT sequence, account_id FROM orvex.ledger_hold_releases
		WHERE round_id = $1 AND sequence <= $2`, roundID, bound)
	if err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read releases: %w", err)
	}
	for rows.Next() {
		var r releaseRow
		if err := rows.Scan(&r.sequence, &r.accountID); err != nil {
			rows.Close()
			return ledgerRows{}, fmt.Errorf("eventstore: read releases: %w", err)
		}
		if r.sequence < 0 {
			rows.Close()
			return ledgerRows{}, fmt.Errorf("%w: release at sequence %d", ErrCorrupt, r.sequence)
		}
		out.releases[r.sequence] = append(out.releases[r.sequence], r)
		refs[r.accountID] = true
	}
	if err := rows.Err(); err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read releases: %w", err)
	}

	rows, err = tx.Query(ctx, `SELECT transaction_id, kind, sequence FROM orvex.ledger_transactions
		WHERE round_id = $1 AND sequence <= $2`, roundID, bound)
	if err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read transactions: %w", err)
	}
	var txs []transactionRow
	for rows.Next() {
		var t transactionRow
		if err := rows.Scan(&t.id, &t.kind, &t.sequence); err != nil {
			rows.Close()
			return ledgerRows{}, fmt.Errorf("eventstore: read transactions: %w", err)
		}
		if t.sequence < 0 {
			rows.Close()
			return ledgerRows{}, fmt.Errorf("%w: transaction at sequence %d", ErrCorrupt, t.sequence)
		}
		txs = append(txs, t)
	}
	if err := rows.Err(); err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read transactions: %w", err)
	}

	byTx := map[[16]byte][]postingRow{}
	rows, err = tx.Query(ctx, `SELECT p.transaction_id, p.posting_index, p.debit_account_id, p.credit_account_id,
		p.amount, p.event_sequence FROM orvex.ledger_postings p JOIN orvex.ledger_transactions t USING (transaction_id)
		WHERE t.round_id = $1 AND t.sequence <= $2`, roundID, bound)
	if err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read postings: %w", err)
	}
	for rows.Next() {
		var (
			txID  [16]byte
			index int32
			p     postingRow
		)
		if err := rows.Scan(&txID, &index, &p.debit, &p.credit, &p.amount, &p.eventSequence); err != nil {
			rows.Close()
			return ledgerRows{}, fmt.Errorf("eventstore: read postings: %w", err)
		}
		p.index = int64(index)
		byTx[txID] = append(byTx[txID], p)
		refs[p.debit] = true
		refs[p.credit] = true
	}
	if err := rows.Err(); err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read postings: %w", err)
	}
	for _, t := range txs {
		t.postings = byTx[t.id]
		out.transactions[t.sequence] = append(out.transactions[t.sequence], t)
	}

	ids := make([]pgtype.UUID, 0, len(refs))
	for id := range refs {
		ids = append(ids, pgtype.UUID{Bytes: id, Valid: true})
	}
	rows, err = tx.Query(ctx, `SELECT account_id, kind, player_id, name, round_id FROM orvex.ledger_accounts
		WHERE account_id = ANY($1)`, ids)
	if err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read accounts: %w", err)
	}
	for rows.Next() {
		var (
			a   accountRow
			rid pgtype.UUID
		)
		if err := rows.Scan(&a.id, &a.kind, &a.playerID, &a.name, &rid); err != nil {
			rows.Close()
			return ledgerRows{}, fmt.Errorf("eventstore: read accounts: %w", err)
		}
		if rid.Valid {
			id := [16]byte(rid.Bytes)
			a.roundID = &id
		}
		out.accounts = append(out.accounts, a)
	}
	if err := rows.Err(); err != nil {
		return ledgerRows{}, fmt.Errorf("eventstore: read accounts: %w", err)
	}
	if len(out.accounts) != len(refs) {
		return ledgerRows{}, fmt.Errorf("%w: %d of %d referenced ledger accounts found", ErrCorrupt, len(out.accounts), len(refs))
	}
	return out, nil
}

func compareID(a, b [16]byte) int {
	return bytes.Compare(a[:], b[:])
}

// encodeHeaderV2 builds the v2 history header line (LBT-18 section 5.2):
// the v1 header members, format v2 and the account table sorted by ID.
// Rows it cannot encode give an error wrapping ErrCorrupt.
func encodeHeaderV2(head chain.Hash, cps []Checkpoint, configSecret []byte, accounts []accountRow) ([]byte, error) {
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
	sorted := slices.SortedFunc(slices.Values(accounts), func(a, b accountRow) int { return compareID(a.id, b.id) })
	accs := make(canonjson.Array, len(sorted))
	for i, a := range sorted {
		o := canonjson.Object{"accountId": canonjson.UUID(a.id), "kind": canonjson.String(a.kind)}
		switch a.kind {
		case "player":
			if a.playerID == nil || *a.playerID == "" {
				return nil, fmt.Errorf("%w: player account %s without a player id", ErrCorrupt, canonjson.UUID(a.id))
			}
			o["player"] = canonjson.String(*a.playerID)
		case "system":
			if a.name == nil {
				return nil, fmt.Errorf("%w: system account %s without a name", ErrCorrupt, canonjson.UUID(a.id))
			}
			var rid [16]byte // nil UUID: a global account
			if a.roundID != nil {
				rid = *a.roundID
			}
			o["name"] = canonjson.String(*a.name)
			o["roundId"] = canonjson.UUID(rid)
		default:
			return nil, fmt.Errorf("%w: account %s has kind %q", ErrCorrupt, canonjson.UUID(a.id), a.kind)
		}
		accs[i] = o
	}
	b, err := canonjson.Marshal(canonjson.Object{
		"accounts":     accs,
		"checkpoints":  list,
		"configSecret": secret,
		"format":       canonjson.String(verification.FormatV2),
		"head":         canonjson.Hex(head[:]),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: header: %w", ErrCorrupt, err)
	}
	return b, nil
}

// encodeLedgerLine builds ledger line sequence (LBT-18 section 5.3) from the
// rows of that sequence, sorted in Go: holds and releases by account,
// transactions by ID, postings by index. A NULL event_sequence encodes as
// "0". Rows it cannot encode give an error wrapping ErrCorrupt.
func encodeLedgerLine(sequence uint64, holds []holdRow, releases []releaseRow, txs []transactionRow) ([]byte, error) {
	hs := slices.SortedFunc(slices.Values(holds), func(a, b holdRow) int { return compareID(a.accountID, b.accountID) })
	holdList := make(canonjson.Array, len(hs))
	for i, h := range hs {
		holdList[i] = canonjson.Object{"accountId": canonjson.UUID(h.accountID), "amount": canonjson.Int(h.amount)}
	}
	rs := slices.SortedFunc(slices.Values(releases), func(a, b releaseRow) int { return compareID(a.accountID, b.accountID) })
	releaseList := make(canonjson.Array, len(rs))
	for i, r := range rs {
		releaseList[i] = canonjson.UUID(r.accountID)
	}
	ts := slices.SortedFunc(slices.Values(txs), func(a, b transactionRow) int { return compareID(a.id, b.id) })
	txList := make(canonjson.Array, len(ts))
	for i, t := range ts {
		ps := slices.SortedFunc(slices.Values(t.postings), func(a, b postingRow) int { return cmp.Compare(a.index, b.index) })
		postings := make(canonjson.Array, len(ps))
		for j, p := range ps {
			if p.index < 0 {
				return nil, fmt.Errorf("%w: transaction %s posting index %d", ErrCorrupt, canonjson.UUID(t.id), p.index)
			}
			var event uint64 // 0: no event reference
			if p.eventSequence != nil {
				if *p.eventSequence < 0 {
					return nil, fmt.Errorf("%w: transaction %s event sequence %d", ErrCorrupt, canonjson.UUID(t.id), *p.eventSequence)
				}
				event = uint64(*p.eventSequence)
			}
			postings[j] = canonjson.Object{
				"amount":          canonjson.Int(p.amount),
				"creditAccountId": canonjson.UUID(p.credit),
				"debitAccountId":  canonjson.UUID(p.debit),
				"eventSequence":   canonjson.Uint(event),
				"postingIndex":    canonjson.Uint(uint64(p.index)),
			}
		}
		txList[i] = canonjson.Object{
			"kind":          canonjson.String(t.kind),
			"postings":      postings,
			"transactionId": canonjson.UUID(t.id),
		}
	}
	b, err := canonjson.Marshal(canonjson.Object{
		"holds":        holdList,
		"kind":         canonjson.String("ledger"),
		"releases":     releaseList,
		"sequence":     canonjson.Uint(sequence),
		"transactions": txList,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: ledger line %d: %w", ErrCorrupt, sequence, err)
	}
	return b, nil
}

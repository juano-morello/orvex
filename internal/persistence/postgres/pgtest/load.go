package pgtest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// Loaded describes a history inserted by LoadHistory.
type Loaded struct {
	RoundID [16]byte
	// Head is the link hash of the last record.
	Head    chain.Hash
	Records int
	// CommandIDs holds the command ID of record k at index k-1.
	CommandIDs [][16]byte
	// Hashes holds the link hash of record k at index k.
	Hashes []chain.Hash
}

type checkpoint struct {
	sequence  uint64
	stateHash []byte
}

// LoadHistory inserts an LBT-9 history file, format v1 or v2, into a
// migrated database exactly as round creation, admission and execution
// would (spec section 7.3), in one READ COMMITTED transaction. For a v2
// file it also inserts the ledger rows (LBT-18 L-14): the header's accounts
// (reusing an existing row of identical identity), a funding grant from the
// global system account "faucet" of each player account's total holds, the
// holds after the admissions, and per record its releases, transactions and
// postings. checkpoints selects which of the header's checkpoints become
// snapshot rows: nil selects all, an empty slice none; a sequence missing
// from the header fails the test. Any error, including one at commit, fails
// the test.
func LoadHistory(t testing.TB, db *pgxpool.Pool, history []byte, checkpoints []uint64) Loaded {
	t.Helper()
	lines := bytes.Split(bytes.TrimSuffix(history, []byte("\n")), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("pgtest: history has %d lines, want a header and a genesis record", len(lines))
	}
	secret, cps := parseHeader(t, lines[0])
	selected := selectCheckpoints(t, cps, checkpoints)

	records := lines[1:]
	var lg *ledgerFile
	if headerFormat(t, lines[0]) == verification.FormatV2 {
		lg = parseLedgerFile(t, lines[0], records)
		records = records[:len(records)-len(lg.lines)]
	}
	g, ok := decode(t, records[0]).(chain.Genesis)
	if !ok {
		t.Fatal("pgtest: record 0 is not a genesis record")
	}
	n := int64(len(records) - 1)
	out := Loaded{RoundID: g.RoundID, Records: len(records)}

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, postgres.ExecutionTxOptions())
	if err != nil {
		t.Fatalf("pgtest: begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("pgtest: load %s: %v", what, err)
		}
	}

	exec("round", `INSERT INTO orvex.rounds
		(round_id, game, rules_version, schema_version, state_version, chain, created_at_unix_micro)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		g.RoundID, string(g.Game), int64(g.RulesVersion), int64(g.SchemaVersion), int64(g.StateVersion),
		chain.Version, g.CreatedAtUnixMicro)
	exec("secret", `INSERT INTO orvex.round_secrets (round_id, config_secret) VALUES ($1, $2)`, g.RoundID, secret)
	exec("admission head", `INSERT INTO orvex.round_admission_heads (round_id, admitted_sequence) VALUES ($1, $2)`,
		g.RoundID, n)
	exec("execution head", `INSERT INTO orvex.round_execution_heads (round_id, processed_sequence, fencing_token)
		VALUES ($1, $2, 0)`, g.RoundID, n)
	if lg != nil {
		lg.loadAccounts(t, tx)
	}

	cmds := make([]chain.Command, 0, n)
	for i, line := range records[1:] {
		c, ok := decode(t, line).(chain.Command)
		if !ok {
			t.Fatalf("pgtest: record %d is not a command record", i+1)
		}
		cmds = append(cmds, c)
		out.CommandIDs = append(out.CommandIDs, c.CommandID)
		exec("admission", `INSERT INTO orvex.admissions
			(round_id, sequence, command_id, effective_time_unix_micro, command)
			VALUES ($1, $2, $3, $4, $5)`,
			c.RoundID, int64(c.Sequence), c.CommandID, c.EffectiveTimeUnixMicro, c.Payload) //nolint:gosec // G115: a golden history sequence (< 100); a wrapped value would fail the column CHECK
	}
	if lg != nil {
		lg.loadHolds(t, tx, g.RoundID)
	}

	for i, line := range records {
		h := chain.LinkHash(line)
		out.Hashes = append(out.Hashes, h)
		var prev chain.Hash
		var status, reason *string
		if i > 0 {
			c := cmds[i-1]
			prev = c.PrevHash
			status = new("accepted")
			if c.Result.Rejected {
				status, reason = new("rejected"), new(c.Result.Reason)
			}
		}
		exec("chain record", `INSERT INTO orvex.chain_records
			(round_id, sequence, prev_hash, hash, record, result_status, result_reason, fencing_token)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 0)`,
			g.RoundID, int64(i), prev[:], h[:], line, status, reason)
	}
	out.Head = out.Hashes[len(out.Hashes)-1]

	for _, c := range cmds {
		for _, e := range c.Events {
			o, err := canonjson.Unmarshal(e.Payload)
			if err != nil {
				t.Fatalf("pgtest: event %d: %v", e.Sequence, err)
			}
			typ, err := canonjson.Text(o["type"])
			if err != nil {
				t.Fatalf("pgtest: event %d type: %v", e.Sequence, err)
			}
			exec("domain event", `INSERT INTO orvex.domain_events (round_id, event_sequence, sequence, event_type)
				VALUES ($1, $2, $3, $4)`, c.RoundID, int64(e.Sequence), int64(c.Sequence), typ) //nolint:gosec // G115: a golden history sequence (< 100); a wrapped value would fail the column CHECK
		}
	}
	if lg != nil {
		lg.loadRows(t, tx, g.RoundID)
	}

	for _, cp := range selected {
		exec("snapshot", `INSERT INTO orvex.round_snapshots (round_id, sequence, state_hash, state, engine_build)
			VALUES ($1, $2, $3, NULL, 'golden')`, g.RoundID, int64(cp.sequence), cp.stateHash) //nolint:gosec // G115: a golden history sequence (< 100); a wrapped value would fail the column CHECK
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("pgtest: load commit: %v", err)
	}
	return out
}

func decode(t testing.TB, line []byte) any {
	t.Helper()
	rec, err := chain.Decode(line)
	if err != nil {
		t.Fatalf("pgtest: decode record: %v", err)
	}
	return rec
}

// parseHeader returns the canonical secret config bytes and the checkpoints
// of a history header line.
func parseHeader(t testing.TB, line []byte) ([]byte, []checkpoint) {
	t.Helper()
	o, err := canonjson.Unmarshal(line)
	if err != nil {
		t.Fatalf("pgtest: history header: %v", err)
	}
	so, err := canonjson.Obj(o["configSecret"])
	if err != nil {
		t.Fatalf("pgtest: history header configSecret: %v", err)
	}
	secret, err := canonjson.Marshal(so)
	if err != nil {
		t.Fatalf("pgtest: history header configSecret: %v", err)
	}
	list, err := canonjson.List(o["checkpoints"])
	if err != nil {
		t.Fatalf("pgtest: history header checkpoints: %v", err)
	}
	cps := make([]checkpoint, 0, len(list))
	for i, v := range list {
		co, err := canonjson.Obj(v)
		if err != nil {
			t.Fatalf("pgtest: checkpoint %d: %v", i, err)
		}
		seq, err := canonjson.ParseUint(co["sequence"])
		if err != nil {
			t.Fatalf("pgtest: checkpoint %d sequence: %v", i, err)
		}
		h, err := canonjson.ParseHex(co["stateHash"], 32)
		if err != nil {
			t.Fatalf("pgtest: checkpoint %d stateHash: %v", i, err)
		}
		cps = append(cps, checkpoint{sequence: seq, stateHash: h})
	}
	return secret, cps
}

func selectCheckpoints(t testing.TB, cps []checkpoint, want []uint64) []checkpoint {
	t.Helper()
	if want == nil {
		return cps
	}
	out := make([]checkpoint, 0, len(want))
	for _, seq := range want {
		found := false
		for _, cp := range cps {
			if cp.sequence == seq {
				out = append(out, cp)
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("pgtest: checkpoint %d is not in the history header", seq)
		}
	}
	return out
}

// ---------------------------------------------------------------- format v2 (LBT-18 L-14)

// headerFormat returns the "format" of a history header line.
func headerFormat(t testing.TB, line []byte) string {
	t.Helper()
	o, err := canonjson.Unmarshal(line)
	if err != nil {
		t.Fatalf("pgtest: history header: %v", err)
	}
	f, err := canonjson.Text(o["format"])
	if err != nil {
		t.Fatalf("pgtest: history header format: %v", err)
	}
	return f
}

// ledgerAccount is an account of a v2 header account table; roundID is
// zero for a global system account (round_id NULL).
type ledgerAccount struct {
	id      [16]byte
	kind    string
	player  string
	name    string
	roundID [16]byte
}

type ledgerHold struct {
	account [16]byte
	amount  int64
}

type ledgerPosting struct {
	index         int64
	debit, credit [16]byte
	amount        int64
	event         int64 // 0: event_sequence NULL
}

type ledgerTransaction struct {
	id       [16]byte
	kind     string
	postings []ledgerPosting
}

type ledgerLine struct {
	holds        []ledgerHold
	releases     [][16]byte
	transactions []ledgerTransaction
}

// ledgerFile holds the ledger part of a v2 history: the header account
// table and the ledger lines, line j at index j.
type ledgerFile struct {
	accounts []ledgerAccount
	lines    []ledgerLine
}

// fields reads canonjson values and fails the test on a malformed one.
type fields struct{ t testing.TB }

func (f fields) check(what string, err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("pgtest: v2 history %s: %v", what, err)
	}
}

func (f fields) obj(what string, v canonjson.Value) canonjson.Object {
	f.t.Helper()
	o, err := canonjson.Obj(v)
	f.check(what, err)
	return o
}

func (f fields) list(what string, v canonjson.Value) canonjson.Array {
	f.t.Helper()
	a, err := canonjson.List(v)
	f.check(what, err)
	return a
}

func (f fields) text(what string, v canonjson.Value) string {
	f.t.Helper()
	s, err := canonjson.Text(v)
	f.check(what, err)
	return s
}

func (f fields) uuid(what string, v canonjson.Value) [16]byte {
	f.t.Helper()
	id, err := canonjson.ParseUUID(v)
	f.check(what, err)
	return id
}

func (f fields) int(what string, v canonjson.Value) int64 {
	f.t.Helper()
	n, err := canonjson.ParseInt(v)
	f.check(what, err)
	return n
}

// parseLedgerFile splits off the ledger lines that follow the records and
// parses them with the header's account table.
func parseLedgerFile(t testing.TB, header []byte, rest [][]byte) *ledgerFile {
	t.Helper()
	f := fields{t}
	objs := make([]canonjson.Object, len(rest))
	first := -1
	for i, line := range rest {
		o, err := canonjson.Unmarshal(line)
		f.check("line", err)
		objs[i] = o
		if first < 0 && o["kind"] == canonjson.String("ledger") {
			first = i
		}
	}
	if first < 1 {
		t.Fatal("pgtest: v2 history has no records followed by ledger lines")
	}

	lf := &ledgerFile{}
	h, err := canonjson.Unmarshal(header)
	f.check("header", err)
	for _, v := range f.list("accounts", h["accounts"]) {
		o := f.obj("account", v)
		a := ledgerAccount{id: f.uuid("accountId", o["accountId"]), kind: f.text("account kind", o["kind"])}
		switch a.kind {
		case "player":
			a.player = f.text("player", o["player"])
		case "system":
			a.name = f.text("name", o["name"])
			a.roundID = f.uuid("roundId", o["roundId"])
		default:
			t.Fatalf("pgtest: v2 history account kind %q", a.kind)
		}
		lf.accounts = append(lf.accounts, a)
	}

	for j, o := range objs[first:] {
		seq, err := canonjson.ParseUint(o["sequence"])
		f.check("ledger line sequence", err)
		if f.text("ledger line kind", o["kind"]) != "ledger" || seq != uint64(j) {
			t.Fatalf("pgtest: v2 history ledger line %d is not ledger line %d", first+j, j)
		}
		var ll ledgerLine
		for _, v := range f.list("holds", o["holds"]) {
			ho := f.obj("hold", v)
			ll.holds = append(ll.holds, ledgerHold{account: f.uuid("hold accountId", ho["accountId"]), amount: f.int("hold amount", ho["amount"])})
		}
		for _, v := range f.list("releases", o["releases"]) {
			ll.releases = append(ll.releases, f.uuid("release", v))
		}
		for _, v := range f.list("transactions", o["transactions"]) {
			to := f.obj("transaction", v)
			tr := ledgerTransaction{id: f.uuid("transactionId", to["transactionId"]), kind: f.text("transaction kind", to["kind"])}
			for _, pv := range f.list("postings", to["postings"]) {
				po := f.obj("posting", pv)
				tr.postings = append(tr.postings, ledgerPosting{
					index:  f.int("postingIndex", po["postingIndex"]),
					debit:  f.uuid("debitAccountId", po["debitAccountId"]),
					credit: f.uuid("creditAccountId", po["creditAccountId"]),
					amount: f.int("amount", po["amount"]),
					event:  f.int("eventSequence", po["eventSequence"]),
				})
			}
			ll.transactions = append(ll.transactions, tr)
		}
		lf.lines = append(lf.lines, ll)
	}
	return lf
}

func ledgerExec(t testing.TB, tx pgx.Tx, what, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("pgtest: load %s: %v", what, err)
	}
}

// nullUUID is id as a uuid parameter, NULL for the zero ID.
func nullUUID(id [16]byte) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: id != [16]byte{}}
}

// loadAccounts inserts the header's accounts, or requires an existing row
// of identical identity, then funds every player account with its total
// holds in the file from the global system account "faucet".
func (lf *ledgerFile) loadAccounts(t testing.TB, tx pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	for _, a := range lf.accounts {
		var (
			kind         string
			player, name *string
			rid          pgtype.UUID
		)
		err := tx.QueryRow(ctx, `SELECT kind, player_id, name, round_id FROM orvex.ledger_accounts WHERE account_id = $1`, a.id).
			Scan(&kind, &player, &name, &rid)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if a.kind == "player" {
				ledgerExec(t, tx, "player account", `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced)
					VALUES ($1, 'player', $2, true)`, a.id, a.player)
			} else {
				ledgerExec(t, tx, "system account", `INSERT INTO orvex.ledger_accounts (account_id, kind, name, round_id, floor_enforced)
					VALUES ($1, 'system', $2, $3, false)`, a.id, a.name, nullUUID(a.roundID))
			}
			continue
		case err != nil:
			t.Fatalf("pgtest: load account: %v", err)
		}
		var got ledgerAccount
		got.id, got.kind = a.id, kind
		if player != nil {
			got.player = *player
		}
		if name != nil {
			got.name = *name
		}
		if rid.Valid {
			got.roundID = rid.Bytes
		}
		if got != a {
			t.Fatalf("pgtest: account %s exists with another identity", canonjson.UUID(a.id))
		}
	}

	ledgerExec(t, tx, "faucet", `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
		SELECT gen_random_uuid(), 'system', 'faucet', false
		WHERE NOT EXISTS (SELECT 1 FROM orvex.ledger_accounts WHERE kind = 'system' AND name = 'faucet' AND round_id IS NULL)`)
	for _, a := range lf.accounts {
		if a.kind != "player" {
			continue
		}
		var total int64
		for _, ll := range lf.lines {
			for _, h := range ll.holds {
				if h.account == a.id {
					total += h.amount
				}
			}
		}
		if total == 0 {
			continue
		}
		var txID [16]byte
		if err := tx.QueryRow(ctx, `INSERT INTO orvex.ledger_transactions (transaction_id, kind)
			VALUES (gen_random_uuid(), 'grant') RETURNING transaction_id`).Scan(&txID); err != nil {
			t.Fatalf("pgtest: load grant: %v", err)
		}
		ledgerExec(t, tx, "grant posting", `INSERT INTO orvex.ledger_postings
			(transaction_id, posting_index, debit_account_id, credit_account_id, amount)
			SELECT $1, 0, f.account_id, $2, $3 FROM orvex.ledger_accounts f
			WHERE f.kind = 'system' AND f.name = 'faucet' AND f.round_id IS NULL`, txID, a.id, total)
	}
}

// loadHolds inserts every hold; the admissions exist.
func (lf *ledgerFile) loadHolds(t testing.TB, tx pgx.Tx, roundID [16]byte) {
	t.Helper()
	for j, ll := range lf.lines {
		for _, h := range ll.holds {
			ledgerExec(t, tx, "hold", `INSERT INTO orvex.ledger_holds (round_id, sequence, account_id, amount)
				VALUES ($1, $2, $3, $4)`, roundID, int64(j), h.account, h.amount)
		}
	}
}

// loadRows inserts, per record in sequence order, its releases, then its
// transactions and their postings; the chain records and domain events
// exist. Releasing first keeps the account floor satisfied.
func (lf *ledgerFile) loadRows(t testing.TB, tx pgx.Tx, roundID [16]byte) {
	t.Helper()
	for j, ll := range lf.lines {
		for _, id := range ll.releases {
			ledgerExec(t, tx, "hold release", `INSERT INTO orvex.ledger_hold_releases (round_id, sequence, account_id)
				VALUES ($1, $2, $3)`, roundID, int64(j), id)
		}
		for _, tr := range ll.transactions {
			ledgerExec(t, tx, "ledger transaction", `INSERT INTO orvex.ledger_transactions (transaction_id, kind, round_id, sequence)
				VALUES ($1, $2, $3, $4)`, tr.id, tr.kind, roundID, int64(j))
			for _, p := range tr.postings {
				var event *int64 // "0": NULL
				if p.event != 0 {
					event = &p.event
				}
				ledgerExec(t, tx, "ledger posting", `INSERT INTO orvex.ledger_postings
					(transaction_id, posting_index, debit_account_id, credit_account_id, amount, round_id, event_sequence)
					VALUES ($1, $2, $3, $4, $5, $6, $7)`, tr.id, p.index, p.debit, p.credit, p.amount, roundID, event)
			}
		}
	}
}

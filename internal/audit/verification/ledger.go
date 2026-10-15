package verification

import (
	"bytes"
	"errors"
	"fmt"
	"math"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/replay"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/ledger"
)

// ledgerKind is the "kind" of a v2 ledger line (LBT-18 section 5.3).
const ledgerKind = "ledger"

// Account kinds of the v2 header account table (LBT-18 section 5.2).
const (
	accountKindPlayer = "player"
	accountKindSystem = "system"
)

// account is one entry of a v2 header account table.
type account struct {
	id      [16]byte
	system  bool
	player  string   // player accounts
	name    string   // system accounts
	roundID [16]byte // system accounts; zero for a global account
}

// hold is one hold of a ledger line.
type hold struct {
	account [16]byte
	amount  int64
}

// posting is one exported posting of a ledger transaction.
type posting struct {
	index         uint64
	debit, credit [16]byte
	amount        int64
	event         uint64 // 0: no event reference
}

// transaction is one exported ledger transaction.
type transaction struct {
	id       [16]byte
	kind     string
	postings []posting
}

// ledgerLine is the parsed ledger line of one record.
type ledgerLine struct {
	holds        []hold
	releases     [][16]byte
	transactions []transaction
}

// ---------------------------------------------------------------- options

// checkLedgerRules rejects a nil rule and two rules for one (game, rules
// version) (LBT-18 L-7).
func checkLedgerRules(rules []ledger.Rule) error {
	type key struct {
		g game.ID
		v game.RulesVersion
	}
	seen := map[key]bool{}
	for i, r := range rules {
		if r == nil {
			return fmt.Errorf("verification: ledger rule %d is nil", i)
		}
		d := r.Descriptor()
		k := key{d.Game, d.RulesVersion}
		if seen[k] {
			return fmt.Errorf("verification: two ledger rules for %s %d", d.Game, d.RulesVersion)
		}
		seen[k] = true
	}
	return nil
}

// selectLedgerRule returns the rule for d's game and rules version, which
// must report exactly d.
func selectLedgerRule(rules []ledger.Rule, d game.Descriptor) (ledger.Rule, error) {
	for _, r := range rules {
		rd := r.Descriptor()
		if rd.Game != d.Game || rd.RulesVersion != d.RulesVersion {
			continue
		}
		if rd != d {
			return nil, fmt.Errorf("verification: ledger rule for %s %d has schema %d state %d; rules have schema %d state %d",
				d.Game, d.RulesVersion, rd.SchemaVersion, rd.StateVersion, d.SchemaVersion, d.StateVersion)
		}
		return r, nil
	}
	return nil, fmt.Errorf("verification: no ledger rule for %s %d", d.Game, d.RulesVersion)
}

// ---------------------------------------------------------------- format (syntax)

// validName reports whether s matches ^[a-z][a-z0-9_]{0,63}$.
func validName(s string) bool {
	if len(s) == 0 || len(s) > 64 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func increasing(prev, next [16]byte) bool {
	return bytes.Compare(prev[:], next[:]) < 0
}

// parseAccounts parses the v2 header account table (LBT-18 section 5.2).
func parseAccounts(v canonjson.Value) ([]account, error) {
	arr, err := canonjson.List(v)
	if err != nil {
		return nil, err
	}
	out := make([]account, 0, len(arr))
	for i, x := range arr {
		a, err := parseAccount(x)
		if err != nil {
			return nil, fmt.Errorf("account %d: %w", i, err)
		}
		if i > 0 && !increasing(out[i-1].id, a.id) {
			return nil, fmt.Errorf("account %d: accountId does not strictly increase", i)
		}
		out = append(out, a)
	}
	return out, nil
}

func parseAccount(v canonjson.Value) (account, error) {
	o, err := canonjson.Obj(v)
	if err != nil {
		return account{}, err
	}
	kind, err := canonjson.Text(o["kind"])
	if err != nil {
		return account{}, fmt.Errorf("kind: %w", err)
	}
	var a account
	switch kind {
	case accountKindPlayer:
		if _, err := canonjson.Fields(o, "accountId", "kind", "player"); err != nil {
			return account{}, err
		}
		if a.player, err = canonjson.Text(o["player"]); err != nil {
			return account{}, fmt.Errorf("player: %w", err)
		}
		if a.player == "" {
			return account{}, errors.New("player: empty")
		}
	case accountKindSystem:
		if _, err := canonjson.Fields(o, "accountId", "kind", "name", "roundId"); err != nil {
			return account{}, err
		}
		a.system = true
		if a.name, err = canonjson.Text(o["name"]); err != nil {
			return account{}, fmt.Errorf("name: %w", err)
		}
		if !validName(a.name) {
			return account{}, errors.New("name: not a valid account name")
		}
		if a.roundID, err = canonjson.ParseUUID(o["roundId"]); err != nil {
			return account{}, fmt.Errorf("roundId: %w", err)
		}
	default:
		return account{}, errors.New("kind: neither player nor system")
	}
	if a.id, err = canonjson.ParseUUID(o["accountId"]); err != nil {
		return account{}, fmt.Errorf("accountId: %w", err)
	}
	return a, nil
}

// parseLedgerLine parses a ledger line whose kind is "ledger" (LBT-18
// section 5.3) and returns it with its sequence.
func parseLedgerLine(o canonjson.Object) (ledgerLine, uint64, error) {
	if _, err := canonjson.Fields(o, "holds", "kind", "releases", "sequence", "transactions"); err != nil {
		return ledgerLine{}, 0, fmt.Errorf("verification: ledger line: %w", err)
	}
	seq, err := canonjson.ParseUint(o["sequence"])
	if err != nil {
		return ledgerLine{}, 0, fmt.Errorf("verification: ledger line sequence: %w", err)
	}
	var ll ledgerLine
	if ll.holds, err = parseHolds(o["holds"]); err != nil {
		return ledgerLine{}, 0, fmt.Errorf("verification: ledger line holds: %w", err)
	}
	if ll.releases, err = parseReleases(o["releases"]); err != nil {
		return ledgerLine{}, 0, fmt.Errorf("verification: ledger line releases: %w", err)
	}
	if ll.transactions, err = parseTransactions(o["transactions"]); err != nil {
		return ledgerLine{}, 0, fmt.Errorf("verification: ledger line transactions: %w", err)
	}
	return ll, seq, nil
}

func parseHolds(v canonjson.Value) ([]hold, error) {
	arr, err := canonjson.List(v)
	if err != nil {
		return nil, err
	}
	out := make([]hold, 0, len(arr))
	for i, x := range arr {
		o, err := canonjson.Obj(x)
		if err == nil {
			_, err = canonjson.Fields(o, "accountId", "amount")
		}
		var h hold
		if err == nil {
			h.account, err = canonjson.ParseUUID(o["accountId"])
		}
		if err == nil {
			h.amount, err = canonjson.ParseInt(o["amount"])
		}
		if err != nil {
			return nil, fmt.Errorf("hold %d: %w", i, err)
		}
		if i > 0 && !increasing(out[i-1].account, h.account) {
			return nil, fmt.Errorf("hold %d: accountId does not strictly increase", i)
		}
		out = append(out, h)
	}
	return out, nil
}

func parseReleases(v canonjson.Value) ([][16]byte, error) {
	arr, err := canonjson.List(v)
	if err != nil {
		return nil, err
	}
	out := make([][16]byte, 0, len(arr))
	for i, x := range arr {
		id, err := canonjson.ParseUUID(x)
		if err != nil {
			return nil, fmt.Errorf("release %d: %w", i, err)
		}
		if i > 0 && !increasing(out[i-1], id) {
			return nil, fmt.Errorf("release %d: accountId does not strictly increase", i)
		}
		out = append(out, id)
	}
	return out, nil
}

func parseTransactions(v canonjson.Value) ([]transaction, error) {
	arr, err := canonjson.List(v)
	if err != nil {
		return nil, err
	}
	out := make([]transaction, 0, len(arr))
	for i, x := range arr {
		t, err := parseTransaction(x)
		if err != nil {
			return nil, fmt.Errorf("transaction %d: %w", i, err)
		}
		if i > 0 && !increasing(out[i-1].id, t.id) {
			return nil, fmt.Errorf("transaction %d: transactionId does not strictly increase", i)
		}
		out = append(out, t)
	}
	return out, nil
}

func parseTransaction(v canonjson.Value) (transaction, error) {
	o, err := canonjson.Obj(v)
	if err != nil {
		return transaction{}, err
	}
	if _, err := canonjson.Fields(o, "kind", "postings", "transactionId"); err != nil {
		return transaction{}, err
	}
	var t transaction
	if t.kind, err = canonjson.Text(o["kind"]); err != nil {
		return transaction{}, fmt.Errorf("kind: %w", err)
	}
	if !validName(t.kind) {
		return transaction{}, errors.New("kind: not a valid transaction kind")
	}
	if t.id, err = canonjson.ParseUUID(o["transactionId"]); err != nil {
		return transaction{}, fmt.Errorf("transactionId: %w", err)
	}
	arr, err := canonjson.List(o["postings"])
	if err != nil {
		return transaction{}, fmt.Errorf("postings: %w", err)
	}
	t.postings = make([]posting, 0, len(arr))
	for i, x := range arr {
		p, err := parsePosting(x)
		if err != nil {
			return transaction{}, fmt.Errorf("posting %d: %w", i, err)
		}
		if i > 0 && p.index <= t.postings[i-1].index {
			return transaction{}, fmt.Errorf("posting %d: postingIndex does not strictly increase", i)
		}
		t.postings = append(t.postings, p)
	}
	return t, nil
}

func parsePosting(v canonjson.Value) (posting, error) {
	o, err := canonjson.Obj(v)
	if err != nil {
		return posting{}, err
	}
	if _, err := canonjson.Fields(o, "amount", "creditAccountId", "debitAccountId", "eventSequence", "postingIndex"); err != nil {
		return posting{}, err
	}
	var p posting
	if p.amount, err = canonjson.ParseInt(o["amount"]); err != nil {
		return posting{}, fmt.Errorf("amount: %w", err)
	}
	if p.credit, err = canonjson.ParseUUID(o["creditAccountId"]); err != nil {
		return posting{}, fmt.Errorf("creditAccountId: %w", err)
	}
	if p.debit, err = canonjson.ParseUUID(o["debitAccountId"]); err != nil {
		return posting{}, fmt.Errorf("debitAccountId: %w", err)
	}
	if p.event, err = canonjson.ParseUint(o["eventSequence"]); err != nil {
		return posting{}, fmt.Errorf("eventSequence: %w", err)
	}
	if p.index, err = canonjson.ParseUint(o["postingIndex"]); err != nil {
		return posting{}, fmt.Errorf("postingIndex: %w", err)
	}
	if p.index > math.MaxInt32 {
		return posting{}, errors.New("postingIndex: above 2147483647")
	}
	return p, nil
}

// ---------------------------------------------------------------- ledger stage

// checkLedger runs the ledger stage (LBT-18 section 6.5) over a v2 history
// whose records replayed: records[i-1] is record i with its decoded events.
func checkLedger(h *history, records []replay.Record, rule ledger.Rule) (*LedgerReport, error) {
	rid := h.records[0].(chain.Genesis).RoundID
	houseID, roundAcct := ledger.HouseAccountID(), ledger.RoundAccountID(rid)
	table := h.header.accounts

	// A. Header.
	byID := make(map[[16]byte]account, len(table))
	byPlayer := map[string][16]byte{}
	for _, a := range table {
		byID[a.id] = a
		if a.system {
			continue
		}
		if _, dup := byPlayer[a.player]; dup {
			return nil, fail(StageLedger, HeaderSequence, errors.New("verification: two accounts of one player"))
		}
		byPlayer[a.player] = a.id
	}
	type scope struct {
		name  string
		round [16]byte
	}
	scopes := map[scope]bool{}
	for _, a := range table {
		if !a.system {
			continue
		}
		s := scope{a.name, a.roundID}
		if scopes[s] {
			return nil, fail(StageLedger, HeaderSequence, fmt.Errorf("verification: two system accounts named %q in one scope", a.name))
		}
		scopes[s] = true
	}
	var global [16]byte
	for _, a := range table {
		isHouse := a.system && a.name == ledger.HouseAccountName && a.roundID == global
		isRound := a.system && a.name == ledger.RoundAccountName && a.roundID == rid
		switch {
		case a.system && a.name == ledger.HouseAccountName && (a.id != houseID || a.roundID != global):
			return nil, fail(StageLedger, HeaderSequence, errors.New("verification: house account has the wrong id or scope"))
		case a.system && a.name == ledger.RoundAccountName && (a.roundID != rid || a.id != roundAcct):
			return nil, fail(StageLedger, HeaderSequence, errors.New("verification: round account has the wrong id or round"))
		case a.id == houseID && !isHouse:
			return nil, fail(StageLedger, HeaderSequence, errors.New("verification: the house account id names another account"))
		case a.id == roundAcct && !isRound:
			return nil, fail(StageLedger, HeaderSequence, errors.New("verification: the round account id names another account"))
		}
	}

	// B. Records.
	rep := &LedgerReport{Accounts: len(table)}
	referenced := map[[16]byte]bool{}
	seen := map[uint64]bool{}
	next := uint64(1) // f_i
	for i, ll := range h.ledger {
		pos := int64(i)
		rep.Holds += len(ll.holds)
		rep.Releases += len(ll.releases)
		rep.Transactions += len(ll.transactions)
		for _, hd := range ll.holds {
			referenced[hd.account] = true
		}
		for _, id := range ll.releases {
			referenced[id] = true
		}
		for _, t := range ll.transactions {
			rep.Postings += len(t.postings)
			for _, p := range t.postings {
				referenced[p.debit] = true
				referenced[p.credit] = true
			}
		}
		// B1.
		if i == 0 {
			if len(ll.holds) != 0 || len(ll.releases) != 0 || len(ll.transactions) != 0 {
				return nil, fail(StageLedger, 0, errors.New("verification: genesis ledger line has rows"))
			}
			continue
		}
		events := records[i-1].Events
		first := next
		next += uint64(len(events))
		if err := checkLedgerRows(ll, byID, first, next, seen); err != nil {
			return nil, fail(StageLedger, pos, err)
		}
		if err := checkPostings(ll, rule, events, first, rid, uint64(i), byID, byPlayer); err != nil {
			return nil, fail(StageLedger, pos, err)
		}
	}

	// C. Closed table.
	for _, a := range table {
		if !referenced[a.id] {
			return nil, fail(StageLedger, HeaderSequence, fmt.Errorf("verification: account %s is not referenced", canonjson.UUID(a.id)))
		}
	}
	return rep, nil
}

// checkLedgerRows runs checks B2 to B6 on the exported rows of a command
// record whose events have sequences [first, end).
func checkLedgerRows(ll ledgerLine, byID map[[16]byte]account, first, end uint64, seen map[uint64]bool) error {
	known := func(id [16]byte) error {
		if _, ok := byID[id]; !ok {
			return fmt.Errorf("verification: account %s is not in the account table", canonjson.UUID(id))
		}
		return nil
	}
	// B2.
	for _, hd := range ll.holds {
		if err := known(hd.account); err != nil {
			return err
		}
	}
	for _, id := range ll.releases {
		if err := known(id); err != nil {
			return err
		}
	}
	for _, t := range ll.transactions {
		for _, p := range t.postings {
			if err := known(p.debit); err != nil {
				return err
			}
			if err := known(p.credit); err != nil {
				return err
			}
		}
	}
	// B3.
	for _, hd := range ll.holds {
		if hd.amount < 1 {
			return errors.New("verification: hold amount below 1")
		}
	}
	// B4.
	if len(ll.releases) != len(ll.holds) {
		return errors.New("verification: released accounts differ from held accounts")
	}
	for k, hd := range ll.holds {
		if ll.releases[k] != hd.account {
			return errors.New("verification: released accounts differ from held accounts")
		}
	}
	// B5.
	for _, t := range ll.transactions {
		for _, p := range t.postings {
			if p.amount < 1 || p.debit == p.credit {
				return errors.New("verification: posting does not balance")
			}
		}
	}
	// B6.
	for _, t := range ll.transactions {
		for _, p := range t.postings {
			if p.event < first || p.event >= end {
				return fmt.Errorf("verification: posting references event %d outside the record", p.event)
			}
			if seen[p.event] {
				return fmt.Errorf("verification: event %d is posted twice", p.event)
			}
			seen[p.event] = true
		}
	}
	return nil
}

// checkPostings runs checks B7 to B9: it recomputes record seq's postings
// with rule, resolves their accounts and compares them with the exported
// transactions.
func checkPostings(ll ledgerLine, rule ledger.Rule, events []any, first uint64, rid [16]byte, seq uint64,
	byID map[[16]byte]account, byPlayer map[string][16]byte) error {
	// B7.
	kind, ps, err := rule.Postings(events, first)
	if err != nil {
		return fmt.Errorf("verification: posting rule: %w", err)
	}
	if (kind == "") != (len(ps) == 0) {
		return errors.New("verification: posting rule returned a kind without postings or postings without a kind")
	}
	end := first + uint64(len(events))
	ruleEvents := map[uint64]bool{}
	for _, p := range ps {
		if p.Amount < 1 {
			return errors.New("verification: posting rule returned an amount below 1")
		}
		if p.EventSequence < first || p.EventSequence >= end || ruleEvents[p.EventSequence] {
			return errors.New("verification: posting rule returned a bad event sequence")
		}
		ruleEvents[p.EventSequence] = true
		if !p.Debit.Valid() || !p.Credit.Valid() {
			return errors.New("verification: posting rule returned an invalid account")
		}
		if p.FromHold && p.Debit.Kind != ledger.AccountPlayer {
			return errors.New("verification: posting rule consumes a hold of a system account")
		}
	}

	// B8.
	resolve := func(a ledger.Account, fromHold bool, amount int64) ([16]byte, error) {
		switch a.Kind {
		case ledger.AccountRound:
			return ledger.RoundAccountID(rid), nil
		case ledger.AccountHouse:
			return ledger.HouseAccountID(), nil
		}
		if fromHold {
			if len(ll.holds) != 1 {
				return [16]byte{}, fmt.Errorf("verification: a bet needs exactly one hold, record has %d", len(ll.holds))
			}
			hd := ll.holds[0]
			if acc := byID[hd.account]; acc.system || acc.player != a.Player {
				return [16]byte{}, errors.New("verification: the hold is not on the bet player's account")
			}
			if hd.amount != amount {
				return [16]byte{}, errors.New("verification: the hold amount differs from the bet")
			}
			return hd.account, nil
		}
		id, ok := byPlayer[a.Player]
		if !ok {
			return [16]byte{}, errors.New("verification: the player has no account in the account table")
		}
		return id, nil
	}
	want := make([]posting, 0, len(ps))
	for j, p := range ps {
		debit, err := resolve(p.Debit, p.FromHold, p.Amount)
		if err != nil {
			return err
		}
		credit, err := resolve(p.Credit, false, p.Amount)
		if err != nil {
			return err
		}
		if debit == credit {
			return errors.New("verification: recomputed posting does not balance")
		}
		want = append(want, posting{index: uint64(j), debit: debit, credit: credit, amount: p.Amount, event: p.EventSequence})
	}

	// B9.
	if len(want) == 0 {
		if len(ll.transactions) != 0 {
			return errors.New("verification: transactions: want none")
		}
		return nil
	}
	if len(ll.transactions) != 1 {
		return fmt.Errorf("verification: transactions: %d, want 1", len(ll.transactions))
	}
	t := ll.transactions[0]
	switch {
	case t.id != ledger.TransactionID(rid, seq):
		return errors.New("verification: transactionId differs")
	case t.kind != kind:
		return errors.New("verification: transaction kind differs")
	case len(t.postings) != len(want):
		return fmt.Errorf("verification: postings: %d, want %d", len(t.postings), len(want))
	}
	for j, w := range want {
		got := t.postings[j]
		switch {
		case got.index != w.index:
			return fmt.Errorf("verification: posting %d postingIndex differs", j)
		case got.debit != w.debit:
			return fmt.Errorf("verification: posting %d debitAccountId differs", j)
		case got.credit != w.credit:
			return fmt.Errorf("verification: posting %d creditAccountId differs", j)
		case got.amount != w.amount:
			return fmt.Errorf("verification: posting %d amount differs", j)
		case got.event != w.event:
			return fmt.Errorf("verification: posting %d eventSequence differs", j)
		}
	}
	return nil
}

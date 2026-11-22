//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
)

// fault is an injected failure of one admission transaction.
type fault uint8

const (
	faultNone fault = iota
	// faultCommitThenEOF runs the real COMMIT, then reports
	// io.ErrUnexpectedEOF, as a connection lost after the commit would.
	faultCommitThenEOF
	// faultLockTimeout fails the admission call with SQLSTATE 55P03
	// without running it.
	faultLockTimeout
	// faultSerialization fails the admission call with SQLSTATE 40001
	// without running it.
	faultSerialization
	// faultCanceled fails the admission call with context.Canceled
	// without running it.
	faultCanceled
)

// faultDB wraps the sequencer's pool. The admission transactions begun
// after arm get its faults in order; read transactions and every other
// admission transaction pass through.
type faultDB struct {
	db admission.DB

	mu    sync.Mutex
	armed []fault
}

func (f *faultDB) arm(faults ...fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = append(f.armed[:0], faults...)
}

func (f *faultDB) unused() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.armed)
}

func (f *faultDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	if opts != postgres.AdmissionTxOptions() {
		return f.db.BeginTx(ctx, opts)
	}
	f.mu.Lock()
	fl := faultNone
	if len(f.armed) > 0 {
		fl, f.armed = f.armed[0], f.armed[1:]
	}
	f.mu.Unlock()
	tx, err := f.db.BeginTx(ctx, opts)
	if err != nil || fl == faultNone {
		return tx, err
	}
	return &faultTx{Tx: tx, fault: fl}, nil
}

type faultTx struct {
	pgx.Tx
	fault fault
}

func (tx *faultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	switch tx.fault {
	case faultLockTimeout:
		return errRow{&pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout (injected)"}}
	case faultSerialization:
		return errRow{&pgconn.PgError{Code: "40001", Message: "could not serialize access (injected)"}}
	case faultCanceled:
		return errRow{fmt.Errorf("admit (injected): %w", context.Canceled)}
	default:
		return tx.Tx.QueryRow(ctx, sql, args...)
	}
}

func (tx *faultTx) Commit(ctx context.Context) error {
	if tx.fault != faultCommitThenEOF {
		return tx.Tx.Commit(ctx)
	}
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// storedAdmission is a row of orvex.admissions.
type storedAdmission struct {
	sequence, effTime int64
}

func (s *stack) admissions(t *testing.T, round [16]byte) []storedAdmission {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT sequence, effective_time_unix_micro FROM orvex.admissions WHERE round_id = $1 ORDER BY sequence`, round)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (storedAdmission, error) {
		var a storedAdmission
		err := r.Scan(&a.sequence, &a.effTime)
		return a, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (s *stack) int(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// requireReceipt requires r to be a 202 receipt for client in round at the
// stored admission a.
func requireReceipt(t *testing.T, what string, r reply, round, client [16]byte, a storedAdmission, duplicate bool) {
	t.Helper()
	want := map[string]any{
		"clientCommandId":        uuidText(client),
		"roundId":                uuidText(round),
		"sequence":               fmt.Sprint(a.sequence),
		"effectiveTimeUnixMicro": fmt.Sprint(a.effTime),
		"duplicate":              duplicate,
		"status":                 "admitted",
	}
	if r.status != 202 || !maps.Equal(r.body, want) {
		t.Fatalf("%s = %d %v, want 202 %v", what, r.status, r.body, want)
	}
}

// TestSubmitRetryAfterAmbiguousCommit loses each cash-out's first COMMIT
// acknowledgment after the commit, then fails the sequencer's later
// attempts in a way S-12 classifies as busy, unavailable or canceled. The
// gateway must answer outcome_unknown, and the identical retry must return
// the one durable admission with its original sequence and effective
// time, before execution and after the round is sealed. The control fails
// dave's first attempt before any COMMIT, so that retry is a new
// admission.
func TestSubmitRetryAfterAmbiguousCommit(t *testing.T) {
	s := newStack(t)
	round, _, d := s.createRound(t, 3*time.Second, 6*time.Second)
	players := []struct {
		name, token, player string
		faults              []fault
		committed           bool
	}{
		{"lock timeout after an ambiguous commit", aliceToken, "alice", []fault{faultCommitThenEOF, faultLockTimeout}, true},
		{"serialization failures after an ambiguous commit", bobToken, "bob", []fault{faultCommitThenEOF, faultSerialization, faultSerialization}, true},
		{"cancellation after an ambiguous commit", carolToken, "carol", []fault{faultCommitThenEOF, faultCanceled}, true},
		{"lock timeout before any commit", daveToken, "dave", []fault{faultLockTimeout}, false},
	}
	for _, p := range players {
		roundtest.FundPlayer(t, s.pool, p.player, 1000)
		if r := s.submit(t, p.token, round, randomUUID(t), placeBet("100")); r.status != 202 {
			t.Fatalf("%s's bet = %d %v", p.player, r.status, r.body)
		}
	}
	s.drain(t, round, len(players))
	start, err := s.system(t, round, v1.StartRound{Authority: v1.AuthoritySystem})
	if err != nil {
		t.Fatal(err)
	}
	s.drain(t, round, 1)
	s.waitDBClock(t, start.EffectiveTimeUnixMicro+1)

	type cashOutAttempt struct {
		client  [16]byte
		receipt reply
		stored  storedAdmission
	}
	attempts := make([]cashOutAttempt, len(players))
	for i, p := range players {
		client := randomUUID(t)
		before := s.admissions(t, round)
		s.faults.arm(p.faults...)
		r := s.submit(t, p.token, round, client, cashOut)
		if n := s.faults.unused(); n != 0 {
			t.Fatalf("%s: %d injected faults unused", p.name, n)
		}
		if r.status != 503 || r.body["code"] != "outcome_unknown" {
			t.Errorf("%s: cash-out = %d %v, want 503 outcome_unknown", p.name, r.status, r.body)
		}
		after := s.admissions(t, round)
		if p.committed != (len(after) == len(before)+1) || len(after) > len(before)+1 {
			t.Fatalf("%s: %d admissions after the failed submission, had %d; committed %v", p.name, len(after), len(before), p.committed)
		}

		s.waitDBClock(t, s.dbNow(t)+50_000)
		retry := s.submit(t, p.token, round, client, cashOut)
		stored := s.admissions(t, round)
		if len(stored) != len(before)+1 {
			t.Fatalf("%s: %d admissions after the retry, had %d; want exactly one durable admission", p.name, len(stored), len(before))
		}
		last := stored[len(stored)-1]
		if p.committed && last != after[len(after)-1] {
			t.Fatalf("%s: retry stored %+v, want the first commit %+v", p.name, last, after[len(after)-1])
		}
		requireReceipt(t, p.name+": retry", retry, round, client, last, p.committed)
		attempts[i] = cashOutAttempt{client: client, receipt: retry, stored: last}
	}
	s.drain(t, round, len(players))

	payouts := make([]string, len(players))
	for i, p := range players {
		look := s.call(t, "GET", "/v1/commands/"+uuidText(attempts[i].client), p.token, "")
		events, _ := look.body["events"].([]any)
		if look.status != 200 || look.body["status"] != "accepted" || look.body["sequence"] != attempts[i].receipt.body["sequence"] || len(events) != 1 {
			t.Fatalf("%s: lookup = %d %v, want the accepted cash-out", p.name, look.status, look.body)
		}
		ev, _ := events[0].(map[string]any)
		payouts[i], _ = ev["payoutCredits"].(string)
		if ev["type"] != "BetCashedOut" || ev["cashedOutAtUnixMicro"] != fmt.Sprint(attempts[i].stored.effTime) || payouts[i] == "" {
			t.Errorf("%s: cash-out event %v, want BetCashedOut at the admission's effective time %d", p.name, ev, attempts[i].stored.effTime)
		}
	}

	if _, err := s.system(t, round, v1.CloseRound{Authority: v1.AuthoritySystem}); err != nil {
		if !errors.Is(err, admission.ErrClosureNotDue) {
			t.Fatal(err)
		}
		s.waitDBClock(t, start.EffectiveTimeUnixMicro+d)
		if _, err := s.system(t, round, v1.CloseRound{Authority: v1.AuthoritySystem}); err != nil {
			t.Fatal(err)
		}
	}
	s.drain(t, round, 1)
	if _, err := s.system(t, round, v1.SettleRound{Authority: v1.AuthoritySystem}); err != nil {
		t.Fatal(err)
	}
	s.drain(t, round, 1)

	for i, p := range players {
		sealed := s.submit(t, p.token, round, attempts[i].client, cashOut)
		requireReceipt(t, p.name+": retry after settlement", sealed, round, attempts[i].client, attempts[i].stored, true)
		const paid = `SELECT count(*) FROM orvex.ledger_postings p
			JOIN orvex.ledger_transactions t USING (transaction_id)
			JOIN orvex.ledger_accounts a ON a.account_id = p.credit_account_id
			WHERE t.round_id = $1 AND t.kind = 'crash_cash_out' AND a.player_id = $2`
		if n := s.int(t, paid, round, p.player); n != 1 {
			t.Errorf("%s: %d cash-out payouts, want 1", p.name, n)
		}
		balance := s.int(t, `SELECT credits_posted - debits_posted FROM orvex.ledger_accounts WHERE player_id = $1`, p.player)
		if want := 1000 - 100 + mustInt(t, payouts[i]); balance != want {
			t.Errorf("%s: balance %d, want %d", p.name, balance, want)
		}
	}
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	return n
}

// TestSubmitMalformedWritesNothing sends refused submissions for a funded
// player in an open round. None may reach admission: the round keeps no
// admission and the player no hold, and the next valid bet is sequence 1.
func TestSubmitMalformedWritesNothing(t *testing.T) {
	s := newStack(t)
	round, _, _ := s.createRound(t, time.Second, 3*time.Second)
	roundtest.FundPlayer(t, s.pool, "alice", 1000)
	path := "/v1/rounds/" + uuidText(round) + "/commands"
	client := uuidText(randomUUID(t))
	id := `"clientCommandId":"` + client + `"`
	for _, tc := range []struct {
		name, method, body string
		status             int
		code               string
	}{
		{"repeated id", "POST", `{` + id + `,"clientCommandId":"` + uuidText(randomUUID(t)) + `",` + placeBet("100") + `}`, 400, "invalid_request"},
		{"repeated credits", "POST", `{` + id + `,"placeBet":{"betCredits":"1","betCredits":"100"}}`, 400, "invalid_request"},
		{"mixed-case key", "POST", `{"ClientCommandId":"` + client + `",` + placeBet("100") + `}`, 400, "invalid_request"},
		{"unknown nested field", "POST", `{` + id + `,"placeBet":{"betCredits":"100","player":"bob"}}`, 400, "invalid_request"},
		{"oversized", "POST", `{` + id + `,` + placeBet("100") + strings.Repeat(" ", 5000) + `}`, 413, "request_too_large"},
		{"wrong method", "PUT", `{` + id + `,` + placeBet("100") + `}`, 405, "method_not_allowed"},
	} {
		if r := s.call(t, tc.method, path, aliceToken, tc.body); r.status != tc.status || r.body["code"] != tc.code || len(r.body) != 1 {
			t.Errorf("%s: %d %v, want %d %s", tc.name, r.status, r.body, tc.status, tc.code)
		}
	}
	if n := len(s.admissions(t, round)); n != 0 {
		t.Errorf("%d admissions after refused submissions, want 0", n)
	}
	if n := s.int(t, `SELECT count(*) FROM orvex.ledger_holds`); n != 0 {
		t.Errorf("%d ledger holds after refused submissions, want 0", n)
	}
	bet := s.submit(t, aliceToken, round, randomUUID(t), placeBet("100"))
	if bet.status != 202 || bet.body["sequence"] != "1" || bet.body["status"] != "admitted" || len(bet.body) != 6 {
		t.Errorf("valid bet = %d %v, want the 202 receipt for admission 1", bet.status, bet.body)
	}
}

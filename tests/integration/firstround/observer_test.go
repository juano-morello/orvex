//go:build integration && acceptance

package firstround

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/audit/chain"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

// observer reads what no client can see. It never writes.
type observer struct {
	t  *testing.T
	db *pgxpool.Pool
}

// storedRecord is one chain record of a crash/v1 round, decoded.
type storedRecord struct {
	Sequence int64
	Hash     chain.Hash
	Genesis  *chain.Genesis
	Record   chain.Command
	Command  v1.Command
	Events   []v1.Event
}

func eventType(e v1.Event) string { return reflect.TypeOf(e).Name() }

func (r storedRecord) types() []string {
	out := make([]string, len(r.Events))
	for i, e := range r.Events {
		out[i] = eventType(e)
	}
	return out
}

// records returns the round's chain records in sequence order. It checks
// each stored hash against the record bytes.
func (o observer) records(round [16]byte) []storedRecord {
	o.t.Helper()
	rows, err := o.db.Query(context.Background(),
		`SELECT sequence, hash, record FROM orvex.chain_records WHERE round_id = $1 ORDER BY sequence`, round)
	if err != nil {
		o.t.Fatal(err)
	}
	defer rows.Close()
	var out []storedRecord
	for rows.Next() {
		var (
			seq        int64
			hash, line []byte
		)
		if err := rows.Scan(&seq, &hash, &line); err != nil {
			o.t.Fatal(err)
		}
		r := storedRecord{Sequence: seq}
		copy(r.Hash[:], hash)
		if lh := chain.LinkHash(line); !bytes.Equal(lh[:], hash) {
			o.t.Fatalf("record %d: stored hash differs from the record's link hash", seq)
		}
		rec, err := chain.Decode(line)
		if err != nil {
			o.t.Fatalf("record %d: %v", seq, err)
		}
		switch rec := rec.(type) {
		case chain.Genesis:
			r.Genesis = &rec
		case chain.Command:
			r.Record = rec
			if r.Command, err = (v1.Codec{}).DecodeCommand(rec.Payload); err != nil {
				o.t.Fatalf("record %d command: %v", seq, err)
			}
			for _, e := range rec.Events {
				ev, err := (v1.Codec{}).DecodeEvent(e.Payload)
				if err != nil {
					o.t.Fatalf("record %d event: %v", seq, err)
				}
				r.Events = append(r.Events, ev)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		o.t.Fatal(err)
	}
	return out
}

// admitted is one stored admission, decoded.
type admitted struct {
	Sequence      int64
	CommandID     [16]byte
	EffectiveTime int64
	Command       v1.Command
}

func (o observer) admissions(round [16]byte) []admitted {
	o.t.Helper()
	rows, err := o.db.Query(context.Background(),
		`SELECT sequence, command_id, effective_time_unix_micro, command FROM orvex.admissions WHERE round_id = $1 ORDER BY sequence`, round)
	if err != nil {
		o.t.Fatal(err)
	}
	defer rows.Close()
	var out []admitted
	for rows.Next() {
		var (
			a   admitted
			cmd []byte
		)
		if err := rows.Scan(&a.Sequence, &a.CommandID, &a.EffectiveTime, &cmd); err != nil {
			o.t.Fatal(err)
		}
		if a.Command, err = (v1.Codec{}).DecodeCommand(cmd); err != nil {
			o.t.Fatalf("admission %d: %v", a.Sequence, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		o.t.Fatal(err)
	}
	return out
}

func (o observer) int(sql string, args ...any) int64 {
	o.t.Helper()
	var n int64
	if err := o.db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		o.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (o observer) now() int64 {
	o.t.Helper()
	return o.int(`SELECT orvex.unix_micro(clock_timestamp())`)
}

// waitClock waits until the database clock, the clock of every effective
// time, reaches at.
func (o observer) waitClock(at int64) {
	o.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for o.now() < at {
		if time.Now().After(deadline) {
			o.t.Fatalf("database clock did not reach %d", at)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// account is a ledger account's balance (credits minus debits) and hold.
type account struct{ balance, held int64 }

func (o observer) player(name string) account {
	o.t.Helper()
	var a account
	if err := o.db.QueryRow(context.Background(),
		`SELECT credits_posted - debits_posted, held FROM orvex.ledger_accounts WHERE kind = 'player' AND player_id = $1`, name).
		Scan(&a.balance, &a.held); err != nil {
		o.t.Fatalf("account of %s: %v", name, err)
	}
	return a
}

func (o observer) accountByID(id [16]byte) account {
	o.t.Helper()
	var a account
	if err := o.db.QueryRow(context.Background(),
		`SELECT credits_posted - debits_posted, held FROM orvex.ledger_accounts WHERE account_id = $1`, id).
		Scan(&a.balance, &a.held); err != nil {
		o.t.Fatalf("account %x: %v", id, err)
	}
	return a
}

func (o observer) requirePlayer(name string, balance, held int64) {
	o.t.Helper()
	if got := o.player(name); got != (account{balance, held}) {
		o.t.Errorf("%s: balance %d held %d, want balance %d held %d", name, got.balance, got.held, balance, held)
	}
}

// requireSettledLedger checks the ledger after every round in the database
// settled: nothing held, every round account empty, the house holding
// Σ bets − Σ payouts, and postings balanced.
func (o observer) requireSettledLedger(rounds [][16]byte, houseBalance int64) {
	o.t.Helper()
	if n := o.int(`SELECT count(*) FROM orvex.ledger_accounts WHERE held <> 0`); n != 0 {
		o.t.Errorf("%d accounts still hold credits", n)
	}
	for _, r := range rounds {
		if got := o.accountByID(ledger.RoundAccountID(r)); got.balance != 0 {
			o.t.Errorf("round %x account balance %d, want 0", r, got.balance)
		}
	}
	if got := o.accountByID(ledger.HouseAccountID()); got.balance != houseBalance {
		o.t.Errorf("house balance %d, want Σ bets − Σ payouts = %d", got.balance, houseBalance)
	}
	if n := o.int(`SELECT coalesce(sum(credits_posted - debits_posted), 0) FROM orvex.ledger_accounts`); n != 0 {
		o.t.Errorf("ledger accounts sum to %d, want 0", n)
	}
}

// settled reports whether the round's secret config was revealed, which
// the record that settles the round writes.
func (o observer) settled(round [16]byte) bool {
	o.t.Helper()
	return o.int(`SELECT count(*) FROM orvex.round_secret_reveals WHERE round_id = $1`, round) == 1
}

func (o observer) admissionState(round [16]byte) string {
	o.t.Helper()
	var s string
	if err := o.db.QueryRow(context.Background(),
		`SELECT admission_state FROM orvex.round_admission_heads WHERE round_id = $1`, round).Scan(&s); err != nil {
		o.t.Fatal(err)
	}
	return s
}

// payoutsTo returns the amounts of the round's postings that credit the
// player.
func (o observer) payoutsTo(round [16]byte, name string) []int64 {
	o.t.Helper()
	rows, err := o.db.Query(context.Background(), `SELECT p.amount FROM orvex.ledger_postings p
		JOIN orvex.ledger_transactions x USING (transaction_id)
		JOIN orvex.ledger_accounts a ON a.account_id = p.credit_account_id
		WHERE x.round_id = $1 AND a.kind = 'player' AND a.player_id = $2`, round, name)
	if err != nil {
		o.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			o.t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		o.t.Fatal(err)
	}
	return out
}

// seed unseals the round's secret config with the test keyring, which
// sealed it, and returns its seed whether or not settlement revealed it.
// The seed must match the genesis commitment.
func (o observer) seed(round [16]byte) [32]byte {
	o.t.Helper()
	var sealed sealing.Sealed
	if err := o.db.QueryRow(context.Background(),
		`SELECT secret_scheme, secret_key_id, secret_nonce, secret_ciphertext FROM orvex.round_secrets WHERE round_id = $1`, round).
		Scan(&sealed.Scheme, &sealed.KeyID, &sealed.Nonce, &sealed.Ciphertext); err != nil {
		o.t.Fatalf("sealed secret of round %x: %v", round, err)
	}
	plain, err := pgtest.Keyring().Open(round, sealed)
	if err != nil {
		o.t.Fatalf("unseal the secret of round %x: %v", round, err)
	}
	records := o.records(round)
	if len(records) == 0 || records[0].Genesis == nil {
		o.t.Fatalf("round %x has no genesis record", round)
	}
	cfg, err := v1.Codec{}.DecodeConfig(round, records[0].Genesis.Config, plain)
	if err != nil {
		o.t.Fatalf("secret config of round %x: %v", round, err)
	}
	return cfg.Seed
}

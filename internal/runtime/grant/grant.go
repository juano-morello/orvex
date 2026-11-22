// Package grant applies operator credit grants (crashctl ledger grant): one
// balanced ledger transaction, outside every round, that moves credits from
// the global faucet system account to a player's account, creating either
// account if it is absent.
//
// A grant is idempotent by its grant key. The ledger transaction ID is
// ledger.GrantTransactionID(key), so the key's primary key entry in
// orvex.ledger_transactions is the serialization point: one key names one
// grant across all players and all time. Applying a grant whose key is
// stored with the same player and credits changes nothing (Existing);
// with another player or credits it changes nothing and fails
// (*KeyReusedError). A failed or interrupted Apply commits nothing or
// commits the whole grant, so rerunning it is always safe and credits land
// exactly once.
//
// Grants are not part of any round's chain or audit file: a round's v2
// export reads only the ledger transactions of its own records.
package grant

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

const (
	// MaxCredits is the largest single grant: far below the bigint totals'
	// range and 1000 times crash/v1's largest bet.
	MaxCredits int64 = 1_000_000_000_000
	// MaxPlayerBytes bounds a player ID, well inside a btree index entry.
	MaxPlayerBytes = 256
)

// Apply's transaction-local timeouts. A lock wait longer than lockTimeout
// fails with SQLSTATE 55P03 and commits nothing.
const (
	lockTimeout = 3 * time.Second
	idleTimeout = time.Second
)

var (
	keyPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)
	creditsPattern = regexp.MustCompile(`^[1-9][0-9]{0,12}$`)
)

var (
	// ErrInvalid: Parse rejected an operand. Nothing was read or written.
	ErrInvalid = errors.New("invalid grant")
	// ErrCorrupt: the stored ledger transaction with the grant's ID is not
	// a grant of one posting from the faucet to a player. Nothing was
	// written.
	ErrCorrupt = errors.New("grant: the stored ledger transaction with the grant's ID is not a faucet-to-player grant")
)

// KeyReusedError reports that the grant key names a stored grant to another
// player or of other credits. Nothing was written.
type KeyReusedError struct {
	Key     string
	Player  string
	Credits int64
}

func (e *KeyReusedError) Error() string {
	return fmt.Sprintf("grant: grant key %s was used for player %q, credits %d", e.Key, e.Player, e.Credits)
}

// Grant is a validated grant. Parse is its only constructor.
type Grant struct {
	key     string
	player  string
	credits int64
}

// Key returns the grant key.
func (g Grant) Key() string { return g.key }

// Player returns the player ID the grant credits.
func (g Grant) Player() string { return g.player }

// Credits returns the granted amount.
func (g Grant) Credits() int64 { return g.credits }

// Parse validates a grant's operands: a grant key matching
// ^[a-z0-9][a-z0-9._:-]{0,127}$; a player ID of 1 to MaxPlayerBytes bytes
// of valid UTF-8 without control characters; credits in canonical decimal
// from 1 to MaxCredits. Errors wrap ErrInvalid.
func Parse(key, player, credits string) (Grant, error) {
	if !keyPattern.MatchString(key) {
		return Grant{}, fmt.Errorf("%w: the grant key must be 1 to 128 characters of a-z, 0-9, '.', '_', ':' and '-', starting with a letter or digit", ErrInvalid)
	}
	if !validPlayer(player) {
		return Grant{}, fmt.Errorf("%w: the player ID must be 1 to %d bytes of UTF-8 without control characters", ErrInvalid, MaxPlayerBytes)
	}
	n, err := strconv.ParseInt(credits, 10, 64)
	if !creditsPattern.MatchString(credits) || err != nil || n > MaxCredits {
		return Grant{}, fmt.Errorf("%w: credits must be a decimal integer from 1 to %d without sign or leading zeros", ErrInvalid, MaxCredits)
	}
	return Grant{key: key, player: player, credits: n}, nil
}

func validPlayer(p string) bool {
	if p == "" || len(p) > MaxPlayerBytes || !utf8.ValidString(p) {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Status says what Apply found.
type Status uint8

const (
	// Applied: this call wrote the grant.
	Applied Status = iota + 1
	// Existing: the grant was already stored with the same player and
	// credits; this call wrote nothing.
	Existing
)

func (s Status) String() string {
	switch s {
	case Applied:
		return "applied"
	case Existing:
		return "existing"
	default:
		return "unknown"
	}
}

// Result describes the stored grant.
type Result struct {
	TransactionID [16]byte
	// AccountID is the player's account.
	AccountID [16]byte
	Status    Status
}

// DB begins Apply's transaction.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Apply stores g in one READ COMMITTED transaction: the grant's ledger
// transaction row first, so that a concurrent Apply of the same key waits
// on its primary key holding no account lock; then the faucet and player
// accounts if absent; then the one posting, whose trigger updates both
// accounts in ascending account_id. A stored grant of the same key is
// compared, never rewritten. Errors other than *KeyReusedError and
// ErrCorrupt wrap the database or context error; after any of them the
// grant is stored exactly once or not at all.
func Apply(ctx context.Context, db DB, g Grant) (Result, error) {
	txID := ledger.GrantTransactionID(g.key)
	tx, err := db.BeginTx(ctx, postgres.ExecutionTxOptionsWithTimeouts(lockTimeout, idleTimeout))
	if err != nil {
		return Result{}, fmt.Errorf("grant: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `INSERT INTO orvex.ledger_transactions (transaction_id, kind) VALUES ($1, $2)
		ON CONFLICT (transaction_id) DO NOTHING`, txID, ledger.KindGrant)
	if err != nil {
		return Result{}, fmt.Errorf("grant: insert transaction: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return stored(ctx, tx, txID, g)
	}

	// The NOT EXISTS guards skip the insert, and its wait on a row another
	// transaction is updating, whenever the account exists. A faucet made
	// before grants existed has a random ID; the conflict on
	// ledger_accounts_system_name_key keeps it.
	if _, err := tx.Exec(ctx, `INSERT INTO orvex.ledger_accounts (account_id, kind, name, floor_enforced)
		SELECT $1::uuid, 'system', $2::text, false
		WHERE NOT EXISTS (SELECT 1 FROM orvex.ledger_accounts WHERE kind = 'system' AND name = $2::text AND round_id IS NULL)
		ON CONFLICT DO NOTHING`, ledger.FaucetAccountID(), ledger.FaucetAccountName); err != nil {
		return Result{}, fmt.Errorf("grant: insert faucet account: %w", err)
	}
	var faucet [16]byte
	if err := tx.QueryRow(ctx, `SELECT account_id FROM orvex.ledger_accounts
		WHERE kind = 'system' AND name = $1 AND round_id IS NULL`, ledger.FaucetAccountName).Scan(&faucet); err != nil {
		return Result{}, fmt.Errorf("grant: read faucet account: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced)
		SELECT gen_random_uuid(), 'player', $1::text, true
		WHERE NOT EXISTS (SELECT 1 FROM orvex.ledger_accounts WHERE player_id = $1::text)
		ON CONFLICT (player_id) DO NOTHING`, g.player); err != nil {
		return Result{}, fmt.Errorf("grant: insert player account: %w", err)
	}
	var account [16]byte
	if err := tx.QueryRow(ctx, `SELECT account_id FROM orvex.ledger_accounts WHERE player_id = $1`,
		g.player).Scan(&account); err != nil {
		return Result{}, fmt.Errorf("grant: read player account: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO orvex.ledger_postings (transaction_id, posting_index, debit_account_id, credit_account_id, amount)
		VALUES ($1, 0, $2, $3, $4)`, txID, faucet, account, g.credits); err != nil {
		return Result{}, fmt.Errorf("grant: insert posting: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("grant: commit: %w", err)
	}
	return Result{TransactionID: txID, AccountID: account, Status: Applied}, nil
}

// stored compares the committed grant with ID txID to g. It writes nothing;
// Apply's deferred rollback ends the transaction.
func stored(ctx context.Context, tx pgx.Tx, txID [16]byte, g Grant) (Result, error) {
	rows, err := tx.Query(ctx, `SELECT
			coalesce(t.kind = $2 AND t.round_id IS NULL AND t.sequence IS NULL
				AND p.posting_index = 0 AND p.round_id IS NULL AND p.event_sequence IS NULL
				AND d.kind = 'system' AND d.name = $3 AND d.round_id IS NULL
				AND c.kind = 'player', false),
			coalesce(c.player_id, ''), c.account_id, p.amount
		FROM orvex.ledger_transactions t
		JOIN orvex.ledger_postings p ON p.transaction_id = t.transaction_id
		JOIN orvex.ledger_accounts d ON d.account_id = p.debit_account_id
		JOIN orvex.ledger_accounts c ON c.account_id = p.credit_account_id
		WHERE t.transaction_id = $1`, txID, ledger.KindGrant, ledger.FaucetAccountName)
	if err != nil {
		return Result{}, fmt.Errorf("grant: read stored grant: %w", err)
	}
	var (
		n       int
		shaped  bool
		player  string
		account [16]byte
		credits int64
	)
	_, err = pgx.ForEachRow(rows, []any{&shaped, &player, &account, &credits}, func() error {
		n++
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("grant: read stored grant: %w", err)
	}
	switch {
	case n != 1 || !shaped:
		return Result{}, fmt.Errorf("%w: transaction %s", ErrCorrupt, canonjson.UUID(txID))
	case player != g.player || credits != g.credits:
		return Result{}, &KeyReusedError{Key: g.key, Player: player, Credits: credits}
	}
	return Result{TransactionID: txID, AccountID: account, Status: Existing}, nil
}

package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

// forbiddenDB fails the test on any call.
type forbiddenDB struct{ t *testing.T }

func (d forbiddenDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	d.t.Error("BeginTx called")
	return nil, errors.New("forbidden")
}

func (d forbiddenDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	d.t.Error("Query called")
	return nil, errors.New("forbidden")
}

func (d forbiddenDB) QueryRow(context.Context, string, ...any) pgx.Row {
	d.t.Error("QueryRow called")
	return nil
}

func registry(t *testing.T) *game.Registry {
	t.Helper()
	reg, err := game.NewRegistry(v1.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestDefaultConfig(t *testing.T) {
	want := Config{EngineBuild: buildinfo.Version, LockTimeout: 3 * time.Second, IdleInTransactionTimeout: time.Second, MaxAttempts: 3}
	if got := DefaultConfig(); got != want {
		t.Errorf("DefaultConfig = %+v, want %+v", got, want)
	}
	if err := DefaultConfig().validate(); err != nil {
		t.Errorf("DefaultConfig is invalid: %v", err)
	}
}

// testKeyring is the in-package tests' keyring. They must not import
// pgtest, which imports this package.
func testKeyring(t *testing.T) *sealing.Keyring {
	t.Helper()
	k, err := sealing.NewKeyring("test", map[string][]byte{"test": bytes.Repeat([]byte{7}, sealing.KeySize)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestNewRequiresKeyring: Config.Keyring is required (LBT-21 K-8).
func TestNewRequiresKeyring(t *testing.T) {
	base := Config{EngineBuild: "golden", LockTimeout: 3 * time.Second, IdleInTransactionTimeout: time.Second, MaxAttempts: 3}
	if _, err := New(forbiddenDB{t}, registry(t), base, CrashV1Policy()); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("nil Keyring: err = %v, want ErrInvalidConfig", err)
	}
	base.Keyring = testKeyring(t)
	if _, err := New(forbiddenDB{t}, registry(t), base, CrashV1Policy()); err != nil {
		t.Errorf("with a keyring: %v", err)
	}
}

// policyWith is the crash/v1 policy with another descriptor or rule.
type policyWith struct {
	Policy
	d    game.Descriptor
	rule ledger.Rule
}

func (p policyWith) Descriptor() game.Descriptor { return p.d }
func (p policyWith) Ledger() ledger.Rule         { return p.rule }

type ruleWith struct {
	ledger.Rule
	d game.Descriptor
}

func (r ruleWith) Descriptor() game.Descriptor { return r.d }

func TestNewValidation(t *testing.T) {
	db := forbiddenDB{t}
	reg := registry(t)
	base := Config{EngineBuild: "golden", LockTimeout: 3 * time.Second, IdleInTransactionTimeout: time.Second, MaxAttempts: 3, Keyring: testKeyring(t)}
	if _, err := New(db, reg, base, CrashV1Policy()); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	edges := []Config{
		{EngineBuild: "golden", LockTimeout: 60 * time.Second, IdleInTransactionTimeout: 50 * time.Millisecond, MaxAttempts: 1, Keyring: base.Keyring},
		{EngineBuild: strings.Repeat("x", 128), LockTimeout: 60 * time.Second, IdleInTransactionTimeout: 60*time.Second - time.Millisecond, MaxAttempts: 10, Keyring: base.Keyring},
	}
	for _, c := range edges {
		if _, err := New(db, reg, c, CrashV1Policy()); err != nil {
			t.Errorf("edge config %+v: %v", c, err)
		}
	}
	mut := func(f func(*Config)) Config {
		c := base
		f(&c)
		return c
	}
	bad := map[string]Config{
		"lock below 10ms":      mut(func(c *Config) { c.LockTimeout = 10*time.Millisecond - time.Nanosecond }),
		"lock above 60s":       mut(func(c *Config) { c.LockTimeout = 60*time.Second + time.Nanosecond }),
		"idle below 50ms":      mut(func(c *Config) { c.IdleInTransactionTimeout = 50*time.Millisecond - time.Nanosecond }),
		"idle above 60s":       mut(func(c *Config) { c.IdleInTransactionTimeout = 60*time.Second + time.Nanosecond }),
		"lock equals idle":     mut(func(c *Config) { c.LockTimeout, c.IdleInTransactionTimeout = time.Second, time.Second }),
		"lock below idle":      mut(func(c *Config) { c.LockTimeout, c.IdleInTransactionTimeout = time.Second, 2*time.Second }),
		"attempts 0":           mut(func(c *Config) { c.MaxAttempts = 0 }),
		"attempts 11":          mut(func(c *Config) { c.MaxAttempts = 11 }),
		"empty build":          mut(func(c *Config) { c.EngineBuild = "" }),
		"129-byte build":       mut(func(c *Config) { c.EngineBuild = strings.Repeat("x", 129) }),
		"build with a space":   mut(func(c *Config) { c.EngineBuild = "go lden" }),
		"build with non-ASCII": mut(func(c *Config) { c.EngineBuild = "gölden" }),
		"build with a control": mut(func(c *Config) { c.EngineBuild = "gold\x01en" }),
	}
	for name, c := range bad {
		if _, err := New(db, reg, c, CrashV1Policy()); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidConfig", name, err)
		}
	}
	dice := game.Descriptor{Game: "dice", RulesVersion: 1, SchemaVersion: 1, StateVersion: 1}
	other := v1.Descriptor()
	other.StateVersion = 9
	policies := map[string][]Policy{
		"no policies":        nil,
		"nil policy":         {nil},
		"two for one key":    {CrashV1Policy(), CrashV1Policy()},
		"no adapter":         {policyWith{Policy: CrashV1Policy(), d: dice, rule: ruleWith{ledger.CrashV1(), dice}}},
		"adapter descriptor": {policyWith{Policy: CrashV1Policy(), d: other, rule: ruleWith{ledger.CrashV1(), other}}},
		"ledger descriptor":  {policyWith{Policy: CrashV1Policy(), d: v1.Descriptor(), rule: ruleWith{ledger.CrashV1(), other}}},
		"no ledger rule":     {policyWith{Policy: CrashV1Policy(), d: v1.Descriptor()}},
	}
	for name, ps := range policies {
		if _, err := New(db, reg, base, ps...); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidConfig", name, err)
		}
	}
	if _, err := New(nil, reg, base, CrashV1Policy()); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("nil DB: err = %v", err)
	}
	if _, err := New(db, nil, base, CrashV1Policy()); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("nil registry: err = %v", err)
	}
}

func TestExecuteNextValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Keyring = testKeyring(t)
	x, err := New(forbiddenDB{t}, registry(t), cfg, CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	round := [16]byte{1}
	for name, l := range map[string]Lease{
		"nil round id": {RoundID: [16]byte{}},
		"token -1":     {RoundID: round, FencingToken: -1},
	} {
		if _, err := x.ExecuteNext(context.Background(), l); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("ExecuteNext %s: err = %v, want ErrInvalidRequest", name, err)
		}
		if _, err := x.Drain(context.Background(), l, 1); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Drain %s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if n, err := x.Drain(context.Background(), Lease{RoundID: round}, 0); n != 0 || !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("Drain limit 0 = %d, %v; want ErrInvalidRequest", n, err)
	}
	if _, err := x.CreateRound(context.Background(), NewRound{Game: v1.GameID, RulesVersion: v1.RulesVersionNumber}); !errors.Is(err, ErrInvalidRound) {
		t.Errorf("CreateRound nil round id: err = %v, want ErrInvalidRound", err)
	}
	x.Forget(round)
}

// beginErrDB fails every BeginTx with err and any other call with a test
// error.
type beginErrDB struct {
	t      *testing.T
	err    error
	begins int
}

func (d *beginErrDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	d.begins++
	return nil, d.err
}

func (d *beginErrDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	d.t.Error("Query called")
	return nil, errors.New("unexpected")
}

func (d *beginErrDB) QueryRow(context.Context, string, ...any) pgx.Row {
	d.t.Error("QueryRow called")
	return nil
}

// TestContextErrorWrapsCtxErr: when ctx has ended and the driver reports a
// PgError that does not wrap the context error (57014, query canceled),
// ExecuteNext and CreateRound still return an error wrapping ctx.Err(), keep
// the driver's error reachable, and ExecuteNext keeps the round's entry
// (E-15).
func TestContextErrorWrapsCtxErr(t *testing.T) {
	canceled := &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}
	db := &beginErrDB{t: t, err: canceled}
	cfg := DefaultConfig()
	cfg.EngineBuild = "golden"
	cfg.Keyring = testKeyring(t)
	x, err := New(db, registry(t), cfg, CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	round := [16]byte{0x0e, 1}

	_, err = x.ExecuteNext(ctx, Lease{RoundID: round})
	if !errors.Is(err, context.Canceled) || postgres.SQLState(err) != "57014" {
		t.Errorf("ExecuteNext = %v; want an error wrapping context.Canceled and the 57014 PgError", err)
	}
	for _, s := range []error{ErrInternal, ErrUnavailable, ErrBusy, ErrCorrupt, ErrFenced} {
		if errors.Is(err, s) {
			t.Errorf("ExecuteNext error matches %v", s)
		}
	}
	x.mu.Lock()
	en := x.entries[round]
	x.mu.Unlock()
	if en == nil || en.dropped.Load() || en.halt != nil {
		t.Errorf("ExecuteNext dropped or halted the entry after a context error")
	}
	if db.begins != 1 {
		t.Errorf("%d BeginTx calls, want 1 (no retry once ctx ended)", db.begins)
	}

	var seed [32]byte
	seed[0] = 0xab
	_, err = x.CreateRound(ctx, NewRound{RoundID: round, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		Config:             v1.Config{RoundID: round, Seed: seed, MinBetCredits: 1, MaxBetCredits: 100, MaxBets: 10},
		CreatedAtUnixMicro: 1_760_000_000_000_000})
	if !errors.Is(err, context.Canceled) || postgres.SQLState(err) != "57014" {
		t.Errorf("CreateRound = %v; want an error wrapping context.Canceled and the 57014 PgError", err)
	}

	// With ctx alive, contextError adds nothing.
	live := contextError(context.Background(), fmt.Errorf("x: %w", context.Canceled))
	if !errors.Is(live, context.Canceled) {
		t.Errorf("contextError lost the wrapped cause: %v", live)
	}
}

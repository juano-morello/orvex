package execution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/buildinfo"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
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
	base := Config{EngineBuild: "golden", LockTimeout: 3 * time.Second, IdleInTransactionTimeout: time.Second, MaxAttempts: 3}
	if _, err := New(db, reg, base, CrashV1Policy()); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	edges := []Config{
		{EngineBuild: "golden", LockTimeout: 60 * time.Second, IdleInTransactionTimeout: 50 * time.Millisecond, MaxAttempts: 1},
		{EngineBuild: strings.Repeat("x", 128), LockTimeout: 60 * time.Second, IdleInTransactionTimeout: 60*time.Second - time.Millisecond, MaxAttempts: 10},
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
	x, err := New(forbiddenDB{t}, registry(t), DefaultConfig(), CrashV1Policy())
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

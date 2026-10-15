package admission

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres"
)

// --- Fakes ---

// attemptScript scripts one admission transaction.
type attemptScript struct {
	beginErr  error
	callErr   error
	outcome   string
	reason    *string
	sequence  *int64
	effTime   *int64
	commitErr error
}

// readScript scripts one read transaction (Lookup, the 23505 resolve).
type readScript struct {
	beginErr error
	queryErr error
	round    [16]byte
	sequence int64
	effTime  int64
	command  []byte
	status   *string
	reason   *string
}

// fakeDB hands out scripted transactions. It tells admission transactions
// from read transactions by their TxOptions and counts each.
type fakeDB struct {
	t       *testing.T
	forbid  bool
	admits  []attemptScript
	reads   []readScript
	mu      sync.Mutex
	nAdmit  int
	nRead   int
	commits int
}

func (d *fakeDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.forbid {
		d.t.Error("BeginTx called")
		return nil, errors.New("fake: BeginTx forbidden")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch opts {
	case postgres.AdmissionTxOptions():
		if d.nAdmit >= len(d.admits) {
			d.t.Errorf("admission attempt %d not scripted", d.nAdmit+1)
			return nil, errors.New("fake: unscripted attempt")
		}
		s := d.admits[d.nAdmit]
		d.nAdmit++
		if s.beginErr != nil {
			return nil, s.beginErr
		}
		return &fakeTx{db: d, admit: &s}, nil
	case postgres.SnapshotReadTxOptions():
		if d.nRead >= len(d.reads) {
			d.t.Errorf("read %d not scripted", d.nRead+1)
			return nil, errors.New("fake: unscripted read")
		}
		s := d.reads[d.nRead]
		d.nRead++
		if s.beginErr != nil {
			return nil, s.beginErr
		}
		return &fakeTx{db: d, read: &s}, nil
	default:
		d.t.Errorf("BeginTx with unexpected options %+v", opts)
		return nil, errors.New("fake: unexpected options")
	}
}

type fakeTx struct {
	pgx.Tx
	db    *fakeDB
	admit *attemptScript
	read  *readScript
}

func (tx *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.read != nil {
		r := tx.read
		if r.queryErr != nil {
			return fakeRow{err: r.queryErr}
		}
		return fakeRow{vals: []any{r.round, r.sequence, r.effTime, r.command, r.status, r.reason}}
	}
	a := tx.admit
	if len(args) != 10 {
		tx.db.t.Errorf("admit_command called with %d arguments, want 10", len(args))
	}
	if a.callErr != nil {
		return fakeRow{err: a.callErr}
	}
	return fakeRow{vals: []any{a.outcome, a.reason, a.sequence, a.effTime}}
}

func (tx *fakeTx) Commit(context.Context) error {
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	if tx.read != nil {
		tx.db.t.Error("Commit called on a read transaction")
	}
	tx.db.commits++
	return tx.admit.commitErr
}

func (tx *fakeTx) Rollback(context.Context) error { return nil }

type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return errors.New("fake: scan arity")
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d).Elem()
		if r.vals[i] == nil {
			dv.SetZero()
			continue
		}
		dv.Set(reflect.ValueOf(r.vals[i]))
	}
	return nil
}

// testPolicy returns a fixed classification for every command.
type testPolicy struct {
	d  game.Descriptor
	cl Classification
}

func (p testPolicy) Descriptor() game.Descriptor          { return p.d }
func (p testPolicy) Classify(any) (Classification, error) { return p.cl, nil }

func registry(t *testing.T) *game.Registry {
	t.Helper()
	reg, err := game.NewRegistry(v1.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func newTestSequencer(t *testing.T, db DB, mutate func(*Config)) *Sequencer {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(db, registry(t), cfg, CrashV1Policy())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return s
}

var (
	testRound   = [16]byte{0: 0x01, 15: 0x01}
	otherRound  = [16]byte{0: 0x01, 15: 0x02}
	testCommand = [16]byte{0: 0x02, 15: 0x01}
)

func request(cmd any) Request {
	return Request{RoundID: testRound, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, CommandID: testCommand, Command: cmd}
}

func cashOut() Request {
	return request(v1.CashOut{Authority: v1.AuthorityPlayer, Player: "alice"})
}

func admitted(seq, et int64) attemptScript {
	return attemptScript{outcome: "admitted", sequence: new(seq), effTime: new(et)}
}

// --- Tests ---

func TestDefaultConfig(t *testing.T) {
	want := Config{LockTimeout: 3 * time.Second, IdleInTransactionTimeout: time.Second, MaxAttempts: 3, MaxInFlightPerRound: 4}
	if got := DefaultConfig(); got != want {
		t.Errorf("DefaultConfig() = %+v, want %+v", got, want)
	}
	if err := DefaultConfig().validate(); err != nil {
		t.Errorf("DefaultConfig().validate() = %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	db := &fakeDB{t: t, forbid: true}
	reg := registry(t)
	cfgCases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"LockTimeout below 10ms", func(c *Config) { c.LockTimeout = 10*time.Millisecond - 1 }},
		{"LockTimeout above 60s", func(c *Config) { c.LockTimeout = 60*time.Second + 1 }},
		{"Idle below 50ms", func(c *Config) { c.IdleInTransactionTimeout = 50*time.Millisecond - 1 }},
		{"Idle above 60s", func(c *Config) { c.IdleInTransactionTimeout, c.LockTimeout = 60*time.Second+1, 60*time.Second }},
		{"MaxAttempts 0", func(c *Config) { c.MaxAttempts = 0 }},
		{"MaxAttempts 11", func(c *Config) { c.MaxAttempts = 11 }},
		{"MaxInFlightPerRound 0", func(c *Config) { c.MaxInFlightPerRound = 0 }},
		{"MaxInFlightPerRound 65", func(c *Config) { c.MaxInFlightPerRound = 65 }},
		{"LockTimeout equal to Idle", func(c *Config) { c.LockTimeout = c.IdleInTransactionTimeout }},
		{"LockTimeout below Idle", func(c *Config) { c.LockTimeout, c.IdleInTransactionTimeout = time.Second, 2*time.Second }},
	}
	for _, c := range cfgCases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			c.mutate(&cfg)
			if _, err := New(db, reg, cfg, CrashV1Policy()); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("New err = %v, want ErrInvalidConfig", err)
			}
		})
	}
	// The bounds themselves are valid.
	for _, cfg := range []Config{
		{LockTimeout: 60 * time.Millisecond, IdleInTransactionTimeout: 50 * time.Millisecond, MaxAttempts: 1, MaxInFlightPerRound: 1},
		{LockTimeout: 60 * time.Second, IdleInTransactionTimeout: 60*time.Second - 1, MaxAttempts: 10, MaxInFlightPerRound: 64},
	} {
		if _, err := New(db, reg, cfg, CrashV1Policy()); err != nil {
			t.Errorf("New(%+v) = %v, want nil", cfg, err)
		}
	}
	if _, err := New(db, reg, Config{LockTimeout: 10 * time.Millisecond, IdleInTransactionTimeout: 50 * time.Millisecond, MaxAttempts: 1, MaxInFlightPerRound: 1}, CrashV1Policy()); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("LockTimeout 10ms with Idle 50ms: err = %v, want ErrInvalidConfig", err)
	}

	d := v1.Descriptor()
	other := d
	other.Game = "dice"
	schema := d
	schema.SchemaVersion = 2
	state := d
	state.StateVersion = 2
	policyCases := []struct {
		name     string
		db       DB
		reg      *game.Registry
		policies []Policy
	}{
		{"nil db", nil, reg, []Policy{CrashV1Policy()}},
		{"nil registry", db, nil, []Policy{CrashV1Policy()}},
		{"no policies", db, reg, nil},
		{"nil policy", db, reg, []Policy{nil}},
		{"two policies for one key", db, reg, []Policy{CrashV1Policy(), testPolicy{d: d}}},
		{"no registered adapter", db, reg, []Policy{testPolicy{d: other}}},
		{"schema version differs", db, reg, []Policy{testPolicy{d: schema}}},
		{"state version differs", db, reg, []Policy{testPolicy{d: state}}},
	}
	for _, c := range policyCases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.db, c.reg, DefaultConfig(), c.policies...); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("New err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestAdmitValidation(t *testing.T) {
	db := &fakeDB{t: t, forbid: true}
	s := newTestSequencer(t, db, nil)
	cases := []struct {
		name string
		edit func(*Request)
		want error
	}{
		{"nil round id", func(r *Request) { r.RoundID = [16]byte{} }, ErrInvalidRequest},
		{"nil command id", func(r *Request) { r.CommandID = [16]byte{} }, ErrInvalidRequest},
		{"game dice", func(r *Request) { r.Game = "dice" }, ErrUnknownRules},
		{"rules version 2", func(r *Request) { r.RulesVersion = 2 }, ErrUnknownRules},
		{"unencodable authority", func(r *Request) { r.Command = v1.PlaceBet{Authority: 7, Player: "alice", BetCredits: 1} }, ErrInvalidCommand},
		{"nil command", func(r *Request) { r.Command = nil }, ErrInvalidCommand},
		{"pointer command", func(r *Request) { r.Command = &v1.CashOut{Authority: v1.AuthorityPlayer, Player: "alice"} }, ErrInvalidCommand},
		{"NUL player", func(r *Request) { r.Command = v1.CashOut{Authority: v1.AuthorityPlayer, Player: "a\x00b"} }, ErrInvalidCommand},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := cashOut()
			c.edit(&req)
			if _, err := s.Admit(context.Background(), req); !errors.Is(err, c.want) {
				t.Errorf("Admit err = %v, want %v", err, c.want)
			}
		})
	}

	// A policy bug: an inconsistent hold.
	bad, err := New(db, registry(t), DefaultConfig(), testPolicy{d: v1.Descriptor(), cl: Classification{Class: ClassOrdinary, Hold: Hold{PlayerID: "alice"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Admit(context.Background(), cashOut()); !errors.Is(err, ErrInternal) {
		t.Errorf("invalid classification: err = %v, want ErrInternal", err)
	}
	bad, err = New(db, registry(t), DefaultConfig(), testPolicy{d: v1.Descriptor(), cl: Classification{Class: 9}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Admit(context.Background(), cashOut()); !errors.Is(err, ErrInternal) {
		t.Errorf("invalid class: err = %v, want ErrInternal", err)
	}
}

func TestAdmitOutcomeMapping(t *testing.T) {
	closeReq := request(v1.CloseRound{Authority: v1.AuthoritySystem})
	nilRow := func(outcome string) attemptScript { return attemptScript{outcome: outcome} }
	cases := []struct {
		name    string
		req     Request
		script  attemptScript
		want    Admission
		wantErr error
		check   func(t *testing.T, err error)
		commits int
	}{
		{name: "admitted", req: cashOut(), script: admitted(7, 100),
			want: Admission{RoundID: testRound, CommandID: testCommand, Sequence: 7, EffectiveTimeUnixMicro: 100}, commits: 1},
		{name: "duplicate", req: cashOut(), script: attemptScript{outcome: "duplicate", sequence: new(int64(3)), effTime: new(int64(50))},
			want: Admission{RoundID: testRound, CommandID: testCommand, Sequence: 3, EffectiveTimeUnixMicro: 50, Duplicate: true}},
		{name: "conflict", req: cashOut(), script: nilRow("conflict"), wantErr: ErrCommandIDReused},
		{name: "not_due", req: closeReq, script: attemptScript{outcome: "not_due", effTime: new(int64(900))}, wantErr: ErrClosureNotDue,
			check: func(t *testing.T, err error) {
				var e *ClosureNotDueError
				if !errors.As(err, &e) || e.EffectiveTimeUnixMicro != 900 {
					t.Errorf("err = %#v, want ClosureNotDueError{900}", err)
				}
			}},
		{name: "retired invalid_not_before", req: closeReq, script: nilRow("invalid_not_before"), wantErr: ErrInternal},
		{name: "rules_mismatch", req: cashOut(), script: nilRow("rules_mismatch"), wantErr: ErrInvalidRequest},
		{name: "not_found", req: cashOut(), script: nilRow("not_found"), wantErr: ErrRoundNotFound},
		{name: "corrupt", req: cashOut(), script: nilRow("corrupt"), wantErr: ErrInternal},
		{name: "bogus", req: cashOut(), script: nilRow("bogus"), wantErr: ErrInternal},
		{name: "rejected bogus reason", req: cashOut(), script: attemptScript{outcome: "rejected", reason: new("bogus")}, wantErr: ErrInternal},
		{name: "rejected without reason", req: cashOut(), script: nilRow("rejected"), wantErr: ErrInternal},
		{name: "admitted without sequence", req: cashOut(), script: nilRow("admitted"), wantErr: ErrInternal},
	}
	for _, r := range []Reason{ReasonRoundClosed, ReasonRoundSealed, ReasonLifecycleOrder, ReasonInsufficientCredits} {
		cases = append(cases, struct {
			name    string
			req     Request
			script  attemptScript
			want    Admission
			wantErr error
			check   func(t *testing.T, err error)
			commits int
		}{name: "rejected " + string(r), req: cashOut(), script: attemptScript{outcome: "rejected", reason: new(string(r))}, wantErr: ErrRejected,
			check: func(t *testing.T, err error) {
				var e *RejectedError
				if !errors.As(err, &e) || e.Reason != r {
					t.Errorf("err = %v, want RejectedError %s", err, r)
				}
			}})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeDB{t: t, admits: []attemptScript{c.script}}
			s := newTestSequencer(t, db, nil)
			got, err := s.Admit(context.Background(), c.req)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Errorf("Admit err = %v, want %v", err, c.wantErr)
				}
				if c.check != nil {
					c.check(t, err)
				}
			} else if err != nil || got != c.want {
				t.Errorf("Admit = %+v, %v; want %+v", got, err, c.want)
			}
			if got.Input() != (game.Input{Sequence: got.Sequence, EffectiveTimeUnixMicro: got.EffectiveTimeUnixMicro}) {
				t.Errorf("Input() = %+v", got.Input())
			}
			if db.commits != c.commits {
				t.Errorf("Commit called %d times, want %d", db.commits, c.commits)
			}
			if db.nAdmit != 1 {
				t.Errorf("%d attempts, want 1", db.nAdmit)
			}
		})
	}
}

func TestAdmitRetryLoop(t *testing.T) {
	encoded, err := v1.Adapter().EncodeCommand(cashOut().Command)
	if err != nil {
		t.Fatal(err)
	}
	connErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	call := func(code, constraint string) attemptScript { return attemptScript{callErr: pgErr(code, constraint)} }
	commitEOF := func() attemptScript { s := admitted(1, 10); s.commitErr = io.ErrUnexpectedEOF; return s }
	ok := Admission{RoundID: testRound, CommandID: testCommand, Sequence: 1, EffectiveTimeUnixMicro: 10}
	dup := ok
	dup.Duplicate = true

	cases := []struct {
		name    string
		admits  []attemptScript
		reads   []readScript
		want    Admission
		wantErr []error
		notErr  []error
		// wantCode and wantConstraint, when set, are the PgError reachable
		// through errors.As.
		wantCode       string
		wantConstraint string
		attempts       int
		nReads         int
	}{
		{name: "40001 twice then admitted", admits: []attemptScript{call("40001", ""), call("40001", ""), admitted(1, 10)}, want: ok, attempts: 3},
		{name: "40P01 three times", admits: []attemptScript{call("40P01", ""), call("40P01", ""), call("40P01", "")}, wantErr: []error{ErrUnavailable}, notErr: []error{ErrOutcomeUnknown}, attempts: 3},
		{name: "55P03", admits: []attemptScript{call("55P03", "")}, wantErr: []error{ErrBusy}, notErr: []error{ErrOutcomeUnknown, ErrUnavailable}, attempts: 1},
		{name: "connection error then admitted", admits: []attemptScript{{callErr: connErr}, admitted(1, 10)}, want: ok, attempts: 2},
		{name: "BeginTx connection error then admitted", admits: []attemptScript{{beginErr: connErr}, admitted(1, 10)}, want: ok, attempts: 2},
		{name: "commit EOF then duplicate", admits: []attemptScript{commitEOF(), {outcome: "duplicate", sequence: new(int64(1)), effTime: new(int64(10))}}, want: dup, attempts: 2},
		{name: "commit EOF three times", admits: []attemptScript{commitEOF(), commitEOF(), commitEOF()}, wantErr: []error{ErrOutcomeUnknown, io.ErrUnexpectedEOF}, attempts: 3},
		{name: "commit 40001 then admitted", admits: []attemptScript{func() attemptScript { s := admitted(1, 10); s.commitErr = pgErr("40001", ""); return s }(), admitted(1, 10)}, want: ok, attempts: 2},
		{name: "commit 23000", admits: []attemptScript{func() attemptScript { s := admitted(1, 10); s.commitErr = pgErr("23000", ""); return s }()}, wantErr: []error{ErrInternal}, attempts: 1},
		{name: "floor check", admits: []attemptScript{call("23514", "ledger_accounts_floor_check")}, wantErr: []error{ErrRejected},
			wantCode: "23514", wantConstraint: "ledger_accounts_floor_check", attempts: 1},
		{name: "OX005", admits: []attemptScript{call("OX005", "")}, wantErr: []error{ErrInternal}, attempts: 1},
		{name: "23505 resolves to another round", admits: []attemptScript{call("23505", "admissions_command_id_key")},
			reads: []readScript{{round: otherRound, sequence: 4, effTime: 9, command: encoded}}, wantErr: []error{ErrCommandIDReused},
			wantCode: "23505", wantConstraint: "admissions_command_id_key", attempts: 1, nReads: 1},
		{name: "23505 resolves to other bytes", admits: []attemptScript{call("23505", "admissions_command_id_key")},
			reads: []readScript{{round: testRound, sequence: 4, effTime: 9, command: []byte(`{}`)}}, wantErr: []error{ErrCommandIDReused},
			wantCode: "23505", wantConstraint: "admissions_command_id_key", attempts: 1, nReads: 1},
		{name: "23505 resolves to the original", admits: []attemptScript{call("23505", "admissions_command_id_key")},
			reads: []readScript{{round: testRound, sequence: 1, effTime: 10, command: encoded}}, want: dup, attempts: 1, nReads: 1},
		{name: "23505 without an admission", admits: []attemptScript{call("23505", "admissions_command_id_key")},
			reads: []readScript{{queryErr: pgx.ErrNoRows}}, wantErr: []error{ErrInternal}, attempts: 1, nReads: 1},
		{name: "23505 resolve read loses its connection, then duplicate",
			admits: []attemptScript{call("23505", "admissions_command_id_key"), {outcome: "duplicate", sequence: new(int64(1)), effTime: new(int64(10))}},
			reads:  []readScript{{beginErr: connErr}}, want: dup, attempts: 2, nReads: 1},
		{name: "23505 resolve read fails with 42P01", admits: []attemptScript{call("23505", "admissions_command_id_key")},
			reads: []readScript{{queryErr: pgErr("42P01", "")}}, wantErr: []error{ErrInternal}, attempts: 1, nReads: 1},
		// S-12: ErrOutcomeUnknown only when the last failure was ambiguous.
		{name: "commit EOF then 40001 twice", admits: []attemptScript{commitEOF(), call("40001", ""), call("40001", "")},
			wantErr: []error{ErrUnavailable}, notErr: []error{ErrOutcomeUnknown}, wantCode: "40001", attempts: 3},
		{name: "commit EOF then 55P03", admits: []attemptScript{commitEOF(), call("55P03", "")},
			wantErr: []error{ErrBusy}, notErr: []error{ErrOutcomeUnknown}, wantCode: "55P03", attempts: 2},
		{name: "40001 twice then commit EOF", admits: []attemptScript{call("40001", ""), call("40001", ""), commitEOF()},
			wantErr: []error{ErrOutcomeUnknown, io.ErrUnexpectedEOF}, notErr: []error{ErrUnavailable}, attempts: 3},
		{name: "40001 then commit EOF then admitted", admits: []attemptScript{call("40001", ""), commitEOF(), admitted(1, 10)}, want: ok, attempts: 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeDB{t: t, admits: c.admits, reads: c.reads}
			s := newTestSequencer(t, db, nil)
			got, err := s.Admit(context.Background(), cashOut())
			if c.wantErr != nil {
				for _, w := range c.wantErr {
					if !errors.Is(err, w) {
						t.Errorf("Admit err = %v, want %v", err, w)
					}
				}
				for _, w := range c.notErr {
					if errors.Is(err, w) {
						t.Errorf("Admit err = %v, want no %v", err, w)
					}
				}
				if c.wantCode != "" {
					var pe *pgconn.PgError
					if !errors.As(err, &pe) || pe.Code != c.wantCode || (c.wantConstraint != "" && pe.ConstraintName != c.wantConstraint) {
						t.Errorf("Admit err = %v, want a reachable PgError %s %s", err, c.wantCode, c.wantConstraint)
					}
				}
			} else if err != nil || got != c.want {
				t.Errorf("Admit = %+v, %v; want %+v", got, err, c.want)
			}
			if db.nAdmit != c.attempts {
				t.Errorf("%d attempts, want %d", db.nAdmit, c.attempts)
			}
			if db.nRead != c.nReads {
				t.Errorf("%d reads, want %d", db.nRead, c.nReads)
			}
		})
	}

	t.Run("ctx cancelled during backoff", func(t *testing.T) {
		db := &fakeDB{t: t, admits: []attemptScript{call("40001", ""), admitted(1, 10)}}
		s := newTestSequencer(t, db, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}
		_, err := s.Admit(ctx, cashOut())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Admit err = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrOutcomeUnknown) {
			t.Errorf("Admit err = %v, want no ErrOutcomeUnknown after a definite failure", err)
		}
		if db.nAdmit != 1 {
			t.Errorf("%d attempts, want 1", db.nAdmit)
		}
	})

	t.Run("ctx cancelled during backoff after an ambiguous commit", func(t *testing.T) {
		db := &fakeDB{t: t, admits: []attemptScript{commitEOF(), admitted(1, 10)}}
		s := newTestSequencer(t, db, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}
		_, err := s.Admit(ctx, cashOut())
		if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, context.Canceled) {
			t.Errorf("Admit err = %v, want ErrOutcomeUnknown and context.Canceled", err)
		}
		if db.nAdmit != 1 {
			t.Errorf("%d attempts, want 1", db.nAdmit)
		}
	})

	t.Run("MaxAttempts 1", func(t *testing.T) {
		db := &fakeDB{t: t, admits: []attemptScript{call("40001", "")}}
		s := newTestSequencer(t, db, func(c *Config) { c.MaxAttempts = 1 })
		if _, err := s.Admit(context.Background(), cashOut()); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Admit err = %v, want ErrUnavailable", err)
		}
		if db.nAdmit != 1 {
			t.Errorf("%d attempts, want 1", db.nAdmit)
		}
	})
}

func TestLookupErrors(t *testing.T) {
	connErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	cases := []struct {
		name string
		read readScript
		want error
	}{
		{"BeginTx connection error", readScript{beginErr: connErr}, ErrUnavailable},
		{"08006", readScript{queryErr: pgErr("08006", "")}, ErrUnavailable},
		{"42P01", readScript{queryErr: pgErr("42P01", "")}, ErrInternal},
		{"bogus result status", readScript{round: testRound, sequence: 1, effTime: 5, command: []byte(`{}`), status: new("bogus")}, ErrInternal},
		{"no rows", readScript{queryErr: pgx.ErrNoRows}, ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeDB{t: t, reads: []readScript{c.read}}
			s := newTestSequencer(t, db, nil)
			if _, err := s.Lookup(context.Background(), testCommand); !errors.Is(err, c.want) {
				t.Errorf("Lookup err = %v, want %v", err, c.want)
			}
		})
	}

	t.Run("cancelled ctx", func(t *testing.T) {
		db := &fakeDB{t: t, reads: []readScript{{}}}
		s := newTestSequencer(t, db, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.Lookup(ctx, testCommand); !errors.Is(err, context.Canceled) {
			t.Errorf("Lookup err = %v, want context.Canceled", err)
		}
	})

	t.Run("outcomes", func(t *testing.T) {
		base := readScript{round: testRound, sequence: 3, effTime: 77, command: []byte(`{}`)}
		accepted, rejected := base, base
		accepted.status = new("accepted")
		rejected.status, rejected.reason = new("rejected"), new("no_bet")
		db := &fakeDB{t: t, reads: []readScript{base, accepted, rejected}}
		s := newTestSequencer(t, db, nil)
		wantAdmission := Admission{RoundID: testRound, CommandID: testCommand, Sequence: 3, EffectiveTimeUnixMicro: 77}
		for _, want := range []Status{
			{Admission: wantAdmission, Outcome: OutcomePending},
			{Admission: wantAdmission, Outcome: OutcomeAccepted},
			{Admission: wantAdmission, Outcome: OutcomeRejected, Reason: "no_bet"},
		} {
			got, err := s.Lookup(context.Background(), testCommand)
			if err != nil || got != want {
				t.Errorf("Lookup = %+v, %v; want %+v", got, err, want)
			}
		}
	})
}

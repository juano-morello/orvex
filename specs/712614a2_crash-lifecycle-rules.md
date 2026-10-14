# Plan: Crash v1 round rules: Decide/Evolve, lifecycle invariants and public projection (LBT-8)

Linear: [LBT-8](https://linear.app/lbtworks/issue/LBT-8/implement-crash-decideevolve-and-lifecycle-invariants), milestone M1 · Deterministic Core. It was blocked by LBT-6 (contract) and LBT-7 (math), and both have merged.

Governing documents: ADR-003 (primary), ADR-004, ADR-001, ADR-002, ADR-006, ADR-007 (mirrors in `docs/adr/`), and the approved math spec `specs/e96d124d_crash-v1-math-spec.md` (called "the math spec" below).

## 0. Goal

Add the Crash v1 shared-round rules to package `v1` in `internal/game/crash/v1/`:

- a typed `Rules` implementing `game.Game[State, Command, Event, Config]` (`internal/game/contract.go`);
- the round state with phases BETTING → RUNNING → CRASHED → SETTLED;
- commands `PlaceBet`, `StartRound`, `CashOut`, `CloseRound`, `SettleRound`. The three lifecycle commands require system authority;
- events `BetPlaced`, `RoundStarted`, `BetCashedOut`, `RoundCrashed`, `BetLost`, `RoundSettled`;
- deterministic rejections with stable reason codes (terminal command results without events);
- `Evolve` that validates every event against the state;
- a public projection that hides the crash outcome until `RoundCrashed` and the seed until `RoundSettled`;
- registration constants plus `Descriptor()` and `Adapter()` for `game.NewRegistry`.

Pure Go. No database, network, clock, randomness, `go.mod` change or new dependency.

## 1. Current state (verified during planning)

- Branch base `main` at `cc6413f`. `internal/game/{contract.go,registry.go}` (LBT-6) and `internal/game/crash/v1/{params,curve,payout,fairness}.go` plus golden vectors (LBT-7) exist. Toolchain: go1.26.5, golangci-lint 2.14.0, Python 3.14.6.
- `go.mod` requires only `github.com/cockroachdb/apd/v3 v3.2.3`. This change adds nothing.
- There is no global registration. Games are wired with `game.Adapt(desc, g)` and `game.NewRegistry(...)`, so "registered through registry.go" means exporting an `Adapter()` that a registry accepts.
- The existing crash/v1 tests are internal (`package v1`). `params_test.go`'s `TestNoIOImports` parses **every** non-test `.go` file in the package. It allows only these imports: `crypto/hmac`, `crypto/sha256`, `encoding/binary`, `encoding/hex`, `errors`, `fmt` (only `fmt.Errorf`), `math`, `math/bits` and apd. It allows only the package-level vars `ErrOutOfRange` and `ErrArithmetic`, and it bans `float32`/`float64` identifiers. The new files fail it until it is extended (step 8).
- Package identifiers already taken (do not redeclare): funcs `Commitment`, `CrashPoint`, `CrashTick`, `Payout`, `CashOutTiming`, `MultiplierAtTick`, `DeadlineOffsetMicros`, `OutcomeMessage`, `OutcomeR`, `CrashPointFromR`, `VerifyCommitment`, `ArithmeticContext`, `apdCheck`, `mulNonNeg`, `subChecked`, `outcomeHash`, `crashPointRaw`; const `RulesVersion` (the **string** `"crash/v1"`), `OutcomeTag`, `roundIDTextLen`; and test helpers and tests including `TestNoIOImports`, `TestCashOutTimingDeadlineEqualityLoses`, `loadVectors`, `multiplierTable`, `parseRoundID`, `parseHex32`, `isLowerHex`. Struct **field** names such as `Commitment` and `CrashTick` do not collide with these funcs.
- Cost facts from LBT-7: one `MultiplierAtTick` is about 70 µs (several times that under `-race`). `CrashTick` is at most 14 of them.
- Test seeds computed during planning with an independent Python stdlib script (HMAC/SHA-256 and the committed `multiplier_ticks.json`, no Go). All use the worked-example round ID `0190f3a8-7b3c-7d4e-8f00-000000000001`:

| name | seed | C | n_c | D (µs) |
|---|---|---|---|---|
| worked | bytes `00 01 … 1f` (math spec D-10) | 17,577 | 941 | 9,410,000 |
| instant | `sha256("orvex/crash/v1/lifecycle-test/58")` = `e73a4280…1cbf5d5f` | 10,000 | 0 | 0 |
| long | `sha256("orvex/crash/v1/lifecycle-test/6")` = `39a17b3a…0064569c` | 196,616 | 4,965 | 49,650,000 |

  Worked-example commitment: `630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd`. `M(940) = 17,576`, `M(941) = 17,587`. If Go disagrees with any of these numbers, stop and report. Never adjust an expectation to match Go output.

## 2. Decisions this plan makes, with ADR alignment (copy into the PR description)

No ADR is contradicted. No approved math-spec decision is reopened. The items below resolve open points or interpret the ADRs. AGENTS.md "Flag conflicts explicitly" asks for each to be named in the PR.

1. **Round ID type (math spec §10 item 1, D-11): UUID, kept.** `Config.RoundID` is a `[16]byte` holding the UUID bytes. It goes unchanged into the frozen `CrashPoint(seed, roundID)`, which formats the canonical lowercase 8-4-4-4-12 text. D-11 stays closed. LBT-10 must store round IDs as UUIDs. Refinement: `InitialState` rejects the nil UUID (all zero bytes) as invalid config. This guards against an uninitialized ID and does not touch the derivation, which still accepts any 16 bytes.
2. **`T0` (math spec §10 item 2, D-9).** `T0` is the `StartRound` command's `game.Input.EffectiveTimeUnixMicro`. It is recorded in `RoundStarted.StartedAtUnixMicro` and stored as `State.StartedAtUnixMicro`. `StartRound` rejects `T0` outside `[0, MaxStartMicros]` (D-7). LBT-11 produces the time.
3. **Aggregates and per-round bet cap (math spec §10 item 4, D-7).** Both options D-7 offers are adopted:
   - a new hard rules limit, `MaxBetsPerRound = 10_000`. Runtime config may narrow it (`Config.MaxBets`) but never widen it, the same rule D-7 applies to bet amounts;
   - every aggregate sum (`TotalBetCredits`, `TotalPayoutCredits`, the absolute deadline `T0 + D`) uses checked addition that fails with `ErrArithmetic`.

   With the cap, the largest possible payout total is 10,000 × 9,996,596,800,000 ≈ 1.0e17, below `MaxInt64`, so the checks are defence in depth. Why 10,000: state is copy-on-write (ADR-003 purity), so each event copies the bet slice and a round costs O(n²) copies. At 10,000 bets that is about 10⁸ `Bet` copies in the worst case per round. At 100,000 it would be about 10¹⁰. The value is a new frozen v1 constant, adjustable until the first rules release. **The engineer should confirm it.** The constant goes in the new `round.go`, not in `params.go`, and is not added to `params.json` or the golden vectors (both frozen).
4. **Bet amount limits are config, narrowed within D-7.** `Config.MinBetCredits` and `Config.MaxBetCredits` must satisfy `MinBetCredits(1) <= min <= max <= MaxBetCredits(1e9)`. They are explicit: there are no zero-means-default fields.
5. **Seed enters through config (D-10).** The runtime generates the 32-byte seed with `crypto/rand` and passes it in `Config.Seed`, recorded as round creation input (ADR-003: randomness comes in as recorded data). `InitialState` derives the commitment, `C`, `n_c` and `D` once and stores them in `State`. Refinement: an all-zero seed is rejected as invalid config, because it signals a runtime that forgot to set it (a real `crypto/rand` draw is zero with probability 2⁻²⁵⁶). D-10's derivation is unchanged. Protecting the seed at rest stays with LBT-10/LBT-12 (math spec §10 item 3).
6. **Authority is a field on each command, not on `game.Input`.** ADR-003 says "lifecycle commands require system authority". Each command carries `Authority` (`AuthorityPlayer` or `AuthoritySystem`; the zero value `AuthorityNone` is always rejected). The runtime sets it from the authenticated admission channel, never from client data (LBT-11/LBT-13). `Decide` enforces it, so decision replay reproduces the rejection. Lifecycle commands need `AuthoritySystem`; `PlaceBet` and `CashOut` need `AuthorityPlayer`. `internal/game/contract.go` is not changed.
7. **Event split between `CloseRound` and `SettleRound`.** `CloseRound` is accepted only when its effective time is at or after the absolute crash deadline `T0 + D` ("crash is determined by the committed deadline even if processing is delayed", ADR-004). It emits `RoundCrashed`, which reveals `C`, `n_c`, `D` and the absolute deadline: D-12's authoritative numeric outputs, stored in events per ADR-004. `SettleRound` emits one `BetLost` per still-open bet, in placement order, followed by `RoundSettled` with the totals and the revealed seed (D-10 step 7). This follows ADR-003's event order and ADR-004's "settlement … resolves", and puts all loss postings in the same execution transaction as `RoundSettled` (LBT-12). Before `CloseRound` is accepted, a cash-out with `t_a >= T0 + D` is rejected (`deadline_passed`). That bet stays open and is lost at settlement.
8. **Rejections are typed.** `Decide` returns `*RejectionError{Reason}`. `errors.Is(err, ErrRejected)` holds for it and `RejectionReason(err)` extracts the code. The reason strings are stable codes for LBT-10's terminal command results and become frozen at the first rules release. Every other `Decide` error (`ErrInvalidState`, `ErrArithmetic`) means fail closed: no events and no terminal result (math spec D-7). Out-of-range command inputs are pre-validated and become rejections (`invalid_amount`, `time_out_of_range`, `not_after_start`), which is how D-7's "turn `ErrOutOfRange` into a terminal command rejection" is met.
9. **Betting-window length is not enforced by the rules.** The math spec D-6 leaves it to the runtime. The phase change at `StartRound`'s admission sequence is the authoritative cut-off for bets. The trusted scheduler (LBT-11/LBT-13) chooses when to issue `StartRound`.
10. **Player identity.** `PlayerID` is an opaque non-empty `string`. LBT-10/12 may narrow it, for example to a UUID, before the first rules release.
11. **Not implemented here (ADR-003 invariant "deterministic canonical state hash").** Owned by LBT-9, as the prompt says. `State` is shaped for it: exported fixed-width fields, no maps, and bets in a slice in canonical placement order.
12. **Registry keys.** `GameID = "crash"`, `RulesVersionNumber = 1`, `SchemaVersion = 1`, `StateVersion = 1`. These are pre-release values. ADR-003 keeps the three versions separate. Payloads do not repeat `"crash/v1"`; the event envelope (LBT-9/10) carries the rules version (D-12).
13. **ADR-004 status amendment** (math spec §1.3 item 3): not part of this change. It happens after the first release.

## 3. Out of scope (do not create or change)

- `internal/game/crash/v1/{params,curve,payout,fairness}.go` and everything under `internal/game/crash/v1/testdata/`. These must stay **byte-identical**, including comments. Use the existing functions as they are.
- `internal/game/*.go` (contract, registry and their tests), `cmd/*`, `go.mod`, `go.sum`, `Makefile`, `.github/`, `AGENTS.md`, `docs/adr/*`, `odin/`, `.omp/`, `app_docs/`.
- Canonical codec, state hash, hash chain, `crashctl` replay (LBT-9). Schema, admission, sequencer, ledger, outbox (LBT-10 to LBT-12). Transport (LBT-13). Wiring the adapter into `cmd/engine` or `cmd/crashctl`.
- Round cancellation or void, auto cash-out, betting-window timers, sequence-monotonicity checks (runtime concerns).

## 4. Files

| File | Action |
|---|---|
| `internal/game/crash/v1/round.go` | new: registry constants, `Descriptor`, `Adapter`, `MaxBetsPerRound`, `Phase`, `BetStatus`, `PlayerID`, `Config`, `State`, `Bet`, sentinel errors, `Rules`, `InitialState`, unexported helpers |
| `internal/game/crash/v1/commands.go` | new: `Authority`, `Command` and the five command types |
| `internal/game/crash/v1/events.go` | new: `Event` and the six event types |
| `internal/game/crash/v1/decide.go` | new: `Reason`, `RejectionError`, `RejectionReason`, `Rules.Decide` |
| `internal/game/crash/v1/evolve.go` | new: `Rules.Evolve` |
| `internal/game/crash/v1/projection.go` | new: `Projection`, `Project` |
| `internal/game/crash/v1/params_test.go` | edit `TestNoIOImports` only (step 8) |
| `internal/game/crash/v1/round_test.go` | new: shared lifecycle test helpers, InitialState and registration tests |
| `internal/game/crash/v1/decide_test.go` | new: acceptance and rejection tests |
| `internal/game/crash/v1/evolve_test.go` | new: applicability tests |
| `internal/game/crash/v1/projection_test.go` | new: secrecy tests |
| `internal/game/crash/v1/purity_test.go` | new: purity, aliasing, concurrency and secret-leak tests |
| `internal/game/crash/v1/round_fuzz_test.go` | new: `FuzzRoundLifecycle` |
| `README.md` | edit the Status line and the `internal/game/crash/v1` Layout bullet |

Rules for every new non-test file:

- `package v1`. Imports only from `errors`, `fmt` (only `fmt.Errorf`), `math`, `slices` and `github.com/juano-morello/orvex/internal/game`, grouped for goimports (stdlib, blank line, local).
- No floats.
- Package-level `var` only for the four new sentinel errors, all declared in one `var` block in `round.go`. Do **not** add `var _ game.Game[...] = Rules{}`: `TestNoIOImports` would flag the blank var, and `Adapter()` already checks the interface at compile time through `game.Adapt`'s type inference.
- Error strings are lowercase with no trailing punctuation (staticcheck ST1005) and prefixed `crash/v1: `.

## 5. API and semantics

### 5.1 `round.go`

```go
// Registry keys of crash/v1 (ADR-003 keeps the three versions separate).
// RulesVersionNumber is the numeric key paired with the textual RulesVersion.
const (
	GameID             game.ID            = "crash"
	RulesVersionNumber game.RulesVersion  = 1
	SchemaVersion      game.SchemaVersion = 1
	StateVersion       game.StateVersion  = 1
)

// Descriptor returns the registry descriptor of crash/v1.
func Descriptor() game.Descriptor // {GameID, RulesVersionNumber, SchemaVersion, StateVersion}

// Adapter returns the type-erased adapter for game.NewRegistry: game.Adapt(Descriptor(), Rules{}).
func Adapter() game.Adapter

// MaxBetsPerRound is the hard crash/v1 limit on bets in one round (LBT-8; resolves
// math spec D-7's aggregate item). Config.MaxBets may narrow it, never widen it.
const MaxBetsPerRound int64 = 10_000

type Phase uint8

const (
	PhaseInvalid Phase = iota // zero value; never a valid state
	PhaseBetting
	PhaseRunning
	PhaseCrashed
	PhaseSettled
)

// String returns "BETTING", "RUNNING", "CRASHED", "SETTLED" or "INVALID".
func (p Phase) String() string

type BetStatus uint8

const (
	StatusInvalid BetStatus = iota // zero value; never valid
	StatusOpen
	StatusCashedOut
	StatusLost
)

// PlayerID is an opaque, non-empty player identifier set by the runtime from the
// authenticated session.
type PlayerID string

// Config is the recorded round-creation input. The runtime generates Seed with
// crypto/rand (math spec D-10); it is secret until RoundSettled.
type Config struct {
	RoundID       [16]byte // UUID bytes (D-11); not the nil UUID
	Seed          [32]byte // not all zero
	MinBetCredits int64    // in [MinBetCredits, MaxBetCredits]
	MaxBetCredits int64    // in [Config.MinBetCredits, MaxBetCredits]
	MaxBets       int64    // in [1, MaxBetsPerRound]
}

// Bet is one player's bet. It holds no secret. Cash-out fields are zero unless
// Status is StatusCashedOut.
type Bet struct {
	Player                  PlayerID
	BetCredits              int64
	PlacedAtUnixMicro       int64
	Status                  BetStatus
	CashedOutAtUnixMicro    int64
	CashOutTick             int64
	CashOutMultiplierScaled int64
	PayoutCredits           int64
}

// State is the authoritative, engine-internal state of one round. From
// InitialState on it holds the secret seed and crash outcome: never send it to
// clients; publish Project(state) instead (ADR-003, math spec section 7).
type State struct {
	RoundID       [16]byte
	Phase         Phase
	MinBetCredits int64
	MaxBetCredits int64
	MaxBets       int64

	Commitment [32]byte // public: SHA-256(Seed)
	Seed       [32]byte // secret until RoundSettled

	// Secret until RoundCrashed (math spec section 7).
	CrashPointScaled          int64
	CrashTick                 int64
	CrashDeadlineOffsetMicros int64
	CrashDeadlineUnixMicro    int64 // T0 + D; set by RoundStarted

	StartedAtUnixMicro int64 // T0; set by RoundStarted
	ClosedAtUnixMicro  int64 // set by RoundCrashed
	SettledAtUnixMicro int64 // set by RoundSettled

	Bets               []Bet // placement order; at most one per player
	TotalBetCredits    int64
	TotalPayoutCredits int64
}

var (
	ErrInvalidConfig      = errors.New("crash/v1: invalid round config")
	ErrInvalidState       = errors.New("crash/v1: invalid round state")
	ErrEventNotApplicable = errors.New("crash/v1: event does not apply to round state")
	ErrRejected           = errors.New("crash/v1: command rejected") // matched by every *RejectionError
)

// Rules implements game.Game[State, Command, Event, Config] for crash/v1
// (ADR-003, ADR-004, LBT-8; spec specs/712614a2_crash-lifecycle-rules.md).
// The doc comment summarizes the lifecycle, the authority rule and the secrecy rule.
type Rules struct{}

func (Rules) InitialState(cfg Config) (State, error)
```

`InitialState` steps:

1. Validate. Each failure returns `State{}` plus `fmt.Errorf("%w: <what>", ErrInvalidConfig)`. The message names the field and may print the limit values, but never seed bytes. The failures are:
   - `RoundID == [16]byte{}`;
   - `Seed == [32]byte{}`;
   - `MinBetCredits < MinBetCredits`;
   - `MaxBetCredits > MaxBetCredits` (the package constants);
   - `cfg.MinBetCredits > cfg.MaxBetCredits`;
   - `MaxBets < 1`;
   - `MaxBets > MaxBetsPerRound`.
2. Derive the outcome:
   - `commitment := Commitment(cfg.Seed)`;
   - `c := CrashPoint(cfg.Seed, cfg.RoundID)`;
   - `n, err := CrashTick(c)`;
   - `d, err := DeadlineOffsetMicros(n)`.

   On an error, return `State{}` and `fmt.Errorf("%w: derive crash outcome", ErrArithmetic)`. **Do not wrap or format the inner error**, because its text contains the crash point or tick.
3. Return `State{Phase: PhaseBetting, RoundID, limits, Commitment, Seed, CrashPointScaled: c, CrashTick: n, CrashDeadlineOffsetMicros: d}` with `Bets: nil` and zero times and totals.

Unexported helpers in `round.go`:

- `addNonNeg(a, b int64) (int64, error)` returns `fmt.Errorf("%w: int64 addition overflows", ErrArithmetic)` when an operand is negative or `b > math.MaxInt64-a`, and `a+b` otherwise. The message must not contain the operands, because one of them can be the secret `D`. `math` is already on the `TestNoIOImports` allowlist, so `round.go` may import it.
- `findBet(bets []Bet, p PlayerID) int` returns the index or -1 (linear scan; `slices.IndexFunc` is fine).
- `validPhase(p Phase) bool`.

### 5.2 `commands.go`

```go
// Authority is who submitted a command. The runtime sets it from the
// authenticated admission channel, never from client data (ADR-003, ADR-004:
// lifecycle commands are trusted-only).
type Authority uint8

const (
	AuthorityNone   Authority = iota // zero value; always rejected
	AuthorityPlayer
	AuthoritySystem
)

// Command is a sealed sum type; pass commands by value.
type Command interface{ isCommand() }

type PlaceBet struct {
	Authority  Authority
	Player     PlayerID
	BetCredits int64
}
type StartRound struct{ Authority Authority }
type CashOut struct {
	Authority Authority
	Player    PlayerID
}
type CloseRound struct{ Authority Authority }
type SettleRound struct{ Authority Authority }
```

Each type gets a value-receiver `isCommand()` and a doc comment naming its required authority and phase.

### 5.3 `events.go`

Event payloads hold only plain values: no pointers, maps or slices. Field names follow D-12 (unit in the name, int64 fixed units, times in µs since the Unix epoch, UTC).

```go
// Event is a sealed sum type; pass events by value. No event carries a secret
// before it is due: RoundCrashed reveals the outcome, RoundSettled the seed.
type Event interface{ isEvent() }

type BetPlaced struct {
	Player            PlayerID
	BetCredits        int64
	PlacedAtUnixMicro int64
}
type RoundStarted struct {
	StartedAtUnixMicro int64 // T0 (D-9)
}
type BetCashedOut struct {
	Player                  PlayerID
	CashedOutAtUnixMicro    int64 // t_a
	CashOutTick             int64
	CashOutMultiplierScaled int64
	PayoutCredits           int64
}
type RoundCrashed struct {
	ClosedAtUnixMicro         int64 // CloseRound effective time, >= CrashDeadlineUnixMicro
	CrashPointScaled          int64
	CrashTick                 int64
	CrashDeadlineOffsetMicros int64
	CrashDeadlineUnixMicro    int64
}
type BetLost struct {
	Player     PlayerID
	BetCredits int64
}
type RoundSettled struct {
	SettledAtUnixMicro int64
	BetCount           int64
	TotalBetCredits    int64
	TotalPayoutCredits int64
	Seed               [32]byte // reveal (D-10 step 7)
}
```

### 5.4 `decide.go`

```go
// Reason is the stable code of a deterministic command rejection, recorded as
// the terminal command result (ADR-003). Frozen with the rules version.
type Reason string

const (
	ReasonInvalidCommand   Reason = "invalid_command"    // nil/unknown command type, empty player
	ReasonUnauthorized     Reason = "unauthorized"       // wrong Authority for the command
	ReasonTimeOutOfRange   Reason = "time_out_of_range"  // effective time < 0; StartRound T0 > MaxStartMicros (D-7)
	ReasonWrongPhase       Reason = "wrong_phase"
	ReasonRoundSettled     Reason = "round_settled"      // any command after settlement
	ReasonInvalidAmount    Reason = "invalid_amount"     // bet outside the round's limits (D-7)
	ReasonDuplicateBet     Reason = "duplicate_bet"
	ReasonBetLimitReached  Reason = "bet_limit_reached"
	ReasonNoBet            Reason = "no_bet"
	ReasonAlreadyCashedOut Reason = "already_cashed_out"
	ReasonNotAfterStart    Reason = "not_after_start"    // t_a <= T0 (ADR-002, D-9)
	ReasonDeadlinePassed   Reason = "deadline_passed"    // t_a >= T0 + D; equality loses
	ReasonCrashNotDue      Reason = "crash_not_due"      // CloseRound before T0 + D
)

// RejectionError is a deterministic command rejection. Its text is the reason
// code only, so it never carries a secret.
type RejectionError struct{ Reason Reason }

func (e *RejectionError) Error() string { return "crash/v1: command rejected: " + string(e.Reason) }
func (e *RejectionError) Unwrap() error { return ErrRejected }

// RejectionReason reports the reason of a rejection returned by Decide.
func RejectionReason(err error) (Reason, bool) // errors.As

func (Rules) Decide(s State, c Command, in game.Input) ([]Event, error)
```

`Decide` reads only. `t` means `in.EffectiveTimeUnixMicro`; `in.Sequence` is not used. A rejection returns `nil, &RejectionError{...}`. Success returns at least one event. Checks run in this exact order, and the first failure wins:

1. `!validPhase(s.Phase)` → `nil, fmt.Errorf("%w: unknown phase", ErrInvalidState)`.
2. `s.Phase == PhaseSettled` → `round_settled`, for every command including nil.
3. Type switch on `c`. Anything except the five value types (nil, pointers, foreign types) → `invalid_command`.
4. Authority: `PlaceBet`/`CashOut` need `AuthorityPlayer`; `StartRound`/`CloseRound`/`SettleRound` need `AuthoritySystem`. Otherwise `unauthorized`.
5. `t < 0` → `time_out_of_range`.
6. Phase: `PlaceBet` and `StartRound` need BETTING, `CashOut` and `CloseRound` need RUNNING, `SettleRound` needs CRASHED. Otherwise `wrong_phase`.
7. Command-specific:

| Command | Checks, in order | Events |
|---|---|---|
| `PlaceBet` | `Player == ""` → `invalid_command`; `BetCredits` outside `[s.MinBetCredits, s.MaxBetCredits]` → `invalid_amount`; `findBet >= 0` → `duplicate_bet`; `int64(len(s.Bets)) >= s.MaxBets` → `bet_limit_reached`; `addNonNeg(s.TotalBetCredits, BetCredits)` error → return it (fail closed) | `BetPlaced{Player, BetCredits, PlacedAtUnixMicro: t}` |
| `StartRound` | `t > MaxStartMicros` → `time_out_of_range`; `addNonNeg(t, s.CrashDeadlineOffsetMicros)` error → return it | `RoundStarted{StartedAtUnixMicro: t}` |
| `CashOut` | `Player == ""` → `invalid_command`; no bet → `no_bet`; status `StatusCashedOut` → `already_cashed_out`; status not `StatusOpen` → `ErrInvalidState`; `t <= s.StartedAtUnixMicro` → `not_after_start`; `tick, ok, err := CashOutTiming(s.StartedAtUnixMicro, t, s.CrashTick)`: `err != nil` → `fmt.Errorf("%w: cash-out timing", ErrInvalidState)` (**inner error dropped**: it formats the crash tick); `!ok` → `deadline_passed`; `m, err := MultiplierAtTick(tick)` and `p, err := Payout(bet.BetCredits, m)`: errors → `fmt.Errorf("%w: cash-out payout: %w", ErrInvalidState, err)` (their text holds only the public tick, bet and multiplier); `addNonNeg(s.TotalPayoutCredits, p)` error → return it | `BetCashedOut{Player, CashedOutAtUnixMicro: t, CashOutTick: tick, CashOutMultiplierScaled: m, PayoutCredits: p}` |
| `CloseRound` | `t < s.CrashDeadlineUnixMicro` → `crash_not_due` | `RoundCrashed{ClosedAtUnixMicro: t, CrashPointScaled, CrashTick, CrashDeadlineOffsetMicros, CrashDeadlineUnixMicro}` from state |
| `SettleRound` | none | for each bet with `StatusOpen`, in slice order: `BetLost{Player, BetCredits}`; then `RoundSettled{SettledAtUnixMicro: t, BetCount: int64(len(s.Bets)), TotalBetCredits, TotalPayoutCredits, Seed: s.Seed}` |

Notes:

- Deadline equality loses because `CashOutTiming` treats `e >= D` as ineligible (D-9). The instant-crash round (`D = 0`) can never pay.
- `CloseRound` at exactly `T0 + D` is accepted (`>=`).
- Secrecy rule for every error text in `decide.go`, `evolve.go`, `projection.go` and `round.go`: never format `Seed`, `CrashPointScaled`, `CrashTick`, `CrashDeadlineOffsetMicros` or `CrashDeadlineUnixMicro`, and never wrap or format an error returned by `CrashTick`, `DeadlineOffsetMicros` or `CashOutTiming` (math spec §7). `TestRoundErrorsDoNotLeakSecrets` checks this.

### 5.5 `evolve.go`

`func (Rules) Evolve(s State, e Event) (State, error)`.

Principle: Evolve accepts an event only if `Decide` could have produced it from `s` for some valid `Input` (ADR-003 "validated event applicability"). On error it returns `State{}`.

It is copy-on-write: `next := s`, and **every successful Evolve gives `next.Bets` a fresh backing array**:

- `slices.Clone(s.Bets)`;
- for `BetPlaced`, `make([]Bet, len, len+1)` plus `copy` plus `append`.

It never appends to or writes into `s.Bets`. This applies even to events that do not touch bets, so callers can never alias the input.

Order of checks:

1. `!validPhase(s.Phase)` → `fmt.Errorf("%w: unknown phase", ErrInvalidState)`.
2. `s.Phase == PhaseSettled` → `fmt.Errorf("%w: round settled", ErrEventNotApplicable)`.
3. Type switch. Anything except the six value types → `ErrEventNotApplicable`.
4. Per event. Every failure is `fmt.Errorf("%w: <EventName>: <fixed reason text>", ErrEventNotApplicable)`, except where noted.

| Event | Applies only if | Effect |
|---|---|---|
| `BetPlaced` | phase BETTING; `Player != ""`; no bet for player; `BetCredits` in `[s.MinBetCredits, s.MaxBetCredits]`; `len < s.MaxBets`; `PlacedAtUnixMicro >= 0`; `addNonNeg` ok (its error is returned as is) | append `Bet{Player, BetCredits, PlacedAtUnixMicro, Status: StatusOpen}`; `TotalBetCredits += BetCredits` |
| `RoundStarted` | phase BETTING; `0 <= StartedAtUnixMicro <= MaxStartMicros`; `addNonNeg(StartedAt, s.CrashDeadlineOffsetMicros)` ok | phase RUNNING; set `StartedAtUnixMicro` and `CrashDeadlineUnixMicro` |
| `BetCashedOut` | phase RUNNING; bet exists with `StatusOpen`; `CashedOutAtUnixMicro > s.StartedAtUnixMicro`; `CashOutTiming(s.StartedAt, ev.CashedOutAt, s.CrashTick)` returns eligible with `tick == ev.CashOutTick` (timing error → `ErrInvalidState` with inner dropped; ineligible or tick mismatch → not applicable); `MultiplierAtTick(tick) == ev.CashOutMultiplierScaled`; `Payout(bet, m) == ev.PayoutCredits` (math errors → `fmt.Errorf("%w: ...: %w", ErrInvalidState, err)`); `addNonNeg` ok | bet: `StatusCashedOut` plus the four cash-out fields; `TotalPayoutCredits += PayoutCredits` |
| `RoundCrashed` | phase RUNNING; the four outcome fields equal the state's (message: fixed text "crash outcome does not match", no values); `ClosedAtUnixMicro >= s.CrashDeadlineUnixMicro` | phase CRASHED; set `ClosedAtUnixMicro` |
| `BetLost` | phase CRASHED; bet exists with `StatusOpen`; `BetCredits == bet.BetCredits` | bet `StatusLost` |
| `RoundSettled` | phase CRASHED; no bet with `StatusOpen`; `BetCount == len(Bets)`; both totals equal the state's; `Seed == s.Seed` (fixed text, no bytes); `SettledAtUnixMicro >= 0` | phase SETTLED; set `SettledAtUnixMicro` |

### 5.6 `projection.go`

```go
// Projection is the public view of a round (ADR-003 "Replay"; math spec section 7).
// Crash fields are zero before PhaseCrashed; Seed and SettledAtUnixMicro are zero
// before PhaseSettled; StartedAtUnixMicro is zero in PhaseBetting. Phase says
// which fields are meaningful.
type Projection struct {
	RoundID       [16]byte
	Phase         Phase
	Commitment    [32]byte
	MinBetCredits int64
	MaxBetCredits int64
	MaxBets       int64

	StartedAtUnixMicro int64
	Bets               []Bet // deep copy, placement order
	TotalBetCredits    int64
	TotalPayoutCredits int64

	ClosedAtUnixMicro         int64
	CrashPointScaled          int64
	CrashTick                 int64
	CrashDeadlineOffsetMicros int64
	CrashDeadlineUnixMicro    int64

	SettledAtUnixMicro int64
	Seed               [32]byte
}

// Project returns the public view of s. It copies fields by a per-phase
// allow-list and never copies State wholesale.
func Project(s State) (Projection, error)
```

Behaviour:

- Invalid phase → `Projection{}` and `fmt.Errorf("%w: unknown phase", ErrInvalidState)`.
- Otherwise build the projection **by allow-listing per phase**:
  - always: `RoundID`, `Phase`, `Commitment`, the limits, `Bets: slices.Clone(s.Bets)` and the totals;
  - from RUNNING: `StartedAtUnixMicro`;
  - from CRASHED: `ClosedAtUnixMicro` and the four crash fields;
  - in SETTLED: `SettledAtUnixMicro` and `Seed`.
- Never write `p := Projection(s)`-style conversions or copy whole structs.

## 6. Tests (each named test is required)

All tests are internal (`package v1`), table-driven, standard `testing` only. Test files may import anything (`crypto/sha256`, `reflect`, `strconv`, `strings`, `sync`, `errors`, `slices`, `encoding/hex`, the `game` package).

### 6.1 `round_test.go`: shared helpers and setup tests

Helpers (unexported, used across the new test files):

- `const testT0 int64 = 1_760_000_000_000_000`.
- `workedRoundID` is the `[16]byte` of `0190f3a8-7b3c-7d4e-8f00-000000000001`.
- `workedSeed()` returns bytes `0..31`.
- `lifecycleSeed(i int) [32]byte` returns `sha256.Sum256([]byte("orvex/crash/v1/lifecycle-test/" + strconv.Itoa(i)))`. Index 58 is the instant round and index 6 is the long round.
- `testConfig(seed [32]byte) Config` returns RoundID `workedRoundID`, `MinBetCredits: 1`, `MaxBetCredits: MaxBetCredits`, `MaxBets: 100`.
- `newRound(t, cfg) State`, which calls `t.Fatal` on error.
- Command builders with the correct authority: `placeBet(p PlayerID, credits int64)`, `cashOut(p)`, `startRound()`, `closeRound()`, `settleRound()`.
- `step(t, s, c, at int64) (State, []Event)`: Decide with `game.Input{Sequence: 1, EffectiveTimeUnixMicro: at}`, fatal on error, then Evolve every event in order, fatal on error.
- `rejected(t, s, c, at) Reason`: fatal unless Decide returns nil events and an error with `RejectionReason` ok.
- `runningRound(t, seed, players ...)`: each player bets 1,000 at `testT0-1_000`, then `StartRound` at `testT0`.

Tests:

- `TestInitialStateDerivesOutcome`: for the worked, instant and long seeds, the state has PhaseBetting, the `Seed`, the limits copied, `Commitment == Commitment(seed)`, and C/n_c/D equal to section 1's table. For the worked seed it also checks the commitment hex `630dcd29…10dd`. `Bets` is empty, and times and totals are zero.
- `TestInitialStateRejectsInvalidConfig`: zero RoundID, zero Seed, `MinBetCredits` 0, `MinBetCredits` -1, `MaxBetCredits` 1,000,000,001, min > max, `MaxBets` 0, `MaxBets` -1, and `MaxBets = MaxBetsPerRound + 1` → `errors.Is(err, ErrInvalidConfig)` and the returned state equals `State{}`. Boundary values that must be accepted: `MaxBets = MaxBetsPerRound`, min = max = 1, min = max = `MaxBetCredits`.
- `TestAdapterRegistration`:
  - `game.NewRegistry(Adapter())` succeeds. `Lookup("crash", 1)` returns an adapter whose `Descriptor()` equals `Descriptor()`, which equals `{crash, 1, 1, 1}`.
  - Through the adapter: `InitialState(testConfig(workedSeed()))`, then `Decide(placeBet)`, then `Evolve` yields a state with one bet.
  - A rejection (duplicate bet) passes through the adapter with `RejectionReason` intact.
  - `Decide` with state `struct{}{}`, or a `nil` state → `game.ErrTypeMismatch`.
  - `Lookup("crash", 2)` → `game.ErrUnknownRules`.

### 6.2 `decide_test.go`: LBT-8 acceptance and command semantics

- `TestRoundFullWorkedExample`: the worked seed, `MaxBets: 100`.
  1. Bets: alice 1,000; bob 250; carol 1,000,000,000; dave 7. Each one asserts the exact `BetPlaced`.
  2. `StartRound` at `testT0` → `RoundStarted{testT0}`.
  3. carol `CashOut` at `testT0` → `not_after_start`.
  4. carol at `testT0+1` → `BetCashedOut{carol, testT0+1, 0, 10_000, 1_000_000_000}`.
  5. alice at `testT0+9_409_999` → `BetCashedOut{alice, …, 940, 17_576, 1_757}`.
  6. bob at `testT0+9_410_000` → `deadline_passed`.
  7. `CloseRound` at `testT0+9_409_999` → `crash_not_due`.
  8. `CloseRound` at `testT0+9_410_000` → `RoundCrashed{ClosedAt: testT0+9_410_000, 17_577, 941, 9_410_000, testT0+9_410_000}`.
  9. `SettleRound` at `testT0+10_000_000` → exactly `[BetLost{bob, 250}, BetLost{dave, 7}, RoundSettled{testT0+10_000_000, 4, 1_000_001_257, 1_000_001_757, workedSeed()}]`.

  Final state: PhaseSettled, alice and carol `StatusCashedOut`, bob and dave `StatusLost`, and `VerifyCommitment(final.Seed, final.Commitment)`.
- `TestRoundOneBetPerPlayer` (acceptance):
  - a second `PlaceBet` by alice, with a different amount, → `duplicate_bet` and the state is untouched;
  - bob can still bet;
  - `Evolve` of a forged second `BetPlaced{alice}` → `ErrEventNotApplicable`;
  - after settlement, alice appears once in `Bets` and in at most one `BetLost`.
- `TestRoundNoDoubleCashOut` (acceptance): alice cashes out at `T0+1_000_000`.
  - A second `CashOut` at `T0+2_000_000` → `already_cashed_out`.
  - A second one after the deadline → `already_cashed_out`.
  - Forged second `BetCashedOut` for alice → `ErrEventNotApplicable`.
  - `TotalPayoutCredits` equals the single payout.
  - `SettleRound` emits no `BetLost` for alice.
- `TestRoundDeadlineEqualityLoses` (acceptance), for the worked round and the long round:
  - `CashOut` at `T0+D-1` → eligible, tick `n_c-1`;
  - another player's `CashOut` at `T0+D` → `deadline_passed`;
  - `CloseRound` at `T0+D` → accepted;
  - `SettleRound` → that player gets `BetLost`.
  - Also `Evolve` of a forged `BetCashedOut` with `CashedOutAtUnixMicro = T0+D` and tick `n_c` with the matching `M(n_c)` and payout → `ErrEventNotApplicable`.
- `TestRoundNoCashOutAtOrAfterCrash`:
  - RUNNING at `T0+D` and at `T0+D+1` → `deadline_passed`;
  - after `CloseRound` (CRASHED) → `wrong_phase`;
  - after `SettleRound` → `round_settled`;
  - instant round (`D = 0`): `CashOut` at `T0` → `not_after_start`, at `T0+1` → `deadline_passed`, `CloseRound` at `T0` → accepted, and settlement loses every bet;
  - no `BetCashedOut` is ever produced with `CashOutMultiplierScaled >= CrashPointScaled`.
- `TestRoundCloseWaitsForDeadline`: worked round, `CloseRound` at `T0+D-1` → `crash_not_due`, then at `T0+D+5_000_000` → `RoundCrashed` with `ClosedAt = T0+D+5_000_000` and the same outcome fields (processing delay does not move the crash).
- `TestRoundLifecycleAuthority`:
  - each of `StartRound`/`CloseRound`/`SettleRound`, in the phase where it would otherwise succeed, with `AuthorityPlayer` and with `AuthorityNone` → `unauthorized`;
  - `PlaceBet`/`CashOut` with `AuthoritySystem` and `AuthorityNone` → `unauthorized`;
  - the same commands with the right authority succeed.
- `TestRoundRejectionReasons`: a table of `{name, state builder, command, t, want Reason}` that covers **every** `Reason` constant. The test keeps an `allReasons` slice of the 13 constants and fails if any is missing from the table. It must include:
  - nil command and `&PlaceBet{…}` (pointer) → `invalid_command`;
  - empty player for `PlaceBet` and `CashOut` → `invalid_command`;
  - `t = -1` → `time_out_of_range`;
  - `StartRound` at `MaxStartMicros+1` → `time_out_of_range`, while `StartRound` at `MaxStartMicros` succeeds;
  - bet 0 and bet `max+1` with config limits 10..100 → `invalid_amount`, while 10 and 100 succeed;
  - `MaxBets: 2` with a third player → `bet_limit_reached`, and a duplicate when full → `duplicate_bet` (check order);
  - `CashOut` with no bet → `no_bet`;
  - wrong-phase cases for every command;
  - `round_settled` for every command type, and for nil.
- `TestRoundSettleResolvesOpenBets`: three bets, one cashed out. `SettleRound` emits `BetLost` for the other two in placement order, then `RoundSettled` with `BetCount 3` and correct totals. `RoundSettled` is last.
- `TestRoundSettledStateImmutable` (acceptance): take a settled worked round. Snapshot the state with `takeRoundSnapshot` and save its `Project` result. Run every command type with every authority at several times, then Evolve every event type, including exact copies of events from its own history. Afterwards, the snapshot reports no change and `Project` returns a value `reflect.DeepEqual` to the saved projection.
- `TestRoundNoTransitionsAfterSettlement`: on the same settled state, every `Decide` returns `nil` events and reason `round_settled`, and every `Evolve` returns `ErrEventNotApplicable` and `State{}`.

### 6.3 `evolve_test.go`

- `TestEvolveValidatesApplicability` (ADR-003 "validated event applicability"): each case must give `errors.Is(err, ErrEventNotApplicable)` and a returned state equal to `State{}`. Cases:
  - `BetPlaced`: in RUNNING; empty player; below min; above max; when `MaxBets` is reached; duplicate player; `PlacedAt = -1`.
  - `RoundStarted`: in RUNNING; at -1; at `MaxStartMicros+1`.
  - `BetCashedOut`: in BETTING; unknown player; already cashed out; time `== T0`; time `== deadline`; tick off by one; multiplier off by one; payout off by one.
  - `RoundCrashed`: in BETTING; `ClosedAt = deadline-1`; each of the four outcome fields off by one.
  - `BetLost`: in RUNNING; unknown player; cashed-out player; already lost; wrong amount.
  - `RoundSettled`: in RUNNING; with an open bet left; wrong `BetCount`; wrong total bet; wrong total payout; wrong seed (one bit flipped).
  - Every one of the six event types in SETTLED.
  - nil event; `&BetPlaced{…}` pointer.
- `TestRoundRejectsInvalidState`: `State{}` and a state with `Phase: 99` → `Decide`, `Evolve` and `Project` each return `ErrInvalidState`. `Decide` does not return a rejection here.
- `TestEvolveAppliesDecidedEvents`: for every success path in `TestRoundFullWorkedExample`, applying the events from `Decide` gives the expected fields (phase, bet statuses, totals, times, `CrashDeadlineUnixMicro = T0+D`). This can be folded into the `step` helper's assertions plus explicit checks.

### 6.4 `projection_test.go`

- `TestProjectionHidesOutcomeBeforeCrash`: drive the worked round and the long round with the same script:
  - alice bets 1,000 and bob 500 at `T0-1_000`;
  - `StartRound` at `T0`;
  - alice cashes out at `T0+1_000_000`;
  - bob cashes out at `T0+5_000_000`. Both are eligible in both rounds.

  After every step (BETTING and RUNNING), both projections have zero crash fields, zero `ClosedAt`, zero `SettledAt` and a zero `Seed`. With `Commitment` zeroed, the two projections are `reflect.DeepEqual`. This shows that before the crash the projection depends on the seed only through the commitment.
- `TestProjectionRevealsOutcomeAtCrashAndSeedAtSettlement`:
  - in CRASHED, the crash fields equal the state's and `Seed` is zero;
  - in SETTLED, `Seed == state.Seed` and `VerifyCommitment(p.Seed, p.Commitment)`, and `CrashPoint(p.Seed, p.RoundID) == p.CrashPointScaled`;
  - in BETTING, `StartedAtUnixMicro` is zero even if the state field is non-zero. Construct such a state by hand to prove the allow-listing.
- `TestProjectionDoesNotAliasState`: mutating `p.Bets[0]` leaves the state unchanged.

### 6.5 `purity_test.go`

- Helpers:
  - `roundSnapshot{state State; backing []Bet}` with `takeRoundSnapshot(s)` (deep copy plus `s.Bets[:cap(s.Bets)]`) and `changed(s)`;
  - `withSpareCap(s State) State`, which re-allocates `Bets` with capacity `len+8`, so append aliasing is caught;
  - `rulesUnderTest()` returns the typed `Rules{}` and a wrapper that goes through `Adapter()` (mirror `internal/game/contract_test.go`'s `erased`);
  - `roundPurityViolation(g, s, c, in) string`: Decide twice, then Evolve twice per produced event, all on the same `s`. It compares events, states and error texts, and checks `s` against the snapshot after each call. After each Evolve it also overwrites every element of `next.Bets` and re-checks the snapshot (returned states must not alias the input).
- `TestRulesPurity`:
  - states, each passed through `withSpareCap`:
    - fresh BETTING;
    - BETTING with 2 bets;
    - RUNNING with one cashed-out and one open bet;
    - CRASHED;
    - SETTLED;
  - × every command type with the correct and the wrong authority;
  - × inputs at `T0-1`, `T0+1`, `T0+D-1`, `T0+D` and `T0+D+1`;
  - × both rules-under-test.

  Every combination must report no violation.
- `TestRoundPurityCheckDetectsMutation`, the negative control. Each of these wrappers around `Rules` must be detected:
  - one whose Evolve writes `s.Bets[0].Status`;
  - one whose Evolve appends into spare capacity (`s.Bets = append(s.Bets, Bet{})`, returned);
  - one whose Evolve returns `next` sharing `s.Bets` and then mutates it.
- `TestRulesConcurrentUse`: 8 goroutines run Decide (`CashOut` on a RUNNING state) and Evolve on one shared state. All results equal the sequential ones and the shared state is unchanged. It is meaningful under `-race`.
- `TestRoundErrorsDoNotLeakSecrets`, on the worked round. Collect the `.Error()` text of:
  - every rejection reachable in `TestRoundFullWorkedExample`;
  - `deadline_passed` and `crash_not_due`;
  - Evolve errors for forged `RoundCrashed` (each field off by one), `RoundSettled` (wrong seed), `BetCashedOut` (at the deadline);
  - `Project` and `Decide` on `State{Phase: 99}` carrying the worked secrets.

  None may contain `"17577"`, `"9410000"`, `strconv.FormatInt(testT0+9_410_000, 10)` or `hex.EncodeToString(workedSeed[:])`.

### 6.6 `round_fuzz_test.go`

`FuzzRoundLifecycle(f *testing.F)` with arguments `(seedInput []byte, script []byte)`.

Setup:

- The round seed is `sha256.Sum256(seedInput)` (skip if zero). RoundID `workedRoundID`, `MinBetCredits: 1`, `MaxBetCredits: MaxBetCredits`, `MaxBets: 3`. Players `p0..p3`, so the bet limit is reachable.
- The clock starts at `testT0 - 1_000_000`.

Decode `script` in 3-byte ops `(b0, b1, b2)`:

- kind `b0 % 6`:
  - 0 `PlaceBet`;
  - 1 `StartRound`;
  - 2 `CashOut`;
  - 3 `CloseRound`;
  - 4 `SettleRound`;
  - 5 the same as `b1 % 5`, but with a wrong authority;
- player `b1 % 4`;
- amount from `[]int64{0, 1, 7, 1_000, 1_000_000_000, 1_000_000_001}[int(b1>>2)%6]`;
- clock advance `int64(b2)` µs if `b0&0x80 == 0`, else `int64(b2) * 100_000` µs.

Per op:

- run `roundPurityViolation` (typed rules only) and fail on any violation;
- `Decide` either returns events, all of which `Evolve` applies without error, or an error with `errors.Is(err, ErrRejected)`. Any other error fails the test.

After each applied step, `checkRoundInvariants(t, prev, next)`:

- the phase is valid and never moves backwards;
- player IDs in `Bets` are unique, and `len(Bets) <= MaxBets`;
- `prev.Bets` is a prefix of `next.Bets`, except that an element may go from Open to CashedOut or Lost, never back and never CashedOut↔Lost;
- `TotalBetCredits` is the sum of bets, and `TotalPayoutCredits` is the sum of payouts;
- every cashed-out bet has `StartedAt < CashedOutAt < CrashDeadlineUnixMicro`, `CashOutTick == (CashedOutAt-StartedAt)/TickMicros < CrashTick` and `CashOutMultiplierScaled < CrashPointScaled`;
- `StatusLost` appears only in CRASHED or SETTLED;
- in SETTLED, no bet is Open;
- `Project`: in BETTING and RUNNING the crash fields and Seed are zero; in CRASHED the Seed is zero and the crash fields match; in SETTLED the Seed matches.

When the state is SETTLED, also assert:

- every command kind, each with both authorities, → `round_settled`;
- each event from this round's history re-applied → `ErrEventNotApplicable`;
- the state is unchanged.

After the script, drive the round to completion deterministically and assert each step succeeds:

1. `StartRound` if BETTING;
2. `CloseRound` at `max(clock, CrashDeadlineUnixMicro)` if RUNNING;
3. `SettleRound` if CRASHED.

Then the final state is SETTLED, `VerifyCommitment(Seed, Commitment)` holds, and the invariants hold.

Seed corpus (`f.Add`), at least:

- a happy path (3 bets, start, 2 cash-outs, close, settle) with seed input `"worked"`;
- a double cash-out;
- a bet limit with 4 players;
- wrong-authority lifecycle commands;
- a script that idles past any deadline (many `0x80|…` advance ops);
- an empty script.

The corpus runs under `make test`.

### 6.7 `params_test.go`: edit `TestNoIOImports` only

- Add `"slices"` and `"github.com/juano-morello/orvex/internal/game"` to `allowed`.
- Add `"ErrInvalidConfig"`, `"ErrInvalidState"`, `"ErrEventNotApplicable"` and `"ErrRejected"` to `allowedVars`.
- Extend the `checked` list to `params.go`, `curve.go`, `payout.go`, `fairness.go`, `round.go`, `commands.go`, `events.go`, `decide.go`, `evolve.go` and `projection.go`.
- Update the doc comment to say the rule covers the lifecycle files too.
- Nothing else in `params_test.go` changes.

## 7. `README.md`

- Status line: replace "Status: scaffold ([LBT-5](…)) plus the game contract ([LBT-6](…)). No Crash rules, runtime or persistence logic exists yet." with "Status: scaffold ([LBT-5](…)), the game contract ([LBT-6](…)), the frozen Crash v1 math ([LBT-7](https://linear.app/lbtworks/issue/LBT-7/freeze-crash-mathematical-specification-and-golden-vectors)) and the Crash v1 round rules ([LBT-8](https://linear.app/lbtworks/issue/LBT-8/implement-crash-decideevolve-and-lifecycle-invariants)). No runtime, persistence or transport exists yet." Keep the existing LBT-5/LBT-6 link URLs exactly.
- Layout bullet `internal/game/crash/v1`: "Crash v1 rules: frozen arithmetic, fairness derivation and golden vectors (ADR-004, LBT-7); round lifecycle Decide/Evolve, command authority and public projection (ADR-003, LBT-8)".

No other README changes. No `app_docs/` file: a later pipeline stage writes it.

## 8. Implementation order

1. `commands.go` and `events.go` (types only).
2. `round.go`, then `decide.go`, then `evolve.go`, then `projection.go`.
3. Edit `params_test.go` (§6.7), then run `go test ./internal/game/crash/v1 -run TestNoIOImports`.
4. Write the test files in the order of §6: helpers first.
5. README.
6. Run the gates once at the end (§9). Do not regenerate or edit any golden file. If a frozen-math expectation in section 1 disagrees with Go, stop and report it.

## 9. Verification (each must exit 0)

```sh
go test -race -count=1 ./internal/game/crash/v1 -run 'TestRound|TestEvolve|TestProjection|TestRules|TestInitialState|TestAdapterRegistration|TestNoIOImports' -v
go test -race -count=1 ./internal/game/crash/v1 -run '^$' -fuzz=FuzzRoundLifecycle -fuzztime=30s
make test
make lint
go vet ./...
make build
make golden-check
git diff --exit-code -- internal/game/crash/v1/params.go internal/game/crash/v1/curve.go internal/game/crash/v1/payout.go internal/game/crash/v1/fairness.go internal/game/crash/v1/testdata internal/game/contract.go internal/game/registry.go go.mod go.sum
```

The fuzz smoke run is a one-off. Do not commit any `testdata/fuzz/` corpus it creates, unless it is a crasher that you then fix; in that case keep the crasher as a regression input under `internal/game/crash/v1/testdata/fuzz/FuzzRoundLifecycle/`. Note that this path is under `testdata/`. If you add one, report it, because the `git diff` gate above only checks tracked files.

Acceptance mapping:

| Requirement | Demonstrated by |
|---|---|
| one bet per player | `TestRoundOneBetPerPlayer`, fuzz invariants |
| no double cash-out | `TestRoundNoDoubleCashOut`, fuzz invariants |
| deadline equality loses | `TestRoundDeadlineEqualityLoses`, `TestRoundFullWorkedExample` |
| settled state immutable | `TestRoundSettledStateImmutable`, fuzz SETTLED checks |
| no cash-out at/after crash | `TestRoundNoCashOutAtOrAfterCrash` |
| no transitions after settlement | `TestRoundNoTransitionsAfterSettlement` |
| validated event applicability | `TestEvolveValidatesApplicability` |
| lifecycle commands need system authority | `TestRoundLifecycleAuthority` |
| invalid commands → terminal result, no events | `TestRoundRejectionReasons` (nil events, `ErrRejected`) |
| projection hides outcome and seed | `TestProjectionHidesOutcomeBeforeCrash`, `TestProjectionRevealsOutcomeAtCrashAndSeedAtSettlement`, `TestRoundErrorsDoNotLeakSecrets` |
| pure and deterministic, no input mutation | `TestRulesPurity`, `TestRoundPurityCheckDetectsMutation`, `TestRulesConcurrentUse`, `TestNoIOImports` |
| registered through the registry | `TestAdapterRegistration` |
| frozen math untouched | the `git diff --exit-code` gate, `make golden-check` |

## 10. Notes for the reviewer

- Every frozen file stays byte-identical. The new constant `MaxBetsPerRound` lives in `round.go`.
- Cost: each `CashOut` costs one `Exp` in `Decide` and one in `Evolve` (about 70 µs each). `InitialState` costs at most 14. Each event copies the bet slice, so a round is O(n²) with n ≤ `MaxBetsPerRound`.
- Open for the engineer: confirm `MaxBetsPerRound = 10_000` and the `CloseRound`/`SettleRound` event split (§2 items 3 and 7). Both can change freely until the first rules release.

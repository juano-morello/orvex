# Plan: Generic game contract and versioned adapter (LBT-6 / ADR-003)

## Goal

Create the `internal/game` package:

- a typed, pure rules contract `Game[S, C, E, Cfg]` with `InitialState`, `Decide` and `Evolve`;
- the deterministic input envelope `Input`;
- a type-erased `Adapter` built with the generic `Adapt` function;
- an immutable `Registry` keyed by game ID and rules version, with `rulesVersion`, `schemaVersion` and `stateVersion` kept as three distinct types (ADR-003 "Replay").

There is no reflection, no plugin loading and no global or `init()` registration. Tests demonstrate all three LBT-6 acceptance points: a sample typed game compiles, the pure functions have no I/O, and no call mutates caller state.

Governing ADRs: ADR-003 (contract), ADR-001 (replay, historical rules), ADR-006 (fail closed, fuzz/property tests), ADR-002 (where the envelope's values come from), ADR-007 (layout, single module).

## Current state (verified)

- `internal/game/` does not exist. The only `internal/` package is `internal/buildinfo`.
- `go.mod`: `module github.com/juano-morello/orvex`, `go 1.26.0`, no `require`, no `go.sum`. This change adds no dependencies.
- Toolchain: go1.26.5 and golangci-lint 2.14.0. cgo works, so `make test` runs `-race`.
- `.golangci.yml` uses linters `default: standard` (errcheck, govet, ineffassign, staticcheck, unused) plus the `gofmt` and `goimports` formatters, with local prefix `github.com/juano-morello/orvex`. Staticcheck's ST1005 applies, so error strings must be lowercase with no trailing punctuation. Its SA4000 flags `x == x`.
- Test style in the repo uses the standard `testing` package only, table-driven tests and `t.Fatalf`/`t.Errorf`. There are no assertion libraries.
- Gates (`odin/odin.config.yaml`): `make test`, `make lint`, `go vet ./...`, `make build`.
- Checked: Go infers all four type arguments in `game.Adapt(desc, tally{})` from the methods of `tally{}` (Go ≥1.21 method-based inference). It compiled and ran under go1.26.5. Callers do not need explicit type arguments.

## ADR alignment and flags (copy into the PR description)

No ADR is contradicted. Three refinements fill details that ADR-003 leaves open. Name them in the PR description, as AGENTS.md "Flag conflicts explicitly" requires:

1. **Config type parameter.** ADR-003 §Decision names the contract `Game[S,C,E]` and gives `InitialState(config)` without a type for config. To keep `InitialState` typed, the plan appends a fourth type parameter: `Game[S, C, E, Cfg]`. S, C and E keep the ADR's order and meaning. The alternatives were worse. `any` or `[]byte` config would be untyped, and `[]byte` would force codec decisions that belong to LBT-9. A concrete `game.Config` struct would preempt LBT-8's round configuration. If the reviewer reads this as a deviation from §Decision rather than a refinement, the fix is a one-line ADR-003 amendment that writes the signature as `Game[S,C,E,Cfg]`. Do not silently drop the type parameter.
2. **Error returns.** All three methods return an `error`. `Decide` uses it for deterministic rejections (ADR-003 §Crash v1 domain: "Invalid commands produce durable terminal command results without necessarily emitting domain events"). `Evolve` uses it for inapplicable events (ADR-003 §Invariants: "validated event applicability"). `InitialState` uses it for invalid config.
3. **Purity is enforced by contract and by tests, not at runtime.** The adapter cannot deep-copy or freeze arbitrary `S` without reflection, which LBT-6 rules out. Instead the package doc states the purity rules, and the tests below demonstrate them for the sample game and guard the package's imports. LBT-8 will apply the same kind of tests to Crash.

`Input.EffectiveTimeUnixMicro` is an `int64` count of microseconds rather than a `time.Time`. ADR-002 makes the database clock authoritative, and PostgreSQL `timestamptz` has microsecond precision. A plain integer is comparable with `==`. It carries no monotonic reading or `*Location`, and it cannot be used for location-dependent calls such as `Hour()`. That keeps it deterministic by construction. This is consistent with ADR-002 and is noted here for the reviewer.

## Out of scope (do not create or change)

- Crash anything: rules, states, commands, events, projection, lifecycle authority (LBT-8). Crash math, `cockroachdb/apd/v3`, golden vectors (LBT-7). Canonical event/state codec, state hashing, hash chains, the `crashctl` audit/replay CLI (LBT-9). Persistence, transport, admission, sequencing.
- Wiring the registry into `cmd/engine` or `cmd/crashctl`, because there is no real game to register yet. The binaries stay unchanged.
- A reusable conformance/test-helper package such as `internal/game/gametest`, an `internal/game/crash/` directory, or any non-test sample game. ADR-003: "Avoid premature abstractions for hypothetical games." ADR-007: create directories only when they hold real code.
- Encoding/decoding hooks on `Adapter`, a `Latest(game)` lookup, a listing API, sealing `Adapter`, and fields on `Input` beyond the two below.
- `go.mod` changes, new dependencies, `Makefile`, CI, `AGENTS.md`, `docs/adr/*`, anything under `odin/` or `.omp/`.

## Files

| File | Action |
|---|---|
| `internal/game/contract.go` | new: package doc, `Game`, `Input` |
| `internal/game/registry.go` | new: version types, `Descriptor`, `Adapter`, `Adapt`, `Registry`, `NewRegistry`, `Lookup`, sentinel errors |
| `internal/game/sample_game_test.go` | new: test-only sample typed game `tally` (package `game_test`) |
| `internal/game/contract_test.go` | new: purity, determinism, no-mutation, no-I/O tests and fuzz target |
| `internal/game/registry_test.go` | new: adapter and registry tests |
| `README.md` | edit: status line and one layout bullet |

All test files use the external test package `game_test`, so the tests exercise only the exported API, which is what `internal/game/crash` will use.

---

## 1. `internal/game/contract.go` (new)

Imports: none. Everything below is declarations only.

```go
// Package game defines the typed rules contract every Orvex game implements
// (ADR-003) and the versioned, type-erased adapter and registry through which
// the runtime and the audit verifier select a rules version (ADR-001, ADR-006).
//
// It is deliberately small: no plugin loading, no reflection and no global
// registration. Games are wired explicitly with Adapt and NewRegistry.
package game

// Game is the typed rules contract for one game at one rules version
// (ADR-003). S is the state, C the command, E the event and Cfg the
// configuration InitialState builds the first state from.
//
// InitialState, Decide and Evolve are pure and deterministic:
//
//   - No I/O, no clock reads and no uncontrolled randomness. Time and
//     ordering come from Input; random values (for example a round seed) are
//     generated by the runtime and passed in as recorded data.
//   - No mutation of arguments, including memory reachable through maps,
//     slices or pointers inside S, C, E or Cfg. Build new values instead
//     (copy-on-write); never append into a slice taken from an argument.
//   - Identical arguments give identical results and identical errors, so
//     results must not depend on map iteration order, goroutine scheduling
//     or package-level mutable state. A Game value holds no mutable state
//     and is safe for concurrent use.
//
// The runtime owns admission, sequencing, persistence, ownership and
// transport. A Game sees none of them.
type Game[S, C, E, Cfg any] interface {
	// InitialState returns the first state of a new aggregate built from
	// config, or an error if config is invalid.
	InitialState(config Cfg) (S, error)

	// Decide validates command against state and returns the events it
	// produces. A non-nil error rejects the command: no events apply and
	// the runtime records a terminal command result (ADR-003). Rejections
	// must be as deterministic as events, because decision replay re-runs
	// Decide with the recorded Input and compares the results.
	Decide(state S, command C, in Input) ([]E, error)

	// Evolve applies one event to state and returns the next state. It
	// returns an error when event does not apply to state (ADR-003
	// "validated event applicability"); replay treats that as a failure.
	Evolve(state S, event E) (S, error)
}

// Input is the deterministic input envelope: the facts Decide may use
// besides state and command. The runtime fixes them at durable admission
// (ADR-002) and records them with the command, so decision replay hands
// Decide the same values (ADR-001; ADR-003 "Replay"). Input holds only
// plain values: it is comparable and copying it never shares memory.
//
// Add a field only when a game needs another admission-time fact. Before
// the first rules release that is a free change; after it, it changes what
// is recorded for replay.
type Input struct {
	// Sequence is the command's authoritative per-round admission
	// sequence (ADR-002).
	Sequence uint64

	// EffectiveTimeUnixMicro is the command's durable effective admission
	// time (ADR-002) in microseconds since the Unix epoch, UTC, matching
	// PostgreSQL timestamptz precision. Every time-dependent rule uses it;
	// a game never reads a clock.
	EffectiveTimeUnixMicro int64
}
```

Wording may be tightened, but every rule above has to stay in the doc comments, because the doc is the contract LBT-8 implements against.

## 2. `internal/game/registry.go` (new)

Imports: `errors`, `fmt` only. Do not import `reflect`; `TestNoIOImports` enforces this. Of `fmt`, only `fmt.Errorf` may be used.

### Version identity

```go
// ID names a game, for example "crash". It is the first half of a registry key.
type ID string

// RulesVersion identifies one immutable Decide/Evolve implementation of a
// game. It is the second half of a registry key and is never reused for
// different behavior (ADR-001, ADR-007). Zero is invalid.
type RulesVersion uint32

// SchemaVersion identifies the shape of the commands and events a rules
// version reads and writes (their canonical encoding is defined
// elsewhere). Zero is invalid.
type SchemaVersion uint32

// StateVersion identifies the shape of a rules version's state, as stored
// in checkpoints and covered by the state hash. Zero is invalid.
type StateVersion uint32

// Descriptor identifies a registered rules implementation. ADR-003 keeps
// the three versions separate: several rules versions may share a schema
// or state version, and a rules fix that leaves payloads unchanged does not
// bump SchemaVersion.
type Descriptor struct {
	Game          ID
	RulesVersion  RulesVersion
	SchemaVersion SchemaVersion
	StateVersion  StateVersion
}
```

Distinct named types are what keep the versions apart. A typed `SchemaVersion` value passed where a `RulesVersion` is expected does not compile.

### Sentinel errors

```go
var (
	ErrTypeMismatch        = errors.New("game: value has the wrong type for the rules version")
	ErrUnknownRules        = errors.New("game: unknown game rules version")
	ErrInvalidRegistration = errors.New("game: invalid registration")
	ErrDuplicateRules      = errors.New("game: duplicate game rules version")
)
```

Each gets a one-line doc comment. Return them wrapped with `fmt.Errorf("%w: ...", Err..., ...)`, adding the game ID, the rules version and, for mismatches, the role (`config`, `state`, `command` or `event`) and the received dynamic type (`%T`). Tests check only `errors.Is`.

### Adapter

```go
// Adapter is the type-erased runtime boundary for one rules version of one
// game (ADR-003). Values cross it as the game's own Go types boxed in any;
// a value of any other dynamic type, including nil, is rejected with
// ErrTypeMismatch before the game runs. Errors returned by the game pass
// through unwrapped. Build Adapters with Adapt; this is not a plugin
// interface.
type Adapter interface {
	Descriptor() Descriptor
	InitialState(config any) (any, error)
	Decide(state, command any, in Input) ([]any, error)
	Evolve(state, event any) (any, error)
}

// Adapt wraps a typed Game as the Adapter for rules version d. g must be
// non-nil. Type arguments are inferred: game.Adapt(d, rules{}).
func Adapt[S, C, E, Cfg any](d Descriptor, g Game[S, C, E, Cfg]) Adapter
```

The implementation is an unexported generic struct, `adapter[S, C, E, Cfg any]{d Descriptor; g Game[S, C, E, Cfg]}`, with value receivers:

- `InitialState`: `cfg, ok := config.(Cfg)`, and if `!ok` return `nil` plus a wrapped `ErrTypeMismatch`. Call `g.InitialState(cfg)`. On a game error return `nil, err`, unwrapped. Otherwise return `s, nil`.
- `Decide`: assert `state.(S)` and `command.(C)`, in that order. Call `g.Decide(s, c, in)`, passing `in` unchanged. **On a game error return `nil, err` and discard any events the game also returned.** On success, copy the events into a new `[]any` with the same length and order.
- `Evolve`: assert `state.(S)` and `event.(E)`. On a game error return `nil, err` and discard the returned state.
- `Descriptor()` returns `d` exactly.

A type assertion to a type parameter is plain Go. It is not reflection. A nil interface value fails every assertion, so nil is reported as a mismatch.

### Registry

```go
// Registry maps a game and rules version to its Adapter. NewRegistry builds
// it and it never changes afterwards, so it is safe for concurrent use.
// Lookup never falls back to another rules version: history recorded under
// an unregistered version must fail closed (ADR-006). Historical versions
// stay registered next to current ones (ADR-003 "Consequences").
type Registry struct {
	adapters map[key]Adapter
}

type key struct {
	game  ID
	rules RulesVersion
}

// NewRegistry validates and indexes adapters.
func NewRegistry(adapters ...Adapter) (*Registry, error)

// Lookup returns the Adapter registered for game at rules version rules,
// or an error wrapping ErrUnknownRules.
func (r *Registry) Lookup(game ID, rules RulesVersion) (Adapter, error)
```

`NewRegistry` rules. On any error it returns a `nil` registry:

- a nil `Adapter`, an empty `Game`, or a zero `RulesVersion`, `SchemaVersion` or `StateVersion` → wrapped `ErrInvalidRegistration`;
- a second adapter with the same `(Game, RulesVersion)` → wrapped `ErrDuplicateRules`, even when its schema or state version differs;
- `NewRegistry()` with no arguments returns a valid empty registry.

`Lookup` returns `nil` plus a wrapped `ErrUnknownRules` on a miss. There is no mutex, because nothing writes after construction.

## 3. `internal/game/sample_game_test.go` (new, package `game_test`)

This is the test-only sample typed game. Imports are restricted to `errors`, `maps`, `slices` and `github.com/juano-morello/orvex/internal/game`, and `TestNoIOImports` checks that. Keep it close to this shape. It must not resemble Crash: no bets, multipliers or phases.

```go
// tally is a minimal typed game: players score points up to a limit until
// the tally is closed. It is a test fixture that proves the contract
// compiles for a concrete game and gives the purity tests a subject.
type tally struct{}

var _ game.Game[tallyState, tallyCommand, tallyEvent, tallyConfig] = tally{}

type tallyConfig struct{ MaxPoints int }

type tallyState struct {
	MaxPoints int
	Total     int
	Closed    bool
	Scores    map[string]int // reference type: an in-place write would leak to the caller
	History   []uint64       // admission sequences applied, in order; also a reference type
}

type tallyCommand interface{ isTallyCommand() } // sealed sum type
type score struct{ Player string; Points int }
type closeTally struct{}

type tallyEvent interface{ isTallyEvent() } // sealed sum type
type scored struct{ Player string; Points int; Seq uint64; AtUnixMicro int64 }
type closed struct{ Seq uint64; AtUnixMicro int64 }
```

Sentinel errors are lowercase with a `tally:` prefix: `errInvalidConfig`, `errClosed`, `errInvalidPoints`, `errOverLimit`, `errUnknownCommand`, `errUnknownEvent`.

Behavior:

- `InitialState(cfg)`: if `MaxPoints <= 0` return `errInvalidConfig`. Otherwise return `tallyState{MaxPoints: cfg.MaxPoints, Scores: map[string]int{}}`.
- `Decide(s, c, in)` reads only. `s.Closed` gives `errClosed`. `score` with `Points <= 0` gives `errInvalidPoints`. `score` that would take `Total+Points` above `MaxPoints` gives `errOverLimit`. A valid `score` gives `[]tallyEvent{scored{Player, Points, in.Sequence, in.EffectiveTimeUnixMicro}}`, and `closeTally` gives `[]tallyEvent{closed{in.Sequence, in.EffectiveTimeUnixMicro}}`. Any other command gives `errUnknownCommand`. Event time and sequence come only from `in`.
- `Evolve(s, e)` is copy-on-write. `s.Closed` gives `errClosed`. Otherwise:
  - `next := s`, then `next.Scores = maps.Clone(s.Scores)`. If the clone is nil, use `map[string]int{}`, because `maps.Clone(nil)` is nil and a write to it would panic.
  - `next.History = slices.Clone(s.History)`, then append. Never `append(s.History, …)`, because that can write into the caller's backing array.
  - `scored`: if `Total+Points > MaxPoints` return `errOverLimit` (applicability check). Otherwise add to `Scores[Player]` and `Total`, and append `Seq`.
  - `closed`: set `Closed` and append `Seq`.
  - Any other event gives `errUnknownEvent`.
  - On error return `tallyState{}, err`.

Also put the shared fixtures in this file:

- `baseState() tallyState`: a literal with `MaxPoints: 100, Total: 15, Scores: map[string]int{"alice": 10, "bob": 5}` and `History: append(make([]uint64, 0, 8), 1, 2)`. **The spare capacity is deliberate**, so the append-aliasing checks can catch writes past `len`.
- `sampleDescriptor(rules game.RulesVersion) game.Descriptor`, returning `{Game: "tally", RulesVersion: rules, SchemaVersion: 1, StateVersion: 1}`.

## 4. `internal/game/contract_test.go` (new, package `game_test`)

### Shared helpers (test code, free to import `reflect`, `go/*` and so on)

- `erased{a game.Adapter}` implements `game.Game[tallyState, tallyCommand, tallyEvent, tallyConfig]` by calling the type-erased adapter and type-asserting the results back. `[]any` becomes `[]tallyEvent`, and a nil `any` becomes the zero `tallyState`. Add the compile-time assertion `var _ game.Game[...] = erased{}`. This lets every purity check run against both the typed game and the runtime boundary.
- `stateSnapshot`/`takeSnapshot(s tallyState)` deep-copies `s`: `maps.Clone` for `Scores`, `slices.Clone` for `History`, plus a copy of `s.History[:cap(s.History)]` (the full backing array). `snapshot.changed(s) bool` reports whether `reflect.DeepEqual(snapshot.state, s)` is false **or** the full-capacity backing array differs.
- `purityViolation(g game.Game[tallyState, tallyCommand, tallyEvent, tallyConfig], s tallyState, c tallyCommand, in game.Input) string` takes a snapshot of `s` and runs `Decide(s, c, in)` twice. The events must be `reflect.DeepEqual` and the errors equal, meaning both nil or the same `Error()` text. It then runs `Evolve(s, e)` twice for every produced event, applying each to the original `s`; the next states and errors must be equal. After each call it checks `snapshot.changed(s)`. It returns a description of the first breach, or `""` if everything was pure.
- `mustBeComparable[T comparable]() {}` with `var _ = mustBeComparable[game.Input]`. This compile-time guard keeps `Input` comparable and free of reference fields. Do **not** write `game.Input{} == game.Input{}`, which trips SA4000.

### Tests

| Test | Asserts |
|---|---|
| `TestSampleGamePurity` | Table of named cases on `baseState()`: accepted `score`; rejected over-limit `score`; rejected `Points: 0`; accepted `closeTally`; `score` against a closed state (set `Closed: true` on a fresh `baseState()`). Each case runs as subtests `typed` (`tally{}`) and `adapter` (`erased{game.Adapt(sampleDescriptor(1), tally{})}`). Each subtest requires `purityViolation(...) == ""`. **This is the "no mutation of caller state" acceptance test.** |
| `TestPurityCheckDetectsMutation` | Negative control, which earns the test above. Three leaky variants embed `tally` and override one method: `decideWritesMap` (Decide does `s.Scores["leak"]++`), `evolveWritesMap` (Evolve writes `s.Scores` in place) and `evolveAppendsShared` (Evolve does `s.History = append(s.History, 0)` with no clone). With an accepted `score` on `baseState()`, each must make `purityViolation` return a non-empty string. The third row only fails because of the full-capacity backing check. |
| `TestSampleGameConcurrentUse` | 8 goroutines call `Decide` and then `Evolve` on one shared `baseState()` value with the same command and input. All results equal the sequential result, and the shared state is unchanged afterwards. Under `-race` (`make test`), any write to the shared map or slice fails the run. This backs the doc claim that a Game is safe for concurrent use. |
| `FuzzSampleGamePurity` | Arguments `(player string, points int, seq uint64, at int64, closeIt bool)`. Seed with `f.Add` rows covering accepted, over-limit, zero/negative points, close, and extreme `seq`/`at` (0, max). The body builds the command (`closeTally` if `closeIt`, otherwise `score{player, points}`) and `in := game.Input{Sequence: seq, EffectiveTimeUnixMicro: at}`, then requires `purityViolation == ""` for both `tally{}` and the `erased` adapter on `baseState()`. Plain `go test` runs only the seed corpus, which is ADR-006's "fuzz smoke". |
| `TestNoIOImports` | **This is the "pure functions have no I/O" acceptance test, a static guard.** It globs `*.go` in the package directory and checks every non-`_test.go` file plus `sample_game_test.go`. It parses each file with `go/parser` (full parse, not `ImportsOnly`, because selectors are inspected). Every import path must be in the allowlist {`errors`, `fmt`, `maps`, `slices`, `github.com/juano-morello/orvex/internal/game`}. The allowlist leaves out `os`, `io`, `net`, `time`, `math/rand`, `crypto/rand`, `sync`, `unsafe` and `reflect`, so I/O, clock reads, uncontrolled randomness and reflection cannot creep in. In addition, every `fmt.X` selector must be `fmt.Errorf`, which blocks `fmt.Print*` and `fmt.Scan*`. The test also asserts that `contract.go`, `registry.go` and `sample_game_test.go` were all checked, so it cannot pass vacuously. |

## 5. `internal/game/registry_test.go` (new, package `game_test`)

| Test | Asserts |
|---|---|
| `TestAdapterMatchesTypedGame` | Runs one script through `tally{}` and through `erased{game.Adapt(sampleDescriptor(1), tally{})}` side by side: `InitialState({MaxPoints: 20})`, then `Decide`+`Evolve` for `score{alice,5}` at `Input{Sequence: 1, EffectiveTimeUnixMicro: 1_000}`, an over-limit `score{bob,50}` at seq 2 (rejected, `errors.Is(err, errOverLimit)`), and `closeTally` at seq 3 / time 3_000. Every event, error and resulting state must be equal across the two paths. The emitted events must carry exactly the `Sequence` and `EffectiveTimeUnixMicro` that were passed in, which proves the adapter forwards `Input` unchanged. `adapter.Descriptor()` must equal `sampleDescriptor(1)`. |
| `TestAdapterRejectsWrongTypes` | Table on `a := game.Adapt(sampleDescriptor(1), tally{})`: `InitialState("x")`, `InitialState(nil)`, `Decide(1, score{}, in)` (bad state), `Decide(baseState(), nil, in)`, `Decide(baseState(), scored{}, in)` (an event passed as a command), `Evolve(nil, scored{})`, `Evolve(baseState(), nil)`, `Evolve(baseState(), score{})` (a command passed as an event). Each must return `errors.Is(err, game.ErrTypeMismatch)` and a nil result (nil state or nil events). |
| `TestAdapterGameErrors` | (a) An over-limit `score` through the adapter returns `errors.Is(err, errOverLimit)`, `!errors.Is(err, game.ErrTypeMismatch)` and nil events. (b) `InitialState(tallyConfig{})` returns `errInvalidConfig` and a nil state. (c) A stub `sloppy` (embeds `tally`) has `Decide` return `[]tallyEvent{closed{}}, errClosed` and `Evolve` return `baseState(), errClosed`. Through the adapter, `Decide` must give nil events and `Evolve` a nil state, both with `errors.Is(err, errClosed)`. |
| `TestRegistryLookup` | `NewRegistry(game.Adapt(v1, tally{}), game.Adapt(v2, tally{}))` with `v1 = {tally, Rules 1, Schema 1, State 1}` and `v2 = {tally, Rules 2, Schema 1, State 2}`. The two rules versions share a schema version but differ in state version, which shows the three versions are carried independently. `Lookup("tally", 1).Descriptor() == v1` and `Lookup("tally", 2).Descriptor() == v2`. Both stay available, which is the historical-rules requirement. |
| `TestRegistryLookupFailsClosed` | Table: unregistered rules version `Lookup("tally", 3)` on the registry above; unknown game `Lookup("crash", 1)`; zero values `Lookup("", 0)`; any lookup on `NewRegistry()` with no adapters. Each returns `errors.Is(err, game.ErrUnknownRules)` and a nil adapter. For version 3 the test explicitly checks that v2 is *not* returned. |
| `TestNewRegistryRejectsInvalid` | Table. Nil adapter, empty `Game`, zero `RulesVersion`, zero `SchemaVersion` and zero `StateVersion` each wrap `game.ErrInvalidRegistration`. Two adapters with the same game and rules version but different schema/state versions wrap `game.ErrDuplicateRules`. Every row also returns a nil `*Registry`. |
| `TestRegistryConcurrentLookup` | 16 goroutines each call `Lookup` for v1 and v2 many times on one registry and check the descriptors. This backs the "safe for concurrent use" doc claim under `-race`. |

## 6. `README.md` (edit)

- Status line. Replace `Status: scaffold ([LBT-5](...)). No game, runtime or persistence logic exists yet.` with:
  `Status: scaffold ([LBT-5](https://linear.app/lbtworks/issue/LBT-5/scaffold-go-module-and-repository-conventions)) plus the game contract ([LBT-6](https://linear.app/lbtworks/issue/LBT-6/implement-generic-game-contract-and-versioned-adapter)). No Crash rules, runtime or persistence logic exists yet.`
- Layout. Add a bullet after `internal/buildinfo`:
  `` - `internal/game`: typed game contract, deterministic input envelope, versioned adapter and rules registry (ADR-003) ``

Make no other README edits.

## Builder pitfalls

- `unused` linter: the sealed-interface marker methods (`isTallyCommand`, `isTallyEvent`) and the leaky/sloppy stub overrides must actually be reached through the interfaces or tests. Run `make lint` to confirm.
- ST1005: error strings must be lowercase, with no trailing period, including the test sentinels.
- `goimports` local prefix: group `github.com/juano-morello/orvex/...` imports separately after the standard library.
- Do not add a `testdata/` fuzz corpus. The seed rows go in `f.Add`.
- `TestNoIOImports` resolves paths relative to the package directory, which is the working directory when `go test` runs. Use `filepath.Glob("*.go")` and do not hard-code absolute paths.
- No `t.Parallel()` is needed. If you use it, keep the fixtures free of shared mutation by calling `baseState()` per test.

## Verification (the builder runs these and judges by exit status)

1. `go vet ./...`
2. `go test -race -count=1 -v ./internal/game/` must pass. Every test named above must appear in the `-v` output.
3. `go test -run '^$' -fuzz '^FuzzSampleGamePurity$' -fuzztime 15s ./internal/game/` must exit 0. It must not leave a `testdata/` directory behind. If it does, a real failure was found and must be fixed, not committed.
4. Optional negative check, reverted before handoff: add `import _ "os"` to `contract.go` and confirm `go test -run TestNoIOImports ./internal/game/` fails; then revert and confirm `git diff --stat` shows only the files in the table above.
5. `make test`, `make lint` and `make build` must all exit 0.

## Acceptance mapping (LBT-6)

| Acceptance | Demonstrated by |
|---|---|
| Sample typed game compiles | `var _ game.Game[tallyState, tallyCommand, tallyEvent, tallyConfig] = tally{}` in `sample_game_test.go`; `game.Adapt(..., tally{})` inferring the type arguments in every adapter and registry test |
| Pure functions have no I/O | `TestNoIOImports`; determinism in `TestSampleGamePurity`, `FuzzSampleGamePurity` and `TestSampleGameConcurrentUse` |
| No mutation of caller state | `TestSampleGamePurity` (typed and adapter paths, full-capacity backing check), `TestSampleGameConcurrentUse` under `-race`, and `TestPurityCheckDetectsMutation` as the negative control |
| Versioned type-erased adapter | `TestAdapterMatchesTypedGame`, `TestAdapterRejectsWrongTypes`, `TestAdapterGameErrors` |
| Registry keyed by game and rules version; three versions distinct | `TestRegistryLookup`, `TestRegistryLookupFailsClosed`, `TestNewRegistryRejectsInvalid`, `TestRegistryConcurrentLookup`; distinct named types in `registry.go` |
| Gates | `make test`, `make lint`, `make build`, `go vet ./...` |

## Deferred decisions (record in PR, do not implement)

- How Crash receives its seed and commitment, through `Cfg` or a system command, and whether `Input` needs an actor/authority field for "lifecycle commands require system authority". Both belong to LBT-8. Adding an `Input` field before the first rules release is a free change.
- The encoding of rejections in terminal command results and of canonical events and state belongs to LBT-9 and the runtime work.
- Wiring the registry into `cmd/engine` and `cmd/crashctl` happens once Crash v1 exists.

# Game contract and versioned adapter (LBT-6)

## What changed
New package `internal/game` implements the ADR-003 game contract and a versioned runtime boundary. It contains no Crash rules, codec, persistence or admission logic (LBT-7/8/9 and runtime work are out of scope). The README status line and layout list now mention the package.

## Where it lives
- `internal/game/contract.go`
  - `Game[S, C, E, Cfg]`: `InitialState(config) (S, error)`, `Decide(state, command, Input) ([]E, error)`, `Evolve(state, event) (S, error)`. Doc comments state the purity rules: no I/O, clock or uncontrolled randomness; no mutation of arguments, including memory reachable through maps, slices or pointers; identical inputs give identical results and errors. A non-nil `Decide` error rejects the command; `Evolve` errors on inapplicable events.
  - `Input`: deterministic input envelope, plain comparable values: `Sequence uint64` (per-round admission sequence, ADR-002) and `EffectiveTimeUnixMicro int64` (durable effective time, µs since epoch UTC).
- `internal/game/registry.go`
  - Distinct types `ID`, `RulesVersion`, `SchemaVersion`, `StateVersion` (zero is invalid), grouped in `Descriptor`.
  - `Adapter`: type-erased interface (`Descriptor`, `InitialState`, `Decide`, `Evolve` over `any`). Built with `Adapt(descriptor, typedGame)`; wrong dynamic types, including nil, return `ErrTypeMismatch` before the game runs. Game errors pass through unwrapped.
  - `Registry`: `NewRegistry(adapters...)` validates (`ErrInvalidRegistration`, `ErrDuplicateRules`) and is immutable afterwards; `Lookup(game, rules)` returns `ErrUnknownRules` and never falls back to another version, so unknown history fails closed.
  - No reflection plugin loading and no global registration; wiring is explicit.

## Tests
- `internal/game/sample_game_test.go`: a sample typed tally game (compiles against `Game`).
- `internal/game/contract_test.go`: purity checks run on the typed game and through the adapter (determinism, no caller-state mutation including spare slice capacity), negative controls proving the checks catch leaky games, concurrent use, and a compile-time check that `Input` is comparable.
- `internal/game/registry_test.go`: adapter and registry behavior.

A design spec is at `docs/specs/9d7bead2_game-contract-registry.md`.

## Verify
`make test`, `make lint`, `make build`; or `go test -race ./internal/game/...`.

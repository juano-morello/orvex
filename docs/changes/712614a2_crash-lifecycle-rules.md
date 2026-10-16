# Crash v1 round rules: Decide/Evolve, lifecycle and public projection (LBT-8)

## What changed
`internal/game/crash/v1` now implements the Crash v1 round as a pure `game.Game` (`Rules`), on top of the frozen math (curve, payout, fairness, params), which is untouched. No database, network or clock is involved.

- **Lifecycle:** `BETTING -> RUNNING -> CRASHED -> SETTLED`, forward only. `SETTLED` accepts nothing.
- **Commands:** `PlaceBet`, `CashOut` (need `AuthorityPlayer`); `StartRound`, `CloseRound`, `SettleRound` (need `AuthoritySystem`). The zero `AuthorityNone` is always rejected. Authority is a field on each command and `Decide` enforces it, so replay reproduces rejections. `internal/game/contract.go` is unchanged.
- **Events:** `BetPlaced`, `RoundStarted`, `BetCashedOut`, `RoundCrashed`, `BetLost`, `RoundSettled`.
- **Rejections:** `Decide` returns `*RejectionError{Reason}` (`errors.Is(err, ErrRejected)`, `RejectionReason(err)`) with no events. Codes: `invalid_command`, `unauthorized`, `time_out_of_range`, `wrong_phase`, `round_settled`, `invalid_amount`, `duplicate_bet`, `bet_limit_reached`, `no_bet`, `already_cashed_out`, `not_after_start`, `deadline_passed`, `crash_not_due`. Other errors (`ErrInvalidState`, `ErrArithmetic`) mean fail closed: no events, no terminal result.
- **Event split:** `CloseRound` is accepted only at or after `T0 + D` and emits `RoundCrashed` (reveals crash point, tick, `D`, deadline). `SettleRound` emits one `BetLost` per open bet in placement order, then `RoundSettled` (totals, seed).
- **Public projection:** `Project(State)` copies fields by a per-phase allow-list. Crash outcome is zero before `CRASHED`, seed zero before `SETTLED`. `State` itself holds the secrets from `InitialState` on and must never be sent to clients. No error text formats a secret.

## Invariants enforced
One bet per player; no double cash-out; cash-out only strictly after `T0` and strictly before `T0 + D` (equality loses); no transitions after settlement; `Evolve` validates event applicability (`ErrEventNotApplicable`).

## Resolved spec section 10 items
- **Round ID:** UUID kept (`Config.RoundID [16]byte`); the nil UUID is rejected. D-11 not reopened.
- **T0:** the `StartRound` effective admission time, stored in `RoundStarted.StartedAtUnixMicro`; must be in `[0, MaxStartMicros]`.
- **Aggregates and cap:** new `MaxBetsPerRound = 10_000` in `round.go` (`Config.MaxBets` may narrow, never widen); all sums use checked addition (`ErrArithmetic`). **Needs engineer confirmation**, as does the `CloseRound`/`SettleRound` split. Both may change until the first rules release.
- Also: all-zero seed rejected in `InitialState`; bet-window length is left to the runtime scheduler; registry keys `crash`, rules/schema/state version 1 (`Descriptor()`, `Adapter()`).

No ADR is contradicted. State hashing is left to LBT-9.

## Where
`round.go` (types, config, `InitialState`, constants), `commands.go`, `events.go`, `decide.go`, `evolve.go`, `projection.go`, all in `internal/game/crash/v1/`. Design plan: `docs/specs/712614a2_crash-lifecycle-rules.md`. `README.md` status and layout updated. `params_test.go` `TestNoIOImports` now also covers the new files.

## Verify
Tests: `decide_test.go` (acceptance cases), `evolve_test.go`, `projection_test.go`, `round_test.go`, `purity_test.go` (determinism, no input mutation), `round_fuzz_test.go`.
Run `make test`, `make lint`, `make build`, `make golden-check`. I did not run them while writing this.

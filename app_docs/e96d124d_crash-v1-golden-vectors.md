# Crash v1 math spec freeze and golden vectors (LBT-7)

## What changed
The Crash v1 arithmetic is now frozen in code and in committed golden vectors. The spec `specs/e96d124d_crash-v1-math-spec.md` is marked approved (decisions D-1 to D-14). `specs/e96d124d_crash-v1-implementation.md` is also added.

Package `internal/game/crash/v1` (package `v1`, rules version string `crash/v1`) holds the pure math. It does no I/O, and the Crash lifecycle, commands, events and Decide/Evolve (LBT-8) are not part of it.

- `params.go`: frozen constants. Scale 10,000 (1.0000x), tick 10,000 µs, 1% edge, crash point 1.0000x to 10,000.0000x, `MaxTick` 15,351, bet 1 to 1,000,000,000, `MaxPayoutCredits` 9,996,596,800,000. It also holds the apd context (decimal128, half-even, default traps) and the errors `ErrOutOfRange` and `ErrArithmetic`.
- `curve.go`: `MultiplierAtTick`, `CrashTick`, `DeadlineOffsetMicros`, `CashOutTiming`. Growth is the exact decimal 6E-4 per tick.
- `fairness.go`: `Commitment`, `VerifyCommitment`, `OutcomeMessage` (tag `orvex/crash/v1/crash-point:`), `OutcomeR`, `CrashPointFromR`, `CrashPoint`.
- `payout.go`: `Payout(bet, multiplier)`.
- `go.mod` and `go.sum` pin `github.com/cockroachdb/apd/v3 v3.2.3`.

## Golden vectors
`internal/game/crash/v1/testdata/golden/` holds `crash_points.json`, `deadlines.json`, `fairness.json`, `multiplier_ticks.json`, `params.json` and `payouts.json`. They cover deadlines, rounding, overflow and versioned outcomes.

`generate.py` creates them. It is an independent oracle: Python 3.11+, standard library only. It does not use Go output. Never regenerate the vectors from Go.

## Tests
`curve_test.go`, `fairness_test.go`, `params_test.go`, `payout_test.go`, `fuzz_test.go` and `golden_test.go` check the Go code against the vectors.

## Build and CI
- New `make golden-check` runs `python3 internal/game/crash/v1/testdata/golden/generate.py --check`. It fails if the committed files differ from the regenerated ones. `PYTHON` can be overridden.
- CI runs `make golden-check` after `make test`.
- `AGENTS.md` and `README.md` mention the new target.

## Verify
```
make test
make lint
make build
make golden-check
```

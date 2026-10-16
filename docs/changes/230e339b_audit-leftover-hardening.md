# LBT-23: audit-leftover hardening (M1/M2)

Five small hardening items. The frozen crash/v1 math, constants, golden vectors and fixtures are unchanged. The spec is `docs/specs/230e339b_audit-leftover-hardening.md`.

## What changed

1. **`RoundSettled` re-verifies the reveal** (`internal/game/crash/v1/evolve.go`, `evolveRoundSettled`). Besides `e.Seed == s.Seed`, it now rejects the event as not applicable unless `VerifyCommitment(e.Seed, s.Commitment)` holds and `CrashPoint(e.Seed, s.RoundID) == s.CrashPointScaled`. This is defence in depth for LBT-9 A-12. `InitialState` already guarantees both, so no history crash/v1 can produce changes. Tests: two new cases in `evolve_test.go` (`TestEvolveValidatesApplicability`), plus error-text checks in `purity_test.go` (`TestRoundErrorsDoNotLeakSecrets`).
2. **Max payout is pinned** (`payout_test.go`, `TestMaxPayoutCreditsIsReachableMaximum`). It asserts `MaxPayoutCredits == Payout(MaxBetCredits, M(MaxTick-1))`. It also checks that tick is reachable through `CashOutTiming`, and that `M(MaxTick)` is out of range (math spec D-7).
3. **Admission bet ceiling is pinned** (`internal/runtime/admission/crashv1.go`). The new constant `crashV1MaxHoldCredits = 1_000_000_000` replaces `v1.MaxBetCredits` in `Classify`. `TestCrashV1HoldCeiling` in `crashv1_test.go` requires it to equal 1,000,000,000 and `v1.MaxBetCredits` (S-10).
4. **`TestAdmitConcurrentCrossRoundHolds`** (`concurrency_integration_test.go`) now fails if any admission returns SQLSTATE 23514. This proves the S-11 account row lock, not the floor check, refuses the over-holds.
5. **`crashctl migrate` exit codes** (LBT-10 P-6):
   - `cmd/crashctl/migrate_test.go`: an unknown flag, and several invalid `ORVEX_DATABASE_URL` forms (syntax, bad parameter, bad sslmode, unterminated key=value) across `up`, `down` and `status`. Both URL and key=value unreachable DSNs also map to the I/O exit code.
   - `cmd/crashctl/migrate_integration_test.go` (`TestMigrateCLIExitCodes`, real PostgreSQL):
     - exit 1: SQL error and SQL_ASCII encoding.
     - usage exit: `down` target above the current version.
     - unsupported exit: checksum mismatch and a schema newer than the build.
     - I/O exit: missing database (3D000) and wrong password (28P01).

## Verify

`make test`, `make test-integration` (needs Docker), `make lint`, `make build`, `make golden-check`. No ADR conflict. The change report did not capture command results.

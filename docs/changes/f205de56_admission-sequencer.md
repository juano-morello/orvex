# LBT-11: durable per-round admission sequencer

## What changed

Orvex now has the durable admission step of ADR-002 / ADR-005 "Transaction A". `internal/runtime/admission` calls a new PL/pgSQL function, `orvex.admit_command`, added by migration `0007_admission_window`. Together they give each command a per-round admission sequence and an effective time.

The approved spec is `docs/specs/5a3d52fc_admission-sequencer.md`. The database side is described in `docs/architecture/persistence.md` (section 12). `AGENTS.md` and `README.md` were also touched: `AGENTS.md` now asks for `make test-integration` when you change `internal/runtime/`.

## How it works

- `Sequencer.Admit` encodes the typed command with the round's codec and classifies it with the rules version's `Policy`. It then runs one READ COMMITTED transaction that calls `orvex.admit_command` once and commits or rolls back.
- The result is a durable receipt (`Admission`). It is not an execution result. It is returned only after COMMIT succeeds, or when the same command ID was already admitted (`Duplicate`). `Lookup` reads the status by command ID later.
- The round's `orvex.round_admission_heads` row, locked `FOR UPDATE`, is the per-round serialization point. Sequence order is lock grant order. At most one player account row is locked after it.
- Effective time is the database clock, read once right after the lock.
- Command IDs are globally unique. The same round and bytes return the original admission. Anything else returns `ErrCommandIDReused`.
- The admission window moves open, started, closed, sealed. The transitions are driven by admitted system StartRound, CloseRound and SettleRound commands. Trigger `check_admission_window` raises OX005 if a window moves backwards.
- A CloseRound is admitted only at an effective time at or after the caller's `NotBeforeUnixMicro`. Otherwise `ClosureNotDueError` is returned. A sealed round admits nothing.
- Credit holds are checked at admission. `insufficient_credits` is returned if the hold exceeds the available credits or the player has no account.
- `Policy` (`policy.go`) maps a command to a class (ordinary, start, close, settle) and a hold. `crashv1.go` provides the Crash v1 policy.
- `gate.go` is an in-process gate that bounds the calls per round that hold a pool connection.
- Errors each wrap exactly one sentinel (`errors.go`): `ErrBusy`, `ErrUnavailable`, `ErrOutcomeUnknown`, `ErrCommandIDReused`, and others. Rejections are `*RejectedError` with reasons `round_closed`, `round_sealed`, `lifecycle_order` and `insufficient_credits`.
- Ambiguous commits and connection loss are retried with the same command ID, up to `Config.MaxAttempts` (`retry.go`).

## Flagged deviations (spec section 2)

- **S-5, lock-free fast path.** Duplicates, unknown rounds, and closed or sealed windows are answered before the round lock is taken. Every decision is checked again under the lock. This deviates from LBT-10.
- **S-7, effective-time clamp.** The effective time is the database clock clamped to at least the previous admission's effective time, so it never moves backwards within a round. This deviates from LBT-10.

## Open item from review

After an ambiguous COMMIT followed only by definite failures, S-12 returns `ErrUnavailable`, `ErrBusy` or not-admitted. The earlier COMMIT may in fact have succeeded.

Resubmitting the same command ID resolves it through dedupe. It returns the original admission as a `Duplicate`. Callers must treat these errors as "resubmit", not as a final rejection.

## Files

- Code: `internal/runtime/admission/{sequencer,policy,crashv1,errors,gate,retry,doc}.go`
- Schema: `migrations/0007_admission_window.up.sql` and `.down.sql`
- Migration test plumbing: `internal/persistence/postgres/pgtest/fingerprint.go`, the postgres `migrate_*test.go` files, and `cmd/crashctl/migrate_integration_test.go`
- Other specs added in the diff: `docs/specs/f205de56_admission-sequencer-implementation*.md`

## Verify

- `make test` and `make lint`.
- `make test-integration` (needs Docker). It runs the admission integration tests (admit, barrier, commit, concurrency) and `admission_window_integration_test.go`.

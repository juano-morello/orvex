# LBT-12: atomic execution commit, ledger and outbox

Spec: `docs/specs/e0df34d3_execution-commit-ledger-outbox.md` (approved). Build plan: `docs/specs/6f13b30a_execution-ledger-implementation.md` (and `_v2`).

## What changed
- **`internal/runtime/execution`** (new): `Executor.ExecuteNext(ctx, lease)` takes the next admitted command, runs the pure `Decide`/`Evolve`, and commits Transaction B atomically. B holds the chain records, the terminal result, balanced ledger postings, outbox messages, snapshots and the advanced `processed_sequence`. It is guarded by a compare-and-set, a fencing-token check and the `prev_hash` foreign key. Round state is replayed from the chain and cached in memory, and the cache is discarded and reloaded on any conflict. `Executor.CreateRound` creates a round in one transaction: rows, secret, heads, genesis record, snapshot 0, round ledger account, genesis outbox message. Files: `executor.go`, `commit.go`, `load.go`, `transition.go`, `round.go`, `retry.go`, `errors.go`, `policy.go`, `crashv1.go`.
- **`internal/ledger`** (new, pure): crash/v1 posting rules as deterministic functions of chained events, plus deterministic transaction and account IDs (`crashv1.go`, `ids.go`, `ledger.go`). An import test restricts its dependencies.
- **Migration `0008_execution_ledger`**: unique index for at most one posting per chained event; deferred check `OX009` requiring every hold of an executed admission to be released in the record's transaction.
- **Migration `0009_close_bound`** (engineer addendum, below).
- Edits to `internal/persistence/postgres` (`tx.go`, `errors.go`), `internal/runtime/admission` (`sequencer.go`, `errors.go`), `docs/architecture/persistence.md`, `README.md`, and migration-count assertions in existing tests.

## Engineer addendum: database-stored close bound
`CloseRound` admission no longer trusts the caller-supplied `NotBeforeUnixMicro`.
- `CreateRound` stores the deadline offset `D` (microseconds, `D = n_c * Q`) in a column only the `SECURITY DEFINER` function `orvex.admit_command` reads. The admitting role has no grant on it.
- Admitting `StartRound` records `close_not_before = T0 + D` on the admission head (`T0` is that admission's effective time).
- `admit_command` refuses a `CloseRound` with an effective time before the bound, using the existing not-yet-due rejection class. At exactly `T0 + D` it is admitted.
- An early `CloseRound` can therefore no longer close the window. This resolves the inherited risk in flagged point 14.
- It changes LBT-11 S-8, the admitting role's grants and E-16. Tests: `internal/runtime/execution/close_bound_integration_test.go`, `internal/persistence/postgres/close_bound_integration_test.go` and `internal/runtime/admission/role_integration_test.go`.

## Spec section 2 flagged points
No ADR is contradicted. Deviations marked "needs approval" were approved with the spec.
1. Deviation from LBT-10 P-23: B's `BEGIN` is sent with one `set_config` statement (timeouts, `synchronous_commit` only when it is `off`). It reads no data, so the compare-and-set is the first data-touching statement.
2. Interpretation of P-23 isolation: single-statement reads of immutable rows run outside an explicit transaction. The cold load uses `SnapshotReadTxOptions()`.
3. The in-memory cache is the "known state version". The chain stays authoritative.
4. `round_snapshots.state` stays NULL. Recovery replays from genesis and snapshots act as checkpoints. Snapshot-based recovery is left to LBT-14.
5. Addition: the 0008 unique posting index and the `OX009` hold-release check.
6. A floor-check violation in Transaction B is a bug (`ErrInternal`), not a business outcome.
7. Outbox payload is the exact chain record bytes, topic `round.record.v1`. LBT-13 defines the wire format.
8. Deviation: the ledger stage and history format v2 of LBT-9 §11 are not added. Postings stay outside the hash chain.
9. One net settlement posting at `RoundSettled` instead of one per `BetLost`.
10. LBT-12 owns production round creation.
11. `internal/ledger` is a new top-level internal package.
12. A command whose `Decide` fails closed gets no record. The round halts and its hold stays held until operators act.
13. The fencing token is checked, not `lease_expires_at`.
14. Resolved by the addendum.
15. LBT-11 is treated as approved. If S-10, S-11, S-12 or §7 changed, E-4, E-10 and E-15 need review.
16. Existing migration tests (`TestEmbeddedMigrations`, `TestMigrateCLI`, `TestMigrateDownRefusesAuditData`) were updated.
17. Failures are injected in tests only. Production failpoints belong to LBT-15.
18-20. Ledger references alignment, a public projection with no secrets in the outbox, and no new Go module.

## Follow-up issues (not done here)
- **LBT-18**: audit ledger stage (history format v2). The verifier recomputes postings from chained events with `internal/ledger`, compares them with exported ledger rows, and updates the oracle and golden files.
- **LBT-19**: `crashctl audit export ROUND_ID`.
- Also out of scope: the outbox relay and wire format (LBT-13), and leases, the engine loop and snapshot state (LBT-14).

## Verify
`make test`, `make lint`, `make test-integration` (needs Docker). Run `make golden-check` if the vectors are touched. I did not re-run them for this write-up.

# ADR-006: Correctness & Verification Strategy

Source: https://linear.app/lbtworks/document/adr-006-correctness-and-verification-strategy-83ecdddbbeeb

Status: Accepted

## Decision

Define executable game, financial, distributed and audit invariants. Implement layered verification: pure unit tests, Go fuzz/property tests, golden arithmetic and replay vectors, independent audit CLI, real PostgreSQL integration tests using Testcontainers for Go, model-based concurrency testing, deterministic failpoints, process/Kubernetes chaos, and workload-aware load tests.

## Audit verifier

Validate canonical event hash chain, state replay hash, historical decision reproduction, balanced ledger, and seed commitment/reveal. Fail closed on unknown historical schema/rules artifacts. Keep visual replay separate from cryptographic verification.

## Critical failure tests

Duplicate admission, conflicting per-round commits, double cash-out, cross-round overspending, rollback on ledger failure, ambiguous commit response, stale fencing token, crash before/after commit, settlement barrier, pod termination and recovery.

## Performance experiments

Compare many independent rounds, one hot round, cash-out burst near crash, retry storms, and pod failure under load. Measure admission and execution p50/p95/p99, backlog, lock waits, WAL and connection pool saturation. Targets (10k rounds, 5k aggregate commands/s, p99 <100ms, zero observed invariant violations) are hypotheses requiring specified hardware/workload, not promises.

## CI

PR: unit, golden, fuzz smoke, PostgreSQL integration, race/static checks. Nightly/release: extended fuzz, multi-replica chaos, load and replay verification. No distributed feature is complete without a reproducible failure test. Testing alone is not formal proof; bounded model checking/TLA+ is a possible later experiment.

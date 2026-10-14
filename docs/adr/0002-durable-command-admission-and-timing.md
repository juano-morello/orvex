# ADR-002: Durable Command Admission & Timing

Source: https://linear.app/lbtworks/document/adr-002-durable-command-admission-and-timing-be124b9ab8e6

Status: Accepted with amendment

## Context

Cash-outs near the crash deadline must have reproducible eligibility even when gateway and engine instances disagree about time or process commands late.

## Decision

Separate durable command admission from deterministic execution. Assign an authoritative per-round admission sequence and effective timestamp; initially use PostgreSQL row-lock sequencing and a database clock read after acquiring the lock. Gateway receive time is diagnostic, not authoritative. A cash-out is timing-eligible only when its durable effective admission time is strictly before the crash deadline and after round start, subject to other game invariants. Admission acknowledgment means durable receipt, not successful execution. Deduplicate retries by stable command ID. The admission sequencer is replaceable; benchmark a fenced owner-based sequencer later.

## Important semantics

Use the same serialization boundary for admission and round closure/settlement barrier. Do not equate transaction start time with lock-acquisition time; PostgreSQL now() is transaction-start time, so consider clock_timestamp() after lock acquisition. Define handling for duplicate IDs, unknown commit outcomes, and deadline equality.

## Trade-offs

Hot-round row contention and database dependency versus clear auditable ordering. Database admission time can penalize requests delayed in queues. It is a server fairness policy, not proof of client click time.

## Open validation

Benchmark contention and clock behavior; specify exact admission SQL, rejection evidence, late-command policy and failover guarantees.

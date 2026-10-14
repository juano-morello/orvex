# ADR-001: Audit-First Deterministic Execution

Source: https://linear.app/lbtworks/document/adr-001-audit-first-deterministic-execution-901f8bd77d96

Status: Accepted

## Context

Orvex must explain and reproduce every committed outcome in a shared multiplayer Crash round, including after crashes and software upgrades.

## Decision

The immutable, ordered domain event history is the authoritative record; current state is a projection. Support (1) state replay by evolving recorded events, (2) decision replay by reexecuting original admitted commands and deterministic inputs with historical rules, and (3) visual playback from recorded timeline metadata. Each round is a consistency and ownership boundary. Preserve original commands, effective times, event and schema versions, rule/build identifiers, randomness commitments/reveals, state checkpoints and ledger references. Use canonical event/state encodings and per-round hash chains, with independent anchoring as a future strengthening measure.

## Invariants

Contiguous event sequence; replay yields identical canonical state hash; a bet settles at most once; every decision is attributable to a command and rules version; historical rules remain reproducible.

## Trade-offs and limitations

Event schema evolution, storage, and verifier complexity. Hash chains in the same mutable database are tamper-evident only against a separately trusted anchor. Visual replay cannot prove exactly what an individual client saw.

## Implementation notes

Define canonical encoding, verification CLI, retention and artifact versioning before claiming audit-grade behavior.

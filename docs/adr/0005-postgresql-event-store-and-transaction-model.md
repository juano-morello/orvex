# ADR-005: PostgreSQL Event Store & Transaction Model

Source: https://linear.app/lbtworks/document/adr-005-postgresql-event-store-and-transaction-model-d2b62c7cf5e2

Status: Accepted

## Decision

PostgreSQL is the authoritative system of record for rounds, immutable command admissions, terminal command results, append-only domain events, simulated-credit double-entry ledger, outbox, and eventual snapshots. It is not necessarily the permanent sequencing mechanism.

## Transaction A: admission

Serialize admission per round; check command ID deduplication and admission window; assign sequence and effective timestamp; insert immutable admission; commit before acknowledging admitted status. The round closure/barrier must coordinate through the same serialization boundary.

## Transaction B: execution

Read next admitted command and known state version; Decide and Evolve deterministically; atomically validate expected version, next processed admission sequence and ownership fencing token, then append events, one terminal result, balanced ledger postings, outbox records, and advance processed/state versions. On conflict discard computed transition and retry from authoritative state.

## Failure semantics

Before admission commit: retry may create admission. After admission commit: retry returns original admission. Before execution commit: no partial outcome. After execution commit but before response: return existing durable result. Stale owner cannot commit. Outbox delivers at least once; consumers deduplicate.

## Cross-round balances

Protect shared player accounts with account-level transactional debit/reservation checks, not only round serialization. Settlement barriers must prevent finalization while earlier admitted commands remain unresolved.

## Trade-offs

Two durable transaction boundaries and per-round lock contention; benchmark owner sequencing later. ACID alone is not a backup/replication/retention policy.

## Open validation

Exact SQL schema, isolation, deferred ledger balance enforcement, immutable audit storage, outbox retry and backup/restore tests.

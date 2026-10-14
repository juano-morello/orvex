# ADR-004: Multiplayer Crash Game Specification

Source: https://linear.app/lbtworks/document/adr-004-multiplayer-crash-game-specification-b93b4142286c

Status: Accepted (implementation constants pending)

## Decision

Implement a shared multiplayer Crash game with phases BETTING, RUNNING, CRASHED, SETTLED. The outcome is fixed before betting opens. Use exponential multiplier growth with [github.com/cockroachdb/apd/v3](<http://github.com/cockroachdb/apd/v3>) decimal arithmetic, pinned dependency version, explicit precision and rounding, golden vectors, checked conversions and integer simulated credits. Cash-out timing uses the durable admission time from ADR-002; equality with crash deadline loses. Payout = floor(betCredits \* scaledMultiplier / scale) using checked arithmetic.

## Fairness

Generate a cryptographically random 256-bit server seed; publish a SHA-256 commitment before betting; derive a versioned crash outcome from seed and round identity; reveal after settlement for independent verification. Commitment/reveal proves consistency with the committed seed, not impartial seed selection by itself.

## Lifecycle and settlement

Lifecycle commands are trusted-only. Crash is determined by the committed deadline even if processing is delayed. Settlement waits for the durable admission barrier and resolves all earlier eligible commands. Use balanced simulated-credit ledger postings; no real-money payments.

## Open implementation parameters

Freeze exponential coefficient, time quantum, multiplier scale, apd context, rounding, max duration, crash-point probability distribution, exact seed derivation and canonical encoding before first production rule-version release. Store authoritative numeric outputs in events; retain historical rule artifacts.

## Exclusions

No real-money flows, KYC, regulatory certification, jackpots, multi-currency or cross-region active-active.

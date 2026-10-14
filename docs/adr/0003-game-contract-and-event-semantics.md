# ADR-003: Game Contract & Event Semantics

Source: https://linear.app/lbtworks/document/adr-003-game-contract-and-event-semantics-0c918d047ea9

Status: Accepted

## Decision

Use a minimal typed Go Game\[S,C,E\] contract with InitialState(config), Decide(state, command, deterministicInput) -> events, and Evolve(state,event) -> nextState. Decide and Evolve are pure and deterministic: no I/O, uncontrolled randomness, wall-clock access, or mutation of input state. The runtime owns admission, sequencing, persistence, ownership and transport. Use a versioned type-erased adapter at the runtime boundary, not a general plugin framework.

## Replay

State replay: initial state + ordered events through Evolve. Decision verification: historical state + original command + recorded deterministic inputs through Decide, compare canonical events. Preserve rulesVersion, schemaVersion, stateVersion separately. The public projection must not expose secret crash outcomes.

## Crash v1 domain

Shared multiplayer round aggregate; commands include PlaceBet, StartRound, CashOut, CloseRound, SettleRound; lifecycle commands require system authority. Events include BetPlaced, RoundStarted, BetCashedOut, RoundCrashed, BetLost and RoundSettled. Invalid commands produce durable terminal command results without necessarily emitting domain events.

## Invariants

One bet per player per round; no double cash-out; no cash-out at/after crash; no transitions after settlement; deterministic canonical state hash; validated event applicability.

## Consequences

Historical game adapters and canonical serializers must remain available for audit replay. Avoid premature abstractions for hypothetical games.

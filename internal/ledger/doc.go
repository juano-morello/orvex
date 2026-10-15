// Package ledger holds the pure simulated-credit posting rules and the
// deterministic ledger IDs (ADR-004 balanced simulated-credit ledger;
// ADR-005 Transaction B; LBT-12,
// docs/specs/e0df34d3_execution-commit-ledger-outbox.md decisions E-8 and
// E-9).
//
// A Rule turns the decoded events of one executed chain record into at most
// one ledger transaction kind and its two-sided postings. Accounts are
// references (the player's account, the round's "round_pool" account, the
// global "house" account) that the executor resolves to account IDs.
// HouseAccountID, RoundAccountID and TransactionID derive RFC 9562 UUIDv8
// IDs from SHA-256, so the postings of a round are a pure function of its
// chain and a later audit stage can recompute them.
//
// The package does no I/O and reads no clock or randomness: it imports only
// the standard library's hashing and formatting packages and the game
// packages.
package ledger

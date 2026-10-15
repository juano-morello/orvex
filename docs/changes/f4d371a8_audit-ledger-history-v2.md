# LBT-18: audit ledger stage and history format v2

Spec: `docs/specs/ac81f88f_audit-ledger-stage.md` (implementation plan: `docs/specs/f4d371a8_audit-ledger-implementation.md`).

## What changed

Audit histories can now carry the simulated-credit ledger, and the verifier checks it.

- **History format v2** (`orvex/audit/history/v2`-style export, see `export_v2.go`): the v1 header and records 0..S verbatim, followed by one ledger line per record holding that sequence's ledger rows. `eventstore.ExportHistoryV2` writes it from PostgreSQL in the same transaction style as `ExportHistory`; `ledger.go` in `internal/persistence/eventstore` reads the ledger rows.
- **Ledger stage** in `internal/audit/verification/ledger.go`, wired through `verification.go`: re-derives the expected postings, transactions, holds and releases from the replayed events using the ledger rules (`ledger.CrashV1()`) and compares them with the ledger lines. Failures are reported as audit failures; `Report.Ledger` carries counts.
- **`crashctl audit`** reads v1 and v2. In verify mode with a v2 file it passes `LedgerRules` and prints `ledger ok accounts N transactions N postings N holds N releases N` before `ok`.
- **Golden v2 histories** in `internal/audit/verification/testdata/golden-v2/`: valid histories (`worked-example`, `empty-round`, `in-progress`, `instant-crash`), ~25 `tamper/*.jsonl` files, and `manifest.json`. They come from `generate.py`, an independent standard-library Python oracle; `make golden-check` now runs it with `--check`. Never regenerate from Go output.
- `pgtest/load.go` loads v2 histories for integration tests; `docs/architecture/persistence.md` and `README.md` are updated.

## Flagged points (spec section 2)

- **Verify-mode only.** The ledger checks run only in verify mode, not in the lighter modes.
- **"Unbalanced" means** a self-transfer, a zero amount or a negative amount (golden tamper cases `unbalanced-self-transfer`, `-zero-amount`, `-negative-amount`).
- **Posting rules and IDs freeze at first release.** Changing ledger posting rules or deterministic ledger IDs after release would break verification of existing histories.
- **Ledger lines are not hashed.** They are not part of the hash chain; the stage detects edits by recomputing from the events, not by hash.
- **Cross-round balances are out of reach.** A history covers one round, so per-account balances across rounds cannot be verified.

## Verify

- `make test` and `make lint`
- `make golden-check` (runs the v2 oracle)
- `make test-integration` (needs Docker): export v2 test (`export_integration_test.go`) and `internal/runtime/execution/ledger_audit_integration_test.go`
- `crashctl audit verify` on a `golden-v2` file: success output includes the `ledger ok` line; each `tamper/` file must fail.

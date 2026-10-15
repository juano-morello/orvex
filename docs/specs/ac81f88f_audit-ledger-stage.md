# Audit ledger stage and history format v2 (LBT-18)

Status: **APPROVED (2026-10-15).** The engineer approved L-1 to L-14 as written, with no edits, and
accepted every point of section 2. Implementation plan: `docs/specs/f4d371a8_audit-ledger-implementation.md`.

- Linear: [LBT-18](https://linear.app/lbtworks/issue/LBT-18/audit-ledger-stage-history-format-v2),
  milestone M2 · Durable Execution. Blocked by LBT-12; related to LBT-9.
- Linear text: "Add a verifier stage that recomputes ledger postings from the chained events (using
  `internal/ledger`, LBT-12) and compares them with the exported ledger rows. This needs history format v2
  so exports carry ledger rows, plus the oracle and golden changes `make golden-check` needs. … Postings
  stay outside the hash chain (no chain v2). Acceptance: the verifier recomputes every posting of a golden
  history and matches the exported rows; tampered ledger rows (amount, account, missing or extra posting,
  unbalanced transaction) fail verification; history v1 files still verify unchanged. Reference
  ADR-001/005/006."
- Governing ADRs: ADR-006 (primary: audit verifier "balanced ledger", fail closed on unknown artifacts),
  ADR-001 (ledger references, a bet settles at most once, artifact versioning), ADR-005 (double-entry
  simulated-credit ledger, balanced postings, cross-round balances), ADR-004 (balanced simulated-credit
  ledger), ADR-007 (layout, historical artifacts). Mirrors in `docs/adr/`.
- Approved inputs:
  - "LBT-9 spec": `docs/specs/8bebb70f_canonical-codec-audit-cli.md` (A-1 to A-20; §11 anticipated this
    stage), merged.
  - "LBT-10 spec": `docs/specs/17380a6e_postgresql-schema-migrations.md` (P-1 to P-26; ledger P-21, export
    P-24, loader §7.3), merged.
  - "LBT-11 spec": `docs/specs/5a3d52fc_admission-sequencer.md` (S-1 to S-14; hold policy S-10), merged.
  - "LBT-12 spec": the approved execution/ledger spec (E-1 to E-17; accounts E-8, deterministic IDs E-8,
    posting rules E-9, hold releases E-10, snapshots E-13). It is being implemented and is not on `main`.
    The copy read for this plan is outside the repository and is not copied in.
  - "Math spec" (D-1 to D-14) and "lifecycle spec" (LBT-8): no decision here touches them.

Decision IDs are `L-n`, so they do not clash with `D-n`, `A-n`, `P-n`, `S-n` or `E-n`. Oracle tamper
fixtures are `T1`–`T26`; Go byte mutations are `M1`–`M7`.

## 0. What this run delivers

- This document is the deliverable. The engineer approves its copy under `docs/specs/`.
- **Builder, this run: make no repository changes.** The planner already wrote the only change, this
  spec's copy under `docs/specs/`. Do not write Go code, Python, fixtures, SQL or migrations, and do not
  touch `go.mod`, `go.sum`, `Makefile`, CI, `README.md`, `AGENTS.md`, `docs/adr/` or
  `docs/architecture/`. Do not start section 11.
- The gates must still pass on the tree: `make test`, `make lint`, `go vet ./...`, `make build`,
  `make golden-check`.
- After approval, section 11 is the implementation plan. The literal values in section 5.5 and the counts
  in section 9 were computed for the proposed values. If the engineer edits a decision, the implementation
  run recomputes them with the oracle and updates this spec first.

## 1. Current state (verified while planning)

- `main` is at `8b26867`. Toolchain: go1.26.5, Python 3.14.6. `make golden-check` and `go vet ./...` pass.
  No git tag exists, so no crash/v1 release has been cut and A-20's freeze has not formally started. This
  spec still treats every v1 artifact as frozen (L-4).
- LBT-11 is merged (`migrations/0007_admission_window`, `internal/runtime/admission`). LBT-12 is **not**
  merged: no `internal/ledger`, no `internal/runtime/execution`, no migration `0008`.
- `internal/audit/verification/verification.go`:
  - `const Format = "orvex/audit-history/v1"`, `MaxLineBytes = 64 << 20`, `HeaderSequence = -1`;
  - `Mode` (`ModeChain`, `ModeReplay`, `ModeVerify`), `Options{Mode, Registry, ExpectHead}`, the seven
    `Stage` constants, `Failure`, `Checkpoint`, `Report`;
  - `read` runs the format stage: `parseHeader` checks `format` before the key set (an unknown format is
    unsupported at the header), then every later line goes through `chain.Decode`;
  - `replayHistory` runs rules, decode (with fairness) and `replay.Run`, and holds every decoded event
    (`replay.Record.Events []any`).
- `verification_test.go` byte mutation `B11-format-v2` rewrites the header format to
  `orvex/audit-history/v2` and expects exit 3, `format`, `header`.
- `internal/persistence/eventstore/export.go`: `ExportHistory` writes format v1 (P-24) in one
  `SnapshotReadTxOptions` transaction, ending at the newest snapshot `S`. It encodes the header itself
  (`encodeHeader`). `export_test.go` rebuilds each golden header byte for byte;
  `export_integration_test.go` `TestExportGoldenHistories` loads each v1 golden with `pgtest.LoadHistory`
  and requires a byte-identical export.
- `pgtest.LoadHistory` reads v1 only: every line after the header goes through `chain.Decode`.
- `cmd/crashctl/main.go` prints `format <rep.Format> chain <rep.Chain>` and builds
  `game.NewRegistry(crashv1.Adapter())` for replay and verify.
- `Makefile` `golden-check` runs two oracles (`crash/v1` math, audit histories). CI runs
  `make golden-check` and `make test-integration`.
- Ledger schema (LBT-10 §5.6, unchanged by LBT-12 except `0008`): `ledger_accounts(account_id, kind
  player|system, player_id, name, round_id, floor_enforced, debits_posted, credits_posted, held)`;
  `ledger_transactions(transaction_id, kind, round_id, sequence)`, unique per record;
  `ledger_postings(transaction_id, posting_index, debit_account_id, credit_account_id, amount > 0,
  round_id, event_sequence)` with distinct accounts; `ledger_holds(round_id, sequence, account_id,
  amount > 0)`; `ledger_hold_releases(round_id, sequence, account_id)`. LBT-12 `0008` adds the unique
  `ledger_postings_event_key` and the deferred `OX009` "every hold of an executed admission is released"
  check.
- LBT-12 facts this spec relies on (E-8, E-9, E-10, E-13, §7, §8):
  - `ledger.Rule{Descriptor(); Postings(events []any, first uint64) (kind string, postings []Posting,
    err error)}`; `Posting{Debit, Credit Account; Amount int64; EventSequence uint64; FromHold bool}`;
    `Account{Kind AccountPlayer|AccountRound|AccountHouse; Player string}`; `CrashV1()`,
    `HouseAccountID()`, `RoundAccountID(r)`, `TransactionID(r, k)`; names `house`, `round_pool`;
  - crash/v1 rule: `BetPlaced` player → round from the hold (`crash_bet`); `BetCashedOut` round → player
    (`crash_cash_out`); `RoundSettled` one net posting between round and house (`crash_settlement`), none
    when even; nothing for `RoundStarted`, `RoundCrashed`, `BetLost`; one ledger transaction per record
    with postings, ID `TransactionID(round, k)`, `posting_index` = position;
  - every hold of admission `k` is released in record `k`'s transaction, whatever the result; a
    `BetPlaced` consumes exactly its one hold, whose account is the event player's and whose amount is
    the bet;
  - snapshots at 0 and after records emitting `RoundStarted`, `RoundCrashed` or `RoundSettled`.
- Prototype (a throwaway Python script outside the repository, computing format v2 from the committed v1
  goldens under the proposed decisions):
  - reproduced LBT-12 §1's ID literals (house, worked-example round account, transactions 1, 12, 17, 21);
  - worked-example: 8 accounts, 6 transactions, 6 postings, 7 holds, 7 releases; round account net 0,
    house net −757,599,784, Σ nets 0 (LBT-12 §8);
  - instant-crash: 3 accounts, 2/2/1/1, round net 0, house net +500; empty-round: no ledger rows;
    in-progress: 2 accounts, 2/2/1/1, round net −2,320;
  - the exact bytes are in §5.5.

## 2. ADR alignment, conflicts and flagged interpretations

No decision contradicts an accepted ADR. These points depart from, extend or interpret approved decisions
and need the engineer's explicit attention (AGENTS.md "Flag conflicts explicitly"). Copy this section into
the implementation PR.

1. **LBT-9 A-20, immutability of history format v1 and its goldens (aligned; one test edit).** Format v1,
   the v1 audit oracle, its four histories, 26 tamper fixtures and manifest stay byte-identical; the v1
   reader path, `Report` and `crashctl` output for v1 files do not change; `ExportHistory` keeps writing
   v1. Format v2 is the new version A-20 prescribes ("`orvex/audit-history/v2`"). One approved **Go test
   case** changes: LBT-9 §8.5 `B11` rewrote the header to `orvex/audit-history/v2` to show an unknown
   format is unsupported. Once v2 is known that mutation yields a malformed v2 header (exit 1). L-4
   retargets `B11` to `orvex/audit-history/v3`, keeping its purpose and expectation (exit 3, `format`,
   `header`), and adds `M2` for the v2 relabelling. No fixture changes.
2. **LBT-9 §6, "In verify and replay modes every byte of the file is checked" (interpretation, needs
   approval).** For v2 files, ledger lines and the `accounts` table get their semantic checks only in
   `verify` mode (L-8). `replay` and `chain` check their syntax. In `verify`, two kinds of v2 bytes are
   carried but not checked (L-5): the player of an account that only unconsumed holds reference, and the
   presence and amount of holds that no posting consumes. Fixtures T25 and T26 pin this limitation, as
   LBT-9 R26 pinned the anchor limitation.
3. **LBT-9 §11, "LBT-12 (ledger)" (implemented, with interpretations).**
   - "a `ledger` stage after state replay": yes (L-8).
   - "format becomes `orvex/audit-history/v2` with a `ledger` section": the section is a header account
     table plus one ledger line per record (L-1, L-3).
   - "every posting references a chained `(roundId, eventSequence)`", "postings equal those derived from
     the chained events by a pure crash/v1 posting rule", "each bet settles at most once": checked
     (L-5, §6.5 B6–B9).
   - "each posting transaction balances": structural under P-21's two-sided postings (L-6, item 5).
   - "totals equal `RoundSettled`": no separate generic verifier check. It follows from recomputation
     (bet and payout postings carry the event amounts that state replay summed) and is self-checked by
     the oracle on every golden (L-5).
   - "If LBT-12 decides postings must be hash-committed, it introduces chain `orvex/audit/v2`": not done;
     postings stay outside the chain, as LBT-12 E-9 and the Linear issue decide.
4. **LBT-9 A-13, A-16, A-17 (additions).** A new stable stage name `ledger` after `state-replay`; a new
   success line `ledger ok …` in `crashctl audit verify` output for v2 files only; `Options.LedgerRules`;
   `internal/audit/verification` gains an import of `internal/ledger` (A-17's import table lists `chain`,
   `replay`, `canonjson`, `game`). `internal/ledger` imports `internal/game/crash/v1`, so the verifier
   links crash/v1 through it. `crashctl` already does.
5. **LBT-10 P-21, two-sided postings, versus "unbalanced transaction" in the acceptance (interpretation,
   needs approval).** A posting row moves one `amount` from one account to another, so every ledger
   transaction balances by construction and neither the database nor v2 (L-2) can hold an unbalanced
   transaction in the debit ≠ credit sense. L-6 plans "unbalanced transaction" as the only ways an
   exported posting can fail to move value consistently: a self-transfer (debit = credit), a zero amount
   and a negative amount (fixtures T5–T7). They mirror P-21's `ledger_postings_accounts_check` and
   `ledger_postings_amount_check`.
6. **LBT-10 P-24 and §7.3 (addition).** `ExportHistory` is unchanged. `ExportHistoryV2` is new and reads
   the same snapshot plus the round's ledger rows (L-9). `pgtest.LoadHistory` also loads v2 files (L-14);
   its v1 behaviour is unchanged.
7. **LBT-12 E-8, E-9 (addition, needs approval).** L-13 freezes, with the first release, the crash/v1
   posting rule, the `orvex/ledger/v1/…` ID derivations, the account names `house` and `round_pool`, and
   the transaction kinds `crash_bet`, `crash_cash_out`, `crash_settlement`. LBT-12 does not state their
   immutability. Without it, v2 verification of historical rounds would recompute with a changed rule.
8. **LBT-12 E-1 (alignment).** The verifier uses `internal/ledger`, as E-1's rationale intended ("that
   stage can import it without importing pgx or the executor"). `ledger.Rule` is not changed.
9. **LBT-11 S-10 (scope).** The verifier does not recompute which admissions carry a hold (S-10). The
   pure classifier lives in `internal/runtime/admission`, a pgx-linked runtime package that the audit
   side should not import. The oracle models S-10 to produce the golden holds. Follow-up: open item 2.
10. **LBT-12 approval status.** The LBT-12 copy read for this plan is headed "PROPOSED" with unticked
    boxes. This spec treats E-1 to E-17 as approved, as the request states. If approval changed E-8, E-9,
    E-10 or E-13, decisions L-5, L-7, L-10, L-11 and L-14 need review.
11. **ADR-001 limitation (unchanged and extended).** Ledger lines are not hashed. The ledger stage proves
    the exported ledger is consistent with the chain. It cannot prove the ledger file was not edited,
    only that any edit which changes a recomputable fact fails. Edits limited to unrecomputable facts
    (item 2) pass.
12. **ADR-005 "Open validation: … deferred ledger balance enforcement" (not addressed).** P-21 chose
    balance by construction. Cross-round balances (account totals, funding, holds of other rounds) are
    out of reach of a single-round file (L-5). A multi-round ledger audit is a follow-up (open item 6).
13. **ADR-007 (alignment).** No new Go package, no new module. A new test-data directory
    `internal/audit/verification/testdata/golden-v2/` with a third Python standard-library oracle, within
    the math spec D-14 approval for test oracles. `make golden-check` gains one line. `make audit` is
    still not added (LBT-9 §2 item 9 reasoning holds until LBT-19 can export rounds).
14. **AGENTS.md "Never regenerate vectors or fixtures from Go output".** All v2 files come from the v2
    oracle (L-10).

| ADR clause | Where satisfied |
|---|---|
| ADR-006 audit verifier: balanced ledger | L-5, L-6, §6.5 |
| ADR-006: fail closed on unknown historical artifacts | L-4 (format v3), L-7 (missing ledger rule) |
| ADR-006: golden replay vectors, fuzz | L-11, L-12 |
| ADR-001: preserve ledger references; a bet settles at most once | L-2, L-5 (B6) |
| ADR-001: artifact versioning, historical rules reproducible | L-4, L-13 |
| ADR-005: double-entry ledger, balanced postings | L-6 |
| ADR-005: cross-round balances | L-5 (out of reach, follow-up) |
| ADR-004: balanced simulated-credit ledger | L-5, L-6 |
| ADR-007: single module, no new package, historical rules selected by version | L-7, L-10 |

## 3. Decision summary

Tick a box to approve, or edit the value. The implementation run uses approved values only.

| ID | Decision | Proposed value | Approve |
|---|---|---|---|
| L-1 | History format v2 layout | header line; record lines exactly as v1; then one `"kind":"ledger"` line per record, sequences 0..N in order; lines dispatched by `kind` | [x] |
| L-2 | Ledger line contents and encoding | OCJ `{holds, kind, releases, sequence, transactions}`; postings two-sided with explicit `postingIndex`; every list strictly sorted; NULL event reference = `"0"`; no timestamps, totals or floor flags | [x] |
| L-3 | Account identity | header `accounts`: every account referenced in the file and only those, sorted; player `{accountId, kind, player}`, system `{accountId, kind, name, roundId}` (nil UUID = global) | [x] |
| L-4 | Versioning and v1 compatibility | header `format` = `orvex/audit-history/v2`, checked first; no separate ledger version; v1 path, v1 files, v1 oracle and `ExportHistory` unchanged; LBT-9 test `B11` retargeted to v3 | [x] |
| L-5 | Verified ledger facts | recomputed postings equal exported rows; balance; account identity; at most one posting per event; holds released and consumed; out of reach: cross-round balances, hold policy for unconsumed holds | [x] |
| L-6 | Balance semantics | two-sided postings: every exported posting has `amount ≥ 1` and debit ≠ credit; "unbalanced transaction" = self-transfer, zero or negative amount | [x] |
| L-7 | Posting rule in the Go verifier | `internal/ledger` reused via `Options.LedgerRules []ledger.Rule`, selected by (game, rules version) with full descriptor equality; IDs from `internal/ledger` | [x] |
| L-8 | Stage, modes, failures | stage `ledger` after replay, `verify` mode only; first failure wins; exit 1; missing ledger rule unsupported at `rules` 0 (exit 3); `ledger ok …` output line | [x] |
| L-9 | Export | new `eventstore.ExportHistoryV2`, same snapshot and end `S` as v1, faithful rows with sequence ≤ `S`, sorted in Go; `ExportHistory` stays v1 | [x] |
| L-10 | Oracle independence | new stdlib script `testdata/golden-v2/generate.py`; inputs only the four committed v1 goldens; recomputes holds, releases, postings and IDs from spec text; never reads Go or `internal/ledger`; v1 oracle untouched | [x] |
| L-11 | Golden v2 histories | four, one per v1 golden, same records, checkpoints and head plus ledger; fixture player account IDs `0190f3a8-7b3c-7d4e-8fa0-00000000000N` | [x] |
| L-12 | Ledger tamper catalogue | 26 oracle fixtures (T1–T26), 7 Go byte mutations (M1–M7), exhaustive single-byte flips of `instant-crash` v2, fuzz over `in-progress` v2 | [x] |
| L-13 | Immutability | v2 format, v2 oracle and fixtures, the ledger stage, and crash/v1's posting rule, ID tags, account names and transaction kinds freeze with the first release | [x] |
| L-14 | Test harness and end-to-end | `pgtest.LoadHistory` loads v2 ledger rows (with a test funding grant); an executor end-to-end test exports executed goldens as v2 and verifies them | [x] |

## 4. Decisions

### L-1. History format v2 layout

**Proposal.** "Orvex audit history v2", extension `.jsonl`, one round per file:
- Line 1 is the header (§5.2): the v1 keys with `format` = `orvex/audit-history/v2`, plus `accounts`.
- Lines 2..N+2 are records 0..N, each exactly the OCJ bytes that were hashed, as in v1 (A-11). Chain
  `orvex/audit/v1`, unchanged.
- Lines N+3..2N+3 are ledger lines 0..N (§5.3): ledger line `j` carries the ledger rows of record `j` and
  has `"kind":"ledger"` and `"sequence":"j"`. Every record has exactly one ledger line, the genesis
  included.
- A-11's rules carry over: every line ends with `\n`, no `\r`, no blank lines, at most 64 MiB per line,
  checkpoints non-empty, strictly increasing, last at N.
- The reader tells the sections apart by the `kind` member: a line whose `kind` is `"ledger"` starts the
  ledger section, and every later line must be a ledger line.

**Rationale.**
- The record lines are the v1 lines, so the chain, the A-9 `sha256sum` check (`sed -n 2p`), the
  checkpoints and the head are unchanged. A v2 file of a round has the same head as its v1 file.
- One ledger line per record mirrors ADR-005 Transaction B and LBT-12 E-6: one record commits its
  releases, at most one ledger transaction and its postings. A line holds at most a few rows, far from the
  line cap, however long the round.
- Dense lines (one per record, empty when nothing happened) make a missing line a format error rather
  than "no ledger effects", and give line `N+3+i` ↔ record `i`.
- Ledger lines are not hashed: postings stay outside the chain (Linear, LBT-12 E-9).

**Alternatives.**
- The whole ledger in one header object: holds are unbounded per round (every admitted player `PlaceBet`
  gets one, LBT-11 S-10; there is no admission cap), so one line could exceed 64 MiB.
- One line per database row with a row-kind discriminator: five line kinds, and the verifier regroups
  them per record.
- A ledger line after each record line: breaks "line i+2 is record i" and the A-9 tooling.
- A sidecar ledger file: two files to keep together and to version.
- Sparse ledger lines (only records with rows): about 60 bytes per record smaller, but an omitted line
  is indistinguishable from a record without ledger rows.
- Framing by the header's last checkpoint instead of `kind`: implicit, and a tampered checkpoint would
  surface as misframed lines instead of a chain-stage header failure.
- A chain v2 with a posting digest in each record: out of scope (Linear); LBT-9 §11 left it optional.

### L-2. Ledger line contents and canonical encoding

**Proposal.** §5.3 is normative. Every ledger line is an OCJ object (A-1 to A-3) with:
- `holds`: `[{accountId, amount}]`, the round's `ledger_holds` rows of admission `j`, strictly increasing
  `accountId`;
- `kind`: `"ledger"`;
- `releases`: `[accountId]`, the `ledger_hold_releases` rows of admission `j`, strictly increasing;
- `sequence`: `j`;
- `transactions`: `[{kind, postings, transactionId}]`, the `ledger_transactions` rows with
  `(round_id, sequence) = (R, j)`, strictly increasing `transactionId`;
- each posting: `{amount, creditAccountId, debitAccountId, eventSequence, postingIndex}`, strictly
  increasing `postingIndex`; `eventSequence` `"0"` stands for a NULL `event_sequence` (event sequences
  start at 1).

Not exported: `created_at` columns (wall-clock, not deterministic), account totals and `floor_enforced`
(cross-round, L-5), and `ledger_postings.round_id` (a posting belongs to its transaction's round; a NULL
`round_id` forces a NULL `event_sequence`, exported as `"0"`).

**Rationale.**
- Every value is a deterministic fact of the round. The same execution exports the same bytes, which
  lets tests compare an executed round's export with the oracle's golden byte for byte (L-14).
- Lists hold what the tables hold, without merging holds and releases or assuming at most one
  transaction. The exporter stays faithful, and the verifier, not the export, rejects a second
  transaction, an unreleased hold or a release without a hold.
- Strict sort orders keep A-2's "one value, one encoding".
- `postingIndex` is explicit because the table allows gaps; positional indices would hide them.

**Alternatives.**
- A single `transaction` member that is absent when there is none: OCJ has no null, and A-3 forbids
  omission.
- Holds with a `released` marker instead of a release list: the format could not show a release without
  a hold.
- Exporting `created_at`: makes exports non-reproducible.
- Leg-based postings: see L-6.

### L-3. Account identity: the header account table

**Proposal.** The header member `accounts` lists every account that a hold, release or posting in the
file references, and no other, sorted by `accountId`:
- player account: `{"accountId": UUID, "kind": "player", "player": <player ID>}`;
- system account: `{"accountId": UUID, "kind": "system", "name": <name>, "roundId": UUID}`, with the nil
  UUID `00000000-0000-0000-0000-000000000000` for a global account (`round_id` NULL; round IDs are never
  nil, LBT-10 P-14).

**Rationale.**
- One source per fact (A-5's principle): postings, holds and releases refer to accounts by ID, and an
  account's identity is stated once.
- Player account IDs are operator-assigned (LBT-11 S-11 never creates accounts), so the verifier can only
  check a posting against a player through an exported player ID.
- System account IDs are derivable (E-8). Exporting `name` and `roundId` lets the verifier detect a
  database row whose identity contradicts its ID.
- Only referenced accounts: the file shows nothing about players outside the round.

**Alternatives.**
- Identity inline in every posting leg and hold: repeated per line, and the verifier must cross-check the
  repetitions.
- IDs only: player identity cannot be checked.
- One line per account: a third line kind.
- Limitation of the proposal: about 110 bytes per account in one line, so the 64 MiB cap allows about
  600,000 distinct accounts per round. LBT-15 can measure; a later format could split the table.

### L-4. Versioning and v1 compatibility

**Proposal.**
- The header `format` is `orvex/audit-history/v2`. The reader still checks `format` first (LBT-9 §5.5):
  v1 takes the existing path, v2 the new one, anything else is unsupported at the header (exit 3).
- No separate ledger version field. Format v2 means ledger model `orvex/ledger/v1`: E-8's accounts and ID
  tags, E-9's rules selected by rules version, E-10's holds. A different ledger model needs a new format.
- v1 is untouched (§2 item 1): reader path, checks, `Report` (`Ledger` stays nil), `crashctl` output,
  failures, the v1 oracle and every v1 fixture, `ExportHistory`. `Options.LedgerRules` is ignored for v1.
- Old verifiers (before LBT-18) report v2 files as unsupported, exit 3, because they check `format` first.
  This is the fail-closed behaviour LBT-9 §11 predicted.
- LBT-9 test `B11` (an unknown format is unsupported) mutates to `orvex/audit-history/v3`; new `M2` covers
  a v1 file relabelled v2 (missing `accounts`: exit 1, `format`, `header`).

**Rationale.** A-20: a change that alters bytes is a new version, and the old one stays verifiable. A
ledger version field could only be a constant written by the exporter, since no table stores a per-round
ledger model; L-13 freezes the model instead.

**Alternatives.**
- An optional `ledger` member in v1: changes v1's meaning (A-20).
- A `ledger: "orvex/ledger/v1"` header field: no per-round source to fill it from.
- Upgrading v1 files to v2 inside the verifier: v1 files carry no ledger rows.

### L-5. Which ledger facts are verified

**Proposal.** In `verify` mode for a v2 file, after state and decision replay passed (§6.5 is normative):

| Fact | Check | Source |
|---|---|---|
| Recomputed postings equal exported rows | for every command record, `rule.Postings(events, first)` resolved to account IDs gives exactly the exported transactions: count (0 or 1), `transactionId = TransactionID(R, i)`, `kind`, postings in order with `postingIndex`, debit, credit, `amount`, `eventSequence` | E-9, E-8, P-21 "one transaction per record" |
| Balance | every exported posting moves `amount ≥ 1` between two different accounts (L-6) | P-21 |
| Account identity | system: `house` and `round_pool` have the derived IDs, scope and names, and those IDs belong to no other account; player: a consumed hold's account belongs to the event's player; a payout's credit account is the table's account of that player; player IDs unique | E-8, E-10, P-21 |
| At most one posting per event | every exported posting references an event of its own record, and no event twice in the file | ADR-001, E-11 |
| Holds released | in every command record, the released accounts equal the held accounts; nothing in the genesis line | E-10, `OX009` |
| Holds consumed | a `FromHold` posting needs exactly one hold in its record, on a player account of the event's player, of exactly the posting's amount | E-10 |
| Closed table | every account in the table is referenced, every referenced account is in the table | L-3 |

Out of reach of a single-round file, not checked:
- cross-round balances: account totals (`debits_posted`, `credits_posted`, `held`), funding grants,
  available credits, holds of other rounds (ADR-005 cross-round balances; follow-up open item 6);
- the S-10 hold policy: whether an admission that no posting consumes carried a hold, for which player
  and amount (§2 items 2 and 9; follow-up open item 2);
- when holds and releases were written (no timestamps, L-2).

LBT-9 §11's "totals equal `RoundSettled`" is not a separate check (§2 item 3).

**Rationale.** These are the facts ADR-006 ("balanced ledger") and ADR-001 ("a bet settles at most once",
"ledger references") ask about that one round's chain determines. The verifier stays generic over rules
versions: every crash-specific fact comes from the rule.

**Alternatives.**
- A crash-specific "round account ends at 0 after settlement" check: the generic verifier has no notion
  of settlement. Recomputation catches a wrong settlement amount, and the oracle self-checks the
  property on every golden.
- Verifying holds through `admission.CrashV1Policy().Classify`: imports a pgx-linked runtime package into
  the audit side (§2 item 9).

### L-6. Balance semantics

**Proposal.**
- Postings stay two-sided, as stored (P-21). For every exported posting the verifier requires
  `amount ≥ 1` and `debitAccountId ≠ creditAccountId`. Then each transaction balances: Σ debit legs =
  Σ credit legs = Σ amounts. This is structural and needs no further check.
- The acceptance item "unbalanced transaction" is tested with the degenerate postings a file can carry:
  T5 self-transfer, T6 zero amount, T7 negative amount (with debit and credit swapped). Each fails
  `ledger` at its record before recomputation, with a "posting does not balance" detail.

**Rationale.** P-21 made imbalance unrepresentable in the system of record. A format that could express
it would only add a failure mode that no real export can produce.

**Alternatives.**
- Legs (each posting exported as two signed entries, paired by `postingIndex`): imbalance becomes
  representable and checkable, but the file no longer maps 1:1 to rows and doubles posting bytes.
- Per-transaction debit and credit totals in the file: redundant fields that exist only to be checked.

### L-7. The posting rule in the Go verifier

**Proposal.**
- `verification.Options` gains `LedgerRules []ledger.Rule`: the posting rules `verify` uses for v2 files.
  `Verify` returns a plain error (not a `*Failure`) for a nil entry or two entries with the same
  `(Game, RulesVersion)`.
- In `verify` mode for a v2 file, the rules stage selects the rule whose `Descriptor()` has the genesis
  `game` and `rulesVersion`. None → unsupported at `rules` 0. A descriptor that differs from the
  adapter's in any field → unsupported at `rules` 0.
- Account and transaction IDs come from `ledger.HouseAccountID`, `ledger.RoundAccountID` and
  `ledger.TransactionID`; names from `ledger.HouseAccountName` and `ledger.RoundAccountName`.
- `crashctl` passes `[]ledger.Rule{ledger.CrashV1()}` next to `game.NewRegistry(crashv1.Adapter())`.

**Rationale.**
- ADR-007: the verifier selects the historical rules version. The posting rule is versioned with it
  (`Rule.Descriptor`).
- One Go implementation of the rule, shared by executor and verifier, as LBT-9 shares the codec between
  engine and verifier. Independence comes from the Python oracle (L-10) and the executor end-to-end test
  (L-14), as in A-18.

**Alternatives.**
- A second Go implementation inside `internal/audit`: two Go copies of one rule; same language, weak
  independence.
- A `Ledger()` method on `game.Adapter`: a contract change; LBT-12 E-1 kept the ledger out of the game
  packages.
- Hard-coding `ledger.CrashV1()` in `verification`: not generic, and no fail-closed lookup.
- A ledger registry type: one more type for a slice of one entry.

### L-8. Stage placement, modes, failures and exit codes

**Proposal.**
- Order: format → chain → rules (plus the ledger rule for v2 in `verify`) → decode (+fairness) → replay
  (decision, state, checkpoints per record) → **ledger**. First failure wins (A-13).
- The ledger stage runs only in `verify` mode and only for v2 files. `chain` and `replay` check the
  ledger lines and the account table syntactically in the format stage, and report nothing about the
  ledger.
- Ledger failures: stage `ledger`, at `header` (account table) or at sequence `i` (ledger line `i`),
  `Unsupported` false, exit 1. Format failures inside a ledger line: stage `format` at its `j`.
- Inside the stage, a fixed order: header checks, then records 0..N with a fixed per-record check order,
  then the closed-table check (at `header`). §6.5 gives it.
- `Report.Ledger` (`*LedgerReport`: accounts, transactions, postings, holds, releases) is set only when
  the stage ran. `crashctl audit verify` prints `ledger ok accounts A transactions T postings P holds H
  releases R` after the `state-replay ok` lines and before `ok`.

**Rationale.**
- Recomputation trusts the events. Running it after decision and state replay reports a forged event at
  its own record, not as a ledger symptom.
- `replay` stays ADR-001's state replay. `verify` is "all checks".
- No new exit code: a ledger mismatch is a history that fails verification.

**Alternatives.**
- Ledger before replay: reports ledger symptoms of a forged event instead of the forgery.
- Ledger interleaved per record with replay: needs only events, but would report a ledger failure at
  record 1 ahead of a decision forgery at record 12.
- Ledger also in `replay` mode: keeps LBT-9 §6's wording for `replay` (§2 item 2), at the cost of mixing
  ledger audit into state replay.
- A `crashctl audit ledger` subcommand, or a separate exit code: no need.

### L-9. Export: `eventstore.ExportHistoryV2`

**Proposal.** §8 is normative.
- `func ExportHistoryV2(ctx, db TxBeginner, roundID [16]byte, w io.Writer) (Export, error)`: one
  `SnapshotReadTxOptions` transaction, the same secret, snapshots, end `S`, head and verbatim records
  `0..S` as `ExportHistory`.
- It also reads the round's holds, releases, transactions and their postings with `sequence ≤ S`, and the
  referenced accounts. It sorts them in Go and writes the v2 header, the records, then ledger lines
  `0..S`.
- It never filters, repairs or recomputes rows. Rows it cannot encode are `ErrCorrupt`.
- `ExportHistory` keeps writing v1 with unchanged bytes.

**Rationale.**
- The verifier compares the system of record with the chain. The exporter must therefore copy, not
  derive. Computing postings at export would prove nothing (cf. LBT-9 §11 on state hashes).
- Additive: P-24's approved function and the LBT-10 golden round-trip test keep their meaning. LBT-19
  (`crashctl audit export`) chooses its default; this spec recommends v2.

**Alternatives.**
- Switch `ExportHistory` to v2: changes approved P-24 behaviour and LBT-10's `TestExportGoldenHistories`.
- `ExportHistoryFormat(ctx, db, roundID, format, w)`: equivalent; two named functions read more clearly
  with two formats.

### L-10. Oracle independence

**Proposal.** §9.1 is normative. A new Python 3.11+ standard-library script,
`internal/audit/verification/testdata/golden-v2/generate.py`:
- it never imports, runs or reads Go code, `go.mod`, `internal/ledger` or the v1 oracle script;
- its only inputs are the four committed v1 golden histories in `../golden/` (themselves outputs of the
  independent v1 oracle), read as bytes;
- from spec text it recomputes holds (LBT-11 S-10), releases (E-10), postings, transaction kinds and IDs
  (E-8, E-9), and the account table (L-3);
- it writes four v2 histories, 26 tamper fixtures and `manifest.json` (`--write`), or byte-compares them
  (`--check`). `make golden-check` runs it after the v1 audit oracle.

**How this catches Go bugs.**
- `ExportHistoryV2` must reproduce every v2 header and ledger line byte for byte from rows that the
  loader inserted from the oracle's files (encoder, sort orders, NULL encodings).
- The verifier must accept every golden and reject every tamper case: posting rule, ID derivation and
  account resolution.
- The executor end-to-end test (L-14) requires LBT-12's executor to write rows that export to the
  oracle's ledger lines: rule, kinds, IDs, holds and releases in Transaction B.

**Alternatives.**
- Extending the v1 oracle: edits the script A-20 freezes, with v1 bytes one mistake away.
- Importing the v1 script as a Python module: couples to its internals.
- Re-running the crash model in the v2 oracle: duplicates about 400 lines whose outputs already exist
  as the v1 files.
- Fixtures from Go output: forbidden (AGENTS.md).

### L-11. Golden v2 histories

**Proposal.** §9.2 is normative. Four histories, one per v1 golden, under `testdata/golden-v2/` with the
same names:
- the v1 header fields, record lines, checkpoints and head unchanged;
- `format` v2, the account table and one ledger line per record added;
- the ledger rows are those LBT-12's executor writes for the scenario: holds per S-10 at admission,
  released in their record, postings per E-9;
- player accounts get fixture IDs `0190f3a8-7b3c-7d4e-8fa0-` followed by `n` as 12 lowercase hex digits:
  alice 1, bob 2, `P3` (LBT-9 §5.6) 3, dave 4, erin 5, frank 6. The same player has the same account in
  every round, as in production.

**Rationale.** Reusing the v1 scenarios covers every event type, every posting kind, a house-to-round
settlement (worked-example), a round-to-house settlement (instant-crash), an even settlement with no
posting (empty-round), an unsettled round with a payout (in-progress), holds on rejected bets
(`duplicate_bet`, `bet_limit_reached`, `wrong_phase`, `round_settled`), admissions without a hold
(amount 0, empty player, cash-outs, lifecycle) and the escaped non-BMP player `P3`. Identical records make
"v2 adds rows and changes no chain byte" testable.

**Alternatives.**
- Goldens shaped like LBT-12 exports (snapshots at phase changes, the file ending at the newest snapshot):
  in-progress would lose its cash-out and worked-example its post-settlement hold, and the records would
  differ from v1's. L-14's end-to-end test compares executed exports with prefixes of these goldens
  instead.
- An extra scenario with an even settlement after bets: LBT-12's unit tests cover `B = W`; the verifier
  has no special case for it.

### L-12. Ledger tamper catalogue

**Proposal.** §9.4 and §9.5 are normative.
- 26 oracle-made fixtures (T1–T26, base worked-example v2) with expected outcomes in the v2 manifest:
  - amount: T1;
  - account: T2, T15, T16;
  - missing posting: T3;
  - extra posting: T4;
  - unbalanced: T5–T7;
  - events: T8, T9;
  - transaction: T10, T11;
  - holds and releases: T12–T14;
  - system accounts and the table: T17–T19;
  - genesis line: T20;
  - format: T21–T24;
  - documented limitations (exit 0): T25, T26.
- Seven Go byte mutations (M1–M7) applied in memory, plus the `B11` retarget.
- Every single-byte change (XOR 0x01 at each offset) of `instant-crash` v2 must not verify, and a fuzz
  target over `in-progress` v2. Both files have every account consumed by a posting, so the T25/T26
  limitation does not apply to them.

**Rationale.** A-19's split: cases that need canonical re-encoding come from the oracle, pure byte edits
from Go. Each acceptance category has at least one fixture, and T25/T26 make the limits explicit, as
LBT-9 R26 does.

### L-13. Immutability

**Proposal.** With the first crash/v1 release these freeze (ADR-001, ADR-007, A-20):
- history format v2: header, account table, ledger lines, line structure;
- the ledger stage's checks and their order;
- the v2 oracle and its fixtures;
- for crash/v1: `ledger.CrashV1()`'s outputs, the `orvex/ledger/v1/account:` and
  `orvex/ledger/v1/transaction:` derivations, the names `house` and `round_pool`, and the kinds
  `crash_bet`, `crash_cash_out`, `crash_settlement` (§2 item 7).

A change to any of them goes into a new history format, or for the rule a new rules version (or a
per-round ledger model version decided then). The old path stays verifiable. The v2 goldens are the
regression lock.

### L-14. Test harness and end-to-end test

**Proposal.**
- `pgtest.LoadHistory` accepts v1 and v2 files. For v2 it also inserts, in the same transaction:
  - the header's accounts (reusing an existing row with identical identity);
  - one funding grant per player account, from a global test system account `faucet`, of the player's
    total holds in the file;
  - the holds after the admissions;
  - after the chain records and domain events, per sequence: releases, transactions, postings.
  v1 loading is unchanged. Section 11, step 5 gives the order.
- `internal/runtime/execution/ledger_audit_integration_test.go` runs each golden through LBT-12's
  executor with the golden player account IDs, exports v2 and verifies it. The export's ledger lines must
  equal the golden v2 ledger lines up to the export's end `S` byte for byte.

**Rationale.** The loader round trip proves the exporter against oracle bytes. The end-to-end test closes
the loop executor → rows → export → verifier against the oracle.

**Alternatives.**
- A separate `pgtest.LoadLedger`: two calls per test and a split responsibility for one file.
- Executor-only tests: could not export rows the executor cannot produce (tamper-like states), and would
  depend on LBT-12's snapshot cadence for coverage.

## 5. Normative format: `orvex/audit-history/v2`

### 5.1 File layout

```
line 1               header (§5.2)
lines 2 .. N+2       chain records 0..N, byte-identical to v1 (chain orvex/audit/v1, LBT-9 §5.4)
lines N+3 .. 2N+3    ledger lines 0..N (§5.3)
```

Every line ends with `\n`, at most 64 MiB without it, OCJ (LBT-9 §5.1). "Position" of a ledger line is its
`sequence` `j`.

### 5.2 Header

| key | value |
|---|---|
| `accounts` | array of account objects, strictly increasing `accountId` (byte order of the UUID text, which equals byte order of the 16 bytes) |
| `checkpoints` | as v1 (LBT-9 §5.5) |
| `configSecret` | as v1 |
| `format` | `"orvex/audit-history/v2"` |
| `head` | as v1 |

Account objects, keys by `kind`:

| `kind` | keys | values |
|---|---|---|
| `player` | `accountId`, `kind`, `player` | UUID text (D-11); `"player"`; non-empty string (the `player_id`) |
| `system` | `accountId`, `kind`, `name`, `roundId` | UUID text; `"system"`; `^[a-z][a-z0-9_]{0,63}$`; UUID text, nil UUID for a global account |

### 5.3 Ledger lines

| key | value |
|---|---|
| `holds` | array of `{"accountId": UUID, "amount": int64}`, strictly increasing `accountId` |
| `kind` | `"ledger"` |
| `releases` | array of UUID strings, strictly increasing |
| `sequence` | uint64, equal to the line's position `j` |
| `transactions` | array of transaction objects, strictly increasing `transactionId` |

Transaction object: `kind` (`^[a-z][a-z0-9_]{0,63}$`), `postings` (array of posting objects, strictly
increasing `postingIndex`), `transactionId` (UUID).

Posting object: `amount` (int64), `creditAccountId` (UUID), `debitAccountId` (UUID), `eventSequence`
(uint64; `"0"` means no event reference), `postingIndex` (uint64 ≤ 2,147,483,647).

Integers use A-3's syntax. Amounts are syntactically any int64; the ledger stage rejects values below 1
(L-6).

### 5.4 Row mapping (exporter)

| v2 field | source (round `R`, sequence `j ≤ S`) |
|---|---|
| `accounts[]` | `ledger_accounts` rows referenced below: `account_id`, `kind`, `player_id` (player), `name` and `round_id` (system; NULL → nil UUID) |
| line `j` `holds` | `ledger_holds` with `(round_id, sequence) = (R, j)`: `account_id`, `amount` |
| line `j` `releases` | `ledger_hold_releases` with `(round_id, sequence) = (R, j)`: `account_id` |
| line `j` `transactions` | `ledger_transactions` with `(round_id, sequence) = (R, j)`: `transaction_id`, `kind` |
| `postings` | `ledger_postings` of that transaction: `posting_index`, `debit_account_id`, `credit_account_id`, `amount`, `event_sequence` (NULL → `0`) |

### 5.5 Literal cross-check values

Computed during planning with Python's standard library from the committed v1 goldens (§1 prototype).
The Go tests and the oracle's self-checks must reproduce them exactly. If either disagrees, stop and find
which side contradicts this spec. Never adjust a literal to match code.

`uuid8(s)` is LBT-12 §1's construction: the first 16 bytes of SHA-256 of UTF-8 `s`, byte 6 =
`(b6 & 0x0f) | 0x80`, byte 8 = `(b8 & 0x3f) | 0x80`.

| Value | Literal |
|---|---|
| house account | `4688699a-7bf4-8e07-8cec-c5f6bef86c36` |
| round account, rounds `…01` / `…02` / `…03` / `…04` | `13565418-c5ee-8f13-85c6-552f9e8755bd` / `aff26097-23f5-858c-90c3-a6a5925eab88` / `0bcd27f6-b696-839e-b6a5-dd7a1eb9e3cc` / `209fda50-25f4-857b-bbbe-df4491ff3cbe` |
| worked-example transactions 1, 2, 3 | `862c9c4b-3cb4-847f-80eb-d0364134723c`, `f6dcb55d-533f-8465-b3fd-ac2d043c0c10`, `7c84957e-9dc3-88c2-ba7f-ba3b6d2ed8b5` |
| worked-example transactions 12, 17, 21 | `5687be29-0e53-89bc-886e-babaeada505f`, `4357c225-96f0-803b-a757-26ca7f5055ac`, `d61426fa-cde4-8067-a6f6-163fb1aeb196` |
| instant-crash transactions 1, 5 | `a839c9f2-fad9-8876-b49f-e366cbb47340`, `0e9dca48-8900-8844-8924-75b55ff85b7e` |
| in-progress transactions 1, 3 | `5786a0a2-da6c-887e-a42d-9979ea3a9a33`, `caff4675-e4f6-8624-a11e-fe7ef6095573` |

Worked-example v2 header (one line, exact bytes; `P3` appears escaped as in LBT-9 §5.6):

```
{"accounts":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","kind":"player","player":"alice"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000002","kind":"player","player":"bob"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000003","kind":"player","player":"zoë \"☃\"\\\u0001😀"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000004","kind":"player","player":"dave"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000005","kind":"player","player":"erin"},{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000006","kind":"player","player":"frank"},{"accountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","kind":"system","name":"round_pool","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001"},{"accountId":"4688699a-7bf4-8e07-8cec-c5f6bef86c36","kind":"system","name":"house","roundId":"00000000-0000-0000-0000-000000000000"}],"checkpoints":[{"sequence":"0","stateHash":"1c98ea460213ad0d5900a92fbf37cc3078fa9e822d2065a65037b667734de7e7"},{"sequence":"9","stateHash":"bbf9bc3861a0bdcfccf201b185f4f08a9da2c8c61a53142a5caf13b1edd9f90f"},{"sequence":"22","stateHash":"ba0cff56c3e9473736d3ef45dfc2d9cd4d4cf0f31ea6f600af9ada84125bb02a"}],"configSecret":{"seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"},"format":"orvex/audit-history/v2","head":"7cf8cc4b1807099cdd1f16cbc0c790f1a877f30b696ebc0de751997d73ac3da7"}
```

Worked-example v2 ledger lines 0, 1, 4, 12, 21 and 22 (exact bytes):

```
{"holds":[],"kind":"ledger","releases":[],"sequence":"0","transactions":[]}
{"holds":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","amount":"100"}],"kind":"ledger","releases":["0190f3a8-7b3c-7d4e-8fa0-000000000001"],"sequence":"1","transactions":[{"kind":"crash_bet","postings":[{"amount":"100","creditAccountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","debitAccountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","eventSequence":"1","postingIndex":"0"}],"transactionId":"862c9c4b-3cb4-847f-80eb-d0364134723c"}]}
{"holds":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","amount":"50"}],"kind":"ledger","releases":["0190f3a8-7b3c-7d4e-8fa0-000000000001"],"sequence":"4","transactions":[]}
{"holds":[],"kind":"ledger","releases":[],"sequence":"12","transactions":[{"kind":"crash_cash_out","postings":[{"amount":"134","creditAccountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","debitAccountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","eventSequence":"5","postingIndex":"0"}],"transactionId":"5687be29-0e53-89bc-886e-babaeada505f"}]}
{"holds":[],"kind":"ledger","releases":[],"sequence":"21","transactions":[{"kind":"crash_settlement","postings":[{"amount":"757599784","creditAccountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","debitAccountId":"4688699a-7bf4-8e07-8cec-c5f6bef86c36","eventSequence":"9","postingIndex":"0"}],"transactionId":"d61426fa-cde4-8067-a6f6-163fb1aeb196"}]}
{"holds":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000006","amount":"10"}],"kind":"ledger","releases":["0190f3a8-7b3c-7d4e-8fa0-000000000006"],"sequence":"22","transactions":[]}
```

Instant-crash v2 ledger line 5 (a round-to-house settlement):

```
{"holds":[],"kind":"ledger","releases":[],"sequence":"5","transactions":[{"kind":"crash_settlement","postings":[{"amount":"500","creditAccountId":"4688699a-7bf4-8e07-8cec-c5f6bef86c36","debitAccountId":"aff26097-23f5-858c-90c3-a6a5925eab88","eventSequence":"5","postingIndex":"0"}],"transactionId":"0e9dca48-8900-8844-8924-75b55ff85b7e"}]}
```

In-progress v2 header (no house account; the round is unsettled):

```
{"accounts":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","kind":"player","player":"alice"},{"accountId":"209fda50-25f4-857b-bbbe-df4491ff3cbe","kind":"system","name":"round_pool","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000004"}],"checkpoints":[{"sequence":"0","stateHash":"9eafd3962055c7ef130563c7d46870628fc38a8b85e4070816540edec91dde09"},{"sequence":"3","stateHash":"559fa7527d33184bbc9fe6842a1c1c6fb26620e77f62f52101a06a2972c57e28"}],"configSecret":{"seed":"8d28ecf551864a1db9dccebdf0b4a358e13c61332beff3a0dd67f69760eb995e"},"format":"orvex/audit-history/v2","head":"f041d95d5bb917ce0f0d51d30c47738c1b97932bebd26ac7f117f98a279c46f6"}
```

Whole golden files under the proposed values (the oracle's `--write` output must match):

| File | Bytes | SHA-256 |
|---|---|---|
| `worked-example.jsonl` | 16,385 | `3e8583c90026409e1feec68cd05d67d062f0bf56ef4a4192058fe08ef4bf29ef` |
| `instant-crash.jsonl` | 4,941 | `f672a030fea0fbfd97001eb742af9c49ed2646464108ff3fd25f12af4ca5b5a8` |
| `empty-round.jsonl` | 2,781 | `a28074bf8f5211f16efcedac693a9b3b4f159435e105048b56b46509374b09a0` |
| `in-progress.jsonl` | 3,640 | `90a6b51d72ba42f6707461249b20bc44edbfc17aa1157738a490ad75bc627dee` |

## 6. Verification algorithm changes (normative)

LBT-9 §6 stays normative for v1 files and for every step not named here.

### 6.1 Format stage

1. Header: `format` first (v1 → the v1 path, unchanged; v2 → step 2; anything else → unsupported at
   `header`).
2. v2 header: exact key set `accounts, checkpoints, configSecret, format, head`; `checkpoints`,
   `configSecret`, `head` as v1; `accounts` per §5.2 (array, objects, `kind` read first, then the exact key
   set of that kind, field syntax, strictly increasing `accountId`). Any violation → `format` at `header`.
3. Lines after the header, in order:
   - strict `canonjson.Unmarshal`, else `format` at the line's position;
   - **record section** (until the first ledger line): a line whose `kind` is `"ledger"` ends the
     section; any other line → `chain.Decode` exactly as v1 (positions `0..`; `ErrUnsupportedChain` at 0
     → unsupported);
   - **ledger section**: line `j` must be a ledger line (`kind` = `"ledger"`), else `format` at `j`
     ("record line after the ledger section"); exact key set and field syntax per §5.3, else `format` at
     `j`; `sequence` = `j`, else `format` at `j`; `j` = number of records → `format` at `j` ("ledger line
     without a record").
4. End of input: v1's rules (empty input at `header`, no genesis at 0, unterminated or oversized lines at
   their position); fewer ledger lines than records → `format` at the number of ledger lines ("missing
   ledger line").

### 6.2 Chain stage

Unchanged; it covers the record section only.

### 6.3 Rules stage

Unchanged, plus, for a v2 file in `ModeVerify`: select the ledger rule per L-7; none, or a descriptor not
equal to the adapter's, → unsupported at `rules` 0.

### 6.4 Decode, fairness, replay

Unchanged.

### 6.5 Ledger stage (`ModeVerify`, v2 files, after replay succeeded)

Notation: round `R` (genesis `roundId`); table = header `accounts`; `n_i` = number of events of record
`i`; `f_i = 1 + n_1 + … + n_{i−1}` (`f_i` is the next event sequence when record `i` has no events);
`events_i` the decoded events of record `i`; `seen` an empty set of event sequences.

**A. Header** (failure at `header`), in order:
1. player strings are unique among player accounts;
2. `(name, roundId)` pairs are unique among system accounts;
3. a system account named `house` has ID `HouseAccountID()` and the nil `roundId`; one named
   `round_pool` has `roundId` = `R` and ID `RoundAccountID(R)`; an account whose ID is `HouseAccountID()`
   or `RoundAccountID(R)` is a system account with the matching name and `roundId`.

**B. Records**, for `i = 0..N` with ledger line `i` (failure at `i`), in order:
1. `i = 0`: `holds`, `releases` and `transactions` are empty. Continue with `i = 1`.
2. Every account ID in `holds`, `releases` and every posting's debit and credit is in the table.
3. Every hold has `amount ≥ 1`.
4. `releases` equals the list of hold account IDs (both sorted): every hold released, no release without
   a hold.
5. Balance (L-6): every exported posting has `amount ≥ 1` and debit ≠ credit.
6. Every exported posting's `eventSequence` lies in `[f_i, f_i + n_i − 1]` and is not in `seen`; add it.
7. `(kind, ps, err) = rule.Postings(events_i, f_i)` (also for rejected records, with no events, as the
   executor calls it). `err` → failure. The output must satisfy: `kind == ""` iff `ps` is empty; every
   `Amount ≥ 1`; every `EventSequence` in `[f_i, f_i + n_i − 1]` and distinct; `FromHold` only on a
   player debit; `Player` set iff the account kind is player; known account kinds. Else failure.
8. Resolve each posting's accounts:
   - `AccountRound` → `RoundAccountID(R)`;
   - `AccountHouse` → `HouseAccountID()`;
   - player debit with `FromHold` → line `i` has exactly one hold; its account is a player account in
     the table whose `player` is `Debit.Player`; its amount equals `Amount`; the debit is that account;
   - any other player account → the table's player account whose `player` is `Player` (none → failure).
   Then debit ≠ credit.
9. Compare with the exported `transactions`:
   - expected `[]` if `ps` is empty, else one transaction `{transactionId: TransactionID(R, i), kind,
     postings: [{postingIndex: j, debit, credit, amount, eventSequence}, …]}` in `ps` order;
   - equal element by element: count, `transactionId`, `kind`, posting count, then per posting
     `postingIndex`, `debitAccountId`, `creditAccountId`, `amount`, `eventSequence`;
   - the first difference → failure naming the field.

**C. Closed table** (failure at `header`): every table account is referenced by some hold, release or
posting of the file.

**D.** `Report.Ledger` = `{Accounts: len(table), Transactions, Postings, Holds, Releases}` counted over the
file.

Checks B2–B6 run on exported rows before recomputation, so degenerate rows get specific details. Any row
failing them would also fail B9. Tests assert stage and sequence only, never detail text.

### 6.6 Modes and report

| Mode | v1 file | v2 file |
|---|---|---|
| `chain` | unchanged | format (incl. ledger syntax), chain |
| `replay` | unchanged | + rules (adapter only), decode, fairness, state replay |
| `verify` | unchanged (no ledger stage, `LedgerRules` ignored) | + ledger rule, decision replay, ledger stage |

`Report.Format` is the file's format. `Report.Ledger` is nil unless the ledger stage ran.

## 7. `crashctl audit` changes

- Usage, flags, subcommands and exit codes are unchanged (LBT-9 §7).
- Replay and verify build `game.NewRegistry(crashv1.Adapter())` as today. Verify also passes
  `LedgerRules: []ledger.Rule{ledger.CrashV1()}`.
- v1 output is byte-identical to today. For v2 the `format` line reads `format orvex/audit-history/v2 chain
  orvex/audit/v1`. In `verify` mode, `ledger ok accounts A transactions T postings P holds H releases R`
  comes after the last `state-replay ok` line and before `ok`.
- Failures: `crashctl: audit verify: ledger failed at sequence 12: …` or `… at header: …`, exit 1. A missing
  ledger rule is `rules unsupported at sequence 0`, exit 3.

Worked-example v2, `crashctl audit verify`:

```
verifier crashctl <buildinfo.Version>
format orvex/audit-history/v2 chain orvex/audit/v1
round 0190f3a8-7b3c-7d4e-8f00-000000000001
rules crash 1 schema 1 state 1
chain ok records 23 head 7cf8cc4b1807099cdd1f16cbc0c790f1a877f30b696ebc0de751997d73ac3da7
fairness ok
decision-replay ok commands 22 accepted 8 rejected 14 events 9
state-replay ok sequence 0 state 1c98ea460213ad0d5900a92fbf37cc3078fa9e822d2065a65037b667734de7e7
state-replay ok sequence 9 state bbf9bc3861a0bdcfccf201b185f4f08a9da2c8c61a53142a5caf13b1edd9f90f
state-replay ok sequence 22 state ba0cff56c3e9473736d3ef45dfc2d9cd4d4cf0f31ea6f600af9ada84125bb02a
ledger ok accounts 8 transactions 6 postings 6 holds 7 releases 7
ok
```

## 8. Export: `eventstore.ExportHistoryV2` (normative)

```go
// ExportHistoryV2 writes round roundID as an "orvex/audit-history/v2" file:
// ExportHistory's header fields, records 0..S verbatim, then the round's
// ledger rows of sequences 0..S as one ledger line per record (LBT-18 L-9).
// On error, w may hold a partial file the caller must discard.
func ExportHistoryV2(ctx context.Context, db TxBeginner, roundID [16]byte, w io.Writer) (Export, error)
```

1. Steps 1 to 4 of LBT-10 §7.2 (snapshot transaction, secret, snapshots and `S`, head), unchanged.
2. In the same transaction (SQL illustrative; the row sets and the `≤ S` bound are normative):
   - `SELECT sequence, account_id, amount FROM orvex.ledger_holds WHERE round_id = $1 AND sequence <= $2`;
   - `SELECT sequence, account_id FROM orvex.ledger_hold_releases WHERE round_id = $1 AND sequence <= $2`;
   - `SELECT transaction_id, kind, sequence FROM orvex.ledger_transactions WHERE round_id = $1 AND
     sequence <= $2`;
   - `SELECT p.transaction_id, p.posting_index, p.debit_account_id, p.credit_account_id, p.amount,
     p.event_sequence FROM orvex.ledger_postings p JOIN orvex.ledger_transactions t USING
     (transaction_id) WHERE t.round_id = $1 AND t.sequence <= $2`;
   - the set of account IDs in those rows, then `SELECT account_id, kind, player_id, name, round_id FROM
     orvex.ledger_accounts WHERE account_id = ANY($1)`; the row count must equal the set size.
3. `ErrCorrupt` when a row cannot be encoded:
   - an account `kind` other than `player` or `system`;
   - a player row without a non-empty `player_id`, or a system row without `name`;
   - a negative `posting_index` or `event_sequence`;
   - a string `canonjson` rejects;
   - a missing account.
4. Sort in Go with `bytes.Compare` on the 16-byte IDs, never trusting SQL order: accounts by ID; per
   sequence, holds and releases by account, transactions by ID; postings by `posting_index`.
5. Write the v2 header (`canonjson.Marshal` of §5.2), then records `0..S` exactly as LBT-10 §7.2 step 6
   (same checks), then ledger lines `0..S`, each followed by `\n`. Flush. Return `Export` as v1 does.

`ExportHistory` keeps its code path and bytes. Shared helpers are allowed only if
`TestExportGoldenHistories` and `TestEncodeHeaderMatchesGolden` pass unchanged.

## 9. Oracle, golden v2 histories and tamper cases

### 9.1 Oracle rules (`internal/audit/verification/testdata/golden-v2/generate.py`)

1. Python 3.11+, standard library only (`argparse`, `hashlib`, `json`, `pathlib`, `sys`). It never
   imports, runs or reads Go code, `go.mod`, `internal/ledger` or the v1 oracle script.
2. Inputs: exactly `../golden/{worked-example,instant-crash,empty-round,in-progress}.jsonl`, resolved
   relative to the script.
3. Typed in from this spec, LBT-12 E-8/E-9/E-10 and LBT-11 S-10: `uuid8`, the two ID tags, `house`,
   `round_pool`, the three kinds, the posting rule, the hold rule (player-authority `PlaceBet` with a
   non-empty player and `1 ≤ betCredits ≤ 1,000,000,000` → a hold of `betCredits` on the player's account),
   release-always, player account IDs (§9.2), all v2 key names.
4. Encoder: the v1 oracle's OCJ encoder and validator, copied (not imported).
5. Assembly per file:
   - the v2 header: the input header's members, `format` v2, `accounts`;
   - the input's record lines copied byte for byte;
   - ledger lines `0..N`.
6. Self-checks (exit 2 on failure):
   - each input header's `format` is v1, and its record lines chain (every `prevHash` and `head`
     recomputed with `orvex/audit/v1/record:`);
   - the §5.5 ID literals, header lines and ledger lines;
   - the §9.2 counts;
   - per history: Σ account nets = 0; every hold released; at most one posting per event;
   - for settled histories: round account net 0; Σ `crash_bet` amounts = `RoundSettled.totalBetCredits`;
     Σ `crash_cash_out` amounts = `totalPayoutCredits`;
   - house net −757,599,784 (worked-example) and +500 (instant-crash);
   - worked-example holds at sequences 1, 2, 3, 4, 7, 11, 22 with amounts 100, 250, 1,000,000,000, 50,
     10, 10, 10 (LBT-12 §8).
7. `--write` writes the histories, `tamper/`, `manifest.json`; `--check` regenerates in memory,
   byte-compares, reports unexpected files under `tamper/`, exits 1 on a difference. JSONL lines end with
   `\n`; `manifest.json` is `json.dumps(indent=1, sort_keys=True) + "\n"`.
8. Never regenerate from Go output. Frozen with the first release (L-13). The docstring names this spec.

### 9.2 Golden v2 histories (`testdata/golden-v2/`)

| Name | Records | Accounts | Transactions | Postings | Holds | Releases |
|---|---|---|---|---|---|---|
| `worked-example` | 23 | 8 | 6 | 6 | 7 | 7 |
| `instant-crash` | 6 | 3 | 2 | 2 | 1 | 1 |
| `empty-round` | 4 | 0 | 0 | 0 | 0 | 0 |
| `in-progress` | 4 | 2 | 2 | 2 | 1 | 1 |

Command, accepted, rejected and event counts, heads and checkpoints equal the v1 manifest's. Player
accounts: alice `…8fa0-000000000001`, bob `…02`, `P3` `…03`, dave `…04`, erin `…05`, frank `…06`
(prefix `0190f3a8-7b3c-7d4e-8fa0-`).

### 9.3 `manifest.json`

As LBT-9 §8.3, with `"format": "orvex/audit-history/v2"`. Each history entry adds `base` (the v1 file
name), `accounts`, `transactions`, `postings`, `holds`, `releases`. Tamper entries have the v1 shape, with
`base` `"worked-example"`.

### 9.4 Oracle-made tamper fixtures (`golden-v2/tamper/<name>.jsonl`, base worked-example v2)

No case touches a record line, so no case rehashes. "Line `j`" is ledger line `j`. Ledger-stage cases
list `verify` and `replay`; format cases list `verify` and `chain`.

| # | name | mutation | expected |
|---|---|---|---|
| T1 | `posting-amount-edited` | line 12 posting `amount` 134 → 135 | verify: 1 ledger 12; replay: 0 |
| T2 | `posting-account-edited` | line 12 posting `creditAccountId` alice → bob (`…02`) | verify: 1 ledger 12; replay: 0 |
| T3 | `posting-missing` | line 17 `transactions` → `[]` | verify: 1 ledger 17; replay: 0 |
| T4 | `posting-extra` | line 21's transaction gains posting `{postingIndex 1, debit round account, credit house, amount 250, eventSequence 8}` | verify: 1 ledger 21; replay: 0 |
| T5 | `unbalanced-self-transfer` | line 1 posting `creditAccountId` → alice (= debit) | verify: 1 ledger 1; replay: 0 |
| T6 | `unbalanced-zero-amount` | line 2 posting `amount` → `0` | verify: 1 ledger 2; replay: 0 |
| T7 | `unbalanced-negative-amount` | line 3 posting debit and credit swapped, `amount` → `-1000000000` | verify: 1 ledger 3; replay: 0 |
| T8 | `event-posted-twice` | line 12 gains a second transaction: kind `crash_cash_out`, ID `uuid8("orvex/audit/v2/tamper/second-transaction")`, the same posting (event 5); transactions sorted | verify: 1 ledger 12; replay: 0 |
| T9 | `posting-event-edited` | line 17 posting `eventSequence` 6 → 5 | verify: 1 ledger 17; replay: 0 |
| T10 | `transaction-id-edited` | line 1 `transactionId` → transaction 2's (`f6dcb55d-…`) | verify: 1 ledger 1; replay: 0 |
| T11 | `transaction-kind-edited` | line 12 `kind` `crash_cash_out` → `crash_bet` | verify: 1 ledger 12; replay: 0 |
| T12 | `hold-unreleased` | line 4 `releases` → `[]` | verify: 1 ledger 4; replay: 0 |
| T13 | `hold-amount-edited` | line 1 hold `amount` 100 → 99 | verify: 1 ledger 1; replay: 0 |
| T14 | `release-without-hold` | line 5 `releases` → `[dave …04]` | verify: 1 ledger 5; replay: 0 |
| T15 | `player-account-renamed` | header: alice's account `player` → `mallory` | verify: 1 ledger 1; replay: 0 |
| T16 | `account-missing` | header: frank's account removed | verify: 1 ledger 22; replay: 0 |
| T17 | `house-account-renamed` | header: the house account `name` → `faucet` | verify: 1 ledger header; replay: 0 |
| T18 | `round-account-other-round` | header: the round account `roundId` → `…8f00-000000000009` | verify: 1 ledger header; replay: 0 |
| T19 | `unreferenced-account` | header gains `{accountId …8fa0-000000000007, kind player, player grace}` | verify: 1 ledger header; replay: 0 |
| T20 | `genesis-ledger-rows` | line 0 `holds` → `[{alice, 100}]`, `releases` → `[alice]` | verify: 1 ledger 0; replay: 0 |
| T21 | `ledger-line-missing` | line 22 deleted | verify: 1 format 22; chain: 1 format 22 |
| T22 | `ledger-lines-swapped` | lines 4 and 5 swapped | verify: 1 format 4; chain: 1 format 4 |
| T23 | `ledger-unknown-key` | line 1 gains `"note":"x"` | verify: 1 format 1; chain: 1 format 1 |
| T24 | `accounts-unsorted` | header: first two accounts swapped | verify: 1 format header; chain: 1 format header |
| T25 | `hold-only-account-renamed` | header: dave's account `player` → `dan` (dave only holds, at 7) | verify: **0** (limitation, L-5) |
| T26 | `unconsumed-hold-removed` | line 4 `holds` and `releases` → `[]` (alice stays referenced) | verify: **0** (limitation, L-5) |

### 9.5 Go byte mutations and the `B11` retarget

Applied in memory by Go tests (no encoder needed).

| # | mutation | expected (verify) |
|---|---|---|
| M1 | v2 worked-example: header `format` → `orvex/audit-history/v3` | 3 format header |
| M2 | v1 worked-example: header `format` → `orvex/audit-history/v2` | 1 format header |
| M3 | v2 worked-example: delete ledger line 0 | 1 format 0 |
| M4 | v2 worked-example: duplicate the last line | 1 format 23 |
| M5 | v2 worked-example: move ledger line 0 directly after the genesis record | 1 format 1 |
| M6 | v2 worked-example: drop the final `\n` | 1 format 22 |
| M7 | v2 worked-example: change one digit of header `head` | 1 chain 22 |
| B11 (LBT-9, retargeted) | v1 worked-example: header `format` → `orvex/audit-history/v3` | 3 format header (unchanged expectation) |

## 10. LBT-18 acceptance as tests

| Acceptance | Tests (section 11 names the files) |
|---|---|
| The verifier recomputes every posting of a golden history and matches the exported rows | `TestGoldenHistoriesV2` (all four v2 goldens pass verify with `Report.Ledger` equal to the manifest); `TestExportV2GoldenHistories` (rows loaded into PostgreSQL export byte-identical and verify); `TestExecutedGoldensExportVerifiedLedger` (rows written by LBT-12's executor export to the golden ledger lines and verify); `TestAuditGoldenV2` (CLI) |
| Tampered ledger rows fail: amount | T1, T13; `TestTamperFixturesV2`, `TestAuditFailuresV2` |
| … account | T2, T15, T16, T17, T18 |
| … missing posting | T3 |
| … extra posting | T4, T8 |
| … unbalanced transaction | T5, T6, T7 (L-6) |
| Holds released and consumed | T12, T13, T14, T20 |
| At most one posting per event | T8, T9 |
| History v1 files still verify unchanged | the existing v1 tests unchanged (`TestGoldenHistories`, `TestTamperFixtures`, `TestByteMutations` with `B11` retargeted, `TestAuditGolden` exact v1 output, `TestExportGoldenHistories`); `TestV1UnaffectedByLedgerRules`; `TestExportV1UnchangedWithLedgerRows`; v1 golden files byte-unchanged (`make golden-check`, `git diff`) |
| Fail closed (ADR-006) | M1 (format v3), `TestLedgerRuleRequired`, `TestLedgerRuleDescriptorMismatch` (exit 3) |
| Every v2 byte is covered (except L-5's limits) | `TestEverySingleByteFlipFailsV2`, `FuzzSingleByteFlipV2`; T25, T26 pin the limits |

## 11. Implementation plan (after approval)

**Precondition: LBT-12 is merged on `main` and the worktree is based on it.** Check that
`internal/ledger` (with `Rule`, `Posting`, `Account`, `AccountPlayer/AccountRound/AccountHouse`,
`CrashV1`, `HouseAccountID`, `RoundAccountID`, `TransactionID`, `HouseAccountName`, `RoundAccountName`),
`internal/runtime/execution` and migration `0008_execution_ledger` exist. If LBT-12 is not merged, stop
and report "blocked on LBT-12". If merged names differ, use the merged names: this spec fixes behaviour,
not identifiers. If merged behaviour differs from §1's LBT-12 facts (IDs, tags, names, kinds, rule
outputs, release-always, snapshot cadence), stop and report.

Rules for new or edited Go code:
- goimports grouping with the `github.com/juano-morello/orvex` local prefix;
- error strings lowercase without trailing punctuation, prefixed `verification: ` or `eventstore: `;
- tests assert stages, sequences, exit codes, sentinels and bytes, never detail text.

No `go.mod` change and no migration.

### Files

| File | Action |
|---|---|
| `internal/audit/verification/testdata/golden-v2/generate.py` | new (oracle, §9.1) |
| `internal/audit/verification/testdata/golden-v2/{worked-example,instant-crash,empty-round,in-progress}.jsonl`, `tamper/*.jsonl` (26), `manifest.json` | new, written by `generate.py --write` only |
| `internal/audit/verification/verification.go` | edit: package doc; `FormatV2`, `StageLedger`, `Options.LedgerRules`, `LedgerReport`, `Report.Ledger`; v2 header and section dispatch in `read`; ledger-rule selection in the rules stage; call the ledger stage |
| `internal/audit/verification/ledger.go` | new: account table and ledger line parsing (format stage), ledger stage (§6.5) |
| `internal/audit/verification/verification_test.go` | edit: `B11` mutates to `orvex/audit-history/v3`; nothing else |
| `internal/audit/verification/ledger_test.go` | new |
| `internal/audit/verification/fuzz_test.go` | edit: add `FuzzSingleByteFlipV2` |
| `internal/persistence/eventstore/export.go` | edit: `ExportHistoryV2`; package doc |
| `internal/persistence/eventstore/ledger.go` | new: ledger row reads, account resolution, sorting, `encodeHeaderV2`, `encodeLedgerLine` |
| `internal/persistence/eventstore/ledger_test.go` | new (unit, `package eventstore`) |
| `internal/persistence/eventstore/export_integration_test.go` | edit: add the v2 tests |
| `internal/persistence/postgres/pgtest/load.go` | edit: v2 loading (L-14) |
| `internal/runtime/execution/ledger_audit_integration_test.go` | new (integration, `package execution_test`) |
| `cmd/crashctl/main.go` | edit: `LedgerRules` wiring, `ledger ok` report line, doc comment |
| `cmd/crashctl/main_test.go` | edit: add the v2 tests |
| `Makefile` | edit: `AUDIT_GOLDEN_V2_GEN := internal/audit/verification/testdata/golden-v2/generate.py`; a third `golden-check` line after the v1 audit line; the comment names three oracles |
| `README.md` | edit: Status adds LBT-18; Layout: `internal/audit/verification` "history reader (formats v1 and v2), staged verifier with the ledger stage, golden histories", `internal/persistence/eventstore` "audit history export (v1 LBT-10, v2 with ledger rows LBT-18)" |
| `docs/architecture/persistence.md` | edit: a section "Audit export v2 (LBT-18)" after LBT-12's section: what `ExportHistoryV2` reads (§5.4, §8), what it omits (L-2) and the `S` bound |

Unchanged: `internal/audit/verification/testdata/golden/**` (v1 oracle, histories, tamper, manifest),
`internal/audit/{chain,replay}`, `internal/canonjson`, `internal/game/**`, `internal/ledger`, non-test
code of `internal/runtime/**`, `migrations/`, `go.mod`, `go.sum`, `.github/`, `docs/adr/`, `AGENTS.md`
(its `make golden-check` line stays accurate).

### Step 1: v2 oracle and fixtures

Write `generate.py` per §9.1–§9.4. Run `python3 generate.py --write` and commit its outputs. Its
self-checks must pass, the four files must match §5.5's sizes and hashes, and `--check` must exit 0. If a
§5.5 value disagrees, find which side contradicts this spec.

### Step 2: `internal/audit/verification`

Implement §6 and L-7, L-8. API (names may be refined, behaviour may not):

```go
const FormatV2 = "orvex/audit-history/v2"
const StageLedger Stage = "ledger"

type Options struct {
	Mode       Mode
	Registry   *game.Registry
	// LedgerRules holds the posting rules ModeVerify uses for v2 histories,
	// at most one per (game, rules version). v1 histories ignore it.
	LedgerRules []ledger.Rule
	ExpectHead  *chain.Hash
}

// LedgerReport counts the ledger rows of a v2 history that passed the ledger stage.
type LedgerReport struct{ Accounts, Transactions, Postings, Holds, Releases int }

// Report gains:
//	Ledger *LedgerReport // nil unless the ledger stage ran
```

The v1 path must be the existing code path; v2 additions branch on the parsed format.

Tests, `ledger_test.go` (`package verification_test`; `loadManifestV2` mirrors `loadManifest` with
`DisallowUnknownFields`, format v2, 4 histories, 26 tamper cases):
- `TestGoldenHistoriesV2`: per v2 history, verify, replay and chain pass. `Report` equals the manifest:
  counts, head, checkpoints, `Format` v2. In verify `Ledger` equals the manifest's ledger counts; in
  replay and chain `Ledger` is nil.
- `TestV2RecordsEqualV1`: each v2 file's record lines equal its base v1 file's record lines byte for
  byte. Its header without `accounts` and `format` equals the v1 header's other members.
- `TestV2Literals`: the §5.5 header lines and ledger lines, typed in, equal the golden lines byte for byte.
- `TestTamperFixturesV2`: every v2 manifest check gives exactly its stage, sequence and `Unsupported`;
  exit-0 checks succeed (T25, T26).
- `TestByteMutationsV2`: M1–M7.
- `TestEverySingleByteFlipFailsV2`: every XOR 0x01 of `golden-v2/instant-crash.jsonl` in verify mode
  never succeeds (skipped under `-short`). If one succeeds, stop and report a format hole; do not weaken
  the test.
- `TestLedgerRuleRequired`: v2 verify without `LedgerRules` → unsupported `rules` 0. v2 replay and chain
  without it pass. v1 verify without it passes.
- `TestLedgerRuleDescriptorMismatch`: a test rule wrapping `ledger.CrashV1()` whose `Descriptor()`
  reports state version 2 → unsupported `rules` 0.
- `TestLedgerRulesInvalidOptions`: a nil rule, or two rules for crash 1, → a non-`*Failure` error before
  any read.
- `TestLedgerRuleErrorIsLedgerFailure`, over the worked-example v2 golden: a test rule wrapping
  `ledger.CrashV1()` that returns an error when called with `first == 1` and one event (record 1) →
  `ledger` at 1. A test rule that returns record 1's posting with amount 0 → `ledger` at 1.
- `TestV1UnaffectedByLedgerRules`: each v1 golden in each mode gives an identical `Report` with and
  without `LedgerRules`, and `Ledger` is nil.

`verification_test.go`: `B11` mutates to `orvex/audit-history/v3` and is renamed `B11-format-v3`. Its
expectation stays 3, `format`, `header`.

`fuzz_test.go`: `FuzzSingleByteFlipV2(offset, xor)` over `golden-v2/in-progress.jsonl`: any change with
`xor != 0` never verifies.

### Step 3: `cmd/crashctl`

Implement §7.

Tests (`main_test.go`):
- `TestAuditGoldenV2`: every v2 golden under the three subcommands exits 0 with last line `ok`. The
  worked-example verify output equals §7's text exactly, with the manifest's head and checkpoints.
  replay and chain print no `ledger` line.
- `TestAuditFailuresV2`: every v2 manifest check through the CLI: the exit code, nothing on stdout, and
  stderr starting with `crashctl: audit <sub>: <stage> failed at <sequence N|header>: ` (`unsupported`
  for exit 3).
- `TestAuditGolden`, `TestAuditFailures`, `TestAuditReplayStateHash`, `TestAuditUsage`, `TestAuditIO`,
  `TestRun`: unchanged. `TestAuditGolden` pins the exact v1 output.

### Step 4: `eventstore.ExportHistoryV2` (code and unit tests)

Implement §8 in `export.go` and `ledger.go`.

Unit tests (`ledger_test.go`, `package eventstore`):
- `TestEncodeLedgerMatchesGoldenV2`: per v2 golden, parse the header `accounts` and every ledger line into
  the exporter's row structs, deliberately unsorted, then encode with `encodeHeaderV2` and
  `encodeLedgerLine`. The header and every ledger line equal the golden bytes.
- `TestEncodeLedgerNullEventSequence`: a posting with a NULL `event_sequence` encodes `"eventSequence":"0"`.
- `TestEncodeLedgerRejectsCorrupt`: each gives `ErrCorrupt`:
  - account kind `bank`;
  - a player row with NULL or empty `player_id`;
  - a system row with NULL `name`;
  - `posting_index` −1;
  - `event_sequence` −1;
  - a player ID with invalid UTF-8.
- `TestEncodeHeaderMatchesGolden`, `TestEncodeHeaderRejectsBadSecret`: unchanged.

### Step 5: `pgtest.LoadHistory` v2 and export integration tests

`LoadHistory` reads the header `format`. v1: exactly today's behaviour. v2, in the same READ COMMITTED
transaction:
1. split lines into records and ledger lines by `kind`;
2. after the heads: insert each header account, or require an existing row with the same `kind`,
   `player_id`, `name` and `round_id` (`t.Fatal` otherwise). Players get `floor_enforced = true`, system
   accounts `false`;
3. find or create the global system account `faucet` (`gen_random_uuid()`). For each player account with
   holds in the file, insert one ledger transaction (`kind 'grant'`, round NULL, `gen_random_uuid()`)
   with one posting faucet → player of the player's total hold amount;
4. after the admissions: every hold (`round_id`, `sequence`, `account_id`, `amount`);
5. after chain records and domain events, for each ledger line in sequence order:
   - its releases;
   - its transactions (`round_id`, `sequence`);
   - its postings: `round_id` R, `event_sequence` NULL for `"0"`;
6. snapshots and commit as today.

Update the doc comment ("an LBT-9 history file, format v1 or v2").

Integration tests (`export_integration_test.go`, each on its own `pgtest.MigratedDatabase`):
- `TestExportV2GoldenHistories`: per v2 golden: `LoadHistory`, then `ExportHistoryV2`. The output equals
  the golden file byte for byte, and `verification.Verify(ModeVerify, crash/v1 registry,
  LedgerRules{CrashV1})` passes with the manifest's ledger counts. `Export` holds the round ID, the
  record count and the head.
- `TestExportV1UnchangedWithLedgerRows`: after loading a v2 golden, `ExportHistory` equals the base v1
  golden byte for byte.
- `TestExportV2StopsAtLatestSnapshot`: load the v2 worked-example with checkpoints `{0, 9}`. The export
  has records 0..9 and ledger lines equal to the golden's lines 0..9. Its accounts are alice, bob, `P3`,
  dave and the round account (no erin, frank or house). It verifies with 5 accounts, 3 transactions, 3
  postings, 5 holds, 5 releases.
- `TestExportV2SharedAccounts`: load the worked-example and instant-crash v2 goldens into one database
  (they share alice's and the house account). Each round's export equals its golden: no rows of the
  other round, no unreferenced account.
- `TestExportV2WithoutLedgerRowsFailsVerification`: load the **v1** worked-example. `ExportHistoryV2` has
  `accounts` `[]` and 23 empty ledger lines. Verify fails `ledger` at 1 (the bet's `FromHold` posting has
  no hold).
- `TestExportGoldenHistories`, `TestExportStopsAtLatestSnapshot`, `TestExportRoundNotFound`,
  `TestExportNoCheckpoint`: unchanged. Add `ExportHistoryV2` cases to the round-not-found and
  no-checkpoint tests (same sentinels).

### Step 6: executor end-to-end test

`internal/runtime/execution/ledger_audit_integration_test.go` (`//go:build integration`, `package
execution_test`, uses LBT-12's `TestMain` and helpers):

- `TestExecutedGoldensExportVerifiedLedger`, per golden:
  1. fresh database;
  2. insert each player account of the v2 golden header with its golden ID;
  3. `seedGolden` (LBT-12's helper: `CreateRound` from the golden genesis, fund players with their total
     holds, seed every admission with its S-10 hold);
  4. `Drain` with limit 100, `EngineBuild "golden"`;
  5. `ExportHistoryV2`, then verify (`ModeVerify` with `LedgerRules`).

  Let `S` be the export's last checkpoint (worked-example 21, instant-crash 5, empty-round 3,
  in-progress 2, per E-13). Then:
  - the record lines equal the golden lines 0..S;
  - the ledger lines equal the golden v2 ledger lines 0..S byte for byte;
  - `accounts` equals the golden table restricted to IDs referenced in lines 0..S;
  - `Report.Ledger` is worked-example 7/6/6/6/6, instant-crash 3/2/2/1/1, empty-round 0/0/0/0/0,
    in-progress 2/1/1/1/1 (accounts/transactions/postings/holds/releases).

  If the merged `seedGolden` or `fundPlayer` cannot reuse pre-created player accounts (it inserts
  unconditionally or by another key), write local helpers in this file that do the same with the golden
  IDs. Change no LBT-12 file.

### Step 7: Makefile and docs

Make the files-table edits. CI needs no change: it already runs `make golden-check` and
`make test-integration`.

### Verification for the implementation run

Each must exit 0:

```sh
make test
make test GOTESTFLAGS=
make lint
go vet ./...
go vet -tags integration ./...
make build
make golden-check
python3 internal/audit/verification/testdata/golden-v2/generate.py --check
make test-integration
./bin/crashctl audit verify internal/audit/verification/testdata/golden-v2/worked-example.jsonl
./bin/crashctl audit verify internal/audit/verification/testdata/golden/worked-example.jsonl
./bin/crashctl audit replay internal/audit/verification/testdata/golden-v2/tamper/posting-amount-edited.jsonl
git diff --exit-code main -- internal/audit/verification/testdata/golden internal/audit/chain internal/audit/replay internal/canonjson internal/game internal/ledger migrations go.mod go.sum docs/adr
```

- The first `crashctl` command prints `ledger ok accounts 8 transactions 6 postings 6 holds 7 releases 7`.
- The second prints exactly what it printed before this change (no `ledger` line).
- `./bin/crashctl audit verify internal/audit/verification/testdata/golden-v2/tamper/posting-amount-edited.jsonl`
  must exit 1.
- `./bin/crashctl audit verify` of a copy of the v2 worked-example with `format` set to
  `orvex/audit-history/v3` must exit 3.

If Go and the oracle disagree, find which side contradicts this spec and fix that side. Never regenerate
fixtures from Go output, never edit a §5.5 literal to match code, and never weaken a golden comparison.

## 12. Out of scope and open items

Out of scope:
- in this run: any Go code, Python, fixtures, SQL, migrations, `go.mod`, Makefile, CI or docs change
  other than this spec;
- a chain v2 (postings stay outside the hash chain);
- `crashctl audit export` (LBT-19);
- cross-round ledger auditing: account totals, funding, holds across rounds;
- external anchoring of heads or ledger files (ADR-001 future measure).

Open for the engineer:
1. "Unbalanced transaction" interpreted as degenerate two-sided postings (§2 item 5, L-6).
2. The hold policy (S-10) is not verified for holds no posting consumes (§2 items 2 and 9, T25, T26).
   Follow-up: extract S-10's pure hold rule into `internal/ledger` (or another pure package) used by
   both admission and the verifier, then verify every hold.
3. Ledger stage in `verify` mode only (§2 item 2, L-8).
4. Freezing crash/v1's posting rule, ID tags, names and kinds with the first release (§2 item 7, L-13).
5. `ExportHistory` stays v1; LBT-19 picks the export default (this spec recommends v2).
6. Follow-up issue: multi-round ledger audit that reconciles `ledger_accounts` totals with Σ postings and
   unreleased holds across rounds and funding grants (ADR-005 cross-round balances).
7. The header account table's 64 MiB line cap, about 600,000 distinct accounts per round (L-3).
8. LBT-9 test `B11` retargeted from v2 to v3 (§2 item 1).

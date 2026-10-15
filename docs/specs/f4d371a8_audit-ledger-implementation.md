# LBT-18 implementation plan: audit ledger stage and history format v2

- Linear: [LBT-18](https://linear.app/lbtworks/issue/LBT-18/audit-ledger-stage-history-format-v2), milestone M2.
- Normative design: the approved spec, added in step 0 as `docs/specs/ac81f88f_audit-ledger-stage.md`
  (decisions L-1 to L-14, approved as written with no edits; every point of its section 2 accepted).
  Source copy: `/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/f4d371a8/context_handoff/lbt-18-approved-spec.md`
  (byte-identical to `/home/juano/Workspaces/LBTWorks/Orvex/requests/lbt-18-approved-spec.md`, sha256
  `0a9368585c969906937ca962f0ded7c380af50d8eaa6749f08cd5e8bf2e06337`, 1234 lines).
- This plan is spec §11 applied to the current `main` (`e74bc19`). Where the plan and the spec disagree on
  **behaviour**, the spec wins: stop and report. Where this plan fixes a name, a file placement or an
  ambiguity (section 2), follow the plan.
- Governing ADRs: ADR-006 (primary), ADR-001, ADR-005, ADR-004, ADR-007. **No ADR conflict.** The spec's
  section 2 lists the approved interpretations (B11 retarget, ledger stage in `verify` only, "unbalanced"
  = degenerate two-sided postings, S-10 hold policy not verified for unconsumed holds, L-13 freeze). Copy
  spec section 2 into the PR description and the report.

## 1. Preconditions, verified on `main` while planning

Spec §11's precondition holds: LBT-12 is merged (PR #9, `e74bc19`).

| Spec §1 / §11 fact | On `main` |
|---|---|
| `internal/ledger`: `Rule{Descriptor(); Postings(events []any, first uint64) (string, []Posting, error)}`, `Posting{Debit, Credit Account; Amount int64; EventSequence uint64; FromHold bool}`, `Account{Kind; Player}`, `AccountPlayer/AccountRound/AccountHouse`, `Account.Valid()`, `CrashV1()`, `HouseAccountID()`, `RoundAccountID(r)`, `TransactionID(r, k)`, `HouseAccountName = "house"`, `RoundAccountName = "round_pool"`, `KindCrashBet/KindCrashCashOut/KindCrashSettlement`, `ErrInvalidEvents` | present, same names (`internal/ledger/{ledger,ids,crashv1}.go`) |
| ID literals (house `4688699a-…`, worked round account `13565418-…`, transactions 1/12/17/21) | pinned identically in `internal/ledger/ids_test.go` |
| crash/v1 rule outputs (bet from hold, cash-out, one net settlement, none for start/crash/lost) | `internal/ledger/crashv1.go` matches |
| release-always, exactly one hold per `FromHold` bet | `internal/runtime/execution/transition.go` (`consumeHold`, releases = all hold accounts) |
| snapshot cadence → export end `S`: worked-example 21, instant-crash 5, empty-round 3, in-progress 2 | `execute_integration_test.go` `goldenExpectations.snapshots` = `{0,9,19,21}`, `{0,2,4,5}`, `{0,1,2,3}`, `{0,2}` |
| S-10 hold rule (player-authority `PlaceBet`, non-empty player, `1 ≤ betCredits ≤ 1e9`) | `internal/runtime/admission/crashv1.go`, `v1.MinBetCredits = 1`, `v1.MaxBetCredits = 1_000_000_000` |
| migration `0008_execution_ledger` (`ledger_postings_event_key`, deferred `OX009`) | present |

A throwaway stdlib prototype run while planning (outside the repo) reproduced spec §5.5's
`in-progress.jsonl` v2 bytes from `main`'s v1 golden: 3,640 bytes, sha256 `90a6b51d…627dee`.

## 2. Differences from the spec and plan-level resolutions (put these in the report)

1. **`main` differs from the spec's §1 snapshot** (it was written before LBT-12 merged): LBT-12 also
   shipped migration `0009_close_bound` (engineer addendum: `round_secrets.close_offset_micros`,
   `round_admission_heads.close_not_before_unix_micro`, `SECURITY DEFINER admit_command`). No LBT-18
   effect: `ExportHistoryV2` reads only `config_secret` from `round_secrets`, and `pgtest.LoadHistory`
   keeps leaving both new nullable columns NULL, as it does today.
2. **Spec §11 step 6 helpers:** `main`'s `seedGolden(t, db, x, name, upTo)` and `fundPlayer` already reuse
   a pre-created player account (`INSERT … ON CONFLICT (player_id) DO NOTHING`; holds and payouts look
   the account up by `player_id`). No local replacement helpers are needed. Do not edit any LBT-12 file.
3. **File placement (LBT-19 runs in parallel and may touch `cmd/crashctl`, `eventstore.Export`,
   `verification.Report`):** `ExportHistoryV2` goes in a **new** file
   `internal/persistence/eventstore/export_v2.go`, not in `export.go` as the spec's files table says.
   `export.go` gets only the package-doc paragraph. The body of `ExportHistory`, `encodeHeader`,
   `writeLine` and the `Export`/`Checkpoint` types stay untouched. V2's snapshot/secret/head/record
   reads are new unexported helpers in `export_v2.go`, mirroring `ExportHistory`'s queries, checks and
   sentinels exactly. Spec §8 allows shared helpers only on conditions; leaving v1 untouched is the
   additive option. New fields go at the **end** of `verification.Options` and `verification.Report`.
4. **Spec ambiguity, the position of an unterminated or oversized line in a v2 file:** in the ledger
   section it is the next ledger index `j` (the number of ledger lines read so far); otherwise it is v1's
   record position. M6 (drop the final `\n`) gives `format 22`, as spec §9.5 expects.
5. **Spec ambiguity, manifest `base` of a v2 history entry ("the v1 file name"):** use the file name,
   for example `"worked-example.jsonl"`. Tamper entries keep v1's `"base":"worked-example"` (spec §9.3).
6. **Spec ambiguity, invalid `LedgerRules`:** `Verify` rejects a nil entry or a duplicate
   `(Game, RulesVersion)` in **every** mode, before it reads the input.
7. The approved spec copy keeps its §0/§1 text verbatim. §0 ("Builder, this run: make no repository
   changes") described the planning run and is superseded by this plan. Do not edit it.

## 3. Rules for the whole change

- Follow `AGENTS.md`, ADR-007 and spec §11's builder rules: goimports grouping with the
  `github.com/juano-morello/orvex` local prefix; error strings lowercase, no trailing punctuation,
  prefixed `verification: `, `eventstore: ` or `pgtest: `; tests assert stage, sequence, exit code,
  `Unsupported`, sentinels and bytes, **never detail text**; `//go:build integration` as the first line
  of integration test files.
- No `go.mod`/`go.sum` change, no migration, no new package, no new module, no CI change.
- Must stay byte-identical (checked in step 8): `internal/audit/verification/testdata/golden/**` (v1
  oracle, 4 histories, 26 tamper fixtures, manifest), `internal/audit/chain`, `internal/audit/replay`,
  `internal/canonjson`, `internal/game/**` (crash/v1 math and golden vectors), `internal/ledger`,
  `migrations/`, `docs/adr/`, `AGENTS.md`, `.github/`, and every non-test file of `internal/runtime/**`.
- The v1 reader path, v1 `Report` contents, v1 `crashctl` output, `ExportHistory` bytes and
  `pgtest.LoadHistory`'s v1 behaviour do not change.
- Every v2 file is written by `generate.py --write` only. Never regenerate fixtures from Go output, never
  edit a spec §5.5 literal to match code, never weaken a golden comparison or a byte-flip test. If Go and
  the oracle disagree, find which side contradicts the spec and fix that side.
- Golangci `standard` includes `unused`: no dead helpers.
- Do not `git commit`, `push`, `merge`, `rebase` or `checkout`.

## 4. Files

| File | Action | Step |
|---|---|---|
| `docs/specs/ac81f88f_audit-ledger-stage.md` | new: the approved spec, approval edits only | 0 |
| `internal/audit/verification/testdata/golden-v2/generate.py` | new: v2 oracle (spec §9.1–§9.4) | 1 |
| `internal/audit/verification/testdata/golden-v2/{worked-example,instant-crash,empty-round,in-progress}.jsonl`, `tamper/*.jsonl` (26), `manifest.json` | new, written by `generate.py --write` only | 1 |
| `internal/audit/verification/verification.go` | edit: package doc, `FormatV2`, `StageLedger`, `Options.LedgerRules`, `LedgerReport`, `Report.Ledger`, v2 header/section dispatch, ledger-rule selection, ledger-stage call | 2 |
| `internal/audit/verification/ledger.go` | new: account-table and ledger-line parsing, ledger stage (§6.5) | 2 |
| `internal/audit/verification/ledger_test.go` | new | 2 |
| `internal/audit/verification/verification_test.go` | edit: `B11` only | 2 |
| `internal/audit/verification/fuzz_test.go` | edit: add `FuzzSingleByteFlipV2` | 2 |
| `cmd/crashctl/main.go` | edit: doc comment, `LedgerRules` wiring, `ledger ok` line | 3 |
| `cmd/crashctl/main_test.go` | edit: add `TestAuditGoldenV2`, `TestAuditFailuresV2` and their helpers | 3 |
| `internal/persistence/eventstore/export.go` | edit: package doc only | 4 |
| `internal/persistence/eventstore/export_v2.go` | new: `ExportHistoryV2` and its base-read helpers | 4 |
| `internal/persistence/eventstore/ledger.go` | new: ledger row reads, account resolution, sorting, `encodeHeaderV2`, `encodeLedgerLine` | 4 |
| `internal/persistence/eventstore/ledger_test.go` | new (unit, `package eventstore`) | 4 |
| `internal/persistence/postgres/pgtest/load.go` | edit: v2 loading | 5 |
| `internal/persistence/eventstore/export_integration_test.go` | edit: v2 tests; v2 cases appended to two existing tests | 5 |
| `internal/runtime/execution/ledger_audit_integration_test.go` | new (integration, `package execution_test`) | 6 |
| `Makefile` | edit: third `golden-check` oracle | 7 |
| `README.md` | edit: Status and two Layout bullets | 7 |
| `docs/architecture/persistence.md` | edit: new section 14 | 7 |

## 5. Work order

### Step 0: add the approved spec

1. `cp /home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/f4d371a8/context_handoff/lbt-18-approved-spec.md docs/specs/ac81f88f_audit-ledger-stage.md`
2. Replace lines 3–4 (`Status: **PROPOSED, awaiting engineer approval.** Approve, edit or reject each
   decision in the section 3` / `table. Implementation (section 11) starts only after approval, in a later
   run.`) with exactly these two lines:

   ```
   Status: **APPROVED (2026-10-15).** The engineer approved L-1 to L-14 as written, with no edits, and
   accepted every point of section 2. Implementation plan: `docs/specs/f4d371a8_audit-ledger-implementation.md`.
   ```
3. In the §3 table (lines 195–208, rows L-1 to L-14) change each trailing `| [ ] |` to `| [x] |`
   (14 rows).
4. Check: `diff <staged copy> docs/specs/ac81f88f_audit-ledger-stage.md` shows exactly the hunks `3,4c3,4`
   and `195,208c195,208`. diff exits 1 because the files differ, so inspect the hunks, not the exit code.
   Nothing else in the spec changes.
5. Before editing any code, record the v1 baseline output for step 8:
   `make build-crashctl VERSION=lbt18-check && ./bin/crashctl audit verify internal/audit/verification/testdata/golden/worked-example.jsonl > /tmp/lbt18-v1-verify-before.txt`
   (a fixed `VERSION` keeps the `verifier` line comparable once the tree is dirty).

### Step 1: v2 oracle and fixtures (spec §9.1–§9.4, L-10, L-11, L-12)

Write `internal/audit/verification/testdata/golden-v2/generate.py`: Python 3.11+, standard library only
(`argparse`, `hashlib`, `json`, `pathlib`, `sys`). It never imports, runs or reads Go code, `go.mod`,
`internal/ledger` or `../golden/generate.py`.

- **Docstring:** like the v1 oracle's (purpose, independence rules of spec §9.1, usage, "never regenerated
  from Go output", frozen with the first release per L-13). It names `docs/specs/ac81f88f_audit-ledger-stage.md`.
- **Encoder and validator:** copy (do not import) the v1 oracle's `OracleError`, `check`, `validate` and
  `ocj` (sorted keys, `ensure_ascii=False`, `(",", ":")`, keys `^[a-z][A-Za-z0-9]*$`, no surrogates,
  depth ≤ 32, UTF-8 output).
- **Inputs:** exactly `HERE.parent / "golden" / f"{name}.jsonl"` for `worked-example`, `instant-crash`,
  `empty-round`, `in-progress`, read as bytes. Split into lines that end with `\n`. Keep the raw record
  lines and `json.loads` each line for its values.
- **Input self-checks:** header `format` is `orvex/audit-history/v1`. `ocj(header) == header line`.
  Genesis `prevHash` is 64 zeros. Every later `prevHash` equals `sha256(b"orvex/audit/v1/record:" + previous
  line)`. The header `head` equals the last line's link hash.
- **Typed constants (from the spec, LBT-12 E-8/E-9/E-10, LBT-11 S-10):**
  - `uuid8(s)`: the first 16 bytes of `sha256(s.encode())`, `b[6] = (b[6] & 0x0f) | 0x80`,
    `b[8] = (b[8] & 0x3f) | 0x80`, as lowercase 8-4-4-4-12 text;
  - house = `uuid8("orvex/ledger/v1/account:global/house")`;
    round account = `uuid8("orvex/ledger/v1/account:" + roundId + "/round_pool")`;
    transaction = `uuid8("orvex/ledger/v1/transaction:" + roundId + "/" + str(k))`;
  - names `house` and `round_pool`; kinds `crash_bet`, `crash_cash_out`, `crash_settlement`;
  - player accounts `0190f3a8-7b3c-7d4e-8fa0-` + `n` as 12 lowercase hex digits: alice 1, bob 2, P3 3,
    dave 4, erin 5, frank 6. P3 is the Python string `"zo\u00eb \"\u2603\"\\\u0001\U0001F600"`. A player
    that is not in the map raises `OracleError`.
- **Per history:**
  - holds of command record `k`: a `PlaceBet` with `authority == "player"`, a non-empty `player` and
    `1 ≤ int(betCredits) ≤ 1_000_000_000` gets one hold `{accountId: player account, amount: betCredits}`;
  - releases = the hold accounts (release-always);
  - postings, events in order:
    - `BetPlaced`: player → round, `betCredits`, kind `crash_bet`, and the record must have exactly one
      hold, of that player and amount;
    - `BetCashedOut`: round → player, `payoutCredits`, kind `crash_cash_out`;
    - `RoundSettled`: `B = totalBetCredits`, `W = totalPayoutCredits`. If `B > W`, round → house `B − W`;
      if `B < W`, house → round `W − B`; if equal, nothing. Kind `crash_settlement`;
    - `RoundStarted`, `RoundCrashed`, `BetLost`: nothing. Two kinds in one record raise `OracleError`;
  - a record with postings gets one transaction `{kind, postings: [{amount, creditAccountId,
    debitAccountId, eventSequence, postingIndex: "0"..}], transactionId: tx(R, k)}`;
  - ledger line `k` is `{"holds", "kind": "ledger", "releases", "sequence": str(k), "transactions"}`, with
    every list strictly sorted (holds and releases by account ID, transactions by ID, postings by index).
    Line 0 is empty;
  - account table: every account a hold, release or posting references, sorted by `accountId`. Players are
    `{accountId, kind: "player", player}`. Systems are `{accountId, kind: "system", name, roundId}`, with
    house's `roundId` the nil UUID and the round account's `roundId` = R;
  - file = `ocj(v1 header + {"format": "orvex/audit-history/v2", "accounts": table})` + `\n`, then the v1
    record lines byte for byte, then the ledger lines, each followed by `\n`.
- **Self-checks (exit 2 on failure), spec §9.1 step 6:**
  - every §5.5 ID literal;
  - the worked-example and in-progress header lines;
  - worked-example ledger lines 0, 1, 4, 12, 21, 22 and instant-crash ledger line 5. The literals are
    typed in as `str` and `.encode()`d; the worked-example header contains escaped JSON
    (`\"`, `\\`, `\u0001`) inside the text, so type it as a raw string;
  - the §9.2 counts;
  - per history: Σ account nets = 0, every hold released, at most one posting per event;
  - settled histories: round account net 0, Σ `crash_bet` = `totalBetCredits`, Σ `crash_cash_out` =
    `totalPayoutCredits`;
  - house net −757,599,784 (worked-example) and +500 (instant-crash);
  - worked-example holds at 1, 2, 3, 4, 7, 11, 22 with amounts 100, 250, 1,000,000,000, 50, 10, 10, 10.
- **Tamper T1–T26** (spec §9.4 table, base worked-example v2, in table order): mutate parsed copies of the
  header and ledger objects, then re-encode only the affected lines with `ocj`. Record lines are never
  touched or rehashed.
  - T19: re-sort `accounts` after adding grace.
  - T8: add the second transaction, ID `uuid8("orvex/audit/v2/tamper/second-transaction")`, then sort
    the transactions.
  - T24: swap the first two accounts and do **not** re-sort.
  - T21: delete ledger line 22. T22: swap ledger lines 4 and 5.
  - T4: `{postingIndex "1", debit = round account, credit = house, amount "250", eventSequence "8"}`.
  - T7: swap debit and credit, amount `"-1000000000"`.
  - Checks:
    - ledger-stage cases: `[verify 1 ledger <seq|header>, replay 0]`;
    - format cases T21–T24: `[verify 1 format <seq|header>, chain 1 format <seq|header>]`;
    - T25 and T26: `[verify 0]`.
    - Check rows have v1's shape (`args []`, `command`, `exit`, `sequence` string or null, `stage` string
      or null).
- **Manifest** (`json.dumps(indent=1, sort_keys=True) + "\n"`, ASCII):
  - `{"format": "orvex/audit-history/v2", "histories": [...4], "tamper": [...26]}`;
  - each history has v1's keys (`name`, `file`, `records`, `commands`, `accepted`, `rejected`, `events`,
    `head`, `checkpoints[{sequence int, stateHash}]`) plus `base` (v1 file name, section 2 item 5),
    `accounts`, `transactions`, `postings`, `holds`, `releases` (ints);
  - each tamper entry is `{name, file: "tamper/<name>.jsonl", base: "worked-example", description,
    checks}`. `check(len(tamper) == 26)`.
- **`main(argv)`:** mirror the v1 oracle's `--write` / `--check` (mutually exclusive, required).
  - `--check` byte-compares in memory, reports unexpected files under `tamper/`, and exits 1 on a
    difference.
  - Print prefixes `audit-golden-v2:` / `audit-golden-v2-check:`.

Run `python3 internal/audit/verification/testdata/golden-v2/generate.py --write`, then `--check` (exit 0).
Then confirm the spec §5.5 sizes and hashes:

`wc -c internal/audit/verification/testdata/golden-v2/*.jsonl && sha256sum internal/audit/verification/testdata/golden-v2/*.jsonl`

| File | Bytes | SHA-256 |
|---|---|---|
| `worked-example.jsonl` | 16,385 | `3e8583c9…bf29ef` |
| `instant-crash.jsonl` | 4,941 | `f672a030…b5a8` |
| `empty-round.jsonl` | 2,781 | `a28074bf…09a0` |
| `in-progress.jsonl` | 3,640 | `90a6b51d…7dee` |

The full hashes are in the spec. If one disagrees, find which side contradicts the spec, and never edit
the literal.

### Step 2: `internal/audit/verification` (spec §6, L-4, L-5, L-6, L-7, L-8)

**`verification.go`.** Edits branch on the parsed format, and the v1 path keeps its code:

- Package doc: formats `orvex/audit-history/v1` and `/v2`, the `ledger` stage after state replay in
  `ModeVerify` for v2, and a reference to `docs/specs/ac81f88f_audit-ledger-stage.md` next to the LBT-9
  reference.
- Keep `const Format = "orvex/audit-history/v1"` (v1, unchanged). Add
  `const FormatV2 = "orvex/audit-history/v2"`.
- Add `StageLedger Stage = "ledger"` after `StageStateReplay`.
- `Options`: append after `ExpectHead`:
  ```go
  // LedgerRules holds the posting rules ModeVerify uses for v2 histories,
  // at most one per (game, rules version). v1 histories ignore it.
  LedgerRules []ledger.Rule
  ```
- New type `LedgerReport struct{ Accounts, Transactions, Postings, Holds, Releases int }` (doc: counts
  the ledger rows of a v2 history that passed the ledger stage). Append to `Report`:
  `Ledger *LedgerReport // nil unless the ledger stage ran`.
- `Verify`: after the mode switch and before `read`, validate `opt.LedgerRules`. A nil entry, or two
  entries whose `Descriptor()` has the same `(Game, RulesVersion)`, returns a plain
  `errors.New`/`fmt.Errorf` error (not a `*Failure`), in every mode.
- `header` gains `format string` (set for v1 too) and `accounts []account`. `history` gains
  `ledger []ledgerLine`.
- `parseHeader`:
  - accept `Format` or `FormatV2`; anything else stays unsupported at `header`;
  - the exact key set is v1's, plus `accounts` for v2;
  - the head, secret and checkpoint parsing is unchanged;
  - for v2, `hd.accounts, err = parseAccounts(o["accounts"])`, and any error is `format` at `header`.
- `read`, for v2 only (the v1 branch keeps `chain.Decode` exactly as today). Every line after the header:
  1. `canonjson.Unmarshal` strictly, else `format` at its position;
  2. in the record section, `kind == String("ledger")` starts the ledger section. Any other line goes
     through `chain.Decode(line)` with v1's handling: `ErrUnsupportedChain` at 0 is unsupported, any other
     error fails at the position. It is appended to `lines`/`records`;
  3. in the ledger section, at `j = len(h.ledger)`:
     - a line without `kind` "ledger" fails `format` at `j` ("record line after the ledger section");
     - otherwise `parseLedgerLine(o)` checks the exact key set and syntax (§5.3), else `format` at `j`;
     - `sequence != j` fails `format` at `j`;
     - `j == len(h.records)` fails `format` at `j` ("ledger line without a record");
  4. unterminated or oversized lines fail at `j` in the ledger section, else at v1's position
     (section 2 item 4);
  5. at EOF, after v1's empty-input and no-genesis rules: `len(h.ledger) < len(h.records)` fails `format`
     at `len(h.ledger)` ("missing ledger line").
- `checkChain`: `rep.Format = h.header.format`. Nothing else changes, and it covers only the record
  section.
- `replayHistory`:
  - after the existing registry lookup and the schema/state check, if
    `opt.Mode == ModeVerify && h.header.format == FormatV2`, select the rule from `opt.LedgerRules` whose
    `Descriptor()` has `Game == g.Game && RulesVersion == g.RulesVersion`. None fails
    `unsupported(StageRules, 0, …)`. `rule.Descriptor() != d` (any field) also fails
    `unsupported(StageRules, 0, …)`. This runs before `DecodeConfig`;
  - after `replay.Run` and the checkpoint check succeed, in the same condition, call
    `checkLedger(h, records, rule)` (records index `i-1` is record `i`). Its `*Failure` is returned, and
    on success `rep.Ledger` is set.

**`ledger.go`** (new, same package):

- Types: `account{id [16]byte; system bool; player, name string; roundID [16]byte}`,
  `hold{account [16]byte; amount int64}`, `posting{index uint64; debit, credit [16]byte; amount int64; event uint64}`,
  `transaction{id [16]byte; kind string; postings []posting}`,
  `ledgerLine{holds []hold; releases [][16]byte; transactions []transaction}`.
- `parseAccounts(v canonjson.Value) ([]account, error)` (§5.2):
  - an array of objects;
  - `kind` is read first. `player` needs exactly `{accountId, kind, player}` with a non-empty `player`.
    `system` needs exactly `{accountId, kind, name, roundId}`, with `name` matching
    `^[a-z][a-z0-9_]{0,63}$` (a byte loop, no regexp needed) and `roundId` a UUID (nil allowed). Any other
    kind is an error;
  - `accountId` values strictly increase (`bytes.Compare` on the 16 bytes).
- `parseLedgerLine(o canonjson.Object) (ledgerLine, uint64, error)` (§5.3):
  - exact keys `holds, kind, releases, sequence, transactions`;
  - holds: exact `{accountId, amount}`, `ParseUUID`/`ParseInt`, strictly increasing account;
  - releases: UUID strings, strictly increasing;
  - `sequence`: `ParseUint`;
  - transactions: exact `{kind, postings, transactionId}`, kind regex as above, strictly increasing ID;
  - postings: exact `{amount, creditAccountId, debitAccountId, eventSequence, postingIndex}`, `ParseInt`
    amount (any int64), `ParseUint` `eventSequence`, `ParseUint` `postingIndex ≤ 2147483647`, strictly
    increasing index.
- `checkLedger(h *history, records []replay.Record, rule ledger.Rule) (*LedgerReport, error)` implements
  spec §6.5 in exactly this order (A, then B per record, then C). Every failure is
  `fail(StageLedger, header or i, …)`, never unsupported.
  - **A** (at header):
    1. player strings are unique;
    2. `(name, roundId)` is unique among system accounts;
    3. an account named `house` has `ledger.HouseAccountID()` and the nil `roundId`. An account named
       `round_pool` has `roundId == R` and `ledger.RoundAccountID(R)`. Any account whose ID is one of
       those two is a system account with the matching name and `roundId`. Use `ledger.HouseAccountName`
       and `ledger.RoundAccountName`.
  - **B**, per `i = 0..N`, with `f_i = 1 + Σ_{k<i} n_k` and `n_i = len(records[i-1].Events)`:
    1. line 0 has empty `holds`, `releases` and `transactions`;
    2. every hold, release, debit and credit account is in the table;
    3. every hold `amount ≥ 1`;
    4. `releases` equals the hold accounts;
    5. every posting `amount ≥ 1` and debit ≠ credit;
    6. every posting `eventSequence` is in `[f_i, f_i+n_i−1]` and not yet in `seen`, then add it;
    7. `rule.Postings(records[i-1].Events, f_i)`, also for rejected records with no events. An `err`
       fails. The output must have `kind == ""` iff no postings, every `Amount ≥ 1`, distinct
       `EventSequence`s in range, `FromHold` only on a player debit, and every `Account.Valid()`;
    8. resolve the accounts:
       - round → `RoundAccountID(R)`; house → `HouseAccountID()`;
       - a `FromHold` debit needs exactly one hold on line `i`. That hold's account must be a player
         account in the table whose `player == Debit.Player`, with `amount == Amount`, and the debit is
         that account;
       - any other player account is the table's account with that player, and none is a failure;
       - then debit ≠ credit;
    9. compare with the exported transactions: none expected when there are no postings, else one
       `{TransactionID(R, i), kind, postings[j] = {j, debit, credit, Amount, EventSequence}}`. Compare
       count, ID, kind, posting count, then per posting index, debit, credit, amount, event. The first
       difference fails.
  - **C** (at header): every table account is referenced by some hold, release or posting.
  - **D**: return the counts over the file.

**`verification_test.go`:** change only `B11`. Rename it `B11-format-v3` and replace with
`` `"format":"orvex/audit-history/v3"` ``. The expectation stays `3, "format", "header"`. Nothing else in
the file changes.

**`ledger_test.go`** (new, `package verification_test`). Helpers: `goldenV2Dir = "testdata/golden-v2"`,
`manifestV2` (the v1 manifest struct plus `Base` and the five ledger counts on histories),
`loadManifestV2` (`DisallowUnknownFields`; format `verification.FormatV2`, 4 histories, 26 tamper cases),
`readGoldenV2`, `ledgerRules() []ledger.Rule{ledger.CrashV1()}`, `optionsV2(t, command, args)` =
`options(…)` plus `LedgerRules`. Reuse `options`, `wantOutcome`, `goldenLines`, `clone`, `join`,
`replaceOnce`, `flipHexDigit`, `valueStart` and `readGolden` from `verification_test.go`. Tests:

- `TestGoldenHistoriesV2`: per v2 history × {chain, replay, verify}:
  - success; `Format == FormatV2`, `Chain == "orvex/audit/v1"`; counts and head equal the manifest;
  - replay/verify: `Descriptor == v1.Descriptor()` and checkpoints equal the manifest;
  - verify: `*Ledger` equals the manifest's five counts. Chain and replay: `Ledger == nil`.
- `TestV2RecordsEqualV1`: the v2 record lines (lines 1..N+1) equal the base v1 file's lines 1..N+1 byte
  for byte. The v2 header, minus `accounts` and `format`, equals the v1 header minus `format`
  (canonjson objects, `reflect.DeepEqual`). The v2 file has exactly N+1 ledger lines.
- `TestV2Literals`:
  - the spec §5.5 worked-example header, in-progress header, worked-example ledger lines 0/1/4/12/21/22
    and instant-crash ledger line 5, typed in as Go raw strings, equal the golden lines byte for byte;
  - `canonjson.UUID` of `ledger.HouseAccountID()`, `ledger.RoundAccountID` (rounds `…01`–`…04`) and
    `ledger.TransactionID` (worked-example 1, 2, 3, 12, 17, 21; instant-crash 1, 5; in-progress 1, 3)
    equal the §5.5 table.
- `TestTamperFixturesV2`: every v2 manifest check gives exactly its stage, sequence and `Unsupported`
  (`wantOutcome` with `optionsV2`). The exit-0 checks succeed (T25, T26, and the `replay 0` checks of the
  ledger cases).
- `TestByteMutationsV2`: M1–M7 per spec §9.5, applied in memory, verify mode with `optionsV2`. Line
  index: 0 header, `1+s` record `s`, `24+j` ledger line `j` for worked-example. M2 mutates the **v1**
  worked-example. Each case asserts that the mutation changed bytes. Expected: M1 `3 format header`,
  M2 `1 format header`, M3 `1 format 0`, M4 `1 format 23`, M5 `1 format 1`, M6 `1 format 22`,
  M7 `1 chain 22`.
- `TestEverySingleByteFlipFailsV2`: every `^= 0x01` of `golden-v2/instant-crash.jsonl` in verify mode
  returns a `*Failure` (skipped under `-short`). If one verifies, stop and report a format hole. Do not
  weaken the test.
- `TestLedgerRuleRequired`: v2 worked-example in verify with `LedgerRules` nil gives `3 rules 0`. v2
  replay and chain without it pass. v1 worked-example verify without it passes.
- `TestLedgerRuleDescriptorMismatch`: a test type embedding `ledger.CrashV1()` whose `Descriptor()`
  returns `v1.Descriptor()` with `StateVersion = 2` gives `3 rules 0`.
- `TestLedgerRulesInvalidOptions`: `LedgerRules` `[]ledger.Rule{nil}`, and
  `{ledger.CrashV1(), ledger.CrashV1()}`, in each mode, return an error that is not a `*Failure`, without
  reading the input (a reader that counts `Read` calls must report 0).
- `TestLedgerRuleErrorIsLedgerFailure` (worked-example v2):
  - a rule wrapping `CrashV1()` that returns an error when `first == 1 && len(events) == 1` gives
    `1 ledger 1`;
  - a rule wrapping `CrashV1()` that, for that call, returns the real posting with `Amount = 0` gives
    `1 ledger 1`.
- `TestV1UnaffectedByLedgerRules`: each v1 golden × each mode gives `reflect.DeepEqual` reports with
  `LedgerRules` nil and with `ledgerRules()`, and `Ledger == nil`.

**`fuzz_test.go`:** add `FuzzSingleByteFlipV2(offset uint, xor byte)` over `golden-v2/in-progress.jsonl`,
with the same seed pattern and body as `FuzzSingleByteFlip` and `optionsV2(f, "verify", nil)`.
`xor == 0` is skipped. Any other flip must not verify.

### Step 3: `cmd/crashctl` (spec §7)

`main.go`:
- Add one doc-comment sentence: history formats v1 and v2, where verify adds the ledger stage for v2
  (LBT-18, `docs/specs/ac81f88f_audit-ledger-stage.md`).
- Import `internal/ledger`. Inside the existing `if mode != verification.ModeChain {…}` block, after
  `opt.Registry = reg`, add
  `if mode == verification.ModeVerify { opt.LedgerRules = []ledger.Rule{ledger.CrashV1()} }`.
- In `report()`, after the `state-replay` loop and before `ok`, add
  `if rep.Ledger != nil { "ledger ok accounts %d transactions %d postings %d holds %d releases %d\n" }`.
- Do not touch `usage`, the exit codes, `run`'s dispatch or the flags. LBT-19 edits those.

`main_test.go` (add, change nothing existing):
- Add `goldenV2Dir = "../../internal/audit/verification/testdata/golden-v2"`, a `manifestV2` struct
  (histories with the ledger counts, tamper checks) and `loadManifestV2` (4 histories, 26 tamper cases).
- `TestAuditGoldenV2`:
  - every v2 golden × {verify, replay, chain} exits 0 with an empty stderr, a first line `verifier …` and
    a last line `ok`, and the `chain ok` line from the manifest;
  - the `format orvex/audit-history/v2 chain orvex/audit/v1` line is present;
  - a `ledger ok` line is present iff the subcommand is `verify`, and equals the manifest counts;
  - the worked-example verify stdout equals spec §7's text exactly. Build it from the manifest head and
    checkpoints, with the line `ledger ok accounts 8 transactions 6 postings 6 holds 7 releases 7` before
    `ok`.
- `TestAuditFailuresV2`: every v2 manifest check through the CLI:
  - the exit code matches;
  - exit 0 gives stdout ending `\nok\n` and an empty stderr;
  - other exits give an empty stdout and exactly one stderr line starting
    `crashctl: audit <sub>: <stage> failed|unsupported at <sequence N|header>: `;
  - plus: a temp copy of the v2 worked-example with `format` set to `orvex/audit-history/v3` exits 3 with
    `format unsupported at header`, and `verify` of `tamper/posting-amount-edited.jsonl` gives
    `ledger failed at sequence 12`.
- `TestAuditGolden`, `TestAuditFailures`, `TestAuditReplayStateHash`, `TestAuditUsage`, `TestAuditIO`,
  `TestRun`: unchanged. `TestAuditGolden` keeps pinning the exact v1 output.

### Step 4: `eventstore.ExportHistoryV2` code and unit tests (spec §5.4, §8, L-9)

- `export.go`: append one paragraph to the package doc: `ExportHistoryV2` writes format v2 with the
  round's ledger rows (LBT-18, `docs/specs/ac81f88f_audit-ledger-stage.md`). No other change.
- `export_v2.go` (new): `ExportHistoryV2` with spec §8's signature and doc comment.
  1. One `postgres.SnapshotReadTxOptions()` transaction. Read the secret, snapshots, `S` and head with
     the same queries, checks and sentinels as `ExportHistory` (`ErrRoundNotFound`, `ErrNoCheckpoint`,
     `ErrCorrupt`), in an unexported helper.
  2. Read the ledger rows (`ledger.go`) before any write. Then encode the header.
  3. Write the header. Stream records `0..S` with `ExportHistory`'s per-record checks (sequence order,
     `MaxLineBytes`, no `\n`, count `S+1`).
  4. Write ledger lines `0..S`, flush, and return `Export{RoundID, Records, Head, Checkpoints}`.
  5. Nothing is written to `w` before every read and encode that can fail with
     `ErrRoundNotFound`/`ErrNoCheckpoint` (the existing tests assert `out.Len() == 0`).
- `ledger.go` (new):
  - row structs that model the nullable columns:
    - `accountRow{id [16]byte; kind string; playerID, name *string; roundID *[16]byte}`;
    - `holdRow{sequence int64; accountID [16]byte; amount int64}`;
    - `releaseRow{sequence int64; accountID [16]byte}`;
    - `transactionRow{id [16]byte; kind string; sequence int64; postings []postingRow}`;
    - `postingRow{index int64; debit, credit [16]byte; amount int64; eventSequence *int64}`;
  - `readLedger(ctx, tx, roundID, S)` runs spec §8 step 2's four queries plus the account query
    `WHERE account_id = ANY($1)`, with IDs passed as `[]pgtype.UUID` as in
    `internal/runtime/execution/commit.go`. Fewer account rows than referenced IDs → `ErrCorrupt`;
  - `encodeHeaderV2(head chain.Hash, cps []Checkpoint, configSecret []byte, accounts []accountRow) ([]byte, error)`
    builds spec §5.2 with `canonjson.Marshal`, sorts a copy of the accounts by `bytes.Compare`, maps a
    NULL `round_id` to the nil UUID, and uses `verification.FormatV2`;
  - `encodeLedgerLine(sequence uint64, holds []holdRow, releases []releaseRow, txs []transactionRow) ([]byte, error)`
    sorts copies (holds and releases by account, transactions by ID, postings by index) and maps a NULL
    `event_sequence` to `"0"`;
  - both return errors wrapping `ErrCorrupt` for spec §8 step 3: account kind other than player/system,
    player without a non-empty `player_id`, system without `name`, negative `posting_index` or
    `event_sequence`, and any string `canonjson.Marshal` rejects;
  - never filter, repair or recompute rows (amounts ≤ 0, self-transfers and line-0 rows are exported
    faithfully; the verifier rejects them).
- `ledger_test.go` (new, `package eventstore`, uses `goldenV2Dir = "../../audit/verification/testdata/golden-v2"`):
  - `TestEncodeLedgerMatchesGoldenV2`: per v2 golden, parse the header `accounts` and every ledger line
    into the row structs, deliberately unsorted (reverse every list), then encode with `encodeHeaderV2`
    and `encodeLedgerLine`. The header and every ledger line equal the golden bytes;
  - `TestEncodeLedgerNullEventSequence`: a posting with `eventSequence == nil` encodes
    `"eventSequence":"0"`;
  - `TestEncodeLedgerRejectsCorrupt`: `errors.Is(err, ErrCorrupt)` for account kind `bank`, a player with
    a NULL `player_id`, a player with an empty `player_id`, a system with a NULL `name`,
    `posting_index` −1, `event_sequence` −1, and a player ID with invalid UTF-8 (`"\xff"`);
  - `TestEncodeHeaderMatchesGolden`, `TestEncodeHeaderRejectsBadSecret`: unchanged.

### Step 5: `pgtest.LoadHistory` v2 and the export integration tests (L-14, spec §11 step 5)

`load.go`:
- Update the doc comment ("an LBT-9 history file, format v1 or v2; for v2 also the ledger rows, see
  LBT-18 L-14").
- Read the header `format`. If it is not `verification.FormatV2`, run exactly today's code.
  `pgtest` may import `internal/audit/verification`; there is no import cycle.
- For v2, in the same READ COMMITTED transaction:
  1. split the lines after the header at the first line whose `kind` is `"ledger"`: records before,
     ledger lines from there. Parse the ledger lines with `canonjson`; any parse error calls `t.Fatal`;
  2. after the heads, insert each header account, or require an existing row with the same `kind`,
     `player_id`, `name` and `round_id` and call `t.Fatalf` otherwise. Look up by `account_id` first,
     then `INSERT` if absent. Players use `floor_enforced = true`; system accounts use `false` and a
     NULL `round_id` for the nil UUID;
  3. find or create the global system account `faucet` (`gen_random_uuid()`, the SQL of execution's
     `fundPlayer`). For each player account with holds in the file, insert one `ledger_transactions` row
     (`gen_random_uuid()`, kind `'grant'`, round and sequence NULL) and one posting faucet → player of
     that account's total hold amount (`round_id`/`event_sequence` NULL);
  4. after the admissions, insert every hold (`round_id`, `sequence`, `account_id`, `amount`);
  5. after the chain records and domain events, for each ledger line in sequence order: its releases,
     then its transactions (`round_id` R, `sequence` j), then their postings (`round_id` R,
     `event_sequence` NULL for `"0"`). Releases before postings keep the floor check satisfied;
  6. snapshots and commit as today (the deferred `OX009` sees every hold released).
- `Loaded` is unchanged.

`export_integration_test.go`. Add `goldenV2Dir`, `manifestV2`/`loadManifestV2`, `readGoldenV2` and
`verifyV2` (ModeVerify, crash/v1 registry, `LedgerRules{ledger.CrashV1()}`). Each new test calls
`t.Parallel()` and uses its own `pgtest.MigratedDatabase`:
- `TestExportV2GoldenHistories`: per v2 golden, `LoadHistory` then `ExportHistoryV2`.
  - The output equals the golden file byte for byte.
  - `verifyV2` passes, and its `Ledger` equals the manifest counts.
  - `Export` holds the round ID, the record count, the head and the checkpoints from the manifest.
- `TestExportV1UnchangedWithLedgerRows`: per v2 golden, after loading it, `ExportHistory` equals the base
  v1 golden byte for byte.
- `TestExportV2StopsAtLatestSnapshot`: load the v2 worked-example with checkpoints `{0, 9}`.
  - The export has records 0..9 (equal to the golden lines) and ledger lines equal to the golden ledger
    lines 0..9.
  - Its `accounts` are alice, bob, P3, dave and the round account.
  - It verifies with `Ledger` = 5/3/3/5/5.
- `TestExportV2SharedAccounts`: load the worked-example and instant-crash v2 goldens into one database
  (they share alice's and the house account). Each round's `ExportHistoryV2` equals its golden.
- `TestExportV2WithoutLedgerRowsFailsVerification`: load the **v1** worked-example.
  - `ExportHistoryV2` has `"accounts":[]` and 23 empty ledger lines.
  - `verifyV2` fails `ledger` at 1 (assert stage and sequence via `errors.As`).
- `TestExportRoundNotFound` and `TestExportNoCheckpoint`: keep every existing assertion. Append the same
  call and assertions for `ExportHistoryV2` (the same sentinel and `out.Len() == 0`, with a fresh buffer).
- `TestExportGoldenHistories` and `TestExportStopsAtLatestSnapshot`: unchanged.

### Step 6: executor end-to-end test (L-14, spec §11 step 6)

New `internal/runtime/execution/ledger_audit_integration_test.go`: `//go:build integration`,
`package execution_test`. It reuses `TestMain`, `newDB`, `newExecutor`, `seedGolden`, `lease`,
`crashRegistry` and `ctx`. Local names must not collide with existing ones: use `goldenV2Dir`
(`"../../audit/verification/testdata/golden-v2"`), `readGoldenV2`, `exportHistoryV2`, `verifyExportV2`.

`TestExecutedGoldensExportVerifiedLedger`, per golden `worked-example`, `instant-crash`, `empty-round`,
`in-progress`:
1. fresh `newDB`, `newExecutor(t, pool, nil, nil)` (EngineBuild `golden`);
2. insert each **player** account of the v2 golden header with its golden ID
   (`INSERT INTO orvex.ledger_accounts (account_id, kind, player_id, floor_enforced) VALUES ($1, 'player', $2, true)`);
3. `gr := seedGolden(t, pool, x, name, 0)`. It funds by `player_id` and reuses the accounts from step 2;
4. `x.Drain(ctx, lease(gr.roundID), 100)` must execute `len(gr.cmds)`;
5. `ExportHistoryV2`, then verify (`ModeVerify`, crash/v1 registry, `LedgerRules{ledger.CrashV1()}`)
   with no error.

Let `S` be the export's last checkpoint sequence, which must be 21, 5, 3 and 2 respectively. Then:
- the record lines equal golden lines 0..S;
- the ledger lines equal the golden v2 ledger lines 0..S byte for byte;
- the export header `accounts` equals the golden table restricted to the IDs that ledger lines 0..S
  reference (compare canonjson values);
- `Report.Ledger` (accounts/transactions/postings/holds/releases) is worked-example 7/6/6/6/6,
  instant-crash 3/2/2/1/1, empty-round 0/0/0/0/0, in-progress 2/1/1/1/1.

Change no existing LBT-12 file.

### Step 7: Makefile and docs

- `Makefile`:
  - add `AUDIT_GOLDEN_V2_GEN := internal/audit/verification/testdata/golden-v2/generate.py` after
    `AUDIT_GOLDEN_GEN`;
  - add the third line `$(PYTHON) $(AUDIT_GOLDEN_V2_GEN) --check` to `golden-check` after the v1 audit
    line;
  - change the comment to name three oracles: the Crash v1 math vectors, the audit golden histories
    (format v1), and the format v2 histories with ledger rows;
  - no other change (the lint pin stays in sync with CI).
- `README.md`:
  - Status: turn "… (LBT-11) and the atomic execution commit … (LBT-12)." into "… (LBT-11), the atomic
    execution commit … (LBT-12) and the audit ledger stage with history format v2
    ([LBT-18](https://linear.app/lbtworks/issue/LBT-18/audit-ledger-stage-history-format-v2)).";
  - Layout: `internal/audit/verification` becomes "history reader (formats v1 and v2), staged verifier
    with the ledger stage, golden histories". `internal/persistence/eventstore` becomes "audit history
    export from the database (v1 LBT-10, v2 with ledger rows LBT-18)".
- `docs/architecture/persistence.md`: append `## 14. Audit export v2 (LBT-18)`, which covers:
  - what `ExportHistoryV2` reads (spec §5.4 row mapping, §8 queries, the same snapshot transaction and
    end `S`, rows with `sequence ≤ S`, only referenced accounts, sorted in Go);
  - what it omits (L-2: `created_at`, account totals, `floor_enforced`, `ledger_postings.round_id`;
    a NULL `event_sequence` is `"0"`);
  - that `ExportHistory` stays v1;
  - a link to the spec.
- `AGENTS.md` and CI: no change (`make golden-check` already runs in CI, and AGENTS.md's description
  stays accurate).

## 6. Acceptance mapping (spec §10)

| Acceptance | Tests |
|---|---|
| Verifier recomputes every posting of a golden and matches the exported rows | `TestGoldenHistoriesV2`, `TestExportV2GoldenHistories`, `TestExecutedGoldensExportVerifiedLedger`, `TestAuditGoldenV2` |
| Tampered amount / account / missing / extra / unbalanced fail | T1, T13 / T2, T15–T18 / T3 / T4, T8 / T5–T7, via `TestTamperFixturesV2`, `TestAuditFailuresV2` |
| Holds, at most one posting per event | T12–T14, T20; T8, T9 |
| v1 files still verify unchanged | existing v1 tests untouched (B11 retargeted), `TestV1UnaffectedByLedgerRules`, `TestExportV1UnchangedWithLedgerRows`, v1 goldens byte-identical (`make golden-check`, step 8 `git diff`) |
| Fail closed | M1, `TestLedgerRuleRequired`, `TestLedgerRuleDescriptorMismatch` |
| Every v2 byte covered except L-5's limits | `TestEverySingleByteFlipFailsV2`, `FuzzSingleByteFlipV2`; T25, T26 |

## 7. Stop and report (do not work around)

- A spec §5.5 literal or file hash cannot be reproduced and the spec is internally inconsistent.
- A single-byte flip of `instant-crash` v2, or the fuzz run, finds a v2 file that verifies.
- A database constraint rejects the spec's loader order or the executor's rows export differently from
  the golden ledger lines. Never change a migration or an LBT-12 non-test file.
- Any v1 artifact or v1 output would have to change.

## 8. Verification

Run each command in the foreground to completion, timed (for example with bash's `time`). Report each
exact command, its exit status and its duration. All must exit 0 unless stated otherwise.

```sh
python3 internal/audit/verification/testdata/golden-v2/generate.py --check
wc -c internal/audit/verification/testdata/golden-v2/*.jsonl
sha256sum internal/audit/verification/testdata/golden-v2/*.jsonl   # must equal spec §5.5
make test
make test GOTESTFLAGS=
go test -run '^$' -fuzz '^FuzzSingleByteFlipV2$' -fuzztime 30s ./internal/audit/verification
make lint
go vet ./...
go vet -tags integration ./...
make build
make golden-check
make test-integration
make build-crashctl VERSION=lbt18-check
./bin/crashctl audit verify internal/audit/verification/testdata/golden-v2/worked-example.jsonl
./bin/crashctl audit verify internal/audit/verification/testdata/golden/worked-example.jsonl > /tmp/lbt18-v1-verify-after.txt
cmp /tmp/lbt18-v1-verify-before.txt /tmp/lbt18-v1-verify-after.txt
./bin/crashctl audit replay internal/audit/verification/testdata/golden-v2/tamper/posting-amount-edited.jsonl
./bin/crashctl audit verify internal/audit/verification/testdata/golden-v2/tamper/posting-amount-edited.jsonl   # must exit 1
sed 's#"format":"orvex/audit-history/v2"#"format":"orvex/audit-history/v3"#' internal/audit/verification/testdata/golden-v2/worked-example.jsonl > /tmp/lbt18-v3.jsonl
./bin/crashctl audit verify /tmp/lbt18-v3.jsonl   # must exit 3
make build
git diff --exit-code main -- internal/audit/verification/testdata/golden internal/audit/chain internal/audit/replay internal/canonjson internal/game internal/ledger migrations go.mod go.sum docs/adr AGENTS.md .github
git status --porcelain -- internal/audit/verification/testdata/golden internal/audit/chain internal/audit/replay internal/canonjson internal/game internal/ledger migrations go.mod go.sum docs/adr AGENTS.md .github   # must print nothing
git status --porcelain -- internal/runtime   # must print only: ?? internal/runtime/execution/ledger_audit_integration_test.go
```

Expected:
- The v2 worked-example verify prints `ledger ok accounts 8 transactions 6 postings 6 holds 7 releases 7`
  before `ok`.
- The v1 verify output is byte-identical to the step 0 baseline (no `ledger` line).
- The final `make build` restores the default-version binaries.
- If the fuzz run fails, stop and report the input. Do not keep the `testdata/fuzz/` corpus file it
  writes, and do not weaken the target.
- `git status --porcelain` shows only the files in section 4.

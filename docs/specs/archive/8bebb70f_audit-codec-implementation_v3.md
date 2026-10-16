# LBT-9 close-out (v3): verify the finished implementation and report every changed file

- Linear: [LBT-9](https://linear.app/lbtworks/issue/LBT-9/build-canonical-event-codec-and-audit-replay-cli)
  (M1 · Deterministic Core; blocks LBT-10).
- Normative spec: `docs/specs/8bebb70f_canonical-codec-audit-cli.md` ("the spec"), with decisions A-1
  to A-20 approved and every point in its section 2 accepted.
- Implementation plan the work followed: `docs/specs/8bebb70f_audit-codec-implementation_v2.md`
  ("plan v2"). Plan v1 (`..._implementation.md`) is the earlier record.
- This plan (v3) supersedes neither. It records what was verified and tells the builder how to close
  the run, including listing every changed file by its exact path (§4). The previous build report
  collapsed 26 fixture files into one directory entry.

## 0. Bottom line

**The implementation is complete.** While planning, every "done means" item in the prompt was
checked by running it, and each passed (§1). The builder makes **no code, fixture or doc changes**
unless a gate in §2 fails on re-run. Its main output is a report that lists all 63 changed paths
individually, as given in §4.

## 1. What was verified while planning

Worktree: `HEAD` `866a7c7` on branch `odin/8bebb70f`, merge base with `main` `6ec9f16`. The worktree
does not contain PR #5. Uncommitted: 9 modified files and 43 untracked files (§4.2).

Each result below was judged by the command's exit status.

| Check | Command | Result |
|---|---|---|
| Unit, golden, race tests, uncached | `go test -count=1 -race ./...` (same as `make test` plus `-count=1`) | exit 0; all 10 packages `ok` |
| Lint | `make lint` (golangci-lint v2.14.0) | exit 0; `0 issues.` |
| Vet | `go vet ./...` | exit 0 |
| Build | `make build` | exit 0; `bin/gateway`, `bin/engine`, `bin/crashctl` |
| Both oracles | `make golden-check` | exit 0; LBT-7 `golden-check: 6 files match`, audit `audit-golden-check: 31 files match` |
| Oracles on the minimum Python | `uv run --no-project --python 3.11 python <each generate.py> --check` | exit 0 for both on CPython 3.11.15 |
| Frozen files unchanged | `git diff --exit-code 6ec9f16 -- go.mod go.sum internal/game/crash/v1/{params,curve,payout,fairness,commands,events,decide,evolve,projection}.go internal/game/crash/v1/testdata specs app_docs .github docs/adr` | exit 0 |
| No dependency change | `go mod tidy -diff` | exit 0 |
| Formatting | `gofmt -l cmd internal` | no output |
| `round.go` | `git diff 6ec9f16 -- internal/game/crash/v1/round.go` | only `Adapter()` now passes `Codec{}`; PR #5's comment line (172) untouched |
| Fuzz smoke | `FuzzSingleByteFlip`, `FuzzUnmarshal`, `FuzzDecodeEvent`, 10 s each | exit 0; no corpus files written to the tree |

Spec conformance checks:

- **Spec approved.** Status reads `**APPROVED** on 2026-10-14 …` and names plan v2. The section 3
  table has 20 `[x]` and no `[ ]`.
- **A-18 golden histories.** `manifest.json` matches spec §8.2 (columns: records, commands, accepted,
  rejected, events, checkpoints):

  | history | records | commands | accepted | rejected | events | checkpoints |
  |---|---|---|---|---|---|---|
  | worked-example | 23 | 22 | 8 | 14 | 9 | 0, 9, 22 |
  | instant-crash | 6 | 5 | 4 | 1 | 5 | 5 |
  | empty-round | 4 | 3 | 3 | 0 | 3 | 3 |
  | in-progress | 4 | 3 | 3 | 0 | 3 | 0, 3 |

- **A-19 tamper and reorder cases.**
  - There are 26 files in `tamper/`. Every manifest check (subcommand, args, exit code, stage,
    sequence) equals the spec §8.4 "expected" column, R1 to R26.
  - `TestByteMutations` has subtests B1 to B16. B16 has two subtests (empty input and header only).
  - `TestEverySingleByteFlipFails` (B17) runs on `empty-round.jsonl` and is skipped only under
    `-short`.
- **A-16 CLI, run with `bin/crashctl`.**
  - `audit verify` on `worked-example.jsonl` → exit 0. The report has spec §7's line order. Head
    `7cf8cc4b…3da7`; state hashes at checkpoints 0, 9, 22, the first being `1c98ea46…e7e7` (spec §5.6).
  - `audit replay` on `in-progress.jsonl` → 0.
  - `audit chain -` with `empty-round.jsonl` on stdin → 0.
  - `tamper/payout-edited.jsonl` → 1 (`decision-replay failed at sequence 12`).
  - `tamper/rules-version-2.jsonl` → 3 (`rules unsupported at sequence 0`).
  - No FILE → 2. `/nonexistent` → 4. No arguments → 2, with usage.
- **A-9 third-tool check.** The `sha256sum` command prints
  `cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284`, the spec's `hash_0`.
- **Oracle independence (spec §8.1).** `generate.py` imports only `argparse`, `bisect`, `hashlib`,
  `hmac`, `json`, `pathlib`, `sys`, and reads no Go file.
- **Every test named in plan v2 exists**, as do the extra tests `TestAccessors`,
  `TestAdapterCodecErrorsPassThrough`, `TestEncodeRejectsUndecodable` and `TestFailureError`. §3 maps
  them to the acceptance criteria.

## 2. Builder steps

1. **Change nothing first.** Do not edit Go code, fixtures, the oracles, `Makefile`, `README.md`,
   `AGENTS.md` or any spec.
   - Never run `generate.py --write`; use `--check` only.
   - The spec's status line keeps pointing at plan v2, the plan the implementation followed. Do not
     repoint it to v3.
2. **Re-run the gates once.** Each must exit 0:

   ```sh
   make test
   make lint
   go vet ./...
   make build
   make golden-check
   go mod tidy -diff
   git diff --exit-code 6ec9f16 -- go.mod go.sum internal/game/crash/v1/params.go internal/game/crash/v1/curve.go internal/game/crash/v1/payout.go internal/game/crash/v1/fairness.go internal/game/crash/v1/testdata specs app_docs .github docs/adr
   ./bin/crashctl audit verify internal/audit/verification/testdata/golden/worked-example.jsonl
   ./bin/crashctl audit replay internal/audit/verification/testdata/golden/in-progress.jsonl
   ./bin/crashctl audit chain - < internal/audit/verification/testdata/golden/empty-round.jsonl
   ```

   These must exit with the code shown:
   - `./bin/crashctl audit verify internal/audit/verification/testdata/golden/tamper/payout-edited.jsonl`
     → 1
   - `./bin/crashctl audit verify internal/audit/verification/testdata/golden/tamper/rules-version-2.jsonl`
     → 3
   - `./bin/crashctl audit verify` → 2
   - `./bin/crashctl audit verify /nonexistent` → 4
3. **Only if a gate fails:**
   - Fix the cause under plan v2's rules (§2 items 1–17 there).
   - Never edit a spec §5.6 literal, a manifest expectation or a fixture to match Go. Fix the side
     that contradicts the spec.
   - Re-run every gate in step 2.
   - Add any file you touch to the report. Do not rewrite the §4 list.
4. **Report.** List every path in §4.1, §4.2 and §4.3 individually, by exact path from the repository
   root.
   - No directory entries, globs, brace expansions or counts in place of paths. Each of the 26 tamper
     fixtures is its own entry.
   - The list is relative to the merge base `6ec9f16`, so it stays correct however the orchestrator
     splits commits. The plan commit for v2 (`866a7c7`) swept partial implementation work into a
     "docs(spec)" commit; this plan's commit may do the same with the files in §4.2.
5. **PR description content** (for whoever opens the PR):
   - the spec's section 2 items 1–10, the ADR points flagged for the engineer;
   - plan v2's §2 item 1 deviation: `tallyCodec` lives in `internal/game/codec_test.go`;
   - no new dependency (`go.mod` and `go.sum` unchanged);
   - the crash/v1 math files and golden vectors are byte-identical;
   - the branch predates PR #5. On merge, `specs/` and `app_docs/` stay as `main` has them; this
     branch does not touch them.

## 3. Acceptance → tests (all present and passing)

| Acceptance | Tests |
|---|---|
| Golden history verifies | `TestGoldenHistories` (`internal/audit/verification`), `TestAuditGolden` (`cmd/crashctl`) |
| Tampered history fails | `TestTamperFixtures` (R3–R25), `TestByteMutations` (B1, B2, B6–B16), `TestEverySingleByteFlipFails` (B17), `FuzzSingleByteFlip`, `TestAuditFailures` (every manifest check through the CLI, plus B1) |
| Reordered history fails | `TestTamperFixtures` (R1 `reorder-bets`, R2 `cashout-before-start`), `TestByteMutations` (B3, B4, B5) |
| Exported replay reconstructs identical state hash | `TestGoldenHistories` (replayed checkpoint hashes equal the oracle's manifest values), `TestAuditReplayStateHash`, `TestWorkedExampleInitialStateHash` (`internal/audit/chain`) |
| Fail closed on unknown artifacts (ADR-006) | R14–R18 and B11 in `TestTamperFixtures` and `TestByteMutations`; exit 3 in `TestAuditFailures` |
| Commitment/reveal (ADR-006) | R7, R8, B8; `TestDecodeConfigRevealMismatch` |
| Anchor limitation (ADR-001) | R26 (exit 0 without `-expect-head`, exit 1 with it); `TestExpectHead` |
| CLI contract (A-16) | `TestRun`, `TestRunWriteError`, `TestAuditUsage`, `TestAuditIO`, `TestAuditGolden`, `TestAuditFailures`, `TestAuditReplayStateHash` |
| Codec contract (A-6) | `TestAdapterCodecRejectsWrongTypes`, `TestAdapterCodecPassThrough`, `TestAdapterCodecErrorsPassThrough`, `TestAdapterRejectionCode` (`internal/game`); `TestAdapterRegistration` (crash/v1) |
| Canonical encoding (A-1 to A-4) | `TestMarshal`, `TestMarshalRejects`, `TestUnmarshalRoundTrip`, `TestUnmarshalRejects`, `TestInts`, `TestHexUUID`, `TestAccessors`, `FuzzUnmarshal`, `FuzzStringRoundTrip` (`internal/canonjson`); `TestCodecVectors`, `TestCodecRoundTrip`, `TestCodecRejectsMalformed`, `TestCodecRejectsUnencodable`, `TestEncodeStateCoversEveryField`, `TestRejectionCode`, `FuzzDecodeCommand`, `FuzzDecodeEvent` (crash/v1) |
| Chain records (A-7 to A-10) | `TestWorkedExampleGenesis`, `TestWorkedExampleRecord1`, `TestDecodeRoundTrip`, `TestDecodeRejects`, `TestEncodeRejectsUndecodable`, `TestDecodeUnsupportedChain`, `TestStateHashBindsVersion` |
| Replay (A-13, A-15) | `TestRunReproducesRound`, `TestDecisionMismatch`, `TestFailClosedDecideIsMismatch`, `TestEvolveErrorIsStateMismatch` |
| Verifier robustness (A-11, §6) | `TestLineLimit`, `TestChainModeNeedsNoRegistry`, `TestReadError`, `TestFailureError` |

## 4. Every changed file, by exact path

The paths are relative to merge base `6ec9f16` and were generated from `git` while planning, not
typed by hand. A = added, M = modified. There are 63 files in total.

### 4.1 Already committed on this branch (`6ec9f16..866a7c7`)

11 files.

- A `docs/specs/8bebb70f_audit-codec-implementation.md`
- A `docs/specs/8bebb70f_audit-codec-implementation_v2.md`
- A `docs/specs/8bebb70f_canonical-codec-audit-cli.md`
- A `internal/canonjson/canonjson.go`
- A `internal/canonjson/canonjson_test.go`
- A `internal/canonjson/fuzz_test.go`
- A `internal/game/codec.go`
- M `internal/game/contract.go`
- M `internal/game/contract_test.go`
- M `internal/game/registry.go`
- M `internal/game/registry_test.go`

### 4.2 Uncommitted in the worktree at planning time

52 files: 9 modified and 43 untracked. `docs/specs/8bebb70f_canonical-codec-audit-cli.md` also appears in 4.1. 51 of these files are not in 4.1.

- M `AGENTS.md`
- M `Makefile`
- M `README.md`
- M `cmd/crashctl/main.go`
- M `cmd/crashctl/main_test.go`
- M `docs/specs/8bebb70f_canonical-codec-audit-cli.md`
- A `internal/audit/chain/chain.go`
- A `internal/audit/chain/chain_test.go`
- A `internal/audit/replay/replay.go`
- A `internal/audit/replay/replay_test.go`
- A `internal/audit/verification/fuzz_test.go`
- A `internal/audit/verification/testdata/golden/empty-round.jsonl`
- A `internal/audit/verification/testdata/golden/generate.py`
- A `internal/audit/verification/testdata/golden/in-progress.jsonl`
- A `internal/audit/verification/testdata/golden/instant-crash.jsonl`
- A `internal/audit/verification/testdata/golden/manifest.json`
- A `internal/audit/verification/testdata/golden/tamper/accepted-to-rejected.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/cashout-before-start.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/chain-v2.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/commitment-edited.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/crash-point-edited.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/delete-rejection.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/duplicate-command-id.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/effective-time-edited.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/event-sequence-gap.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/genesis-prevhash-nonzero.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/json-number.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/leading-zero-int.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/payout-edited.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/rejected-with-events.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/rejection-reason-edited.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/reorder-bets.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/roundid-mismatch.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/rules-version-2.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/schema-version-2.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/state-version-2.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/unknown-event-field.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/unknown-event-type.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/unknown-game.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/unknown-record-kind.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/unsorted-keys.jsonl`
- A `internal/audit/verification/testdata/golden/tamper/uppercase-hex.jsonl`
- A `internal/audit/verification/testdata/golden/worked-example.jsonl`
- A `internal/audit/verification/verification.go`
- A `internal/audit/verification/verification_test.go`
- A `internal/game/codec_test.go`
- A `internal/game/crash/v1/codec.go`
- A `internal/game/crash/v1/codec_fuzz_test.go`
- A `internal/game/crash/v1/codec_test.go`
- M `internal/game/crash/v1/params_test.go`
- M `internal/game/crash/v1/round.go`
- M `internal/game/crash/v1/round_test.go`

### 4.3 Added by this plan

- A `docs/specs/8bebb70f_audit-codec-implementation_v3.md`

### 4.4 Complete list against `6ec9f16` (the report's file list)

63 files: 51 added and 12 modified.

```
AGENTS.md
Makefile
README.md
cmd/crashctl/main.go
cmd/crashctl/main_test.go
docs/specs/8bebb70f_audit-codec-implementation.md
docs/specs/8bebb70f_audit-codec-implementation_v2.md
docs/specs/8bebb70f_audit-codec-implementation_v3.md
docs/specs/8bebb70f_canonical-codec-audit-cli.md
internal/audit/chain/chain.go
internal/audit/chain/chain_test.go
internal/audit/replay/replay.go
internal/audit/replay/replay_test.go
internal/audit/verification/fuzz_test.go
internal/audit/verification/testdata/golden/empty-round.jsonl
internal/audit/verification/testdata/golden/generate.py
internal/audit/verification/testdata/golden/in-progress.jsonl
internal/audit/verification/testdata/golden/instant-crash.jsonl
internal/audit/verification/testdata/golden/manifest.json
internal/audit/verification/testdata/golden/tamper/accepted-to-rejected.jsonl
internal/audit/verification/testdata/golden/tamper/cashout-before-start.jsonl
internal/audit/verification/testdata/golden/tamper/chain-v2.jsonl
internal/audit/verification/testdata/golden/tamper/commitment-edited.jsonl
internal/audit/verification/testdata/golden/tamper/crash-point-edited.jsonl
internal/audit/verification/testdata/golden/tamper/delete-rejection.jsonl
internal/audit/verification/testdata/golden/tamper/duplicate-command-id.jsonl
internal/audit/verification/testdata/golden/tamper/effective-time-edited.jsonl
internal/audit/verification/testdata/golden/tamper/event-sequence-gap.jsonl
internal/audit/verification/testdata/golden/tamper/genesis-prevhash-nonzero.jsonl
internal/audit/verification/testdata/golden/tamper/json-number.jsonl
internal/audit/verification/testdata/golden/tamper/leading-zero-int.jsonl
internal/audit/verification/testdata/golden/tamper/payout-edited.jsonl
internal/audit/verification/testdata/golden/tamper/rejected-with-events.jsonl
internal/audit/verification/testdata/golden/tamper/rejection-reason-edited.jsonl
internal/audit/verification/testdata/golden/tamper/reorder-bets.jsonl
internal/audit/verification/testdata/golden/tamper/roundid-mismatch.jsonl
internal/audit/verification/testdata/golden/tamper/rules-version-2.jsonl
internal/audit/verification/testdata/golden/tamper/schema-version-2.jsonl
internal/audit/verification/testdata/golden/tamper/state-version-2.jsonl
internal/audit/verification/testdata/golden/tamper/unknown-event-field.jsonl
internal/audit/verification/testdata/golden/tamper/unknown-event-type.jsonl
internal/audit/verification/testdata/golden/tamper/unknown-game.jsonl
internal/audit/verification/testdata/golden/tamper/unknown-record-kind.jsonl
internal/audit/verification/testdata/golden/tamper/unsorted-keys.jsonl
internal/audit/verification/testdata/golden/tamper/uppercase-hex.jsonl
internal/audit/verification/testdata/golden/worked-example.jsonl
internal/audit/verification/verification.go
internal/audit/verification/verification_test.go
internal/canonjson/canonjson.go
internal/canonjson/canonjson_test.go
internal/canonjson/fuzz_test.go
internal/game/codec.go
internal/game/codec_test.go
internal/game/contract.go
internal/game/contract_test.go
internal/game/crash/v1/codec.go
internal/game/crash/v1/codec_fuzz_test.go
internal/game/crash/v1/codec_test.go
internal/game/crash/v1/params_test.go
internal/game/crash/v1/round.go
internal/game/crash/v1/round_test.go
internal/game/registry.go
internal/game/registry_test.go
```

# Implement LBT-9: canonical codec, hash chain, audit replay and `crashctl audit`

- Linear: [LBT-9](https://linear.app/lbtworks/issue/LBT-9/build-canonical-event-codec-and-audit-replay-cli)
  (M1 · Deterministic Core; blocks LBT-10).
- **Normative spec: `docs/specs/8bebb70f_canonical-codec-audit-cli.md` ("the spec").** The engineer
  approved decisions A-1 to A-20 as written, with no edits, and accepted every point in its section 2.
  That includes the ADR-007 layout deviation for `internal/canonjson/` and `internal/audit/chain/`, and
  the LBT-6 contract change.
- Read the spec in full before starting. Where this plan and the spec differ, this plan wins only for
  the items marked **deviation** in §2. Everything else here clarifies or orders the spec's section 10.
- ADRs: ADR-001 (primary), ADR-003, ADR-006, ADR-004, ADR-005, ADR-002, ADR-007 (`docs/adr/`). Follow
  `AGENTS.md`.

## 0. Done means

1. LBT-9 acceptance is covered by tests, using every fixture and case the spec lists:
   - the golden histories verify;
   - tampered and reordered histories fail (R1–R26, B1–B17);
   - an exported replay reconstructs the identical state hash (spec §9).
2. `crashctl audit verify|replay|chain` behaves exactly as spec A-16 and §7 say, exit codes included.
3. `make test`, `make lint`, `make build` and `make golden-check` pass. `make golden-check` runs both
   the LBT-7 oracle and the new audit oracle.
4. The crash/v1 math, its parameters and its golden vectors are byte-identical:
   `params.go`, `curve.go`, `payout.go`, `fairness.go`, and everything under
   `internal/game/crash/v1/testdata/`.
5. `go.mod` and `go.sum` are unchanged (no new module).
6. The spec is marked approved (§3 step 0).

Out of scope: everything in the spec's section 12; `make audit` (deferred by spec §2 item 9); the
ADR-004 amendment (spec §2 item 10); write-ups under `docs/changes/`.

## 1. Workspace facts the builder must know

- **This worktree is based on `6ec9f16` and does not contain PR #5** (`4da99af` on `main`). PR #5 moved
  `specs/*` to `docs/specs/*` and `app_docs/*` to `docs/changes/*`. It also edited one comment line in
  each of three files: `params.go` line 2, `round.go` line 172 here (the `Rules` doc comment) and the LBT-7
  `generate.py` docstring.
- In this worktree the older specs are still at `specs/` (same names). Read them there, or with
  `git show main:docs/specs/<name>`.
- **Do not move or edit `specs/`, `app_docs/`, or those three comment lines.** PR #5 already did that
  on `main`, and doing it again would conflict when this branch merges.
- New code comments cite spec paths in their final location: `docs/specs/8bebb70f_canonical-codec-audit-cli.md`,
  `docs/specs/e96d124d_crash-v1-math-spec.md`, and so on.
- The only `round.go` edit is the `Adapter()` body (line 34), a different hunk from PR #5's.
- `docs/specs/8bebb70f_canonical-codec-audit-cli.md` is untracked in this worktree. It is part of this
  change.
- Toolchain: go1.26.5, golangci-lint v2.14.0, Python 3.14.6 locally (the oracle must run on 3.11+).
  CI's `ubuntu-latest` Python is 3.12.
- Never run `git commit`, `push`, `merge`, `rebase` or `checkout`.

## 2. Clarifications and deviations

Each item resolves a gap found while re-reading the spec against the code. None changes an approved
decision.

1. **Deviation (test-file location only): `tallyCodec` goes in `internal/game/codec_test.go`, not
   `sample_game_test.go`.**
   - `internal/game/contract_test.go`'s `TestNoIOImports` checks `sample_game_test.go`'s imports
     against `errors`, `fmt`, `maps`, `slices` and `internal/game`. `tallyCodec` needs
     `internal/canonjson`, which must not join that allowlist, because the allowlist guards production
     game code.
   - Also add `"codec.go"` to that test's must-check list. `internal/game/codec.go` imports only
     `errors`.
2. **crash/v1 `codec.go` imports exactly:** `fmt` (`fmt.Errorf` only),
   `github.com/juano-morello/orvex/internal/canonjson`, `github.com/juano-morello/orvex/internal/game`,
   and `errors` if needed. No package-level `var`: enum mappings are `switch` statements, sentinels
   come from `game`. No floats.
   - crash/v1's `TestNoIOImports` (`params_test.go`) gains the `canonjson` path in `allowed` and
     `"codec.go"` in the must-check list. Nothing else in `params_test.go` changes.
3. **`game.Adapt` inference.** `game.Adapt(Descriptor(), Rules{}, Codec{})` should infer the type
   arguments as the two-argument form does today. If it does not compile, pass them explicitly:
   `game.Adapt[State, Command, Event, Config](...)`.
4. **Erased codec methods on `adapter`.**
   - Each `Encode*` type-asserts its argument with roles `config`, `command`, `event` and `state`. A
     mismatch, including nil, returns `ErrTypeMismatch` through the existing `mismatch` helper, and
     the codec is not called.
   - Each `Decode*` returns `(nil, err)` on error, otherwise the typed value boxed in `any`.
   - Codec errors are returned unwrapped.
   - `RejectionCode` delegates directly.
   - Interface types include pointer types in their method sets: `*crash.PlaceBet` satisfies
     `crash.Command`, so the adapter passes it through and the crash codec rejects it with
     `game.ErrUnencodable` (its type switch covers value types only).
5. **`contract.go`.** Two edits only:
   - Replace the `Decide` doc sentence "A non-nil error rejects the command: no events apply and the
     runtime records a terminal command result (ADR-003)." with the spec A-6 wording.
   - Extend the package doc's first sentence to mention the canonical codec contract.
6. **`canonjson` details.**
   - Depth: the top-level object is depth 1, and each nested object or array adds 1. Depth 33 fails.
   - `Marshal(Object)` requires a top-level object; `Unmarshal` returns an `Object`.
   - Errors wrap `ErrInvalid` (Marshal) or `ErrNotCanonical` (Unmarshal, with the byte offset in the
     message).
   - `Marshal` validates strings with `utf8.ValidString`, which also rejects surrogates and overlong
     forms.
   - Keys never need escaping: they are ASCII alphanumerics.
7. **`chain` package.**
   - Declares `var ErrUnsupportedChain = errors.New("chain: unsupported chain version")`.
   - `Decode` wraps it when a `chain` key holds any other value.
   - Payload sub-objects (`config`, `command`, `event`) are returned as their `canonjson.Marshal` bytes.
     Because parsing is strict, these equal the original sub-slices.
   - `EncodeGenesis` and `EncodeCommand` validate their inputs with the same syntax rules as `Decode`:
     game ID regex, non-zero versions, `engineBuild` charset and length, reason regex, payloads that
     are OCJ objects. They return an error rather than writing an undecodable record.
8. **`verification` package.**
   - `const MaxLineBytes = 64 << 20`.
   - `Options.Registry == nil` is allowed only in `ModeChain`. In other modes `Verify` returns a plain
     (non-`*Failure`) error before reading.
   - Header checks run in this order: strict OCJ parse; `format` key present and a string (otherwise
     malformed, i.e. format failure); `format` value equals `orvex/audit-history/v1` (otherwise
     unsupported at header); then the exact key set and field syntax.
   - Line 2 checks run in this order: strict parse; `chain` key present with another value → unsupported
     at 0; then kind and key-set checks.
   - A non-`*Failure` error from `Verify` always means I/O.
   - `Report.Descriptor` is zero in `ModeChain`. `Report.Checkpoints` is empty in `ModeChain`.
9. **`replay` package.** `Run` calls `visit(0, initial)` before record 1. For `TestFailClosedDecideIsMismatch`, wrap
   the crash adapter in a test type that embeds `game.Adapter` and overrides `Decide` to return
   `crashv1.ErrInvalidState`. That needs no extra test game or codec.
10. **`crashctl` argument handling** (spec §7 plus these rules).
    - Top level:
      - parse flags;
      - with `-version` and no arguments, print the version as today;
      - with `-version` and any argument, `unexpected arguments`, exit 2 (as today);
      - no arguments, usage on stderr, exit 2;
      - first argument `audit`, the audit dispatcher;
      - any other first argument, `crashctl: unexpected arguments: [...]`, exit 2 (keeps the existing
        `TestRun` "positional" row).
    - `audit` with no subcommand or an unknown one: usage, exit 2.
    - Each subcommand has its own `flag.FlagSet` (`ContinueOnError`, output to stderr) with the single
      flag `-expect-head`.
    - Flags come before `FILE` (Go `flag` semantics): `audit verify FILE -expect-head X` has three
      positionals, so exit 2.
    - `-h` or `-help` on a subcommand prints its usage and exits 0, mirroring the top-level `-h`.
    - If `-expect-head` was set (detect it with `fs.Visit`), its value must be exactly 64 lowercase hex
      characters, otherwise exit 2. An explicitly empty value is also exit 2.
    - Exactly one positional `FILE`; `-` means stdin.
    - Opening the file fails → exit 4 (`crashctl: audit <sub>: <err>` on stderr).
    - The registry is built with `game.NewRegistry(crashv1.Adapter())`. An error there prints and
      exits 1 (it cannot happen).
    - `run(args []string, stdin io.Reader, stdout, stderr io.Writer) int`; `main` passes `os.Stdin`.
    - Success output: buffer the report and write it once. A write error is exit 4.
    - The first line is `"verifier " + buildinfo.String(name)`.
11. **Spec §12 open items**, as written and approved: `configSecret` stays required in v1, and the
    worked example keeps `maxBets = 3`.
12. **CI:** no change. The `test` job already runs `make golden-check` after `make test`, and the
    Makefile change makes it cover both oracles.

## 3. Steps

Order: 0 → 1 → 2 → 3 → (4 and 5 in parallel) → 6 → 7 → 8 → 9 → 10. Run nothing mid-flight except the
package being built (`go test ./internal/canonjson/` and so on). Run the full gates once at step 10.
Rules for new non-test Go files are in spec §10 (goimports grouping with the local prefix; lowercase,
package-prefixed error strings; no floats; no package state besides sentinels and constants).

### Step 0: mark the spec approved (`docs/specs/8bebb70f_canonical-codec-audit-cli.md`)

- Replace the two-line status paragraph (lines 3–4, starting `Status: **PROPOSED, awaiting engineer
  approval.**`) with:

  ```
  Status: **APPROVED** on 2026-10-14. The engineer approved decisions A-1 to A-20 as written, with no
  edits, and accepted every point in section 2, including the ADR-007 layout deviation and the LBT-6
  contract change. Section 0 described the planning run and no longer applies; implementation follows
  section 10, as planned in `docs/specs/8bebb70f_audit-codec-implementation.md`.
  ```

- In the section 3 table, change all 20 `| [ ] |` cells to `| [x] |`.
- Edit nothing else in the spec. Its `specs/...` references are a historical record; PR #5 left such
  references in older specs too.

### Step 1: `internal/canonjson` (spec §10 step 1, A-1 to A-3, §5.1)

Files: `canonjson.go` (package doc cites A-1/A-2 and RFC 8785), `canonjson_test.go`, `fuzz_test.go`.

Tests to add:
- `TestMarshal`
- `TestMarshalRejects`
- `TestUnmarshalRoundTrip`
- `TestUnmarshalRejects`
- `TestInts`
- `TestHexUUID`
- `FuzzUnmarshal`
- `FuzzStringRoundTrip`

The case lists are in spec §10 step 1.

### Step 2: `internal/game` codec contract (A-6)

- `codec.go`: the typed `Codec[S, C, E, Cfg]` interface and the three sentinels, with doc comments
  exactly as in spec A-6.
- `registry.go`:
  - `Adapter` gains `EncodeConfig`, `DecodeConfig`, `EncodeCommand`, `DecodeCommand`, `EncodeEvent`,
    `DecodeEvent`, `EncodeState` and `RejectionCode` (erased signatures in A-6);
  - `Adapt(d, g, c)`, with "c must be non-nil" documented next to g's rule;
  - `adapter` gets a `c` field and the erased methods (§2 item 4);
  - the `Adapter` doc mentions that codec errors pass through unwrapped.
- `contract.go`: §2 item 5.
- Test edits:
  - `registry_test.go`: every `game.Adapt(...)` call gains `tallyCodec{}`. That is lines 14, 84, 134,
    153 (`sloppy{}` shares tally's types), 171, 226 and 241.
  - `contract_test.go`: the `Adapt` call at line 138 gains `tallyCodec{}`; `TestNoIOImports` adds
    `"codec.go"` to its must-check list.
- New `codec_test.go` (package `game_test`):
  - `tallyCodec` implements `game.Codec[tallyState, tallyCommand, tallyEvent, tallyConfig]` with
    `canonjson`. Keep it minimal: commands, events, config with an empty secret object `{}`, state
    with `Scores` as an object, and `RejectionCode` mapping `errClosed` → `closed`, `errInvalidPoints` →
    `invalid_points`, `errOverLimit` → `over_limit`, `errUnknownCommand` → `unknown_command`
    (matched with `errors.Is`), and returning `false` for every other error.
  - Tests to add: `TestAdapterCodecRejectsWrongTypes`, `TestAdapterCodecPassThrough`,
    `TestAdapterRejectionCode`.

All existing `internal/game` tests must pass unchanged in behavior.

### Step 3: crash/v1 codec (A-4, §5.3)

- New `codec.go`: `type Codec struct{}` implementing `game.Codec[State, Command, Event, Config]`.
  - Key tables and enum strings exactly as spec §5.3; zero values always present (A-3).
  - `EncodeConfig` writes `commitment = Commitment(cfg.Seed)`.
  - `DecodeConfig(roundID, public, secret)` checks `VerifyCommitment` and wraps
    `game.ErrRevealMismatch` without formatting seed bytes.
  - Malformed input wraps `game.ErrMalformed`; unencodable values wrap `game.ErrUnencodable`
    (A-4 list).
  - `RejectionCode(err)` returns `string(r), true` when `RejectionReason(err)` is ok.
  - The doc comment cites the spec path and states that the codec is frozen with crash/v1 (A-20).
- `round.go`: `Adapter()` returns `game.Adapt(Descriptor(), Rules{}, Codec{})`. No other edit.
- `params_test.go`: the §2 item 2 allowlist and must-check edits only.
- `round_test.go` `TestAdapterRegistration`: also assert that
  `Adapter().EncodeEvent(RoundStarted{StartedAtUnixMicro: 1760000000000000})` returns exactly
  `{"startedAtUnixMicro":"1760000000000000","type":"RoundStarted"}`, and that
  `Adapter().EncodeEvent(struct{}{})` fails with `game.ErrTypeMismatch`.
- New `codec_test.go` (package `v1`). Tests to add:
  - `TestCodecVectors`
  - `TestCodecRoundTrip`
  - `TestCodecRejectsMalformed`
  - `TestCodecRejectsUnencodable`
  - `TestDecodeConfigRevealMismatch`
  - `TestEncodeStateCoversEveryField`: it also pins `reflect.TypeOf(State{}).NumField() == 17` and
    `reflect.TypeOf(Bet{}).NumField() == 8`, so a new field forces a deliberate test and
    `stateVersion` decision.
  - `TestRejectionCode`

  `TestCodecVectors` uses the spec §5.6 literals typed into the test, with a local worked config:
  round `0190f3a8-7b3c-7d4e-8f00-000000000001`, seed `00..1f`, min 1, max 1,000,000,000,
  `maxBets` 3. Reuse `workedRoundID` and `workedSeed()` from `round_test.go`. Do not use
  `testConfig`, which sets `MaxBets` 100.
- New `codec_fuzz_test.go`: `FuzzDecodeCommand`, `FuzzDecodeEvent`. Seed both with every §5.6 payload
  line.

### Step 4: `internal/audit/chain` (A-7 to A-10, §5.2, §5.4)

- `chain.go`: the API in spec §10 step 4, plus `ErrUnsupportedChain` (§2 item 7). The package doc
  cites the spec and ADR-001.
- `chain_test.go` (package `chain_test`). Tests to add:
  - `TestWorkedExampleGenesis`
  - `TestWorkedExampleRecord1`
  - `TestWorkedExampleInitialStateHash`, which uses the crash `Codec{}.EncodeState` of
    `Rules{}.InitialState(worked config)`.
  - `TestDecodeRoundTrip`
  - `TestDecodeRejects`
  - `TestDecodeUnsupportedChain`
  - `TestStateHashBindsVersion`

  The literal bytes and hashes are spec §5.6: `hash_0 = cfdd0537…c284`, `hash_1 = 6cbb649e…f061`,
  `stateHash_0 = 1c98ea46…e7e7` (use the full values).

### Step 5: audit oracle and fixtures (A-18, A-19, §8)

Write `internal/audit/verification/testdata/golden/generate.py` following spec §8.1, all ten rules.
Suggested structure:

1. Module docstring: purpose, the independence rules (§8.1), usage, and the A-9 third-tool command
   with its expected output `cfdd0537…c284` (full value).
2. Constants typed from the spec:
   - tags and versions;
   - `T0`;
   - the four round IDs, seeds, configs and players (including `P3 = "zoë \"☃\"\\\u0001😀"`);
   - the command-ID pattern `0190f3a8-7b3c-7d4e-8f0<k>-<12 hex of s>`;
   - `engineBuild` `"golden"`; `createdAtUnixMicro = T0 − 30,000,000`;
   - `TABLE_PATH = HERE.parents[3] / "game/crash/v1/testdata/golden/multiplier_ticks.json"`.
3. `ocj(obj) -> bytes`: a validator (dict, list and str only; key regex; no surrogates; depth ≤ 32)
   followed by `json.dumps(..., ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode()`.
4. `link_hash(line_bytes)` and `state_hash(game, state_version, state_obj)` per §5.2.
5. A crash/v1 model:
   - `outcome(seed, round_text)` (HMAC, `r`, `raw`, clamp, `bisect_left`, `D`);
   - `decide(state, cmd, t)` following the check order in lifecycle spec §5.4 (`specs/712614a2…`
     here, `docs/specs/…` on `main`), returning events or a reason;
   - `evolve(state, event)`; state dicts with exactly the §5.3 keys.
6. A scenario runner. Each step carries its expected result from the §8.2 tables, and the model must
   agree or the script exits non-zero. It builds the genesis and command record dicts, global event
   numbering, and state hashes at the listed checkpoints.
7. `history_bytes(header, records, raw_overrides)` writes JSONL, every line ending in `\n`. A
   `rechain(records, raw)` helper recomputes `prevHash` for records 1..N in order from the actual line
   bytes (raw overrides included) and returns the head. `renumber(records)` resets `sequence` to the
   list index and `eventSequence` to a running counter from 1.
8. Tamper builders R1–R26, exactly as spec §8.4:
   - start from a deep copy of the worked example;
   - call `renumber` where the table says renumber (R1, R2, R6, R13, R26), then the mutation;
   - R11 applies its gap after renumbering;
   - then `rechain`, except R9's genesis `prevHash` stays as mutated;
   - R19 and R20 supply raw line bytes for record 5 that are rebuilt after its `prevHash` is set;
   - R26 updates only the final checkpoint's sequence (22 → 21).
9. `manifest.json` per spec §8.3:
   - histories with counts, head and checkpoints;
   - tamper entries with `checks` exactly as the §8.4 "expected" column, where `sequence` is a decimal
     string or `"header"`, and `stage` and `sequence` are `null` for exit 0;
   - R26's second check has `args` `["-expect-head", <worked-example head>]`.
10. Self-checks (§8.1 item 7):
    - every §5.6 literal, including the P3 hex and the three hashes;
    - the outcomes in §8.2 (`C`, `n_c`, `D` for all four rounds and the four commitments);
    - `M(500) = 13498`, `M(940) = 17576`, `M(2000) = 33201`;
    - the §8.2 counts: worked 23/22/8/14/9, instant 6 records and 5 events, empty 4/3, in-progress
      4/3.
11. `--write` and `--check`:
    - `--write` writes `worked-example.jsonl`, `instant-crash.jsonl`, `empty-round.jsonl`,
      `in-progress.jsonl`, `tamper/<name>.jsonl` (26 files, names as in §8.4) and `manifest.json`;
    - `--check` regenerates in memory, byte-compares every file, and also fails if `tamper/`
      contains a file the oracle does not produce;
    - exit status 1 on any difference; self-check failures also exit non-zero.

Run `python3 internal/audit/verification/testdata/golden/generate.py --write`, then `--check`.
Spot-check the A-9 `sha256sum` command on the generated worked example: it must print the full
`hash_0`. Never edit the fixtures by hand, and never regenerate them from Go output.

### Step 6: `internal/audit/replay` (A-13, A-15)

`replay.go` follows the API in spec §10 step 6. Tests to add (`replay_test.go`):
- `TestRunReproducesRound`
- `TestDecisionMismatch`
- `TestFailClosedDecideIsMismatch` (§2 item 9)
- `TestEvolveErrorIsStateMismatch`

The records are built in Go from crash/v1 `Decide`, `Evolve` and `Codec`.

### Step 7: `internal/audit/verification` (A-11, A-13, A-14, §6)

`verification.go` follows spec §10 step 7's API, implements §6 exactly, and applies §2 item 8. Tests
to add:
- `TestGoldenHistories`
- `TestTamperFixtures`
- `TestByteMutations` (B1–B16, one subtest per case, named as in spec §8.5)
- `TestEverySingleByteFlipFails` (B17; skip under `testing.Short()`)
- `TestExpectHead`
- `TestLineLimit`
- `TestChainModeNeedsNoRegistry`
- `TestReadError`
- `FuzzSingleByteFlip` (in `fuzz_test.go`)

Expected outcomes come from `testdata/golden/manifest.json` (decode it with `encoding/json` in the
test) and from spec §8.5. Map manifest `exit` to error shape:
- 0 → nil error;
- 1 → `*Failure` with `!Unsupported`;
- 3 → `*Failure` with `Unsupported`.

`stage` and `sequence` must match exactly, with `"header"` meaning `Sequence == -1`.

`TestLineLimit` feeds a lazy `io.Reader` that yields `MaxLineBytes + 1` bytes of `'a'` without
allocating them in the test, and expects a format failure at `header`.

### Step 8: `cmd/crashctl` (A-16, §7)

`main.go` implements spec §7 and §2 item 10. `main_test.go` changes:
- Update `TestRun`: the "no args" row now expects exit 2 and stderr containing `usage: crashctl`. Every
  other row is unchanged. Calls pass `strings.NewReader("")` as stdin.
- Add:
  - `TestAuditGolden`
  - `TestAuditReplayStateHash`
  - `TestAuditFailures`
  - `TestAuditUsage`, which also covers the `audit verify -h` → 0 row and the flag-after-FILE → 2 row
  - `TestAuditIO`
- `TestRunWriteError` stays.

Fixture paths: `../../internal/audit/verification/testdata/golden/...`. Expected heads and state hashes
come from `manifest.json`. B1 is applied to a copy written under `t.TempDir()`.

### Step 9: Makefile, README, AGENTS.md

- `Makefile`:
  - keep `GOLDEN_GEN`; add
    `AUDIT_GOLDEN_GEN := internal/audit/verification/testdata/golden/generate.py`;
  - the `golden-check` recipe runs `$(PYTHON) $(GOLDEN_GEN) --check` then
    `$(PYTHON) $(AUDIT_GOLDEN_GEN) --check`;
  - the comment above the variables says both oracles are independent Python 3.11+ standard-library
    scripts (Crash v1 math vectors, audit golden histories).
- `README.md`:
  - Status: add "the canonical codec, hash chain and audit CLI
    ([LBT-9](https://linear.app/lbtworks/issue/LBT-9/build-canonical-event-codec-and-audit-replay-cli))".
    Keep "No runtime, persistence or transport exists yet."
  - Layout:
    - `cmd/crashctl`: "replay/audit CLI: `crashctl audit verify|replay|chain` (LBT-9)";
    - add `internal/canonjson`: "Orvex Canonical JSON (RFC 8785 subset): encoder and strict decoder
      (LBT-9)";
    - add `internal/audit/chain`: "chain records, link hash and state hash";
    - add `internal/audit/replay`: "state replay and decision replay through the rules registry";
    - add `internal/audit/verification`: "history reader, staged verifier and golden histories";
    - the `internal/game` bullet adds "canonical codec contract";
    - the `internal/game/crash/v1` bullet adds "canonical codec (ADR-001, LBT-9)".
  - Development: the `make golden-check` bullet names both oracles.
- `AGENTS.md`: edit only the `make golden-check` bullet: "regenerate the Crash v1 golden vectors and
  the audit golden histories with the independent Python oracles (Python 3.11+, standard library only)
  and fail if they differ from the committed files. Never regenerate vectors or fixtures from Go
  output."
- `.github/workflows/ci.yml`: unchanged (§2 item 12).

### Step 10: gates

Run once, at the end. Each must exit 0:

```sh
make test
make lint
go vet ./...
make build
make golden-check
./bin/crashctl audit verify internal/audit/verification/testdata/golden/worked-example.jsonl
./bin/crashctl audit replay internal/audit/verification/testdata/golden/in-progress.jsonl
./bin/crashctl audit chain - < internal/audit/verification/testdata/golden/empty-round.jsonl
{ printf '%s' 'orvex/audit/v1/record:'; sed -n 2p internal/audit/verification/testdata/golden/worked-example.jsonl | tr -d '\n'; } | sha256sum
# prints cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284
```

Exit-code checks:
- `./bin/crashctl audit verify internal/audit/verification/testdata/golden/tamper/payout-edited.jsonl`
  → 1;
- `./bin/crashctl audit verify internal/audit/verification/testdata/golden/tamper/rules-version-2.jsonl`
  → 3;
- `./bin/crashctl audit verify` (no FILE) → 2;
- `./bin/crashctl audit verify /nonexistent` → 4.

Integrity checks:
- `git diff --exit-code 6ec9f16 -- go.mod go.sum internal/game/crash/v1/params.go internal/game/crash/v1/curve.go internal/game/crash/v1/payout.go internal/game/crash/v1/fairness.go internal/game/crash/v1/testdata specs app_docs .github`
  → 0 (no change);
- `go mod tidy && git diff --exit-code go.mod go.sum` → 0.

If Go and the oracle disagree, find which side contradicts the spec and fix that side. Never edit a
§5.6 literal or a fixture to match Go.

## 4. Files touched

| File | Action |
|---|---|
| `docs/specs/8bebb70f_canonical-codec-audit-cli.md` | status line and 20 checkboxes (step 0) |
| `internal/canonjson/{canonjson.go,canonjson_test.go,fuzz_test.go}` | new |
| `internal/game/codec.go` | new |
| `internal/game/registry.go`, `contract.go` | edit |
| `internal/game/codec_test.go` | new (includes `tallyCodec`; deviation §2 item 1) |
| `internal/game/registry_test.go`, `contract_test.go` | edit (`Adapt` call sites; `TestNoIOImports` must-check list) |
| `internal/game/crash/v1/codec.go`, `codec_test.go`, `codec_fuzz_test.go` | new |
| `internal/game/crash/v1/round.go` | edit `Adapter()` only |
| `internal/game/crash/v1/params_test.go` | edit `TestNoIOImports` only |
| `internal/game/crash/v1/round_test.go` | edit `TestAdapterRegistration` only |
| `internal/audit/chain/{chain.go,chain_test.go}` | new |
| `internal/audit/replay/{replay.go,replay_test.go}` | new |
| `internal/audit/verification/{verification.go,verification_test.go,fuzz_test.go}` | new |
| `internal/audit/verification/testdata/golden/generate.py` | new |
| `internal/audit/verification/testdata/golden/{worked-example,instant-crash,empty-round,in-progress}.jsonl`, `tamper/*.jsonl` (26), `manifest.json` | new, written by `generate.py --write` |
| `cmd/crashctl/main.go`, `main_test.go` | edit |
| `Makefile`, `README.md`, `AGENTS.md` | edit (step 9) |

Not touched: `go.mod`, `go.sum`, `.github/`, `docs/adr/`, `specs/`, `app_docs/`, the frozen crash/v1
files and testdata, `commands.go`, `events.go`, `decide.go`, `evolve.go`, `projection.go`.

## 5. Acceptance → tests

| Acceptance | Tests |
|---|---|
| Golden history verifies | `TestGoldenHistories`, `TestAuditGolden` |
| Tampered history fails | `TestTamperFixtures` (R3–R25), `TestByteMutations` (B1, B2, B6–B16), `TestEverySingleByteFlipFails`, `FuzzSingleByteFlip`, `TestAuditFailures` |
| Reordered history fails | `TestTamperFixtures` (R1, R2), `TestByteMutations` (B3, B4, B5) |
| Exported replay reconstructs identical state hash | `TestGoldenHistories` (checkpoint hashes equal the oracle's), `TestAuditReplayStateHash`, `TestWorkedExampleInitialStateHash` |
| Fail closed on unknown artifacts | R14–R18, B11; `TestAuditFailures` (exit 3) |
| Commitment/reveal | R7, R8, B8, `TestDecodeConfigRevealMismatch` |
| Anchor limitation | R26 (exit 0 without, exit 1 with `-expect-head`), `TestExpectHead` |
| CLI contract (A-16) | `TestRun`, `TestAuditUsage`, `TestAuditIO`, `TestAuditGolden`, `TestAuditFailures` |
| Codec contract (A-6) | `TestAdapterCodecRejectsWrongTypes`, `TestAdapterCodecPassThrough`, `TestAdapterRejectionCode`, `TestAdapterRegistration` |
| Canonical encoding (A-1 to A-4) | step 1 and step 3 tests and fuzzers; `TestEncodeStateCoversEveryField` |

## 6. PR description must include

- The spec's section 2 items 1–10 (flagged ADR points) and this plan's §2 item 1 deviation.
- The justification that no new dependency was added.
- That the crash/v1 math files and vectors are unchanged.

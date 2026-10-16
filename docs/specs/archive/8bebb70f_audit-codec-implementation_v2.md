# Implement LBT-9 (v2, continuation): canonical codec, hash chain, audit replay and `crashctl audit`

- Linear: [LBT-9](https://linear.app/lbtworks/issue/LBT-9/build-canonical-event-codec-and-audit-replay-cli)
  (M1 · Deterministic Core; blocks LBT-10).
- **Normative spec: `docs/specs/8bebb70f_canonical-codec-audit-cli.md` ("the spec").** The engineer
  approved A-1 to A-20 as written, with no edits, and accepted every point in its section 2. That
  includes the ADR-007 layout deviation for `internal/canonjson/` and `internal/audit/chain/`, and the
  LBT-6 contract change.
- **This plan supersedes `docs/specs/8bebb70f_audit-codec-implementation.md` ("plan v1").** A
  previous build run carried out part of plan v1 and stopped with the tree not compiling. This plan
  records what exists, what was reviewed, and what remains. It also adds clarifications found while
  checking every tamper expectation in the spec against its section 6 algorithm (§2 below).
- Read the spec in full before starting. Where this plan and the spec differ, this plan wins only for
  the items marked **deviation** in §2. Everything else clarifies or orders the spec's section 10.
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
4. The crash/v1 math, its parameters and its golden vectors are byte-identical: `params.go`,
   `curve.go`, `payout.go`, `fairness.go`, and everything under `internal/game/crash/v1/testdata/`.
5. `go.mod` and `go.sum` are unchanged (no new module).
6. The spec is marked approved (already done; one path edit remains, §3 step 0).

Out of scope: everything in the spec's section 12; `make audit` (deferred by spec §2 item 9); the
ADR-004 amendment (spec §2 item 10); write-ups under `docs/changes/`.

## 1. Workspace state (verified while planning this version)

### 1.1 Base

- `HEAD` is `ea3f8b0`, which commits plan v1 and the spec, on top of `6ec9f16`. **The worktree does not
  contain PR #5** (`4da99af` on `main`).
- PR #5 moved `specs/*` to `docs/specs/*` and `app_docs/*` to `docs/changes/*`. It also edited one
  comment line in each of three files: `params.go` line 2, `round.go` line 172 here (the `Rules` doc
  comment) and the LBT-7 `generate.py` docstring.
- In this worktree the older specs are still at `specs/` (same names). Read them there, or with
  `git show main:docs/specs/<name>`. The lifecycle spec is `specs/712614a2_crash-lifecycle-rules.md`,
  the math spec `specs/e96d124d_crash-v1-math-spec.md`.
- **Do not move or edit `specs/`, `app_docs/`, or those three comment lines.** PR #5 already did that
  on `main`, and doing it again would conflict at merge.
- New code comments cite final paths: `docs/specs/8bebb70f_canonical-codec-audit-cli.md`,
  `docs/specs/e96d124d_crash-v1-math-spec.md`, and so on.
- Toolchain: go1.26.5, golangci-lint v2.14.0, Python 3.14.6 locally. The oracle must run on 3.11+;
  CI's `ubuntu-latest` has 3.12.
- Never run `git commit`, `push`, `merge`, `rebase` or `checkout`.

### 1.2 Work already in the tree (uncommitted): keep it

| Item | State | Review result |
|---|---|---|
| Step 0: spec status line and 20 `[x]` checkboxes | done | correct; one path edit left (§3 step 0) |
| Step 1: `internal/canonjson/{canonjson.go,canonjson_test.go,fuzz_test.go}` | done | Reviewed against spec A-1 to A-3 and §5.1: grammar, escapes, strict parser, depth, int/uint/version/hex/UUID helpers, `Fields`. `go test -race` and `go vet` pass. Tests present: `TestMarshal`, `TestMarshalRejects`, `TestUnmarshalRoundTrip`, `TestUnmarshalRejects`, `TestInts`, `TestHexUUID`, `TestAccessors`, `FuzzUnmarshal`, `FuzzStringRoundTrip`. Do not rewrite it. |
| Step 2: `internal/game/codec.go` | done | matches spec A-6 |
| Step 2: `internal/game/registry.go` | done | `Adapter` has the 8 erased methods; `Adapt(d, g, c)`; type mismatch is checked before the codec runs; codec errors pass through unwrapped |
| Step 2: `internal/game/contract.go` | done | package doc and the `Decide` comment match A-6 |
| Step 2: `internal/game/registry_test.go`, `contract_test.go` | done | every `Adapt` call passes `tallyCodec{}`; `codec.go` is on `TestNoIOImports`' must-check list |
| Step 2: `internal/game/codec_test.go` (`tallyCodec` and three tests) | **missing** | the `game_test` package does not compile (`undefined: tallyCodec`) |
| Step 3 onwards | **not started** | `go build ./...` fails: `crash/v1/round.go:34` still calls the two-argument `game.Adapt` |

Everything from step 2's `codec_test.go` onward is still to do.

## 2. Clarifications and deviations

None changes an approved decision. Items 1–12 carry over from plan v1. Items 13–17 are new: they come
from checking each R and B case against spec §6.

1. **Deviation (test-file location only): `tallyCodec` lives in `internal/game/codec_test.go`, not
   `sample_game_test.go`.** `contract_test.go`'s `TestNoIOImports` checks `sample_game_test.go`'s
   imports against a pure-code allowlist that must not gain `internal/canonjson`. `codec_test.go` is
   not checked by that test.
2. **crash/v1 `codec.go` imports exactly:** `fmt` (`fmt.Errorf` only),
   `github.com/juano-morello/orvex/internal/canonjson`, `github.com/juano-morello/orvex/internal/game`,
   and `errors` if needed.
   - No package-level `var`: enum mappings are `switch` statements, and sentinels come from `game`.
     No floats.
   - crash/v1's `TestNoIOImports` (`params_test.go`) gains the `canonjson` import path in `allowed`
     and `"codec.go"` in its must-check list. Nothing else in `params_test.go` changes.
3. **`game.Adapt` inference.** `game.Adapt(Descriptor(), Rules{}, Codec{})` should infer its type
   arguments. If it does not, write them explicitly:
   `game.Adapt[State, Command, Event, Config](Descriptor(), Rules{}, Codec{})`.
4. **Erased codec methods** (already implemented):
   - Interface method sets include pointer types: `*v1.PlaceBet` satisfies `v1.Command`, so the
     adapter passes it through.
   - The crash codec's type switch covers value types only, so it rejects a pointer with
     `game.ErrUnencodable`.
5. **`contract.go`**: done; no further edits.
6. **`canonjson`**: done (§1.2).
   - Codecs wrap a canonjson error as `fmt.Errorf("%w: ...: %w", game.ErrMalformed, err)`, using two
     `%w`, so callers can match either sentinel.
7. **`chain` package.**
   - Declare `var ErrUnsupportedChain = errors.New("chain: unsupported chain version")`. `Decode`
     wraps it when a `chain` key holds any other value.
   - Payload sub-objects (`config`, `command`, `event`) are returned as their `canonjson.Marshal`
     bytes. Because parsing is strict, these equal the original sub-slices.
   - `EncodeGenesis` and `EncodeCommand` validate their inputs with `Decode`'s syntax rules and return
     an error rather than writing an undecodable record. The rules are: game ID regex
     `^[a-z][a-z0-9-]{0,31}$`, non-zero versions, `engineBuild` of 1–128 bytes in 0x21–0x7E, reason
     regex `^[a-z][a-z0-9_]*$`, payloads that are OCJ objects, and an accepted result carrying no
     reason.
   - `StateHash` parses the codec's state bytes with `canonjson.Unmarshal`, embeds them as the
     `state` member, and marshals the wrapper document.
8. **`verification` package.**
   - `const MaxLineBytes = 64 << 20`. The limit counts line content without the `\n`, so a line of
     exactly `MaxLineBytes` is allowed.
   - `Options.Registry == nil` is allowed only in `ModeChain`. Otherwise `Verify` returns a plain
     (non-`*Failure`) error before reading.
   - Header checks, in order:
     1. strict OCJ parse;
     2. the `format` key is present and is a string (otherwise a format failure);
     3. its value equals `orvex/audit-history/v1` (otherwise unsupported at header);
     4. exact key set and field syntax.
   - Line 2 checks, in order:
     1. strict parse;
     2. a `chain` key present with another value → unsupported at 0;
     3. `kind` and key-set checks.
   - A non-`*Failure` error from `Verify` always means I/O.
   - In `ModeChain`, `Report.Descriptor` is zero and `Report.Checkpoints` is empty.
   - Classifying `DecodeConfig` errors: `errors.Is(err, game.ErrRevealMismatch)` → stage `fairness`;
     any other error → stage `decode`, both at sequence 0.
9. **`replay` package.**
   - `Run` calls `visit(0, initial)` before record 1.
   - For `TestFailClosedDecideIsMismatch`, wrap the crash adapter in a test type that embeds
     `game.Adapter` and overrides `Decide` to return `v1.ErrInvalidState`.
10. **`crashctl` argument handling** (spec §7 plus these rules).
    - Top level:
      - parse flags;
      - with `-version` and no arguments, print the version as today;
      - with `-version` and any argument, `unexpected arguments`, exit 2;
      - with no arguments, usage on stderr, exit 2;
      - first argument `audit`, the audit dispatcher;
      - any other first argument, `crashctl: unexpected arguments: [...]`, exit 2. This keeps the
        "positional" row of `TestRun`.
    - `audit` with no subcommand or an unknown one: usage, exit 2.
    - Each subcommand has its own `flag.FlagSet` (`ContinueOnError`, output to stderr) with the single
      flag `-expect-head`.
    - Flags come before `FILE` (Go `flag` semantics): `audit verify FILE -expect-head X` leaves three
      positionals, so it exits 2.
    - `-h` or `-help` on a subcommand prints its usage and exits 0.
    - If `-expect-head` was set (detect it with `fs.Visit`), its value must be exactly 64 lowercase hex
      characters, otherwise exit 2. An explicitly empty value is exit 2 as well.
    - Exactly one positional `FILE`; `-` means stdin.
    - The file cannot be opened → exit 4, with `crashctl: audit <sub>: <err>` on stderr.
    - The registry is `game.NewRegistry(v1.Adapter())`. If building it fails, print the error and exit
      1 (this cannot happen).
    - `run(args []string, stdin io.Reader, stdout, stderr io.Writer) int`; `main` passes `os.Stdin`.
    - Success: buffer the report and write it once. A write error is exit 4.
    - The first report line is `"verifier " + buildinfo.String(name)`.
    - Failure: nothing on stdout; one stderr line,
      `crashctl: audit <sub>: <stage> failed|unsupported at sequence <n>|header: <detail>`, with exit 1
      or 3. Any non-`*Failure` error from `Verify` is exit 4.
11. **Spec §12 open items**, as approved: `configSecret` stays required in v1, and the worked example
    keeps `maxBets = 3`.
12. **CI: no change.** The `test` job already runs `make golden-check`, which will cover both oracles.
13. **Every R1–R26 and B1–B16 expectation in spec §8.4 and §8.5 was traced through the spec §6
    algorithm while planning, and all are consistent.** Implement §6 literally. If a fixture produces
    a different stage or sequence, the implementation deviates from §6. Do not change the
    expectation.
    - Chain-stage checks run per position, in the §6 order, and the first failure wins: `sequence`,
      `kind`, `roundId`, `prevHash`, `commandId` uniqueness, rejected-with-events, `eventSequence`.
    - Only after every position passes: header `head`, then `-expect-head`, both failing at `N`; then
      the checkpoint structure, failing at header.
14. **Line-count failures (B6, B14, B16).** Lines are processed in order.
    - Input with no line at all → format failure at header.
    - A valid header and no record line → format failure at sequence 0.
    - The last line without a terminating `\n` → format failure at that line's position (B14: 22).
15. **B-case mutation helpers** (Go tests, applied to the bytes in memory).
    - Mutations that must stay well-formed replace a hex digit with a different lowercase hex digit:
      `'0'` becomes `'1'`, anything else becomes `'0'`. This applies to B7 (`head`), B8 (last digit of
      `configSecret.seed`), B9 and B10 (first digit of the `stateHash`).
    - B1 changes the payout `"134"` to `"135"` in record 12's line.
    - B2 changes `"frank"` to `"frans"` in record 22's line.
    - B12 replaces the first `":"` with `": "` in record 5's line.
    - B15 inserts `,"note":"x"` immediately before the header line's closing `}`. The header keys
      are `checkpoints`, `configSecret`, `format`, `head`, and `note` sorts after `head`, so the line
      stays canonical OCJ and only the key-set check fails.
    - Find lines by position: line `i+2` is sequence `i`. Never search the whole file for a substring.
16. **`TestLineLimit` must tell a cap from no cap.**
    - An oversized line also fails JSON parsing, so the test feeds a reader that yields `'a'` bytes
      endlessly. Once it has produced `MaxLineBytes + 1<<20` bytes, the reader returns a sentinel
      error.
    - The test expects a `*Failure` with stage `format` at header, whose message mentions the line
      limit.
    - The reader's sentinel error must never come back, which proves `Verify` stopped reading near the
      cap.
17. **`TestAuditFailures` walks the whole tamper manifest through the CLI.** For every manifest
    tamper check it calls `run` with `audit <command> <args...> <file>` and asserts:
    - the exit code;
    - for non-zero exits, stderr containing `<stage> failed at sequence <n>` or
      `<stage> unsupported at sequence <n>` (with `header` in place of `sequence <n>` where that
      applies).

    It also runs B1 through a temp file. This gives CLI exit-code coverage for all 26 fixtures.

## 3. Steps

Order: 0 → finish 2 → 3 → (4 and 5 in parallel) → 6 → 7 → 8 → 9 → 10.
- While building, run only the package in hand (`go test ./internal/game/...`, and so on). Run the full
  gates once, at step 10.
- Rules for new non-test Go files are in spec §10: goimports grouping with the local prefix; lowercase,
  package-prefixed error strings; no floats; no package state besides sentinels and constants.
- If several agents work in parallel: steps 4 (Go `chain`) and 5 (Python oracle) touch disjoint files
  and share only the spec's §5 formats and §5.6 literals. One owner integrates and runs step 10.

### Step 0: spec status path (one edit)

In `docs/specs/8bebb70f_canonical-codec-audit-cli.md` line 6, change
`docs/specs/8bebb70f_audit-codec-implementation.md` to
`docs/specs/8bebb70f_audit-codec-implementation_v2.md`. Change nothing else in the spec: its status
paragraph and checkboxes are already done, and its `specs/...` references are a historical record.

### Step 1: `internal/canonjson`: done

No changes. Step 10 re-runs its tests.

### Step 2 (finish): `internal/game/codec_test.go`

New file, package `game_test`.

- `tallyCodec` implements `game.Codec[tallyState, tallyCommand, tallyEvent, tallyConfig]` with
  `canonjson`. Keep it minimal:
  - commands: `score` is `{"player","points","type":"score"}`, `closeTally` is `{"type":"close"}`;
  - events: `scored` is `{"atUnixMicro","player","points","seq","type":"scored"}`, `closed` is
    `{"atUnixMicro","seq","type":"closed"}`;
  - config: public `{"maxPoints"}`, secret `{}`. `DecodeConfig` ignores `roundID` and fails with
    `game.ErrRevealMismatch` if the secret is not exactly `{}`;
  - state: `{"closed":"true"|"false","history":[...],"maxPoints","scores":{player: points},"total"}`.
    Player names come from tests (`alice`, `bob`, `carol`) and are valid keys;
  - `RejectionCode`, matched with `errors.Is`: `errClosed` → `closed`, `errInvalidPoints` →
    `invalid_points`, `errOverLimit` → `over_limit`, `errUnknownCommand` → `unknown_command`;
    `false` for everything else;
  - `int` fields go through `canonjson.Int(int64(n))` and `ParseInt`. Decode errors wrap
    `game.ErrMalformed`. Encoding an unknown command or event type returns `game.ErrUnencodable`.
- Tests:
  - `TestAdapterCodecRejectsWrongTypes`: `EncodeConfig`, `EncodeCommand`, `EncodeEvent` and
    `EncodeState`, each with `nil` and with a foreign type (`struct{}{}`), return `ErrTypeMismatch`.
    To show the codec was not called, use an adapter built with a spy codec: a `tallyCodec` wrapper
    that counts calls. The count must stay 0.
  - `TestAdapterCodecPassThrough`:
    - every erased method's result equals the typed `tallyCodec` result (bytes, and `reflect.DeepEqual`
      on decoded values);
    - `DecodeCommand` and `DecodeEvent` return the boxed dynamic types `score`, `closeTally`,
      `scored` and `closed`;
    - with a codec stub that returns a sentinel `errStub`, every erased method returns that same error
      value (`err == errStub`) and nil results.
  - `TestAdapterRejectionCode`: the erased `RejectionCode` matches `tallyCodec` for all four tally
    errors, a wrapped `fmt.Errorf("x: %w", errOverLimit)`, and `errors.New("other")` (which gives
    `false`).

`go test -race ./internal/game/` must pass; it cannot pass until step 3 makes the tree compile, so run
it after step 3.

### Step 3: crash/v1 codec (A-4, §5.3)

- New `codec.go`: `type Codec struct{}` implementing `game.Codec[State, Command, Event, Config]`.
  - Key tables and enum strings exactly as spec §5.3. Zero values are always present (A-3).
  - `EncodeConfig` writes `commitment = Commitment(cfg.Seed)` in the public part and `seed` in the
    secret part.
  - `DecodeConfig(roundID, public, secret)`:
    1. strict parse and `Fields` on both parts;
    2. hex, int and every other syntax check; any failure wraps `game.ErrMalformed`;
    3. only then `VerifyCommitment`, failing with `game.ErrRevealMismatch` and without formatting
       seed bytes.

    No range checks: `InitialState` owns those. R22 relies on the syntax checks coming first.
  - Unencodable values wrap `game.ErrUnencodable` (A-4 list).
  - `RejectionCode(err)` returns `string(r), true` when `RejectionReason(err)` is ok.
  - The doc comment cites the spec path and says the codec is frozen with crash/v1 (A-20).
- `round.go`: `Adapter()` returns `game.Adapt(Descriptor(), Rules{}, Codec{})`. Nothing else changes.
- `params_test.go`: only the §2 item 2 edits.
- `round_test.go` `TestAdapterRegistration`: also assert that
  - `Adapter().EncodeEvent(RoundStarted{StartedAtUnixMicro: 1760000000000000})` returns exactly
    `{"startedAtUnixMicro":"1760000000000000","type":"RoundStarted"}`;
  - `Adapter().EncodeEvent(struct{}{})` fails with `game.ErrTypeMismatch`.
- New `codec_test.go` (package `v1`) with these tests (cases in spec §10 step 3):
  - `TestCodecVectors`. Every spec §5.6 payload line, the P3 hex, and the `state` member of the §5.6
    state document. The literals are typed into the test. It uses a local worked config:
    `workedRoundID`, `workedSeed()`, min 1, max 1,000,000,000, `MaxBets` 3. Do not use `testConfig`,
    whose `MaxBets` is 100.
  - `TestCodecRoundTrip`
  - `TestCodecRejectsMalformed`
  - `TestCodecRejectsUnencodable`
  - `TestDecodeConfigRevealMismatch`
  - `TestEncodeStateCoversEveryField`. It also pins `reflect.TypeOf(State{}).NumField() == 17` and
    `reflect.TypeOf(Bet{}).NumField() == 8`.
  - `TestRejectionCode`
- New `codec_fuzz_test.go`: `FuzzDecodeCommand` and `FuzzDecodeEvent`, both seeded with every §5.6
  payload line.

After this step, `go build ./...` and `go test -race ./internal/game/...` pass.

### Step 4: `internal/audit/chain` (A-7 to A-10, §5.2, §5.4)

- `chain.go`: the API in spec §10 step 4, plus `ErrUnsupportedChain` (§2 item 7). The package doc cites
  the spec and ADR-001.
- `chain_test.go` (package `chain_test`):
  - `TestWorkedExampleGenesis`
  - `TestWorkedExampleRecord1`
  - `TestWorkedExampleInitialStateHash`: the crash `Codec{}.EncodeState` of `Rules{}.InitialState` of
    the worked config.
  - `TestDecodeRoundTrip`
  - `TestDecodeRejects`
  - `TestDecodeUnsupportedChain`
  - `TestStateHashBindsVersion`

  Literals (spec §5.6): `hash_0 = cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284`,
  `hash_1 = 6cbb649e00d7ac798be564e2a2f90910ffd69caa9f95207e9ee2f9e72afcf061`,
  `stateHash_0 = 1c98ea460213ad0d5900a92fbf37cc3078fa9e822d2065a65037b667734de7e7`.

### Step 5: audit oracle and fixtures (A-18, A-19, §8)

Write `internal/audit/verification/testdata/golden/generate.py` following spec §8.1 (all ten rules).
Structure:

1. **Module docstring.** Purpose, the independence rules (§8.1), usage, and the A-9 third-tool command
   with its expected output `cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284`.
2. **Constants typed from the spec.**
   - Tags and versions; `T0 = 1760000000000000`.
   - The four round IDs, seeds, configs and players, including `P3 = "zoë \"☃\"\\\u0001😀"`.
   - The command-ID pattern `0190f3a8-7b3c-7d4e-8f0<k>-<s as 12 lowercase hex digits>`.
   - `engineBuild` `"golden"` and `createdAtUnixMicro = T0 - 30_000_000`.
   - `TABLE_PATH = HERE.parents[3] / "game/crash/v1/testdata/golden/multiplier_ticks.json"`.
3. **`ocj(obj) -> bytes`.** A validator first (dict, list and str only; key regex; no surrogates; depth
   ≤ 32), then `json.dumps(obj, ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode("utf-8")`.
4. **Hashes.** `link_hash(line_bytes)` and `state_hash(game, state_version, state_obj)` per §5.2.
5. **crash/v1 model.**
   - `outcome(seed, round_text)`: HMAC, `r`, `raw`, clamp, `bisect_left` on the table, `D`.
   - `decide(state, cmd, t)`: follows the check order of lifecycle spec §5.4 and returns events or a
     reason.
   - `evolve(state, event)`.
   - State dicts have exactly the §5.3 keys.
6. **Scenario runner.**
   - Each step carries its expected result from the §8.2 tables. The model must agree, or the script
     exits non-zero.
   - It builds the genesis and command record dicts, numbers events globally, and computes state
     hashes at the listed checkpoints.
7. **Assembly helpers.**
   - `history_bytes(header, records, raw_overrides)` writes JSONL; every line ends in `\n`.
   - `rechain(records, raw)` recomputes `prevHash` for records 1..N in order, from the actual line
     bytes (raw overrides included), and returns the head.
   - `renumber(records)` resets `sequence` to the list index and `eventSequence` to a running counter
     from 1.
8. **Tamper builders R1–R26, exactly as spec §8.4.**
   - Each starts from a deep copy of the worked example.
   - Call `renumber` where the table says renumber (R1, R2, R6, R13, R26), then apply the mutation,
     then `rechain`.
   - R9 keeps its mutated genesis `prevHash`.
   - R19 and R20 supply raw line bytes for record 5. Build those bytes after its `prevHash` is set.
   - R26 changes only the final checkpoint's sequence (22 → 21); its hash stays the same.
9. **`manifest.json` per spec §8.3.**
   - Histories carry counts, head and checkpoints.
   - Tamper entries carry `checks` exactly as the §8.4 "expected" column. `sequence` is a decimal
     string or `"header"`; `stage` and `sequence` are `null` for exit 0.
   - R26's second check has `args` `["-expect-head", <worked-example head>]`.
10. **Self-checks (§8.1 item 7).**
    - Every §5.6 literal, including the P3 hex and the three hashes.
    - The four outcomes and commitments of §8.2.
    - `M(500) = 13498`, `M(940) = 17576`, `M(2000) = 33201`.
    - The §8.2 counts:
      - worked: 23 records, 22 commands, 8 accepted, 14 rejected, 9 events;
      - instant: 6 records, 5 events;
      - empty: 4 records, 3 events;
      - in-progress: 4 records, 3 events.
11. **`--write` and `--check`.**
    - `--write` writes:
      - `worked-example.jsonl`, `instant-crash.jsonl`, `empty-round.jsonl`, `in-progress.jsonl`;
      - `tamper/<name>.jsonl`: 26 files, named as in §8.4;
      - `manifest.json`.
    - `--check` regenerates in memory, byte-compares every file, and also fails if `tamper/` holds a
      file the oracle does not produce. It exits 1 on any difference. Self-check failures also exit
      non-zero.

Run `python3 internal/audit/verification/testdata/golden/generate.py --write`, then `--check`. Run the
A-9 `sha256sum` command on the generated worked example; it must print `hash_0` exactly. Never edit
fixtures by hand, and never regenerate them from Go output.

### Step 6: `internal/audit/replay` (A-13, A-15)

`replay.go` follows the API in spec §10 step 6. `replay_test.go` adds:
- `TestRunReproducesRound`
- `TestDecisionMismatch`
- `TestFailClosedDecideIsMismatch` (§2 item 9)
- `TestEvolveErrorIsStateMismatch`

Build the test records in Go from crash/v1 `Decide`, `Evolve` and `Codec`.

### Step 7: `internal/audit/verification` (A-11, A-13, A-14, §6)

`verification.go` follows the API in spec §10 step 7, implements §6 exactly, and applies §2 items 8,
13 and 14. The reader uses a `bufio.Reader` (for example `ReadSlice('\n')`, accumulating until `\n`)
and stops as soon as a line passes `MaxLineBytes`.

Tests:
- `TestGoldenHistories`
- `TestTamperFixtures`
- `TestByteMutations`: B1–B16, one subtest per case, named as in spec §8.5; helpers per §2 item 15.
- `TestEverySingleByteFlipFails`: B17; skipped under `testing.Short()`.
- `TestExpectHead`
- `TestLineLimit`: per §2 item 16.
- `TestChainModeNeedsNoRegistry`
- `TestReadError`
- `FuzzSingleByteFlip`, in `fuzz_test.go`.

Expected outcomes come from `testdata/golden/manifest.json`, decoded with `encoding/json` in the test.
Map the manifest's `exit` to the result:
- `0` → no error;
- `1` → a `*Failure` with `!Unsupported`;
- `3` → a `*Failure` with `Unsupported`.

`stage` and `sequence` must match exactly; `"header"` means `Sequence == -1`.

### Step 8: `cmd/crashctl` (A-16, §7)

`main.go` implements spec §7 and §2 item 10. Changes to `main_test.go`:
- Update `TestRun`: the "no args" row now expects exit 2 and stderr containing `usage: crashctl`.
  Every other row stays the same. Calls pass `strings.NewReader("")` as stdin.
- Add:
  - `TestAuditGolden`
  - `TestAuditReplayStateHash`
  - `TestAuditFailures`: per §2 item 17, covering R3 → 1, R15 → 3, R26 with and without
    `-expect-head`, and B1.
  - `TestAuditUsage`: also covers `audit verify -h` → 0 and flag-after-FILE → 2.
  - `TestAuditIO`
- Keep `TestRunWriteError`.

Fixture paths are `../../internal/audit/verification/testdata/golden/...`. Expected heads and state
hashes come from `manifest.json`.

### Step 9: Makefile, README, AGENTS.md

- **`Makefile`.**
  - Keep `GOLDEN_GEN`. Add
    `AUDIT_GOLDEN_GEN := internal/audit/verification/testdata/golden/generate.py`.
  - The `golden-check` recipe runs `$(PYTHON) $(GOLDEN_GEN) --check`, then
    `$(PYTHON) $(AUDIT_GOLDEN_GEN) --check`.
  - Update the comment: both oracles are independent Python 3.11+ standard-library scripts, one for
    the Crash v1 math vectors and one for the audit golden histories.
- **`README.md`.**
  - Status: add "the canonical codec, hash chain and audit CLI
    ([LBT-9](https://linear.app/lbtworks/issue/LBT-9/build-canonical-event-codec-and-audit-replay-cli))".
    Keep "No runtime, persistence or transport exists yet."
  - Layout:
    - `cmd/crashctl`: "replay/audit CLI: `crashctl audit verify|replay|chain` (LBT-9)";
    - add `internal/canonjson` ("Orvex Canonical JSON (RFC 8785 subset): encoder and strict decoder
      (LBT-9)");
    - add `internal/audit/chain` ("chain records, link hash and state hash");
    - add `internal/audit/replay` ("state replay and decision replay through the rules registry");
    - add `internal/audit/verification` ("history reader, staged verifier and golden histories");
    - the `internal/game` bullet adds "canonical codec contract";
    - the `internal/game/crash/v1` bullet adds "canonical codec (ADR-001, LBT-9)".
  - Development: the `make golden-check` bullet names both oracles.
- **`AGENTS.md`.** Edit only the `make golden-check` bullet: "regenerate the Crash v1 golden vectors
  and the audit golden histories with the independent Python oracles (Python 3.11+, standard library
  only) and fail if they differ from the committed files. Never regenerate vectors or fixtures from Go
  output."
- **`.github/workflows/ci.yml`.** Unchanged (§2 item 12).

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
  → 1
- `./bin/crashctl audit verify internal/audit/verification/testdata/golden/tamper/rules-version-2.jsonl`
  → 3
- `./bin/crashctl audit verify` (no FILE) → 2
- `./bin/crashctl audit verify /nonexistent` → 4

Integrity checks:
- `git diff --exit-code 6ec9f16 -- go.mod go.sum internal/game/crash/v1/params.go internal/game/crash/v1/curve.go internal/game/crash/v1/payout.go internal/game/crash/v1/fairness.go internal/game/crash/v1/testdata specs app_docs .github`
  → 0 (no change).
- `go mod tidy && git diff --exit-code go.mod go.sum` → 0.

If Go and the oracle disagree, find which side contradicts the spec and fix that side. Never edit a
§5.6 literal, a manifest expectation, or a fixture to match Go.

## 4. Files touched (whole change)

| File | Action | State |
|---|---|---|
| `docs/specs/8bebb70f_canonical-codec-audit-cli.md` | status, checkboxes, plan path | mostly done; step 0 |
| `internal/canonjson/{canonjson.go,canonjson_test.go,fuzz_test.go}` | new | done |
| `internal/game/codec.go` | new | done |
| `internal/game/registry.go`, `contract.go` | edit | done |
| `internal/game/registry_test.go`, `contract_test.go` | edit | done |
| `internal/game/codec_test.go` | new (`tallyCodec`; deviation §2 item 1) | to do |
| `internal/game/crash/v1/codec.go`, `codec_test.go`, `codec_fuzz_test.go` | new | to do |
| `internal/game/crash/v1/round.go` | edit `Adapter()` only | to do |
| `internal/game/crash/v1/params_test.go` | edit `TestNoIOImports` only | to do |
| `internal/game/crash/v1/round_test.go` | edit `TestAdapterRegistration` only | to do |
| `internal/audit/chain/{chain.go,chain_test.go}` | new | to do |
| `internal/audit/replay/{replay.go,replay_test.go}` | new | to do |
| `internal/audit/verification/{verification.go,verification_test.go,fuzz_test.go}` | new | to do |
| `internal/audit/verification/testdata/golden/generate.py` | new | to do |
| `internal/audit/verification/testdata/golden/{worked-example,instant-crash,empty-round,in-progress}.jsonl`, `tamper/*.jsonl` (26), `manifest.json` | new, written only by `generate.py --write` | to do |
| `cmd/crashctl/main.go`, `main_test.go` | edit | to do |
| `Makefile`, `README.md`, `AGENTS.md` | edit (step 9) | to do |

Not touched: `go.mod`, `go.sum`, `.github/`, `docs/adr/`, `specs/`, `app_docs/`, the frozen crash/v1
files and testdata, `commands.go`, `events.go`, `decide.go`, `evolve.go`, `projection.go`,
`docs/specs/8bebb70f_audit-codec-implementation.md` (plan v1, kept as the record).

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
| Canonical encoding (A-1 to A-4) | the canonjson tests (done), the step 3 tests and fuzzers, `TestEncodeStateCoversEveryField` |

## 6. PR description must include

- The spec's section 2 items 1–10 (flagged ADR points) and this plan's §2 item 1 deviation.
- That no new dependency was added.
- That the crash/v1 math files and vectors are unchanged.

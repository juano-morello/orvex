# Plan: implement the approved Crash v1 math, vectors and apd pin (LBT-7)

The governing document is `specs/e96d124d_crash-v1-math-spec.md` (called "the spec" below). The
engineer approved D-1 to D-14 as written, with no edits. This plan turns the spec's section 9 into
concrete steps. Where it adds detail (file schemas, helper names, extra tests), the detail is
binding. Where it deviates from section 9 it says so in section 2 and gives the reason.

Do not change any frozen value. If something in the spec looks wrong while you implement it, stop
and report it. Do not "fix" a value, a vector or the generator to match Go output.

## 1. Current state (verified)

- The worktree branch `odin/e96d124d` sits at `d6092f9`. **`main` has moved to `2b7b5a8`** (LBT-6
  merged): it adds `internal/game/{contract,registry}.go` plus tests, `specs/9d7bead2_*`,
  `app_docs/9d7bead2_*`, and edits two hunks of `README.md` (the Status line and an
  `internal/game` bullet in Layout). None of that is in this worktree. The v1 package does not need
  it: it imports only the standard library and apd (spec section 9.2). Do not copy or recreate
  `internal/game/*.go`.
- `main` did not touch `go.mod`, `Makefile`, `.github/workflows/ci.yml` or `AGENTS.md`.
- `go.mod` is `module github.com/juano-morello/orvex` / `go 1.26.0`, with no `require` and no `go.sum`.
- `specs/e96d124d_crash-v1-math-spec.md` is in the worktree, untracked. It still says
  "PROPOSED" and has empty checkboxes.
- Toolchain: go1.26.5, golangci-lint 2.14.0, Python 3.14.6 (`decimal` on libmpdec 4.0.0). CI
  `ubuntu-latest` ships python3 3.12.
- Facts measured during planning (scratch files under
  `/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/e96d124d/scratch/`, not deliverables):
  - The exact D-4 procedure (Exp, then `Mul` by `apd.New(1, 4)`, then `Floor`, then `Int64`) on apd
    v3.2.3 matches the Python oracle at all 15,352 ticks. Every returned condition is within
    `Inexact|Rounded`.
  - Full table time: 1.1 s, or 6.5 s under `-race`.
  - In a copy of this `go.mod`, `go get github.com/cockroachdb/apd/v3@v3.2.3` followed by
    `go mod tidy` yields exactly `require github.com/cockroachdb/apd/v3 v3.2.3` with no
    `toolchain` line, and four `go.sum` lines (apd and lib/pq, the hashes in spec D-13).
  - `debug.ReadBuildInfo()` inside a test binary lists `github.com/cockroachdb/apd/v3 v3.2.3`.
  - `git merge-file` of the README edits in step 6 against `main`'s README merges with 0 conflicts.

## 2. Deviations from the spec's section 9 (flag in the PR and the report)

No ADR conflict. These are process and doc deviations only:

1. **Commit A before commit B (spec 8.1 item 4, 9.1).** Agents cannot commit, and code makes one
   commit at the end of the run. Instead, independence is kept by the order of work. Write and run
   the generator, and get `--check` passing, **before any `.go` file exists in
   `internal/game/crash/v1/`**. If you can spawn a subagent, delegate step 3 to one whose only
   inputs are the spec and section 3 of this plan. Never regenerate vectors from Go output.
2. **README (spec 9.2).** The worktree predates LBT-6's README edit on `main`. Rewriting the Status
   line, or inserting a Layout bullet next to `main`'s new `internal/game` bullet, conflicts at
   merge (verified with `git merge-file`). So:
   - leave the Status line untouched;
   - put the `internal/game/crash/v1` bullet **before** `internal/buildinfo`;
   - add the prerequisite and command lines, which `main` did not touch.
   After merge, the Status line (from `main`) still reads "No Crash rules, runtime or persistence
   logic exists yet". The new bullet says the Crash lifecycle is not implemented. A follow-up
   (documenter or LBT-8) should reword the Status line once both branches are on `main`.

## 3. Step 1: generator and vectors (do this first, no Go yet)

Create `internal/game/crash/v1/testdata/golden/generate.py`. The rules:

- Python 3.11 or newer, standard library only: `argparse`, `bisect`, `decimal`, `fractions`,
  `hashlib`, `hmac`, `json`, `pathlib`, `sys`. Start with a `#!/usr/bin/env python3` shebang. The
  file mode does not matter, because make calls it through `python3`.
- It never imports, runs or reads Go code, `go.mod` or the Go constants. Parameters are typed in from
  the spec.
- The module docstring states:
  - its purpose, as the independent golden-vector oracle for `crash/v1` (spec section 8);
  - the independence rules from spec 8.1;
  - usage: `--write`, `--check`;
  - the two cross-check commands from spec D-10 for the worked example.
- Paths are resolved relative to `pathlib.Path(__file__).resolve().parent`, so the script runs from
  any working directory.
- CLI: a required, mutually exclusive `--write` or `--check`.
  - `--write` writes the six files in binary mode.
  - `--check` builds the same bytes in memory and compares them with the files on disk. For each
    missing or different file it prints `golden-check: <file> differs from generator output` to
    stderr, then exits 1. On success it prints `golden-check: 6 files match` and exits 0.
- Serialization for every file: `json.dumps(doc, indent=1, ensure_ascii=True).encode("ascii") + b"\n"`.
  Key order is the insertion order written below. Nothing environment-dependent goes in: no
  timestamps, no Python or libmpdec version.

### 3.1 Constants (from the spec)

`RULES_VERSION = "crash/v1"`, `SCALE = 10000`, `TICK_MICROS = 10000`, growth `Decimal(6).scaleb(-4)`
(string `"0.0006"`), `EDGE = 9900`, `MIN_C = 10000`, `MAX_C = 100000000`, `MAX_TICK = 15351`,
`MAX_DURATION = 153510000`, `MIN_BET = 1`, `MAX_BET = 1000000000`, `MAX_PAYOUT = 9996596800000`,
`INT64_MAX = 2**63 - 1`, `MAX_START = INT64_MAX - MAX_DURATION` (= 9223372036701265807),
`TAG = b"orvex/crash/v1/crash-point:"`, `APD_MODULE = "github.com/cockroachdb/apd/v3"`,
`APD_VERSION = "v3.2.3"`, `ORACLE_PREC = 80`, `CROSS_PREC = 120`, `T0 = 1760000000000000`,
`TWO64 = 2**64`.

### 3.2 Oracle functions

- Multiplier: `M(n, prec)` uses `Context(prec=prec, rounding=ROUND_HALF_EVEN, Emax=999999, Emin=-999999)`.
  Compute `v = ctx.multiply(ctx.exp(Decimal(6*n).scaleb(-4)), Decimal(10000))` (every operation through `ctx`), and return
  `(int(v.to_integral_value(rounding=ROUND_FLOOR)), v)`.
- Table: `M(n, 80)` for `n` in `0..15351`. Assert it equals `M(n, 120)` at every `n`.
- Margin: over `n >= 1`, the minimum of `min(frac, 1 - frac)` where `frac = v - floor`. Exclude `n = 0`,
  because `exp(0) = 1` exactly. Record it as `"{:.6e}".format(margin)` together with its tick.
- `raw(r) = EDGE * TWO64 // (TWO64 - r)`; `C(r) = min(MAX_C, max(MIN_C, raw(r)))`.
- `crash_tick(C) = bisect.bisect_left(table, C)`; deadline `= crash_tick * TICK_MICROS`.
- `rmin(X) = TWO64 - (EDGE * TWO64) // X`, the smallest `r` with `raw(r) >= X`.
- RTP: `Fraction(M, SCALE) * Fraction(EDGE * TWO64 // (M + 1), TWO64)` for each tick `0..15350`.
  Format it as a 20-place string: `Decimal(num) / Decimal(den)` at precision 60, then
  `quantize(Decimal("1e-20"), ROUND_HALF_EVEN)`.
- Round ID text: `h = b.hex()`, then `h[0:8]-h[8:12]-h[12:16]-h[16:20]-h[20:32]`, built explicitly.
  Do not use the `uuid` module.
- Fairness: commitment `hashlib.sha256(seed).digest()`; message `TAG + text.encode("ascii")`;
  outcome hash `hmac.new(seed, message, hashlib.sha256).digest()`;
  `r = int.from_bytes(hash[:8], "big")`.
- Timing reference: out of range if `start < 0`, `start > MAX_START`, `eff < 0`, `ct < 0` or
  `ct > MAX_TICK`. Otherwise `e = eff - start`; eligible iff `0 < e < ct * TICK_MICROS`; then
  `tick = e // TICK_MICROS` and the multiplier is `table[tick]`.
- Payout reference: out of range unless `MIN_BET <= bet <= MAX_BET` and `SCALE <= m <= MAX_C - 1`.
  Otherwise `bet * m // SCALE`.

### 3.3 Files and exact shapes

All six files start with `"rulesVersion": "crash/v1"`. Field order is as listed.

1. `params.json`: `rulesVersion`, `scale`, `tickMicros`, `growthPerTick` (`"0.0006"`),
   `edgeNumerator`, `minCrashPointScaled`, `maxCrashPointScaled`, `maxTick`, `maxDurationMicros`,
   `minBetCredits`, `maxBetCredits`, `maxPayoutCredits`, `maxStartMicros`, `outcomeTag`
   (`"orvex/crash/v1/crash-point:"`), `apdModule`, `apdVersion`, then:
   - `apdContext`: `{"precision": 34, "maxExponent": 6144, "minExponent": -6143, "rounding": "half_even", "traps": "DefaultTraps"}`;
   - `oracle`: `{"precision": 80, "crossCheckPrecision": 120, "minMarginToInteger": "<.6e string>", "minMarginTick": <int>}`;
   - `rtp`: `{"min": "<20 places>", "minTick": <int>, "max": "<20 places>", "maxTick": <int>}`.
2. `multiplier_ticks.json`: `rulesVersion`, `maxTick`, `multipliersScaled` (list of 15,352 ints,
   where index = tick).
3. `crash_points.json`: `rulesVersion`, `cases` (list). Each case has `name`, `rHex` (16 lowercase
   hex), `raw` (base-10 string), `crashPointScaled`, `crashTick`, `deadlineOffsetMicros`. The rows,
   in this order:
   - `r-zero` (`r = 0`);
   - `raw-9999` (`rmin(10000) - 1`);
   - `raw-10000-first` (`rmin(10000)`);
   - `instant-last` (`rmin(10001) - 1`);
   - `non-instant-first` (`rmin(10001)`);
   - `int64-sign-below` (`2**63 - 1`);
   - `int64-sign-exact-median` (`2**63`);
   - `cap-below` (`rmin(99999999)`);
   - `cap-exact` (`rmin(100000000)`);
   - `cap-above` (`rmin(100000001)`);
   - `raw-2pow63` (`rmin(2**63)`);
   - `raw-2pow64` (`rmin(2**64)`);
   - `r-max` (`2**64 - 1`);
   - `spread-000` to `spread-999`, where `r_i` is the first 8 bytes, big-endian, of
     `sha256(b"orvex/crash/v1/vector-r/" + str(i).encode())`.
4. `fairness.json`: `rulesVersion`, `cases`, `commitmentChecks`.
   - Each case has `name`, `seedHex`, `roundID` (canonical text), `commitmentHex`, `messageHex`,
     `outcomeHashHex`, `rHex`, `crashPointScaled`, `crashTick`, `deadlineOffsetMicros`. The rows:
     - `worked-example`: seed `bytes(range(32))`, round ID `0190f3a8-7b3c-7d4e-8f00-000000000001`;
     - `worked-example-round-2`: the same seed with `...000000000002`;
     - `seed-zero`: 32 zero bytes, with the worked-example round ID;
     - `seed-ff`: 32 `0xff` bytes, with the worked-example round ID;
     - `derived-00` to `derived-63`: seed `sha256(b"orvex/crash/v1/vector-seed/" + str(i).encode())`,
       round ID the first 16 bytes of `sha256(b"orvex/crash/v1/vector-round/" + str(i).encode())`.
   - Each `commitmentChecks` entry has `name`, `seedHex`, `commitmentHex`, `matches`:
     - `worked-example-match` (`true`);
     - `worked-example-bit-flip`: the last commitment byte XOR `0x01` (`false`).
5. `deadlines.json`: `rulesVersion`, `cases`. Each case has `name`; `crashPointScaled` (only on
   rows derived from a crash point); `crashTick`; `startMicros`; `effectiveMicros`. Then either
   `"error": "out_of_range"`, or `eligible`, plus `tick` and `multiplierScaled` only when eligible.
   Rows, with `start = T0` and `eff = T0 + e` unless noted:
   - C 10000 (n_c 0): e = -1, 0, 1: all ineligible.
   - C 10001 (n_c 1): e = 0 ineligible; 1 → tick 0, m 10000; 9999 → tick 0; 10000 ineligible
     (equality); 10001 ineligible.
   - C 10006 (n_c 1): e = 9999 → tick 0; 10000 ineligible.
   - C 10007 (n_c 2): e = 10000 → tick 1, m 10006; 19999 → tick 1; 20000 ineligible.
   - C 17577 (n_c 941): e = 9409999 → tick 940, m 17576; 9410000 ineligible.
   - C 100000000 (n_c 15351): e = 153509999 → tick 15350, m 99965968; 153510000 ineligible.
   - Domain rows, without `crashPointScaled`:
     - `start-negative`: start -1, eff T0, ct 1 → error;
     - `effective-negative`: start 0, eff -1, ct 1 → error;
     - `crash-tick-negative`: ct -1 → error;
     - `crash-tick-above-max`: ct 15352 → error;
     - `start-above-max`: start `MAX_START + 1`, eff `INT64_MAX`, ct 1 → error;
     - `start-at-max`: start `MAX_START`, eff `MAX_START + 1`, ct 15351 → eligible, tick 0, m 10000;
     - `effective-int64-max`: start 0, eff `INT64_MAX`, ct 15351 → ineligible, no error.
6. `payouts.json`: `rulesVersion`, `cases`. Each case has `name`, `betCredits`, `multiplierScaled`,
   then `payoutCredits` or `"error": "out_of_range"`. Rows, as `(bet, m)`:
   - successes:
     - (1, 10000) → 1
     - (1, 19999) → 1
     - (1, 20000) → 2
     - (3, 10006) → 3
     - (7, 14286) → 10
     - (9999, 10001) → 9999
     - (10000, 12345) → 12345
     - (123456789, 17576) → 216987652
     - (1000000000, 10000) → 1000000000
     - (1000000000, 99965968) → 9996596800000
     - (1000000000, 99999999) → 9999999900000
   - `out_of_range`:
     - bet 0, -1, 1000000001 and -2**63, each with m 10000;
     - bet 1 with m 9999, 0, -1, 100000000 and `INT64_MAX`;
     - (`INT64_MAX`, 99999999);
     - (2**62, 99999999).

### 3.4 Self-checks: the generator asserts these before writing or comparing

- Precision 80 and precision 120 agree at every tick.
- The margin is at least `1e-6`, at tick 2710.
- `M(0) == 10000`, `M` is strictly increasing, `M(15350) == 99965968` and `M(15351) == 100025966`.
- `rtp.min == "0.98990100989901009897"` at tick 0, `rtp.max == "0.98999999009662949582"` at tick
  15350, and every RTP lies in `[0.9899, 0.99)`.
- The boundary rows match spec 8.3:
  - `rHex` values: `028f5c28f5c28f5c`, `028f5c28f5c28f5d`, `0295d8f034d54722`, `0295d8f034d54723`,
    `fff9830e3bc2fc1e`, `fff9830e3cd9a523`, `fff9830e3df04e27`, `ffffffffffffb2a8`,
    `ffffffffffffd954`;
  - raw values: 9900, 9999, 10000, 10000, 10001, 19799, 19800, 99999999, 100000000, 100000001,
    `2**63`, `2**64`, 182622766329724560998400.
- The worked example matches spec D-10 exactly:
  - commitment `630dcd29…10dd`;
  - hash `6fd00d5e…e675`;
  - `r = 0x6fd00d5e48f2092a`, C 17577, n_c 941, D 9410000.
- `worked-example` and `worked-example-round-2` have different `r`.
- The `crashTick` values in `deadlines.json` are 0, 1, 1, 2, 941 and 15351, as listed.
- Every int written as a JSON number fits int64. Every output value is below `2**53`: that covers
  `crashPointScaled`, `crashTick`, `deadlineOffsetMicros`, `tick`, `multiplierScaled` and
  `payoutCredits`.

Then run `python3 internal/game/crash/v1/testdata/golden/generate.py --write` followed by `--check`.
The second command must exit 0.

## 4. Step 2: apd pin

After step 3, create the Go files in step 5. Then run
`go get github.com/cockroachdb/apd/v3@v3.2.3 && go mod tidy`. `go mod tidy` drops a requirement
that nothing imports yet. Expected result:

- `go.mod` gains `require github.com/cockroachdb/apd/v3 v3.2.3` and nothing else; if a `toolchain`
  line appears, remove it.
- `go.sum` holds exactly the four lines for apd and lib/pq (the hashes in spec D-13).

## 5. Step 3: Go package `internal/game/crash/v1` (`package v1`)

Imports allowed in non-test files are `crypto/hmac`, `crypto/sha256`, `encoding/binary`,
`encoding/hex`, `errors`, `fmt` (only `fmt.Errorf`), `math`, `math/bits` and
`github.com/cockroachdb/apd/v3`. There is no package-level mutable state; a `var` holds only the
two sentinel errors. No clock, no I/O, no `crypto/rand`, no `float64`. Group imports for
goimports: standard library, a blank line, then apd.

### 5.1 `params.go`

- Package doc: Package v1 holds the frozen arithmetic and fairness derivation of Crash rules
  `crash/v1` (ADR-004; LBT-7; spec `specs/e96d124d_crash-v1-math-spec.md`). Its functions are pure
  and deterministic, so the Crash `Decide`/`Evolve` (LBT-8) may call them (ADR-003). Released
  values, vectors and serializers are immutable (ADR-001, ADR-007).
- Exported constants, exactly as in spec 9.3: `RulesVersion = "crash/v1"`, `Scale`, `TickMicros`,
  `EdgeNumerator`, `MinCrashPointScaled`, `MaxCrashPointScaled`, `MaxTick`, `MaxDurationMicros`,
  `MinBetCredits`, `MaxBetCredits`, `MaxPayoutCredits`,
  `MaxStartMicros = math.MaxInt64 - MaxDurationMicros`, and
  `OutcomeTag = "orvex/crash/v1/crash-point:"`. All numeric ones are typed `int64`.
  - `RulesVersion`'s doc comment says it is the textual rules identifier used in `OutcomeTag` and in
    the golden vectors. It is distinct from the numeric `game.RulesVersion` registry key, which LBT-8
    wires.
- Unexported constants: `growthCoefficient int64 = 6`, `growthExponent int32 = -4` (the exact
  decimal `6E-4`), and `scaleExponent int32 = 4` (`10^scaleExponent == Scale`).
- `var ErrOutOfRange = errors.New("crash/v1: input out of range")` and
  `var ErrArithmetic = errors.New("crash/v1: arithmetic failure")`. Callers wrap them with
  `fmt.Errorf("%w: ...")`.
- `func ArithmeticContext() apd.Context` returns a fresh literal on every call:
  `apd.Context{Precision: 34, MaxExponent: 6144, MinExponent: -6143, Traps: apd.DefaultTraps, Rounding: apd.RoundHalfEven}`.
- `func apdCheck(op string, c apd.Condition, err error) error`. It returns a wrapped
  `ErrArithmetic` if `err != nil` or `c &^ (apd.Inexact | apd.Rounded) != 0`, and nil otherwise.
- `func mulNonNeg(a, b int64) (int64, error)`. It returns a wrapped `ErrArithmetic` if `a < 0` or
  `b < 0`. Otherwise it computes `hi, lo := bits.Mul64(uint64(a), uint64(b))` and returns a wrapped
  `ErrArithmetic` if `hi != 0 || lo > math.MaxInt64`.
- `func subChecked(a, b int64) (int64, error)`. This is the standard signed overflow test, returning
  a wrapped `ErrArithmetic` on overflow.

### 5.2 `curve.go`

- `MultiplierAtTick(n int64) (int64, error)`.
  1. Reject `n` outside `[0, MaxTick]` with `ErrOutOfRange`.
  2. Get a local context with `ctx := ArithmeticContext()`.
  3. `ctx.Exp(&y, apd.New(growthCoefficient*n, growthExponent))`.
  4. `ctx.Mul(&v, &y, apd.New(1, scaleExponent))`.
  5. `ctx.Floor(&f, &v)`.
  6. `f.Int64()`.

  Pass every apd result through `apdCheck`; a conversion error wraps `ErrArithmetic`. As a sanity
  check, a result `< Scale` is `ErrArithmetic`. Do not use `Ln`, `Pow` or floats.
- `CrashTick(crashPointScaled int64) (int64, error)` rejects values outside
  `[MinCrashPointScaled, MaxCrashPointScaled]`. It then binary searches
  `lo, hi := 0, MaxTick`, keeping `M(hi) >= C`. At each step `mid := lo + (hi-lo)/2`; if
  `M(mid) >= C` then `hi = mid`, else `lo = mid + 1`. It returns `lo` and propagates errors. There is
  no memoization: no package state.
- `DeadlineOffsetMicros(crashTick int64) (int64, error)` checks `[0, MaxTick]`, then returns
  `mulNonNeg(crashTick, TickMicros)`.
- `CashOutTiming(startMicros, effectiveMicros, crashTick int64) (tick int64, eligible bool, err error)`.
  - The domain is the D-7 table: `0 <= startMicros <= MaxStartMicros`, `effectiveMicros >= 0` and
    `0 <= crashTick <= MaxTick`. Outside it, return `0, false` and a wrapped `ErrOutOfRange`.
  - Compute `e, err := subChecked(effectiveMicros, startMicros)` and
    `d := DeadlineOffsetMicros(crashTick)`.
  - If `e <= 0 || e >= d`, return `0, false, nil`. Equality loses: this is spec D-9.
  - Otherwise return `e / TickMicros, true, nil`.

### 5.3 `payout.go`

`Payout(betCredits, multiplierScaled int64) (int64, error)`:
1. Reject `betCredits` outside `[MinBetCredits, MaxBetCredits]` and `multiplierScaled` outside
   `[Scale, MaxCrashPointScaled-1]` with `ErrOutOfRange`.
2. `p, err := mulNonNeg(betCredits, multiplierScaled)`.
3. Return `p / Scale`.

### 5.4 `fairness.go`

- `Commitment(seed [32]byte) [32]byte` returns `sha256.Sum256(seed[:])`.
- `VerifyCommitment(seed, commitment [32]byte) bool` returns `Commitment(seed) == commitment`. These
  are public values, so a plain comparison is fine (spec section 7).
- `OutcomeMessage(roundID [16]byte) []byte` returns `OutcomeTag` followed by the 36-byte lowercase
  `8-4-4-4-12` text, built with `hex.Encode` into a fixed `[36]byte` with hyphens at 8, 13, 18 and
  23.
- Unexported `outcomeHash(seed [32]byte, roundID [16]byte) [32]byte` computes
  `hmac.New(sha256.New, seed[:])`, writes the message and returns the sum. `hash.Hash.Write` never
  errors. If errcheck flags the call, use `_, _ = mac.Write(msg)` with that comment.
- `OutcomeR(seed, roundID) uint64` returns `binary.BigEndian.Uint64` of `outcomeHash(...)[:8]`.
- Unexported `crashPointRaw(r uint64) *apd.BigInt` computes
  `EdgeNumerator * 2^64 / (2^64 - r)` exactly with `apd.BigInt` (`Lsh`, `Mul`, `SetUint64`, `Sub`,
  `Quo`). For non-negative values, `Quo` truncation equals floor.
- `CrashPointFromR(r uint64) int64` clamps `crashPointRaw(r)` before narrowing. A raw value
  `>= MaxCrashPointScaled` returns `MaxCrashPointScaled`, and `<= MinCrashPointScaled` returns
  `MinCrashPointScaled`. Otherwise it returns `raw.Int64()`, which is safe after the clamp.
- `CrashPoint(seed, roundID) int64` returns `CrashPointFromR(OutcomeR(seed, roundID))`.

## 6. Step 4: Makefile, CI, AGENTS.md, README, spec status

- `Makefile`:
  - add `PYTHON ?= python3` and
    `GOLDEN_GEN := internal/game/crash/v1/testdata/golden/generate.py` beside the other variables;
  - add `golden-check` to `.PHONY`;
  - add the target below. The recipe line is tab-indented.

  ```make
  golden-check:
  	$(PYTHON) $(GOLDEN_GEN) --check
  ```

- `.github/workflows/ci.yml`: in job `test`, right after `- run: make test`, add
  `- run: make golden-check`.
- `AGENTS.md`, under `## Commands`: after the `make lint` bullet, add

  `` - `make golden-check`: regenerate the Crash v1 golden vectors with the independent Python oracle (Python 3.11+, standard library only) and fail if they differ from the committed files. Never regenerate vectors from Go output. ``

- `README.md`. Do not touch the Status line or any existing bullet, because of the merge conflict in
  section 2.
  - Insert this line directly before the `` - `internal/buildinfo` `` bullet:

    `` - `internal/game/crash/v1`: frozen Crash v1 arithmetic, fairness derivation and golden vectors (ADR-004, LBT-7); the Crash lifecycle is not implemented yet ``
  - Replace `Prerequisites: Go 1.26 and golangci-lint v2.14.0.` with

    `` Prerequisites: Go 1.26, golangci-lint v2.14.0 and Python 3.11 or newer (standard library only; used by `make golden-check`). ``
  - Insert this line directly after the `` - `make lint` `` bullet:

    `` - `make golden-check`: regenerate the Crash v1 golden vectors with the independent Python oracle and compare them with the committed files ``
- `specs/e96d124d_crash-v1-math-spec.md`. Change only these two things:
  - Replace the four-line Status paragraph (lines 3 to 6, from `Status: **PROPOSED` to `release.`)
    with:

    ```
    Status: **APPROVED** on 2026-10-14. The engineer approved decisions D-1 to D-14 as written, with
    no edits. Section 0 described the planning run and no longer applies; implementation follows
    section 9, as planned in `specs/e96d124d_crash-v1-implementation.md`. ADR-004 requires these
    values to be frozen before the first production rules-version release.
    ```
  - In the section 3 table, change all 14 `| [ ] |` cells (lines 89 to 102) to `| [x] |`.

## 7. Tests (all required)

Test files use the internal package `v1`, because some tests reach unexported helpers. Use
standard `testing` only, with no assertion libraries. Read vectors with
`os.ReadFile(filepath.Join("testdata", "golden", name))`.

### 7.1 Shared helpers (in `golden_test.go`)

- `decodeVectors(data []byte, dst any) error`.
  1. Unmarshal a header struct `{RulesVersion string "json:\"rulesVersion\""}`.
  2. Return an error unless it equals `RulesVersion`. A missing value counts as an error.
  3. Decode into `dst` with `json.Decoder.DisallowUnknownFields()`.
  Every file struct carries a `RulesVersion` field. Optional fields (`crashPointScaled`,
  `eligible`, `tick`, `multiplierScaled`, `payoutCredits`) are pointers, and `error` is a string.
- `loadVectors(t, name, dst)` wraps `ReadFile` and `decodeVectors`, calling `t.Fatal` on error.
- `multiplierTable(t)`: `MultiplierAtTick(n)` for all ticks, computed once per test binary through
  `sync.OnceValues`. It is shared to keep `-race` runtime down (about 6.5 s per full table).
- `parseRoundID(t, s) [16]byte`: requires exactly 36 lowercase characters with hyphens at 8, 13, 18
  and 23, then hex-decodes the rest. `parseHex32(t, s) [32]byte` does the same for 32 bytes.

### 7.2 `internal/game/crash/v1/golden_test.go`

| test | asserts |
|---|---|
| `TestGoldenParams` | Every `params.json` constant equals its Go constant. `growthPerTick == apd.New(growthCoefficient, growthExponent).String()`. `ArithmeticContext()` matches `apdContext`: `string(Rounding) == "half_even"`, and `"DefaultTraps"` maps to `apd.DefaultTraps`. `apdModule`/`apdVersion` equal the dep in `debug.ReadBuildInfo()`. |
| `TestGoldenMultiplierTable` | `len == MaxTick+1`, and `multiplierTable` equals `multipliersScaled` at every tick. On failure, report the first differing tick. |
| `TestGoldenCrashPoints` | For every row: `crashPointRaw(r).String() == raw`, `CrashPointFromR(r)`, `CrashTick(C)` and `DeadlineOffsetMicros(n_c)` match. `r` comes from `strconv.ParseUint(rHex, 16, 64)`. |
| `TestGoldenFairness` | For every case: `Commitment`, `OutcomeMessage` (hex), `outcomeHash`, `OutcomeR`, `CrashPoint`, `CrashTick` and `DeadlineOffsetMicros` match. For every `commitmentChecks` row, `VerifyCommitment` returns `matches`. |
| `TestGoldenDeadlines` | For every row, `CrashTick(crashPointScaled) == crashTick` when present. Then `CashOutTiming`: error rows give `errors.Is(err, ErrOutOfRange)` and `eligible == false`. Other rows give `err == nil` and `eligible` as listed. Eligible rows match `tick`, and `MultiplierAtTick(tick) == multiplierScaled`. Ineligible rows have `tick == 0`. |
| `TestGoldenPayouts` | For every row, `Payout` equals `payoutCredits`, or `errors.Is(err, ErrOutOfRange)`. |
| `TestGoldenRejectsUnknownRulesVersion` | `decodeVectors` errors on `{"rulesVersion":"crash/v2","cases":[]}` and on `{"cases":[]}`. |

### 7.3 `internal/game/crash/v1/curve_test.go`

| test | asserts |
|---|---|
| `TestArithmeticContextFrozen` | Precision 34, MaxExponent 6144, MinExponent -6143, `RoundHalfEven`, `apd.DefaultTraps`. Setting `Precision = 5` on a returned copy leaves the next `ArithmeticContext()` unchanged. |
| `TestApdVersionPinned` | `debug.ReadBuildInfo()` lists `github.com/cockroachdb/apd/v3` at `v3.2.3`. |
| `TestMultiplierStrictlyIncreasing` | `multiplierTable[n+1] > multiplierTable[n]` for every `n`, and `[0] == Scale`. |
| `TestMultiplierAtTickOutOfRange` | `-1`, `15352`, `math.MinInt64` and `math.MaxInt64` give `ErrOutOfRange`. |
| `TestCrashTickAndDeadlineOutOfRange` (added) | `CrashTick` with 9999, 100000001, 0, -1 and `math.MaxInt64`, and `DeadlineOffsetMicros` with -1 and 15352, all give `ErrOutOfRange`. |
| `TestCrashTickBracketsCrashPoint` | For C in {10000, 10001, 10006, 17577, 19800, 100000000}: `M(n_c-1) < C <= M(n_c)`. Only the right inequality applies when `n_c == 0`. |
| `TestCashOutTimingDeadlineEqualityLoses` | For C in {10001, 10007, 17577, 100000000}, with `T0 = 1_760_000_000_000_000`: `e = D` is ineligible, and `e = D-1` is eligible with tick `n_c-1`. |
| `TestHouseEdgeBounds` | Computes `RTP(n)` with `math/big` (`big.Int.Quo` for the floor, `big.Rat` for the product) over ticks 0 to 15350. The minimum is at tick 0 and the maximum at tick 15350. `FloatString(20)` of each equals `params.json` `rtp.min`/`rtp.max`; `FloatString` rounds halves away from zero, and neither value is a half. |

### 7.4 Other test files

- `internal/game/crash/v1/payout_test.go`:
  - `TestPayoutTickZeroReturnsBet`: `Payout(b, 10000) == b` for b in {1, 7, 1000000000}.
  - `TestPayoutRejectsOutOfRange`: the bet and multiplier bounds of D-7, plus
    (`math.MaxInt64`, 99999999) and (`1<<62`, 99999999), all `ErrOutOfRange`.
- `internal/game/crash/v1/fairness_test.go`:
  - `TestOutcomeMessageCanonicalText`: for an ID of 16 `0xff` bytes and one of mixed bytes, the
    message is `OutcomeTag` followed by 36 bytes, all lowercase, in `8-4-4-4-12` form.
- `internal/game/crash/v1/params_test.go` (both tests added):
  - `TestCheckedArithmeticOverflow`:
    - `mulNonNeg(3037000499, 3037000499)` returns 9223372030926249001;
    - `(3037000500, 3037000500)`, `(math.MaxInt64, 2)` and `(-1, 1)` give `ErrArithmetic`;
    - `subChecked(math.MinInt64, 1)` and `subChecked(math.MaxInt64, -1)` give `ErrArithmetic`;
    - `subChecked(5, 7) == -2`.
  - `TestNoIOImports`: mirrors `internal/game`'s test on `main`. It parses every non-test `.go`
    file in the package with `go/parser`. Each import must be on the section 5 allowlist and must
    not be renamed. `fmt` is allowed only as `fmt.Errorf`. Every non-test file must be checked.
- `internal/game/crash/v1/fuzz_test.go` (`make test` runs only the seed corpora):
  - `FuzzPayout(bet, m int64)`: never panics, and returns `ErrOutOfRange` iff the inputs are outside
    the domain. Otherwise the result equals the `math/big` value `bet*m/Scale`. Seeds: the payout
    vector edges.
  - `FuzzCrashPointFromR(r uint64)`: the result lies in `[10000, 100000000]` and equals a `math/big`
    reference of the D-5 formula. Seeds: 0, `1<<63`, `math.MaxUint64`, `0xfff9830e3cd9a523`.
  - `FuzzCashOutTiming(start, eff, ct int64)`: `ErrOutOfRange` iff the inputs are outside the
    domain. Otherwise `eligible` iff `0 < eff-start < ct*TickMicros`, and when eligible,
    `tick == (eff-start)/TickMicros < ct`. Seeds: the deadline vector rows.

LBT-7 acceptance mapping:
- deadlines: `deadlines.json`, `TestGoldenDeadlines`, `TestCashOutTimingDeadlineEqualityLoses`;
- rounding: `multiplier_ticks.json`, `payouts.json`, `crash_points.json`, `TestGoldenMultiplierTable`,
  `TestGoldenPayouts`;
- overflow: the raw `2^63`/`2^64`/`r-max` rows, the payout wrap rows and the start-above-max row,
  plus `TestCheckedArithmeticOverflow`;
- versioned outcomes: `fairness.json` (`messageHex` pins the `crash/v1` tag), the `rulesVersion`
  header on every file, and `TestGoldenRejectsUnknownRulesVersion`.

## 8. Verification (each must exit 0; judge by exit status)

```sh
python3 internal/game/crash/v1/testdata/golden/generate.py --check
make golden-check
make test
make lint
go vet ./...
make build
go mod verify
go list -m github.com/cockroachdb/apd/v3   # prints: github.com/cockroachdb/apd/v3 v3.2.3
```

Also:

```sh
printf '%s' 'orvex/crash/v1/crash-point:0190f3a8-7b3c-7d4e-8f00-000000000001' | openssl dgst -sha256 -mac HMAC -macopt hexkey:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
printf '%s' '000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f' | xxd -r -p | sha256sum
```

They must print `6fd00d5e48f2092aca65a8c3a649b50a498716e8457736d38584c9ec964fe675` and
`630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd`, the `worked-example` row of
`fairness.json`.

## 9. Out of scope

- Crash lifecycle, commands, events, `Decide`/`Evolve`, projection, registry wiring (LBT-8).
- The ADR-004 amendment (spec section 10, item 5) and an arm64 CI runner (item 6).
- Anything under `internal/game/` other than `crash/v1/`.
- `cmd/*`, `odin/`, `.omp/` and `app_docs/`.
- Editing the spec beyond the status paragraph and the checkboxes.
- `actions/setup-python` in CI (python3 is preinstalled), and any change to `setup-go`'s
  `cache-dependency-path`.

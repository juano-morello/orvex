# Crash v1 mathematical specification and golden vector plan (LBT-7)

Status: **PROPOSED, awaiting engineer approval.** Every item marked **Decision D-n** is a proposal.
Nothing is frozen, and no code, golden vectors or `go.mod` changes are made, until the engineer
approves it. ADR-004 requires these values to be frozen before the first production rules-version
release.

- Linear: [LBT-7](https://linear.app/lbtworks/issue/LBT-7/freeze-crash-mathematical-specification-and-golden-vectors) (blocks LBT-8). Milestone M1, Deterministic Core.
- Governing ADRs: ADR-004 (primary), ADR-001, ADR-002, ADR-003, ADR-006, ADR-007 (all mirrored in `docs/adr/`).
- Rules version named by this spec: `crash/v1`.

## 0. What this run delivers

- This document is the deliverable. Its copy under `specs/` is the record the engineer approves.
- **Builder, this run: make no repository changes.** Do not write Go code, the vector generator,
  golden vector files, or touch `go.mod`, `go.sum`, `Makefile`, CI, `README.md` or `AGENTS.md`.
  The prompt puts all of that out of scope until the spec is approved. The quality gates
  (`make test`, `make lint`, `go vet ./...`, `make build`) must still pass on the unchanged tree.
- After approval, section 9 is the implementation plan for a separate run.
- Out of scope everywhere in this document: Crash lifecycle `Decide`/`Evolve` (LBT-8), the generic
  game contract (LBT-6), the event codec (LBT-9), schema and admission SQL (LBT-10, LBT-11), ledger
  postings (LBT-12), real-money flows (ADR-004 "Exclusions").

## 1. ADR alignment

### 1.1 ADR-004 "Open implementation parameters" mapped to decisions

| ADR-004 open parameter | Decision |
|---|---|
| exponential coefficient | D-3 |
| time quantum | D-2 |
| multiplier scale | D-1 |
| apd context | D-4 |
| rounding | D-4 (rounding table), D-5, D-8 |
| max duration | D-6 |
| crash-point probability distribution | D-5 (house edge stated there and in section 6) |
| exact seed derivation | D-10, D-11 |
| canonical encoding (of numeric outputs) | D-12 |

LBT-7 also asks for limits (D-7), payout rounding (D-8), the apd pin (D-13) and independent golden
vectors (D-14, section 8).

### 1.2 Other ADR clauses

| ADR clause | Where satisfied |
|---|---|
| ADR-004 Decision: apd/v3, pinned version, explicit precision and rounding, golden vectors, checked conversions, integer credits | D-4, D-13, section 8, D-7, D-12 |
| ADR-004 Decision: cash-out timing uses durable admission time; equality with crash deadline loses | D-9 |
| ADR-004 Decision: payout = floor(betCredits * scaledMultiplier / scale), checked | D-8 |
| ADR-004 Fairness: 256-bit seed, SHA-256 commitment before betting, versioned outcome from seed and round identity, reveal after settlement | D-10, D-11 |
| ADR-004 "Store authoritative numeric outputs in events; retain historical rule artifacts" | D-12, section 8 |
| ADR-002 Decision: eligible only when effective admission time is strictly before the crash deadline and after round start | D-9 |
| ADR-003: Decide/Evolve pure, no uncontrolled randomness or wall clock | D-10 (runtime generates the seed and passes it as a recorded deterministic input) |
| ADR-001: historical rules reproducible, canonical encodings | D-12, D-13, section 8 |
| ADR-006: golden arithmetic vectors, fuzz/property tests, fail closed on unknown rules | section 8, section 9 |
| ADR-007: `internal/game/crash/v1/`, pinned apd, minimal justified dependencies | D-13, section 9 |

### 1.3 Conflicts and flagged interpretations

No conflict with any ADR. Three points need the engineer's explicit attention:

1. **ADR-002 Decision, "after round start".** Read as strictly after (elapsed time `e > 0`). See D-9.
2. **ADR-007 tooling.** The independent golden-vector oracle is a Python 3 standard-library script
   (D-14). It is not a Go module, not a build dependency and not an orchestrator, so it does not
   touch ADR-007's single-module rule. ADR-007 does not list Python among the project's tools, so
   approving D-14 approves Python as a test-oracle-only tool.
3. **ADR-004 status line "Accepted (implementation constants pending)".** Once these values ship in
   the first rules release, ADR-004 (Linear source and `docs/adr/` mirror, same change, per
   AGENTS.md) should be amended to reference them. Not part of this run or of section 9.

## 2. Notation and units

- `S`: multiplier scale. A scaled multiplier is an int64 `m` meaning `m / S` times the bet.
- `Q`: time quantum (one tick), in microseconds.
- `n`: tick index. `M(n)`: scaled multiplier at tick `n`.
- `r`: 64-bit outcome integer. `C`: crash point, scaled. `n_c`: crash tick. `D`: crash deadline
  offset from round start, in microseconds.
- `T0`: round start time. `t_a`: a cash-out's durable effective admission time (ADR-002). Both are
  int64 microseconds since the Unix epoch, UTC (PostgreSQL `timestamptz` resolution). `e = t_a - T0`.
- `floor(a / b)` on non-negative integers is Go integer division.
- `2^64` is 18,446,744,073,709,551,616. `MaxInt64` is 9,223,372,036,854,775,807.

## 3. Decision summary

Tick a box to approve, or edit the value. The implementation run uses approved values only.

| ID | Parameter | Proposed value | Approve |
|---|---|---|---|
| D-1 | Multiplier scale | `S = 10,000` (4 decimal places) | [ ] |
| D-2 | Time quantum | `Q = 10 ms = 10,000 µs`; `tick = floor(e / Q)` | [ ] |
| D-3 | Growth coefficient | `M(n) = floor(S * exp(0.0006 * n))`, i.e. `e^(0.06 t)` with `t` in seconds | [ ] |
| D-4 | apd context and rounding | Precision 34, MaxExponent 6144, MinExponent -6143, `RoundHalfEven`, `apd.DefaultTraps`; one floor at the end | [ ] |
| D-5 | Crash distribution and house edge | `C = clamp(floor(9,900 * 2^64 / (2^64 - r)), 10,000, 100,000,000)`; edge 1% | [ ] |
| D-6 | Max multiplier and max duration | 10,000.0000x; 15,351 ticks = 153.51 s | [ ] |
| D-7 | Limits and overflow | bet 1 to 1,000,000,000 credits; typed errors, never wrap or saturate | [ ] |
| D-8 | Payout | `floor(bet * m / 10,000)`, exact checked product, single floor | [ ] |
| D-9 | Deadline and eligibility | `n_c = min{n : M(n) >= C}`; `D = n_c * Q`; eligible iff `0 < e < D` | [ ] |
| D-10 | Seed, commitment, outcome derivation | 32-byte `crypto/rand` seed; commitment `SHA-256(seed)`; `r` = first 8 bytes of `HMAC-SHA256(seed, "orvex/crash/v1/crash-point:" + roundID)` | [ ] |
| D-11 | Round identity encoding | UUID, canonical lowercase 8-4-4-4-12 text | [ ] |
| D-12 | Canonical encoding of numeric outputs | int64 in fixed units; canonical base-10 text; lowercase hex for bytes; no floats | [ ] |
| D-13 | apd pin | `github.com/cockroachdb/apd/v3 v3.2.3` | [ ] |
| D-14 | Golden vectors | Python 3 stdlib oracle (libmpdec); exhaustive tick table plus boundary sets; `make golden-check` in CI | [ ] |

## 4. Decisions

### D-1. Multiplier scale: `S = 10,000`

**Proposal.** Every multiplier (curve value, crash point, cash-out multiplier) is an int64 scaled by
10,000: 1.0000x is 10,000 and 2.3456x is 23,456. One scale for all multipliers.

**Rationale.**
- With D-2 and D-3 the curve rises by at least 6 scaled units per tick (measured minimum over all
  ticks), so `M` is strictly increasing and every tick pays a distinct multiplier.
- Crash points sit on the same grid as tick multipliers, so "a cash-out at tick `n` survives iff
  `M(n) < C`" is one integer comparison.
- The grid costs the player little: expected return at tick `n` is about `0.99 * M / (M + 1)`, at
  most 0.0099 percentage points below 99% (D-5). At `S = 100` the same effect is about 1 percentage
  point at low multipliers.
- Overflow headroom stays large: max bet times max multiplier is about 1e17, 92 times below
  `MaxInt64` (D-7).

**Alternatives considered.**
- `S = 100` (the familiar "2.37x"): the curve repeats values for the first ~17 ticks (growth per tick
  at 1.00x is 0.06 of a unit), and the grid penalty grows to about 1%. A UI can still show two
  decimals by truncating `S = 10,000` values.
- `S = 10^8`: finer than any credit amount can use, and it cuts overflow headroom; the max bet would
  have to fall to about 9e6 credits at the same cap.
- Separate scales for crash point (10^2) and curve (10^4): two conversions, and the survival check
  needs rescaling.

### D-2. Time quantum: `Q = 10 ms`

**Proposal.** `Q = 10,000 µs`. A cash-out with elapsed time `e = t_a - T0 > 0` falls in tick
`n = floor(e / Q)` and is paid `M(n)`. The multiplier is constant within a tick.

**Rationale.**
- The domain of `M` becomes finite and small: ticks 0 to 15,351 (D-6). Golden vectors can then list
  every multiplier the game can ever pay, so the curve has no untested inputs (section 8).
- 10 ms is below human reaction time and typical network jitter, and close to a 60 to 100 Hz
  animation frame. Microsecond admission timestamps divide it exactly.
- Time handling is plain integer division; no decimal arithmetic touches time.

**Alternatives considered.**
- 1 ms: about 153,500 ticks (still checkable exhaustively, with a ten times larger vector file). Growth per
  tick at 1.0x is 0.6 of a scaled unit, so the curve repeats values at low multipliers, and the
  crash-tick search costs more `Exp` calls.
- 100 ms: 0.6% jumps per tick, visible steps and coarse payouts.
- No quantization (continuous `M(e)` at microsecond resolution): about 1.5e8 possible inputs, too
  many to check exhaustively. Payout correctness would rest on a statistical margin argument instead
  of a complete check.

### D-3. Growth coefficient: `g = 0.0006` per tick

**Proposal.** `M(n) = floor(S * exp(g * n))` with `g = 0.0006` exactly (decimal `6E-4`) and `n` in
`[0, 15,351]`. The continuous equivalent is `e^(0.06 t)` with `t` in seconds, or `e^(0.00006 ms)`.

Measured milestones (first tick at or above the multiplier):

| multiplier | tick | time | `M(tick)` |
|---|---|---|---|
| 2x | 1,156 | 11.56 s | 20,009 |
| 10x | 3,838 | 38.38 s | 100,021 |
| 100x | 7,676 | 76.76 s | 1,000,429 |
| 1,000x | 11,513 | 115.13 s | 10,000,447 |
| 10,000x | 15,351 | 153.51 s | 100,025,966 |

The median crash point (1.98x, D-5) is reached after about 11.4 s.

**Rationale.** `g * n` is an exact decimal (coefficient `6n`, exponent -4), so `exp` is the only
inexact operation in the curve. The rate matches the curve popularized by open-source Crash clients
such as bustabit (`e^(0.00006 ms)`), so the pacing is familiar. With the D-6 cap the longest round
runs about two and a half minutes.

**Alternatives considered.**
- `g = 0.001` per tick (0.1/s): 2x at 6.9 s, 10,000x at 92.1 s. Faster rounds, less time to react at
  low multipliers.
- `g = 0.0003` per tick (0.03/s): 2x at 23.1 s, 10,000x at 307 s. Long rounds.
- Polynomial or piecewise growth: ADR-004 decides exponential growth.

### D-4. apd context and rounding

**Proposal.** One frozen context for every apd operation in `crash/v1`:

| field | value |
|---|---|
| `Precision` | 34 |
| `MaxExponent` | 6144 |
| `MinExponent` | -6143 |
| `Rounding` | `apd.RoundHalfEven` |
| `Traps` | `apd.DefaultTraps` (SystemOverflow, SystemUnderflow, Overflow, Underflow, Subnormal, DivisionUndefined, DivisionByZero, DivisionImpossible, InvalidOperation) |

`M(n)` is computed in exactly these steps:

1. Reject `n` outside `[0, 15,351]` with `ErrOutOfRange`.
2. `x = apd.New(6*n, -4)` (exact).
3. `ctx.Exp(y, x)`.
4. `ctx.Mul(v, y, apd.New(1, 4))` (times 10^4; exact, the coefficient is unchanged).
5. `ctx.Floor(f, v)`.
6. `f.Int64()` (checked conversion).

After every apd call, a non-nil error, or a returned `Condition` with any flag outside
`Inexact|Rounded`, fails the call with `ErrArithmetic`. The authoritative v1 path uses no `apd.Ln`,
no `apd.Pow` and no `float64`. The crash tick is found by searching `M` (D-9), not by computing
`ln(C) / g`.

**Rounding rules for all of v1:**

| quantity | rounding | where |
|---|---|---|
| `exp` intermediate | half-even to 34 digits (apd internal) | D-4 step 3 |
| curve value `M(n)` | floor to an integer scaled multiplier | D-4 step 5 |
| tick from elapsed time | floor (`e / Q`, `e > 0`) | D-2 |
| crash point `raw` | floor (exact integer division) | D-5 |
| payout | floor (exact product, one integer division) | D-8 |

Every floor rounds toward the house: the player never receives more than the exact curve value.

**Rationale.**
- Precision 34, exponent range 6144/-6143 and half-even rounding are IEEE 754 decimal128. Other
  decimal libraries reproduce that context exactly (Python `decimal`, Java `MathContext.DECIMAL128`),
  which the independent oracle relies on.
- apd's `Exp` rounds half-even internally whatever `ctx.Rounding` says: `context.go` in v3.2.3 sets
  `nc.Rounding = RoundHalfEven` on its working context and uses it for the final round. Declaring
  half-even states what the code does; a directed mode in the context would be ignored and
  misleading.
- **Margin.** Over all 15,352 ticks, the exact value `S * e^(g n)` never comes closer than
  3.7266e-5 to an integer (closest at tick 2,710). At 34 significant digits the unit in the last
  place is at most 1e-25 for values up to 1.0003e8. The floored integer therefore cannot change
  unless the computed `exp` is wrong by about 1e20 units in the last place.
- **Measured.** apd v3.2.3 with this context produces the same `M(n)` as the independent oracle at
  all 15,352 ticks. Its 34-digit `exp` intermediate differs from the correctly rounded value in the
  last digit at 105 ticks. Example, tick 334, `exp(0.2004)`: apd returns
  `1.221891416988684153689110970384258`, the correctly rounded value is `...384257`, and the exact
  value is `1.22189141698868415368911097038425749...`. apd's `Exp` stays within one unit in the last
  place but is not always correctly rounded. Consequence: the 34-digit intermediate is never
  authoritative, never stored and never compared in golden vectors. Only the floored integer is.
- apd's `Exp` uses `float64` internally to pick its Taylor-series term count (`context.go`, stage 3).
  Another architecture (fused multiply-add on arm64, a different `math.Log10`) could pick a
  different count and change at most the last digit of the intermediate. The margin keeps the
  floored result identical, and the exhaustive tick table (section 8) would catch any drift.

**Alternatives considered.**
- Precision 16 (decimal64): still floors correctly, but the conservative margin factor drops from
  about 1e20 to about 400. No measurable speed gain for that loss.
- Precision 50 or more: no benefit, slower.
- `apd.BaseContext`: precision 0 makes `Exp` fail with "Context may not have 0 Precision".
- A directed rounding mode (`RoundFloor`) in the context: ignored by `Exp` (see above).
- Computing the crash tick with `Ln`: adds a second transcendental function whose rounding near
  integer boundaries must agree with `Exp`. Searching `M` is exact by construction.
- An integer tick table generated offline and embedded in Go, with no apd at runtime: O(1) lookups
  and no transcendental code in production. ADR-004 decided apd decimal arithmetic for the curve, so
  this would need an ADR-004 amendment. The exhaustive golden table gives the same assurance while
  keeping apd.

### D-5. Crash-point distribution and house edge

**Proposal.**

1. `r`: the uint64 from the outcome hash (D-10).
2. `raw = floor(9,900 * 2^64 / (2^64 - r))`, computed exactly with `apd.BigInt`. The denominator
   ranges from 1 to `2^64` (`r = 0` gives `2^64`, which does not fit in a uint64) and `raw` reaches
   1.826e23, so neither may be computed in uint64 or int64.
3. `C = min(100,000,000, max(10,000, raw))`, then narrowed to int64.

`9,900 = S * (1 - h)` with house edge `h = 1%`.

**Resulting distribution.** For grid values `X` with `10,000 < X <= 100,000,000`:
`P(C >= X) = floor(9,900 * 2^64 / X) / 2^64`, which is `0.99 / (X / S)` to within 2^-64.
`P(C >= 10,000) = 1`.

| event | probability |
|---|---|
| `C = 1.0000x` (instant crash, nobody can cash out) | 0.0100989901009899 (about 1 round in 99) |
| `C >= 2x` | 0.495 |
| `C >= 10x` | 0.099 |
| `C >= 100x` | 0.0099 |
| `C = 10,000x` (cap) | 0.0000990 (about 1 round in 10,101) |
| median | exactly 1.9800x (`P(C >= 19,800) = 1/2`, reached at `r = 2^63`) |

**House edge and expected return.** A cash-out at tick `n` survives iff `C > M(n)` (D-9). Its
expected return per credit, before payout flooring, is

`RTP(n) = (M(n) / S) * floor(9,900 * 2^64 / (M(n) + 1)) / 2^64`, approximately `0.99 * M(n) / (M(n) + 1)`.

Over every reachable tick (0 to 15,350):

- minimum `RTP = 0.98990100989901009897`, at tick 0 (cash-out at 1.0000x);
- maximum `RTP = 0.98999999009662949582`, at tick 15,350 (9,996.5968x).

**The house edge is between 1.0000010% and 1.0098990% for every cash-out target, before payout
flooring.** Payout flooring (D-8) keeps a further fraction of a credit below 1 per winning cash-out.
A bet that never cashes out returns nothing. The cap does not change any `RTP(n)`, because no
cash-out can happen at or above the cap.

**Rationale.** Inverse-transform sampling of a 1/x tail gives the same edge at every target, so no
cash-out strategy beats another apart from the grid and flooring effects listed. The formula is
integer-only: anyone can verify it with big integers, without floats or transcendental functions.
64 bits of `r` give a probability resolution of 2^-64 (about 5.4e-20), far below the smallest
probability that matters (9.9e-5 at the cap).

**Alternatives considered.**
- `h = 0%` (numerator 10,000): no credit drain, `RTP` about `M / (M + 1)`. A reasonable pick if the
  simulated economy should not drain.
- `h = 3%` (numerator 9,700).
- The bustabit-style scheme (1 instant crash in 101, then 1/x): uneven edge across targets and
  harder to state exactly.
- 52-bit `r` (a JavaScript float legacy): no reason for it in Go integer code.
- Full 256-bit `r`: 256-bit arithmetic for no measurable change in probabilities.
- No cap: `raw` reaches 1.8e23x, so duration and overflow become unbounded.

### D-6. Max multiplier and max duration

**Proposal.**
- Cap `C_max = 100,000,000` (10,000.0000x).
- Max crash tick `n_max = 15,351`, the first tick with `M(n) >= C_max` (`M(15,351) = 100,025,966`).
- Max running duration: `15,351 * Q = 153,510,000 µs` (153.51 s) after `T0`.
- Highest payable multiplier: `M(15,350) = 99,965,968` (9,996.5968x).

The betting-window length is a runtime parameter outside this spec (LBT-8, LBT-11).

**Rationale.** Bounds round length and all arithmetic. The cap is hit about once per 10,101 rounds.

**Alternatives considered.**
- 1,000x: `n_max = 11,513` (115.13 s). The cap shows up about once per 1,010 rounds, often enough to
  be noticed.
- 1,000,000x: `n_max = 23,026` (230.26 s). Max bet times max multiplier reaches 1e19, past
  `MaxInt64`, so the max bet would have to fall below 9.2e8 credits.

### D-7. Limits and overflow behaviour

**Hard limits of `crash/v1`.** Runtime configuration may narrow them, never widen them.

| quantity | min | max |
|---|---|---|
| bet (credits, int64) | 1 | 1,000,000,000 |
| crash point `C` (scaled) | 10,000 | 100,000,000 |
| tick `n` accepted by `M(n)` | 0 | 15,351 |
| crash tick `n_c` | 0 | 15,351 |
| deadline offset `D` (µs) | 0 | 153,510,000 |
| multiplier accepted by `Payout` (scaled) | 10,000 | 99,999,999 (highest reachable: 99,965,968) |
| payout (credits) | 1 | 9,996,596,800,000 (reachable); 9,999,999,900,000 (domain bound) |
| `T0` (µs since epoch) | 0 | `MaxInt64 - 153,510,000` |
| `t_a` (µs since epoch) | 0 | `MaxInt64` |

**Headroom.** The largest intermediate product is `1,000,000,000 * 99,999,999 =
99,999,999,000,000,000`, 92 times below `MaxInt64`. Payouts, multipliers, ticks and offsets are all
below `2^53` (9,007,199,254,740,992), so they survive JSON and JavaScript number handling;
epoch-microsecond timestamps stay below `2^53` until the year 2255.

**Overflow behaviour.**
- Every v1 function checks its inputs against this table and returns `ErrOutOfRange` outside it.
  Inside the table, overflow is impossible (bounds above).
- Every int64 multiply, add and subtract is checked anyway. A detected overflow returns
  `ErrArithmetic`. Nothing panics, wraps, saturates or clamps. The one clamp in v1 is the crash-point
  clamp in D-5, which is part of the distribution.
- Conversions are checked: apd `Decimal` to int64 through `Int64()`; `apd.BigInt` to int64 only after
  the clamp; the uint64 `r` is never cast to int64.
- Callers (LBT-8 `Decide`) turn `ErrOutOfRange` into a terminal command rejection and treat
  `ErrArithmetic` as fail-closed: no events.
- Aggregates (round totals, balances) belong to LBT-12 and need checked addition. int64 holds about
  922,000 maximum payouts, so LBT-8 or LBT-12 should bound bets per round or check every sum.

**Alternatives considered.** Saturating arithmetic (hides bugs; ADR-004 asks for checked
arithmetic). Big-integer payouts (unnecessary inside the limits). A smaller max bet (fine;
operator configuration can narrow it).

### D-8. Payout

**Proposal.** `payout = floor(bet * m / 10,000)`. `bet` and `m` are validated (D-7). The product is
computed exactly with a checked int64 multiply (`math/bits.Mul64` on the validated non-negative
values, rejecting `hi != 0` or `lo > MaxInt64`), followed by one integer division. That is one
rounding step, at the end, toward zero, which equals floor because every value is non-negative. The
house keeps the remainder (under 1 credit). A cash-out at tick 0 (`m = 10,000`) pays the bet back
exactly. Profit and ledger postings belong to LBT-12.

**Rationale.** This is ADR-004's formula. The only choices left are the mechanism and the order of
operations.

**Alternatives considered.** Half-even rounding (pays above the exact value; contradicts ADR-004).
`bet * floor(m / S)` (drops the fractional multiplier). apd for the product (same result, needless
for integers).

### D-9. Crash deadline and cash-out eligibility

**Proposal.**
- `T0` is the effective admission time recorded for the round start (the `StartRound` command,
  ADR-002).
- `n_c = min{ n in [0, 15,351] : M(n) >= C }`. It always exists because `M(15,351) >= C_max >= C`.
  Compute it by binary search over `M` (at most 14 `Exp` calls). Memoizing `M` is allowed.
- `D = n_c * Q` microseconds. The absolute crash deadline is `T0 + D` (checked).
- A cash-out with effective admission time `t_a` and `e = t_a - T0` is timing-eligible iff
  `0 < e < D`. Then `n = floor(e / Q) <= n_c - 1`, so `M(n) < C`.
- `e = D` is ineligible: deadline equality loses (ADR-002, ADR-004). `e > D` is ineligible.
  `e <= 0` is ineligible: not after round start.
- `C = 1.0000x` gives `n_c = 0` and `D = 0`: no cash-out can be eligible.
- A tick whose multiplier equals `C` is never payable. Example: `C = 10,006 = M(1)` gives `n_c = 1`,
  so only tick 0 pays.
- Phase checks, bet existence and one cash-out per bet belong to LBT-8.

**Rationale.** The deadline falls on a tick boundary, so time, eligibility and payout stay in
integers, and "survives iff `M(n) < C`" holds by construction instead of depending on `Ln` agreeing
with `Exp`.

**Sub-decision: the start boundary.** Strict `e > 0` follows ADR-002's "after round start"
literally. The alternative, `e >= 0`, would let a cash-out admitted in the same microsecond as the
start pay 1.0000x (the bet back). Either is harmless; strict is proposed.

**Alternatives considered.**
- `n_c = min{n : M(n) > C}`: would make a tick paying exactly `C` eligible, which is a cash-out at
  the crash (ADR-003 invariant "no cash-out at/after crash").
- A continuous deadline `T0 + ceil(ln(C / S) / g * Q)`: needs `Ln`, and the final partial tick
  behaves inconsistently with the tick multiplier.

### D-10. Seed, commitment and outcome derivation

**Proposal.**
1. **Seed.** 32 bytes from `crypto/rand`, generated by the runtime when the round is created (never
   inside `Decide`, ADR-003), recorded as the round's deterministic input, unique per round, and
   secret until settlement. If `crypto/rand` fails, round creation fails.
2. **Commitment.** `SHA-256(seed)`, 32 bytes, published as 64 lowercase hex characters before the
   round accepts bets.
3. **Message.** The ASCII bytes of `orvex/crash/v1/crash-point:` followed by the ASCII bytes of the
   round ID's canonical text (D-11). The prefix is fixed and the only variable field comes last, so
   the encoding is unambiguous without length prefixes.
4. **Outcome hash.** `HMAC-SHA256(key = seed, message)`.
5. **`r`.** Bytes 0 to 7 of the outcome hash, read as a big-endian uint64.
6. `C`, `n_c` and `D` follow from D-5 and D-9.
7. **Reveal.** The seed (64 lowercase hex) is published after `RoundSettled`. Verification:
   `SHA-256(seed) == commitment`, then recompute `r`, `C`, `n_c`, `D` and compare with the recorded
   events.

**Versioning.** The message prefix carries the rules version. A future `crash/v2` must use its own
prefix and its own code path. Every round records `rulesVersion = "crash/v1"` (ADR-003); the audit
verifier selects the derivation by that field and fails closed on unknown versions (ADR-006).

**Worked example** (computed with Python, cross-checked with `openssl` and `sha256sum`):

| field | value |
|---|---|
| seed | `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f` |
| commitment | `630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd` |
| round ID | `0190f3a8-7b3c-7d4e-8f00-000000000001` |
| message | `orvex/crash/v1/crash-point:0190f3a8-7b3c-7d4e-8f00-000000000001` |
| outcome hash | `6fd00d5e48f2092aca65a8c3a649b50a498716e8457736d38584c9ec964fe675` |
| `r` | `0x6fd00d5e48f2092a` = 8,056,954,431,967,725,866 |
| `raw` = `C` | 17,577 (1.7577x) |
| `n_c`, `D` | 941, 9,410,000 µs (9.41 s) |
| last payable tick | 940, `M(940) = 17,576` |

Anyone can verify a revealed round with stock tools:

```sh
printf '%s' 'orvex/crash/v1/crash-point:<roundID>' | openssl dgst -sha256 -mac HMAC -macopt hexkey:<seedHex>
printf '%s' '<seedHex>' | xxd -r -p | sha256sum
```

**Rationale.** Follows ADR-004's fairness section exactly, with standard primitives that stock tools
can check. HMAC is the standard keyed construction; the round ID binds the outcome to its round.

**Alternatives considered.**
- Commitment `SHA-256(tag || roundID || seed)`: domain-separated and tied to the round, but it breaks
  the plain `sha256sum` check and departs from ADR-004's wording. The seed is 256-bit random and
  single-use, so the plain hash already hides and binds it.
- `SHA-256(seed || roundID)` instead of HMAC: workable, but an ad hoc construction where a standard
  one exists.
- HKDF-Expand: more machinery for one 64-bit value.
- Mixing in a client seed or a public randomness beacon: defends against operator seed grinding,
  which ADR-004 leaves open on purpose ("proves consistency with the committed seed, not impartial
  seed selection"). Needs an ADR-004 amendment.

### D-11. Round identity encoding

**Proposal.** Round IDs are UUIDs (16 bytes). The outcome message uses their canonical text: 36
lowercase ASCII characters, hex groups 8-4-4-4-12 joined by hyphens (RFC 9562, section 4). The v1
function takes the 16 bytes and formats them itself, so callers cannot pass upper-case or braced
variants. Any 16 bytes are accepted; version and variant bits are not checked.

**Dependency.** LBT-8 and LBT-10 own the round ID type. If they pick something other than a UUID,
this decision reopens before v1 is frozen.

**Alternatives considered.** Raw 16 bytes in the message (cannot be checked with a shell one-liner
from a JSON reveal). An int64 sequence as decimal text (leaks round counts and ties outcomes to
database sequence allocation). A length-prefixed binary encoding (only needed with several variable
fields).

### D-12. Canonical encoding of numeric outputs

**Proposal.**
- Authoritative numeric outputs are int64 values in fixed units, named with their unit:
  `crashPointScaled`, `crashTick`, `crashDeadlineOffsetMicros` (and the absolute deadline in
  microseconds), `cashOutTick`, `cashOutMultiplierScaled`, `payoutCredits`, `betCredits`, and
  effective times in microseconds since the Unix epoch, UTC. They are stored in events (ADR-004)
  together with `rulesVersion = "crash/v1"`. Event schemas themselves belong to LBT-8 and LBT-9.
- Canonical text form of an integer: base-10 ASCII matching `^(0|-?[1-9][0-9]*)$`. No plus sign,
  leading zeros, exponent, decimal point or `-0`.
- Byte strings (seed, commitment, outcome hash): lowercase hex of fixed length (64 characters for 32
  bytes). `r` is derivable, so it is not stored; where it appears (vectors), it is 16 lowercase hex
  characters.
- Display form of a multiplier: `m / 10,000` with exactly four fractional digits (`1.7577`); a UI may
  truncate to two. Presentation only: never parsed back, never hashed, never authoritative.
- No floats anywhere in v1 arithmetic, events or vectors, and no apd decimal text in events.
- The event codec (LBT-9) picks the wire format but must round-trip these int64 values exactly.

**Alternatives considered.** Decimal strings such as `"1.7577"` as the authoritative form (one value
has several spellings: `1.7577`, `1.75770`, `17577E-4`). apd's own text output (exponent notation
varies with coefficient and exponent). Floats (forbidden by determinism).

### D-13. apd dependency pin: `github.com/cockroachdb/apd/v3 v3.2.3`

**Proposal.** Pin `github.com/cockroachdb/apd/v3 v3.2.3`.

| item | value |
|---|---|
| tag | `v3.2.3`, released 2026-03-23 |
| commit | `6d9c587326e78bcfbea630bf52893ff45f6f9ed5` |
| `go.sum` module hash | `h1:4Zx+I3R35bFXMnltzmjP79i2cravE4jTRL6ps9Aux80=` |
| `go.sum` go.mod hash | `h1:klXJcjp+FffLTHlhIG69tezTDvdP065naDsHzKhYSqc=` |
| apd's own `go.mod` | `go 1.17`; requires `github.com/lib/pq v1.10.7`, used only by apd's `sql_test.go` |

`go mod tidy` adds two `lib/pq` lines to `go.sum` (checked in a scratch module). `lib/pq` is not
compiled into Orvex binaries.

**Rationale.** v3.2.3 is the latest release. Compared with v3.2.1 (2023-09), v3.2.2 and v3.2.3
fix `BigInt` `Mul`/`Quo`/`Rem` returning negative zero, a shadowed
variable in `NumDigits` for negative values (`table.go`), and `NewFromString` accepting malformed
input such as `.-5`. None of these touch the positive-only v1 path, but freezing an immutable
artifact on the newest release avoids carrying known bugs. Measured: with v3.2.3 and the D-4
context, `M(n)` matches the independent oracle at all 15,352 ticks.

**Alternatives considered.** v3.2.1 (older; carries the bugs above). v3.2.2 (lacks the `NumDigits` fix). Floating on the latest version
(ruled out for historical rules by ADR-007).

**Upgrade policy after release.** An apd upgrade is allowed only if the full v1 golden suite,
including the exhaustive tick table, passes unchanged.

### D-14. Golden vectors produced independently

**Proposal.** A Python 3 standard-library generator (`decimal`, backed by libmpdec, whose `exp` is
documented as correctly rounded; `hashlib`; `hmac`; `json`) produces the vectors from this spec. It
never imports, runs or reads Go code. A `make golden-check` target regenerates the vectors in memory
and byte-compares them with the committed files; CI runs it. Section 8 has the details.

**Rationale.** A different language and a different decimal library (libmpdec, not apd) catch apd
bugs, Go transcription errors and parameter typos. Python ships on the CI image (`ubuntu-latest`).

**Alternatives considered.**
- A Go oracle with `math/big`: same language, and `math/big` has no `exp`, so it would mean
  hand-writing a series; weaker independence.
- `bc -l` plus `openssl`: workable, but the shell glue for floors, hex and JSON output is brittle.
- Vectors produced by the Go implementation itself: not independent, which LBT-7 rules out.
- Committed vectors without `golden-check` in CI: cheaper, but generator drift goes unnoticed.

## 5. Normative algorithm (`crash/v1`)

Inputs: `seed` (32 bytes), `roundID` (16 bytes), `T0`, and per cash-out `t_a` and `bet`.

1. `commitment = SHA-256(seed)`; publish it before betting opens.
2. `msg = "orvex/crash/v1/crash-point:" || canonicalText(roundID)`.
3. `h = HMAC-SHA256(seed, msg)`; `r = uint64be(h[0:8])`.
4. `raw = floor(9,900 * 2^64 / (2^64 - r))`; `C = min(100,000,000, max(10,000, raw))`.
5. `M(n) = floor(10,000 * exp(0.0006 * n))` per D-4, for `n` in `[0, 15,351]`.
6. `n_c = min{n : M(n) >= C}`; `D = n_c * 10,000 µs`; crash deadline `T0 + D`.
7. Cash-out: `e = t_a - T0`; eligible iff `0 < e < D`; `n = floor(e / 10,000)`; `m = M(n)`.
8. `payout = floor(bet * m / 10,000)`.
9. Record: `rulesVersion`, commitment (before betting), `C` and `n_c` (kept out of the public
   projection until the crash, ADR-003), cash-out tick, `m`, payout; the seed after settlement.

## 6. Evidence

Computed during planning in scratch files outside the repository. They are not deliverables:

- `/home/juano/Workspaces/LBTWorks/Orvex/odin/data/sessions/e96d124d/scratch/check.py` and
  `bounds.py`: Python 3.14.6, `decimal` with libmpdec 4.0.0, precision 60, 80 and 120.
- `scratch/apdcheck/main.go`: apd v3.2.3 with the D-4 context, all 15,352 ticks, compared line by
  line with `scratch/oracle.py`.

| claim | result |
|---|---|
| `M(n)` at precision 60 equals precision 120, all ticks | yes |
| `M(n)` from apd v3.2.3 (D-4) equals the oracle, all ticks | yes, 15,352 of 15,352 |
| correctly rounded 34-digit `exp`, then floor, equals the high-precision floor, all ticks | yes |
| apd 34-digit `exp` intermediate equals the correctly rounded value | no, differs in the last digit at 105 ticks |
| closest approach of `S * e^(g n)` to an integer | 3.7266e-5, tick 2,710 |
| `M` strictly increasing | yes; smallest step 6 |
| `M(0)`, `M(15,350)`, `M(15,351)` | 10,000; 99,965,968; 100,025,966 |
| `RTP` minimum and maximum over ticks 0 to 15,350 | 0.98990100989901009897 (tick 0); 0.98999999009662949582 (tick 15,350) |
| `P(C = 1.0000x)` | 0.0100989901009899 |
| `P(C = cap)` | 0.0000989999999999999669 |
| max payout, max product | 9,996,596,800,000; 99,965,968,000,000,000 (below `MaxInt64`) |
| worked example (D-10) via `openssl dgst -hmac` and `sha256sum` | matches |

## 7. Security notes

- The seed stays confidential until `RoundSettled`: never in the public projection (ADR-003), logs,
  error messages or client-visible events.
- `C`, `n_c` and `D` determine each other, so the absolute deadline is as secret as the crash point
  and must stay out of the public projection until `RoundCrashed`.
- Seeds come from `crypto/rand` only. There is no fallback to `math/rand`.
- Seeds are single-use. The audit verifier (LBT-9) should reject duplicate commitments across rounds.
- Commit/reveal proves consistency with the committed seed, not impartial seed choice (ADR-004).
  Operator seed grinding remains possible; closing it needs an ADR change (D-10 alternatives).
- Commitment verification compares public values, so a plain byte comparison is fine.

## 8. Golden vector plan

### 8.1 Independence rules

1. The generator is Python 3.11 or newer, standard library only. It does not import, execute or read
   Go code, `go.mod`, or the Go constants.
2. Parameters are typed into the generator from this spec, separately from the Go constants. The Go
   test `TestGoldenParams` compares the two transcriptions, so a typo on either side fails.
3. Curve values are computed at precision 80 and re-checked at precision 120. The two must agree at
   every tick. The generator also records the closest approach to an integer and fails if it is
   below 1e-6.
4. The generator and the vectors land in their own commit, before any v1 Go code, written by a
   different agent than the Go code where the pipeline allows.
5. The vectors are never regenerated from Go output. When Go and a vector disagree, the side that
   contradicts this spec is fixed.
6. Third-tool cross-check: the generator's docstring lists the `openssl` and `sha256sum` commands
   for the first fairness row (D-10), and the implementation run executes them once.

### 8.2 Files

Directory `internal/game/crash/v1/testdata/golden/` (Go ignores `testdata`):

| file | purpose |
|---|---|
| `generate.py` | the oracle; `--write` writes the JSON files, `--check` regenerates in memory and byte-compares, exiting 1 on any difference |
| `params.json` | the parameter manifest (below) |
| `multiplier_ticks.json` | `M(n)` for every tick |
| `crash_points.json` | `r` to `raw`, `C`, `n_c`, `D` |
| `fairness.json` | seed and round ID to commitment, message, hash, `r`, `C`, `n_c`, `D`; commitment checks |
| `deadlines.json` | timing eligibility around deadlines |
| `payouts.json` | payout rounding and rejected inputs |

Format rules: UTF-8 JSON, fixed key order, indent 1, LF line endings, trailing newline, nothing
environment-dependent (no timestamps, no Python version). Every file has a top-level
`"rulesVersion": "crash/v1"`. Integers that fit int64 are JSON numbers. Values that can exceed int64
(`raw`) are base-10 strings. Byte strings are lowercase hex. Expected failures are
`"error": "out_of_range"`.

`params.json` holds: `rulesVersion`, `scale` 10000, `tickMicros` 10000, `growthPerTick` `"0.0006"`,
`edgeNumerator` 9900, `minCrashPointScaled` 10000, `maxCrashPointScaled` 100000000, `maxTick` 15351,
`maxDurationMicros` 153510000, `minBetCredits` 1, `maxBetCredits` 1000000000, `maxPayoutCredits`
9996596800000, `maxStartMicros` (`MaxInt64 - 153510000`), `outcomeTag`
`"orvex/crash/v1/crash-point:"`, `apdModule`, `apdVersion` `"v3.2.3"`, the D-4 context fields, the
oracle precisions with the recorded minimum margin and its tick, and `rtpMin`/`rtpMax` rounded half-even to 20 decimal places,
as decimal strings, with their ticks.

### 8.3 Cases

LBT-7 acceptance requires deadlines, rounding, overflow and versioned outcomes. Each case below is
tagged with what it covers.

**`multiplier_ticks.json`** (rounding, exhaustive): `multipliersScaled[n]` for every `n` from 0 to
15,351, 15,352 values.

**`crash_points.json`** (rounding, overflow, clamps). Each row gives `rHex`, `raw` (string), `C`,
`n_c`, `D`:

| case | `r` | expected |
|---|---|---|
| zero (denominator `2^64`) | `0000000000000000` | raw 9,900, C 10,000 |
| just below raw 10,000 | `028f5c28f5c28f5c` | raw 9,999, C 10,000 |
| first raw 10,000 | `028f5c28f5c28f5d` | raw 10,000, C 10,000 |
| last instant crash | `0295d8f034d54722` | raw 10,000, C 10,000 |
| first non-instant | `0295d8f034d54723` | raw 10,001, C 10,001, n_c 1 |
| int64 sign boundary, below | `7fffffffffffffff` | raw 19,799 |
| int64 sign boundary, exact division | `8000000000000000` | raw 19,800 (median) |
| just below cap | `fff9830e3bc2fc1e` | raw 99,999,999, C 99,999,999 |
| exactly cap | `fff9830e3cd9a523` | raw 100,000,000, C 100,000,000, n_c 15,351 |
| above cap | `fff9830e3df04e27` | raw 100,000,001, C 100,000,000 |
| raw reaches `2^63` (overflow) | `ffffffffffffb2a8` | raw 9,223,372,036,854,775,808, C 100,000,000 |
| raw reaches `2^64` (overflow) | `ffffffffffffd954` | raw 18,446,744,073,709,551,616, C 100,000,000 |
| max `r` (overflow) | `ffffffffffffffff` | raw 182,622,766,329,724,560,998,400, C 100,000,000 |
| spread | 1,000 rows, `r_i` = first 8 bytes of `SHA-256("orvex/crash/v1/vector-r/" + decimal(i))` | per formula |

**`fairness.json`** (versioned outcomes). Each row gives `seedHex`, `roundID`, `commitmentHex`,
`messageHex` (pins the version tag and the round ID text), `outcomeHashHex`, `rHex`, `C`, `n_c`, `D`:
- the D-10 worked example;
- seed all zeros, and seed all `ff`;
- one seed with two round IDs (`...0001`, `...0002`), giving different outcomes;
- 64 rows with seed `SHA-256("orvex/crash/v1/vector-seed/" + decimal(i))` and round ID from the first
  16 bytes of `SHA-256("orvex/crash/v1/vector-round/" + decimal(i))`, which covers non-v4/v7 bit
  patterns;
- `commitmentChecks`: the worked example's seed against its commitment (`matches: true`) and against
  the same commitment with one bit flipped (`matches: false`).

**`deadlines.json`** (deadlines, overflow). Rows give `crashPointScaled`, `crashTick`,
`startMicros`, `effectiveMicros`, then `eligible`, `tick`, `multiplierScaled`, or an `error`.
`T0 = 1,760,000,000,000,000` unless stated.

| C | n_c, D (µs) | elapsed `e` values and expectations |
|---|---|---|
| 10,000 | 0, 0 | `e = -1, 0, 1`: all ineligible |
| 10,001 | 1, 10,000 | `e = 0`: ineligible; `1` and `9,999`: tick 0, m 10,000; `10,000` (equality): ineligible; `10,001`: ineligible |
| 10,006 (equals `M(1)`) | 1, 10,000 | `9,999`: tick 0; `10,000`: ineligible (a multiplier equal to C is never paid) |
| 10,007 | 2, 20,000 | `10,000`: tick 1, m 10,006; `19,999`: tick 1; `20,000`: ineligible |
| 17,577 (worked example) | 941, 9,410,000 | `9,409,999`: tick 940, m 17,576 (one unit below C); `9,410,000`: ineligible |
| 100,000,000 | 15,351, 153,510,000 | `153,509,999`: tick 15,350, m 99,965,968; `153,510,000`: ineligible |

Domain and overflow rows: `startMicros = -1`, `effectiveMicros = -1`, `crashTick = -1`,
`crashTick = 15,352`, and `startMicros = MaxInt64 - 153,510,000 + 1` all give `out_of_range`.
`startMicros = 0, effectiveMicros = MaxInt64, crashTick = 15,351` is ineligible with no error
(`e` is large but fits int64).

**`payouts.json`** (rounding, overflow). Rows give `betCredits`, `multiplierScaled`, then
`payoutCredits` or an `error`:

| bet | m | expected | covers |
|---|---|---|---|
| 1 | 10,000 | 1 | tick 0 returns the bet |
| 1 | 19,999 | 1 | floor of 1.9999 |
| 1 | 20,000 | 2 | exact |
| 3 | 10,006 | 3 | floor of 3.0018 |
| 7 | 14,286 | 10 | floor of 10.0002 |
| 9,999 | 10,001 | 9,999 | floor of 9,999.9999 |
| 10,000 | 12,345 | 12,345 | exact |
| 123,456,789 | 17,576 | generator computes | mixed |
| 1,000,000,000 | 10,000 | 1,000,000,000 | max bet at 1x |
| 1,000,000,000 | 99,965,968 | 9,996,596,800,000 | highest reachable payout |
| 1,000,000,000 | 99,999,999 | 9,999,999,900,000 | domain maximum |
| 0, -1, 1,000,000,001 | 10,000 | `out_of_range` | bet bounds |
| 1 | 9,999, 0, -1, 100,000,000, `MaxInt64` | `out_of_range` | multiplier bounds |
| `MaxInt64` | 99,999,999 | `out_of_range` | naive int64 product would wrap |
| 4,611,686,018,427,387,904 (`2^62`) | 99,999,999 | `out_of_range` | naive int64 product would wrap |

### 8.4 Generator self-checks

The generator asserts all of these before writing anything:
- precision 80 and precision 120 agree on every `M(n)`; the minimum margin is at least 1e-6;
- `M(0) = 10,000`, `M` strictly increasing, `M(15,350) < 100,000,000 <= M(15,351)`;
- every `RTP(n)` for `n` from 0 to 15,350 lies in `[0.9899, 0.99)`;
- every JSON-number integer fits int64, and every stored output is below `2^53`.

### 8.5 Change policy

Until the first production release of `crash/v1`, a change to these vectors needs a change to this
spec and re-approval. After release, the v1 vectors, generator and parameters are immutable (ADR-001,
ADR-003, ADR-007, AGENTS.md); new rules go in a new version with its own tag and files.

## 9. Implementation plan after approval (separate run)

Use only approved values. If the engineer edits a decision, update this spec first.

### 9.1 Order

1. Commit A: `generate.py` and the six JSON files (section 8). Run `generate.py --write`, then
   `--check`.
2. Commit B: `go.mod`/`go.sum` (apd pin), the v1 Go package and its tests, `Makefile`, CI, README and
   AGENTS.md updates. `go mod tidy` drops an unused requirement, so the pin lands together with the
   code that imports apd.

### 9.2 Files

| file | change |
|---|---|
| `go.mod`, `go.sum` | `go get github.com/cockroachdb/apd/v3@v3.2.3`, then `go mod tidy`. Expect the apd require line plus four `go.sum` lines (apd and lib/pq, D-13). |
| `internal/game/crash/v1/params.go` | package doc; constants; frozen context; `ArithmeticContext()`; `ErrOutOfRange`, `ErrArithmetic` |
| `internal/game/crash/v1/curve.go` | `MultiplierAtTick`, `CrashTick`, `DeadlineOffsetMicros`, `CashOutTiming` |
| `internal/game/crash/v1/payout.go` | `Payout` |
| `internal/game/crash/v1/fairness.go` | `Commitment`, `VerifyCommitment`, `OutcomeMessage`, `OutcomeR`, `CrashPointFromR`, `CrashPoint` |
| `internal/game/crash/v1/testdata/golden/*` | from commit A |
| `internal/game/crash/v1/*_test.go` | section 9.4 |
| `Makefile` | `PYTHON ?= python3`; target `golden-check` running `$(PYTHON) internal/game/crash/v1/testdata/golden/generate.py --check`; add it to `.PHONY` |
| `.github/workflows/ci.yml` | in job `test`, after `make test`: `- run: make golden-check` |
| `AGENTS.md` | Commands: add `make golden-check` |
| `README.md` | Layout: add `internal/game/crash/v1`; Development: add `make golden-check`; update the status line, which says no game logic exists |

The package is `package v1` in `internal/game/crash/v1/` (ADR-007 example path). Importers alias it,
for example `crashv1`. It imports only the standard library and apd. It holds no state, does no I/O
and reads no clock, so LBT-8's `Decide` can call it (ADR-003).

### 9.3 API

Constants: `RulesVersion = "crash/v1"`, `Scale = 10_000`, `TickMicros = 10_000`,
`EdgeNumerator = 9_900`, `MinCrashPointScaled = 10_000`, `MaxCrashPointScaled = 100_000_000`,
`MaxTick = 15_351`, `MaxDurationMicros = 153_510_000`, `MinBetCredits = 1`,
`MaxBetCredits = 1_000_000_000`, `MaxPayoutCredits = 9_996_596_800_000`,
`MaxStartMicros = math.MaxInt64 - MaxDurationMicros`, `OutcomeTag = "orvex/crash/v1/crash-point:"`.
The growth step stays unexported as the exact decimal `6E-4`.

| function | domain | result |
|---|---|---|
| `ArithmeticContext() apd.Context` | none | copy of the D-4 context |
| `MultiplierAtTick(n int64) (int64, error)` | `n` in `[0, MaxTick]` | `M(n)` |
| `CrashTick(crashPointScaled int64) (int64, error)` | `[MinCrashPointScaled, MaxCrashPointScaled]` | `n_c` |
| `DeadlineOffsetMicros(crashTick int64) (int64, error)` | `[0, MaxTick]` | `crashTick * TickMicros` |
| `CashOutTiming(startMicros, effectiveMicros, crashTick int64) (tick int64, eligible bool, err error)` | D-7 table | eligibility and tick (`0, false` when ineligible) |
| `Payout(betCredits, multiplierScaled int64) (int64, error)` | bet `[1, MaxBetCredits]`, m `[Scale, MaxCrashPointScaled-1]` | payout |
| `Commitment(seed [32]byte) [32]byte` | any | `SHA-256(seed)` |
| `VerifyCommitment(seed, commitment [32]byte) bool` | any | equality |
| `OutcomeMessage(roundID [16]byte) []byte` | any | tag plus canonical text |
| `OutcomeR(seed [32]byte, roundID [16]byte) uint64` | any | `r` |
| `CrashPointFromR(r uint64) int64` | any | `C` (exact `apd.BigInt`, clamp, then narrow) |
| `CrashPoint(seed [32]byte, roundID [16]byte) int64` | any | `C` |

Errors wrap the two sentinels with `fmt.Errorf("%w: ...")`; tests use `errors.Is`.

### 9.4 Tests (each is required)

`internal/game/crash/v1/golden_test.go` (a loader shared by all golden tests rejects any file whose
`rulesVersion` is not `RulesVersion`):
- `TestGoldenParams`: `params.json` equals the Go constants and `ArithmeticContext()`; `apdVersion`
  equals the module version in the test binary's build info.
- `TestGoldenMultiplierTable`: `MultiplierAtTick(n)` equals the vector for all 15,352 ticks.
- `TestGoldenCrashPoints`: `CrashPointFromR`, `CrashTick` and `DeadlineOffsetMicros` match every row.
- `TestGoldenFairness`: `Commitment`, `OutcomeMessage` (hex), `OutcomeR`, `CrashPoint`, `CrashTick` and
  the deadline match every row; `VerifyCommitment` matches every `commitmentChecks` row.
- `TestGoldenDeadlines`: `CashOutTiming` matches every row, including `out_of_range` rows; for
  eligible rows `MultiplierAtTick(tick)` equals `multiplierScaled`.
- `TestGoldenPayouts`: `Payout` matches every row; error rows satisfy `errors.Is(err, ErrOutOfRange)`.
- `TestGoldenRejectsUnknownRulesVersion`: the loader returns an error for an inline document with
  `"rulesVersion": "crash/v2"`.

`internal/game/crash/v1/curve_test.go`:
- `TestArithmeticContextFrozen`: precision 34, exponents 6144/-6143, `RoundHalfEven`,
  `apd.DefaultTraps`; mutating the returned copy does not change the next call's result.
- `TestApdVersionPinned`: `debug.ReadBuildInfo()` lists `github.com/cockroachdb/apd/v3` at `v3.2.3`
  (confirmed to work in test binaries during planning).
- `TestMultiplierStrictlyIncreasing`: `M(n+1) > M(n)` for every tick.
- `TestMultiplierAtTickOutOfRange`: `-1`, `15,352`, `math.MinInt64`, `math.MaxInt64` return
  `ErrOutOfRange`.
- `TestCrashTickBracketsCrashPoint`: for `C` in {10,000, 10,001, 10,006, 17,577, 19,800, 100,000,000},
  `M(n_c - 1) < C <= M(n_c)` (only the right side when `n_c = 0`).
- `TestCashOutTimingDeadlineEqualityLoses`: `e = D` is ineligible and `e = D - 1` is eligible, for
  several `C`.
- `TestHouseEdgeBounds`: with `math/big.Rat`, computes `RTP(n)` for every `n` from 0 to 15,350. The minimum is
  at tick 0 and the maximum at tick 15,350; rounded half-even to 20 decimal places they equal `params.json`
  `rtpMin` (`0.98990100989901009897`) and `rtpMax` (`0.98999999009662949582`).

`internal/game/crash/v1/payout_test.go`:
- `TestPayoutTickZeroReturnsBet`: `Payout(b, 10,000) == b` for b in {1, 7, 1,000,000,000}.
- `TestPayoutRejectsOutOfRange`: the bet and multiplier bounds of D-7, including inputs whose naive
  int64 product would wrap.

`internal/game/crash/v1/fairness_test.go`:
- `TestOutcomeMessageCanonicalText`: the message is the tag followed by exactly 36 lowercase
  characters in 8-4-4-4-12 form, for an ID with high bits set (`ff...`).

`internal/game/crash/v1/fuzz_test.go` (seed corpora run under `make test`):
- `FuzzPayout`: never panics; returns `ErrOutOfRange` exactly when inputs are outside the domain;
  otherwise equals a `math/big` reference.
- `FuzzCrashPointFromR`: result in `[10,000, 100,000,000]` and equal to a `math/big` reference of the
  D-5 formula.
- `FuzzCashOutTiming`: `eligible` iff inputs are in the domain and `0 < e < crashTick * TickMicros`;
  when eligible, `tick < crashTick`.

### 9.5 Verification (each must exit 0)

```sh
python3 internal/game/crash/v1/testdata/golden/generate.py --check
make golden-check
make test
make lint
go vet ./...
make build
go mod verify
go list -m github.com/cockroachdb/apd/v3   # prints v3.2.3
```

Also run the two D-10 shell commands and confirm they print the worked example's outcome hash and
commitment, as stored in `fairness.json`.

## 10. Open items and dependencies

1. Round ID type (D-11): owned by LBT-8 and LBT-10. A non-UUID choice reopens D-11.
2. `T0` (D-9) is the `StartRound` effective admission time: LBT-8 records it, LBT-11 produces it at
   microsecond resolution.
3. Seed storage and protection at rest: LBT-10 and LBT-12.
4. Aggregate sums and a possible per-round bet cap (D-7): LBT-8 and LBT-12.
5. ADR-004 amendment recording the frozen values after the first release (section 1.3).
6. Optional later: run the golden tests on an arm64 CI runner, given apd's `float64` term-count
   estimate (D-4). The measured margin makes divergence very unlikely, so this is not part of
   section 9 unless the engineer asks for it.

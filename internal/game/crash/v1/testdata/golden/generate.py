#!/usr/bin/env python3
"""Independent golden-vector oracle for Crash rules ``crash/v1``.

Purpose
    This script produces the golden vectors that pin the arithmetic and the
    fairness derivation of ``crash/v1`` (spec
    ``docs/specs/e96d124d_crash-v1-math-spec.md``, section 8; ADR-004, ADR-006).
    The Go package ``internal/game/crash/v1`` is tested against the JSON files
    it writes.

Independence rules (spec section 8.1)
    1. Python 3.11 or newer, standard library only. This script never imports,
       executes or reads Go code, ``go.mod`` or the Go constants.
    2. Every parameter below is typed in from the spec, separately from the Go
       constants. The Go test ``TestGoldenParams`` compares both transcriptions.
    3. Curve values are computed with ``decimal`` (libmpdec) at precision 80 and
       re-checked at precision 120; both must agree at every tick. The closest
       approach of the exact curve to an integer is recorded and must be at
       least 1e-6.
    4. The vectors are never regenerated from Go output. When Go and a vector
       disagree, the side that contradicts the spec is fixed.
    5. After the first production release of ``crash/v1`` this script, its
       parameters and its output are immutable (ADR-001, ADR-003, ADR-007).

Usage
    python3 generate.py --write   # write the six JSON files next to this script
    python3 generate.py --check   # regenerate in memory, byte-compare, exit 1 on a difference

    ``make golden-check`` runs ``--check``.

Third-tool cross-check of the ``worked-example`` fairness row (spec D-10)
    printf '%s' 'orvex/crash/v1/crash-point:0190f3a8-7b3c-7d4e-8f00-000000000001' | openssl dgst -sha256 -mac HMAC -macopt hexkey:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
    printf '%s' '000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f' | xxd -r -p | sha256sum

    They print the outcome hash
    6fd00d5e48f2092aca65a8c3a649b50a498716e8457736d38584c9ec964fe675 and the
    commitment
    630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd.
"""

import argparse
import bisect
import hashlib
import hmac
import json
import pathlib
import sys
from decimal import ROUND_FLOOR, ROUND_HALF_EVEN, Context, Decimal, localcontext
from fractions import Fraction

if sys.version_info < (3, 11):
    sys.exit("generate.py requires Python 3.11 or newer")

# Parameters, typed in from the spec (D-1 to D-13). Never read from Go.
RULES_VERSION = "crash/v1"
SCALE = 10000
TICK_MICROS = 10000
GROWTH = Decimal(6).scaleb(-4)
GROWTH_TEXT = "0.0006"
EDGE = 9900
MIN_C = 10000
MAX_C = 100000000
MAX_TICK = 15351
MAX_DURATION = 153510000
MIN_BET = 1
MAX_BET = 1000000000
MAX_PAYOUT = 9996596800000
INT64_MAX = 2**63 - 1
INT64_MIN = -(2**63)
MAX_START = INT64_MAX - MAX_DURATION
TAG = b"orvex/crash/v1/crash-point:"
APD_MODULE = "github.com/cockroachdb/apd/v3"
APD_VERSION = "v3.2.3"
ORACLE_PREC = 80
CROSS_PREC = 120
T0 = 1760000000000000
TWO64 = 2**64
TWO53 = 2**53
OUT_OF_RANGE = "out_of_range"

HERE = pathlib.Path(__file__).resolve().parent
FILES = (
    "params.json",
    "multiplier_ticks.json",
    "crash_points.json",
    "fairness.json",
    "deadlines.json",
    "payouts.json",
)


def check(cond, msg):
    if not cond:
        raise AssertionError("self-check failed: " + msg)


# ---------------------------------------------------------------- curve (D-3, D-4)


def multiplier(n, prec):
    """Return (floor(SCALE * exp(0.0006 * n)), unfloored value) at precision prec."""
    ctx = Context(prec=prec, rounding=ROUND_HALF_EVEN, Emax=999999, Emin=-999999)
    x = Decimal(6 * n).scaleb(-4, context=ctx)
    v = ctx.multiply(ctx.exp(x), Decimal(SCALE))
    return int(v.to_integral_value(rounding=ROUND_FLOOR, context=ctx)), v, ctx


def build_table():
    table = []
    margin = None
    margin_tick = None
    for n in range(MAX_TICK + 1):
        m80, v, ctx = multiplier(n, ORACLE_PREC)
        m120, _, _ = multiplier(n, CROSS_PREC)
        check(m80 == m120, "precision %d and %d disagree at tick %d" % (ORACLE_PREC, CROSS_PREC, n))
        table.append(m80)
        if n == 0:
            continue  # exp(0) = 1 exactly; not a rounding margin
        frac = ctx.subtract(v, Decimal(m80))
        dist = min(frac, ctx.subtract(Decimal(1), frac))
        if margin is None or dist < margin:
            margin, margin_tick = dist, n
    check(margin >= Decimal("1e-6"), "minimum margin %s below 1e-6" % margin)
    check(margin_tick == 2710, "minimum margin at tick %d, spec says 2710" % margin_tick)
    check(table[0] == SCALE, "M(0) != SCALE")
    for n in range(MAX_TICK):
        check(table[n + 1] > table[n], "M not strictly increasing at tick %d" % n)
    check(table[15350] == 99965968, "M(15350) != 99965968")
    check(table[15351] == 100025966, "M(15351) != 100025966")
    check(table[MAX_TICK - 1] < MAX_C <= table[MAX_TICK], "cap not reached exactly at MAX_TICK")
    return table, "{:.6e}".format(margin), margin_tick


def crash_tick(table, c):
    return bisect.bisect_left(table, c)


# ---------------------------------------------------------------- distribution (D-5)


def raw_crash_point(r):
    return EDGE * TWO64 // (TWO64 - r)


def crash_point(r):
    return min(MAX_C, max(MIN_C, raw_crash_point(r)))


def rmin(x):
    """Smallest r with raw(r) >= x."""
    return TWO64 - (EDGE * TWO64) // x


def rtp(m):
    return Fraction(m, SCALE) * Fraction(EDGE * TWO64 // (m + 1), TWO64)


def rtp_text(f):
    with localcontext() as ctx:
        ctx.prec = 60
        ctx.rounding = ROUND_HALF_EVEN
        d = Decimal(f.numerator) / Decimal(f.denominator)
        return str(d.quantize(Decimal("1e-20"), rounding=ROUND_HALF_EVEN))


def rtp_bounds(table):
    lo = hi = None
    lo_tick = hi_tick = None
    for n in range(MAX_TICK):  # ticks 0..15350 are the payable ones
        f = rtp(table[n])
        check(Fraction(9899, 10000) <= f < Fraction(99, 100), "RTP out of [0.9899, 0.99) at tick %d" % n)
        if lo is None or f < lo:
            lo, lo_tick = f, n
        if hi is None or f > hi:
            hi, hi_tick = f, n
    lo_text, hi_text = rtp_text(lo), rtp_text(hi)
    check(lo_tick == 0 and lo_text == "0.98990100989901009897", "RTP minimum differs from spec")
    check(hi_tick == 15350 and hi_text == "0.98999999009662949582", "RTP maximum differs from spec")
    return lo_text, lo_tick, hi_text, hi_tick


# ---------------------------------------------------------------- fairness (D-10, D-11)


def round_id_text(b):
    check(len(b) == 16, "round ID must be 16 bytes")
    h = b.hex()
    return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]


def round_id_bytes(text):
    return bytes.fromhex(text.replace("-", ""))


def fairness_case(table, name, seed, rid):
    check(len(seed) == 32, "seed must be 32 bytes")
    commitment = hashlib.sha256(seed).digest()
    text = round_id_text(rid)
    message = TAG + text.encode("ascii")
    digest = hmac.new(seed, message, hashlib.sha256).digest()
    r = int.from_bytes(digest[:8], "big")
    c = crash_point(r)
    ct = crash_tick(table, c)
    return {
        "name": name,
        "seedHex": seed.hex(),
        "roundID": text,
        "commitmentHex": commitment.hex(),
        "messageHex": message.hex(),
        "outcomeHashHex": digest.hex(),
        "rHex": "%016x" % r,
        "crashPointScaled": c,
        "crashTick": ct,
        "deadlineOffsetMicros": ct * TICK_MICROS,
    }


# ---------------------------------------------------------------- timing and payout (D-7, D-8, D-9)


def timing(table, start, eff, ct):
    if start < 0 or start > MAX_START or eff < 0 or ct < 0 or ct > MAX_TICK:
        return None
    e = eff - start
    if 0 < e < ct * TICK_MICROS:
        tick = e // TICK_MICROS
        return True, tick, table[tick]
    return False, None, None


def payout(bet, m):
    if not (MIN_BET <= bet <= MAX_BET) or not (SCALE <= m <= MAX_C - 1):
        return None
    return bet * m // SCALE


# ---------------------------------------------------------------- documents


def params_doc(margin_text, margin_tick, rtp_info):
    lo_text, lo_tick, hi_text, hi_tick = rtp_info
    return {
        "rulesVersion": RULES_VERSION,
        "scale": SCALE,
        "tickMicros": TICK_MICROS,
        "growthPerTick": GROWTH_TEXT,
        "edgeNumerator": EDGE,
        "minCrashPointScaled": MIN_C,
        "maxCrashPointScaled": MAX_C,
        "maxTick": MAX_TICK,
        "maxDurationMicros": MAX_DURATION,
        "minBetCredits": MIN_BET,
        "maxBetCredits": MAX_BET,
        "maxPayoutCredits": MAX_PAYOUT,
        "maxStartMicros": MAX_START,
        "outcomeTag": TAG.decode("ascii"),
        "apdModule": APD_MODULE,
        "apdVersion": APD_VERSION,
        "apdContext": {
            "precision": 34,
            "maxExponent": 6144,
            "minExponent": -6143,
            "rounding": "half_even",
            "traps": "DefaultTraps",
        },
        "oracle": {
            "precision": ORACLE_PREC,
            "crossCheckPrecision": CROSS_PREC,
            "minMarginToInteger": margin_text,
            "minMarginTick": margin_tick,
        },
        "rtp": {"min": lo_text, "minTick": lo_tick, "max": hi_text, "maxTick": hi_tick},
    }


def multiplier_doc(table):
    return {"rulesVersion": RULES_VERSION, "maxTick": MAX_TICK, "multipliersScaled": table}


def crash_points_doc(table):
    boundary = [
        ("r-zero", 0),
        ("raw-9999", rmin(10000) - 1),
        ("raw-10000-first", rmin(10000)),
        ("instant-last", rmin(10001) - 1),
        ("non-instant-first", rmin(10001)),
        ("int64-sign-below", 2**63 - 1),
        ("int64-sign-exact-median", 2**63),
        ("cap-below", rmin(99999999)),
        ("cap-exact", rmin(100000000)),
        ("cap-above", rmin(100000001)),
        ("raw-2pow63", rmin(2**63)),
        ("raw-2pow64", rmin(2**64)),
        ("r-max", 2**64 - 1),
    ]
    spread = []
    for i in range(1000):
        d = hashlib.sha256(b"orvex/crash/v1/vector-r/" + str(i).encode("ascii")).digest()
        spread.append(("spread-%03d" % i, int.from_bytes(d[:8], "big")))

    # Spec 8.3 boundary expectations, typed in independently of rmin().
    want_hex = {
        "raw-9999": "028f5c28f5c28f5c",
        "raw-10000-first": "028f5c28f5c28f5d",
        "instant-last": "0295d8f034d54722",
        "non-instant-first": "0295d8f034d54723",
        "cap-below": "fff9830e3bc2fc1e",
        "cap-exact": "fff9830e3cd9a523",
        "cap-above": "fff9830e3df04e27",
        "raw-2pow63": "ffffffffffffb2a8",
        "raw-2pow64": "ffffffffffffd954",
    }
    want_raw = [
        9900, 9999, 10000, 10000, 10001, 19799, 19800, 99999999, 100000000, 100000001,
        2**63, 2**64, 182622766329724560998400,
    ]

    cases = []
    for idx, (name, r) in enumerate(boundary + spread):
        check(0 <= r < TWO64, "r out of uint64 range in %s" % name)
        raw = raw_crash_point(r)
        c = crash_point(r)
        ct = crash_tick(table, c)
        if name in want_hex:
            check("%016x" % r == want_hex[name], "rHex of %s differs from spec" % name)
        if idx < len(boundary):
            check(raw == want_raw[idx], "raw of %s differs from spec" % name)
        cases.append(
            {
                "name": name,
                "rHex": "%016x" % r,
                "raw": str(raw),
                "crashPointScaled": c,
                "crashTick": ct,
                "deadlineOffsetMicros": ct * TICK_MICROS,
            }
        )
    by_name = {c["name"]: c for c in cases}
    check(by_name["non-instant-first"]["crashTick"] == 1, "non-instant-first crash tick != 1")
    check(by_name["cap-exact"]["crashTick"] == MAX_TICK, "cap-exact crash tick != MAX_TICK")
    return {"rulesVersion": RULES_VERSION, "cases": cases}


def fairness_doc(table):
    example_seed = bytes(range(32))
    rid1 = round_id_bytes("0190f3a8-7b3c-7d4e-8f00-000000000001")
    rid2 = round_id_bytes("0190f3a8-7b3c-7d4e-8f00-000000000002")
    cases = [
        fairness_case(table, "worked-example", example_seed, rid1),
        fairness_case(table, "worked-example-round-2", example_seed, rid2),
        fairness_case(table, "seed-zero", bytes(32), rid1),
        fairness_case(table, "seed-ff", b"\xff" * 32, rid1),
    ]
    for i in range(64):
        seed = hashlib.sha256(b"orvex/crash/v1/vector-seed/" + str(i).encode("ascii")).digest()
        rid = hashlib.sha256(b"orvex/crash/v1/vector-round/" + str(i).encode("ascii")).digest()[:16]
        cases.append(fairness_case(table, "derived-%02d" % i, seed, rid))

    ex = cases[0]
    check(ex["commitmentHex"] == "630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd",
          "worked-example commitment differs from D-10")
    check(ex["outcomeHashHex"] == "6fd00d5e48f2092aca65a8c3a649b50a498716e8457736d38584c9ec964fe675",
          "worked-example outcome hash differs from D-10")
    check(ex["rHex"] == "6fd00d5e48f2092a", "worked-example r differs from D-10")
    check(ex["crashPointScaled"] == 17577, "worked-example C differs from D-10")
    check(ex["crashTick"] == 941, "worked-example n_c differs from D-10")
    check(ex["deadlineOffsetMicros"] == 9410000, "worked-example D differs from D-10")
    check(cases[0]["rHex"] != cases[1]["rHex"], "two round IDs gave the same r")

    commitment = bytes.fromhex(ex["commitmentHex"])
    flipped = commitment[:-1] + bytes([commitment[-1] ^ 0x01])
    checks = [
        {"name": "worked-example-match", "seedHex": example_seed.hex(),
         "commitmentHex": commitment.hex(),
         "matches": hashlib.sha256(example_seed).digest() == commitment},
        {"name": "worked-example-bit-flip", "seedHex": example_seed.hex(),
         "commitmentHex": flipped.hex(),
         "matches": hashlib.sha256(example_seed).digest() == flipped},
    ]
    check(checks[0]["matches"] is True and checks[1]["matches"] is False, "commitment checks")
    return {"rulesVersion": RULES_VERSION, "cases": cases, "commitmentChecks": checks}


def deadline_row(table, name, c, ct, start, eff, want):
    row = {"name": name}
    if c is not None:
        row["crashPointScaled"] = c
    row["crashTick"] = ct
    row["startMicros"] = start
    row["effectiveMicros"] = eff
    got = timing(table, start, eff, ct)
    if got is None:
        check(want == OUT_OF_RANGE, "%s: expected %r, got out_of_range" % (name, want))
        row["error"] = OUT_OF_RANGE
        return row
    eligible, tick, m = got
    check(want != OUT_OF_RANGE, "%s: expected out_of_range" % name)
    row["eligible"] = eligible
    if eligible:
        check(want == (tick, m), "%s: expected %r, got tick %d m %d" % (name, want, tick, m))
        row["tick"] = tick
        row["multiplierScaled"] = m
    else:
        check(want is False, "%s: expected %r, got ineligible" % (name, want))
    return row


def elapsed_name(c, e):
    return "c%d-e%s" % (c, ("m%d" % -e) if e < 0 else str(e))


def deadlines_doc(table):
    # (C, expected n_c, [(e, expectation)]); expectation is False for ineligible,
    # or the expected (tick, multiplier) when eligible.
    curve_rows = [
        (10000, 0, [(-1, False), (0, False), (1, False)]),
        (10001, 1, [(0, False), (1, (0, 10000)), (9999, (0, 10000)), (10000, False), (10001, False)]),
        (10006, 1, [(9999, (0, 10000)), (10000, False)]),
        (10007, 2, [(10000, (1, 10006)), (19999, (1, 10006)), (20000, False)]),
        (17577, 941, [(9409999, (940, 17576)), (9410000, False)]),
        (100000000, 15351, [(153509999, (15350, 99965968)), (153510000, False)]),
    ]
    cases = []
    for c, want_ct, rows in curve_rows:
        ct = crash_tick(table, c)
        check(ct == want_ct, "crash tick of %d is %d, spec says %d" % (c, ct, want_ct))
        for e, want in rows:
            cases.append(deadline_row(table, elapsed_name(c, e), c, ct, T0, T0 + e, want))
    domain_rows = [
        ("start-negative", -1, T0, 1, OUT_OF_RANGE),
        ("effective-negative", 0, -1, 1, OUT_OF_RANGE),
        ("crash-tick-negative", T0, T0 + 1, -1, OUT_OF_RANGE),
        ("crash-tick-above-max", T0, T0 + 1, MAX_TICK + 1, OUT_OF_RANGE),
        ("start-above-max", MAX_START + 1, INT64_MAX, 1, OUT_OF_RANGE),
        ("start-at-max", MAX_START, MAX_START + 1, MAX_TICK, (0, 10000)),
        ("effective-int64-max", 0, INT64_MAX, MAX_TICK, False),
    ]
    for name, start, eff, ct, want in domain_rows:
        cases.append(deadline_row(table, name, None, ct, start, eff, want))
    return {"rulesVersion": RULES_VERSION, "cases": cases}


def payouts_doc():
    rows = [
        (1, 10000, 1),
        (1, 19999, 1),
        (1, 20000, 2),
        (3, 10006, 3),
        (7, 14286, 10),
        (9999, 10001, 9999),
        (10000, 12345, 12345),
        (123456789, 17576, 216987652),
        (1000000000, 10000, 1000000000),
        (1000000000, 99965968, 9996596800000),
        (1000000000, 99999999, 9999999900000),
        (0, 10000, OUT_OF_RANGE),
        (-1, 10000, OUT_OF_RANGE),
        (1000000001, 10000, OUT_OF_RANGE),
        (INT64_MIN, 10000, OUT_OF_RANGE),
        (1, 9999, OUT_OF_RANGE),
        (1, 0, OUT_OF_RANGE),
        (1, -1, OUT_OF_RANGE),
        (1, 100000000, OUT_OF_RANGE),
        (1, INT64_MAX, OUT_OF_RANGE),
        (INT64_MAX, 99999999, OUT_OF_RANGE),
        (2**62, 99999999, OUT_OF_RANGE),
    ]
    cases = []
    for bet, m, want in rows:
        row = {"name": "bet%d-m%d" % (bet, m), "betCredits": bet, "multiplierScaled": m}
        got = payout(bet, m)
        if got is None:
            check(want == OUT_OF_RANGE, "payout(%d, %d) unexpectedly out of range" % (bet, m))
            row["error"] = OUT_OF_RANGE
        else:
            check(got == want, "payout(%d, %d) = %d, spec says %r" % (bet, m, got, want))
            check(got <= MAX_PAYOUT or m > 99965968, "payout above reachable maximum")
            row["payoutCredits"] = got
        cases.append(row)
    return {"rulesVersion": RULES_VERSION, "cases": cases}


# ---------------------------------------------------------------- serialization


# Computed outputs per file; each must be a non-negative int below 2^53 (spec 8.4).
# Inputs (bets, multipliers fed to payout, times, domain-row crash ticks) are only
# required to fit int64.
OUTPUT_KEYS = {
    "params.json": set(),
    "multiplier_ticks.json": {"multipliersScaled"},
    "crash_points.json": {"crashPointScaled", "crashTick", "deadlineOffsetMicros"},
    "fairness.json": {"crashPointScaled", "crashTick", "deadlineOffsetMicros"},
    "deadlines.json": {"tick", "multiplierScaled"},
    "payouts.json": {"payoutCredits"},
}


def check_ints(node, outputs, key=None):
    if isinstance(node, bool):
        return
    if isinstance(node, int):
        check(INT64_MIN <= node <= INT64_MAX, "%s=%d does not fit int64" % (key, node))
        if key in outputs:
            check(0 <= node < TWO53, "output %s=%d not below 2^53" % (key, node))
    elif isinstance(node, dict):
        for k, v in node.items():
            check_ints(v, outputs, k)
    elif isinstance(node, list):
        for v in node:
            check_ints(v, outputs, key)
    else:
        check(isinstance(node, str), "unexpected JSON value type %s" % type(node).__name__)


def serialize(name, doc):
    check(next(iter(doc)) == "rulesVersion" and doc["rulesVersion"] == RULES_VERSION,
          "%s must start with rulesVersion %s" % (name, RULES_VERSION))
    check_ints(doc, OUTPUT_KEYS[name])
    return json.dumps(doc, indent=1, ensure_ascii=True).encode("ascii") + b"\n"


def build():
    table, margin_text, margin_tick = build_table()
    docs = {
        "params.json": params_doc(margin_text, margin_tick, rtp_bounds(table)),
        "multiplier_ticks.json": multiplier_doc(table),
        "crash_points.json": crash_points_doc(table),
        "fairness.json": fairness_doc(table),
        "deadlines.json": deadlines_doc(table),
        "payouts.json": payouts_doc(),
    }
    return {name: serialize(name, docs[name]) for name in FILES}


def main(argv):
    parser = argparse.ArgumentParser(description="crash/v1 golden-vector oracle")
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--write", action="store_true", help="write the JSON vector files")
    mode.add_argument("--check", action="store_true", help="byte-compare the committed files")
    args = parser.parse_args(argv)

    out = build()
    if args.write:
        for name, data in out.items():
            with open(HERE / name, "wb") as f:
                f.write(data)
        print("golden: wrote %d files" % len(out))
        return 0

    failed = False
    for name, data in out.items():
        path = HERE / name
        try:
            on_disk = path.read_bytes()
        except FileNotFoundError:
            on_disk = None
        if on_disk != data:
            print("golden-check: %s differs from generator output" % name, file=sys.stderr)
            failed = True
    if failed:
        return 1
    print("golden-check: %d files match" % len(out))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

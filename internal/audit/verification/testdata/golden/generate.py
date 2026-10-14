#!/usr/bin/env python3
"""Independent oracle for the audit golden histories of chain ``orvex/audit/v1``.

Purpose
    This script writes the golden audit histories, the oracle-made tamper
    fixtures and their manifest for LBT-9 (spec
    ``docs/specs/8bebb70f_canonical-codec-audit-cli.md``, sections 5 and 8;
    ADR-001, ADR-006). The Go verifier in ``internal/audit/verification`` and
    ``crashctl audit`` are tested against the files it writes: Go must
    reproduce every link hash, every event byte string ``Decide`` emits and
    every checkpoint state hash from bytes made here.

Independence rules (spec section 8.1)
    1. Python 3.11 or newer, standard library only. This script never imports,
       executes or reads Go code or ``go.mod``.
    2. Key names, tags, enum strings, reason codes and scenarios are typed in
       from the spec.
    3. The only input file is the LBT-7 oracle's committed
       ``internal/game/crash/v1/testdata/golden/multiplier_ticks.json``,
       resolved relative to this script.
    4. Fairness uses ``hashlib.sha256``, ``hmac`` and ``bisect`` on the tick
       table, as in the math spec section 5.
    5. The canonical encoder is ``json.dumps(v, ensure_ascii=False,
       separators=(",", ":"), sort_keys=True)``, behind a validator: dict, list
       and str only; keys match ^[a-z][A-Za-z0-9]*$; no surrogates; depth <= 32.
    6. A small crash/v1 model runs the scripted commands. Every scripted step
       states its expected result and the model must agree, or the script
       exits non-zero.
    7. Self-checks: every spec section 5.6 literal, the crash outcomes of
       section 8.2, M(500), M(940) and M(2000), and the section 8.2 counts.
    8. JSONL lines end with ``\\n``. ``manifest.json`` is
       ``json.dumps(indent=1, sort_keys=True) + "\\n"``.
    9. The fixtures are never regenerated from Go output. When Go and a fixture
       disagree, the side that contradicts the spec is fixed. This script and
       its output freeze with the first crash/v1 release (A-20).

Usage
    python3 generate.py --write   # write the histories, tamper/ and manifest.json
    python3 generate.py --check   # regenerate in memory, byte-compare, exit 1 on a difference

    ``make golden-check`` runs ``--check``.

Third-tool check of the worked example's first link (spec A-9)
    { printf '%s' 'orvex/audit/v1/record:'; sed -n 2p worked-example.jsonl | tr -d '\\n'; } | sha256sum

    It prints
    cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284, the
    "prevHash" of line 3.
"""

import argparse
import bisect
import hashlib
import hmac
import json
import pathlib
import sys

if sys.version_info < (3, 11):
    sys.exit("generate.py requires Python 3.11 or newer")

# ---------------------------------------------------------------- constants (spec sections 5 and 8)

CHAIN = "orvex/audit/v1"
RECORD_TAG = b"orvex/audit/v1/record:"
STATE_TAG = b"orvex/audit/v1/state:"
FORMAT = "orvex/audit-history/v1"
ZERO_HASH = "0" * 64
GAME = "crash"
RULES_VERSION = "1"
SCHEMA_VERSION = "1"
STATE_VERSION = "1"
OUTCOME_TAG = b"orvex/crash/v1/crash-point:"

# crash/v1 parameters (math spec D-1 to D-7; lifecycle spec MaxBetsPerRound).
SCALE = 10000
TICK_MICROS = 10000
EDGE = 9900
MIN_C = 10000
MAX_C = 100000000
MAX_TICK = 15351
MAX_DURATION = 153510000
INT64_MAX = 2**63 - 1
MAX_START = INT64_MAX - MAX_DURATION
TWO64 = 2**64

T0 = 1760000000000000
ENGINE_BUILD = "golden"
CREATED_AT = T0 - 30_000_000
P3 = "zoë \"☃\"\\\u0001😀"

HERE = pathlib.Path(__file__).resolve().parent
TABLE_PATH = HERE.parents[3] / "game/crash/v1/testdata/golden/multiplier_ticks.json"

PLAYER, SYSTEM = "player", "system"
ACCEPTED = "accepted"

KEY_FIRST = "abcdefghijklmnopqrstuvwxyz"
KEY_REST = KEY_FIRST + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
MAX_DEPTH = 32


class OracleError(Exception):
    pass


def check(cond, msg):
    if not cond:
        raise OracleError("self-check failed: " + msg)


# ---------------------------------------------------------------- OCJ (spec A-1, section 5.1)


def validate(v, depth):
    if isinstance(v, dict):
        check(depth <= MAX_DEPTH, "nesting deeper than %d" % MAX_DEPTH)
        for k, x in v.items():
            check(isinstance(k, str) and len(k) > 0 and k[0] in KEY_FIRST and all(c in KEY_REST for c in k),
                  "bad key %r" % (k,))
            validate(x, depth + 1)
    elif isinstance(v, list):
        check(depth <= MAX_DEPTH, "nesting deeper than %d" % MAX_DEPTH)
        for x in v:
            validate(x, depth + 1)
    elif isinstance(v, str):
        check(not any(0xD800 <= ord(c) <= 0xDFFF for c in v), "surrogate in string")
    else:
        check(False, "value of type %s is not OCJ" % type(v).__name__)


def ocj(obj):
    check(isinstance(obj, dict), "OCJ documents are objects")
    validate(obj, 1)
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode("utf-8")


def sha256_hex(data):
    return hashlib.sha256(data).hexdigest()


def link_hash(line):
    return sha256_hex(RECORD_TAG + line)


def state_hash(state_obj):
    return sha256_hex(STATE_TAG + ocj({"game": GAME, "state": state_obj, "stateVersion": STATE_VERSION}))


def clone(v):
    if isinstance(v, dict):
        return {k: clone(x) for k, x in v.items()}
    if isinstance(v, list):
        return [clone(x) for x in v]
    return v


# ---------------------------------------------------------------- fairness (math spec D-5, D-9, D-10)


def load_table():
    doc = json.loads(TABLE_PATH.read_text(encoding="ascii"))
    check(doc["rulesVersion"] == "crash/v1", "multiplier table is not crash/v1")
    table = doc["multipliersScaled"]
    check(len(table) == MAX_TICK + 1, "multiplier table length")
    return table


def round_id(k):
    return "0190f3a8-7b3c-7d4e-8f00-%012x" % k


def command_id(k, s):
    return "0190f3a8-7b3c-7d4e-8f0%d-%012x" % (k, s)


def outcome(table, seed, rid):
    digest = hmac.new(seed, OUTCOME_TAG + rid.encode("ascii"), hashlib.sha256).digest()
    r = int.from_bytes(digest[:8], "big")
    c = min(MAX_C, max(MIN_C, EDGE * TWO64 // (TWO64 - r)))
    n = bisect.bisect_left(table, c)
    return c, n, n * TICK_MICROS


# ---------------------------------------------------------------- crash/v1 model (lifecycle spec 5.4, 5.5)


def initial_state(table, rid, seed, min_bet, max_bet, max_bets):
    c, n, d = outcome(table, seed, rid)
    return {
        "roundId": rid, "phase": "BETTING", "minBetCredits": min_bet, "maxBetCredits": max_bet,
        "maxBets": max_bets, "commitment": sha256_hex(seed), "seed": seed.hex(),
        "crashPointScaled": c, "crashTick": n, "crashDeadlineOffsetMicros": d,
        "crashDeadlineUnixMicro": 0, "startedAtUnixMicro": 0, "closedAtUnixMicro": 0,
        "settledAtUnixMicro": 0, "bets": [], "totalBetCredits": 0, "totalPayoutCredits": 0,
    }


def state_obj(s):
    """Render a model state as its OCJ object (spec section 5.3)."""
    out = {k: (v if isinstance(v, str) else str(v)) for k, v in s.items() if k != "bets"}
    out["bets"] = [{k: (v if isinstance(v, str) else str(v)) for k, v in b.items()} for b in s["bets"]]
    return out


def find_bet(s, player):
    for i, b in enumerate(s["bets"]):
        if b["player"] == player:
            return i
    return -1


REQUIRED = {
    "PlaceBet": (PLAYER, "BETTING"),
    "StartRound": (SYSTEM, "BETTING"),
    "CashOut": (PLAYER, "RUNNING"),
    "CloseRound": (SYSTEM, "RUNNING"),
    "SettleRound": (SYSTEM, "CRASHED"),
}


def decide(table, s, cmd, t):
    """Return (events, None) or (None, reason) in the lifecycle spec check order."""
    if s["phase"] == "SETTLED":
        return None, "round_settled"
    typ = cmd["type"]
    if typ not in REQUIRED:
        return None, "invalid_command"
    need, phase = REQUIRED[typ]
    if cmd["authority"] != need:
        return None, "unauthorized"
    if t < 0:
        return None, "time_out_of_range"
    if s["phase"] != phase:
        return None, "wrong_phase"
    if typ == "PlaceBet":
        if cmd["player"] == "":
            return None, "invalid_command"
        if not s["minBetCredits"] <= cmd["betCredits"] <= s["maxBetCredits"]:
            return None, "invalid_amount"
        if find_bet(s, cmd["player"]) >= 0:
            return None, "duplicate_bet"
        if len(s["bets"]) >= s["maxBets"]:
            return None, "bet_limit_reached"
        return [{"type": "BetPlaced", "player": cmd["player"], "betCredits": cmd["betCredits"],
                 "placedAtUnixMicro": t}], None
    if typ == "StartRound":
        if t > MAX_START:
            return None, "time_out_of_range"
        return [{"type": "RoundStarted", "startedAtUnixMicro": t}], None
    if typ == "CashOut":
        if cmd["player"] == "":
            return None, "invalid_command"
        i = find_bet(s, cmd["player"])
        if i < 0:
            return None, "no_bet"
        bet = s["bets"][i]
        check(bet["status"] in ("open", "cashed_out"), "bet status while running")
        if bet["status"] == "cashed_out":
            return None, "already_cashed_out"
        if t <= s["startedAtUnixMicro"]:
            return None, "not_after_start"
        e = t - s["startedAtUnixMicro"]
        if not 0 < e < s["crashTick"] * TICK_MICROS:
            return None, "deadline_passed"
        tick = e // TICK_MICROS
        m = table[tick]
        return [{"type": "BetCashedOut", "player": cmd["player"], "cashedOutAtUnixMicro": t,
                 "cashOutTick": tick, "cashOutMultiplierScaled": m,
                 "payoutCredits": bet["betCredits"] * m // SCALE}], None
    if typ == "CloseRound":
        if t < s["crashDeadlineUnixMicro"]:
            return None, "crash_not_due"
        return [{"type": "RoundCrashed", "closedAtUnixMicro": t, "crashPointScaled": s["crashPointScaled"],
                 "crashTick": s["crashTick"], "crashDeadlineOffsetMicros": s["crashDeadlineOffsetMicros"],
                 "crashDeadlineUnixMicro": s["crashDeadlineUnixMicro"]}], None
    events = [{"type": "BetLost", "player": b["player"], "betCredits": b["betCredits"]}
              for b in s["bets"] if b["status"] == "open"]
    events.append({"type": "RoundSettled", "settledAtUnixMicro": t, "betCount": len(s["bets"]),
                   "totalBetCredits": s["totalBetCredits"], "totalPayoutCredits": s["totalPayoutCredits"],
                   "seed": s["seed"]})
    return events, None


def evolve(s, e):
    s = clone(s)
    typ = e["type"]
    if typ == "BetPlaced":
        s["bets"].append({"player": e["player"], "betCredits": e["betCredits"],
                          "placedAtUnixMicro": e["placedAtUnixMicro"], "status": "open",
                          "cashedOutAtUnixMicro": 0, "cashOutTick": 0, "cashOutMultiplierScaled": 0,
                          "payoutCredits": 0})
        s["totalBetCredits"] += e["betCredits"]
    elif typ == "RoundStarted":
        s["phase"] = "RUNNING"
        s["startedAtUnixMicro"] = e["startedAtUnixMicro"]
        s["crashDeadlineUnixMicro"] = e["startedAtUnixMicro"] + s["crashDeadlineOffsetMicros"]
    elif typ == "BetCashedOut":
        b = s["bets"][find_bet(s, e["player"])]
        b["status"] = "cashed_out"
        for k in ("cashedOutAtUnixMicro", "cashOutTick", "cashOutMultiplierScaled", "payoutCredits"):
            b[k] = e[k]
        s["totalPayoutCredits"] += e["payoutCredits"]
    elif typ == "RoundCrashed":
        s["phase"] = "CRASHED"
        s["closedAtUnixMicro"] = e["closedAtUnixMicro"]
    elif typ == "BetLost":
        s["bets"][find_bet(s, e["player"])]["status"] = "lost"
    elif typ == "RoundSettled":
        s["phase"] = "SETTLED"
        s["settledAtUnixMicro"] = e["settledAtUnixMicro"]
    else:
        check(False, "unknown event " + typ)
    return s


def payload(d):
    """Render a command or event dict as its OCJ object: ints become D-12 text."""
    return {k: (v if isinstance(v, str) else str(v)) for k, v in d.items()}


# ---------------------------------------------------------------- scenarios (spec section 8.2)


def place(player, credits, authority=PLAYER):
    return {"type": "PlaceBet", "authority": authority, "player": player, "betCredits": credits}


def cash(player, authority=PLAYER):
    return {"type": "CashOut", "authority": authority, "player": player}


def start():
    return {"type": "StartRound", "authority": SYSTEM}


def close():
    return {"type": "CloseRound", "authority": SYSTEM}


def settle():
    return {"type": "SettleRound", "authority": SYSTEM}


def fixture_seed(n):
    return hashlib.sha256(b"orvex/audit/v1/fixture-seed/%d" % n).digest()


WORKED_SEED = bytes(range(32))

# Each step: (command, offset from T0 in microseconds, expected result,
# expected event types). Typed in from spec section 8.2.
SCENARIOS = [
    {
        "name": "worked-example", "k": 1, "seed": WORKED_SEED,
        "seedHex": "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
        "commitment": "630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd",
        "outcome": (17577, 941, 9410000), "config": (1, 1000000000, 3), "checkpoints": [0, 9, 22],
        "counts": (23, 22, 8, 14, 9),
        "steps": [
            (place("alice", 100), -20_000_000, ACCEPTED, ["BetPlaced"]),
            (place("bob", 250), -19_000_000, ACCEPTED, ["BetPlaced"]),
            (place(P3, 1_000_000_000), -18_000_000, ACCEPTED, ["BetPlaced"]),
            (place("alice", 50), -17_000_000, "duplicate_bet", []),
            (place("dave", 0), -16_000_000, "invalid_amount", []),
            (place("", 10), -15_000_000, "invalid_command", []),
            (place("dave", 10), -14_000_000, "bet_limit_reached", []),
            (cash("alice"), -13_000_000, "wrong_phase", []),
            (start(), 0, ACCEPTED, ["RoundStarted"]),
            (cash("bob"), 0, "not_after_start", []),
            (place("erin", 10), 1_000, "wrong_phase", []),
            (cash("alice"), 5_000_000, ACCEPTED, ["BetCashedOut"]),
            (cash("dave"), 5_500_000, "no_bet", []),
            (cash("alice"), 6_000_000, "already_cashed_out", []),
            (cash("bob", SYSTEM), 7_000_000, "unauthorized", []),
            (close(), 9_409_998, "crash_not_due", []),
            (cash(P3), 9_409_999, ACCEPTED, ["BetCashedOut"]),
            (cash("bob"), 9_410_000, "deadline_passed", []),
            (close(), 9_410_000, ACCEPTED, ["RoundCrashed"]),
            (cash("bob"), 9_420_000, "wrong_phase", []),
            (settle(), 10_000_000, ACCEPTED, ["BetLost", "RoundSettled"]),
            (place("frank", 10), 11_000_000, "round_settled", []),
        ],
    },
    {
        "name": "instant-crash", "k": 2, "seed": fixture_seed(37),
        "seedHex": "e0cc22a8caa1b0c09b790180fa2c3a9f46b83563b5a00a65485b8f6a54f1fe6d",
        "commitment": "4f74afc5272a2452c28c2e7f0d8e7b85205cf9a168d2783a0e4806e70321247e",
        "outcome": (10000, 0, 0), "config": (1, 1000000000, 10000), "checkpoints": [5],
        "counts": (6, 5, 4, 1, 5),
        "steps": [
            (place("alice", 500), -5_000_000, ACCEPTED, ["BetPlaced"]),
            (start(), 0, ACCEPTED, ["RoundStarted"]),
            (cash("alice"), 1, "deadline_passed", []),
            (close(), 1, ACCEPTED, ["RoundCrashed"]),
            (settle(), 2_000_000, ACCEPTED, ["BetLost", "RoundSettled"]),
        ],
    },
    {
        "name": "empty-round", "k": 3, "seed": fixture_seed(0),
        "seedHex": "1fdc2c2201210a3857f273d7600f48ed37295aab586a8189b2fd2866ed5fcde9",
        "commitment": "6c2511c9ab3d3a10a8471485c107baa9d2f202e28dc94b1e4fe39b0e3f07fbe1",
        "outcome": (110245, 4001, 40010000), "config": (1, 1000000000, 10000), "checkpoints": [3],
        "counts": (4, 3, 3, 0, 3),
        "steps": [
            (start(), 0, ACCEPTED, ["RoundStarted"]),
            (close(), 40_010_000, ACCEPTED, ["RoundCrashed"]),
            (settle(), 41_000_000, ACCEPTED, ["RoundSettled"]),
        ],
    },
    {
        "name": "in-progress", "k": 4, "seed": fixture_seed(2),
        "seedHex": "8d28ecf551864a1db9dccebdf0b4a358e13c61332beff3a0dd67f69760eb995e",
        "commitment": "f6af5699688e8306e2a6b50cded2a2a384a67b9d434fa8ad8ef3dcd6617c9daf",
        "outcome": (38532, 2249, 22490000), "config": (1, 1000000000, 10000), "checkpoints": [0, 3],
        "counts": (4, 3, 3, 0, 3),
        "steps": [
            (place("alice", 1_000), -1_000_000, ACCEPTED, ["BetPlaced"]),
            (start(), 0, ACCEPTED, ["RoundStarted"]),
            (cash("alice"), 20_000_000, ACCEPTED, ["BetCashedOut"]),
        ],
    },
]


def run_scenario(table, sc):
    """Return (records, states, header secret) for a scenario. records[0] is
    the genesis dict; states[i] is the model state after sequence i."""
    rid = round_id(sc["k"])
    seed = sc["seed"]
    check(seed.hex() == sc["seedHex"], "%s seed" % sc["name"])
    check(sha256_hex(seed) == sc["commitment"], "%s commitment" % sc["name"])
    check(outcome(table, seed, rid) == sc["outcome"], "%s outcome %r" % (sc["name"], outcome(table, seed, rid)))
    min_bet, max_bet, max_bets = sc["config"]
    genesis = {
        "chain": CHAIN,
        "config": {"commitment": sc["commitment"], "maxBetCredits": str(max_bet), "maxBets": str(max_bets),
                   "minBetCredits": str(min_bet)},
        "createdAtUnixMicro": str(CREATED_AT), "engineBuild": ENGINE_BUILD, "game": GAME, "kind": "genesis",
        "prevHash": ZERO_HASH, "roundId": rid, "rulesVersion": RULES_VERSION, "schemaVersion": SCHEMA_VERSION,
        "sequence": "0", "stateVersion": STATE_VERSION,
    }
    s = initial_state(table, rid, seed, min_bet, max_bet, max_bets)
    records, states = [genesis], [s]
    event_seq = 0
    for seq, (cmd, offset, want, want_events) in enumerate(sc["steps"], start=1):
        t = T0 + offset
        events, reason = decide(table, s, cmd, t)
        got = ACCEPTED if reason is None else reason
        check(got == want, "%s step %d: model says %s, spec says %s" % (sc["name"], seq, got, want))
        events = events or []
        check([e["type"] for e in events] == want_events, "%s step %d events" % (sc["name"], seq))
        items = []
        for e in events:
            event_seq += 1
            s = evolve(s, e)
            items.append({"event": payload(e), "eventSequence": str(event_seq)})
        result = {"status": "accepted"} if reason is None else {"reason": reason, "status": "rejected"}
        records.append({
            "command": payload(cmd), "commandId": command_id(sc["k"], seq), "effectiveTimeUnixMicro": str(t),
            "engineBuild": ENGINE_BUILD, "events": items, "kind": "command", "prevHash": "",
            "result": result, "roundId": rid, "sequence": str(seq),
        })
        states.append(s)
    return records, states, {"seed": seed.hex()}


# ---------------------------------------------------------------- assembly (spec sections 5.4, 5.5, A-11)


def line_of(records, raw, i):
    return raw[i](records[i]) if i in raw else ocj(records[i])


def rechain(records, raw):
    """Recompute prevHash of records 1..N from the actual line bytes and
    return the head."""
    for i in range(1, len(records)):
        records[i]["prevHash"] = link_hash(line_of(records, raw, i - 1))
    return link_hash(line_of(records, raw, len(records) - 1))


def renumber(records):
    n = 0
    for i, r in enumerate(records):
        r["sequence"] = str(i)
        for item in r.get("events", []):
            n += 1
            item["eventSequence"] = str(n)


def history_bytes(header, records, raw):
    lines = [ocj(header)] + [line_of(records, raw, i) for i in range(len(records))]
    return b"".join(line + b"\n" for line in lines)


def header_of(checkpoints, secret, head):
    return {"checkpoints": [{"sequence": str(s), "stateHash": h} for s, h in checkpoints],
            "configSecret": secret, "format": FORMAT, "head": head}


# ---------------------------------------------------------------- tamper fixtures (spec section 8.4)


def check_row(command, exit_code, stage=None, sequence=None, args=None):
    return {"command": command, "args": args or [], "exit": exit_code, "stage": stage,
            "sequence": None if sequence is None else str(sequence)}


def raw_sequence_first(r):
    rest = dict(r)
    seq = rest.pop("sequence")
    body = ocj(rest)
    return b'{"sequence":' + json.dumps(seq).encode("ascii") + b"," + body[1:]


def raw_json_number(r):
    b = ocj(r)
    old = b'"sequence":"%s"' % r["sequence"].encode("ascii")
    check(b.count(old) == 1, "R20 sequence member must occur once")
    return b.replace(old, b'"sequence":%s' % r["sequence"].encode("ascii"))


def tamper_cases(worked_head):
    """Each case: (name, description, mutate(records, checkpoints) -> raw, renumber?, checks)."""

    def event(r, j=0):
        return r["events"][j]["event"]

    def swap12(rs, cp):
        rs[1], rs[2] = rs[2], rs[1]

    def move12to9(rs, cp):
        rs.insert(9, rs.pop(12))

    def payout(rs, cp):
        check(event(rs[12])["payoutCredits"] == "134", "R3 base payout")
        event(rs[12])["payoutCredits"] = "135"

    def eff_time(rs, cp):
        rs[12]["effectiveTimeUnixMicro"] = str(int(rs[12]["effectiveTimeUnixMicro"]) + 10_000)

    def reason(rs, cp):
        check(rs[13]["result"]["reason"] == "no_bet", "R5 base reason")
        rs[13]["result"]["reason"] = "wrong_phase"

    def to_rejected(rs, cp):
        rs[17]["result"] = {"reason": "deadline_passed", "status": "rejected"}
        rs[17]["events"] = []

    def crash_point(rs, cp):
        event(rs[19])["crashPointScaled"] = "20000"

    def commitment(rs, cp):
        rs[0]["config"]["commitment"] = sha256_hex(bytes([1] * 32))

    def genesis_prev(rs, cp):
        rs[0]["prevHash"] = ZERO_HASH[:-1] + "1"

    def round_id9(rs, cp):
        rs[7]["roundId"] = round_id(9)

    def event_gap(rs, cp):
        check(rs[12]["events"][0]["eventSequence"] == "5", "R11 base event sequence")
        rs[12]["events"][0]["eventSequence"] = "6"

    def dup_id(rs, cp):
        rs[8]["commandId"] = rs[7]["commandId"]

    def rejected_events(rs, cp):
        cmd = rs[4]["command"]
        rs[4]["events"] = [{"event": {"betCredits": cmd["betCredits"],
                                      "placedAtUnixMicro": rs[4]["effectiveTimeUnixMicro"],
                                      "player": cmd["player"], "type": "BetPlaced"},
                            "eventSequence": "4"}]

    def genesis_field(key, value):
        def f(rs, cp):
            rs[0][key] = value
        return f

    def leading_zero(rs, cp):
        rs[1]["command"]["betCredits"] = "0100"

    def upper_commitment(rs, cp):
        rs[0]["config"]["commitment"] = rs[0]["config"]["commitment"].upper()

    def event_note(rs, cp):
        event(rs[1])["note"] = "x"

    def event_type(rs, cp):
        event(rs[1])["type"] = "BetVoided"

    def kind(rs, cp):
        rs[5]["kind"] = "admission"

    def delete13(rs, cp):
        check(rs[13]["result"].get("reason") == "no_bet", "R26 base record")
        del rs[13]
        check(cp[-1][0] == 22, "R26 final checkpoint")
        cp[-1] = (21, cp[-1][1])

    V, R, C = "verify", "replay", "chain"
    return [
        ("reorder-bets", "swap records 1 and 2 (bob's bet first), renumber sequence and eventSequence",
         swap12, {}, True, [check_row(V, 1, "state-replay", 9), check_row(C, 0)]),
        ("cashout-before-start", "move record 12 to position 9; shift 9-11 to 10-12; renumber",
         move12to9, {}, True, [check_row(V, 1, "decision-replay", 9), check_row(R, 1, "state-replay", 9)]),
        ("payout-edited", "record 12 payoutCredits 134 -> 135",
         payout, {}, False, [check_row(V, 1, "decision-replay", 12), check_row(R, 1, "state-replay", 12)]),
        ("effective-time-edited", "record 12 effectiveTimeUnixMicro +10,000 (event unchanged)",
         eff_time, {}, False, [check_row(V, 1, "decision-replay", 12), check_row(R, 0)]),
        ("rejection-reason-edited", "record 13 reason no_bet -> wrong_phase",
         reason, {}, False, [check_row(V, 1, "decision-replay", 13), check_row(R, 0)]),
        ("accepted-to-rejected",
         "record 17 result -> rejected deadline_passed, events removed, later eventSequence renumbered",
         to_rejected, {}, True, [check_row(V, 1, "decision-replay", 17), check_row(R, 1, "state-replay", 21)]),
        ("crash-point-edited", "record 19 crashPointScaled -> 20000",
         crash_point, {}, False, [check_row(V, 1, "decision-replay", 19), check_row(R, 1, "state-replay", 19)]),
        ("commitment-edited", "genesis commitment -> SHA-256(32 x 0x01)",
         commitment, {}, False, [check_row(V, 1, "fairness", 0), check_row(C, 0)]),
        ("genesis-prevhash-nonzero", "genesis prevHash last digit -> 1",
         genesis_prev, {}, False, [check_row(V, 1, "chain", 0)]),
        ("roundid-mismatch", "record 7 roundId -> ...000000000009",
         round_id9, {}, False, [check_row(V, 1, "chain", 7)]),
        ("event-sequence-gap", "record 12 eventSequence 5 -> 6",
         event_gap, {}, False, [check_row(V, 1, "chain", 12)]),
        ("duplicate-command-id", "record 8 commandId = record 7's",
         dup_id, {}, False, [check_row(V, 1, "chain", 8)]),
        ("rejected-with-events", "record 4 gains a BetPlaced event at eventSequence 4; later ones renumbered",
         rejected_events, {}, True, [check_row(V, 1, "chain", 4)]),
        ("chain-v2", "genesis chain -> orvex/audit/v2",
         genesis_field("chain", "orvex/audit/v2"), {}, False, [check_row(V, 3, "format", 0)]),
        ("rules-version-2", "genesis rulesVersion -> 2",
         genesis_field("rulesVersion", "2"), {}, False, [check_row(V, 3, "rules", 0), check_row(C, 0)]),
        ("schema-version-2", "genesis schemaVersion -> 2",
         genesis_field("schemaVersion", "2"), {}, False, [check_row(V, 3, "rules", 0)]),
        ("state-version-2", "genesis stateVersion -> 2",
         genesis_field("stateVersion", "2"), {}, False, [check_row(V, 3, "rules", 0)]),
        ("unknown-game", "genesis game -> dice",
         genesis_field("game", "dice"), {}, False, [check_row(V, 3, "rules", 0)]),
        ("unsorted-keys", "record 5 written with sequence as its first key (raw bytes, hash over them)",
         None, {5: raw_sequence_first}, False, [check_row(V, 1, "format", 5)]),
        ("json-number", 'record 5 "sequence":"5" -> "sequence":5',
         None, {5: raw_json_number}, False, [check_row(V, 1, "format", 5)]),
        ("leading-zero-int", 'record 1 command betCredits -> "0100"',
         leading_zero, {}, False, [check_row(V, 1, "decode", 1)]),
        ("uppercase-hex", "genesis config commitment in uppercase",
         upper_commitment, {}, False, [check_row(V, 1, "decode", 0)]),
        ("unknown-event-field", 'record 1 event gains "note":"x"',
         event_note, {}, False, [check_row(V, 1, "decode", 1)]),
        ("unknown-event-type", "record 1 event type -> BetVoided",
         event_type, {}, False, [check_row(V, 1, "decode", 1)]),
        ("unknown-record-kind", "record 5 kind -> admission",
         kind, {}, False, [check_row(V, 1, "format", 5)]),
        ("delete-rejection",
         "delete record 13 (no_bet); renumber 14-22 -> 13-21; final checkpoint sequence -> 21 (hash unchanged)",
         delete13, {}, True,
         [check_row(V, 0), check_row(V, 1, "chain", 21, ["-expect-head", worked_head])]),
    ]


# ---------------------------------------------------------------- self-checks (spec section 5.6)

LIT_PAYLOADS = [
    ({"type": "PlaceBet", "authority": PLAYER, "player": "alice", "betCredits": 100},
     '{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"}'),
    ({"type": "StartRound", "authority": SYSTEM}, '{"authority":"system","type":"StartRound"}'),
    ({"type": "CashOut", "authority": PLAYER, "player": "alice"},
     '{"authority":"player","player":"alice","type":"CashOut"}'),
    ({"type": "CloseRound", "authority": SYSTEM}, '{"authority":"system","type":"CloseRound"}'),
    ({"type": "SettleRound", "authority": SYSTEM}, '{"authority":"system","type":"SettleRound"}'),
    ({"type": "BetPlaced", "player": "alice", "betCredits": 100, "placedAtUnixMicro": T0 - 20_000_000},
     '{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"}'),
    ({"type": "RoundStarted", "startedAtUnixMicro": T0},
     '{"startedAtUnixMicro":"1760000000000000","type":"RoundStarted"}'),
    ({"type": "BetCashedOut", "player": "alice", "cashedOutAtUnixMicro": T0 + 5_000_000, "cashOutTick": 500,
      "cashOutMultiplierScaled": 13498, "payoutCredits": 134},
     '{"cashOutMultiplierScaled":"13498","cashOutTick":"500","cashedOutAtUnixMicro":"1760000005000000",'
     '"payoutCredits":"134","player":"alice","type":"BetCashedOut"}'),
    ({"type": "RoundCrashed", "closedAtUnixMicro": T0 + 9_410_000, "crashPointScaled": 17577, "crashTick": 941,
      "crashDeadlineOffsetMicros": 9_410_000, "crashDeadlineUnixMicro": T0 + 9_410_000},
     '{"closedAtUnixMicro":"1760000009410000","crashDeadlineOffsetMicros":"9410000",'
     '"crashDeadlineUnixMicro":"1760000009410000","crashPointScaled":"17577","crashTick":"941",'
     '"type":"RoundCrashed"}'),
    ({"type": "BetLost", "player": "bob", "betCredits": 250}, '{"betCredits":"250","player":"bob","type":"BetLost"}'),
    ({"type": "RoundSettled", "settledAtUnixMicro": T0 + 10_000_000, "betCount": 3, "totalBetCredits": 1000000350,
      "totalPayoutCredits": 1757600134, "seed": WORKED_SEED.hex()},
     '{"betCount":"3","seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",'
     '"settledAtUnixMicro":"1760000010000000","totalBetCredits":"1000000350","totalPayoutCredits":"1757600134",'
     '"type":"RoundSettled"}'),
    ({"commitment": "630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd",
      "maxBetCredits": 1000000000, "maxBets": 3, "minBetCredits": 1},
     '{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd",'
     '"maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1"}'),
    ({"seed": WORKED_SEED.hex()}, '{"seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"}'),
    ({"player": "alice", "betCredits": 100, "placedAtUnixMicro": T0 - 20_000_000, "status": "cashed_out",
      "cashedOutAtUnixMicro": T0 + 5_000_000, "cashOutTick": 500, "cashOutMultiplierScaled": 13498,
      "payoutCredits": 134},
     '{"betCredits":"100","cashOutMultiplierScaled":"13498","cashOutTick":"500",'
     '"cashedOutAtUnixMicro":"1760000005000000","payoutCredits":"134","placedAtUnixMicro":"1759999980000000",'
     '"player":"alice","status":"cashed_out"}'),
    ({"reason": "duplicate_bet", "status": "rejected"}, '{"reason":"duplicate_bet","status":"rejected"}'),
]

LIT_P3_HEX = (
    "7b22617574686f72697479223a22706c61796572222c2262657443726564697473223a2231303030303030303030222c22706c"
    "61796572223a227a6fc3ab205c22e298835c225c5c5c7530303031f09f9880222c2274797065223a22506c616365426574227d"
)
LIT_GENESIS = (
    '{"chain":"orvex/audit/v1","config":{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b858'
    '1bd710dd","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1"},"createdAtUnixMicro":"17599999700'
    '00000","engineBuild":"golden","game":"crash","kind":"genesis","prevHash":"00000000000000000000000000000000000'
    '00000000000000000000000000000","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001","rulesVersion":"1","schemaVe'
    'rsion":"1","sequence":"0","stateVersion":"1"}'
)
LIT_HASH0 = "cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284"
LIT_RECORD1 = (
    '{"command":{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"},"commandId":"0190f3a8-'
    '7b3c-7d4e-8f01-000000000001","effectiveTimeUnixMicro":"1759999980000000","engineBuild":"golden","events":[{"ev'
    'ent":{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"},"eventSeq'
    'uence":"1"}],"kind":"command","prevHash":"cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284","r'
    'esult":{"status":"accepted"},"roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001","sequence":"1"}'
)
LIT_HASH1 = "6cbb649e00d7ac798be564e2a2f90910ffd69caa9f95207e9ee2f9e72afcf061"
LIT_STATE_DOC = (
    '{"game":"crash","state":{"bets":[],"closedAtUnixMicro":"0","commitment":"630dcd2966c4336691125448bbb25b4ff412a'
    '49c732db2c8abc1b8581bd710dd","crashDeadlineOffsetMicros":"9410000","crashDeadlineUnixMicro":"0","crashPointSca'
    'led":"17577","crashTick":"941","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1","phase":"BETTIN'
    'G","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001","seed":"000102030405060708090a0b0c0d0e0f101112131415161718'
    '191a1b1c1d1e1f","settledAtUnixMicro":"0","startedAtUnixMicro":"0","totalBetCredits":"0","totalPayoutCredits":'
    '"0"},"stateVersion":"1"}'
)
LIT_STATE_HASH0 = "1c98ea460213ad0d5900a92fbf37cc3078fa9e822d2065a65037b667734de7e7"


def self_checks(table, worked_records, worked_states):
    check(table[500] == 13498 and table[940] == 17576 and table[2000] == 33201, "M(500), M(940), M(2000)")
    for obj, want in LIT_PAYLOADS:
        check(ocj(payload(obj)) == want.encode("utf-8"), "section 5.6 literal " + want)
    p3 = ocj(payload({"type": "PlaceBet", "authority": PLAYER, "player": P3, "betCredits": 1_000_000_000}))
    check(p3.hex() == LIT_P3_HEX, "P3 hex")
    genesis = ocj(worked_records[0])
    check(genesis == LIT_GENESIS.encode("ascii"), "worked genesis bytes")
    check(link_hash(genesis) == LIT_HASH0, "hash_0")
    record1 = ocj(worked_records[1])
    check(record1 == LIT_RECORD1.encode("ascii"), "worked record 1 bytes")
    check(link_hash(record1) == LIT_HASH1, "hash_1")
    doc = ocj({"game": GAME, "state": state_obj(worked_states[0]), "stateVersion": STATE_VERSION})
    check(doc == LIT_STATE_DOC.encode("ascii"), "worked state-hash document")
    check(state_hash(state_obj(worked_states[0])) == LIT_STATE_HASH0, "stateHash_0")
    e5 = worked_records[12]["events"][0]["event"]
    check((e5["cashOutTick"], e5["cashOutMultiplierScaled"], e5["payoutCredits"]) == ("500", "13498", "134"), "e5")
    e6 = worked_records[17]["events"][0]["event"]
    check((e6["cashOutTick"], e6["cashOutMultiplierScaled"], e6["payoutCredits"]) == ("940", "17576", "1757600000"),
          "e6")
    e9 = worked_records[21]["events"][1]["event"]
    check((e9["betCount"], e9["totalBetCredits"], e9["totalPayoutCredits"]) == ("3", "1000000350", "1757600134"),
          "e9")


# ---------------------------------------------------------------- build


def counts(records):
    commands = records[1:]
    accepted = sum(1 for r in commands if r["result"]["status"] == "accepted")
    events = sum(len(r["events"]) for r in commands)
    return len(records), len(commands), accepted, len(commands) - accepted, events


def build():
    table = load_table()
    out = {}
    histories = []
    worked = None
    for sc in SCENARIOS:
        records, states, secret = run_scenario(table, sc)
        head = rechain(records, {})
        check(counts(records) == sc["counts"], "%s counts %r" % (sc["name"], counts(records)))
        checkpoints = [(s, state_hash(state_obj(states[s]))) for s in sc["checkpoints"]]
        out[sc["name"] + ".jsonl"] = history_bytes(header_of(checkpoints, secret, head), records, {})
        n, c, a, r, e = counts(records)
        histories.append({
            "name": sc["name"], "file": sc["name"] + ".jsonl", "records": n, "commands": c, "accepted": a,
            "rejected": r, "events": e, "head": head,
            "checkpoints": [{"sequence": s, "stateHash": h} for s, h in checkpoints],
        })
        if sc["name"] == "worked-example":
            self_checks(table, records, states)
            worked = (records, checkpoints, secret, head)

    base_records, base_checkpoints, secret, worked_head = worked
    tamper = []
    for name, description, mutate, raw, renum, checks in tamper_cases(worked_head):
        records = clone(base_records)
        checkpoints = list(base_checkpoints)
        if mutate is not None:
            mutate(records, checkpoints)
        if renum:
            renumber(records)
        head = rechain(records, raw)
        path = "tamper/" + name + ".jsonl"
        out[path] = history_bytes(header_of(checkpoints, secret, head), records, raw)
        tamper.append({"name": name, "file": path, "base": "worked-example", "description": description,
                       "checks": checks})
    check(len(tamper) == 26, "26 tamper fixtures")

    manifest = {"format": FORMAT, "histories": histories, "tamper": tamper}
    out["manifest.json"] = (json.dumps(manifest, indent=1, sort_keys=True) + "\n").encode("ascii")
    return out


def main(argv):
    parser = argparse.ArgumentParser(description="orvex/audit/v1 golden-history oracle")
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--write", action="store_true", help="write the histories, tamper fixtures and manifest")
    mode.add_argument("--check", action="store_true", help="byte-compare the committed files")
    args = parser.parse_args(argv)

    try:
        out = build()
    except OracleError as err:
        print("audit-golden: %s" % err, file=sys.stderr)
        return 2

    if args.write:
        (HERE / "tamper").mkdir(exist_ok=True)
        for name, data in out.items():
            (HERE / name).write_bytes(data)
        print("audit-golden: wrote %d files" % len(out))
        return 0

    failed = False
    for name, data in out.items():
        path = HERE / name
        try:
            on_disk = path.read_bytes()
        except FileNotFoundError:
            on_disk = None
        if on_disk != data:
            print("audit-golden-check: %s differs from generator output" % name, file=sys.stderr)
            failed = True
    tamper_dir = HERE / "tamper"
    if tamper_dir.is_dir():
        for path in sorted(tamper_dir.iterdir()):
            if "tamper/" + path.name not in out:
                print("audit-golden-check: unexpected file tamper/%s" % path.name, file=sys.stderr)
                failed = True
    if failed:
        return 1
    print("audit-golden-check: %d files match" % len(out))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

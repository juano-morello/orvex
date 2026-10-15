#!/usr/bin/env python3
"""Independent oracle for the audit golden histories of format ``orvex/audit-history/v2``.

Purpose
    This script writes the golden v2 audit histories, the oracle-made ledger
    tamper fixtures and their manifest for LBT-18 (spec
    ``docs/specs/ac81f88f_audit-ledger-stage.md``, sections 5 and 9; ADR-001,
    ADR-005, ADR-006). A v2 history is a v1 history with format
    ``orvex/audit-history/v2``, a header account table and one ledger line per
    record. The Go verifier's ledger stage in ``internal/audit/verification``,
    ``eventstore.ExportHistoryV2`` and ``crashctl audit`` are tested against
    the files it writes: Go must recompute every posting and reproduce every
    header and ledger line from bytes made here.

Independence rules (spec section 9.1)
    1. Python 3.11 or newer, standard library only. This script never imports,
       executes or reads Go code, ``go.mod``, ``internal/ledger`` or the v1
       oracle script ``../golden/generate.py``.
    2. The only inputs are the four committed v1 golden histories
       ``../golden/{worked-example,instant-crash,empty-round,in-progress}.jsonl``,
       resolved relative to this script and read as bytes. Their record lines
       are copied byte for byte.
    3. Typed in from the spec, LBT-12 E-8/E-9/E-10 and LBT-11 S-10: uuid8, the
       two ID tags, the account names ``house`` and ``round_pool``, the three
       transaction kinds, the crash/v1 posting rule, the hold rule (a
       player-authority PlaceBet with a non-empty player and
       1 <= betCredits <= 1,000,000,000 holds betCredits on the player's
       account), release-always, the fixture player account IDs and every v2
       key name.
    4. The canonical encoder and validator are the v1 oracle's, copied (not
       imported): ``json.dumps(v, ensure_ascii=False, separators=(",", ":"),
       sort_keys=True)`` behind a validator (dict, list and str only; keys
       match ^[a-z][A-Za-z0-9]*$; no surrogates; depth <= 32).
    5. Self-checks: the inputs chain; every spec section 5.5 literal (IDs,
       header lines, ledger lines); the section 9.2 counts; per history
       sum of account nets 0, every hold released, at most one posting per
       event; for settled histories round account net 0 and posted bets and
       payouts equal to RoundSettled's totals; the house nets; the
       worked-example holds.
    6. JSONL lines end with ``\\n``. ``manifest.json`` is
       ``json.dumps(indent=1, sort_keys=True) + "\\n"``.
    7. The fixtures are never regenerated from Go output. When Go and a fixture
       disagree, the side that contradicts the spec is fixed. This script and
       its output freeze with the first crash/v1 release (L-13).

Usage
    python3 generate.py --write   # write the histories, tamper/ and manifest.json
    python3 generate.py --check   # regenerate in memory, byte-compare, exit 1 on a difference

    ``make golden-check`` runs ``--check``.
"""

import argparse
import hashlib
import json
import pathlib
import sys

if sys.version_info < (3, 11):
    sys.exit("generate.py requires Python 3.11 or newer")

# ---------------------------------------------------------------- constants (spec sections 5 and 9)

FORMAT_V1 = "orvex/audit-history/v1"
FORMAT_V2 = "orvex/audit-history/v2"
RECORD_TAG = b"orvex/audit/v1/record:"
ZERO_HASH = "0" * 64
NIL_UUID = "00000000-0000-0000-0000-000000000000"

# LBT-12 E-8: deterministic ledger IDs and system account names.
ACCOUNT_TAG = "orvex/ledger/v1/account:"
TRANSACTION_TAG = "orvex/ledger/v1/transaction:"
HOUSE = "house"
ROUND_POOL = "round_pool"

# LBT-12 E-9: crash/v1 transaction kinds.
KIND_BET = "crash_bet"
KIND_CASH_OUT = "crash_cash_out"
KIND_SETTLEMENT = "crash_settlement"

# LBT-11 S-10: the hold of a player-authority PlaceBet.
MIN_HOLD = 1
MAX_HOLD = 1_000_000_000

# LBT-9 section 5.6 player P3, and the fixture player accounts (spec L-11, section 9.2).
P3 = "zo\u00eb \"\u2603\"\\\u0001\U0001F600"
PLAYER_NUMBERS = {"alice": 1, "bob": 2, P3: 3, "dave": 4, "erin": 5, "frank": 6}
PLAYER_ACCOUNT_PREFIX = "0190f3a8-7b3c-7d4e-8fa0-"

NAMES = ["worked-example", "instant-crash", "empty-round", "in-progress"]

HERE = pathlib.Path(__file__).resolve().parent
V1_DIR = HERE.parent / "golden"

KEY_FIRST = "abcdefghijklmnopqrstuvwxyz"
KEY_REST = KEY_FIRST + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
MAX_DEPTH = 32


class OracleError(Exception):
    pass


def check(cond, msg):
    if not cond:
        raise OracleError("self-check failed: " + msg)


# ---------------------------------------------------------------- OCJ (LBT-9 A-1, copied from the v1 oracle)


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


def clone(v):
    if isinstance(v, dict):
        return {k: clone(x) for k, x in v.items()}
    if isinstance(v, list):
        return [clone(x) for x in v]
    return v


# ---------------------------------------------------------------- ledger IDs (LBT-12 E-8)


def uuid8(s):
    b = bytearray(hashlib.sha256(s.encode("utf-8")).digest()[:16])
    b[6] = (b[6] & 0x0F) | 0x80
    b[8] = (b[8] & 0x3F) | 0x80
    h = b.hex()
    return "%s-%s-%s-%s-%s" % (h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])


def house_account_id():
    return uuid8(ACCOUNT_TAG + "global/" + HOUSE)


def round_account_id(rid):
    return uuid8(ACCOUNT_TAG + rid + "/" + ROUND_POOL)


def transaction_id(rid, k):
    return uuid8(TRANSACTION_TAG + rid + "/" + str(k))


def player_account_id(player):
    if player not in PLAYER_NUMBERS:
        raise OracleError("player %r has no fixture account" % (player,))
    return PLAYER_ACCOUNT_PREFIX + "%012x" % PLAYER_NUMBERS[player]


# ---------------------------------------------------------------- inputs (spec section 9.1 steps 2 and 6)


def read_v1(name):
    """Return (header object, record lines without '\\n', record objects) of a v1 golden."""
    data = (V1_DIR / (name + ".jsonl")).read_bytes()
    check(data.endswith(b"\n"), "%s ends with a newline" % name)
    lines = data[:-1].split(b"\n")
    header_line, record_lines = lines[0], lines[1:]
    header = json.loads(header_line.decode("utf-8"))
    check(header.get("format") == FORMAT_V1, "%s header format is v1" % name)
    check(ocj(header) == header_line, "%s header is OCJ" % name)
    records = []
    prev = ZERO_HASH
    for i, line in enumerate(record_lines):
        r = json.loads(line.decode("utf-8"))
        check(ocj(r) == line, "%s record %d is OCJ" % (name, i))
        check(r["sequence"] == str(i), "%s record %d sequence" % (name, i))
        check(r["prevHash"] == prev, "%s record %d prevHash" % (name, i))
        prev = link_hash(line)
        records.append(r)
    check(records[0]["kind"] == "genesis", "%s genesis" % name)
    check(header["head"] == prev, "%s head" % name)
    return header, record_lines, records


# ---------------------------------------------------------------- ledger rows (LBT-11 S-10, LBT-12 E-9, E-10)


def holds_of(command):
    """S-10: the (player, amount) holds of one admission."""
    if command.get("type") != "PlaceBet" or command.get("authority") != "player":
        return []
    player = command.get("player", "")
    amount = int(command["betCredits"])
    if player == "" or not (MIN_HOLD <= amount <= MAX_HOLD):
        return []
    return [(player, amount)]


def postings_of(record, holds):
    """E-9: the crash/v1 transaction kind and postings of one command record.

    Sides are ("player", P), ("round",) or ("house",); a posting is
    (debit, credit, amount, eventSequence)."""
    kind, postings = None, []

    def add(k, debit, credit, amount, seq):
        nonlocal kind
        if kind is not None and kind != k:
            raise OracleError("record %s posts both %s and %s" % (record["sequence"], kind, k))
        kind = k
        postings.append((debit, credit, amount, seq))

    for item in record["events"]:
        e, seq = item["event"], int(item["eventSequence"])
        t = e["type"]
        if t == "BetPlaced":
            amount = int(e["betCredits"])
            check(holds == [(e["player"], amount)], "record %s BetPlaced consumes exactly its hold" % record["sequence"])
            add(KIND_BET, ("player", e["player"]), ("round",), amount, seq)
        elif t == "BetCashedOut":
            add(KIND_CASH_OUT, ("round",), ("player", e["player"]), int(e["payoutCredits"]), seq)
        elif t == "RoundSettled":
            b, w = int(e["totalBetCredits"]), int(e["totalPayoutCredits"])
            if b > w:
                add(KIND_SETTLEMENT, ("round",), ("house",), b - w, seq)
            elif b < w:
                add(KIND_SETTLEMENT, ("house",), ("round",), w - b, seq)
        elif t in ("RoundStarted", "RoundCrashed", "BetLost"):
            pass
        else:
            raise OracleError("unknown event type %r" % (t,))
    return kind, postings


def strictly_sorted(keys):
    return all(a < b for a, b in zip(keys, keys[1:]))


def build_ledger(records):
    """Return (account table, ledger line objects 0..N, nets, stats) for one round."""
    rid = records[0]["roundId"]
    accounts = {}

    def player(p):
        aid = player_account_id(p)
        accounts[aid] = {"accountId": aid, "kind": "player", "player": p}
        return aid

    def system(name, scope):
        aid = house_account_id() if name == HOUSE else round_account_id(rid)
        accounts[aid] = {"accountId": aid, "kind": "system", "name": name, "roundId": scope}
        return aid

    def resolve(side):
        if side[0] == "player":
            return player(side[1])
        if side[0] == "round":
            return system(ROUND_POOL, rid)
        return system(HOUSE, NIL_UUID)

    lines = [{"holds": [], "kind": "ledger", "releases": [], "sequence": "0", "transactions": []}]
    nets = {}
    stats = {"transactions": 0, "postings": 0, "holds": 0, "releases": 0,
             KIND_BET: 0, KIND_CASH_OUT: 0, "events": set(), "holdList": []}
    for k, r in enumerate(records[1:], start=1):
        hs = holds_of(r["command"])
        holds = sorted(({"accountId": player(p), "amount": str(a)} for p, a in hs), key=lambda h: h["accountId"])
        releases = sorted(h["accountId"] for h in holds)  # E-10: release-always
        kind, ps = postings_of(r, hs)
        transactions = []
        if ps:
            postings = []
            for j, (debit, credit, amount, seq) in enumerate(ps):
                d, c = resolve(debit), resolve(credit)
                check(d != c and amount >= 1, "record %d posting %d balances" % (k, j))
                check(seq not in stats["events"], "event %d posted twice" % seq)
                stats["events"].add(seq)
                nets[d] = nets.get(d, 0) - amount
                nets[c] = nets.get(c, 0) + amount
                stats[kind] = stats.get(kind, 0) + amount
                postings.append({"amount": str(amount), "creditAccountId": c, "debitAccountId": d,
                                 "eventSequence": str(seq), "postingIndex": str(j)})
            transactions.append({"kind": kind, "postings": postings, "transactionId": transaction_id(rid, k)})
        check(strictly_sorted([h["accountId"] for h in holds]), "record %d holds sorted" % k)
        check(strictly_sorted(releases), "record %d releases sorted" % k)
        check(releases == [h["accountId"] for h in holds], "record %d every hold released" % k)
        lines.append({"holds": holds, "kind": "ledger", "releases": releases, "sequence": str(k),
                      "transactions": transactions})
        stats["transactions"] += len(transactions)
        stats["postings"] += sum(len(t["postings"]) for t in transactions)
        stats["holds"] += len(holds)
        stats["releases"] += len(releases)
        stats["holdList"] += [(k, int(h["amount"])) for h in holds]
    table = [accounts[a] for a in sorted(accounts)]
    return table, lines, nets, stats


# ---------------------------------------------------------------- assembly (spec section 5.1)


def v2_header(v1_header, table):
    h = clone(v1_header)
    h["format"] = FORMAT_V2
    h["accounts"] = table
    return h


def history_bytes(header, record_lines, ledger_lines):
    lines = [ocj(header)] + list(record_lines) + [ocj(x) for x in ledger_lines]
    return b"".join(line + b"\n" for line in lines)


# ---------------------------------------------------------------- tamper fixtures (spec section 9.4)


def check_row(command, exit_code, stage=None, sequence=None, args=None):
    return {"command": command, "args": args or [], "exit": exit_code, "stage": stage,
            "sequence": None if sequence is None else str(sequence)}


def tamper_cases(rid):
    """Each case: (name, description, mutate(header, ledger) -> ledger, checks)."""
    alice, bob, dave = player_account_id("alice"), player_account_id("bob"), player_account_id("dave")
    frank = player_account_id("frank")
    round_acct, house = round_account_id(rid), house_account_id()

    def posting(lg, j):
        ts = lg[j]["transactions"]
        check(len(ts) == 1 and len(ts[0]["postings"]) == 1, "line %d has one posting" % j)
        return ts[0]["postings"][0]

    def account(hd, aid):
        found = [a for a in hd["accounts"] if a["accountId"] == aid]
        check(len(found) == 1, "account %s in the table" % aid)
        return found[0]

    def amount_edited(hd, lg):
        check(posting(lg, 12)["amount"] == "134", "T1 base amount")
        posting(lg, 12)["amount"] = "135"
        return lg

    def account_edited(hd, lg):
        check(posting(lg, 12)["creditAccountId"] == alice, "T2 base credit")
        posting(lg, 12)["creditAccountId"] = bob
        return lg

    def missing(hd, lg):
        check(len(lg[17]["transactions"]) == 1, "T3 base transaction")
        lg[17]["transactions"] = []
        return lg

    def extra(hd, lg):
        t = lg[21]["transactions"][0]
        check(len(t["postings"]) == 1, "T4 base postings")
        t["postings"].append({"amount": "250", "creditAccountId": house, "debitAccountId": round_acct,
                              "eventSequence": "8", "postingIndex": "1"})
        return lg

    def self_transfer(hd, lg):
        p = posting(lg, 1)
        check(p["debitAccountId"] == alice, "T5 base debit")
        p["creditAccountId"] = alice
        return lg

    def zero_amount(hd, lg):
        posting(lg, 2)["amount"] = "0"
        return lg

    def negative_amount(hd, lg):
        p = posting(lg, 3)
        check(p["amount"] == "1000000000", "T7 base amount")
        p["debitAccountId"], p["creditAccountId"] = p["creditAccountId"], p["debitAccountId"]
        p["amount"] = "-1000000000"
        return lg

    def posted_twice(hd, lg):
        t = lg[12]["transactions"][0]
        second = {"kind": KIND_CASH_OUT, "postings": clone(t["postings"]),
                  "transactionId": uuid8("orvex/audit/v2/tamper/second-transaction")}
        check(second["postings"][0]["eventSequence"] == "5", "T8 base event")
        lg[12]["transactions"] = sorted([t, second], key=lambda x: x["transactionId"])
        return lg

    def event_edited(hd, lg):
        check(posting(lg, 17)["eventSequence"] == "6", "T9 base event")
        posting(lg, 17)["eventSequence"] = "5"
        return lg

    def transaction_id_edited(hd, lg):
        lg[1]["transactions"][0]["transactionId"] = transaction_id(rid, 2)
        return lg

    def kind_edited(hd, lg):
        check(lg[12]["transactions"][0]["kind"] == KIND_CASH_OUT, "T11 base kind")
        lg[12]["transactions"][0]["kind"] = KIND_BET
        return lg

    def unreleased(hd, lg):
        check(lg[4]["releases"] == [alice], "T12 base releases")
        lg[4]["releases"] = []
        return lg

    def hold_amount(hd, lg):
        check(lg[1]["holds"] == [{"accountId": alice, "amount": "100"}], "T13 base hold")
        lg[1]["holds"][0]["amount"] = "99"
        return lg

    def release_without_hold(hd, lg):
        check(lg[5]["holds"] == [] and lg[5]["releases"] == [], "T14 base line")
        lg[5]["releases"] = [dave]
        return lg

    def player_renamed(hd, lg):
        account(hd, alice)["player"] = "mallory"
        return lg

    def account_missing(hd, lg):
        account(hd, frank)
        hd["accounts"] = [a for a in hd["accounts"] if a["accountId"] != frank]
        return lg

    def house_renamed(hd, lg):
        account(hd, house)["name"] = "faucet"
        return lg

    def other_round(hd, lg):
        account(hd, round_acct)["roundId"] = "0190f3a8-7b3c-7d4e-8f00-000000000009"
        return lg

    def unreferenced(hd, lg):
        hd["accounts"].append({"accountId": PLAYER_ACCOUNT_PREFIX + "%012x" % 7, "kind": "player",
                               "player": "grace"})
        hd["accounts"].sort(key=lambda a: a["accountId"])
        return lg

    def genesis_rows(hd, lg):
        lg[0]["holds"] = [{"accountId": alice, "amount": "100"}]
        lg[0]["releases"] = [alice]
        return lg

    def line_missing(hd, lg):
        check(len(lg) == 23, "T21 base ledger lines")
        del lg[22]
        return lg

    def lines_swapped(hd, lg):
        lg[4], lg[5] = lg[5], lg[4]
        return lg

    def unknown_key(hd, lg):
        lg[1]["note"] = "x"
        return lg

    def unsorted(hd, lg):
        hd["accounts"][0], hd["accounts"][1] = hd["accounts"][1], hd["accounts"][0]
        return lg

    def hold_only_renamed(hd, lg):
        check([j for j, x in enumerate(lg) if any(h["accountId"] == dave for h in x["holds"])] == [7],
              "T25 dave only holds, at 7")
        account(hd, dave)["player"] = "dan"
        return lg

    def unconsumed_removed(hd, lg):
        check(lg[4]["transactions"] == [], "T26 base line")
        lg[4]["holds"] = []
        lg[4]["releases"] = []
        return lg

    V, R, C = "verify", "replay", "chain"

    def ledger(seq):
        return [check_row(V, 1, "ledger", seq), check_row(R, 0)]

    def fmt(seq):
        return [check_row(V, 1, "format", seq), check_row(C, 1, "format", seq)]

    return [
        ("posting-amount-edited", "line 12 posting amount 134 -> 135", amount_edited, ledger(12)),
        ("posting-account-edited", "line 12 posting creditAccountId alice -> bob", account_edited, ledger(12)),
        ("posting-missing", "line 17 transactions -> []", missing, ledger(17)),
        ("posting-extra",
         "line 21 transaction gains posting 1: round account -> house, amount 250, eventSequence 8",
         extra, ledger(21)),
        ("unbalanced-self-transfer", "line 1 posting creditAccountId -> alice (= debit)", self_transfer, ledger(1)),
        ("unbalanced-zero-amount", "line 2 posting amount -> 0", zero_amount, ledger(2)),
        ("unbalanced-negative-amount", "line 3 posting debit and credit swapped, amount -> -1000000000",
         negative_amount, ledger(3)),
        ("event-posted-twice",
         "line 12 gains a second crash_cash_out transaction with the same posting (event 5); transactions sorted",
         posted_twice, ledger(12)),
        ("posting-event-edited", "line 17 posting eventSequence 6 -> 5", event_edited, ledger(17)),
        ("transaction-id-edited", "line 1 transactionId -> transaction 2's", transaction_id_edited, ledger(1)),
        ("transaction-kind-edited", "line 12 kind crash_cash_out -> crash_bet", kind_edited, ledger(12)),
        ("hold-unreleased", "line 4 releases -> []", unreleased, ledger(4)),
        ("hold-amount-edited", "line 1 hold amount 100 -> 99", hold_amount, ledger(1)),
        ("release-without-hold", "line 5 releases -> [dave]", release_without_hold, ledger(5)),
        ("player-account-renamed", "header: alice's account player -> mallory", player_renamed, ledger(1)),
        ("account-missing", "header: frank's account removed", account_missing, ledger(22)),
        ("house-account-renamed", "header: the house account name -> faucet", house_renamed, ledger("header")),
        ("round-account-other-round", "header: the round account roundId -> ...8f00-000000000009",
         other_round, ledger("header")),
        ("unreferenced-account", "header gains player account ...8fa0-000000000007 of grace",
         unreferenced, ledger("header")),
        ("genesis-ledger-rows", "line 0 holds -> [alice 100], releases -> [alice]", genesis_rows, ledger(0)),
        ("ledger-line-missing", "ledger line 22 deleted", line_missing, fmt(22)),
        ("ledger-lines-swapped", "ledger lines 4 and 5 swapped", lines_swapped, fmt(4)),
        ("ledger-unknown-key", 'ledger line 1 gains "note":"x"', unknown_key, fmt(1)),
        ("accounts-unsorted", "header: first two accounts swapped", unsorted, fmt("header")),
        ("hold-only-account-renamed", "header: dave's account player -> dan (dave only holds, at 7); "
         "limitation L-5", hold_only_renamed, [check_row(V, 0)]),
        ("unconsumed-hold-removed", "line 4 holds and releases -> [] (alice stays referenced); limitation L-5",
         unconsumed_removed, [check_row(V, 0)]),
    ]


# ---------------------------------------------------------------- self-checks (spec section 5.5)

LIT_IDS = [
    (house_account_id, (), "4688699a-7bf4-8e07-8cec-c5f6bef86c36"),
    (round_account_id, ("0190f3a8-7b3c-7d4e-8f00-000000000001",), "13565418-c5ee-8f13-85c6-552f9e8755bd"),
    (round_account_id, ("0190f3a8-7b3c-7d4e-8f00-000000000002",), "aff26097-23f5-858c-90c3-a6a5925eab88"),
    (round_account_id, ("0190f3a8-7b3c-7d4e-8f00-000000000003",), "0bcd27f6-b696-839e-b6a5-dd7a1eb9e3cc"),
    (round_account_id, ("0190f3a8-7b3c-7d4e-8f00-000000000004",), "209fda50-25f4-857b-bbbe-df4491ff3cbe"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000001", 1), "862c9c4b-3cb4-847f-80eb-d0364134723c"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000001", 2), "f6dcb55d-533f-8465-b3fd-ac2d043c0c10"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000001", 3), "7c84957e-9dc3-88c2-ba7f-ba3b6d2ed8b5"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000001", 12), "5687be29-0e53-89bc-886e-babaeada505f"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000001", 17), "4357c225-96f0-803b-a757-26ca7f5055ac"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000001", 21), "d61426fa-cde4-8067-a6f6-163fb1aeb196"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000002", 1), "a839c9f2-fad9-8876-b49f-e366cbb47340"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000002", 5), "0e9dca48-8900-8844-8924-75b55ff85b7e"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000004", 1), "5786a0a2-da6c-887e-a42d-9979ea3a9a33"),
    (transaction_id, ("0190f3a8-7b3c-7d4e-8f00-000000000004", 3), "caff4675-e4f6-8624-a11e-fe7ef6095573"),
]

LIT_WORKED_HEADER = (
    r'{"accounts":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","kind":"player","player":"alice"},'
    r'{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000002","kind":"player","player":"bob"},'
    r'{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000003","kind":"player","player":"zoë \"☃\"\\\u0001😀"},'
    r'{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000004","kind":"player","player":"dave"},'
    r'{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000005","kind":"player","player":"erin"},'
    r'{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000006","kind":"player","player":"frank"},'
    r'{"accountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","kind":"system","name":"round_pool",'
    r'"roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001"},'
    r'{"accountId":"4688699a-7bf4-8e07-8cec-c5f6bef86c36","kind":"system","name":"house",'
    r'"roundId":"00000000-0000-0000-0000-000000000000"}],'
    r'"checkpoints":[{"sequence":"0","stateHash":"1c98ea460213ad0d5900a92fbf37cc3078fa9e822d2065a65037b667734de7e7"},'
    r'{"sequence":"9","stateHash":"bbf9bc3861a0bdcfccf201b185f4f08a9da2c8c61a53142a5caf13b1edd9f90f"},'
    r'{"sequence":"22","stateHash":"ba0cff56c3e9473736d3ef45dfc2d9cd4d4cf0f31ea6f600af9ada84125bb02a"}],'
    r'"configSecret":{"seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"},'
    r'"format":"orvex/audit-history/v2",'
    r'"head":"7cf8cc4b1807099cdd1f16cbc0c790f1a877f30b696ebc0de751997d73ac3da7"}'
)

LIT_IN_PROGRESS_HEADER = (
    '{"accounts":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","kind":"player","player":"alice"},'
    '{"accountId":"209fda50-25f4-857b-bbbe-df4491ff3cbe","kind":"system","name":"round_pool",'
    '"roundId":"0190f3a8-7b3c-7d4e-8f00-000000000004"}],'
    '"checkpoints":[{"sequence":"0","stateHash":"9eafd3962055c7ef130563c7d46870628fc38a8b85e4070816540edec91dde09"},'
    '{"sequence":"3","stateHash":"559fa7527d33184bbc9fe6842a1c1c6fb26620e77f62f52101a06a2972c57e28"}],'
    '"configSecret":{"seed":"8d28ecf551864a1db9dccebdf0b4a358e13c61332beff3a0dd67f69760eb995e"},'
    '"format":"orvex/audit-history/v2",'
    '"head":"f041d95d5bb917ce0f0d51d30c47738c1b97932bebd26ac7f117f98a279c46f6"}'
)

LIT_WORKED_LEDGER = {
    0: '{"holds":[],"kind":"ledger","releases":[],"sequence":"0","transactions":[]}',
    1: ('{"holds":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","amount":"100"}],"kind":"ledger",'
        '"releases":["0190f3a8-7b3c-7d4e-8fa0-000000000001"],"sequence":"1","transactions":[{"kind":"crash_bet",'
        '"postings":[{"amount":"100","creditAccountId":"13565418-c5ee-8f13-85c6-552f9e8755bd",'
        '"debitAccountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","eventSequence":"1","postingIndex":"0"}],'
        '"transactionId":"862c9c4b-3cb4-847f-80eb-d0364134723c"}]}'),
    4: ('{"holds":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001","amount":"50"}],"kind":"ledger",'
        '"releases":["0190f3a8-7b3c-7d4e-8fa0-000000000001"],"sequence":"4","transactions":[]}'),
    12: ('{"holds":[],"kind":"ledger","releases":[],"sequence":"12","transactions":[{"kind":"crash_cash_out",'
         '"postings":[{"amount":"134","creditAccountId":"0190f3a8-7b3c-7d4e-8fa0-000000000001",'
         '"debitAccountId":"13565418-c5ee-8f13-85c6-552f9e8755bd","eventSequence":"5","postingIndex":"0"}],'
         '"transactionId":"5687be29-0e53-89bc-886e-babaeada505f"}]}'),
    21: ('{"holds":[],"kind":"ledger","releases":[],"sequence":"21","transactions":[{"kind":"crash_settlement",'
         '"postings":[{"amount":"757599784","creditAccountId":"13565418-c5ee-8f13-85c6-552f9e8755bd",'
         '"debitAccountId":"4688699a-7bf4-8e07-8cec-c5f6bef86c36","eventSequence":"9","postingIndex":"0"}],'
         '"transactionId":"d61426fa-cde4-8067-a6f6-163fb1aeb196"}]}'),
    22: ('{"holds":[{"accountId":"0190f3a8-7b3c-7d4e-8fa0-000000000006","amount":"10"}],"kind":"ledger",'
         '"releases":["0190f3a8-7b3c-7d4e-8fa0-000000000006"],"sequence":"22","transactions":[]}'),
}

LIT_INSTANT_LEDGER_5 = (
    '{"holds":[],"kind":"ledger","releases":[],"sequence":"5","transactions":[{"kind":"crash_settlement",'
    '"postings":[{"amount":"500","creditAccountId":"4688699a-7bf4-8e07-8cec-c5f6bef86c36",'
    '"debitAccountId":"aff26097-23f5-858c-90c3-a6a5925eab88","eventSequence":"5","postingIndex":"0"}],'
    '"transactionId":"0e9dca48-8900-8844-8924-75b55ff85b7e"}]}'
)

# Spec section 9.2: records, accounts, transactions, postings, holds, releases.
COUNTS = {
    "worked-example": (23, 8, 6, 6, 7, 7),
    "instant-crash": (6, 3, 2, 2, 1, 1),
    "empty-round": (4, 0, 0, 0, 0, 0),
    "in-progress": (4, 2, 2, 2, 1, 1),
}

HOUSE_NETS = {"worked-example": -757_599_784, "instant-crash": 500}
WORKED_HOLDS = [(1, 100), (2, 250), (3, 1_000_000_000), (4, 50), (7, 10), (11, 10), (22, 10)]


def self_check_literals():
    for f, args, want in LIT_IDS:
        check(f(*args) == want, "section 5.5 ID %s%r" % (f.__name__, args))
    check(player_account_id(P3) == PLAYER_ACCOUNT_PREFIX + "000000000003", "P3 account")


def self_check_history(name, records, header_line, ledger_lines, table, nets, stats):
    rec_n, acc_n, tx_n, post_n, hold_n, rel_n = COUNTS[name]
    check((len(records), len(table), stats["transactions"], stats["postings"], stats["holds"], stats["releases"])
          == (rec_n, acc_n, tx_n, post_n, hold_n, rel_n), "%s section 9.2 counts" % name)
    check(len(ledger_lines) == len(records), "%s one ledger line per record" % name)
    check(sum(nets.values()) == 0, "%s account nets sum to 0" % name)
    referenced = set()
    for x in ledger_lines:
        referenced.update(h["accountId"] for h in x["holds"])
        referenced.update(x["releases"])
        for t in x["transactions"]:
            for p in t["postings"]:
                referenced.update((p["debitAccountId"], p["creditAccountId"]))
    check(referenced == {a["accountId"] for a in table}, "%s account table is closed" % name)
    rid = records[0]["roundId"]
    settled = [item["event"] for r in records[1:] for item in r["events"] if item["event"]["type"] == "RoundSettled"]
    if settled:
        e = settled[0]
        check(nets.get(round_account_id(rid), 0) == 0, "%s round account net 0" % name)
        check(stats[KIND_BET] == int(e["totalBetCredits"]), "%s posted bets equal totalBetCredits" % name)
        check(stats[KIND_CASH_OUT] == int(e["totalPayoutCredits"]), "%s posted payouts equal totalPayoutCredits" % name)
    if name in HOUSE_NETS:
        check(nets.get(house_account_id(), 0) == HOUSE_NETS[name], "%s house net" % name)
    if name == "worked-example":
        check(stats["holdList"] == WORKED_HOLDS, "worked-example holds")
        check(header_line == LIT_WORKED_HEADER.encode("utf-8"), "worked-example header line")
        for j, lit in LIT_WORKED_LEDGER.items():
            check(ocj(ledger_lines[j]) == lit.encode("utf-8"), "worked-example ledger line %d" % j)
    if name == "instant-crash":
        check(ocj(ledger_lines[5]) == LIT_INSTANT_LEDGER_5.encode("utf-8"), "instant-crash ledger line 5")
    if name == "in-progress":
        check(header_line == LIT_IN_PROGRESS_HEADER.encode("utf-8"), "in-progress header line")
        check(nets.get(round_account_id(rid), 0) == -2320, "in-progress round account net")


# ---------------------------------------------------------------- build


def counts(records):
    commands = records[1:]
    accepted = sum(1 for r in commands if r["result"]["status"] == "accepted")
    events = sum(len(r["events"]) for r in commands)
    return len(records), len(commands), accepted, len(commands) - accepted, events


def build():
    self_check_literals()
    out = {}
    histories = []
    worked = None
    for name in NAMES:
        v1_header, record_lines, records = read_v1(name)
        table, ledger_lines, nets, stats = build_ledger(records)
        header = v2_header(v1_header, table)
        self_check_history(name, records, ocj(header), ledger_lines, table, nets, stats)
        out[name + ".jsonl"] = history_bytes(header, record_lines, ledger_lines)
        n, c, a, r, e = counts(records)
        histories.append({
            "name": name, "file": name + ".jsonl", "base": name + ".jsonl", "records": n, "commands": c,
            "accepted": a, "rejected": r, "events": e, "head": v1_header["head"],
            "checkpoints": [{"sequence": int(cp["sequence"]), "stateHash": cp["stateHash"]}
                            for cp in v1_header["checkpoints"]],
            "accounts": len(table), "transactions": stats["transactions"], "postings": stats["postings"],
            "holds": stats["holds"], "releases": stats["releases"],
        })
        if name == "worked-example":
            worked = (header, record_lines, ledger_lines, records[0]["roundId"])

    base_header, base_records, base_ledger, rid = worked
    tamper = []
    for name, description, mutate, checks in tamper_cases(rid):
        header = clone(base_header)
        ledger_lines = mutate(header, clone(base_ledger))
        path = "tamper/" + name + ".jsonl"
        data = history_bytes(header, base_records, ledger_lines)
        check(data != out["worked-example.jsonl"], "%s changes the base" % name)
        out[path] = data
        tamper.append({"name": name, "file": path, "base": "worked-example", "description": description,
                       "checks": checks})
    check(len(tamper) == 26, "26 tamper fixtures")

    manifest = {"format": FORMAT_V2, "histories": histories, "tamper": tamper}
    out["manifest.json"] = (json.dumps(manifest, indent=1, sort_keys=True) + "\n").encode("ascii")
    return out


def main(argv):
    parser = argparse.ArgumentParser(description="orvex/audit-history/v2 golden-history oracle")
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--write", action="store_true", help="write the histories, tamper fixtures and manifest")
    mode.add_argument("--check", action="store_true", help="byte-compare the committed files")
    args = parser.parse_args(argv)

    try:
        out = build()
    except OracleError as err:
        print("audit-golden-v2: %s" % err, file=sys.stderr)
        return 2

    if args.write:
        (HERE / "tamper").mkdir(exist_ok=True)
        for name, data in out.items():
            (HERE / name).write_bytes(data)
        print("audit-golden-v2: wrote %d files" % len(out))
        return 0

    failed = False
    for name, data in out.items():
        path = HERE / name
        try:
            on_disk = path.read_bytes()
        except FileNotFoundError:
            on_disk = None
        if on_disk != data:
            print("audit-golden-v2-check: %s differs from generator output" % name, file=sys.stderr)
            failed = True
    tamper_dir = HERE / "tamper"
    if tamper_dir.is_dir():
        for path in sorted(tamper_dir.iterdir()):
            if "tamper/" + path.name not in out:
                print("audit-golden-v2-check: unexpected file tamper/%s" % path.name, file=sys.stderr)
                failed = True
    if failed:
        return 1
    print("audit-golden-v2-check: %d files match" % len(out))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

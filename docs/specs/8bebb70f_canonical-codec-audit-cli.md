# Canonical event codec, hash chain and `crashctl audit` (LBT-9)

Status: **APPROVED** on 2026-10-14. The engineer approved decisions A-1 to A-20 as written, with no
edits, and accepted every point in section 2, including the ADR-007 layout deviation and the LBT-6
contract change. Section 0 described the planning run and no longer applies; implementation follows
section 10, as planned in `docs/specs/8bebb70f_audit-codec-implementation_v2.md`.

- Linear: [LBT-9](https://linear.app/lbtworks/issue/LBT-9/build-canonical-event-codec-and-audit-replay-cli),
  milestone M1 · Deterministic Core, blocks LBT-10.
- Acceptance (Linear): "golden history verifies and tampered/reordered history fails; exported replay
  reconstructs identical state hash. Reference ADR-001/006."
- Governing ADRs: ADR-001 (primary), ADR-003, ADR-006, ADR-004, ADR-005, plus ADR-002 and ADR-007
  (mirrors in `docs/adr/`).
- Builds on: `internal/game/{contract.go,registry.go}` (LBT-6), `internal/game/crash/v1/` (LBT-7 math,
  LBT-8 rules), `cmd/crashctl/`. Approved specs: `specs/e96d124d_crash-v1-math-spec.md` ("math spec",
  D-1 to D-14), `specs/712614a2_crash-lifecycle-rules.md` ("lifecycle spec"),
  `specs/9d7bead2_game-contract-registry.md` ("contract spec").

Decision IDs are `A-n` so they do not clash with the math spec's `D-n`.

## 0. What this run delivers

- This document is the deliverable. Its copy under `docs/specs/` is the record the engineer approves.
- **Builder, this run: make no repository changes.** Do not write Go code, the Python oracle, fixtures,
  or touch `go.mod`, `go.sum`, `Makefile`, CI, `README.md`, `AGENTS.md` or `docs/adr/`. The prompt puts
  all of that out of scope until approval. The gates (`make test`, `make lint`, `go vet ./...`,
  `make build`, `make golden-check`) must still pass on the unchanged tree.
- After approval, section 10 is the implementation plan. Values in sections 5 and 8 that depend on a
  decision (key names, tags, hashes) were computed for the proposed values. If the engineer edits a
  decision, the implementation run recomputes them with the oracle and updates this spec first.

## 1. Current state (verified while planning)

- `main` at `6ec9f16`. Toolchain: go1.26.5, Python 3.14.6. `go.mod` requires only
  `github.com/cockroachdb/apd/v3 v3.2.3`. This plan adds no module.
- `game.Descriptor{Game ID; RulesVersion, SchemaVersion, StateVersion uint32}`. `game.Registry.Lookup`
  never falls back to another version and returns `ErrUnknownRules`. `game.Adapter` has
  `Descriptor/InitialState/Decide/Evolve` and no codec. The contract spec deferred "Encoding/decoding
  hooks on Adapter" and "the encoding of rejections in terminal command results" to LBT-9.
- `game.Input{Sequence uint64; EffectiveTimeUnixMicro int64}` is the deterministic input envelope.
- crash/v1: `GameID "crash"`, `RulesVersionNumber 1`, `SchemaVersion 1`, `StateVersion 1`; textual
  `RulesVersion = "crash/v1"` (math spec). Commands carry `Authority`. Events, `State`, `Bet` and
  `Config` hold only fixed-width ints, `[N]byte` arrays, `PlayerID string` and `uint8` enums; bets are a
  slice in placement order. No maps (lifecycle spec item 11 shaped `State` for this issue).
- `Decide` returns `*RejectionError{Reason}` for 13 stable reason codes, or `ErrInvalidState` /
  `ErrArithmetic`, which fail closed with no terminal result (lifecycle spec item 8).
- `TestNoIOImports` (`crash/v1/params_test.go`) allows only a fixed import list, `fmt.Errorf`, the six
  sentinel `var`s and no floats in every non-test file of the package.
- `cmd/crashctl` is a scaffold: `-version`, otherwise "not implemented yet", exit 1.
- `make golden-check` runs `internal/game/crash/v1/testdata/golden/generate.py --check`; CI runs it.
- Measured while planning:
  - Python `json.dumps(v, ensure_ascii=False, separators=(",",":"), sort_keys=True)` leaves U+007F and
    U+2028 literal and writes control characters as `\b \t \n \f \r` or lowercase `\u00xx`, which is
    RFC 8785 string serialization.
  - Go `encoding/json` writes `{"k":"\u2028\u003c\u0026\ufffd"}` for `"\u2028<&\xff"`: it escapes
    U+2028 and HTML characters, and silently replaces invalid UTF-8.
  - Go 1.26 ships RFC 8785 canonicalization only as experimental `encoding/json/jsontext`
    (`GOEXPERIMENT=jsonv2`). Its documentation states that JCS loses integers beyond ±2⁵³.

## 2. ADR alignment, conflicts and flagged interpretations

No decision contradicts an ADR or an approved math-spec decision. These points need the engineer's
explicit attention (AGENTS.md "Flag conflicts explicitly"). Copy them into the implementation PR.

1. **ADR-007 layout (deviation, needs approval).** AGENTS.md says the layout is ADR-007's "Proposed
   layout". A-17 adds `internal/canonjson/` (not in the layout) and `internal/audit/chain/` (a third
   subpackage next to the listed `replay` and `verification`). ADR-007 calls the layout "a starting
   convention", and `internal/buildinfo` is a precedent. Alternatives are in A-17.
2. **ADR-006 "canonical event hash chain" (interpretation).** The chain links one record per executed
   command (A-7). Each record embeds that command's events with a round-contiguous event sequence, so
   every event is chained in order. The links are command records, not individual events, so that
   rejected commands are chained too.
3. **D-12 "stored in events together with rulesVersion = "crash/v1"" and lifecycle spec item 12 "the
   event envelope carries the rules version" (interpretation).** The rules version is stored once per
   round in the genesis record as the registry key (`"game":"crash"`, `"rulesVersion":"1"`). Every
   event is chained to it. `crash/v1` is the display form `game + "/v" + rulesVersion` (A-5).
4. **ADR-001 "event and schema versions" (interpretation).** The event version is the round's
   `schemaVersion`. There is no per-event version field (A-5).
5. **LBT-6 contract change (not an ADR conflict).** `game.Adapter` gains codec methods and `game.Adapt`
   gains a mandatory codec argument (A-6). The contract spec deferred exactly this to LBT-9.
   `contract.go`'s `Decide` comment ("A non-nil error rejects the command") is narrowed to match
   LBT-8: only errors the codec recognises as rejections become terminal results; any other error
   fails closed.
6. **ADR-001 trade-off "tamper-evident only against a separately trusted anchor" (limitation).**
   Someone who rewrites a history and recomputes every hash can pass `verify` if the forgery is
   rule-consistent (tamper case R26). `--expect-head` (A-16) compares the head with a value held
   elsewhere. Designing anchoring stays out of scope.
7. **ADR-002 timing (no verifier check).** The verifier does not require effective times to be
   non-decreasing across sequences. That is an LBT-11 property (`clock_timestamp()` can step back).
8. **ADR-007 tooling.** The second golden oracle is again a Python standard-library test oracle (A-18).
   The math spec's D-14 approved Python for test oracles only, and this stays within that approval.
9. **ADR-007 `make audit`.** Not added. Without PostgreSQL exports (LBT-10) it would only repeat
   `go test`. Add it when there are real histories to verify.
10. **ADR-004 "canonical encoding" among constants to freeze before the first release.** A-1 to A-5
    freeze it. After the first release ADR-004 (Linear source and mirror, same change) should
    reference this spec, as math spec §1.3 item 3 already notes for the math constants. That
    amendment is not part of this issue.

| ADR clause | Where satisfied |
|---|---|
| ADR-001: immutable ordered history authoritative; state replay; decision replay with historical rules | A-7, A-13, A-14, A-15 |
| ADR-001: preserve commands, effective times, event and schema versions, rule/build identifiers, commitments/reveals, state checkpoints | A-5, A-8, A-11, A-12 |
| ADR-001: canonical event/state encodings, per-round hash chains | A-1 to A-4, A-9, A-10 |
| ADR-001 invariants: contiguous event sequence; identical canonical state hash; decisions attributable to command and rules version | A-8, A-10, A-13 |
| ADR-003: compare canonical events in decision verification; rulesVersion/schemaVersion/stateVersion kept separate; historical serializers retained | A-15, A-5, A-6, A-20 |
| ADR-004: authoritative numeric outputs in events; commitment before betting, reveal after settlement | A-4, A-12 |
| ADR-005: one execution transaction appends events and one terminal result | A-7, section 11 |
| ADR-006: validate chain, state replay hash, decision reproduction, commitment/reveal; fail closed on unknown artifacts; golden replay vectors, fuzz | A-13, A-14, A-18, A-19, section 9 |
| ADR-007: single module, no new dependency, thin `cmd/crashctl`, historical rules selected by version | A-6, A-16, A-17 |
| Math spec D-11 (UUID text), D-12 (int64 base-10 text, lowercase hex, no floats) | A-3 |

## 3. Decision summary

Tick a box to approve or edit the value. The implementation run uses approved values only.

| ID | Decision | Proposed value | Approve |
|---|---|---|---|
| A-1 | Canonical encoding format | Orvex Canonical JSON (OCJ): a subset of RFC 8785 with objects, arrays and strings only; integers as D-12 strings | [x] |
| A-2 | Decoding discipline | Strict: only the exact canonical bytes decode; hand-written encoder and parser, no `encoding/json`, no reflection | [x] |
| A-3 | Scalars | ints: D-12 text in a JSON string, range-checked; bytes: fixed-length lowercase hex; UUIDs: D-11 text; enums: fixed strings; zero values always present | [x] |
| A-4 | crash/v1 payload schemas | Key tables in §5.3; codec in `internal/game/crash/v1/codec.go` | [x] |
| A-5 | Where versions live | genesis: `game`, `rulesVersion`, `schemaVersion`, `stateVersion`, `chain`; file header: `format`; state hash document: `game`, `stateVersion`; event version = `schemaVersion`; payloads carry none | [x] |
| A-6 | Codec in the registry | typed `game.Codec[S,C,E,Cfg]`; `Adapter` gains erased codec methods; `game.Adapt(d, g, c)` requires the codec | [x] |
| A-7 | Chain unit | one record per executed admitted command (accepted or rejected), events nested; genesis record at sequence 0 | [x] |
| A-8 | Record contents | §5.4 tables: round ID, sequence (= admission sequence), prev hash, command ID (UUID), effective time, engine build, command, result, events with event sequence | [x] |
| A-9 | Link hash | `SHA-256("orvex/audit/v1/record:" ‖ recordBytes)`; `prevHash` inside the record; genesis `prevHash` = 32 zero bytes; head = hash of the last record | [x] |
| A-10 | State hash | `SHA-256("orvex/audit/v1/state:" ‖ OCJ({"game","state","stateVersion"}))` over the full state, secrets included; at checkpoints, not per link | [x] |
| A-11 | Exported history file | JSON Lines: header line, then one line per record holding exactly the hashed bytes; LF; one round per file; 64 MiB line cap | [x] |
| A-12 | Commitment and reveal | genesis holds the commitment, header `configSecret` the seed; codec checks `SHA-256(seed) == commitment`; `InitialState` and `Evolve` recheck the outcome | [x] |
| A-13 | Verification stages | format → chain → rules → decode (+fairness) → per record: decision replay, then state replay, then checkpoint; first failure wins | [x] |
| A-14 | Historical rules selection | exact registry lookup by (game, rulesVersion); descriptor schema/state versions must match genesis; unknown format/chain/game/version → "unsupported", exit 3 | [x] |
| A-15 | Decision replay comparison | rejections by codec reason code; acceptances by canonical event bytes; a fail-closed `Decide` error on a recorded command fails verification | [x] |
| A-16 | CLI | `crashctl audit {verify\|replay\|chain} [-expect-head HEX] FILE\|-`; exit 0 ok, 1 failed, 2 usage, 3 unsupported, 4 I/O | [x] |
| A-17 | Packages | `internal/canonjson`, `internal/audit/chain`, `internal/audit/replay`, `internal/audit/verification` | [x] |
| A-18 | Golden histories | Python stdlib oracle `internal/audit/verification/testdata/golden/generate.py`; four histories; `make golden-check` runs both oracles | [x] |
| A-19 | Tamper and reorder cases | 26 oracle-made rehashed fixtures (R1–R26) plus 17 Go byte-mutation cases (B1–B17) | [x] |
| A-20 | Immutability | OCJ rules, chain v1, crash/v1 codec and the golden histories freeze with the first crash/v1 release | [x] |

## 4. Decisions

### A-1. Canonical encoding: Orvex Canonical JSON (OCJ)

**Proposal.** Every canonical artifact (commands, events, config, state, chain records, the history
file's lines) is an OCJ document:

- UTF-8 JSON text whose top-level value is an object. Values are objects, arrays and strings only:
  no numbers, `true`, `false` or `null`.
- Object keys are ASCII matching `^[a-z][A-Za-z0-9]*$`, unique, sorted by byte value. For ASCII keys
  this equals RFC 8785's UTF-16 code-unit order.
- No whitespace outside strings.
- String serialization is RFC 8785 §3.2.2.2. §5.1 has the exact rules.

Every OCJ document is already RFC 8785 canonical: canonicalizing it with any JCS implementation
returns the same bytes. Any JCS tool can therefore reproduce the bytes, and so can Python's standard
`json.dumps` with the options above (measured, §1).

**Rationale, against ADR-001/006 and D-12:**
- D-12 fixes integers as canonical base-10 text, lowercase hex for bytes and no floats. A text format
  satisfies it literally. JSON strings carry the D-12 text unchanged.
- Integers as strings sidestep JCS's number rule. JCS formats numbers as IEEE-754 doubles, so int64
  values beyond 2⁵³ cannot round-trip. Valid crash/v1 times reach `MaxStartMicros ≈ 9.2e18`.
- ADR-006 wants an independent verifier. JSON is readable with stock tools, and Python's standard
  library writes identical bytes, which gives an oracle in another language without new tooling (A-18).
- ADR-001: one value has exactly one encoding (A-2), and the format has a public specification
  (RFC 8785) that outlives this codebase.
- Cost: field names repeat in every record and integers take more bytes than binary. Rounds are
  bounded (`MaxBetsPerRound = 10,000`), and the audit path is not hot.

**Alternatives considered.**

| Option | Verdict |
|---|---|
| Full RFC 8785 with JSON numbers for integers | Rejected: integers above 2⁵³ lose precision (D-12 round-trip), and number semantics are floats. |
| Deterministic custom binary (length-prefixed, big-endian int64) | Compact and simple to make canonical, but integers are not base-10 text, so D-12 would need reinterpreting. It is unreadable without our tooling, needs a bespoke spec, and auditors need our parser. |
| Deterministic CBOR (RFC 8949 §4.2.1) | Standard and compact, but needs a Go dependency or a hand-written encoder. Python's standard library has no CBOR, so the oracle would hand-write it (weaker independence). Integers are binary unless forced to strings. |
| Protocol Buffers with deterministic serialization | Rejected: deterministic is not canonical across versions and languages (default omission, unknown fields). Adds `google.golang.org/protobuf`. Protobuf stays a transport format (LBT-13). |
| Go `encoding/json` output | Rejected: HTML and U+2028 escaping, struct-order keys, silent U+FFFD replacement of invalid UTF-8 (measured, §1). Hashes would depend on Go's encoder. |
| Go `encoding/json/jsontext.Canonicalize` | Experimental in Go 1.26 (`GOEXPERIMENT=jsonv2`); a frozen serializer cannot rest on an experimental API. Could become a test cross-check once stable. |
| Bencode | Canonical by design, with base-10 integers, but niche and not a standards-track format. Byte strings would need a hex convention anyway. |

### A-2. Strict decoding, hand-written codec

**Proposal.** `canonjson.Unmarshal(b)` succeeds only if `b` is exactly `canonjson.Marshal(v)` for the
value `v` it returns. It rejects whitespace, unsorted or duplicate keys, numbers, literals, any escape
other than the canonical one, invalid UTF-8, a BOM, trailing bytes and nesting deeper than 32. Typed
decoders reject unknown and missing keys. The encoder and parser are hand-written in
`internal/canonjson` without `encoding/json` or reflection.

**Rationale.** An artifact that decodes must have exactly one byte form. Otherwise two different byte
strings, with different hashes, could stand for one history. Strictness also makes the
`decode(encode(x)) == x` and `encode(decode(b)) == b` fuzz properties exact.

**Alternatives.** Lenient parsing with re-canonicalization: friendlier to hand-edited files, but the
hashed bytes would no longer be the stored bytes, and a verifier that "fixes" input hides tampering.
`encoding/json` with `DisallowUnknownFields` plus a re-encode check: reflection, and its decoder
accepts non-canonical forms that would then need a second pass anyway.

### A-3. Scalar and enum encodings

**Proposal.**

| Go value | OCJ string | Decoder accepts |
|---|---|---|
| `int64` | D-12: `^(0\|-?[1-9][0-9]*)$` | that regex, within int64 |
| `uint64` (sequences) | `^(0\|[1-9][0-9]*)$` | within uint64 |
| `uint32` version (`RulesVersion`, `SchemaVersion`, `StateVersion`) | as `uint64` | within uint32 and non-zero |
| `[N]byte` (seed, commitment, hashes) | lowercase hex, exactly `2N` characters | exactly that |
| UUID `[16]byte` (round ID, command ID) | D-11 text: 36 lowercase characters, 8-4-4-4-12 | exactly that |
| enum | fixed strings from the tables in §5 | exactly those |
| `string` (e.g. `PlayerID`) | the string itself; must be valid UTF-8; no Unicode normalization | any valid UTF-8 (byte-exact; "é" precomposed and decomposed are different players) |

Every field is always present. A zero value is encoded (`"0"`, 64 zero digits, `[]`), never omitted,
so one value has one encoding and no default rule is needed. A nil and an empty slice encode the same
way (`[]`).

**Rationale.** D-12 and D-11 verbatim. Explicit zero values remove the "absent or zero?" ambiguity.

**Alternatives.** `omitempty`-style omission (two encodings per value). Booleans for flags (none are
needed; enums cover them). Base64 for bytes (D-12 says hex).

### A-4. crash/v1 payload schemas and codec location

**Proposal.** The key tables in §5.3 define crash/v1's canonical command, event, config and state
objects and its rejection codes. The rule: key = Go field name with the first letter lowercased, `ID`
written `Id`, plus `"type"` naming the command or event. The codec is `type Codec struct{}` in a new
file, `internal/game/crash/v1/codec.go`. It implements `game.Codec[State, Command, Event, Config]`
(A-6) and is frozen with the rules version.

- Encoders are total over every value `Decide` can be handed or can produce: negative times, empty
  player, `AuthorityNone`.
- They return `game.ErrUnencodable` only for values with no canonical form: an `Authority`, `Phase` or
  `BetStatus` outside its enum; `PhaseInvalid` or `StatusInvalid` in a state; a nil or foreign command
  or event type; a pointer; a string that is not valid UTF-8.

**Rationale.** ADR-003 "Consequences": historical serializers must stay available next to their
rules. Keeping the codec in package `v1` makes "crash/v1" one immutable unit, and `TestNoIOImports`
then guards the codec's purity too.

**Alternatives.** A subpackage `crash/v1/codec`: splits the frozen artifact into two places. An
audit-owned codec: wrong dependency direction, because the engine (LBT-12) must also encode.

### A-5. Where versions live

**Proposal.**

| Version | Lives in | Encoded as |
|---|---|---|
| Game and rules version (registry key) | genesis record: `game`, `rulesVersion` | `"crash"`, `"1"`; display form `crash/v1` |
| Schema version (shape of commands and events) | genesis: `schemaVersion` | `"1"` |
| Event version | is the round's `schemaVersion` | none per event |
| State version (shape of state) | genesis: `stateVersion`; also inside the state-hash document | `"1"` |
| Chain construction (record shapes, hash tags) | genesis: `chain`; and the hash tags themselves | `"orvex/audit/v1"` |
| Export container | history file header: `format` | `"orvex/audit-history/v1"` |
| Engine build that wrote a record | each record: `engineBuild` | `buildinfo.Version` text |

Commands, events, config and state payloads carry no version field. A round keeps one rules version
for life. The genesis record is chained into every later link, and every record repeats the round ID
(A-8).

**Rationale.** One source per fact, so there is no cross-field consistency to check. ADR-003 keeps the
three versions separate. The chain and container versions can evolve independently of game rules: a
ledger section (LBT-12) bumps `format` without touching records.

**Alternatives.** Repeating `rulesVersion` (or all versions) in every record: self-describing rows,
but redundant fields the verifier must cross-check. Per-event-type versions (`BetPlaced/1`): a
dimension the registry cannot select on. Using the string `"crash/v1"`: needs a parse convention the
registry does not have.

### A-6. Codec in the registry

**Proposal.** A new file, `internal/game/codec.go`:

```go
// Codec is the canonical serializer of one rules version (ADR-001, ADR-003).
// Encodings are OCJ objects (internal/canonjson). All methods are pure and
// deterministic and never mutate their arguments.
type Codec[S, C, E, Cfg any] interface {
	// EncodeConfig splits cfg into its public part, recorded in the round's
	// genesis record, and its secret part, revealed later. The round ID is in
	// neither: the genesis record carries it.
	EncodeConfig(cfg Cfg) (public, secret []byte, err error)
	// DecodeConfig rebuilds the config of round roundID. It fails with an error
	// wrapping ErrRevealMismatch when secret does not match public's commitment.
	DecodeConfig(roundID [16]byte, public, secret []byte) (Cfg, error)
	EncodeCommand(c C) ([]byte, error)
	DecodeCommand(b []byte) (C, error)
	EncodeEvent(e E) ([]byte, error)
	DecodeEvent(b []byte) (E, error)
	EncodeState(s S) ([]byte, error)
	// RejectionCode returns the stable code of a deterministic rejection
	// returned by Decide. ok is false for any other error: it fails closed.
	RejectionCode(err error) (code string, ok bool)
}

var (
	ErrMalformed      = errors.New("game: bytes are not a canonical encoding for the rules version")
	ErrUnencodable    = errors.New("game: value has no canonical encoding")
	ErrRevealMismatch = errors.New("game: revealed secret does not match its commitment")
)
```

- `Adapter` gains the type-erased versions of these eight methods, with the same names and `any` in
  place of the type parameters.
- `func Adapt[S, C, E, Cfg any](d Descriptor, g Game[S, C, E, Cfg], c Codec[S, C, E, Cfg]) Adapter`.
  `c` must be non-nil, documented like `g`.
- Erased `Encode*` methods reject a wrong dynamic type, including nil, with `ErrTypeMismatch` before
  the codec runs. Erased `Decode*` methods box the codec's typed result. Codec errors pass through
  unwrapped, as game errors already do.
- `crash/v1`: `Adapter()` returns `game.Adapt(Descriptor(), Rules{}, Codec{})`.
- `contract.go`'s `Decide` comment becomes: "A non-nil error is either a deterministic rejection,
  which the rules version's `Codec.RejectionCode` recognises and the runtime records as a terminal
  command result, or a fail-closed error: no events and no terminal result."

**Rationale.** The verifier reads `(game, rulesVersion)` from a file and must get rules and codec
together. One registry entry per rules version, with the codec mandatory at compile time, means a
registered version cannot lack its serializer (ADR-003 "Consequences"). A bytes-based interface keeps
`internal/game` free of the JSON package.

**Alternatives.**
- (a) Codec methods discovered on the `Game` value by type assertion: no test churn, but a missing
  codec surfaces only at runtime.
- (c) A separate codec registry in `internal/audit`: two lists to keep in sync.
- (d) `AdaptWithCodec` next to an unchanged `Adapt`: leaves a codec-less path that is useless outside
  tests.

The cost of the proposal: about eight `game.Adapt` call sites in `internal/game` tests take a test
codec.

### A-7. Chain unit: one record per executed command

**Proposal.**
- Sequence 0 is the genesis record: round creation, the versions, the public config with the seed
  commitment.
- Sequences 1..N are command records, one per admitted command that execution processed, in admission
  sequence order, whether `Decide` accepted it or rejected it. A command record embeds the command,
  its deterministic input, its terminal result and the events it produced.
- Event sequence numbers start at 1 and are contiguous across the round.
- An admitted command whose `Decide` failed closed has no record. Its round is stuck until operators
  act, so the chain ends before it.

**Rationale.**
- ADR-005 Transaction B commits one command's events and its one terminal result atomically: one
  record per transaction is exactly that unit.
- Rejections are terminal results (ADR-003) and decision replay must reproduce them (`contract.go`).
  A per-event chain would not cover them.
- Record sequence equals `game.Input.Sequence`, so a gap in admissions is a gap in the chain.

**Alternatives.**
- One link per event, with attribution fields on each: rejected commands stay unchained (deletable
  undetected), and one transaction writes several links.
- One mixed stream of admission, result and event entries: admission (Transaction A) and execution
  (Transaction B) would both append to one chain, so link order would depend on timing between two
  writers.
- A per-round Merkle tree: enables inclusion proofs, but ADR-001 decides on hash chains. It could be
  layered on later.

### A-8. Record contents

**Proposal.** §5.4 gives the exact keys. Decisions inside:

- Every record carries `roundId`, `sequence`, `prevHash`, `kind` and `engineBuild`.
- Command records carry `commandId` (UUID, D-11 text), `effectiveTimeUnixMicro`, `command` (codec
  bytes), `result` and `events`.
- `game.Input` maps to `{Sequence: sequence, EffectiveTimeUnixMicro: effectiveTimeUnixMicro}`. Adding a
  field to `game.Input` adds a record key under a new chain version.
- `result` is `{"status":"accepted"}` or `{"reason":<code>,"status":"rejected"}`. A rejected record has
  no events.
- The genesis record carries `chain`, `game`, the three versions, `config` (public), and
  `createdAtUnixMicro`, the round's creation time and so the commitment publication time.
- `engineBuild` is 1–128 printable ASCII bytes (0x21–0x7E). It covers ADR-001's "build identifiers",
  and the verifier does not interpret it.
- Command IDs must be unique within a round. The verifier checks this, which covers ADR-006's
  "duplicate admission".

**Constraint on LBT-11/LBT-13:** command IDs are UUIDs, round IDs are UUIDs (already required by
lifecycle spec item 1), and admission sequences are gapless from 1. Reopen this decision before the
first release if they choose otherwise.

**Alternatives.** An opaque string command ID (needs a length bound and charset rule; UUIDs already
have D-11's form). Omitting `engineBuild` (loses ADR-001's build attribution). Omitting
`createdAtUnixMicro` (chain order alone proves the commitment preceded betting, but the time is a
cheap, useful fact).

### A-9. Link hash

**Proposal.**
- `hash_i = SHA-256("orvex/audit/v1/record:" ‖ recordBytes_i)`, where `recordBytes_i` is the OCJ
  record including its `prevHash` field.
- `prevHash_0` is 64 `0` characters. `prevHash_i = hex(hash_{i-1})` for `i ≥ 1`.
- The head is `hash_N`.
- Each link therefore covers: the round ID, the sequence (= admission sequence), the previous hash,
  the command ID, the effective time, the engine build, the command payload with its authority, the
  result, and every event with its event sequence. Through the chain it also covers the genesis
  versions and the commitment.

Third-party check with stock tools (line 2 of the file is sequence 0):

```sh
{ printf '%s' 'orvex/audit/v1/record:'; sed -n 2p history.jsonl | tr -d '\n'; } | sha256sum
# equals the "prevHash" in line 3
```

**Rationale.** SHA-256 is what ADR-004 fairness already uses, is in Go's and Python's standard
libraries and `sha256sum`. The ASCII tag separates link hashes from state hashes and commitments, and
versions the construction like D-10's `orvex/crash/v1/crash-point:`. A zero genesis value is
conventional, and the genesis record already binds the round ID.

**Alternatives.**
- SHA-512/256: faster on 64-bit CPUs, less common tooling.
- SHA3-256: available in both standard libraries, no advantage here.
- BLAKE3: a dependency, and no Python standard-library support.
- An HMAC-keyed chain: not publicly verifiable.
- Genesis value `SHA-256(tag ‖ roundId)`: redundant with `roundId` inside every record.
- Storing `hash` inside the record: impossible, since it is the hash of the record. Storing it next to
  the line: redundant with the next record's `prevHash` and the header's `head`.

### A-10. Canonical state hash

**Proposal.**
- `stateHash = SHA-256("orvex/audit/v1/state:" ‖ OCJ({"game": <id>, "state": <codec EncodeState>,
  "stateVersion": <n>}))`.
- It covers the full authoritative state, including the seed and the crash outcome. The public
  projection is not hashed.
- State hashes are computed at checkpoints (the history header, and LBT-10 snapshots later), not in
  every link.

**Rationale.** ADR-003 "deterministic canonical state hash" concerns the authoritative state. The
document binds the hash to the state shape version (`StateVersion` doc: "covered by the state hash").
A per-link state hash would make the engine re-encode the whole state on every command: O(n²) bytes
per round, about 20 GB hashed for a 10,000-bet round. Checkpoints give the same replay assurance at
chosen points.

**Alternatives.** A state hash in every link (cost above). Hashing the projection (misses secret
fields that replay must reproduce). An incrementally hashed (Merkle) state (premature).

### A-11. Exported history file

**Proposal.** "Orvex audit history v1", extension `.jsonl`, one round per file:

- Line 1 is the header (OCJ, §5.5): `format`, `head`, `checkpoints`, `configSecret`.
- Lines 2..N+2 are records 0..N, each line exactly the OCJ bytes that were hashed.
- Every line ends with `\n`. No `\r`, no blank lines, no trailing bytes.
- Each line is at most 64 MiB. A `SettleRound` record with 10,000 `BetLost` events is about 1.5 MB.
- `checkpoints` is non-empty, its sequences strictly increase within `[0, N]`, and the last one is `N`,
  so every replay ends with a state-hash comparison.
- `configSecret` is required in v1. Exports are operator artifacts, and for a settled round the seed
  is public anyway.

**Rationale.** The hashed bytes are the file's lines: no re-encoding is needed to hash them, a line
maps 1:1 to an LBT-10 row, the format streams, and both `grep` and `sed` work on it. Python and Go
read it with their standard libraries.

**Alternatives.**
- A single JSON document with embedded record objects: the verifier must re-canonicalize records to
  hash them.
- Records as base64 or JSON-string fields: unreadable.
- A tar or zip of files: more container format for no gain.
- SQLite: ties audit to a storage engine.
- An optional `configSecret` for public exports of unsettled rounds: deferred. `chain` already works
  without interpreting the secret.

### A-12. Seed commitment and reveal

**Proposal.**
- The genesis `config` holds `commitment` (public). The header's `configSecret` holds `seed`.
- The crash codec's `DecodeConfig` checks `SHA-256(seed) == commitment` (`VerifyCommitment`) and
  otherwise fails with `game.ErrRevealMismatch` ("fairness" failure).
- `InitialState(config)` then rederives `C`, `n_c` and `D` from seed and round ID (D-5, D-9, D-10).
- Replay rechecks that `RoundCrashed` reveals exactly that outcome (`evolveRoundCrashed`) and that
  `RoundSettled.seed` equals the seed (`evolveRoundSettled`). Decision replay makes `Decide` emit them
  again and compares the bytes.

The genesis record is therefore the commitment announcement and is safe to publish: it holds no
secret. The seed never enters a hashed record before `RoundSettled`, so LBT-10 can store and protect
it separately (math spec §10 item 3).

**Alternatives.** The seed inside the genesis config: the genesis could never be published before
settlement, and LBT-10 would have to keep the seed inside immutable hashed bytes. Taking the seed only
from `RoundSettled`: crash-specific knowledge in the generic verifier, and in-progress rounds could
not be replayed.

### A-13. Verification stages and order

**Proposal.** §6 is normative. Summary:
- Stages run in this fixed order: format → chain → rules → decode (config first; a reveal mismatch is
  reported as "fairness") → replay.
- Replay goes record by record: decision replay (verify mode only), then state replay of the recorded
  events, then the checkpoint comparison if one exists at that sequence.
- The first failure stops verification. It is reported as `(stage, sequence | header, detail)`.

**Rationale.** A deterministic order gives every tamper case one expected outcome, testable and
explainable. Deciding before evolving reports a forged decision at its own record instead of at a
later symptom.

**Alternatives.** Collecting every failure: friendlier, but later failures are often consequences of
the first. Can be added later as an option.

### A-14. Historical rules selection, fail closed

**Proposal.**
- `registry.Lookup(game, rulesVersion)` is an exact match and never falls back (already
  `Registry.Lookup` semantics).
- The adapter's `Descriptor().SchemaVersion` and `.StateVersion` must equal the genesis values.
- An unknown header `format`, genesis `chain`, game, rules version, or a schema or state version
  mismatch is **unsupported**: `Failure.Unsupported = true`, exit 3.
- An unknown event or command `type`, or an unknown key under a known schema, is a **failure**
  (history invalid for its declared schema), exit 1.
- `crashctl` builds its registry explicitly in `main.go`: `game.NewRegistry(crashv1.Adapter())`.
  Historical versions are added there as they ship.

**Rationale.** ADR-006 "Fail closed on unknown historical schema/rules artifacts" and ADR-007 "the
audit verifier selects the historical rules version". A separate exit code tells operators "use a
verifier build that knows this artifact" rather than "this history is wrong".

### A-15. Decision replay comparison

**Proposal.** For command record `i` with state `s_{i-1}`, run
`events', err := Decide(s_{i-1}, command_i, Input{i, t_i})`:

- If `err != nil` and `RejectionCode(err)` returns `ok`, the record must be
  `{"status":"rejected","reason":code}`.
- If `err != nil` and `ok` is false (fail-closed error), verification fails: an executed record
  exists where the rules now refuse to decide.
- If `err == nil`, the record must be accepted with the same number of events, and
  `EncodeEvent(events'[j])` must be byte-identical to recorded event `j`.

**Rationale.** ADR-003: "compare canonical events". Byte comparison does not depend on Go equality
semantics.

**Alternatives.** `reflect.DeepEqual` on decoded values: ties the verdict to Go types and
nil-versus-empty slices instead of the canonical form.

### A-16. `crashctl audit` CLI

**Proposal.** §7 is normative.
- Usage: `crashctl audit verify|replay|chain [-expect-head HEX] FILE`, where `FILE` may be `-` for
  stdin.
- Output: a fixed-format report on stdout on success; one failure line on stderr otherwise.
- Exit codes: 0 all requested checks passed; 1 verification failed; 2 usage error; 3 unsupported
  artifact (fail closed); 4 I/O error (cannot read input or write output).
- `crashctl -version` keeps its current behavior (exit 1 on write failure).
- `crashctl` with no arguments now prints usage and exits 2 instead of "not implemented", exit 1.

**Alternatives.**
- One `verify` command with an `-only` flag: subcommands map 1:1 to ADR-001's replay kinds.
- Only exit codes 0 and 1: cannot separate "unsupported" from "tampered", which ADR-006's
  fail-closed rule needs.
- JSON report output: deferred, LBT-16 may want it.
- Dropping `-expect-head`: without it the CLI cannot show the ADR-001 anchor limitation or accept an
  externally held head.

### A-17. Packages

**Proposal.**

| Package | Holds | Imports |
|---|---|---|
| `internal/canonjson` | OCJ value model, `Marshal`, strict `Unmarshal`, int/hex/UUID helpers, exact-key-set helper | stdlib only (`errors`, `fmt`, `slices`, `maps`, `strconv`, `strings`, `unicode/utf8`, `encoding/hex`); pure |
| `internal/audit/chain` | record types, record encode/decode, `LinkHash`, `StateHash`, tags and version constants | `canonjson`, `game`, `crypto/sha256` |
| `internal/audit/replay` | state replay and decision replay over decoded records | `game`, `chain` |
| `internal/audit/verification` | history-file reader, stages, `Report`, `Failure` | `chain`, `replay`, `canonjson`, `game`, `io`, `bufio` |

`internal/canonjson` is a leaf: game codecs, audit and persistence (LBT-10) all import it.
`internal/audit/chain` is imported by the engine in LBT-12 to write records. Engine importing
`internal/audit/...` is an accepted direction: the record format is the audit contract.

**Alternatives.** `internal/game/canonjson` (persistence and audit would import a game subpackage for
a generic encoder). `internal/audit/canonjson` (game codecs would import audit: wrong direction).
Merging `chain` into `verification` (the engine would import the verifier to write records).

### A-18. Golden histories and the independent oracle

**Proposal.** §8 is normative.
- A Python 3.11+ standard-library script, `internal/audit/verification/testdata/golden/generate.py`,
  writes four golden histories, 26 tamper fixtures and `manifest.json`.
- Like the LBT-7 oracle it never imports, runs or reads Go code. It types every key name, tag and
  scenario in from this spec.
- Its canonical encoder is Python's `json.dumps` (a different implementation from Go's hand-written
  one), behind an OCJ validator.
- It reads only the LBT-7 oracle's committed `multiplier_ticks.json` for `M(n)`.
- `--write` writes the files; `--check` regenerates them in memory and byte-compares.
  `make golden-check` runs both oracles.

**How this catches encoder bugs.** The Go verifier must reproduce, from Python-made bytes:
- every link hash (Go hashes the stored bytes);
- every event byte string that `Decide` emits (Go's codec re-encodes them for decision replay);
- every checkpoint state hash (Go's codec encodes the replayed state).

A wrong key name, sort order, escape, integer form, hex case, enum string or missing field in Go's
encoder fails at least one of these. The spec literals in §5.6 are a third, hand-checked source that
the Go unit tests and the oracle's self-checks both pin.

**Alternatives.** Fixtures produced by the Go implementation (not independent; LBT-7 ruled this out
for vectors). Reusing the LBT-7 script (its outputs are frozen math artifacts and should not change).
A Go oracle with `encoding/json` (same language, and its encoder is not canonical, §1).

### A-19. Tamper and reorder catalogue

**Proposal.**
- Cases that need an attacker to recompute hashes (R1–R26, §8.4) are oracle-made fixture files, with
  expected outcomes in `manifest.json`.
- Pure byte-level mutations (B1–B17, §8.5) are applied in memory by Go tests to the golden bytes. They
  need no encoder, so independence is not at stake.
- An exhaustive test flips every byte of the smallest golden history and requires that verification
  never succeeds.

### A-20. Immutability

**Proposal.** With the first crash/v1 release these freeze (ADR-001, ADR-003, ADR-007, AGENTS.md):
- the OCJ rules, i.e. `internal/canonjson`'s observable behavior;
- the chain v1 record shapes and tags;
- the history format v1;
- the crash/v1 codec;
- the audit oracle and its fixtures.

A change that alters any byte goes into a new version (`orvex/audit/v2`, a new schema or state
version, `orvex/audit-history/v2`). The old path stays registered and verifiable. The golden histories
are the regression lock.

## 5. Normative formats

### 5.1 OCJ grammar

```
document := object
value    := object | array | string
object   := '{' [ member ( ',' member )* ] '}'
member   := key ':' value        ; key is a string matching ^[a-z][A-Za-z0-9]*$
                                 ; keys strictly increasing by byte value (no duplicates)
array    := '[' [ value ( ',' value )* ] ']'
string   := '"' char* '"'
char     := any Unicode scalar value U+0020..U+10FFFF except '"' and '\', as UTF-8
          | '\"' | '\\' | '\b' | '\t' | '\n' | '\f' | '\r'
          | '\u00' hex hex       ; only for U+0000..U+001F other than the five above; hex is [0-9a-f]
```

No whitespace anywhere outside strings, no BOM, no trailing bytes, nesting depth ≤ 32. U+007F,
U+2028, U+2029 and every non-ASCII scalar are written literally. Escapes such as `\/`, `\u0041` or
`\ud83d\ude00`, and uppercase hex, are invalid.

### 5.2 Hash constructions and identifiers

| Name | Value |
|---|---|
| chain version (genesis `chain`) | `orvex/audit/v1` |
| link hash | `SHA-256("orvex/audit/v1/record:" ‖ recordLineBytes)`; the trailing `\n` is not hashed |
| state hash | `SHA-256("orvex/audit/v1/state:" ‖ OCJ({"game","state","stateVersion"}))` |
| genesis `prevHash` | `0000000000000000000000000000000000000000000000000000000000000000` |
| history format (header `format`) | `orvex/audit-history/v1` |

### 5.3 crash/v1 payloads (schema version 1, state version 1)

Every integer is an A-3 string. `"type"` is in every command and event object.

**Enums.**

| Go | OCJ |
|---|---|
| `AuthorityNone`, `AuthorityPlayer`, `AuthoritySystem` | `"none"`, `"player"`, `"system"` |
| `PhaseBetting`, `PhaseRunning`, `PhaseCrashed`, `PhaseSettled` | `"BETTING"`, `"RUNNING"`, `"CRASHED"`, `"SETTLED"` (`PhaseInvalid` is unencodable) |
| `StatusOpen`, `StatusCashedOut`, `StatusLost` | `"open"`, `"cashed_out"`, `"lost"` (`StatusInvalid` is unencodable) |

**Commands.**

| `type` | other keys |
|---|---|
| `PlaceBet` | `authority`, `player`, `betCredits` |
| `StartRound` | `authority` |
| `CashOut` | `authority`, `player` |
| `CloseRound` | `authority` |
| `SettleRound` | `authority` |

**Events.**

| `type` | other keys |
|---|---|
| `BetPlaced` | `player`, `betCredits`, `placedAtUnixMicro` |
| `RoundStarted` | `startedAtUnixMicro` |
| `BetCashedOut` | `player`, `cashedOutAtUnixMicro`, `cashOutTick`, `cashOutMultiplierScaled`, `payoutCredits` |
| `RoundCrashed` | `closedAtUnixMicro`, `crashPointScaled`, `crashTick`, `crashDeadlineOffsetMicros`, `crashDeadlineUnixMicro` |
| `BetLost` | `player`, `betCredits` |
| `RoundSettled` | `settledAtUnixMicro`, `betCount`, `totalBetCredits`, `totalPayoutCredits`, `seed` (hex64) |

**Config.**
- Public part (genesis `config`): `commitment` (hex64, `SHA-256(seed)`), `minBetCredits`,
  `maxBetCredits`, `maxBets`.
- Secret part (header `configSecret`): `seed` (hex64).
- `DecodeConfig(roundID, public, secret)` returns
  `Config{RoundID: roundID, Seed, MinBetCredits, MaxBetCredits, MaxBets}` after checking the commitment.

**State.**
- Keys: `roundId`, `phase`, `minBetCredits`, `maxBetCredits`, `maxBets`, `commitment`, `seed`,
  `crashPointScaled`, `crashTick`, `crashDeadlineOffsetMicros`, `crashDeadlineUnixMicro`,
  `startedAtUnixMicro`, `closedAtUnixMicro`, `settledAtUnixMicro`, `bets`, `totalBetCredits`,
  `totalPayoutCredits`.
- `bets` is an array in slice (placement) order. Each bet has: `player`, `betCredits`,
  `placedAtUnixMicro`, `status`, `cashedOutAtUnixMicro`, `cashOutTick`, `cashOutMultiplierScaled`,
  `payoutCredits`.

**Rejection codes.** `RejectionCode` returns `string(Reason)` for every `*RejectionError`. These are the
13 codes in `decide.go`: `invalid_command`, `unauthorized`, `time_out_of_range`, `wrong_phase`,
`round_settled`, `invalid_amount`, `duplicate_bet`, `bet_limit_reached`, `no_bet`,
`already_cashed_out`, `not_after_start`, `deadline_passed`, `crash_not_due`.

### 5.4 Chain records (chain `orvex/audit/v1`)

**Genesis record** (`kind` `genesis`, sequence 0):

| key | value |
|---|---|
| `chain` | `"orvex/audit/v1"` |
| `config` | codec public config object |
| `createdAtUnixMicro` | int64 |
| `engineBuild` | 1–128 printable ASCII bytes |
| `game` | `^[a-z][a-z0-9-]{0,31}$` |
| `kind` | `"genesis"` |
| `prevHash` | hex64 (must be all zero; checked in the chain stage) |
| `roundId` | UUID text |
| `rulesVersion`, `schemaVersion`, `stateVersion` | uint32 > 0 |
| `sequence` | uint64 (must be `"0"`; checked in the chain stage) |

**Command record** (`kind` `command`, sequence ≥ 1):

| key | value |
|---|---|
| `command` | codec command object |
| `commandId` | UUID text |
| `effectiveTimeUnixMicro` | int64 (`game.Input.EffectiveTimeUnixMicro`) |
| `engineBuild` | as genesis |
| `events` | array of `{"event": <codec event object>, "eventSequence": uint64 ≥ 1}` |
| `kind` | `"command"` |
| `prevHash` | hex64 |
| `result` | `{"status":"accepted"}` or `{"reason": ^[a-z][a-z0-9_]*$, "status":"rejected"}` |
| `roundId` | UUID text |
| `sequence` | uint64 (`game.Input.Sequence`) |

### 5.5 History header (format `orvex/audit-history/v1`)

| key | value |
|---|---|
| `checkpoints` | non-empty array of `{"sequence": uint64, "stateHash": hex64}` |
| `configSecret` | codec secret config object |
| `format` | `"orvex/audit-history/v1"` |
| `head` | hex64 |

The reader checks `format` before the key set, so a future format's different keys are reported as
unsupported, not malformed.

### 5.6 Literal cross-check values

These values were computed during planning with the Python standard library (`json.dumps` with the A-1
options, `hashlib`) and the committed LBT-7 `multiplier_ticks.json`. Both the Go tests and the oracle's
self-checks must reproduce them exactly. If either disagrees, stop and find which side contradicts the
spec. Never adjust a literal to match code.

Constants: `T0 = 1760000000000000`; round ID `0190f3a8-7b3c-7d4e-8f00-000000000001`; seed bytes
`00 01 … 1f`; commitment `630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd`;
`C = 17577`, `n_c = 941`, `D = 9410000`; `M(500) = 13498`, `M(940) = 17576`, `M(2000) = 33201`.

Payload objects (one line each, exact bytes):

```
{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"}
{"authority":"system","type":"StartRound"}
{"authority":"player","player":"alice","type":"CashOut"}
{"authority":"system","type":"CloseRound"}
{"authority":"system","type":"SettleRound"}
{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"}
{"startedAtUnixMicro":"1760000000000000","type":"RoundStarted"}
{"cashOutMultiplierScaled":"13498","cashOutTick":"500","cashedOutAtUnixMicro":"1760000005000000","payoutCredits":"134","player":"alice","type":"BetCashedOut"}
{"closedAtUnixMicro":"1760000009410000","crashDeadlineOffsetMicros":"9410000","crashDeadlineUnixMicro":"1760000009410000","crashPointScaled":"17577","crashTick":"941","type":"RoundCrashed"}
{"betCredits":"250","player":"bob","type":"BetLost"}
{"betCount":"3","seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f","settledAtUnixMicro":"1760000010000000","totalBetCredits":"1000000350","totalPayoutCredits":"1757600134","type":"RoundSettled"}
{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1"}
{"seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"}
{"betCredits":"100","cashOutMultiplierScaled":"13498","cashOutTick":"500","cashedOutAtUnixMicro":"1760000005000000","payoutCredits":"134","placedAtUnixMicro":"1759999980000000","player":"alice","status":"cashed_out"}
{"reason":"duplicate_bet","status":"rejected"}
```

The last two are a state bet object and a result object.

String escaping. The player `P3` is the Go string `"zoë \"☃\"\\\u0001😀"` (space, quotes, snowman,
backslash, U+0001, U+1F600). `PlaceBet{AuthorityPlayer, P3, 1000000000}` encodes to the hex bytes:

```
7b22617574686f72697479223a22706c61796572222c2262657443726564697473223a2231303030303030303030222c22706c61796572223a227a6fc3ab205c22e298835c225c5c5c7530303031f09f9880222c2274797065223a22506c616365426574227d
```

Worked-example genesis record (sequence 0) bytes:

```
{"chain":"orvex/audit/v1","config":{"commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1"},"createdAtUnixMicro":"1759999970000000","engineBuild":"golden","game":"crash","kind":"genesis","prevHash":"0000000000000000000000000000000000000000000000000000000000000000","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001","rulesVersion":"1","schemaVersion":"1","sequence":"0","stateVersion":"1"}
```

`hash_0 = cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284`

Worked-example record 1 bytes:

```
{"command":{"authority":"player","betCredits":"100","player":"alice","type":"PlaceBet"},"commandId":"0190f3a8-7b3c-7d4e-8f01-000000000001","effectiveTimeUnixMicro":"1759999980000000","engineBuild":"golden","events":[{"event":{"betCredits":"100","placedAtUnixMicro":"1759999980000000","player":"alice","type":"BetPlaced"},"eventSequence":"1"}],"kind":"command","prevHash":"cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284","result":{"status":"accepted"},"roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001","sequence":"1"}
```

`hash_1 = 6cbb649e00d7ac798be564e2a2f90910ffd69caa9f95207e9ee2f9e72afcf061`

Worked-example state-hash document after `InitialState`:

```
{"game":"crash","state":{"bets":[],"closedAtUnixMicro":"0","commitment":"630dcd2966c4336691125448bbb25b4ff412a49c732db2c8abc1b8581bd710dd","crashDeadlineOffsetMicros":"9410000","crashDeadlineUnixMicro":"0","crashPointScaled":"17577","crashTick":"941","maxBetCredits":"1000000000","maxBets":"3","minBetCredits":"1","phase":"BETTING","roundId":"0190f3a8-7b3c-7d4e-8f00-000000000001","seed":"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f","settledAtUnixMicro":"0","startedAtUnixMicro":"0","totalBetCredits":"0","totalPayoutCredits":"0"},"stateVersion":"1"}
```

`stateHash_0 = 1c98ea460213ad0d5900a92fbf37cc3078fa9e822d2065a65037b667734de7e7`

## 6. Verification algorithm

Input: the history bytes, a mode (`chain`, `replay`, `verify`), an optional expected head and a
registry. "Position `i`" is line `i+2`. A failure carries `Stage`, `Sequence` (the position `i`, or
`header`), `Unsupported`, and a detail error.

1. **format** (whole file).
   - Split on `\n`. Every line, including the last, must end with `\n`, and there must be at least two
     lines. A line over 64 MiB fails at its position.
   - Each line must pass strict `canonjson.Unmarshal`.
   - Header: `format` must equal `orvex/audit-history/v1` (otherwise unsupported, at `header`), then
     the exact key set and field syntax of §5.5.
   - Line 2: if it has a `chain` key whose value is not `orvex/audit/v1`, unsupported at 0. A
     line 2 without a `chain` key fails the key-set check below (format at 0).
   - Each record: its `kind` must be `genesis` or `command`, then the exact key set and envelope field
     syntax of §5.4 for that kind.
   - Payload objects (`config`, `command`, `event`, `configSecret`) only need to be OCJ objects here.
2. **chain**. For `i = 0..N`, check in this order and fail at `i` on the first violation:
   - `sequence == i`;
   - `kind` is `genesis` iff `i == 0`;
   - `roundId` equals the genesis `roundId`;
   - `prevHash` is all zero for `i == 0`, otherwise `hex(hash_{i-1})`;
   - `commandId` has not been seen in this round;
   - a rejected record has no events;
   - each `eventSequence` is the next expected value, starting at 1.

   Then:
   - `hash_N` must equal header `head`, and the expected head if given (failure at `N`);
   - checkpoint sequences must strictly increase within `[0, N]` and end at `N` (failure at `header`).
   - `chain` mode stops here and succeeds.
3. **rules** (replay and verify). `Lookup(game, rulesVersion)`; `ErrUnknownRules` is unsupported at 0.
   The descriptor's `SchemaVersion` and `StateVersion` must equal the genesis values, otherwise
   unsupported at 0.
4. **decode**.
   - First `DecodeConfig(roundId, config, configSecret)`: `ErrRevealMismatch` → stage **fairness** at
     0; any other error → decode at 0.
   - Then for `i = 1..N`: `DecodeCommand`, and `DecodeEvent` for each event. Any error → decode at `i`.
5. **replay**.
   - `s, err := InitialState(config)`; an error is a state-replay failure at 0. If a checkpoint exists
     at 0, compare its state hash (state-replay at 0).
   - For `i = 1..N`, in order:
     1. (verify mode) decision replay per A-15; failure → `decision-replay` at `i`.
     2. Evolve each recorded event in order; an error → `state-replay` at `i`.
     3. If a checkpoint exists at `i`: `StateHash(game, stateVersion, EncodeState(s))` must equal it,
        otherwise `state-replay` at `i`.
6. Success returns a `Report` with:
   - format and chain version, round ID, descriptor;
   - record, command, accepted, rejected and event counts;
   - head;
   - the verified checkpoints with their state hashes.

Stage names (stable, used in output and tests): `format`, `chain`, `rules`, `decode`, `fairness`,
`decision-replay`, `state-replay`.

In verify and replay modes every byte of the file is checked: header fields by format, chain,
fairness and checkpoints; record bytes by their hashes. `chain` mode does not check `configSecret` or
the checkpoint hashes.

## 7. `crashctl audit`

```
usage: crashctl [-version]
       crashctl audit verify [-expect-head HEX] FILE
       crashctl audit replay [-expect-head HEX] FILE
       crashctl audit chain  [-expect-head HEX] FILE
FILE "-" reads standard input.
```

| Subcommand | Stages | Needs |
|---|---|---|
| `chain` | format, chain | nothing but the file |
| `replay` | format, chain, rules, decode, fairness, state replay with checkpoints | registry |
| `verify` | all, with decision replay | registry |

Success: stdout, exit 0, lines in this order. Lines in brackets appear only in the named modes.

```
verifier crashctl <buildinfo.Version>
format orvex/audit-history/v1 chain orvex/audit/v1
round <uuid>
[replay, verify] rules <game> <rulesVersion> schema <n> state <n>
chain ok records <N+1> head <hex64>
[replay, verify] fairness ok
[verify] decision-replay ok commands <C> accepted <A> rejected <R> events <E>
[replay, verify] state-replay ok sequence <s> state <hex64>     (one line per checkpoint)
ok
```

Failure: nothing on stdout, one line on stderr:
`crashctl: audit <sub>: <stage> failed at sequence <n>: <detail>`, or with `unsupported` in place of
`failed`, or `at header`.

Exit codes:

| Code | Meaning |
|---|---|
| 0 | all checks of the subcommand passed |
| 1 | verification failed: a malformed, tampered or non-reproducing history |
| 2 | usage: unknown subcommand, missing or extra arguments, bad `-expect-head` (not 64 lowercase hex), unknown flag |
| 3 | unsupported: unknown format, chain, game, rules, schema or state version (fail closed, ADR-006) |
| 4 | I/O: cannot open or read `FILE`, or cannot write stdout |

`run` becomes `run(args []string, stdin io.Reader, stdout, stderr io.Writer) int`. `main.go` stays
thin: it parses arguments, opens the file, builds the registry, calls `verification.Verify`, formats
the report and maps errors to exit codes.

## 8. Golden histories and tamper cases

### 8.1 Oracle rules (`internal/audit/verification/testdata/golden/generate.py`)

1. Python 3.11+, standard library only (`argparse`, `bisect`, `hashlib`, `hmac`, `json`, `pathlib`,
   `sys`). It never imports, runs or reads Go code or `go.mod`.
2. Key names, tags, enum strings, reason codes and scenarios are typed in from this spec.
3. The only input file is `internal/game/crash/v1/testdata/golden/multiplier_ticks.json` (LBT-7
   oracle output), resolved relative to the script.
4. Fairness uses `hashlib.sha256`, `hmac` and `bisect` on the tick table, as in the math spec §5.
5. Encoder: `json.dumps(v, ensure_ascii=False, separators=(",", ":"), sort_keys=True).encode()`,
   preceded by a validator: only dict, list and str; keys match the regex; strings have no surrogates.
6. It contains a small crash/v1 model for the scripted commands. Every scripted step states its
   expected result, and the model must agree, or the script exits non-zero.
7. Self-checks: every §5.6 literal; the crash outcomes in §8.2; `M(500)`, `M(940)`, `M(2000)`.
8. Usage: `--write` writes the files; `--check` regenerates in memory, byte-compares, and exits 1 on a
   difference. JSONL lines end with `\n`. `manifest.json` is
   `json.dumps(indent=1, sort_keys=True) + "\n"`.
9. Never regenerate from Go output. Frozen with the first release (A-20).
10. The module docstring lists the A-9 `sha256sum` check for the worked example's first link.

### 8.2 Golden histories

During planning, each scenario below was run through the LBT-8 Go rules (`Decide` then `Evolve`)
with a scratch test passed to `go test -overlay`, without touching the tree. Every listed result,
reason code, crash outcome, tick, multiplier, payout and total matched.

All four use `engineBuild` `"golden"` and `createdAtUnixMicro = T0 − 30,000,000`. The command ID for
round `k`, sequence `s` is `0190f3a8-7b3c-7d4e-8f0k-` followed by `s` as 12 lowercase hex digits. Times
below are offsets from `T0` in µs. Players `alice`, `bob`, `dave`, `erin` and `frank` are ASCII; `P3` is
the §5.6 string.

**`worked-example.jsonl`**
- Round `…000000000001`, seed bytes `00…1f`; `C 17577`, `n_c 941`, `D 9,410,000`.
- Config: min 1, max 1,000,000,000, `maxBets` **3**.
- Checkpoints at 0, 9 and 22.
- 23 records, 22 commands (8 accepted, 14 rejected), 9 events.

| seq | command (authority) | t | result | events |
|---|---|---|---|---|
| 1 | PlaceBet alice 100 (player) | −20,000,000 | accepted | e1 BetPlaced |
| 2 | PlaceBet bob 250 (player) | −19,000,000 | accepted | e2 BetPlaced |
| 3 | PlaceBet P3 1,000,000,000 (player) | −18,000,000 | accepted | e3 BetPlaced |
| 4 | PlaceBet alice 50 (player) | −17,000,000 | `duplicate_bet` | |
| 5 | PlaceBet dave 0 (player) | −16,000,000 | `invalid_amount` | |
| 6 | PlaceBet "" 10 (player) | −15,000,000 | `invalid_command` | |
| 7 | PlaceBet dave 10 (player) | −14,000,000 | `bet_limit_reached` | |
| 8 | CashOut alice (player) | −13,000,000 | `wrong_phase` | |
| 9 | StartRound (system) | 0 | accepted | e4 RoundStarted |
| 10 | CashOut bob (player) | 0 | `not_after_start` | |
| 11 | PlaceBet erin 10 (player) | +1,000 | `wrong_phase` | |
| 12 | CashOut alice (player) | +5,000,000 | accepted | e5 BetCashedOut tick 500, M 13,498, payout 134 |
| 13 | CashOut dave (player) | +5,500,000 | `no_bet` | |
| 14 | CashOut alice (player) | +6,000,000 | `already_cashed_out` | |
| 15 | CashOut bob (**system**) | +7,000,000 | `unauthorized` | |
| 16 | CloseRound (system) | +9,409,998 | `crash_not_due` | |
| 17 | CashOut P3 (player) | +9,409,999 | accepted | e6 BetCashedOut tick 940, M 17,576, payout 1,757,600,000 |
| 18 | CashOut bob (player) | +9,410,000 | `deadline_passed` (equality loses) | |
| 19 | CloseRound (system) | +9,410,000 | accepted | e7 RoundCrashed |
| 20 | CashOut bob (player) | +9,420,000 | `wrong_phase` | |
| 21 | SettleRound (system) | +10,000,000 | accepted | e8 BetLost bob 250; e9 RoundSettled (3 bets, 1,000,000,350 bet, 1,757,600,134 paid, seed) |
| 22 | PlaceBet frank 10 (player) | +11,000,000 | `round_settled` | |

This covers every command and event type, 12 of the 13 reason codes (`time_out_of_range` needs a
negative admission time, which LBT-8's unit tests cover), the last payable tick, deadline equality,
string escaping and a non-BMP character.

**`instant-crash.jsonl`**
- Round `…000000000002`, seed `SHA-256("orvex/audit/v1/fixture-seed/37")` =
  `e0cc22a8caa1b0c09b790180fa2c3a9f46b83563b5a00a65485b8f6a54f1fe6d`, commitment
  `4f74afc5272a2452c28c2e7f0d8e7b85205cf9a168d2783a0e4806e70321247e`; `C 10000`, `n_c 0`, `D 0`.
- Config: min 1, max 1e9, `maxBets` 10,000.
- Commands:
  1. PlaceBet alice 500 at −5,000,000 → accepted;
  2. StartRound at 0 → accepted;
  3. CashOut alice at +1 → `deadline_passed`;
  4. CloseRound at +1 → accepted (`RoundCrashed`, deadline `T0`);
  5. SettleRound at +2,000,000 → `BetLost` alice 500, `RoundSettled` (1, 500, 0).
- Checkpoint at 5. 6 records, 5 events.

**`empty-round.jsonl`**
- Round `…000000000003`, seed `SHA-256("orvex/audit/v1/fixture-seed/0")` =
  `1fdc2c2201210a3857f273d7600f48ed37295aab586a8189b2fd2866ed5fcde9`, commitment
  `6c2511c9ab3d3a10a8471485c107baa9d2f202e28dc94b1e4fe39b0e3f07fbe1`; `C 110245`, `n_c 4001`,
  `D 40,010,000`.
- Commands:
  1. StartRound at 0;
  2. CloseRound at +40,010,000 (exactly the deadline);
  3. SettleRound at +41,000,000 → `RoundSettled` (0, 0, 0).
- Checkpoint at 3. 4 records, 3 events.

**`in-progress.jsonl`** (unsettled; operator export with `configSecret`)
- Round `…000000000004`, seed `SHA-256("orvex/audit/v1/fixture-seed/2")` =
  `8d28ecf551864a1db9dccebdf0b4a358e13c61332beff3a0dd67f69760eb995e`, commitment
  `f6af5699688e8306e2a6b50cded2a2a384a67b9d434fa8ad8ef3dcd6617c9daf`; `C 38532`, `n_c 2249`,
  `D 22,490,000`.
- Commands:
  1. PlaceBet alice 1,000 at −1,000,000;
  2. StartRound at 0;
  3. CashOut alice at +20,000,000 → tick 2000, M 33,201, payout 3,320.
- Checkpoints at 0 and 3. 4 records, 3 events.

### 8.3 `manifest.json`

```
{
 "format": "orvex/audit-history/v1",
 "histories": [ {"name", "file", "records", "commands", "accepted", "rejected", "events",
                 "head", "checkpoints": [{"sequence", "stateHash"}]} ],
 "tamper": [ {"name", "file", "base", "description",
              "checks": [{"command": "verify"|"replay"|"chain", "args": [...],
                          "exit": 0|1|3, "stage": "<stage>"|null, "sequence": "<n>"|"header"|null}]} ]
}
```

Counts are JSON numbers here: the manifest is test metadata, not an audit artifact.

### 8.4 Oracle-made tamper fixtures (`tamper/<name>.jsonl`, base `worked-example`)

"Rehashed" means the oracle recomputed every later `prevHash` and the header `head` with its own
hasher, as an attacker would. Checkpoints are unchanged unless stated. Each case lists its expected
checks as (subcommand: exit, stage, sequence).

| # | name | mutation (rehashed) | expected |
|---|---|---|---|
| R1 | `reorder-bets` | swap records 1 and 2 (bob's bet first), renumber `sequence` and `eventSequence` | verify: 1 state-replay 9; chain: 0 |
| R2 | `cashout-before-start` | move record 12 to position 9; shift 9–11 to 10–12; renumber | verify: 1 decision-replay 9; replay: 1 state-replay 9 |
| R3 | `payout-edited` | record 12 `payoutCredits` 134 → 135 | verify: 1 decision-replay 12; replay: 1 state-replay 12 |
| R4 | `effective-time-edited` | record 12 `effectiveTimeUnixMicro` +10,000 (event unchanged) | verify: 1 decision-replay 12; replay: 0 |
| R5 | `rejection-reason-edited` | record 13 reason `no_bet` → `wrong_phase` | verify: 1 decision-replay 13; replay: 0 |
| R6 | `accepted-to-rejected` | record 17 result → rejected `deadline_passed`, events removed, later `eventSequence` renumbered | verify: 1 decision-replay 17; replay: 1 state-replay 21 |
| R7 | `crash-point-edited` | record 19 `crashPointScaled` → 20000 | verify: 1 decision-replay 19; replay: 1 state-replay 19 |
| R8 | `commitment-edited` | genesis `commitment` → `SHA-256(32 × 0x01)` | verify: 1 fairness 0; chain: 0 |
| R9 | `genesis-prevhash-nonzero` | genesis `prevHash` last digit → `1` | verify: 1 chain 0 |
| R10 | `roundid-mismatch` | record 7 `roundId` → `…000000000009` | verify: 1 chain 7 |
| R11 | `event-sequence-gap` | record 12 `eventSequence` 5 → 6 | verify: 1 chain 12 |
| R12 | `duplicate-command-id` | record 8 `commandId` = record 7's | verify: 1 chain 8 |
| R13 | `rejected-with-events` | record 4 gains a `BetPlaced` event at `eventSequence` 4; later ones renumbered | verify: 1 chain 4 |
| R14 | `chain-v2` | genesis `chain` → `orvex/audit/v2` | verify: 3 format 0 |
| R15 | `rules-version-2` | genesis `rulesVersion` → `2` | verify: 3 rules 0; chain: 0 |
| R16 | `schema-version-2` | genesis `schemaVersion` → `2` | verify: 3 rules 0 |
| R17 | `state-version-2` | genesis `stateVersion` → `2` | verify: 3 rules 0 |
| R18 | `unknown-game` | genesis `game` → `dice` | verify: 3 rules 0 |
| R19 | `unsorted-keys` | record 5 written with `sequence` as its first key (raw bytes, hash over them) | verify: 1 format 5 |
| R20 | `json-number` | record 5 `"sequence":"5"` → `"sequence":5` | verify: 1 format 5 |
| R21 | `leading-zero-int` | record 1 command `betCredits` → `"0100"` | verify: 1 decode 1 |
| R22 | `uppercase-hex` | genesis config `commitment` in uppercase | verify: 1 decode 0 |
| R23 | `unknown-event-field` | record 1 event gains `"note":"x"` | verify: 1 decode 1 |
| R24 | `unknown-event-type` | record 1 event `type` → `BetVoided` | verify: 1 decode 1 |
| R25 | `unknown-record-kind` | record 5 `kind` → `admission` | verify: 1 format 5 |
| R26 | `delete-rejection` | delete record 13 (`no_bet`); renumber 14–22 → 13–21; final checkpoint sequence → 21 (hash unchanged) | verify: **0** (rule-consistent forgery); verify `-expect-head <worked-example head>`: 1 chain 21 |

### 8.5 Go byte-mutation cases (applied in memory to `worked-example.jsonl`)

| # | mutation (no rehash) | expected (verify) |
|---|---|---|
| B1 | change one digit of record 12's payout | 1 chain 13 |
| B2 | change record 22's player | 1 chain 22 (head) |
| B3 | swap the lines of records 4 and 5 | 1 chain 4 |
| B4 | delete record 13's line | 1 chain 13 |
| B5 | duplicate record 13's line | 1 chain 14 |
| B6 | delete the last line | 1 chain 21 (head) |
| B7 | change one digit of header `head` | 1 chain 22 |
| B8 | change the last digit of header `configSecret.seed` | 1 fairness 0 |
| B9 | change the final checkpoint's `stateHash` | 1 state-replay 22 |
| B10 | change checkpoint 9's `stateHash` | 1 state-replay 9 |
| B11 | header `format` → `orvex/audit-history/v2` | 3 format header |
| B12 | insert a space after a colon in record 5 | 1 format 5 |
| B13 | LF → CRLF everywhere | 1 format header |
| B14 | drop the final `\n` | 1 format 22 |
| B15 | header gains `"note":"x"` (sorted in) | 1 format header |
| B16 | empty input; header-only input | 1 format header; 1 format 0 |
| B17 | every single-byte change (XOR 0x01 at each offset) of `empty-round.jsonl` | never exit 0 |

## 9. LBT-9 acceptance as tests

| Acceptance | Tests (section 10 names them per file) |
|---|---|
| Golden history verifies | `TestGoldenHistories` (verification): all four histories pass verify, replay and chain, with manifest counts and head. `TestAuditGolden` (crashctl): exit 0, last line `ok`. |
| Tampered history fails | `TestTamperFixtures` (R3–R25), `TestByteMutations` (B1, B2, B6–B16), `TestEverySingleByteFlipFails` (B17), `FuzzSingleByteFlip`, `TestAuditFailures` (crashctl exit codes 1 and 3) |
| Reordered history fails | R1, R2 (rehashed reorders caught by replay), B3, B4, B5 (raw reorders caught by the chain) |
| Exported replay reconstructs identical state hash | `TestGoldenHistories` compares every replayed checkpoint hash with the oracle's value in the manifest; `TestAuditReplayStateHash` checks the `state-replay ok sequence N state <hex>` lines of `crashctl audit replay`; `TestWorkedExampleInitialStateHash` pins `stateHash_0` from §5.6 |
| Fail closed on unknown artifacts (ADR-006) | R14–R18, B11 (exit 3) |
| Seed commitment/reveal (ADR-006) | R8, B8, R7, `TestDecodeConfigRevealMismatch` |
| Anchor limitation documented (ADR-001) | R26 (both checks) |

## 10. Implementation plan (after approval)

Build order: steps 1–3, then 4 and 5 in parallel, then 6–9. Rules for every new non-test Go file:
- goimports grouping (stdlib, blank line, `github.com/juano-morello/orvex/...`);
- error strings lowercase without trailing punctuation, prefixed with the package (`canonjson: `,
  `chain: `, `replay: `, `verification: `);
- no floats; no package state besides sentinel errors and constants.

No `go.mod` change.

### Files

| File | Action |
|---|---|
| `internal/canonjson/canonjson.go` | new |
| `internal/canonjson/canonjson_test.go`, `fuzz_test.go` | new |
| `internal/game/codec.go` | new: `Codec[S,C,E,Cfg]`, `ErrMalformed`, `ErrUnencodable`, `ErrRevealMismatch` |
| `internal/game/registry.go` | edit: `Adapter` gains 8 methods, `Adapt(d, g, c)`, erased codec methods |
| `internal/game/contract.go` | edit: package doc mentions codecs; `Decide` comment per A-6 |
| `internal/game/codec_test.go` | new |
| `internal/game/sample_game_test.go` | edit: add `tallyCodec` |
| `internal/game/registry_test.go`, `contract_test.go` | edit: every `game.Adapt` call passes `tallyCodec{}` |
| `internal/game/crash/v1/codec.go` | new: `type Codec struct{}` |
| `internal/game/crash/v1/round.go` | edit: `Adapter()` passes `Codec{}` |
| `internal/game/crash/v1/params_test.go` | edit `TestNoIOImports` only: allow `github.com/juano-morello/orvex/internal/canonjson`; add `codec.go` to the must-check list |
| `internal/game/crash/v1/codec_test.go`, `codec_fuzz_test.go` | new |
| `internal/audit/chain/chain.go`, `chain_test.go` | new |
| `internal/audit/replay/replay.go`, `replay_test.go` | new |
| `internal/audit/verification/verification.go`, `verification_test.go`, `fuzz_test.go` | new |
| `internal/audit/verification/testdata/golden/generate.py` | new (oracle) |
| `internal/audit/verification/testdata/golden/{worked-example,instant-crash,empty-round,in-progress}.jsonl`, `tamper/*.jsonl` (26), `manifest.json` | new, written by `generate.py --write` only |
| `cmd/crashctl/main.go`, `main_test.go` | edit |
| `Makefile` | edit `golden-check` to run both oracles (two recipe lines with `--check`); the comment names both |
| `README.md` | edit: Status adds LBT-9; Layout adds `internal/canonjson`, `internal/audit/{chain,replay,verification}`, `cmd/crashctl` "replay/audit CLI (`crashctl audit`)"; Development `make golden-check` line names both oracles |
| `AGENTS.md` | edit the `make golden-check` line only: it regenerates "the Crash v1 golden vectors and the audit golden histories with the independent Python oracles" |

Unchanged: `go.mod`, `go.sum`, `.github/`, `docs/adr/`, the crash/v1 frozen files
(`params.go`, `curve.go`, `payout.go`, `fairness.go`, `testdata/golden/*`), `decide.go`, `evolve.go`,
`projection.go`, `commands.go`, `events.go`.

### Step 1: `internal/canonjson`

API (names may be refined; behavior may not):

```go
type Value interface{ isValue() } // String, Array or Object
type String string
type Array []Value
type Object map[string]Value

func Marshal(o Object) ([]byte, error)    // keys sorted; ErrInvalid: bad key, invalid UTF-8, nil value, depth > 32
func Unmarshal(b []byte) (Object, error)  // strict (A-2); ErrNotCanonical with byte offset

func Int(n int64) String;  func ParseInt(v Value) (int64, error)
func Uint(n uint64) String; func ParseUint(v Value) (uint64, error)
func ParseVersion(v Value) (uint32, error) // uint32, non-zero
func Hex(b []byte) String; func ParseHex(v Value, n int) ([]byte, error) // exactly n bytes, lowercase
func UUID(id [16]byte) String; func ParseUUID(v Value) ([16]byte, error)
func Text(v Value) (string, error); func List(v Value) (Array, error); func Obj(v Value) (Object, error)
// Fields returns o if its key set is exactly keys, else an error naming missing and unknown keys.
func Fields(o Object, keys ...string) (Object, error)

var ErrInvalid, ErrNotCanonical error
```

Tests (`canonjson_test.go`):
- `TestMarshal`: exact-bytes table:
  - `{}`, `{"a":[]}`, nested objects and arrays;
  - key order `"aB"` before `"aa"` before `"b"`;
  - escapes for `"` `\` U+0008 U+0009 U+000A U+000C U+000D; U+0000 → `\u0000`; U+001F → `\u001f`;
  - literal U+007F, U+2028, `é`, `😀`.
- `TestMarshalRejects`:
  - invalid UTF-8, including `\xff`, overlong `\xc0\xaf` and a surrogate `\xed\xa0\x80`;
  - bad keys `""`, `"A"`, `"1a"`, `"a-b"`, `"é"`;
  - nil value, nil array element, depth 33.
- `TestUnmarshalRoundTrip`: every `TestMarshal` vector parses to an equal value.
- `TestUnmarshalRejects`:
  - whitespace before, between and after tokens; unsorted keys; a duplicate key;
  - `1`, `-0`, `1.0`, `true`, `false`, `null`;
  - `\/`, `\u0041`, `\u001F`, `\u007f`, `\u2028`, `\ud83d\ude00`;
  - a raw U+0001 in a string; the invalid UTF-8 cases above;
  - a BOM, empty input, trailing `x`, a top-level array or string, depth 33.
- `TestInts`:
  - accepted: `0`, `-1`, `9223372036854775807`, `-9223372036854775808`;
  - rejected: `-0`, `01`, `+1`, `1e3`, `" 1"`, `9223372036854775808`, `""`;
  - uint and version cases, including version `0` and `4294967296` rejected.
- `TestHexUUID`: lowercase fixed length accepted; uppercase, odd length, wrong length, braces and
  missing hyphens rejected.

Fuzz (`fuzz_test.go`):
- `FuzzUnmarshal`: if `Unmarshal(b)` succeeds, `Marshal` of the result equals `b`.
- `FuzzStringRoundTrip`: for any string `s`, valid UTF-8 round-trips through `{"k":s}` with stable
  bytes; invalid UTF-8 fails `Marshal`.

### Step 2: `internal/game` codec contract

- Implement A-6. Erased methods follow the existing `mismatch` pattern with roles `config`,
  `command`, `event` and `state`.
- `tallyCodec` (test only) implements `Codec[tallyState, tallyCommand, tallyEvent, tallyConfig]` with
  `canonjson`. It is minimal, but its `EncodeState` must sort map keys, which `canonjson.Object`
  already does.

Tests (`codec_test.go`):
- `TestAdapterCodecRejectsWrongTypes`: `EncodeConfig/EncodeCommand/EncodeEvent/EncodeState` with nil
  and foreign types → `ErrTypeMismatch`, and the codec is not called.
- `TestAdapterCodecPassThrough`: erased results equal typed results; `Decode*` boxes the typed value;
  codec errors are returned unwrapped (`errors.Is` and identity).
- `TestAdapterRejectionCode`: delegates to the typed codec.

Every existing `internal/game` test still passes with the new `Adapt` signature.

### Step 3: crash/v1 codec

- `codec.go` implements §5.3 with `switch` statements: no package `var` tables (`TestNoIOImports`),
  no `fmt` beyond `Errorf`, no floats.
- Errors wrap `game.ErrMalformed`, `game.ErrUnencodable` or `game.ErrRevealMismatch` and never format
  seed bytes.
- `DecodeConfig` uses `VerifyCommitment`.

Tests (`codec_test.go`):
- `TestCodecVectors`: every §5.6 payload line, the `P3` hex, and the state member of the §5.6 state
  document (`EncodeState(InitialState(worked config))` equals it byte for byte). Typed in from this
  spec.
- `TestCodecRoundTrip`:
  - every command, event, config and state shape;
  - edge values `math.MinInt64` and `math.MaxInt64` in times and amounts;
  - `AuthorityNone`, an empty player, `P3`, a zero seed;
  - states in all four phases with bets in all three statuses.
- `TestCodecRejectsMalformed`:
  - missing and unknown keys per type; unknown `type`;
  - authority `"admin"`, phase `"betting"`, status `"open "`;
  - a non-canonical integer, uppercase hex, short hex, a bad UUID;
  - a non-canonical document (whitespace).
- `TestCodecRejectsUnencodable`:
  - `Authority(3)`, `Phase(0)`, `Phase(5)`, `BetStatus(0)`, `BetStatus(4)`;
  - an invalid-UTF-8 player; a nil command; `&PlaceBet{}`; nil and pointer events.
- `TestDecodeConfigRevealMismatch`: the seed and commitment disagree → `errors.Is(err,
  game.ErrRevealMismatch)`, and the error text contains no seed hex.
- `TestEncodeStateCoversEveryField`: for each exported field of `State` and `Bet` (via `reflect`, in
  the test only), change that one field to another valid value and require a different encoding. This
  guards against a new field escaping the state hash without a `stateVersion` bump.
- `TestRejectionCode`: each of the 13 reasons, plain and wrapped with `fmt.Errorf("%w")`, maps to its
  string. `ErrInvalidState`, `ErrArithmetic` and `errors.New("x")` give `ok == false`.

Fuzz (`codec_fuzz_test.go`): `FuzzDecodeCommand` and `FuzzDecodeEvent`: success implies re-encoding
equals the input.

Edits: `TestNoIOImports` allowlist and must-check list. `TestAdapterRegistration` also asserts that
`Adapter().EncodeEvent(RoundStarted{...})` returns the §5.6 bytes.

### Step 4: `internal/audit/chain`

```go
const (
	Version   = "orvex/audit/v1"
	RecordTag = "orvex/audit/v1/record:"
	StateTag  = "orvex/audit/v1/state:"
)
type Hash [32]byte
type Genesis struct {
	RoundID [16]byte; PrevHash Hash; Game game.ID
	RulesVersion game.RulesVersion; SchemaVersion game.SchemaVersion; StateVersion game.StateVersion
	Config []byte // OCJ public config
	CreatedAtUnixMicro int64; EngineBuild string
}
type Event struct { Sequence uint64; Payload []byte }
type Result struct { Rejected bool; Reason string }
type Command struct {
	RoundID [16]byte; Sequence uint64; PrevHash Hash; CommandID [16]byte
	EffectiveTimeUnixMicro int64; EngineBuild string
	Payload []byte; Result Result; Events []Event
}
func (c Command) Input() game.Input
func EncodeGenesis(g Genesis) ([]byte, error)
func EncodeCommand(c Command) ([]byte, error)
func Decode(line []byte) (any, error) // Genesis or Command; envelope syntax per §5.4; payloads must be OCJ objects
func LinkHash(record []byte) Hash
func StateHash(g game.ID, v game.StateVersion, state []byte) (Hash, error)

var ErrUnsupportedChain = errors.New("chain: unsupported chain version")
```

`Decode` checks the `chain` field and returns an error wrapping `ErrUnsupportedChain` for anything but
`Version`. It validates syntax only: zero `prevHash` and sequence rules belong to the chain stage.

Tests (`chain_test.go`, package `chain_test`):
- `TestWorkedExampleGenesis` and `TestWorkedExampleRecord1`: §5.6 bytes and hashes, both from encoding
  structs and from `LinkHash` of the literal bytes.
- `TestWorkedExampleInitialStateHash`: `StateHash("crash", 1, crash Codec.EncodeState(InitialState(...)))`
  equals `stateHash_0`.
- `TestDecodeRoundTrip`.
- `TestDecodeRejects`:
  - a missing or unknown key; kind `admission`;
  - sequence `"01"`; uppercase `prevHash`; a bad UUID;
  - `engineBuild` empty, with a space, or 129 bytes;
  - game `Crash`; version `"0"`;
  - an accepted result with a reason; a rejected result without one; reason `Bad`;
  - `eventSequence` `"0"`; a non-object `command`.
- `TestDecodeUnsupportedChain`.
- `TestStateHashBindsVersion`: the same state bytes under state version 1 and 2, or game `crash` and
  `dice`, hash differently.

### Step 5: oracle and fixtures

Write `generate.py` per §8.1 and run `python3 generate.py --write`. Commit the outputs. Its self-checks
must pass. Spot-check the A-9 `sha256sum` command on the generated worked example.

### Step 6: `internal/audit/replay`

```go
type Mode int
const ( StateOnly Mode = iota + 1; StateAndDecision )
type Record struct {
	Sequence uint64; Input game.Input; Command any
	Result chain.Result; Events []any; EventBytes [][]byte
}
type Mismatch struct { Decision bool; Sequence uint64; Err error }
func (m *Mismatch) Error() string
func (m *Mismatch) Unwrap() error
// Run replays records from initial (sequence 0's state) through a, calling
// visit(sequence, state) after sequence 0 and after every record. A visit
// error stops the run and is returned as is.
func Run(a game.Adapter, initial any, records []Record, mode Mode, visit func(uint64, any) error) error
```

Tests (`replay_test.go`). Records are built in Go from crash/v1 `Decide` and the crash codec; this is
a unit level, not the independence test.
- `TestRunReproducesRound`: both modes pass; `visit` sees sequences 0..N in order.
- `TestDecisionMismatch` (table): changed event bytes; recorded rejected but `Decide` accepts;
  recorded accepted but `Decide` rejects; different reason; event count differs. Each gives
  `Mismatch{Decision: true}` at the right sequence, and `StateOnly` mode passes or fails per A-13.
- `TestFailClosedDecideIsMismatch`: an adapter for a tiny test game whose `Decide` returns a
  non-rejection error yields a decision `Mismatch`.
- `TestEvolveErrorIsStateMismatch`.

### Step 7: `internal/audit/verification`

```go
type Mode int // ModeChain, ModeReplay, ModeVerify
type Options struct { Mode Mode; Registry *game.Registry; ExpectHead *chain.Hash }
type Stage string // the seven names of section 6
type Failure struct { Stage Stage; Sequence int64 /* -1 = header */; Unsupported bool; Err error }
func (f *Failure) Error() string // "<stage> failed|unsupported at sequence N|header: <err>"
func (f *Failure) Unwrap() error
type Checkpoint struct { Sequence uint64; StateHash chain.Hash }
type Report struct {
	Format, Chain string; RoundID [16]byte; Descriptor game.Descriptor
	Records, Commands, Accepted, Rejected, Events int
	Head chain.Hash; Checkpoints []Checkpoint
}
// Verify returns a *Failure for verification outcomes and any other error for I/O.
func Verify(r io.Reader, opt Options) (Report, error)
```

It implements section 6 exactly. It reads with a `bufio.Reader` and enforces the 64 MiB line cap
without buffering more than one oversized line.

Tests (`verification_test.go`, `fuzz_test.go`):
- `TestGoldenHistories`: per manifest history, all three modes pass. The report equals the manifest:
  counts, head, checkpoint sequences and hashes.
- `TestTamperFixtures`: every manifest tamper check gives exactly its stage, sequence and
  `Unsupported` (exit 3 ⇔ `Unsupported`); exit 0 checks return no error.
- `TestByteMutations`: B1–B16.
- `TestEverySingleByteFlipFails`: B17, skipped under `-short`.
- `TestExpectHead`: a matching value passes; a mismatch fails the chain stage at `N`.
- `TestLineLimit`: a 64 MiB + 1 line fails format.
- `TestChainModeNeedsNoRegistry`: nil registry with `ModeChain` works; nil registry with
  `ModeReplay` returns an error, not a panic.
- `TestReadError`: a reader that fails mid-stream returns a non-`Failure` error.
- `FuzzSingleByteFlip(offset, xor)` over `worked-example.jsonl`: any change with `xor != 0` never
  verifies.

### Step 8: `cmd/crashctl`

Implement section 7. Wire `game.NewRegistry(crashv1.Adapter())`. `-expect-head` parses with
`canonjson.ParseHex`-equivalent strictness.

Tests (`main_test.go`):
- `TestRun`: update the "no args" row to exit 2 with usage on stderr. The other rows are unchanged.
- `TestAuditGolden`: every golden history under all three subcommands → exit 0, last stdout line
  `ok`, and `chain ok records … head <manifest head>`.
- `TestAuditReplayStateHash`: `audit replay worked-example.jsonl` prints exactly the manifest's
  `state-replay ok sequence s state <hash>` lines for s = 0, 9, 22.
- `TestAuditFailures`: R3 → 1 (`decision-replay failed at sequence 12`); R15 → 3 (`rules unsupported
  at sequence 0`); B1 via a temp file → 1; R26 with and without `-expect-head`.
- `TestAuditUsage`: `audit` alone; an unknown subcommand; no file; two files; `-expect-head` that is
  not hex or is uppercase; an unknown flag → 2.
- `TestAuditIO`: a nonexistent file → 4; a failing stdout writer → 4; `-` reads the golden history
  from stdin → 0.
- `TestRunWriteError` stays as is.

Fixtures are referenced as `../../internal/audit/verification/testdata/golden/...`.

### Step 9: docs and Makefile

Edit as in the files table. CI needs no change (`make golden-check` already runs).

### Verification for the implementation run

Each must exit 0:

```sh
make test
make lint
go vet ./...
make build
make golden-check
python3 internal/audit/verification/testdata/golden/generate.py --check
./bin/crashctl audit verify internal/audit/verification/testdata/golden/worked-example.jsonl
./bin/crashctl audit replay internal/audit/verification/testdata/golden/in-progress.jsonl
{ printf '%s' 'orvex/audit/v1/record:'; sed -n 2p internal/audit/verification/testdata/golden/worked-example.jsonl | tr -d '\n'; } | sha256sum
# must print cfdd053761b51e0dd2c301c785c2bca451a31ed10cdb62f8c1da6cc6b8ecc284
```

And these must exit with the code shown:
- `./bin/crashctl audit verify .../tamper/payout-edited.jsonl` → 1;
- `./bin/crashctl audit verify .../tamper/rules-version-2.jsonl` → 3.

`git diff --stat` must show no change to `go.mod`, `go.sum` or the crash/v1 frozen files.

If Go and the oracle disagree, find which side contradicts this spec and fix that side. Never
regenerate fixtures from Go output, and never edit a §5.6 literal to match code.

## 11. Expectations on later issues (not designed here)

**LBT-10 (PostgreSQL schema, ADR-005).**
- Rows:
  - one immutable row per chain record: `round_id uuid`, `sequence bigint` (0 = genesis),
    `record bytea` (the exact OCJ bytes), `hash bytea` (32 bytes, the A-9 link hash),
    `prev_hash bytea`;
  - primary key `(round_id, sequence)`, which also makes a fork impossible;
  - `CHECK (octet_length(hash) = 32)`.
- Never store the canonical bytes as `jsonb`, which reorders keys and drops duplicates. Columns
  derived for queries (event type, player, amounts, an events table keyed by
  `(round_id, event_sequence)`) are never the source for hashing or export.
- The export reads `record` bytes verbatim, orders by `sequence`, writes `head` = the last row's
  `hash`, and never re-encodes from columns.
- Admission rows (LBT-11) store the codec's canonical command bytes, the command ID (UUID), the
  gapless sequence and the effective time. Transaction B copies them into the record.
- Transaction B computes record bytes, `hash` and `prev_hash` (the previous row's `hash`, read under
  the same per-round serialization and fencing) and inserts them together with the terminal result,
  ledger and outbox.
- Round creation writes the genesis row in the same transaction that creates the round.
- The seed is stored outside record bytes, with protection chosen by LBT-10. It is joined into the
  export's `configSecret`.
- Snapshot rows `(round_id, sequence, state_hash, state bytes?)` become export checkpoints. Their
  hashes must come from the engine's state at commit time, not from re-running replay at export, or
  the comparison proves nothing.
- Retention of record rows is an LBT-10 and operations decision; ADR-001 requires it before claiming
  audit-grade behavior.

**LBT-11.** Gapless per-round admission sequences from 1, assigned by a row-lock counter and not a
PostgreSQL `SEQUENCE`. UUID command IDs. UTF-8 validation of player IDs at admission (the codec
rejects invalid UTF-8). Effective-time monotonicity is its own property; the verifier does not check
it.

**LBT-12 (ledger).**
- The verifier gains a `ledger` stage after state replay, and the history format becomes
  `orvex/audit-history/v2` with a `ledger` section. v1 files stay verifiable. A v1 verifier reports a
  v2 file as unsupported (exit 3), because the header `format` is checked first.
- Checks:
  - every posting references a chained `(roundId, eventSequence)`;
  - each posting transaction balances (debits = credits);
  - postings equal those derived from the chained events by a pure crash/v1 posting rule;
  - each bet settles at most once (ADR-001);
  - totals equal `RoundSettled`.
- Cross-round account balances need a multi-round export and are outside one round's verifier.
- If LBT-12 decides postings must be hash-committed, it introduces chain `orvex/audit/v2` with a
  posting digest in command records. v1 rounds keep verifying under v1.

**LBT-14 and LBT-16.** Recovery and the visual replay debugger can reuse `internal/audit/replay`.
Visual replay stays separate from cryptographic verification (ADR-006). Not designed here.

## 12. Out of scope and open items

Out of scope: Go code, fixtures, `go.mod` changes in this run; the PostgreSQL schema (LBT-10);
admission (LBT-11); the ledger (LBT-12); visual replay (LBT-16); external hash anchoring (ADR-001
future measure); a database export command; JSON report output; multi-round files.

Open for the engineer:
1. The A-17 layout additions (section 2 item 1).
2. The A-6 change to `game.Adapt` and `game.Adapter`.
3. Whether `configSecret` should become optional for public exports of unsettled rounds (A-11
   alternative; would be format v2 or an approved v1 change before release).
4. The worked example's `maxBets = 3` (chosen to reach `bet_limit_reached`). It is a fixture value,
   not a rules constant.

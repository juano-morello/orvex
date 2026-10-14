// Package chain defines the per-round audit hash chain, chain version
// orvex/audit/v1 (ADR-001; decisions A-7 to A-10 and sections 5.2 and 5.4 of
// docs/specs/8bebb70f_canonical-codec-audit-cli.md).
//
// A round's chain is a genesis record at sequence 0 followed by one command
// record per executed admitted command, accepted or rejected, in admission
// sequence order. Each record is an OCJ document (internal/canonjson) that
// embeds the previous record's link hash, so the head hash covers the whole
// round. State hashes bind a codec's canonical state bytes to the game and
// state version.
//
// The record shapes, tags and hash constructions are frozen with the first
// crash/v1 release (A-20); a change goes into a new chain version.
package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
)

// Chain construction identifiers (spec section 5.2).
const (
	// Version is the genesis record's "chain" value.
	Version = "orvex/audit/v1"
	// RecordTag prefixes a record's bytes in its link hash.
	RecordTag = "orvex/audit/v1/record:"
	// StateTag prefixes the state-hash document.
	StateTag = "orvex/audit/v1/state:"
)

// Record kinds.
const (
	KindGenesis = "genesis"
	KindCommand = "command"
)

// Result statuses.
const (
	statusAccepted = "accepted"
	statusRejected = "rejected"
)

// maxEngineBuild is the longest engineBuild value in bytes.
const maxEngineBuild = 128

// ErrUnsupportedChain reports a genesis "chain" value other than Version.
var ErrUnsupportedChain = errors.New("chain: unsupported chain version")

// Hash is a SHA-256 link or state hash.
type Hash [32]byte

// String returns h as 64 lowercase hex characters.
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

// Genesis is the record at sequence 0: the round's identity, versions and
// public config, which carries the seed commitment (A-8, A-12).
type Genesis struct {
	RoundID [16]byte
	// Sequence must be 0 in a valid chain. It is kept so that Decode can
	// return a non-zero value for the chain stage to report.
	Sequence      uint64
	PrevHash      Hash
	Game          game.ID
	RulesVersion  game.RulesVersion
	SchemaVersion game.SchemaVersion
	StateVersion  game.StateVersion
	// Config is the codec's public config, an OCJ object.
	Config             []byte
	CreatedAtUnixMicro int64
	EngineBuild        string
}

// Event is one event of a command record with its round-contiguous event
// sequence, starting at 1.
type Event struct {
	Sequence uint64
	// Payload is the codec's event, an OCJ object.
	Payload []byte
}

// Result is a command's terminal result. Reason is set only when Rejected.
type Result struct {
	Rejected bool
	Reason   string
}

// Command is the record of one executed admitted command (A-7, A-8).
type Command struct {
	RoundID                [16]byte
	Sequence               uint64
	PrevHash               Hash
	CommandID              [16]byte
	EffectiveTimeUnixMicro int64
	EngineBuild            string
	// Payload is the codec's command, an OCJ object.
	Payload []byte
	Result  Result
	Events  []Event
}

// Input returns the deterministic input Decide saw for this command.
func (c Command) Input() game.Input {
	return game.Input{Sequence: c.Sequence, EffectiveTimeUnixMicro: c.EffectiveTimeUnixMicro}
}

// EncodeGenesis returns the canonical bytes of g. It refuses to write a
// record Decode would reject.
func EncodeGenesis(g Genesis) ([]byte, error) {
	switch {
	case !validGameID(string(g.Game)):
		return nil, errors.New("chain: genesis: invalid game id")
	case g.RulesVersion == 0 || g.SchemaVersion == 0 || g.StateVersion == 0:
		return nil, errors.New("chain: genesis: zero version")
	case !validEngineBuild(g.EngineBuild):
		return nil, errors.New("chain: genesis: invalid engine build")
	}
	config, err := payload("genesis config", g.Config)
	if err != nil {
		return nil, err
	}
	return marshal(canonjson.Object{
		"chain":              canonjson.String(Version),
		"config":             config,
		"createdAtUnixMicro": canonjson.Int(g.CreatedAtUnixMicro),
		"engineBuild":        canonjson.String(g.EngineBuild),
		"game":               canonjson.String(g.Game),
		"kind":               canonjson.String(KindGenesis),
		"prevHash":           canonjson.Hex(g.PrevHash[:]),
		"roundId":            canonjson.UUID(g.RoundID),
		"rulesVersion":       canonjson.Uint(uint64(g.RulesVersion)),
		"schemaVersion":      canonjson.Uint(uint64(g.SchemaVersion)),
		"sequence":           canonjson.Uint(g.Sequence),
		"stateVersion":       canonjson.Uint(uint64(g.StateVersion)),
	})
}

// EncodeCommand returns the canonical bytes of c. It refuses to write a
// record Decode would reject.
func EncodeCommand(c Command) ([]byte, error) {
	if !validEngineBuild(c.EngineBuild) {
		return nil, errors.New("chain: command: invalid engine build")
	}
	result := canonjson.Object{"status": canonjson.String(statusAccepted)}
	if c.Result.Rejected {
		if !validReason(c.Result.Reason) {
			return nil, errors.New("chain: command: invalid rejection reason")
		}
		result = canonjson.Object{
			"reason": canonjson.String(c.Result.Reason),
			"status": canonjson.String(statusRejected),
		}
	} else if c.Result.Reason != "" {
		return nil, errors.New("chain: command: accepted result with a reason")
	}
	command, err := payload("command", c.Payload)
	if err != nil {
		return nil, err
	}
	events := make(canonjson.Array, len(c.Events))
	for i, e := range c.Events {
		if e.Sequence == 0 {
			return nil, fmt.Errorf("chain: command: event %d: zero event sequence", i)
		}
		ev, err := payload("event", e.Payload)
		if err != nil {
			return nil, err
		}
		events[i] = canonjson.Object{"event": ev, "eventSequence": canonjson.Uint(e.Sequence)}
	}
	return marshal(canonjson.Object{
		"command":                command,
		"commandId":              canonjson.UUID(c.CommandID),
		"effectiveTimeUnixMicro": canonjson.Int(c.EffectiveTimeUnixMicro),
		"engineBuild":            canonjson.String(c.EngineBuild),
		"events":                 events,
		"kind":                   canonjson.String(KindCommand),
		"prevHash":               canonjson.Hex(c.PrevHash[:]),
		"result":                 result,
		"roundId":                canonjson.UUID(c.RoundID),
		"sequence":               canonjson.Uint(c.Sequence),
	})
}

// Decode parses one record line strictly and returns a Genesis or a
// Command. It checks envelope syntax only (spec section 5.4): payloads must
// be OCJ objects, and the zero genesis prevHash, sequence order, links and
// event numbering are the chain stage's to check. A "chain" key holding
// anything but Version fails with an error wrapping ErrUnsupportedChain.
func Decode(line []byte) (any, error) {
	o, err := canonjson.Unmarshal(line)
	if err != nil {
		return nil, fmt.Errorf("chain: record: %w", err)
	}
	if v, ok := o["chain"]; ok && v != canonjson.String(Version) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedChain, describe(v))
	}
	kind, err := canonjson.Text(o["kind"])
	if err != nil {
		return nil, fmt.Errorf("chain: record kind: %w", err)
	}
	switch kind {
	case KindGenesis:
		return decodeGenesis(o)
	case KindCommand:
		return decodeCommand(o)
	default:
		return nil, errors.New("chain: record kind: neither genesis nor command")
	}
}

func decodeGenesis(o canonjson.Object) (Genesis, error) {
	if _, err := canonjson.Fields(o, "chain", "config", "createdAtUnixMicro", "engineBuild", "game", "kind",
		"prevHash", "roundId", "rulesVersion", "schemaVersion", "sequence", "stateVersion"); err != nil {
		return Genesis{}, fmt.Errorf("chain: genesis: %w", err)
	}
	d := fieldReader{what: "genesis", o: o}
	var g Genesis
	gameID := d.text("game")
	if d.err == nil && !validGameID(gameID) {
		d.fail("game", errors.New("does not match ^[a-z][a-z0-9-]{0,31}$"))
	}
	g.Game = game.ID(gameID)
	g.RulesVersion = game.RulesVersion(d.version("rulesVersion"))
	g.SchemaVersion = game.SchemaVersion(d.version("schemaVersion"))
	g.StateVersion = game.StateVersion(d.version("stateVersion"))
	g.RoundID = d.uuid("roundId")
	g.Sequence = d.uint("sequence")
	g.PrevHash = d.hash("prevHash")
	g.CreatedAtUnixMicro = d.int("createdAtUnixMicro")
	g.EngineBuild = d.engineBuild("engineBuild")
	g.Config = d.payload("config")
	if d.err != nil {
		return Genesis{}, d.err
	}
	return g, nil
}

func decodeCommand(o canonjson.Object) (Command, error) {
	if _, err := canonjson.Fields(o, "command", "commandId", "effectiveTimeUnixMicro", "engineBuild", "events",
		"kind", "prevHash", "result", "roundId", "sequence"); err != nil {
		return Command{}, fmt.Errorf("chain: command: %w", err)
	}
	d := fieldReader{what: "command", o: o}
	c := Command{
		RoundID:                d.uuid("roundId"),
		Sequence:               d.uint("sequence"),
		PrevHash:               d.hash("prevHash"),
		CommandID:              d.uuid("commandId"),
		EffectiveTimeUnixMicro: d.int("effectiveTimeUnixMicro"),
		EngineBuild:            d.engineBuild("engineBuild"),
		Payload:                d.payload("command"),
	}
	c.Result = d.result("result")
	c.Events = d.events("events")
	if d.err != nil {
		return Command{}, d.err
	}
	return c, nil
}

// LinkHash returns SHA-256(RecordTag || record), the link hash of a record's
// exact bytes (A-9). The next record's prevHash and the history head are
// link hashes.
func LinkHash(record []byte) Hash {
	h := sha256.New()
	// hash.Hash.Write never returns an error.
	_, _ = h.Write([]byte(RecordTag))
	_, _ = h.Write(record)
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// StateHash returns SHA-256(StateTag || OCJ({"game","state","stateVersion"}))
// for a codec's canonical state bytes (A-10). It fails if state is not an
// OCJ object.
func StateHash(g game.ID, v game.StateVersion, state []byte) (Hash, error) {
	s, err := canonjson.Unmarshal(state)
	if err != nil {
		return Hash{}, fmt.Errorf("chain: state: %w", err)
	}
	doc, err := canonjson.Marshal(canonjson.Object{
		"game":         canonjson.String(g),
		"state":        s,
		"stateVersion": canonjson.Uint(uint64(v)),
	})
	if err != nil {
		return Hash{}, fmt.Errorf("chain: state hash document: %w", err)
	}
	h := sha256.New()
	_, _ = h.Write([]byte(StateTag))
	_, _ = h.Write(doc)
	var out Hash
	copy(out[:], h.Sum(nil))
	return out, nil
}

func marshal(o canonjson.Object) ([]byte, error) {
	b, err := canonjson.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("chain: encode record: %w", err)
	}
	return b, nil
}

// payload parses a codec payload, which must be an OCJ object.
func payload(what string, b []byte) (canonjson.Object, error) {
	o, err := canonjson.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("chain: %s payload: %w", what, err)
	}
	return o, nil
}

// describe names a value without quoting attacker-controlled text at length.
func describe(v canonjson.Value) string {
	if s, ok := v.(canonjson.String); ok && len(s) <= 64 {
		return fmt.Sprintf("%q", string(s))
	}
	return "a non-string or long value"
}

// validGameID reports whether s matches ^[a-z][a-z0-9-]{0,31}$.
func validGameID(s string) bool {
	if len(s) < 1 || len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// validEngineBuild reports whether s is 1 to 128 printable ASCII bytes
// (0x21 to 0x7E).
func validEngineBuild(s string) bool {
	if len(s) < 1 || len(s) > maxEngineBuild {
		return false
	}
	for i := range len(s) {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// validReason reports whether s matches ^[a-z][a-z0-9_]*$.
func validReason(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// fieldReader reads typed envelope fields from an object whose key set was
// already checked. It keeps the first error; later reads return zero values.
type fieldReader struct {
	what string
	o    canonjson.Object
	err  error
}

func (d *fieldReader) fail(key string, err error) {
	if d.err == nil {
		d.err = fmt.Errorf("chain: %s: %s: %w", d.what, key, err)
	}
}

func (d *fieldReader) text(key string) string {
	s, err := canonjson.Text(d.o[key])
	if err != nil {
		d.fail(key, err)
	}
	return s
}

func (d *fieldReader) int(key string) int64 {
	n, err := canonjson.ParseInt(d.o[key])
	if err != nil {
		d.fail(key, err)
	}
	return n
}

func (d *fieldReader) uint(key string) uint64 {
	n, err := canonjson.ParseUint(d.o[key])
	if err != nil {
		d.fail(key, err)
	}
	return n
}

func (d *fieldReader) version(key string) uint32 {
	n, err := canonjson.ParseVersion(d.o[key])
	if err != nil {
		d.fail(key, err)
	}
	return n
}

func (d *fieldReader) uuid(key string) [16]byte {
	id, err := canonjson.ParseUUID(d.o[key])
	if err != nil {
		d.fail(key, err)
	}
	return id
}

func (d *fieldReader) hash(key string) Hash {
	var h Hash
	b, err := canonjson.ParseHex(d.o[key], len(h))
	if err != nil {
		d.fail(key, err)
		return h
	}
	copy(h[:], b)
	return h
}

func (d *fieldReader) engineBuild(key string) string {
	s := d.text(key)
	if d.err == nil && !validEngineBuild(s) {
		d.fail(key, errors.New("not 1 to 128 printable ASCII bytes"))
	}
	return s
}

// payload returns the canonical bytes of an object-valued field.
func (d *fieldReader) payload(key string) []byte {
	o, err := canonjson.Obj(d.o[key])
	if err != nil {
		d.fail(key, err)
		return nil
	}
	b, err := canonjson.Marshal(o)
	if err != nil {
		d.fail(key, err)
		return nil
	}
	return b
}

func (d *fieldReader) result(key string) Result {
	o, err := canonjson.Obj(d.o[key])
	if err != nil {
		d.fail(key, err)
		return Result{}
	}
	switch o["status"] {
	case canonjson.String(statusAccepted):
		if _, err := canonjson.Fields(o, "status"); err != nil {
			d.fail(key, err)
		}
		return Result{}
	case canonjson.String(statusRejected):
		if _, err := canonjson.Fields(o, "reason", "status"); err != nil {
			d.fail(key, err)
			return Result{}
		}
		reason, err := canonjson.Text(o["reason"])
		if err != nil {
			d.fail(key, err)
			return Result{}
		}
		if !validReason(reason) {
			d.fail(key, errors.New("reason does not match ^[a-z][a-z0-9_]*$"))
			return Result{}
		}
		return Result{Rejected: true, Reason: reason}
	default:
		d.fail(key, errors.New("status is neither accepted nor rejected"))
		return Result{}
	}
}

func (d *fieldReader) events(key string) []Event {
	a, err := canonjson.List(d.o[key])
	if err != nil {
		d.fail(key, err)
		return nil
	}
	events := make([]Event, 0, len(a))
	for i, v := range a {
		o, err := canonjson.Obj(v)
		if err == nil {
			_, err = canonjson.Fields(o, "event", "eventSequence")
		}
		if err != nil {
			d.fail(fmt.Sprintf("%s[%d]", key, i), err)
			return nil
		}
		item := fieldReader{what: fmt.Sprintf("%s: %s[%d]", d.what, key, i), o: o}
		seq := item.uint("eventSequence")
		if item.err == nil && seq == 0 {
			item.fail("eventSequence", errors.New("event sequences start at 1"))
		}
		p := item.payload("event")
		if item.err != nil {
			if d.err == nil {
				d.err = item.err
			}
			return nil
		}
		events = append(events, Event{Sequence: seq, Payload: p})
	}
	return events
}

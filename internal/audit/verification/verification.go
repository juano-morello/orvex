// Package verification reads an exported audit history (formats
// orvex/audit-history/v1 and orvex/audit-history/v2) and verifies it in
// fixed stages: format, chain, rules, decode (with fairness), then decision
// and state replay with checkpoint state hashes (ADR-001, ADR-006;
// decisions A-11, A-13, A-14 and section 6 of
// docs/specs/8bebb70f_canonical-codec-audit-cli.md). For a v2 history,
// ModeVerify adds the ledger stage after state replay: it recomputes every
// ledger posting from the chained events and compares it with the exported
// ledger rows (LBT-18, docs/specs/ac81f88f_audit-ledger-stage.md).
//
// The first failure stops verification and is reported as a *Failure
// naming its stage and the record sequence (or the header). Unknown
// formats, chain versions, games, rules, schema or state versions fail
// closed as unsupported.
package verification

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/replay"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/ledger"
)

// Format is the history file format v1 (header "format").
const Format = "orvex/audit-history/v1"

// FormatV2 is history file format v2: format v1 plus a header account table
// and one ledger line per record (LBT-18 L-1).
const FormatV2 = "orvex/audit-history/v2"

// MaxLineBytes is the longest line, without its terminating '\n' (A-11).
const MaxLineBytes = 64 << 20

// HeaderSequence is Failure.Sequence for a failure located in the header.
const HeaderSequence int64 = -1

// Mode selects the checks Verify runs.
type Mode int

const (
	// ModeChain checks format and chain only; it needs no registry.
	ModeChain Mode = iota + 1
	// ModeReplay adds rules, decode, fairness and state replay with
	// checkpoint state hashes.
	ModeReplay
	// ModeVerify adds decision replay before each record's state replay.
	ModeVerify
)

// Options configures Verify.
type Options struct {
	Mode Mode
	// Registry selects the historical rules; it may be nil only in ModeChain.
	Registry *game.Registry
	// ExpectHead, if set, must equal the history's head: a value held
	// elsewhere, so that a fully rehashed forgery is detected (ADR-001).
	ExpectHead *chain.Hash
	// LedgerRules holds the posting rules ModeVerify uses for v2 histories,
	// at most one per (game, rules version). v1 histories ignore it.
	LedgerRules []ledger.Rule
}

// Stage names a verification stage. The names are stable: they appear in
// crashctl output and tests.
type Stage string

// The stages of spec section 6, in order.
const (
	StageFormat         Stage = "format"
	StageChain          Stage = "chain"
	StageRules          Stage = "rules"
	StageDecode         Stage = "decode"
	StageFairness       Stage = "fairness"
	StageDecisionReplay Stage = "decision-replay"
	StageStateReplay    Stage = "state-replay"
	StageLedger         Stage = "ledger"
)

// Failure is a verification outcome other than success: the history is
// malformed, tampered or does not reproduce, or (Unsupported) it uses an
// artifact this verifier does not know.
type Failure struct {
	Stage Stage
	// Sequence is the record's position, or HeaderSequence.
	Sequence    int64
	Unsupported bool
	Err         error
}

func (f *Failure) Error() string {
	verdict := "failed"
	if f.Unsupported {
		verdict = "unsupported"
	}
	where := "header"
	if f.Sequence != HeaderSequence {
		where = fmt.Sprintf("sequence %d", f.Sequence)
	}
	return fmt.Sprintf("%s %s at %s: %v", f.Stage, verdict, where, f.Err)
}

func (f *Failure) Unwrap() error { return f.Err }

// Checkpoint is a sequence and the state hash after it.
type Checkpoint struct {
	Sequence  uint64
	StateHash chain.Hash
}

// Report describes a history that passed every check of its mode.
type Report struct {
	Format, Chain string
	RoundID       [16]byte
	// Descriptor is the rules version used; zero in ModeChain.
	Descriptor game.Descriptor
	Records    int
	Commands   int
	Accepted   int
	Rejected   int
	Events     int
	Head       chain.Hash
	// Checkpoints are the verified checkpoints; empty in ModeChain.
	Checkpoints []Checkpoint
	// State is the replayed state after the last record in ModeReplay and
	// ModeVerify; nil in ModeChain. Callers must not mutate it.
	State any
	// Ledger is nil unless the ledger stage ran.
	Ledger *LedgerReport
}

// LedgerReport counts the ledger rows of a v2 history that passed the
// ledger stage.
type LedgerReport struct {
	Accounts, Transactions, Postings, Holds, Releases int
}

func fail(stage Stage, seq int64, err error) *Failure {
	return &Failure{Stage: stage, Sequence: seq, Err: err}
}

func unsupported(stage Stage, seq int64, err error) *Failure {
	return &Failure{Stage: stage, Sequence: seq, Unsupported: true, Err: err}
}

// header is the parsed history header.
type header struct {
	format       string
	checkpoints  []Checkpoint
	configSecret []byte
	head         chain.Hash
	accounts     []account // v2 only
}

// history is the parsed file: the header and every record with its line,
// and for v2 every ledger line.
type history struct {
	header  header
	lines   [][]byte
	records []any // chain.Genesis at 0, chain.Command after
	ledger  []ledgerLine
}

// Verify reads one history from r and runs the checks of opt.Mode. It
// returns a *Failure for verification outcomes and any other error for
// I/O or invalid options.
func Verify(r io.Reader, opt Options) (Report, error) {
	switch opt.Mode {
	case ModeChain:
	case ModeReplay, ModeVerify:
		if opt.Registry == nil {
			return Report{}, errors.New("verification: replay and verify need a registry")
		}
	default:
		return Report{}, fmt.Errorf("verification: unknown mode %d", opt.Mode)
	}
	if err := checkLedgerRules(opt.LedgerRules); err != nil {
		return Report{}, err
	}

	h, err := read(r)
	if err != nil {
		return Report{}, err
	}
	rep, err := checkChain(h, opt.ExpectHead)
	if err != nil {
		return Report{}, err
	}
	if opt.Mode == ModeChain {
		return rep, nil
	}
	return replayHistory(h, rep, opt)
}

// ---------------------------------------------------------------- format

type lineStatus int

const (
	lineOK lineStatus = iota
	lineEOF
	lineUnterminated
	lineTooLong
)

// readLine returns the next line without its '\n'. It stops reading as
// soon as a line passes MaxLineBytes.
func readLine(br *bufio.Reader) ([]byte, lineStatus, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			if len(line)+len(frag)-1 > MaxLineBytes {
				return nil, lineTooLong, nil
			}
			line = append(line, frag[:len(frag)-1]...)
			return line, lineOK, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if len(line)+len(frag) > MaxLineBytes {
				return nil, lineTooLong, nil
			}
			line = append(line, frag...)
		case errors.Is(err, io.EOF):
			if len(line)+len(frag) > MaxLineBytes {
				return nil, lineTooLong, nil
			}
			line = append(line, frag...)
			if len(line) == 0 {
				return nil, lineEOF, nil
			}
			return line, lineUnterminated, nil
		default:
			return nil, lineOK, fmt.Errorf("verification: read history: %w", err)
		}
	}
}

// read runs the format stage over the whole input.
func read(r io.Reader) (*history, error) {
	br := bufio.NewReader(r)
	h := &history{}
	inLedger := false // v2: a ledger line has been read
	for n := 0; ; n++ {
		pos := int64(n) - 1 // HeaderSequence for the first line
		if inLedger {
			pos = int64(len(h.ledger)) // a ledger line's position is its j
		}
		line, status, err := readLine(br)
		if err != nil {
			return nil, err
		}
		switch status {
		case lineEOF:
			if n == 0 {
				return nil, fail(StageFormat, HeaderSequence, errors.New("verification: empty input"))
			}
			if n == 1 {
				return nil, fail(StageFormat, 0, errors.New("verification: no genesis record"))
			}
			if len(h.ledger) < len(h.records) && h.header.format == FormatV2 {
				return nil, fail(StageFormat, int64(len(h.ledger)), errors.New("verification: missing ledger line"))
			}
			return h, nil
		case lineTooLong:
			return nil, fail(StageFormat, pos, fmt.Errorf("verification: line exceeds the %d-byte line limit", MaxLineBytes))
		case lineUnterminated:
			return nil, fail(StageFormat, pos, errors.New("verification: line does not end with a newline"))
		}
		if n == 0 {
			hd, ferr := parseHeader(line)
			if ferr != nil {
				return nil, ferr
			}
			h.header = hd
			continue
		}
		if h.header.format == FormatV2 {
			if ferr := h.readV2Line(line, pos, &inLedger); ferr != nil {
				return nil, ferr
			}
			continue
		}
		rec, err := chain.Decode(line)
		if err != nil {
			if pos == 0 && errors.Is(err, chain.ErrUnsupportedChain) {
				return nil, unsupported(StageFormat, 0, err)
			}
			return nil, fail(StageFormat, pos, err)
		}
		h.lines = append(h.lines, line)
		h.records = append(h.records, rec)
	}
}

// readV2Line reads one line after a v2 header (LBT-18 section 6.1 step 3):
// record lines until the first ledger line, then ledger lines only.
func (h *history) readV2Line(line []byte, pos int64, inLedger *bool) *Failure {
	o, err := canonjson.Unmarshal(line)
	if err != nil {
		return fail(StageFormat, pos, err)
	}
	isLedger := o["kind"] == canonjson.String(ledgerKind)
	if !*inLedger && !isLedger {
		rec, err := chain.Decode(line)
		if err != nil {
			if pos == 0 && errors.Is(err, chain.ErrUnsupportedChain) {
				return unsupported(StageFormat, 0, err)
			}
			return fail(StageFormat, pos, err)
		}
		h.lines = append(h.lines, line)
		h.records = append(h.records, rec)
		return nil
	}
	*inLedger = true
	j := len(h.ledger)
	if !isLedger {
		return fail(StageFormat, int64(j), errors.New("verification: record line after the ledger section"))
	}
	ll, seq, err := parseLedgerLine(o)
	if err != nil {
		return fail(StageFormat, int64(j), err)
	}
	if seq != uint64(j) {
		return fail(StageFormat, int64(j), fmt.Errorf("verification: ledger line sequence %d at position %d", seq, j))
	}
	if j == len(h.records) {
		return fail(StageFormat, int64(j), errors.New("verification: ledger line without a record"))
	}
	h.ledger = append(h.ledger, ll)
	return nil
}

func parseHeader(line []byte) (header, *Failure) {
	o, err := canonjson.Unmarshal(line)
	if err != nil {
		return header{}, fail(StageFormat, HeaderSequence, err)
	}
	f, ok := o["format"]
	if !ok {
		return header{}, fail(StageFormat, HeaderSequence, errors.New("verification: header has no format"))
	}
	format, err := canonjson.Text(f)
	if err != nil {
		return header{}, fail(StageFormat, HeaderSequence, fmt.Errorf("verification: header format: %w", err))
	}
	if format != Format && format != FormatV2 {
		return header{}, unsupported(StageFormat, HeaderSequence, errors.New("verification: unknown history format"))
	}
	keys := []string{"checkpoints", "configSecret", "format", "head"}
	if format == FormatV2 {
		keys = append(keys, "accounts")
	}
	if _, err := canonjson.Fields(o, keys...); err != nil {
		return header{}, fail(StageFormat, HeaderSequence, fmt.Errorf("verification: header: %w", err))
	}
	hd := header{format: format}
	head, err := canonjson.ParseHex(o["head"], len(hd.head))
	if err != nil {
		return header{}, fail(StageFormat, HeaderSequence, fmt.Errorf("verification: header head: %w", err))
	}
	copy(hd.head[:], head)
	secret, err := canonjson.Obj(o["configSecret"])
	if err == nil {
		hd.configSecret, err = canonjson.Marshal(secret)
	}
	if err != nil {
		return header{}, fail(StageFormat, HeaderSequence, fmt.Errorf("verification: header configSecret: %w", err))
	}
	cps, err := canonjson.List(o["checkpoints"])
	if err != nil {
		return header{}, fail(StageFormat, HeaderSequence, fmt.Errorf("verification: header checkpoints: %w", err))
	}
	if len(cps) == 0 {
		return header{}, fail(StageFormat, HeaderSequence, errors.New("verification: header checkpoints: empty"))
	}
	for i, v := range cps {
		cp, err := parseCheckpoint(v)
		if err != nil {
			return header{}, fail(StageFormat, HeaderSequence, fmt.Errorf("verification: header checkpoint %d: %w", i, err))
		}
		hd.checkpoints = append(hd.checkpoints, cp)
	}
	if format == FormatV2 {
		if hd.accounts, err = parseAccounts(o["accounts"]); err != nil {
			return header{}, fail(StageFormat, HeaderSequence, fmt.Errorf("verification: header accounts: %w", err))
		}
	}
	return hd, nil
}

func parseCheckpoint(v canonjson.Value) (Checkpoint, error) {
	o, err := canonjson.Obj(v)
	if err != nil {
		return Checkpoint{}, err
	}
	if _, err := canonjson.Fields(o, "sequence", "stateHash"); err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	if cp.Sequence, err = canonjson.ParseUint(o["sequence"]); err != nil {
		return Checkpoint{}, err
	}
	b, err := canonjson.ParseHex(o["stateHash"], len(cp.StateHash))
	if err != nil {
		return Checkpoint{}, err
	}
	copy(cp.StateHash[:], b)
	return cp, nil
}

// ---------------------------------------------------------------- chain

func checkChain(h *history, expect *chain.Hash) (Report, error) {
	rep := Report{Format: h.header.format, Chain: chain.Version, Records: len(h.records)}
	var roundID [16]byte
	seen := map[[16]byte]bool{}
	nextEvent := uint64(1)
	for i, rec := range h.records {
		pos := int64(i)
		var (
			seq      uint64
			isGen    bool
			rid      [16]byte
			prevHash chain.Hash
		)
		switch r := rec.(type) {
		case chain.Genesis:
			seq, isGen, rid, prevHash = r.Sequence, true, r.RoundID, r.PrevHash
		case chain.Command:
			seq, rid, prevHash = r.Sequence, r.RoundID, r.PrevHash
		}
		if seq != uint64(i) {
			return Report{}, fail(StageChain, pos, fmt.Errorf("verification: record sequence %d at position %d", seq, i))
		}
		if isGen != (i == 0) {
			return Report{}, fail(StageChain, pos, errors.New("verification: genesis must be exactly the first record"))
		}
		if i == 0 {
			roundID = rid
		} else if rid != roundID {
			return Report{}, fail(StageChain, pos, errors.New("verification: round id differs from the genesis round id"))
		}
		var want chain.Hash
		if i > 0 {
			want = chain.LinkHash(h.lines[i-1])
		}
		if prevHash != want {
			return Report{}, fail(StageChain, pos, errors.New("verification: prevHash does not link to the previous record"))
		}
		if i == 0 {
			continue
		}
		c := rec.(chain.Command)
		if seen[c.CommandID] {
			return Report{}, fail(StageChain, pos, errors.New("verification: duplicate command id"))
		}
		seen[c.CommandID] = true
		if c.Result.Rejected && len(c.Events) > 0 {
			return Report{}, fail(StageChain, pos, errors.New("verification: rejected command with events"))
		}
		for _, e := range c.Events {
			if e.Sequence != nextEvent {
				return Report{}, fail(StageChain, pos, fmt.Errorf("verification: event sequence %d, want %d", e.Sequence, nextEvent))
			}
			nextEvent++
		}
		rep.Commands++
		if c.Result.Rejected {
			rep.Rejected++
		} else {
			rep.Accepted++
		}
		rep.Events += len(c.Events)
	}

	last := int64(len(h.records) - 1)
	rep.RoundID = roundID
	rep.Head = chain.LinkHash(h.lines[last])
	if rep.Head != h.header.head {
		return Report{}, fail(StageChain, last, errors.New("verification: head does not match the header head"))
	}
	if expect != nil && rep.Head != *expect {
		return Report{}, fail(StageChain, last, errors.New("verification: head does not match the expected head"))
	}
	cps := h.header.checkpoints
	for i, cp := range cps {
		if cp.Sequence > uint64(last) || (i > 0 && cp.Sequence <= cps[i-1].Sequence) {
			return Report{}, fail(StageChain, HeaderSequence, errors.New("verification: checkpoint sequences must strictly increase within the chain"))
		}
	}
	if cps[len(cps)-1].Sequence != uint64(last) {
		return Report{}, fail(StageChain, HeaderSequence, errors.New("verification: the last checkpoint must be at the last record"))
	}
	return rep, nil
}

// ---------------------------------------------------------------- rules, decode, replay

func replayHistory(h *history, rep Report, opt Options) (Report, error) {
	g := h.records[0].(chain.Genesis)
	a, err := opt.Registry.Lookup(g.Game, g.RulesVersion)
	if err != nil {
		return Report{}, unsupported(StageRules, 0, err)
	}
	d := a.Descriptor()
	if d.SchemaVersion != g.SchemaVersion || d.StateVersion != g.StateVersion {
		return Report{}, unsupported(StageRules, 0, fmt.Errorf(
			"verification: %s %d supports schema %d state %d; history has schema %d state %d",
			d.Game, d.RulesVersion, d.SchemaVersion, d.StateVersion, g.SchemaVersion, g.StateVersion))
	}
	rep.Descriptor = d
	withLedger := opt.Mode == ModeVerify && h.header.format == FormatV2
	var rule ledger.Rule
	if withLedger {
		if rule, err = selectLedgerRule(opt.LedgerRules, d); err != nil {
			return Report{}, unsupported(StageRules, 0, err)
		}
	}

	cfg, err := a.DecodeConfig(g.RoundID, g.Config, h.header.configSecret)
	if err != nil {
		if errors.Is(err, game.ErrRevealMismatch) {
			return Report{}, fail(StageFairness, 0, err)
		}
		return Report{}, fail(StageDecode, 0, err)
	}
	records := make([]replay.Record, 0, len(h.records)-1)
	for i, rec := range h.records[1:] {
		pos := int64(i + 1)
		c := rec.(chain.Command)
		cmd, err := a.DecodeCommand(c.Payload)
		if err != nil {
			return Report{}, fail(StageDecode, pos, fmt.Errorf("verification: command: %w", err))
		}
		r := replay.Record{Sequence: c.Sequence, Input: c.Input(), Command: cmd, Result: c.Result}
		for j, e := range c.Events {
			ev, err := a.DecodeEvent(e.Payload)
			if err != nil {
				return Report{}, fail(StageDecode, pos, fmt.Errorf("verification: event %d: %w", j, err))
			}
			r.Events = append(r.Events, ev)
			r.EventBytes = append(r.EventBytes, e.Payload)
		}
		records = append(records, r)
	}

	initial, err := a.InitialState(cfg)
	if err != nil {
		return Report{}, fail(StageStateReplay, 0, fmt.Errorf("verification: initial state: %w", err))
	}
	cps := h.header.checkpoints
	next := 0
	visit := func(seq uint64, s any) error {
		rep.State = s
		if next >= len(cps) || cps[next].Sequence != seq {
			return nil
		}
		b, err := a.EncodeState(s)
		if err != nil {
			return fail(StageStateReplay, int64(seq), fmt.Errorf("verification: encode state: %w", err))
		}
		got, err := chain.StateHash(d.Game, d.StateVersion, b)
		if err != nil {
			return fail(StageStateReplay, int64(seq), err)
		}
		if got != cps[next].StateHash {
			return fail(StageStateReplay, int64(seq), errors.New("verification: state hash differs from the checkpoint"))
		}
		rep.Checkpoints = append(rep.Checkpoints, Checkpoint{Sequence: seq, StateHash: got})
		next++
		return nil
	}
	mode := replay.StateOnly
	if opt.Mode == ModeVerify {
		mode = replay.StateAndDecision
	}
	if err := replay.Run(a, initial, records, mode, visit); err != nil {
		var m *replay.Mismatch
		if errors.As(err, &m) {
			stage := StageStateReplay
			if m.Decision {
				stage = StageDecisionReplay
			}
			return Report{}, fail(stage, int64(m.Sequence), m.Err)
		}
		return Report{}, err
	}
	if next != len(cps) {
		// Unreachable: the chain stage checked every checkpoint sequence.
		return Report{}, fail(StageStateReplay, HeaderSequence, errors.New("verification: checkpoint not reached"))
	}
	if withLedger {
		lr, err := checkLedger(h, records, rule)
		if err != nil {
			return Report{}, err
		}
		rep.Ledger = lr
	}
	return rep, nil
}

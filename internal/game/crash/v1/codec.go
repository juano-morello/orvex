package v1

import (
	"fmt"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
)

// Codec implements game.Codec[State, Command, Event, Config]: the canonical
// OCJ encoding of crash/v1 commands, events, config and state (ADR-001,
// ADR-003; decision A-4 and the key tables in section 5.3 of
// docs/specs/8bebb70f_canonical-codec-audit-cli.md).
//
// Each object's keys are its Go field names with the first letter
// lowercased and "ID" written "Id"; commands and events add "type". Every
// integer is D-12 base-10 text in a string, byte arrays are lowercase hex,
// the round ID is D-11 UUID text, and every field is always present (A-3).
// Decoding is strict: unknown or missing keys, unknown types or enum
// strings and non-canonical scalars wrap game.ErrMalformed. Encoding fails
// with game.ErrUnencodable only for values with no canonical form. No error
// text formats a seed.
//
// The codec is frozen with crash/v1 (A-20): a change to any byte it writes
// needs a new schema or state version.
type Codec struct{}

// Command and event type names.
const (
	typePlaceBet     = "PlaceBet"
	typeStartRound   = "StartRound"
	typeCashOut      = "CashOut"
	typeCloseRound   = "CloseRound"
	typeSettleRound  = "SettleRound"
	typeBetPlaced    = "BetPlaced"
	typeRoundStarted = "RoundStarted"
	typeBetCashedOut = "BetCashedOut"
	typeRoundCrashed = "RoundCrashed"
	typeBetLost      = "BetLost"
	typeRoundSettled = "RoundSettled"
)

// EncodeConfig returns the public config, which carries the seed
// commitment, and the secret config, which carries the seed. The round ID
// is in neither.
func (Codec) EncodeConfig(cfg Config) (public, secret []byte, err error) {
	public, err = marshal("config", canonjson.Object{
		"commitment":    hex32(Commitment(cfg.Seed)),
		"maxBetCredits": canonjson.Int(cfg.MaxBetCredits),
		"maxBets":       canonjson.Int(cfg.MaxBets),
		"minBetCredits": canonjson.Int(cfg.MinBetCredits),
	})
	if err != nil {
		return nil, nil, err
	}
	secret, err = marshal("config secret", canonjson.Object{"seed": hex32(cfg.Seed)})
	if err != nil {
		return nil, nil, err
	}
	return public, secret, nil
}

// DecodeConfig rebuilds the config of round roundID. Every syntax check
// runs before the commitment check, which fails with game.ErrRevealMismatch.
// Ranges are InitialState's to check.
func (Codec) DecodeConfig(roundID [16]byte, public, secret []byte) (Config, error) {
	pub, err := parseObject("config", public, "commitment", "maxBetCredits", "maxBets", "minBetCredits")
	if err != nil {
		return Config{}, err
	}
	sec, err := parseObject("config secret", secret, "seed")
	if err != nil {
		return Config{}, err
	}
	d := decoder{what: "config", o: pub}
	commitment := d.hex32("commitment")
	cfg := Config{
		RoundID:       roundID,
		MinBetCredits: d.int("minBetCredits"),
		MaxBetCredits: d.int("maxBetCredits"),
		MaxBets:       d.int("maxBets"),
	}
	if d.err != nil {
		return Config{}, d.err
	}
	s := decoder{what: "config secret", o: sec}
	cfg.Seed = s.hex32("seed")
	if s.err != nil {
		return Config{}, s.err
	}
	if !VerifyCommitment(cfg.Seed, commitment) {
		return Config{}, fmt.Errorf("%w: crash/v1 seed does not match the commitment", game.ErrRevealMismatch)
	}
	return cfg, nil
}

// EncodeCommand returns the canonical bytes of a command value.
func (Codec) EncodeCommand(c Command) ([]byte, error) {
	var (
		o   canonjson.Object
		a   Authority
		typ string
	)
	switch c := c.(type) {
	case PlaceBet:
		a, typ = c.Authority, typePlaceBet
		o = canonjson.Object{"player": canonjson.String(c.Player), "betCredits": canonjson.Int(c.BetCredits)}
	case StartRound:
		a, typ = c.Authority, typeStartRound
	case CashOut:
		a, typ = c.Authority, typeCashOut
		o = canonjson.Object{"player": canonjson.String(c.Player)}
	case CloseRound:
		a, typ = c.Authority, typeCloseRound
	case SettleRound:
		a, typ = c.Authority, typeSettleRound
	default:
		return nil, fmt.Errorf("%w: crash/v1 command of type %T", game.ErrUnencodable, c)
	}
	auth, ok := authorityText(a)
	if !ok {
		return nil, fmt.Errorf("%w: crash/v1 %s: authority %d", game.ErrUnencodable, typ, a)
	}
	if o == nil {
		o = canonjson.Object{}
	}
	o["authority"] = canonjson.String(auth)
	o["type"] = canonjson.String(typ)
	return marshal(typ, o)
}

// DecodeCommand parses the canonical bytes of a command.
func (Codec) DecodeCommand(b []byte) (Command, error) {
	o, typ, err := parseTyped("command", b)
	if err != nil {
		return nil, err
	}
	var keys []string
	switch typ {
	case typePlaceBet:
		keys = []string{"authority", "betCredits", "player", "type"}
	case typeCashOut:
		keys = []string{"authority", "player", "type"}
	case typeStartRound, typeCloseRound, typeSettleRound:
		keys = []string{"authority", "type"}
	default:
		return nil, fmt.Errorf("%w: crash/v1 command: unknown type", game.ErrMalformed)
	}
	if err := fields(typ, o, keys...); err != nil {
		return nil, err
	}
	d := decoder{what: typ, o: o}
	a := d.authority("authority")
	var c Command
	switch typ {
	case typePlaceBet:
		c = PlaceBet{Authority: a, Player: d.player("player"), BetCredits: d.int("betCredits")}
	case typeCashOut:
		c = CashOut{Authority: a, Player: d.player("player")}
	case typeStartRound:
		c = StartRound{Authority: a}
	case typeCloseRound:
		c = CloseRound{Authority: a}
	default:
		c = SettleRound{Authority: a}
	}
	if d.err != nil {
		return nil, d.err
	}
	return c, nil
}

// EncodeEvent returns the canonical bytes of an event value.
func (Codec) EncodeEvent(e Event) ([]byte, error) {
	switch e := e.(type) {
	case BetPlaced:
		return marshal(typeBetPlaced, canonjson.Object{
			"betCredits":        canonjson.Int(e.BetCredits),
			"placedAtUnixMicro": canonjson.Int(e.PlacedAtUnixMicro),
			"player":            canonjson.String(e.Player),
			"type":              canonjson.String(typeBetPlaced),
		})
	case RoundStarted:
		return marshal(typeRoundStarted, canonjson.Object{
			"startedAtUnixMicro": canonjson.Int(e.StartedAtUnixMicro),
			"type":               canonjson.String(typeRoundStarted),
		})
	case BetCashedOut:
		return marshal(typeBetCashedOut, canonjson.Object{
			"cashOutMultiplierScaled": canonjson.Int(e.CashOutMultiplierScaled),
			"cashOutTick":             canonjson.Int(e.CashOutTick),
			"cashedOutAtUnixMicro":    canonjson.Int(e.CashedOutAtUnixMicro),
			"payoutCredits":           canonjson.Int(e.PayoutCredits),
			"player":                  canonjson.String(e.Player),
			"type":                    canonjson.String(typeBetCashedOut),
		})
	case RoundCrashed:
		return marshal(typeRoundCrashed, canonjson.Object{
			"closedAtUnixMicro":         canonjson.Int(e.ClosedAtUnixMicro),
			"crashDeadlineOffsetMicros": canonjson.Int(e.CrashDeadlineOffsetMicros),
			"crashDeadlineUnixMicro":    canonjson.Int(e.CrashDeadlineUnixMicro),
			"crashPointScaled":          canonjson.Int(e.CrashPointScaled),
			"crashTick":                 canonjson.Int(e.CrashTick),
			"type":                      canonjson.String(typeRoundCrashed),
		})
	case BetLost:
		return marshal(typeBetLost, canonjson.Object{
			"betCredits": canonjson.Int(e.BetCredits),
			"player":     canonjson.String(e.Player),
			"type":       canonjson.String(typeBetLost),
		})
	case RoundSettled:
		return marshal(typeRoundSettled, canonjson.Object{
			"betCount":           canonjson.Int(e.BetCount),
			"seed":               hex32(e.Seed),
			"settledAtUnixMicro": canonjson.Int(e.SettledAtUnixMicro),
			"totalBetCredits":    canonjson.Int(e.TotalBetCredits),
			"totalPayoutCredits": canonjson.Int(e.TotalPayoutCredits),
			"type":               canonjson.String(typeRoundSettled),
		})
	default:
		return nil, fmt.Errorf("%w: crash/v1 event of type %T", game.ErrUnencodable, e)
	}
}

// DecodeEvent parses the canonical bytes of an event.
func (Codec) DecodeEvent(b []byte) (Event, error) {
	o, typ, err := parseTyped("event", b)
	if err != nil {
		return nil, err
	}
	var keys []string
	switch typ {
	case typeBetPlaced:
		keys = []string{"betCredits", "placedAtUnixMicro", "player", "type"}
	case typeRoundStarted:
		keys = []string{"startedAtUnixMicro", "type"}
	case typeBetCashedOut:
		keys = []string{"cashOutMultiplierScaled", "cashOutTick", "cashedOutAtUnixMicro", "payoutCredits", "player", "type"}
	case typeRoundCrashed:
		keys = []string{"closedAtUnixMicro", "crashDeadlineOffsetMicros", "crashDeadlineUnixMicro", "crashPointScaled", "crashTick", "type"}
	case typeBetLost:
		keys = []string{"betCredits", "player", "type"}
	case typeRoundSettled:
		keys = []string{"betCount", "seed", "settledAtUnixMicro", "totalBetCredits", "totalPayoutCredits", "type"}
	default:
		return nil, fmt.Errorf("%w: crash/v1 event: unknown type", game.ErrMalformed)
	}
	if err := fields(typ, o, keys...); err != nil {
		return nil, err
	}
	d := decoder{what: typ, o: o}
	var e Event
	switch typ {
	case typeBetPlaced:
		e = BetPlaced{
			Player:            d.player("player"),
			BetCredits:        d.int("betCredits"),
			PlacedAtUnixMicro: d.int("placedAtUnixMicro"),
		}
	case typeRoundStarted:
		e = RoundStarted{StartedAtUnixMicro: d.int("startedAtUnixMicro")}
	case typeBetCashedOut:
		e = BetCashedOut{
			Player:                  d.player("player"),
			CashedOutAtUnixMicro:    d.int("cashedOutAtUnixMicro"),
			CashOutTick:             d.int("cashOutTick"),
			CashOutMultiplierScaled: d.int("cashOutMultiplierScaled"),
			PayoutCredits:           d.int("payoutCredits"),
		}
	case typeRoundCrashed:
		e = RoundCrashed{
			ClosedAtUnixMicro:         d.int("closedAtUnixMicro"),
			CrashPointScaled:          d.int("crashPointScaled"),
			CrashTick:                 d.int("crashTick"),
			CrashDeadlineOffsetMicros: d.int("crashDeadlineOffsetMicros"),
			CrashDeadlineUnixMicro:    d.int("crashDeadlineUnixMicro"),
		}
	case typeBetLost:
		e = BetLost{Player: d.player("player"), BetCredits: d.int("betCredits")}
	default:
		e = RoundSettled{
			SettledAtUnixMicro: d.int("settledAtUnixMicro"),
			BetCount:           d.int("betCount"),
			TotalBetCredits:    d.int("totalBetCredits"),
			TotalPayoutCredits: d.int("totalPayoutCredits"),
			Seed:               d.hex32("seed"),
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	return e, nil
}

// EncodeState returns the canonical bytes of the full authoritative state,
// secrets included: it is what the state hash covers (A-10).
func (Codec) EncodeState(s State) ([]byte, error) {
	phase, ok := phaseText(s.Phase)
	if !ok {
		return nil, fmt.Errorf("%w: crash/v1 state: phase %d", game.ErrUnencodable, s.Phase)
	}
	bets := make(canonjson.Array, len(s.Bets))
	for i, b := range s.Bets {
		status, ok := statusText(b.Status)
		if !ok {
			return nil, fmt.Errorf("%w: crash/v1 state: bet %d: status %d", game.ErrUnencodable, i, b.Status)
		}
		bets[i] = canonjson.Object{
			"betCredits":              canonjson.Int(b.BetCredits),
			"cashOutMultiplierScaled": canonjson.Int(b.CashOutMultiplierScaled),
			"cashOutTick":             canonjson.Int(b.CashOutTick),
			"cashedOutAtUnixMicro":    canonjson.Int(b.CashedOutAtUnixMicro),
			"payoutCredits":           canonjson.Int(b.PayoutCredits),
			"placedAtUnixMicro":       canonjson.Int(b.PlacedAtUnixMicro),
			"player":                  canonjson.String(b.Player),
			"status":                  canonjson.String(status),
		}
	}
	return marshal("state", canonjson.Object{
		"bets":                      bets,
		"closedAtUnixMicro":         canonjson.Int(s.ClosedAtUnixMicro),
		"commitment":                hex32(s.Commitment),
		"crashDeadlineOffsetMicros": canonjson.Int(s.CrashDeadlineOffsetMicros),
		"crashDeadlineUnixMicro":    canonjson.Int(s.CrashDeadlineUnixMicro),
		"crashPointScaled":          canonjson.Int(s.CrashPointScaled),
		"crashTick":                 canonjson.Int(s.CrashTick),
		"maxBetCredits":             canonjson.Int(s.MaxBetCredits),
		"maxBets":                   canonjson.Int(s.MaxBets),
		"minBetCredits":             canonjson.Int(s.MinBetCredits),
		"phase":                     canonjson.String(phase),
		"roundId":                   canonjson.UUID(s.RoundID),
		"seed":                      hex32(s.Seed),
		"settledAtUnixMicro":        canonjson.Int(s.SettledAtUnixMicro),
		"startedAtUnixMicro":        canonjson.Int(s.StartedAtUnixMicro),
		"totalBetCredits":           canonjson.Int(s.TotalBetCredits),
		"totalPayoutCredits":        canonjson.Int(s.TotalPayoutCredits),
	})
}

// RejectionCode returns the reason code of a *RejectionError returned by
// Decide. Any other error, such as ErrInvalidState or ErrArithmetic, fails
// closed.
func (Codec) RejectionCode(err error) (string, bool) {
	r, ok := RejectionReason(err)
	if !ok {
		return "", false
	}
	return string(r), true
}

func authorityText(a Authority) (string, bool) {
	switch a {
	case AuthorityNone:
		return "none", true
	case AuthorityPlayer:
		return "player", true
	case AuthoritySystem:
		return "system", true
	default:
		return "", false
	}
}

func phaseText(p Phase) (string, bool) {
	switch p {
	case PhaseBetting, PhaseRunning, PhaseCrashed, PhaseSettled:
		return p.String(), true
	default:
		return "", false
	}
}

func statusText(s BetStatus) (string, bool) {
	switch s {
	case StatusOpen:
		return "open", true
	case StatusCashedOut:
		return "cashed_out", true
	case StatusLost:
		return "lost", true
	default:
		return "", false
	}
}

func hex32(b [32]byte) canonjson.String {
	return canonjson.Hex(b[:])
}

// marshal encodes o; a value with no OCJ form (invalid UTF-8) is
// unencodable.
func marshal(what string, o canonjson.Object) ([]byte, error) {
	b, err := canonjson.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("%w: crash/v1 %s: %w", game.ErrUnencodable, what, err)
	}
	return b, nil
}

// parseObject parses b strictly and checks its exact key set.
func parseObject(what string, b []byte, keys ...string) (canonjson.Object, error) {
	o, err := canonjson.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("%w: crash/v1 %s: %w", game.ErrMalformed, what, err)
	}
	if err := fields(what, o, keys...); err != nil {
		return nil, err
	}
	return o, nil
}

// parseTyped parses b strictly and returns its "type" string.
func parseTyped(what string, b []byte) (canonjson.Object, string, error) {
	o, err := canonjson.Unmarshal(b)
	if err != nil {
		return nil, "", fmt.Errorf("%w: crash/v1 %s: %w", game.ErrMalformed, what, err)
	}
	v, ok := o["type"]
	if !ok {
		return nil, "", fmt.Errorf("%w: crash/v1 %s: missing key type", game.ErrMalformed, what)
	}
	typ, err := canonjson.Text(v)
	if err != nil {
		return nil, "", fmt.Errorf("%w: crash/v1 %s: type: %w", game.ErrMalformed, what, err)
	}
	return o, typ, nil
}

func fields(what string, o canonjson.Object, keys ...string) error {
	if _, err := canonjson.Fields(o, keys...); err != nil {
		return fmt.Errorf("%w: crash/v1 %s: %w", game.ErrMalformed, what, err)
	}
	return nil
}

// decoder reads typed fields from an object whose key set was already
// checked. It keeps the first error; later reads return zero values.
type decoder struct {
	what string
	o    canonjson.Object
	err  error
}

func (d *decoder) fail(key string, err error) {
	if d.err == nil {
		d.err = fmt.Errorf("%w: crash/v1 %s: %s: %w", game.ErrMalformed, d.what, key, err)
	}
}

func (d *decoder) failText(key, msg string) {
	if d.err == nil {
		d.err = fmt.Errorf("%w: crash/v1 %s: %s: %s", game.ErrMalformed, d.what, key, msg)
	}
}

func (d *decoder) int(key string) int64 {
	n, err := canonjson.ParseInt(d.o[key])
	if err != nil {
		d.fail(key, err)
		return 0
	}
	return n
}

func (d *decoder) text(key string) string {
	s, err := canonjson.Text(d.o[key])
	if err != nil {
		d.fail(key, err)
		return ""
	}
	return s
}

func (d *decoder) player(key string) PlayerID {
	return PlayerID(d.text(key))
}

// hex32 parses 32 bytes of lowercase hex. Its errors never quote the value,
// which may be a seed.
func (d *decoder) hex32(key string) [32]byte {
	var out [32]byte
	b, err := canonjson.ParseHex(d.o[key], len(out))
	if err != nil {
		d.fail(key, err)
		return out
	}
	copy(out[:], b)
	return out
}

func (d *decoder) authority(key string) Authority {
	switch d.text(key) {
	case "none":
		return AuthorityNone
	case "player":
		return AuthorityPlayer
	case "system":
		return AuthoritySystem
	default:
		d.failText(key, "unknown authority")
		return AuthorityNone
	}
}

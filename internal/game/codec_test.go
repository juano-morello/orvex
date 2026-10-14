package game_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
)

// tallyCodec is the canonical codec of the tally test game. It lives here,
// not in sample_game_test.go, because that file's imports are held to the
// pure-code allowlist of TestNoIOImports, which canonjson is not on.
type tallyCodec struct{}

var _ game.Codec[tallyState, tallyCommand, tallyEvent, tallyConfig] = tallyCodec{}

func (tallyCodec) EncodeConfig(cfg tallyConfig) (public, secret []byte, err error) {
	public, err = marshalTally(canonjson.Object{"maxPoints": canonjson.Int(int64(cfg.MaxPoints))})
	if err != nil {
		return nil, nil, err
	}
	secret, err = marshalTally(canonjson.Object{})
	if err != nil {
		return nil, nil, err
	}
	return public, secret, nil
}

func (tallyCodec) DecodeConfig(_ [16]byte, public, secret []byte) (tallyConfig, error) {
	o, err := parseTally(public, "maxPoints")
	if err != nil {
		return tallyConfig{}, err
	}
	maxPoints, err := tallyInt(o["maxPoints"])
	if err != nil {
		return tallyConfig{}, err
	}
	if string(secret) != "{}" {
		return tallyConfig{}, fmt.Errorf("%w: tally secret must be {}", game.ErrRevealMismatch)
	}
	return tallyConfig{MaxPoints: maxPoints}, nil
}

func (tallyCodec) EncodeCommand(c tallyCommand) ([]byte, error) {
	switch c := c.(type) {
	case score:
		return marshalTally(canonjson.Object{
			"player": canonjson.String(c.Player),
			"points": canonjson.Int(int64(c.Points)),
			"type":   canonjson.String("score"),
		})
	case closeTally:
		return marshalTally(canonjson.Object{"type": canonjson.String("close")})
	default:
		return nil, fmt.Errorf("%w: tally command %T", game.ErrUnencodable, c)
	}
}

func (tallyCodec) DecodeCommand(b []byte) (tallyCommand, error) {
	o, err := parseTally(b)
	if err != nil {
		return nil, err
	}
	switch o["type"] {
	case canonjson.String("score"):
		if _, err := fieldsTally(o, "player", "points", "type"); err != nil {
			return nil, err
		}
		player, err := tallyText(o["player"])
		if err != nil {
			return nil, err
		}
		points, err := tallyInt(o["points"])
		if err != nil {
			return nil, err
		}
		return score{Player: player, Points: points}, nil
	case canonjson.String("close"):
		if _, err := fieldsTally(o, "type"); err != nil {
			return nil, err
		}
		return closeTally{}, nil
	default:
		return nil, fmt.Errorf("%w: unknown tally command type", game.ErrMalformed)
	}
}

func (tallyCodec) EncodeEvent(e tallyEvent) ([]byte, error) {
	switch e := e.(type) {
	case scored:
		return marshalTally(canonjson.Object{
			"atUnixMicro": canonjson.Int(e.AtUnixMicro),
			"player":      canonjson.String(e.Player),
			"points":      canonjson.Int(int64(e.Points)),
			"seq":         canonjson.Uint(e.Seq),
			"type":        canonjson.String("scored"),
		})
	case closed:
		return marshalTally(canonjson.Object{
			"atUnixMicro": canonjson.Int(e.AtUnixMicro),
			"seq":         canonjson.Uint(e.Seq),
			"type":        canonjson.String("closed"),
		})
	default:
		return nil, fmt.Errorf("%w: tally event %T", game.ErrUnencodable, e)
	}
}

func (tallyCodec) DecodeEvent(b []byte) (tallyEvent, error) {
	o, err := parseTally(b)
	if err != nil {
		return nil, err
	}
	switch o["type"] {
	case canonjson.String("scored"):
		if _, err := fieldsTally(o, "atUnixMicro", "player", "points", "seq", "type"); err != nil {
			return nil, err
		}
		var e scored
		if e.Player, err = tallyText(o["player"]); err != nil {
			return nil, err
		}
		if e.Points, err = tallyInt(o["points"]); err != nil {
			return nil, err
		}
		if e.Seq, err = tallyUint(o["seq"]); err != nil {
			return nil, err
		}
		if e.AtUnixMicro, err = tallyInt64(o["atUnixMicro"]); err != nil {
			return nil, err
		}
		return e, nil
	case canonjson.String("closed"):
		if _, err := fieldsTally(o, "atUnixMicro", "seq", "type"); err != nil {
			return nil, err
		}
		var e closed
		if e.Seq, err = tallyUint(o["seq"]); err != nil {
			return nil, err
		}
		if e.AtUnixMicro, err = tallyInt64(o["atUnixMicro"]); err != nil {
			return nil, err
		}
		return e, nil
	default:
		return nil, fmt.Errorf("%w: unknown tally event type", game.ErrMalformed)
	}
}

func (tallyCodec) EncodeState(s tallyState) ([]byte, error) {
	closedText := "false"
	if s.Closed {
		closedText = "true"
	}
	history := make(canonjson.Array, len(s.History))
	for i, seq := range s.History {
		history[i] = canonjson.Uint(seq)
	}
	scores := canonjson.Object{}
	for player, points := range s.Scores {
		scores[player] = canonjson.Int(int64(points))
	}
	return marshalTally(canonjson.Object{
		"closed":    canonjson.String(closedText),
		"history":   history,
		"maxPoints": canonjson.Int(int64(s.MaxPoints)),
		"scores":    scores,
		"total":     canonjson.Int(int64(s.Total)),
	})
}

func (tallyCodec) RejectionCode(err error) (string, bool) {
	switch {
	case errors.Is(err, errClosed):
		return "closed", true
	case errors.Is(err, errInvalidPoints):
		return "invalid_points", true
	case errors.Is(err, errOverLimit):
		return "over_limit", true
	case errors.Is(err, errUnknownCommand):
		return "unknown_command", true
	default:
		return "", false
	}
}

func marshalTally(o canonjson.Object) ([]byte, error) {
	b, err := canonjson.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", game.ErrUnencodable, err)
	}
	return b, nil
}

// parseTally parses b strictly and, if keys are given, checks its key set.
func parseTally(b []byte, keys ...string) (canonjson.Object, error) {
	o, err := canonjson.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", game.ErrMalformed, err)
	}
	if len(keys) > 0 {
		return fieldsTally(o, keys...)
	}
	return o, nil
}

func fieldsTally(o canonjson.Object, keys ...string) (canonjson.Object, error) {
	o, err := canonjson.Fields(o, keys...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", game.ErrMalformed, err)
	}
	return o, nil
}

func tallyText(v canonjson.Value) (string, error) {
	s, err := canonjson.Text(v)
	if err != nil {
		return "", fmt.Errorf("%w: %w", game.ErrMalformed, err)
	}
	return s, nil
}

func tallyInt64(v canonjson.Value) (int64, error) {
	n, err := canonjson.ParseInt(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", game.ErrMalformed, err)
	}
	return n, nil
}

func tallyInt(v canonjson.Value) (int, error) {
	n, err := tallyInt64(v)
	return int(n), err
}

func tallyUint(v canonjson.Value) (uint64, error) {
	n, err := canonjson.ParseUint(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", game.ErrMalformed, err)
	}
	return n, nil
}

// spyCodec counts calls into tallyCodec.
type spyCodec struct {
	tallyCodec
	calls *int
}

func (s spyCodec) EncodeConfig(cfg tallyConfig) ([]byte, []byte, error) {
	*s.calls++
	return s.tallyCodec.EncodeConfig(cfg)
}

func (s spyCodec) EncodeCommand(c tallyCommand) ([]byte, error) {
	*s.calls++
	return s.tallyCodec.EncodeCommand(c)
}

func (s spyCodec) EncodeEvent(e tallyEvent) ([]byte, error) {
	*s.calls++
	return s.tallyCodec.EncodeEvent(e)
}

func (s spyCodec) EncodeState(st tallyState) ([]byte, error) {
	*s.calls++
	return s.tallyCodec.EncodeState(st)
}

func TestAdapterCodecRejectsWrongTypes(t *testing.T) {
	calls := 0
	a := game.Adapt(sampleDescriptor(1), tally{}, game.Codec[tallyState, tallyCommand, tallyEvent, tallyConfig](spyCodec{calls: &calls}))

	for _, v := range []any{nil, struct{}{}} {
		pub, sec, err := a.EncodeConfig(v)
		if !errors.Is(err, game.ErrTypeMismatch) || pub != nil || sec != nil {
			t.Errorf("EncodeConfig(%#v) = %q, %q, %v; want ErrTypeMismatch", v, pub, sec, err)
		}
		for name, enc := range map[string]func(any) ([]byte, error){
			"EncodeCommand": a.EncodeCommand,
			"EncodeEvent":   a.EncodeEvent,
			"EncodeState":   a.EncodeState,
		} {
			if b, err := enc(v); !errors.Is(err, game.ErrTypeMismatch) || b != nil {
				t.Errorf("%s(%#v) = %q, %v; want ErrTypeMismatch", name, v, b, err)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("codec was called %d times for mismatched types; want 0", calls)
	}

	// The spy does count well-typed calls.
	if _, err := a.EncodeState(baseState()); err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d after one well-typed call; want 1", calls)
	}
}

func TestAdapterCodecPassThrough(t *testing.T) {
	typed := tallyCodec{}
	a := game.Adapt(sampleDescriptor(1), tally{}, typed)

	cfg := tallyConfig{MaxPoints: 100}
	wantPub, wantSec, err := typed.EncodeConfig(cfg)
	if err != nil {
		t.Fatalf("typed EncodeConfig: %v", err)
	}
	pub, sec, err := a.EncodeConfig(cfg)
	if err != nil || string(pub) != string(wantPub) || string(sec) != string(wantSec) {
		t.Fatalf("EncodeConfig = %q, %q, %v; want %q, %q", pub, sec, err, wantPub, wantSec)
	}
	if string(pub) != `{"maxPoints":"100"}` || string(sec) != `{}` {
		t.Fatalf("EncodeConfig bytes = %q, %q", pub, sec)
	}
	gotCfg, err := a.DecodeConfig([16]byte{1}, pub, sec)
	if err != nil || !reflect.DeepEqual(gotCfg, cfg) {
		t.Fatalf("DecodeConfig = %#v, %v; want %#v", gotCfg, err, cfg)
	}
	if _, err := a.DecodeConfig([16]byte{1}, pub, []byte(`{"seed":"x"}`)); !errors.Is(err, game.ErrRevealMismatch) {
		t.Fatalf("DecodeConfig with a foreign secret: err = %v; want ErrRevealMismatch", err)
	}

	commands := []tallyCommand{score{Player: "alice", Points: 7}, closeTally{}}
	for _, c := range commands {
		want, err := typed.EncodeCommand(c)
		if err != nil {
			t.Fatalf("typed EncodeCommand(%#v): %v", c, err)
		}
		got, err := a.EncodeCommand(c)
		if err != nil || string(got) != string(want) {
			t.Fatalf("EncodeCommand(%#v) = %q, %v; want %q", c, got, err, want)
		}
		dec, err := a.DecodeCommand(got)
		if err != nil {
			t.Fatalf("DecodeCommand(%q): %v", got, err)
		}
		if reflect.TypeOf(dec) != reflect.TypeOf(c) || !reflect.DeepEqual(dec, c) {
			t.Fatalf("DecodeCommand(%q) = %#v (%T); want %#v (%T)", got, dec, dec, c, c)
		}
	}

	events := []tallyEvent{scored{Player: "bob", Points: 3, Seq: 4, AtUnixMicro: -5}, closed{Seq: 6, AtUnixMicro: 7}}
	for _, e := range events {
		want, err := typed.EncodeEvent(e)
		if err != nil {
			t.Fatalf("typed EncodeEvent(%#v): %v", e, err)
		}
		got, err := a.EncodeEvent(e)
		if err != nil || string(got) != string(want) {
			t.Fatalf("EncodeEvent(%#v) = %q, %v; want %q", e, got, err, want)
		}
		dec, err := a.DecodeEvent(got)
		if err != nil {
			t.Fatalf("DecodeEvent(%q): %v", got, err)
		}
		if reflect.TypeOf(dec) != reflect.TypeOf(e) || !reflect.DeepEqual(dec, e) {
			t.Fatalf("DecodeEvent(%q) = %#v (%T); want %#v (%T)", got, dec, dec, e, e)
		}
	}

	s := baseState()
	s.Scores["carol"] = 0
	want, err := typed.EncodeState(s)
	if err != nil {
		t.Fatalf("typed EncodeState: %v", err)
	}
	got, err := a.EncodeState(s)
	if err != nil || string(got) != string(want) {
		t.Fatalf("EncodeState = %q, %v; want %q", got, err, want)
	}
	const wantState = `{"closed":"false","history":["1","2"],"maxPoints":"100","scores":{"alice":"10","bob":"5","carol":"0"},"total":"15"}`
	if string(got) != wantState {
		t.Fatalf("EncodeState = %s; want %s", got, wantState)
	}
}

var errStub = errors.New("stub codec error")

// stubCodec returns errStub, alongside non-nil results, from every method.
type stubCodec struct{}

func (stubCodec) EncodeConfig(tallyConfig) ([]byte, []byte, error) {
	return []byte("p"), []byte("s"), errStub
}

func (stubCodec) DecodeConfig([16]byte, []byte, []byte) (tallyConfig, error) {
	return tallyConfig{MaxPoints: 1}, errStub
}

func (stubCodec) EncodeCommand(tallyCommand) ([]byte, error) { return []byte("c"), errStub }
func (stubCodec) DecodeCommand([]byte) (tallyCommand, error) { return closeTally{}, errStub }
func (stubCodec) EncodeEvent(tallyEvent) ([]byte, error)     { return []byte("e"), errStub }
func (stubCodec) DecodeEvent([]byte) (tallyEvent, error)     { return closed{}, errStub }
func (stubCodec) EncodeState(tallyState) ([]byte, error)     { return []byte("s"), errStub }
func (stubCodec) RejectionCode(error) (string, bool)         { return "", false }

func TestAdapterCodecErrorsPassThrough(t *testing.T) {
	a := game.Adapt(sampleDescriptor(1), tally{}, stubCodec{})

	pub, sec, err := a.EncodeConfig(tallyConfig{MaxPoints: 1})
	if err != errStub || pub != nil || sec != nil { //nolint:errorlint // identity is the property under test
		t.Errorf("EncodeConfig = %q, %q, %v; want nil, nil, errStub", pub, sec, err)
	}
	if cfg, err := a.DecodeConfig([16]byte{}, nil, nil); err != errStub || cfg != nil { //nolint:errorlint // identity
		t.Errorf("DecodeConfig = %#v, %v; want nil, errStub", cfg, err)
	}
	if b, err := a.EncodeCommand(score{}); err != errStub || b != nil { //nolint:errorlint // identity
		t.Errorf("EncodeCommand = %q, %v; want nil, errStub", b, err)
	}
	if c, err := a.DecodeCommand(nil); err != errStub || c != nil { //nolint:errorlint // identity
		t.Errorf("DecodeCommand = %#v, %v; want nil, errStub", c, err)
	}
	if b, err := a.EncodeEvent(closed{}); err != errStub || b != nil { //nolint:errorlint // identity
		t.Errorf("EncodeEvent = %q, %v; want nil, errStub", b, err)
	}
	if e, err := a.DecodeEvent(nil); err != errStub || e != nil { //nolint:errorlint // identity
		t.Errorf("DecodeEvent = %#v, %v; want nil, errStub", e, err)
	}
	if b, err := a.EncodeState(baseState()); err != errStub || b != nil { //nolint:errorlint // identity
		t.Errorf("EncodeState = %q, %v; want nil, errStub", b, err)
	}
}

func TestAdapterRejectionCode(t *testing.T) {
	typed := tallyCodec{}
	a := game.Adapt(sampleDescriptor(1), tally{}, typed)
	cases := []struct {
		err      error
		wantCode string
		wantOK   bool
	}{
		{errClosed, "closed", true},
		{errInvalidPoints, "invalid_points", true},
		{errOverLimit, "over_limit", true},
		{errUnknownCommand, "unknown_command", true},
		{fmt.Errorf("x: %w", errOverLimit), "over_limit", true},
		{errors.New("other"), "", false},
	}
	for _, tc := range cases {
		code, ok := a.RejectionCode(tc.err)
		tcode, tok := typed.RejectionCode(tc.err)
		if code != tcode || ok != tok || code != tc.wantCode || ok != tc.wantOK {
			t.Errorf("RejectionCode(%v) = %q, %v; typed %q, %v; want %q, %v", tc.err, code, ok, tcode, tok, tc.wantCode, tc.wantOK)
		}
	}
}

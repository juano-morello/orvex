package v1

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/juano-morello/orvex/internal/game"
)

// codecSeedLines are the spec section 5.6 payload lines and the P3 command.
func codecSeedLines(f *testing.F) [][]byte {
	f.Helper()
	p3, err := hex.DecodeString(vecP3Hex)
	if err != nil {
		f.Fatal(err)
	}
	lines := [][]byte{p3}
	for _, s := range []string{
		vecPlaceBet, vecStartRound, vecCashOut, vecCloseRound, vecSettleRound,
		vecBetPlaced, vecRoundStarted, vecBetCashedOut, vecRoundCrashed, vecBetLost, vecRoundSettled,
		vecConfig, vecConfigSecret, vecBet, vecResult,
	} {
		lines = append(lines, []byte(s))
	}
	return lines
}

// FuzzDecodeCommand: a successful decode re-encodes to exactly the input;
// a failure wraps game.ErrMalformed.
func FuzzDecodeCommand(f *testing.F) {
	for _, b := range codecSeedLines(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := Codec{}.DecodeCommand(b)
		if err != nil {
			if !errors.Is(err, game.ErrMalformed) {
				t.Fatalf("DecodeCommand(%q) = %v; want ErrMalformed", b, err)
			}
			return
		}
		again, err := Codec{}.EncodeCommand(c)
		if err != nil || string(again) != string(b) {
			t.Fatalf("DecodeCommand(%q) = %#v, which re-encodes to %q, %v", b, c, again, err)
		}
	})
}

// FuzzDecodeEvent: a successful decode re-encodes to exactly the input; a
// failure wraps game.ErrMalformed.
func FuzzDecodeEvent(f *testing.F) {
	for _, b := range codecSeedLines(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := Codec{}.DecodeEvent(b)
		if err != nil {
			if !errors.Is(err, game.ErrMalformed) {
				t.Fatalf("DecodeEvent(%q) = %v; want ErrMalformed", b, err)
			}
			return
		}
		again, err := Codec{}.EncodeEvent(e)
		if err != nil || string(again) != string(b) {
			t.Fatalf("DecodeEvent(%q) = %#v, which re-encodes to %q, %v", b, e, again, err)
		}
	})
}

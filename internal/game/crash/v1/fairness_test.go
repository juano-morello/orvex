package v1

import (
	"strings"
	"testing"
)

func TestOutcomeMessageCanonicalText(t *testing.T) {
	var high [16]byte
	for i := range high {
		high[i] = 0xff
	}
	mixed := [16]byte{0x01, 0x90, 0xf3, 0xa8, 0x7b, 0x3c, 0x7d, 0x4e, 0x8f, 0x00, 0xab, 0xcd, 0xef, 0x0a, 0xb0, 0x9f}

	cases := []struct {
		id   [16]byte
		want string
	}{
		{high, "ffffffff-ffff-ffff-ffff-ffffffffffff"},
		{mixed, "0190f3a8-7b3c-7d4e-8f00-abcdef0ab09f"},
	}
	for _, c := range cases {
		msg := string(OutcomeMessage(c.id))
		text, ok := strings.CutPrefix(msg, OutcomeTag)
		if !ok {
			t.Fatalf("message %q lacks tag %q", msg, OutcomeTag)
		}
		if len(text) != 36 {
			t.Fatalf("round ID text %q has length %d, want 36", text, len(text))
		}
		for i := range len(text) {
			ch := text[i]
			switch i {
			case 8, 13, 18, 23:
				if ch != '-' {
					t.Errorf("%q: byte %d = %q, want '-'", text, i, ch)
				}
			default:
				if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
					t.Errorf("%q: byte %d = %q is not lowercase hex", text, i, ch)
				}
			}
		}
		if text != c.want {
			t.Errorf("round ID text = %q, want %q", text, c.want)
		}
	}
}

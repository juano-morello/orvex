package canonjson_test

import (
	"bytes"
	"errors"
	"testing"
	"unicode/utf8"

	"github.com/juano-morello/orvex/internal/canonjson"
)

// FuzzUnmarshal: any input Unmarshal accepts re-encodes to exactly itself
// (A-2).
func FuzzUnmarshal(f *testing.F) {
	for _, v := range marshalVectors {
		f.Add([]byte(v.want))
	}
	for _, s := range []string{`{"a":1}`, `{ }`, `{"b":"","a":""}`, `{"a":"\u0000"}`, `{"a":"\u0041"}`, "{\"a\":\"\xff\"}"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		o, err := canonjson.Unmarshal(b)
		if err != nil {
			if !errors.Is(err, canonjson.ErrNotCanonical) {
				t.Fatalf("Unmarshal error %v does not wrap ErrNotCanonical", err)
			}
			return
		}
		got, err := canonjson.Marshal(o)
		if err != nil {
			t.Fatalf("Marshal(Unmarshal(%q)): %v", b, err)
		}
		if !bytes.Equal(got, b) {
			t.Fatalf("Marshal(Unmarshal(%q)) = %q", b, got)
		}
	})
}

// FuzzStringRoundTrip: valid UTF-8 round-trips through {"k":s} with stable
// bytes; invalid UTF-8 fails Marshal.
func FuzzStringRoundTrip(f *testing.F) {
	for _, s := range []string{"", "a", "\"\\\b\t\n\f\r", "\x00\x1f\x7f", "\u2028", "é😀", "\xff", "\xed\xa0\x80", "zoë \"☃\"\\\u0001😀"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := canonjson.Marshal(canonjson.Object{"k": canonjson.String(s)})
		if !utf8.ValidString(s) {
			if !errors.Is(err, canonjson.ErrInvalid) {
				t.Fatalf("Marshal(invalid UTF-8 %q) = %q, %v; want ErrInvalid", s, b, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("Marshal(%q): %v", s, err)
		}
		o, err := canonjson.Unmarshal(b)
		if err != nil {
			t.Fatalf("Unmarshal(%q): %v", b, err)
		}
		got, err := canonjson.Text(o["k"])
		if err != nil || got != s || len(o) != 1 {
			t.Fatalf("round trip of %q = %v, %v", s, o, err)
		}
		again, err := canonjson.Marshal(o)
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("re-Marshal = %q, %v; want %q", again, err, b)
		}
	})
}

package canonjson_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/canonjson"
)

type (
	S = canonjson.String
	A = canonjson.Array
	O = canonjson.Object
)

// nest returns an object nested depth levels deep, counting the outer
// object as 1: {"a":{"a":...{}}}.
func nest(depth int) O {
	o := O{}
	for range depth - 1 {
		o = O{"a": o}
	}
	return o
}

var marshalVectors = []struct {
	name string
	in   O
	want string
}{
	{"empty", O{}, `{}`},
	{"empty array", O{"a": A{}}, `{"a":[]}`},
	{"nil array", O{"a": A(nil)}, `{"a":[]}`},
	{"nil object", O{"a": O(nil)}, `{"a":{}}`},
	{"nested", O{"a": A{S("x"), O{"b": A{A{}}}, S("")}, "b": O{"c": S("d")}}, `{"a":["x",{"b":[[]]},""],"b":{"c":"d"}}`},
	{"key order", O{"b": S("3"), "aa": S("2"), "aB": S("1")}, `{"aB":"1","aa":"2","b":"3"}`},
	{"short escapes", O{"k": S("\"\\\b\t\n\f\r")}, `{"k":"\"\\\b\t\n\f\r"}`},
	{"u0000", O{"k": S("\x00")}, `{"k":"\u0000"}`},
	{"u001f", O{"k": S("\x1f")}, `{"k":"\u001f"}`},
	{"u000b", O{"k": S("\x0b")}, `{"k":"\u000b"}`},
	{"literal del", O{"k": S("\x7f")}, "{\"k\":\"\x7f\"}"},
	{"literal u2028", O{"k": S("\u2028\u2029")}, "{\"k\":\"\u2028\u2029\"}"},
	{"literal e-acute", O{"k": S("é")}, `{"k":"é"}`},
	{"literal emoji", O{"k": S("😀")}, `{"k":"😀"}`},
	{"literal slash and html", O{"k": S("/<&>")}, `{"k":"/<&>"}`},
	{"literal fffd", O{"k": S("\ufffd")}, "{\"k\":\"\ufffd\"}"},
	{"depth 32", nest(32), strings.Repeat(`{"a":`, 31) + `{}` + strings.Repeat(`}`, 31)},
}

func TestMarshal(t *testing.T) {
	for _, v := range marshalVectors {
		t.Run(v.name, func(t *testing.T) {
			got, err := canonjson.Marshal(v.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != v.want {
				t.Errorf("Marshal = %s, want %s", got, v.want)
			}
		})
	}
}

func TestMarshalRejects(t *testing.T) {
	cases := []struct {
		name string
		in   O
	}{
		{"invalid utf8 ff", O{"k": S("\xff")}},
		{"overlong", O{"k": S("\xc0\xaf")}},
		{"surrogate", O{"k": S("\xed\xa0\x80")}},
		{"truncated rune", O{"k": S("\xe2\x98")}},
		{"empty key", O{"": S("x")}},
		{"uppercase key", O{"A": S("x")}},
		{"digit key", O{"1a": S("x")}},
		{"hyphen key", O{"a-b": S("x")}},
		{"non-ascii key", O{"é": S("x")}},
		{"nested bad key", O{"a": O{"B": S("x")}}},
		{"nil value", O{"k": nil}},
		{"nil array element", O{"k": A{S("x"), nil}}},
		{"depth 33", nest(33)},
		{"depth 33 via arrays", O{"a": nestArray(32)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := canonjson.Marshal(c.in)
			if !errors.Is(err, canonjson.ErrInvalid) || got != nil {
				t.Errorf("Marshal = %q, %v; want nil, ErrInvalid", got, err)
			}
		})
	}
}

// nestArray returns an array nested n levels: [[...[]]].
func nestArray(n int) A {
	a := A{}
	for range n - 1 {
		a = A{a}
	}
	return a
}

func TestUnmarshalRoundTrip(t *testing.T) {
	for _, v := range marshalVectors {
		t.Run(v.name, func(t *testing.T) {
			got, err := canonjson.Unmarshal([]byte(v.want))
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(normalize(got), normalize(v.in)) {
				t.Errorf("Unmarshal = %#v, want %#v", got, v.in)
			}
			again, err := canonjson.Marshal(got)
			if err != nil || string(again) != v.want {
				t.Errorf("re-Marshal = %s, %v; want %s", again, err, v.want)
			}
		})
	}
}

// normalize replaces nil arrays and objects with empty ones, which encode
// identically.
func normalize(v canonjson.Value) canonjson.Value {
	switch v := v.(type) {
	case A:
		out := A{}
		for _, e := range v {
			out = append(out, normalize(e))
		}
		return out
	case O:
		out := O{}
		for k, e := range v {
			out[k] = normalize(e)
		}
		return out
	default:
		return v
	}
}

func TestUnmarshalRejects(t *testing.T) {
	cases := []struct{ name, in string }{
		{"leading space", ` {}`},
		{"space after brace", `{ "a":"b"}`},
		{"space before colon", `{"a" :"b"}`},
		{"space after colon", `{"a": "b"}`},
		{"space before comma", `{"a":"b" ,"c":"d"}`},
		{"space in array", `{"a":[ ]}`},
		{"trailing newline", "{}\n"},
		{"trailing space", `{} `},
		{"unsorted keys", `{"b":"1","a":"2"}`},
		{"unsorted case", `{"aa":"1","aB":"2"}`},
		{"duplicate key", `{"a":"1","a":"2"}`},
		{"number", `{"a":1}`},
		{"negative zero", `{"a":-0}`},
		{"float", `{"a":1.0}`},
		{"true", `{"a":true}`},
		{"false", `{"a":false}`},
		{"null", `{"a":null}`},
		{"escaped slash", `{"a":"\/"}`},
		{"escaped letter", `{"a":"\u0041"}`},
		{"uppercase hex escape", `{"a":"\u001F"}`},
		{"escaped del", `{"a":"\u007f"}`},
		{"escaped u2028", `{"a":"\u2028"}`},
		{"surrogate pair escape", `{"a":"\ud83d\ude00"}`},
		{"long form of short escape", `{"a":"\u000a"}`},
		{"long form of backspace", `{"a":"\u0008"}`},
		{"unknown escape", `{"a":"\x"}`},
		{"truncated escape", `{"a":"\u00`},
		{"raw u0001", "{\"a\":\"\x01\"}"},
		{"raw newline", "{\"a\":\"\n\"}"},
		{"invalid utf8 ff", "{\"a\":\"\xff\"}"},
		{"overlong", "{\"a\":\"\xc0\xaf\"}"},
		{"surrogate", "{\"a\":\"\xed\xa0\x80\"}"},
		{"invalid utf8 in key", "{\"\xff\":\"x\"}"},
		{"bom", "\xef\xbb\xbf{}"},
		{"empty", ``},
		{"trailing x", `{}x`},
		{"two objects", `{}{}`},
		{"top-level array", `[]`},
		{"top-level string", `"a"`},
		{"unterminated object", `{"a":"b"`},
		{"unterminated string", `{"a":"b`},
		{"missing colon", `{"a""b"}`},
		{"trailing comma", `{"a":"b",}`},
		{"array trailing comma", `{"a":["b",]}`},
		{"bad key", `{"A":"b"}`},
		{"empty key", `{"":"b"}`},
		{"non-string key", `{a:"b"}`},
		{"depth 33", strings.Repeat(`{"a":`, 32) + `{}` + strings.Repeat(`}`, 32)},
		{"depth 33 arrays", `{"a":` + strings.Repeat(`[`, 32) + strings.Repeat(`]`, 32) + `}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := canonjson.Unmarshal([]byte(c.in))
			if !errors.Is(err, canonjson.ErrNotCanonical) || got != nil {
				t.Errorf("Unmarshal(%q) = %#v, %v; want nil, ErrNotCanonical", c.in, got, err)
			}
			if err != nil && !strings.Contains(err.Error(), "offset ") {
				t.Errorf("error %q has no byte offset", err)
			}
		})
	}
}

func TestInts(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
	}{
		{"0", 0}, {"-1", -1}, {"42", 42},
		{"9223372036854775807", 9223372036854775807},
		{"-9223372036854775808", -9223372036854775808},
	} {
		got, err := canonjson.ParseInt(S(c.in))
		if err != nil || got != c.want {
			t.Errorf("ParseInt(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
		if s := canonjson.Int(c.want); string(s) != c.in {
			t.Errorf("Int(%d) = %q, want %q", c.want, s, c.in)
		}
	}
	for _, in := range []string{"-0", "01", "-01", "+1", "1e3", " 1", "1 ", "-", "", "9223372036854775808", "-9223372036854775809", "0x1", "1.0"} {
		if _, err := canonjson.ParseInt(S(in)); !errors.Is(err, canonjson.ErrNotCanonical) {
			t.Errorf("ParseInt(%q) = %v, want ErrNotCanonical", in, err)
		}
	}
	if _, err := canonjson.ParseInt(A{}); !errors.Is(err, canonjson.ErrNotCanonical) {
		t.Errorf("ParseInt(array) = %v, want ErrNotCanonical", err)
	}

	for _, c := range []struct {
		in   string
		want uint64
	}{{"0", 0}, {"1", 1}, {"18446744073709551615", 18446744073709551615}} {
		got, err := canonjson.ParseUint(S(c.in))
		if err != nil || got != c.want {
			t.Errorf("ParseUint(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
		if s := canonjson.Uint(c.want); string(s) != c.in {
			t.Errorf("Uint(%d) = %q, want %q", c.want, s, c.in)
		}
	}
	for _, in := range []string{"-1", "-0", "01", "+1", "", "18446744073709551616", " 1"} {
		if _, err := canonjson.ParseUint(S(in)); !errors.Is(err, canonjson.ErrNotCanonical) {
			t.Errorf("ParseUint(%q) = %v, want ErrNotCanonical", in, err)
		}
	}

	for _, c := range []struct {
		in   string
		want uint32
	}{{"1", 1}, {"2", 2}, {"4294967295", 4294967295}} {
		got, err := canonjson.ParseVersion(S(c.in))
		if err != nil || got != c.want {
			t.Errorf("ParseVersion(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"0", "4294967296", "01", "-1", ""} {
		if _, err := canonjson.ParseVersion(S(in)); !errors.Is(err, canonjson.ErrNotCanonical) {
			t.Errorf("ParseVersion(%q) = %v, want ErrNotCanonical", in, err)
		}
	}
}

func TestHexUUID(t *testing.T) {
	b, err := canonjson.ParseHex(S("00ff7a"), 3)
	if err != nil || string(b) != "\x00\xff\x7a" {
		t.Errorf("ParseHex = %x, %v", b, err)
	}
	if h := canonjson.Hex([]byte{0x00, 0xff, 0x7a}); h != "00ff7a" {
		t.Errorf("Hex = %q", h)
	}
	for _, in := range []string{"00FF7A", "00ff7", "00ff", "00ff7a00", "zzff7a", " 0ff7a", ""} {
		if _, err := canonjson.ParseHex(S(in), 3); !errors.Is(err, canonjson.ErrNotCanonical) {
			t.Errorf("ParseHex(%q) = %v, want ErrNotCanonical", in, err)
		}
	}

	want := [16]byte{0x01, 0x90, 0xf3, 0xa8, 0x7b, 0x3c, 0x7d, 0x4e, 0x8f, 0x00, 0, 0, 0, 0, 0, 0x01}
	const text = "0190f3a8-7b3c-7d4e-8f00-000000000001"
	id, err := canonjson.ParseUUID(S(text))
	if err != nil || id != want {
		t.Errorf("ParseUUID = %x, %v", id, err)
	}
	if s := canonjson.UUID(want); s != text {
		t.Errorf("UUID = %q, want %q", s, text)
	}
	for _, in := range []string{
		"0190F3A8-7B3C-7D4E-8F00-000000000001",
		"{0190f3a8-7b3c-7d4e-8f00-000000000001}",
		"0190f3a87b3c7d4e8f00000000000001",
		"0190f3a8-7b3c-7d4e-8f00-00000000001",
		"0190f3a8-7b3c-7d4e-8f00-0000000000011",
		"0190f3a8-7b3c-7d4e-8f000-00000000001",
		"0190f3a8-7b3c-7d4e-8f00_000000000001",
		"0190f3a8-7b3c-7d4e-8f00-00000000000g",
		"",
	} {
		if _, err := canonjson.ParseUUID(S(in)); !errors.Is(err, canonjson.ErrNotCanonical) {
			t.Errorf("ParseUUID(%q) = %v, want ErrNotCanonical", in, err)
		}
	}
}

func TestAccessors(t *testing.T) {
	if _, err := canonjson.Text(A{}); !errors.Is(err, canonjson.ErrNotCanonical) {
		t.Errorf("Text(array) = %v", err)
	}
	if _, err := canonjson.List(S("x")); !errors.Is(err, canonjson.ErrNotCanonical) {
		t.Errorf("List(string) = %v", err)
	}
	if _, err := canonjson.Obj(nil); !errors.Is(err, canonjson.ErrNotCanonical) {
		t.Errorf("Obj(nil) = %v", err)
	}
	o := O{"a": S("1"), "b": S("2")}
	if got, err := canonjson.Fields(o, "b", "a"); err != nil || len(got) != 2 {
		t.Errorf("Fields exact = %v, %v", got, err)
	}
	_, err := canonjson.Fields(o, "a", "c")
	if !errors.Is(err, canonjson.ErrNotCanonical) || !strings.Contains(err.Error(), "[c]") || !strings.Contains(err.Error(), "[b]") {
		t.Errorf("Fields mismatch = %v, want missing [c] and unknown [b]", err)
	}
}

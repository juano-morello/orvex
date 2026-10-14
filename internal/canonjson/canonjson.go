// Package canonjson implements Orvex Canonical JSON (OCJ), the canonical
// encoding of every audit artifact: commands, events, config, state, chain
// records and history lines (ADR-001; decisions A-1 to A-3 of
// docs/specs/8bebb70f_canonical-codec-audit-cli.md).
//
// OCJ is a subset of RFC 8785 (JSON Canonicalization Scheme): the top-level
// value is an object; values are objects, arrays and strings only; object
// keys match ^[a-z][A-Za-z0-9]*$ and are sorted by byte value; there is no
// whitespace; strings use RFC 8785 serialization (only '"', '\', the five
// short control escapes and lowercase \u00xx for the other C0 controls are
// escaped; everything else, including U+007F, U+2028 and non-ASCII, is
// literal UTF-8). Nesting depth is at most 32, counting the top-level object
// as 1. Integers travel as strings in math spec D-12 form.
//
// Decoding is strict (A-2): Unmarshal accepts b only if b is exactly
// Marshal of the value it returns. The encoder and parser are hand-written
// and use neither encoding/json nor reflection. Package behavior is frozen
// with the first crash/v1 release (A-20).
package canonjson

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxDepth is the deepest nesting OCJ allows; the top-level object is depth 1.
const MaxDepth = 32

var (
	// ErrInvalid reports a value that has no OCJ encoding: a bad key,
	// invalid UTF-8, a nil value or nesting deeper than MaxDepth.
	ErrInvalid = errors.New("canonjson: value has no canonical encoding")

	// ErrNotCanonical reports bytes or a value that are not the canonical
	// OCJ form expected: Unmarshal and every Parse helper wrap it.
	ErrNotCanonical = errors.New("canonjson: not canonical")
)

// Value is an OCJ value: a String, an Array or an Object.
type Value interface{ isValue() }

// String is an OCJ string. It must be valid UTF-8 to encode.
type String string

// Array is an OCJ array. A nil and an empty Array encode the same way.
type Array []Value

// Object is an OCJ object. Keys are emitted in byte order. A nil and an
// empty Object encode the same way.
type Object map[string]Value

func (String) isValue() {}
func (Array) isValue()  {}
func (Object) isValue() {}

// Marshal returns the canonical bytes of o. It fails with an error wrapping
// ErrInvalid if o holds a bad key, invalid UTF-8, a nil value or nesting
// deeper than MaxDepth.
func Marshal(o Object) ([]byte, error) {
	buf, err := appendObject(nil, o, 1)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func appendValue(buf []byte, v Value, depth int) ([]byte, error) {
	switch v := v.(type) {
	case String:
		return appendString(buf, string(v))
	case Array:
		if depth > MaxDepth {
			return nil, fmt.Errorf("%w: nesting deeper than %d", ErrInvalid, MaxDepth)
		}
		buf = append(buf, '[')
		for i, e := range v {
			if i > 0 {
				buf = append(buf, ',')
			}
			var err error
			if buf, err = appendValue(buf, e, depth+1); err != nil {
				return nil, err
			}
		}
		return append(buf, ']'), nil
	case Object:
		return appendObject(buf, v, depth)
	default:
		return nil, fmt.Errorf("%w: nil or foreign value %T", ErrInvalid, v)
	}
}

func appendObject(buf []byte, o Object, depth int) ([]byte, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("%w: nesting deeper than %d", ErrInvalid, MaxDepth)
	}
	buf = append(buf, '{')
	for i, k := range slices.Sorted(maps.Keys(o)) {
		if !validKey(k) {
			return nil, fmt.Errorf("%w: key %q does not match ^[a-z][A-Za-z0-9]*$", ErrInvalid, k)
		}
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '"')
		buf = append(buf, k...)
		buf = append(buf, '"', ':')
		var err error
		if buf, err = appendValue(buf, o[k], depth+1); err != nil {
			return nil, err
		}
	}
	return append(buf, '}'), nil
}

const lowerHex = "0123456789abcdef"

func appendString(buf []byte, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, fmt.Errorf("%w: string is not valid UTF-8", ErrInvalid)
	}
	buf = append(buf, '"')
	for i := range len(s) {
		c := s[i]
		switch c {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\b':
			buf = append(buf, '\\', 'b')
		case '\t':
			buf = append(buf, '\\', 't')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\f':
			buf = append(buf, '\\', 'f')
		case '\r':
			buf = append(buf, '\\', 'r')
		default:
			if c < 0x20 {
				buf = append(buf, '\\', 'u', '0', '0', lowerHex[c>>4], lowerHex[c&0xf])
			} else {
				buf = append(buf, c)
			}
		}
	}
	return append(buf, '"'), nil
}

// validKey reports whether k matches ^[a-z][A-Za-z0-9]*$.
func validKey(k string) bool {
	if k == "" || k[0] < 'a' || k[0] > 'z' {
		return false
	}
	for i := 1; i < len(k); i++ {
		c := k[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// Unmarshal parses b strictly (A-2): it succeeds only if b is exactly the
// Marshal encoding of the Object it returns. Any other input fails with an
// error wrapping ErrNotCanonical that gives the byte offset.
func Unmarshal(b []byte) (Object, error) {
	p := parser{b: b}
	if len(b) == 0 || b[0] != '{' {
		return nil, p.fail("document must be an object")
	}
	o, err := p.object(1)
	if err != nil {
		return nil, err
	}
	if p.pos != len(b) {
		return nil, p.fail("trailing bytes")
	}
	return o, nil
}

type parser struct {
	b   []byte
	pos int
}

func (p *parser) fail(msg string) error {
	return fmt.Errorf("%w: offset %d: %s", ErrNotCanonical, p.pos, msg)
}

func (p *parser) value(depth int) (Value, error) {
	if p.pos >= len(p.b) {
		return nil, p.fail("unexpected end of input")
	}
	switch p.b[p.pos] {
	case '{':
		return p.object(depth)
	case '[':
		return p.array(depth)
	case '"':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return String(s), nil
	default:
		return nil, p.fail("expected an object, array or string")
	}
}

// object parses an object starting at p.b[p.pos] == '{'.
func (p *parser) object(depth int) (Object, error) {
	if depth > MaxDepth {
		return nil, p.fail("nesting too deep")
	}
	p.pos++ // '{'
	o := Object{}
	if p.peek('}') {
		p.pos++
		return o, nil
	}
	prev := ""
	for {
		if !p.peek('"') {
			return nil, p.fail("expected a key")
		}
		keyAt := p.pos
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		if !validKey(k) {
			p.pos = keyAt
			return nil, p.fail("key does not match ^[a-z][A-Za-z0-9]*$")
		}
		if len(o) > 0 && k <= prev {
			p.pos = keyAt
			return nil, p.fail("keys not strictly increasing")
		}
		if !p.peek(':') {
			return nil, p.fail("expected ':'")
		}
		p.pos++
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		o[k] = v
		prev = k
		switch {
		case p.peek(','):
			p.pos++
		case p.peek('}'):
			p.pos++
			return o, nil
		default:
			return nil, p.fail("expected ',' or '}'")
		}
	}
}

// array parses an array starting at p.b[p.pos] == '['.
func (p *parser) array(depth int) (Array, error) {
	if depth > MaxDepth {
		return nil, p.fail("nesting too deep")
	}
	p.pos++ // '['
	a := Array{}
	if p.peek(']') {
		p.pos++
		return a, nil
	}
	for {
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		switch {
		case p.peek(','):
			p.pos++
		case p.peek(']'):
			p.pos++
			return a, nil
		default:
			return nil, p.fail("expected ',' or ']'")
		}
	}
}

func (p *parser) peek(c byte) bool {
	return p.pos < len(p.b) && p.b[p.pos] == c
}

// str parses a string starting at p.b[p.pos] == '"' and accepts only the
// canonical escapes of RFC 8785.
func (p *parser) str() (string, error) {
	p.pos++ // opening quote
	var sb strings.Builder
	for {
		if p.pos >= len(p.b) {
			return "", p.fail("unterminated string")
		}
		c := p.b[p.pos]
		switch {
		case c == '"':
			p.pos++
			return sb.String(), nil
		case c == '\\':
			r, err := p.escape()
			if err != nil {
				return "", err
			}
			sb.WriteByte(r)
		case c < 0x20:
			return "", p.fail("unescaped control character")
		case c < utf8.RuneSelf:
			sb.WriteByte(c)
			p.pos++
		default:
			r, size := utf8.DecodeRune(p.b[p.pos:])
			if r == utf8.RuneError && size == 1 {
				return "", p.fail("invalid UTF-8")
			}
			sb.Write(p.b[p.pos : p.pos+size])
			p.pos += size
		}
	}
}

// escape parses one escape sequence at p.b[p.pos] == '\' and returns the
// byte it stands for.
func (p *parser) escape() (byte, error) {
	if p.pos+1 >= len(p.b) {
		return 0, p.fail("truncated escape")
	}
	var r byte
	switch p.b[p.pos+1] {
	case '"':
		r = '"'
	case '\\':
		r = '\\'
	case 'b':
		r = '\b'
	case 't':
		r = '\t'
	case 'n':
		r = '\n'
	case 'f':
		r = '\f'
	case 'r':
		r = '\r'
	case 'u':
		if p.pos+6 > len(p.b) {
			return 0, p.fail("truncated escape")
		}
		h := p.b[p.pos+2 : p.pos+6]
		if h[0] != '0' || h[1] != '0' || !isLowerHex(h[2]) || !isLowerHex(h[3]) {
			return 0, p.fail("non-canonical \\u escape")
		}
		r = unhex(h[2])<<4 | unhex(h[3])
		if r >= 0x20 || r == '\b' || r == '\t' || r == '\n' || r == '\f' || r == '\r' {
			return 0, p.fail("non-canonical \\u escape")
		}
		p.pos += 6
		return r, nil
	default:
		return 0, p.fail("non-canonical escape")
	}
	p.pos += 2
	return r, nil
}

func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

func unhex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}

// Int returns the D-12 text of n: base 10, no leading zeros, "-" for
// negatives.
func Int(n int64) String {
	return String(strconv.FormatInt(n, 10))
}

// ParseInt parses a String holding ^(0|-?[1-9][0-9]*)$ within int64.
func ParseInt(v Value) (int64, error) {
	s, err := Text(v)
	if err != nil {
		return 0, err
	}
	digits := strings.TrimPrefix(s, "-")
	if !canonicalDigits(digits) || (s != digits && digits == "0") {
		return 0, fmt.Errorf("%w: %q is not a canonical integer", ErrNotCanonical, s)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is out of the int64 range", ErrNotCanonical, s)
	}
	return n, nil
}

// Uint returns the base-10 text of n with no leading zeros.
func Uint(n uint64) String {
	return String(strconv.FormatUint(n, 10))
}

// ParseUint parses a String holding ^(0|[1-9][0-9]*)$ within uint64.
func ParseUint(v Value) (uint64, error) {
	s, err := Text(v)
	if err != nil {
		return 0, err
	}
	if !canonicalDigits(s) {
		return 0, fmt.Errorf("%w: %q is not a canonical unsigned integer", ErrNotCanonical, s)
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is out of the uint64 range", ErrNotCanonical, s)
	}
	return n, nil
}

// ParseVersion parses a version number: a canonical unsigned integer in
// [1, 2^32-1].
func ParseVersion(v Value) (uint32, error) {
	n, err := ParseUint(v)
	if err != nil {
		return 0, err
	}
	if n == 0 || n > 1<<32-1 {
		return 0, fmt.Errorf("%w: version %d not in [1, 4294967295]", ErrNotCanonical, n)
	}
	return uint32(n), nil
}

// canonicalDigits reports whether s matches ^(0|[1-9][0-9]*)$.
func canonicalDigits(s string) bool {
	if s == "" || (s[0] == '0' && len(s) > 1) {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Hex returns b as lowercase hex.
func Hex(b []byte) String {
	return String(hex.EncodeToString(b))
}

// ParseHex parses a String of exactly 2n lowercase hex characters.
func ParseHex(v Value, n int) ([]byte, error) {
	s, err := Text(v)
	if err != nil {
		return nil, err
	}
	if len(s) != 2*n || !allLowerHex(s) {
		return nil, fmt.Errorf("%w: not %d bytes of lowercase hex", ErrNotCanonical, n)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotCanonical, err)
	}
	return b, nil
}

func allLowerHex(s string) bool {
	for i := range len(s) {
		if !isLowerHex(s[i]) {
			return false
		}
	}
	return true
}

// UUID returns id as D-11 text: 36 lowercase characters, 8-4-4-4-12.
func UUID(id [16]byte) String {
	h := hex.EncodeToString(id[:])
	return String(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32])
}

// ParseUUID parses D-11 UUID text: exactly 36 characters, lowercase hex
// groups 8-4-4-4-12 separated by hyphens.
func ParseUUID(v Value) ([16]byte, error) {
	var id [16]byte
	s, err := Text(v)
	if err != nil {
		return id, err
	}
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return id, fmt.Errorf("%w: not a canonical uuid", ErrNotCanonical)
	}
	h := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if !allLowerHex(h) {
		return id, fmt.Errorf("%w: not a canonical uuid", ErrNotCanonical)
	}
	if _, err := hex.Decode(id[:], []byte(h)); err != nil {
		return id, fmt.Errorf("%w: %w", ErrNotCanonical, err)
	}
	return id, nil
}

// Text returns the string held by a String value.
func Text(v Value) (string, error) {
	s, ok := v.(String)
	if !ok {
		return "", fmt.Errorf("%w: want a string, got %s", ErrNotCanonical, kind(v))
	}
	return string(s), nil
}

// List returns the Array held by v.
func List(v Value) (Array, error) {
	a, ok := v.(Array)
	if !ok {
		return nil, fmt.Errorf("%w: want an array, got %s", ErrNotCanonical, kind(v))
	}
	return a, nil
}

// Obj returns the Object held by v.
func Obj(v Value) (Object, error) {
	o, ok := v.(Object)
	if !ok {
		return nil, fmt.Errorf("%w: want an object, got %s", ErrNotCanonical, kind(v))
	}
	return o, nil
}

func kind(v Value) string {
	switch v.(type) {
	case String:
		return "a string"
	case Array:
		return "an array"
	case Object:
		return "an object"
	default:
		return "nothing"
	}
}

// Fields returns o if its key set is exactly keys, otherwise an error
// wrapping ErrNotCanonical that names the missing and unknown keys.
func Fields(o Object, keys ...string) (Object, error) {
	var missing, unknown []string
	for _, k := range keys {
		if _, ok := o[k]; !ok {
			missing = append(missing, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(o)) {
		if !slices.Contains(keys, k) {
			unknown = append(unknown, k)
		}
	}
	if len(missing) == 0 && len(unknown) == 0 {
		return o, nil
	}
	return nil, fmt.Errorf("%w: missing keys %v, unknown keys %v", ErrNotCanonical, missing, unknown)
}

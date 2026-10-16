package sealing_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

var (
	key1 = bytes.Repeat([]byte{0x11}, sealing.KeySize)
	key2 = bytes.Repeat([]byte{0x22}, sealing.KeySize)
	// key3 differs from key1 but is stored under key1's ID in some tests.
	key3 = bytes.Repeat([]byte{0x33}, sealing.KeySize)

	round1 = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	round2 = [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}

	secret = []byte(`{"seed":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`)
)

func b64(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

func newKeyring(t *testing.T, active string, keys map[string][]byte) *sealing.Keyring {
	t.Helper()
	k, err := sealing.NewKeyring(active, keys)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := newKeyring(t, "k1", map[string][]byte{"k1": key1})
	if got := k.ActiveKeyID(); got != "k1" {
		t.Fatalf("ActiveKeyID = %q, want k1", got)
	}
	s, err := k.Seal(round1, secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if s.Scheme != "aes-256-gcm/v1" {
		t.Errorf("Scheme = %q, want aes-256-gcm/v1", s.Scheme)
	}
	if s.KeyID != "k1" {
		t.Errorf("KeyID = %q, want k1", s.KeyID)
	}
	if len(s.Nonce) != 12 {
		t.Errorf("len(Nonce) = %d, want 12", len(s.Nonce))
	}
	if len(s.Ciphertext) != len(secret)+16 {
		t.Errorf("len(Ciphertext) = %d, want %d", len(s.Ciphertext), len(secret)+16)
	}
	if bytes.Contains(s.Ciphertext, secret) {
		t.Error("the ciphertext contains the plaintext")
	}
	got, err := k.Open(round1, s)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("Open = %q, want %q", got, secret)
	}
}

func TestSealRandomNonce(t *testing.T) {
	k := newKeyring(t, "k1", map[string][]byte{"k1": key1})
	a, err := k.Seal(round1, secret)
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.Seal(round1, secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Nonce, b.Nonce) {
		t.Error("two seals drew the same nonce")
	}
	if bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Error("two seals of the same input gave the same ciphertext")
	}
}

// TestSealedFormatMatchesSpec opens a Seal output with the standard library
// directly, building the AAD byte by byte from K-5 independently of the
// package.
func TestSealedFormatMatchesSpec(t *testing.T) {
	k := newKeyring(t, "k2026-10", map[string][]byte{"k2026-10": key2})
	s, err := k.Seal(round1, secret)
	if err != nil {
		t.Fatal(err)
	}
	var aad []byte
	aad = append(aad, []byte("orvex/round-secret/v1")...)
	aad = append(aad, 0x00)
	aad = append(aad, []byte("aes-256-gcm/v1")...)
	aad = append(aad, 0x00)
	aad = append(aad, []byte("k2026-10")...)
	aad = append(aad, 0x00)
	aad = append(aad, round1[:]...)

	block, err := aes.NewCipher(key2)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	if gcm.NonceSize() != len(s.Nonce) || gcm.Overhead() != 16 {
		t.Fatalf("nonce %d, overhead %d", gcm.NonceSize(), gcm.Overhead())
	}
	got, err := gcm.Open(nil, s.Nonce, s.Ciphertext, aad)
	if err != nil {
		t.Fatalf("cipher.GCM Open with the spec AAD: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("plaintext = %q, want %q", got, secret)
	}
	// The same AAD with another round ID must not open.
	other := append(append([]byte(nil), aad[:len(aad)-16]...), round2[:]...)
	if _, err := gcm.Open(nil, s.Nonce, s.Ciphertext, other); err == nil {
		t.Error("opened with another round's AAD")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	k := newKeyring(t, "k1", map[string][]byte{"k1": key1})
	s, err := k.Seal(round1, secret)
	if err != nil {
		t.Fatal(err)
	}
	clone := func() sealing.Sealed {
		return sealing.Sealed{
			Scheme:     s.Scheme,
			KeyID:      s.KeyID,
			Nonce:      append([]byte(nil), s.Nonce...),
			Ciphertext: append([]byte(nil), s.Ciphertext...),
		}
	}
	flipCT := clone()
	flipCT.Ciphertext[0] ^= 1
	flipTag := clone()
	flipTag.Ciphertext[len(flipTag.Ciphertext)-1] ^= 0x80
	flipNonce := clone()
	flipNonce.Nonce[5] ^= 1
	shortNonce := clone()
	shortNonce.Nonce = shortNonce.Nonce[:11]
	otherScheme := clone()
	otherScheme.Scheme = "aes-256-gcm/v2"
	missingID := clone()
	missingID.KeyID = "k9"

	sameIDOtherKey := newKeyring(t, "k1", map[string][]byte{"k1": key3})

	cases := []struct {
		name  string
		k     *sealing.Keyring
		round [16]byte
		s     sealing.Sealed
		want  error
	}{
		{"flipped ciphertext bit", k, round1, flipCT, sealing.ErrOpen},
		{"flipped tag bit", k, round1, flipTag, sealing.ErrOpen},
		{"flipped nonce bit", k, round1, flipNonce, sealing.ErrOpen},
		{"short nonce", k, round1, shortNonce, sealing.ErrOpen},
		{"another round id", k, round2, clone(), sealing.ErrOpen},
		{"another scheme", k, round1, otherScheme, sealing.ErrScheme},
		{"key id missing from keyring", k, round1, missingID, sealing.ErrUnknownKey},
		{"nil keyring", nil, round1, clone(), sealing.ErrUnknownKey},
		{"another key under the same id", sameIDOtherKey, round1, clone(), sealing.ErrOpen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.k.Open(tc.round, tc.s)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open error = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("Open returned plaintext %q with an error", got)
			}
			msg := err.Error()
			for _, leak := range [][]byte{secret, tc.s.Nonce, tc.s.Ciphertext} {
				if len(leak) > 0 && (strings.Contains(msg, string(leak)) || strings.Contains(msg, hex.EncodeToString(leak))) {
					t.Errorf("error %q leaks sealed material", msg)
				}
			}
		})
	}
}

func TestOpenWithInactiveKey(t *testing.T) {
	old := newKeyring(t, "k1", map[string][]byte{"k1": key1})
	s, err := old.Seal(round1, secret)
	if err != nil {
		t.Fatal(err)
	}
	rotated := newKeyring(t, "k2", map[string][]byte{"k1": key1, "k2": key2})
	got, err := rotated.Open(round1, s)
	if err != nil {
		t.Fatalf("Open under inactive k1: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("Open = %q, want %q", got, secret)
	}
	s2, err := rotated.Seal(round1, secret)
	if err != nil {
		t.Fatal(err)
	}
	if s2.KeyID != "k2" {
		t.Errorf("Seal used key %q, want the active k2", s2.KeyID)
	}
	if _, err := old.Open(round1, s2); !errors.Is(err, sealing.ErrUnknownKey) {
		t.Errorf("old keyring opening a k2 row: %v, want ErrUnknownKey", err)
	}
}

func TestNewKeyring(t *testing.T) {
	many := map[string][]byte{}
	for i := range sealing.MaxKeys + 1 {
		many[fmt.Sprintf("k%d", i)] = key1
	}
	cases := []struct {
		name   string
		active string
		keys   map[string][]byte
	}{
		{"no keys", "k1", map[string][]byte{}},
		{"65 keys", "k1", many},
		{"31-byte key", "k1", map[string][]byte{"k1": key1[:31]}},
		{"33-byte key", "k1", map[string][]byte{"k1": append(append([]byte(nil), key1...), 0)}},
		{"bad id", "K1", map[string][]byte{"K1": key1}},
		{"active not listed", "k2", map[string][]byte{"k1": key1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, err := sealing.NewKeyring(tc.active, tc.keys)
			if !errors.Is(err, sealing.ErrKeyring) {
				t.Fatalf("NewKeyring error = %v, want ErrKeyring", err)
			}
			if k != nil {
				t.Error("NewKeyring returned a keyring with an error")
			}
			var ke *sealing.KeyringError
			if !errors.As(err, &ke) || ke.Reason == "" {
				t.Errorf("error %v is not a *KeyringError with a reason", err)
			}
		})
	}
	k, err := sealing.NewKeyring("a.b_c-1", map[string][]byte{"a.b_c-1": key1, "0": key2})
	if err != nil {
		t.Fatalf("valid keyring: %v", err)
	}
	if k.ActiveKeyID() != "a.b_c-1" {
		t.Errorf("ActiveKeyID = %q", k.ActiveKeyID())
	}
}

func TestParseKeyring(t *testing.T) {
	var sixtyFive strings.Builder
	sixtyFive.WriteString("active k0\n")
	for i := range sealing.MaxKeys + 1 {
		fmt.Fprintf(&sixtyFive, "key k%d %s\n", i, b64(key1))
	}
	sixtyFour := strings.Join(strings.Split(sixtyFive.String(), "\n")[:sealing.MaxKeys+1], "\n") + "\n"

	valid := []struct {
		name, text, active string
	}{
		{"minimal", "active k1\nkey k1 " + b64(key1) + "\n", "k1"},
		{"no trailing newline", "active k1\nkey k1 " + b64(key1), "k1"},
		{"comments, blanks, tabs and CRLF",
			"# keyring\r\n\r\n   # indented comment\r\n\tactive \t k2026-10\r\nkey k2026-01\t" + b64(key1) + "\r\n  key   k2026-10   " + b64(key2) + "  \r\n\r\n",
			"k2026-10"},
		{"64 keys", sixtyFour, "k0"},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			k, err := sealing.ParseKeyring([]byte(tc.text))
			if err != nil {
				t.Fatalf("ParseKeyring: %v", err)
			}
			if k.ActiveKeyID() != tc.active {
				t.Errorf("ActiveKeyID = %q, want %q", k.ActiveKeyID(), tc.active)
			}
			s, err := k.Seal(round1, secret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := k.Open(round1, s); err != nil {
				t.Fatalf("Open: %v", err)
			}
		})
	}

	line2 := "key k1 " + b64(key1)
	notPadded := strings.TrimRight(b64(key1), "=")
	invalid := []struct {
		name, text, reason string
	}{
		{"empty", "", "no active line"},
		{"no active", line2 + "\n", "no active line"},
		{"no key lines", "active k1\n", "no key lines"},
		{"two active lines", "active k1\nactive k1\n" + line2 + "\n", "line 2: second active line"},
		{"active not listed", "active k2\n" + line2 + "\n", "active key id is not listed"},
		{"duplicate id", "active k1\n" + line2 + "\nkey k1 " + b64(key2) + "\n", "line 3: duplicate key id"},
		{"bad key id", "active k1\n" + line2 + "\nkey Bad " + b64(key2) + "\n", "line 3: invalid key id"},
		{"bad active id", "active -k\n" + line2 + "\n", "line 1: invalid key id"},
		{"bad base64", "active k1\nkey k1 !!!" + b64(key1)[3:] + "\n", "line 2: key is not padded standard base64"},
		{"unpadded base64", "active k1\nkey k1 " + notPadded + "\n", "line 2: key is not padded standard base64"},
		{"url-safe base64", "active k1\nkey k1 " + base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)) + "\n", "line 2: key is not padded standard base64"},
		{"31-byte key", "active k1\nkey k1 " + b64(key1[:31]) + "\n", "line 2: key is not 32 bytes"},
		{"33-byte key", "active k1\nkey k1 " + b64(append(append([]byte(nil), key1...), 0x44)) + "\n", "line 2: key is not 32 bytes"},
		{"unknown directive", "active k1\n" + line2 + "\nretired k0\n", "line 3: unknown directive"},
		{"extra field on key", "active k1\n" + line2 + " extra\n", "line 2: wrong number of fields"},
		{"extra field on active", "active k1 k2\n" + line2 + "\n", "line 1: wrong number of fields"},
		{"missing key field", "active k1\nkey k1\n", "line 2: wrong number of fields"},
		{"not ascii", "active k1\n" + line2 + "\n# clé\n", "line 3: not ASCII"},
		{"CR inside the key field", "active k1\nkey k1 " + b64(key1)[:20] + "\r" + b64(key1)[20:] + "\n", "line 2: stray carriage return"},
		{"doubled trailing CR", "active k1\r\n" + line2 + "\r\r\n", "line 2: stray carriage return"},
		{"bare CR line break", "active k1\r" + line2 + "\n", "line 1: stray carriage return"},
		{"65 keys", sixtyFive.String(), "line 66: more than 64 keys"},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			k, err := sealing.ParseKeyring([]byte(tc.text))
			if !errors.Is(err, sealing.ErrKeyring) {
				t.Fatalf("ParseKeyring error = %v, want ErrKeyring", err)
			}
			if k != nil {
				t.Error("ParseKeyring returned a keyring with an error")
			}
			var ke *sealing.KeyringError
			if !errors.As(err, &ke) {
				t.Fatalf("error %v is not a *KeyringError", err)
			}
			if ke.Reason != tc.reason {
				t.Errorf("Reason = %q, want %q", ke.Reason, tc.reason)
			}
			msg := err.Error()
			for _, k := range [][]byte{key1, key2} {
				if strings.Contains(msg, b64(k)) || strings.Contains(msg, b64(k)[:20]) || strings.Contains(msg, string(k)) {
					t.Errorf("error %q contains key material", msg)
				}
			}
			for _, frag := range []string{"!!!", "Bad", "retired", "extra", "clé", "-k"} {
				if strings.Contains(msg, frag) {
					t.Errorf("error %q contains line content %q", msg, frag)
				}
			}
		})
	}
}

func writeFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { //nolint:gosec // G302: the test sets a too-open mode to check that LoadKeyringFile rejects it
		t.Fatal(err)
	}
	return path
}

func TestLoadKeyringFile(t *testing.T) {
	dir := t.TempDir()
	content := "active k1\nkey k1 " + b64(key1) + "\n"

	for _, mode := range []os.FileMode{0o400, 0o440, 0o600, 0o640} {
		t.Run(fmt.Sprintf("accepts %04o", uint32(mode)), func(t *testing.T) {
			path := writeFile(t, dir, fmt.Sprintf("ok-%04o", uint32(mode)), content, mode)
			k, err := sealing.LoadKeyringFile(path)
			if err != nil {
				t.Fatalf("LoadKeyringFile: %v", err)
			}
			if k.ActiveKeyID() != "k1" {
				t.Errorf("ActiveKeyID = %q", k.ActiveKeyID())
			}
		})
	}
	t.Run("accepts a symlink to a 0400 file", func(t *testing.T) {
		target := writeFile(t, dir, "target", content, 0o400)
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := sealing.LoadKeyringFile(link); err != nil {
			t.Fatalf("LoadKeyringFile via symlink: %v", err)
		}
	})

	big := "active k1\nkey k1 " + b64(key1) + "\n" + "#" + strings.Repeat("x", sealing.MaxKeyringFileBytes) + "\n"
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	rejects := []struct {
		name, path, reason string
	}{
		{"0604", writeFile(t, dir, "bad-0604", content, 0o604), "permissions too open (0604)"},
		{"0660", writeFile(t, dir, "bad-0660", content, 0o660), "permissions too open (0660)"},
		{"0644", writeFile(t, dir, "bad-0644", content, 0o644), "permissions too open (0644)"},
		{"directory", sub, "not a regular file"},
		{"missing file", filepath.Join(dir, "missing"), "unreadable"},
		{"over 64 KiB", writeFile(t, dir, "big", big, 0o600), "larger than 64 KiB"},
		{"malformed", writeFile(t, dir, "malformed", "active k1\nkey k1 !!!\n", 0o600), "line 2: key is not padded standard base64"},
	}
	for _, tc := range rejects {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			k, err := sealing.LoadKeyringFile(tc.path)
			if !errors.Is(err, sealing.ErrKeyring) {
				t.Fatalf("LoadKeyringFile error = %v, want ErrKeyring", err)
			}
			if k != nil {
				t.Error("returned a keyring with an error")
			}
			var ke *sealing.KeyringError
			if !errors.As(err, &ke) || ke.Reason != tc.reason {
				t.Fatalf("error %v, want reason %q", err, tc.reason)
			}
			msg := err.Error()
			if strings.Contains(msg, tc.path) || strings.Contains(msg, dir) || strings.Contains(msg, filepath.Base(tc.path)) {
				t.Errorf("error %q names the path", msg)
			}
			if strings.Contains(msg, b64(key1)) {
				t.Errorf("error %q contains key material", msg)
			}
		})
	}
}

func TestKeyringFormatting(t *testing.T) {
	k := newKeyring(t, "k2026-10", map[string][]byte{"k2026-01": key1, "k2026-10": key2})
	var outs []string
	for _, v := range []any{k, *k} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			outs = append(outs, fmt.Sprintf(verb, v))
		}
	}
	outs = append(outs, k.String(), k.GoString(), (*k).String(), (*k).GoString(), fmt.Sprint(k), fmt.Sprint([]*sealing.Keyring{k}))
	for _, out := range outs {
		if !strings.Contains(out, "k2026-10") {
			t.Errorf("%q does not name the active key", out)
		}
		if !strings.Contains(out, "keys: 2") {
			t.Errorf("%q does not count the keys", out)
		}
		for _, key := range [][]byte{key1, key2} {
			if strings.Contains(out, b64(key)) || strings.Contains(out, string(key)) || strings.Contains(out, hex.EncodeToString(key)) || strings.Contains(out, hex.EncodeToString(key[:4])) {
				t.Errorf("%q contains key material", out)
			}
		}
	}
	if got, want := k.String(), `sealing.Keyring{active: "k2026-10", keys: 2}`; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestSealNilKeyring(t *testing.T) {
	var k *sealing.Keyring
	if _, err := k.Seal(round1, secret); !errors.Is(err, sealing.ErrKeyring) {
		t.Errorf("nil Seal error = %v, want ErrKeyring", err)
	}
	if k.ActiveKeyID() != "" {
		t.Error("nil ActiveKeyID is not empty")
	}
}

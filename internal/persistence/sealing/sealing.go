// Package sealing seals and opens round secret configs with AES-256-GCM under
// a keyring of 256-bit keys (LBT-21,
// docs/specs/87d8d90c_round-secret-protection.md, decisions K-2 to K-5).
//
// Sealing and opening happen in Go, in the process that executes the round:
// PostgreSQL only ever receives the sealed form and never sees a key. A
// sealed secret binds its scheme, its key ID and its round ID through the
// additional authenticated data, so a ciphertext copied into another
// round's row fails to open.
//
// No error text, and no String, GoString or Format output, contains key
// bytes, base64 key text, plaintext, a nonce or ciphertext. Keyring errors
// name the rule broken (and the line, for a keyring file), never the file's
// contents or its path.
package sealing

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const (
	// Scheme is the sealing scheme of every sealed row this package
	// writes: AES-256-GCM with a random 96-bit nonce (K-5).
	Scheme = "aes-256-gcm/v1"
	// KeyringFileEnv names the environment variable that holds the path of
	// the keyring file (K-2). It never holds key material.
	KeyringFileEnv = "ORVEX_SECRET_KEYRING_FILE"
	// KeySize is the size of every key in bytes (AES-256).
	KeySize = 32
	// NonceSize is the size of every GCM nonce in bytes.
	NonceSize = 12
	// TagSize is the size of the GCM authentication tag in bytes; the
	// ciphertext of a sealed secret ends with it.
	TagSize = 16
	// MaxKeys is the largest number of keys a keyring may hold.
	MaxKeys = 64
	// MaxKeyringFileBytes is the largest keyring file LoadKeyringFile
	// accepts.
	MaxKeyringFileBytes = 64 << 10
)

// aadPrefix starts the additional authenticated data of every sealed secret
// (K-5).
const aadPrefix = "orvex/round-secret/v1"

var (
	// ErrKeyring reports a keyring that cannot be used. A *KeyringError
	// wraps it.
	ErrKeyring = errors.New("sealing: invalid keyring")
	// ErrUnknownKey reports a sealed secret whose key ID the keyring does
	// not hold.
	ErrUnknownKey = errors.New("sealing: the keyring has no key with the sealed secret's key id")
	// ErrScheme reports a sealed secret of a scheme this package does not
	// implement.
	ErrScheme = errors.New("sealing: unknown sealing scheme")
	// ErrOpen reports a sealed secret that failed authentication: a wrong
	// key under the right ID, a tampered nonce or ciphertext, or a
	// ciphertext bound to another round.
	ErrOpen = errors.New("sealing: sealed secret failed authentication")
)

// keyIDPattern is the K-2 key ID rule; the schema checks the same pattern
// on round_secrets.secret_key_id.
var keyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// KeyringError is a keyring that cannot be used. Reason is "unreadable",
// "not a regular file", "permissions too open (0644)", "larger than 64 KiB",
// "line N: <rule>" or a whole-keyring rule. It never holds file contents,
// key material or the path. errors.Is(err, ErrKeyring) holds.
type KeyringError struct {
	Reason string
}

// Error returns "sealing: invalid keyring: <reason>".
func (e *KeyringError) Error() string { return "sealing: invalid keyring: " + e.Reason }

// Unwrap returns ErrKeyring.
func (e *KeyringError) Unwrap() error { return ErrKeyring }

// Sealed is one sealed secret config. It maps one-to-one onto
// round_secrets' four sealed columns (K-5): secret_scheme, secret_key_id,
// secret_nonce and secret_ciphertext.
type Sealed struct {
	Scheme string
	KeyID  string
	Nonce  []byte
	// Ciphertext is the GCM ciphertext followed by the TagSize-byte tag.
	Ciphertext []byte
}

// Keyring holds one AEAD per key ID and the ID of the active key. It keeps
// no raw key bytes. Its zero value holds no keys; build one with
// NewKeyring, ParseKeyring or LoadKeyringFile.
type Keyring struct {
	active string
	aeads  map[string]cipher.AEAD
}

// NewKeyring builds a keyring from 1 to MaxKeys keys of KeySize bytes each,
// keyed by IDs that follow the K-2 pattern; active must be one of them. A
// rule broken is a *KeyringError. The caller may clear keys afterwards: the
// keyring keeps only the derived AEADs.
func NewKeyring(active string, keys map[string][]byte) (*Keyring, error) {
	switch {
	case len(keys) == 0:
		return nil, &KeyringError{Reason: "no keys"}
	case len(keys) > MaxKeys:
		return nil, &KeyringError{Reason: "more than 64 keys"}
	}
	aeads := make(map[string]cipher.AEAD, len(keys))
	for id, key := range keys {
		if !keyIDPattern.MatchString(id) {
			return nil, &KeyringError{Reason: "invalid key id"}
		}
		if len(key) != KeySize {
			return nil, &KeyringError{Reason: "key is not 32 bytes"}
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, &KeyringError{Reason: "key is not 32 bytes"}
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, &KeyringError{Reason: "cannot build AES-256-GCM"}
		}
		aeads[id] = aead
	}
	if _, ok := aeads[active]; !ok {
		return nil, &KeyringError{Reason: "active key id is not listed"}
	}
	return &Keyring{active: active, aeads: aeads}, nil
}

// lineError is the *KeyringError of a rule broken on line n.
func lineError(n int, rule string) error {
	return &KeyringError{Reason: fmt.Sprintf("line %d: %s", n, rule)}
}

// ParseKeyring parses a keyring file's contents (K-2): ASCII text with LF
// or CRLF line endings and no other CR; fields separated by spaces or tabs; blank lines and
// lines whose first non-blank character is '#' ignored; exactly one
// "active <id>" line and 1 to MaxKeys "key <id> <base64>" lines with unique
// IDs, the active ID among them; each key padded standard base64 (RFC 4648
// §4) of exactly KeySize bytes. Any other line or field is an error. Every
// error is a *KeyringError naming the line and the rule, never the line's
// content.
func ParseKeyring(b []byte) (*Keyring, error) {
	var (
		active     string
		haveActive bool
		keys       = map[string][]byte{}
	)
	defer func() {
		for _, k := range keys {
			clear(k)
		}
	}()
	for i, line := range bytes.Split(b, []byte("\n")) {
		n := i + 1
		line = bytes.TrimSuffix(line, []byte("\r"))
		for _, c := range line {
			if c >= 0x80 {
				return nil, lineError(n, "not ASCII")
			}
		}
		// Only one CR, the line ending's, is allowed: base64's decoders
		// silently skip CR and LF, so a CR left inside a field would let a
		// nonconforming key through.
		if bytes.IndexByte(line, '\r') >= 0 {
			return nil, lineError(n, "stray carriage return")
		}
		fields := strings.FieldsFunc(string(line), func(r rune) bool { return r == ' ' || r == '\t' })
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch fields[0] {
		case "active":
			if len(fields) != 2 {
				return nil, lineError(n, "wrong number of fields")
			}
			if haveActive {
				return nil, lineError(n, "second active line")
			}
			if !keyIDPattern.MatchString(fields[1]) {
				return nil, lineError(n, "invalid key id")
			}
			active, haveActive = fields[1], true
		case "key":
			if len(fields) != 3 {
				return nil, lineError(n, "wrong number of fields")
			}
			id := fields[1]
			if !keyIDPattern.MatchString(id) {
				return nil, lineError(n, "invalid key id")
			}
			if _, dup := keys[id]; dup {
				return nil, lineError(n, "duplicate key id")
			}
			if len(keys) == MaxKeys {
				return nil, lineError(n, "more than 64 keys")
			}
			key, err := base64.StdEncoding.Strict().DecodeString(fields[2])
			if err != nil {
				return nil, lineError(n, "key is not padded standard base64")
			}
			if len(key) != KeySize {
				clear(key)
				return nil, lineError(n, "key is not 32 bytes")
			}
			keys[id] = key
		default:
			return nil, lineError(n, "unknown directive")
		}
	}
	switch {
	case !haveActive:
		return nil, &KeyringError{Reason: "no active line"}
	case len(keys) == 0:
		return nil, &KeyringError{Reason: "no key lines"}
	}
	if _, ok := keys[active]; !ok {
		return nil, &KeyringError{Reason: "active key id is not listed"}
	}
	return NewKeyring(active, keys)
}

// LoadKeyringFile reads and parses the keyring file at path (K-2). It
// follows symlinks; the target must be a regular file of at most
// MaxKeyringFileBytes whose permission bits grant no group write and
// nothing to others (perm & 0o027 == 0). The mode and size are checked on
// the opened file, so the file parsed is the file checked. Every error is a
// *KeyringError; none names the path or holds file contents.
func LoadKeyringFile(path string) (*Keyring, error) {
	unreadable := &KeyringError{Reason: "unreadable"}
	// Stat before opening, so a FIFO or a device never blocks the open.
	before, err := os.Stat(path)
	if err != nil {
		return nil, unreadable
	}
	if !before.Mode().IsRegular() {
		return nil, &KeyringError{Reason: "not a regular file"}
	}
	f, err := os.Open(path) //nolint:gosec // G304: ORVEX_SECRET_KEYRING_FILE names the operator's own keyring; mode and size are checked on the opened file before parsing
	if err != nil {
		return nil, unreadable
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, unreadable
	}
	if !os.SameFile(before, info) || !info.Mode().IsRegular() {
		return nil, &KeyringError{Reason: "not a regular file"}
	}
	if perm := info.Mode().Perm(); perm&0o027 != 0 {
		return nil, &KeyringError{Reason: fmt.Sprintf("permissions too open (%04o)", uint32(perm))}
	}
	if info.Size() > MaxKeyringFileBytes {
		return nil, &KeyringError{Reason: "larger than 64 KiB"}
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxKeyringFileBytes+1))
	if err != nil {
		return nil, unreadable
	}
	defer clear(b)
	if len(b) > MaxKeyringFileBytes {
		return nil, &KeyringError{Reason: "larger than 64 KiB"}
	}
	return ParseKeyring(b)
}

// ActiveKeyID returns the ID of the key Seal uses, or "" for a nil keyring.
func (k *Keyring) ActiveKeyID() string {
	if k == nil {
		return ""
	}
	return k.active
}

// aad is the additional authenticated data of a sealed secret (K-5): the
// ASCII "orvex/round-secret/v1", 0x00, the scheme, 0x00, the key ID, 0x00,
// then the 16 raw bytes of the round ID.
func aad(scheme, keyID string, roundID [16]byte) []byte {
	b := make([]byte, 0, len(aadPrefix)+len(scheme)+len(keyID)+3+len(roundID))
	b = append(b, aadPrefix...)
	b = append(b, 0)
	b = append(b, scheme...)
	b = append(b, 0)
	b = append(b, keyID...)
	b = append(b, 0)
	return append(b, roundID[:]...)
}

// Seal seals plaintext for roundID under the active key with a fresh random
// nonce. It fails only on a nil keyring or a crypto/rand failure.
func (k *Keyring) Seal(roundID [16]byte, plaintext []byte) (Sealed, error) {
	if k == nil || k.aeads == nil {
		return Sealed{}, fmt.Errorf("%w: no keyring", ErrKeyring)
	}
	aead := k.aeads[k.active]
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, fmt.Errorf("sealing: draw nonce: %w", err)
	}
	return Sealed{
		Scheme:     Scheme,
		KeyID:      k.active,
		Nonce:      nonce,
		Ciphertext: aead.Seal(nil, nonce, plaintext, aad(Scheme, k.active, roundID)),
	}, nil
}

// Open authenticates and decrypts s for roundID with the key named by
// s.KeyID, active or not. An unknown scheme is ErrScheme, a key ID the
// keyring lacks is ErrUnknownKey, and a failed authentication is ErrOpen.
func (k *Keyring) Open(roundID [16]byte, s Sealed) ([]byte, error) {
	if s.Scheme != Scheme {
		return nil, ErrScheme
	}
	var aead cipher.AEAD
	if k != nil {
		aead = k.aeads[s.KeyID]
	}
	if aead == nil {
		if keyIDPattern.MatchString(s.KeyID) {
			return nil, fmt.Errorf("%w (key id %s)", ErrUnknownKey, s.KeyID)
		}
		return nil, ErrUnknownKey
	}
	if len(s.Nonce) != NonceSize {
		return nil, ErrOpen
	}
	plaintext, err := aead.Open(nil, s.Nonce, s.Ciphertext, aad(s.Scheme, s.KeyID, roundID))
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}

// String returns the active key ID and the number of keys, never key
// material. Its value receiver makes both Keyring and *Keyring print
// through it.
func (k Keyring) String() string {
	return fmt.Sprintf("sealing.Keyring{active: %q, keys: %d}", k.active, len(k.aeads))
}

// GoString returns String's text.
func (k Keyring) GoString() string { return k.String() }

// Format prints String's text for every verb, so no formatting verb reaches
// the AEADs and their key schedules.
func (k Keyring) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, k.String())
}

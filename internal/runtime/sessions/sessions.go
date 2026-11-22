// Package sessions holds the credentials of the first playable milestone:
// the players file, which maps bearer tokens to player IDs for the gateway,
// and the service token file, the secret the gateway presents to the engine.
//
// Both files are secrets. They load like the round-secret keyring (LBT-21
// K-2): a regular file whose permission bits grant no group write and
// nothing to others, read once at start. No error names a path or quotes
// file content, and a Players value keeps only SHA-256 digests of tokens.
package sessions

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// PlayerID is an authenticated player's ID: 1 to 64 characters of
// [a-z0-9_-].
type PlayerID string

var playerIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// ParsePlayerID returns s as a PlayerID if it matches ^[a-z0-9_-]{1,64}$.
func ParsePlayerID(s string) (PlayerID, bool) {
	if !playerIDPattern.MatchString(s) {
		return "", false
	}
	return PlayerID(s), true
}

// Token length bounds, in bytes.
const (
	MinPlayerTokenBytes  = 16
	MinServiceTokenBytes = 32
	maxTokenBytes        = 512
	maxFileBytes         = 1 << 20
)

// ErrInvalid is wrapped by every error that describes bad file content.
var ErrInvalid = errors.New("sessions: invalid credentials file")

// Players authenticates bearer tokens. The zero value authenticates
// nobody.
type Players struct {
	byDigest map[[sha256.Size]byte]PlayerID
}

// ParsePlayers parses a players file: one JSON object mapping each bearer
// token to a player ID, for example {"<token>": "alice"}. Tokens are
// 16 to 512 bytes of printable ASCII without spaces and appear once;
// several tokens may name one player.
func ParsePlayers(b []byte) (*Players, error) {
	notObject := fmt.Errorf("%w: players file is not a JSON object of strings", ErrInvalid)
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, notObject
	}
	p := &Players{byDigest: make(map[[sha256.Size]byte]PlayerID)}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, notObject
		}
		value, err := dec.Token()
		player, isString := value.(string)
		if err != nil || !isString {
			return nil, notObject
		}
		token, _ := key.(string)
		if !validToken(token, MinPlayerTokenBytes) {
			return nil, fmt.Errorf("%w: a token is not %d to %d bytes of printable ASCII without spaces",
				ErrInvalid, MinPlayerTokenBytes, maxTokenBytes)
		}
		id, ok := ParsePlayerID(player)
		if !ok {
			return nil, fmt.Errorf("%w: a player ID does not match %s", ErrInvalid, playerIDPattern)
		}
		digest := sha256.Sum256([]byte(token))
		if _, repeated := p.byDigest[digest]; repeated {
			return nil, fmt.Errorf("%w: a token appears more than once", ErrInvalid)
		}
		p.byDigest[digest] = id
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, notObject
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: players file has data after the object", ErrInvalid)
	}
	if len(p.byDigest) == 0 {
		return nil, fmt.Errorf("%w: players file names no player", ErrInvalid)
	}
	return p, nil
}

// Authenticate returns the player whose token is token.
func (p *Players) Authenticate(token string) (PlayerID, bool) {
	if p == nil {
		return "", false
	}
	id, ok := p.byDigest[sha256.Sum256([]byte(token))]
	return id, ok
}

// LoadPlayersFile reads and parses the players file at path.
func LoadPlayersFile(path string) (*Players, error) {
	b, err := readSecretFile(path)
	if err != nil {
		return nil, err
	}
	defer clear(b)
	return ParsePlayers(b)
}

// LoadServiceTokenFile reads the shared gateway-to-engine service token at
// path: one line of 32 to 512 bytes of printable ASCII without spaces,
// optionally followed by a newline.
func LoadServiceTokenFile(path string) (string, error) {
	b, err := readSecretFile(path)
	if err != nil {
		return "", err
	}
	defer clear(b)
	token := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if !validToken(token, MinServiceTokenBytes) {
		return "", fmt.Errorf("%w: service token is not %d to %d bytes of printable ASCII without spaces",
			ErrInvalid, MinServiceTokenBytes, maxTokenBytes)
	}
	return token, nil
}

// ValidServiceToken reports whether token is a service token
// LoadServiceTokenFile would return.
func ValidServiceToken(token string) bool { return validToken(token, MinServiceTokenBytes) }

func validToken(t string, minBytes int) bool {
	if len(t) < minBytes || len(t) > maxTokenBytes {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] <= ' ' || t[i] > '~' {
			return false
		}
	}
	return true
}

// readSecretFile follows symlinks; the target must be a regular file of at
// most 1 MiB whose permission bits satisfy perm&0o027 == 0. The checks run
// on the opened file.
func readSecretFile(path string) ([]byte, error) {
	unreadable := errors.New("sessions: credentials file is unreadable")
	before, err := os.Stat(path)
	if err != nil {
		return nil, unreadable
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrInvalid)
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator names their own credentials file; mode and size are checked on the opened file
	if err != nil {
		return nil, unreadable
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, unreadable
	}
	if !os.SameFile(before, info) || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrInvalid)
	}
	if perm := info.Mode().Perm(); perm&0o027 != 0 {
		return nil, fmt.Errorf("%w: permissions too open (%04o)", ErrInvalid, uint32(perm))
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, unreadable
	}
	if len(b) > maxFileBytes {
		clear(b)
		return nil, fmt.Errorf("%w: larger than 1 MiB", ErrInvalid)
	}
	return b, nil
}

package pgtest

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/juano-morello/orvex/internal/persistence/sealing"
)

// KeyID is the key ID of the fixed test keyring (LBT-21 K-2: CI holds only
// this key).
const KeyID = "pgtest"

// testKey returns the fixed test key, SHA-256("orvex/pgtest/keyring/v1"). It
// seals nothing outside tests.
func testKey() []byte {
	k := sha256.Sum256([]byte("orvex/pgtest/keyring/v1"))
	return k[:]
}

var keyring = sync.OnceValue(func() *sealing.Keyring {
	k, err := sealing.NewKeyring(KeyID, map[string][]byte{KeyID: testKey()})
	if err != nil {
		panic("pgtest: test keyring: " + err.Error())
	}
	return k
})

// Keyring returns the fixed test keyring: one key, ID "pgtest", active.
// LoadHistory seals with it and roundtest's executors use it.
func Keyring() *sealing.Keyring { return keyring() }

// KeyringFile writes the fixed test keyring in the K-2 file format to a
// temporary file with mode 0600 and returns its path.
func KeyringFile(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keyring")
	content := "active " + KeyID + "\nkey " + KeyID + " " + base64.StdEncoding.EncodeToString(testKey()) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("pgtest: write keyring file: %v", err)
	}
	return path
}

// Seal seals secret for roundID with the fixed test keyring and fails the
// test on error.
func Seal(t testing.TB, roundID [16]byte, secret []byte) sealing.Sealed {
	t.Helper()
	s, err := Keyring().Seal(roundID, secret)
	if err != nil {
		t.Fatalf("pgtest: seal: %v", err)
	}
	return s
}

package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlayersAuthenticate(t *testing.T) {
	p, err := ParsePlayers([]byte(`{"alice-token-0001": "alice", "alice-token-0002": "alice", "bob-token-000001": "b_o-b9"}`))
	if err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]PlayerID{"alice-token-0001": "alice", "alice-token-0002": "alice", "bob-token-000001": "b_o-b9"} {
		if got, ok := p.Authenticate(token); !ok || got != want {
			t.Errorf("Authenticate(%q) = %q, %v; want %q", token, got, ok, want)
		}
	}
	for _, token := range []string{"", "alice", "alice-token-0003", "ALICE-TOKEN-0001"} {
		if got, ok := p.Authenticate(token); ok {
			t.Errorf("Authenticate(%q) = %q, want no player", token, got)
		}
	}
	var none *Players
	if _, ok := none.Authenticate("alice-token-0001"); ok {
		t.Error("nil Players authenticated a token")
	}
}

func TestParsePlayersRejects(t *testing.T) {
	for name, content := range map[string]string{ //nolint:gosec // G101: malformed test fixtures, not credentials
		"not json":         `alice`,
		"array":            `["alice"]`,
		"number value":     `{"alice-token-0001": 7}`,
		"empty":            `{}`,
		"trailing":         `{"alice-token-0001": "alice"} {}`,
		"short token":      `{"short": "alice"}`,
		"token with space": `{"alice token 0001": "alice"}`,
		"upper player":     `{"alice-token-0001": "Alice"}`,
		"empty player":     `{"alice-token-0001": ""}`,
		"long player":      `{"alice-token-0001": "` + strings.Repeat("a", 65) + `"}`,
		"nul player":       `{"alice-token-0001": "al\u0000ice"}`,
		"repeated token":   `{"alice-token-0001": "alice", "alice-token-0001": "mallory"}`,
		"repeated same":    `{"alice-token-0001": "alice", "alice-token-0001": "alice"}`,
		"escaped repeat":   `{"alice-token-0001": "alice", "\u0061lice-token-0001": "mallory"}`,
		"nested object":    `{"alice-token-0001": {"player": "alice"}}`,
		"null":             `null`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePlayers([]byte(content))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParsePlayers = %v, want ErrInvalid", err)
			}
			if strings.Contains(err.Error(), "alice-token") || strings.Contains(err.Error(), "mallory") {
				t.Errorf("error %q quotes the file's content", err)
			}
		})
	}
}

func TestLoadPlayersFile(t *testing.T) {
	p, err := LoadPlayersFile(writeFile(t, `{"alice-token-0001": "alice"}`, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := p.Authenticate("alice-token-0001"); !ok || got != "alice" {
		t.Errorf("Authenticate = %q, %v", got, ok)
	}
	if _, err := LoadPlayersFile(writeFile(t, `{"alice-token-0001": "alice"}`, 0o604)); !errors.Is(err, ErrInvalid) {
		t.Errorf("world-readable file: %v, want ErrInvalid", err)
	}
	if _, err := LoadPlayersFile(t.TempDir()); !errors.Is(err, ErrInvalid) {
		t.Errorf("directory: %v, want ErrInvalid", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := LoadPlayersFile(missing); err == nil || strings.Contains(err.Error(), missing) {
		t.Errorf("missing file: %v, want an error that does not name the path", err)
	}
}

func TestLoadServiceTokenFile(t *testing.T) {
	token := strings.Repeat("k", MinServiceTokenBytes)
	for _, content := range []string{token, token + "\n", token + "\r\n"} {
		got, err := LoadServiceTokenFile(writeFile(t, content, 0o640))
		if err != nil || got != token {
			t.Errorf("LoadServiceTokenFile(%q) = %q, %v; want the token", content, got, err)
		}
	}
	for name, content := range map[string]string{
		"short":        token[1:],
		"two lines":    token + "\n" + token,
		"inner space":  token[:10] + " " + token[11:],
		"empty":        "",
		"blank prefix": " " + token,
	} {
		if _, err := LoadServiceTokenFile(writeFile(t, content, 0o600)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := LoadServiceTokenFile(writeFile(t, token, 0o660)); !errors.Is(err, ErrInvalid) {
		t.Errorf("group-writable file: %v, want ErrInvalid", err)
	}
}

func TestParsePlayerID(t *testing.T) {
	for _, s := range []string{"a", "p_ana", "0-9_z", strings.Repeat("x", 64)} {
		if _, ok := ParsePlayerID(s); !ok {
			t.Errorf("ParsePlayerID(%q) refused", s)
		}
	}
	for _, s := range []string{"", "A", "a b", "a.b", "a\x00", strings.Repeat("x", 65)} {
		if _, ok := ParsePlayerID(s); ok {
			t.Errorf("ParsePlayerID(%q) accepted", s)
		}
	}
}

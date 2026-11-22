package grant_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/runtime/grant"
)

func TestParse(t *testing.T) {
	key128 := "k" + strings.Repeat("x", 127)
	player256 := strings.Repeat("é", 128)
	for _, c := range []struct {
		key, player, credits string
		want                 int64
	}{
		{"a", "alice", "1", 1},
		{"0", "bob", "500", 500},
		{"demo:alice.1_x-9", "alice", "1000000000000", grant.MaxCredits},
		{key128, "Bob Smith", "42", 42},
		{"k", player256, "7", 7},
		{"k", "ünïcødé ✓", "7", 7},
	} {
		g, err := grant.Parse(c.key, c.player, c.credits)
		if err != nil {
			t.Errorf("Parse(%q, %q, %q): %v", c.key, c.player, c.credits, err)
			continue
		}
		if g.Key() != c.key || g.Player() != c.player || g.Credits() != c.want {
			t.Errorf("Parse(%q, %q, %q) = %q %q %d", c.key, c.player, c.credits, g.Key(), g.Player(), g.Credits())
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, c := range []struct {
		name, key, player, credits string
	}{
		{"empty key", "", "alice", "1"},
		{"uppercase key", "Alice-1", "alice", "1"},
		{"key starts with a dash", "-a", "alice", "1"},
		{"key starts with a dot", ".a", "alice", "1"},
		{"key with a space", "a b", "alice", "1"},
		{"key with a slash", "a/b", "alice", "1"},
		{"key too long", "k" + strings.Repeat("x", 128), "alice", "1"},
		{"key with a newline", "a\n", "alice", "1"},
		{"empty player", "k", "", "1"},
		{"player with a newline", "k", "ali\nce", "1"},
		{"player with a tab", "k", "ali\tce", "1"},
		{"player with DEL", "k", "ali\x7fce", "1"},
		{"player with a C1 control", "k", "ali\u0085ce", "1"},
		{"player not UTF-8", "k", "ali\xffce", "1"},
		{"player too long", "k", strings.Repeat("x", grant.MaxPlayerBytes+1), "1"},
		{"zero credits", "k", "alice", "0"},
		{"negative credits", "k", "alice", "-1"},
		{"signed credits", "k", "alice", "+5"},
		{"leading zero", "k", "alice", "01"},
		{"empty credits", "k", "alice", ""},
		{"decimal credits", "k", "alice", "1.5"},
		{"credits with a space", "k", "alice", " 5"},
		{"credits over the cap", "k", "alice", "1000000000001"},
		{"credits over int64", "k", "alice", "99999999999999999999"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := grant.Parse(c.key, c.player, c.credits); !errors.Is(err, grant.ErrInvalid) {
				t.Errorf("Parse(%q, %q, %q) = %v, want ErrInvalid", c.key, c.player, c.credits, err)
			}
		})
	}
}

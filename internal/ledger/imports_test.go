package ledger

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLedgerImportsArePure keeps the package free of I/O, clocks and
// randomness (E-1): non-test files import only the allowlist.
func TestLedgerImportsArePure(t *testing.T) {
	allowed := map[string]bool{
		"crypto/sha256": true,
		"encoding/hex":  true,
		"errors":        true,
		"fmt":           true,
		"strconv":       true,
		"github.com/juano-morello/orvex/internal/game":          true,
		"github.com/juano-morello/orvex/internal/game/crash/v1": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s: %v", name, imp.Path.Value, err)
			}
			if !allowed[path] {
				t.Errorf("%s imports %q, which is not on the pure-code allowlist", name, path)
			}
		}
	}
	if checked < 4 {
		t.Errorf("checked %d files, want at least 4 (doc, ledger, ids, crashv1)", checked)
	}
}

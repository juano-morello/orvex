package v1

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/apd/v3"
)

func TestCheckedArithmeticOverflow(t *testing.T) {
	if got, err := mulNonNeg(3_037_000_499, 3_037_000_499); err != nil || got != 9_223_372_030_926_249_001 {
		t.Errorf("mulNonNeg(3037000499, 3037000499) = %d, %v", got, err)
	}
	for _, c := range []struct{ a, b int64 }{
		{3_037_000_500, 3_037_000_500},
		{math.MaxInt64, 2},
		{-1, 1},
	} {
		if got, err := mulNonNeg(c.a, c.b); !errors.Is(err, ErrArithmetic) {
			t.Errorf("mulNonNeg(%d, %d) = %d, %v; want ErrArithmetic", c.a, c.b, got, err)
		}
	}
	for _, c := range []struct{ a, b int64 }{
		{math.MinInt64, 1},
		{math.MaxInt64, -1},
	} {
		if got, err := subChecked(c.a, c.b); !errors.Is(err, ErrArithmetic) {
			t.Errorf("subChecked(%d, %d) = %d, %v; want ErrArithmetic", c.a, c.b, got, err)
		}
	}
	if got, err := subChecked(5, 7); err != nil || got != -2 {
		t.Errorf("subChecked(5, 7) = %d, %v; want -2", got, err)
	}
}

// TestApdCheck pins the D-4 fail-closed rule: an apd error, or any condition
// outside Inexact|Rounded, is ErrArithmetic; Inexact and Rounded alone pass.
func TestApdCheck(t *testing.T) {
	allowed := []apd.Condition{0, apd.Inexact, apd.Rounded, apd.Inexact | apd.Rounded}
	for _, c := range allowed {
		if err := apdCheck("op", c, nil); err != nil {
			t.Errorf("apdCheck(%v, nil) = %v; want nil", c, err)
		}
	}

	forbidden := []apd.Condition{
		apd.SystemOverflow,
		apd.SystemUnderflow,
		apd.Overflow,
		apd.Underflow,
		apd.Subnormal,
		apd.DivisionUndefined,
		apd.DivisionByZero,
		apd.DivisionImpossible,
		apd.InvalidOperation,
		apd.Clamped,
	}
	for _, f := range forbidden {
		for _, extra := range allowed {
			c := f | extra
			if err := apdCheck("op", c, nil); !errors.Is(err, ErrArithmetic) {
				t.Errorf("apdCheck(%v, nil) = %v; want ErrArithmetic", c, err)
			}
		}
	}

	apdErr := errors.New("apd failure")
	for _, c := range append(allowed, apd.Overflow) {
		err := apdCheck("op", c, apdErr)
		if !errors.Is(err, ErrArithmetic) {
			t.Errorf("apdCheck(%v, error) = %v; want ErrArithmetic", c, err)
		}
	}
}

// TestNoIOImports keeps the package pure (ADR-003): no I/O, clock, randomness,
// floats or mutable package state in any non-test file, covering the frozen
// math files and the round lifecycle files (LBT-8) alike.
func TestNoIOImports(t *testing.T) {
	allowed := map[string]bool{
		"crypto/hmac":                   true,
		"crypto/sha256":                 true,
		"encoding/binary":               true,
		"encoding/hex":                  true,
		"errors":                        true,
		"fmt":                           true,
		"math":                          true,
		"math/bits":                     true,
		"slices":                        true,
		"github.com/cockroachdb/apd/v3": true,
		"github.com/juano-morello/orvex/internal/game":      true,
		"github.com/juano-morello/orvex/internal/canonjson": true,
	}
	allowedVars := map[string]bool{
		"ErrOutOfRange":         true,
		"ErrArithmetic":         true,
		"ErrInvalidConfig":      true,
		"ErrInvalidState":       true,
		"ErrEventNotApplicable": true,
		"ErrRejected":           true,
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked[name] = true

		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s: %v", name, imp.Path.Value, err)
			}
			if !allowed[path] {
				t.Errorf("%s imports %q, which is not on the pure-code allowlist", name, path)
			}
			if imp.Name != nil {
				t.Errorf("%s renames import %q; named, blank and dot imports are not allowed", name, path)
			}
		}

		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, id := range spec.(*ast.ValueSpec).Names {
					if !allowedVars[id.Name] {
						t.Errorf("%s: package-level var %s; only the sentinel errors are allowed", fset.Position(id.Pos()), id.Name)
					}
				}
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && x.Name == "fmt" && n.Sel.Name != "Errorf" {
					t.Errorf("%s: fmt.%s is not allowed; only fmt.Errorf", fset.Position(n.Pos()), n.Sel.Name)
				}
			case *ast.Ident:
				if n.Name == "float32" || n.Name == "float64" {
					t.Errorf("%s: %s is not allowed in crash/v1 arithmetic", fset.Position(n.Pos()), n.Name)
				}
			}
			return true
		})
	}

	for _, want := range []string{
		"params.go", "curve.go", "payout.go", "fairness.go",
		"round.go", "commands.go", "events.go", "decide.go", "evolve.go", "projection.go", "codec.go",
	} {
		if !checked[want] {
			t.Errorf("%s was not checked", want)
		}
	}
}

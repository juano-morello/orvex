package verification_test

import (
	"bytes"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/verification"
)

// FuzzSingleByteFlip: changing any one byte of the worked example never
// verifies (spec A-19).
func FuzzSingleByteFlip(f *testing.F) {
	data := readGolden(f, "worked-example.jsonl")
	for _, seed := range []struct {
		offset uint
		xor    byte
	}{{0, 1}, {1, 0x20}, {uint(len(data) / 2), 1}, {uint(len(data) - 1), 1}, {uint(len(data) - 2), 0xff}} {
		f.Add(seed.offset, seed.xor)
	}
	opt := options(f, "verify", nil)
	f.Fuzz(func(t *testing.T, offset uint, xor byte) {
		if xor == 0 {
			return
		}
		mutated := bytes.Clone(data)
		mutated[offset%uint(len(mutated))] ^= xor
		if _, err := verification.Verify(bytes.NewReader(mutated), opt); err == nil {
			t.Fatalf("flip %#x at offset %d verifies", xor, offset%uint(len(mutated)))
		}
	})
}

// FuzzSingleByteFlipV2: changing any one byte of the in-progress v2 history
// never verifies (LBT-18 L-12).
func FuzzSingleByteFlipV2(f *testing.F) {
	data := readGoldenV2(f, "in-progress.jsonl")
	for _, seed := range []struct {
		offset uint
		xor    byte
	}{{0, 1}, {1, 0x20}, {uint(len(data) / 2), 1}, {uint(len(data) - 1), 1}, {uint(len(data) - 2), 0xff}} {
		f.Add(seed.offset, seed.xor)
	}
	opt := optionsV2(f, "verify", nil)
	f.Fuzz(func(t *testing.T, offset uint, xor byte) {
		if xor == 0 {
			return
		}
		mutated := bytes.Clone(data)
		mutated[offset%uint(len(mutated))] ^= xor
		if _, err := verification.Verify(bytes.NewReader(mutated), opt); err == nil {
			t.Fatalf("flip %#x at offset %d verifies", xor, offset%uint(len(mutated)))
		}
	})
}

package eventstore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
)

const goldenDir = "../../audit/verification/testdata/golden"

var goldenFiles = []string{"worked-example.jsonl", "instant-crash.jsonl", "empty-round.jsonl", "in-progress.jsonl"}

// TestEncodeHeaderMatchesGolden rebuilds each golden history's header from
// its parsed fields and requires the exact bytes.
func TestEncodeHeaderMatchesGolden(t *testing.T) {
	for _, name := range goldenFiles {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(goldenDir, name)) //nolint:gosec // G304: fixed golden fixture under testdata
			if err != nil {
				t.Fatal(err)
			}
			line, _, _ := bytes.Cut(b, []byte("\n"))
			o, err := canonjson.Unmarshal(line)
			if err != nil {
				t.Fatal(err)
			}
			var head chain.Hash
			hb, err := canonjson.ParseHex(o["head"], len(head))
			if err != nil {
				t.Fatal(err)
			}
			copy(head[:], hb)
			so, err := canonjson.Obj(o["configSecret"])
			if err != nil {
				t.Fatal(err)
			}
			secret, err := canonjson.Marshal(so)
			if err != nil {
				t.Fatal(err)
			}
			list, err := canonjson.List(o["checkpoints"])
			if err != nil {
				t.Fatal(err)
			}
			var cps []Checkpoint
			for _, v := range list {
				co, err := canonjson.Obj(v)
				if err != nil {
					t.Fatal(err)
				}
				seq, err := canonjson.ParseUint(co["sequence"])
				if err != nil {
					t.Fatal(err)
				}
				var cp Checkpoint
				h, err := canonjson.ParseHex(co["stateHash"], len(cp.StateHash))
				if err != nil {
					t.Fatal(err)
				}
				cp.Sequence = seq
				copy(cp.StateHash[:], h)
				cps = append(cps, cp)
			}
			got, err := encodeHeader(head, cps, secret)
			if err != nil {
				t.Fatalf("encodeHeader: %v", err)
			}
			if !bytes.Equal(got, line) {
				t.Errorf("encodeHeader =\n%s\nwant\n%s", got, line)
			}
		})
	}
}

func TestEncodeHeaderRejectsBadSecret(t *testing.T) {
	head, _ := hex.DecodeString("7cf8cc4b1807099cdd1f16cbc0c790f1a877f30b696ebc0de751997d73ac3da7")
	var h chain.Hash
	copy(h[:], head)
	cps := []Checkpoint{{Sequence: 0}}
	for name, secret := range map[string]string{
		"not canonical":  `{"seed":"00", "x":"1"}`,
		"unsorted keys":  `{"seed":"00","a":"1"}`,
		"not an object":  `["seed"]`,
		"string":         `"seed"`,
		"empty":          ``,
		"trailing bytes": `{"seed":"00"}x`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := encodeHeader(h, cps, []byte(secret)); !errors.Is(err, ErrCorrupt) {
				t.Errorf("encodeHeader(%q) err = %v, want ErrCorrupt", secret, err)
			}
		})
	}
}

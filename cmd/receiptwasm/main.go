//go:build js && wasm

// Command receiptwasm exposes the historical Crash verifier to a dedicated browser worker.
package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"syscall/js"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/game"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
)

func main() {
	reg, err := game.NewRegistry(v1.Adapter())
	if err != nil {
		panic(err)
	}
	js.Global().Set("verifySettled", js.FuncOf(func(_ js.Value, args []js.Value) any {
		var head chain.Hash
		if len(args) != 2 || len(args[1].String()) != hex.EncodedLen(len(head)) {
			return "Invalid expected chain head."
		}
		if _, err := hex.Decode(head[:], []byte(args[1].String())); err != nil {
			return "Invalid expected chain head."
		}
		data := make([]byte, args[0].Length())
		js.CopyBytesToGo(data, args[0])
		// Bound historical replay before decoding. Every v2 record occupies one line.
		if bytes.Count(data, []byte{'\n'}) > 4096 {
			return "Browser limit is 4,096 history lines. Use crashctl to verify this export."
		}
		report, err := verification.Verify(bytes.NewReader(data), verification.Options{
			Mode: verification.ModeVerify, Registry: reg, ExpectHead: &head, LedgerRules: []ledger.Rule{ledger.CrashV1()},
		})
		if err != nil {
			var failure *verification.Failure
			if errors.As(err, &failure) {
				verdict := "failed"
				if failure.Unsupported {
					verdict = "unsupported"
				}
				location := "header"
				if failure.Sequence != verification.HeaderSequence {
					location = fmt.Sprintf("sequence %d", failure.Sequence)
				}
				return fmt.Sprintf("Audit %s: %s at %s.", verdict, failure.Stage, location)
			}
			return "Round history failed audit verification."
		}
		state, ok := report.State.(v1.State)
		if !ok || state.Phase != v1.PhaseSettled || report.Ledger == nil {
			return "A settled Crash v1 history with v2 ledger evidence is required."
		}
		return true
	}))
	select {}
}

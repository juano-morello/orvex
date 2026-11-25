//go:build integration && acceptance && browser

package firstround

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestBrowserRound(t *testing.T) {
	p := newPlatform(t, "alice", "bob", "charlie")
	for _, player := range []string{"alice", "bob"} {
		p.grant("browser-"+player, player, 1000)
	}
	engine := p.startEngine(engineConfig{createRounds: false, bettingWindow: 15 * time.Second})
	gateway := p.startGateway(engine.addr)
	round := createControlledRound(t, p, controlledMinD, controlledMaxD)
	dir := t.TempDir()
	run := func(script string, extra ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "node", script) //nolint:gosec // G204: the test selects a checked-in browser scenario.
		cmd.Dir = "../../../web"
		cmd.Env = append(os.Environ(), "ORVEX_GATEWAY_ADDR="+gateway.addr, "ALICE_TOKEN="+p.tokens["alice"], "BOB_TOKEN="+p.tokens["bob"], "CHARLIE_TOKEN="+p.tokens["charlie"], "EVIDENCE_DIR="+dir)
		cmd.Env = append(cmd.Env, extra...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("browser round: %v\n%s", err, out)
		}
	}
	run("tests/round.mjs")
	records := p.obs.records(round.id)
	_, history := p.auditV2(round.id, records)
	if err := os.WriteFile(filepath.Join(dir, "round.jsonl"), history, 0600); err != nil {
		t.Fatal(err)
	}
	run("tests/verify.mjs", "EXPECTED_HEAD="+records[len(records)-1].Hash.String())
	run("tests/reconnect.mjs")

	if payouts := p.obs.payoutsTo(round.id, "alice"); len(payouts) != 1 {
		t.Fatalf("Alice received %d payouts, want one", len(payouts))
	}
}

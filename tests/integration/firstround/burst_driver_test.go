//go:build integration && acceptance && spectatorload

package firstround

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
)

type burstAttempt struct {
	Player, ClientID, Kind                            string
	ScheduledUS, SentUS, ReceivedUS, Seq, EffectiveUS int64
	HTTPStatus                                        int
	Error                                             string
	Provenance                                        string `json:",omitempty"`
	ClockOffsetUS, ClockRTTUS                         int64  `json:",omitempty"`
}

// TestHotRoundBaseline runs bounded, open-loop trials against real services.
// ORVEX_BURST_OUT=/absolute/path go test -tags 'integration acceptance spectatorload' -run '^TestHotRoundBaseline$' -count=1 -timeout 2h -v ./tests/integration/firstround
// ORVEX_BURST_LEVELS (default 10,40,80) and ORVEX_BURST_REPEATS (default 3)
// control the player counts and rotated repetitions. Each level runs normal,
// deadline, and same-ID retry arms. Docker and Go are required.
func TestHotRoundBaseline(t *testing.T) {
	if raceEnabled {
		t.Fatal("run the baseline without -race")
	}
	out := os.Getenv("ORVEX_BURST_OUT")
	if !filepath.IsAbs(out) {
		t.Fatal("ORVEX_BURST_OUT must be an absolute directory")
	}
	if err := os.MkdirAll(out, 0o700); err != nil { //nolint:gosec // G703: operator-selected experiment output directory
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(out); err != nil || len(entries) > 0 {
		t.Fatal("choose an empty ORVEX_BURST_OUT directory for each experiment")
	}
	var levels []int
	for _, s := range strings.Split(envString("ORVEX_BURST_LEVELS", "10,40,80"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 || n > 100 {
			t.Fatalf("invalid player count %q (want 1..100)", s)
		}
		if slices.Contains(levels, n) {
			t.Fatalf("duplicate player count %d", n)
		}
		levels = append(levels, n)
	}
	repeats := envInt(t, "ORVEX_BURST_REPEATS", 3)
	arms := strings.Split(envString("ORVEX_BURST_ARMS", "normal,deadline,retry"), ",")
	seenArms := make(map[string]bool)
	for _, arm := range arms {
		if !slices.Contains([]string{"normal", "deadline", "retry"}, arm) || seenArms[arm] {
			t.Fatalf("invalid or duplicate burst arm %q", arm)
		}
		seenArms[arm] = true
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Reserve this directory before starting services. Concurrent experiments
	// must never aggregate each other's raw trials.
	plan, err := os.OpenFile(filepath.Join(out, "workload.txt"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304,G703: fixed filename in the operator-selected directory
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(plan, "Players %v; repeats %d; %v trials.\nBet window %s, ordinary cash-out window %s, deadline window 100ms, retry delay 20ms.\n", levels, repeats, arms, benchBetSpan, benchCashSpan)
	closeErr := plan.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	src := buildBenchSource(t, benchSource{Label: "head", Dir: root})
	if !t.Run("warmup", func(t *testing.T) {
		runBurstTrial(t, src, t.TempDir(), "normal", levels[0], 0)
	}) {
		t.Fatal("warmup failed")
	}
	for rep := range repeats {
		for j := range len(levels) * len(arms) {
			index := (j + rep) % (len(levels) * len(arms))
			n, arm := levels[index/len(arms)], arms[index%len(arms)]
			t.Run(fmt.Sprintf("%s-%d-repeat-%d", arm, n, rep), func(t *testing.T) { runBurstTrial(t, src, out, arm, n, rep) })
		}
	}
}

func burstID(s string) [16]byte {
	h := sha256.Sum256([]byte("orvex/hot-round-baseline/" + s))
	var id [16]byte
	copy(id[:], h[:16])
	id[6] = id[6]&15 | 0x40
	id[8] = id[8]&63 | 0x80
	return id
}
func burstUUID(s string) string { return string(canonjson.UUID(burstID(s))) }

func runBurstTrial(t *testing.T, source benchSource, out, arm string, count, repeat int) {
	t.Helper()
	names := make([]string, count)
	for i := range names {
		names[i] = burstUUID(fmt.Sprintf("player/%d", i))
	}
	p := newPlatformFrom(t, source.bin, names...)
	for _, name := range names {
		roundtest.FundPlayer(t, p.obs.db, name, benchFunding)
	}
	engine := p.startEngine(engineConfig{createRounds: false, bettingWindow: benchWindow, poolMaxConns: 4})
	gatewayAddr := "127.0.0.1:0"
	if host := os.Getenv("ORVEX_BURST_GATEWAY_HOST"); host != "" {
		if net.ParseIP(host) == nil {
			t.Fatal("ORVEX_BURST_GATEWAY_HOST must be an IP address")
		}
		gatewayAddr = net.JoinHostPort(host, "0")
	}
	gw := p.startGatewayAt(engine.addr, gatewayAddr)
	players := make([]*player, count)
	for i, name := range names {
		players[i] = p.player(name, gw)
	}
	env := captureEnv(t, p, source, benchConfig{PoolMaxConns: 4})
	var remote *burstRemote
	if launcher := os.Getenv("ORVEX_BURST_REMOTE_LAUNCHER"); launcher != "" {
		if os.Getenv("ORVEX_BURST_GATEWAY_HOST") == "" {
			t.Fatal("remote mode requires ORVEX_BURST_GATEWAY_HOST")
		}
		remote = startBurstRemote(t, launcher)
	}
	r := createRoundWithID(t, p, burstID(fmt.Sprintf("%s/%d/%d", arm, count, repeat)), benchMinD, benchMaxD)
	stop := startBurstMonitor(t, p, r.id)
	var mu sync.Mutex
	var wg sync.WaitGroup
	attempts := make([]burstAttempt, 0, count*3)
	fire := func(i int, kind, id string, at time.Time, body []byte) {
		wg.Add(1)
		time.AfterFunc(time.Until(at), func() {
			defer wg.Done()
			s := players[i].timedSubmit(r.text, body, at)
			a := burstAttempt{Player: names[i], ClientID: id, Kind: kind, ScheduledUS: s.Sched, SentUS: s.Sent, ReceivedUS: s.Recv, Seq: s.Sequence, EffectiveUS: s.Effective, HTTPStatus: s.Status, Error: s.Code}
			mu.Lock()
			attempts = append(attempts, a)
			mu.Unlock()
		})
	}
	created := time.UnixMicro(r.created)
	var remoteBets []remoteAttempt
	for i := range players {
		id := burstUUID(fmt.Sprintf("%s/%d/%d/bet/%d", arm, count, repeat, i))
		at := created.Add(benchBetFrom + time.Duration(i)*benchBetSpan/time.Duration(count))
		if remote == nil {
			fire(i, "bet", id, at, placeBetBody(t, id, benchBetCredits))
		} else {
			remoteBets = append(remoteBets, remoteAttempt{Player: names[i], Token: p.tokens[names[i]], ClientID: id, Kind: "bet", OffsetUS: at.Sub(created).Microseconds(), BetCredits: benchBetCredits})
		}
	}
	if remote != nil {
		attempts = append(attempts, remote.phase(t, "bets", "http://"+gw.addr, r.text, created, remoteBets)...)
	}
	deadline := created.Add(benchWindow + time.Duration(r.d)*time.Microsecond + benchStallAfter)
	start, _, ok := waitFor(t, p.obs, startedSQL, r.id, created.Add(benchWindow-50*time.Millisecond), deadline)
	if ok {
		var remoteCash []remoteAttempt
		for i := range players {
			id := burstUUID(fmt.Sprintf("%s/%d/%d/cash/%d", arm, count, repeat, i))
			at := time.UnixMicro(start).Add(benchCashFrom + time.Duration(i)*benchCashSpan/time.Duration(count))
			if arm == "deadline" {
				offset := time.Duration(0)
				if count > 1 {
					offset = -50*time.Millisecond + time.Duration(i)*100*time.Millisecond/time.Duration(count-1)
				}
				at = time.UnixMicro(start + r.d).Add(offset)
			}
			body := cashOutBody(t, id)
			if remote == nil {
				fire(i, "cashout", id, at, body)
				if arm == "retry" {
					fire(i, "retry", id, at.Add(20*time.Millisecond), body)
				}
			} else {
				item := remoteAttempt{Player: names[i], Token: p.tokens[names[i]], ClientID: id, Kind: "cashout", OffsetUS: at.Sub(time.UnixMicro(start)).Microseconds()}
				remoteCash = append(remoteCash, item)
				if arm == "retry" {
					item.Kind = "retry"
					item.OffsetUS += 20000
					remoteCash = append(remoteCash, item)
				}
			}
		}
		if remote != nil {
			attempts = append(attempts, remote.phase(t, "cash", "http://"+gw.addr, r.text, time.UnixMicro(start), remoteCash)...)
		}
	}
	wg.Wait()
	settled := false
	if ok {
		_, _, settled = waitFor(t, p.obs, settledSQL, r.id, time.Now(), deadline)
	}
	metrics := stop()
	metrics.Environment = env
	verification := reconcileBurst(t, p, r.id, attempts)
	verification.Problems = append(verification.Problems, checkPlayerWorkload(names, arm, attempts, verification)...)
	if !ok {
		verification.Problems = append(verification.Problems, "round did not start before the deadline")
	}
	if !settled {
		verification.Problems = append(verification.Problems, "round did not settle before the deadline")
	}
	writeBurstTrial(t, out, arm, count, repeat, attempts, verification, metrics)
	requireCleanStop(t, gw)
	requireCleanStop(t, engine)
	p.requireNoSecretsInOutput(seedSecret("round seed", r.seed))
	for _, problem := range verification.Problems {
		t.Error(problem)
	}
}

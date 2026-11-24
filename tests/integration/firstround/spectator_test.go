//go:build integration && acceptance && spectatorload

package firstround

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
)

// TestSpectatorLoad measures whether spectators of a round slow the
// admission of player commands, with the real engine and gateway against
// a disposable PostgreSQL (ADR-006 performance experiments). Each trial
// gets a fresh database and fresh services and plays the same seeded
// workload; only the number of spectators changes:
//
//   - 80 players each bet once while betting is open and cash out once
//     after the start, at fixed times on an open-loop schedule, so a slow
//     answer never delays the next request;
//   - spectators follow each round's stream from genesis like EventSource
//     clients; every tenth one reads the response headers and then nothing
//     until the round is over, and in every measured round the others
//     drop their connections at once during the cash-outs and resume with
//     Last-Event-ID;
//   - one probe watcher follows every round in every arm, for stream lag
//     and for its frames against the chain records.
//
// Arms are each source's spectator counts plus a repeated 0, the A/A
// control that sets the noise band. A block runs every arm once in an
// order rotated from the previous block. After one discarded warmup trial,
// every trial writes its raw receipts to ORVEX_SPECTATOR_OUT, and the
// summary there applies the PR 23 plan's rule.
//
// The engine is this source's cmd/engine built with an overlay that
// prints the pool's pgxpool.Stat every 100ms; nothing in the tree
// changes. The benchmark needs a build without -race:
//
//	ORVEX_SPECTATOR_OUT=/abs/dir/outside/the/checkout \
//	go test -tags 'integration acceptance spectatorload' -run '^TestSpectatorLoad$' \
//	    -count=1 -timeout 3h -v ./tests/integration/firstround
//
// Optional settings: ORVEX_SPECTATOR_SOURCES=label=/abs/dir,... builds and
// interleaves several checkouts, such as 11db2aa and a repaired candidate
// (default: this checkout, labeled head). The load client takes either
// gateway contract, a "pending" or "admitted" receipt and a final-cursor
// 204 or "end", and each trial counts the receipt statuses it saw.
// ORVEX_SPECTATOR_WATCHERS (default 0,100,500),
// ORVEX_SPECTATOR_BLOCKS (5), ORVEX_SPECTATOR_ROUNDS (6 measured rounds after
// one warmup round per trial) and ORVEX_SPECTATOR_POOL (pool_max_conns, 4,
// pgxpool's default on a 4-core host). Before each trial the benchmark
// waits, up to ORVEX_SPECTATOR_QUIET_WAIT_MIN minutes (60), for the host to
// be at most 15% busy, and it runs a trial again, up to three times, when
// other processes used more than half a core during it; the discarded
// runs stay in the directory as discarded-*. ORVEX_SPECTATOR_REPORT=1 only
// rebuilds the summary from the trial files in ORVEX_SPECTATOR_OUT.
func TestSpectatorLoad(t *testing.T) {
	cfg := loadBenchConfig(t)
	if os.Getenv("ORVEX_SPECTATOR_REPORT") == "" {
		if raceEnabled {
			t.Fatal("the benchmark measures release builds: run it without -race")
		}
		runExperiment(t, cfg)
	}
	writeSummary(t, cfg.Out)
}

// The fixed workload.
const (
	benchPlayers    = 80
	benchBetCredits = 100
	benchFunding    = 1_000_000
	benchWindow     = 2500 * time.Millisecond
	benchBetFrom    = 200 * time.Millisecond
	benchBetSpan    = 1800 * time.Millisecond
	benchMinD       = 3000 * time.Millisecond
	benchMaxD       = 3500 * time.Millisecond
	benchCashFrom   = 300 * time.Millisecond
	benchCashSpan   = 800 * time.Millisecond
	benchBurstAt    = 700 * time.Millisecond
	benchStallEvery = 10
	benchDrain      = 30 * time.Second
	// benchStallAfter is how far past its due settlement a round may run
	// before the trial records it as stalled and ends.
	benchStallAfter = 30 * time.Second
)

type benchSource struct {
	Label  string `json:"label"`
	Dir    string `json:"dir"`
	Commit string `json:"commit"`
	Dirty  bool   `json:"dirty"`
	bin    binaries
}

type benchConfig struct {
	Out            string        `json:"-"`
	Sources        []benchSource `json:"sources"`
	Watchers       []int         `json:"watchers"`
	Blocks         int           `json:"blocks"`
	MeasuredRounds int           `json:"measuredRounds"`
	PoolMaxConns   int           `json:"poolMaxConns"`
	Players        int           `json:"players"`
	Workload       string        `json:"workload"`
	// QuietBusy is the host busy share below which a trial starts, and
	// QuietWait how long to wait for it. MaxOtherCores is the CPU other
	// processes may use during a trial before it is run again.
	QuietBusy     float64       `json:"quietBusy"`
	QuietWait     time.Duration `json:"quietWait"`
	MaxOtherCores float64       `json:"maxOtherCores"`
}

func loadBenchConfig(t *testing.T) benchConfig {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("ORVEX_SPECTATOR_OUT")
	if out == "" || !filepath.IsAbs(out) {
		t.Fatal("set ORVEX_SPECTATOR_OUT to an absolute directory outside the checkout for the raw receipts")
	}
	if rel, err := filepath.Rel(root, out); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("ORVEX_SPECTATOR_OUT %s is inside the checkout %s; keep receipts outside the tree", out, root)
	}
	if err := os.MkdirAll(out, 0o700); err != nil { //nolint:gosec // G703: the receipt directory the operator named
		t.Fatal(err)
	}
	cfg := benchConfig{
		Out: out, Blocks: envInt(t, "ORVEX_SPECTATOR_BLOCKS", 5), MeasuredRounds: envInt(t, "ORVEX_SPECTATOR_ROUNDS", 6),
		PoolMaxConns: envInt(t, "ORVEX_SPECTATOR_POOL", 4), Players: benchPlayers,
		QuietBusy: 0.15, QuietWait: time.Duration(envInt(t, "ORVEX_SPECTATOR_QUIET_WAIT_MIN", 60)) * time.Minute, MaxOtherCores: 0.5,
		Workload: fmt.Sprintf("bets %v+[0,%v) cash-outs T0+%v+[0,%v) burst T0+%v D [%v,%v] window %v stall 1/%d",
			benchBetFrom, benchBetSpan, benchCashFrom, benchCashSpan, benchBurstAt, benchMinD, benchMaxD, benchWindow, benchStallEvery),
	}
	for _, w := range strings.Split(envString("ORVEX_SPECTATOR_WATCHERS", "0,100,500"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(w))
		if err != nil || n < 0 {
			t.Fatalf("ORVEX_SPECTATOR_WATCHERS: %q is not a count", w)
		}
		cfg.Watchers = append(cfg.Watchers, n)
	}
	if !slices.Contains(cfg.Watchers, 0) {
		t.Fatal("ORVEX_SPECTATOR_WATCHERS must include 0, the baseline of every pair")
	}
	for _, s := range strings.Split(envString("ORVEX_SPECTATOR_SOURCES", "head="+root), ",") {
		label, dir, ok := strings.Cut(s, "=")
		if !ok || label == "" || !filepath.IsAbs(dir) {
			t.Fatalf("ORVEX_SPECTATOR_SOURCES: %q is not label=/abs/dir", s)
		}
		cfg.Sources = append(cfg.Sources, benchSource{Label: label, Dir: dir})
	}
	return cfg
}

func envString(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q: want a positive integer", name, v)
	}
	return n
}

// trialSpec names one trial.
type trialSpec struct {
	Index    int    `json:"index"`
	Block    int    `json:"block"`
	Source   string `json:"source"`
	Watchers int    `json:"watchers"`
	// Arm is the spectator count, with a prime on the repeated control.
	Arm    string `json:"arm"`
	Warmup bool   `json:"warmup"`
}

// benchArms lists every arm of a block: each source's spectator counts,
// then each source's control replicate of 0.
func benchArms(cfg benchConfig) []trialSpec {
	var arms []trialSpec
	for _, s := range cfg.Sources {
		for _, w := range cfg.Watchers {
			arms = append(arms, trialSpec{Source: s.Label, Watchers: w, Arm: strconv.Itoa(w)})
		}
		arms = append(arms, trialSpec{Source: s.Label, Watchers: 0, Arm: "0'"})
	}
	return arms
}

func runExperiment(t *testing.T, cfg benchConfig) {
	for i := range cfg.Sources {
		cfg.Sources[i] = buildBenchSource(t, cfg.Sources[i])
	}
	if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(cfg.Out, "config.json"), b, 0o600)
	}
	arms := benchArms(cfg)
	plan := []trialSpec{{Source: cfg.Sources[0].Label, Watchers: slices.Max(cfg.Watchers), Arm: "warmup", Warmup: true}}
	for b := range cfg.Blocks {
		for j := range arms {
			spec := arms[(j+b)%len(arms)]
			spec.Block = b
			plan = append(plan, spec)
		}
	}
	for i, spec := range plan {
		spec.Index = i
		for attempt := 1; ; attempt++ {
			quiet := waitQuietHost(t, cfg)
			var rec trialRecord
			if !t.Run(fmt.Sprintf("%03d-%s-%s-%d", i, spec.Source, spec.Arm, attempt), func(t *testing.T) {
				rec = runTrial(t, cfg, spec)
				rec.Env.Quiet = quiet
			}) {
				t.Fatalf("trial %d (%s %s) failed; its services' output is above", i, spec.Source, spec.Arm)
			}
			other := metricsOf(rec).CPU.Other
			if other <= cfg.MaxOtherCores || attempt == benchAttempts {
				if other > cfg.MaxOtherCores {
					rec.Errors = append(rec.Errors, fmt.Sprintf("other processes used %.2f cores, above %.2f, in all %d attempts", other, cfg.MaxOtherCores, attempt))
				}
				writeTrial(t, cfg.Out, "trial", rec)
				t.Logf("trial %d %s %s: %d requests, %d errors, other CPU %.2f cores", i, spec.Source, spec.Arm, len(rec.Requests), len(rec.Errors), other)
				break
			}
			writeTrial(t, cfg.Out, fmt.Sprintf("discarded-%d", attempt), rec)
			t.Logf("trial %d %s %s attempt %d discarded: other processes used %.2f cores", i, spec.Source, spec.Arm, attempt, other)
		}
	}
}

// benchAttempts bounds the runs of one trial on a host that other work
// keeps busy.
const benchAttempts = 3

// quietHost records the host before a trial: the busy share of every CPU
// over two seconds with no service of this test running, and how long the
// benchmark waited for it to fall below ORVEX_SPECTATOR_QUIET_BUSY.
type quietHost struct {
	Busy    float64 `json:"busy"`
	WaitedS float64 `json:"waitedS"`
	Quiet   bool    `json:"quiet"`
}

func waitQuietHost(t *testing.T, cfg benchConfig) quietHost {
	t.Helper()
	start := time.Now()
	for {
		b0, t0 := hostCPU()
		time.Sleep(2 * time.Second)
		b1, t1 := hostCPU()
		q := quietHost{Busy: float64(b1-b0) / float64(max(t1-t0, 1)), WaitedS: time.Since(start).Seconds()}
		q.Quiet = q.Busy <= cfg.QuietBusy
		if q.Quiet || time.Since(start) > cfg.QuietWait {
			if !q.Quiet {
				t.Logf("host still %.0f%% busy after %v; running anyway", 100*q.Busy, cfg.QuietWait)
			}
			return q
		}
		time.Sleep(15 * time.Second)
	}
}

// requestSample is one player command, times in Unix microseconds.
type requestSample struct {
	Kind   string `json:"k"`
	Round  int    `json:"r"`
	Player int    `json:"p"`
	Sched  int64  `json:"s"`
	Sent   int64  `json:"t"`
	Recv   int64  `json:"v"`
	// Status is the HTTP status, 0 on a transport error.
	Status    int    `json:"c"`
	Code      string `json:"e,omitempty"`
	Effective int64  `json:"f,omitempty"`
	Sequence  int64  `json:"q,omitempty"`
	Receipt   string `json:"rs,omitempty"`
}

// roundSample is what the database holds about one round after it settled.
type roundSample struct {
	Index    int   `json:"index"`
	Measured bool  `json:"measured"`
	Created  int64 `json:"created"`
	D        int64 `json:"d"`
	// StartSeen is when the load generator saw the start admission.
	StartSeen   int64          `json:"startSeen"`
	Start       int64          `json:"start"`
	Close       int64          `json:"close"`
	Settle      int64          `json:"settle"`
	SettledSeen int64          `json:"settledSeen"`
	Records     int            `json:"records"`
	Admissions  int            `json:"admissions"`
	Gapless     bool           `json:"gapless"`
	Results     map[string]int `json:"results"`
	// DuplicatePayouts counts players credited more than once.
	DuplicatePayouts int `json:"duplicatePayouts"`
	CashedOut        int `json:"cashedOut"`
	PlayerCredits    int `json:"playerCredits"`
	// ExecLag and PubLag are outbox created_at and published_at minus the
	// admission's effective time, per player command record, in µs.
	ExecLag    []int64 `json:"execLag"`
	PubLag     []int64 `json:"pubLag"`
	ProbeMatch bool    `json:"probeMatch"`
	// Stalled is "start" or "settle" when the round missed that step by
	// benchStallAfter, which ends the trial.
	Stalled string `json:"stalled,omitempty"`
}

// spectatorRound is one spectator's view of one round.
type spectatorRound struct {
	Spectator   int        `json:"i"`
	Round       int        `json:"r"`
	Kind        string     `json:"k"`
	ConnectUS   int64      `json:"c"`
	ReconnectUS []int64    `json:"rc,omitempty"`
	Frames      int        `json:"n"`
	Repeats     int        `json:"d"`
	Reconnects  int        `json:"x"`
	Refused     int        `json:"rf,omitempty"`
	Bursted     bool       `json:"b,omitempty"`
	Ended       bool       `json:"e"`
	Violation   string     `json:"v,omitempty"`
	Lags        []int32    `json:"l,omitempty"`
	SettleLagUS int64      `json:"s,omitempty"`
	Types       [][]string `json:"t,omitempty"`
}

type trialRecord struct {
	Spec       trialSpec        `json:"spec"`
	Env        trialEnv         `json:"env"`
	Window     [2]int64         `json:"window"`
	Requests   []requestSample  `json:"requests"`
	Rounds     []roundSample    `json:"rounds"`
	Spectators []spectatorRound `json:"spectators"`
	Pool       []poolSample     `json:"pool"`
	PG         []pgSample       `json:"pg"`
	CPU        [2]cpuSample     `json:"cpu"`
	Xacts      [2]int64         `json:"xacts"`
	Ledger     []string         `json:"ledger"`
	Errors     []string         `json:"errors"`
}

func benchRoundID(k int) [16]byte {
	h := sha256.Sum256([]byte("orvex/spectatorload/round/" + strconv.Itoa(k)))
	var id [16]byte
	copy(id[:], h[:16])
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id
}

func runTrial(t *testing.T, cfg benchConfig, spec trialSpec) trialRecord {
	src := cfg.source(t, spec.Source)
	names := make([]string, cfg.Players)
	for i := range names {
		names[i] = fmt.Sprintf("p%02d", i)
	}
	p := newPlatformFrom(t, src.bin, names...)
	for _, n := range names {
		roundtest.FundPlayer(t, p.obs.db, n, benchFunding)
	}
	engine := p.startEngine(engineConfig{createRounds: false, bettingWindow: benchWindow, poolMaxConns: cfg.PoolMaxConns})
	gw := p.startGateway(engine.addr)
	rec := trialRecord{Spec: spec, Env: captureEnv(t, p, src, cfg)}
	players := make([]*player, len(names))
	for i, n := range names {
		players[i] = p.player(n, gw)
	}
	base := "http://" + gw.addr
	specTransport := &http.Transport{MaxIdleConnsPerHost: 2*spec.Watchers + 2}
	defer specTransport.CloseIdleConnections()
	spectators := []*spectator{{index: 0, kind: "probe", base: base, http: &http.Client{Transport: specTransport}}}
	for i := 1; i <= spec.Watchers; i++ {
		kind := "healthy"
		if i%benchStallEvery == 0 {
			kind = "stalled"
		}
		spectators = append(spectators, &spectator{index: i, kind: kind, base: base, http: &http.Client{Transport: specTransport}})
	}
	sampler := startPGSampler(t, p)
	var seeds []secret
	for k := 0; k <= cfg.MeasuredRounds; k++ {
		if k == 1 {
			rec.Window[0] = time.Now().UnixMicro()
			rec.CPU[0] = sampleCPU(p, engine, gw)
			rec.Xacts[0] = p.obs.int(`SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = current_database()`)
		}
		rs, reqs, views, errs := runRound(t, p, k, players, spectators)
		rs.Measured = k > 0
		rec.Rounds = append(rec.Rounds, rs)
		rec.Requests = append(rec.Requests, reqs...)
		rec.Spectators = append(rec.Spectators, views...)
		rec.Errors = append(rec.Errors, errs...)
		seeds = append(seeds, seedSecret(fmt.Sprintf("the seed of round %d", k), p.obs.seed(benchRoundID(k))))
		if rs.Stalled != "" {
			rec.Errors = append(rec.Errors, fmt.Sprintf("round %d stalled before its %s step; the trial ends there", k, rs.Stalled))
			break
		}
	}
	rec.Window[1] = time.Now().UnixMicro()
	rec.CPU[1] = sampleCPU(p, engine, gw)
	rec.PG = sampler.stop()
	requireCleanStop(t, gw)
	requireCleanStop(t, engine)
	rec.Xacts[1] = p.obs.int(`SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = current_database()`)
	rec.Pool = parsePoolSamples(engine.log.String())
	if len(rec.Pool) == 0 {
		rec.Errors = append(rec.Errors, "the probe engine printed no pool statistics")
	}
	rec.Ledger = ledgerProblems(p.obs)
	p.requireNoSecretsInOutput(seeds...)
	return rec
}

func (cfg benchConfig) source(t *testing.T, label string) benchSource {
	t.Helper()
	for _, s := range cfg.Sources {
		if s.Label == label {
			return s
		}
	}
	t.Fatalf("no source %s", label)
	return benchSource{}
}

// runRound plays round k and returns its database facts, the player
// commands, every spectator's view and any harness problem.
func runRound(t *testing.T, p *platform, k int, players []*player, spectators []*spectator) (roundSample, []requestSample, []spectatorRound, []string) {
	t.Helper()
	r := createRoundWithID(t, p, benchRoundID(k), benchMinD, benchMaxD)
	rs := roundSample{Index: k, Created: r.created, D: r.d}
	var problems []string

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	burst := make(chan struct{})
	views := make([]spectatorRound, len(spectators))
	var all, following sync.WaitGroup
	for i, s := range spectators {
		all.Add(1)
		if s.kind != "stalled" {
			following.Add(1)
		}
		var b <-chan struct{}
		if s.kind == "healthy" && k > 0 {
			b = burst
		}
		go func() {
			defer all.Done()
			if s.kind != "stalled" {
				defer following.Done()
			}
			views[i] = s.follow(ctx, r.text, k, b)
		}()
	}

	var mu sync.Mutex
	var reqs []requestSample
	var sent sync.WaitGroup
	fire := func(kind string, i int, at time.Time, body []byte) {
		sent.Add(1)
		time.AfterFunc(time.Until(at), func() {
			defer sent.Done()
			s := players[i].timedSubmit(r.text, body, at)
			s.Kind, s.Round, s.Player = kind, k, i
			mu.Lock()
			reqs = append(reqs, s)
			mu.Unlock()
		})
	}
	n := time.Duration(len(players))
	created := time.UnixMicro(r.created)
	for i := range players {
		fire("bet", i, created.Add(benchBetFrom+time.Duration(i)*benchBetSpan/n), placeBetBody(t, newUUID(t), benchBetCredits))
	}
	stall := created.Add(benchWindow + time.Duration(r.d)*time.Microsecond + benchStallAfter)
	t0, seen, ok := waitFor(t, p.obs, startedSQL, r.id, created.Add(benchWindow-50*time.Millisecond), stall)
	rs.Start, rs.StartSeen = t0, seen
	if ok {
		start := time.UnixMicro(t0)
		for i := range players {
			fire("cashout", i, start.Add(benchCashFrom+time.Duration(i)*benchCashSpan/n), cashOutBody(t, newUUID(t)))
		}
		if k > 0 {
			time.AfterFunc(time.Until(start.Add(benchBurstAt)), func() { close(burst) })
		}
		_, rs.SettledSeen, ok = waitFor(t, p.obs, settledSQL, r.id, start, stall)
		if !ok {
			rs.Stalled = "settle"
		}
	} else {
		rs.Stalled = "start"
	}
	sent.Wait()

	if rs.Stalled == "" {
		done := make(chan struct{})
		go func() { following.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(benchDrain):
			problems = append(problems, fmt.Sprintf("round %d: watchers still following %v after settlement", k, benchDrain))
		}
	}
	cancel()
	all.Wait()
	roundFacts(t, p.obs, r.id, &rs, views[0])
	return rs, reqs, views, problems
}

// timedSubmit sends one command once, with no retry, and records when it
// was due, sent and answered.
func (p *player) timedSubmit(round string, body []byte, due time.Time) requestSample {
	s := requestSample{Sched: due.UnixMicro()}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/v1/rounds/"+round+"/commands", bytes.NewReader(body))
	if err != nil {
		s.Code = "request: " + err.Error()
		return s
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	s.Sent = time.Now().UnixMicro()
	resp, err := p.http.Do(req)
	if err != nil {
		s.Recv, s.Code = time.Now().UnixMicro(), "transport"
		return s
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Recv, s.Status = time.Now().UnixMicro(), resp.StatusCode
	if err != nil {
		s.Code = "transport"
		return s
	}
	if resp.StatusCode != http.StatusAccepted {
		s.Code = errorCode(b)
		return s
	}
	var r receipt
	if err := json.Unmarshal(b, &r); err != nil {
		s.Code = "malformed receipt"
		return s
	}
	s.Effective, s.Sequence, s.Receipt = int64(r.EffectiveTimeUnixMicro), int64(r.Sequence), r.Status
	return s
}

// startedSQL selects the effective time T0 of the round's start command,
// and settledSQL the effective time of the command that settled it.
const (
	startedSQL = `SELECT a.effective_time_unix_micro FROM orvex.round_admission_heads h
		JOIN orvex.admissions a ON a.round_id = h.round_id AND a.sequence = h.started_sequence
		WHERE h.round_id = $1`
	settledSQL = `SELECT a.effective_time_unix_micro FROM orvex.round_secret_reveals r
		JOIN orvex.admissions a ON a.round_id = r.round_id AND a.sequence = r.sequence
		WHERE r.round_id = $1`
)

// waitFor polls query for the round every 5ms from `from` until it returns
// a row or the deadline passes. It returns the row's time, when it was
// seen, and whether it was.
func waitFor(t *testing.T, o observer, query string, round [16]byte, from, deadline time.Time) (int64, int64, bool) {
	t.Helper()
	time.Sleep(time.Until(from))
	for {
		var at int64
		err := o.db.QueryRow(context.Background(), query, round).Scan(&at)
		if err == nil {
			return at, time.Now().UnixMicro(), true
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("round %x: %v", round, err)
		}
		if time.Now().After(deadline) {
			return 0, time.Now().UnixMicro(), false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// roundFacts fills rs from the database and checks the probe watcher's
// frames against the chain records.
func roundFacts(t *testing.T, o observer, round [16]byte, rs *roundSample, probe spectatorRound) {
	t.Helper()
	ctx := context.Background()
	records := o.records(round)
	admissions := o.admissions(round)
	rs.Records, rs.Admissions = len(records), len(admissions)
	rs.Gapless = len(records) == len(admissions)+1
	for i, r := range records {
		rs.Gapless = rs.Gapless && r.Sequence == int64(i)
	}
	effective := map[int64]int64{}
	player := map[int64]bool{}
	for _, a := range admissions {
		effective[a.Sequence] = a.EffectiveTime
		switch a.Command.(type) {
		case v1.CloseRound:
			rs.Close = a.EffectiveTime
		case v1.SettleRound:
			rs.Settle = a.EffectiveTime
		case v1.PlaceBet, v1.CashOut:
			player[a.Sequence] = true
		}
	}
	rs.Results = map[string]int{}
	for _, r := range records[1:] {
		kind := strings.TrimPrefix(fmt.Sprintf("%T", r.Command), "v1.")
		result := "accepted"
		if r.Record.Result.Rejected {
			result = r.Record.Result.Reason
		}
		rs.Results[kind+" "+result]++
		for _, e := range r.Events {
			if eventType(e) == "BetCashedOut" {
				rs.CashedOut++
			}
		}
	}
	rows, err := o.db.Query(ctx, `SELECT sequence, orvex.unix_micro(min(created_at)), orvex.unix_micro(min(published_at))
		FROM orvex.outbox_messages WHERE round_id = $1 GROUP BY sequence ORDER BY sequence`, round)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq, created int64
		var published *int64
		if err := rows.Scan(&seq, &created, &published); err != nil {
			t.Fatal(err)
		}
		if !player[seq] {
			continue
		}
		rs.ExecLag = append(rs.ExecLag, created-effective[seq])
		if published != nil {
			rs.PubLag = append(rs.PubLag, *published-effective[seq])
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rs.PlayerCredits = int(o.int(`SELECT count(*) FROM orvex.ledger_postings p
		JOIN orvex.ledger_transactions x USING (transaction_id)
		JOIN orvex.ledger_accounts a ON a.account_id = p.credit_account_id
		WHERE x.round_id = $1 AND a.kind = 'player'`, round))
	rs.DuplicatePayouts = int(o.int(`SELECT count(*) FROM (SELECT a.player_id FROM orvex.ledger_postings p
		JOIN orvex.ledger_transactions x USING (transaction_id)
		JOIN orvex.ledger_accounts a ON a.account_id = p.credit_account_id
		WHERE x.round_id = $1 AND a.kind = 'player' GROUP BY a.player_id HAVING count(*) > 1) d`, round))
	rs.ProbeMatch = probe.Ended && len(probe.Types) == len(records)
	for i := 0; rs.ProbeMatch && i < len(records); i++ {
		rs.ProbeMatch = slices.Equal(probe.Types[i], records[i].types())
	}
}

// ledgerProblems runs requireSettledLedger's balance checks without
// failing the trial.
func ledgerProblems(o observer) []string {
	var out []string
	if n := o.int(`SELECT count(*) FROM orvex.ledger_accounts WHERE held <> 0`); n != 0 {
		out = append(out, fmt.Sprintf("%d accounts still hold credits", n))
	}
	if n := o.int(`SELECT coalesce(sum(credits_posted - debits_posted), 0) FROM orvex.ledger_accounts`); n != 0 {
		out = append(out, fmt.Sprintf("ledger accounts sum to %d", n))
	}
	return out
}

// spectator is one anonymous stream client.
type spectator struct {
	index int
	// kind is probe, healthy or stalled.
	kind string
	base string
	http *http.Client
}

func (s *spectator) open(ctx context.Context, round string, cursor int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/v1/rounds/"+round+"/stream", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if cursor >= 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
	}
	return s.http.Do(req)
}

// follow watches round k until its stream ends or ctx does. A stalled
// spectator reads only the headers. A healthy one drops its connection
// when burst closes and resumes at once from its cursor.
func (s *spectator) follow(ctx context.Context, round string, k int, burst <-chan struct{}) spectatorRound {
	v := spectatorRound{Spectator: s.index, Round: k, Kind: s.kind, ConnectUS: -1}
	if s.kind == "stalled" {
		start := time.Now()
		resp, err := s.open(ctx, round, -1)
		if err != nil {
			return v
		}
		v.ConnectUS = time.Since(start).Microseconds()
		<-ctx.Done()
		_ = resp.Body.Close()
		return v
	}
	cursor := int64(-1)
	for ctx.Err() == nil {
		conn, cancel := context.WithCancel(ctx)
		start := time.Now()
		resp, err := s.open(conn, round, cursor)
		if err == nil {
			if v.ConnectUS < 0 {
				v.ConnectUS = time.Since(start).Microseconds()
			} else {
				v.ReconnectUS = append(v.ReconnectUS, time.Since(start).Microseconds())
			}
		}
		var bursted atomic.Bool
		switch {
		case err != nil:
		case resp.StatusCode == http.StatusNoContent && cursor >= 0:
			_ = resp.Body.Close()
			cancel()
			v.Ended = true
			return v
		case resp.StatusCode != http.StatusOK:
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			v.Refused++
		default:
			if burst != nil {
				go func() {
					select {
					case <-burst:
						bursted.Store(true)
						cancel()
					case <-conn.Done():
					}
				}()
			}
			err = readSSE(resp.Body, func(ev sseEvent) error {
				if ev.event == "end" {
					return errEnded
				}
				now := time.Now().UnixMicro()
				f, err := parseBenchFrame(ev)
				if err != nil {
					return err
				}
				switch {
				case f.seq <= cursor:
					v.Repeats++
					return nil
				case f.seq != cursor+1:
					return violationf("gap: frame %d after %d", f.seq, cursor)
				}
				cursor = f.seq
				v.Lags = append(v.Lags, int32(now-f.effective)) //nolint:gosec // G115: lags are far below 2^31 µs
				if f.settled {
					v.SettleLagUS = now - f.effective
				}
				if s.kind == "probe" {
					v.Types = append(v.Types, f.types)
				}
				return nil
			})
			_ = resp.Body.Close()
		}
		cancel()
		v.Frames = int(cursor + 1)
		if errors.Is(err, errEnded) {
			v.Ended = true
			return v
		}
		if errors.As(err, new(violation)) {
			v.Violation = err.Error()
			return v
		}
		v.Reconnects++
		if bursted.Load() {
			burst, v.Bursted = nil, true
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(reconnectDelay):
		}
	}
	return v
}

type benchFrame struct {
	seq, effective int64
	settled        bool
	types          []string
}

func parseBenchFrame(ev sseEvent) (benchFrame, error) {
	if !ev.hasID {
		return benchFrame{}, violationf("record frame without id")
	}
	var rec struct {
		Sequence               wireInt     `json:"sequence"`
		EffectiveTimeUnixMicro wireInt     `json:"effectiveTimeUnixMicro"`
		Events                 []wireEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(ev.data), &rec); err != nil {
		return benchFrame{}, violationf("frame %s: %v", ev.id, err)
	}
	if strconv.FormatInt(int64(rec.Sequence), 10) != ev.id {
		return benchFrame{}, violationf("frame id %s carries sequence %d", ev.id, rec.Sequence)
	}
	f := benchFrame{seq: int64(rec.Sequence), effective: int64(rec.EffectiveTimeUnixMicro), types: []string{}}
	for _, e := range rec.Events {
		f.types = append(f.types, e.Type)
		f.settled = f.settled || e.Type == "RoundSettled"
	}
	return f, nil
}

func writeTrial(t *testing.T, dir, prefix string, rec trialRecord) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(rec); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%s-%03d.json.gz", prefix, rec.Spec.Index)
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

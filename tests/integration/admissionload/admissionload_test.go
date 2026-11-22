//go:build integration && admissionload

// Package admissionload is the lower-level control of the spectator
// benchmark (tests/integration/firstround, TestSpectatorLoad): the same
// seeded player workload driven straight into the admission sequencer and
// the executor over one pool of four connections, with no gateway, engine
// API, relay or subscriber. It uses only APIs that trunk (ff022fc) already
// has, so the same file measures trunk and a later head:
//
//	git worktree add /tmp/orvex-trunk ff022fc
//	cp -r tests/integration/admissionload /tmp/orvex-trunk/tests/integration/
//	(cd /tmp/orvex-trunk && go test -c -tags 'integration admissionload' -o /tmp/load-trunk ./tests/integration/admissionload)
//	go test -c -tags 'integration admissionload' -o /tmp/load-head ./tests/integration/admissionload
//	for i in 1 2 3 4 5; do
//	  for side in trunk head; do
//	    ORVEX_LOAD_OUT=/abs/out/$side-$i.json ORVEX_LOAD_LABEL=$side /tmp/load-$side -test.run '^TestAdmissionLoad$' -test.count=1
//	  done
//	done
//
// Each run starts its own PostgreSQL container, plays one warmup round and
// six measured rounds, and writes every admission's timing and the pool's
// statistics to ORVEX_LOAD_OUT.
package admissionload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest/roundtest"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// The workload of TestSpectatorLoad.
const (
	players    = 80
	rounds     = 6
	window     = 2500 * time.Millisecond
	betFrom    = 200 * time.Millisecond
	betSpan    = 1800 * time.Millisecond
	minD       = 3000 * time.Millisecond
	maxD       = 3500 * time.Millisecond
	cashFrom   = 300 * time.Millisecond
	cashSpan   = 800 * time.Millisecond
	poolConns  = 4
	drainEvery = 20 * time.Millisecond
	drainBatch = 64
)

// sample is one admission, times in Unix microseconds.
type sample struct {
	Kind      string `json:"k"`
	Round     int    `json:"r"`
	Sched     int64  `json:"s"`
	Sent      int64  `json:"t"`
	Recv      int64  `json:"v"`
	Effective int64  `json:"f,omitempty"`
	Error     string `json:"e,omitempty"`
}

type poolStat struct {
	Acquires    int64 `json:"acquires"`
	AcquireUS   int64 `json:"acquireUs"`
	Empty       int64 `json:"emptyAcquires"`
	EmptyWaitUS int64 `json:"emptyWaitUs"`
}

func statOf(p *pgxpool.Pool) poolStat {
	s := p.Stat()
	return poolStat{s.AcquireCount(), s.AcquireDuration().Microseconds(), s.EmptyAcquireCount(), s.EmptyAcquireWaitTime().Microseconds()}
}

type result struct {
	Label    string             `json:"label"`
	Go       string             `json:"go"`
	Postgres string             `json:"postgres"`
	LoadAvg  string             `json:"loadAvg"`
	Pool     [2]poolStat        `json:"pool"`
	Samples  []sample           `json:"samples"`
	Summary  map[string]float64 `json:"summary"`
}

func TestAdmissionLoad(t *testing.T) {
	out := os.Getenv("ORVEX_LOAD_OUT")
	if out == "" {
		t.Fatal("set ORVEX_LOAD_OUT to the result file")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(pgtest.MigratedDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = poolConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	seq, err := admission.New(pool, roundtest.Registry(t), admission.DefaultConfig(), admission.CrashV1Policy())
	if err != nil {
		t.Fatal(err)
	}
	x := roundtest.NewExecutor(t, pool, nil, nil)
	names := make([]v1.PlayerID, players)
	for i := range names {
		names[i] = v1.PlayerID(fmt.Sprintf("p%02d", i))
		roundtest.FundPlayer(t, pool, string(names[i]), 1_000_000)
	}
	res := result{Label: os.Getenv("ORVEX_LOAD_LABEL"), Go: runtime.Version()}
	if err := pool.QueryRow(ctx, `SHOW server_version`).Scan(&res.Postgres); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		res.LoadAvg = string(b)
	}
	for k := 0; k <= rounds; k++ {
		if k == 1 {
			res.Pool[0] = statOf(pool)
		}
		res.Samples = append(res.Samples, playRound(t, pool, seq, x, k, names)...)
	}
	res.Pool[1] = statOf(pool)
	res.Summary = summarize(res)
	b, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o600); err != nil { //nolint:gosec // G703: the result file the operator named
		t.Fatal(err)
	}
	t.Logf("%s: %v", res.Label, res.Summary)
}

func roundID(k int) [16]byte {
	h := sha256.Sum256([]byte("orvex/spectatorload/round/" + strconv.Itoa(k)))
	var id [16]byte
	copy(id[:], h[:16])
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id
}

// seedWithDeadline is roundtest.SeedWithDeadline, which trunk lacks.
func seedWithDeadline(t *testing.T, id [16]byte) (v1.Config, int64) {
	t.Helper()
	for n := uint64(1); n <= 100_000; n++ {
		var seed [32]byte
		binary.BigEndian.PutUint64(seed[24:], n)
		cfg := v1.Config{RoundID: id, Seed: seed, MinBetCredits: 1, MaxBetCredits: 1_000_000_000, MaxBets: 100}
		s, err := v1.Rules{}.InitialState(cfg)
		if err == nil && s.CrashDeadlineOffsetMicros >= minD.Microseconds() && s.CrashDeadlineOffsetMicros <= maxD.Microseconds() {
			return cfg, s.CrashDeadlineOffsetMicros
		}
	}
	t.Fatal("no seed with D in range")
	return v1.Config{}, 0
}

func randomUUID(t *testing.T) [16]byte {
	t.Helper()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id
}

func dbNow(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT orvex.unix_micro(clock_timestamp())`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// playRound plays round k like the engine would: a drain loop executes
// admissions every 20ms, the test admits the lifecycle commands when due,
// and players' commands arrive on the fixed schedule.
func playRound(t *testing.T, pool *pgxpool.Pool, seq *admission.Sequencer, x *execution.Executor, k int, names []v1.PlayerID) []sample {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := roundID(k)
	cfg, d := seedWithDeadline(t, id)
	created := dbNow(t, pool)
	if _, err := x.CreateRound(ctx, execution.NewRound{RoundID: id, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, Config: cfg, CreatedAtUnixMicro: created}); err != nil {
		t.Fatalf("CreateRound: %v", err)
	}
	lease := execution.Lease{RoundID: id}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ctx.Err() == nil {
			if _, err := x.Drain(ctx, lease, drainBatch); err != nil && ctx.Err() == nil {
				t.Errorf("Drain: %v", err)
				return
			}
			time.Sleep(drainEvery)
		}
	}()
	admit := func(cmd v1.Command) (admission.Admission, error) {
		return seq.Admit(ctx, admission.Request{RoundID: id, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber, CommandID: randomUUID(t), Command: cmd})
	}

	var mu sync.Mutex
	var out []sample
	var wg sync.WaitGroup
	fire := func(kind string, at time.Time, cmd v1.Command) {
		wg.Add(1)
		time.AfterFunc(time.Until(at), func() {
			defer wg.Done()
			s := sample{Kind: kind, Round: k, Sched: at.UnixMicro(), Sent: time.Now().UnixMicro()}
			a, err := admit(cmd)
			s.Recv = time.Now().UnixMicro()
			if err != nil {
				s.Error = err.Error()
			} else {
				s.Effective = a.EffectiveTimeUnixMicro
			}
			mu.Lock()
			out = append(out, s)
			mu.Unlock()
		})
	}
	n := time.Duration(len(names))
	start := time.UnixMicro(created)
	for i, p := range names {
		fire("bet", start.Add(betFrom+time.Duration(i)*betSpan/n), v1.PlaceBet{Authority: v1.AuthorityPlayer, Player: p, BetCredits: 100})
	}
	time.Sleep(time.Until(start.Add(window)))
	a, err := admit(v1.StartRound{Authority: v1.AuthoritySystem})
	if err != nil {
		t.Fatalf("StartRound: %v", err)
	}
	t0 := time.UnixMicro(a.EffectiveTimeUnixMicro)
	for i, p := range names {
		fire("cashout", t0.Add(cashFrom+time.Duration(i)*cashSpan/n), v1.CashOut{Authority: v1.AuthorityPlayer, Player: p})
	}
	time.Sleep(time.Until(t0.Add(time.Duration(d) * time.Microsecond)))
	for {
		_, err := admit(v1.CloseRound{Authority: v1.AuthoritySystem})
		if errors.Is(err, admission.ErrClosureNotDue) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("CloseRound: %v", err)
		}
		break
	}
	if _, err := admit(v1.SettleRound{Authority: v1.AuthoritySystem}); err != nil {
		t.Fatalf("SettleRound: %v", err)
	}
	wg.Wait()
	deadline := time.Now().Add(time.Minute)
	for {
		var revealed int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM orvex.round_secret_reveals WHERE round_id = $1`, id).Scan(&revealed); err != nil {
			t.Fatal(err)
		}
		if revealed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("round %d did not settle", k)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-drained
	if k == 0 {
		return nil
	}
	return out
}

func percentile(v []int64, q float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return float64(s[max(int(math.Ceil(q*float64(len(s))))-1, 0)]) / 1000
}

func summarize(r result) map[string]float64 {
	var all, pre []int64
	errs := 0
	for _, s := range r.Samples {
		if s.Error != "" {
			errs++
			continue
		}
		all = append(all, s.Recv-s.Sched)
		pre = append(pre, s.Effective-s.Sent)
	}
	a, b := r.Pool[0], r.Pool[1]
	return map[string]float64{
		"n": float64(len(all)), "errors": float64(errs),
		"p50": percentile(all, 0.5), "p95": percentile(all, 0.95), "p99": percentile(all, 0.99), "max": percentile(all, 1),
		"preP99":           percentile(pre, 0.99),
		"emptyAcquires":    float64(b.Empty - a.Empty),
		"emptyWaitMs":      float64(b.EmptyWaitUS-a.EmptyWaitUS) / 1000,
		"waitPerAdmission": float64(b.EmptyWaitUS-a.EmptyWaitUS) / 1000 / math.Max(float64(len(all)), 1),
	}
}

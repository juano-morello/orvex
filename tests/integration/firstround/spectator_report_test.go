//go:build integration && acceptance && spectatorload

package firstround

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// dist is a latency distribution in milliseconds, by nearest rank.
type dist struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

func distOf(us []int64) dist {
	if len(us) == 0 {
		return dist{}
	}
	s := slices.Clone(us)
	slices.Sort(s)
	rank := func(q float64) float64 {
		i := int(math.Ceil(q*float64(len(s)))) - 1
		return float64(s[max(i, 0)]) / 1000
	}
	return dist{N: len(s), P50: rank(0.50), P95: rank(0.95), P99: rank(0.99), Max: float64(s[len(s)-1]) / 1000}
}

// cores is CPU use in cores over the window.
type cores struct {
	Engine  float64 `json:"engine"`
	Gateway float64 `json:"gateway"`
	Load    float64 `json:"loadGenerator"`
	PG      float64 `json:"postgres"`
	Host    float64 `json:"hostBusy"`
	// Other is the host's busy CPU minus the four above, in cores: work
	// that was not this trial's.
	Other float64 `json:"other"`
}

// trialMetrics summarizes the measured rounds of one trial.
type trialMetrics struct {
	Index    int    `json:"index"`
	Block    int    `json:"block"`
	Source   string `json:"source"`
	Arm      string `json:"arm"`
	Watchers int    `json:"watchers"`
	Warmup   bool   `json:"warmup"`

	Admission    dist           `json:"admission"`
	Bet          dist           `json:"bet"`
	CashOut      dist           `json:"cashOut"`
	PreAdmission dist           `json:"preAdmission"`
	GeneratorLag dist           `json:"generatorLag"`
	Refused      map[string]int `json:"refused"`
	Receipts     map[string]int `json:"receipts"`
	// DeadlineMisses are cash-outs sent before T0 + D and admitted at or
	// after it; DeadlineLost are those sent before it and not admitted.
	DeadlineMisses int `json:"deadlineMisses"`
	DeadlineLost   int `json:"deadlineLost"`
	// MarginMin is the least time from an admitted cash-out's effective
	// time to its round's crash deadline, in ms: what a late cash-out has
	// left.
	MarginMin float64        `json:"cashOutMarginMinMs"`
	Results   map[string]int `json:"results"`

	PoolAcquires     int64   `json:"poolAcquires"`
	PoolEmpty        int64   `json:"poolEmptyAcquires"`
	PoolEmptyWaitMS  float64 `json:"poolEmptyWaitMs"`
	PoolWaitPerAdmMS float64 `json:"poolWaitPerAdmissionMs"`
	PoolMeanAcqMS    float64 `json:"poolMeanAcquireMs"`
	PoolCanceled     int64   `json:"poolCanceledAcquires"`
	PoolMaxAcquired  int32   `json:"poolMaxAcquired"`
	PoolMax          int32   `json:"poolMax"`
	PoolFull         float64 `json:"poolFullFraction"`
	PGFull           float64 `json:"pgBusyFullFraction"`
	PGMaxBackends    int     `json:"pgMaxBackends"`

	ExecLag   dist `json:"execLag"`
	PubLag    dist `json:"pubLag"`
	StreamLag dist `json:"streamLag"`
	ProbeLag  dist `json:"probeLag"`
	SettleLag dist `json:"settleLag"`
	Connect   dist `json:"connect"`
	Reconnect dist `json:"reconnect"`

	Views      int `json:"views"`
	Incomplete int `json:"incomplete"`
	// Healthy counts the healthy spectators' views of measured rounds and
	// HealthyDone those that reached the round's end with every record.
	Healthy       int     `json:"healthyViews"`
	HealthyDone   int     `json:"healthyComplete"`
	Refusals      int     `json:"refusals"`
	Violations    int     `json:"violations"`
	Repeats       int     `json:"repeats"`
	StalledOpen   int     `json:"stalledOpen"`
	Frames        int64   `json:"frames"`
	XactsPerFrame float64 `json:"xactsPerFrame"`
	StartLateMax  float64 `json:"startLateMaxMs"`
	CloseLateMax  float64 `json:"closeLateMaxMs"`
	StartSeenMax  float64 `json:"startSeenLagMaxMs"`

	// Stalled counts rounds, the warmup round too, that missed a step by
	// benchStallAfter; Completed counts measured rounds that settled.
	Stalled          int      `json:"stalled"`
	Completed        int      `json:"completed"`
	DuplicatePayouts int      `json:"duplicatePayouts"`
	MissingSequences int      `json:"missingSequences"`
	PayoutMismatches int      `json:"payoutMismatches"`
	ProbeMismatches  int      `json:"probeMismatches"`
	Ledger           []string `json:"ledger"`
	Errors           []string `json:"errors"`
	CPU              cores    `json:"cpu"`
	Env              trialEnv `json:"env"`
}

func loadTrials(t *testing.T, dir string) []trialRecord {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "trial-*.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var out []trialRecord
	for _, f := range files {
		fh, err := os.Open(f) //nolint:gosec // G304: the receipts this benchmark wrote
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(fh)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var rec trialRecord
		if err := json.NewDecoder(zr).Decode(&rec); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		_ = fh.Close()
		out = append(out, rec)
	}
	if len(out) == 0 {
		t.Fatalf("no trial files in %s", dir)
	}
	return out
}

// poolAt returns the last probe sample at or before ts, else the first.
func poolAt(s []poolSample, ts int64) poolSample {
	i := sort.Search(len(s), func(i int) bool { return s[i].TS > ts })
	return s[max(i-1, 0)]
}

func metricsOf(rec trialRecord) trialMetrics {
	m := trialMetrics{Index: rec.Spec.Index, Block: rec.Spec.Block, Source: rec.Spec.Source, Arm: rec.Spec.Arm,
		Watchers: rec.Spec.Watchers, Warmup: rec.Spec.Warmup, Refused: map[string]int{}, Receipts: map[string]int{},
		Results: map[string]int{}, Ledger: rec.Ledger, Errors: rec.Errors, Env: rec.Env}
	rounds := map[int]roundSample{}
	for _, r := range rec.Rounds {
		rounds[r.Index] = r
		if r.Stalled != "" {
			m.Stalled++
			m.StartLateMax = max(m.StartLateMax, float64(r.StartSeen-r.Created-benchWindow.Microseconds())/1000)
			continue
		}
		if !r.Measured {
			continue
		}
		m.Completed++
		for k, v := range r.Results {
			m.Results[k] += v
		}
		if !r.Gapless {
			m.MissingSequences++
		}
		m.DuplicatePayouts += r.DuplicatePayouts
		if r.CashedOut != r.PlayerCredits {
			m.PayoutMismatches++
		}
		if !r.ProbeMatch {
			m.ProbeMismatches++
		}
		m.StartLateMax = max(m.StartLateMax, float64(r.Start-r.Created-benchWindow.Microseconds())/1000)
		m.CloseLateMax = max(m.CloseLateMax, float64(r.Close-r.Start-r.D)/1000)
		m.StartSeenMax = max(m.StartSeenMax, float64(r.StartSeen-r.Start)/1000)
	}
	var adm, bet, cash, pre, gen, execLag, pubLag []int64
	m.MarginMin = math.Inf(1)
	for _, s := range rec.Requests {
		r := rounds[s.Round]
		if !r.Measured {
			continue
		}
		gen = append(gen, s.Sent-s.Sched)
		deadline := r.Start + r.D
		if s.Status != 202 {
			code := s.Code
			if s.Status != 0 {
				code = strconv.Itoa(s.Status) + " " + s.Code
			}
			m.Refused[code]++
			if s.Kind == "cashout" && s.Sent < deadline {
				m.DeadlineLost++
			}
			continue
		}
		m.Receipts[s.Receipt]++
		l := s.Recv - s.Sched
		adm = append(adm, l)
		pre = append(pre, s.Effective-s.Sent)
		if s.Kind == "bet" {
			bet = append(bet, l)
		} else {
			cash = append(cash, l)
			if s.Sent < deadline && s.Effective >= deadline {
				m.DeadlineMisses++
			}
			m.MarginMin = min(m.MarginMin, float64(deadline-s.Effective)/1000)
		}
	}
	for _, r := range rec.Rounds {
		if r.Measured {
			execLag = append(execLag, r.ExecLag...)
			pubLag = append(pubLag, r.PubLag...)
		}
	}
	if math.IsInf(m.MarginMin, 1) {
		m.MarginMin = 0
	}
	m.Admission, m.Bet, m.CashOut, m.PreAdmission, m.GeneratorLag = distOf(adm), distOf(bet), distOf(cash), distOf(pre), distOf(gen)
	m.ExecLag, m.PubLag = distOf(execLag), distOf(pubLag)

	var lags, probe, settle, connect, reconnect []int64
	for _, v := range rec.Spectators {
		r := rounds[v.Round]
		if !r.Measured || r.Stalled != "" {
			continue
		}
		if v.Kind == "stalled" {
			if v.ConnectUS >= 0 {
				m.StalledOpen++
			}
			continue
		}
		m.Views++
		complete := v.Ended && v.Frames == r.Records
		if !complete {
			m.Incomplete++
		}
		m.Refusals += v.Refused
		if v.Kind == "healthy" {
			m.Healthy++
			if complete {
				m.HealthyDone++
			}
		}
		if v.Violation != "" {
			m.Violations++
		}
		m.Repeats += v.Repeats
		m.Frames += int64(v.Frames)
		for _, l := range v.Lags {
			lags = append(lags, int64(l))
			if v.Kind == "probe" {
				probe = append(probe, int64(l))
			}
		}
		if v.SettleLagUS > 0 {
			settle = append(settle, v.SettleLagUS)
		}
		if v.ConnectUS >= 0 {
			connect = append(connect, v.ConnectUS)
		}
		reconnect = append(reconnect, v.ReconnectUS...)
	}
	m.StreamLag, m.ProbeLag, m.SettleLag, m.Connect, m.Reconnect = distOf(lags), distOf(probe), distOf(settle), distOf(connect), distOf(reconnect)
	if m.Frames > 0 {
		m.XactsPerFrame = float64(rec.Xacts[1]-rec.Xacts[0]) / float64(m.Frames)
	}

	if len(rec.Pool) > 0 {
		a, b := poolAt(rec.Pool, rec.Window[0]), poolAt(rec.Pool, rec.Window[1])
		m.PoolAcquires, m.PoolEmpty, m.PoolCanceled = b.Acquires-a.Acquires, b.Empty-a.Empty, b.Canceled-a.Canceled
		m.PoolEmptyWaitMS = float64(b.EmptyWaitUS-a.EmptyWaitUS) / 1000
		if m.PoolAcquires > 0 {
			m.PoolMeanAcqMS = float64(b.AcquireUS-a.AcquireUS) / 1000 / float64(m.PoolAcquires)
		}
		if len(adm) > 0 {
			m.PoolWaitPerAdmMS = m.PoolEmptyWaitMS / float64(len(adm))
		}
		full, n := 0, 0
		for _, s := range rec.Pool {
			if s.TS < rec.Window[0] || s.TS > rec.Window[1] {
				continue
			}
			n++
			m.PoolMaxAcquired, m.PoolMax = max(m.PoolMaxAcquired, s.Acquired), s.Max
			if s.Acquired >= s.Max {
				full++
			}
		}
		if n > 0 {
			m.PoolFull = float64(full) / float64(n)
		}
	}
	full, n := 0, 0
	for _, s := range rec.PG {
		m.PGMaxBackends = max(m.PGMaxBackends, s.Total)
		if s.TS < rec.Window[0] || s.TS > rec.Window[1] {
			continue
		}
		n++
		if s.Active+s.IdleTx >= rec.Env.PoolMaxConns {
			full++
		}
	}
	if n > 0 {
		m.PGFull = float64(full) / float64(n)
	}
	a, b := rec.CPU[0], rec.CPU[1]
	if wall := float64(b.TS - a.TS); wall > 0 {
		m.CPU = cores{Engine: float64(b.Engine-a.Engine) / wall, Gateway: float64(b.Gateway-a.Gateway) / wall,
			Load: float64(b.Self-a.Self) / wall, PG: -1}
		if a.PG >= 0 && b.PG >= 0 {
			m.CPU.PG = float64(b.PG-a.PG) / wall
		}
		if b.HostTotal > a.HostTotal {
			m.CPU.Host = float64(b.HostBusy-a.HostBusy) / float64(b.HostTotal-a.HostTotal)
			m.CPU.Other = m.CPU.Host*float64(rec.Env.CPUs) - m.CPU.Engine - m.CPU.Gateway - m.CPU.Load - max(m.CPU.PG, 0)
		}
	}
	return m
}

// pairRow is one block's comparison of an arm against that block's 0.
type pairRow struct {
	Block    int     `json:"block"`
	BaseP99  float64 `json:"baseP99"`
	CtrlP99  float64 `json:"controlP99"`
	ArmP99   float64 `json:"armP99"`
	Delta    float64 `json:"delta"`
	BaseWait float64 `json:"baseWait"`
	ArmWait  float64 `json:"armWait"`
	Exceeds  bool    `json:"exceeds"`
	Rises    bool    `json:"poolRises"`
	// ArmStalled: a round of the arm's trial stalled, which counts as
	// beyond the band whatever its p99.
	ArmStalled bool `json:"armStalled"`
}

// armDecision applies the plan's rule to one source's spectator count: a
// p99 increase beyond the A/A band in at least four of five pairs, with
// pool waits rising beyond their own A/A band, confirms contention.
type armDecision struct {
	Source    string    `json:"source"`
	Watchers  int       `json:"watchers"`
	Band      float64   `json:"bandMs"`
	PoolBand  float64   `json:"poolBandMs"`
	Pairs     []pairRow `json:"pairs"`
	Count     int       `json:"count"`
	Need      int       `json:"need"`
	Confirmed bool      `json:"confirmed"`
}

func decide(ms []trialMetrics, source string, watchers []int) []armDecision {
	byArm := map[string]map[int]trialMetrics{}
	for _, m := range ms {
		if m.Warmup || m.Source != source {
			continue
		}
		if byArm[m.Arm] == nil {
			byArm[m.Arm] = map[int]trialMetrics{}
		}
		byArm[m.Arm][m.Block] = m
	}
	var band, poolBand float64
	for b, base := range byArm["0"] {
		if ctrl, ok := byArm["0'"][b]; ok {
			band = max(band, math.Abs(ctrl.Admission.P99-base.Admission.P99))
			poolBand = max(poolBand, math.Abs(ctrl.PoolWaitPerAdmMS-base.PoolWaitPerAdmMS))
		}
	}
	var out []armDecision
	for _, w := range watchers {
		if w == 0 {
			continue
		}
		d := armDecision{Source: source, Watchers: w, Band: band, PoolBand: poolBand}
		for b, base := range byArm["0"] {
			arm, ok := byArm[strconv.Itoa(w)][b]
			if !ok {
				continue
			}
			r := pairRow{Block: b, BaseP99: base.Admission.P99, CtrlP99: byArm["0'"][b].Admission.P99, ArmP99: arm.Admission.P99,
				BaseWait: base.PoolWaitPerAdmMS, ArmWait: arm.PoolWaitPerAdmMS}
			r.Delta = r.ArmP99 - r.BaseP99
			r.ArmStalled = arm.Stalled > 0 && base.Stalled == 0
			r.Exceeds = r.Delta > band || r.ArmStalled
			r.Rises = r.ArmWait > r.BaseWait+poolBand
			if r.Exceeds && r.Rises {
				d.Count++
			}
			d.Pairs = append(d.Pairs, r)
		}
		sort.Slice(d.Pairs, func(i, j int) bool { return d.Pairs[i].Block < d.Pairs[j].Block })
		d.Need = int(math.Ceil(0.8 * float64(len(d.Pairs))))
		d.Confirmed = len(d.Pairs) > 0 && d.Count >= d.Need
		out = append(out, d)
	}
	return out
}

// spread is the median and range of v.
func spread(v []float64) string {
	if len(v) == 0 {
		return "n/a"
	}
	s := slices.Clone(v)
	slices.Sort(s)
	med := s[len(s)/2]
	if len(s)%2 == 0 {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return fmt.Sprintf("%.1f [%.1f–%.1f]", med, s[0], s[len(s)-1])
}

type summary struct {
	Config    json.RawMessage `json:"config"`
	Trials    []trialMetrics  `json:"trials"`
	Decisions []armDecision   `json:"decisions"`
}

func writeSummary(t *testing.T, dir string) {
	t.Helper()
	recs := loadTrials(t, dir)
	var ms []trialMetrics
	for _, r := range recs {
		ms = append(ms, metricsOf(r))
	}
	sum := summary{Trials: ms}
	sum.Config, _ = os.ReadFile(filepath.Join(dir, "config.json")) //nolint:gosec // G304: written by this benchmark
	var sources []string
	watchers := map[int]bool{}
	for _, m := range ms {
		if !slices.Contains(sources, m.Source) {
			sources = append(sources, m.Source)
		}
		watchers[m.Watchers] = true
	}
	var ws []int
	for w := range watchers {
		ws = append(ws, w)
	}
	slices.Sort(ws)

	var md strings.Builder
	fmt.Fprintf(&md, "# Spectator load\n\nCells are the median across blocks [min–max]. Latencies in ms, nearest-rank percentiles over the measured rounds of each trial. Warmup trials are excluded. A round that misses its start or settlement by 30 s stalls and ends its trial; latencies then cover the requests made until then, and a stalled arm counts as beyond the band.\n")
	for _, src := range sources {
		fmt.Fprintf(&md, "\n## %s\n\n", src)
		fmt.Fprintf(&md, "| arm | trials | adm p50 | adm p95 | adm p99 | bet p99 | cash-out p99 | pre-adm p99 | gen lag p99 | pool wait, all acquirers, per adm | empty acquires | pool full | stream p99 | settle-lag p99 | exec p99 | pub p99 | reconnect p99 | CPU eng/gw/load/pg | host busy | other cores | stalled rounds | start late max |\n")
		fmt.Fprintf(&md, "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		arms := []string{}
		for _, w := range ws {
			arms = append(arms, strconv.Itoa(w))
		}
		arms = append(arms, "0'")
		for _, arm := range arms {
			var sel []trialMetrics
			for _, m := range ms {
				if !m.Warmup && m.Source == src && m.Arm == arm {
					sel = append(sel, m)
				}
			}
			if len(sel) == 0 {
				continue
			}
			col := func(f func(trialMetrics) float64) string {
				var v []float64
				for _, m := range sel {
					v = append(v, f(m))
				}
				return spread(v)
			}
			fmt.Fprintf(&md, "| %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s/%s/%s/%s | %s | %s | %s | %s |\n", arm, len(sel),
				col(func(m trialMetrics) float64 { return m.Admission.P50 }),
				col(func(m trialMetrics) float64 { return m.Admission.P95 }),
				col(func(m trialMetrics) float64 { return m.Admission.P99 }),
				col(func(m trialMetrics) float64 { return m.Bet.P99 }),
				col(func(m trialMetrics) float64 { return m.CashOut.P99 }),
				col(func(m trialMetrics) float64 { return m.PreAdmission.P99 }),
				col(func(m trialMetrics) float64 { return m.GeneratorLag.P99 }),
				col(func(m trialMetrics) float64 { return m.PoolWaitPerAdmMS }),
				col(func(m trialMetrics) float64 { return float64(m.PoolEmpty) }),
				col(func(m trialMetrics) float64 { return m.PoolFull }),
				col(func(m trialMetrics) float64 { return m.StreamLag.P99 }),
				col(func(m trialMetrics) float64 { return m.SettleLag.P99 }),
				col(func(m trialMetrics) float64 { return m.ExecLag.P99 }),
				col(func(m trialMetrics) float64 { return m.PubLag.P99 }),
				col(func(m trialMetrics) float64 { return m.Reconnect.P99 }),
				col(func(m trialMetrics) float64 { return m.CPU.Engine }),
				col(func(m trialMetrics) float64 { return m.CPU.Gateway }),
				col(func(m trialMetrics) float64 { return m.CPU.Load }),
				col(func(m trialMetrics) float64 { return m.CPU.PG }),
				col(func(m trialMetrics) float64 { return m.CPU.Host }),
				col(func(m trialMetrics) float64 { return m.CPU.Other }),
				col(func(m trialMetrics) float64 { return float64(m.Stalled) }),
				col(func(m trialMetrics) float64 { return m.StartLateMax }))
		}
		fmt.Fprintf(&md, "\nStreams and the pool, per trial: healthy views of measured rounds that reached the end with every record, refused stream responses (any status but 200 and 204), time to stream headers, the least cash-out margin to the crash deadline, and the engine pool.\n\n")
		fmt.Fprintf(&md, "| arm | healthy complete / views | refused | connect p99 | reconnect p99 | cash-out margin min | pool acquires | mean acquire | empty-pool wait total | pool max |\n|---|---|---|---|---|---|---|---|---|---|\n")
		for _, arm := range arms {
			var sel []trialMetrics
			for _, m := range ms {
				if !m.Warmup && m.Source == src && m.Arm == arm {
					sel = append(sel, m)
				}
			}
			if len(sel) == 0 {
				continue
			}
			col := func(f func(trialMetrics) float64) string {
				var v []float64
				for _, m := range sel {
					v = append(v, f(m))
				}
				return spread(v)
			}
			fmt.Fprintf(&md, "| %s | %s / %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", arm,
				col(func(m trialMetrics) float64 { return float64(m.HealthyDone) }),
				col(func(m trialMetrics) float64 { return float64(m.Healthy) }),
				col(func(m trialMetrics) float64 { return float64(m.Refusals) }),
				col(func(m trialMetrics) float64 { return m.Connect.P99 }),
				col(func(m trialMetrics) float64 { return m.Reconnect.P99 }),
				col(func(m trialMetrics) float64 { return m.MarginMin }),
				col(func(m trialMetrics) float64 { return float64(m.PoolAcquires) }),
				col(func(m trialMetrics) float64 { return m.PoolMeanAcqMS }),
				col(func(m trialMetrics) float64 { return m.PoolEmptyWaitMS }),
				col(func(m trialMetrics) float64 { return float64(m.PoolMax) }))
		}
		decisions := decide(ms, src, ws)
		sum.Decisions = append(sum.Decisions, decisions...)
		for _, d := range decisions {
			fmt.Fprintf(&md, "\n### %s spectators against 0\n\nA/A band: p99 %.1f ms, pool wait of all acquirers per player admission %.2f ms.\n\n", strconv.Itoa(d.Watchers), d.Band, d.PoolBand)
			fmt.Fprintf(&md, "| block | 0 p99 | 0' p99 | %d p99 | Δ p99 | %d stalled | beyond band | 0 pool wait/adm | %d pool wait/adm | pool rises |\n|---|---|---|---|---|---|---|---|---|---|\n", d.Watchers, d.Watchers, d.Watchers)
			for _, r := range d.Pairs {
				fmt.Fprintf(&md, "| %d | %.1f | %.1f | %.1f | %+.1f | %v | %v | %.2f | %.2f | %v |\n", r.Block, r.BaseP99, r.CtrlP99, r.ArmP99, r.Delta, r.ArmStalled, r.Exceeds, r.BaseWait, r.ArmWait, r.Rises)
			}
			verdict := "not confirmed"
			if d.Confirmed {
				verdict = "confirmed contention: mitigation required, then a rerun"
			}
			fmt.Fprintf(&md, "\nRule: %d of %d pairs beyond the band with rising pool waits, %d needed. Verdict: %s.\n", d.Count, len(d.Pairs), d.Need, verdict)
		}
	}
	fmt.Fprintf(&md, "\n## Correctness and load\n\n| trial | source | arm | adm n | refused | receipts | deadline misses/lost | dup payouts | missing seq | payout mismatch | probe mismatch | incomplete/views | violations | stalled open | ledger | errors | containers | quiet busy | other cores |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, m := range ms {
		fmt.Fprintf(&md, "| %d%s | %s | %s | %d | %v | %v | %d/%d | %d | %d | %d | %d | %d/%d | %d | %d | %d | %d | %d | %.2f | %.2f |\n", m.Index, map[bool]string{true: " (warmup)"}[m.Warmup],
			m.Source, m.Arm, m.Admission.N, m.Refused, m.Receipts, m.DeadlineMisses, m.DeadlineLost, m.DuplicatePayouts, m.MissingSequences,
			m.PayoutMismatches, m.ProbeMismatches, m.Incomplete, m.Views, m.Violations, m.StalledOpen, len(m.Ledger), len(m.Errors), m.Env.Containers, m.Env.Quiet.Busy, m.CPU.Other)
	}
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.md"), []byte(md.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("summary in %s:\n%s", dir, md.String())
}

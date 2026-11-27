//go:build integration && acceptance && spectatorload

package firstround

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type burstSample struct {
	TimeUS    int64 `json:"timeUs"`
	Admitted  int64 `json:"admitted"`
	Processed int64 `json:"processed"`
	LockWaits int   `json:"lockWaits"`
	WALBytes  int64 `json:"walBytes"`
}

type burstMetrics struct {
	Environment trialEnv        `json:"environment"`
	StartedUS   int64           `json:"startedUs"`
	StoppedUS   int64           `json:"stoppedUs"`
	Samples     []burstSample   `json:"samples"`
	Pool        []poolSample    `json:"pool"`
	Terminal    map[int64]int64 `json:"firstObservedTerminalUs"`
	Errors      []string        `json:"errors"`
}

// startBurstMonitor samples on a dedicated connection, outside the engine pool.
// Stop it after drain to capture both the peak and the recovery.
func startBurstMonitor(t *testing.T, p *platform, roundID [16]byte) func() burstMetrics {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), p.ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	m := burstMetrics{StartedUS: time.Now().UnixMicro(), Terminal: make(map[int64]int64)}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = conn.Close(context.Background()) }()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		finished := false
		var previous int64
		for {
			var s burstSample
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := conn.QueryRow(ctx, `SELECT orvex.unix_micro(clock_timestamp()), h.admitted_sequence, e.processed_sequence,
				(SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()),
				pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::bigint
				FROM orvex.round_admission_heads h JOIN orvex.round_execution_heads e USING (round_id)
				WHERE h.round_id = $1`, roundID).Scan(&s.TimeUS, &s.Admitted, &s.Processed, &s.LockWaits, &s.WALBytes)
			cancel()
			if err != nil {
				m.Errors = append(m.Errors, err.Error())
			} else {
				for seq := previous + 1; seq <= s.Processed; seq++ {
					m.Terminal[seq] = s.TimeUS
				}
				previous = s.Processed
				m.Samples = append(m.Samples, s)
			}
			if finished {
				return
			}
			select {
			case <-stop:
				finished = true
			case <-tick.C:
			}
		}
	}()
	stopMonitor := sync.OnceFunc(func() { close(stop); <-done })
	t.Cleanup(stopMonitor)
	return func() burstMetrics {
		stopMonitor()
		m.StoppedUS = time.Now().UnixMicro()
		for _, s := range p.services {
			if s.name == "engine" {
				m.Pool = append(m.Pool, parsePoolSamples(s.log.String())...)
			}
		}
		return m
	}
}

type burstTrial struct {
	Arm          string            `json:"arm"`
	Players      int               `json:"players"`
	Repeat       int               `json:"repeat"`
	Attempts     []burstAttempt    `json:"attempts"`
	Verification burstVerification `json:"verification"`
	Metrics      burstMetrics      `json:"metrics"`
	Config       string            `json:"config"`
}

// writeBurstTrial retains each raw trial and rebuilds the aggregate from all
// trial files. Existing results are never overwritten by a repeated run.
func writeBurstTrial(t *testing.T, dir, arm string, players, repeat int, attempts []burstAttempt, verification burstVerification, metrics burstMetrics) {
	t.Helper()
	r := burstTrial{Arm: arm, Players: players, Repeat: repeat, Attempts: attempts, Verification: verification, Metrics: metrics,
		Config: "levels=" + envString("ORVEX_BURST_LEVELS", "10,40,80") + " repeats=" + envString("ORVEX_BURST_REPEATS", "3")}
	f, err := os.CreateTemp(dir, "burst-trial-*.json") //nolint:gosec // G703: unique file under the operator-selected output directory
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	writeBurstReport(t, dir)
}

func writeBurstReport(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "burst-trial-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var b strings.Builder
	b.WriteString("# Burst trials\n\nEach row is one retained trial. Percentiles use nearest rank, in milliseconds. Backlog is admitted minus processed; drain is the first observed zero after the sampled peak. WAL is the cluster-wide LSN difference, not per-round bytes. Pool fullness is the fraction of probe samples at maximum acquired connections. Missing samples are unmeasured.\n\nPredeclared descriptive hypotheses: each repeat has generator p99 at most 10 ms and successful acknowledgement p99 at most 100 ms for each kind, all requests resolved and backlog drained within 1 s of last send, a valid audit, and complete measurements. Every configured repeat must pass for a level and arm to pass. Missing data is inconclusive. Expected deadline rejections are not failures.\n\n")
	b.WriteString("| trial | arm | players | repeat | attempts | failures | verification problems | peak backlog | drain ms | peak lock waits | WAL bytes | pool full |\n|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	var details, bounds strings.Builder
	repeats := envInt(t, "ORVEX_BURST_REPEATS", 3)
	trials := map[string]map[int]string{}
	for _, path := range files {
		data, err := os.ReadFile(path) //nolint:gosec // G304,G703: files matched by the fixed trial pattern in the experiment directory
		if err != nil {
			t.Fatal(err)
		}
		var r burstTrial
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		status := describeBurstTrial(&b, &details, filepath.Base(path), r)
		key := fmt.Sprintf("%s/%d", r.Arm, r.Players)
		if trials[key] == nil {
			trials[key] = map[int]string{}
		}
		if _, duplicate := trials[key][r.Repeat]; duplicate || r.Repeat < 0 || r.Repeat >= repeats {
			t.Fatalf("invalid or duplicate repeat %d for %s", r.Repeat, key)
		}
		trials[key][r.Repeat] = status
	}
	b.WriteString("\n## Descriptive bracket\n\n| arm/players | result | repeats observed/configured |\n|---|---|---|\n")
	for _, arm := range strings.Split(envString("ORVEX_BURST_ARMS", "normal,deadline,retry"), ",") {
		highestPassing, lowestFailing := 0, 0
		for _, level := range strings.Split(envString("ORVEX_BURST_LEVELS", "10,40,80"), ",") {
			key := arm + "/" + strings.TrimSpace(level)
			results := trials[key]
			result := "pass"
			if len(results) != repeats {
				result = "inconclusive"
			}
			for _, x := range results {
				if x == "inconclusive" {
					result = "inconclusive"
				}
			}
			for _, x := range results {
				if x == "fail" {
					result = "fail"
				}
			}
			players, _ := strconv.Atoi(strings.TrimSpace(level))
			if result == "pass" {
				highestPassing = max(highestPassing, players)
			}
			if result == "fail" && (lowestFailing == 0 || players < lowestFailing) {
				lowestFailing = players
			}
			fmt.Fprintf(&b, "| %s | %s | %d/%d |\n", key, result, len(results), repeats)
		}
		describeLevel := func(n int) string {
			if n == 0 {
				return "none observed"
			}
			return strconv.Itoa(n)
		}
		fmt.Fprintf(&bounds, "\n%s (players per round): highest tested pass %s, lowest tested failure %s.\n", arm, describeLevel(highestPassing), describeLevel(lowestFailing))
	}
	b.WriteString(bounds.String())
	b.WriteString(details.String())
	if err := os.WriteFile(filepath.Join(dir, "burst-report.md"), []byte(b.String()), 0o600); err != nil { //nolint:gosec // G703: the run owns this output directory
		t.Fatal(err)
	}
}

func burstDist(d dist) string {
	if d.N == 0 {
		return "unmeasured"
	}
	return fmt.Sprintf("%.1f/%.1f/%.1f/%.1f ms (p50/p95/p99/max, n=%d)", d.P50, d.P95, d.P99, d.Max, d.N)
}

func describeBurstTrial(summary, details *strings.Builder, filename string, r burstTrial) string {
	fail, peak, locks, peakIdx := 0, int64(0), 0, -1
	byKind := map[string][]burstAttempt{}
	for _, a := range r.Attempts {
		byKind[a.Kind] = append(byKind[a.Kind], a)
		acknowledged := a.HTTPStatus == 202 && a.Error == ""
		expectedRejection := r.Arm == "deadline" && a.Kind == "cashout" && a.HTTPStatus == 409 && (a.Error == "round_closed" || a.Error == "round_sealed")
		if !acknowledged && !expectedRejection {
			fail++
		}
	}
	for i, s := range r.Metrics.Samples {
		if s.Admitted-s.Processed > peak {
			peak = s.Admitted - s.Processed
			peakIdx = i
		}
		locks = max(locks, s.LockWaits)
	}
	drain, wal := "n/a", "n/a"
	ss := r.Metrics.Samples
	if len(ss) > 1 {
		wal = fmt.Sprint(ss[len(ss)-1].WALBytes - ss[0].WALBytes)
	}
	if peakIdx >= 0 {
		for _, s := range ss[peakIdx+1:] {
			if s.Admitted == s.Processed {
				drain = fmt.Sprintf("%.1f", float64(s.TimeUS-ss[peakIdx].TimeUS)/1000)
				break
			}
		}
	}
	full, count := 0, 0
	for _, s := range r.Metrics.Pool {
		if s.TS >= r.Metrics.StartedUS && s.TS <= r.Metrics.StoppedUS {
			count++
			if s.Max > 0 && s.Acquired >= s.Max {
				full++
			}
		}
	}
	pool := "n/a"
	if count > 0 {
		pool = fmt.Sprintf("%.1f%%", 100*float64(full)/float64(count))
	}
	fmt.Fprintf(summary, "| %s | %s | %d | %d | %d | %d | %d | %d | %s | %d | %s | %s |\n", filename, strings.ReplaceAll(r.Arm, "|", "\\|"), r.Players, r.Repeat, len(r.Attempts), fail, len(r.Verification.Problems), peak, drain, locks, wal, pool)
	e := r.Metrics.Environment
	fmt.Fprintf(details, "\n## %s, %d players, repeat %d\n\nGo %s, PostgreSQL %s, kernel %s, %d CPUs, engine pool %d. Commit `%s` (dirty %t). %s.\n", r.Arm, r.Players, r.Repeat, e.GoTest, e.Postgres, e.Kernel, e.CPUs, e.PoolMaxConns, e.Commit, e.Dirty, r.Config)
	fmt.Fprintf(details, "\nAudit verified %t. %d accepted and %d rejected terminal records including lifecycle. %d same-ID retries.\n", r.Verification.AuditOK, r.Verification.Accepted, r.Verification.Rejected, r.Verification.Retries)
	incomplete, missed := false, false

	expectedRetries := 0
	if r.Arm == "retry" {
		expectedRetries = r.Players
	}
	if len(byKind["bet"]) != r.Players || len(byKind["cashout"]) != r.Players || len(byKind["retry"]) != expectedRetries {
		missed = true
		fmt.Fprintln(details, "\nScheduled workload does not match the expected attempt counts.")
	}
	if len(ss) < 2 || len(r.Metrics.Errors) > 0 || count < 2 || len(r.Attempts) == 0 {
		incomplete = true
	}
	var lastSend, lastRecv, lastSequence int64
	for _, a := range r.Attempts {
		lastSequence = max(lastSequence, a.Seq)
		lastSend = max(lastSend, a.SentUS)
		lastRecv = max(lastRecv, a.ReceivedUS)
		if a.SentUS == 0 || a.ReceivedUS == 0 {
			incomplete = true
		}
	}
	drainAt := int64(0)
	for _, sample := range ss {
		hostTime := sample.TimeUS - e.ClockSkewUS
		if hostTime >= lastRecv && sample.Processed >= lastSequence && sample.Admitted == sample.Processed {
			drainAt = hostTime
			break
		}
	}
	if drainAt == 0 || lastSend == 0 {
		incomplete = true
	} else if max(drainAt, lastRecv)-lastSend > 1_000_000 {
		missed = true
	}
	if drainAt != 0 {
		fmt.Fprintf(details, "\nLast send to observed drain %.1f ms.\n", float64(drainAt-lastSend)/1000)
	}
	if count >= 2 {
		var first, last poolSample
		found := false
		for _, p := range r.Metrics.Pool {
			if p.TS >= r.Metrics.StartedUS && p.TS <= r.Metrics.StoppedUS {
				if !found {
					first = p
					found = true
				}
				last = p
			}
		}
		fmt.Fprintf(details, "\nPool deltas: acquires %d; empty acquires %d; empty wait %.1f ms; canceled %d.\n", last.Acquires-first.Acquires, last.Empty-first.Empty, float64(last.EmptyWaitUS-first.EmptyWaitUS)/1000, last.Canceled-first.Canceled)
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		var gen, ack, terminal []int64
		closed, sealed := 0, 0
		var firstTerminal, lastTerminal int64
		var firstScheduled, lastScheduled, firstReceived, lastReceived int64
		for _, a := range byKind[k] {
			if a.HTTPStatus == 409 {
				switch a.Error {
				case "round_closed":
					closed++
				case "round_sealed":
					sealed++
				}
			}
			if firstScheduled == 0 || a.ScheduledUS < firstScheduled {
				firstScheduled = a.ScheduledUS
			}
			lastScheduled = max(lastScheduled, a.ScheduledUS)
			if a.ReceivedUS > 0 {
				if firstReceived == 0 || a.ReceivedUS < firstReceived {
					firstReceived = a.ReceivedUS
				}
				lastReceived = max(lastReceived, a.ReceivedUS)
			}
			if ts, ok := r.Metrics.Terminal[a.Seq]; ok && a.Seq > 0 && a.EffectiveUS > 0 {
				terminal = append(terminal, ts-a.EffectiveUS)
				if firstTerminal == 0 || ts < firstTerminal {
					firstTerminal = ts
				}
				lastTerminal = max(lastTerminal, ts)
			}
			if a.SentUS != 0 && a.ScheduledUS != 0 {
				gen = append(gen, a.SentUS-a.ScheduledUS)
			}
			if a.HTTPStatus == 202 && a.Error == "" && a.ReceivedUS != 0 && a.SentUS != 0 {
				ack = append(ack, a.ReceivedUS-a.SentUS)
			}
		}
		g, a := distOf(gen), distOf(ack)
		if len(gen) != len(byKind[k]) || len(ack) == 0 || len(terminal) != len(ack) {
			incomplete = true
		}
		if g.P99 > 10 || a.P99 > 100 {
			missed = true
		}
		if len(terminal) > 0 {
			d := distOf(terminal)
			fmt.Fprintf(details, "\n%s first observed durable execution (50 ms polling) n=%d p50/p95/p99/max %.1f/%.1f/%.1f/%.1f ms.\n", k, d.N, d.P50, d.P95, d.P99, d.Max)
		} else {
			fmt.Fprintf(details, "\n%s first observed durable execution (50 ms polling): unmeasured.\n", k)
		}
		if lastScheduled > firstScheduled && lastReceived > firstReceived {
			fmt.Fprintf(details, "\n%s offered %.2f/s over scheduled phase, %.2f HTTP responses/s over response phase.\n", k, float64(len(byKind[k])-1)*1e6/float64(lastScheduled-firstScheduled), float64(len(byKind[k])-1)*1e6/float64(lastReceived-firstReceived))
		}
		if k != "retry" && lastTerminal > firstTerminal {
			fmt.Fprintf(details, "\n%s terminal outcomes observed at %.2f/s between first and last observed execution.\n", k, float64(len(terminal)-1)*1e6/float64(lastTerminal-firstTerminal))
		}
		if closed+sealed > 0 {
			fmt.Fprintf(details, "\n%s admission rejections: %d round closed, %d round sealed.\n", k, closed, sealed)
		}
		fmt.Fprintf(details, "\n%s (%d attempts): generator lag %s, successful acknowledgment %s.\n", k, len(byKind[k]), burstDist(g), burstDist(a))
	}
	if len(r.Verification.Problems) > 0 || !r.Verification.AuditOK || fail > 0 {
		missed = true
	}
	status := "pass"
	if incomplete {
		status = "inconclusive"
	}
	if missed {
		status = "fail"
	}
	fmt.Fprintf(details, "\nTrial hypothesis: %s.\n", status)

	for _, problem := range r.Verification.Problems {
		fmt.Fprintf(details, "\n- Verification: %s", problem)
	}
	for _, problem := range r.Metrics.Errors {
		fmt.Fprintf(details, "\n- Monitor: %s", problem)
	}
	details.WriteString("\n")
	return status
}

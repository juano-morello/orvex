//go:build integration && acceptance && spectatorload

package firstround

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The engine's main passes engineapi.Serve, which receives the pool, to
// run. The overlay wraps exactly that argument.
const (
	probeCall    = "os.Stderr, engineapi.Serve)"
	probeWrapped = "os.Stderr, poolProbe(engineapi.Serve))"
)

const poolProbeSource = `package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/juano-morello/orvex/internal/runtime/admission"
)

// poolProbe prints the pool's statistics to stdout every 100ms while serve
// runs. It exists only in the spectator benchmark's build.
func poolProbe(serve serveFunc) serveFunc {
	return func(ctx context.Context, ln net.Listener, pool *pgxpool.Pool, seq *admission.Sequencer, token string) error {
		pctx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				s := pool.Stat()
				fmt.Fprintf(os.Stdout, "probe-pool %d %d %d %d %d %d %d %d %d\n", time.Now().UnixMicro(),
					s.AcquireCount(), s.AcquireDuration().Microseconds(), s.EmptyAcquireCount(),
					s.EmptyAcquireWaitTime().Microseconds(), s.CanceledAcquireCount(),
					s.AcquiredConns(), s.TotalConns(), s.MaxConns())
				select {
				case <-pctx.Done():
					return
				case <-tick.C:
				}
			}
		}()
		err := serve(ctx, ln, pool, seq, token)
		stop()
		<-done
		return err
	}
}
`

// buildBenchSource builds the source's gateway and crashctl, and its
// engine with the pool probe overlaid, into a temporary directory.
func buildBenchSource(t *testing.T, s benchSource) benchSource {
	t.Helper()
	mainFile := filepath.Join(s.Dir, "cmd", "engine", "main.go")
	src, err := os.ReadFile(mainFile) //nolint:gosec // G304: a checkout the operator named
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(src, []byte(probeCall)); n != 1 {
		t.Fatalf("%s: found %q %d times, want once: the pool probe wraps that argument of run", mainFile, probeCall, n)
	}
	dir := t.TempDir()
	patched := filepath.Join(dir, "main.go")
	probe := filepath.Join(dir, "zz_poolprobe.go")
	if err := os.WriteFile(patched, bytes.Replace(src, []byte(probeCall), []byte(probeWrapped), 1), 0o600); err != nil { //nolint:gosec // G703: a file in this test's temporary directory
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, []byte(poolProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {
		mainFile: patched,
		filepath.Join(s.Dir, "cmd", "engine", "zz_poolprobe.go"): probe,
	}})
	if err != nil {
		t.Fatal(err)
	}
	overlayFile := writeFile(t, dir, "overlay.json", string(overlay))
	bin := filepath.Join(dir, "bin")
	ldflags := "-X github.com/juano-morello/orvex/internal/buildinfo.Version=" + buildVersion
	goTool(t, s.Dir, "build", "-trimpath", "-ldflags", ldflags, "-overlay", overlayFile, "-o", filepath.Join(bin, "engine"), "./cmd/engine")
	goTool(t, s.Dir, "build", "-trimpath", "-ldflags", ldflags, "-o", bin+string(filepath.Separator), "./cmd/gateway", "./cmd/crashctl")
	s.bin = binaries{engine: filepath.Join(bin, "engine"), gateway: filepath.Join(bin, "gateway"), crashctl: filepath.Join(bin, "crashctl")}
	s.Commit = strings.TrimSpace(command(t, s.Dir, "git", "rev-parse", "HEAD"))
	s.Dirty = strings.TrimSpace(command(t, s.Dir, "git", "status", "--porcelain", "--untracked-files=no")) != ""
	return s
}

func goTool(t *testing.T, dir string, args ...string) {
	t.Helper()
	command(t, dir, "go", args...)
}

func command(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // G204: fixed tools with arguments this test builds
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s in %s: %v\n%s", name, strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// poolSample is one probe-pool line: cumulative counts, durations in µs.
type poolSample struct {
	TS          int64 `json:"t"`
	Acquires    int64 `json:"a"`
	AcquireUS   int64 `json:"ad"`
	Empty       int64 `json:"e"`
	EmptyWaitUS int64 `json:"ew"`
	Canceled    int64 `json:"c"`
	Acquired    int32 `json:"u"`
	Total       int32 `json:"n"`
	Max         int32 `json:"m"`
}

func parsePoolSamples(log string) []poolSample {
	var out []poolSample
	for _, line := range strings.Split(log, "\n") {
		f := strings.Fields(strings.TrimPrefix(line, "stdout: "))
		if len(f) != 10 || f[0] != "probe-pool" {
			continue
		}
		var v [9]int64
		ok := true
		for i := range v {
			n, err := strconv.ParseInt(f[i+1], 10, 64)
			ok = ok && err == nil
			v[i] = n
		}
		if ok {
			out = append(out, poolSample{v[0], v[1], v[2], v[3], v[4], v[5], int32(v[6]), int32(v[7]), int32(v[8])}) //nolint:gosec // G115: connection counts
		}
	}
	return out
}

// trialEnv pins what the trial ran on.
type trialEnv struct {
	Source       string    `json:"source"`
	Commit       string    `json:"commit"`
	Dirty        bool      `json:"dirty"`
	GoTest       string    `json:"goTest"`
	GoBuild      string    `json:"goBuild"`
	Postgres     string    `json:"postgres"`
	Docker       string    `json:"docker"`
	Kernel       string    `json:"kernel"`
	CPUs         int       `json:"cpus"`
	LoadAvg      string    `json:"loadAvg"`
	Containers   int       `json:"containers"`
	ClockSkewUS  int64     `json:"clockSkewUs"`
	PoolMaxConns int       `json:"poolMaxConns"`
	Quiet        quietHost `json:"quiet"`
	// PGProxies counts the docker-proxy processes of the database port
	// whose CPU counts as the database's.
	PGProxies int `json:"pgProxies"`
}

func captureEnv(t *testing.T, p *platform, s benchSource, cfg benchConfig) trialEnv {
	t.Helper()
	e := trialEnv{Source: s.Label, Commit: s.Commit, Dirty: s.Dirty, GoTest: runtime.Version(),
		CPUs: runtime.NumCPU(), PoolMaxConns: cfg.PoolMaxConns}
	e.GoBuild = strings.TrimSpace(command(t, s.Dir, "go", "env", "GOVERSION"))
	if err := p.obs.db.QueryRow(context.Background(), `SHOW server_version`).Scan(&e.Postgres); err != nil {
		t.Fatal(err)
	}
	e.Docker = strings.TrimSpace(command(t, s.Dir, "docker", "version", "--format", "{{.Server.Version}}"))
	e.Containers = len(strings.Fields(command(t, s.Dir, "docker", "ps", "-q")))
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		e.Kernel = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		e.LoadAvg = strings.TrimSpace(string(b))
	}
	before := time.Now().UnixMicro()
	db := p.obs.now()
	e.ClockSkewUS = db - (before+time.Now().UnixMicro())/2
	containerCPU(p)
	e.PGProxies = len(pgProxies)
	return e
}

// cpuSample holds cumulative CPU time in µs; PG is -1 when the database
// container's cgroup is not readable. PG includes the docker-proxy that
// forwards the database port, and host time leaves out interrupts, which
// the loopback traffic of the trial itself causes.
type cpuSample struct {
	TS        int64 `json:"t"`
	Engine    int64 `json:"engine"`
	Gateway   int64 `json:"gateway"`
	Self      int64 `json:"self"`
	PG        int64 `json:"pg"`
	HostBusy  int64 `json:"hostBusy"`
	HostTotal int64 `json:"hostTotal"`
}

// clockTick is USER_HZ on Linux.
const clockTick = 10_000

func sampleCPU(p *platform, engine, gw *service) cpuSample {
	busy, total := hostCPU()
	return cpuSample{
		TS: time.Now().UnixMicro(), Engine: processCPU(engine.cmd.Process.Pid), Gateway: processCPU(gw.cmd.Process.Pid),
		Self: processCPU(os.Getpid()), PG: containerCPU(p), HostBusy: busy, HostTotal: total,
	}
}

func processCPU(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	_, after, ok := strings.Cut(string(b), ") ")
	f := strings.Fields(after)
	if !ok || len(f) < 13 {
		return -1
	}
	utime, err1 := strconv.ParseInt(f[11], 10, 64)
	stime, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return -1
	}
	return (utime + stime) * clockTick
}

func hostCPU() (busy, total int64) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return -1, -1
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 9 || f[0] != "cpu" {
		return -1, -1
	}
	var v [8]int64
	for i := range v {
		v[i], _ = strconv.ParseInt(f[i+1], 10, 64)
		total += v[i]
	}
	// user, nice, system and steal; not idle, iowait, irq or softirq.
	return (v[0] + v[1] + v[2] + v[7]) * clockTick, total * clockTick
}

var (
	pgCgroupOnce sync.Once
	pgCgroup     string
	pgProxies    []int
)

// containerCPU reads the PostgreSQL container's cgroup usage, finding the
// container by the port the test database listens on.
func containerCPU(p *platform) int64 {
	pgCgroupOnce.Do(func() {
		u, err := url.Parse(p.ownerDSN)
		if err != nil {
			return
		}
		_, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			return
		}
		out, err := exec.Command("docker", "ps", "--no-trunc", "-q", "--filter", "publish="+port).Output() //nolint:gosec // G204: fixed tool, port from the test DSN
		if ids := strings.Fields(string(out)); err == nil && len(ids) == 1 {
			pgCgroup = "/sys/fs/cgroup/system.slice/docker-" + ids[0] + ".scope/cpu.stat"
		}
		cmdlines, _ := filepath.Glob("/proc/[0-9]*/cmdline")
		for _, c := range cmdlines {
			b, err := os.ReadFile(c) //nolint:gosec // G304: /proc entries
			if err != nil || !bytes.HasPrefix(b, []byte("/usr/bin/docker-proxy\x00")) || !bytes.Contains(b, []byte("\x00-host-port\x00"+port+"\x00")) {
				continue
			}
			if pid, err := strconv.Atoi(strings.Split(c, "/")[2]); err == nil {
				pgProxies = append(pgProxies, pid)
			}
		}
	})
	b, err := os.ReadFile(pgCgroup) //nolint:gosec // G304: the cgroup file of the test database container
	if pgCgroup == "" || err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return -1
			}
			for _, pid := range pgProxies {
				n += max(processCPU(pid), 0)
			}
			return n
		}
	}
	return -1
}

// pgSample counts the engine role's backends by state.
type pgSample struct {
	TS     int64 `json:"t"`
	Active int   `json:"a"`
	IdleTx int   `json:"i"`
	Total  int   `json:"n"`
}

type pgSampler struct {
	stopCh  chan struct{}
	done    chan struct{}
	samples []pgSample
}

// startPGSampler samples pg_stat_activity for the engine's role every
// 50ms over its own connection, outside the observer's pool.
func startPGSampler(t *testing.T, p *platform) *pgSampler {
	t.Helper()
	u, err := url.Parse(p.engineDSN)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(context.Background(), p.ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	s := &pgSampler{stopCh: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer func() { _ = conn.Close(context.Background()) }()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			var v pgSample
			if err := conn.QueryRow(context.Background(), `SELECT
				count(*) FILTER (WHERE state = 'active'),
				count(*) FILTER (WHERE state LIKE 'idle in transaction%'),
				count(*)
				FROM pg_stat_activity WHERE usename = $1`, u.User.Username()).Scan(&v.Active, &v.IdleTx, &v.Total); err == nil {
				v.TS = time.Now().UnixMicro()
				s.samples = append(s.samples, v)
			}
			select {
			case <-s.stopCh:
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

func (s *pgSampler) stop() []pgSample {
	close(s.stopCh)
	<-s.done
	return s.samples
}

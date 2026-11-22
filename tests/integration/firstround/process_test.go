//go:build integration && acceptance

package firstround

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	buildVersion = "acceptance"
	readyTimeout = 60 * time.Second
	stopGrace    = 15 * time.Second
)

type binaries struct{ engine, gateway, crashctl string }

var (
	binDirMu sync.Mutex
	binDir   string
	build    = sync.OnceValues(buildBinaries)
)

// builtBinaries builds engine, gateway and crashctl once per test binary
// from this checkout's main packages.
func builtBinaries(t *testing.T) binaries {
	t.Helper()
	b, err := build()
	if err != nil {
		t.Fatalf("build the services: %v", err)
	}
	return b
}

func buildBinaries() (binaries, error) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		return binaries{}, err
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return binaries{}, fmt.Errorf("module root %s: %w", root, err)
	}
	dir, err := os.MkdirTemp("", "orvex-acceptance-bin-")
	if err != nil {
		return binaries{}, err
	}
	binDirMu.Lock()
	binDir = dir
	binDirMu.Unlock()
	goTool, err := exec.LookPath("go")
	if err != nil {
		return binaries{}, err
	}
	args := []string{"build", "-trimpath", "-ldflags", "-X github.com/juano-morello/orvex/internal/buildinfo.Version=" + buildVersion, "-o", dir + string(filepath.Separator)}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, "./cmd/engine", "./cmd/gateway", "./cmd/crashctl")
	cmd := exec.Command(goTool, args...) //nolint:gosec // G204: the go tool and its arguments are fixed by this test
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return binaries{}, fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return binaries{
		engine:   filepath.Join(dir, "engine"),
		gateway:  filepath.Join(dir, "gateway"),
		crashctl: filepath.Join(dir, "crashctl"),
	}, nil
}

func removeBinaries() {
	binDirMu.Lock()
	defer binDirMu.Unlock()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
}

// requireVersion requires bin -version to print "<name> acceptance": the
// test runs the binaries it built.
func requireVersion(t *testing.T, bin, name string) {
	t.Helper()
	out, err := exec.Command(bin, "-version").Output() //nolint:gosec // G204: bin is a binary this test built
	if err != nil {
		t.Fatalf("%s -version: %v", name, err)
	}
	if got, want := strings.TrimSpace(string(out)), name+" "+buildVersion; got != want {
		t.Fatalf("%s -version = %q, want %q", name, got, want)
	}
}

var readyLine = regexp.MustCompile(`^ready (\S+)$`)

// serviceLog keeps a child's stdout and stderr lines in order and reports
// the address of its first "ready <address>" line.
type serviceLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan string
	found bool
}

func newServiceLog() *serviceLog { return &serviceLog{ready: make(chan string, 1)} }

func (l *serviceLog) line(stream, s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, "%s: %s\n", stream, s)
	if m := readyLine.FindStringSubmatch(s); m != nil && !l.found {
		l.found = true
		l.ready <- m[1]
	}
}

func (l *serviceLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// lineWriter splits one stream into lines. os/exec copies each stream on
// its own goroutine, so partial is never shared.
type lineWriter struct {
	log     *serviceLog
	stream  string
	partial []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.log.line(w.stream, string(w.partial[:i]))
		w.partial = w.partial[i+1:]
	}
}

// service is a running child process in its own process group.
type service struct {
	name   string
	addr   string
	cmd    *exec.Cmd
	log    *serviceLog
	exited chan struct{}
}

// launch starts bin with exactly env (nothing inherited) and waits for its
// readiness line. A child that exits or stays silent past timeout is an
// error that carries its output.
func launch(name, bin string, args, env []string, timeout time.Duration) (*service, error) {
	log := newServiceLog()
	cmd := exec.Command(bin, args...) //nolint:gosec // G204: bin is a binary this test built
	cmd.Env = env
	cmd.Stdout = &lineWriter{log: log, stream: "stdout"}
	cmd.Stderr = &lineWriter{log: log, stream: "stderr"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	s := &service{name: name, cmd: cmd, log: log, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(s.exited)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case addr := <-log.ready:
		if _, _, err := net.SplitHostPort(addr); err != nil {
			_, _ = s.stop()
			return nil, fmt.Errorf("%s printed ready with %q, not host:port: %w\n%s", name, addr, err, log)
		}
		s.addr = addr
		return s, nil
	case <-s.exited:
		return nil, fmt.Errorf("%s exited with status %d before printing \"ready <address>\"\n%s", name, cmd.ProcessState.ExitCode(), log)
	case <-timer.C:
		_, _ = s.stop()
		return nil, fmt.Errorf("%s printed no \"ready <address>\" line within %v\n%s", name, timeout, log)
	}
}

// startService launches a service, stops it at cleanup and logs its output
// if the test failed.
func startService(t *testing.T, name, bin string, env []string) *service {
	t.Helper()
	s, err := launch(name, bin, nil, env, readyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s output:\n%s", name, s.log)
		}
	})
	t.Cleanup(func() { _, _ = s.stop() })
	return s
}

// stop sends SIGTERM to the process group, waits stopGrace, then sends
// SIGKILL. It returns the exit status; calling it again returns the same.
func (s *service) stop() (int, error) {
	select {
	case <-s.exited:
		return s.cmd.ProcessState.ExitCode(), nil
	default:
	}
	pgid := -s.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	timer := time.NewTimer(stopGrace)
	defer timer.Stop()
	select {
	case <-s.exited:
		return s.cmd.ProcessState.ExitCode(), nil
	case <-timer.C:
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-s.exited
		return s.cmd.ProcessState.ExitCode(), errors.New("did not exit within " + stopGrace.String() + " of SIGTERM")
	}
}

// requireCleanStop requires the service to exit 0 on SIGTERM.
func requireCleanStop(t *testing.T, s *service) {
	t.Helper()
	code, err := s.stop()
	if err != nil || code != 0 {
		t.Errorf("%s on SIGTERM: exit status %d, %v; want 0", s.name, code, err)
	}
}

package pgtest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestComposeImageMatchesPinnedImage(t *testing.T) {
	b, err := os.ReadFile("../../../../deployments/docker/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "image: "+Image) {
		t.Errorf("deployments/docker/compose.yaml does not use image %s", Image)
	}
}

// fakeM records whether the tests would have run.
type fakeM struct{ ran bool }

func (m *fakeM) Run() int {
	m.ran = true
	return 0
}

// TestRunFailsWithoutDocker pins decision P-8: without a Docker-compatible
// daemon the integration tests fail with a diagnostic and never run (or
// skip). The start step is a stub, so the host's Docker state is irrelevant.
func TestRunFailsWithoutDocker(t *testing.T) {
	m := &fakeM{}
	var stderr bytes.Buffer
	start := func(context.Context) (string, func() error, error) {
		return "", nil, errors.New("Cannot connect to the Docker daemon")
	}
	if code := run(m, start, &stderr); code == 0 {
		t.Errorf("run = %d, want non-zero", code)
	}
	if m.ran {
		t.Error("the tests ran without a database")
	}
	want := "pgtest: integration tests need a Docker-compatible daemon: Cannot connect to the Docker daemon\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

// TestRunFailsWhenDatabaseUnreachable: a started container whose database
// cannot be reached also fails the run, and the container is still stopped.
func TestRunFailsWhenDatabaseUnreachable(t *testing.T) {
	m := &fakeM{}
	var stderr bytes.Buffer
	stopped := false
	start := func(context.Context) (string, func() error, error) {
		return "127.0.0.1:1", func() error { stopped = true; return nil }, nil
	}
	if code := run(m, start, &stderr); code == 0 {
		t.Errorf("run = %d, want non-zero", code)
	}
	if m.ran {
		t.Error("the tests ran without a database")
	}
	if !stopped {
		t.Error("the container was not stopped")
	}
	if !strings.HasPrefix(stderr.String(), "pgtest: admin ") {
		t.Errorf("stderr = %q, want a pgtest: admin diagnostic", stderr.String())
	}
}

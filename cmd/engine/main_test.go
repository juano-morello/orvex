package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/buildinfo"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string // substring; empty means stderr must be empty
		stderrAny  bool   // stderr must be non-empty, content unchecked
	}{
		{name: "version", args: []string{"-version"}, wantCode: 0, wantStdout: name + " " + buildinfo.Version + "\n"},
		{name: "no args", args: nil, wantCode: 1, wantStderr: "not implemented"},
		{name: "positional", args: []string{"extra"}, wantCode: 2, wantStderr: "unexpected arguments"},
		{name: "unknown flag", args: []string{"-bogus"}, wantCode: 2, stderrAny: true},
		{name: "help", args: []string{"-h"}, wantCode: 0, stderrAny: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			switch {
			case tt.stderrAny:
				if stderr.Len() == 0 {
					t.Error("stderr is empty, want output")
				}
			case tt.wantStderr == "":
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want empty", stderr.String())
				}
			case !strings.Contains(stderr.String(), tt.wantStderr):
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestRunWriteError(t *testing.T) {
	if code := run([]string{"-version"}, failingWriter{}, io.Discard); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

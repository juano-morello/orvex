package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// TestCommandIDs: lifecycle command IDs are uuid8 of the round and the
// step, checked against values computed independently with Python's
// hashlib.
func TestCommandIDs(t *testing.T) {
	round := [16]byte{6: 0x40, 8: 0x80, 15: 1}
	want := map[string]string{
		"start":  "ae21c8c1-1a27-8d2e-8c5a-caae56a81192",
		"close":  "321d6c46-efb1-8927-b617-c548477a14f6",
		"settle": "6847e8d1-20d8-8b9b-be0c-344cb3241f47",
	}
	for _, st := range steps {
		if got := string(canonjson.UUID(commandID(round, st.name))); got != want[st.name] {
			t.Errorf("commandID(%s) = %s, want %s", st.name, got, want[st.name])
		}
	}
	if len(steps) != len(want) {
		t.Errorf("%d steps, want %d", len(steps), len(want))
	}
	if commandID(round, "start") == commandID([16]byte{15: 2}, "start") {
		t.Error("two rounds share a start command ID")
	}
}

// TestSchedulerLockKey pins the advisory lock key, computed independently
// with Python's hashlib.
func TestSchedulerLockKey(t *testing.T) {
	if schedulerLockKey != 8451814261399681253 {
		t.Errorf("schedulerLockKey = %d", schedulerLockKey)
	}
}

func TestNewRoundIDIsV4(t *testing.T) {
	a, b := newRoundID(), newRoundID()
	if a == b {
		t.Fatal("two equal round IDs")
	}
	if a[6]>>4 != 4 || a[8]>>6 != 2 {
		t.Errorf("round ID %s is not an RFC 9562 version 4 UUID", canonjson.UUID(a))
	}
	if s1, s2 := randomSeed(a), randomSeed(a); s1 == s2 {
		t.Error("two equal seeds")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(nil, nil, nil, Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("nil dependencies: err = %v, want ErrInvalidConfig", err)
	}
}

// TestDescribe: driver text never reaches a log line; Orvex text does.
func TestDescribe(t *testing.T) {
	connect := &pgconn.ConnectError{}
	dsnText := "failed to connect to `user=orvex database=orvex`"
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"orvex error": {
			err:  fmt.Errorf("%w: phase INVALID has no lifecycle step", errHalted),
			want: "lifecycle: round halted: phase INVALID has no lifecycle step",
		},
		"server error": {
			err:  fmt.Errorf("%w: %w", execution.ErrUnavailable, &pgconn.PgError{Code: "08006", Message: "connection for user orvex lost"}),
			want: "execution: database unavailable; call again later: SQLSTATE 08006",
		},
		"connect error": {
			err:  fmt.Errorf("%w: %w", admission.ErrUnavailable, connect),
			want: "admission: database unavailable; command not admitted: database driver error",
		},
		"halted refusal": {
			err:  fmt.Errorf("%w: %w", errHalted, fmt.Errorf("x: %w: %w", &admission.RejectedError{Reason: admission.ReasonLifecycleOrder}, &pgconn.PgError{Code: "P0001"})),
			want: "lifecycle: round halted: admission: rejected: lifecycle_order: SQLSTATE P0001",
		},
		"no rows": {
			err:  fmt.Errorf("%w: %w", execution.ErrInternal, pgx.ErrNoRows),
			want: "execution: internal error: database driver error",
		},
		"context": {
			err:  fmt.Errorf("wrap: %w", context.Canceled),
			want: "wrap: context canceled",
		},
	} {
		got := Describe(c.err)
		if got != c.want {
			t.Errorf("%s: Describe = %q, want %q", name, got, c.want)
		}
		if strings.Contains(got, "user") || strings.Contains(got, dsnText) {
			t.Errorf("%s: Describe = %q carries driver text", name, got)
		}
	}
	if Describe(nil) != "" {
		t.Error("Describe(nil) is not empty")
	}
}

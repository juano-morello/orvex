package admission

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRejectedError(t *testing.T) {
	for _, r := range []Reason{ReasonRoundClosed, ReasonRoundSealed, ReasonLifecycleOrder, ReasonInsufficientCredits} {
		err := fmt.Errorf("wrapped: %w", &RejectedError{Reason: r})
		if !errors.Is(err, ErrRejected) {
			t.Errorf("%s: errors.Is(err, ErrRejected) = false", r)
		}
		var re *RejectedError
		if !errors.As(err, &re) || re.Reason != r {
			t.Errorf("%s: errors.As recovered %+v", r, re)
		}
		if got, want := (&RejectedError{Reason: r}).Error(), "admission: rejected: "+string(r); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
		if got, ok := reasonOf(string(r)); !ok || got != r {
			t.Errorf("reasonOf(%q) = %q, %v", r, got, ok)
		}
	}
	if _, ok := reasonOf("bogus"); ok {
		t.Error("reasonOf(bogus) is known")
	}
}

func TestClosureNotDueError(t *testing.T) {
	e := &ClosureNotDueError{EffectiveTimeUnixMicro: 1760000000123457}
	err := fmt.Errorf("wrapped: %w", e)
	if !errors.Is(err, ErrClosureNotDue) {
		t.Error("errors.Is(err, ErrClosureNotDue) = false")
	}
	var got *ClosureNotDueError
	if !errors.As(err, &got) || got.EffectiveTimeUnixMicro != 1760000000123457 {
		t.Errorf("errors.As recovered %+v", got)
	}
	if strings.ContainsAny(e.Error(), "0123456789") {
		t.Errorf("Error() = %q contains digits", e.Error())
	}
}

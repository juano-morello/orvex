package execution

import (
	"errors"
	"fmt"
	"testing"
)

func TestFailClosedError(t *testing.T) {
	cause := errors.New("boom")
	round := [16]byte{0x01, 0x90, 0xf3, 0xa8, 0x7b, 0x3c, 0x7d, 0x4e, 0x8f, 0, 0, 0, 0, 0, 0, 0x01}
	err := fmt.Errorf("wrapped: %w", &FailClosedError{RoundID: round, Sequence: 7, Err: cause})
	if !errors.Is(err, ErrFailClosed) || !errors.Is(err, cause) {
		t.Errorf("errors.Is(ErrFailClosed) = %v, errors.Is(cause) = %v; want both", errors.Is(err, ErrFailClosed), errors.Is(err, cause))
	}
	var fc *FailClosedError
	if !errors.As(err, &fc) || fc.Sequence != 7 || fc.RoundID != round {
		t.Errorf("errors.As recovered %+v", fc)
	}
	want := "execution: round 0190f3a8-7b3c-7d4e-8f00-000000000001 halted at sequence 7: boom"
	if got := fc.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	for _, s := range []error{ErrInvalidConfig, ErrInvalidRequest, ErrInvalidRound, ErrUnknownRules, ErrRoundNotFound,
		ErrRoundExists, ErrFenced, ErrFailClosed, ErrCorrupt, ErrBusy, ErrUnavailable, ErrInternal} {
		if errors.Is(err, s) && s != ErrFailClosed { //nolint:errorlint // s and ErrFailClosed are table sentinels compared by identity to skip the one err must match
			t.Errorf("a FailClosedError matches %v", s)
		}
	}
}

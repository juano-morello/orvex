package v1

import (
	"errors"
	"math"
	"testing"
)

func TestPayoutTickZeroReturnsBet(t *testing.T) {
	for _, b := range []int64{1, 7, 1_000_000_000} {
		if got, err := Payout(b, Scale); err != nil || got != b {
			t.Errorf("Payout(%d, %d) = %d, %v; want %d", b, Scale, got, err, b)
		}
	}
}

func TestPayoutRejectsOutOfRange(t *testing.T) {
	cases := []struct{ bet, m int64 }{
		{0, Scale},
		{-1, Scale},
		{MaxBetCredits + 1, Scale},
		{math.MinInt64, Scale},
		{1, Scale - 1},
		{1, 0},
		{1, -1},
		{1, MaxCrashPointScaled},
		{1, math.MaxInt64},
		{1, math.MinInt64},
		// A naive int64 product of these would wrap.
		{math.MaxInt64, MaxCrashPointScaled - 1},
		{1 << 62, MaxCrashPointScaled - 1},
	}
	for _, c := range cases {
		if got, err := Payout(c.bet, c.m); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("Payout(%d, %d) = %d, %v; want ErrOutOfRange", c.bet, c.m, got, err)
		}
	}
}

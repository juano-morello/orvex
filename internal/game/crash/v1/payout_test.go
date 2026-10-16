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

// TestMaxPayoutCreditsIsReachableMaximum pins MaxPayoutCredits to the
// reachable maximum payout (math spec D-7): payable ticks are 0..MaxTick-1, M
// is non-decreasing, and Payout is monotone in m, so the largest payout is
// Payout(MaxBetCredits, M(MaxTick-1)).
func TestMaxPayoutCreditsIsReachableMaximum(t *testing.T) {
	last, err := MultiplierAtTick(MaxTick - 1)
	if err != nil {
		t.Fatalf("MultiplierAtTick(%d): %v", MaxTick-1, err)
	}
	if got, err := Payout(MaxBetCredits, last); err != nil || got != MaxPayoutCredits {
		t.Errorf("Payout(%d, M(%d) = %d) = %d, %v; want MaxPayoutCredits %d",
			MaxBetCredits, MaxTick-1, last, got, err, MaxPayoutCredits)
	}

	// Reachable: the latest eligible cash-out of the longest round lands on MaxTick-1.
	tick, eligible, err := CashOutTiming(0, MaxDurationMicros-1, MaxTick)
	if err != nil || !eligible || tick != MaxTick-1 {
		t.Errorf("CashOutTiming(0, %d, %d) = %d, %v, %v; want %d, true, nil",
			MaxDurationMicros-1, MaxTick, tick, eligible, err, MaxTick-1)
	}

	// Maximal: MaxTick itself reaches the cap and is never payable.
	capM, err := MultiplierAtTick(MaxTick)
	if err != nil || capM < MaxCrashPointScaled {
		t.Fatalf("MultiplierAtTick(%d) = %d, %v; want at least %d", MaxTick, capM, err, MaxCrashPointScaled)
	}
	if got, err := Payout(MaxBetCredits, capM); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("Payout(%d, M(%d) = %d) = %d, %v; want ErrOutOfRange", MaxBetCredits, MaxTick, capM, got, err)
	}
}

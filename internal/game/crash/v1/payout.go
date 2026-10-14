package v1

import "fmt"

// Payout returns floor(betCredits * multiplierScaled / Scale) with an exact,
// checked int64 product and a single floor at the end (D-8).
func Payout(betCredits, multiplierScaled int64) (int64, error) {
	if betCredits < MinBetCredits || betCredits > MaxBetCredits {
		return 0, fmt.Errorf("%w: bet %d not in [%d, %d]", ErrOutOfRange, betCredits, MinBetCredits, MaxBetCredits)
	}
	if multiplierScaled < Scale || multiplierScaled > MaxCrashPointScaled-1 {
		return 0, fmt.Errorf("%w: multiplier %d not in [%d, %d]",
			ErrOutOfRange, multiplierScaled, Scale, MaxCrashPointScaled-1)
	}
	p, err := mulNonNeg(betCredits, multiplierScaled)
	if err != nil {
		return 0, err
	}
	return p / Scale, nil
}

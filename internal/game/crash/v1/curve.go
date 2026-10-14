package v1

import (
	"fmt"

	"github.com/cockroachdb/apd/v3"
)

// MultiplierAtTick returns M(n) = floor(Scale * exp(0.0006 * n)) for n in
// [0, MaxTick], computed exactly as frozen in D-4: Exp, multiply by 10^4,
// Floor, checked Int64 conversion.
func MultiplierAtTick(n int64) (int64, error) {
	if n < 0 || n > MaxTick {
		return 0, fmt.Errorf("%w: tick %d not in [0, %d]", ErrOutOfRange, n, MaxTick)
	}
	ctx := ArithmeticContext()
	var y, v, f apd.Decimal
	c, err := ctx.Exp(&y, apd.New(growthCoefficient*n, growthExponent))
	if err := apdCheck("exp", c, err); err != nil {
		return 0, err
	}
	c, err = ctx.Mul(&v, &y, apd.New(1, scaleExponent))
	if err := apdCheck("mul", c, err); err != nil {
		return 0, err
	}
	c, err = ctx.Floor(&f, &v)
	if err := apdCheck("floor", c, err); err != nil {
		return 0, err
	}
	m, err := f.Int64()
	if err != nil {
		return 0, fmt.Errorf("%w: tick %d: %v", ErrArithmetic, n, err)
	}
	if m < Scale {
		return 0, fmt.Errorf("%w: tick %d: multiplier %d below scale", ErrArithmetic, n, m)
	}
	return m, nil
}

// CrashTick returns n_c = min{n : M(n) >= crashPointScaled} by binary search
// over M (D-9).
func CrashTick(crashPointScaled int64) (int64, error) {
	if crashPointScaled < MinCrashPointScaled || crashPointScaled > MaxCrashPointScaled {
		return 0, fmt.Errorf("%w: crash point %d not in [%d, %d]",
			ErrOutOfRange, crashPointScaled, MinCrashPointScaled, MaxCrashPointScaled)
	}
	// Invariant: M(hi) >= crashPointScaled, which holds for hi = MaxTick.
	lo, hi := int64(0), MaxTick
	for lo < hi {
		mid := lo + (hi-lo)/2
		m, err := MultiplierAtTick(mid)
		if err != nil {
			return 0, err
		}
		if m >= crashPointScaled {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, nil
}

// DeadlineOffsetMicros returns D = crashTick * TickMicros, the crash deadline
// offset from round start (D-9).
func DeadlineOffsetMicros(crashTick int64) (int64, error) {
	if crashTick < 0 || crashTick > MaxTick {
		return 0, fmt.Errorf("%w: crash tick %d not in [0, %d]", ErrOutOfRange, crashTick, MaxTick)
	}
	return mulNonNeg(crashTick, TickMicros)
}

// CashOutTiming decides timing eligibility of a cash-out with durable effective
// admission time effectiveMicros in a round started at startMicros whose crash
// tick is crashTick (D-9, ADR-002). With e = effectiveMicros - startMicros it is
// eligible iff 0 < e < crashTick*TickMicros; deadline equality loses. When
// eligible, tick is floor(e / TickMicros). Ineligible results are (0, false, nil).
func CashOutTiming(startMicros, effectiveMicros, crashTick int64) (tick int64, eligible bool, err error) {
	if startMicros < 0 || startMicros > MaxStartMicros {
		return 0, false, fmt.Errorf("%w: start %d not in [0, %d]", ErrOutOfRange, startMicros, MaxStartMicros)
	}
	if effectiveMicros < 0 {
		return 0, false, fmt.Errorf("%w: effective time %d is negative", ErrOutOfRange, effectiveMicros)
	}
	if crashTick < 0 || crashTick > MaxTick {
		return 0, false, fmt.Errorf("%w: crash tick %d not in [0, %d]", ErrOutOfRange, crashTick, MaxTick)
	}
	e, err := subChecked(effectiveMicros, startMicros)
	if err != nil {
		return 0, false, err
	}
	d, err := DeadlineOffsetMicros(crashTick)
	if err != nil {
		return 0, false, err
	}
	if e <= 0 || e >= d {
		return 0, false, nil
	}
	return e / TickMicros, true, nil
}

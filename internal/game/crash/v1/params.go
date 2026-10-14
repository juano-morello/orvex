// Package v1 holds the frozen arithmetic and fairness derivation of Crash rules
// "crash/v1" (ADR-004; LBT-7; spec specs/e96d124d_crash-v1-math-spec.md).
//
// Its functions are pure and deterministic: no I/O, no clock, no randomness and
// no package state. The Crash Decide/Evolve (LBT-8) may therefore call them
// (ADR-003). Released values, golden vectors and serializers are immutable
// (ADR-001, ADR-007); new rules go in a new version with its own package.
package v1

import (
	"errors"
	"fmt"
	"math"
	"math/bits"

	"github.com/cockroachdb/apd/v3"
)

// RulesVersion is the textual rules identifier used in OutcomeTag and in the
// golden vectors. It is distinct from the numeric game.RulesVersion registry
// key, which LBT-8 wires.
const RulesVersion = "crash/v1"

// Frozen crash/v1 parameters (spec D-1 to D-7). Multipliers are int64 values
// scaled by Scale; times are int64 microseconds; amounts are int64 credits.
const (
	// Scale is the multiplier scale: 10,000 means 1.0000x (D-1).
	Scale int64 = 10_000
	// TickMicros is the time quantum Q in microseconds (D-2).
	TickMicros int64 = 10_000
	// EdgeNumerator is Scale * (1 - house edge) with a 1% edge (D-5).
	EdgeNumerator int64 = 9_900
	// MinCrashPointScaled is the lowest crash point, 1.0000x (D-5).
	MinCrashPointScaled int64 = 10_000
	// MaxCrashPointScaled is the crash-point cap, 10,000.0000x (D-6).
	MaxCrashPointScaled int64 = 100_000_000
	// MaxTick is the first tick whose multiplier reaches the cap (D-6).
	MaxTick int64 = 15_351
	// MaxDurationMicros is MaxTick * TickMicros (D-6).
	MaxDurationMicros int64 = 153_510_000
	// MinBetCredits is the smallest bet (D-7).
	MinBetCredits int64 = 1
	// MaxBetCredits is the largest bet (D-7).
	MaxBetCredits int64 = 1_000_000_000
	// MaxPayoutCredits is the highest reachable payout: MaxBetCredits at M(MaxTick-1) (D-7).
	MaxPayoutCredits int64 = 9_996_596_800_000
	// MaxStartMicros is the latest accepted round start time (D-7).
	MaxStartMicros int64 = math.MaxInt64 - MaxDurationMicros
)

// OutcomeTag prefixes the outcome HMAC message and carries the rules version (D-10).
const OutcomeTag = "orvex/crash/v1/crash-point:"

// Growth per tick is the exact decimal growthCoefficient * 10^growthExponent = 6E-4
// (D-3); 10^scaleExponent == Scale.
const (
	growthCoefficient int64 = 6
	growthExponent    int32 = -4
	scaleExponent     int32 = 4
)

var (
	// ErrOutOfRange reports an input outside the crash/v1 limits (D-7).
	// Callers turn it into a terminal command rejection.
	ErrOutOfRange = errors.New("crash/v1: input out of range")
	// ErrArithmetic reports a failed or overflowing computation. Callers fail
	// closed and emit no events.
	ErrArithmetic = errors.New("crash/v1: arithmetic failure")
)

// ArithmeticContext returns a fresh copy of the frozen D-4 apd context:
// IEEE 754 decimal128 precision and exponent range, half-even rounding and
// apd.DefaultTraps.
func ArithmeticContext() apd.Context {
	return apd.Context{
		Precision:   34,
		MaxExponent: 6144,
		MinExponent: -6143,
		Traps:       apd.DefaultTraps,
		Rounding:    apd.RoundHalfEven,
	}
}

// apdCheck fails an apd call that returned an error or any condition other
// than Inexact or Rounded (D-4).
func apdCheck(op string, c apd.Condition, err error) error {
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrArithmetic, op, err)
	}
	if c&^(apd.Inexact|apd.Rounded) != 0 {
		return fmt.Errorf("%w: %s: unexpected condition %v", ErrArithmetic, op, c)
	}
	return nil
}

// mulNonNeg returns a*b for non-negative operands, failing on overflow.
func mulNonNeg(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, fmt.Errorf("%w: negative operand in %d * %d", ErrArithmetic, a, b)
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return 0, fmt.Errorf("%w: %d * %d overflows int64", ErrArithmetic, a, b)
	}
	return int64(lo), nil
}

// subChecked returns a-b, failing on signed overflow.
func subChecked(a, b int64) (int64, error) {
	d := a - b
	if (b > 0 && d > a) || (b < 0 && d < a) {
		return 0, fmt.Errorf("%w: %d - %d overflows int64", ErrArithmetic, a, b)
	}
	return d, nil
}

package v1

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

func FuzzPayout(f *testing.F) {
	var vectors payoutsFile
	loadVectors(f, "payouts.json", &vectors)
	for _, c := range vectors.Cases {
		f.Add(c.BetCredits, c.MultiplierScaled)
	}
	f.Fuzz(func(t *testing.T, bet, m int64) {
		got, err := Payout(bet, m)
		inDomain := bet >= MinBetCredits && bet <= MaxBetCredits && m >= Scale && m <= MaxCrashPointScaled-1
		if !inDomain {
			if !errors.Is(err, ErrOutOfRange) {
				t.Fatalf("Payout(%d, %d) = %d, %v; want ErrOutOfRange", bet, m, got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("Payout(%d, %d): %v", bet, m, err)
		}
		want := new(big.Int).Mul(big.NewInt(bet), big.NewInt(m))
		want.Quo(want, big.NewInt(Scale))
		if !want.IsInt64() || want.Int64() != got {
			t.Fatalf("Payout(%d, %d) = %d, want %s", bet, m, got, want)
		}
		if got > MaxBetCredits*(MaxCrashPointScaled-1)/Scale {
			t.Fatalf("Payout(%d, %d) = %d above the domain bound", bet, m, got)
		}
	})
}

func FuzzCrashPointFromR(f *testing.F) {
	for _, r := range []uint64{0, 1 << 63, math.MaxUint64, 0xfff9830e3cd9a523} {
		f.Add(r)
	}
	two64 := new(big.Int).Lsh(big.NewInt(1), 64)
	num := new(big.Int).Mul(big.NewInt(EdgeNumerator), two64)
	f.Fuzz(func(t *testing.T, r uint64) {
		got := CrashPointFromR(r)
		den := new(big.Int).Sub(two64, new(big.Int).SetUint64(r))
		raw := new(big.Int).Quo(num, den)
		want := raw
		if raw.Cmp(big.NewInt(MaxCrashPointScaled)) > 0 {
			want = big.NewInt(MaxCrashPointScaled)
		}
		if raw.Cmp(big.NewInt(MinCrashPointScaled)) < 0 {
			want = big.NewInt(MinCrashPointScaled)
		}
		if got < MinCrashPointScaled || got > MaxCrashPointScaled || want.Int64() != got {
			t.Fatalf("CrashPointFromR(%#x) = %d, want %s (raw %s)", r, got, want, raw)
		}
	})
}

func FuzzCashOutTiming(f *testing.F) {
	var vectors deadlinesFile
	loadVectors(f, "deadlines.json", &vectors)
	for _, c := range vectors.Cases {
		f.Add(c.StartMicros, c.EffectiveMicros, c.CrashTick)
	}
	f.Fuzz(func(t *testing.T, start, eff, ct int64) {
		tick, eligible, err := CashOutTiming(start, eff, ct)
		inDomain := start >= 0 && start <= MaxStartMicros && eff >= 0 && ct >= 0 && ct <= MaxTick
		if !inDomain {
			if !errors.Is(err, ErrOutOfRange) || eligible || tick != 0 {
				t.Fatalf("CashOutTiming(%d, %d, %d) = %d, %v, %v; want ErrOutOfRange", start, eff, ct, tick, eligible, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("CashOutTiming(%d, %d, %d): %v", start, eff, ct, err)
		}
		e := eff - start // both non-negative: cannot overflow
		want := e > 0 && e < ct*TickMicros
		if eligible != want {
			t.Fatalf("CashOutTiming(%d, %d, %d) eligible = %v, want %v", start, eff, ct, eligible, want)
		}
		if eligible && (tick != e/TickMicros || tick >= ct) {
			t.Fatalf("CashOutTiming(%d, %d, %d) tick = %d, want %d < %d", start, eff, ct, tick, e/TickMicros, ct)
		}
		if !eligible && tick != 0 {
			t.Fatalf("CashOutTiming(%d, %d, %d) ineligible with tick %d", start, eff, ct, tick)
		}
	})
}

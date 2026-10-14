package v1

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/cockroachdb/apd/v3"
)

func TestArithmeticContextFrozen(t *testing.T) {
	ctx := ArithmeticContext()
	if ctx.Precision != 34 || ctx.MaxExponent != 6144 || ctx.MinExponent != -6143 {
		t.Errorf("context = precision %d, exponents %d/%d; want 34, 6144/-6143",
			ctx.Precision, ctx.MaxExponent, ctx.MinExponent)
	}
	if ctx.Rounding != apd.RoundHalfEven {
		t.Errorf("rounding = %q, want %q", ctx.Rounding, apd.RoundHalfEven)
	}
	if ctx.Traps != apd.DefaultTraps {
		t.Errorf("traps = %v, want apd.DefaultTraps", ctx.Traps)
	}

	ctx.Precision = 5
	ctx.Rounding = apd.RoundFloor
	if again := ArithmeticContext(); again.Precision != 34 || again.Rounding != apd.RoundHalfEven {
		t.Errorf("mutating a returned context changed the next call: %+v", again)
	}
}

func TestApdVersionPinned(t *testing.T) {
	const module = "github.com/cockroachdb/apd/v3"
	if got := apdCompiledVersion(t); got != "v3.2.3" {
		t.Errorf("test binary compiles %s %s, want v3.2.3 (D-13)", module, got)
	}
	if got := pinnedModuleVersion(t, module); got != "v3.2.3" {
		t.Errorf("go.mod pins %s %s, want v3.2.3 (D-13)", module, got)
	}
	sum := string(moduleRootFile(t, "go.sum"))
	for _, line := range []string{
		module + " v3.2.3 h1:4Zx+I3R35bFXMnltzmjP79i2cravE4jTRL6ps9Aux80=",
		module + " v3.2.3/go.mod h1:klXJcjp+FffLTHlhIG69tezTDvdP065naDsHzKhYSqc=",
	} {
		if !strings.Contains(sum, line+"\n") {
			t.Errorf("go.sum lacks the D-13 line %q", line)
		}
	}
}

func TestMultiplierStrictlyIncreasing(t *testing.T) {
	table := multiplierTable(t)
	if table[0] != Scale {
		t.Errorf("M(0) = %d, want %d", table[0], Scale)
	}
	for n := range len(table) - 1 {
		if table[n+1] <= table[n] {
			t.Fatalf("M(%d) = %d is not above M(%d) = %d", n+1, table[n+1], n, table[n])
		}
	}
}

func TestMultiplierAtTickOutOfRange(t *testing.T) {
	for _, n := range []int64{-1, MaxTick + 1, math.MinInt64, math.MaxInt64} {
		if m, err := MultiplierAtTick(n); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("MultiplierAtTick(%d) = %d, %v; want ErrOutOfRange", n, m, err)
		}
	}
}

func TestCrashTickAndDeadlineOutOfRange(t *testing.T) {
	for _, c := range []int64{9_999, 100_000_001, 0, -1, math.MaxInt64} {
		if n, err := CrashTick(c); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("CrashTick(%d) = %d, %v; want ErrOutOfRange", c, n, err)
		}
	}
	for _, n := range []int64{-1, MaxTick + 1} {
		if d, err := DeadlineOffsetMicros(n); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("DeadlineOffsetMicros(%d) = %d, %v; want ErrOutOfRange", n, d, err)
		}
	}
}

func TestCrashTickBracketsCrashPoint(t *testing.T) {
	table := multiplierTable(t)
	for _, c := range []int64{10_000, 10_001, 10_006, 17_577, 19_800, 100_000_000} {
		n, err := CrashTick(c)
		if err != nil {
			t.Fatalf("CrashTick(%d): %v", c, err)
		}
		if table[n] < c {
			t.Errorf("C=%d: M(n_c=%d) = %d < C", c, n, table[n])
		}
		if n > 0 && table[n-1] >= c {
			t.Errorf("C=%d: M(n_c-1=%d) = %d >= C", c, n-1, table[n-1])
		}
	}
}

func TestCashOutTimingDeadlineEqualityLoses(t *testing.T) {
	const t0 int64 = 1_760_000_000_000_000
	for _, c := range []int64{10_001, 10_007, 17_577, 100_000_000} {
		n, err := CrashTick(c)
		if err != nil {
			t.Fatalf("CrashTick(%d): %v", c, err)
		}
		d, err := DeadlineOffsetMicros(n)
		if err != nil {
			t.Fatalf("DeadlineOffsetMicros(%d): %v", n, err)
		}
		if tick, ok, err := CashOutTiming(t0, t0+d, n); err != nil || ok || tick != 0 {
			t.Errorf("C=%d: e = D gives %d, %v, %v; want ineligible", c, tick, ok, err)
		}
		if tick, ok, err := CashOutTiming(t0, t0+d-1, n); err != nil || !ok || tick != n-1 {
			t.Errorf("C=%d: e = D-1 gives %d, %v, %v; want eligible at tick %d", c, tick, ok, err, n-1)
		}
	}
}

func TestHouseEdgeBounds(t *testing.T) {
	var p paramsFile
	loadVectors(t, "params.json", &p)
	table := multiplierTable(t)

	two64 := new(big.Int).Lsh(big.NewInt(1), 64)
	edge := new(big.Int).Mul(big.NewInt(EdgeNumerator), two64)
	var lo, hi *big.Rat
	var loTick, hiTick int64
	// Payable ticks are 0 to MaxTick-1: no cash-out reaches the cap.
	for n := range MaxTick {
		m := table[n]
		survive := new(big.Int).Quo(edge, big.NewInt(m+1)) // floor: all values positive
		rtp := new(big.Rat).Mul(big.NewRat(m, Scale), new(big.Rat).SetFrac(survive, two64))
		if lo == nil || rtp.Cmp(lo) < 0 {
			lo, loTick = rtp, n
		}
		if hi == nil || rtp.Cmp(hi) > 0 {
			hi, hiTick = rtp, n
		}
	}
	if loTick != 0 || hiTick != MaxTick-1 {
		t.Errorf("RTP minimum at tick %d and maximum at tick %d; want 0 and %d", loTick, hiTick, MaxTick-1)
	}
	if loTick != p.RTP.MinTick || hiTick != p.RTP.MaxTick {
		t.Errorf("RTP ticks %d/%d, params.json %d/%d", loTick, hiTick, p.RTP.MinTick, p.RTP.MaxTick)
	}
	// FloatString rounds halves away from zero; neither bound is a half at 20 places.
	if got := lo.FloatString(20); got != p.RTP.Min {
		t.Errorf("RTP minimum = %s, params.json = %s", got, p.RTP.Min)
	}
	if got := hi.FloatString(20); got != p.RTP.Max {
		t.Errorf("RTP maximum = %s, params.json = %s", got, p.RTP.Max)
	}
	floor := big.NewRat(9_899, 10_000)
	ceil := big.NewRat(99, 100)
	if lo.Cmp(floor) < 0 || hi.Cmp(ceil) >= 0 {
		t.Errorf("RTP range [%s, %s] not inside [0.9899, 0.99)", lo.FloatString(20), hi.FloatString(20))
	}
}

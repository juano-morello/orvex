package v1

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cockroachdb/apd/v3"
)

// The golden vectors under testdata/golden are produced by the independent
// Python oracle generate.py (spec section 8). Never regenerate them from Go.

type paramsFile struct {
	RulesVersion        string `json:"rulesVersion"`
	Scale               int64  `json:"scale"`
	TickMicros          int64  `json:"tickMicros"`
	GrowthPerTick       string `json:"growthPerTick"`
	EdgeNumerator       int64  `json:"edgeNumerator"`
	MinCrashPointScaled int64  `json:"minCrashPointScaled"`
	MaxCrashPointScaled int64  `json:"maxCrashPointScaled"`
	MaxTick             int64  `json:"maxTick"`
	MaxDurationMicros   int64  `json:"maxDurationMicros"`
	MinBetCredits       int64  `json:"minBetCredits"`
	MaxBetCredits       int64  `json:"maxBetCredits"`
	MaxPayoutCredits    int64  `json:"maxPayoutCredits"`
	MaxStartMicros      int64  `json:"maxStartMicros"`
	OutcomeTag          string `json:"outcomeTag"`
	ApdModule           string `json:"apdModule"`
	ApdVersion          string `json:"apdVersion"`
	ApdContext          struct {
		Precision   uint32 `json:"precision"`
		MaxExponent int32  `json:"maxExponent"`
		MinExponent int32  `json:"minExponent"`
		Rounding    string `json:"rounding"`
		Traps       string `json:"traps"`
	} `json:"apdContext"`
	Oracle struct {
		Precision           int    `json:"precision"`
		CrossCheckPrecision int    `json:"crossCheckPrecision"`
		MinMarginToInteger  string `json:"minMarginToInteger"`
		MinMarginTick       int64  `json:"minMarginTick"`
	} `json:"oracle"`
	RTP struct {
		Min     string `json:"min"`
		MinTick int64  `json:"minTick"`
		Max     string `json:"max"`
		MaxTick int64  `json:"maxTick"`
	} `json:"rtp"`
}

type multiplierTicksFile struct {
	RulesVersion      string  `json:"rulesVersion"`
	MaxTick           int64   `json:"maxTick"`
	MultipliersScaled []int64 `json:"multipliersScaled"`
}

type crashPointsFile struct {
	RulesVersion string `json:"rulesVersion"`
	Cases        []struct {
		Name                 string `json:"name"`
		RHex                 string `json:"rHex"`
		Raw                  string `json:"raw"`
		CrashPointScaled     int64  `json:"crashPointScaled"`
		CrashTick            int64  `json:"crashTick"`
		DeadlineOffsetMicros int64  `json:"deadlineOffsetMicros"`
	} `json:"cases"`
}

type fairnessFile struct {
	RulesVersion string `json:"rulesVersion"`
	Cases        []struct {
		Name                 string `json:"name"`
		SeedHex              string `json:"seedHex"`
		RoundID              string `json:"roundID"`
		CommitmentHex        string `json:"commitmentHex"`
		MessageHex           string `json:"messageHex"`
		OutcomeHashHex       string `json:"outcomeHashHex"`
		RHex                 string `json:"rHex"`
		CrashPointScaled     int64  `json:"crashPointScaled"`
		CrashTick            int64  `json:"crashTick"`
		DeadlineOffsetMicros int64  `json:"deadlineOffsetMicros"`
	} `json:"cases"`
	CommitmentChecks []struct {
		Name          string `json:"name"`
		SeedHex       string `json:"seedHex"`
		CommitmentHex string `json:"commitmentHex"`
		Matches       bool   `json:"matches"`
	} `json:"commitmentChecks"`
}

type deadlineCase struct {
	Name             string `json:"name"`
	CrashPointScaled *int64 `json:"crashPointScaled"`
	CrashTick        int64  `json:"crashTick"`
	StartMicros      int64  `json:"startMicros"`
	EffectiveMicros  int64  `json:"effectiveMicros"`
	Error            string `json:"error"`
	Eligible         *bool  `json:"eligible"`
	Tick             *int64 `json:"tick"`
	MultiplierScaled *int64 `json:"multiplierScaled"`
}

type deadlinesFile struct {
	RulesVersion string         `json:"rulesVersion"`
	Cases        []deadlineCase `json:"cases"`
}

type payoutCase struct {
	Name             string `json:"name"`
	BetCredits       int64  `json:"betCredits"`
	MultiplierScaled int64  `json:"multiplierScaled"`
	PayoutCredits    *int64 `json:"payoutCredits"`
	Error            string `json:"error"`
}

type payoutsFile struct {
	RulesVersion string       `json:"rulesVersion"`
	Cases        []payoutCase `json:"cases"`
}

const errOutOfRangeText = "out_of_range"

// decodeVectors fails closed: it rejects any document whose rulesVersion is
// missing or is not RulesVersion, and any field dst does not declare.
func decodeVectors(data []byte, dst any) error {
	var header struct {
		RulesVersion string `json:"rulesVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("decode header: %w", err)
	}
	if header.RulesVersion != RulesVersion {
		return fmt.Errorf("rulesVersion %q, want %q", header.RulesVersion, RulesVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode vectors: %w", err)
	}
	return nil
}

func loadVectors(t testing.TB, name string, dst any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeVectors(data, dst); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// fullMultiplierTable computes M(n) for every tick once per test binary; a full
// table costs several seconds under -race.
var fullMultiplierTable = sync.OnceValues(func() ([]int64, error) {
	table := make([]int64, MaxTick+1)
	for n := range table {
		m, err := MultiplierAtTick(int64(n))
		if err != nil {
			return nil, fmt.Errorf("tick %d: %w", n, err)
		}
		table[n] = m
	}
	return table, nil
})

func multiplierTable(t *testing.T) []int64 {
	t.Helper()
	table, err := fullMultiplierTable()
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func isLowerHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func parseRoundID(t *testing.T, s string) [16]byte {
	t.Helper()
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		t.Fatalf("round ID %q is not canonical 8-4-4-4-12 text", s)
	}
	digits := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if !isLowerHex(digits) {
		t.Fatalf("round ID %q is not lowercase hex", s)
	}
	var out [16]byte
	if _, err := hex.Decode(out[:], []byte(digits)); err != nil {
		t.Fatalf("round ID %q: %v", s, err)
	}
	return out
}

func parseHex32(t *testing.T, s string) [32]byte {
	t.Helper()
	if len(s) != 64 || !isLowerHex(s) {
		t.Fatalf("%q is not 64 lowercase hex characters", s)
	}
	var out [32]byte
	if _, err := hex.Decode(out[:], []byte(s)); err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	return out
}

func parseR(t *testing.T, s string) uint64 {
	t.Helper()
	if len(s) != 16 || !isLowerHex(s) {
		t.Fatalf("rHex %q is not 16 lowercase hex characters", s)
	}
	r, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		t.Fatalf("rHex %q: %v", s, err)
	}
	return r
}

// moduleRootFile reads name from the module root, the nearest ancestor
// directory of the package that holds go.mod.
func moduleRootFile(t *testing.T, name string) []byte {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package directory")
		}
		dir = parent
	}
}

// pinnedModuleVersion returns the version go.mod requires for module and fails
// if go.mod replaces or excludes it.
func pinnedModuleVersion(t *testing.T, module string) string {
	t.Helper()
	var version string
	block := ""
	for _, line := range strings.Split(string(moduleRootFile(t, "go.mod")), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
			continue
		case block != "" && fields[0] == ")":
			block = ""
			continue
		case block == "" && len(fields) == 2 && fields[1] == "(":
			block = fields[0]
			continue
		}
		directive := block
		if directive == "" {
			directive, fields = fields[0], fields[1:]
		}
		if len(fields) == 0 || fields[0] != module {
			continue
		}
		switch directive {
		case "require":
			if len(fields) != 2 {
				t.Fatalf("go.mod: malformed require for %s", module)
			}
			version = fields[1]
		case "replace", "exclude":
			t.Fatalf("go.mod must not %s %s", directive, module)
		}
	}
	if version == "" {
		t.Fatalf("go.mod does not require %s", module)
	}
	return version
}

// apdCompiledVersion returns the version of the apd module compiled into the
// test binary. go test binaries of library packages carry no dependency list in
// debug.ReadBuildInfo, so the version is read from the recorded source path of
// compiled apd code, which ends in "<module>@<version>/<file>" in the module
// cache, with or without -trimpath. A replaced or vendored apd has no
// "@<version>" element and fails.
func apdCompiledVersion(t *testing.T) string {
	t.Helper()
	const module = "github.com/cockroachdb/apd/v3"
	fn := runtime.FuncForPC(reflect.ValueOf(apd.New).Pointer())
	if fn == nil {
		t.Fatal("no runtime function information for apd.New")
	}
	file, _ := fn.FileLine(fn.Entry())
	file = filepath.ToSlash(file)
	_, rest, ok := strings.Cut(file, module+"@")
	if !ok {
		t.Fatalf("compiled apd.New comes from %q, not from %s@<version>", file, module)
	}
	version, _, ok := strings.Cut(rest, "/")
	if !ok || version == "" {
		t.Fatalf("compiled apd.New source path %q has no module version", file)
	}
	return version
}

func TestGoldenParams(t *testing.T) {
	var p paramsFile
	loadVectors(t, "params.json", &p)

	ints := []struct {
		name      string
		got, want int64
	}{
		{"scale", p.Scale, Scale},
		{"tickMicros", p.TickMicros, TickMicros},
		{"edgeNumerator", p.EdgeNumerator, EdgeNumerator},
		{"minCrashPointScaled", p.MinCrashPointScaled, MinCrashPointScaled},
		{"maxCrashPointScaled", p.MaxCrashPointScaled, MaxCrashPointScaled},
		{"maxTick", p.MaxTick, MaxTick},
		{"maxDurationMicros", p.MaxDurationMicros, MaxDurationMicros},
		{"minBetCredits", p.MinBetCredits, MinBetCredits},
		{"maxBetCredits", p.MaxBetCredits, MaxBetCredits},
		{"maxPayoutCredits", p.MaxPayoutCredits, MaxPayoutCredits},
		{"maxStartMicros", p.MaxStartMicros, MaxStartMicros},
	}
	for _, c := range ints {
		if c.got != c.want {
			t.Errorf("params.json %s = %d, Go constant = %d", c.name, c.got, c.want)
		}
	}
	if p.OutcomeTag != OutcomeTag {
		t.Errorf("outcomeTag = %q, Go = %q", p.OutcomeTag, OutcomeTag)
	}
	if g := apd.New(growthCoefficient, growthExponent).String(); p.GrowthPerTick != g {
		t.Errorf("growthPerTick = %q, Go = %q", p.GrowthPerTick, g)
	}
	if Scale != 10_000 || apd.New(1, scaleExponent).String() != "1E+4" {
		t.Errorf("scaleExponent %d does not match Scale %d", scaleExponent, Scale)
	}

	ctx := ArithmeticContext()
	if ctx.Precision != p.ApdContext.Precision {
		t.Errorf("precision = %d, params.json = %d", ctx.Precision, p.ApdContext.Precision)
	}
	if ctx.MaxExponent != p.ApdContext.MaxExponent {
		t.Errorf("maxExponent = %d, params.json = %d", ctx.MaxExponent, p.ApdContext.MaxExponent)
	}
	if ctx.MinExponent != p.ApdContext.MinExponent {
		t.Errorf("minExponent = %d, params.json = %d", ctx.MinExponent, p.ApdContext.MinExponent)
	}
	if string(ctx.Rounding) != p.ApdContext.Rounding {
		t.Errorf("rounding = %q, params.json = %q", ctx.Rounding, p.ApdContext.Rounding)
	}
	traps := map[string]apd.Condition{"DefaultTraps": apd.DefaultTraps}
	want, ok := traps[p.ApdContext.Traps]
	if !ok {
		t.Errorf("unknown traps %q in params.json", p.ApdContext.Traps)
	} else if ctx.Traps != want {
		t.Errorf("traps = %v, params.json %s = %v", ctx.Traps, p.ApdContext.Traps, want)
	}

	if got := pinnedModuleVersion(t, p.ApdModule); got != p.ApdVersion {
		t.Errorf("go.mod pins %s %s, params.json pins %s", p.ApdModule, got, p.ApdVersion)
	}
	if p.ApdModule != "github.com/cockroachdb/apd/v3" {
		t.Errorf("params.json apdModule = %q, the package imports github.com/cockroachdb/apd/v3", p.ApdModule)
	}
	if got := apdCompiledVersion(t); got != p.ApdVersion {
		t.Errorf("test binary compiles %s %s, params.json pins %s", p.ApdModule, got, p.ApdVersion)
	}
}

func TestGoldenMultiplierTable(t *testing.T) {
	var f multiplierTicksFile
	loadVectors(t, "multiplier_ticks.json", &f)
	if f.MaxTick != MaxTick || int64(len(f.MultipliersScaled)) != MaxTick+1 {
		t.Fatalf("vector maxTick %d with %d values, want %d with %d", f.MaxTick, len(f.MultipliersScaled), MaxTick, MaxTick+1)
	}
	table := multiplierTable(t)
	for n, want := range f.MultipliersScaled {
		if table[n] != want {
			t.Fatalf("first difference at tick %d: MultiplierAtTick = %d, vector = %d", n, table[n], want)
		}
	}
}

func TestGoldenCrashPoints(t *testing.T) {
	var f crashPointsFile
	loadVectors(t, "crash_points.json", &f)
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range f.Cases {
		r := parseR(t, c.RHex)
		if got := crashPointRaw(r).String(); got != c.Raw {
			t.Errorf("%s: raw = %s, want %s", c.Name, got, c.Raw)
		}
		if got := CrashPointFromR(r); got != c.CrashPointScaled {
			t.Errorf("%s: CrashPointFromR = %d, want %d", c.Name, got, c.CrashPointScaled)
		}
		ct, err := CrashTick(c.CrashPointScaled)
		if err != nil || ct != c.CrashTick {
			t.Errorf("%s: CrashTick = %d, %v, want %d", c.Name, ct, err, c.CrashTick)
		}
		d, err := DeadlineOffsetMicros(c.CrashTick)
		if err != nil || d != c.DeadlineOffsetMicros {
			t.Errorf("%s: DeadlineOffsetMicros = %d, %v, want %d", c.Name, d, err, c.DeadlineOffsetMicros)
		}
	}
}

func TestGoldenFairness(t *testing.T) {
	var f fairnessFile
	loadVectors(t, "fairness.json", &f)
	if len(f.Cases) == 0 || len(f.CommitmentChecks) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range f.Cases {
		seed := parseHex32(t, c.SeedHex)
		rid := parseRoundID(t, c.RoundID)
		if got := Commitment(seed); got != parseHex32(t, c.CommitmentHex) {
			t.Errorf("%s: Commitment = %x, want %s", c.Name, got, c.CommitmentHex)
		}
		if got := hex.EncodeToString(OutcomeMessage(rid)); got != c.MessageHex {
			t.Errorf("%s: OutcomeMessage = %s, want %s", c.Name, got, c.MessageHex)
		}
		if got := outcomeHash(seed, rid); got != parseHex32(t, c.OutcomeHashHex) {
			t.Errorf("%s: outcome hash = %x, want %s", c.Name, got, c.OutcomeHashHex)
		}
		if got := OutcomeR(seed, rid); got != parseR(t, c.RHex) {
			t.Errorf("%s: OutcomeR = %016x, want %s", c.Name, got, c.RHex)
		}
		if got := CrashPoint(seed, rid); got != c.CrashPointScaled {
			t.Errorf("%s: CrashPoint = %d, want %d", c.Name, got, c.CrashPointScaled)
		}
		ct, err := CrashTick(c.CrashPointScaled)
		if err != nil || ct != c.CrashTick {
			t.Errorf("%s: CrashTick = %d, %v, want %d", c.Name, ct, err, c.CrashTick)
		}
		d, err := DeadlineOffsetMicros(c.CrashTick)
		if err != nil || d != c.DeadlineOffsetMicros {
			t.Errorf("%s: DeadlineOffsetMicros = %d, %v, want %d", c.Name, d, err, c.DeadlineOffsetMicros)
		}
	}
	for _, c := range f.CommitmentChecks {
		seed := parseHex32(t, c.SeedHex)
		commitment := parseHex32(t, c.CommitmentHex)
		if got := VerifyCommitment(seed, commitment); got != c.Matches {
			t.Errorf("%s: VerifyCommitment = %v, want %v", c.Name, got, c.Matches)
		}
	}
}

func TestGoldenDeadlines(t *testing.T) {
	var f deadlinesFile
	loadVectors(t, "deadlines.json", &f)
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range f.Cases {
		if c.CrashPointScaled != nil {
			ct, err := CrashTick(*c.CrashPointScaled)
			if err != nil || ct != c.CrashTick {
				t.Errorf("%s: CrashTick(%d) = %d, %v, want %d", c.Name, *c.CrashPointScaled, ct, err, c.CrashTick)
			}
		}
		tick, eligible, err := CashOutTiming(c.StartMicros, c.EffectiveMicros, c.CrashTick)
		switch {
		case c.Error == errOutOfRangeText:
			if !errors.Is(err, ErrOutOfRange) || eligible || tick != 0 {
				t.Errorf("%s: CashOutTiming = %d, %v, %v, want ErrOutOfRange", c.Name, tick, eligible, err)
			}
		case c.Error != "":
			t.Errorf("%s: unknown error kind %q", c.Name, c.Error)
		case c.Eligible == nil:
			t.Errorf("%s: row has neither error nor eligible", c.Name)
		case err != nil:
			t.Errorf("%s: CashOutTiming error %v", c.Name, err)
		case eligible != *c.Eligible:
			t.Errorf("%s: eligible = %v, want %v", c.Name, eligible, *c.Eligible)
		case eligible:
			if c.Tick == nil || c.MultiplierScaled == nil {
				t.Fatalf("%s: eligible row lacks tick or multiplierScaled", c.Name)
			}
			if tick != *c.Tick {
				t.Errorf("%s: tick = %d, want %d", c.Name, tick, *c.Tick)
			}
			m, err := MultiplierAtTick(tick)
			if err != nil || m != *c.MultiplierScaled {
				t.Errorf("%s: MultiplierAtTick(%d) = %d, %v, want %d", c.Name, tick, m, err, *c.MultiplierScaled)
			}
		default:
			if tick != 0 {
				t.Errorf("%s: ineligible tick = %d, want 0", c.Name, tick)
			}
		}
	}
}

func TestGoldenPayouts(t *testing.T) {
	var f payoutsFile
	loadVectors(t, "payouts.json", &f)
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range f.Cases {
		got, err := Payout(c.BetCredits, c.MultiplierScaled)
		switch {
		case c.Error == errOutOfRangeText:
			if !errors.Is(err, ErrOutOfRange) {
				t.Errorf("%s: Payout = %d, %v, want ErrOutOfRange", c.Name, got, err)
			}
		case c.Error != "":
			t.Errorf("%s: unknown error kind %q", c.Name, c.Error)
		case c.PayoutCredits == nil:
			t.Errorf("%s: row has neither error nor payoutCredits", c.Name)
		case err != nil || got != *c.PayoutCredits:
			t.Errorf("%s: Payout = %d, %v, want %d", c.Name, got, err, *c.PayoutCredits)
		}
	}
}

func TestGoldenRejectsUnknownRulesVersion(t *testing.T) {
	for _, doc := range []string{
		`{"rulesVersion":"crash/v2","cases":[]}`,
		`{"cases":[]}`,
	} {
		var f payoutsFile
		if err := decodeVectors([]byte(doc), &f); err == nil {
			t.Errorf("decodeVectors accepted %s", doc)
		}
	}
	var f payoutsFile
	if err := decodeVectors([]byte(`{"rulesVersion":"crash/v1","cases":[],"extra":1}`), &f); err == nil {
		t.Error("decodeVectors accepted an unknown field")
	}
}

package v1

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
)

// fuzzOp encodes one FuzzRoundLifecycle op: kind (0 PlaceBet, 1 StartRound,
// 2 CashOut, 3 CloseRound, 4 SettleRound, 5 wrong authority for kind b1%5),
// b1 selecting player and amount, and b2 the clock advance, in units of 1 µs,
// or 100 ms when fast is set.
func fuzzOp(kind int, fast bool, b1, b2 byte) []byte {
	b0 := byte(kind)
	if fast {
		// 128 % 6 == 2, so 128 + ((kind+4) % 6) has kind kind.
		b0 = 128 + byte((kind+4)%6)
	}
	return []byte{b0, b1, b2}
}

func fuzzScript(ops ...[]byte) []byte {
	var s []byte
	for _, op := range ops {
		s = append(s, op...)
	}
	return s
}

var fuzzAmounts = []int64{0, 1, 7, 1_000, 1_000_000_000, 1_000_000_001}

// fuzzCommand decodes one op into a command and the clock advance.
func fuzzCommand(b0, b1, b2 byte) (Command, int64) {
	kind := int(b0 % 6)
	wrong := kind == 5
	if wrong {
		kind = int(b1 % 5)
	}
	player := PlayerID("p" + string(rune('0'+b1%4)))
	amount := fuzzAmounts[int(b1>>2)%6]
	advance := int64(b2)
	if b0&0x80 != 0 {
		advance *= 100_000
	}

	auth := func(right Authority) Authority {
		if !wrong {
			return right
		}
		if b2&1 == 0 {
			return AuthorityNone
		}
		if right == AuthorityPlayer {
			return AuthoritySystem
		}
		return AuthorityPlayer
	}
	switch kind {
	case 0:
		return PlaceBet{Authority: auth(AuthorityPlayer), Player: player, BetCredits: amount}, advance
	case 1:
		return StartRound{Authority: auth(AuthoritySystem)}, advance
	case 2:
		return CashOut{Authority: auth(AuthorityPlayer), Player: player}, advance
	case 3:
		return CloseRound{Authority: auth(AuthoritySystem)}, advance
	default:
		return SettleRound{Authority: auth(AuthoritySystem)}, advance
	}
}

// checkRoundInvariants checks next against itself and against prev, the
// state before the last applied event.
func checkRoundInvariants(t *testing.T, prev, next State) {
	t.Helper()
	if !validPhase(next.Phase) || next.Phase < prev.Phase {
		t.Fatalf("phase %v after %v", next.Phase, prev.Phase)
	}
	if int64(len(next.Bets)) > next.MaxBets {
		t.Fatalf("%d bets above the limit %d", len(next.Bets), next.MaxBets)
	}
	if len(next.Bets) < len(prev.Bets) {
		t.Fatalf("bets shrank from %d to %d", len(prev.Bets), len(next.Bets))
	}

	seen := map[PlayerID]bool{}
	var totalBet, totalPayout int64
	for i, b := range next.Bets {
		if seen[b.Player] {
			t.Fatalf("player %s has two bets", b.Player)
		}
		seen[b.Player] = true
		totalBet += b.BetCredits
		totalPayout += b.PayoutCredits

		if i < len(prev.Bets) {
			p := prev.Bets[i]
			if p.Player != b.Player || p.BetCredits != b.BetCredits || p.PlacedAtUnixMicro != b.PlacedAtUnixMicro {
				t.Fatalf("bet %d changed identity: %+v -> %+v", i, p, b)
			}
			if p.Status != b.Status && p.Status != StatusOpen {
				t.Fatalf("bet %d moved from status %d to %d", i, p.Status, b.Status)
			}
			if p.Status == StatusCashedOut && p != b {
				t.Fatalf("cashed-out bet %d changed: %+v -> %+v", i, p, b)
			}
		}

		switch b.Status {
		case StatusOpen, StatusLost:
			if b.CashedOutAtUnixMicro != 0 || b.CashOutTick != 0 || b.CashOutMultiplierScaled != 0 || b.PayoutCredits != 0 {
				t.Fatalf("unpaid bet has cash-out fields: %+v", b)
			}
			if b.Status == StatusLost && next.Phase != PhaseCrashed && next.Phase != PhaseSettled {
				t.Fatalf("lost bet in phase %v", next.Phase)
			}
			if b.Status == StatusOpen && next.Phase == PhaseSettled {
				t.Fatalf("open bet in a settled round: %+v", b)
			}
		case StatusCashedOut:
			if b.CashedOutAtUnixMicro <= next.StartedAtUnixMicro || b.CashedOutAtUnixMicro >= next.CrashDeadlineUnixMicro {
				t.Fatalf("cash-out at %d outside (%d, %d)", b.CashedOutAtUnixMicro, next.StartedAtUnixMicro, next.CrashDeadlineUnixMicro)
			}
			if b.CashOutTick != (b.CashedOutAtUnixMicro-next.StartedAtUnixMicro)/TickMicros || b.CashOutTick >= next.CrashTick {
				t.Fatalf("cash-out tick %d does not match its time or reaches the crash tick", b.CashOutTick)
			}
			if b.CashOutMultiplierScaled >= next.CrashPointScaled {
				t.Fatalf("cash-out multiplier %d reaches the crash point", b.CashOutMultiplierScaled)
			}
		default:
			t.Fatalf("bet with invalid status: %+v", b)
		}
	}
	if totalBet != next.TotalBetCredits || totalPayout != next.TotalPayoutCredits {
		t.Fatalf("totals %d/%d, want sums %d/%d", next.TotalBetCredits, next.TotalPayoutCredits, totalBet, totalPayout)
	}

	p, err := Project(next)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	switch next.Phase {
	case PhaseBetting, PhaseRunning:
		assertHidden(t, p)
	case PhaseCrashed:
		if p.Seed != ([32]byte{}) || p.CrashPointScaled != next.CrashPointScaled || p.CrashTick != next.CrashTick ||
			p.CrashDeadlineOffsetMicros != next.CrashDeadlineOffsetMicros || p.CrashDeadlineUnixMicro != next.CrashDeadlineUnixMicro {
			t.Fatalf("crashed projection = %+v", p)
		}
	case PhaseSettled:
		if p.Seed != next.Seed {
			t.Fatalf("settled projection hides the seed")
		}
	}
}

// checkSettledFrozen asserts that no command or event moves a settled round.
func checkSettledFrozen(t *testing.T, s State, history []Event, at int64) {
	t.Helper()
	snap := takeRoundSnapshot(s)
	for _, a := range []Authority{AuthorityPlayer, AuthoritySystem} {
		for _, c := range []Command{
			PlaceBet{Authority: a, Player: "p0", BetCredits: 1},
			StartRound{Authority: a},
			CashOut{Authority: a, Player: "p0"},
			CloseRound{Authority: a},
			SettleRound{Authority: a},
		} {
			events, err := Rules{}.Decide(s, c, inputAt(at))
			if r, ok := RejectionReason(err); events != nil || !ok || r != ReasonRoundSettled {
				t.Fatalf("Decide(%#v) on a settled round = %v, %v", c, events, err)
			}
		}
	}
	for _, e := range history {
		if _, err := (Rules{}).Evolve(s, e); !errors.Is(err, ErrEventNotApplicable) {
			t.Fatalf("Evolve(%#v) on a settled round = %v", e, err)
		}
	}
	if snap.changed(s) {
		t.Fatal("settled state changed")
	}
}

func FuzzRoundLifecycle(f *testing.F) {
	bet := func(player, amountIdx byte) []byte { return fuzzOp(0, false, player|amountIdx<<2, 1) }
	start := fuzzOp(1, false, 0, 1)
	cash := func(player, tenths byte) []byte { return fuzzOp(2, true, player, tenths) }
	closeOp := fuzzOp(3, true, 0, 255)
	settle := fuzzOp(4, false, 0, 1)

	f.Add([]byte("worked"), fuzzScript(bet(0, 3), bet(1, 3), bet(2, 2), start,
		cash(0, 3), cash(1, 5), closeOp, closeOp, settle))
	f.Add([]byte("double cash-out"), fuzzScript(bet(0, 3), bet(1, 4), start,
		cash(0, 1), cash(0, 1), cash(1, 1), cash(1, 255), closeOp, settle, settle))
	f.Add([]byte("bet limit"), fuzzScript(bet(0, 1), bet(1, 2), bet(2, 3), bet(3, 4), bet(0, 5), bet(3, 0), start, settle))
	f.Add([]byte("wrong authority"), fuzzScript(bet(0, 3),
		fuzzOp(5, false, 0, 0), fuzzOp(5, false, 1, 1), fuzzOp(5, false, 2, 2), fuzzOp(5, false, 3, 3),
		fuzzOp(5, false, 4, 4), fuzzOp(5, false, 5, 5),
		start, fuzzOp(5, false, 3, 1), fuzzOp(5, false, 2, 0), closeOp, fuzzOp(5, false, 4, 1), settle))
	idle := [][]byte{bet(0, 3), bet(1, 3), start}
	for range 20 {
		idle = append(idle, fuzzOp(2, true, 0, 255))
	}
	f.Add([]byte("idle"), fuzzScript(append(idle, closeOp, settle)...))
	f.Add([]byte("empty"), []byte{})

	f.Fuzz(func(t *testing.T, seedInput, script []byte) {
		seed := sha256.Sum256(seedInput)
		if seed == ([32]byte{}) {
			t.Skip()
		}
		cfg := testConfig(seed)
		cfg.MaxBets = 3
		s := newRound(t, cfg)
		clock := testT0 - 1_000_000
		var history []Event

		apply := func(c Command) {
			t.Helper()
			in := inputAt(clock)
			if v := roundPurityViolation(Rules{}, s, c, in); v != "" {
				t.Fatalf("%#v at %d: %s", c, clock, v)
			}
			events, err := Rules{}.Decide(s, c, in)
			if err != nil {
				if events != nil || !errors.Is(err, ErrRejected) {
					t.Fatalf("Decide(%#v at %d) = %v, %v; want a rejection", c, clock, events, err)
				}
				return
			}
			for _, e := range events {
				next, err := Rules{}.Evolve(s, e)
				if err != nil {
					t.Fatalf("Evolve(%#v) of a decided event: %v", e, err)
				}
				checkRoundInvariants(t, s, next)
				s = next
				history = append(history, e)
			}
		}

		for i := 0; i+3 <= len(script); i += 3 {
			c, advance := fuzzCommand(script[i], script[i+1], script[i+2])
			clock += advance
			apply(c)
			if s.Phase == PhaseSettled {
				checkSettledFrozen(t, s, history, clock)
			}
		}

		mustApply := func(c Command) {
			t.Helper()
			before := s.Phase
			apply(c)
			if s.Phase == before {
				t.Fatalf("%#v at %d did not advance phase %v", c, clock, before)
			}
		}
		if s.Phase == PhaseBetting {
			mustApply(startRound())
		}
		if s.Phase == PhaseRunning {
			clock = max(clock, s.CrashDeadlineUnixMicro)
			mustApply(closeRound())
		}
		if s.Phase == PhaseCrashed {
			mustApply(settleRound())
		}
		if s.Phase != PhaseSettled || !VerifyCommitment(s.Seed, s.Commitment) {
			t.Fatalf("round did not settle verifiably: %+v", s)
		}
		checkSettledFrozen(t, s, history, clock)
		if !reflect.DeepEqual(s.Seed, seed) {
			t.Fatal("settled seed differs from the config seed")
		}
	})
}

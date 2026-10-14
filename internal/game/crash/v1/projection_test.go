package v1

import (
	"reflect"
	"testing"
)

func mustProject(t *testing.T, s State) Projection {
	t.Helper()
	p, err := Project(s)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	return p
}

// assertHidden fails unless p hides the crash outcome and the seed.
func assertHidden(t *testing.T, p Projection) {
	t.Helper()
	if p.CrashPointScaled != 0 || p.CrashTick != 0 || p.CrashDeadlineOffsetMicros != 0 || p.CrashDeadlineUnixMicro != 0 {
		t.Errorf("%v projection exposes the crash outcome: %+v", p.Phase, p)
	}
	if p.ClosedAtUnixMicro != 0 || p.SettledAtUnixMicro != 0 || p.Seed != ([32]byte{}) {
		t.Errorf("%v projection exposes close, settlement or seed: %+v", p.Phase, p)
	}
}

func TestProjectionHidesOutcomeBeforeCrash(t *testing.T) {
	script := []struct {
		c  Command
		at int64
	}{
		{placeBet("alice", 1_000), testT0 - 1_000},
		{placeBet("bob", 500), testT0 - 1_000},
		{startRound(), testT0},
		{cashOut("alice"), testT0 + 1_000_000},
		{cashOut("bob"), testT0 + 5_000_000},
	}
	worked := newRound(t, testConfig(workedOutcome().seed))
	long := newRound(t, testConfig(longOutcome().seed))
	check := func() {
		t.Helper()
		pw, pl := mustProject(t, worked), mustProject(t, long)
		assertHidden(t, pw)
		assertHidden(t, pl)
		pw.Commitment, pl.Commitment = [32]byte{}, [32]byte{}
		if !reflect.DeepEqual(pw, pl) {
			t.Errorf("projections of different outcomes differ beyond the commitment:\n%+v\n%+v", pw, pl)
		}
	}
	check()
	for _, st := range script {
		worked, _ = step(t, worked, st.c, st.at)
		long, _ = step(t, long, st.c, st.at)
		check()
	}
	if worked.Phase != PhaseRunning || worked.Bets[1].Status != StatusCashedOut {
		t.Fatalf("script did not reach two cash-outs while running: %+v", worked)
	}
}

func TestProjectionRevealsOutcomeAtCrashAndSeedAtSettlement(t *testing.T) {
	s := runningRound(t, workedSeed(), "alice")
	if p := mustProject(t, s); p.StartedAtUnixMicro != testT0 {
		t.Errorf("running projection StartedAt = %d, want T0", p.StartedAtUnixMicro)
	}

	crashed, _ := step(t, s, closeRound(), testT0+9_410_000)
	p := mustProject(t, crashed)
	if p.CrashPointScaled != crashed.CrashPointScaled || p.CrashTick != crashed.CrashTick ||
		p.CrashDeadlineOffsetMicros != crashed.CrashDeadlineOffsetMicros ||
		p.CrashDeadlineUnixMicro != crashed.CrashDeadlineUnixMicro || p.ClosedAtUnixMicro != crashed.ClosedAtUnixMicro {
		t.Errorf("crashed projection = %+v, want the state's outcome", p)
	}
	if p.Seed != ([32]byte{}) || p.SettledAtUnixMicro != 0 {
		t.Errorf("crashed projection exposes the seed: %+v", p)
	}

	settled, _ := step(t, crashed, settleRound(), testT0+9_420_000)
	p = mustProject(t, settled)
	if p.Seed != settled.Seed || p.SettledAtUnixMicro != settled.SettledAtUnixMicro {
		t.Errorf("settled projection = %+v, want the seed revealed", p)
	}
	if !VerifyCommitment(p.Seed, p.Commitment) {
		t.Error("revealed seed does not verify against the commitment")
	}
	if CrashPoint(p.Seed, p.RoundID) != p.CrashPointScaled {
		t.Error("revealed seed does not reproduce the crash point")
	}

	// Allow-listing: fields the phase does not publish stay zero even when
	// the state holds a value.
	forged := newRound(t, testConfig(workedSeed()))
	forged.StartedAtUnixMicro = testT0
	forged.CrashDeadlineUnixMicro = testT0 + 9_410_000
	forged.ClosedAtUnixMicro = testT0 + 9_410_000
	forged.SettledAtUnixMicro = testT0 + 9_420_000
	p = mustProject(t, forged)
	if p.StartedAtUnixMicro != 0 {
		t.Errorf("betting projection StartedAt = %d, want 0", p.StartedAtUnixMicro)
	}
	assertHidden(t, p)
}

func TestProjectionDoesNotAliasState(t *testing.T) {
	s := runningRound(t, workedSeed(), "alice", "bob")
	snap := takeRoundSnapshot(s)
	p := mustProject(t, s)
	p.Bets[0] = Bet{Player: "mallory", BetCredits: -1}
	if snap.changed(s) {
		t.Fatal("writing the projection's bets changed the state")
	}
}

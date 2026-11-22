package lifecycle

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/runtime/admission"
	"github.com/juano-morello/orvex/internal/runtime/execution"
)

// step is the lifecycle command that moves a round out of one phase.
type step struct {
	// name is the step's part of its command ID.
	name    string
	command v1.Command
	// due returns the earliest database-clock time, in Unix microseconds,
	// to admit the command.
	due func(r round, s v1.State, window time.Duration) int64
}

// steps maps each phase before PhaseSettled to its step. It is the only
// place the engine builds system-authority commands (ADR-004: lifecycle
// commands are trusted-only). Close is scheduled from the state's crash
// deadline, never from the stored close bound, which only
// orvex.admit_command reads.
var steps = map[v1.Phase]step{
	v1.PhaseBetting: {
		name: "start", command: v1.StartRound{Authority: v1.AuthoritySystem},
		due: func(r round, _ v1.State, window time.Duration) int64 {
			return r.createdAtUnixMicro + window.Microseconds()
		},
	},
	v1.PhaseRunning: {
		name: "close", command: v1.CloseRound{Authority: v1.AuthoritySystem},
		due: func(_ round, s v1.State, _ time.Duration) int64 { return s.CrashDeadlineUnixMicro },
	},
	v1.PhaseCrashed: {
		name: "settle", command: v1.SettleRound{Authority: v1.AuthoritySystem},
		due: func(round, v1.State, time.Duration) int64 { return 0 },
	},
}

const commandIDPrefix = "orvex/engine/v1/command:"

// commandID returns the command ID of step name of round roundID:
// uuid8("orvex/engine/v1/command:<round uuid>/<name>"), built like
// internal/ledger's IDs. Every engine process derives the same ID, so the
// admission deduplicates a step across restarts.
func commandID(roundID [16]byte, name string) [16]byte {
	sum := sha256.Sum256([]byte(commandIDPrefix + roundText(roundID) + "/" + name))
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = id[6]&0x0f | 0x80
	id[8] = id[8]&0x3f | 0x80
	return id
}

// drive claims r and drives it to settlement. It returns nil once r is
// settled, an error wrapping errHalted when r halts, and any other error to
// stop the scheduler.
func (s *Scheduler) drive(ctx context.Context, r round) error {
	failures := 0
	var lease execution.Lease
	for {
		var err error
		if lease, err = s.x.ClaimLease(ctx, r.id, s.owner, leaseTTL); err == nil {
			break
		}
		if err := s.failed(ctx, "claim round", err, &failures, "round", roundText(r.id)); err != nil {
			return err
		}
	}
	s.log.Info("round claimed", "round", roundText(r.id), "fencing_token", lease.FencingToken)
	d := &driver{s: s, round: r, lease: lease}
	failures = 0
	for {
		settled, err := d.tick(ctx)
		switch {
		case settled:
			return nil
		case err == nil:
			failures = 0
		default:
			if err := s.failed(ctx, "drive round", err, &failures, "round", roundText(r.id)); err != nil {
				return err
			}
		}
	}
}

// driver drives one round under its lease.
type driver struct {
	s     *Scheduler
	round round
	lease execution.Lease
	// phase is the last phase logged.
	phase v1.Phase
}

// tick executes the admitted commands, then admits the next lifecycle
// command if it is due, or waits up to one tick for it.
func (d *driver) tick(ctx context.Context) (settled bool, err error) {
	st, err := d.current(ctx)
	if err != nil {
		return false, err
	}
	if st.Phase == v1.PhaseSettled {
		return true, nil
	}
	next, ok := steps[st.Phase]
	if !ok {
		return false, fmt.Errorf("%w: phase %v has no lifecycle step", errHalted, st.Phase)
	}
	now, err := d.s.now(ctx)
	if err != nil {
		return false, err
	}
	if wait := next.due(d.round, st, d.s.cfg.BettingWindow) - now; wait > 0 {
		poll := d.s.timing.waiting
		if st.Phase == v1.PhaseRunning {
			poll = d.s.timing.running
		}
		return false, sleepCtx(ctx, min(time.Duration(wait)*time.Microsecond, poll))
	}
	return false, d.advance(ctx, next, st.Phase)
}

// current executes every admitted command and returns the round's state.
func (d *driver) current(ctx context.Context) (v1.State, error) {
	for {
		n, err := d.s.x.Drain(ctx, d.lease, drainBatch)
		if err != nil {
			return v1.State{}, err
		}
		if n < drainBatch {
			break
		}
	}
	got, err := d.s.x.State(ctx, d.lease)
	if err != nil {
		return v1.State{}, err
	}
	st, ok := got.(v1.State)
	if !ok {
		return v1.State{}, fmt.Errorf("%w: the round's state is not a crash/v1 state", errHalted)
	}
	if st.Phase != d.phase {
		d.phase = st.Phase
		d.s.log.Info("round phase", "round", roundText(d.round.id), "phase", st.Phase.String())
	}
	return st, nil
}

// advance admits next for a round in phase from and executes it. The round
// halts when it stays in from: the command was rejected at admission or by
// Decide.
func (d *driver) advance(ctx context.Context, next step, from v1.Phase) error {
	id := commandID(d.round.id, next.name)
	_, admitErr := d.s.seq.Admit(ctx, admission.Request{
		RoundID: d.round.id, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		CommandID: id, Command: next.command,
	})
	refused, isRefused := errors.AsType[*admission.RejectedError](admitErr)
	if admitErr != nil && !isRefused {
		return admitErr
	}
	st, err := d.current(ctx)
	if err != nil || st.Phase != from {
		return err
	}
	if isRefused {
		return fmt.Errorf("%w: %s refused at admission: %w", errHalted, next.name, refused)
	}
	status, err := d.s.seq.Lookup(ctx, id)
	switch {
	case err != nil:
		return err
	case status.Outcome == admission.OutcomeRejected:
		return fmt.Errorf("%w: %s rejected: %s", errHalted, next.name, status.Reason)
	case status.Outcome == admission.OutcomeAccepted:
		return fmt.Errorf("%w: %s accepted, and the round stayed in %v", errHalted, next.name, from)
	default:
		return errNotExecuted
	}
}

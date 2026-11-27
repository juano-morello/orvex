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
	driveCtx, cancel := context.WithCancelCause(ctx)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		d.renew(driveCtx, cancel)
	}()
	defer func() { cancel(nil); <-renewed }()
	failures = 0
	for {
		settled, err := d.tick(driveCtx)
		switch {
		case driveCtx.Err() != nil:
			return context.Cause(driveCtx)
		case settled:
			return nil
		case err == nil:
			failures = 0
		default:
			if err := s.failed(driveCtx, "drive round", err, &failures, "round", roundText(r.id)); err != nil {
				if driveCtx.Err() != nil {
					return context.Cause(driveCtx)
				}
				return err
			}
		}
	}
}

// Renew independently of replay and command batches, which may take longer than a lease.
func (d *driver) renew(ctx context.Context, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(leaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.s.x.RenewLease(ctx, d.lease, d.s.owner, leaseTTL); err != nil {
				if errors.Is(err, execution.ErrFenced) || errors.Is(err, execution.ErrRoundNotFound) {
					cancel(err)
					return
				}
				if ctx.Err() == nil {
					d.s.log.Warn("round renewal failed", "error", Describe(err))
				}
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
	// awaiting is the step admitted, or refused, out of the round's phase;
	// nil before one is.
	awaiting *awaiting
}

// awaiting is a lifecycle step out of phase from. The round must have left
// from once the admission at sequence through has executed. through is the
// step's own admission or, for a refused step, the round's last admission
// at the refusal, which may still move the round.
type awaiting struct {
	name    string
	from    v1.Phase
	through uint64
	// refused is the step's rejection at admission; nil when admitted.
	refused *admission.RejectedError
}

// batch is what one call of execute did.
type batch struct {
	// full: drainBatch commands executed; more may be admitted.
	full bool
	// last is the sequence of the last command executed, 0 for none.
	last uint64
	// final: the last command made the round final.
	final bool
}

// executed reports whether every admission through seq, committed before
// the batch began, has executed. A batch that ran out of admissions
// executed them all.
func (b batch) executed(seq uint64) bool { return !b.full || b.last >= seq }

// tick executes one batch of admitted commands, then admits the round's
// next lifecycle step if it is due, or waits up to one tick for it when
// nothing is left to execute. A due step is thus admitted at the next batch
// boundary even when admissions never stop, behind those admitted before
// it.
func (d *driver) tick(ctx context.Context) (settled bool, err error) {
	b, err := d.execute(ctx)
	switch {
	case err != nil:
		return false, err
	case b.final:
		// The executor dropped the round's state with its final record
		// (E-3); reading it again would replay the whole chain.
		d.logPhase(v1.PhaseSettled)
		return true, nil
	}
	st, err := d.state(ctx)
	if err != nil {
		return false, err
	}
	if st.Phase == v1.PhaseSettled {
		// The final record committed without this driver seeing it
		// executed, and a cold load cached the settled state.
		d.s.x.Forget(d.round.id)
		return true, nil
	}
	if a := d.awaiting; a != nil && a.from == st.Phase {
		if !b.executed(a.through) {
			return false, nil
		}
		return false, d.stalled(ctx, *a)
	}
	d.awaiting = nil
	next, ok := steps[st.Phase]
	if !ok {
		return false, fmt.Errorf("%w: phase %v has no lifecycle step", errHalted, st.Phase)
	}
	now, err := d.s.now(ctx)
	if err != nil {
		return false, err
	}
	if wait := next.due(d.round, st, d.s.cfg.BettingWindow) - now; wait > 0 {
		if b.full {
			return false, nil
		}
		poll := d.s.timing.waiting
		if st.Phase == v1.PhaseRunning {
			poll = d.s.timing.running
		}
		return false, sleepCtx(ctx, min(time.Duration(wait)*time.Microsecond, poll))
	}
	return false, d.admit(ctx, next, st.Phase)
}

// execute executes up to drainBatch admitted commands in sequence order,
// stopping after the final record.
func (d *driver) execute(ctx context.Context) (batch, error) {
	var b batch
	for range drainBatch {
		out, err := d.s.x.ExecuteNext(ctx, d.lease)
		if err != nil || !out.Executed {
			return b, err
		}
		b.last, b.final = out.Sequence, out.Final
		if out.Final {
			return b, nil
		}
	}
	b.full = true
	return b, nil
}

// state returns the round's state after its last executed command.
func (d *driver) state(ctx context.Context) (v1.State, error) {
	got, err := d.s.x.State(ctx, d.lease)
	if err != nil {
		return v1.State{}, err
	}
	st, ok := got.(v1.State)
	if !ok {
		return v1.State{}, fmt.Errorf("%w: the round's state is not a crash/v1 state", errHalted)
	}
	d.logPhase(st.Phase)
	return st, nil
}

func (d *driver) logPhase(p v1.Phase) {
	if p != d.phase {
		d.phase = p
		d.s.log.Info("round phase", "round", roundText(d.round.id), "phase", p.String())
	}
}

// admit admits next, the step out of phase from, and records the sequence
// it executes at. Admitting it again, after a restart or an unknown
// outcome, is Duplicate and returns the same sequence.
func (d *driver) admit(ctx context.Context, next step, from v1.Phase) error {
	a, err := d.s.seq.Admit(ctx, admission.Request{
		RoundID: d.round.id, Game: v1.GameID, RulesVersion: v1.RulesVersionNumber,
		CommandID: commandID(d.round.id, next.name), Command: next.command,
	})
	refused, isRefused := errors.AsType[*admission.RejectedError](err)
	switch {
	case isRefused:
		var head int64
		if err := d.s.pool.QueryRow(ctx, `SELECT admitted_sequence FROM orvex.round_admission_heads WHERE round_id = $1`,
			d.round.id).Scan(&head); err != nil {
			return err
		}
		d.awaiting = &awaiting{name: next.name, from: from, through: uint64(head), refused: refused} //nolint:gosec // G115: round_admission_heads_admitted_sequence_check holds admitted_sequence >= 0
	case err != nil:
		return err
	default:
		d.awaiting = &awaiting{name: next.name, from: from, through: a.Sequence}
	}
	return nil
}

// stalled handles a round still in a.from after the admission at a.through
// executed: its step was refused at admission or rejected by Decide, so the
// round halts.
func (d *driver) stalled(ctx context.Context, a awaiting) error {
	if a.refused != nil {
		return fmt.Errorf("%w: %s refused at admission: %w", errHalted, a.name, a.refused)
	}
	status, err := d.s.seq.Lookup(ctx, commandID(d.round.id, a.name))
	switch {
	case err != nil:
		return err
	case status.Outcome == admission.OutcomeRejected:
		return fmt.Errorf("%w: %s rejected: %s", errHalted, a.name, status.Reason)
	case status.Outcome == admission.OutcomeAccepted:
		return fmt.Errorf("%w: %s accepted, and the round stayed in %v", errHalted, a.name, a.from)
	default:
		return errNotExecuted
	}
}

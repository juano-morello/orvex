// Package replay re-executes a round's recorded history through the rules
// registry (ADR-001 state replay and decision replay; decisions A-13 and
// A-15 of docs/specs/8bebb70f_canonical-codec-audit-cli.md).
//
// State replay folds the recorded events through Evolve. Decision replay
// first runs Decide on each recorded command at its recorded input and
// compares the outcome with the record: a rejection by its codec reason
// code, an acceptance by the canonical bytes of every event. A fail-closed
// Decide error on a recorded command is a decision mismatch.
package replay

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/game"
)

// Mode selects what Run checks.
type Mode int

const (
	// StateOnly folds the recorded events through Evolve.
	StateOnly Mode = iota + 1
	// StateAndDecision also replays every decision before its events.
	StateAndDecision
)

// Record is one decoded command record.
type Record struct {
	// Sequence is the record's chain sequence, the admission sequence.
	Sequence uint64
	// Input is the deterministic input Decide saw.
	Input game.Input
	// Command is the decoded command, boxed as the adapter expects.
	Command any
	// Result is the recorded terminal result.
	Result chain.Result
	// Events are the decoded recorded events, in order.
	Events []any
	// EventBytes are the recorded canonical bytes of Events.
	EventBytes [][]byte
}

// Mismatch reports a record whose replay differs from the history.
// Decision is true for a decision-replay mismatch and false for a state
// replay (Evolve) failure.
type Mismatch struct {
	Decision bool
	Sequence uint64
	Err      error
}

func (m *Mismatch) Error() string {
	kind := "state"
	if m.Decision {
		kind = "decision"
	}
	return fmt.Sprintf("replay: %s mismatch at sequence %d: %v", kind, m.Sequence, m.Err)
}

func (m *Mismatch) Unwrap() error { return m.Err }

// Run replays records from initial, the state after sequence 0, through a,
// calling visit(sequence, state) after sequence 0 and after every record. A
// visit error stops the run and is returned as is; a replay difference is
// returned as a *Mismatch. visit may be nil.
func Run(a game.Adapter, initial any, records []Record, mode Mode, visit func(uint64, any) error) error {
	if mode != StateOnly && mode != StateAndDecision {
		return fmt.Errorf("replay: unknown mode %d", mode)
	}
	if visit == nil {
		visit = func(uint64, any) error { return nil }
	}
	s := initial
	if err := visit(0, s); err != nil {
		return err
	}
	for _, r := range records {
		if mode == StateAndDecision {
			if err := decide(a, s, r); err != nil {
				return &Mismatch{Decision: true, Sequence: r.Sequence, Err: err}
			}
		}
		for j, e := range r.Events {
			next, err := a.Evolve(s, e)
			if err != nil {
				return &Mismatch{Sequence: r.Sequence, Err: fmt.Errorf("replay: event %d: %w", j, err)}
			}
			s = next
		}
		if err := visit(r.Sequence, s); err != nil {
			return err
		}
	}
	return nil
}

// decide compares Decide's outcome for r at state s with the recorded one.
func decide(a game.Adapter, s any, r Record) error {
	events, err := a.Decide(s, r.Command, r.Input)
	if err != nil {
		code, ok := a.RejectionCode(err)
		switch {
		case !ok:
			return fmt.Errorf("replay: rules fail closed on a recorded command: %w", err)
		case !r.Result.Rejected:
			return fmt.Errorf("replay: rules reject with %q; the record is accepted", code)
		case code != r.Result.Reason:
			return fmt.Errorf("replay: rules reject with %q; the record is rejected with %q", code, r.Result.Reason)
		}
		return nil
	}
	if r.Result.Rejected {
		return fmt.Errorf("replay: rules accept; the record is rejected with %q", r.Result.Reason)
	}
	if len(events) != len(r.EventBytes) {
		return fmt.Errorf("replay: rules emit %d events; the record has %d", len(events), len(r.EventBytes))
	}
	for j, e := range events {
		b, err := a.EncodeEvent(e)
		if err != nil {
			return fmt.Errorf("replay: encode event %d: %w", j, err)
		}
		if !bytes.Equal(b, r.EventBytes[j]) {
			return errEventDiffers(j)
		}
	}
	return nil
}

// ErrEventDiffers reports a replayed event whose canonical bytes differ
// from the recorded event.
var ErrEventDiffers = errors.New("replay: event differs from the record")

func errEventDiffers(j int) error {
	return fmt.Errorf("%w: event %d", ErrEventDiffers, j)
}

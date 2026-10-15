package execution

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/game"
	"github.com/juano-morello/orvex/internal/ledger"
)

// rulesEntry is one rules version's adapter, execution policy and posting
// rule.
type rulesEntry struct {
	adapter game.Adapter
	policy  Policy
	rule    ledger.Rule
}

// roundState is the cached state of a round after record processed (E-3).
type roundState struct {
	state     any
	processed uint64
	head      chain.Hash
	// nextEventSequence is the sequence of the round's next event.
	nextEventSequence uint64
}

// holdRow is one credit hold of an admission (§5.3).
type holdRow struct {
	accountID [16]byte
	amount    int64
	// player is the account's player ID, "" for a non-player account.
	player string
}

// admissionRow is the admitted command read before Transaction B (E-4).
type admissionRow struct {
	sequence      uint64
	commandID     [16]byte
	effectiveTime int64
	command       []byte
	// holds are ordered by account ID.
	holds []holdRow
}

// postingRow is one posting of a transition. Accounts other than the hold's
// are resolved before Transaction B (§5.5).
type postingRow struct {
	ledger.Posting
	// holdAccount is the hold's account for a FromHold debit.
	holdAccount [16]byte
}

// transition is everything Transaction B writes for record sequence (§5.4).
type transition struct {
	sequence   uint64
	commandID  [16]byte
	record     []byte
	hash       chain.Hash
	result     chain.Result
	events     []any
	eventSeqs  []int64
	eventTypes []string
	next       any
	kind       string
	postings   []postingRow
	// releases are the admission's hold accounts, ascending.
	releases   [][16]byte
	checkpoint bool
	stateHash  chain.Hash
	final      bool
	// betPlayers memoizes player -> account for the FromHold postings.
	betPlayers map[string][16]byte
}

// computeTransition computes record st.processed + 1 of round roundID from
// admission adm (E-5). It is pure. A failure of the rules, codec or posting
// rule is a *FailClosedError; inconsistent holds are ErrCorrupt.
func computeTransition(r rulesEntry, roundID [16]byte, st roundState, adm admissionRow, engineBuild string) (*transition, error) {
	k := st.processed + 1
	failClosed := func(err error) (*transition, error) {
		return nil, &FailClosedError{RoundID: roundID, Sequence: k, Err: err}
	}
	if adm.sequence != k {
		return nil, fmt.Errorf("%w: admission %d read for record %d", ErrCorrupt, adm.sequence, k)
	}
	a := r.adapter
	desc := a.Descriptor()

	// 1-2. Decode and decide.
	cmd, err := a.DecodeCommand(adm.command)
	if err != nil {
		return failClosed(fmt.Errorf("decode command: %w", err))
	}
	tr := &transition{sequence: k, commandID: adm.commandID, next: st.state}
	events, err := a.Decide(st.state, cmd, game.Input{Sequence: k, EffectiveTimeUnixMicro: adm.effectiveTime})
	if err != nil {
		code, ok := a.RejectionCode(err)
		if !ok {
			return failClosed(fmt.Errorf("decide: %w", err))
		}
		tr.result = chain.Result{Rejected: true, Reason: code}
		events = nil
	} else {
		for j, e := range events {
			if tr.next, err = a.Evolve(tr.next, e); err != nil {
				return failClosed(fmt.Errorf("evolve event %d: %w", j, err))
			}
		}
	}
	tr.events = events

	// 3. Encode the events with contiguous event sequences.
	first := st.nextEventSequence
	chained := make([]chain.Event, len(events))
	for j, e := range events {
		b, err := a.EncodeEvent(e)
		if err != nil {
			return failClosed(fmt.Errorf("encode event %d: %w", j, err))
		}
		typ, err := eventType(b)
		if err != nil {
			return failClosed(fmt.Errorf("event %d: %w", j, err))
		}
		seq := first + uint64(j)
		chained[j] = chain.Event{Sequence: seq, Payload: b}
		tr.eventSeqs = append(tr.eventSeqs, int64(seq))
		tr.eventTypes = append(tr.eventTypes, typ)
	}

	// 4. The record, from the admission's exact command bytes.
	tr.record, err = chain.EncodeCommand(chain.Command{
		RoundID: roundID, Sequence: k, PrevHash: st.head, CommandID: adm.commandID,
		EffectiveTimeUnixMicro: adm.effectiveTime, EngineBuild: engineBuild,
		Payload: adm.command, Result: tr.result, Events: chained,
	})
	if err != nil {
		return failClosed(fmt.Errorf("encode record: %w", err))
	}
	if len(tr.record) > MaxRecordBytes {
		return failClosed(errRecordTooLong)
	}
	tr.hash = chain.LinkHash(tr.record)

	// 5. Postings and holds (E-9, E-10).
	kind, postings, err := r.rule.Postings(events, first)
	if err != nil {
		return failClosed(fmt.Errorf("ledger rule: %w", err))
	}
	if err := validatePostings(kind, postings, first, uint64(len(events))); err != nil {
		return failClosed(err)
	}
	tr.kind = kind
	for _, h := range adm.holds {
		tr.releases = append(tr.releases, h.accountID)
	}
	slices.SortFunc(tr.releases, func(x, y [16]byte) int { return bytes.Compare(x[:], y[:]) })
	for _, p := range postings {
		row := postingRow{Posting: p}
		if p.FromHold {
			h, err := consumeHold(adm.holds, p)
			if err != nil {
				return nil, err
			}
			row.holdAccount = h.accountID
			if tr.betPlayers == nil {
				tr.betPlayers = map[string][16]byte{}
			}
			tr.betPlayers[p.Debit.Player] = h.accountID
		}
		tr.postings = append(tr.postings, row)
	}

	// 6. Snapshot.
	tr.checkpoint = r.policy.Checkpoint(events)
	if tr.checkpoint {
		b, err := a.EncodeState(tr.next)
		if err != nil {
			return failClosed(fmt.Errorf("encode state: %w", err))
		}
		if tr.stateHash, err = chain.StateHash(desc.Game, desc.StateVersion, b); err != nil {
			return failClosed(fmt.Errorf("state hash: %w", err))
		}
	}

	// 7. Finality.
	if tr.final, err = r.policy.Final(tr.next); err != nil {
		return failClosed(fmt.Errorf("final: %w", err))
	}
	return tr, nil
}

var (
	errRecordTooLong = errors.New("execution: record exceeds MaxRecordBytes")
	errBadPosting    = errors.New("execution: invalid posting")
)

// eventType returns the type member of a canonical event object.
func eventType(b []byte) (string, error) {
	o, err := canonjson.Unmarshal(b)
	if err != nil {
		return "", err
	}
	return canonjson.Text(o["type"])
}

// validatePostings checks a rule's output (E-9): kind set iff postings,
// valid account references, distinct sides, amounts > 0, every event
// sequence inside the record, at most one posting per event, and FromHold
// only on a player debit, at most once.
func validatePostings(kind string, postings []ledger.Posting, first, n uint64) error {
	if (kind == "") != (len(postings) == 0) {
		return fmt.Errorf("%w: kind %q with %d postings", errBadPosting, kind, len(postings))
	}
	seen := map[uint64]bool{}
	fromHold := 0
	for i, p := range postings {
		switch {
		case !p.Debit.Valid() || !p.Credit.Valid():
			return fmt.Errorf("%w: posting %d has an invalid account", errBadPosting, i)
		case p.Debit == p.Credit:
			return fmt.Errorf("%w: posting %d debits and credits one account", errBadPosting, i)
		case p.Amount <= 0:
			return fmt.Errorf("%w: posting %d amount is not positive", errBadPosting, i)
		case p.EventSequence < first || p.EventSequence-first >= n:
			return fmt.Errorf("%w: posting %d settles an event outside the record", errBadPosting, i)
		case seen[p.EventSequence]:
			return fmt.Errorf("%w: two postings settle one event", errBadPosting)
		case p.FromHold && p.Debit.Kind != ledger.AccountPlayer:
			return fmt.Errorf("%w: posting %d consumes a hold from a non-player account", errBadPosting, i)
		}
		seen[p.EventSequence] = true
		if p.FromHold {
			fromHold++
		}
	}
	if fromHold > 1 {
		return fmt.Errorf("%w: %d postings consume one admission's hold", errBadPosting, fromHold)
	}
	return nil
}

// consumeHold checks that a FromHold posting consumes exactly the
// admission's single hold (E-10).
func consumeHold(holds []holdRow, p ledger.Posting) (holdRow, error) {
	switch {
	case len(holds) != 1:
		return holdRow{}, fmt.Errorf("%w: a bet needs exactly one credit hold, the admission has %d", ErrCorrupt, len(holds))
	case holds[0].player == "" || holds[0].player != p.Debit.Player:
		return holdRow{}, fmt.Errorf("%w: the bet's credit hold is on another account", ErrCorrupt)
	case holds[0].amount != p.Amount:
		return holdRow{}, fmt.Errorf("%w: the bet's credit hold differs from the bet", ErrCorrupt)
	}
	return holds[0], nil
}

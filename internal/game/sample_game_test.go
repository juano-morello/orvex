package game_test

import (
	"errors"
	"maps"
	"slices"

	"github.com/juano-morello/orvex/internal/game"
)

// tally is a minimal typed game: players score points up to a limit until
// the tally is closed. It is a test fixture that proves the contract
// compiles for a concrete game and gives the purity tests a subject.
type tally struct{}

var _ game.Game[tallyState, tallyCommand, tallyEvent, tallyConfig] = tally{}

type tallyConfig struct{ MaxPoints int }

type tallyState struct {
	MaxPoints int
	Total     int
	Closed    bool
	Scores    map[string]int // reference type: an in-place write would leak to the caller
	History   []uint64       // admission sequences applied, in order; also a reference type
}

// tallyCommand is a sealed sum type.
type tallyCommand interface{ isTallyCommand() }

type score struct {
	Player string
	Points int
}

type closeTally struct{}

func (score) isTallyCommand()      {}
func (closeTally) isTallyCommand() {}

// tallyEvent is a sealed sum type.
type tallyEvent interface{ isTallyEvent() }

type scored struct {
	Player      string
	Points      int
	Seq         uint64
	AtUnixMicro int64
}

type closed struct {
	Seq         uint64
	AtUnixMicro int64
}

func (scored) isTallyEvent() {}
func (closed) isTallyEvent() {}

var (
	errInvalidConfig  = errors.New("tally: invalid config")
	errClosed         = errors.New("tally: closed")
	errInvalidPoints  = errors.New("tally: points must be positive")
	errOverLimit      = errors.New("tally: over the points limit")
	errUnknownCommand = errors.New("tally: unknown command")
	errUnknownEvent   = errors.New("tally: unknown event")
)

func (tally) InitialState(cfg tallyConfig) (tallyState, error) {
	if cfg.MaxPoints <= 0 {
		return tallyState{}, errInvalidConfig
	}
	return tallyState{MaxPoints: cfg.MaxPoints, Scores: map[string]int{}}, nil
}

func (tally) Decide(s tallyState, c tallyCommand, in game.Input) ([]tallyEvent, error) {
	if s.Closed {
		return nil, errClosed
	}
	switch c := c.(type) {
	case score:
		if c.Points <= 0 {
			return nil, errInvalidPoints
		}
		if c.Points > s.MaxPoints-s.Total {
			return nil, errOverLimit
		}
		return []tallyEvent{scored{Player: c.Player, Points: c.Points, Seq: in.Sequence, AtUnixMicro: in.EffectiveTimeUnixMicro}}, nil
	case closeTally:
		return []tallyEvent{closed{Seq: in.Sequence, AtUnixMicro: in.EffectiveTimeUnixMicro}}, nil
	default:
		return nil, errUnknownCommand
	}
}

func (tally) Evolve(s tallyState, e tallyEvent) (tallyState, error) {
	if s.Closed {
		return tallyState{}, errClosed
	}
	next := s
	next.Scores = maps.Clone(s.Scores)
	if next.Scores == nil {
		next.Scores = map[string]int{}
	}
	next.History = slices.Clone(s.History)
	switch e := e.(type) {
	case scored:
		if e.Points > s.MaxPoints-s.Total {
			return tallyState{}, errOverLimit
		}
		next.Scores[e.Player] += e.Points
		next.Total += e.Points
		next.History = append(next.History, e.Seq)
	case closed:
		next.Closed = true
		next.History = append(next.History, e.Seq)
	default:
		return tallyState{}, errUnknownEvent
	}
	return next, nil
}

// baseState is an open tally with some scores. History has spare capacity
// on purpose, so the purity checks can catch appends that write past len
// into the caller's backing array.
func baseState() tallyState {
	return tallyState{
		MaxPoints: 100,
		Total:     15,
		Scores:    map[string]int{"alice": 10, "bob": 5},
		History:   append(make([]uint64, 0, 8), 1, 2),
	}
}

func sampleDescriptor(rules game.RulesVersion) game.Descriptor {
	return game.Descriptor{Game: "tally", RulesVersion: rules, SchemaVersion: 1, StateVersion: 1}
}

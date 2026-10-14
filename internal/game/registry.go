package game

import (
	"errors"
	"fmt"
)

// ID names a game, for example "crash". It is the first half of a registry key.
type ID string

// RulesVersion identifies one immutable Decide/Evolve implementation of a
// game. It is the second half of a registry key and is never reused for
// different behavior (ADR-001, ADR-007). Zero is invalid.
type RulesVersion uint32

// SchemaVersion identifies the shape of the commands and events a rules
// version reads and writes (their canonical encoding is defined
// elsewhere). Zero is invalid.
type SchemaVersion uint32

// StateVersion identifies the shape of a rules version's state, as stored
// in checkpoints and covered by the state hash. Zero is invalid.
type StateVersion uint32

// Descriptor identifies a registered rules implementation. ADR-003 keeps
// the three versions separate: several rules versions may share a schema
// or state version, and a rules fix that leaves payloads unchanged does not
// bump SchemaVersion.
type Descriptor struct {
	Game          ID
	RulesVersion  RulesVersion
	SchemaVersion SchemaVersion
	StateVersion  StateVersion
}

var (
	// ErrTypeMismatch reports a value whose dynamic type is not the one the
	// rules version uses for its role (config, state, command or event).
	ErrTypeMismatch = errors.New("game: value has the wrong type for the rules version")

	// ErrUnknownRules reports a lookup for a game and rules version that is
	// not registered. Lookups never fall back to another version.
	ErrUnknownRules = errors.New("game: unknown game rules version")

	// ErrInvalidRegistration reports an adapter that cannot be registered:
	// nil, or a descriptor with an empty game or a zero version.
	ErrInvalidRegistration = errors.New("game: invalid registration")

	// ErrDuplicateRules reports two adapters for the same game and rules
	// version.
	ErrDuplicateRules = errors.New("game: duplicate game rules version")
)

// Adapter is the type-erased runtime boundary for one rules version of one
// game (ADR-003). Values cross it as the game's own Go types boxed in any;
// a value of any other dynamic type, including nil, is rejected with
// ErrTypeMismatch before the game runs. Errors returned by the game pass
// through unwrapped, and any events or state returned alongside them are
// dropped. Build Adapters with Adapt; this is not a plugin interface.
type Adapter interface {
	// Descriptor returns the versions this adapter implements.
	Descriptor() Descriptor

	// InitialState is Game.InitialState with config boxed in any.
	InitialState(config any) (any, error)

	// Decide is Game.Decide with state, command and events boxed in any.
	Decide(state, command any, in Input) ([]any, error)

	// Evolve is Game.Evolve with state and event boxed in any.
	Evolve(state, event any) (any, error)
}

// Adapt wraps a typed Game as the Adapter for rules version d. g must be
// non-nil. Type arguments are inferred: game.Adapt(d, rules{}).
func Adapt[S, C, E, Cfg any](d Descriptor, g Game[S, C, E, Cfg]) Adapter {
	return adapter[S, C, E, Cfg]{d: d, g: g}
}

type adapter[S, C, E, Cfg any] struct {
	d Descriptor
	g Game[S, C, E, Cfg]
}

func (a adapter[S, C, E, Cfg]) Descriptor() Descriptor {
	return a.d
}

func (a adapter[S, C, E, Cfg]) InitialState(config any) (any, error) {
	cfg, ok := config.(Cfg)
	if !ok {
		return nil, a.mismatch("config", config)
	}
	s, err := a.g.InitialState(cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (a adapter[S, C, E, Cfg]) Decide(state, command any, in Input) ([]any, error) {
	s, ok := state.(S)
	if !ok {
		return nil, a.mismatch("state", state)
	}
	c, ok := command.(C)
	if !ok {
		return nil, a.mismatch("command", command)
	}
	events, err := a.g.Decide(s, c, in)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(events))
	for i, e := range events {
		out[i] = e
	}
	return out, nil
}

func (a adapter[S, C, E, Cfg]) Evolve(state, event any) (any, error) {
	s, ok := state.(S)
	if !ok {
		return nil, a.mismatch("state", state)
	}
	e, ok := event.(E)
	if !ok {
		return nil, a.mismatch("event", event)
	}
	next, err := a.g.Evolve(s, e)
	if err != nil {
		return nil, err
	}
	return next, nil
}

func (a adapter[S, C, E, Cfg]) mismatch(role string, got any) error {
	return fmt.Errorf("%w: %s %d %s: got %T", ErrTypeMismatch, a.d.Game, a.d.RulesVersion, role, got)
}

// Registry maps a game and rules version to its Adapter. NewRegistry builds
// it and it never changes afterwards, so it is safe for concurrent use.
// Lookup never falls back to another rules version: history recorded under
// an unregistered version must fail closed (ADR-006). Historical versions
// stay registered next to current ones (ADR-003 "Consequences").
type Registry struct {
	adapters map[key]Adapter
}

type key struct {
	game  ID
	rules RulesVersion
}

// NewRegistry validates and indexes adapters. It rejects a nil adapter or a
// descriptor with an empty game or a zero version (ErrInvalidRegistration)
// and a second adapter for the same game and rules version
// (ErrDuplicateRules). On error it returns a nil Registry.
func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{adapters: make(map[key]Adapter, len(adapters))}
	for i, a := range adapters {
		if a == nil {
			return nil, fmt.Errorf("%w: adapter %d is nil", ErrInvalidRegistration, i)
		}
		d := a.Descriptor()
		switch {
		case d.Game == "":
			return nil, fmt.Errorf("%w: adapter %d: empty game, rules version %d", ErrInvalidRegistration, i, d.RulesVersion)
		case d.RulesVersion == 0:
			return nil, fmt.Errorf("%w: %s: zero rules version", ErrInvalidRegistration, d.Game)
		case d.SchemaVersion == 0:
			return nil, fmt.Errorf("%w: %s %d: zero schema version", ErrInvalidRegistration, d.Game, d.RulesVersion)
		case d.StateVersion == 0:
			return nil, fmt.Errorf("%w: %s %d: zero state version", ErrInvalidRegistration, d.Game, d.RulesVersion)
		}
		k := key{game: d.Game, rules: d.RulesVersion}
		if _, dup := r.adapters[k]; dup {
			return nil, fmt.Errorf("%w: %s %d", ErrDuplicateRules, d.Game, d.RulesVersion)
		}
		r.adapters[k] = a
	}
	return r, nil
}

// Lookup returns the Adapter registered for game at rules version rules,
// or an error wrapping ErrUnknownRules.
func (r *Registry) Lookup(game ID, rules RulesVersion) (Adapter, error) {
	a, ok := r.adapters[key{game: game, rules: rules}]
	if !ok {
		return nil, fmt.Errorf("%w: %q %d", ErrUnknownRules, game, rules)
	}
	return a, nil
}

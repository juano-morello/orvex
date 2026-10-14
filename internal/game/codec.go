package game

import "errors"

// Codec is the canonical serializer of one rules version (ADR-001, ADR-003).
// Encodings are OCJ objects (internal/canonjson). All methods are pure and
// deterministic and never mutate their arguments.
type Codec[S, C, E, Cfg any] interface {
	// EncodeConfig splits cfg into its public part, recorded in the round's
	// genesis record, and its secret part, revealed later. The round ID is in
	// neither: the genesis record carries it.
	EncodeConfig(cfg Cfg) (public, secret []byte, err error)
	// DecodeConfig rebuilds the config of round roundID. It fails with an error
	// wrapping ErrRevealMismatch when secret does not match public's commitment.
	DecodeConfig(roundID [16]byte, public, secret []byte) (Cfg, error)
	EncodeCommand(c C) ([]byte, error)
	DecodeCommand(b []byte) (C, error)
	EncodeEvent(e E) ([]byte, error)
	DecodeEvent(b []byte) (E, error)
	EncodeState(s S) ([]byte, error)
	// RejectionCode returns the stable code of a deterministic rejection
	// returned by Decide. ok is false for any other error: it fails closed.
	RejectionCode(err error) (code string, ok bool)
}

var (
	// ErrMalformed reports bytes that are not a canonical encoding under the
	// rules version's schema: non-canonical OCJ, a missing or unknown key, an
	// unknown type or enum string, or a malformed scalar.
	ErrMalformed = errors.New("game: bytes are not a canonical encoding for the rules version")
	// ErrUnencodable reports a value with no canonical encoding, such as an
	// enum outside its range or a string that is not valid UTF-8.
	ErrUnencodable = errors.New("game: value has no canonical encoding")
	// ErrRevealMismatch reports a revealed config secret that does not match
	// the commitment in the public config (ADR-004 commit/reveal).
	ErrRevealMismatch = errors.New("game: revealed secret does not match its commitment")
)

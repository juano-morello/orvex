package engineapi

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/persistence/eventstore"
	"github.com/juano-morello/orvex/internal/runtime/sessions"
	orvexv1 "github.com/juano-morello/orvex/proto/orvex/v1"
)

// errBadRecord wraps every failure to map a stored record. Its texts come
// from the chain and crash/v1 decoders, which never quote a seed.
var errBadRecord = errors.New("engineapi: stored record does not map to a public frame")

// commandID derives the admission command ID of a player's client command
// ID: the first 16 bytes of SHA-256("orvex/command-id/v1" 0x00 player 0x00
// client), as an RFC 9562 version 8 UUID. Player IDs hold no U+0000, so the
// input is unambiguous, and two players never derive the same ID. The
// derivation is frozen: TestCommandIDGolden pins it.
func commandID(player sessions.PlayerID, client [16]byte) [16]byte {
	h := sha256.New()
	h.Write([]byte("orvex/command-id/v1\x00"))
	h.Write([]byte(player))
	h.Write([]byte{0})
	h.Write(client[:])
	var id [16]byte
	copy(id[:], h.Sum(nil))
	id[6] = id[6]&0x0f | 0x80
	id[8] = id[8]&0x3f | 0x80
	return id
}

// publicRecord maps a stored chain record of round to its public frame.
// The frame is an allow-list: a command record's public part is its
// effective time and, if accepted, its events. The command, its ID, the
// engine build, the hashes and a rejection reason stay in the audit
// record.
func publicRecord(round [16]byte, r eventstore.StoredRecord) (*orvexv1.SubscribeRoundResponse, error) {
	rec, err := chain.Decode(r.Record)
	if err != nil {
		return nil, fmt.Errorf("%w: record %d: %w", errBadRecord, r.Sequence, err)
	}
	switch rec := rec.(type) {
	case chain.Genesis:
		if r.Sequence != 0 || rec.Sequence != 0 || rec.RoundID != round {
			return nil, fmt.Errorf("%w: genesis at sequence %d", errBadRecord, r.Sequence)
		}
		if rec.Game != v1.GameID || rec.RulesVersion != v1.RulesVersionNumber {
			return nil, fmt.Errorf("%w: genesis is not crash/v1", errBadRecord)
		}
		g, err := publicGenesis(rec.Config)
		if err != nil {
			return nil, err
		}
		return &orvexv1.SubscribeRoundResponse{
			Sequence:               0,
			EffectiveTimeUnixMicro: rec.CreatedAtUnixMicro,
			Body:                   &orvexv1.SubscribeRoundResponse_Genesis{Genesis: g},
		}, nil
	case chain.Command:
		if rec.Sequence != r.Sequence || r.Sequence == 0 || rec.RoundID != round {
			return nil, fmt.Errorf("%w: command record %d", errBadRecord, r.Sequence)
		}
		events, err := publicEvents(rec)
		if err != nil {
			return nil, err
		}
		return &orvexv1.SubscribeRoundResponse{
			Sequence:               rec.Sequence,
			EffectiveTimeUnixMicro: rec.EffectiveTimeUnixMicro,
			Body:                   &orvexv1.SubscribeRoundResponse_Events{Events: &orvexv1.Events{Events: events}},
		}, nil
	default:
		return nil, fmt.Errorf("%w: record %d of type %T", errBadRecord, r.Sequence, rec)
	}
}

// publicGenesis parses the crash/v1 public config: the seed commitment and
// the bet limits, the exact key set of v1.Codec.EncodeConfig's public
// half.
func publicGenesis(config []byte) (*orvexv1.Genesis, error) {
	bad := func(err error) (*orvexv1.Genesis, error) {
		return nil, fmt.Errorf("%w: genesis config: %w", errBadRecord, err)
	}
	o, err := canonjson.Unmarshal(config)
	if err != nil {
		return bad(err)
	}
	if _, err := canonjson.Fields(o, "commitment", "maxBetCredits", "maxBets", "minBetCredits"); err != nil {
		return bad(err)
	}
	commitment, err := canonjson.ParseHex(o["commitment"], sha256.Size)
	if err != nil {
		return bad(err)
	}
	g := &orvexv1.Genesis{Commitment: commitment}
	if g.MinBetCredits, err = canonjson.ParseInt(o["minBetCredits"]); err != nil {
		return bad(err)
	}
	if g.MaxBetCredits, err = canonjson.ParseInt(o["maxBetCredits"]); err != nil {
		return bad(err)
	}
	if g.MaxBets, err = canonjson.ParseInt(o["maxBets"]); err != nil {
		return bad(err)
	}
	return g, nil
}

// publicEvents returns an accepted command's events; a rejected command has
// none.
func publicEvents(c chain.Command) ([]*orvexv1.CrashEvent, error) {
	if c.Result.Rejected {
		return nil, nil
	}
	out := make([]*orvexv1.CrashEvent, 0, len(c.Events))
	for _, e := range c.Events {
		ev, err := v1.Codec{}.DecodeEvent(e.Payload)
		if err != nil {
			return nil, fmt.Errorf("%w: record %d: %w", errBadRecord, c.Sequence, err)
		}
		pe, err := crashEvent(ev)
		if err != nil {
			return nil, err
		}
		out = append(out, pe)
	}
	return out, nil
}

// crashEvent copies a crash/v1 event field by field. Every crash/v1 event
// is public once emitted (math spec section 7).
func crashEvent(e v1.Event) (*orvexv1.CrashEvent, error) {
	var ce orvexv1.CrashEvent
	switch e := e.(type) {
	case v1.BetPlaced:
		ce.Event = &orvexv1.CrashEvent_BetPlaced{BetPlaced: &orvexv1.BetPlaced{
			PlayerId:          string(e.Player),
			BetCredits:        e.BetCredits,
			PlacedAtUnixMicro: e.PlacedAtUnixMicro,
		}}
	case v1.RoundStarted:
		ce.Event = &orvexv1.CrashEvent_RoundStarted{RoundStarted: &orvexv1.RoundStarted{
			StartedAtUnixMicro: e.StartedAtUnixMicro,
		}}
	case v1.BetCashedOut:
		ce.Event = &orvexv1.CrashEvent_BetCashedOut{BetCashedOut: &orvexv1.BetCashedOut{
			PlayerId:                string(e.Player),
			CashedOutAtUnixMicro:    e.CashedOutAtUnixMicro,
			CashOutTick:             e.CashOutTick,
			CashOutMultiplierScaled: e.CashOutMultiplierScaled,
			PayoutCredits:           e.PayoutCredits,
		}}
	case v1.RoundCrashed:
		ce.Event = &orvexv1.CrashEvent_RoundCrashed{RoundCrashed: &orvexv1.RoundCrashed{
			ClosedAtUnixMicro:         e.ClosedAtUnixMicro,
			CrashPointScaled:          e.CrashPointScaled,
			CrashTick:                 e.CrashTick,
			CrashDeadlineOffsetMicros: e.CrashDeadlineOffsetMicros,
			CrashDeadlineUnixMicro:    e.CrashDeadlineUnixMicro,
		}}
	case v1.BetLost:
		ce.Event = &orvexv1.CrashEvent_BetLost{BetLost: &orvexv1.BetLost{
			PlayerId:   string(e.Player),
			BetCredits: e.BetCredits,
		}}
	case v1.RoundSettled:
		ce.Event = &orvexv1.CrashEvent_RoundSettled{RoundSettled: &orvexv1.RoundSettled{
			SettledAtUnixMicro: e.SettledAtUnixMicro,
			BetCount:           e.BetCount,
			TotalBetCredits:    e.TotalBetCredits,
			TotalPayoutCredits: e.TotalPayoutCredits,
			Seed:               e.Seed[:],
		}}
	default:
		return nil, fmt.Errorf("%w: crash/v1 event of type %T", errBadRecord, e)
	}
	return &ce, nil
}

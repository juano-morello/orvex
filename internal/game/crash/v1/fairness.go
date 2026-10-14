package v1

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"github.com/cockroachdb/apd/v3"
)

// roundIDTextLen is the length of a UUID's canonical 8-4-4-4-12 text (D-11).
const roundIDTextLen = 36

// Commitment returns SHA-256(seed), published before the round accepts bets (D-10).
func Commitment(seed [32]byte) [32]byte {
	return sha256.Sum256(seed[:])
}

// VerifyCommitment reports whether commitment is SHA-256(seed). Both values are
// public at reveal time, so a plain comparison is used (spec section 7).
func VerifyCommitment(seed, commitment [32]byte) bool {
	return Commitment(seed) == commitment
}

// OutcomeMessage returns OutcomeTag followed by the round ID's canonical
// lowercase 8-4-4-4-12 text (D-10, D-11). Any 16 bytes are accepted.
func OutcomeMessage(roundID [16]byte) []byte {
	var text [roundIDTextLen]byte
	hex.Encode(text[0:8], roundID[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], roundID[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], roundID[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], roundID[8:10])
	text[23] = '-'
	hex.Encode(text[24:36], roundID[10:16])

	msg := make([]byte, 0, len(OutcomeTag)+roundIDTextLen)
	msg = append(msg, OutcomeTag...)
	return append(msg, text[:]...)
}

// outcomeHash returns HMAC-SHA256(key = seed, OutcomeMessage(roundID)) (D-10).
func outcomeHash(seed [32]byte, roundID [16]byte) [32]byte {
	mac := hmac.New(sha256.New, seed[:])
	// hash.Hash.Write never returns an error.
	_, _ = mac.Write(OutcomeMessage(roundID))
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// OutcomeR returns r, bytes 0 to 7 of the outcome hash read as a big-endian
// uint64 (D-10).
func OutcomeR(seed [32]byte, roundID [16]byte) uint64 {
	h := outcomeHash(seed, roundID)
	return binary.BigEndian.Uint64(h[:8])
}

// crashPointRaw returns floor(EdgeNumerator * 2^64 / (2^64 - r)) exactly (D-5).
// Every value is non-negative, so Quo's truncation equals floor.
func crashPointRaw(r uint64) *apd.BigInt {
	var twoTo64, num, den, rr apd.BigInt
	twoTo64.Lsh(apd.NewBigInt(1), 64)
	num.Mul(apd.NewBigInt(EdgeNumerator), &twoTo64)
	rr.SetUint64(r)
	den.Sub(&twoTo64, &rr)
	return new(apd.BigInt).Quo(&num, &den)
}

// CrashPointFromR returns C = min(MaxCrashPointScaled, max(MinCrashPointScaled, raw))
// (D-5). The clamp is applied to the exact big integer before narrowing to int64.
func CrashPointFromR(r uint64) int64 {
	raw := crashPointRaw(r)
	if raw.Cmp(apd.NewBigInt(MaxCrashPointScaled)) >= 0 {
		return MaxCrashPointScaled
	}
	if raw.Cmp(apd.NewBigInt(MinCrashPointScaled)) <= 0 {
		return MinCrashPointScaled
	}
	return raw.Int64()
}

// CrashPoint returns the crash point of the round derived from seed and roundID
// (D-5, D-10).
func CrashPoint(seed [32]byte, roundID [16]byte) int64 {
	return CrashPointFromR(OutcomeR(seed, roundID))
}

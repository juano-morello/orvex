package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// ID derivation (E-8). Every ID is uuid8 of a namespaced string.
const (
	accountPrefix     = "orvex/ledger/v1/account:"
	transactionPrefix = "orvex/ledger/v1/transaction:"
)

// HouseAccountID returns the ID of the global house account:
// uuid8("orvex/ledger/v1/account:global/house").
func HouseAccountID() [16]byte {
	return uuid8(accountPrefix + "global/" + HouseAccountName)
}

// RoundAccountID returns the ID of round roundID's round_pool account:
// uuid8("orvex/ledger/v1/account:<round uuid>/round_pool").
func RoundAccountID(roundID [16]byte) [16]byte {
	return uuid8(accountPrefix + uuidText(roundID) + "/" + RoundAccountName)
}

// TransactionID returns the ID of the ledger transaction of record sequence
// of round roundID: uuid8("orvex/ledger/v1/transaction:<round uuid>/<k>").
func TransactionID(roundID [16]byte, sequence uint64) [16]byte {
	return uuid8(transactionPrefix + uuidText(roundID) + "/" + strconv.FormatUint(sequence, 10))
}

// uuid8 returns the RFC 9562 UUIDv8 made of the first 16 bytes of SHA-256
// of s, with version nibble 8 and variant 10.
func uuid8(s string) [16]byte {
	sum := sha256.Sum256([]byte(s))
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = id[6]&0x0f | 0x80
	id[8] = id[8]&0x3f | 0x80
	return id
}

// uuidText formats id in the lowercase hyphenated form.
func uuidText(id [16]byte) string {
	var b [36]byte
	hex.Encode(b[0:8], id[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], id[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], id[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], id[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], id[10:16])
	return string(b[:])
}

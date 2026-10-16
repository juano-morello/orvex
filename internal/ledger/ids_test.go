package ledger

import (
	"encoding/binary"
	"math/rand/v2"
	"strconv"
	"testing"
)

var workedRoundID = [16]byte{0x01, 0x90, 0xf3, 0xa8, 0x7b, 0x3c, 0x7d, 0x4e, 0x8f, 0x00, 0, 0, 0, 0, 0, 0x01}

func TestHouseAccountID(t *testing.T) {
	if got := uuidText(HouseAccountID()); got != "4688699a-7bf4-8e07-8cec-c5f6bef86c36" {
		t.Errorf("HouseAccountID = %s", got)
	}
}

func TestRoundAccountID(t *testing.T) {
	if got := uuidText(workedRoundID); got != "0190f3a8-7b3c-7d4e-8f00-000000000001" {
		t.Fatalf("uuidText = %s", got)
	}
	if got := uuidText(RoundAccountID(workedRoundID)); got != "13565418-c5ee-8f13-85c6-552f9e8755bd" {
		t.Errorf("RoundAccountID = %s", got)
	}
}

func TestTransactionID(t *testing.T) {
	for _, c := range []struct {
		seq  uint64
		want string
	}{
		{1, "862c9c4b-3cb4-847f-80eb-d0364134723c"},
		{12, "5687be29-0e53-89bc-886e-babaeada505f"},
		{17, "4357c225-96f0-803b-a757-26ca7f5055ac"},
		{21, "d61426fa-cde4-8067-a6f6-163fb1aeb196"},
	} {
		if got := uuidText(TransactionID(workedRoundID, c.seq)); got != c.want {
			t.Errorf("TransactionID(%d) = %s, want %s", c.seq, got, c.want)
		}
	}
}

func TestIDsAreUUIDv8(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a seeded PCG keeps the test inputs reproducible; no secret is derived
	check := func(what string, id [16]byte) {
		t.Helper()
		if id[6]>>4 != 8 || id[8]>>6 != 2 {
			t.Fatalf("%s %s: version %x variant %b, want 8 and 10", what, uuidText(id), id[6]>>4, id[8]>>6)
		}
	}
	check("house", HouseAccountID())
	for range 1000 {
		var round [16]byte
		binary.BigEndian.PutUint64(round[:8], r.Uint64())
		binary.BigEndian.PutUint64(round[8:], r.Uint64())
		check("round account", RoundAccountID(round))
		check("transaction", TransactionID(round, r.Uint64()))
	}
}

func TestIDsDistinct(t *testing.T) {
	other := workedRoundID
	other[15] = 0x02
	ids := map[[16]byte]string{}
	add := func(what string, id [16]byte) {
		t.Helper()
		if prev, ok := ids[id]; ok {
			t.Errorf("%s collides with %s", what, prev)
		}
		ids[id] = what
	}
	add("house", HouseAccountID())
	add("round account", RoundAccountID(workedRoundID))
	add("other round account", RoundAccountID(other))
	for k := range uint64(50) {
		add("transaction worked/"+strconv.FormatUint(k, 10), TransactionID(workedRoundID, k))
		add("transaction other/"+strconv.FormatUint(k, 10), TransactionID(other, k))
	}
	if uuid8("orvex/ledger/v1/account:global/"+RoundAccountName) == HouseAccountID() {
		t.Error("account names do not separate IDs")
	}
}

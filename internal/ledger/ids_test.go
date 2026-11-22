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

func TestFaucetAccountID(t *testing.T) {
	if got := uuidText(FaucetAccountID()); got != "aaeb433a-e909-8dae-95fd-f1d64f9e40f3" {
		t.Errorf("FaucetAccountID = %s", got)
	}
}

func TestGrantTransactionID(t *testing.T) {
	for _, c := range []struct {
		key  string
		want string
	}{
		{"a", "b54483d0-b1ad-88aa-964c-3a1408358956"},
		{"bob-1", "ac538930-8952-8c7b-a462-5f08b0b8809f"},
		{"alice-2026-11-22", "f93fbbec-4b3a-89e7-a493-4512bdcb461f"},
		{"demo:alice.1_x-9", "1ea50446-26ce-895c-b444-bedc0a18aad7"},
	} {
		if got := uuidText(GrantTransactionID(c.key)); got != c.want {
			t.Errorf("GrantTransactionID(%q) = %s, want %s", c.key, got, c.want)
		}
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
	check("faucet", FaucetAccountID())
	for range 1000 {
		var round [16]byte
		binary.BigEndian.PutUint64(round[:8], r.Uint64())
		binary.BigEndian.PutUint64(round[8:], r.Uint64())
		check("round account", RoundAccountID(round))
		check("transaction", TransactionID(round, r.Uint64()))
		check("grant", GrantTransactionID(strconv.FormatUint(r.Uint64(), 36)))
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
	add("faucet", FaucetAccountID())
	add("round account", RoundAccountID(workedRoundID))
	add("other round account", RoundAccountID(other))
	for k := range uint64(50) {
		add("transaction worked/"+strconv.FormatUint(k, 10), TransactionID(workedRoundID, k))
		add("transaction other/"+strconv.FormatUint(k, 10), TransactionID(other, k))
		add("grant "+strconv.FormatUint(k, 10), GrantTransactionID(strconv.FormatUint(k, 10)))
	}
	// A grant key spelled like a transaction suffix still names another ID.
	if GrantTransactionID(uuidText(workedRoundID)+"/1") == TransactionID(workedRoundID, 1) {
		t.Error("the grant prefix does not separate grant IDs from transaction IDs")
	}
	if uuid8("orvex/ledger/v1/account:global/"+RoundAccountName) == HouseAccountID() {
		t.Error("account names do not separate IDs")
	}
}

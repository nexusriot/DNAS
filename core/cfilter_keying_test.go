package core

import (
	"encoding/json"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A filter must be queried with the key it was BUILT with.
//
// From UpgradeFilterCommit the key stops being the block hash and becomes a
// digest of PrevHash and MerkleRoot — fields fixed before the nonce search, so
// the filter can be committed inside the block hash. Match had no way to know
// that: it keyed from BlockHash unconditionally, so above the activation height
// every filter was built with one key and read with another.
//
// Nothing about that fails loudly. The filter still decodes, still has the
// right item count, and still answers — it just answers "not in this block" for
// addresses that are in it. A non-match is supposed to be a sound proof of
// non-inclusion, so the failure mode is a light wallet that is told, with the
// chain's own work behind it, that payments it received never happened.

func filterBlockAt(t *testing.T, height uint64, addrs ...string) Block {
	t.Helper()
	b := Block{
		Index:      height,
		PrevHash:   "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		Hash:       "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100",
		MerkleRoot: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
	}
	for i := 0; i+1 < len(addrs); i += 2 {
		b.Transactions = append(b.Transactions, Transaction{From: addrs[i], To: addrs[i+1]})
	}
	return b
}

// The guarantee, at a height where the key derivation has moved.
func TestFilterHasNoFalseNegativesAboveTheCommitUpgrade(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)

	a, _ := wallet.New()
	b, _ := wallet.New()
	c, _ := wallet.New()
	d, _ := wallet.New()
	blk := filterBlockAt(t, 5, a.Address(), b.Address(), c.Address(), d.Address())

	f := BuildBlockFilter(blk)
	if f.Key == "" {
		t.Fatal("a filter built above the commit upgrade carries no key seed, so a matcher cannot reproduce it")
	}
	if f.Key == f.BlockHash {
		t.Fatal("the key seed is the block hash, which is the derivation the upgrade exists to move away from")
	}
	for _, addr := range blockAddresses(blk) {
		if !f.Match(addr) {
			t.Fatalf("address %s is in the block but does not match its filter (false negative)", addr)
		}
	}
}

// A filter is served on its own (/cfilter/{height}), so whatever the matcher
// needs has to survive the wire. A key that lived only in memory would work in
// a unit test and fail for every real light client.
func TestFilterKeySurvivesSerialization(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 1)

	a, _ := wallet.New()
	b, _ := wallet.New()
	blk := filterBlockAt(t, 9, a.Address(), b.Address())
	built := BuildBlockFilter(blk)

	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	var shipped BlockFilter
	if err := json.Unmarshal(raw, &shipped); err != nil {
		t.Fatal(err)
	}
	for _, addr := range blockAddresses(blk) {
		if !shipped.Match(addr) {
			t.Fatalf("address %s stopped matching after a round trip through JSON: %s", addr, raw)
		}
	}
}

// Below the activation the key is the block hash and no seed is carried, so
// filters built before this field existed keep matching exactly as they did.
func TestFilterBelowTheUpgradeCarriesNoKeyAndStillMatches(t *testing.T) {
	withUpgradeAt(t, UpgradeFilterCommit, 100)

	a, _ := wallet.New()
	b, _ := wallet.New()
	blk := filterBlockAt(t, 5, a.Address(), b.Address())
	f := BuildBlockFilter(blk)

	if f.Key != "" {
		t.Fatalf("a pre-upgrade filter carries a key seed %q; it should key by block hash and stay byte-identical on the wire", f.Key)
	}
	for _, addr := range blockAddresses(blk) {
		if !f.Match(addr) {
			t.Fatalf("address %s in the block did not match its own filter", addr)
		}
	}
	// And a filter with no seed at all must still be matchable — that is what
	// every filter already serialized looks like.
	bare := BlockFilter{Index: f.Index, N: f.N, BlockHash: f.BlockHash, Data: f.Data}
	for _, addr := range blockAddresses(blk) {
		if !bare.Match(addr) {
			t.Fatalf("address %s did not match a filter carrying no key seed", addr)
		}
	}
}

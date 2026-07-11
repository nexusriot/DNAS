package core

import (
	"fmt"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestSipHash24Vectors checks the implementation against the official SipHash-2-4
// test vectors (key = bytes 0x00..0x0f), so the filter's hashing is provably
// correct rather than merely self-consistent.
func TestSipHash24Vectors(t *testing.T) {
	k0 := uint64(0x0706050403020100) // little-endian of key bytes 00..07
	k1 := uint64(0x0f0e0d0c0b0a0908) // little-endian of key bytes 08..0f
	cases := []struct {
		inLen int
		want  uint64
	}{
		{0, 0x726fdb47dd0e0e31},
		{1, 0x74f839c593dc67fd},
		{8, 0x93f5f5799a932462}, // exercises the full 8-byte block path
	}
	for _, c := range cases {
		in := make([]byte, c.inLen)
		for i := range in {
			in[i] = byte(i)
		}
		if got := sipHash24(k0, k1, in); got != c.want {
			t.Errorf("siphash(len %d) = %#x, want %#x", c.inLen, got, c.want)
		}
	}
}

// blockWith builds a (non-mined) block whose transactions touch the given
// addresses; only From/To matter to the filter.
func blockWith(index uint64, hash string, addrs ...string) Block {
	b := Block{Index: index, Hash: hash}
	b.Transactions = append(b.Transactions, NewCoinbase("dnasminer", 1)) // coinbase.To
	for i := 0; i+1 < len(addrs); i += 2 {
		b.Transactions = append(b.Transactions, Transaction{From: addrs[i], To: addrs[i+1]})
	}
	if len(addrs)%2 == 1 { // odd: last address as a lone recipient
		b.Transactions = append(b.Transactions, Transaction{From: "dnassomeone", To: addrs[len(addrs)-1]})
	}
	return b
}

func TestCompactFilterNoFalseNegatives(t *testing.T) {
	// Every address a block actually touches MUST match its filter — this is the
	// guarantee that a non-match is a sound proof of non-inclusion.
	a, _ := wallet.New()
	b, _ := wallet.New()
	c, _ := wallet.New()
	blk := blockWith(1, "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		a.Address(), b.Address(), c.Address(), a.Address())
	f := BuildBlockFilter(blk)

	for _, addr := range blockAddresses(blk) {
		if !f.Match(addr) {
			t.Fatalf("address %s in the block did not match its filter (false negative!)", addr)
		}
	}
}

func TestCompactFilterNonInclusion(t *testing.T) {
	// Build filters for many blocks, then confirm addresses that appear in NO
	// block almost never match (low false-positive rate) — the property that lets
	// a wallet skip provably-irrelevant blocks.
	var filters []BlockFilter
	for i := 0; i < 20; i++ {
		p1, _ := wallet.New()
		p2, _ := wallet.New()
		hash := fmt.Sprintf("%064x", i*2654435761+1)
		filters = append(filters, BuildBlockFilter(blockWith(uint64(i), hash, p1.Address(), p2.Address())))
	}

	falsePositives := 0
	const probes = 500
	for i := 0; i < probes; i++ {
		outsider, _ := wallet.New()
		for _, f := range filters {
			if f.Match(outsider.Address()) {
				falsePositives++
			}
		}
	}
	// Expected ~ probes*len(filters)/gcsM ≈ 500*20/784931 ≈ 0.013. Allow a tiny
	// margin; a large count would mean the coding is broken.
	if falsePositives > 3 {
		t.Fatalf("too many false positives: %d over %d probes across %d filters", falsePositives, probes, len(filters))
	}
	t.Logf("false positives: %d / %d probes", falsePositives, probes*len(filters))
}

func TestFilterHeaderChain(t *testing.T) {
	mk := func(seed int) []BlockFilter {
		var fs []BlockFilter
		for i := 0; i < 5; i++ {
			p, _ := wallet.New()
			hash := fmt.Sprintf("%064x", (i+seed)*0x100000001b3)
			fs = append(fs, BuildBlockFilter(blockWith(uint64(i), hash, p.Address(), p.Address())))
		}
		return fs
	}
	fs := mk(0)
	chain := FilterHeaderChain(fs)
	if len(chain) != len(fs) {
		t.Fatalf("chain length %d, want %d", len(chain), len(fs))
	}
	// Deterministic: same filters -> same chain.
	for i, h := range FilterHeaderChain(fs) {
		if h != chain[i] {
			t.Fatal("filter-header chain is not deterministic")
		}
	}
	// Each header must depend on its predecessor (changing an early filter
	// changes it and every later header).
	altered := append([]BlockFilter(nil), fs...)
	other, _ := wallet.New()
	altered[1] = BuildBlockFilter(blockWith(1, altered[1].BlockHash, other.Address(), other.Address()))
	alt := FilterHeaderChain(altered)
	if alt[0] != chain[0] {
		t.Error("header before the change should be unaffected")
	}
	for i := 1; i < len(chain); i++ {
		if alt[i] == chain[i] {
			t.Errorf("header %d should change after altering filter 1", i)
		}
	}
}

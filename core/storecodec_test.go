package core

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// The store's record format changed from JSON to a compact binary encoding. A
// bug here silently corrupts a chain store, so the bar is higher than "the
// fields look right": every case round-trips and is compared by the block's
// HASH, which covers every field the header commits, plus the transactions are
// compared by txid, which covers every field they encode.

// allShapesBlock builds one block carrying every transaction shape the codec
// has a branch for. If a shape is missing here, its branch is untested.
func allShapesBlock(t *testing.T) Block {
	t.Helper()
	a, _ := wallet.New()
	b, _ := wallet.New()
	c, _ := wallet.New()

	ms, err := wallet.MultisigAddress(2, []string{a.PublicKeyHex(), b.PublicKeyHex(), c.PublicKeyHex()})
	if err != nil {
		t.Fatal(err)
	}
	const preimageHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	plain := signedTx(t, a, b.Address(), 3*Coin, testFee, 0)
	memoWindow := Transaction{
		From: a.Address(), To: b.Address(), Amount: Coin, Fee: testFee, Nonce: 1,
		Expiry: 900, LockUntil: 800, Memo: "two coffees — with a non-ASCII dash",
	}
	if err := memoWindow.Sign(a); err != nil {
		t.Fatal(err)
	}
	multi := Transaction{From: a.Address(), Fee: testFee, Nonce: 2, Outputs: []Output{
		{To: b.Address(), Amount: Coin}, {To: c.Address(), Amount: 2 * Coin},
	}}
	if err := multi.Sign(a); err != nil {
		t.Fatal(err)
	}
	issue := Transaction{From: a.Address(), Fee: testFee, Nonce: 3,
		Issue: &AssetIssue{Ticker: "GOLD", Supply: 1_000_000}}
	if err := issue.Sign(a); err != nil {
		t.Fatal(err)
	}
	assetMove := Transaction{From: a.Address(), To: b.Address(), Amount: 25, Fee: testFee,
		Nonce: 4, AssetID: AssetID(a.Address(), "GOLD", 3)}
	if err := assetMove.Sign(a); err != nil {
		t.Fatal(err)
	}
	sponsored := Transaction{From: a.Address(), To: b.Address(), Amount: Coin, Fee: testFee,
		Nonce: 5, FeePayer: c.Address(), FeePayerPubKey: c.PublicKeyHex(),
		FeePayerSig: "aabbccdd"}
	if err := sponsored.Sign(a); err != nil {
		t.Fatal(err)
	}
	multisigSpend := Transaction{
		From: ms, To: b.Address(), Amount: Coin, Fee: testFee, Nonce: 0,
		Multisig:   &MultisigScript{Threshold: 2, PubKeys: []string{a.PublicKeyHex(), b.PublicKeyHex(), c.PublicKeyHex()}},
		Signatures: []string{"1111", "2222"},
	}
	htlcSpend := Transaction{
		From: b.Address(), To: c.Address(), Amount: Coin, Fee: testFee, Nonce: 9,
		HTLC:     &HTLCScript{Hash: preimageHash, Recipient: b.PublicKeyHex(), Sender: a.PublicKeyHex(), Timeout: 500},
		Preimage: "74657374",
	}
	vaultSpend := Transaction{
		From: c.Address(), To: a.Address(), Amount: Coin, Fee: testFee, Nonce: 7,
		Vault: &VaultScript{Hot: a.PublicKeyHex(), Cold: b.PublicKeyHex(), Unlock: 1000},
	}

	return Block{
		Index: 42, Timestamp: GenesisTimestamp + 1234,
		PrevHash: hashBytes([]byte("prev")), MerkleRoot: hashBytes([]byte("merkle")),
		StateRoot: hashBytes([]byte("state")), BaseFee: 17, Bits: GenesisBits, Nonce: 99,
		Hash: hashBytes([]byte("hash")),
		Transactions: []Transaction{
			NewCoinbase(a.Address(), InitialBlockReward),
			plain, memoWindow, multi, issue, assetMove, sponsored,
			multisigSpend, htlcSpend, vaultSpend,
		},
	}
}

func TestStoreCodecRoundTripsEveryShape(t *testing.T) {
	orig := allShapesBlock(t)
	got, err := decodeStoredBlock(encodeBlockV4(orig))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Header fidelity, via the hash the header commits to.
	if got.Header().ComputeHash() != orig.Header().ComputeHash() {
		t.Errorf("header changed across the round trip:\n  want %s\n  got  %s",
			orig.Header().ComputeHash(), got.Header().ComputeHash())
	}
	if got.Hash != orig.Hash {
		t.Errorf("stored Hash = %s, want %s", got.Hash, orig.Hash)
	}
	if got.Timestamp != orig.Timestamp {
		t.Errorf("Timestamp = %d, want %d", got.Timestamp, orig.Timestamp)
	}

	// Transaction fidelity, via txid — which covers every field the canonical
	// encoding includes, including the optional blocks.
	if len(got.Transactions) != len(orig.Transactions) {
		t.Fatalf("got %d transactions, want %d", len(got.Transactions), len(orig.Transactions))
	}
	for i := range orig.Transactions {
		if got.Transactions[i].Hash() != orig.Transactions[i].Hash() {
			t.Errorf("transaction %d txid changed: %s -> %s",
				i, orig.Transactions[i].Hash()[:12], got.Transactions[i].Hash()[:12])
		}
	}

	// And the merkle root, which is what a block's validity actually turns on.
	if MerkleRoot(got.Transactions) != MerkleRoot(orig.Transactions) {
		t.Error("the merkle root over the decoded transactions differs")
	}
}

// A negative timestamp must survive: it is an int64 and the encoding stores it
// as a u64, which is only correct if the conversion round-trips.
func TestStoreCodecHandlesNegativeAndExtremeValues(t *testing.T) {
	for _, b := range []Block{
		{Index: 0, Timestamp: -1, Bits: 1},
		{Index: ^uint64(0), Timestamp: -(1 << 62), BaseFee: ^uint64(0), Nonce: ^uint64(0), Bits: ^uint32(0)},
		{Index: 7, Timestamp: 0},
	} {
		got, err := decodeStoredBlock(encodeBlockV4(b))
		if err != nil {
			t.Fatalf("decode %+v: %v", b, err)
		}
		if got.Timestamp != b.Timestamp || got.Index != b.Index ||
			got.BaseFee != b.BaseFee || got.Nonce != b.Nonce || got.Bits != b.Bits {
			t.Errorf("extreme values changed:\n  want %+v\n  got  %+v", b, got)
		}
	}
}

// Hex compaction is the source of the saving and the likeliest place to lose
// data. Only values that re-encode identically may take the compact path.
func TestHexCompactionIsLossless(t *testing.T) {
	cases := map[string]bool{ // value -> should take the hex path
		"":                                 false, // empty
		"abc":                              false, // odd length
		"ABCDEF":                           false, // uppercase would re-encode lowercase
		"dnas1234":                         false, // not hex at all
		"zz":                               false,
		"00":                               true,
		"deadbeef":                         true,
		"9f86d081884c7d659a2feaa0c55ad015": true,
	}
	for v, wantHex := range cases {
		_, isHex := decodeHexLower(v)
		if isHex != wantHex {
			t.Errorf("decodeHexLower(%q) took hex path = %v, want %v", v, isHex, wantHex)
		}
		// Whatever path it takes, a round trip through a record must be exact.
		s := &sbuf{}
		s.str(v)
		r := &srdr{b: s.b}
		if got := r.str(); got != v {
			t.Errorf("string %q round-tripped to %q", v, got)
		}
		if r.err != nil {
			t.Errorf("string %q: %v", v, r.err)
		}
	}
}

// An address is not hex (it has a "dnas" prefix), a signature is. Both must
// survive, and the test names them so a future format change cannot quietly
// route one down the wrong path.
func TestStoreCodecKeepsAddressesAndSignaturesExact(t *testing.T) {
	w, _ := wallet.New()
	tx := signedTx(t, w, w.Address(), Coin, testFee, 0)
	blk := Block{Index: 1, Transactions: []Transaction{tx}}

	got, err := decodeStoredBlock(encodeBlockV4(blk))
	if err != nil {
		t.Fatal(err)
	}
	dec := got.Transactions[0]
	if dec.From != tx.From {
		t.Errorf("From %q -> %q", tx.From, dec.From)
	}
	if dec.PubKey != tx.PubKey {
		t.Errorf("PubKey %q -> %q", tx.PubKey, dec.PubKey)
	}
	if dec.Signature != tx.Signature {
		t.Errorf("Signature %q -> %q", tx.Signature, dec.Signature)
	}
	if !strings.HasPrefix(dec.From, "dnas") {
		t.Error("the address lost its prefix")
	}
}

// The point of the change: the compact form must actually be smaller.
func TestBinaryRecordsAreSmallerThanJSON(t *testing.T) {
	blk := allShapesBlock(t)
	jsonBytes, err := json.Marshal(blk)
	if err != nil {
		t.Fatal(err)
	}
	binBytes := encodeBlockV4(blk)
	if len(binBytes) >= len(jsonBytes) {
		t.Errorf("binary record is %d bytes, JSON is %d — no saving", len(binBytes), len(jsonBytes))
	}
	t.Logf("block with %d transactions: JSON %d bytes -> binary %d bytes (%.0f%% of JSON)",
		len(blk.Transactions), len(jsonBytes), len(binBytes),
		100*float64(len(binBytes))/float64(len(jsonBytes)))
}

// A store written by an older build holds JSON records. Those must still load,
// or upgrading the binary would orphan every existing chain.
func TestStoreReadsLegacyJSONRecords(t *testing.T) {
	blk := allShapesBlock(t)
	data, err := json.Marshal(blk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeStoredBlock(data)
	if err != nil {
		t.Fatalf("a legacy JSON record must still decode: %v", err)
	}
	if got.Header().ComputeHash() != blk.Header().ComputeHash() {
		t.Error("a legacy JSON record decoded to a different block")
	}
}

func TestStoreRejectsUnrecognizedRecords(t *testing.T) {
	for _, data := range [][]byte{
		{},
		{0xff, 0x01, 0x02},
		{storeRecordV4},             // tag only: truncated
		{storeRecordV4, 0x00, 0x00}, // truncated mid-field
		append([]byte{storeRecordV4}, make([]byte, 8)...),
	} {
		if _, err := decodeStoredBlock(data); err == nil {
			t.Errorf("decodeStoredBlock(%v) should have failed", data)
		}
	}
}

// Trailing bytes mean the record and the framing disagree, which is corruption
// however plausible the prefix looked.
func TestStoreRejectsTrailingBytes(t *testing.T) {
	blk := Block{Index: 1, Timestamp: 5}
	data := append(encodeBlockV4(blk), 0x00)
	if _, err := decodeStoredBlock(data); err == nil {
		t.Error("a record with trailing bytes should be refused")
	}
}

// End to end: a chain written with the new format must reopen, and a chain
// written in the OLD format must still reopen after the upgrade.
func TestChainRoundTripsThroughTheBinaryStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	sink, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	fatBlocks(t, bc, miner, sink.Address(), 15)
	wantTip, wantHeight := bc.Tip().Hash, bc.Height()
	wantBal := bc.Balance(sink.Address())
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen a binary store: %v", err)
	}
	defer reopened.Close()
	if reopened.Height() != wantHeight || reopened.Tip().Hash != wantTip {
		t.Errorf("height %d tip %s, want %d / %s",
			reopened.Height(), reopened.Tip().Hash[:12], wantHeight, wantTip[:12])
	}
	if reopened.Balance(sink.Address()) != wantBal {
		t.Error("balances did not survive the binary store")
	}
}

func BenchmarkStoreEncodeBlock(b *testing.B) {
	blk := Block{Index: 1}
	w, _ := wallet.New()
	for i := 0; i < 200; i++ {
		tx := Transaction{From: w.Address(), To: w.Address(), Amount: Coin, Fee: 10, Nonce: uint64(i)}
		_ = tx.Sign(w)
		blk.Transactions = append(blk.Transactions, tx)
	}
	b.Run("binary", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = encodeBlockV4(blk)
		}
	})
	b.Run("json", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := json.Marshal(blk); err != nil {
				b.Fatal(err)
			}
		}
	})
	_ = fmt.Sprint()
}

// Startup cost is a DECODE cost: every record in the file is parsed on open.
// Encoding happens once per block. Both are measured because the first version
// of this codec was slower to encode than the JSON it replaced.
func BenchmarkStoreDecodeBlock(b *testing.B) {
	blk := Block{Index: 1}
	w, _ := wallet.New()
	for i := 0; i < 200; i++ {
		tx := Transaction{From: w.Address(), To: w.Address(), Amount: Coin, Fee: 10, Nonce: uint64(i)}
		_ = tx.Sign(w)
		blk.Transactions = append(blk.Transactions, tx)
	}
	binData := encodeBlockV4(blk)
	jsonData, err := json.Marshal(blk)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("record size: binary %d, json %d", len(binData), len(jsonData))
	b.Run("binary", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := decodeBlockV4(binData); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("json", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var out Block
			if err := json.Unmarshal(jsonData, &out); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// A store written before the header carried a Version must still load: the tag
// byte tells the two records apart, and a V2 block decodes with Version 0, which
// is what those blocks were mined with — so the chain replays to the same hashes
// it always did rather than failing to open.
func TestStoreReadsPreVersionRecords(t *testing.T) {
	blk := allShapesBlock(t)
	blk.Version = 0
	blk.Hash = blk.ComputeHash()

	got, err := decodeStoredBlock(encodeBlockV2(blk))
	if err != nil {
		t.Fatalf("an old record failed to load: %v", err)
	}
	if got.Hash != blk.Hash || got.ComputeHash() != blk.Hash {
		t.Fatalf("v2 record round-tripped to a different block (hash %s, want %s)", got.ComputeHash(), blk.Hash)
	}
	if got.Version != 0 {
		t.Errorf("v2 record decoded with version %d, want 0", got.Version)
	}

	// And the current format carries the field through unchanged.
	blk.Version = SignalVersion(3, 9)
	blk.Hash = blk.ComputeHash()
	got, err = decodeStoredBlock(encodeBlockV4(blk))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != blk.Version || got.ComputeHash() != blk.Hash {
		t.Fatalf("the current record lost the version: got %#x, want %#x", got.Version, blk.Version)
	}

	// A V3 record — written after the header gained a version but before assets
	// could be minted — must still load, with no operation on its transactions.
	v3, err := decodeStoredBlock(encodeBlockV3(blk))
	if err != nil {
		t.Fatalf("a v3 record failed to load: %v", err)
	}
	if v3.ComputeHash() != blk.Hash {
		t.Fatalf("v3 record round-tripped to a different block")
	}
}

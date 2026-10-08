package core

import "testing"

// The wire encodings must round-trip exactly — a block that changes shape in
// transit changes its hash, and a peer would reject what it was sent.
func TestWireCodecRoundTrips(t *testing.T) {
	tx := Transaction{From: "dnasfrom", To: "dnasto", Amount: 5 * Coin, Fee: 10,
		Nonce: 3, Memo: "hello", PubKey: "aa", Signature: "bb"}
	gotTx, err := DecodeTransaction(EncodeTransaction(tx))
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	if gotTx.Hash() != tx.Hash() {
		t.Fatalf("transaction hash changed in transit: %s -> %s", tx.Hash()[:12], gotTx.Hash()[:12])
	}
	if gotTx.Signature != tx.Signature || gotTx.PubKey != tx.PubKey {
		t.Fatal("the signature did not survive: a peer could not verify this")
	}

	b := Block{Version: 2, Index: 9, Timestamp: 1700000000, PrevHash: "aa",
		MerkleRoot: "bb", StateRoot: "cc", BaseFee: 7, Bits: 0x1f0fffff, Nonce: 11,
		Hash: "dd", FilterRoot: "ee", HistoryRoot: "ff",
		Transactions: []Transaction{NewCoinbase("dnasminer", 50*Coin), tx}}
	gotB, err := DecodeBlock(EncodeBlock(b))
	if err != nil {
		t.Fatalf("block: %v", err)
	}
	if gotB.ComputeHash() != b.ComputeHash() {
		t.Fatal("block hash changed in transit")
	}
	if len(gotB.Transactions) != 2 {
		t.Fatalf("block carried %d transactions, want 2", len(gotB.Transactions))
	}

	gotH, err := DecodeHeader(EncodeHeader(b.Header()))
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if gotH.ComputeHash() != b.Header().ComputeHash() {
		t.Fatal("header hash changed in transit")
	}
	if gotH.FilterRoot != "ee" || gotH.HistoryRoot != "ff" {
		t.Fatal("the scheduled commitments did not survive the header encoding")
	}
}

func TestDecodeTransactionRejectsGarbage(t *testing.T) {
	if _, err := DecodeTransaction(nil); err == nil {
		t.Fatal("empty input accepted")
	}
	if _, err := DecodeTransaction([]byte{0xff, 0x00}); err == nil {
		t.Fatal("an unknown record version was accepted")
	}
	if _, err := DecodeTransaction([]byte{storeRecordV5}); err == nil {
		t.Fatal("a truncated record was accepted")
	}
}

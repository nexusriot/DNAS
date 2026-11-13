package core

import "testing"

// Every optional transaction field must survive the binary codec — which is
// both the block store and the peer wire format — with its HASH intact. V5
// silently dropped Script, Name and Issue.Metadata, so a block holding any of
// them decoded to a different merkle root on every peer that received it.
func TestCodecRoundTripsEveryOptionalField(t *testing.T) {
	cases := map[string]Transaction{
		"script": {From: "dnasaa", To: "dnasbb", Amount: 5, Fee: 1, Nonce: 2,
			Script: &ScriptAuth{Script: "51", Args: []string{"aa", "bbcc"}}},
		"script-no-args": {From: "dnasaa", To: "dnasbb", Amount: 5,
			Script: &ScriptAuth{Script: "5151"}},
		"name": {From: "dnasaa", Fee: 9, Nonce: 1,
			Name: &NameOp{Op: "register", Name: "alice", Target: "dnascc", Periods: 3}},
		"nft": {From: "dnasaa", Fee: 9,
			Issue: &AssetIssue{Ticker: "ART", Supply: 1, Metadata: "ab12"}},
		"fungible-issue": {From: "dnasaa", Fee: 9, Issue: &AssetIssue{Ticker: "GOLD", Supply: 100}},
		"plain":          {From: "dnasaa", To: "dnasbb", Amount: 1, Fee: 1},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeTransaction(EncodeTransaction(tx))
			if err != nil {
				t.Fatal(err)
			}
			if got.Hash() != tx.Hash() {
				t.Fatalf("transaction hash changed across the codec: %+v -> %+v", tx, got)
			}
			b := Block{Index: 1, Transactions: []Transaction{NewCoinbase("dnasminer", 1), tx}}
			b.MerkleRoot = MerkleRoot(b.Transactions)
			db, err := DecodeBlock(EncodeBlock(b))
			if err != nil {
				t.Fatal(err)
			}
			if MerkleRoot(db.Transactions) != b.MerkleRoot {
				t.Fatal("block merkle root changed across the codec")
			}
			st, err := encodeStoredBlock(b)
			if err != nil {
				t.Fatal(err)
			}
			sb, err := decodeStoredBlock(st)
			if err != nil || MerkleRoot(sb.Transactions) != b.MerkleRoot {
				t.Fatalf("store round trip lost data: %v", err)
			}
		})
	}
}

// Blocks without the new fields keep their V5 bytes, so existing stores and
// older peers see no change.
func TestCodecKeepsV5ForOrdinaryBlocks(t *testing.T) {
	b := Block{Index: 1, Transactions: []Transaction{NewCoinbase("dnasminer", 1),
		{From: "dnasaa", To: "dnasbb", Amount: 1, Fee: 1}}}
	if got := EncodeBlock(b)[0]; got != storeRecordV5 {
		t.Fatalf("ordinary block encoded as record %#x, want V5", got)
	}
	b.Transactions[1].Name = &NameOp{Op: "release", Name: "x"}
	if got := EncodeBlock(b)[0]; got != storeRecordV6 {
		t.Fatalf("block with a name op encoded as record %#x, want V6", got)
	}
}

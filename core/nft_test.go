package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func metaHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func nftChain(t *testing.T, issuer *wallet.Wallet) (*Blockchain, *Mempool) {
	t.Helper()
	withUpgradeAt(t, UpgradeNonFungible, 0)
	withUpgradeAt(t, UpgradeAssetOps, 0)
	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, issuer.Address(), nil))
	}
	matureCoinbase(t, bc)
	return bc, NewMempoolWithPolicy(64, 0).UseAccounts(bc)
}

func issueNFT(t *testing.T, w *wallet.Wallet, ticker, meta string, nonce uint64) Transaction {
	t.Helper()
	tx := Transaction{From: w.Address(), Fee: testFee, Nonce: nonce,
		Issue: &AssetIssue{Ticker: ticker, Supply: 1, Metadata: meta}}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestNonFungibleIssuanceAndTransfer(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	bc, mp := nftChain(t, alice)

	meta := metaHash("a picture of a duck")
	tx := issueNFT(t, alice, "DUCK", meta, 0)
	if ok, err := mp.Add(tx); !ok || err != nil {
		t.Fatalf("issue: ok=%v err=%v", ok, err)
	}
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), mp.Select(bc, 10)))
	mp.Reconcile(bc.Height())

	id := NonFungibleAssetID(alice.Address(), "DUCK", 0, meta)
	if !IsNonFungibleID(id) {
		t.Fatalf("id %q does not read as non-fungible", id)
	}
	if got := bc.Account(alice.Address()).Assets[id]; got != 1 {
		t.Fatalf("issuer holds %d of the token, want 1", got)
	}

	info, ok := bc.Asset(id)
	if !ok {
		t.Fatal("the token is not in the registry")
	}
	if !info.NonFungible || info.Metadata != meta || info.Supply != 1 {
		t.Fatalf("registry says %+v", info)
	}

	// It transfers like any other asset.
	move := Transaction{From: alice.Address(), To: bob.Address(), Amount: 1,
		AssetID: id, Fee: testFee, Nonce: 1}
	if err := move.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if ok, err := mp.Add(move); !ok || err != nil {
		t.Fatalf("transfer: ok=%v err=%v", ok, err)
	}
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), mp.Select(bc, 10)))
	if got := bc.Account(bob.Address()).Assets[id]; got != 1 {
		t.Fatalf("bob holds %d of the token, want 1", got)
	}
	if _, still := bc.Account(alice.Address()).Assets[id]; still {
		t.Fatal("the issuer still holds a token it transferred away")
	}
}

// The point of the thing: an issuer must not be able to inflate a token it
// sold as unique. This is enforced by the id derivation rather than a lookup,
// so the test checks that a mint naming it simply cannot prove authority.
func TestNonFungibleCannotBeMintedOrBurned(t *testing.T) {
	alice, _ := wallet.New()
	bc, mp := nftChain(t, alice)

	meta := metaHash("unique thing")
	if ok, err := mp.Add(issueNFT(t, alice, "ONE", meta, 0)); !ok || err != nil {
		t.Fatalf("issue: %v", err)
	}
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), mp.Select(bc, 10)))
	mp.Reconcile(bc.Height())
	id := NonFungibleAssetID(alice.Address(), "ONE", 0, meta)

	for _, op := range []string{AssetOpMint, AssetOpBurn} {
		tx := Transaction{From: alice.Address(), AssetID: id, Fee: testFee, Nonce: 1,
			AssetOp: &AssetOp{Op: op, Amount: 1000, Ticker: "ONE", IssueNonce: 0}}
		if err := tx.Sign(alice); err != nil {
			t.Fatal(err)
		}
		// It must fail at CONSENSUS, not merely at relay: a miner that included
		// it anyway must produce a block its peers reject.
		blk := mineOn(t, bc, alice.Address(), []Transaction{tx})
		if err := bc.AddBlock(blk); err == nil {
			t.Fatalf("%s of a non-fungible asset was accepted into a block", op)
		}
	}
	if info, ok := bc.Asset(id); !ok || info.Supply != 1 {
		t.Fatalf("supply moved to %d (present=%v)", info.Supply, ok)
	}
}

// A fungible asset must still mint and burn exactly as before: the new
// derivation must not have moved any existing id.
func TestFungibleAssetIDUnchanged(t *testing.T) {
	alice, _ := wallet.New()
	fungible := &AssetIssue{Ticker: "GOLD", Supply: 1000}
	if got, want := IssuedAssetID(alice.Address(), 7, fungible), AssetID(alice.Address(), "GOLD", 7); got != want {
		t.Fatalf("a fungible issuance derives %s, want the unchanged %s", got, want)
	}
	if IsNonFungibleID(AssetID(alice.Address(), "GOLD", 7)) {
		t.Fatal("a fungible id reads as non-fungible")
	}
}

// The metadata is part of the id, so the same issuer, ticker and nonce with
// different content is a different token — not the same one re-pointed.
func TestMetadataIsBoundIntoTheID(t *testing.T) {
	a := NonFungibleAssetID("dnasissuer", "ART", 1, metaHash("first"))
	b := NonFungibleAssetID("dnasissuer", "ART", 1, metaHash("second"))
	if a == b {
		t.Fatal("changing the metadata did not change the id")
	}
}

// Supply must be exactly one, and the commitment must be a real digest —
// otherwise the field is a free place to park bytes in the state root.
func TestNonFungibleIssuanceValidation(t *testing.T) {
	alice, _ := wallet.New()
	bc, _ := nftChain(t, alice)

	for name, issue := range map[string]*AssetIssue{
		"supply above one":   {Ticker: "X", Supply: 2, Metadata: metaHash("x")},
		"metadata too short": {Ticker: "X", Supply: 1, Metadata: "abcd"},
		"metadata not hex":   {Ticker: "X", Supply: 1, Metadata: strings.Repeat("z", 64)},
		"metadata uppercase": {Ticker: "X", Supply: 1, Metadata: strings.ToUpper(metaHash("x"))},
	} {
		t.Run(name, func(t *testing.T) {
			tx := Transaction{From: alice.Address(), Fee: testFee, Nonce: 0, Issue: issue}
			if err := tx.Sign(alice); err != nil {
				t.Fatal(err)
			}
			if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err == nil {
				t.Fatal("an invalid non-fungible issuance was accepted")
			}
		})
	}
}

// Before activation, an issuance carrying metadata must be refused outright,
// or nodes would disagree about whether a block is valid.
func TestNonFungibleRejectedBeforeActivation(t *testing.T) {
	alice, _ := wallet.New()
	ClearUpgradeHeight(UpgradeNonFungible)
	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	}
	matureCoinbase(t, bc)

	tx := issueNFT(t, alice, "EARLY", metaHash("too soon"), 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err == nil {
		t.Fatal("a non-fungible issuance was accepted before its upgrade activated")
	}
}

// Adding the metadata field must not change the encoding of a transaction that
// does not carry it — that is what keeps existing txids and stored chains valid.
func TestFungibleIssuanceEncodingUnchangedByNFTField(t *testing.T) {
	alice, _ := wallet.New()
	plain := Transaction{From: alice.Address(), Fee: testFee, Nonce: 0,
		Issue: &AssetIssue{Ticker: "GOLD", Supply: 100}}
	before := plain.Hash()

	withUpgradeAt(t, UpgradeNonFungible, 0)
	if after := plain.Hash(); after != before {
		t.Fatalf("scheduling the upgrade changed a fungible issuance's txid: %s -> %s",
			before[:12], after[:12])
	}
	// And a token WITH metadata must hash differently, or the commitment is not
	// actually signed.
	nft := plain
	nft.Issue = &AssetIssue{Ticker: "GOLD", Supply: 1, Metadata: metaHash("x")}
	if nft.Hash() == before {
		t.Fatal("the metadata is not covered by the transaction id")
	}
}

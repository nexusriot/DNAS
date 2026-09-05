package core

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A hash-time-locked contract can hold a native ASSET, not just coin — which is
// what makes an on-chain asset-for-coin swap possible with the primitive that
// already exists. The one wrinkle is that fees are always paid in coin, so an
// asset contract must be funded with a little coin of its own or its claim
// cannot pay its way into a block.
func TestAssetHTLCClaim(t *testing.T) {
	bc := NewBlockchain()
	issuer, _ := wallet.New()
	recipient, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), nil))
	matureCoinbase(t, bc)

	issue := Transaction{From: issuer.Address(), Fee: testFee, Nonce: 0, Issue: &AssetIssue{Ticker: "GOLD", Supply: 1000}}
	if err := issue.Sign(issuer); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{issue}))
	assetID := AssetID(issuer.Address(), "GOLD", 0)

	preimage := []byte("swap-secret-000000000000000000000")
	sum := sha256.Sum256(preimage)
	hashHex := hex.EncodeToString(sum[:])
	htlcAddr, err := wallet.HTLCAddress(hashHex, recipient.PublicKeyHex(), issuer.PublicKeyHex(), 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Fund the contract with the asset AND with coin for its own fee.
	fundAsset := Transaction{From: issuer.Address(), To: htlcAddr, Amount: 500, Fee: testFee, Nonce: 1, AssetID: assetID}
	if err := fundAsset.Sign(issuer); err != nil {
		t.Fatal(err)
	}
	fundCoin := signedTx(t, issuer, htlcAddr, 10*testFee, testFee, 2)
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{fundAsset, fundCoin}))
	if got := bc.Account(htlcAddr).Assets[assetID]; got != 500 {
		t.Fatalf("htlc asset balance = %d, want 500", got)
	}

	claim := Transaction{From: htlcAddr, To: recipient.Address(), Amount: 500, Fee: testFee, Nonce: 0,
		AssetID: assetID,
		HTLC:    &HTLCScript{Hash: hashHex, Recipient: recipient.PublicKeyHex(), Sender: issuer.PublicKeyHex(), Timeout: 1000}}
	claim.SignHTLCClaim(recipient, preimage)
	if err := bc.AddBlock(mineOn(t, bc, issuer.Address(), []Transaction{claim})); err != nil {
		t.Fatalf("asset htlc claim rejected: %v", err)
	}
	if got := bc.Account(recipient.Address()).Assets[assetID]; got != 500 {
		t.Fatalf("recipient asset balance = %d, want 500", got)
	}
}

// The refund branch works the same way for an asset: after the timeout the
// sender reclaims what the recipient never took.
func TestAssetHTLCRefund(t *testing.T) {
	bc := NewBlockchain()
	issuer, _ := wallet.New()
	recipient, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), nil))
	matureCoinbase(t, bc)

	issue := Transaction{From: issuer.Address(), Fee: testFee, Nonce: 0, Issue: &AssetIssue{Ticker: "SILV", Supply: 100}}
	if err := issue.Sign(issuer); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{issue}))
	assetID := AssetID(issuer.Address(), "SILV", 0)

	sum := sha256.Sum256([]byte("never-revealed"))
	hashHex := hex.EncodeToString(sum[:])
	timeout := bc.Height() + 5
	htlcAddr, err := wallet.HTLCAddress(hashHex, recipient.PublicKeyHex(), issuer.PublicKeyHex(), timeout)
	if err != nil {
		t.Fatal(err)
	}
	fundAsset := Transaction{From: issuer.Address(), To: htlcAddr, Amount: 100, Fee: testFee, Nonce: 1, AssetID: assetID}
	if err := fundAsset.Sign(issuer); err != nil {
		t.Fatal(err)
	}
	fundCoin := signedTx(t, issuer, htlcAddr, 10*testFee, testFee, 2)
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{fundAsset, fundCoin}))

	refund := Transaction{From: htlcAddr, To: issuer.Address(), Amount: 100, Fee: testFee, Nonce: 0,
		AssetID: assetID,
		HTLC:    &HTLCScript{Hash: hashHex, Recipient: recipient.PublicKeyHex(), Sender: issuer.PublicKeyHex(), Timeout: timeout}}
	refund.SignHTLCRefund(issuer)

	// Too early: the refund branch is not open yet.
	if err := bc.AddBlock(mineOn(t, bc, issuer.Address(), []Transaction{refund})); err == nil {
		t.Fatal("an asset refund was accepted before the timeout")
	}
	for bc.Height() < timeout {
		mustAdd(t, bc, mineOn(t, bc, issuer.Address(), nil))
	}
	if err := bc.AddBlock(mineOn(t, bc, issuer.Address(), []Transaction{refund})); err != nil {
		t.Fatalf("asset refund rejected after the timeout: %v", err)
	}
	if got := bc.Account(htlcAddr).Assets[assetID]; got != 0 {
		t.Fatalf("contract still holds %d of the asset after the refund", got)
	}
}

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// htlcFixture funds a fresh HTLC (recipient=alice, sender=bob) with a matured
// coinbase and returns the pieces a spend needs.
func htlcFixture(t *testing.T, timeout uint64) (bc *Blockchain, alice, bob *wallet.Wallet, preimage []byte, script *HTLCScript, addr string) {
	t.Helper()
	bc = NewBlockchain()
	alice, _ = wallet.New()
	bob, _ = wallet.New()
	preimage = []byte("the-secret-preimage-revealed-on-claim")
	sum := sha256.Sum256(preimage)
	hash := hex.EncodeToString(sum[:])
	var err error
	addr, err = wallet.HTLCAddress(hash, alice.PublicKeyHex(), bob.PublicKeyHex(), timeout)
	if err != nil {
		t.Fatal(err)
	}
	script = &HTLCScript{Hash: hash, Recipient: alice.PublicKeyHex(), Sender: bob.PublicKeyHex(), Timeout: timeout}

	if err := bc.AddBlock(mineOn(t, bc, addr, nil)); err != nil { // fund via coinbase
		t.Fatal(err)
	}
	if bc.Balance(addr) != BlockReward(1) {
		t.Fatalf("htlc not funded: %d", bc.Balance(addr))
	}
	matureCoinbase(t, bc)
	return
}

func TestHTLCClaimOnChain(t *testing.T) {
	bc, alice, _, preimage, script, addr := htlcFixture(t, 1000)
	miner, _ := wallet.New()

	claim := Transaction{From: addr, To: alice.Address(), Amount: 10 * Coin, Fee: Coin, Nonce: 0, HTLC: script}
	claim.SignHTLCClaim(alice, preimage)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{claim})); err != nil {
		t.Fatalf("valid claim should be accepted: %v", err)
	}
	if bc.Balance(alice.Address()) != 10*Coin {
		t.Fatalf("alice = %d, want %d", bc.Balance(alice.Address()), 10*Coin)
	}
	if want := BlockReward(1) - 11*Coin; bc.Balance(addr) != want {
		t.Fatalf("htlc balance = %d, want %d", bc.Balance(addr), want)
	}
	// The preimage is now published on-chain — the property atomic swaps rely on.
	proof, _ := bc.FindTxProof(claim.Hash())
	if proof.Tx.Preimage != hex.EncodeToString(preimage) {
		t.Fatal("claim must publish the preimage on-chain")
	}
}

func TestHTLCRefundRespectsTimeout(t *testing.T) {
	// Chain height after funding+maturity is 1+CoinbaseMaturity. Pick a timeout a
	// couple of blocks ahead so we can test both sides of it.
	timeout := uint64(1 + CoinbaseMaturity + 2)
	bc, _, bob, _, script, addr := htlcFixture(t, timeout)
	miner, _ := wallet.New()

	// Refund before the timeout must be rejected by consensus.
	early := Transaction{From: addr, To: bob.Address(), Amount: 5 * Coin, Fee: Coin, Nonce: 0, HTLC: script}
	early.SignHTLCRefund(bob)
	if bc.Height()+1 >= timeout {
		t.Fatalf("test setup: next height %d already >= timeout %d", bc.Height()+1, timeout)
	}
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{early})); err == nil {
		t.Fatal("refund before timeout must be rejected")
	}

	// Advance to the timeout height with empty blocks, then the refund is valid.
	for bc.Height()+1 < timeout {
		if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	refund := Transaction{From: addr, To: bob.Address(), Amount: 5 * Coin, Fee: Coin, Nonce: 0, HTLC: script}
	refund.SignHTLCRefund(bob)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{refund})); err != nil {
		t.Fatalf("refund at/after timeout should be accepted: %v", err)
	}
	if bc.Balance(bob.Address()) != 5*Coin {
		t.Fatalf("bob = %d, want %d", bc.Balance(bob.Address()), 5*Coin)
	}
}

func TestHTLCVerificationFailures(t *testing.T) {
	alice, _ := wallet.New() // recipient
	bob, _ := wallet.New()   // sender
	outsider, _ := wallet.New()
	preimage := []byte("correct-horse-battery-staple")
	sum := sha256.Sum256(preimage)
	hash := hex.EncodeToString(sum[:])
	addr, _ := wallet.HTLCAddress(hash, alice.PublicKeyHex(), bob.PublicKeyHex(), 50)
	script := &HTLCScript{Hash: hash, Recipient: alice.PublicKeyHex(), Sender: bob.PublicKeyHex(), Timeout: 50}

	base := func() Transaction {
		return Transaction{From: addr, To: alice.Address(), Amount: Coin, Nonce: 0, HTLC: script}
	}

	// Valid claim verifies.
	ok := base()
	ok.SignHTLCClaim(alice, preimage)
	if err := ok.VerifySignature(); err != nil {
		t.Fatalf("valid claim should verify: %v", err)
	}

	// Wrong preimage.
	badPre := base()
	badPre.SignHTLCClaim(alice, []byte("wrong-secret"))
	if badPre.VerifySignature() == nil {
		t.Error("claim with a wrong preimage must fail")
	}

	// Correct preimage but signed by someone who is not the recipient.
	badSigner := base()
	badSigner.SignHTLCClaim(outsider, preimage)
	if badSigner.VerifySignature() == nil {
		t.Error("claim not signed by the recipient must fail")
	}

	// Refund signed by the recipient (only the sender may refund).
	badRefund := base()
	badRefund.SignHTLCRefund(alice)
	if badRefund.VerifySignature() == nil {
		t.Error("refund not signed by the sender must fail")
	}

	// A script that does not hash to From (timeout differs from the funded one).
	forged := base()
	forged.HTLC = &HTLCScript{Hash: hash, Recipient: alice.PublicKeyHex(), Sender: bob.PublicKeyHex(), Timeout: 999}
	forged.SignHTLCClaim(alice, preimage)
	if forged.VerifySignature() == nil {
		t.Error("a script not matching the sender address must fail")
	}

	// Tampering with the amount after signing invalidates the signature.
	tampered := base()
	tampered.SignHTLCClaim(alice, preimage)
	tampered.Amount = 999 * Coin
	if tampered.VerifySignature() == nil {
		t.Error("a tampered claim must fail")
	}
}

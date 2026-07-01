package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// mineOn builds and mines a valid block (coinbase to minerAddr + txs) on the tip.
func mineOn(t *testing.T, bc *Blockchain, minerAddr string, txs []Transaction) Block {
	t.Helper()
	tip := bc.Tip()
	var fees uint64
	for _, tx := range txs {
		fees += tx.Fee
	}
	cb := NewCoinbase(minerAddr, BlockReward(tip.Index+1)+fees)
	block := Block{
		Index:        tip.Index + 1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: append([]Transaction{cb}, txs...),
		PrevHash:     tip.Hash,
		Difficulty:   bc.NextDifficulty(),
	}
	mined, ok := Mine(block, nil)
	if !ok {
		t.Fatal("mining aborted unexpectedly")
	}
	return mined
}

func signedTx(t *testing.T, from *wallet.Wallet, to string, amount, fee, nonce uint64) Transaction {
	t.Helper()
	tx := Transaction{From: from.Address(), To: to, Amount: amount, Fee: fee, Nonce: nonce}
	if err := tx.Sign(from); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tx
}

func TestGenesisDeterministic(t *testing.T) {
	if GenesisBlock().Hash != GenesisBlock().Hash {
		t.Fatal("genesis not deterministic")
	}
	// Two independent chains must share a genesis, or they can never agree.
	if NewBlockchain().Tip().Hash != NewBlockchain().Tip().Hash {
		t.Fatal("independent chains have different genesis")
	}
}

func TestMineReward(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatalf("add block: %v", err)
	}
	if got, want := bc.Balance(alice.Address()), BlockReward(1); got != want {
		t.Fatalf("reward = %d, want %d", got, want)
	}
}

func TestTransferAndFee(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New() // miner of block 2

	// Alice mines block 1 and earns the reward.
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	reward1 := BlockReward(1)

	// Alice pays Bob 10 with a fee of 1; Carol mines it.
	amount, fee := 10*Coin, 1*Coin
	tx := signedTx(t, alice, bob.Address(), amount, fee, 0)
	if err := bc.AddBlock(mineOn(t, bc, carol.Address(), []Transaction{tx})); err != nil {
		t.Fatalf("add block 2: %v", err)
	}
	reward2 := BlockReward(2)

	if got, want := bc.Balance(alice.Address()), reward1-amount-fee; got != want {
		t.Errorf("alice = %d, want %d", got, want)
	}
	if got := bc.Balance(bob.Address()); got != amount {
		t.Errorf("bob = %d, want %d", got, amount)
	}
	if got, want := bc.Balance(carol.Address()), reward2+fee; got != want {
		t.Errorf("carol (miner) = %d, want %d", got, want)
	}
	if got := bc.Account(alice.Address()).Nonce; got != 1 {
		t.Errorf("alice nonce = %d, want 1", got)
	}
}

func TestReplayRejected(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	tx := signedTx(t, alice, bob.Address(), 5*Coin, 0, 0)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err != nil {
		t.Fatal(err)
	}
	// Replaying the same nonce-0 tx must fail (nonce is now 1).
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err == nil {
		t.Fatal("expected replay to be rejected")
	}
}

func TestOverspendRejected(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	tx := signedTx(t, alice, bob.Address(), BlockReward(1)+Coin, 0, 0) // more than she has
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err == nil {
		t.Fatal("expected overspend to be rejected")
	}
}

func TestTamperedSignatureRejected(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	tx := signedTx(t, alice, bob.Address(), 1*Coin, 0, 0)
	tx.Amount = 1000 * Coin // tamper after signing
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err == nil {
		t.Fatal("expected tampered tx to be rejected")
	}
}

func TestForgedFromRejected(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	mallory, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	// Mallory tries to spend from Alice's address using his own key.
	tx := Transaction{From: alice.Address(), To: mallory.Address(), Amount: Coin, Nonce: 0}
	tx.PubKey = mallory.PublicKeyHex()
	tx.Signature = mallory.Sign(tx.signingBytes())
	if err := bc.AddBlock(mineOn(t, bc, mallory.Address(), []Transaction{tx})); err == nil {
		t.Fatal("expected forged sender to be rejected")
	}
}

func TestInvalidPoWRejected(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	tip := bc.Tip()
	cb := NewCoinbase(alice.Address(), BlockReward(1))
	block := Block{
		Index:        1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: []Transaction{cb},
		PrevHash:     tip.Hash,
		Difficulty:   bc.NextDifficulty(),
	}
	block.MerkleRoot = MerkleRoot(block.Transactions)
	block.Hash = block.ComputeHash() // computed but not mined -> almost certainly fails difficulty
	if err := bc.AddBlock(block); err == nil {
		t.Fatal("expected invalid PoW to be rejected")
	}
}

func TestExpiryEnforcedInBlock(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	// Tip is now height 1, so the next block is height 2.

	// A transaction that expires at height 1 cannot be mined into block 2.
	expired := Transaction{From: alice.Address(), To: bob.Address(), Amount: Coin, Nonce: 0, Expiry: 1}
	if err := expired.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{expired})); err == nil {
		t.Fatal("expected expired transaction to be rejected in block")
	}

	// The same transfer expiring at height 2 is valid at height 2.
	ok := Transaction{From: alice.Address(), To: bob.Address(), Amount: Coin, Nonce: 0, Expiry: 2}
	if err := ok.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{ok})); err != nil {
		t.Fatalf("non-expired transaction should be accepted: %v", err)
	}
	if bc.Balance(bob.Address()) != Coin {
		t.Fatalf("bob balance = %d, want %d", bc.Balance(bob.Address()), Coin)
	}
}

func TestReplaceChainLongestWins(t *testing.T) {
	src := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 3; i++ {
		if err := src.AddBlock(mineOn(t, src, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}

	dst := NewBlockchain()
	adopted, err := dst.ReplaceChain(src.Blocks())
	if err != nil || !adopted {
		t.Fatalf("adopt longer chain: adopted=%v err=%v", adopted, err)
	}
	if dst.Height() != 3 {
		t.Fatalf("height = %d, want 3", dst.Height())
	}

	// A chain no longer than ours must not be adopted.
	shorter := NewBlockchain()
	if err := shorter.AddBlock(mineOn(t, shorter, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	adopted, err = dst.ReplaceChain(shorter.Blocks())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if adopted {
		t.Fatal("adopted a shorter chain")
	}
}

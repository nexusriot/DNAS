package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// testFee is a per-transaction fee comfortably above the EIP-1559 base fee at
// any height reached in the tests (the base fee starts at InitialBaseFee and only
// decays on the near-empty test chains), so transactions are always mineable.
const testFee = uint64(1_000_000)

// mineOn builds and mines a valid block (coinbase to minerAddr + txs) on the tip.
func mineOn(t *testing.T, bc *Blockchain, minerAddr string, txs []Transaction) Block {
	t.Helper()
	tip := bc.Tip()
	baseFee := bc.NextBaseFee()
	cb := NewCoinbase(minerAddr, CoinbaseAmount(tip.Index+1, txs, baseFee))
	block := Block{
		Index:        tip.Index + 1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: append([]Transaction{cb}, txs...),
		PrevHash:     tip.Hash,
		BaseFee:      baseFee,
		Difficulty:   bc.NextDifficulty(),
	}
	block.StateRoot, _ = bc.NextStateRoot(block) // commit post-block state (err ignored: invalid blocks are meant to be rejected)
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

// matureCoinbase mines CoinbaseMaturity empty blocks to a throwaway miner so a
// just-mined coinbase becomes spendable, without touching any asserted balances.
func matureCoinbase(t *testing.T, bc *Blockchain) {
	t.Helper()
	sink, _ := wallet.New()
	for i := 0; i < CoinbaseMaturity; i++ {
		if err := bc.AddBlock(mineOn(t, bc, sink.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
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
	matureCoinbase(t, bc) // let alice's coinbase mature before she spends it

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
	_ = reward2
	// The miner keeps only the tip: fee minus the burned base fee of that block.
	baseFee := bc.Tip().BaseFee
	if got, want := bc.Balance(carol.Address()), BlockReward(bc.Height())+fee-baseFee; got != want {
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
	matureCoinbase(t, bc)
	tx := signedTx(t, alice, bob.Address(), 5*Coin, testFee, 0)
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
	tx := signedTx(t, alice, bob.Address(), BlockReward(1)+Coin, testFee, 0) // more than she has
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
	tx := signedTx(t, alice, bob.Address(), 1*Coin, testFee, 0)
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
	tx := Transaction{From: alice.Address(), To: mallory.Address(), Amount: Coin, Fee: testFee, Nonce: 0}
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
	matureCoinbase(t, bc)
	next := bc.Height() + 1 // the height the spend will land at

	// A transaction whose expiry is already in the past can't be mined.
	expired := Transaction{From: alice.Address(), To: bob.Address(), Amount: Coin, Nonce: 0, Expiry: next - 1}
	if err := expired.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{expired})); err == nil {
		t.Fatal("expected expired transaction to be rejected in block")
	}

	// The same transfer expiring at exactly `next` is still valid at `next`.
	ok := Transaction{From: alice.Address(), To: bob.Address(), Amount: Coin, Fee: testFee, Nonce: 0, Expiry: next}
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

func TestBadCoinbaseAmountRejected(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	tip := bc.Tip()
	// Coinbase paying more than the reward (no fees to justify it).
	cb := NewCoinbase(alice.Address(), BlockReward(1)+Coin)
	block := Block{
		Index:        1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: []Transaction{cb},
		PrevHash:     tip.Hash,
		Difficulty:   bc.NextDifficulty(),
	}
	mined, _ := Mine(block, nil)
	if err := bc.AddBlock(mined); err == nil {
		t.Fatal("expected an over-paying coinbase to be rejected")
	}
}

func TestFirstTxMustBeCoinbase(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	tip := bc.Tip()
	tx := signedTx(t, alice, "dnasx", Coin, 0, 0) // a normal tx, not coinbase
	block := Block{
		Index:        1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: []Transaction{tx},
		PrevHash:     tip.Hash,
		Difficulty:   bc.NextDifficulty(),
	}
	mined, _ := Mine(block, nil)
	if err := bc.AddBlock(mined); err == nil {
		t.Fatal("expected a block whose first tx is not coinbase to be rejected")
	}
}

func TestNonIncreasingTimestampRejected(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	tip := bc.Tip()
	cb := NewCoinbase(alice.Address(), BlockReward(1))
	block := Block{
		Index:        1,
		Timestamp:    tip.Timestamp, // equal to parent -> not increasing
		Transactions: []Transaction{cb},
		PrevHash:     tip.Hash,
		Difficulty:   bc.NextDifficulty(),
	}
	mined, _ := Mine(block, nil)
	if err := bc.AddBlock(mined); err == nil {
		t.Fatal("expected a non-increasing timestamp to be rejected")
	}
}

func TestReplaceChainRejectsForeignGenesis(t *testing.T) {
	bc := NewBlockchain()
	// A single block claiming huge difficulty (so it out-weighs our genesis on
	// the work check) but with a genesis hash that isn't ours.
	foreign := GenesisBlock()
	foreign.Difficulty = 12
	foreign.Hash = "not-our-genesis"
	adopted, err := bc.ReplaceChain([]Block{foreign})
	if adopted || err == nil {
		t.Fatalf("foreign genesis should be rejected: adopted=%v err=%v", adopted, err)
	}
}

// buildIndependentChain mines a fresh chain of the given height to a random
// miner, so its blocks (and therefore its tip hash) differ from any other such
// chain while carrying identical cumulative work.
func buildIndependentChain(t *testing.T, height int) []Block {
	t.Helper()
	bc := NewBlockchain()
	w, _ := wallet.New()
	for i := 0; i < height; i++ {
		if err := bc.AddBlock(mineOn(t, bc, w.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	return bc.Blocks()
}

func TestForkChoiceTieBreakConverges(t *testing.T) {
	c1 := buildIndependentChain(t, 2)
	c2 := buildIndependentChain(t, 2)
	if ChainWork(c1).Cmp(ChainWork(c2)) != 0 {
		t.Fatal("equal-height chains should carry equal work")
	}
	tip1, tip2 := c1[len(c1)-1].Hash, c2[len(c2)-1].Hash
	if tip1 == tip2 {
		t.Fatal("independent chains unexpectedly share a tip hash")
	}
	// `low` has the smaller tip hash and is therefore the canonical winner.
	low, high := c1, c2
	if tip1 > tip2 {
		low, high = c2, c1
	}
	lowTip := low[len(low)-1].Hash

	// A node already on `high`, offered the equal-work `low` (smaller tip): adopts.
	onHigh := NewBlockchain()
	if ok, err := onHigh.ReplaceChain(high); !ok || err != nil {
		t.Fatalf("seed high: ok=%v err=%v", ok, err)
	}
	if ok, err := onHigh.ReplaceChain(low); !ok || err != nil {
		t.Fatalf("should adopt smaller-tip chain: ok=%v err=%v", ok, err)
	}
	if onHigh.Tip().Hash != lowTip {
		t.Fatal("node on high did not converge to the canonical tip")
	}

	// A node already on `low`, offered the equal-work `high` (larger tip): keeps low.
	onLow := NewBlockchain()
	if _, err := onLow.ReplaceChain(low); err != nil {
		t.Fatal(err)
	}
	if ok, err := onLow.ReplaceChain(high); ok || err != nil {
		t.Fatalf("should keep smaller-tip chain: ok=%v err=%v", ok, err)
	}
	if onLow.Tip().Hash != lowTip {
		t.Fatal("node on low should have kept the canonical tip")
	}
	// Both nodes ended on the same tip => the network converges deterministically.
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

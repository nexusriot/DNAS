package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A transaction the mempool accepts but a block cannot contain does not merely
// waste a queue slot: the miner selects it into every candidate, each candidate
// is then invalid, and the node stops producing blocks until it is evicted. These
// tests pin the agreement — for each poison class, the mempool must refuse it,
// Select must never offer it, and consensus must reject it if it appears in a
// block anyway.
func TestMempoolAndConsensusAgreeOnInvalidTxs(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()

	cases := []struct {
		name string
		tx   Transaction
	}{
		{"memo over the limit", Transaction{
			To: bob.Address(), Amount: 1000, Fee: testFee,
			Memo: strings.Repeat("x", MaxMemoBytes+1),
		}},
		{"amount+fee overflows", Transaction{
			To: bob.Address(), Amount: ^uint64(0) - testFee + 1, Fee: testFee,
		}},
		{"no recipient", Transaction{Amount: 1000, Fee: testFee}},
		{"recipient address absurdly long", Transaction{
			To: strings.Repeat("z", MaxAddressBytes+1), Amount: 1000, Fee: testFee,
		}},
		{"empty transfer", Transaction{To: bob.Address()}},
		{"asset transfer of nothing", Transaction{
			To: bob.Address(), Fee: testFee, AssetID: AssetID(alice.Address(), "GOLD", 0),
		}},
		{"issue with a bad ticker", Transaction{
			Fee: testFee, Issue: &AssetIssue{Ticker: "not a ticker!", Supply: 10},
		}},
		{"issue of zero supply", Transaction{
			Fee: testFee, Issue: &AssetIssue{Ticker: "GOLD", Supply: 0},
		}},
		{"single-key tx carrying multisig signatures", Transaction{
			To: bob.Address(), Amount: 1000, Fee: testFee, Signatures: []string{"deadbeef"},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := NewBlockchain()
			mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
			matureCoinbase(t, bc)

			tx := tc.tx
			tx.From = alice.Address()
			if err := tx.Sign(alice); err != nil {
				t.Fatal(err)
			}

			if err := CheckTxSanity(tx); err == nil {
				t.Fatal("CheckTxSanity accepted a transaction no block can contain")
			}
			mp := NewMempoolWithPolicy(100, DefaultMinRelayFee)
			if added, err := mp.Add(tx); added || err == nil {
				t.Fatalf("mempool admitted it: added=%v err=%v", added, err)
			}
			// Even if it were queued somehow, the miner must not select it.
			if sel := mp.Select(bc, MaxBlockTxs); len(sel) != 0 {
				t.Fatalf("Select offered %d unmineable transaction(s)", len(sel))
			}
			// And a peer that puts it in a block must have that block rejected.
			if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err == nil {
				t.Fatal("consensus accepted a block carrying it")
			}
		})
	}
}

// The dust limit is height-activated, so it cannot be checked at admission time —
// a queued transaction becomes unmineable when the upgrade activates. Select must
// mirror the rule, or the miner would build nothing but invalid blocks from then
// on.
func TestSelectHonoursHeightActivatedDustLimit(t *testing.T) {
	ClearUpgrades()
	defer ClearUpgrades()

	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)

	dust := signedTx(t, alice, bob.Address(), DustThreshold-1, testFee, 0)
	mp := NewMempoolWithPolicy(100, DefaultMinRelayFee)
	if added, err := mp.Add(dust); !added || err != nil {
		t.Fatalf("dust tx should be relayable before activation: added=%v err=%v", added, err)
	}
	if got := len(mp.Select(bc, MaxBlockTxs)); got != 1 {
		t.Fatalf("selected %d txs before activation, want 1", got)
	}

	SetUpgradeHeight(UpgradeDustLimit, bc.Height()+1)
	if got := len(mp.Select(bc, MaxBlockTxs)); got != 0 {
		t.Fatalf("selected %d txs after the dust limit activated, want 0", got)
	}
	// The block the miner would now build must still be valid.
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), mp.Select(bc, MaxBlockTxs)))
}

// buildBlockFor recovers from an unmineable selected transaction by dropping it,
// which it can only do if the rejection says *which* transaction failed.
func TestApplyBlockReportsTheOffendingTxIndex(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)

	good := signedTx(t, alice, bob.Address(), 1000, testFee, 0)
	bad := signedTx(t, alice, bob.Address(), 1000, testFee, 7) // wrong nonce
	err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{good, bad}))
	if err == nil {
		t.Fatal("block with a bad nonce was accepted")
	}
	var rej *TxRejection
	if !errors.As(err, &rej) {
		t.Fatalf("error %v is not a *TxRejection", err)
	}
	if rej.Index != 2 { // 0 is the coinbase, 1 is `good`
		t.Fatalf("rejection index = %d, want 2", rej.Index)
	}
}

// A coinbase pays no fee and is exempt from MaxBlockBytes, so without its own
// shape rules a miner could commit an arbitrarily large block, or hide junk in
// fields that mean nothing there.
func TestCoinbaseShapeIsPinned(t *testing.T) {
	miner, _ := wallet.New()
	cases := []struct {
		name   string
		mutate func(*Transaction)
	}{
		{"oversized memo", func(cb *Transaction) { cb.Memo = strings.Repeat("x", MaxCoinbaseBytes) }},
		{"nonzero fee", func(cb *Transaction) { cb.Fee = 1 }},
		{"carries a nonce", func(cb *Transaction) { cb.Nonce = 1 }},
		{"carries an asset", func(cb *Transaction) { cb.Issue = &AssetIssue{Ticker: "GOLD", Supply: 1} }},
		{"carries a signature", func(cb *Transaction) { cb.Signature = "deadbeef" }},
		{"carries a script", func(cb *Transaction) { cb.HTLC = &HTLCScript{Hash: "00"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := NewBlockchain()
			tip := bc.Tip()
			cb := NewCoinbase(miner.Address(), BlockReward(1))
			tc.mutate(&cb)
			block := Block{
				Index:        1,
				Timestamp:    tip.Timestamp + 1,
				Transactions: []Transaction{cb},
				PrevHash:     tip.Hash,
				BaseFee:      bc.NextBaseFee(),
				Bits:         bc.NextBits(),
			}
			block.StateRoot, _ = bc.NextStateRoot(block)
			mined, ok := Mine(block, nil)
			if !ok {
				t.Fatal("mining aborted")
			}
			if err := bc.AddBlock(mined); err == nil {
				t.Fatal("block with a malformed coinbase was accepted")
			}
		})
	}
}

// An honest coinbase (what NewCoinbase produces) still connects.
func TestPlainCoinbaseStillValid(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	if bc.Balance(miner.Address()) != BlockReward(1) {
		t.Fatalf("miner balance = %d, want %d", bc.Balance(miner.Address()), BlockReward(1))
	}
}

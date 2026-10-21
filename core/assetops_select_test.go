package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// Selection must simulate what a mint or a burn does to the ISSUER's holding,
// for the same reason it already simulates coin balances, asset transfers and
// name claims: two operations that each look fine against the confirmed chain
// are not independent once they share a block. applyAssetOp burns only what the
// issuer still holds and mints only up to MaxAssetSupply, so the second of such
// a pair fails on apply — and a transaction consensus refuses does not cost one
// slot, it invalidates the whole candidate. The miner then drops a perfectly
// valid transaction to get moving again, so the user's operation is destroyed
// rather than mined in the next block.

// opChain funds an issuer, issues GOLD, and returns the chain, a mempool bound
// to it, the issuer and the asset id. The issuer's next free nonce is 2.
func opChain(t *testing.T) (*Blockchain, *Mempool, *wallet.Wallet, string) {
	t.Helper()
	withUpgradeAt(t, UpgradeAssetOps, 0)
	bc, issuer, id := issuedChain(t)
	matureCoinbase(t, bc)
	return bc, NewMempoolWithPolicy(64, 0).UseAccounts(bc), issuer, id
}

func TestSelectNeverPutsTwoUnbackedBurnsInABlock(t *testing.T) {
	bc, mp, issuer, id := opChain(t)
	held := bc.Account(issuer.Address()).Assets[id]
	if held == 0 {
		t.Fatal("issuer holds no asset to burn")
	}

	// Two burns of the whole holding. Each is affordable on its own; together
	// they burn twice what exists.
	if ok, err := mp.Add(opTx(t, issuer, id, AssetOpBurn, held, 1)); !ok {
		t.Fatalf("first burn: %v", err)
	}
	if ok, err := mp.Add(opTx(t, issuer, id, AssetOpBurn, held, 2)); !ok {
		t.Fatalf("second burn: %v", err)
	}

	sel := mp.Select(bc, 10)
	burns := 0
	for _, tx := range sel {
		if tx.IsAssetOp() && tx.AssetOp.Op == AssetOpBurn {
			burns++
		}
	}
	if burns != 1 {
		t.Fatalf("%d burns of one holding selected into a block, want 1", burns)
	}
	// And the block the miner would build from it must actually be valid.
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), sel))
}

func TestSelectNeverPutsTwoMintsPastTheCapInABlock(t *testing.T) {
	bc, mp, issuer, id := opChain(t)
	held := bc.Account(issuer.Address()).Assets[id]

	// Each mint alone fits under the cap; the pair does not.
	room := MaxAssetSupply - held
	if ok, err := mp.Add(opTx(t, issuer, id, AssetOpMint, room, 1)); !ok {
		t.Fatalf("first mint: %v", err)
	}
	if ok, err := mp.Add(opTx(t, issuer, id, AssetOpMint, room, 2)); !ok {
		t.Fatalf("second mint: %v", err)
	}

	sel := mp.Select(bc, 10)
	mints := 0
	for _, tx := range sel {
		if tx.IsAssetOp() && tx.AssetOp.Op == AssetOpMint {
			mints++
		}
	}
	if mints != 1 {
		t.Fatalf("%d mints past the supply cap selected into a block, want 1", mints)
	}
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), sel))
}

// The simulation must not be so strict that it refuses legitimate sequences: a
// mint followed by a burn of what it created belongs in one block.
func TestSelectKeepsAMintAndTheBurnItFunds(t *testing.T) {
	bc, mp, issuer, id := opChain(t)
	held := bc.Account(issuer.Address()).Assets[id]

	if ok, err := mp.Add(opTx(t, issuer, id, AssetOpMint, 500, 1)); !ok {
		t.Fatalf("mint: %v", err)
	}
	if ok, err := mp.Add(opTx(t, issuer, id, AssetOpBurn, held+500, 2)); !ok {
		t.Fatalf("burn: %v", err)
	}

	sel := mp.Select(bc, 10)
	if len(sel) != 2 {
		t.Fatalf("selected %d transactions, want both the mint and the burn it funds", len(sel))
	}
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), sel))
	if got := bc.Account(issuer.Address()).Assets[id]; got != 0 {
		t.Fatalf("issuer holds %d after minting 500 and burning the lot, want 0", got)
	}
}

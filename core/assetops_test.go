package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// issuedChain funds a wallet, issues 1000 GOLD from it, and returns the chain,
// the issuer and the asset id.
func issuedChain(t *testing.T) (*Blockchain, *wallet.Wallet, string) {
	t.Helper()
	bc := NewBlockchain()
	issuer, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), nil))
	matureCoinbase(t, bc)

	issue := Transaction{From: issuer.Address(), Fee: testFee, Nonce: 0,
		Issue: &AssetIssue{Ticker: "GOLD", Supply: 1000}}
	if err := issue.Sign(issuer); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{issue}))
	return bc, issuer, AssetID(issuer.Address(), "GOLD", 0)
}

// opTx builds a signed mint/burn against an asset issued at nonce 0.
func opTx(t *testing.T, w *wallet.Wallet, id, op string, amount, nonce uint64) Transaction {
	t.Helper()
	tx := Transaction{
		From: w.Address(), AssetID: id, Fee: testFee, Nonce: nonce,
		AssetOp: &AssetOp{Op: op, Amount: amount, Ticker: "GOLD", IssueNonce: 0},
	}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestAssetMintAndBurnMoveTheSupply(t *testing.T) {
	t.Cleanup(ClearUpgrades)
	ClearUpgrades()
	SetUpgradeHeight(UpgradeAssetOps, 0)

	bc, issuer, id := issuedChain(t)
	if got := bc.Account(issuer.Address()).Assets[id]; got != 1000 {
		t.Fatalf("issuer holds %d after issuance, want 1000", got)
	}

	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{opTx(t, issuer, id, AssetOpMint, 500, 1)}))
	if got := bc.Account(issuer.Address()).Assets[id]; got != 1500 {
		t.Fatalf("after minting 500 the issuer holds %d, want 1500", got)
	}

	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{opTx(t, issuer, id, AssetOpBurn, 700, 2)}))
	if got := bc.Account(issuer.Address()).Assets[id]; got != 800 {
		t.Fatalf("after burning 700 the issuer holds %d, want 800", got)
	}

	// The registry reports the current total, and what was originally issued.
	info, ok := bc.Asset(id)
	if !ok {
		t.Fatal("the asset vanished from the registry")
	}
	if info.Supply != 800 {
		t.Errorf("registry supply = %d, want 800", info.Supply)
	}
	if info.Issued != 1000 {
		t.Errorf("registry issued = %d, want 1000", info.Issued)
	}
}

// The authority check is the whole design: an id that matches can only have been
// produced by the account that issued it, so nothing is looked up.
func TestOnlyTheIssuerCanMint(t *testing.T) {
	t.Cleanup(ClearUpgrades)
	ClearUpgrades()
	SetUpgradeHeight(UpgradeAssetOps, 0)

	bc, issuer, id := issuedChain(t)
	thief, _ := wallet.New()
	// Fund the thief so the failure is about authority and not about the fee.
	pay := signedTx(t, issuer, thief.Address(), 10*Coin, testFee, 1)
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{pay}))
	matureCoinbase(t, bc)

	steal := Transaction{
		From: thief.Address(), AssetID: id, Fee: testFee, Nonce: 0,
		AssetOp: &AssetOp{Op: AssetOpMint, Amount: 1_000_000, Ticker: "GOLD", IssueNonce: 0},
	}
	if err := steal.Sign(thief); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(mineOn(t, bc, issuer.Address(), []Transaction{steal})); err == nil {
		t.Fatal("someone who did not issue the asset minted a million units of it")
	}
	if got := bc.Account(thief.Address()).Assets[id]; got != 0 {
		t.Errorf("the thief ended up holding %d", got)
	}
}

// Naming the wrong preimage fails for the same reason: the id no longer derives.
func TestMintWithTheWrongPreimageIsRefused(t *testing.T) {
	t.Cleanup(ClearUpgrades)
	ClearUpgrades()
	SetUpgradeHeight(UpgradeAssetOps, 0)

	bc, issuer, id := issuedChain(t)
	for name, op := range map[string]*AssetOp{
		"wrong ticker": {Op: AssetOpMint, Amount: 1, Ticker: "SILV", IssueNonce: 0},
		"wrong nonce":  {Op: AssetOpMint, Amount: 1, Ticker: "GOLD", IssueNonce: 7},
	} {
		tx := Transaction{From: issuer.Address(), AssetID: id, Fee: testFee, Nonce: 1, AssetOp: op}
		if err := tx.Sign(issuer); err != nil {
			t.Fatal(err)
		}
		if err := bc.AddBlock(mineOn(t, bc, issuer.Address(), []Transaction{tx})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An issuer may only destroy what it holds. Burning someone else's units would
// be confiscation, which is a different feature.
func TestBurnCannotExceedWhatTheIssuerHolds(t *testing.T) {
	t.Cleanup(ClearUpgrades)
	ClearUpgrades()
	SetUpgradeHeight(UpgradeAssetOps, 0)

	bc, issuer, id := issuedChain(t)
	holder, _ := wallet.New()
	send := Transaction{From: issuer.Address(), To: holder.Address(), Amount: 900,
		AssetID: id, Fee: testFee, Nonce: 1}
	if err := send.Sign(issuer); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{send}))
	if got := bc.Account(issuer.Address()).Assets[id]; got != 100 {
		t.Fatalf("issuer holds %d after sending 900, want 100", got)
	}

	// 500 > the 100 it kept, even though 1000 exist.
	over := opTx(t, issuer, id, AssetOpBurn, 500, 2)
	if err := bc.AddBlock(mineOn(t, bc, issuer.Address(), []Transaction{over})); err == nil {
		t.Fatal("the issuer burned units it did not hold")
	}
	if got := bc.Account(holder.Address()).Assets[id]; got != 900 {
		t.Errorf("the holder's balance moved to %d", got)
	}
}

// Before the upgrade activates, a supply is fixed at issuance and holders can
// rely on that.
func TestAssetOpsAreInactiveUntilTheirUpgrade(t *testing.T) {
	t.Cleanup(ClearUpgrades)
	ClearUpgrades()

	bc, issuer, id := issuedChain(t)
	mint := opTx(t, issuer, id, AssetOpMint, 1, 1)
	err := bc.AddBlock(mineOn(t, bc, issuer.Address(), []Transaction{mint}))
	if err == nil {
		t.Fatal("a mint was accepted with the upgrade unscheduled")
	}
	if !strings.Contains(err.Error(), "not active at this height") {
		t.Fatalf("unhelpful error: %v", err)
	}

	// Scheduled above this height, it is still refused...
	SetUpgradeHeight(UpgradeAssetOps, bc.Height()+5)
	if err := bc.AddBlock(mineOn(t, bc, issuer.Address(), []Transaction{mint})); err == nil {
		t.Fatal("a mint was accepted below the activation height")
	}
	// ...and accepted at it.
	SetUpgradeHeight(UpgradeAssetOps, bc.Height()+1)
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{mint}))
}

// Conservation must account for the new ways an asset total can move, or every
// mint would be rejected as unexplained.
func TestConservationAccountsForMintsAndBurns(t *testing.T) {
	t.Cleanup(ClearUpgrades)
	ClearUpgrades()
	SetUpgradeHeight(UpgradeAssetOps, 0)

	bc, issuer, id := issuedChain(t)
	// A block carrying both a mint and a burn of the same asset, netting +200.
	txs := []Transaction{
		opTx(t, issuer, id, AssetOpMint, 500, 1),
		opTx(t, issuer, id, AssetOpBurn, 300, 2),
	}
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), txs))
	if got := bc.Account(issuer.Address()).Assets[id]; got != 1200 {
		t.Fatalf("issuer holds %d, want 1200", got)
	}
	if s := bc.Supply(); !s.Consistent {
		t.Fatal("coin supply became inconsistent through an asset operation")
	}

	// And a mint that credits a different amount from the one it states is caught
	// by the rule directly.
	state := map[string]Account{"issuer": {Assets: map[string]uint64{id: 999}}}
	undo := []undoEntry{{addr: "issuer"}}
	stated := []Transaction{NewCoinbase("m", 0), {From: "issuer", AssetID: id,
		AssetOp: &AssetOp{Op: AssetOpMint, Amount: 10, Ticker: "GOLD", IssueNonce: 0}}}
	if err := checkConservation(state, undo, stated, 0, 0); err == nil {
		t.Fatal("a mint of 10 that credited 999 was accepted")
	}
}

// A reorg that discards a mint must take the supply back with it.
func TestRegistrySupplyFollowsAReorg(t *testing.T) {
	t.Cleanup(ClearUpgrades)
	ClearUpgrades()
	SetUpgradeHeight(UpgradeAssetOps, 0)

	bc, issuer, id := issuedChain(t)
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{opTx(t, issuer, id, AssetOpMint, 500, 1)}))
	if info, _ := bc.Asset(id); info.Supply != 1500 {
		t.Fatalf("supply after the mint = %d, want 1500", info.Supply)
	}

	// A longer branch from before the mint, which never contained it.
	forkAt := bc.Height() - 1
	blocks := bc.Blocks()[:forkAt+1]
	rival := NewBlockchain()
	for _, b := range blocks[1:] {
		mustAdd(t, rival, b)
	}
	sink, _ := wallet.New()
	for i := 0; i < 3; i++ {
		mustAdd(t, rival, mineOn(t, rival, sink.Address(), nil))
	}
	replaced, _, err := bc.ReplaceChain(rival.Blocks())
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("the longer chain was not adopted, so the reorg path went untested")
	}
	if info, ok := bc.Asset(id); !ok || info.Supply != 1000 {
		t.Fatalf("after the reorg the registry says %d (present=%v), want 1000", info.Supply, ok)
	}
}

func TestAssetOpShapeIsChecked(t *testing.T) {
	base := func() Transaction {
		return Transaction{From: "dnasx", AssetID: "tok1", Fee: 1, Nonce: 0,
			AssetOp: &AssetOp{Op: AssetOpMint, Amount: 5, Ticker: "GOLD"}}
	}
	if err := CheckTxSanity(base()); err != nil {
		t.Fatalf("a well-formed operation was refused: %v", err)
	}
	cases := map[string]func(*Transaction){
		"no asset named":   func(tx *Transaction) { tx.AssetID = "" },
		"also an issuance": func(tx *Transaction) { tx.Issue = &AssetIssue{Ticker: "X", Supply: 1} },
		"also a transfer":  func(tx *Transaction) { tx.To, tx.Amount = "dnasy", 5 },
		"unknown op":       func(tx *Transaction) { tx.AssetOp.Op = "confiscate" },
		"zero amount":      func(tx *Transaction) { tx.AssetOp.Amount = 0 },
		"absurd amount":    func(tx *Transaction) { tx.AssetOp.Amount = MaxAssetSupply + 1 },
		"bad ticker":       func(tx *Transaction) { tx.AssetOp.Ticker = "not a ticker!" },
		"no ticker":        func(tx *Transaction) { tx.AssetOp.Ticker = "" },
	}
	for name, mutate := range cases {
		tx := base()
		mutate(&tx)
		if err := CheckTxSanity(tx); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The operation is covered by the signature, so it cannot be altered in flight.
func TestAssetOpIsSigned(t *testing.T) {
	w, _ := wallet.New()
	tx := Transaction{From: w.Address(), AssetID: "tok1", Fee: 1, Nonce: 0,
		AssetOp: &AssetOp{Op: AssetOpMint, Amount: 5, Ticker: "GOLD"}}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("a correctly signed operation failed verification: %v", err)
	}

	tampered := tx
	op := *tx.AssetOp
	op.Amount = 1_000_000
	tampered.AssetOp = &op
	if err := tampered.VerifySignature(); err == nil {
		t.Fatal("the minted amount was changed without invalidating the signature")
	}
	if tampered.Hash() == tx.Hash() {
		t.Fatal("changing the operation did not change the txid")
	}
}

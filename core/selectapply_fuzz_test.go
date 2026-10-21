package core

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// The one question block production turns on: does the miner's simulation agree
// with consensus?
//
// Mempool.Select re-implements, against a simulated ledger, every rule
// applyTxsAndCoinbase will apply for real — balances, nonces, asset holdings,
// name claims, the byte and verify budgets. The two are mirrored BY HAND, and
// the cost of them drifting is out of proportion to the cause: a transaction
// Select picks and consensus refuses does not waste one slot, it invalidates
// the whole candidate. The node then drops a valid transaction to get moving
// again (Node.buildBlockFor), so the symptom is a user's payment quietly
// disappearing rather than an error anyone sees.
//
// The targets below are not differential in the difffuzz.go sense — there is no
// second opinion to compare against, because consensus IS the answer. They
// assert an INVARIANT instead: whatever the pool holds, what Select returns
// must apply. That is the property the hand-mirroring exists to provide, and it
// is the one that was false for asset mint/burn, which Select did not simulate
// at all.

// fuzzLedger is a chain with several funded signers and one issued asset, with
// every upgrade active so each transaction kind the generator can produce is
// reachable. It returns the chain, a mempool bound to it, the signers and the
// asset id.
func fuzzLedger(t *testing.T, signerCount int) (*Blockchain, *Mempool, []*wallet.Wallet, string) {
	t.Helper()
	for _, u := range Upgrades() {
		withUpgradeAt(t, u, 0)
	}
	bc := NewBlockchain()
	signers := fuzzSigners(t, signerCount)
	for _, w := range signers {
		mustAdd(t, bc, mineOn(t, bc, w.Address(), nil))
	}
	matureCoinbase(t, bc)

	// One asset, so transfers, mints and burns have something real to move.
	issuer := signers[0]
	issue := Transaction{From: issuer.Address(), Fee: testFee, Nonce: 0,
		Issue: &AssetIssue{Ticker: "FUZZ", Supply: 1_000_000}}
	if err := issue.Sign(issuer); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, bc, mineOn(t, bc, issuer.Address(), []Transaction{issue}))
	matureCoinbase(t, bc)

	return bc, NewMempoolWithPolicy(256, 0).UseAccounts(bc), signers, AssetID(issuer.Address(), "FUZZ", 0)
}

// fuzzPoolTx builds a transaction aimed at the live ledger: a real sender with a
// plausible nonce, so it has a chance of being admitted and selected rather than
// bouncing off the first check. It deliberately reaches every kind Select
// branches on — that is the point, since a kind Select does not know about is
// exactly how the two sides drift apart.
func fuzzPoolTx(t *testing.T, r *rand.Rand, bc *Blockchain, signers []*wallet.Wallet, assetID string) Transaction {
	t.Helper()
	from := signers[r.Intn(len(signers))]
	to := signers[r.Intn(len(signers))]
	nonce := bc.Account(from.Address()).Nonce
	if r.Intn(8) == 0 { // occasionally off-by-something, to exercise the gap rules
		nonce += uint64(r.Intn(3))
	}
	tx := Transaction{From: from.Address(), Fee: testFee + uint64(r.Int63n(1000)), Nonce: nonce}

	switch r.Intn(8) {
	case 0: // plain transfer
		tx.To, tx.Amount = to.Address(), uint64(1+r.Int63n(5*int64(Coin)))
	case 1: // multi-output transfer
		n := 1 + r.Intn(3)
		for i := 0; i < n; i++ {
			tx.Outputs = append(tx.Outputs, Output{
				To: signers[r.Intn(len(signers))].Address(), Amount: uint64(1 + r.Int63n(int64(Coin))),
			})
		}
	case 2: // asset transfer
		tx.AssetID, tx.To, tx.Amount = assetID, to.Address(), uint64(1+r.Int63n(2000))
	case 3: // asset mint
		tx.AssetID = assetID
		tx.AssetOp = &AssetOp{Op: AssetOpMint, Amount: uint64(1 + r.Int63n(500_000)), Ticker: "FUZZ", IssueNonce: 0}
	case 4: // asset burn
		tx.AssetID = assetID
		tx.AssetOp = &AssetOp{Op: AssetOpBurn, Amount: uint64(1 + r.Int63n(500_000)), Ticker: "FUZZ", IssueNonce: 0}
	case 5: // issuance
		tx.Issue = &AssetIssue{Ticker: "NEW", Supply: uint64(1 + r.Int63n(1000))}
	case 6: // name operation
		names := []string{"alpha", "beta", "gamma"}
		tx.Name = &NameOp{Op: NameOpRegister, Name: names[r.Intn(len(names))], Target: to.Address()}
	case 7: // sponsored transfer
		payer := signers[r.Intn(len(signers))]
		tx.To, tx.Amount = to.Address(), uint64(1+r.Int63n(int64(Coin)))
		if payer.Address() != from.Address() {
			tx.FeePayer = payer.Address()
		}
	}

	if tx.IsSponsored() {
		if err := tx.Sign(from); err != nil {
			t.Fatal(err)
		}
		payer := signers[0]
		for _, w := range signers {
			if w.Address() == tx.FeePayer {
				payer = w
			}
		}
		_ = tx.SponsorFee(payer)
		return tx
	}
	if err := tx.Sign(from); err != nil {
		t.Fatal(err)
	}
	return tx
}

// assertSelectionApplies is the invariant: the transactions Select returned must
// be exactly a block body consensus accepts, and must respect the budgets Select
// claims to enforce.
func assertSelectionApplies(t *testing.T, bc *Blockchain, minerAddr string, selected []Transaction) {
	t.Helper()
	tip := bc.Tip()
	baseFee := bc.NextBaseFee()
	height := tip.Index + 1
	cb := NewCoinbaseAt(minerAddr, CoinbaseAmount(height, selected, baseFee), height)
	candidate := Block{
		Index:        height,
		Timestamp:    tip.Timestamp + 1,
		Transactions: append([]Transaction{cb}, selected...),
		PrevHash:     tip.Hash,
		BaseFee:      baseFee,
		Bits:         bc.NextBits(),
	}
	candidate.MerkleRoot = MerkleRoot(candidate.Transactions)

	// NextStateRoot runs applyTxsAndCoinbase — the real consensus path, and the
	// exact call Node.buildBlockFor uses to discover that a selection is doomed.
	if _, err := bc.NextStateRoot(candidate); err != nil {
		var rej *TxRejection
		if errors.As(err, &rej) && rej.Index >= 1 && rej.Index <= len(selected) {
			bad := selected[rej.Index-1]
			t.Fatalf("Select returned a transaction consensus refuses: %v\n  kind=%s from=%s nonce=%d",
				rej.Err, txKind(bad), bad.From, bad.Nonce)
		}
		t.Fatalf("the selected block does not apply: %v", err)
	}

	if w := blockWeight(candidate); w > MaxBlockBytes {
		t.Fatalf("selection is %d transaction bytes, over the %d budget", w, MaxBlockBytes)
	}
	if ops := BlockVerifyOps(candidate.Transactions); ops > MaxBlockVerifyOps {
		t.Fatalf("selection costs %d verify ops, over the %d budget", ops, MaxBlockVerifyOps)
	}
}

// txKind names a transaction's form, so a failure says which branch of Select
// and applyTxTo disagreed rather than just which index.
func txKind(tx Transaction) string {
	switch {
	case tx.IsNameOp():
		return "name:" + tx.Name.Op
	case tx.IsIssue():
		return "issue"
	case tx.IsAssetOp():
		return "assetop:" + tx.AssetOp.Op
	case tx.IsAssetTransfer():
		return "assettransfer"
	case tx.IsMultiOutput():
		return "multioutput"
	case tx.IsSponsored():
		return "sponsored"
	default:
		return "transfer"
	}
}

// Whatever the pool holds, Select must return a body that applies. This is the
// property the hand-mirroring between Select and applyTxsAndCoinbase exists to
// provide; nothing but a test like this keeps it true as kinds are added.
func FuzzSelectProducesAnApplicableBlock(f *testing.F) {
	f.Add(int64(1), 6)
	f.Add(int64(42), 20)
	f.Add(int64(7), 1)
	f.Add(int64(2026), 40)
	f.Fuzz(func(t *testing.T, seed int64, count int) {
		if count < 0 || count > 60 {
			t.Skip()
		}
		r := rand.New(rand.NewSource(seed))
		bc, mp, signers, assetID := fuzzLedger(t, 4)

		for i := 0; i < count; i++ {
			// Admission may refuse any of these (a gap, an unaffordable queue, a
			// duplicate nonce); that is policy doing its job and not the subject
			// of this test.
			_, _ = mp.Add(fuzzPoolTx(t, r, bc, signers, assetID))
		}
		assertSelectionApplies(t, bc, signers[0].Address(), mp.Select(bc, MaxBlockTxs))
	})
}

// The same invariant across consecutive blocks: Select, mine what it returned,
// and go round again. A simulation that is right once but does not track what
// the chain just confirmed shows up here rather than in a single-block test.
func FuzzSelectStaysApplicableAcrossBlocks(f *testing.F) {
	f.Add(int64(3), 12, 3)
	f.Add(int64(99), 25, 5)
	f.Fuzz(func(t *testing.T, seed int64, count, rounds int) {
		if count < 0 || count > 40 || rounds < 1 || rounds > 6 {
			t.Skip()
		}
		r := rand.New(rand.NewSource(seed))
		bc, mp, signers, assetID := fuzzLedger(t, 4)

		for round := 0; round < rounds; round++ {
			for i := 0; i < count; i++ {
				_, _ = mp.Add(fuzzPoolTx(t, r, bc, signers, assetID))
			}
			selected := mp.Select(bc, MaxBlockTxs)
			assertSelectionApplies(t, bc, signers[0].Address(), selected)
			// Mine it for real, so the next round selects against a ledger the
			// previous selection actually moved.
			mustAdd(t, bc, mineOn(t, bc, signers[0].Address(), selected))
			mp.Remove(selected)
			mp.Reconcile(bc.Height())
		}
	})
}

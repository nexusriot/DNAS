package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// withUpgrade schedules a consensus upgrade from height 0 for one test and
// clears every scheduled upgrade afterwards.
func withUpgrade(t *testing.T, name string) {
	t.Helper()
	SetUpgradeHeight(name, 0)
	t.Cleanup(ClearUpgrades)
}

// sponsoredTx builds a transfer whose fee is charged to sponsor: the sender
// signs (binding the fee payer), then the sponsor counter-signs.
func sponsoredTx(t *testing.T, from, sponsor *wallet.Wallet, to string, amount, fee, nonce uint64) Transaction {
	t.Helper()
	tx := Transaction{
		From:     from.Address(),
		To:       to,
		Amount:   amount,
		Fee:      fee,
		Nonce:    nonce,
		FeePayer: sponsor.Address(),
	}
	if err := tx.Sign(from); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := tx.SponsorFee(sponsor); err != nil {
		t.Fatalf("sponsor: %v", err)
	}
	return tx
}

// The point of sponsorship: an address holding no coin at all can still pay
// someone, because a third party covers the fee.
func TestSponsoredSenderNeedsNoCoinForTheFee(t *testing.T) {
	withUpgrade(t, UpgradeFeeSponsor)
	bc := NewBlockchain()
	sponsor, _ := wallet.New()
	sender, _ := wallet.New()
	recipient, _ := wallet.New()

	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), nil))
	matureCoinbase(t, bc)
	// The sender is funded with exactly what it will pay out and not one unit more,
	// so it could not cover any fee itself.
	fund := signedTx(t, sponsor, sender.Address(), 500, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), []Transaction{fund}))

	sponsorBefore := bc.Balance(sponsor.Address())
	tx := sponsoredTx(t, sender, sponsor, recipient.Address(), 500, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), []Transaction{tx}))

	if got := bc.Balance(sender.Address()); got != 0 {
		t.Fatalf("sender balance = %d, want 0 (it paid out everything it had)", got)
	}
	if got := bc.Balance(recipient.Address()); got != 500 {
		t.Fatalf("recipient balance = %d, want 500", got)
	}
	// The sponsor paid the fee, and was paid the block's tip back as its miner —
	// so it is out exactly the burned base-fee portion.
	burned := BaseFeeFor(tx, bc.Tip().BaseFee)
	sponsorAfter := bc.Balance(sponsor.Address())
	want := sponsorBefore - burned + BlockReward(bc.Height())
	if sponsorAfter != want {
		t.Fatalf("sponsor balance = %d, want %d (paid the %d burned base fee)", sponsorAfter, want, burned)
	}
	if got := bc.Account(sponsor.Address()).Nonce; got != 1 {
		t.Fatalf("sponsor nonce = %d, want 1 (sponsoring must not consume a nonce)", got)
	}
}

func TestSponsorshipInactiveBeforeUpgrade(t *testing.T) {
	bc := NewBlockchain()
	sponsor, _ := wallet.New()
	sender, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), nil))
	matureCoinbase(t, bc)
	fund := signedTx(t, sponsor, sender.Address(), 10_000, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), []Transaction{fund}))

	tx := sponsoredTx(t, sender, sponsor, sponsor.Address(), 100, testFee, 0)
	err := bc.AddBlock(mineOn(t, bc, sponsor.Address(), []Transaction{tx}))
	if err == nil {
		t.Fatal("a sponsored transaction was accepted before the upgrade activated")
	}
	if !strings.Contains(err.Error(), "fee sponsorship is not active") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestSponsorSignatureIsBoundToTheTransaction(t *testing.T) {
	withUpgrade(t, UpgradeFeeSponsor)
	sponsor, _ := wallet.New()
	sender, _ := wallet.New()
	victim, _ := wallet.New()
	tx := sponsoredTx(t, sender, sponsor, victim.Address(), 100, 10, 0)

	// Lifting the sponsorship onto a different transfer must not verify: the
	// sponsor signed the amount, the recipient and the nonce too.
	other := tx
	other.Amount = 999_999
	if err := other.VerifySignature(); err == nil {
		t.Fatal("a sponsor signature carried over to a different amount")
	}
	forged := tx
	forged.FeePayerSig = tx.Signature // the sender's signature is not the sponsor's
	if err := forged.VerifySignature(); err == nil {
		t.Fatal("the sender's own signature was accepted as the sponsor's")
	}
}

func TestSponsorShapeRules(t *testing.T) {
	w, _ := wallet.New()
	sponsor, _ := wallet.New()
	cases := map[string]Transaction{
		"key without a payer":       {From: w.Address(), To: "dnasx", Amount: 1, FeePayerPubKey: sponsor.PublicKeyHex()},
		"signature without a payer": {From: w.Address(), To: "dnasx", Amount: 1, FeePayerSig: "aa"},
		"payer without a key":       {From: w.Address(), To: "dnasx", Amount: 1, FeePayer: sponsor.Address()},
		"payer is the sender": {From: w.Address(), To: "dnasx", Amount: 1, FeePayer: w.Address(),
			FeePayerPubKey: w.PublicKeyHex(), FeePayerSig: "aa"},
	}
	for name, tx := range cases {
		if err := CheckTxSanity(tx); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestSponsorMustAffordTheFee(t *testing.T) {
	withUpgrade(t, UpgradeFeeSponsor)
	bc := NewBlockchain()
	miner, _ := wallet.New()
	sender, _ := wallet.New()
	broke, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	fund := signedTx(t, miner, sender.Address(), 10_000, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{fund}))

	tx := sponsoredTx(t, sender, broke, miner.Address(), 100, testFee, 0)
	err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{tx}))
	if err == nil {
		t.Fatal("a sponsor with no coin covered a fee")
	}
	if !strings.Contains(err.Error(), "cannot cover the fee") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

// The mempool must hold a sponsor to everything it has promised across the pool,
// not merely to one fee at a time.
func TestMempoolChecksSponsorAcrossItsWholeQueue(t *testing.T) {
	withUpgrade(t, UpgradeFeeSponsor)
	bc := NewBlockchain()
	sponsor, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), nil))
	matureCoinbase(t, bc)

	mp := NewMempool().UseAccounts(bc)
	spendable := bc.SpendableBalance(sponsor.Address())
	fee := spendable/2 + 1 // two of these do not fit; one does

	first, _ := wallet.New()
	second, _ := wallet.New()
	if added, err := mp.Add(sponsoredTx(t, first, sponsor, sponsor.Address(), 0, fee, 0)); !added || err != nil {
		t.Fatalf("first sponsorship refused: added=%v err=%v", added, err)
	}
	added, err := mp.Add(sponsoredTx(t, second, sponsor, sponsor.Address(), 0, fee, 0))
	if added || err == nil {
		t.Fatal("the sponsor was allowed to promise the same coin twice")
	}
	if !strings.Contains(err.Error(), "fee sponsor") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

// A sponsored transaction costs a block one more signature verification than an
// unsponsored one, and the block's verification budget must know it.
func TestSponsorCountsTowardVerifyOps(t *testing.T) {
	sponsor, _ := wallet.New()
	sender, _ := wallet.New()
	plain := signedTx(t, sender, sponsor.Address(), 1, 1, 0)
	sponsored := sponsoredTx(t, sender, sponsor, sponsor.Address(), 1, 1, 0)
	if got, want := VerifyOps(sponsored), VerifyOps(plain)+1; got != want {
		t.Fatalf("sponsored VerifyOps = %d, want %d", got, want)
	}
}

// Sponsorship must not disturb the money supply: the fee is still burned and
// tipped exactly as before, only a different account is debited.
func TestSponsoredTransferConservesSupply(t *testing.T) {
	withUpgrade(t, UpgradeFeeSponsor)
	bc := NewBlockchain()
	sponsor, _ := wallet.New()
	sender, _ := wallet.New()
	recipient, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), nil))
	matureCoinbase(t, bc)
	fund := signedTx(t, sponsor, sender.Address(), 5_000, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), []Transaction{fund}))

	tx := sponsoredTx(t, sender, sponsor, recipient.Address(), 500, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, sponsor.Address(), []Transaction{tx}))

	if s := bc.Supply(); !s.Consistent {
		t.Fatalf("supply broken after a sponsored transfer: minted %d, burned %d, circulating %d",
			s.Minted, s.Burned, s.Circulating)
	}
}

// A vault spend must not disturb it either.
func TestVaultSpendConservesSupply(t *testing.T) {
	withUpgrade(t, UpgradeVault)
	bc := NewBlockchain()
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	addr, err := wallet.VaultAddress(hot.PublicKeyHex(), cold.PublicKeyHex(), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	miner := fundVault(t, bc, addr, 10*testFee)
	spend := vaultSpend(t, cold, hot.PublicKeyHex(), cold.PublicKeyHex(), 1_000_000, miner.Address(), testFee, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{spend}))

	if s := bc.Supply(); !s.Consistent {
		t.Fatalf("supply broken after a vault spend: minted %d, burned %d, circulating %d",
			s.Minted, s.Burned, s.Circulating)
	}
}

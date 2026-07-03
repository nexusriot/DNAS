package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func msKeys(t *testing.T, n int) ([]*wallet.Wallet, []string) {
	t.Helper()
	ws := make([]*wallet.Wallet, n)
	pks := make([]string, n)
	for i := range ws {
		w, err := wallet.New()
		if err != nil {
			t.Fatal(err)
		}
		ws[i], pks[i] = w, w.PublicKeyHex()
	}
	return ws, pks
}

func TestMultisigVerification(t *testing.T) {
	ws, pks := msKeys(t, 3)
	addr, err := wallet.MultisigAddress(2, pks)
	if err != nil {
		t.Fatal(err)
	}
	base := Transaction{From: addr, To: "dnasdest", Amount: Coin, Nonce: 0,
		Multisig: &MultisigScript{Threshold: 2, PubKeys: pks}}

	sign := func(signers ...*wallet.Wallet) Transaction {
		tx := base
		tx.Signatures = nil
		for _, s := range signers {
			tx.AddSignature(s)
		}
		return tx
	}

	if err := sign(ws[0], ws[2]).VerifySignature(); err != nil {
		t.Fatalf("2-of-3 with distinct members should verify: %v", err)
	}
	if sign(ws[0]).VerifySignature() == nil {
		t.Error("1-of-3 (below threshold) must fail")
	}
	if sign(ws[0], ws[0]).VerifySignature() == nil {
		t.Error("the same member signing twice must not satisfy 2-of-3")
	}
	outsider, _ := wallet.New()
	if sign(ws[1], outsider).VerifySignature() == nil {
		t.Error("a non-member signature must not count")
	}

	// Script that doesn't hash to From (claims a lower threshold).
	forged := sign(ws[0])
	forged.Multisig = &MultisigScript{Threshold: 1, PubKeys: pks}
	if forged.VerifySignature() == nil {
		t.Error("a script not matching the sender address must be rejected")
	}

	// Tamper with the amount after signing.
	tampered := sign(ws[0], ws[1])
	tampered.Amount = 999 * Coin
	if tampered.VerifySignature() == nil {
		t.Error("a tampered multisig transaction must be rejected")
	}
}

func TestMultisigSpendOnChain(t *testing.T) {
	bc := NewBlockchain()
	ws, pks := msKeys(t, 3)
	msAddr, err := wallet.MultisigAddress(2, pks)
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := wallet.New()
	miner, _ := wallet.New()

	// Fund the multisig account via a coinbase, then spend 2-of-3 to bob.
	if err := bc.AddBlock(mineOn(t, bc, msAddr, nil)); err != nil {
		t.Fatal(err)
	}
	if bc.Balance(msAddr) != BlockReward(1) {
		t.Fatalf("multisig account not funded: %d", bc.Balance(msAddr))
	}
	matureCoinbase(t, bc)

	spend := Transaction{From: msAddr, To: bob.Address(), Amount: 10 * Coin, Fee: Coin, Nonce: 0,
		Multisig: &MultisigScript{Threshold: 2, PubKeys: pks}}
	spend.AddSignature(ws[0])
	spend.AddSignature(ws[1])
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{spend})); err != nil {
		t.Fatalf("2-of-3 multisig spend should be accepted: %v", err)
	}
	if bc.Balance(bob.Address()) != 10*Coin {
		t.Fatalf("bob = %d, want %d", bc.Balance(bob.Address()), 10*Coin)
	}
	if want := BlockReward(1) - 11*Coin; bc.Balance(msAddr) != want {
		t.Fatalf("multisig balance = %d, want %d", bc.Balance(msAddr), want)
	}
	if bc.Account(msAddr).Nonce != 1 {
		t.Fatal("multisig account nonce should have incremented")
	}
}

func TestMultisigInsufficientSignaturesRejectedOnChain(t *testing.T) {
	bc := NewBlockchain()
	ws, pks := msKeys(t, 3)
	msAddr, _ := wallet.MultisigAddress(2, pks)
	if err := bc.AddBlock(mineOn(t, bc, msAddr, nil)); err != nil {
		t.Fatal(err)
	}
	spend := Transaction{From: msAddr, To: "dnasx", Amount: Coin, Nonce: 0,
		Multisig: &MultisigScript{Threshold: 2, PubKeys: pks}}
	spend.AddSignature(ws[0]) // only 1 of 2
	if err := bc.AddBlock(mineOn(t, bc, msAddr, []Transaction{spend})); err == nil {
		t.Fatal("a block with an under-signed multisig spend must be rejected")
	}
}

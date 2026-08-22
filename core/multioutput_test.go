package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// signedOutputs builds and signs a multi-recipient transfer.
func signedOutputs(t *testing.T, from *wallet.Wallet, fee, nonce uint64, outs ...Output) Transaction {
	t.Helper()
	tx := Transaction{From: from.Address(), Outputs: outs, Fee: fee, Nonce: nonce}
	if err := tx.Sign(from); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tx
}

// activate enables multi-output transfers from the next block onwards.
func activate(t *testing.T, bc *Blockchain) {
	t.Helper()
	ClearUpgrades()
	t.Cleanup(ClearUpgrades)
	SetUpgradeHeight(UpgradeMultiOutput, bc.Height()+1)
}

// The point of the feature: one transaction, one fee, one nonce, many recipients.
func TestMultiOutputTransferPaysEveryRecipient(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)
	activate(t, bc)

	recipients := make([]*wallet.Wallet, 4)
	outs := make([]Output, len(recipients))
	var total uint64
	for i := range recipients {
		w, _ := wallet.New()
		recipients[i] = w
		outs[i] = Output{To: w.Address(), Amount: uint64(1000 * (i + 1))}
		total += outs[i].Amount
	}

	before := bc.Balance(alice.Address())
	tx := signedOutputs(t, alice, testFee, 0, outs...)
	sink, _ := wallet.New() // mine elsewhere, so the tip does not flow back to alice
	mustAdd(t, bc, mineOn(t, bc, sink.Address(), []Transaction{tx}))

	for i, w := range recipients {
		if got := bc.Balance(w.Address()); got != outs[i].Amount {
			t.Errorf("recipient %d balance = %d, want %d", i, got, outs[i].Amount)
		}
	}
	// The sender paid the outputs plus a single fee — and its nonce moved once.
	if got, want := bc.Balance(alice.Address()), before-total-testFee; got != want {
		t.Errorf("sender balance = %d, want %d (paid %d in outputs + one fee %d)", got, want, total, testFee)
	}
	if got := bc.Account(alice.Address()).Nonce; got != 1 {
		t.Errorf("sender nonce = %d, want 1 for one transaction", got)
	}
}

// The rule is height-activated, so nodes start accepting the new form together
// rather than disagreeing about whether a block is valid.
func TestMultiOutputRejectedBeforeActivation(t *testing.T) {
	ClearUpgrades()
	defer ClearUpgrades()

	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)

	tx := signedOutputs(t, alice, testFee, 0, Output{To: bob.Address(), Amount: 1000})
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err == nil {
		t.Fatal("a multi-output transfer was accepted before its activation height")
	}
	// Like the dust limit, this is a height rule rather than a property of the
	// transaction, so the pool may hold it — but the miner must not offer it into a
	// block that would then be rejected.
	mp := NewMempoolWithPolicy(20, 0).UseAccounts(bc)
	if added, err := mp.Add(tx); !added || err != nil {
		t.Fatalf("queueing ahead of activation: added=%v err=%v", added, err)
	}
	if got := len(mp.Select(bc, MaxBlockTxs)); got != 0 {
		t.Fatalf("Select offered %d multi-output transactions before activation", got)
	}

	SetUpgradeHeight(UpgradeMultiOutput, bc.Height()+1)
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), mp.Select(bc, MaxBlockTxs)))
	if got := bc.Balance(bob.Address()); got != 1000 {
		t.Fatalf("bob balance = %d, want 1000", got)
	}
}

// Adding the field must not change any existing transaction: a single-output
// transfer's canonical bytes — and therefore its txid, its signature and the size
// its fee is charged on — have to be exactly what they were.
func TestSingleOutputEncodingUnchanged(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	tx := signedTx(t, alice, bob.Address(), 1234, 5678, 9)

	// The outputs block is only emitted when present, so an empty list must encode
	// identically to a nil one, and neither may add a byte.
	withEmpty := tx
	withEmpty.Outputs = []Output{}
	if withEmpty.Hash() != tx.Hash() {
		t.Error("an empty outputs list changed the txid")
	}
	if withEmpty.Size() != tx.Size() {
		t.Errorf("an empty outputs list changed the size: %d vs %d", withEmpty.Size(), tx.Size())
	}
	// A populated list must change it (the outputs are signed, not decoration).
	withOutputs := tx
	withOutputs.To, withOutputs.Amount = "", 0
	withOutputs.Outputs = []Output{{To: bob.Address(), Amount: 1234}}
	if withOutputs.Hash() == tx.Hash() {
		t.Error("outputs are not committed by the txid")
	}
	if wallet.Verify(tx.PubKey, tx.Signature, withOutputs.signingBytes()) {
		t.Error("a signature over the single-output form also covers a multi-output one")
	}
}

// Malformed output lists are refused, and by the shared sanity check, so the
// mempool and consensus agree.
func TestMultiOutputSanityRules(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	base := func() Transaction {
		return Transaction{From: alice.Address(), Fee: testFee, Nonce: 0}
	}
	cases := []struct {
		name string
		tx   Transaction
	}{
		{"no recipient in an output", func() Transaction {
			tx := base()
			tx.Outputs = []Output{{Amount: 100}}
			return tx
		}()},
		{"output paying nothing", func() Transaction {
			tx := base()
			tx.Outputs = []Output{{To: bob.Address(), Amount: 0}}
			return tx
		}()},
		{"both forms at once", func() Transaction {
			tx := base()
			tx.To, tx.Amount = bob.Address(), 5
			tx.Outputs = []Output{{To: bob.Address(), Amount: 100}}
			return tx
		}()},
		{"outputs carrying an asset", func() Transaction {
			tx := base()
			tx.AssetID = AssetID(alice.Address(), "GOLD", 0)
			tx.Outputs = []Output{{To: bob.Address(), Amount: 100}}
			return tx
		}()},
		{"outputs overflowing", func() Transaction {
			tx := base()
			tx.Outputs = []Output{{To: bob.Address(), Amount: ^uint64(0)}, {To: bob.Address(), Amount: 2}}
			return tx
		}()},
		{"too many outputs", func() Transaction {
			tx := base()
			for i := 0; i <= MaxTxOutputs; i++ {
				tx.Outputs = append(tx.Outputs, Output{To: bob.Address(), Amount: 1})
			}
			return tx
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckTxSanity(tc.tx); err == nil {
				t.Fatal("CheckTxSanity accepted a malformed output list")
			}
		})
	}
}

// A repeated recipient accumulates, and a sender paying itself nets to just the
// fee — the credits are applied one at a time against live state, not batched.
func TestMultiOutputRepeatedAndSelfRecipients(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)
	activate(t, bc)

	before := bc.Balance(alice.Address())
	tx := signedOutputs(t, alice, testFee, 0,
		Output{To: bob.Address(), Amount: 100},
		Output{To: bob.Address(), Amount: 250},
		Output{To: alice.Address(), Amount: 700},
	)
	sink, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, sink.Address(), []Transaction{tx}))

	if got := bc.Balance(bob.Address()); got != 350 {
		t.Errorf("bob balance = %d, want 350 (100 + 250)", got)
	}
	// Alice sent 1050 and got 700 back, so she is out 350 plus the fee.
	if got := bc.Balance(alice.Address()); got != before-350-testFee {
		t.Errorf("alice balance = %d, want %d", got, before-350-testFee)
	}
}

// The miner's simulation must credit outputs exactly as application does, or it
// would build blocks its own rules reject. Selection is exercised on an unbound
// pool here so the admission rules (which require a sender to already hold what
// it spends — see mempool_admission_test.go) do not mask what is under test: that
// a spend of coin received through an output list, in the same block, simulates
// and applies identically.
func TestSelectCreditsOutputsForChainedSpends(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)
	activate(t, bc)

	mp := NewMempoolWithPolicy(20, 0)
	pay := signedOutputs(t, alice, testFee, 0, Output{To: bob.Address(), Amount: 5 * testFee})
	if added, err := mp.Add(pay); !added || err != nil {
		t.Fatalf("multi-output payment: added=%v err=%v", added, err)
	}
	// Bob spends what he is about to receive, in the same block.
	onward := signedTx(t, bob, carol.Address(), testFee, testFee, 0)
	if added, err := mp.Add(onward); !added || err != nil {
		t.Fatalf("chained spend: added=%v err=%v", added, err)
	}
	selected := mp.Select(bc, MaxBlockTxs)
	if len(selected) != 2 {
		t.Fatalf("selected %d transactions, want both", len(selected))
	}
	// The block the miner would build must be valid: the simulation and the
	// application of those output credits have to agree exactly.
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), selected))
	if got := bc.Balance(carol.Address()); got != testFee {
		t.Fatalf("carol balance = %d, want %d", got, testFee)
	}
}

// A light client must not be told an address is provably absent from a block that
// paid it through an output list.
func TestCompactFilterCoversMultiOutputRecipients(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)
	activate(t, bc)

	tx := signedOutputs(t, alice, testFee, 0, Output{To: bob.Address(), Amount: 1000})
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), []Transaction{tx}))

	f, ok := bc.BlockFilterAt(bc.Height())
	if !ok {
		t.Fatal("no filter for the tip")
	}
	if !f.Match(bob.Address()) {
		t.Fatal("the filter says a paid recipient is absent from the block")
	}
}

// The dust limit applies per recipient, so a batch cannot smuggle dust past it.
func TestDustLimitAppliesToEveryOutput(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)
	ClearUpgrades()
	defer ClearUpgrades()
	SetUpgradeHeight(UpgradeMultiOutput, 0)
	SetUpgradeHeight(UpgradeDustLimit, bc.Height()+1)

	tx := signedOutputs(t, alice, testFee, 0,
		Output{To: bob.Address(), Amount: DustThreshold * 10},
		Output{To: carol.Address(), Amount: DustThreshold - 1}, // dust
	)
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{tx})); err == nil {
		t.Fatal("a batch containing a dust output was accepted")
	}
}

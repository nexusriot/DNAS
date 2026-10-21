package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func nameChain(t *testing.T, owner *wallet.Wallet) (*Blockchain, *Mempool) {
	t.Helper()
	withUpgradeAt(t, UpgradeNameService, 0)
	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, owner.Address(), nil))
	}
	matureCoinbase(t, bc)
	return bc, NewMempoolWithPolicy(64, 0).UseAccounts(bc)
}

func nameTx(t *testing.T, w *wallet.Wallet, op *NameOp, nonce uint64) Transaction {
	t.Helper()
	tx := Transaction{From: w.Address(), Fee: testFee, Nonce: nonce, Name: op}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	return tx
}

func mineTxs(t *testing.T, bc *Blockchain, miner *wallet.Wallet, txs ...Transaction) {
	t.Helper()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), txs))
}

func TestNameRegisterResolveAndProve(t *testing.T) {
	alice, _ := wallet.New()
	bc, _ := nameChain(t, alice)

	mineTxs(t, bc, alice, nameTx(t, alice, &NameOp{Op: NameOpRegister, Name: "alice"}, 0))

	rec, held := bc.LookupName("alice")
	if !held {
		t.Fatal("the name was not registered")
	}
	if rec.Owner != alice.Address() {
		t.Fatalf("owner is %s, want %s", rec.Owner, alice.Address())
	}
	if rec.Expires <= bc.Height() {
		t.Fatalf("lease expires at %d, chain is at %d", rec.Expires, bc.Height())
	}

	// The whole point of putting it in consensus: a light client can PROVE the
	// resolution against the header's state root, rather than trusting whoever
	// answered the query.
	proof, found := bc.ProveAccount(NameStateKey("alice"))
	if !found {
		t.Fatal("the name record is not provable")
	}
	valid, present := VerifyAccountProof(proof, bc.Tip().StateRoot)
	if !valid || !present {
		t.Fatalf("name proof: valid=%v present=%v", valid, present)
	}
	if proof.Account.Owner != alice.Address() {
		t.Fatalf("the proof says the owner is %q", proof.Account.Owner)
	}

	// And an unclaimed name must be provably absent, which is what stops a node
	// hiding a registration to make a payer use an address of its choosing.
	absent, found := bc.ProveAccount(NameStateKey("nobody"))
	if found {
		t.Fatal("an unregistered name was found")
	}
	valid, present = VerifyAccountProof(absent, bc.Tip().StateRoot)
	if !valid || present {
		t.Fatalf("absence proof: valid=%v present=%v, want true/false", valid, present)
	}
}

func TestNameCannotBeTakenFromItsOwner(t *testing.T) {
	alice, _ := wallet.New()
	mallory, _ := wallet.New()
	bc, _ := nameChain(t, alice)
	mineTxs(t, bc, alice, nameTx(t, alice, &NameOp{Op: NameOpRegister, Name: "alice"}, 0))

	// Fund mallory so the failure is about ownership, not about the fee.
	mineTxs(t, bc, alice, signedTx(t, alice, mallory.Address(), 10*Coin, testFee, 1))
	matureCoinbase(t, bc)

	for _, op := range []*NameOp{
		{Op: NameOpRegister, Name: "alice"},
		{Op: NameOpTransfer, Name: "alice", Target: mallory.Address()},
		{Op: NameOpRelease, Name: "alice"},
		{Op: NameOpRenew, Name: "alice"},
	} {
		blk := mineOn(t, bc, alice.Address(), []Transaction{nameTx(t, mallory, op, 0)})
		if err := bc.AddBlock(blk); err == nil {
			t.Fatalf("%s of someone else's name was accepted", op.Op)
		}
	}
	if rec, held := bc.LookupName("alice"); !held || rec.Owner != alice.Address() {
		t.Fatal("the name changed hands")
	}
}

func TestNameTransferAndRelease(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	bc, _ := nameChain(t, alice)

	mineTxs(t, bc, alice, nameTx(t, alice, &NameOp{Op: NameOpRegister, Name: "shop"}, 0))
	mineTxs(t, bc, alice, nameTx(t, alice, &NameOp{Op: NameOpTransfer, Name: "shop", Target: bob.Address()}, 1))
	rec, held := bc.LookupName("shop")
	if !held || rec.Owner != bob.Address() {
		t.Fatalf("after transfer the owner is %q", rec.Owner)
	}

	// Bob needs coin to pay the release fee.
	mineTxs(t, bc, alice, signedTx(t, alice, bob.Address(), 10*Coin, testFee, 2))
	matureCoinbase(t, bc)
	mineTxs(t, bc, alice, nameTx(t, bob, &NameOp{Op: NameOpRelease, Name: "shop"}, 0))

	if _, held := bc.LookupName("shop"); held {
		t.Fatal("a released name is still held")
	}
	// A released name must leave NOTHING behind: the state root of a chain
	// where a name was claimed and released must not carry a dead record.
	if _, ok := bc.state.Lookup(NameStateKey("shop")); ok {
		t.Fatal("a released name left a record in state")
	}
}

// A lease, not a freehold. An expired name must be claimable by anyone,
// otherwise the namespace is squatted once and then dead.
func TestExpiredNameIsClaimable(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	bc, _ := nameChain(t, alice)

	// Write a record directly at an already-expired height: mining 100,000
	// blocks to let a lease run out is not a unit test.
	mineTxs(t, bc, alice, nameTx(t, alice, &NameOp{Op: NameOpRegister, Name: "lapsed"}, 0))
	bc.mu.Lock()
	bc.state.Put(NameStateKey("lapsed"), Account{Owner: alice.Address(), Expires: 1})
	bc.mu.Unlock()

	if _, held := bc.LookupName("lapsed"); held {
		t.Fatal("an expired name still reads as held")
	}
	// Fund bob and let him take it.
	mineTxs(t, bc, alice, signedTx(t, alice, bob.Address(), 10*Coin, testFee, 1))
	matureCoinbase(t, bc)

	blk := mineOn(t, bc, alice.Address(), []Transaction{
		nameTx(t, bob, &NameOp{Op: NameOpRegister, Name: "lapsed"}, 0)})
	// The block's state root was computed over the doctored state, so it
	// applies cleanly only if the expiry rule really does release the name.
	if err := bc.AddBlock(blk); err != nil {
		t.Fatalf("claiming an expired name: %v", err)
	}
	if rec, held := bc.LookupName("lapsed"); !held || rec.Owner != bob.Address() {
		t.Fatalf("after reclaiming, owner is %q held=%v", rec.Owner, held)
	}
}

// Renewal extends from the current expiry, so renewing early never costs time.
func TestRenewExtendsFromTheCurrentExpiry(t *testing.T) {
	alice, _ := wallet.New()
	bc, _ := nameChain(t, alice)
	mineTxs(t, bc, alice, nameTx(t, alice, &NameOp{Op: NameOpRegister, Name: "keeper"}, 0))
	before, _ := bc.LookupName("keeper")

	mineTxs(t, bc, alice, nameTx(t, alice, &NameOp{Op: NameOpRenew, Name: "keeper", Periods: 2}, 1))
	after, _ := bc.LookupName("keeper")

	if want := before.Expires + 2*NameLeasePeriod; after.Expires != want {
		t.Fatalf("expiry went to %d, want %d (extended from the old expiry, not from now)", after.Expires, want)
	}
}

func TestNameValidation(t *testing.T) {
	for name, want := range map[string]bool{
		"alice":                           true,
		"a-b-c":                           true,
		"shop123":                         true,
		"ab":                              false, // too short
		"-alice":                          false,
		"alice-":                          false,
		"al--ice":                         false,
		"Alice":                           false, // uppercase renders the same as lowercase
		"ali ce":                          false,
		"ali.ce":                          false,
		strings.Repeat("a", MaxNameLen+1): false,
	} {
		if got := ValidName(name) == nil; got != want {
			t.Fatalf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

// The reserved key space must be unreachable from an address, or a transfer
// could write a balance into a name's trie slot.
func TestAddressCannotOccupyANameKey(t *testing.T) {
	alice, _ := wallet.New()
	for _, tx := range []Transaction{
		{From: alice.Address(), To: NameStateKey("alice"), Amount: 1, Fee: testFee},
		{From: NameStateKey("alice"), To: alice.Address(), Amount: 1, Fee: testFee},
		{From: alice.Address(), To: "dnasx", Amount: 1, Fee: testFee, FeePayer: NameStateKey("alice")},
	} {
		if err := CheckTxSanity(tx); err == nil {
			t.Fatalf("an address occupying a name key was accepted: %+v", tx)
		}
	}
}

// Even with the string check bypassed, the trie must keep the two key spaces
// apart — that is the part that actually holds.
func TestNameAndAddressTrieKeysCannotCollide(t *testing.T) {
	// The same literal string, read as an address and as a name key, must land
	// in different trie positions.
	key := NameStateKey("alice")
	asName := trieKeyFor(key)
	asAddress := trieKeyFor("alice")
	if asName == asAddress {
		t.Fatal("a name and an address hashed to the same trie slot")
	}
}

func TestNameOpsRejectedBeforeActivation(t *testing.T) {
	alice, _ := wallet.New()
	ClearUpgradeHeight(UpgradeNameService)
	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	}
	matureCoinbase(t, bc)
	blk := mineOn(t, bc, alice.Address(), []Transaction{
		nameTx(t, alice, &NameOp{Op: NameOpRegister, Name: "early"}, 0)})
	if err := bc.AddBlock(blk); err == nil {
		t.Fatal("a name operation was accepted before its upgrade activated")
	}
}

// A transaction without a name operation must encode exactly as it did before
// the field existed, or every stored txid moved.
func TestNameFieldDoesNotChangeOtherTxids(t *testing.T) {
	alice, _ := wallet.New()
	plain := Transaction{From: alice.Address(), To: "dnasto", Amount: Coin, Fee: testFee, Nonce: 1}
	before := plain.Hash()
	withUpgradeAt(t, UpgradeNameService, 0)
	if after := plain.Hash(); after != before {
		t.Fatalf("scheduling the name service changed an unrelated txid")
	}
	named := plain
	named.Amount = 0
	named.Name = &NameOp{Op: NameOpRegister, Name: "alice"}
	if named.Hash() == before {
		t.Fatal("the name operation is not covered by the transaction id")
	}
}

// A name operation must not smuggle a transfer alongside it.
func TestNameOpCannotAlsoMoveValue(t *testing.T) {
	alice, _ := wallet.New()
	tx := Transaction{From: alice.Address(), To: "dnasto", Amount: Coin, Fee: testFee,
		Name: &NameOp{Op: NameOpRegister, Name: "alice"}}
	if err := CheckTxSanity(tx); err == nil {
		t.Fatal("a name operation carrying a transfer was accepted")
	}
}

// Two registrations of the same name must never be selected into one block:
// the second fails on apply and invalidates the whole candidate.
func TestSelectNeverPutsTwoClaimsOfOneNameInABlock(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	bc, mp := nameChain(t, alice)
	mineTxs(t, bc, alice, signedTx(t, alice, bob.Address(), 10*Coin, testFee, 0))
	matureCoinbase(t, bc)
	mp.Reconcile(bc.Height())

	if ok, err := mp.Add(nameTx(t, alice, &NameOp{Op: NameOpRegister, Name: "contested"}, 1)); !ok {
		t.Fatalf("alice's claim: %v", err)
	}
	if ok, err := mp.Add(nameTx(t, bob, &NameOp{Op: NameOpRegister, Name: "contested"}, 0)); !ok {
		t.Fatalf("bob's claim: %v", err)
	}

	sel := mp.Select(bc, 10)
	claims := 0
	for _, tx := range sel {
		if tx.IsNameOp() && tx.Name.Name == "contested" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("%d claims of one name selected into a block", claims)
	}
	// And the block must actually be valid.
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), sel))
}

// A chain with no names must have exactly the state root it would have had
// without the feature, or scheduling it forks the chain.
func TestNameFieldsDoNotChangeStateRootWithoutNames(t *testing.T) {
	plain := map[string]Account{"dnasa": {Balance: 5, Nonce: 1}}
	before := stateRoot(memStateOf(plain))
	withUpgradeAt(t, UpgradeNameService, 0)
	if after := stateRoot(memStateOf(plain)); after != before {
		t.Fatal("scheduling the name service changed the state root of a chain with no names")
	}
}

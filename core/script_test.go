package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// scriptSpend builds a spend from a script address, signs the witness items
// that need signing, and returns the transaction.
func scriptSpend(t *testing.T, scriptHex, to string, amount, fee, nonce uint64) Transaction {
	t.Helper()
	addr, err := ScriptAddress(scriptHex)
	if err != nil {
		t.Fatal(err)
	}
	return Transaction{From: addr, To: to, Amount: amount, Fee: fee, Nonce: nonce,
		Script: &ScriptAuth{Script: scriptHex}}
}

// signFor returns the hex signature a key makes over a transaction's signing
// preimage — the same bytes every other authorization shape signs.
func signFor(t *testing.T, w *wallet.Wallet, tx Transaction) string {
	t.Helper()
	return w.Sign(tx.signingBytes())
}

func pubHex(w *wallet.Wallet) string { return w.PublicKeyHex() }

func TestScriptPayToPubKey(t *testing.T) {
	alice, _ := wallet.New()
	script, err := ScriptPayToPubKey(pubHex(alice))
	if err != nil {
		t.Fatal(err)
	}
	tx := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	tx.Script.Args = []string{signFor(t, alice, tx)}

	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("a correctly signed script spend was rejected: %v", err)
	}
	// The wrong key must not do.
	mallory, _ := wallet.New()
	bad := tx
	bad.Script = &ScriptAuth{Script: script, Args: []string{signFor(t, mallory, tx)}}
	if err := bad.VerifySignature(); err == nil {
		t.Fatal("a spend signed by the wrong key was accepted")
	}
}

// The script IS the account. A spender must not be able to present a
// different, easier predicate for an address.
func TestScriptMustHashToTheSenderAddress(t *testing.T) {
	alice, _ := wallet.New()
	mallory, _ := wallet.New()
	real, _ := ScriptPayToPubKey(pubHex(alice))
	swapped, _ := ScriptPayToPubKey(pubHex(mallory))

	tx := scriptSpend(t, real, "dnasto", Coin, testFee, 0)
	// Keep alice's address, present mallory's predicate, sign with mallory.
	tx.Script = &ScriptAuth{Script: swapped}
	tx.Script.Args = []string{signFor(t, mallory, tx)}
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("a substituted predicate was accepted for someone else's address")
	}
}

func TestScriptMultisigRequiresDistinctSigners(t *testing.T) {
	a, _ := wallet.New()
	b, _ := wallet.New()
	c, _ := wallet.New()
	script, err := ScriptMultisig(2, []string{pubHex(a), pubHex(b), pubHex(c)})
	if err != nil {
		t.Fatal(err)
	}

	tx := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	tx.Script.Args = []string{signFor(t, b, tx), signFor(t, a, tx)}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("a valid 2-of-3 was rejected: %v", err)
	}

	// The classic bug: one signature counted twice to meet the threshold.
	dup := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	dup.Script.Args = []string{signFor(t, a, dup), signFor(t, a, dup)}
	if err := dup.VerifySignature(); err == nil {
		t.Fatal("one signature satisfied a 2-of-3 by being counted twice")
	}

	// A non-member cannot contribute.
	outsider, _ := wallet.New()
	out := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	out.Script.Args = []string{signFor(t, outsider, out), signFor(t, a, out)}
	if err := out.VerifySignature(); err == nil {
		t.Fatal("a non-member's signature counted toward the threshold")
	}
}

// A height constraint must be reported, not silently satisfied — and it must
// be enforced where the height is known.
func TestScriptTimeLock(t *testing.T) {
	alice, _ := wallet.New()
	script, err := ScriptTimeLock(pubHex(alice), 500)
	if err != nil {
		t.Fatal(err)
	}
	tx := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	tx.Script.Args = []string{signFor(t, alice, tx)}

	// Authorization passes: the signature is good.
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("authorization failed: %v", err)
	}
	min, err := tx.ScriptMinHeight()
	if err != nil {
		t.Fatal(err)
	}
	if min != 500 {
		t.Fatalf("minimum height is %d, want 500", min)
	}
	if !tx.ScriptNotReady(499) {
		t.Fatal("a spend below the lock height reported as ready")
	}
	if tx.ScriptNotReady(500) {
		t.Fatal("a spend at the lock height reported as not ready")
	}
}

// Both branches of an HTLC written in the language, and the trap in each.
func TestScriptHTLCBothBranches(t *testing.T) {
	recipient, _ := wallet.New()
	sender, _ := wallet.New()
	preimage := []byte("the secret")
	sum := sha256.Sum256(preimage)
	script, err := ScriptHTLC(hex.EncodeToString(sum[:]), pubHex(recipient), pubHex(sender), 900)
	if err != nil {
		t.Fatal(err)
	}

	// Claim: reveal the preimage, sign as the recipient, select the first arm.
	claim := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	claim.Script.Args = []string{
		signFor(t, recipient, claim),
		hex.EncodeToString(preimage), // above the signature: OP_SHA256 takes the top
		"01",                         // branch selector, supplied by the spender
	}
	if err := claim.VerifySignature(); err != nil {
		t.Fatalf("the claim branch was rejected: %v", err)
	}
	if min, _ := claim.ScriptMinHeight(); min != 0 {
		t.Fatalf("the claim branch reported a height constraint of %d, want none", min)
	}

	// A wrong preimage must not claim it.
	bad := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	bad.Script.Args = []string{
		signFor(t, recipient, bad),
		hex.EncodeToString([]byte("not the secret")),
		"01",
	}
	if err := bad.VerifySignature(); err == nil {
		t.Fatal("a wrong preimage claimed the contract")
	}

	// Refund: the sender's signature, second arm, and only from the timeout.
	refund := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	refund.Script.Args = []string{signFor(t, sender, refund), ""}
	if err := refund.VerifySignature(); err != nil {
		t.Fatalf("the refund branch was rejected: %v", err)
	}
	min, _ := refund.ScriptMinHeight()
	if min != 900 {
		t.Fatalf("the refund branch requires height %d, want 900", min)
	}
	if !refund.ScriptNotReady(899) {
		t.Fatal("a refund before the timeout reported as ready")
	}

	// And the recipient must not be able to take the refund branch.
	steal := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	steal.Script.Args = []string{signFor(t, recipient, steal), ""}
	if err := steal.VerifySignature(); err == nil {
		t.Fatal("the recipient took the sender's refund branch")
	}
}

func TestScriptVaultBothKeys(t *testing.T) {
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	script, err := ScriptVault(pubHex(hot), pubHex(cold), 1000)
	if err != nil {
		t.Fatal(err)
	}

	coldSpend := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	coldSpend.Script.Args = []string{signFor(t, cold, coldSpend), "01"}
	if err := coldSpend.VerifySignature(); err != nil {
		t.Fatalf("the cold key was refused: %v", err)
	}
	if min, _ := coldSpend.ScriptMinHeight(); min != 0 {
		t.Fatal("the cold key should not be time-locked")
	}

	hotSpend := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	hotSpend.Script.Args = []string{signFor(t, hot, hotSpend), ""}
	if err := hotSpend.VerifySignature(); err != nil {
		t.Fatalf("the hot key was refused: %v", err)
	}
	if min, _ := hotSpend.ScriptMinHeight(); min != 1000 {
		t.Fatalf("the hot key requires height %d, want 1000", min)
	}
}

// The bound that makes the whole thing safe: a witness is DATA. If it could
// carry operations, a spender could rewrite the predicate it must satisfy.
func TestWitnessIsNeverExecuted(t *testing.T) {
	alice, _ := wallet.New()
	script, _ := ScriptPayToPubKey(pubHex(alice))
	tx := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	// A witness item whose bytes spell OP_TRUE OP_TRUE. If it were executed,
	// the stack would end truthy without any signature.
	tx.Script.Args = []string{hex.EncodeToString([]byte{opTrue, opTrue})}
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("a witness of opcodes authorized a spend")
	}
}

// A script must leave exactly one truthy value. Leftovers are a malleability
// handle and a sign the author did not mean what it says.
func TestScriptMustLeaveACleanStack(t *testing.T) {
	alice, _ := wallet.New()
	// Pay-to-pubkey with an extra push left behind.
	script, err := NewScript().PushHex(pubHex(alice)).Op(opCheckSig).Op(opTrue).Build()
	if err != nil {
		t.Fatal(err)
	}
	tx := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	tx.Script.Args = []string{signFor(t, alice, tx)}
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("a script that left two items on the stack was accepted")
	}
}

// Everything the VM must refuse rather than run.
func TestScriptVMRejectsMalformedPrograms(t *testing.T) {
	alice, _ := wallet.New()
	cases := map[string]string{
		"unknown opcode":    "ff",
		"truncated push":    "20aabb",
		"OP_ELSE alone":     hex.EncodeToString([]byte{opElse}),
		"OP_ENDIF alone":    hex.EncodeToString([]byte{opEndIf}),
		"unterminated IF":   hex.EncodeToString([]byte{opTrue, opIf, opTrue}),
		"stack underflow":   hex.EncodeToString([]byte{opDrop}),
		"empty result":      hex.EncodeToString([]byte{opFalse}),
		"unimplemented op":  hex.EncodeToString([]byte{opCheckSenderVerify}),
		"truncated pushdat": hex.EncodeToString([]byte{opPushData, 0x01}),
	}
	for name, scriptHex := range cases {
		t.Run(name, func(t *testing.T) {
			addr, err := ScriptAddress(scriptHex)
			if err != nil {
				return // rejected before it could run, which is also correct
			}
			tx := Transaction{From: addr, To: "dnasto", Amount: Coin, Fee: testFee,
				Script: &ScriptAuth{Script: scriptHex}}
			if err := tx.VerifySignature(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
	_ = alice
}

// Execution must be bounded before it starts.
func TestScriptLimitsAreEnforced(t *testing.T) {
	// Too long.
	long := strings.Repeat("65", MaxScriptBytes+1) // OP_TRUE repeated
	if _, err := ScriptAddress(long); err == nil {
		t.Fatal("an oversized script was given an address")
	}

	// Too many stack items: push OP_TRUE more times than the stack allows.
	s := NewScript()
	for i := 0; i < MaxScriptStack+2; i++ {
		s = s.Op(opTrue)
	}
	scriptHex, err := s.Build()
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := ScriptAddress(scriptHex)
	tx := Transaction{From: addr, To: "dnasto", Amount: Coin, Fee: testFee,
		Script: &ScriptAuth{Script: scriptHex}}
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("a script that overflowed the stack was accepted")
	}

	// Too many witness items.
	simple, _ := ScriptPayToPubKey(strings.Repeat("aa", 32))
	a2, _ := ScriptAddress(simple)
	args := make([]string, MaxScriptWitness+1)
	for i := range args {
		args[i] = "01"
	}
	tx2 := Transaction{From: a2, To: "dnasto", Amount: Coin, Fee: testFee,
		Script: &ScriptAuth{Script: simple, Args: args}}
	if err := tx2.VerifySignature(); err == nil {
		t.Fatal("an oversized witness was accepted")
	}
}

// A script spend must work end to end on a real chain, under the upgrade.
func TestScriptSpendOnChain(t *testing.T) {
	withUpgradeAt(t, UpgradeScript, 0)
	funder, _ := wallet.New()
	alice, _ := wallet.New()
	dest, _ := wallet.New()

	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, funder.Address(), nil))
	}
	matureCoinbase(t, bc)

	script, err := ScriptPayToPubKey(pubHex(alice))
	if err != nil {
		t.Fatal(err)
	}
	scriptAddr, _ := ScriptAddress(script)

	// Fund the script address with an ordinary transfer.
	mustAdd(t, bc, mineOn(t, bc, funder.Address(),
		[]Transaction{signedTx(t, funder, scriptAddr, 20*Coin, testFee, 0)}))
	if got := bc.Account(scriptAddr).Balance; got != 20*Coin {
		t.Fatalf("the script address holds %d", got)
	}

	// Spend from it.
	spend := Transaction{From: scriptAddr, To: dest.Address(), Amount: 5 * Coin,
		Fee: testFee, Nonce: 0, Script: &ScriptAuth{Script: script}}
	spend.Script.Args = []string{signFor(t, alice, spend)}
	mustAdd(t, bc, mineOn(t, bc, funder.Address(), []Transaction{spend}))

	if got := bc.Account(dest.Address()).Balance; got != 5*Coin {
		t.Fatalf("destination holds %d, want %d", got, 5*Coin)
	}
}

// A time-locked script must be refused by the CHAIN below its height, not
// merely reported as not ready.
func TestScriptHeightEnforcedByConsensus(t *testing.T) {
	withUpgradeAt(t, UpgradeScript, 0)
	funder, _ := wallet.New()
	alice, _ := wallet.New()

	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, funder.Address(), nil))
	}
	matureCoinbase(t, bc)

	script, _ := ScriptTimeLock(pubHex(alice), 10_000)
	scriptAddr, _ := ScriptAddress(script)
	mustAdd(t, bc, mineOn(t, bc, funder.Address(),
		[]Transaction{signedTx(t, funder, scriptAddr, 20*Coin, testFee, 0)}))

	spend := Transaction{From: scriptAddr, To: "dnasto", Amount: Coin,
		Fee: testFee, Nonce: 0, Script: &ScriptAuth{Script: script}}
	spend.Script.Args = []string{signFor(t, alice, spend)}

	if err := bc.AddBlock(mineOn(t, bc, funder.Address(), []Transaction{spend})); err == nil {
		t.Fatal("a time-locked script spend was accepted far below its height")
	}
}

func TestScriptRejectedBeforeActivation(t *testing.T) {
	ClearUpgradeHeight(UpgradeScript)
	funder, _ := wallet.New()
	alice, _ := wallet.New()
	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, funder.Address(), nil))
	}
	matureCoinbase(t, bc)

	script, _ := ScriptPayToPubKey(pubHex(alice))
	scriptAddr, _ := ScriptAddress(script)
	mustAdd(t, bc, mineOn(t, bc, funder.Address(),
		[]Transaction{signedTx(t, funder, scriptAddr, 20*Coin, testFee, 0)}))

	spend := Transaction{From: scriptAddr, To: "dnasto", Amount: Coin,
		Fee: testFee, Nonce: 0, Script: &ScriptAuth{Script: script}}
	spend.Script.Args = []string{signFor(t, alice, spend)}
	if err := bc.AddBlock(mineOn(t, bc, funder.Address(), []Transaction{spend})); err == nil {
		t.Fatal("a script spend was accepted before its upgrade activated")
	}
}

// A transaction with no script must encode exactly as before.
func TestScriptFieldDoesNotChangeOtherTxids(t *testing.T) {
	alice, _ := wallet.New()
	plain := Transaction{From: alice.Address(), To: "dnasto", Amount: Coin, Fee: testFee}
	before := plain.Hash()
	withUpgradeAt(t, UpgradeScript, 0)
	if after := plain.Hash(); after != before {
		t.Fatal("scheduling the script upgrade changed an unrelated txid")
	}
}

// The witness is covered by the txid even though it cannot be covered by the
// signature inside it — otherwise a relay could strip or swap it freely.
func TestWitnessIsCoveredByTheTxid(t *testing.T) {
	alice, _ := wallet.New()
	script, _ := ScriptPayToPubKey(pubHex(alice))
	tx := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	tx.Script.Args = []string{signFor(t, alice, tx)}
	with := tx.Hash()

	stripped := tx
	stripped.Script = &ScriptAuth{Script: script}
	if stripped.Hash() == with {
		t.Fatal("removing the witness did not change the transaction id")
	}
}

func TestDisassembleScriptIsReadable(t *testing.T) {
	alice, _ := wallet.New()
	script, _ := ScriptTimeLock(pubHex(alice), 500)
	text, err := DisassembleScript(script)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CHECKHEIGHTVERIFY", "CHECKSIG", "PUSH "} {
		if !strings.Contains(text, want) {
			t.Fatalf("disassembly lacks %q:\n%s", want, text)
		}
	}
	if _, err := DisassembleScript("ff"); err == nil {
		t.Fatal("an unknown opcode disassembled without complaint")
	}
}

// A script address must be a well-formed, checksummed address like any other,
// so every client already knows how to display and validate one.
func TestScriptAddressIsAWellFormedAddress(t *testing.T) {
	alice, _ := wallet.New()
	script, _ := ScriptPayToPubKey(pubHex(alice))
	addr, err := ScriptAddress(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := wallet.ValidateAddress(addr); err != nil {
		t.Fatalf("script address %q is not valid: %v", addr, err)
	}
	// And it must not collide with the other derivations over the same bytes.
	ms, err := wallet.MultisigAddress(1, []string{pubHex(alice)})
	if err != nil {
		t.Fatal(err)
	}
	if addr == ms {
		t.Fatal("a script address collided with a multisig address")
	}
}

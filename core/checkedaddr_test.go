package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// Consensus has never validated that a recipient is a real address — only that
// it is not absurdly long. Every client checks before signing, but that is a
// convention rather than a rule, so a buggy or malicious client could put
// anything in `To` and the coin would land on a state key nobody holds a key
// for. UpgradeCheckedAddresses closes that; these tests pin both halves: the
// rule bites when active, and changes nothing before its activation height.

// withCheckedAddresses activates the upgrade at height 1 for one test.
func withCheckedAddresses(t *testing.T) {
	t.Helper()
	SetUpgradeHeight(UpgradeCheckedAddresses, 1)
	t.Cleanup(ClearUpgrades)
}

func mustAddr(t *testing.T) (*wallet.Wallet, string) {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	return w, w.Address()
}

func TestMalformedRecipientIsRejectedWhenActive(t *testing.T) {
	withCheckedAddresses(t)
	alice, _ := mustAddr(t)

	for _, tc := range []struct{ name, to string }{
		{"not an address", "hello-world"},
		{"missing prefix", "0123456789abcdef0123456789abcdef01234567cafebabe"},
		{"bad checksum", func() string {
			_, good := mustAddr(t)
			// Flip the last hex digit to another valid hex digit: still parses as
			// hex and has the right length, so ONLY the checksum catches it. This
			// is the realistic typo.
			last := byte('0')
			if good[len(good)-1] == '0' {
				last = '1'
			}
			return good[:len(good)-1] + string(last)
		}()},
		{"truncated", "dnas1234"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := signedTx(t, alice, tc.to, Coin, testFee, 0)
			if err := checkTxAtHeight(tx, 10); err == nil {
				t.Fatalf("recipient %q was accepted", tc.to)
			} else if !strings.Contains(err.Error(), "recipient address") {
				t.Errorf("error should name the recipient: %v", err)
			}
		})
	}
}

func TestWellFormedAddressesAreAcceptedWhenActive(t *testing.T) {
	withCheckedAddresses(t)
	alice, _ := mustAddr(t)
	_, bobAddr := mustAddr(t)

	tx := signedTx(t, alice, bobAddr, Coin, testFee, 0)
	if err := checkTxAtHeight(tx, 10); err != nil {
		t.Fatalf("a valid transfer was rejected: %v", err)
	}
}

// Script addresses (multisig, HTLC, vault) are produced by the wallet package
// in the same checksummed format, so the new rule must not lock them out — that
// would silently disable three features.
func TestScriptAddressesStillValidate(t *testing.T) {
	withCheckedAddresses(t)
	alice, _ := mustAddr(t)
	a, _ := mustAddr(t)
	b, _ := mustAddr(t)
	c, _ := mustAddr(t)

	ms, err := wallet.MultisigAddress(2, []string{a.PublicKeyHex(), b.PublicKeyHex(), c.PublicKeyHex()})
	if err != nil {
		t.Fatal(err)
	}
	const preimageHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	htlc, err := wallet.HTLCAddress(preimageHash, b.PublicKeyHex(), a.PublicKeyHex(), 500)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := wallet.VaultAddress(a.PublicKeyHex(), b.PublicKeyHex(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for name, addr := range map[string]string{"multisig": ms, "htlc": htlc, "vault": vault} {
		tx := signedTx(t, alice, addr, Coin, testFee, 0)
		if err := checkTxAtHeight(tx, 10); err != nil {
			t.Errorf("paying a %s address was rejected: %v", name, err)
		}
	}
}

// Every recipient of a multi-output transfer must be checked, not just the
// first — otherwise a batch payment is a way around the rule.
func TestEveryMultiOutputRecipientIsChecked(t *testing.T) {
	withCheckedAddresses(t)
	SetUpgradeHeight(UpgradeMultiOutput, 1)
	alice, _ := mustAddr(t)
	_, good := mustAddr(t)

	tx := Transaction{From: alice.Address(), Fee: testFee, Nonce: 0, Outputs: []Output{
		{To: good, Amount: Coin},
		{To: "definitely-not-an-address", Amount: Coin},
	}}
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := checkTxAtHeight(tx, 10); err == nil {
		t.Fatal("a batch payment with one bad recipient was accepted")
	}
}

func TestFeePayerAddressIsChecked(t *testing.T) {
	withCheckedAddresses(t)
	SetUpgradeHeight(UpgradeFeeSponsor, 1)
	alice, _ := mustAddr(t)
	_, bobAddr := mustAddr(t)

	tx := Transaction{
		From: alice.Address(), To: bobAddr, Amount: Coin, Fee: testFee, Nonce: 0,
		FeePayer: "not-an-address",
	}
	if err := tx.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if err := checkTxAtHeight(tx, 10); err == nil {
		t.Fatal("a sponsorship naming a malformed payer was accepted")
	}
}

// The activation half: below the scheduled height nothing changes, so a chain
// mined under the old rule still replays. This is what makes the tightening a
// scheduled upgrade rather than a break.
func TestMalformedAddressesStillAcceptedBeforeActivation(t *testing.T) {
	SetUpgradeHeight(UpgradeCheckedAddresses, 100)
	t.Cleanup(ClearUpgrades)
	alice, _ := mustAddr(t)

	tx := signedTx(t, alice, "hello-world", Coin, testFee, 0)
	if err := checkTxAtHeight(tx, 50); err != nil {
		t.Fatalf("below the activation height the old rule must stand, got: %v", err)
	}
	if err := checkTxAtHeight(tx, 100); err == nil {
		t.Fatal("at the activation height the rule must bite")
	}
}

// An unscheduled upgrade is never active, so a node that has not been told about
// it behaves exactly as before.
func TestUnscheduledUpgradeChangesNothing(t *testing.T) {
	ClearUpgrades()
	alice, _ := mustAddr(t)
	tx := signedTx(t, alice, "hello-world", Coin, testFee, 0)
	if err := checkTxAtHeight(tx, 1_000_000); err != nil {
		t.Fatalf("an unscheduled upgrade must not apply: %v", err)
	}
}

func TestCheckedAddressesIsAKnownUpgrade(t *testing.T) {
	if !KnownUpgrade(UpgradeCheckedAddresses) {
		t.Fatal("the upgrade is not in knownUpgrades, so -upgrades would reject its name")
	}
}

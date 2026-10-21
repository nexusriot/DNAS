package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// UpgradeCheckedAddresses demands a well-formed address of every address a
// transaction names. Three transaction forms name no RECIPIENT at all —
// CheckTxSanity requires their To to be empty — so the rule must exempt them.
// Until it did, scheduling the upgrade on a chain that also ran the asset or
// name features made every mint, burn and name operation permanently invalid
// from the activation height: not a tightening, a flag day that quietly
// switched two shipped features off.

func TestCheckedAddressesAllowsTheRecipientlessForms(t *testing.T) {
	withUpgradeAt(t, UpgradeCheckedAddresses, 0)
	withUpgradeAt(t, UpgradeAssetOps, 0)
	withUpgradeAt(t, UpgradeNameService, 0)

	bc, issuer, id := issuedChain(t)
	matureCoinbase(t, bc)

	for name, tx := range map[string]Transaction{
		"mint":  opTx(t, issuer, id, AssetOpMint, 10, 2),
		"burn":  opTx(t, issuer, id, AssetOpBurn, 10, 2),
		"name":  nameTx(t, issuer, &NameOp{Op: NameOpRegister, Name: "issuer"}, 2),
		"renew": nameTx(t, issuer, &NameOp{Op: NameOpRenew, Name: "issuer"}, 2),
	} {
		if tx.To != "" {
			t.Fatalf("%s: test builds a tx with a recipient (%q); it is not the form under test", name, tx.To)
		}
		if err := checkTxAddresses(tx); err != nil {
			t.Errorf("%s rejected under checked addresses: %v", name, err)
		}
	}

	// The exemption is for the recipientless forms only: an ordinary transfer
	// still has to name a real recipient.
	bad := Transaction{From: issuer.Address(), To: "", Amount: 1, Fee: testFee, Nonce: 2}
	if err := checkTxAddresses(bad); err == nil {
		t.Error("a transfer with no recipient passed the address check")
	}
}

// A name points at an address, and that address is where a payer who resolves
// the name will send coin. It is exactly the typo this upgrade exists to catch.
func TestCheckedAddressesValidatesTheNameTarget(t *testing.T) {
	withUpgradeAt(t, UpgradeCheckedAddresses, 0)
	withUpgradeAt(t, UpgradeNameService, 0)

	owner, _ := wallet.New()
	good, _ := wallet.New()

	for _, op := range []string{NameOpRegister, NameOpTransfer} {
		ok := nameTx(t, owner, &NameOp{Op: op, Name: "payme", Target: good.Address()}, 0)
		if err := checkTxAddresses(ok); err != nil {
			t.Errorf("%s at a valid target rejected: %v", op, err)
		}
		bad := nameTx(t, owner, &NameOp{Op: op, Name: "payme", Target: "dnasnotarealaddress"}, 0)
		err := checkTxAddresses(bad)
		if err == nil {
			t.Errorf("%s at a malformed target accepted", op)
			continue
		}
		if !strings.Contains(err.Error(), "name target") {
			t.Errorf("%s: error %q does not say the name target is at fault", op, err)
		}
	}
}

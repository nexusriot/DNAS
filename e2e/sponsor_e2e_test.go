//go:build e2e

package e2e

import "testing"

// The two-party fee-sponsorship flow, end to end through the real CLI: an
// address holding nothing pays someone, and a third party covers the fee.
func TestSponsoredTransfer(t *testing.T) {
	n := startNode(t, nodeOpts{name: "sponsor", extra: []string{"-upgrades", "feesponsor:0"}})
	fundNode(t, n)

	broke := newWallet(t, n.dir, "broke.json")
	payer := newWallet(t, n.dir, "payer.json")
	recipient := newWallet(t, n.dir, "sponsor-recipient.json")
	// The sender gets exactly what it will pay out and not one unit more.
	n.send(broke, 1000, testFee)
	n.send(payer, 5*Coin, testFee)
	n.generate(1)

	req := n.cli("sponsor", "request", "-api", n.apiAddr, "-key", "broke.json",
		"-to", recipient, "-amount", "0.00001", "-payer", payer, "-o", "sponsored.json")
	mustContain(t, req, "wrote sponsored.json", "sponsor request")

	pay := n.cli("sponsor", "pay", "-api", n.apiAddr, "-wallet", "payer.json",
		"-in", "sponsored.json", "-submit")
	mustContain(t, pay, "submitted", "sponsor pay")
	n.generate(1)

	if bal := n.balance(recipient); bal != 1000 {
		t.Fatalf("recipient balance = %d, want 1000", bal)
	}
	if bal := n.balance(broke); bal != 0 {
		t.Fatalf("sender balance = %d, want 0 — it paid out everything and no fee", bal)
	}
	if bal := n.balance(payer); bal >= 5*Coin {
		t.Fatalf("payer balance = %d, want it reduced by the fee it agreed to pay", bal)
	}
}

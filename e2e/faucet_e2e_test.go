//go:build e2e

package e2e

import "testing"

// The faucet exists only on regtest, which is also the only network every node
// in this suite runs — so these are the tests that would silently pass on a
// misconfigured network and have to assert what /info reports as well.

func TestFaucetHandsOutCoinOverHTTP(t *testing.T) {
	n := startNode(t, nodeOpts{name: "faucet", extra: []string{"-faucet"}})
	fundNode(t, n) // the faucet spends the node's own wallet, so it must hold coin

	var info map[string]any
	n.getJSON("/info", &info)
	if info["faucet"] != true {
		t.Fatalf("/info says faucet=%v, want true on a regtest node started with -faucet", info["faucet"])
	}

	bob := newWallet(t, n.dir, "faucet-bob.json")
	code, body := n.post("/faucet", map[string]string{"address": bob})
	if code != 200 {
		t.Fatalf("faucet: status %d, body %v", code, body)
	}
	n.generate(1)
	if bal := n.balance(bob); bal == 0 {
		t.Fatal("the faucet payment never reached the recipient")
	}

	// The cooldown is per requester as well as per address, so a second ask from
	// the same client is refused even for a different address.
	carol := newWallet(t, n.dir, "faucet-carol.json")
	if code, _ := n.post("/faucet", map[string]string{"address": carol}); code != 400 {
		t.Fatalf("second faucet request status = %d, want 400 (cooldown)", code)
	}
}

func TestFaucetRefusedWithoutTheFlag(t *testing.T) {
	n := startNode(t, nodeOpts{name: "nofaucet"})
	bob := newWallet(t, n.dir, "nofaucet-bob.json")
	if code, _ := n.post("/faucet", map[string]string{"address": bob}); code != 403 {
		t.Fatalf("faucet status = %d, want 403 on a node that was not started with -faucet", code)
	}
}

func TestFaucetCLIFundsAWallet(t *testing.T) {
	n := startNode(t, nodeOpts{name: "faucetcli", extra: []string{"-faucet"}})
	fundNode(t, n)
	bob := newWallet(t, n.dir, "cli-bob.json")

	out := n.cli("faucet", "-api", n.apiAddr, "-address", bob)
	mustContain(t, out, "faucet sent", "faucet cli")
	n.generate(1)
	if bal := n.balance(bob); bal == 0 {
		t.Fatal("the CLI faucet did not fund the wallet")
	}
}

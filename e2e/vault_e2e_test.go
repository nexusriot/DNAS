//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// A vault's cold key spends immediately; its hot key must wait for the unlock
// height. Both paths run through the real CLI against a real node.
func TestVaultColdKeySweepsImmediately(t *testing.T) {
	n := startNode(t, nodeOpts{name: "vault", extra: []string{"-upgrades", "vault:0"}})
	fundNode(t, n)

	newWallet(t, n.dir, "hot.json")
	coldAddr := newWallet(t, n.dir, "cold.json")
	hotPub := strings.TrimSpace(n.cli("wallet", "pubkey", "-o", "hot.json"))
	coldPub := strings.TrimSpace(n.cli("wallet", "pubkey", "-o", "cold.json"))

	unlock := n.height() + 1000 // far away: only the cold key can move this
	vaultAddr := strings.TrimSpace(n.cli("vault", "address", "-hot", hotPub, "-cold", coldPub, "-unlock", itoa(unlock)))
	if !strings.HasPrefix(vaultAddr, "dnas") {
		t.Fatalf("vault address: %q", vaultAddr)
	}

	n.send(vaultAddr, 5*Coin, testFee)
	n.generate(1)
	if bal := n.balance(vaultAddr); bal != 5*Coin {
		t.Fatalf("vault balance = %d, want %d", bal, 5*Coin)
	}

	// A hot-key spend this far below the unlock height is admitted to the mempool
	// and simply never mined — the thief parks it at the vault's nonce.
	early := n.cli("vault", "spend", "-api", n.apiAddr, "-wallet", "hot.json",
		"-cold", coldPub, "-unlock", itoa(unlock), "-to", coldAddr, "-fee", itoa(testFee))
	mustContain(t, early, "submitted hot-key sweep", "hot-key spend")
	n.generate(2)
	if bal := n.balance(vaultAddr); bal != 5*Coin {
		t.Fatalf("vault balance = %d after two blocks, want the hot-key spend to be unmineable", bal)
	}

	// The cold key rescues it. It shares the thief's nonce, so it must out-bid
	// them (replace-by-fee) — which it always can, sweeping the whole balance.
	sweep := n.cli("vault", "spend", "-api", n.apiAddr, "-wallet", "cold.json",
		"-hot", hotPub, "-unlock", itoa(unlock), "-to", coldAddr, "-fee", itoa(4*testFee))
	mustContain(t, sweep, "submitted cold-key sweep", "vault cold sweep")
	n.generate(1)
	if bal := n.balance(vaultAddr); bal != 0 {
		t.Fatalf("vault still holds %d after the cold sweep", bal)
	}
	if bal := n.balance(coldAddr); bal == 0 {
		t.Fatal("the cold sweep did not pay out")
	}
}

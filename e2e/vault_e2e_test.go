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

	// A hot-key spend this far below the unlock height is REFUSED by the mempool:
	// a transaction no block will accept for ~1000 blocks would sit in the pool
	// for free (MaxMempoolLockAhead), and here it would also park the thief at
	// the vault's nonce. Refusing it is both the DoS bound and the better vault.
	early := n.cliAllowFail("vault", "spend", "-api", n.apiAddr, "-wallet", "hot.json",
		"-cold", coldPub, "-unlock", itoa(unlock), "-to", coldAddr, "-fee", itoa(testFee))
	if early.err == nil {
		t.Fatalf("a hot-key spend 1000 blocks before unlock was accepted:\n%s", early.out)
	}
	mustContain(t, early.out, "not minable", "hot-key spend refused")
	n.generate(2)
	if bal := n.balance(vaultAddr); bal != 5*Coin {
		t.Fatalf("vault balance = %d after two blocks, want it untouched", bal)
	}

	// The cold key sweeps it at once, with nothing in its way.
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

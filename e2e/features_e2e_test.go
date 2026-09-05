//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// itoa renders a height for a CLI flag.
func itoa(v uint64) string { return strconv.FormatUint(v, 10) }

// End-to-end coverage for the features a unit test cannot reach: they involve
// the real binary, real flags, and the real HTTP surface, which is exactly where
// a wiring mistake hides. Every node here is regtest, which is also the only
// network where a faucet exists.

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

func TestAddressHistoryIndex(t *testing.T) {
	n := startNode(t, nodeOpts{name: "addrindex", extra: []string{"-addrindex"}})
	fundNode(t, n)
	bob := newWallet(t, n.dir, "hist-bob.json")
	first := n.send(bob, 2*Coin, testFee)
	n.generate(1)
	second := n.send(bob, 3*Coin, testFee)
	n.generate(1)

	var hist struct {
		Total   int `json:"total"`
		Entries []struct {
			Hash          string `json:"hash"`
			Height        uint64 `json:"height"`
			Confirmations uint64 `json:"confirmations"`
		} `json:"entries"`
	}
	n.getJSON("/address/"+bob+"/history", &hist)
	if hist.Total != 2 || len(hist.Entries) != 2 {
		t.Fatalf("history = %d entries (total %d), want 2", len(hist.Entries), hist.Total)
	}
	got := []string{hist.Entries[0].Hash, hist.Entries[1].Hash}
	if got[0] != first || got[1] != second {
		t.Fatalf("history = %v, want %v (oldest first)", got, []string{first, second})
	}
	if hist.Entries[0].Confirmations < hist.Entries[1].Confirmations {
		t.Fatal("the older entry should have more confirmations")
	}
}

func TestAddressHistoryUnavailableWithoutTheIndex(t *testing.T) {
	n := startNode(t, nodeOpts{name: "noindex"})
	bob := newWallet(t, n.dir, "noindex-bob.json")
	code, body := n.get("/address/" + bob + "/history")
	if code != 503 {
		t.Fatalf("status = %d (%s), want 503 without -addrindex", code, strings.TrimSpace(body))
	}
}

// The share path end to end: the template advertises an easier target, the
// miner submits work against it, and the node's ledger credits the address.
func TestMinerSubmitsShares(t *testing.T) {
	// A small share factor keeps the run short: at the default 256 the miner posts
	// hundreds of shares per block, and each one is an HTTP round trip.
	n := startNode(t, nodeOpts{name: "shares", extra: []string{"-sharefactor", "4"}})
	payTo := newWallet(t, n.dir, "shares-miner.json")

	var tmpl struct {
		Bits        uint32 `json:"bits"`
		ShareBits   uint32 `json:"share_bits"`
		ShareFactor uint32 `json:"share_factor"`
	}
	n.getJSON("/blocktemplate?address="+payTo, &tmpl)
	if tmpl.ShareBits == 0 || tmpl.ShareFactor == 0 {
		t.Fatalf("template carries no share target: %+v", tmpl)
	}

	out := n.cli("miner", "-api", n.apiAddr, "-address", payTo, "-once", "-shares")
	mustContain(t, out, "✓ mined + submitted block", "share-mining miner")

	var ledger struct {
		Submitted uint64 `json:"submitted"`
		Accepted  uint64 `json:"accepted"`
		Miners    []struct {
			Address string `json:"address"`
			Shares  uint64 `json:"shares"`
		} `json:"miners"`
	}
	n.getJSON("/shares", &ledger)
	// Every share the miner sent must have been accepted: a rejected one means the
	// node and the miner disagree about something structural, which is exactly the
	// class of bug this test exists to catch.
	if ledger.Submitted == 0 || ledger.Submitted != ledger.Accepted {
		t.Fatalf("share ledger: %d submitted, %d accepted (want a non-zero, fully-accepted count)",
			ledger.Submitted, ledger.Accepted)
	}
	if len(ledger.Miners) != 1 || ledger.Miners[0].Address != payTo {
		t.Fatalf("share ledger credits %v, want one row for %s", ledger.Miners, payTo)
	}
}

// A node on another network must be unusable as a peer, and its chain store
// unreadable as ours: this is the whole point of network separation.
func TestNetworkSeparation(t *testing.T) {
	n := startNode(t, nodeOpts{name: "netsep"})
	var info map[string]any
	n.getJSON("/info", &info)
	if info["network"] != "regtest" {
		t.Fatalf("/info network = %v, want regtest", info["network"])
	}
	n.generate(1)

	// The same store read as mainnet must not look like a valid mainnet chain.
	out := n.cli("db", "info", "-db", "chain.db", "-network", "mainnet")
	mustContain(t, out, "MISMATCH", "db info across networks")
	// Read as the network it belongs to, it is fine.
	ok := n.cli("db", "info", "-db", "chain.db", "-network", "regtest")
	mustContain(t, ok, "genesis  ok", "db info on its own network")
}

func TestDBVerifyExportImport(t *testing.T) {
	n := startNode(t, nodeOpts{name: "dbtool"})
	fundNode(t, n)
	n.generate(2)
	height := n.height()
	n.stop() // the store must be closed before another process reads it

	verify := n.cli("db", "verify", "-db", "chain.db", "-network", "regtest")
	mustContain(t, verify, "ok:", "db verify")

	export := n.cli("db", "export", "-db", "chain.db", "-network", "regtest", "-o", "export.json")
	mustContain(t, export, "exported", "db export")

	imported := n.cli("db", "import", "-db", "restored.db", "-network", "regtest", "-in", "export.json")
	mustContain(t, imported, "imported", "db import")

	info := n.cli("db", "info", "-db", "restored.db", "-network", "regtest")
	mustContain(t, info, "genesis  ok", "db info on the restored store")
	if !strings.Contains(info, "height   "+itoa(height)) {
		t.Fatalf("restored store is not at height %d:\n%s", height, info)
	}
}

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

// A late-joining node must learn transactions that were broadcast before it
// connected — the mempool is otherwise only ever filled by live gossip.
func TestMempoolReconciliationOnJoin(t *testing.T) {
	a := startNode(t, nodeOpts{name: "pool-a"})
	fundNode(t, a)
	bob := newWallet(t, a.dir, "pool-bob.json")
	hash := a.send(bob, Coin, testFee)

	var pending []map[string]any
	a.getJSON("/mempool", &pending)
	if len(pending) != 1 {
		t.Fatalf("origin mempool holds %d transactions, want 1", len(pending))
	}

	b := startNode(t, nodeOpts{name: "pool-b", peers: []string{a.p2pAddr}})
	waitFor(t, 45*time.Second, "the late peer to learn the pending transaction", func() bool {
		var theirs []map[string]any
		b.getJSON("/mempool", &theirs)
		for _, tx := range theirs {
			if tx["hash"] == hash {
				return true
			}
		}
		// The transaction may also have been mined out from under us; either way the
		// peer must know about it.
		code, _ := b.get("/tx/" + hash)
		return code == 200
	})
}

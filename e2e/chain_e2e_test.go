//go:build e2e

package e2e

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testFee is comfortably above the per-byte base fee for any transaction these
// tests build, so a payment is never rejected for underpaying.
const testFee = Coin / 100

// fundNode mines until the node wallet holds a spendable (matured) coinbase and
// returns the wallet address. CoinbaseMaturity is 3, so the reward from block 1
// is spendable once the chain reaches height 4.
func fundNode(t *testing.T, n *node) string {
	t.Helper()
	n.generate(5)
	addr := n.address()
	if bal := n.balance(addr); bal == 0 {
		t.Fatalf("node wallet holds nothing after mining 5 blocks")
	}
	return addr
}

func TestNodeStartsAtGenesis(t *testing.T) {
	n := startNode(t, nodeOpts{name: "genesis"})

	if h := n.height(); h != 0 {
		t.Fatalf("a fresh node starts at height %d, want 0", h)
	}
	if n.tip() == "" {
		t.Fatal("no tip hash reported")
	}

	// Genesis is fixed, so an independently started node must compute the same
	// hash — without that, two nodes could never agree on a chain.
	other := startNode(t, nodeOpts{name: "genesis-b"})
	if n.tip() != other.tip() {
		t.Fatalf("genesis differs between nodes: %s vs %s", n.tip(), other.tip())
	}
}

func TestMinePayAndConfirm(t *testing.T) {
	n := startNode(t, nodeOpts{name: "pay"})
	miner := fundNode(t, n)
	bob := newWallet(t, n.dir, "bob.json")

	before := n.balance(miner)
	hash := n.send(bob, 3*Coin, testFee)

	// Submitted but unmined: the same endpoint reports it as pending.
	var pending struct {
		Status        string `json:"status"`
		Confirmations uint64 `json:"confirmations"`
	}
	n.getJSON("/tx/"+hash, &pending)
	if pending.Status != "pending" || pending.Confirmations != 0 {
		t.Fatalf("unmined transaction reported as %+v, want pending with 0 confirmations", pending)
	}
	if n.balance(bob) != 0 {
		t.Fatal("an unmined payment must not move a balance")
	}

	n.generate(1)

	var confirmed struct {
		Status        string `json:"status"`
		Height        uint64 `json:"height"`
		Index         int    `json:"index"`
		Confirmations uint64 `json:"confirmations"`
		BlockHash     string `json:"block_hash"`
	}
	n.getJSON("/tx/"+hash, &confirmed)
	if confirmed.Status != "confirmed" {
		t.Fatalf("mined transaction reported as %q", confirmed.Status)
	}
	if confirmed.Height != n.height() || confirmed.Confirmations != 1 {
		t.Fatalf("confirmed at height %d with %d confirmations, chain is at %d",
			confirmed.Height, confirmed.Confirmations, n.height())
	}
	if confirmed.Index != 1 {
		t.Fatalf("payment is at index %d, want 1 (first non-coinbase)", confirmed.Index)
	}
	if confirmed.BlockHash == "" {
		t.Fatal("confirmed transaction reports no block hash")
	}

	if got := n.balance(bob); got != 3*Coin {
		t.Fatalf("recipient holds %d, want %d", got, 3*Coin)
	}
	// The miner paid 3 coin plus the fee, and earned this block's subsidy and tip
	// back, so its balance must have moved but never dropped by more than the spend.
	if after := n.balance(miner); after+3*Coin+testFee < before {
		t.Fatalf("sender balance %d is short: was %d, spent %d + fee", after, before, 3*Coin)
	}
}

func TestUnknownTransactionIsNotFound(t *testing.T) {
	n := startNode(t, nodeOpts{name: "notfound"})
	unknown := "0000000000000000000000000000000000000000000000000000000000000000"
	if status, body := n.get("/tx/" + unknown); status != http.StatusNotFound {
		t.Fatalf("unknown transaction returned %d: %s", status, body)
	}
}

// Coin is created only by block subsidies and destroyed only by the burned base
// fee, so minted − burned must always equal what accounts hold.
func TestSupplyAccountingHolds(t *testing.T) {
	n := startNode(t, nodeOpts{name: "supply"})

	type supply struct {
		Height      uint64 `json:"height"`
		Minted      uint64 `json:"minted"`
		Burned      uint64 `json:"burned"`
		Circulating uint64 `json:"circulating"`
		Consistent  bool   `json:"consistent"`
	}

	var genesis supply
	n.getJSON("/supply", &genesis)
	if genesis.Minted != 0 || genesis.Circulating != 0 || !genesis.Consistent {
		t.Fatalf("nothing should exist at genesis, got %+v", genesis)
	}

	miner := fundNode(t, n)

	var mined supply
	n.getJSON("/supply", &mined)
	if !mined.Consistent {
		t.Fatalf("supply inconsistent after mining: %+v", mined)
	}
	if mined.Burned != 0 {
		t.Fatalf("empty blocks burned %d, want 0", mined.Burned)
	}
	if mined.Circulating != mined.Minted || mined.Minted == 0 {
		t.Fatalf("with nothing burned, circulating should equal minted: %+v", mined)
	}
	if mined.Circulating != n.balance(miner) {
		t.Fatalf("the only account holds %d but %d is circulating", n.balance(miner), mined.Circulating)
	}

	bob := newWallet(t, n.dir, "bob.json")
	n.send(bob, 2*Coin, testFee)
	n.generate(1)

	var spent supply
	n.getJSON("/supply", &spent)
	if !spent.Consistent {
		t.Fatalf("supply inconsistent after a fee-paying transaction: %+v", spent)
	}
	if spent.Burned == 0 {
		t.Fatal("a fee-paying transaction burned nothing")
	}
	if spent.Minted-spent.Burned != spent.Circulating {
		t.Fatalf("minted %d − burned %d != circulating %d", spent.Minted, spent.Burned, spent.Circulating)
	}

	// The CLI reports the same numbers and says so in words.
	out := n.cli("supply", "-api", n.apiAddr)
	mustContain(t, out, "conservation  ok", "dnas supply")
}

// A node must come back up on the chain it had, not resync from nothing.
func TestChainSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "restart")
	n := startNode(t, nodeOpts{name: "restart", dir: dir})
	miner := fundNode(t, n)
	bob := newWallet(t, dir, "bob.json")
	hash := n.send(bob, Coin, testFee)
	n.generate(1)

	wantHeight, wantTip := n.height(), n.tip()
	wantMiner, wantBob := n.balance(miner), n.balance(bob)
	n.stop()

	restarted := startNode(t, nodeOpts{name: "restart", dir: dir})
	if got := restarted.height(); got != wantHeight {
		t.Fatalf("height %d after restart, want %d", got, wantHeight)
	}
	if got := restarted.tip(); got != wantTip {
		t.Fatalf("tip %s after restart, want %s", got, wantTip)
	}
	if got := restarted.balance(miner); got != wantMiner {
		t.Fatalf("miner balance %d after restart, want %d", got, wantMiner)
	}
	if got := restarted.balance(bob); got != wantBob {
		t.Fatalf("recipient balance %d after restart, want %d", got, wantBob)
	}
	// The transaction index is derived state, rebuilt by replaying the store.
	var tx struct {
		Status string `json:"status"`
	}
	restarted.getJSON("/tx/"+hash, &tx)
	if tx.Status != "confirmed" {
		t.Fatalf("transaction lookup after restart reported %q", tx.Status)
	}
	var s struct {
		Consistent bool `json:"consistent"`
	}
	restarted.getJSON("/supply", &s)
	if !s.Consistent {
		t.Fatal("supply accounting did not survive the restart")
	}
}

// With a token configured, writes need it and reads stay open.
func TestAPITokenGuardsWrites(t *testing.T) {
	const token = "e2e-secret-token"
	n := startNode(t, nodeOpts{name: "auth", token: token})

	if status, body := n.get("/info"); status != http.StatusOK {
		t.Fatalf("reads must stay open, /info returned %d: %s", status, body)
	}

	if status, _ := n.postWithToken("/generate", map[string]int{"n": 1}, ""); status != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated write returned %d, want 401", status)
	}
	if status, _ := n.postWithToken("/generate", map[string]int{"n": 1}, "wrong-token"); status != http.StatusUnauthorized {
		t.Fatalf("a write with the wrong token returned %d, want 401", status)
	}
	if h := n.height(); h != 0 {
		t.Fatalf("a rejected write still mined: height %d", h)
	}

	n.generate(1) // node.token is set, so this carries the header
	if h := n.height(); h != 1 {
		t.Fatalf("an authenticated write did not take effect: height %d", h)
	}
}

// The live event stream should push a block event as soon as one is mined.
func TestEventStreamPushesBlocks(t *testing.T) {
	n := startNode(t, nodeOpts{name: "events"})

	resp, err := http.Get(n.url("/events"))
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("event stream Content-Type is %q", ct)
	}

	lines := make(chan string, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			nRead, err := resp.Body.Read(buf)
			if nRead > 0 {
				lines <- string(buf[:nRead])
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()

	n.generate(1)

	deadline := time.After(20 * time.Second)
	for {
		select {
		case chunk, ok := <-lines:
			if !ok {
				t.Fatal("event stream closed before a block arrived")
			}
			if strings.Contains(chunk, "event: block") && strings.Contains(chunk, "data:") {
				return
			}
		case <-deadline:
			t.Fatal("no block event within 20s")
		}
	}
}

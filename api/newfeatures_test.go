package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/api"
	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

// serverWith builds a node with a specific config (funded and matured) and
// serves it, for the endpoints that only exist under one.
func serverWith(t *testing.T, cfg node.Config, addrIndex bool) (*httptest.Server, *node.Node, *core.Blockchain, *wallet.Wallet) {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	chain := core.NewBlockchain()
	if addrIndex {
		chain.EnableAddressIndex()
	}
	mineOnto(t, chain, w.Address(), nil)
	mature(t, chain)
	cfg.ListenAddr = ":0"
	n := node.New(cfg, chain, core.NewMempool(), w)
	t.Cleanup(n.Shutdown)
	srv := httptest.NewServer(api.New(n).Handler())
	t.Cleanup(srv.Close)
	return srv, n, chain, w
}

// onRegtest switches the process onto regtest (where a faucet is permitted) for
// one test.
func onRegtest(t *testing.T) {
	t.Helper()
	prev, prevRetarget := core.NetworkName(), core.NoRetarget
	if err := core.SetNetwork(core.RegTest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = core.SetNetwork(prev)
		core.NoRetarget = prevRetarget
	})
}

func TestInfoReportsNetworkAndFeatures(t *testing.T) {
	srv, _, _ := testServer(t)
	info := getObj(t, srv.URL+"/info")
	if info["network"] != core.NetworkName() {
		t.Fatalf("network = %v, want %q", info["network"], core.NetworkName())
	}
	if info["address_index"] != false {
		t.Fatalf("address_index = %v, want false by default", info["address_index"])
	}
	if info["faucet"] != false {
		t.Fatalf("faucet = %v, want false by default", info["faucet"])
	}
}

func TestAddressHistoryNeedsTheIndex(t *testing.T) {
	srv, _, w := fundedServer(t)
	resp, err := http.Get(srv.URL + "/address/" + w.Address() + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the node has no address index", resp.StatusCode)
	}
}

func TestAddressHistoryListsPaymentsBothWays(t *testing.T) {
	srv, _, chain, w := serverWith(t, node.Config{}, true)
	bob, _ := wallet.New()
	for i := uint64(0); i < 3; i++ {
		tx := core.Transaction{From: w.Address(), To: bob.Address(), Amount: 1000, Fee: 1_000_000, Nonce: i}
		if err := tx.Sign(w); err != nil {
			t.Fatal(err)
		}
		mineOnto(t, chain, w.Address(), []core.Transaction{tx})
	}

	hist := getObj(t, srv.URL+"/address/"+bob.Address()+"/history")
	if hist["total"].(float64) != 3 {
		t.Fatalf("total = %v, want 3", hist["total"])
	}
	entries := hist["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	first := entries[0].(map[string]any)
	if first["confirmations"].(float64) < 1 {
		t.Fatalf("entry has no confirmations: %v", first)
	}

	// The sender sees the same payments, plus the coinbases that funded it.
	senderHist := getObj(t, srv.URL+"/address/"+w.Address()+"/history")
	if senderHist["total"].(float64) < 3 {
		t.Fatalf("sender total = %v, want at least 3", senderHist["total"])
	}
	// Paging: a limit bounds the page without changing the total.
	page := getObj(t, srv.URL+"/address/"+bob.Address()+"/history?limit=2")
	if len(page["entries"].([]any)) != 2 {
		t.Fatalf("limited page = %d entries, want 2", len(page["entries"].([]any)))
	}
	if page["total"].(float64) != 3 {
		t.Fatalf("limited page total = %v, want the unpaged 3", page["total"])
	}
}

func TestBlockTemplateCarriesShareTarget(t *testing.T) {
	srv, n, _, w := serverWith(t, node.Config{}, false)
	tmpl := getObj(t, srv.URL+"/blocktemplate?address="+w.Address())
	if tmpl["state_root"] == nil || tmpl["bits"] == nil {
		t.Fatalf("template is missing block fields: %v", tmpl)
	}
	shareBits := uint32(tmpl["share_bits"].(float64))
	blockBits := uint32(tmpl["bits"].(float64))
	if shareBits == 0 || core.CompactToBig(shareBits).Cmp(core.CompactToBig(blockBits)) <= 0 {
		t.Fatalf("share_bits %#x is not easier than bits %#x", shareBits, blockBits)
	}
	if uint32(tmpl["share_factor"].(float64)) != n.ShareFactor() {
		t.Fatalf("share_factor = %v, want %d", tmpl["share_factor"], n.ShareFactor())
	}
}

// A long-poll request whose `prev` is already stale must answer at once rather
// than parking a miner for the timeout.
func TestBlockTemplateLongPollReturnsOnStalePrev(t *testing.T) {
	srv, _, _, w := serverWith(t, node.Config{}, false)
	start := time.Now()
	tmpl := getObj(t, srv.URL+"/blocktemplate?address="+w.Address()+"&longpoll=1&prev=notthecurrenttip&timeout=30")
	if tmpl["hash"] == nil && tmpl["error"] != nil {
		t.Fatalf("long poll failed: %v", tmpl["error"])
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("long poll with a stale prev took %s; it should return immediately", elapsed)
	}
}

func TestSubmitShareAndLedger(t *testing.T) {
	srv, n, _, w := serverWith(t, node.Config{}, false)
	tmpl, err := n.BuildTemplate(w.Address())
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	shareBits := core.ShareBits(tmpl.Bits, n.ShareFactor())
	tmpl.MerkleRoot = core.MerkleRoot(tmpl.Transactions)
	found := false
	for i := 0; i < 1_000_000; i++ {
		tmpl.Hash = tmpl.ComputeHash()
		if core.MeetsShareTarget(tmpl.Hash, shareBits) {
			found = true
			break
		}
		tmpl.Nonce++
	}
	if !found {
		t.Fatal("no share found")
	}

	res, code := postObj(t, srv.URL+"/submitshare", tmpl)
	if code != http.StatusOK || res["accepted"] != true {
		t.Fatalf("submit share: status %d, body %v", code, res)
	}
	ledger := getObj(t, srv.URL+"/shares")
	if ledger["accepted"].(float64) != 1 {
		t.Fatalf("ledger accepted = %v, want 1", ledger["accepted"])
	}
	miners := ledger["miners"].([]any)
	if len(miners) != 1 || miners[0].(map[string]any)["address"] != w.Address() {
		t.Fatalf("ledger miners = %v, want one row for the submitting address", miners)
	}
}

func TestSubmitShareRejectsJunk(t *testing.T) {
	srv, _, _, _ := serverWith(t, node.Config{}, false)
	_, code := postObj(t, srv.URL+"/submitshare", core.Block{Index: 99})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a share that builds on nothing", code)
	}
}

func TestFaucetPaysAndCoolsDown(t *testing.T) {
	onRegtest(t)
	srv, _, chain, _ := serverWith(t, node.Config{Faucet: true, FaucetCooldown: time.Hour}, false)
	recipient, _ := wallet.New()

	res, code := postObj(t, srv.URL+"/faucet", map[string]string{"address": recipient.Address()})
	if code != http.StatusOK {
		t.Fatalf("faucet: status %d, body %v", code, res)
	}
	if res["to"] != recipient.Address() {
		t.Fatalf("payout went to %v, want %s", res["to"], recipient.Address())
	}
	// A second request from the same client is refused by the cooldown.
	other, _ := wallet.New()
	_, code = postObj(t, srv.URL+"/faucet", map[string]string{"address": other.Address()})
	if code != http.StatusBadRequest {
		t.Fatalf("second request status = %d, want 400 (cooldown)", code)
	}
	_ = chain
}

func TestFaucetForbiddenWhenDisabled(t *testing.T) {
	srv, _, _, _ := serverWith(t, node.Config{}, false)
	recipient, _ := wallet.New()
	_, code := postObj(t, srv.URL+"/faucet", map[string]string{"address": recipient.Address()})
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when no faucet is configured", code)
	}
}

func TestVaultAddressEndpoint(t *testing.T) {
	srv, _, _ := testServer(t)
	hot, _ := wallet.New()
	cold, _ := wallet.New()
	res, code := postObj(t, srv.URL+"/vault/address", map[string]any{
		"hot": hot.PublicKeyHex(), "cold": cold.PublicKeyHex(), "unlock": 500,
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, res)
	}
	want, err := wallet.VaultAddress(hot.PublicKeyHex(), cold.PublicKeyHex(), 500)
	if err != nil {
		t.Fatal(err)
	}
	if res["address"] != want {
		t.Fatalf("address = %v, want %s", res["address"], want)
	}
	if _, code := postObj(t, srv.URL+"/vault/address", map[string]any{"hot": "zz", "cold": "yy"}); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed script", code)
	}
}

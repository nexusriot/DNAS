package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexusriot/DNAS/api"
	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

// spendableServer serves a node whose wallet holds a matured coinbase, and hands
// back the mempool so a test can stage a pending transaction.
func spendableServer(t *testing.T) (*httptest.Server, *core.Blockchain, *core.Mempool, *wallet.Wallet) {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	chain := core.NewBlockchain()
	mineOnto(t, chain, w.Address(), nil)
	sink, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < core.CoinbaseMaturity; i++ {
		mineOnto(t, chain, sink.Address(), nil)
	}
	mp := core.NewMempool()
	n := node.New(node.Config{ListenAddr: ":0"}, chain, mp, w)
	srv := httptest.NewServer(api.New(n).Handler())
	t.Cleanup(srv.Close)
	return srv, chain, mp, w
}

func signedPayment(t *testing.T, from *wallet.Wallet, to string, nonce uint64) core.Transaction {
	t.Helper()
	tx := core.Transaction{From: from.Address(), To: to, Amount: core.Coin, Fee: 1_000_000, Nonce: nonce}
	if err := tx.Sign(from); err != nil {
		t.Fatal(err)
	}
	return tx
}

func statusOf(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestTxByHashConfirmed(t *testing.T) {
	srv, chain, _, w := spendableServer(t)
	bob, _ := wallet.New()
	tx := signedPayment(t, w, bob.Address(), 0)
	mineOnto(t, chain, w.Address(), []core.Transaction{tx})
	mineOnto(t, chain, w.Address(), nil) // one more, so confirmations > 1

	got := getObj(t, srv.URL+"/tx/"+tx.Hash())
	if got["status"] != "confirmed" {
		t.Fatalf("status = %v, want confirmed", got["status"])
	}
	if uint64(got["confirmations"].(float64)) != 2 {
		t.Errorf("confirmations = %v, want 2", got["confirmations"])
	}
	if uint64(got["height"].(float64)) != chain.Height()-1 {
		t.Errorf("height = %v, want %d", got["height"], chain.Height()-1)
	}
	if got["index"].(float64) != 1 {
		t.Errorf("index = %v, want 1 (first non-coinbase)", got["index"])
	}
	if b, ok := chain.BlockAt(chain.Height() - 1); !ok || got["block_hash"] != b.Hash {
		t.Errorf("block_hash = %v, want %v", got["block_hash"], b.Hash)
	}
	inner, _ := got["tx"].(map[string]any)
	if inner == nil || inner["to"] != bob.Address() {
		t.Errorf("returned transaction does not pay bob: %v", got["tx"])
	}
}

func TestTxByHashPending(t *testing.T) {
	srv, _, mp, w := spendableServer(t)
	bob, _ := wallet.New()
	tx := signedPayment(t, w, bob.Address(), 0)
	if _, err := mp.Add(tx); err != nil {
		t.Fatal(err)
	}

	got := getObj(t, srv.URL+"/tx/"+tx.Hash())
	if got["status"] != "pending" {
		t.Fatalf("status = %v, want pending", got["status"])
	}
	if got["confirmations"].(float64) != 0 {
		t.Errorf("confirmations = %v, want 0", got["confirmations"])
	}
	if got["hash"] != tx.Hash() {
		t.Errorf("hash = %v, want %v", got["hash"], tx.Hash())
	}
}

func TestTxByHashUnknown(t *testing.T) {
	srv, _, _, _ := spendableServer(t)
	if code := statusOf(t, srv.URL+"/tx/0000000000000000000000000000000000000000000000000000000000000000"); code != http.StatusNotFound {
		t.Errorf("unknown transaction: status %d, want 404", code)
	}
	if code := statusOf(t, srv.URL+"/tx/"); code != http.StatusBadRequest {
		t.Errorf("empty hash: status %d, want 400", code)
	}
}

// The lookup route is registered next to the submit endpoint; POST /tx must keep
// working exactly as before.
func TestSubmitTxStillWorksAlongsideLookup(t *testing.T) {
	srv, _, mp, w := spendableServer(t)
	bob, _ := wallet.New()
	tx := signedPayment(t, w, bob.Address(), 0)
	if _, code := postObj(t, srv.URL+"/tx", tx); code != http.StatusOK {
		t.Fatalf("POST /tx returned %d, want 200", code)
	}
	if _, ok := mp.Get(tx.Hash()); !ok {
		t.Fatal("submitted transaction did not reach the mempool")
	}
	// And it is immediately visible through the lookup route as pending.
	if got := getObj(t, srv.URL+"/tx/"+tx.Hash()); got["status"] != "pending" {
		t.Fatalf("status = %v, want pending", got["status"])
	}
}

func TestSupplyEndpoint(t *testing.T) {
	srv, chain, _, w := spendableServer(t)

	before := getObj(t, srv.URL+"/supply")
	if before["consistent"] != true {
		t.Fatalf("supply is inconsistent on a freshly mined chain: %v", before)
	}
	if uint64(before["minted"].(float64)) != core.CumulativeSubsidy(chain.Height()) {
		t.Errorf("minted = %v, want %d", before["minted"], core.CumulativeSubsidy(chain.Height()))
	}
	if uint64(before["burned"].(float64)) != 0 {
		t.Errorf("burned = %v, want 0 before any transaction", before["burned"])
	}

	bob, _ := wallet.New()
	tx := signedPayment(t, w, bob.Address(), 0)
	mineOnto(t, chain, w.Address(), []core.Transaction{tx})

	after := getObj(t, srv.URL+"/supply")
	if after["consistent"] != true {
		t.Fatalf("supply is inconsistent after a transaction: %v", after)
	}
	burned := uint64(after["burned"].(float64))
	if burned == 0 {
		t.Error("a fee-paying transaction burned nothing")
	}
	minted, circulating := uint64(after["minted"].(float64)), uint64(after["circulating"].(float64))
	if minted-burned != circulating {
		t.Errorf("minted %d − burned %d != circulating %d", minted, burned, circulating)
	}
	if s, _ := after["circulating_fmt"].(string); s == "" {
		t.Error("supply is missing its formatted amounts")
	}
}

// An asset balance is an opaque id without somewhere to look it up, so the
// registry has to be reachable over the API too.
func TestAssetEndpoints(t *testing.T) {
	srv, chain, _, w := spendableServer(t)

	if list := getArr(t, srv.URL+"/assets"); len(list) != 0 {
		t.Fatalf("a chain with no issuances lists %d assets", len(list))
	}
	issue := core.Transaction{From: w.Address(), Fee: 1_000_000, Nonce: 0,
		Issue: &core.AssetIssue{Ticker: "GOLD", Supply: 1000}}
	if err := issue.Sign(w); err != nil {
		t.Fatal(err)
	}
	mineOnto(t, chain, w.Address(), []core.Transaction{issue})
	id := core.AssetID(w.Address(), "GOLD", 0)

	list := getArr(t, srv.URL+"/assets")
	if len(list) != 1 {
		t.Fatalf("/assets returned %d entries, want 1", len(list))
	}
	entry := list[0].(map[string]any)
	if entry["ticker"] != "GOLD" || entry["id"] != id {
		t.Fatalf("/assets entry = %+v", entry)
	}
	if byTicker := getArr(t, srv.URL+"/assets?ticker=GOLD"); len(byTicker) != 1 {
		t.Fatalf("?ticker=GOLD returned %d entries", len(byTicker))
	}
	if none := getArr(t, srv.URL+"/assets?ticker=SILVER"); len(none) != 0 {
		t.Fatalf("?ticker=SILVER returned %d entries", len(none))
	}

	one := getObj(t, srv.URL+"/asset/"+id)
	asset := one["asset"].(map[string]any)
	if asset["issuer"] != w.Address() || asset["supply"].(float64) != 1000 {
		t.Fatalf("/asset/{id} = %+v", asset)
	}
	holders := one["holders"].([]any)
	if len(holders) != 1 {
		t.Fatalf("holders = %+v", holders)
	}
	// The held total is served next to the issued supply so a client can check
	// conservation rather than trust the supply figure.
	if one["held"].(float64) != 1000 {
		t.Fatalf("held = %v, want the whole 1000-unit supply", one["held"])
	}
	if code := statusOf(t, srv.URL+"/asset/toknotreal"); code != http.StatusNotFound {
		t.Fatalf("an unknown asset id returned %d, want 404", code)
	}
	if code := statusOf(t, srv.URL+"/asset/"); code != http.StatusBadRequest {
		t.Fatalf("an empty asset id returned %d, want 400", code)
	}
}

// /account grew the formatted balance because the explorer's address search
// rendered "balance undefined": every client would otherwise have to divide by
// the coin unit itself, and every one of them could get the decimals wrong.
func TestAccountReportsAFormattedBalance(t *testing.T) {
	srv, _, _, w := spendableServer(t)
	acc := getObj(t, srv.URL+"/account/"+w.Address())
	raw, ok := acc["balance"].(float64)
	if !ok || raw == 0 {
		t.Fatalf("balance = %v", acc["balance"])
	}
	fmtd, ok := acc["balance_fmt"].(string)
	if !ok || fmtd == "" {
		t.Fatal("/account does not report a formatted balance")
	}
	// The two must agree, or a client showing one and checking the other would be
	// showing a different number than it is acting on.
	if want := core.FormatAmount(uint64(raw)); fmtd != want {
		t.Fatalf("balance_fmt = %q, want %q", fmtd, want)
	}
	// An account nobody has touched answers with zeroes rather than 404, which is
	// what lets a lookup say "nothing here" instead of failing.
	empty := getObj(t, srv.URL+"/account/dnasnobodyatall")
	if empty["balance_fmt"] != core.FormatAmount(0) {
		t.Fatalf("an unknown account reports %v", empty["balance_fmt"])
	}
}

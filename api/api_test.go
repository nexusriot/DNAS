package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/api"
	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

// mineOnto mines a block (coinbase to miner + txs) onto chain.
func mineOnto(t *testing.T, chain *core.Blockchain, miner string, txs []core.Transaction) {
	t.Helper()
	tip := chain.Tip()
	var fees uint64
	for _, tx := range txs {
		fees += tx.Fee
	}
	cb := core.NewCoinbase(miner, core.BlockReward(tip.Index+1)+fees)
	b := core.Block{
		Index:        tip.Index + 1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: append([]core.Transaction{cb}, txs...),
		PrevHash:     tip.Hash,
		Difficulty:   chain.NextDifficulty(),
	}
	mined, ok := core.Mine(b, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	if err := chain.AddBlock(mined); err != nil {
		t.Fatalf("add block: %v", err)
	}
}

// testServer builds a node whose wallet already holds one block reward, and
// serves its API via httptest.
func testServer(t *testing.T) (*httptest.Server, *core.Blockchain, *wallet.Wallet) {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	chain := core.NewBlockchain()
	mineOnto(t, chain, w.Address(), nil) // fund the wallet with block 1's reward
	n := node.New(node.Config{ListenAddr: ":0"}, chain, core.NewMempool(), w)
	srv := httptest.NewServer(api.New(n).Handler())
	t.Cleanup(srv.Close)
	return srv, chain, w
}

func getObj(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return m
}

func getArr(t *testing.T, url string) []any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var a []any
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return a
}

func TestInfoBalanceAccount(t *testing.T) {
	srv, _, w := testServer(t)

	info := getObj(t, srv.URL+"/info")
	if info["height"].(float64) != 1 {
		t.Errorf("height = %v, want 1", info["height"])
	}
	if s, _ := info["work"].(string); s == "" {
		t.Error("info is missing cumulative work")
	}

	bal := getObj(t, srv.URL+"/balance/"+w.Address())
	if uint64(bal["balance"].(float64)) != core.BlockReward(1) {
		t.Errorf("balance = %v, want %d", bal["balance"], core.BlockReward(1))
	}

	acc := getObj(t, srv.URL+"/account/"+w.Address())
	if acc["nonce"].(float64) != 0 {
		t.Errorf("nonce = %v, want 0", acc["nonce"])
	}
}

func TestInfoExposesMinRelayFee(t *testing.T) {
	w, _ := wallet.New()
	chain := core.NewBlockchain()
	mp := core.NewMempoolWithPolicy(5000, 1000) // base relay fee 1000
	n := node.New(node.Config{ListenAddr: ":0"}, chain, mp, w)
	srv := httptest.NewServer(api.New(n).Handler())
	t.Cleanup(srv.Close)

	if got := getObj(t, srv.URL+"/info")["min_relay_fee"]; uint64(got.(float64)) != 1000 {
		t.Errorf("min_relay_fee = %v, want 1000", got)
	}
}

func postObj(t *testing.T, url string, body any) (map[string]any, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return m, resp.StatusCode
}

func TestMultisigAddressEndpoint(t *testing.T) {
	srv, _, _ := testServer(t)
	var pks []string
	for i := 0; i < 3; i++ {
		w, _ := wallet.New()
		pks = append(pks, w.PublicKeyHex())
	}

	obj, code := postObj(t, srv.URL+"/multisig/address", map[string]any{"threshold": 2, "pubkeys": pks})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", code, obj)
	}
	want, err := wallet.MultisigAddress(2, pks)
	if err != nil {
		t.Fatal(err)
	}
	if got := obj["address"].(string); got != want {
		t.Fatalf("address = %s, want %s", got, want)
	}
	if err := wallet.ValidateAddress(obj["address"].(string)); err != nil {
		t.Fatalf("endpoint returned an invalid address: %v", err)
	}

	// A threshold larger than the key count is a 400.
	if _, code := postObj(t, srv.URL+"/multisig/address", map[string]any{"threshold": 9, "pubkeys": pks}); code != http.StatusBadRequest {
		t.Fatalf("bad threshold status = %d, want 400", code)
	}
}

func TestWalletHDEndpoint(t *testing.T) {
	srv, _, _ := testServer(t)

	// Generate a fresh HD wallet (empty mnemonic mints one).
	gen, code := postObj(t, srv.URL+"/wallet/hd", map[string]any{"count": 3})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", code, gen)
	}
	mnemonic := gen["mnemonic"].(string)
	if mnemonic == "" {
		t.Fatal("expected a generated mnemonic")
	}
	first := gen["addresses"].([]any)
	if len(first) != 3 {
		t.Fatalf("got %d addresses, want 3", len(first))
	}
	for _, a := range first {
		if err := wallet.ValidateAddress(a.(string)); err != nil {
			t.Fatalf("derived address invalid: %v", err)
		}
	}

	// Restoring with the same mnemonic derives identical addresses.
	res, _ := postObj(t, srv.URL+"/wallet/hd", map[string]any{"mnemonic": mnemonic, "count": 3})
	second := res["addresses"].([]any)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("address %d not deterministic: %v vs %v", i, first[i], second[i])
		}
	}

	// A malformed mnemonic is a 400.
	if _, code := postObj(t, srv.URL+"/wallet/hd", map[string]any{"mnemonic": "not a real phrase", "count": 1}); code != http.StatusBadRequest {
		t.Fatalf("bad mnemonic status = %d, want 400", code)
	}
}

func TestAddressAndPeers(t *testing.T) {
	srv, _, w := testServer(t)
	if got := getObj(t, srv.URL+"/address")["address"]; got != w.Address() {
		t.Errorf("address = %v, want %s", got, w.Address())
	}
	if peers := getArr(t, srv.URL+"/peers"); len(peers) != 0 {
		t.Errorf("expected no peers, got %v", peers)
	}
}

func TestSendAddsToMempool(t *testing.T) {
	srv, _, _ := testServer(t)
	bob, _ := wallet.New()
	body := fmt.Sprintf(`{"to":%q,"amount":100000000,"fee":1000000}`, bob.Address())
	resp, err := http.Post(srv.URL+"/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["hash"] == nil {
		t.Fatal("response missing tx hash")
	}
	if mp := getArr(t, srv.URL+"/mempool"); len(mp) != 1 {
		t.Fatalf("mempool size = %d, want 1", len(mp))
	}
}

func TestSendRejectsExpired(t *testing.T) {
	srv, _, _ := testServer(t) // chain height 1, next block height 2
	bob, _ := wallet.New()
	body := fmt.Sprintf(`{"to":%q,"amount":100000000,"expiry":1}`, bob.Address())
	resp, err := http.Post(srv.URL+"/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for expired tx", resp.StatusCode)
	}
}

func TestSendRejectsInvalidRecipient(t *testing.T) {
	srv, _, _ := testServer(t)
	// A mistyped address (bad checksum) must be refused before signing.
	body := `{"to":"dnasdeadbeef","amount":100000000}`
	resp, err := http.Post(srv.URL+"/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an invalid recipient", resp.StatusCode)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	srv, _, _ := testServer(t)
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, metric := range []string{"dnas_height", "dnas_difficulty", "dnas_mempool_size", "dnas_peers", "dnas_mining"} {
		if !strings.Contains(string(body), metric) {
			t.Errorf("/metrics missing %q", metric)
		}
	}
}

func TestMineToggleEndpoint(t *testing.T) {
	srv, _, _ := testServer(t)
	// Initially off.
	if getObj(t, srv.URL+"/info")["mining"] != false {
		t.Fatal("mining should start off")
	}
	// Turn it on.
	resp, err := http.Post(srv.URL+"/mine", "application/json", strings.NewReader(`{"on":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if getObj(t, srv.URL+"/info")["mining"] != true {
		t.Fatal("mining should be on after POST /mine {on:true}")
	}
	// And off again.
	r2, _ := http.Post(srv.URL+"/mine", "application/json", strings.NewReader(`{"on":false}`))
	r2.Body.Close()
	if getObj(t, srv.URL+"/info")["mining"] != false {
		t.Fatal("mining should be off after POST /mine {on:false}")
	}
}

func TestSendMethodNotAllowed(t *testing.T) {
	srv, _, _ := testServer(t)
	resp, err := http.Get(srv.URL + "/send")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestSubmitSignedTx(t *testing.T) {
	srv, _, w := testServer(t)
	bob, _ := wallet.New()
	tx := core.Transaction{From: w.Address(), To: bob.Address(), Amount: core.Coin, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(tx)
	resp, err := http.Post(srv.URL+"/tx", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if mp := getArr(t, srv.URL+"/mempool"); len(mp) != 1 {
		t.Fatalf("mempool size = %d, want 1", len(mp))
	}
}

func TestHeadersEndpoint(t *testing.T) {
	srv, chain, w := testServer(t)
	mineOnto(t, chain, w.Address(), nil) // now genesis + 2 blocks

	var hs []core.Header
	resp, err := http.Get(srv.URL + "/headers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&hs); err != nil {
		t.Fatal(err)
	}
	if len(hs) != chain.Len() {
		t.Fatalf("got %d headers, want %d", len(hs), chain.Len())
	}

	// A bad height is 400; an out-of-range height is 404.
	if r, _ := http.Get(srv.URL + "/header/abc"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("/header/abc status = %d, want 400", r.StatusCode)
	}
	if r, _ := http.Get(srv.URL + "/header/9999"); r.StatusCode != http.StatusNotFound {
		t.Errorf("/header/9999 status = %d, want 404", r.StatusCode)
	}
}

// TestSPVProofEndpoint confirms /proof returns a proof that verifies against the
// merkle root exposed by /header — the light-client round trip.
func TestSPVProofEndpoint(t *testing.T) {
	srv, chain, w := testServer(t)
	bob, _ := wallet.New()
	sink, _ := wallet.New()
	for i := 0; i < core.CoinbaseMaturity; i++ { // let w's coinbase mature
		mineOnto(t, chain, sink.Address(), nil)
	}
	tx := core.Transaction{From: w.Address(), To: bob.Address(), Amount: 5 * core.Coin, Fee: core.Coin, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	mineOnto(t, chain, w.Address(), []core.Transaction{tx})

	pr := getObj(t, srv.URL+"/proof/"+tx.Hash())
	if pr["found"] != true {
		t.Fatalf("proof not found: %v", pr)
	}
	idx := uint64(pr["block_index"].(float64))
	root := getObj(t, fmt.Sprintf("%s/header/%d", srv.URL, idx))["merkle_root"].(string)

	var steps []core.MerkleProofStep
	for _, s := range pr["proof"].([]any) {
		m := s.(map[string]any)
		steps = append(steps, core.MerkleProofStep{Hash: m["hash"].(string), Right: m["right"].(bool)})
	}
	if !core.VerifyMerkleProof(tx.Hash(), root, steps) {
		t.Fatal("proof from /proof did not verify against root from /header")
	}

	if r, _ := http.Get(srv.URL + "/proof/deadbeef"); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown tx status = %d, want 404", r.StatusCode)
	}
}

func TestExplorerServedAtRoot(t *testing.T) {
	srv, _, _ := testServer(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "DNAS explorer") {
		t.Error("root page does not look like the explorer")
	}
	// An unknown path under the catch-all returns 404.
	if r, _ := http.Get(srv.URL + "/nope"); r.StatusCode != http.StatusNotFound {
		t.Errorf("/nope status = %d, want 404", r.StatusCode)
	}
}

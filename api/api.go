// Package api exposes a small read/write HTTP interface to a running node.
package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

//go:embed explorer.html
var explorerHTML []byte

// Server serves the HTTP API for a node.
type Server struct{ node *node.Node }

// New returns an API server bound to n.
func New(n *node.Node) *Server { return &Server{node: n} }

// Handler builds the HTTP routing for the API. Exposed so it can be served by
// Start or driven directly in tests via httptest.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", s.info)
	mux.HandleFunc("/chain", s.chain)
	mux.HandleFunc("/balance/", s.balance)
	mux.HandleFunc("/account/", s.account)
	mux.HandleFunc("/mempool", s.mempool)
	mux.HandleFunc("/peers", s.peers)
	mux.HandleFunc("/address", s.address)
	mux.HandleFunc("/tx", s.submitTx)     // POST a fully signed transaction
	mux.HandleFunc("/send", s.send)       // POST {to, amount, fee, expiry?, nonce?}; signed by node wallet
	mux.HandleFunc("/mine", s.mine)       // POST {on: bool}; toggle mining at runtime
	mux.HandleFunc("/metrics", s.metrics) // Prometheus-style metrics
	// Stateless wallet helpers (no node state touched; localhost/toy use).
	mux.HandleFunc("/multisig/address", s.multisigAddress) // POST {threshold, pubkeys[]}
	mux.HandleFunc("/wallet/hd", s.walletHD)               // POST {mnemonic?, passphrase?, count?}
	// SPV / light-client endpoints.
	mux.HandleFunc("/headers", s.headers) // all block headers
	mux.HandleFunc("/header/", s.header)  // one header by height
	mux.HandleFunc("/proof/", s.proof)    // inclusion proof for a tx hash
	mux.HandleFunc("/", s.explorer)       // web block explorer (catch-all)
	return mux
}

// explorer serves the self-contained web block explorer at the root path.
func (s *Server) explorer(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(explorerHTML)
}

// Start blocks serving the API on addr.
func (s *Server) Start(addr string) {
	log.Printf("API listening on http://%s", addr)
	if err := http.ListenAndServe(addr, s.Handler()); err != nil {
		log.Fatalf("api: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	tip := s.node.Chain().Tip()
	writeJSON(w, http.StatusOK, map[string]any{
		"height":          tip.Index,
		"tip":             tip.Hash,
		"next_difficulty": s.node.Chain().NextDifficulty(),
		"work":            s.node.Chain().Work().String(),
		"mempool":         s.node.Mempool().Size(),
		"min_relay_fee":   s.node.Mempool().MinFee(),
		"peers":           s.node.PeerAddrs(),
		"mining":          s.node.Mining(),
	})
}

// metrics exposes node stats in the Prometheus text exposition format.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	tip := s.node.Chain().Tip()
	mining := 0
	if s.node.Mining() {
		mining = 1
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	gauge := func(name, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, v)
	}
	gauge("dnas_height", "Current chain height.", tip.Index)
	gauge("dnas_difficulty", "Difficulty of the next block.", s.node.Chain().NextDifficulty())
	gauge("dnas_mempool_size", "Pending transactions in the mempool.", s.node.Mempool().Size())
	gauge("dnas_min_relay_fee", "Current dynamic minimum relay fee (base units).", s.node.Mempool().MinFee())
	gauge("dnas_peers", "Connected peers.", len(s.node.PeerAddrs()))
	gauge("dnas_mining", "1 if mining is active, else 0.", mining)
}

// mine toggles the node's miner at runtime: POST {"on": true|false}.
func (s *Server) mine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.node.Wallet() == nil {
		writeErr(w, http.StatusBadRequest, "node has no wallet to mine to")
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.node.SetMining(req.On)
	writeJSON(w, http.StatusOK, map[string]bool{"mining": s.node.Mining()})
}

func (s *Server) chain(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Chain().Blocks())
}

func (s *Server) balance(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimPrefix(r.URL.Path, "/balance/")
	acc := s.node.Chain().Account(addr)
	writeJSON(w, http.StatusOK, map[string]any{
		"address":     addr,
		"balance":     acc.Balance,
		"balance_fmt": core.FormatAmount(acc.Balance),
	})
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimPrefix(r.URL.Path, "/account/")
	acc := s.node.Chain().Account(addr)
	writeJSON(w, http.StatusOK, map[string]any{
		"address": addr,
		"balance": acc.Balance,
		"nonce":   acc.Nonce,
	})
}

func (s *Server) mempool(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Mempool().All())
}

func (s *Server) peers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.PeerAddrs())
}

func (s *Server) address(w http.ResponseWriter, r *http.Request) {
	wal := s.node.Wallet()
	if wal == nil {
		writeErr(w, http.StatusNotFound, "node has no wallet")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"address": wal.Address()})
}

// submitTx accepts a fully signed transaction from an external wallet.
func (s *Server) submitTx(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var tx core.Transaction
	if err := json.NewDecoder(r.Body).Decode(&tx); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.node.SubmitTx(tx); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"hash": tx.Hash()})
}

// send builds, signs (with the node's wallet) and broadcasts a transaction.
func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	wal := s.node.Wallet()
	if wal == nil {
		writeErr(w, http.StatusBadRequest, "node has no wallet")
		return
	}
	// Nonce is optional: omit it to auto-select the next one, or set it explicitly
	// (with a higher fee) to fee-bump a stuck transaction. Expiry/LockUntil bound
	// the height window in which the tx is valid; Memo is optional data.
	var req struct {
		To        string  `json:"to"`
		Amount    uint64  `json:"amount"`
		Fee       uint64  `json:"fee"`
		Expiry    uint64  `json:"expiry"`
		LockUntil uint64  `json:"lock_until"`
		Memo      string  `json:"memo"`
		Nonce     *uint64 `json:"nonce"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := wallet.ValidateAddress(req.To); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid recipient: "+err.Error())
		return
	}
	if len(req.Memo) > core.MaxMemoBytes {
		writeErr(w, http.StatusBadRequest, "memo too long")
		return
	}
	nonce := s.node.NextNonce(wal.Address())
	if req.Nonce != nil {
		nonce = *req.Nonce
	}
	tx := core.Transaction{
		From:      wal.Address(),
		To:        req.To,
		Amount:    req.Amount,
		Fee:       req.Fee,
		Nonce:     nonce,
		Expiry:    req.Expiry,
		LockUntil: req.LockUntil,
		Memo:      req.Memo,
	}
	if err := tx.Sign(wal); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.node.SubmitTx(tx); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hash": tx.Hash(), "nonce": tx.Nonce})
}

// multisigAddress derives an M-of-N multisig address from a threshold and member
// public keys. Stateless: it computes and returns an address without touching
// node state or holding any secret.
func (s *Server) multisigAddress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Threshold int      `json:"threshold"`
		PubKeys   []string `json:"pubkeys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	addr, err := wallet.MultisigAddress(req.Threshold, req.PubKeys)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"threshold": req.Threshold,
		"n":         len(req.PubKeys),
		"address":   addr,
	})
}

// walletHD generates or restores a BIP39 HD wallet and returns the mnemonic plus
// the first `count` derived addresses. With an empty mnemonic it mints a fresh
// 12-word phrase; with one supplied it restores. Stateless: nothing is persisted
// on the node — the caller must save the returned mnemonic to keep the keys.
func (s *Server) walletHD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Mnemonic   string `json:"mnemonic"`
		Passphrase string `json:"passphrase"`
		Count      int    `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	switch {
	case req.Count <= 0:
		req.Count = 5
	case req.Count > 100:
		req.Count = 100
	}
	mnemonic := strings.TrimSpace(req.Mnemonic)
	var hd *wallet.HDWallet
	var err error
	if mnemonic == "" {
		mnemonic, hd, err = wallet.NewHD(128, req.Passphrase) // 128 bits -> 12 words
	} else {
		hd, err = wallet.HDFromMnemonic(mnemonic, req.Passphrase)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	addrs := make([]string, req.Count)
	for i := range addrs {
		addrs[i] = hd.Derive(uint32(i)).Address()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mnemonic":  mnemonic,
		"addresses": addrs,
	})
}

// headers returns all block headers (light clients verify PoW + the hash chain
// from these alone).
func (s *Server) headers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Chain().Headers())
}

// header returns a single block header by height: /header/{index}.
func (s *Server) header(w http.ResponseWriter, r *http.Request) {
	idxStr := strings.TrimPrefix(r.URL.Path, "/header/")
	idx, err := strconv.ParseUint(idxStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid height")
		return
	}
	h, ok := s.node.Chain().HeaderAt(idx)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such block")
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// proof returns a transaction-inclusion (SPV) proof: /proof/{txhash}.
func (s *Server) proof(w http.ResponseWriter, r *http.Request) {
	txHash := strings.TrimPrefix(r.URL.Path, "/proof/")
	pr, ok := s.node.Chain().FindTxProof(txHash)
	if !ok {
		writeErr(w, http.StatusNotFound, "transaction not found")
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

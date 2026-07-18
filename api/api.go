// Package api exposes a small read/write HTTP interface to a running node.
package api

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

//go:embed explorer.html
var explorerHTML []byte

// Server serves the HTTP API for a node. When token is non-empty, the mutating
// endpoints (/send, /tx, /mine) require an "Authorization: Bearer <token>"
// header; read endpoints stay open. An empty token leaves the whole API open,
// the localhost/toy default.
type Server struct {
	node  *node.Node
	token string
}

// New returns an API server bound to n, reading the optional bearer token from
// the DNAS_API_TOKEN environment variable (kept out of flags so it doesn't leak
// into `ps`, like the wallet passphrase).
func New(n *node.Node) *Server { return NewWithToken(n, os.Getenv("DNAS_API_TOKEN")) }

// NewWithToken returns an API server that requires the given bearer token on
// write endpoints (empty disables auth).
func NewWithToken(n *node.Node, token string) *Server { return &Server{node: n, token: token} }

// AuthEnabled reports whether write endpoints require a token.
func (s *Server) AuthEnabled() bool { return s.token != "" }

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
	mux.HandleFunc("/tx", s.guard(s.submitTx))       // POST a fully signed transaction
	mux.HandleFunc("/send", s.guard(s.send))         // POST {to, amount, fee, expiry?, nonce?}; signed by node wallet
	mux.HandleFunc("/mine", s.guard(s.mine))         // POST {on: bool}; toggle mining at runtime
	mux.HandleFunc("/generate", s.guard(s.generate)) // POST {n}; regtest-only on-demand mining
	mux.HandleFunc("/estimatefee", s.estimateFee)    // GET ?blocks=N; recommended fee
	mux.HandleFunc("/metrics", s.metrics)            // Prometheus-style metrics
	mux.HandleFunc("/events", s.events)              // Server-Sent Events: live block/tx stream
	// Stateless wallet helpers (no node state touched; localhost/toy use).
	mux.HandleFunc("/multisig/address", s.multisigAddress) // POST {threshold, pubkeys[]}
	mux.HandleFunc("/htlc/address", s.htlcAddress)         // POST {hash, recipient, sender, timeout}
	mux.HandleFunc("/wallet/hd", s.walletHD)               // POST {mnemonic?, passphrase?, count?}
	// SPV / light-client endpoints.
	mux.HandleFunc("/headers", s.headers)        // all block headers
	mux.HandleFunc("/header/", s.header)         // one header by height
	mux.HandleFunc("/block/", s.block)           // one full block body by height
	mux.HandleFunc("/snapshot/", s.snapshot)     // full account state at a height (fast-sync)
	mux.HandleFunc("/proof/", s.proof)           // inclusion proof for a tx hash
	mux.HandleFunc("/cfilters", s.cfilters)      // compact block filters (all)
	mux.HandleFunc("/cfilter/", s.cfilter)       // one compact filter by height
	mux.HandleFunc("/cfheaders", s.cfheaders)    // filter-header chain
	mux.HandleFunc("/stateproof/", s.stateProof) // account balance/nonce proof vs the state root
	mux.HandleFunc("/", s.explorer)              // web block explorer (catch-all)
	return mux
}

// guard wraps a mutating handler so it requires the configured bearer token.
// With no token set it is a pass-through, so read/write behaviour is unchanged
// on an open node.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		h(w, r)
	}
}

// authorized reports whether the request carries the required bearer token. It
// is always true when no token is configured, and uses a constant-time compare
// so a wrong token can't be guessed by timing.
func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.token)) == 1
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
		"next_bits":       s.node.Chain().NextBits(),
		"next_difficulty": core.TargetDifficulty(s.node.Chain().NextBits()),
		"work":            s.node.Chain().Work().String(),
		"mempool":         s.node.Mempool().Size(),
		"min_relay_fee":   s.node.Mempool().MinFee(),
		"base_fee":        s.node.Chain().NextBaseFee(),
		"peers":           s.node.PeerAddrs(),
		"mining":          s.node.Mining(),
	})
}

// estimateFee recommends a fee RATE (base units per byte) for a transaction to
// confirm within roughly `blocks` blocks: GET /estimatefee?blocks=N (default 3).
// It combines the consensus per-byte base fee (mandatory, burned) with a tip rate
// estimated from current mempool congestion, and never returns below the node's
// relay floor. Callers multiply the returned per-byte fee by their transaction's
// size to get the total to pay. The result splits out base_fee and tip so callers
// see where the fee goes.
func (s *Server) estimateFee(w http.ResponseWriter, r *http.Request) {
	blocks := 3
	if q := r.URL.Query().Get("blocks"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "blocks must be a positive integer")
			return
		}
		if n > 100 {
			n = 100
		}
		blocks = n
	}
	baseFee := s.node.Chain().NextBaseFee()
	relayFloor := s.node.Mempool().MinFee()
	tip := s.node.Mempool().EstimateTip(baseFee, blocks*core.MaxBlockBytes)
	fee := baseFee + tip  // per-byte rate
	if fee < relayFloor { // must at least clear the relay floor to be admitted
		fee = relayFloor
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"blocks":        blocks,
		"per_byte":      true,
		"base_fee":      baseFee,
		"tip":           fee - baseFee,
		"fee":           fee,
		"fee_fmt":       core.FormatAmount(fee) + "/byte",
		"min_relay_fee": relayFloor,
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
	gauge("dnas_difficulty", "Difficulty of the next block (PowLimit/target ratio).", core.TargetDifficulty(s.node.Chain().NextBits()))
	gauge("dnas_mempool_size", "Pending transactions in the mempool.", s.node.Mempool().Size())
	gauge("dnas_min_relay_fee", "Current dynamic minimum relay fee (base units per byte).", s.node.Mempool().MinFee())
	gauge("dnas_base_fee", "Current EIP-1559 base fee for the next block (base units per byte).", s.node.Chain().NextBaseFee())
	gauge("dnas_peers", "Connected peers.", len(s.node.PeerAddrs()))
	gauge("dnas_mining", "1 if mining is active, else 0.", mining)
}

// events streams live node events (new blocks, reorgs, mempool transactions) as
// Server-Sent Events, so a browser (EventSource) or any HTTP client can react
// instantly instead of polling. The connection stays open until the client
// disconnects; a periodic comment keeps it alive through idle intermediaries.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := s.node.Subscribe()
	defer unsub()

	fmt.Fprint(w, ": connected\n\n") // open the stream immediately
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
			flusher.Flush()
		}
	}
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

// generate mines N blocks immediately (regtest only): POST {"n": N}. It is the
// on-demand block primitive for tests and demos, so they don't wait on the idle
// mining interval. Refused with 403 outside regtest.
func (s *Server) generate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.node.Regtest() {
		writeErr(w, http.StatusForbidden, "generate is only available in regtest mode (-regtest)")
		return
	}
	var req struct {
		N int `json:"n"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.N <= 0 {
		req.N = 1
	}
	hashes, err := s.node.Generate(req.N)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mined": len(hashes), "hashes": hashes})
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

// htlcAddress derives a hash-time-locked contract address from its script. Like
// multisigAddress it is stateless: it computes an address to fund, holding no
// secret and touching no node state.
func (s *Server) htlcAddress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Hash      string `json:"hash"`
		Recipient string `json:"recipient"`
		Sender    string `json:"sender"`
		Timeout   uint64 `json:"timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	addr, err := wallet.HTLCAddress(req.Hash, req.Recipient, req.Sender, req.Timeout)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"address": addr, "timeout": req.Timeout})
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

// block returns a single full block body by height: /block/{index}. A light
// client fetches only the blocks its compact filter flagged, rather than the
// whole chain.
func (s *Server) block(w http.ResponseWriter, r *http.Request) {
	idxStr := strings.TrimPrefix(r.URL.Path, "/block/")
	idx, err := strconv.ParseUint(idxStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid height")
		return
	}
	b, ok := s.node.Chain().BlockAt(idx)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such block")
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// snapshot serves the full account state as of a height (GET /snapshot/{height}),
// for fast-sync: a new node fetches this plus the PoW-verified header chain,
// checks the accounts hash to the header's committed state root, and bootstraps
// without replaying every block. The path also accepts /snapshot/latest for a
// safely-buried recent height (tip − coinbase maturity).
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	arg := strings.TrimPrefix(r.URL.Path, "/snapshot/")
	var height uint64
	if arg == "latest" || arg == "" {
		if tip := s.node.Chain().Height(); tip > core.CoinbaseMaturity {
			height = tip - core.CoinbaseMaturity
		}
	} else {
		h, err := strconv.ParseUint(arg, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid height")
			return
		}
		height = h
	}
	snap, ok := s.node.Chain().SnapshotAt(height)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such height")
		return
	}
	writeJSON(w, http.StatusOK, snap)
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

// cfilters returns the compact block filter for every block. A light client
// tests these for its addresses to skip provably-irrelevant blocks and prove
// non-inclusion (see core.BlockFilter).
func (s *Server) cfilters(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Chain().BlockFilters())
}

// cfilter returns a single compact block filter by height: /cfilter/{index}.
func (s *Server) cfilter(w http.ResponseWriter, r *http.Request) {
	idxStr := strings.TrimPrefix(r.URL.Path, "/cfilter/")
	idx, err := strconv.ParseUint(idxStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid height")
		return
	}
	f, ok := s.node.Chain().BlockFilterAt(idx)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such block")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// cfheaders returns the filter-header chain (BIP157-style), so a client can
// check that downloaded filters hash into a consistent set.
func (s *Server) cfheaders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Chain().FilterHeaders())
}

// stateProof returns a proof that an address holds a specific balance/nonce under
// the tip block's committed state root: /stateproof/{addr}. A light client folds
// it and checks it reaches the StateRoot of a PoW-verified header. Absent
// addresses return 404 (a plain merkle tree can't prove non-membership).
func (s *Server) stateProof(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimPrefix(r.URL.Path, "/stateproof/")
	p, ok := s.node.Chain().ProveAccount(addr)
	if !ok {
		writeErr(w, http.StatusNotFound, "address has no account (cannot prove the balance of an absent address)")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

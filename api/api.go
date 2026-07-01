// Package api exposes a small read/write HTTP interface to a running node.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
)

// Server serves the HTTP API for a node.
type Server struct{ node *node.Node }

// New returns an API server bound to n.
func New(n *node.Node) *Server { return &Server{node: n} }

// Start blocks serving the API on addr.
func (s *Server) Start(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/info", s.info)
	mux.HandleFunc("/chain", s.chain)
	mux.HandleFunc("/balance/", s.balance)
	mux.HandleFunc("/account/", s.account)
	mux.HandleFunc("/mempool", s.mempool)
	mux.HandleFunc("/peers", s.peers)
	mux.HandleFunc("/address", s.address)
	mux.HandleFunc("/tx", s.submitTx) // POST a fully signed transaction
	mux.HandleFunc("/send", s.send)   // POST {to, amount, fee, expiry?, nonce?}; signed by node wallet
	// SPV / light-client endpoints.
	mux.HandleFunc("/headers", s.headers) // all block headers
	mux.HandleFunc("/header/", s.header)  // one header by height
	mux.HandleFunc("/proof/", s.proof)    // inclusion proof for a tx hash

	log.Printf("API listening on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
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
		"peers":           s.node.PeerAddrs(),
	})
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
	// Nonce is optional: omit it to auto-select the next one, or set it
	// explicitly (with a higher fee) to fee-bump a stuck transaction. Expiry is
	// an optional maximum block height after which the tx is dropped.
	var req struct {
		To     string  `json:"to"`
		Amount uint64  `json:"amount"`
		Fee    uint64  `json:"fee"`
		Expiry uint64  `json:"expiry"`
		Nonce  *uint64 `json:"nonce"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	nonce := s.node.NextNonce(wal.Address())
	if req.Nonce != nil {
		nonce = *req.Nonce
	}
	tx := core.Transaction{
		From:   wal.Address(),
		To:     req.To,
		Amount: req.Amount,
		Fee:    req.Fee,
		Nonce:  nonce,
		Expiry: req.Expiry,
	}
	if err := tx.Sign(wal); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.node.SubmitTx(tx); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hash": tx.Hash(), "nonce": tx.Nonce, "expiry": tx.Expiry})
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

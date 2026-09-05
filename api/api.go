// Package api exposes a small read/write HTTP interface to a running node.
package api

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
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
	node    *node.Node
	token   string
	limiter *limiter // per-client request budget; nil disables the limit
}

// New returns an API server bound to n, reading the optional bearer token from
// the DNAS_API_TOKEN environment variable (kept out of flags so it doesn't leak
// into `ps`, like the wallet passphrase).
func New(n *node.Node) *Server { return NewWithToken(n, os.Getenv("DNAS_API_TOKEN")) }

// NewWithToken returns an API server that requires the given bearer token on
// write endpoints (empty disables auth).
func NewWithToken(n *node.Node, token string) *Server {
	return &Server{node: n, token: token, limiter: newLimiter(DefaultAPIRate, DefaultAPIBurst)}
}

// SetRateLimit replaces the API's per-client request budget. A rate of zero
// removes the limit, for a node whose API already sits behind something that
// does this properly.
func (s *Server) SetRateLimit(rate, burst float64) {
	if rate <= 0 || burst <= 0 {
		s.limiter = nil
		return
	}
	s.limiter = newLimiter(rate, burst)
}

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
	mux.HandleFunc("/mempool/stats", s.mempoolStats) // fee-rate distribution of the pending queue
	mux.HandleFunc("/peers", s.peers)                // GET connected peers, in detail
	mux.HandleFunc("/bans", s.bans)                  // GET scored/banned keys
	mux.HandleFunc("/unban", s.guard(s.unban))       // POST {key}; clear a ban score
	mux.HandleFunc("/addpeer", s.guard(s.addPeer))   // POST {addr}; dial a peer now
	mux.HandleFunc("/droppeer", s.guard(s.dropPeer)) // POST {peer}; close a connection
	mux.HandleFunc("/chainstats", s.chainStats)      // GET ?window=N; hashrate, intervals, fees
	mux.HandleFunc("/reorgs", s.reorgs)              // GET the reorgs this node has lived through
	mux.HandleFunc("/health", s.health)              // GET a supervisor-friendly readiness check
	mux.HandleFunc("/address", s.address)
	mux.HandleFunc("/address/", s.addressHistory)          // GET /address/{addr}/history (needs -addrindex)
	mux.HandleFunc("/tx", s.guard(s.submitTx))             // POST a fully signed transaction
	mux.HandleFunc("/tx/", s.txByHash)                     // GET /tx/HASH; confirmed or pending lookup
	mux.HandleFunc("/supply", s.supply)                    // GET coin supply: minted, burned, circulating
	mux.HandleFunc("/assets", s.assets)                    // GET every issued asset (?ticker=X to filter)
	mux.HandleFunc("/asset/", s.asset)                     // GET /asset/{id}: one asset and who holds it
	mux.HandleFunc("/send", s.guard(s.send))               // POST {to, amount, fee, expiry?, nonce?}; signed by node wallet
	mux.HandleFunc("/mine", s.guard(s.mine))               // POST {on: bool}; toggle mining at runtime
	mux.HandleFunc("/generate", s.guard(s.generate))       // POST {n}; regtest-only on-demand mining
	mux.HandleFunc("/blocktemplate", s.blockTemplate)      // GET ?address=ADDR; candidate block for an external miner
	mux.HandleFunc("/submitblock", s.guard(s.submitBlock)) // POST a mined block
	mux.HandleFunc("/submitshare", s.guard(s.submitShare)) // POST a share (an easier-target candidate)
	mux.HandleFunc("/shares", s.shares)                    // GET the share ledger
	mux.HandleFunc("/faucet", s.guard(s.faucet))           // POST {address}; testnet/regtest only
	mux.HandleFunc("/estimatefee", s.estimateFee)          // GET ?blocks=N; recommended fee
	mux.HandleFunc("/metrics", s.metrics)                  // Prometheus-style metrics
	mux.HandleFunc("/events", s.events)                    // Server-Sent Events: live block/tx stream
	mux.HandleFunc("/webhooks", s.webhooks)                // GET delivery counters for the configured webhooks
	// Stateless wallet helpers (no node state touched; localhost/toy use).
	mux.HandleFunc("/multisig/address", s.multisigAddress) // POST {threshold, pubkeys[]}
	mux.HandleFunc("/htlc/address", s.htlcAddress)         // POST {hash, recipient, sender, timeout}
	mux.HandleFunc("/vault/address", s.vaultAddress)       // POST {hot, cold, unlock}
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

	// The limit wraps everything, including the read endpoints: the expensive
	// requests here are reads (a paged /chain, a /snapshot, a /stateproof).
	return s.RateLimit(mux)
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

// webhooks reports what the node's webhook deliveries have done. A webhook that
// is silently failing is otherwise invisible from the outside: the receiver sees
// nothing, and "nothing" is also what a quiet chain looks like.
func (s *Server) webhooks(w http.ResponseWriter, r *http.Request) {
	stats := s.node.WebhookStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.node.WebhooksEnabled(),
		"urls":    stats.URLs,
		"sent":    stats.Sent,
		"failed":  stats.Failed,
		"dropped": stats.Dropped,
		"queued":  stats.Queued,
	})
}

// assets lists the assets the chain has issued.
//
// Without this an asset balance is unreadable: the id is a hash of (issuer,
// ticker, nonce), so a holder sees `tok3f2a…: 500` with no way to learn what it
// is. The ?ticker filter returns a LIST rather than one asset on purpose —
// anyone may issue "GOLD", and collapsing a ticker to a single asset would be
// choosing an issuer on the caller's behalf.
func (s *Server) assets(w http.ResponseWriter, r *http.Request) {
	list := s.node.Chain().Assets()
	if ticker := r.URL.Query().Get("ticker"); ticker != "" {
		list = s.node.Chain().AssetsByTicker(ticker)
	}
	if list == nil {
		list = []core.AssetInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

// asset returns one asset and its holders: /asset/{id}.
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/asset/")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "asset id required")
		return
	}
	info, ok := s.node.Chain().Asset(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such asset on this chain")
		return
	}
	holders := s.node.Chain().AssetHolders(id)
	if holders == nil {
		holders = []core.AssetHolder{}
	}
	// The held total is reported alongside the issued supply because an asset's
	// total is conserved: the two must be equal, and a client can check that
	// rather than take the supply on trust.
	var held uint64
	for _, h := range holders {
		held += h.Amount
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"asset":   info,
		"holders": holders,
		"held":    held,
	})
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
	node.Infof("API listening", "url", "http://"+addr)
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
		"network":         core.NetworkName(),
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
		"address_index":   s.node.Chain().AddressIndexed(),
		"faucet":          s.node.FaucetEnabled(),
		"webhooks":        s.node.WebhooksEnabled(),
		// What this node can actually serve, so a client can tell "not in the
		// chain" from "not visible from here": the lowest height whose body it
		// holds, and where its filter-header chain starts.
		"body_height":   s.node.Chain().BodyHeight(),
		"filter_base":   s.node.Chain().FilterHeaderBase(),
		"pruned":        s.node.Chain().PruneKeep() > 0,
		"prune_keep":    s.node.Chain().PruneKeep(),
		"pruned_bodies": s.node.Chain().PrunedCount(),
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

// blockTemplateResponse is a candidate block plus the pool-side share target.
// The block's own fields are inlined, so a miner that only knows about blocks
// decodes it exactly as before and ignores the rest.
type blockTemplateResponse struct {
	core.Block
	ShareBits       uint32  `json:"share_bits"`
	ShareFactor     uint32  `json:"share_factor"`
	ShareDifficulty float64 `json:"share_difficulty"`
}

// maxLongPoll bounds how long a template request may block. Beyond this the
// connection is more likely to be dropped by something in the middle than to be
// answered, and the miner can simply ask again.
const maxLongPoll = 120 * time.Second

// blockTemplate returns a candidate block for an external miner to hash:
// GET /blocktemplate?address=ADDR (defaulting to the node's own wallet). The
// miner searches for a Nonce whose hash meets Bits, then POSTs it to
// /submitblock — or, if it only meets the easier share_bits, to /submitshare.
//
// With ?longpoll=1&prev=HASH the request does not answer until the tip moves off
// HASH (or ?timeout=SECONDS elapses). That is the difference between a miner
// starting on a fresh template the instant the old one dies and finding out on
// its next poll, having hashed a dead candidate in between. A prev that is
// already stale returns immediately, so a miner can never be parked waiting for a
// change it has missed.
func (s *Server) blockTemplate(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("address")
	if addr == "" {
		if wal := s.node.Wallet(); wal != nil {
			addr = wal.Address()
		} else {
			writeErr(w, http.StatusBadRequest, "address required (node has no wallet to default to)")
			return
		}
	}
	q := r.URL.Query()
	if q.Get("longpoll") != "" && q.Get("longpoll") != "0" {
		timeout := 30 * time.Second
		if v := q.Get("timeout"); v != "" {
			secs, err := strconv.Atoi(v)
			if err != nil || secs < 1 {
				writeErr(w, http.StatusBadRequest, "timeout must be a positive number of seconds")
				return
			}
			if timeout = time.Duration(secs) * time.Second; timeout > maxLongPoll {
				timeout = maxLongPoll
			}
		}
		s.node.WaitForTip(q.Get("prev"), timeout)
	}
	b, err := s.node.BuildTemplate(addr)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	shareBits := core.ShareBits(b.Bits, s.node.ShareFactor())
	writeJSON(w, http.StatusOK, blockTemplateResponse{
		Block:           b,
		ShareBits:       shareBits,
		ShareFactor:     s.node.ShareFactor(),
		ShareDifficulty: core.TargetDifficulty(shareBits),
	})
}

// submitShare accepts a candidate block that met the share target (POST
// /submitshare). A share that also meets the real block target is accepted as a
// block, which the response says so the miner can log it.
func (s *Server) submitShare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var b core.Block
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid block json")
		return
	}
	res, err := s.node.SubmitShare(b)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// shares reports the node's share ledger: who has been submitting work, how much
// of it, and how many of those shares turned into blocks.
func (s *Server) shares(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Shares())
}

// faucet pays the faucet amount to an address (POST /faucet {"address":"..."}).
// It exists only on a network whose parameters allow one — never mainnet — and
// only when the operator enabled it, since it spends the node's own wallet. The
// client's IP shares the recipient's cooldown, so neither one address nor one
// client can drain it.
func (s *Server) faucet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.node.FaucetEnabled() {
		writeErr(w, http.StatusForbidden,
			fmt.Sprintf("no faucet on this node (network %s)", core.NetworkName()))
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	tx, err := s.node.FaucetFor(req.Address, clientIP(r))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hash":       tx.Hash(),
		"to":         tx.To,
		"amount":     tx.Amount,
		"amount_fmt": core.FormatAmount(tx.Amount),
		"cooldown":   s.node.FaucetCooldown().String(),
	})
}

// clientIP is the requester identity the faucet rate-limits on. It is the peer
// address of the connection, deliberately NOT a forwarded header: a client can
// set those freely, and trusting them would hand out an unlimited number of
// identities to anyone who asked.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// addressHistory serves the transactions that touched an address:
// GET /address/{addr}/history?from=HEIGHT&limit=N, oldest first. It needs the
// node to be running with the address index (-addrindex); without it the answer
// would be "no history" for every address, which is worse than an error.
func (s *Server) addressHistory(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/address/")
	addr, tail, _ := strings.Cut(rest, "/")
	if addr == "" || (tail != "" && tail != "history") {
		writeErr(w, http.StatusNotFound, "use /address/{address}/history")
		return
	}
	var from uint64
	if v := r.URL.Query().Get("from"); v != "" {
		h, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "from must be a block height")
			return
		}
		from = h
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	entries, ok := s.node.Chain().AddressHistory(addr, from, limit)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable,
			"this node does not maintain the address index (start it with -addrindex)")
		return
	}
	total, _ := s.node.Chain().AddressHistoryLen(addr)
	tip := s.node.Chain().Tip().Index
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"height":        e.Height,
			"index":         e.Index,
			"hash":          e.Hash,
			"confirmations": tip - e.Height + 1,
			"tx":            e.Tx,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"address": addr,
		"total":   total,
		"from":    from,
		"count":   len(out),
		"entries": out,
	})
}

// submitBlock accepts a block mined externally (POST /submitblock). It is an
// authenticated write; a stale or invalid block returns 400 so the miner refetches.
func (s *Server) submitBlock(w http.ResponseWriter, r *http.Request) {
	var b core.Block
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid block json")
		return
	}
	if err := s.node.SubmitMinedBlock(b); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "height": b.Index, "hash": b.Hash})
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
	shares := s.node.Shares()
	gauge("dnas_shares_submitted", "Mining shares submitted to this node.", shares.Submitted)
	gauge("dnas_shares_accepted", "Mining shares accepted by this node.", shares.Accepted)
	gauge("dnas_shares_stale", "Mining shares rejected as stale.", shares.Stale)
	gauge("dnas_shares_blocks", "Submitted shares that also met the block target.", shares.Blocks)
	gauge("dnas_share_difficulty", "Difficulty of the current share target.", core.TargetDifficulty(shares.ShareBits))
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

// Paging on the bulk read endpoints.
//
// /chain, /headers and /cfilters each used to serialize the WHOLE chain into one
// response. That is a memory-amplification attack anyone can run with curl (a
// 100k-block header dump is tens of megabytes the node builds in RAM per
// request), and it made the light client the heaviest thing on the network,
// because `dnas spv` re-fetched every header on every command.
//
// Each now takes ?from=HEIGHT&limit=N and answers at most defaultPageLimit
// entries. The response is still a plain JSON array, so a client pages with
// `from` and learns the total from /info's height — no shape change, and the
// server can no longer be asked for an unbounded response.
//
// `?last=N` exists because paging alone could not express what the most common
// consumer actually wants. Every "recent blocks" view asks for the NEWEST
// entries, and with only `from` it has to know the tip height first — so the
// clients kept fetching an unparameterized page (which now means the FIRST page)
// and taking its tail, silently showing blocks 1999..1992 forever once a chain
// passed the page limit. `last` answers the question directly, in one request,
// and cannot drift out of date between the two calls the alternative needs.
const (
	// defaultPageLimit is how many entries a bulk read returns when the caller
	// gives no limit, and the most it may ask for. It matches the P2P layer's
	// headers batch, so the HTTP and peer paths agree on what "a batch" is.
	defaultPageLimit = maxPageLimit
	maxPageLimit     = 2000
)

// pageParams reads ?from=, ?limit= and ?last=, applying the cap. It writes the
// error response itself and reports false when the request is malformed.
//
// `last=N` is resolved against the current height into the same (from, limit)
// pair every accessor already understands, so there is one paging path rather
// than two.
func (s *Server) pageParams(w http.ResponseWriter, r *http.Request) (from uint64, limit int, ok bool) {
	q := r.URL.Query()
	limit = defaultPageLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return 0, 0, false
		}
		if limit = n; limit > maxPageLimit {
			limit = maxPageLimit
		}
	}
	if v := q.Get("last"); v != "" {
		if q.Get("from") != "" {
			writeErr(w, http.StatusBadRequest, "give either from or last, not both")
			return 0, 0, false
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "last must be a positive integer")
			return 0, 0, false
		}
		if n > maxPageLimit {
			n = maxPageLimit
		}
		// Walk back n entries from the tip, clamping at genesis so `last` on a short
		// chain returns the whole chain rather than an empty page.
		height := s.node.Chain().Height()
		if uint64(n) > height+1 {
			n = int(height + 1)
		}
		return height + 1 - uint64(n), n, true
	}
	if v := q.Get("from"); v != "" {
		h, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "from must be a block height")
			return 0, 0, false
		}
		from = h
	}
	return from, limit, true
}

// chain returns block bodies from `from`, at most `limit` of them.
func (s *Server) chain(w http.ResponseWriter, r *http.Request) {
	from, limit, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	to := from + uint64(limit) - 1
	blocks := s.node.Chain().BlocksRange(from, to, limit)
	if blocks == nil {
		blocks = []core.Block{} // an empty page is [], never null
	}
	writeJSON(w, http.StatusOK, blocks)
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
	out := map[string]any{
		"address": addr,
		"balance": acc.Balance,
		"nonce":   acc.Nonce,
		// The formatted amount travels with the raw one, as it does on /balance
		// and /supply: a client that divides by the coin unit itself is a client
		// that can get the decimal places wrong, and every one of them would have
		// to.
		"balance_fmt": core.FormatAmount(acc.Balance),
	}
	if len(acc.Assets) > 0 {
		out["assets"] = acc.Assets
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) mempool(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Mempool().All())
}

// mempoolStats reports the shape of the pending queue: total size and the
// distribution of fee RATES in it. A sender cannot tell from a queue depth alone
// whether their fee will be picked up next block or sit behind a wall of higher
// bidders — the distribution is what answers that, and computing it needs the
// canonical transaction size, which only the node has.
func (s *Server) mempoolStats(w http.ResponseWriter, r *http.Request) {
	st := s.node.Mempool().Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"count":       st.Count,
		"bytes":       st.Bytes,
		"min_rate":    st.MinRate,
		"max_rate":    st.MaxRate,
		"median_rate": st.MedianRate,
		"base_fee":    s.node.Chain().NextBaseFee(),
		"buckets":     st.Buckets,
	})
}

// txByHash looks a transaction up by id: GET /tx/HASH. It answers for both
// stages of a transaction's life — "confirmed" (with the block that holds it and
// its confirmation count, resolved through the chain's transaction index) and
// "pending" (still in this node's mempool) — so a wallet can poll one endpoint
// from submission to confirmation instead of guessing which to ask.
func (s *Server) txByHash(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimPrefix(r.URL.Path, "/tx/")
	if hash == "" {
		writeErr(w, http.StatusBadRequest, "transaction hash required")
		return
	}
	if tx, loc, ok := s.node.Chain().FindTx(hash); ok {
		tip := s.node.Chain().Tip()
		body := map[string]any{
			"status":        "confirmed",
			"hash":          hash,
			"height":        loc.Height,
			"index":         loc.Index,
			"confirmations": tip.Index - loc.Height + 1,
			"tx":            tx,
		}
		if b, ok := s.node.Chain().BlockAt(loc.Height); ok {
			body["block_hash"] = b.Hash
		}
		writeJSON(w, http.StatusOK, body)
		return
	}
	if tx, ok := s.node.Mempool().Get(hash); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "pending",
			"hash":          hash,
			"confirmations": 0,
			"tx":            tx,
		})
		return
	}
	writeErr(w, http.StatusNotFound, "transaction not found (neither confirmed nor pending)")
}

// supply reports coin issuance: how much has ever been minted by block
// subsidies, how much the per-byte base fee has burned, and how much is actually
// held in accounts. `consistent` is the conservation check minted − burned ==
// circulating; false means coin has been created or destroyed outside those two
// paths, which would be an accounting bug.
func (s *Server) supply(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Chain().Supply())
}

// peers reports every live connection in full: the address, the authenticated
// identity, the negotiated version and capabilities, who dialed whom, how long
// it has been up, whether it is currently serving us blocks, and its ban score.
// All of that was already known per connection and previously discarded — a bare
// list of addresses cannot tell you which peer is misbehaving or stalling.
func (s *Server) peers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Peers())
}

// bans lists every scored key with its score and whether it is over the
// threshold. Keys BELOW the threshold are included deliberately: seeing a peer
// at 80 of 100 points before it is cut off is most of the value of scoring.
func (s *Server) bans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"threshold": s.node.BanThreshold(),
		"entries":   s.node.Bans(),
	})
}

// unban clears a key's ban score: POST {"key":"<identity-or-ip>"}. Without it an
// operator whose peer was scored by a bug had to stop the node, edit bans.json
// and start again.
func (s *Server) unban(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.node.Unban(req.Key); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unbanned": req.Key})
}

// addPeer dials a peer at runtime: POST {"addr":"host:port"} — `-peers` without
// a restart.
func (s *Server) addPeer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.node.AddPeer(req.Addr); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dialing": req.Addr})
}

// dropPeer closes a connection, matched by advertised address or identity key:
// POST {"peer":"..."}. An outbound peer is redialed by its dial loop soon after —
// dropping clears a wedged connection, banning keeps someone away.
func (s *Server) dropPeer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Peer string `json:"peer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	closed, err := s.node.DropPeer(req.Peer)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dropped": req.Peer, "connections": closed})
}

// chainStats reports what the header numbers imply: GET /chainstats?window=N.
// Estimated network hashrate, the block-interval distribution against the target,
// difficulty range, fee/burn/tip totals and who mined the window. Difficulty
// alone says nothing about whether the chain is healthy; this does.
func (s *Server) chainStats(w http.ResponseWriter, r *http.Request) {
	window := 0
	if v := r.URL.Query().Get("window"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 2 {
			writeErr(w, http.StatusBadRequest, "window must be at least 2 blocks")
			return
		}
		window = n
	}
	writeJSON(w, http.StatusOK, s.node.Chain().Stats(window))
}

// reorgs returns the chain switches this node has lived through, newest first.
// The SSE stream announces a reorg to whoever is listening at that instant and
// then forgets it; this is the record you want afterwards.
func (s *Server) reorgs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.node.Reorgs())
}

// health is the readiness check a supervisor needs, which /info cannot be: /info
// answers 200 while a node is still syncing, has no peers, or is sitting on a
// tip that stopped moving hours ago. This answers 200 only when the node is
// actually usable and 503 with the reasons when it is not.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	tip := s.node.Chain().Tip()
	age := time.Since(time.Unix(tip.Timestamp, 0))
	peers := len(s.node.PeerAddrs())
	behind := s.node.BlocksBehind()

	var reasons []string
	// A node with no peers cannot know it is on the best chain. Genesis-only is
	// called out separately because it is the normal state of a fresh node, not a
	// fault, but it is still not ready to answer questions about the chain.
	if peers == 0 {
		reasons = append(reasons, "no peers connected")
	}
	if behind > 0 {
		reasons = append(reasons, fmt.Sprintf("%d block(s) behind the best known height", behind))
	}
	if tip.Index == 0 {
		reasons = append(reasons, "chain is at genesis")
	} else if age > staleTipAfter {
		reasons = append(reasons, fmt.Sprintf("tip is %s old", age.Round(time.Second)))
	}

	code := http.StatusOK
	if len(reasons) > 0 {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"ok":            len(reasons) == 0,
		"network":       core.NetworkName(),
		"height":        tip.Index,
		"tip_age":       age.Round(time.Second).String(),
		"peers":         peers,
		"blocks_behind": behind,
		"mempool":       s.node.Mempool().Size(),
		"reasons":       reasons,
	})
}

// staleTipAfter is how long a tip may go unchanged before health calls it stale:
// generous next to TargetBlockTime, since proof-of-work interval variance is
// wide and a quiet chain also mints empty blocks on its own interval.
var staleTipAfter = 20 * time.Duration(core.TargetBlockTime) * time.Second

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
		To        string        `json:"to"`
		Amount    uint64        `json:"amount"`
		Outputs   []core.Output `json:"outputs"`
		Fee       uint64        `json:"fee"`
		Expiry    uint64        `json:"expiry"`
		LockUntil uint64        `json:"lock_until"`
		Memo      string        `json:"memo"`
		Nonce     *uint64       `json:"nonce"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Either one recipient (to/amount) or many (outputs), never both. Every
	// recipient's checksum is validated here so a typo is refused before signing
	// rather than burning the coin.
	if len(req.Outputs) > 0 && (req.To != "" || req.Amount != 0) {
		writeErr(w, http.StatusBadRequest, "give either to/amount or outputs, not both")
		return
	}
	recipients := req.Outputs
	if len(recipients) == 0 {
		recipients = []core.Output{{To: req.To, Amount: req.Amount}}
	}
	if len(recipients) > core.MaxTxOutputs {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("too many outputs (max %d)", core.MaxTxOutputs))
		return
	}
	for i, o := range recipients {
		if err := wallet.ValidateAddress(o.To); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid recipient %d: %s", i, err))
			return
		}
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
		Fee:       req.Fee,
		Nonce:     nonce,
		Expiry:    req.Expiry,
		LockUntil: req.LockUntil,
		Memo:      req.Memo,
	}
	if len(req.Outputs) > 0 {
		tx.Outputs = req.Outputs
	} else {
		tx.To, tx.Amount = req.To, req.Amount
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

// vaultAddress derives a time-delayed vault address from its script. Stateless,
// like the multisig and HTLC helpers: it computes an address to fund and holds
// no secret.
func (s *Server) vaultAddress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Hot    string `json:"hot"`
		Cold   string `json:"cold"`
		Unlock uint64 `json:"unlock"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	addr, err := wallet.VaultAddress(req.Hot, req.Cold, req.Unlock)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"address": addr, "unlock": req.Unlock})
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

// headers returns block headers from `from`, at most `limit` of them (light
// clients verify PoW + the hash chain from these alone, paging as they go).
func (s *Server) headers(w http.ResponseWriter, r *http.Request) {
	from, limit, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	hs := s.node.Chain().HeadersFrom(from, limit)
	if hs == nil {
		hs = []core.Header{}
	}
	writeJSON(w, http.StatusOK, hs)
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
	b, err := s.node.Chain().BlockBodyAt(idx)
	if err != nil {
		// A pruned body is NOT a missing block, and a client told "not found"
		// would conclude the chain is shorter than it is. 410 Gone says the height
		// exists and this node cannot serve it, so the answer is to ask another.
		if errors.Is(err, core.ErrPrunedBody) {
			writeErr(w, http.StatusGone, err.Error())
			return
		}
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
	from, limit, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	fs := s.node.Chain().BlockFiltersFrom(from, limit)
	if fs == nil {
		fs = []core.BlockFilter{}
	}
	writeJSON(w, http.StatusOK, fs)
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
		// An empty filter is a proof of ABSENCE, so a node without the body must
		// not build one: it would tell a light client its address is provably not
		// in a block this node cannot read (see core/prune.go).
		if idx < s.node.Chain().Height() && !s.node.Chain().HasBody(idx) {
			writeErr(w, http.StatusGone,
				fmt.Sprintf("this node has pruned that block's body; filters start at height %d",
					s.node.Chain().BodyHeight()))
			return
		}
		writeErr(w, http.StatusNotFound, "no such block")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// cfheaders returns the filter-header chain (BIP157-style), so a client can
// check that downloaded filters hash into a consistent set.
func (s *Server) cfheaders(w http.ResponseWriter, r *http.Request) {
	from, limit, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	// A node that fast-synced never saw the bodies below its snapshot, so it
	// cannot fold their filter commitments. Answering with an empty list would
	// read as "there are none", which is a different and wrong claim.
	if base := s.node.Chain().FilterHeaderBase(); from < base {
		writeErr(w, http.StatusGone,
			fmt.Sprintf("this node's filter headers start at height %d", base))
		return
	}
	hs := s.node.Chain().FilterHeadersFrom(from, limit)
	if hs == nil {
		hs = []string{}
	}
	writeJSON(w, http.StatusOK, hs)
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

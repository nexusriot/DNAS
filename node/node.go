// Package node wires the blockchain, mempool and wallet to an authenticated,
// encrypted TCP peer network and (optionally) a miner. It handles block and
// transaction gossip, headers-first ranged sync, most-work consensus with
// reorgs, peer discovery, node-identity authentication and ban scoring.
package node

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Defaults for the peer network.
const (
	DefaultNetKey   = "dnas-devnet" // shared secret authenticating the network
	DefaultMaxPeers = 8             // maximum outbound dials
	seenCapacity    = 100_000       // bounded gossip de-dup memory per kind
)

// Config holds a node's network and mining settings.
type Config struct {
	ListenAddr    string   // TCP address to listen on
	AdvertiseAddr string   // address peers should dial us at (defaults to ListenAddr)
	Peers         []string // seed peer addresses to dial
	NetKey        string   // pre-shared network key (defaults to DefaultNetKey)
	MaxPeers      int      // outbound dial cap (defaults to DefaultMaxPeers)
	Mine          bool     // whether to run the miner
	StateDir      string   // directory for persisted peer/ban/mempool state ("" = in-memory only)
	Regtest       bool     // regtest mode: enable on-demand block generation (POST /generate)
}

type peer struct {
	conn *secureConn
	enc  *json.Encoder
	mu   sync.Mutex // serializes writes to enc
	addr string     // peer's advertised address (from hello)
	id   string     // peer's authenticated identity public key
	ip   string     // remote IP, for ban scoring
}

func (p *peer) send(m Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.enc.Encode(m)
}

// Node is a running participant in the DNAS network.
type Node struct {
	cfg      Config
	chain    *core.Blockchain
	mempool  *core.Mempool
	wallet   *wallet.Wallet // mining/signing wallet (may be nil)
	identity *wallet.Wallet // node identity for authenticated handshakes
	psk      []byte

	peersMu sync.Mutex
	peers   map[*peer]bool

	seenBlk *seenSet
	seenTx  *seenSet
	book    *peerbook
	bans    *banbook
	events  *eventBus

	mining atomic.Bool // whether the miner is currently active (toggle at runtime)
	tipGen int64       // atomic; bumped whenever the tip changes to interrupt mining
}

// New constructs a Node. The wallet enables mining and API-side signing; if it
// is nil an ephemeral key is generated to serve as the node's network identity.
func New(cfg Config, chain *core.Blockchain, mp *core.Mempool, w *wallet.Wallet) *Node {
	if cfg.AdvertiseAddr == "" {
		cfg.AdvertiseAddr = cfg.ListenAddr
	}
	if cfg.MaxPeers <= 0 {
		cfg.MaxPeers = DefaultMaxPeers
	}
	if cfg.NetKey == "" {
		cfg.NetKey = DefaultNetKey
	}
	identity := w
	if identity == nil {
		identity, _ = wallet.New()
	}
	n := &Node{
		cfg:      cfg,
		chain:    chain,
		mempool:  mp,
		wallet:   w,
		identity: identity,
		psk:      []byte(cfg.NetKey),
		peers:    map[*peer]bool{},
		seenBlk:  newSeenSet(seenCapacity),
		seenTx:   newSeenSet(seenCapacity),
		book:     newPeerbook(cfg.AdvertiseAddr, cfg.MaxPeers),
		bans:     newBanbook(banThreshold),
		events:   newEventBus(),
	}
	n.mining.Store(cfg.Mine)
	return n
}

// SetMining turns the miner on or off at runtime (no-op if the node has no
// wallet to be paid). Mining reports the current state.
func (n *Node) SetMining(on bool) {
	if n.wallet == nil {
		return
	}
	n.mining.Store(on)
	n.onTipChanged() // wake the miner promptly
}

func (n *Node) Mining() bool { return n.mining.Load() }

// Regtest reports whether the node is in regtest mode (on-demand block
// generation via Generate / POST /generate is available).
func (n *Node) Regtest() bool { return n.cfg.Regtest }

// Shutdown stops mining and closes all peer connections. The chain's store is
// closed separately by the owner (Blockchain.Close).
func (n *Node) Shutdown() {
	n.mining.Store(false)
	n.peersMu.Lock()
	ps := make([]*peer, 0, len(n.peers))
	for p := range n.peers {
		ps = append(ps, p)
	}
	n.peersMu.Unlock()
	for _, p := range ps {
		_ = p.conn.Close()
	}
	n.saveState() // persist peers/bans/mempool so a restart resumes warm
}

// Start begins listening, dials the seed and persisted peers, and starts mining
// if enabled.
func (n *Node) Start() {
	n.loadState() // restore persisted peers/bans/mempool before dialing
	go n.listen()
	for _, a := range n.cfg.Peers {
		n.book.note(a)
	}
	for _, a := range n.book.all() { // seed peers + any restored from disk
		n.maybeDial(a)
	}
	// The miner runs whenever the node has a wallet; the atomic flag gates
	// whether it actually produces blocks, so it can be toggled at runtime.
	if n.wallet != nil {
		go n.mineLoop()
	} else if n.cfg.Mine {
		log.Print("mining requested but no wallet; disabled")
	}
}

func (n *Node) Chain() *core.Blockchain { return n.chain }
func (n *Node) Mempool() *core.Mempool  { return n.mempool }
func (n *Node) Wallet() *wallet.Wallet  { return n.wallet }

// Subscribe registers for real-time node events (new blocks, reorgs, mempool
// transactions). It returns a receive channel and a function that unsubscribes
// and closes it; callers must invoke the latter when done. Slow consumers miss
// events rather than slowing the node.
func (n *Node) Subscribe() (<-chan Event, func()) { return n.events.subscribe() }

// publishBlock emits a block/reorg event describing the current tip.
func (n *Node) publishBlock(reorg bool) {
	tip := n.chain.Tip()
	typ := "block"
	if reorg {
		typ = "reorg"
	}
	n.events.publish(Event{Type: typ, Height: tip.Index, Hash: tip.Hash, Txs: len(tip.Transactions)})
}

// publishTx emits a mempool-transaction event.
func (n *Node) publishTx(tx core.Transaction) {
	n.events.publish(Event{Type: "tx", Hash: tx.Hash(), From: tx.From, To: tx.To, Amount: tx.Amount, Fee: tx.Fee})
}

// PeerAddrs returns the distinct advertised addresses of connected peers.
func (n *Node) PeerAddrs() []string {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	seen := map[string]bool{}
	var out []string
	for p := range n.peers {
		if p.addr != "" && !seen[p.addr] {
			seen[p.addr] = true
			out = append(out, p.addr)
		}
	}
	return out
}

// connectedTo reports whether we already have a peer advertising addr.
func (n *Node) connectedTo(addr string) bool {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	for p := range n.peers {
		if p.addr == addr {
			return true
		}
	}
	return false
}

// NextNonce returns the nonce a new transaction from addr should use, counting
// both confirmed state and transactions already pending in the mempool.
func (n *Node) NextNonce(addr string) uint64 {
	next := n.chain.Account(addr).Nonce
	for _, tx := range n.mempool.All() {
		if tx.From == addr && tx.Nonce >= next {
			next = tx.Nonce + 1
		}
	}
	return next
}

// SubmitTx adds a locally created transaction to the mempool and gossips it.
func (n *Node) SubmitTx(tx core.Transaction) error {
	if next := n.chain.Height() + 1; tx.IsExpiredAt(next) {
		return fmt.Errorf("transaction already expired (expiry %d, next block height %d)", tx.Expiry, next)
	}
	added, err := n.mempool.Add(tx)
	if err != nil {
		return err
	}
	if added {
		n.markSeenTx(tx.Hash())
		n.broadcast(Message{Type: MsgTx, Tx: &tx})
		n.publishTx(tx)
	}
	return nil
}

func (n *Node) listen() {
	ln, err := net.Listen("tcp", n.cfg.ListenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", n.cfg.ListenAddr, err)
	}
	log.Printf("p2p listening on %s (advertising %s)", n.cfg.ListenAddr, n.cfg.AdvertiseAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go n.handleConn(conn)
	}
}

// maybeDial starts a dial loop to addr if we are not already connected to it
// and the peerbook approves (new, not self, under the outbound cap). Skipping
// addresses we already have a connection to avoids redundant mutual dials.
func (n *Node) maybeDial(addr string) {
	if addr == "" || addr == n.cfg.AdvertiseAddr || n.connectedTo(addr) {
		return
	}
	if n.book.shouldDial(addr) {
		go n.dialLoop(addr)
	}
}

func (n *Node) dialLoop(addr string) {
	for {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		n.handleConn(conn) // blocks until the connection drops
		time.Sleep(3 * time.Second)
	}
}

func (n *Node) handleConn(rawConn net.Conn) {
	ip := remoteIP(rawConn)
	if n.bans.banned(ip) {
		_ = rawConn.Close()
		return
	}

	// Establish the encrypted, PSK-authenticated channel.
	sc, sid, err := secureHandshake(rawConn, n.psk)
	if err != nil {
		log.Printf("rejected peer %s: handshake failed: %v", ip, err)
		_ = rawConn.Close()
		return
	}

	p := &peer{conn: sc, enc: json.NewEncoder(sc), ip: ip}
	dec := json.NewDecoder(sc)

	// Authenticated identity exchange: each side proves it holds its identity
	// key by signing the session id, binding the identity to this session.
	p.send(Message{Type: MsgIdentity, PubKey: n.identity.PublicKeyHex(), Sig: n.identity.Sign(sid)})
	var idm Message
	if err := dec.Decode(&idm); err != nil {
		_ = sc.Close()
		return
	}
	if idm.Type != MsgIdentity || !wallet.Verify(idm.PubKey, idm.Sig, sid) {
		log.Printf("rejected peer %s: identity authentication failed", ip)
		_ = sc.Close()
		return
	}
	p.id = idm.PubKey
	if n.bans.banned(p.id) {
		_ = sc.Close()
		return
	}

	n.addPeer(p)
	defer n.removePeer(p)
	log.Printf("peer connected %s id=%s", ip, short(p.id))

	// Introduce ourselves, request peers, and start headers-first catch-up
	// (a block locator lets the peer find our fork point cheaply).
	p.send(Message{Type: MsgHello, Addr: n.cfg.AdvertiseAddr})
	p.send(Message{Type: MsgGetPeers})
	p.send(n.getHeadersMsg())

	for {
		var m Message
		if err := dec.Decode(&m); err != nil {
			return
		}
		n.handleMessage(p, m)
	}
}

func (n *Node) handleMessage(p *peer, m Message) {
	switch m.Type {
	case MsgHello:
		// p.addr is read under peersMu (connectedTo, PeerAddrs), so guard the write.
		n.peersMu.Lock()
		p.addr = m.Addr
		n.peersMu.Unlock()
		n.book.note(m.Addr)

	case MsgGetPeers:
		p.send(Message{Type: MsgPeers, Peers: append(n.book.all(), n.cfg.AdvertiseAddr)})

	case MsgPeers:
		if len(m.Peers) > maxGossipPeers {
			m.Peers = m.Peers[:maxGossipPeers]
		}
		for _, addr := range m.Peers {
			n.book.note(addr)
			n.maybeDial(addr)
		}

	case MsgTx:
		if m.Tx == nil || n.markSeenTx(m.Tx.Hash()) {
			return
		}
		if m.Tx.IsExpiredAt(n.chain.Height() + 1) {
			return
		}
		if added, err := n.mempool.Add(*m.Tx); err != nil || !added {
			return
		}
		n.broadcastExcept(Message{Type: MsgTx, Tx: m.Tx}, p)
		n.publishTx(*m.Tx)

	// block propagation: announce a hash, pull the body we lack
	case MsgInv:
		switch {
		case m.Index == n.chain.Height()+1:
			p.send(Message{Type: MsgGetData, Index: m.Index})
		case m.Index > n.chain.Height()+1:
			p.send(n.getHeadersMsg())
		}

	case MsgGetData:
		if b, ok := n.chain.BlockAt(m.Index); ok {
			p.send(Message{Type: MsgBlock, Block: &b})
		}

	case MsgBlock:
		if m.Block == nil || n.markSeenBlock(m.Block.Hash) {
			return
		}
		if err := n.chain.AddBlock(*m.Block); err != nil {
			// Behind or on a fork: let the locator find where we diverged.
			p.send(n.getHeadersMsg())
			return
		}
		log.Printf("accepted block %d %s", m.Block.Index, short(m.Block.Hash))
		n.afterNewBlock(false)
		n.broadcastExcept(Message{Type: MsgInv, Index: m.Block.Index, Hash: m.Block.Hash}, p)

	// headers-first ranged sync
	case MsgGetHeaders:
		from := m.From
		if len(m.Locator) > 0 { // locator-based: reply from just after the fork point
			from = n.chain.LocatorFork(m.Locator) + 1
		}
		if hs := n.chain.HeadersFrom(from, maxHeadersBatch); len(hs) > 0 {
			p.send(Message{Type: MsgHeaders, Headers: hs})
		}

	case MsgHeaders:
		n.onHeaders(p, m.Headers)

	case MsgGetBlocks:
		if bs := n.chain.BlocksRange(m.From, m.To, maxBlocksBatch); len(bs) > 0 {
			p.send(Message{Type: MsgBlocks, Blocks: bs})
		}

	case MsgBlocks:
		n.onBlocks(p, m.Blocks)

	// whole-chain exchange, used only as a fork/bootstrap fallback
	case MsgGetChain:
		p.send(Message{Type: MsgChain, Chain: n.chain.Blocks()})

	case MsgChain:
		if replaced, err := n.chain.ReplaceChain(m.Chain); err == nil && replaced {
			log.Printf("adopted chain via fallback (height=%d)", n.chain.Height())
			n.afterNewBlock(true)
		}
	}
}

// onHeaders validates a header batch against our chain and, if it links and its
// proof-of-work checks out, requests the corresponding block bodies. A batch
// that doesn't link is a fork (fall back to whole-chain); one that is internally
// invalid is misbehaviour (ban points).
func (n *Node) onHeaders(p *peer, headers []core.Header) {
	if len(headers) == 0 {
		return
	}
	parent, ok := n.chain.HeaderAt(headers[0].Index - 1)
	if !ok || headers[0].PrevHash != parent.Hash {
		p.send(Message{Type: MsgGetChain})
		return
	}
	if err := core.ValidateHeaderChain(headers, parent.Hash, parent.Index); err != nil {
		if n.bans.add(p.id, banBadHeaders) {
			log.Printf("banning peer id=%s: %v", short(p.id), err)
		}
		return
	}
	p.send(Message{Type: MsgGetBlocks, From: headers[0].Index, To: headers[len(headers)-1].Index})
}

// onBlocks applies a downloaded batch of block bodies. A batch that extends our
// tip is appended block by block; a batch that starts below our tip is a fork
// suffix, applied as a reorg (transferring only the divergent blocks). Deep or
// losing forks fall back to a whole-chain exchange.
func (n *Node) onBlocks(p *peer, blocks []core.Block) {
	if len(blocks) == 0 {
		return
	}
	first := blocks[0].Index

	if first <= n.chain.Height() {
		// Fork suffix: reorg from the common ancestor (first-1).
		adopted, err := n.chain.ReorgFrom(first-1, blocks)
		if err != nil || !adopted {
			p.send(Message{Type: MsgGetChain}) // deep/losing fork: fall back
			return
		}
		for _, b := range blocks {
			n.markSeenBlock(b.Hash)
		}
		n.afterNewBlock(true)
		tip := n.chain.Tip()
		log.Printf("reorged onto fork, height=%d %s", tip.Index, short(tip.Hash))
		n.broadcastExcept(Message{Type: MsgInv, Index: tip.Index, Hash: tip.Hash}, p)
		if len(blocks) == maxBlocksBatch { // fork may be deeper than one batch
			p.send(n.getHeadersMsg())
		}
		return
	}

	// Extension: append the batch in order.
	applied := 0
	for i := range blocks {
		if err := n.chain.AddBlock(blocks[i]); err != nil {
			break
		}
		n.markSeenBlock(blocks[i].Hash)
		applied++
	}
	if applied == 0 {
		p.send(n.getHeadersMsg()) // our tip moved under us; re-sync
		return
	}
	n.afterNewBlock(false)
	tip := n.chain.Tip()
	log.Printf("synced %d block(s), height=%d", applied, tip.Index)
	n.broadcastExcept(Message{Type: MsgInv, Index: tip.Index, Hash: tip.Hash}, p)
	if applied == len(blocks) { // a full batch: there may be more
		p.send(n.getHeadersMsg())
	}
}

// getHeadersMsg builds a headers request carrying our block locator.
func (n *Node) getHeadersMsg() Message {
	return Message{Type: MsgGetHeaders, Locator: n.chain.Locator()}
}

func (n *Node) addPeer(p *peer) {
	n.peersMu.Lock()
	n.peers[p] = true
	n.peersMu.Unlock()
}

func (n *Node) removePeer(p *peer) {
	n.peersMu.Lock()
	delete(n.peers, p)
	n.peersMu.Unlock()
	_ = p.conn.Close()
}

func (n *Node) broadcast(m Message) { n.broadcastExcept(m, nil) }

func (n *Node) broadcastExcept(m Message, except *peer) {
	n.peersMu.Lock()
	targets := make([]*peer, 0, len(n.peers))
	for p := range n.peers {
		if p != except {
			targets = append(targets, p)
		}
	}
	n.peersMu.Unlock()
	for _, p := range targets {
		p.send(m)
	}
}

func (n *Node) markSeenBlock(h string) bool { return n.seenBlk.seen(h) }
func (n *Node) markSeenTx(h string) bool    { return n.seenTx.seen(h) }

func (n *Node) onTipChanged() { atomic.AddInt64(&n.tipGen, 1) }

// afterNewBlock runs the bookkeeping common to accepting/adopting new blocks and
// emits a real-time event (reorg=true when the tip changed via a reorg).
// (Persistence is automatic in the chain's append-only store.)
func (n *Node) afterNewBlock(reorg bool) {
	n.onTipChanged()
	n.reconcileMempool()
	n.publishBlock(reorg)
}

// buildBlock assembles the next candidate block on the current tip: a coinbase to
// the node wallet plus mempool-selected transactions. It returns the candidate
// and the selected (non-coinbase) transactions so the caller can drop them from
// the mempool once the block is committed.
func (n *Node) buildBlock() (core.Block, []core.Transaction) {
	tip := n.chain.Tip()
	height := tip.Index + 1
	baseFee := n.chain.NextBaseFee()
	txs := n.mempool.Select(n.chain, core.MaxBlockTxs) // already excludes fee < baseFee
	// Miner is paid the subsidy plus tips (fees above the base fee); the base-fee
	// portion is burned.
	coinbase := core.NewCoinbase(n.wallet.Address(), core.CoinbaseAmount(height, txs, baseFee))
	ts := time.Now().Unix()
	if ts <= tip.Timestamp {
		ts = tip.Timestamp + 1
	}
	candidate := core.Block{
		Index:        height,
		Timestamp:    ts,
		Transactions: append([]core.Transaction{coinbase}, txs...),
		PrevHash:     tip.Hash,
		BaseFee:      baseFee,
		Difficulty:   n.chain.NextDifficulty(),
	}
	// Commit the post-block account state root (part of the PoW-hashed header).
	// On error the candidate keeps an empty root and AddBlock will reject it, so
	// the miner simply rebuilds — no invalid block escapes.
	candidate.StateRoot, _ = n.chain.NextStateRoot(candidate)
	return candidate, txs
}

// commitMined records a freshly mined block: append it, dedup, drop its
// transactions from the mempool, announce it by inventory, and emit an event.
func (n *Node) commitMined(mined core.Block, txs []core.Transaction) error {
	if err := n.chain.AddBlock(mined); err != nil {
		return err
	}
	n.markSeenBlock(mined.Hash)
	log.Printf("mined block %d diff=%d txs=%d reward=%s %s",
		mined.Index, mined.Difficulty, len(txs), core.FormatAmount(core.BlockReward(mined.Index)), short(mined.Hash))
	n.mempool.Remove(txs)
	n.onTipChanged()
	// Announce the new block by inventory; peers pull the body if they lack it.
	n.broadcast(Message{Type: MsgInv, Index: mined.Index, Hash: mined.Hash})
	n.publishBlock(false)
	return nil
}

func (n *Node) mineLoop() {
	log.Printf("miner ready for %s (mining=%v)", n.wallet.Address(), n.Mining())
	for {
		if !n.mining.Load() {
			time.Sleep(200 * time.Millisecond) // paused; poll for the toggle
			continue
		}
		candidate, txs := n.buildBlock()

		// When idle, wait roughly one target interval before minting an empty
		// (coinbase-only) block, so we don't spam the network. Interruptible if
		// a new tip arrives meanwhile.
		if len(txs) == 0 && !n.sleepInterruptible(time.Duration(core.TargetBlockTime)*time.Second) {
			continue
		}

		startGen := atomic.LoadInt64(&n.tipGen)
		mined, ok := core.Mine(candidate, func() bool {
			return atomic.LoadInt64(&n.tipGen) != startGen
		})
		if !ok {
			continue // tip changed; rebuild on the new tip
		}
		if err := n.commitMined(mined, txs); err != nil {
			log.Printf("discarded our block %d (lost the race): %v", mined.Index, err)
		}
	}
}

// Generate mines count blocks immediately to the node wallet, bypassing the idle
// interval and the mining toggle. It is the regtest primitive for producing
// blocks on demand (like Bitcoin's generatetoaddress), returning the mined block
// hashes. Each block is rebuilt on the current tip and retried a few times if a
// concurrent miner moves the tip underneath it.
func (n *Node) Generate(count int) ([]string, error) {
	if n.wallet == nil {
		return nil, fmt.Errorf("node has no wallet to mine to")
	}
	hashes := make([]string, 0, count)
	for i := 0; i < count; i++ {
		committed := false
		for attempt := 0; attempt < 5 && !committed; attempt++ {
			candidate, txs := n.buildBlock()
			mined, ok := core.Mine(candidate, nil)
			if !ok {
				continue
			}
			if err := n.commitMined(mined, txs); err != nil {
				continue // tip moved under us; rebuild and retry
			}
			hashes = append(hashes, mined.Hash)
			committed = true
		}
		if !committed {
			return hashes, fmt.Errorf("failed to mine block %d after retries", i+1)
		}
	}
	return hashes, nil
}

// sleepInterruptible sleeps up to d, returning false early if the tip changes.
func (n *Node) sleepInterruptible(d time.Duration) bool {
	start := atomic.LoadInt64(&n.tipGen)
	steps := int(d / (50 * time.Millisecond))
	for i := 0; i < steps; i++ {
		if atomic.LoadInt64(&n.tipGen) != start {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// reconcileMempool drops transactions that a new block made unmineable: those
// whose nonce is already confirmed, and those that have expired.
func (n *Node) reconcileMempool() {
	n.mempool.PruneExpired(n.chain.Height())
	for _, tx := range n.mempool.All() {
		if tx.Nonce < n.chain.Account(tx.From).Nonce {
			n.mempool.Remove([]core.Transaction{tx})
		}
	}
}

func short(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

// remoteIP returns the IP portion of a connection's remote address (the ban key).
func remoteIP(c net.Conn) string {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return host
}

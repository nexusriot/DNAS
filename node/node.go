// Package node wires the blockchain, mempool and wallet to an authenticated,
// encrypted TCP peer network and (optionally) a miner. It handles block and
// transaction gossip, chain sync, most-work consensus, and peer discovery.
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
	DBPath        string   // chain persistence file ("" disables saving)
}

type peer struct {
	conn *secureConn
	enc  *json.Encoder
	mu   sync.Mutex // serializes writes to enc
	addr string     // peer's advertised address (from hello)
}

func (p *peer) send(m Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.enc.Encode(m)
}

// Node is a running participant in the DNAS network.
type Node struct {
	cfg     Config
	chain   *core.Blockchain
	mempool *core.Mempool
	wallet  *wallet.Wallet
	psk     []byte

	peersMu sync.Mutex
	peers   map[*peer]bool

	seenBlk *seenSet
	seenTx  *seenSet
	book    *peerbook

	tipGen int64 // atomic; bumped whenever the tip changes to interrupt mining
}

// New constructs a Node. The wallet enables mining and API-side signing.
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
	return &Node{
		cfg:     cfg,
		chain:   chain,
		mempool: mp,
		wallet:  w,
		psk:     []byte(cfg.NetKey),
		peers:   map[*peer]bool{},
		seenBlk: newSeenSet(seenCapacity),
		seenTx:  newSeenSet(seenCapacity),
		book:    newPeerbook(cfg.AdvertiseAddr, cfg.MaxPeers),
	}
}

// Start begins listening, dials the seed peers, and starts mining if enabled.
func (n *Node) Start() {
	go n.listen()
	for _, a := range n.cfg.Peers {
		n.book.note(a)
		n.maybeDial(a)
	}
	if n.cfg.Mine {
		if n.wallet == nil {
			log.Print("mining requested but no wallet; disabled")
		} else {
			go n.mineLoop()
		}
	}
}

// --- accessors used by the API/CLI -----------------------------------------

func (n *Node) Chain() *core.Blockchain { return n.chain }
func (n *Node) Mempool() *core.Mempool  { return n.mempool }
func (n *Node) Wallet() *wallet.Wallet  { return n.wallet }

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
	}
	return nil
}

// --- networking -------------------------------------------------------------

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
		log.Printf("connected to peer %s", addr)
		n.handleConn(conn) // blocks until the connection drops
		log.Printf("peer %s disconnected; will retry", addr)
		time.Sleep(3 * time.Second)
	}
}

func (n *Node) handleConn(rawConn net.Conn) {
	sc, err := secureHandshake(rawConn, n.psk)
	if err != nil {
		log.Printf("handshake with %s failed: %v", rawConn.RemoteAddr(), err)
		_ = rawConn.Close()
		return
	}
	p := &peer{conn: sc, enc: json.NewEncoder(sc)}
	n.addPeer(p)
	defer n.removePeer(p)

	// Handshake: introduce ourselves, ask for their chain and their peers.
	p.send(Message{Type: MsgHello, Addr: n.cfg.AdvertiseAddr})
	p.send(Message{Type: MsgGetChain})
	p.send(Message{Type: MsgGetPeers})

	dec := json.NewDecoder(sc)
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
		p.addr = m.Addr
		n.book.note(m.Addr)

	case MsgGetChain:
		p.send(Message{Type: MsgChain, Chain: n.chain.Blocks()})

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

	case MsgChain:
		replaced, err := n.chain.ReplaceChain(m.Chain)
		if err != nil || !replaced {
			return
		}
		log.Printf("adopted heavier chain (height=%d)", n.chain.Height())
		n.onTipChanged()
		n.reconcileMempool()
		n.save()

	case MsgBlock:
		if m.Block == nil || n.markSeenBlock(m.Block.Hash) {
			return
		}
		if err := n.chain.AddBlock(*m.Block); err != nil {
			// We may be behind; ask for the full chain to catch up.
			p.send(Message{Type: MsgGetChain})
			return
		}
		log.Printf("accepted block %d %s", m.Block.Index, short(m.Block.Hash))
		n.onTipChanged()
		n.reconcileMempool()
		n.save()
		n.broadcastExcept(Message{Type: MsgBlock, Block: m.Block}, p)

	case MsgTx:
		if m.Tx == nil || n.markSeenTx(m.Tx.Hash()) {
			return
		}
		if m.Tx.IsExpiredAt(n.chain.Height() + 1) {
			return
		}
		added, err := n.mempool.Add(*m.Tx)
		if err != nil || !added {
			return
		}
		n.broadcastExcept(Message{Type: MsgTx, Tx: m.Tx}, p)
	}
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

// --- mining -----------------------------------------------------------------

func (n *Node) onTipChanged() { atomic.AddInt64(&n.tipGen, 1) }

func (n *Node) mineLoop() {
	log.Printf("mining to %s", n.wallet.Address())
	for {
		tip := n.chain.Tip()
		height := tip.Index + 1
		difficulty := n.chain.NextDifficulty()
		txs := n.mempool.Select(n.chain, core.MaxBlockTxs)

		// When idle, wait roughly one target interval before minting an empty
		// (coinbase-only) block, so we don't spam the network. Interruptible if
		// a new tip arrives meanwhile.
		if len(txs) == 0 && !n.sleepInterruptible(time.Duration(core.TargetBlockTime)*time.Second) {
			continue
		}

		var fees uint64
		for _, tx := range txs {
			fees += tx.Fee
		}
		coinbase := core.NewCoinbase(n.wallet.Address(), core.BlockReward(height)+fees)
		blockTxs := append([]core.Transaction{coinbase}, txs...)

		ts := time.Now().Unix()
		if ts <= tip.Timestamp {
			ts = tip.Timestamp + 1
		}
		candidate := core.Block{
			Index:        height,
			Timestamp:    ts,
			Transactions: blockTxs,
			PrevHash:     tip.Hash,
			Difficulty:   difficulty,
		}

		startGen := atomic.LoadInt64(&n.tipGen)
		mined, ok := core.Mine(candidate, func() bool {
			return atomic.LoadInt64(&n.tipGen) != startGen
		})
		if !ok {
			continue // tip changed; rebuild on the new tip
		}
		if err := n.chain.AddBlock(mined); err != nil {
			log.Printf("discarded our block %d (lost the race): %v", mined.Index, err)
			continue
		}
		n.markSeenBlock(mined.Hash)
		log.Printf("mined block %d diff=%d txs=%d reward=%s %s",
			mined.Index, mined.Difficulty, len(txs), core.FormatAmount(core.BlockReward(height)), short(mined.Hash))
		n.mempool.Remove(txs)
		n.onTipChanged()
		n.save()
		n.broadcast(Message{Type: MsgBlock, Block: &mined})
	}
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

// --- misc -------------------------------------------------------------------

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

func (n *Node) save() {
	if n.cfg.DBPath == "" {
		return
	}
	if err := n.chain.Save(n.cfg.DBPath); err != nil {
		log.Printf("save chain: %v", err)
	}
}

func short(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

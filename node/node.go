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

	// Liveness: an established peer is pinged every pingInterval and must send
	// something (a pong counts) within peerIdleTimeout or the connection is
	// dropped. The timeout is comfortably larger than the interval so a single
	// missed ping doesn't disconnect a healthy but briefly slow peer.
	pingInterval    = 30 * time.Second
	peerIdleTimeout = 90 * time.Second

	// Eclipse/DoS resistance for an open network: cap total inbound connections and
	// inbound connections per IP group (/16), so an attacker can't monopolise a
	// node's inbound slots without controlling many address ranges. Loopback is
	// exempt (local demos/tests). Outbound dials stay capped by MaxPeers.
	maxInbound         = 64
	maxInboundPerGroup = 4

	// Per-peer inbound message rate limit (token bucket): sustained msgs/sec + burst.
	msgRatePerSec = 100.0
	msgRateBurst  = 200.0

	// getChainCooldown throttles the expensive whole-chain request per peer.
	getChainCooldown = 10 * time.Second
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
	Dandelion     bool     // relay new transactions via Dandelion++ stem/fluff (origin privacy)
}

type peer struct {
	conn         *secureConn
	enc          *json.Encoder
	mu           sync.Mutex      // serializes writes to enc
	addr         string          // peer's advertised address (from hello)
	id           string          // peer's authenticated identity public key
	ip           string          // remote IP, for ban scoring
	version      int             // negotiated protocol version
	caps         map[string]bool // advertised capabilities (e.g. Dandelion++)
	lastGetChain time.Time       // throttles the expensive whole-chain request
}

// supports reports whether the peer advertised a capability.
func (p *peer) supports(cap string) bool { return p.caps[cap] }

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

	inboundMu    sync.Mutex     // guards the inbound-connection counters below
	inboundTotal int            // current inbound connections (loopback exempt)
	inboundGroup map[string]int // current inbound connections per IP group

	seenBlk *seenSet
	seenTx  *seenSet
	book    *peerbook
	bans    *banbook
	events  *eventBus
	dand    *dandelion

	mining atomic.Bool // whether the miner is currently active (toggle at runtime)
	tipGen int64       // atomic; bumped whenever the tip changes to interrupt mining
	txGen  int64       // atomic; bumped when a new tx enters the mempool, to wake an idle miner

	// Transport, injectable so tests can drive nodes over an in-memory network
	// with controllable latency and partitions. Production uses TCP.
	dialFn   func(string) (net.Conn, error)
	listenFn func(string) (net.Listener, error)
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
	// An empty NetKey means an OPEN, permissionless network: the handshake is still
	// X25519-ECDH + AES-256-GCM encrypted, but not gated on a shared secret, so
	// anyone may connect (peers are still cryptographically identified by their
	// Ed25519 node identity for ban scoring). A non-empty NetKey authenticates a
	// private network (e.g. a devnet or regtest). It is NOT defaulted to a shared
	// key any more — a real cryptocurrency is permissionless by default.
	identity := w
	if identity == nil {
		identity, _ = wallet.New()
	}
	n := &Node{
		cfg:          cfg,
		chain:        chain,
		mempool:      mp,
		wallet:       w,
		identity:     identity,
		psk:          []byte(cfg.NetKey),
		peers:        map[*peer]bool{},
		inboundGroup: map[string]int{},
		seenBlk:      newSeenSet(seenCapacity),
		seenTx:       newSeenSet(seenCapacity),
		book:         newPeerbook(cfg.AdvertiseAddr, cfg.MaxPeers),
		bans:         newBanbook(banThreshold),
		events:       newEventBus(),
		dand:         newDandelion(),
	}
	n.dialFn = func(addr string) (net.Conn, error) { return net.Dial("tcp", addr) }
	n.listenFn = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
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
	n.dand.stopAll() // cancel any pending Dandelion++ embargo timers
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
		n.onNewTx()              // wake an idle miner so the tx isn't stuck behind the block interval
		n.relayTx(tx, nil, true) // originate on the Dandelion++ stem (origin privacy)
		n.publishTx(tx)
	}
	return nil
}

func (n *Node) listen() {
	ln, err := n.listenFn(n.cfg.ListenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", n.cfg.ListenAddr, err)
	}
	log.Printf("p2p listening on %s (advertising %s)", n.cfg.ListenAddr, n.cfg.AdvertiseAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		// Eclipse/DoS guard: reject the connection before the handshake if it would
		// exceed the total or per-IP-group inbound cap.
		ip := remoteIP(conn)
		if !n.admitInbound(ip) {
			_ = conn.Close()
			continue
		}
		go func() {
			defer n.releaseInbound(ip)
			n.handleConn(conn)
		}()
	}
}

// ipGroup returns the coarse network group an IP belongs to (its /16 for IPv4,
// /32 for IPv6), used to bound how many inbound connections one address range may
// hold — an attacker then needs many distinct ranges to eclipse a node.
func ipGroup(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return fmt.Sprintf("v4:%d.%d", v4[0], v4[1])
	}
	return "v6:" + p.Mask(net.CIDRMask(32, 128)).String()
}

// admitInbound reports whether a new inbound connection from ip is allowed under
// the total and per-group caps, reserving a slot if so. Loopback (and unparseable
// addresses) bypass the caps so local demos/tests with many 127.0.0.1 peers work.
func (n *Node) admitInbound(ip string) bool {
	if !bannableIP(ip) { // loopback / unparseable: exempt
		return true
	}
	n.inboundMu.Lock()
	defer n.inboundMu.Unlock()
	if n.inboundTotal >= maxInbound {
		return false
	}
	g := ipGroup(ip)
	if n.inboundGroup[g] >= maxInboundPerGroup {
		return false
	}
	n.inboundTotal++
	n.inboundGroup[g]++
	return true
}

// releaseInbound frees the slot reserved by admitInbound when the connection ends.
func (n *Node) releaseInbound(ip string) {
	if !bannableIP(ip) {
		return
	}
	n.inboundMu.Lock()
	defer n.inboundMu.Unlock()
	n.inboundTotal--
	g := ipGroup(ip)
	if n.inboundGroup[g]--; n.inboundGroup[g] <= 0 {
		delete(n.inboundGroup, g)
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
		conn, err := n.dialFn(addr)
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

	// Establish the encrypted channel. On a private net (non-empty netkey) this
	// also authenticates network membership; on an open net it is anonymous
	// encryption. A failed handshake is a crypto/protocol error or the wrong
	// netkey; score it by IP so a persistent prober is eventually cut off. Loopback
	// is exempt so many local nodes sharing 127.0.0.1 (tests, demos) don't ban each
	// other. (Peer identity is still proven via Ed25519 below, open net or not.)
	sc, sid, err := secureHandshake(rawConn, n.psk)
	if err != nil {
		log.Printf("rejected peer %s: handshake failed: %v", ip, err)
		if bannableIP(ip) && n.bans.add(ip, banHandshake) {
			log.Printf("banning ip %s: repeated handshake failures", ip)
		}
		_ = rawConn.Close()
		return
	}

	p := &peer{conn: sc, enc: json.NewEncoder(sc), ip: ip}
	dec := json.NewDecoder(sc)

	// Authenticated identity exchange: each side proves it holds its identity
	// key by signing the session id, binding the identity to this session.
	p.send(Message{Type: MsgIdentity, PubKey: n.identity.PublicKeyHex(), Sig: n.identity.Sign(sid)})
	_ = sc.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var idm Message
	if err := dec.Decode(&idm); err != nil {
		_ = sc.Close()
		return
	}
	if idm.Type != MsgIdentity || !wallet.Verify(idm.PubKey, idm.Sig, sid) {
		log.Printf("rejected peer %s: identity authentication failed", ip)
		if bannableIP(ip) {
			n.bans.add(ip, banHandshake)
		}
		_ = sc.Close()
		return
	}
	p.id = idm.PubKey
	if n.bans.banned(p.id) {
		_ = sc.Close()
		return
	}

	// Protocol version + capability negotiation: drop peers speaking an
	// incompatible version, and record capabilities (e.g. Dandelion++) for feature
	// gating.
	p.send(Message{Type: MsgVersion, Version: ProtocolVersion, Caps: n.caps()})
	_ = sc.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var vm Message
	if err := dec.Decode(&vm); err != nil {
		_ = sc.Close()
		return
	}
	if vm.Type != MsgVersion || vm.Version < MinProtocolVersion {
		log.Printf("rejected peer %s: incompatible protocol version %d", ip, vm.Version)
		_ = sc.Close()
		return
	}
	p.version = vm.Version
	p.caps = make(map[string]bool, len(vm.Caps))
	for _, c := range vm.Caps {
		p.caps[c] = true
	}

	n.addPeer(p)
	defer n.removePeer(p)
	log.Printf("peer connected %s id=%s", ip, short(p.id))

	// Keep the connection alive: ping the peer periodically and drop it if it
	// falls silent past the idle timeout (a half-open/dead connection).
	stopPing := make(chan struct{})
	defer close(stopPing)
	go n.pingLoop(p, stopPing)

	// Introduce ourselves, request peers, and start headers-first catch-up
	// (a block locator lets the peer find our fork point cheaply).
	p.send(Message{Type: MsgHello, Addr: n.cfg.AdvertiseAddr})
	p.send(Message{Type: MsgGetPeers})
	p.send(n.getHeadersMsg())

	limiter := newRateLimiter(msgRatePerSec, msgRateBurst)
	for {
		_ = sc.SetReadDeadline(time.Now().Add(peerIdleTimeout))
		var m Message
		if err := dec.Decode(&m); err != nil {
			return
		}
		if !limiter.allow(time.Now()) {
			log.Printf("dropping peer %s: inbound message rate exceeded", ip)
			return
		}
		n.handleMessage(p, m)
	}
}

// pingLoop sends a keepalive ping to p at a fixed interval until the connection
// closes (stop is closed by handleConn's defer).
func (n *Node) pingLoop(p *peer, stop <-chan struct{}) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.send(Message{Type: MsgPing})
		}
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
		if m.Tx == nil {
			return
		}
		h := m.Tx.Hash()
		if !m.Stem {
			n.dand.cancelEmbargo(h) // it's fluffing on the network; stop our embargo
		}
		if n.markSeenTx(h) {
			return
		}
		if m.Tx.IsExpiredAt(n.chain.Height() + 1) {
			return
		}
		if added, err := n.mempool.Add(*m.Tx); err != nil || !added {
			return
		}
		n.onNewTx() // wake an idle miner
		n.relayTx(*m.Tx, p, m.Stem)
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
		// A block that is malformed on its own terms (bad PoW or merkle root) is
		// peer misbehaviour and earns ban points; one that is well-formed but
		// doesn't link to our tip is just a fork, handled below with a re-sync.
		if err := m.Block.SelfValid(); err != nil {
			if n.bans.add(p.id, banInvalidBlock) {
				log.Printf("banning peer id=%s: invalid block: %v", short(p.id), err)
			}
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

	// whole-chain exchange, used only as a fork/bootstrap fallback — throttled
	// per peer because serializing the whole chain is expensive.
	case MsgGetChain:
		if !p.lastGetChain.IsZero() && time.Since(p.lastGetChain) < getChainCooldown {
			return
		}
		p.lastGetChain = time.Now()
		p.send(Message{Type: MsgChain, Chain: n.chain.Blocks()})

	case MsgChain:
		if replaced, err := n.chain.ReplaceChain(m.Chain); err == nil && replaced {
			log.Printf("adopted chain via fallback (height=%d)", n.chain.Height())
			n.afterNewBlock(true)
		}

	case MsgPing:
		p.send(Message{Type: MsgPong})

	case MsgPong:
		// Liveness only: receiving anything already reset the read deadline.
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

// caps returns this node's advertised protocol capabilities.
func (n *Node) caps() []string {
	var c []string
	if n.cfg.Dandelion {
		c = append(c, CapDandelion)
	}
	return c
}

// relayTx propagates a transaction using Dandelion++ when enabled: in the stem
// phase it forwards to a single epoch-stable successor (or fluffs by chance, on
// embargo timeout, or when no Dandelion-capable successor exists); a fluff-phase
// transaction is broadcast to all peers. With Dandelion disabled it is a plain
// broadcast, exactly as before.
func (n *Node) relayTx(tx core.Transaction, from *peer, stemPhase bool) {
	if !n.cfg.Dandelion || !stemPhase {
		n.fluff(tx, from)
		return
	}
	sp := n.stemSuccessor(from)
	if sp == nil || n.dand.rollFluff() {
		n.fluff(tx, from)
		return
	}
	sp.send(Message{Type: MsgTx, Tx: &tx, Stem: true})
	n.dand.startEmbargo(tx.Hash(), func() { n.fluff(tx, nil) })
}

// fluff broadcasts a transaction to every peer except one, ending its stem phase.
func (n *Node) fluff(tx core.Transaction, except *peer) {
	n.broadcastExcept(Message{Type: MsgTx, Tx: &tx}, except)
}

// stemSuccessor returns this epoch's Dandelion++ stem successor: a random,
// epoch-stable connected peer that supports Dandelion, excluding one peer.
func (n *Node) stemSuccessor(exclude *peer) *peer {
	n.peersMu.Lock()
	var cands []*peer
	for p := range n.peers {
		if p != exclude && p.supports(CapDandelion) {
			cands = append(cands, p)
		}
	}
	n.peersMu.Unlock()
	return n.dand.pick(cands, time.Now())
}

func (n *Node) markSeenBlock(h string) bool { return n.seenBlk.seen(h) }
func (n *Node) markSeenTx(h string) bool    { return n.seenTx.seen(h) }

func (n *Node) onTipChanged() { atomic.AddInt64(&n.tipGen, 1) }

// onNewTx signals that a transaction entered the mempool, so a miner idling
// between blocks wakes immediately to build a block including it instead of
// waiting out the target block interval.
func (n *Node) onNewTx() { atomic.AddInt64(&n.txGen, 1) }

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
	return n.buildBlockFor(n.wallet.Address())
}

// buildBlockFor assembles the next candidate block paying the coinbase to
// minerAddr. The internal miner passes its own wallet; BuildTemplate passes an
// external miner's address.
func (n *Node) buildBlockFor(minerAddr string) (core.Block, []core.Transaction) {
	tip := n.chain.Tip()
	height := tip.Index + 1
	baseFee := n.chain.NextBaseFee()
	txs := n.mempool.Select(n.chain, core.MaxBlockTxs) // already excludes fee < baseFee
	// Miner is paid the subsidy plus tips (fees above the base fee); the base-fee
	// portion is burned.
	coinbase := core.NewCoinbase(minerAddr, core.CoinbaseAmount(height, txs, baseFee))
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
		Bits:         n.chain.NextBits(),
	}
	candidate.MerkleRoot = core.MerkleRoot(candidate.Transactions)
	// Commit the post-block account state root (part of the PoW-hashed header).
	// On error the candidate keeps an empty root and AddBlock will reject it, so
	// the miner simply rebuilds — no invalid block escapes.
	candidate.StateRoot, _ = n.chain.NextStateRoot(candidate)
	return candidate, txs
}

// BuildTemplate returns a candidate block to mine paying minerAddr — every field
// filled in except the winning Nonce. An external miner (see `dnas miner`)
// searches for a Nonce whose hash meets Bits, then submits it via
// SubmitMinedBlock / POST /submitblock.
func (n *Node) BuildTemplate(minerAddr string) (core.Block, error) {
	if minerAddr == "" {
		return core.Block{}, fmt.Errorf("miner address required")
	}
	b, _ := n.buildBlockFor(minerAddr)
	if b.StateRoot == "" {
		return core.Block{}, fmt.Errorf("could not compute candidate state root")
	}
	return b, nil
}

// SubmitMinedBlock accepts a block mined by an external miner: it validates and
// appends it, drops its transactions from the mempool, wakes the internal miner,
// and announces it. Returns an error if the block is invalid or no longer builds
// on the tip (the miner should then fetch a fresh template).
func (n *Node) SubmitMinedBlock(b core.Block) error {
	if err := n.chain.AddBlock(b); err != nil {
		return err
	}
	n.markSeenBlock(b.Hash)
	if len(b.Transactions) > 1 {
		n.mempool.Remove(b.Transactions[1:])
	}
	log.Printf("accepted externally-mined block %d diff=%.2f %s",
		b.Index, core.TargetDifficulty(b.Bits), short(b.Hash))
	n.afterNewBlock(false)
	n.broadcast(Message{Type: MsgInv, Index: b.Index, Hash: b.Hash})
	return nil
}

// commitMined records a freshly mined block: append it, dedup, drop its
// transactions from the mempool, announce it by inventory, and emit an event.
func (n *Node) commitMined(mined core.Block, txs []core.Transaction) error {
	if err := n.chain.AddBlock(mined); err != nil {
		return err
	}
	n.markSeenBlock(mined.Hash)
	log.Printf("mined block %d diff=%.2f txs=%d reward=%s %s",
		mined.Index, core.TargetDifficulty(mined.Bits), len(txs), core.FormatAmount(core.BlockReward(mined.Index)), short(mined.Hash))
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

// sleepInterruptible sleeps up to d, returning false early if the tip changes or
// a new transaction arrives (so the idle miner rebuilds promptly rather than
// waiting out the whole interval).
func (n *Node) sleepInterruptible(d time.Duration) bool {
	tip := atomic.LoadInt64(&n.tipGen)
	tx := atomic.LoadInt64(&n.txGen)
	steps := int(d / (50 * time.Millisecond))
	for i := 0; i < steps; i++ {
		if atomic.LoadInt64(&n.tipGen) != tip || atomic.LoadInt64(&n.txGen) != tx {
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

// remoteIP returns the IP portion of a connection's remote address (the ban key
// for pre-identity misbehaviour such as failed handshakes).
func remoteIP(c net.Conn) string {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return host
}

// bannableIP reports whether an IP should accrue ban points. Loopback is never
// banned so that many local nodes sharing 127.0.0.1 (tests, demos, a regtest
// setup) don't ban one another over a single misbehaving local process.
func bannableIP(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && !parsed.IsLoopback()
}

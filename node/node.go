// Package node wires the blockchain, mempool and wallet to an authenticated,
// encrypted TCP peer network and (optionally) a miner. It handles block and
// transaction gossip, headers-first ranged sync, most-work consensus with
// reorgs, peer discovery, node-identity authentication and ban scoring.
package node

import (
	"encoding/json"
	"errors"
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

	// dialRetryInterval is how long a dial loop waits before redialing a peer,
	// whether the dial failed or an established connection dropped. It is the
	// FIRST wait; consecutive failures double it up to dialRetryMax (see
	// dialLoop), because a peer that is gone stays gone.
	dialRetryInterval = 3 * time.Second

	// dialRetryMax caps that backoff. A node with a handful of dead seed
	// addresses was dialing each one every 3 seconds forever: 1200 connection
	// attempts an hour per address, for the whole uptime of the node. That is
	// pointless work here and unsolicited traffic at the other end, which for a
	// host that no longer runs a node looks exactly like being scanned.
	//
	// Five minutes is late enough to be cheap and early enough that a peer coming
	// back is noticed without a restart.
	dialRetryMax = 5 * time.Minute
	// minePollInterval is how often a paused miner re-checks the mining toggle.
	minePollInterval = 200 * time.Millisecond
	// acceptRetryDelay backs the accept loop off after a transient accept error,
	// so a broken listener can't spin a core.
	acceptRetryDelay = 50 * time.Millisecond
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

	// DNSSeeds are hostnames whose A/AAAA records list nodes to bootstrap from,
	// consulted only when this node is short of addresses of its own (see
	// dnsseed.go). Each entry is "host" or "host:port". Empty means no
	// bootstrapping help: the node then needs -peers, as it always did.
	DNSSeeds []string

	// ShareFactor is how many times easier a mining share is than a block (see
	// shares.go). Zero means core.DefaultShareFactor.
	ShareFactor uint32

	// Faucet gives coin away from the node's wallet on request. It only takes
	// effect on a network whose parameters allow one (never mainnet) — see
	// faucet.go. FaucetAmount and FaucetCooldown default when zero.
	Faucet         bool
	FaucetAmount   uint64
	FaucetCooldown time.Duration

	// Identity is the node's NETWORK identity key: the Ed25519 key it proves
	// itself with to peers (see identity.go). It must not be the wallet key — a
	// peer can derive an address from the identity public key it is sent — so when
	// this is nil New falls back to the wallet only for in-process use and logs a
	// warning. Real nodes pass their own key (`-nodekey`).
	Identity *wallet.Wallet

	// Webhooks are URLs the node POSTs every event to, for a service that wants
	// to be called rather than hold an SSE connection open (see webhook.go).
	Webhooks []string

	// SignalBits are the BIP9 version bits this node's miner sets in every block
	// it produces (see core/versionbits.go). Setting a bit is a vote that this
	// node is ready to enforce the deployment claiming it — so it is deliberately
	// opt-in per operator rather than derived from the registered deployments: a
	// node that has merely been TOLD about a deployment has not thereby agreed to
	// it, and a bit set by a node whose operator has not chosen it would be a vote
	// nobody cast.
	SignalBits []uint8

	// EmptyBlockInterval is how long the miner waits before minting a block with
	// no transactions in it, so an idle network isn't flooded with empty blocks.
	// Zero means one TargetBlockTime, which is what a real network wants; devnets
	// and tests set it low to get blocks as fast as proof of work allows. It is
	// local mining policy, not consensus — peers need not agree on it.
	EmptyBlockInterval time.Duration
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
	askedMempool bool            // we have requested this peer's pending transactions
	inbound      bool            // they dialed us (rather than the other way round)
	since        time.Time       // when the connection was established
	// timeOffset is this peer's clock minus ours at handshake, in seconds. The
	// median across peers drives network-adjusted time (see core/nettime.go).
	timeOffset    int64
	hasTimeOffset bool
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
	txReq   *txRequests // transaction bodies requested but not yet received
	// assembly holds compact blocks waiting on the transactions this node lacked.
	assembly *blockAssembly
	// compactHit/compactMiss count reconstructions that produced the miner's
	// block and those that had to fall back to a full fetch, which is the number
	// that says whether compact relay is paying for itself here.
	compactHit  atomic.Int64
	compactMiss atomic.Int64
	shares      *shareLedger
	// window is the pool's PPLNS accounting over recent shares, and stratum the
	// miner-facing server that feeds it (pool.go, stratum.go). Both are nil-safe:
	// a node with no pool simply never touches them.
	window  *shareWindow
	stratum *StratumServer
	faucet  *faucet
	reorgs  *reorgLog
	book    *peerbook
	addrs   *addrman // outbound address manager: tried/new tables + group diversity
	bans    *banbook

	// resolveSeed is the DNS lookup used for -dnsseeds, injectable so tests need
	// no working resolver.
	resolveSeed seedResolver
	events      *eventBus
	webhooks    *webhookSender
	dand        *dandelion
	orphans     *orphanPool

	// Sync state (see sync.go): what ranged block requests are outstanding and to
	// whom, and the highest height any peer has announced. Without this a peer that
	// accepts a request and never answers stalls catch-up forever.
	syncMu     sync.Mutex
	inflight   map[*peer]*blockRequest
	bestHeight int64 // atomic

	mining atomic.Bool // whether the miner is currently active (toggle at runtime)
	tipGen int64       // atomic; bumped whenever the tip changes to interrupt mining
	txGen  int64       // atomic; bumped when a new tx enters the mempool, to wake an idle miner

	// Lifecycle: quit is closed once by Shutdown, which every background loop
	// (accept, dial, mine) selects on so the node leaves nothing running behind
	// it. ln is the accept listener, kept so Shutdown can unblock Accept.
	quit     chan struct{}
	stopOnce sync.Once
	lnMu     sync.Mutex
	ln       net.Listener

	// Transport, injectable so tests can drive nodes over an in-memory network
	// with controllable latency and partitions. Production uses TCP.
	dialFn   func(string) (net.Conn, error)
	dialBase time.Duration // first retry wait; a field so tests need not wait seconds
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
	// Network identity: its own key when one is supplied. Falling back to the
	// wallet lets a peer derive the wallet's address from the identity public key
	// every handshake sends, so it is warned about; falling back to an ephemeral
	// key (no wallet either) costs the node its peer reputation on restart, which
	// only matters to a real node and a real node has a wallet.
	identity := cfg.Identity
	if identity == nil {
		if identity = w; identity != nil {
			warnSharedIdentity(w.Address())
		} else {
			identity, _ = wallet.New()
		}
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
		txReq:        newTxRequests(),
		assembly:     newBlockAssembly(),
		window:       newShareWindow(pplnsWindow),
		shares:       newShareLedger(),
		faucet:       newFaucet(),
		reorgs:       newReorgLog(),
		book:         newPeerbook(cfg.AdvertiseAddr, cfg.MaxPeers),
		addrs:        newAddrman(),
		bans:         newBanbook(banThreshold),
		events:       newEventBus(),
		dand:         newDandelion(),
		orphans:      newOrphanPool(orphanPoolCapacity, orphanPoolBytes),
		inflight:     map[*peer]*blockRequest{},
		quit:         make(chan struct{}),
	}
	// Bind the mempool to the chain so admission can check what a sender has
	// actually confirmed and can actually spend. Without this the pool would accept
	// transactions that can never be mined — see core.Mempool. Sharing the chain's
	// validation cache means a transaction's signatures are verified once, on
	// admission, rather than again when the block carrying it is applied.
	mp.UseAccounts(chain).UseValidationCache(chain.ValidationCache())
	n.dialFn = func(addr string) (net.Conn, error) { return net.Dial("tcp", addr) }
	n.dialBase = dialRetryInterval
	n.webhooks = newWebhookSender(cfg.Webhooks, n.quit,
		core.NetworkName, func() uint64 { return n.chain.Height() })
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

// Shutdown stops the node: it halts mining, stops accepting and redialing peers,
// and closes all peer connections. It is safe to call more than once, and after
// it returns the node's background loops wind down rather than lingering (a
// leaked dial loop would keep reconnecting forever). The chain's store is closed
// separately by the owner (Blockchain.Close).
func (n *Node) Shutdown() {
	n.stopOnce.Do(func() { close(n.quit) })
	n.mining.Store(false)
	n.lnMu.Lock()
	if n.ln != nil {
		_ = n.ln.Close() // unblocks the accept loop
	}
	n.lnMu.Unlock()
	n.dand.stopAll() // cancel any pending Dandelion++ embargo timers
	if n.stratum != nil {
		n.stratum.Stop() // close miner sessions before the state below is written
	}
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
		// Configured peers are the operator's own choice, so they go in as
		// `tried`: they should be preferred over anything gossip offers.
		n.addrs.Add(a, "config")
		n.addrs.Good(a)
	}
	// Bootstrapping: if this node knows almost nobody, ask the DNS seeds. It is
	// skipped entirely once the node has addresses of its own, so a running
	// network never depends on a seed being up.
	if added := n.bootstrapFromDNSSeeds(); added > 0 {
		Infof("bootstrapped from DNS seeds", "addresses", added)
	}
	n.fillOutbound()
	go n.peerLoop()     // keeps the outbound set full and diverse as peers come and go
	go n.compactLoop()  // rewrites the pruned block store off the hot path
	go n.timeSyncLoop() // keeps timestamp validation in step with peers' clocks
	go n.syncLoop()     // times out unanswered block requests and keeps ranges in flight
	if n.webhooks != nil {
		go n.webhooks.run()
		Infof("webhooks enabled", "urls", len(n.webhooks.urls))
	}
	// The miner runs whenever the node has a wallet; the atomic flag gates
	// whether it actually produces blocks, so it can be toggled at runtime.
	if n.wallet != nil {
		go n.mineLoop()
	} else if n.cfg.Mine {
		Warnf("mining requested but no wallet; disabled")
	}
}

// stopped reports whether Shutdown has been called.
func (n *Node) stopped() bool {
	select {
	case <-n.quit:
		return true
	default:
		return false
	}
}

// wait sleeps for d, returning false if the node shuts down first. Background
// loops use it instead of time.Sleep so Shutdown doesn't have to wait out a
// retry interval.
func (n *Node) wait(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-n.quit:
		return false
	case <-t.C:
		return true
	}
}

// setListener publishes the accept listener so Shutdown can close it. It reports
// false if the node was already shut down, in which case the listener is closed
// here and the caller must stop — otherwise a Shutdown racing startup would
// leave the socket open forever.
func (n *Node) setListener(ln net.Listener) bool {
	n.lnMu.Lock()
	defer n.lnMu.Unlock()
	if n.stopped() {
		_ = ln.Close()
		return false
	}
	n.ln = ln
	return true
}

func (n *Node) Chain() *core.Blockchain { return n.chain }
func (n *Node) Mempool() *core.Mempool  { return n.mempool }
func (n *Node) Wallet() *wallet.Wallet  { return n.wallet }

// IdentityKey is this node's network identity public key (hex) — what peers know
// it by, and what ban scores are attributed to.
func (n *Node) IdentityKey() string { return n.identity.PublicKeyHex() }

// IdentityIsWallet reports whether the node is running with its wallet key as
// its network identity, which leaks the wallet address to every peer.
func (n *Node) IdentityIsWallet() bool {
	return n.wallet != nil && n.identity.PublicKeyHex() == n.wallet.PublicKeyHex()
}

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
	n.emit(Event{Type: typ, Height: tip.Index, Hash: tip.Hash, Txs: len(tip.Transactions)})
}

// publishTx emits a mempool-transaction event. A multi-recipient transfer is
// summarized by its total and recipient count, since one event carries one
// To/Amount pair.
func (n *Node) publishTx(tx core.Transaction) {
	e := Event{Type: "tx", Hash: tx.Hash(), From: tx.From, To: tx.To, Amount: tx.Amount, Fee: tx.Fee}
	if tx.IsMultiOutput() {
		total, _ := tx.TotalOut()
		e.Amount = total
		e.Outputs = len(tx.Outputs)
	}
	n.emit(e)
}

// emit publishes an event to the in-process subscribers AND to any webhooks, so
// there is one path and the two cannot come to carry different events.
func (n *Node) emit(e Event) {
	n.events.publish(e)
	n.webhooks.notify(e) // a nil sender is a no-op
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
	if !n.setListener(ln) {
		return
	}
	Infof("p2p listening", "listen", n.cfg.ListenAddr, "advertise", n.cfg.AdvertiseAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if n.stopped() { // Shutdown closed the listener
				return
			}
			time.Sleep(acceptRetryDelay) // transient (e.g. fd exhaustion): back off, don't spin
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
			n.handleConn(conn, true, "") // inbound: they dialed us
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
	// Every address a peer gossips passes through here, so this is where the
	// outbound eclipse defence has to live: the addrman refuses a reservation
	// once this address's network group already holds its share of the outbound
	// set (see addrman.go). The peerbook still owns the total cap.
	if !n.addrs.Reserve(addr) {
		return
	}
	if !n.book.shouldDial(addr) {
		n.addrs.Release(addr)
		return
	}
	go n.dialLoop(addr)
}

func (n *Node) dialLoop(addr string) {
	// The slot is held for as long as this loop lives, so a peer that keeps
	// reconnecting keeps its group's share rather than freeing it between
	// attempts and letting another group take it.
	defer n.addrs.Release(addr)
	base := n.dialBase
	if base <= 0 {
		base = dialRetryInterval
	}
	delay := base
	for !n.stopped() {
		n.addrs.Attempt(addr)
		if conn, err := n.dialFn(addr); err == nil {
			// A connection that was ESTABLISHED resets the backoff: this peer is real
			// and reachable, and a drop after an hour of good service should be
			// retried promptly rather than at whatever interval past failures reached.
			delay = base
			n.handleConn(conn, false, addr) // outbound; blocks until the connection drops
		} else {
			// A dial that never connected is evidence against the address; enough
			// of them and the addrman retires it rather than retrying forever.
			n.addrs.Failed(addr)
			delay = nextDialDelay(delay, base)
		}
		// A dial that turned out to reach ourselves is not retried: the peerbook
		// learned the address is us (see handleConn).
		if n.book.isSelf(addr) {
			n.addrs.Forget(addr)
			return
		}
		if !n.wait(delay) {
			return
		}
	}
}

// nextDialDelay doubles a retry delay, stopping at dialRetryMax.
func nextDialDelay(d, base time.Duration) time.Duration {
	if base <= 0 {
		base = dialRetryInterval
	}
	if d <= 0 {
		return base
	}
	if d >= dialRetryMax/2 {
		return dialRetryMax
	}
	return d * 2
}

func (n *Node) handleConn(rawConn net.Conn, inbound bool, dialed string) {
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
		Warnf("peer rejected", "ip", ip, "reason", "handshake failed", "err", err)
		if bannableIP(ip) && n.bans.add(ip, banHandshake) {
			Warnf("peer banned", "ip", ip, "reason", "repeated handshake failures")
		}
		_ = rawConn.Close()
		return
	}

	p := &peer{conn: sc, enc: json.NewEncoder(sc), ip: ip, inbound: inbound, since: time.Now()}
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
		Warnf("peer rejected", "ip", ip, "reason", "identity authentication failed")
		if bannableIP(ip) {
			n.bans.add(ip, banHandshake)
		}
		_ = sc.Close()
		return
	}
	p.id = idm.PubKey
	// A connection to OURSELVES: two peers cannot be told apart by address (we
	// advertise ":3000" while a peer reaches us as "localhost:3000", and a string
	// comparison sees two different hosts), but they can by identity. Left
	// unchecked this burns an outbound slot, an inbound slot and a goroutine on a
	// loop back to the same process, and gossips the alias onward so other nodes
	// dial us twice.
	if p.id == n.identity.PublicKeyHex() {
		if dialed != "" {
			n.book.noteSelf(dialed)
			Infof("stopped dialing our own address", "addr", dialed)
		}
		_ = sc.Close()
		return
	}
	if n.bans.banned(p.id) {
		_ = sc.Close()
		return
	}

	// Protocol version + capability negotiation: drop peers speaking an
	// incompatible version, and record capabilities (e.g. Dandelion++) for feature
	// gating.
	p.send(Message{Type: MsgVersion, Version: ProtocolVersion, Caps: n.caps(),
		Network: core.NetworkName(), Time: time.Now().Unix()})
	_ = sc.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var vm Message
	if err := dec.Decode(&vm); err != nil {
		_ = sc.Close()
		return
	}
	if vm.Type != MsgVersion || vm.Version < MinProtocolVersion {
		Warnf("peer rejected", "ip", ip, "reason", "incompatible protocol version", "version", vm.Version)
		_ = sc.Close()
		return
	}
	// Different networks have different genesis blocks and mutually invalid
	// signatures, so peering across them can only produce a connection that never
	// converges. Drop it here instead. A peer that predates network names sends
	// none, which means mainnet.
	peerNet := vm.Network
	if peerNet == "" {
		peerNet = core.MainNet
	}
	if peerNet != core.NetworkName() {
		Warnf("peer rejected", "ip", ip, "reason", "different network", "their_network", peerNet, "our_network", core.NetworkName())
		_ = sc.Close()
		return
	}
	// Record how far this peer's clock is from ours. The median across peers,
	// bounded, becomes the offset timestamp validation uses — so one skewed
	// local clock cannot isolate this node from the chain (see core/nettime.go).
	// A peer that predates the field sends 0 and contributes no sample.
	if vm.Time != 0 {
		p.timeOffset = vm.Time - time.Now().Unix()
		p.hasTimeOffset = true
	}
	p.version = vm.Version
	p.caps = make(map[string]bool, len(vm.Caps))
	for _, c := range vm.Caps {
		p.caps[c] = true
	}

	n.addPeer(p)
	defer n.removePeer(p)
	// A completed handshake is the only evidence that an address is a real node,
	// so this is what promotes it from the `new` table to `tried` — and `tried`
	// is what outbound selection prefers on the next start.
	if dialed != "" {
		n.addrs.Good(dialed)
	}
	Infof("peer connected", "ip", ip, "peer", short(p.id))

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
			Warnf("peer dropped", "ip", ip, "reason", "inbound message rate exceeded")
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
		n.addrs.Add(m.Addr, "hello")

	case MsgGetPeers:
		p.send(Message{Type: MsgPeers, Peers: append(n.book.all(), n.cfg.AdvertiseAddr)})

	case MsgPeers:
		if len(m.Peers) > maxGossipPeers {
			m.Peers = m.Peers[:maxGossipPeers]
		}
		for _, addr := range m.Peers {
			n.book.note(addr)
			// Gossip is attacker-controlled, so it only ever ADDS to the `new`
			// table; whether any of it is dialed is the addrman's decision, under
			// the per-group cap.
			n.addrs.Add(addr, p.addr)
			n.maybeDial(addr)
		}

	case MsgTx:
		if m.Tx == nil {
			return
		}
		h := m.Tx.Hash()
		n.txReq.done(h) // whether we asked for it or it was pushed, it is here
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

	case MsgGetMempool:
		if txs := n.mempoolBatch(); len(txs) > 0 {
			p.send(Message{Type: MsgMempool, Txs: txs})
		}

	case MsgMempool:
		n.onMempoolBatch(p, m.Txs)

	case MsgTxInv:
		n.onTxInv(p, m.Hashes)

	case MsgGetTx:
		n.onGetTx(p, m.Hashes)

	case MsgTxs:
		n.onMempoolBatch(p, m.Txs)

	// block propagation: announce a hash, pull the body we lack
	case MsgInv:
		n.noteBestHeight(m.Index)
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
		if m.Block == nil {
			return
		}
		n.onBlockReceived(p, *m.Block)

	// compact block relay: a header plus short ids, reconstructed locally
	case MsgCmpctBlock:
		n.onCompactBlock(p, m.Cmpct)

	case MsgGetBlockTxn:
		n.onGetBlockTxn(p, m.Index, m.Hash, m.Indexes)

	case MsgBlockTxn:
		n.onBlockTxn(p, m.Hash, m.Txs)

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
		replaced, disconnected, err := n.chain.ReplaceChain(m.Chain)
		switch {
		case err == nil && replaced:
			Infof("adopted chain via fallback", "height", n.chain.Height())
			n.noteReorg(disconnected, n.resurrectTxs(disconnected))
			n.afterNewBlock(true)
		case err != nil:
			// A refused reorg is not a peer problem and must not be swallowed: the
			// finality guard has just declined a chain that fork choice preferred,
			// and it will decline the same one every time. If that chain is the
			// network's, this node is now diverged and will stay diverged until an
			// operator intervenes — so it is counted, logged loudly, and surfaced
			// by /health, /reorgs and /metrics.
			if ref, ok := core.AsReorgRefused(err); ok {
				n.noteRefusedReorg(ref)
			} else {
				Debugf("fallback chain rejected", "err", err)
			}
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
			Warnf("peer banned", "peer", short(p.id), "err", err)
		}
		return
	}
	from, to := headers[0].Index, headers[len(headers)-1].Index
	n.noteBestHeight(to)
	p.send(Message{Type: MsgGetBlocks, From: from, To: to})
	// Remember what we asked for: if the bodies never arrive, the sync loop gives
	// up on this peer rather than waiting forever (see sync.go).
	n.trackRequest(p, from, to)
}

// onBlocks applies a downloaded batch of block bodies. A batch that extends our
// tip is appended block by block; a batch that starts below our tip is a fork
// suffix, applied as a reorg (transferring only the divergent blocks). Deep or
// losing forks fall back to a whole-chain exchange.
func (n *Node) onBlocks(p *peer, blocks []core.Block) {
	n.clearRequest(p) // it answered; the slot is free for the next range
	if len(blocks) == 0 {
		return
	}
	n.noteBestHeight(blocks[len(blocks)-1].Index)
	first := blocks[0].Index

	if first <= n.chain.Height() {
		// Fork suffix: reorg from the common ancestor (first-1).
		adopted, disconnected, err := n.chain.ReorgFrom(first-1, blocks)
		if err != nil || !adopted {
			p.send(Message{Type: MsgGetChain}) // deep/losing fork: fall back
			return
		}
		for _, b := range blocks {
			n.markSeenBlock(b.Hash)
		}
		n.noteReorg(disconnected, n.resurrectTxs(disconnected))
		n.afterNewBlock(true)
		tip := n.chain.Tip()
		Infof("reorged onto fork", "height", tip.Index, "hash", short(tip.Hash))
		n.broadcastExcept(Message{Type: MsgInv, Index: tip.Index, Hash: tip.Hash}, p)
		if len(blocks) == maxBlocksBatch { // fork may be deeper than one batch
			p.send(n.getHeadersMsg())
		}
		return
	}

	// Extension: append what links to our tip, and buffer what does not. Ranges
	// are downloaded from several peers at once, so a later window can arrive
	// before the one before it; the orphan pool holds those until their parent
	// lands rather than throwing away a download we already paid for.
	applied, buffered := 0, 0
	for i := range blocks {
		if err := n.chain.AddBlock(blocks[i]); err != nil {
			if n.bufferOrphan(blocks[i]) {
				buffered++
			}
			continue
		}
		n.markSeenBlock(blocks[i].Hash)
		applied++
	}
	applied += n.connectOrphans()
	if applied == 0 {
		if buffered == 0 {
			p.send(n.getHeadersMsg()) // our tip moved under us; re-sync
		}
		return
	}
	n.afterNewBlock(false)
	tip := n.chain.Tip()
	Infof("synced blocks", "count", applied, "height", tip.Index)
	n.broadcastExcept(Message{Type: MsgInv, Index: tip.Index, Hash: tip.Hash}, p)
	if applied >= len(blocks) { // a full batch: there may be more
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
	n.clearRequest(p) // whatever we asked it for will have to come from someone else
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
	c := []string{CapMempool, CapTxInv, CapCompact}
	if n.cfg.Dandelion {
		c = append(c, CapDandelion)
	}
	return c
}

// mempoolBatch is what we serve in answer to MsgGetMempool: our pending
// transactions, capped so one request cannot make us serialize a full pool.
func (n *Node) mempoolBatch() []core.Transaction {
	txs := n.mempool.All()
	if len(txs) > maxMempoolBatch {
		txs = txs[:maxMempoolBatch]
	}
	return txs
}

// onMempoolBatch folds a peer's pending transactions into our own pool. It is
// the answer to MsgGetMempool, sent once per connection, so the batch is bounded
// and each transaction goes through exactly the same admission path a gossiped
// one does — nothing is trusted because it arrived in bulk.
//
// Anything newly admitted is relayed onward in the fluff phase: it is not ours to
// originate, so it gets no Dandelion++ stem, and the peer that sent it obviously
// has it already.
func (n *Node) onMempoolBatch(from *peer, txs []core.Transaction) {
	if len(txs) > maxMempoolBatch {
		txs = txs[:maxMempoolBatch]
	}
	next := n.chain.Height() + 1
	learned := 0
	for _, tx := range txs {
		if tx.IsExpiredAt(next) {
			continue
		}
		h := tx.Hash()
		n.txReq.done(h)
		if n.markSeenTx(h) {
			continue
		}
		if added, err := n.mempool.Add(tx); err != nil || !added {
			continue
		}
		learned++
		n.fluff(tx, from)
		n.publishTx(tx)
	}
	if learned > 0 {
		Infof("learned pending transactions", "count", learned, "peer", short(from.id))
		n.onNewTx() // a miner idling between blocks should build with them
	}
}

// onBlockReceived is the one path a newly-arrived block takes, however it got
// here: pushed in full, pulled after an announcement, or reconstructed from a
// compact block. Keeping it in one place is what lets compact relay be a
// transport detail rather than a second, subtly different validation path.
func (n *Node) onBlockReceived(p *peer, b core.Block) {
	if n.markSeenBlock(b.Hash) {
		return
	}
	// A block that is malformed on its own terms (bad PoW or merkle root) is
	// peer misbehaviour and earns ban points; one that is well-formed but
	// doesn't link to our tip is just a fork, handled below with a re-sync.
	if err := b.SelfValid(); err != nil {
		if n.bans.add(p.id, banInvalidBlock) {
			Warnf("peer banned", "peer", short(p.id), "reason", "invalid block", "err", err)
		}
		return
	}
	n.noteBestHeight(b.Index)
	if err := n.chain.AddBlock(b); err != nil {
		// Ahead of our tip: keep it, so it connects the moment its parent lands
		// instead of costing another round trip. Otherwise we are behind or on a
		// fork, and the locator finds where we diverged.
		n.bufferOrphan(b)
		p.send(n.getHeadersMsg())
		return
	}
	Infof("accepted block", "height", b.Index, "hash", short(b.Hash))
	n.connectOrphans()
	n.afterNewBlock(false)
	n.announceBlock(b, p)
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

// fluff ends a transaction's stem phase by telling every peer but one about it.
// It ANNOUNCES rather than pushes wherever the peer supports it (see
// txrelay.go); a peer that does not still receives the body.
func (n *Node) fluff(tx core.Transaction, except *peer) {
	n.announceTx(tx, except)
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

// RelayStats reports what the two relay optimizations are actually doing, which
// is otherwise invisible: both are pure wins when they work and a silent extra
// round trip when they do not.
type RelayStats struct {
	// CompactHit and CompactMiss count blocks rebuilt from the local mempool and
	// blocks that had to be fetched in full after a failed reconstruction. A miss
	// rate that is not near zero means peers' pools and this node's disagree.
	CompactHit  int64 `json:"compact_hit"`
	CompactMiss int64 `json:"compact_miss"`
	// TxInFlight is how many announced transactions have been requested and not
	// yet received. A number that stays high means peers are announcing and not
	// delivering.
	TxInFlight int `json:"tx_in_flight"`
	// PendingBlocks is how many compact blocks are waiting on transactions.
	PendingBlocks int `json:"pending_blocks"`
}

// RelayStats returns the relay counters.
func (n *Node) RelayStats() RelayStats {
	return RelayStats{
		CompactHit:    n.compactHit.Load(),
		CompactMiss:   n.compactMiss.Load(),
		TxInFlight:    n.txReq.len(),
		PendingBlocks: n.assembly.len(),
	}
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

// maxBuildAttempts bounds how many times buildBlockFor will drop an unmineable
// transaction and reassemble before giving up on this round.
const maxBuildAttempts = 4

// buildBlockFor assembles the next candidate block paying the coinbase to
// minerAddr, retrying if a selected transaction turns out to be unmineable.
//
// The mempool and Mempool.Select apply the same rules block validation does, so
// this should not happen — but if it ever did, the consequence is severe out of
// proportion to the cause: the same transaction would be selected into every
// candidate, each candidate would be rejected after being mined, and the node
// would stop producing blocks entirely while burning CPU. So the state root is
// computed here, and a transaction consensus refuses is dropped from the mempool
// and the block reassembled without it.
func (n *Node) buildBlockFor(minerAddr string) (core.Block, []core.Transaction) {
	for attempt := 0; ; attempt++ {
		candidate, txs := n.assembleBlock(minerAddr)
		root, err := n.chain.NextStateRoot(candidate)
		if err == nil {
			candidate.StateRoot = root
			return candidate, txs
		}
		var rej *core.TxRejection
		if attempt+1 >= maxBuildAttempts || !errors.As(err, &rej) || rej.Index < 1 || rej.Index > len(txs) {
			// Not attributable to one transaction (or too many tries): leave the state
			// root empty, which the miner treats as "do not mine this".
			Errorf("cannot build a block on the current tip", "err", err)
			return candidate, txs
		}
		bad := txs[rej.Index-1] // Transactions[0] is the coinbase
		Warnf("dropped unmineable transaction", "tx", short(bad.Hash()), "err", rej.Err)
		n.mempool.Remove([]core.Transaction{bad})
	}
}

// assembleBlock builds the candidate block body — coinbase plus mempool-selected
// transactions — with everything filled in except the state root and the nonce.
func (n *Node) assembleBlock(minerAddr string) (core.Block, []core.Transaction) {
	tip := n.chain.Tip()
	height := tip.Index + 1
	baseFee := n.chain.NextBaseFee()
	txs := n.mempool.Select(n.chain, core.MaxBlockTxs) // already excludes fee < baseFee
	// Miner is paid the subsidy plus tips (fees above the base fee); the base-fee
	// portion is burned.
	coinbase := core.NewCoinbaseAt(minerAddr, core.CoinbaseAmount(height, txs, baseFee), height)
	ts := time.Now().Unix()
	if ts <= tip.Timestamp {
		ts = tip.Timestamp + 1
	}
	candidate := core.Block{
		Version:      core.SignalVersion(n.cfg.SignalBits...),
		Index:        height,
		Timestamp:    ts,
		Transactions: append([]core.Transaction{coinbase}, txs...),
		PrevHash:     tip.Hash,
		BaseFee:      baseFee,
		Bits:         n.chain.NextBits(),
	}
	candidate.MerkleRoot = core.MerkleRoot(candidate.Transactions)
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
	Infof("accepted externally-mined block", "height", b.Index,
		"difficulty", core.TargetDifficulty(b.Bits), "hash", short(b.Hash))
	n.afterNewBlock(false)
	n.announceBlock(b, nil)
	return nil
}

// commitMined records a freshly mined block: append it, dedup, drop its
// transactions from the mempool, announce it by inventory, and emit an event.
func (n *Node) commitMined(mined core.Block, txs []core.Transaction) error {
	if err := n.chain.AddBlock(mined); err != nil {
		return err
	}
	n.markSeenBlock(mined.Hash)
	Infof("mined block", "height", mined.Index, "difficulty", core.TargetDifficulty(mined.Bits),
		"txs", len(txs), "reward", core.FormatAmount(core.BlockReward(mined.Index)), "hash", short(mined.Hash))
	n.mempool.Remove(txs)
	n.onTipChanged()
	// Announce the new block by inventory; peers pull the body if they lack it.
	n.announceBlock(mined, nil)
	n.publishBlock(false)
	return nil
}

// emptyBlockInterval is how long the miner idles before minting a block with no
// transactions in it (Config.EmptyBlockInterval, defaulting to one target block
// time).
func (n *Node) emptyBlockInterval() time.Duration {
	if n.cfg.EmptyBlockInterval > 0 {
		return n.cfg.EmptyBlockInterval
	}
	return time.Duration(core.TargetBlockTime) * time.Second
}

func (n *Node) mineLoop() {
	Infof("miner ready", "address", n.wallet.Address(), "mining", n.Mining())
	for !n.stopped() {
		if !n.mining.Load() {
			if !n.wait(minePollInterval) { // paused; poll for the toggle
				return
			}
			continue
		}
		candidate, txs := n.buildBlock()
		if candidate.StateRoot == "" {
			// The candidate is already known-invalid (see buildBlockFor). Hashing it
			// would burn a core to produce a block the chain will refuse, so wait for
			// something to change instead of spinning.
			Warnf("no valid block can be built on the current tip; waiting")
			n.sleepInterruptible(n.emptyBlockInterval())
			continue
		}

		// When idle, wait out the empty-block interval before minting a
		// coinbase-only block, so we don't spam the network. Interruptible if a
		// new tip arrives meanwhile.
		if len(txs) == 0 && !n.sleepInterruptible(n.emptyBlockInterval()) {
			continue
		}

		startGen := atomic.LoadInt64(&n.tipGen)
		mined, ok := core.Mine(candidate, func() bool {
			return atomic.LoadInt64(&n.tipGen) != startGen || n.stopped()
		})
		if !ok {
			continue // tip changed; rebuild on the new tip
		}
		if err := n.commitMined(mined, txs); err != nil {
			Warnf("discarded our own block", "height", mined.Index, "err", err)
			// Usually this means we lost the race and the tip already moved, in which
			// case the next round rebuilds on it. If the tip did NOT move, the block
			// was refused for a reason rebuilding won't change (an operator pinning a
			// checkpoint at a future height, say), and retrying immediately would spin
			// a core forever — so back off and let something change first.
			if atomic.LoadInt64(&n.tipGen) == startGen {
				n.sleepInterruptible(n.emptyBlockInterval())
			}
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
			if candidate.StateRoot == "" {
				return hashes, fmt.Errorf("no valid block can be built on the current tip")
			}
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

// sleepInterruptible sleeps up to d, returning false early if the tip changes, a
// new transaction arrives (so the idle miner rebuilds promptly rather than
// waiting out the whole interval), or the node shuts down.
func (n *Node) sleepInterruptible(d time.Duration) bool {
	tip := atomic.LoadInt64(&n.tipGen)
	tx := atomic.LoadInt64(&n.txGen)
	const step = 50 * time.Millisecond
	for left := d; left > 0; left -= step {
		if atomic.LoadInt64(&n.tipGen) != tip || atomic.LoadInt64(&n.txGen) != tx {
			return false
		}
		if !n.wait(min(step, left)) {
			return false
		}
	}
	return true
}

// resurrectTxs returns the transactions of blocks a reorg discarded to the
// mempool, and reports how many were re-queued. Losing a race is not the same as
// being invalid: a payment confirmed only in the orphaned branch is still a valid
// signed transfer, and without this it would silently vanish from the network
// until its sender noticed and re-broadcast it.
//
// Coinbases are skipped — they are minted by the block that died, so they cannot
// be re-mined. So is anything the winning branch already confirms, which the
// transaction index answers in O(1). Callers run this BEFORE reconcileMempool, so
// a resurrected transaction whose nonce the winning branch has since consumed is
// dropped again immediately.
func (n *Node) resurrectTxs(disconnected []core.Block) int {
	count := 0
	for _, b := range disconnected {
		for i := 1; i < len(b.Transactions); i++ {
			tx := b.Transactions[i]
			if n.chain.HasTx(tx.Hash()) {
				continue // also in the winning branch: still confirmed, nothing to do
			}
			if added, err := n.mempool.Add(tx); err == nil && added {
				count++
			}
		}
	}
	if count > 0 {
		Infof("returned orphaned transactions to the mempool", "txs", count, "blocks", len(disconnected))
	}
	return count
}

// reconcileMempool drops transactions a new block made unmineable — expired ones,
// nonces the block confirmed, and anything the sender can no longer reach or
// afford. See core.Mempool.Reconcile.
func (n *Node) reconcileMempool() {
	n.mempool.Reconcile(n.chain.Height())
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

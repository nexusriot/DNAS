package node

import (
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"
)

// The outbound address manager.
//
// Inbound connections have had eclipse caps for a while (a total, plus a
// per-network-group limit), but outbound selection had none: every address a
// peer gossiped went into one flat set, and the first `maxpeers` of them to be
// picked out of a Go map got a dial loop. An attacker who gossips nine addresses
// in one /16 therefore stood a good chance of owning every outbound slot, which
// is the whole eclipse attack — and outbound is the half that matters, because
// those are the peers a node CHOSE and therefore trusts to tell it the truth
// about the chain.
//
// This is a tried/new address manager in the bitcoind tradition, minus the parts
// that only pay off at internet scale:
//
//   - Two tables. `new` holds addresses we have merely heard about; `tried`
//     holds ones we have actually completed a handshake with. Selection is
//     biased toward `tried`, because an address that worked once is evidence
//     and an address someone mentioned is not.
//   - Bucketing by network group (the /16 or /48 the address sits in, via
//     ipGroup). Both the tables and the LIVE outbound set are bounded per group,
//     so filling a node's outbound slots requires addresses in many distinct
//     ranges rather than many addresses in one.
//   - Bounded tables with eviction, so gossip cannot grow memory without limit.
//
// What it deliberately is NOT: there is no ASN lookup (that needs a routing
// table this project has no business shipping), and the bucketing is a coarse
// prefix rather than bitcoind's keyed-hash bucket assignment. A /16 is a weaker
// diversity signal than an ASN — two ranges can share an operator — so this
// raises the cost of an eclipse rather than settling it. Said plainly in the
// ROADMAP too.

const (
	// maxOutboundPerGroup bounds how many LIVE outbound peers may share one
	// network group. This is the eclipse control: with 8 outbound slots and a cap
	// of 2, an attacker needs addresses in 4 distinct groups to own them all,
	// instead of 8 addresses anywhere.
	maxOutboundPerGroup = 2

	// Table bounds. Gossip is attacker-controlled, so both tables are capped and
	// evict rather than growing.
	maxNewEntries      = 4096
	maxTriedEntries    = 1024
	maxEntriesPerGroup = 64

	// maxDialFailures is how many consecutive failed dials retire an address
	// that has never worked. A tried address is kept longer: it worked once, and
	// a node that is merely down should not be forgotten on the first outage.
	maxDialFailures      = 5
	maxTriedDialFailures = 12
)

// addrEntry is one address the node knows about.
type addrEntry struct {
	Addr     string    `json:"addr"`
	Source   string    `json:"source,omitempty"` // who told us: a peer address, "dns", "config"
	Group    string    `json:"group"`            // network group, for diversity accounting
	Tried    bool      `json:"tried"`            // we have completed a handshake with it
	Attempts int       `json:"attempts"`         // consecutive failures since the last success
	LastTry  time.Time `json:"last_try,omitempty"`
	LastOK   time.Time `json:"last_ok,omitempty"`
	Added    time.Time `json:"added"`
}

// addrman holds the known-address tables and the live outbound accounting.
type addrman struct {
	mu      sync.Mutex
	entries map[string]*addrEntry
	// liveGroups counts CURRENT outbound connections per group, which is what the
	// diversity cap is actually enforced against. The table-level bounds stop
	// memory growth; this one stops the eclipse.
	liveGroups map[string]int
	live       map[string]bool // addresses currently held by an outbound dial loop
	rnd        *rand.Rand
	now        func() time.Time // injectable, so tests need no sleeping
}

func newAddrman() *addrman {
	return &addrman{
		entries:    map[string]*addrEntry{},
		liveGroups: map[string]int{},
		live:       map[string]bool{},
		rnd:        rand.New(rand.NewSource(randomSeed())),
		now:        time.Now,
	}
}

// randomSeed picks a per-process seed so two nodes started together do not walk
// their address tables in lockstep.
func randomSeed() int64 { return time.Now().UnixNano() }

// addrGroup is the diversity key for a host:port. It reuses the same coarse
// prefix the inbound caps use, so both halves of the eclipse defence agree on
// what "a different part of the network" means. A hostname that has not been
// resolved is its own group: we cannot know better, and treating every unknown
// name as one shared group would let one DNS seed's results crowd each other out.
func addrGroup(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipGroup(host)
	}
	return "name:" + host
}

// Add records an address we have heard about. Returns true if it was new.
func (a *addrman) Add(addr, source string) bool {
	if addr == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.entries[addr]; ok {
		return false
	}
	group := addrGroup(addr)
	// A single group may not fill the table, or gossip from one range would push
	// out every address a node had learned from anywhere else.
	if a.countGroupLocked(group) >= maxEntriesPerGroup {
		return false
	}
	if a.countLocked(false) >= maxNewEntries {
		if !a.evictWorstLocked(false) {
			return false
		}
	}
	a.entries[addr] = &addrEntry{
		Addr: addr, Source: source, Group: group, Added: a.now(),
	}
	return true
}

// Good marks an address as having completed a handshake: it moves to the tried
// table and its failure count resets.
func (a *addrman) Good(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.entries[addr]
	if e == nil {
		// A peer that dialed US and then proved useful is worth remembering.
		e = &addrEntry{Addr: addr, Source: "inbound", Group: addrGroup(addr), Added: a.now()}
		a.entries[addr] = e
	}
	if !e.Tried && a.countLocked(true) >= maxTriedEntries {
		a.evictWorstLocked(true)
	}
	e.Tried = true
	e.Attempts = 0
	e.LastOK = a.now()
}

// Attempt records that a dial is being made.
func (a *addrman) Attempt(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.entries[addr]; e != nil {
		e.LastTry = a.now()
	}
}

// Failed records a failed dial, retiring the address once it has failed enough
// times to be considered dead.
func (a *addrman) Failed(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.entries[addr]
	if e == nil {
		return
	}
	e.Attempts++
	e.LastTry = a.now()
	limit := maxDialFailures
	if e.Tried {
		limit = maxTriedDialFailures
	}
	if e.Attempts >= limit {
		delete(a.entries, addr)
	}
}

// Forget drops an address entirely (it turned out to be us, or it was banned).
func (a *addrman) Forget(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.entries, addr)
}

// Reserve claims an outbound slot for addr if the diversity cap allows it,
// returning false when this group already holds its share of live connections.
// The caller must Release when the connection ends.
func (a *addrman) Reserve(addr string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reserveLocked(addr)
}

func (a *addrman) reserveLocked(addr string) bool {
	if a.live[addr] {
		return false
	}
	g := addrGroup(addr)
	// Loopback is exempt: a local demo or the e2e suite runs every node on
	// 127.0.0.1, where a strict per-group cap would allow only two peers total.
	// The same exemption the inbound caps make, for the same reason.
	if !isLoopbackAddr(addr) && a.liveGroups[g] >= maxOutboundPerGroup {
		return false
	}
	a.live[addr] = true
	a.liveGroups[g]++
	return true
}

// Release frees the outbound slot held for addr.
func (a *addrman) Release(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.live[addr] {
		return
	}
	delete(a.live, addr)
	g := addrGroup(addr)
	if a.liveGroups[g]--; a.liveGroups[g] <= 0 {
		delete(a.liveGroups, g)
	}
}

// Select returns up to n addresses worth dialing, honouring the per-group
// diversity cap against both the live set and the selection itself, and biased
// toward addresses that have worked before. Selected addresses are reserved, so
// the caller owns them until it Releases them.
func (a *addrman) Select(n int, skip func(string) bool) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n <= 0 {
		return nil
	}

	// Two pools, tried first. Within a pool the order is shuffled so a node does
	// not deterministically prefer whichever address sorts first — that would let
	// an attacker pick a name that always wins.
	var tried, fresh []*addrEntry
	for _, e := range a.entries {
		if a.live[e.Addr] || (skip != nil && skip(e.Addr)) {
			continue
		}
		if e.Tried {
			tried = append(tried, e)
		} else {
			fresh = append(fresh, e)
		}
	}
	a.shuffleLocked(tried)
	a.shuffleLocked(fresh)

	out := make([]string, 0, n)
	for _, pool := range [][]*addrEntry{tried, fresh} {
		for _, e := range pool {
			if len(out) >= n {
				return out
			}
			if a.reserveLocked(e.Addr) {
				out = append(out, e.Addr)
			}
		}
	}
	return out
}

func (a *addrman) shuffleLocked(es []*addrEntry) {
	a.rnd.Shuffle(len(es), func(i, j int) { es[i], es[j] = es[j], es[i] })
}

// countLocked counts entries in one table.
func (a *addrman) countLocked(tried bool) int {
	n := 0
	for _, e := range a.entries {
		if e.Tried == tried {
			n++
		}
	}
	return n
}

func (a *addrman) countGroupLocked(group string) int {
	n := 0
	for _, e := range a.entries {
		if e.Group == group {
			n++
		}
	}
	return n
}

// evictWorstLocked drops the least promising entry from one table: the most
// failures, then the oldest. A live address is never evicted — it is in use.
func (a *addrman) evictWorstLocked(tried bool) bool {
	var worst *addrEntry
	for _, e := range a.entries {
		if e.Tried != tried || a.live[e.Addr] {
			continue
		}
		if worst == nil || betterToEvict(e, worst) {
			worst = e
		}
	}
	if worst == nil {
		return false
	}
	delete(a.entries, worst.Addr)
	return true
}

// betterToEvict reports whether e is a worse address to keep than cur.
func betterToEvict(e, cur *addrEntry) bool {
	if e.Attempts != cur.Attempts {
		return e.Attempts > cur.Attempts
	}
	if !e.LastOK.Equal(cur.LastOK) {
		return e.LastOK.Before(cur.LastOK)
	}
	return e.Added.Before(cur.Added)
}

// Size reports how many addresses are known, split by table.
func (a *addrman) Size() (newCount, triedCount int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.countLocked(false), a.countLocked(true)
}

// Known returns every known address, for gossip and for persistence.
func (a *addrman) Known() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.entries))
	for addr := range a.entries {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}

// Snapshot returns the full table, for persistence across a restart. The tried
// table is the valuable half: it is this node's own hard-won evidence about who
// is real, and losing it on every restart means bootstrapping from gossip again.
func (a *addrman) Snapshot() []addrEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]addrEntry, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// Restore loads a persisted table. Live accounting is deliberately not restored:
// nothing is connected yet at load time.
func (a *addrman) Restore(entries []addrEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range entries {
		e := entries[i]
		if e.Addr == "" {
			continue
		}
		// Recompute the group rather than trusting the file: the grouping rule is
		// code, and a stale file must not pin an old one.
		e.Group = addrGroup(e.Addr)
		a.entries[e.Addr] = &e
	}
}

// isLoopbackAddr reports whether addr is on the loopback interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// Node integration
// ---------------------------------------------------------------------------

// peerRefillInterval is how often the node tops its outbound set back up. Slow
// enough to be invisible, fast enough that a node whose peers all dropped is not
// stranded until something else happens to gossip at it.
const peerRefillInterval = 20 * time.Second

// fillOutbound dials as many addresses as the outbound cap still allows,
// choosing them through the address manager so the per-group diversity limit
// applies. It is the only place outbound connections are initiated other than
// direct gossip, and it is what makes the node self-healing: peers drop, slots
// free, the next tick fills them from `tried` first.
func (n *Node) fillOutbound() {
	want := n.cfg.MaxPeers - n.book.dialCount()
	if want <= 0 {
		return
	}
	for _, addr := range n.addrs.Select(want, func(addr string) bool {
		// Never dial ourselves, anything already connected, or an address the
		// peerbook has learned is another spelling of us.
		return addr == n.cfg.AdvertiseAddr || n.book.isSelf(addr) || n.connectedTo(addr)
	}) {
		// Select already reserved the slot, so go straight to the dial loop; a
		// peerbook refusal hands the reservation back.
		if !n.book.shouldDial(addr) {
			n.addrs.Release(addr)
			continue
		}
		go n.dialLoop(addr)
	}
}

// peerLoop keeps the outbound set topped up for the life of the node.
func (n *Node) peerLoop() {
	t := time.NewTicker(peerRefillInterval)
	defer t.Stop()
	for {
		select {
		case <-n.quit:
			return
		case <-t.C:
			n.fillOutbound()
		}
	}
}

// LiveGroupCount reports how many distinct network groups the current outbound
// connections occupy.
func (a *addrman) LiveGroupCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.liveGroups)
}

package node

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Peer and ban inspection, and operator control over both.
//
// A node with ban scoring that cannot be inspected is a node whose incidents
// cannot be debugged: peers were reported as a bare list of address strings,
// while everything interesting about a connection — the protocol version it
// negotiated, the identity it proved, which side dialed, what it has told us
// about its chain — was known and thrown away. Bans were worse: scores were kept
// and persisted, but nothing exposed them and nothing could clear one, so an
// operator whose peer got banned by a bug had to stop the node, edit bans.json
// and start again.
//
// None of this is consensus. It is the difference between running a node and
// watching one.

// PeerInfo is everything a node knows about one live connection.
type PeerInfo struct {
	Addr      string   `json:"addr"`      // the peer's advertised address ("" until its hello)
	IP        string   `json:"ip"`        // remote IP, the key for pre-identity ban scoring
	Identity  string   `json:"identity"`  // authenticated identity public key (hex)
	Version   int      `json:"version"`   // negotiated protocol version
	Caps      []string `json:"caps"`      // advertised capabilities
	Inbound   bool     `json:"inbound"`   // they dialed us
	BanScore  int      `json:"ban_score"` // current score against their identity
	Connected string   `json:"connected"` // how long the connection has been up
	// Syncing reports whether we currently have a ranged block request
	// outstanding to this peer — the peer-level view of catch-up.
	Syncing bool `json:"syncing"`
	// TimeOffset is this peer's clock minus ours at handshake, in seconds. The
	// node already applies the bounded median of these when validating timestamps
	// (nettime.go); exposing the samples is what lets an operator see that THIS
	// machine is the one that is wrong, which the median deliberately hides.
	TimeOffset int64 `json:"time_offset"`
}

// Peers returns full information about every connected peer, sorted by address
// so repeated calls read consistently.
func (n *Node) Peers() []PeerInfo {
	// Outstanding block requests are tracked under syncMu; snapshot that first so
	// the two locks are never held at once.
	n.syncMu.Lock()
	inflight := make(map[*peer]bool, len(n.inflight))
	for p := range n.inflight {
		inflight[p] = true
	}
	n.syncMu.Unlock()

	n.peersMu.Lock()
	out := make([]PeerInfo, 0, len(n.peers))
	for p := range n.peers {
		caps := make([]string, 0, len(p.caps))
		for c := range p.caps {
			caps = append(caps, c)
		}
		sort.Strings(caps)
		info := PeerInfo{
			Addr:       p.addr,
			IP:         p.ip,
			Identity:   p.id,
			Version:    p.version,
			Caps:       caps,
			Inbound:    p.inbound,
			Syncing:    inflight[p],
			TimeOffset: p.timeOffset,
		}
		if !p.since.IsZero() {
			info.Connected = time.Since(p.since).Round(time.Second).String()
		}
		out = append(out, info)
	}
	n.peersMu.Unlock()

	// Ban scores come from their own lock, after the peer set is copied.
	for i := range out {
		out[i].BanScore = n.bans.scoreOf(out[i].Identity)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Addr != out[j].Addr {
			return out[i].Addr < out[j].Addr
		}
		return out[i].Identity < out[j].Identity
	})
	return out
}

// BanEntry is one scored key: an IP (pre-identity misbehaviour, e.g. a failed
// handshake) or a peer identity (protocol-level misbehaviour).
type BanEntry struct {
	Key    string `json:"key"`
	Score  int    `json:"score"`
	Banned bool   `json:"banned"`
}

// Bans returns every scored key, worst first. A key below the threshold is
// listed too: seeing a peer at 80 points before it is cut off is most of the
// value of having the scores at all.
func (n *Node) Bans() []BanEntry {
	scores := n.bans.snapshot()
	out := make([]BanEntry, 0, len(scores))
	for k, v := range scores {
		out = append(out, BanEntry{Key: k, Score: v, Banned: v >= banThreshold})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// BanThreshold is the score at which a key is cut off, so a caller can show a
// score in context instead of as a bare number.
func (n *Node) BanThreshold() int { return banThreshold }

// Unban clears a key's ban score. It reports an error for a key that has none,
// so "unbanned" never silently means "there was nothing there" — an operator
// mistyping an identity would otherwise believe they had fixed something.
//
// It does not disconnect or reconnect anything: an outbound dial loop retries on
// its own interval, and an inbound peer is free to come back.
func (n *Node) Unban(key string) error {
	if key == "" {
		return errors.New("no key given")
	}
	if !n.bans.clear(key) {
		return fmt.Errorf("no ban score recorded for %q", key)
	}
	return nil
}

// AddPeer asks the node to connect to an address at runtime, the way `-peers`
// does at startup. It reports an error when the address is unusable or already
// in hand, rather than quietly doing nothing — an operator adding a peer wants
// to know whether it took.
func (n *Node) AddPeer(addr string) error {
	if addr == "" {
		return errors.New("no address given")
	}
	if addr == n.cfg.AdvertiseAddr {
		return errors.New("that is this node's own address")
	}
	if n.connectedTo(addr) {
		return fmt.Errorf("already connected to %s", addr)
	}
	n.book.note(addr)
	// An operator asking for this peer by hand is a stronger signal than gossip,
	// so it goes straight into the addrman — but it still takes a reservation,
	// because the per-group cap is a safety property and not a suggestion.
	n.addrs.Add(addr, "api")
	if !n.addrs.Reserve(addr) {
		return fmt.Errorf("not dialing %s: its network group already holds %d outbound peers",
			addr, maxOutboundPerGroup)
	}
	if !n.book.shouldDial(addr) {
		n.addrs.Release(addr)
		return fmt.Errorf("not dialing %s: already dialing it, or the outbound cap of %d is reached",
			addr, n.cfg.MaxPeers)
	}
	go n.dialLoop(addr)
	return nil
}

// AddrStats reports the address manager's tables, so the eclipse defence is
// observable rather than merely present: a node whose `tried` set is tiny, or
// whose addresses all sit in one group, is a node that is cheap to eclipse.
type AddrStats struct {
	New    int `json:"new"`    // addresses heard about but never connected to
	Tried  int `json:"tried"`  // addresses that completed a handshake
	Groups int `json:"groups"` // distinct network groups known
	// LiveGroups is how many distinct groups the CURRENT outbound peers occupy.
	// This is the number that matters: outbound peers in one group is the
	// eclipse, however many addresses the tables hold.
	LiveGroups     int `json:"live_groups"`
	MaxPerGroup    int `json:"max_outbound_per_group"`
	OutboundDialed int `json:"outbound_dialed"`
}

// AddrStats returns a snapshot of the address manager.
func (n *Node) AddrStats() AddrStats {
	newN, tried := n.addrs.Size()
	groups := map[string]bool{}
	for _, e := range n.addrs.Snapshot() {
		groups[e.Group] = true
	}
	return AddrStats{
		New: newN, Tried: tried, Groups: len(groups),
		LiveGroups:     n.addrs.LiveGroupCount(),
		MaxPerGroup:    maxOutboundPerGroup,
		OutboundDialed: n.book.dialCount(),
	}
}

// DropPeer closes the connection to a peer, matched by advertised address or by
// identity key. It reports how many connections were closed (a peer can hold
// more than one if both sides dialed).
//
// A dropped OUTBOUND peer is redialed by its dial loop shortly after, which is
// the honest behaviour to document rather than hide: dropping is for clearing a
// wedged connection, and banning is for keeping someone away.
func (n *Node) DropPeer(match string) (int, error) {
	if match == "" {
		return 0, errors.New("no address or identity given")
	}
	n.peersMu.Lock()
	var targets []*peer
	for p := range n.peers {
		if p.addr == match || p.id == match {
			targets = append(targets, p)
		}
	}
	n.peersMu.Unlock()
	if len(targets) == 0 {
		return 0, fmt.Errorf("no connected peer matches %q", match)
	}
	for _, p := range targets {
		_ = p.conn.Close() // the read loop's defer removes it from the peer set
	}
	return len(targets), nil
}

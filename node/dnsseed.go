package node

import (
	"context"
	"net"
	"strconv"
	"time"
)

// DNS seed bootstrapping.
//
// Joining a DNAS network meant knowing somebody's address and passing it in
// `-peers`. That is fine for a demo and is not a network anyone can join: there
// has to be a way to find a first peer without being told one out of band.
//
// A DNS seed is the standard answer and the cheapest one — a hostname whose A/
// AAAA records list nodes that were recently reachable. It needs no new protocol
// and no server code here: an operator runs anything that publishes records, or
// even a static zone file.
//
// The trust model is worth being explicit about, because a seed is the one place
// a bootstrapping node believes something it cannot yet verify. A seed can:
//
//   - return only addresses it controls, which IS an eclipse if a node takes
//     them all; and
//   - see that a given IP is starting a node.
//
// It cannot forge a chain: every peer still completes the handshake, and every
// block is still validated against proof of work. So the mitigations here are
// about not letting a seed own the outbound set: results go into the addrman as
// `new` entries with no special standing, they are subject to the same per-group
// diversity cap as anything else, seeds are only consulted when the node is
// genuinely short of addresses, and results from ALL configured seeds are pooled
// so one seed does not fill the table alone. Configure more than one.

const (
	// dnsSeedTimeout bounds one round of resolution. A seed that is slow must not
	// hold up startup — the node is perfectly able to run while it waits.
	dnsSeedTimeout = 10 * time.Second

	// dnsSeedThreshold is how few known addresses count as "needs bootstrapping".
	// Above it the node has its own addresses (persisted or gossiped) and has no
	// reason to ask a third party.
	dnsSeedThreshold = 8

	// maxAddrsPerSeed bounds what one seed may contribute in one round, so a seed
	// returning a thousand records cannot crowd out the others.
	maxAddrsPerSeed = 32
)

// seedResolver resolves a hostname to IPs. Injectable so tests need no DNS.
type seedResolver func(ctx context.Context, host string) ([]net.IP, error)

func defaultSeedResolver(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// bootstrapFromDNSSeeds resolves the configured seeds and adds what they return
// to the address manager. It reports how many addresses were new.
//
// A seed may be given as "host" (the network's default port is used) or
// "host:port". Nothing here dials: the addrman decides what is worth dialing,
// under the same rules that govern every other address.
func (n *Node) bootstrapFromDNSSeeds() int {
	seeds := n.cfg.DNSSeeds
	if len(seeds) == 0 {
		return 0
	}
	known, tried := n.addrs.Size()
	if known+tried >= dnsSeedThreshold {
		Debugf("skipping DNS seeds", "known", known+tried, "threshold", dnsSeedThreshold)
		return 0
	}

	resolve := n.resolveSeed
	if resolve == nil {
		resolve = defaultSeedResolver
	}
	ctx, cancel := context.WithTimeout(context.Background(), dnsSeedTimeout)
	defer cancel()

	added := 0
	for _, seed := range seeds {
		host, port := splitSeed(seed, n.defaultPeerPort())
		ips, err := resolve(ctx, host)
		if err != nil {
			// One unreachable seed is normal and not worth failing a startup over.
			Warnf("DNS seed failed", "seed", host, "err", err)
			continue
		}
		taken := 0
		for _, ip := range ips {
			if taken >= maxAddrsPerSeed {
				break
			}
			taken++
			// The source is recorded as the seed hostname, so an operator can see
			// which seed an address came from when one starts behaving oddly.
			if n.addrs.Add(ipPort(ip, port), "dns:"+host) {
				added++
			}
		}
		Infof("DNS seed resolved", "seed", host, "addresses", taken)
	}
	return added
}

// splitSeed splits a seed spec into host and port, defaulting the port.
func splitSeed(seed string, defaultPort string) (host, port string) {
	if h, p, err := net.SplitHostPort(seed); err == nil {
		return h, p
	}
	return seed, defaultPort
}

// ipPort renders an IP and port as a dialable address, bracketing IPv6.
func ipPort(ip net.IP, port string) string { return net.JoinHostPort(ip.String(), port) }

// defaultPeerPort is the port a seed's bare hostnames are assumed to serve on:
// whatever this node itself listens on, since a network's nodes conventionally
// share a port.
func (n *Node) defaultPeerPort() string {
	if _, p, err := net.SplitHostPort(n.cfg.ListenAddr); err == nil && p != "" {
		if _, err := strconv.Atoi(p); err == nil {
			return p
		}
	}
	return "3000"
}

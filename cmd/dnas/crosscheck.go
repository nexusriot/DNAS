package main

import (
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
)

// Cross-checking the tip with peers.
//
// A light client verifies everything the API node sends it against a header
// chain — but the header chain is also the API node's, and proof of work only
// says the chain cost something, not that it is THE chain. An API node (or
// every node a Sybil controls) can serve a valid but stale or minority branch,
// and every proof against it verifies. The defence is to ask other nodes, over
// the peer protocol, where their chain leaves this one: a peer holding a branch
// with MORE work than the one the API node served is direct evidence the client
// is being shown the wrong chain.
//
// A peer can lie for free in one direction only. It can refuse to answer, or
// answer with headers whose work does not check out — both are ignored, and
// merely fail to count as a confirmation. What it cannot do without real work is
// produce a heavier branch, so "a peer showed more work" is never dismissed.

// tipVerdict is what one peer's answer says about the API node's chain.
type tipVerdict int

const (
	tipAgrees       tipVerdict = iota // same chain, same tip
	tipPeerAhead                      // same chain; the peer has blocks past our tip
	tipPeerBehind                     // the peer's chain is a prefix of ours
	tipWeakerFork                     // the peer is on a branch with less work than ours
	tipStrongerFork                   // the peer is on a branch with MORE work: refuse
	tipInvalid                        // the answer does not check out; ignore the peer
)

func (v tipVerdict) String() string {
	return [...]string{"agrees", "ahead", "behind", "weaker fork", "STRONGER FORK", "invalid answer"}[v]
}

// tipBehindSlack is how many blocks a peer may trail our tip and still count as
// confirming it: block propagation takes a moment, and a peer one block behind
// has verified everything but the newest block.
const tipBehindSlack = 2

// tipJudgement is one peer's verdict with the numbers behind it.
type tipJudgement struct {
	Addr    string
	Verdict tipVerdict
	Diverge uint64 // the last height both chains share
	Blocks  int    // blocks the peer is ahead/behind, or the length of its branch
	Detail  string
}

// confirms reports whether the judgement counts toward the tip being right.
func (j tipJudgement) confirms() bool {
	switch j.Verdict {
	case tipAgrees, tipPeerAhead:
		return true
	case tipPeerBehind:
		return j.Blocks <= tipBehindSlack
	}
	return false
}

// judgeTipAnswer compares a peer's FetchHeadersAfter answer with the verified
// header chain the API node served. It touches no network.
func judgeTipAnswer(headers []core.Header, ans node.HeadersAfter) tipJudgement {
	j := tipJudgement{Addr: ans.Addr}
	tip := uint64(len(headers) - 1)
	fork := ans.Fork
	if fork > tip {
		j.Verdict, j.Detail = tipInvalid, fmt.Sprintf("claims to share height %d, above our tip %d", fork, tip)
		return j
	}
	if len(ans.Headers) == 0 {
		// Nothing after the fork point: the peer's tip IS that block.
		j.Diverge, j.Blocks = fork, int(tip-fork)
		if fork == tip {
			j.Verdict = tipAgrees
		} else {
			j.Verdict = tipPeerBehind
		}
		return j
	}
	// The peer's answer must be a valid continuation of the block it says it
	// shares — linkage, work, and the difficulty the retarget requires.
	if err := core.ValidateHeaderChain(ans.Headers, headers[fork].Hash, fork); err != nil {
		j.Verdict, j.Detail = tipInvalid, err.Error()
		return j
	}
	combined := append(append([]core.Header(nil), headers[:fork+1]...), ans.Headers...)
	if err := core.ValidateHeaderDifficulty(combined, int(fork)+1); err != nil {
		j.Verdict, j.Detail = tipInvalid, err.Error()
		return j
	}
	// A locator is sparse, so the fork it reports may sit below the real
	// divergence: skip the headers the two chains still share.
	k := 0
	for k < len(ans.Headers) && fork+1+uint64(k) <= tip && ans.Headers[k].Hash == headers[fork+1+uint64(k)].Hash {
		k++
	}
	diverge := fork + uint64(k)
	rest := ans.Headers[k:]
	j.Diverge = diverge
	switch {
	case diverge == tip && len(rest) == 0:
		j.Verdict = tipAgrees
	case diverge == tip:
		j.Verdict, j.Blocks = tipPeerAhead, len(rest)
	case len(rest) == 0:
		j.Verdict, j.Blocks = tipPeerBehind, int(tip-diverge)
	default:
		theirs, ours := headerWork(rest), headerWork(headers[diverge+1:])
		j.Blocks = len(rest)
		if theirs.Cmp(ours) > 0 {
			j.Verdict = tipStrongerFork
			j.Detail = fmt.Sprintf("its branch from height %d has more work than the API node's", diverge+1)
		} else {
			j.Verdict = tipWeakerFork
		}
	}
	return j
}

func headerWork(hs []core.Header) *big.Int {
	w := new(big.Int)
	for _, h := range hs {
		w.Add(w, core.BlockWork(h.Bits))
	}
	return w
}

// tipCheckResult is the outcome of cross-checking against every peer.
type tipCheckResult struct {
	Judgements []tipJudgement
	Errors     []string // peers that could not be asked
	Confirmed  int
	Groups     int // distinct network groups among the confirming peers
}

// decideTip turns the per-peer judgements into a verdict on the API node's
// chain: refused on any stronger fork, refused when fewer peers confirmed it
// than needed, accepted otherwise.
func decideTip(js []tipJudgement, errs []string, nPeers int) (tipCheckResult, error) {
	res := tipCheckResult{Judgements: js, Errors: errs}
	var confirming []string
	for _, j := range js {
		if j.Verdict == tipStrongerFork {
			return res, fmt.Errorf("peer %s holds a chain with more work than the API node's (diverging after height %d): "+
				"the API node is serving a stale or minority branch", j.Addr, j.Diverge)
		}
		if j.confirms() {
			res.Confirmed++
			confirming = append(confirming, j.Addr)
		}
	}
	res.Groups = distinctGroups(confirming)
	need := nPeers
	if need > 2 {
		need = 2
	}
	if res.Confirmed < need {
		return res, fmt.Errorf("only %d of %d peers confirmed the API node's tip (need %d)", res.Confirmed, nPeers, need)
	}
	return res, nil
}

// crossCheckTip asks every peer where its chain leaves the verified headers.
func crossCheckTip(headers []core.Header, peers []string, psk []byte) (tipCheckResult, error) {
	locator := core.HeaderLocator(headers)
	var js []tipJudgement
	var errs []string
	for _, p := range peers {
		ans, err := node.FetchHeadersAfter(p, psk, locator)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		js = append(js, judgeTipAnswer(headers, ans))
	}
	return decideTip(js, errs, len(peers))
}

// spvTipChecked makes the cross-check run once per command, however many
// times the command verifies the header chain.
var spvTipChecked bool

// crossCheckSPVTip runs the cross-check for the spv command's -peers, once,
// and prints what each peer said. Without -peers it does nothing.
func crossCheckSPVTip(headers []core.Header) error {
	if len(spvPeers) == 0 || spvTipChecked {
		return nil
	}
	spvTipChecked = true
	res, err := crossCheckTip(headers, spvPeers, []byte(spvNetKey))
	for _, j := range res.Judgements {
		line := fmt.Sprintf("  peer %s: %s", j.Addr, j.Verdict)
		switch j.Verdict {
		case tipPeerAhead:
			line += fmt.Sprintf(" by %d block(s) — the API node is behind", j.Blocks)
		case tipPeerBehind:
			line += fmt.Sprintf(" by %d block(s)", j.Blocks)
		case tipWeakerFork:
			line += fmt.Sprintf(" (a %d-block branch after height %d)", j.Blocks, j.Diverge)
		}
		if j.Detail != "" {
			line += " — " + j.Detail
		}
		fmt.Fprintln(os.Stderr, line)
	}
	for _, e := range res.Errors {
		fmt.Fprintln(os.Stderr, "  peer unreachable:", e)
	}
	if err != nil {
		return fmt.Errorf("tip cross-check failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ tip confirmed by %d of %d peer(s)\n", res.Confirmed, len(spvPeers))
	if res.Confirmed >= 2 && res.Groups < 2 {
		fmt.Fprintln(os.Stderr, "warning: every confirming peer is in one network range (/16 or /32); "+
			"one operator could be all of them — add a peer from elsewhere")
	}
	return nil
}

// distinctGroups counts the network groups (/16 for IPv4, /32 for IPv6) among
// peer addresses, the same coarse "one operator" proxy the node uses for its
// own outbound diversity. Loopback and unresolvable hosts each count as their
// own group, so a local test setup is not reported as a Sybil.
func distinctGroups(addrs []string) int {
	seen := map[string]bool{}
	for _, a := range addrs {
		host := a
		if h, _, err := net.SplitHostPort(a); err == nil {
			host = h
		}
		seen[peerGroup(host, a)] = true
	}
	return len(seen)
}

func peerGroup(host, full string) string {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		if ips, err := net.LookupIP(host); err == nil && len(ips) > 0 {
			ip = ips[0]
		}
	}
	switch {
	case ip == nil, ip.IsLoopback():
		return "self:" + full
	case ip.To4() != nil:
		return ip.Mask(net.CIDRMask(16, 32)).String()
	default:
		return ip.Mask(net.CIDRMask(32, 128)).String()
	}
}

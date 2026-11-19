package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
)

// Filters and blocks from peers.
//
// The light client verifies the header chain against -peers (crosscheck.go)
// and the filter-header chain across them (confirmFilterHeaders), but used to
// take the filters and block bodies themselves from the one API node. Those
// bytes are verified wherever they come from — a filter must fold onto the
// confirmed filter-header chain and be keyed as its header dictates, a block
// must hash to its header and fold to its merkle root — so the API node could
// not lie with them. It could still WITHHOLD them: a node that refuses the one
// filter or block a wallet needs leaves the client with nothing to verify.
//
// So with -peers, filters and blocks are fetched from the peers, a range at a
// time, handed round so no single peer carries the whole scan. An answer that
// fails verification, or a peer that cannot answer, costs that peer its turn
// and the next one is asked. Only when every peer has failed a range does the
// client fall back to the API node, and it says so: the answer is just as
// verified, but the point of asking peers was not to depend on that node.

// spvFilterChunk is how many heights one peer is asked for at a time. Big
// enough that a long scan is not one dial per page, small enough that it is
// spread across peers rather than landing on the first one.
const spvFilterChunk = 1024

// spvPool is the session pool for the current command, or nil without -peers.
var spvPool *spvPeerPool

// spvPeerPool lazily opens light sessions to -peers and rotates requests
// across them. A peer that fails is dropped for the rest of the command, and
// the reason is kept for the error that names every peer.
type spvPeerPool struct {
	addrs []string
	psk   []byte
	sess  map[string]*node.LightPeer
	dead  map[string]error
	next  int
	dial  func(addr string, psk []byte) (*node.LightPeer, error)
}

func newSPVPeerPool(addrs []string, psk []byte) *spvPeerPool {
	return &spvPeerPool{
		addrs: addrs,
		psk:   psk,
		sess:  map[string]*node.LightPeer{},
		dead:  map[string]error{},
		dial:  node.DialLightPeer,
	}
}

// try runs fn against peers in turn, starting after the last one that
// answered, until one succeeds. fn's error means "this peer's answer is not
// usable" — unreachable, short, or failing verification — and moves on.
func (pp *spvPeerPool) try(fn func(lp *node.LightPeer) error) error {
	for i := 0; i < len(pp.addrs); i++ {
		k := (pp.next + i) % len(pp.addrs)
		addr := pp.addrs[k]
		if _, gone := pp.dead[addr]; gone {
			continue
		}
		lp, ok := pp.sess[addr]
		if !ok {
			var err error
			if lp, err = pp.dial(addr, pp.psk); err != nil {
				pp.dead[addr] = err
				continue
			}
			pp.sess[addr] = lp
		}
		if err := fn(lp); err != nil {
			pp.dead[addr] = err
			lp.Close()
			delete(pp.sess, addr)
			continue
		}
		pp.next = k + 1
		return nil
	}
	var why []string
	for _, a := range pp.addrs {
		if err := pp.dead[a]; err != nil {
			why = append(why, err.Error())
		}
	}
	return fmt.Errorf("no peer could answer: %s", strings.Join(why, "; "))
}

func (pp *spvPeerPool) Close() {
	for a, lp := range pp.sess {
		lp.Close()
		delete(pp.sess, a)
	}
}

// checkFilterRun verifies filters for heights from.. against the verified
// header chain and filter-header chain: each filter must be bound to its
// header (height, block hash, SipHash key) and the run must fold, from the
// filter header below it, onto exactly the chain the client already trusts.
func checkFilterRun(headers []core.Header, cfheaders []string, from uint64, filters []core.BlockFilter) error {
	if end := from + uint64(len(filters)); end > uint64(len(headers)) || end > uint64(len(cfheaders)) {
		return fmt.Errorf("filters up to height %d, beyond the %d verified headers and %d filter headers",
			end-1, len(headers), len(cfheaders))
	}
	var prev string
	if from > 0 {
		prev = cfheaders[from-1]
	}
	folded, err := core.FoldFilterHeaders(prev, filters)
	if err != nil {
		return err
	}
	for i, f := range filters {
		h := from + uint64(i)
		if err := core.CheckFilterBinding(f, headers[h]); err != nil {
			return err
		}
		if folded[i] != cfheaders[h] {
			return fmt.Errorf("filter %d is inconsistent with the filter-header chain", h)
		}
	}
	return nil
}

// fetchVerifiedFilters returns the filters for heights from..to, each checked
// by checkFilterRun. With -peers it asks them a chunk at a time; otherwise,
// or for a chunk no peer could serve, it asks the API node.
func fetchVerifiedFilters(base string, headers []core.Header, cfheaders []string, from, to uint64) ([]core.BlockFilter, error) {
	if to < from {
		return nil, nil
	}
	if spvPool == nil {
		fs, err := fetchFilterRangeHTTP(base, from, to)
		if err != nil {
			return nil, err
		}
		return fs, checkFilterRun(headers, cfheaders, from, fs)
	}
	var out []core.BlockFilter
	fromPeers, fromAPI := 0, 0
	if from == 0 {
		// Genesis is the one block every client already holds, and a node
		// answers filters over the wire from its body base, height 1. Build it
		// here; it is checked against the filter-header chain all the same.
		g := []core.BlockFilter{core.BuildBlockFilter(core.GenesisBlock())}
		if err := checkFilterRun(headers, cfheaders, 0, g); err != nil {
			return nil, fmt.Errorf("genesis filter: %w", err)
		}
		out = g
		if from = 1; from > to {
			return out, nil
		}
	}
	for lo := from; lo <= to; lo += spvFilterChunk {
		hi := lo + spvFilterChunk - 1
		if hi > to {
			hi = to
		}
		var got []core.BlockFilter
		err := spvPool.try(func(lp *node.LightPeer) error {
			set, err := lp.CFilters(lo, hi)
			if err != nil {
				return err
			}
			if set.From != lo || uint64(len(set.Filters)) != hi-lo+1 {
				return fmt.Errorf("%s served filters for %d heights from %d (base %d), asked %d..%d",
					set.Addr, len(set.Filters), set.From, set.Base, lo, hi)
			}
			if err := checkFilterRun(headers, cfheaders, lo, set.Filters); err != nil {
				return fmt.Errorf("%s: %w", set.Addr, err)
			}
			got = set.Filters
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: filters %d..%d: %v; using the API node (verified the same way)\n", lo, hi, err)
			if got, err = fetchFilterRangeHTTP(base, lo, hi); err != nil {
				return nil, err
			}
			if err := checkFilterRun(headers, cfheaders, lo, got); err != nil {
				return nil, err
			}
			fromAPI += len(got)
		} else {
			fromPeers += len(got)
		}
		out = append(out, got...)
	}
	fmt.Fprintf(os.Stderr, "✓ %d filter(s) from peers, %d from the API node\n", fromPeers, fromAPI)
	return out, nil
}

// fetchFilterRangeHTTP pages the API node's filters for heights from..to.
// A node holds no filters past its tip, so a short answer is returned short
// and the caller's length check reports it.
func fetchFilterRangeHTTP(base string, from, to uint64) ([]core.BlockFilter, error) {
	var all []core.BlockFilter
	for next := from; next <= to; {
		var page []core.BlockFilter
		if err := getJSON(fmt.Sprintf("%s/cfilters?from=%d", base, next), &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		if want := to - next + 1; uint64(len(page)) > want {
			page = page[:want]
		}
		all = append(all, page...)
		next += uint64(len(page))
	}
	return all, nil
}

// authenticateBlock checks a body against the header it is supposed to be.
func authenticateBlock(b core.Block, hdr core.Header) error {
	if b.Index != hdr.Index || b.Hash != hdr.Hash {
		return fmt.Errorf("block %d body does not match the verified header", hdr.Index)
	}
	if core.MerkleRoot(b.Transactions) != hdr.MerkleRoot {
		return fmt.Errorf("block %d transactions do not match the header merkle root", hdr.Index)
	}
	return nil
}

// spvFetchBlock returns the body of the block hdr describes, authenticated
// against it. With -peers it asks them first; the API node is the fallback.
func spvFetchBlock(base string, hdr core.Header) (core.Block, error) {
	if spvPool != nil {
		var got core.Block
		err := spvPool.try(func(lp *node.LightPeer) error {
			bs, err := lp.Blocks(hdr.Index, hdr.Index)
			if err != nil {
				return err
			}
			if err := authenticateBlock(bs[0], hdr); err != nil {
				return fmt.Errorf("%s: %w", lp.Addr(), err)
			}
			got = bs[0]
			return nil
		})
		if err == nil {
			return got, nil
		}
		fmt.Fprintf(os.Stderr, "warning: block %d: %v; using the API node (verified the same way)\n", hdr.Index, err)
	}
	b, err := fetchBlock(base, hdr.Index)
	if err != nil {
		return core.Block{}, err
	}
	return b, authenticateBlock(b, hdr)
}

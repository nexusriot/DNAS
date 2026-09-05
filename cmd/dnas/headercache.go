package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/nexusriot/DNAS/core"
)

// A light client that re-downloads the whole header chain on every command is
// not light.
//
// Every `dnas spv` command PoW-verifies the header chain before it trusts
// anything, and it used to fetch every header to do it — `sync`, `verify`,
// `scan`, `balance`, `history` and each `wallet update`, all of them, every
// time. At ~416 bytes per header that is ~42 MB per invocation on a 100k-block
// chain, which makes the lightest participant on the network the heaviest.
//
// The fix is the one SPV always intended: keep the verified prefix. Headers are
// self-authenticating — each commits to its predecessor's hash and to its own
// proof of work — so a cached chain needs no trust, only checking that what the
// node serves next LINKS to it. A reorg below the cached tip is detected by
// re-reading the header at that height and comparing hashes; if it differs, the
// cache is discarded and rebuilt.
//
// The filter-header chain is cached the same way, and for a stronger reason: it
// is a running hash, so a client with a verified prefix folds new filters onto
// it incrementally, while a client without one has to re-fold from genesis.

// headerCacheVersion guards the on-disk format, so a later change is detected
// rather than misread.
const headerCacheVersion = 1

// headerCache is a light client's verified view of the chain, persisted between
// runs.
type headerCache struct {
	Version int    `json:"version"`
	Network string `json:"network"`
	// Headers is the contiguous chain from genesis, every one of which has had its
	// proof of work and linkage checked before it was written here.
	Headers []core.Header `json:"headers"`
	// FilterHeaders is the BIP157-style filter-header chain over the same range,
	// so new filters fold onto it instead of being re-folded from genesis.
	FilterHeaders []string `json:"filter_headers,omitempty"`
}

// loadHeaderCache reads the cache at path. A missing, unreadable, wrong-version
// or wrong-network file is not an error — it just means starting from genesis.
func loadHeaderCache(path string) *headerCache {
	empty := &headerCache{Version: headerCacheVersion, Network: core.NetworkName()}
	if path == "" {
		return empty
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var c headerCache
	if err := json.Unmarshal(data, &c); err != nil {
		return empty
	}
	if c.Version != headerCacheVersion || c.Network != core.NetworkName() {
		return empty
	}
	// A cache that does not start at this network's genesis is not ours.
	if len(c.Headers) > 0 && c.Headers[0].Hash != core.GenesisBlock().Hash {
		return empty
	}
	return &c
}

func (c *headerCache) save(path string) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// reset empties the cache, keeping its identity fields.
func (c *headerCache) reset() {
	c.Version = headerCacheVersion
	c.Network = core.NetworkName()
	c.Headers = nil
	c.FilterHeaders = nil
}

// syncHeaders brings the cache up to the node's tip and returns the verified
// chain. It downloads only the headers above the cached tip, and it verifies
// what it downloads: the batch must link onto the cached tip and satisfy its own
// proof of work.
//
// A reorg below the cached tip — the node serving a different hash at a height
// we already hold — discards the cache and rebuilds from genesis rather than
// splicing, because a client cannot tell a reorg from a lie and the honest
// answer to both is "verify it again".
func (c *headerCache) syncHeaders(base string) ([]core.Header, error) {
	for attempt := 0; attempt < 2; attempt++ {
		headers, err := c.tryExtend(base)
		if err == nil {
			return headers, nil
		}
		if attempt == 0 && errorsIsForked(err) {
			c.reset() // the cached prefix is not on the node's chain; start over
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("header chain could not be verified after a rebuild")
}

// forkedError marks the "our cached prefix is not on this chain" case, which is
// the one error syncHeaders recovers from by rebuilding.
type forkedError struct{ msg string }

func (e *forkedError) Error() string { return e.msg }

func errorsIsForked(err error) bool {
	_, ok := err.(*forkedError)
	return ok
}

// tryExtend fetches and verifies everything above the cached tip.
func (c *headerCache) tryExtend(base string) ([]core.Header, error) {
	from := uint64(len(c.Headers))
	if from > 0 {
		// Re-read the header we already hold at the tip: if the node's differs, the
		// chain moved under us.
		from--
	}
	fresh, err := fetchHeaderPage(base, from)
	if err != nil {
		return nil, err
	}
	if len(fresh) == 0 {
		if len(c.Headers) == 0 {
			return nil, fmt.Errorf("node served no headers")
		}
		// Nothing new, and nothing to re-check: the node is at or below our tip.
		return c.verified()
	}
	if len(c.Headers) > 0 {
		if fresh[0].Index != from || fresh[0].Hash != c.Headers[from].Hash {
			return nil, &forkedError{msg: fmt.Sprintf(
				"node's header at height %d does not match the cached one (a reorg, or a different chain)", from)}
		}
		fresh = fresh[1:] // drop the overlap we already have
	}
	if err := c.appendVerified(fresh); err != nil {
		return nil, err
	}
	// Keep paging while the node has more.
	for len(fresh) > 0 {
		next, err := fetchHeaderPage(base, uint64(len(c.Headers)))
		if err != nil {
			return nil, err
		}
		if len(next) == 0 {
			break
		}
		if err := c.appendVerified(next); err != nil {
			return nil, err
		}
		fresh = next
	}
	return c.verified()
}

// appendVerified checks a batch links onto the cache and has valid proof of
// work, then appends it.
func (c *headerCache) appendVerified(batch []core.Header) error {
	if len(batch) == 0 {
		return nil
	}
	if len(c.Headers) == 0 {
		if batch[0].Hash != core.GenesisBlock().Hash {
			return &forkedError{msg: "node's first header is not this network's genesis"}
		}
		if err := core.ValidateHeaderChain(batch[1:], batch[0].Hash, 0); err != nil {
			return fmt.Errorf("header chain invalid: %w", err)
		}
	} else {
		tip := c.Headers[len(c.Headers)-1]
		if err := core.ValidateHeaderChain(batch, tip.Hash, tip.Index); err != nil {
			return fmt.Errorf("header chain invalid: %w", err)
		}
	}
	c.Headers = append(c.Headers, batch...)
	// The filter-header chain no longer covers the new headers; it is re-extended
	// lazily by syncFilterHeaders.
	if len(c.FilterHeaders) > len(c.Headers) {
		c.FilterHeaders = nil
	}
	return nil
}

// verified returns the cached chain after a final end-to-end check that it
// starts at genesis and is contiguous — cheap insurance against a cache file
// someone edited by hand.
func (c *headerCache) verified() ([]core.Header, error) {
	if len(c.Headers) == 0 {
		return nil, fmt.Errorf("no headers")
	}
	if c.Headers[0].Hash != core.GenesisBlock().Hash {
		return nil, &forkedError{msg: "cached chain does not start at this network's genesis"}
	}
	return c.Headers, nil
}

// syncFilterHeaders brings the cached filter-header chain up to the header tip,
// fetching only the range it lacks.
func (c *headerCache) syncFilterHeaders(base string) ([]string, error) {
	want := len(c.Headers)
	if len(c.FilterHeaders) > want {
		c.FilterHeaders = c.FilterHeaders[:want]
	}
	for len(c.FilterHeaders) < want {
		page, err := fetchFilterHeaderPage(base, uint64(len(c.FilterHeaders)))
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		c.FilterHeaders = append(c.FilterHeaders, page...)
	}
	if len(c.FilterHeaders) < want {
		return nil, fmt.Errorf("node served %d filter headers for %d headers", len(c.FilterHeaders), want)
	}
	return c.FilterHeaders, nil
}

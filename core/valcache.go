package core

import (
	"runtime"
	"sync"
)

// Signature verification is the most expensive thing a node does per transaction,
// and without a cache it is paid twice for every payment that confirms: once when
// the transaction is admitted to the mempool, and again when the block carrying it
// is applied. During initial sync it is paid once per transaction but serially,
// on one core, while the other cores idle.
//
// ValidationCache fixes both. It remembers which transaction ids have already had
// their authorization verified, so the second check is a map lookup; and
// verifyAllAuthorized pre-warms it for a whole block in parallel, so the serial
// state machine that follows finds every signature already checked.
//
// What it caches is deliberately narrow: only that *this exact transaction*
// (identified by its txid, which commits to every field including the signatures)
// carries valid authorization. Height rules, nonces and balances all still run on
// every application, because those depend on context the txid says nothing about.

// DefaultValidationCacheSize is how many verified transaction ids a cache keeps.
// Each entry is a hex txid, so this is a few megabytes at most, and it only needs
// to cover the window between a transaction entering the mempool and the block
// carrying it arriving.
const DefaultValidationCacheSize = 100_000

// ValidationCache is a bounded set of transaction ids whose authorization has
// been verified. It is safe for concurrent use, and a nil *ValidationCache is
// usable — it simply verifies every time and caches nothing.
type ValidationCache struct {
	mu    sync.Mutex
	max   int
	seen  map[string]struct{}
	order []string // insertion order, for bounded FIFO eviction
}

// NewValidationCache returns a cache holding up to max entries (values <= 0 fall
// back to DefaultValidationCacheSize).
func NewValidationCache(max int) *ValidationCache {
	if max <= 0 {
		max = DefaultValidationCacheSize
	}
	return &ValidationCache{max: max, seen: make(map[string]struct{})}
}

// Verify checks tx's authorization, skipping the work if this exact transaction
// has been verified before. A nil cache always verifies.
func (c *ValidationCache) Verify(tx Transaction) error {
	if c == nil {
		return tx.VerifySignature()
	}
	h := tx.Hash()
	c.mu.Lock()
	_, known := c.seen[h]
	c.mu.Unlock()
	if known {
		return nil
	}
	if err := tx.VerifySignature(); err != nil {
		return err
	}
	c.mark(h)
	return nil
}

// mark records a txid as verified, evicting the oldest entry when full.
func (c *ValidationCache) mark(h string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.seen[h]; dup {
		return
	}
	if len(c.order) >= c.max {
		delete(c.seen, c.order[0])
		c.order = c.order[1:]
	}
	c.seen[h] = struct{}{}
	c.order = append(c.order, h)
}

// Len reports how many verified transactions the cache is holding.
func (c *ValidationCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

// parallelVerifyThreshold is the number of non-coinbase transactions a block must
// carry before pre-warming is worth the goroutines. Below it the serial pass is
// already cheap and the scheduling would cost more than it saves.
const parallelVerifyThreshold = 4

// verifyAllAuthorized pre-warms the cache for every non-coinbase transaction in
// the block, using all available cores. Errors are deliberately discarded: this
// is only a warm-up, and the serial application pass that follows re-checks every
// transaction through the same cache, so it is what reports which transaction
// failed and why. Skipping the error here keeps one code path authoritative.
func (c *ValidationCache) verifyAllAuthorized(txs []Transaction) {
	if c == nil || len(txs) < parallelVerifyThreshold+1 {
		return
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > len(txs)-1 {
		workers = len(txs) - 1
	}
	if workers < 2 {
		return
	}
	jobs := make(chan Transaction)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tx := range jobs {
				_ = c.Verify(tx)
			}
		}()
	}
	for i := 1; i < len(txs); i++ { // 0 is the coinbase, which carries no signature
		jobs <- txs[i]
	}
	close(jobs)
	wg.Wait()
}

// VerifyOps is the number of signature verifications a transaction can cost in
// the worst case — the metered quantity behind MaxBlockVerifyOps.
//
// A single-key or HTLC spend is one verification. A multisig spend is up to
// signatures × members, because each signature is tried against the members not
// yet matched. Charging the worst case rather than the observed cost keeps the
// measure a pure function of the transaction, so every node agrees on it without
// doing the work first.
func VerifyOps(tx Transaction) int {
	if tx.IsCoinbase() {
		return 0
	}
	if tx.IsMultisig() {
		ops := len(tx.Signatures) * len(tx.Multisig.PubKeys)
		if ops < 1 {
			ops = 1
		}
		return ops
	}
	return 1
}

// BlockVerifyOps is the total worst-case verification cost of a block's
// transactions.
func BlockVerifyOps(txs []Transaction) int {
	total := 0
	for _, tx := range txs {
		total += VerifyOps(tx)
	}
	return total
}

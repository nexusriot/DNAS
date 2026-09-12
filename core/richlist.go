package core

import (
	"container/heap"
	"sort"
)

// The rich list: who holds the coin.
//
// The address index (addrindex.go) answers "what happened to this address" and
// is off by default because its size is unbounded by the chain. This answers a
// different question — "who holds the most" — and needs no index at all, because
// the answer is already in the account state the chain keeps resident.
//
// What it costs is a walk of every account per call. That is honest for a ledger
// that already lives in memory (reading it IS the state) and it is the reason the
// result is a bounded TOP-N rather than a sorted ledger: a chain with a million
// accounts should not be able to turn one HTTP request into a million-element
// sort. A partial selection through a bounded min-heap is O(accounts × log N)
// with N fixed, so the cost grows with the ledger and not with what is asked for.

// DefaultRichListLimit is how many holders a query returns by default, and
// MaxRichListLimit the most it may ask for.
const (
	DefaultRichListLimit = 25
	MaxRichListLimit     = 500
)

// RichListEntry is one holder's place in the ranking.
type RichListEntry struct {
	Rank       int     `json:"rank"`
	Address    string  `json:"address"`
	Balance    uint64  `json:"balance"`
	BalanceFmt string  `json:"balance_fmt"`
	Percent    float64 `json:"percent"` // of circulating supply
	Nonce      uint64  `json:"nonce"`
	Assets     int     `json:"assets"` // how many distinct assets this account holds
}

// RichList is the ranking plus the totals that give it meaning: a holder's
// balance says little without knowing how much coin exists and how many accounts
// share it.
type RichList struct {
	Height      uint64          `json:"height"`
	Accounts    int             `json:"accounts"`
	Circulating uint64          `json:"circulating"`
	Shown       int             `json:"shown"`
	TopShare    float64         `json:"top_share"` // % of circulating held by the entries shown
	Entries     []RichListEntry `json:"entries"`
}

// holderHeap is a MIN-heap of holders: the smallest balance sits at the root, so
// keeping the top N is "push, and pop the root once the heap is over N".
type holderHeap []RichListEntry

func (h holderHeap) Len() int      { return len(h) }
func (h holderHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h holderHeap) Less(i, j int) bool {
	if h[i].Balance != h[j].Balance {
		return h[i].Balance < h[j].Balance
	}
	// Ties break on the address, reversed, so that popping the "smallest" discards
	// the one that would sort LAST in the final ranking. Without a total order
	// here, two accounts holding the same amount would make the list depend on Go's
	// map iteration order and differ between calls on identical state.
	return h[i].Address > h[j].Address
}

func (h *holderHeap) Push(x any) { *h = append(*h, x.(RichListEntry)) }

func (h *holderHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// RichList returns the `limit` largest coin holders at the current tip.
func (bc *Blockchain) RichList(limit int) RichList {
	if limit <= 0 {
		limit = DefaultRichListLimit
	}
	if limit > MaxRichListLimit {
		limit = MaxRichListLimit
	}

	bc.mu.RLock()
	defer bc.mu.RUnlock()

	out := RichList{
		Height:      bc.blocks[len(bc.blocks)-1].Index,
		Accounts:    len(bc.state),
		Circulating: totalBalance(bc.state),
	}

	top := &holderHeap{}
	heap.Init(top)
	for addr, acc := range bc.state {
		if acc.Balance == 0 {
			// An account with no coin is not a holder. It can still exist — an asset
			// holder, or an address whose balance was spent to zero — and listing it
			// would push a real holder out of the ranking.
			continue
		}
		heap.Push(top, RichListEntry{
			Address: addr, Balance: acc.Balance, Nonce: acc.Nonce, Assets: len(acc.Assets),
		})
		if top.Len() > limit {
			heap.Pop(top)
		}
	}

	entries := make([]RichListEntry, top.Len())
	copy(entries, *top)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Balance != entries[j].Balance {
			return entries[i].Balance > entries[j].Balance
		}
		return entries[i].Address < entries[j].Address
	})

	var shownTotal uint64
	for i := range entries {
		entries[i].Rank = i + 1
		entries[i].BalanceFmt = FormatAmount(entries[i].Balance)
		if out.Circulating > 0 {
			entries[i].Percent = 100 * float64(entries[i].Balance) / float64(out.Circulating)
		}
		shownTotal += entries[i].Balance
	}
	out.Entries = entries
	out.Shown = len(entries)
	if out.Circulating > 0 {
		out.TopShare = 100 * float64(shownTotal) / float64(out.Circulating)
	}
	return out
}

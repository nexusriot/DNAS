package node

import (
	"sort"
	"sync"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// Pool accounting: per-miner difficulty and a PPLNS payout window.
//
// shares.go counts shares. That is enough to see that a miner is working and not
// enough to run a pool, for three reasons the ROADMAP names: every miner is given
// the same target regardless of how fast it is, there is no scheme for turning
// shares into payouts, and the ledger dies with the process.
//
// PER-MINER DIFFICULTY (vardiff). One target for everyone is wrong in both
// directions: a fast miner floods the pool with shares it has to process, and a
// slow one may go a whole block without producing any, so its work is invisible
// and unpaid. Each connection therefore gets its own share target, tuned so it
// submits roughly one share every vardiffTarget seconds whatever its hashrate.
//
// PPLNS. Paying per share found (PPS) makes the POOL carry the variance, which
// only works if the pool has a float to carry it with. Pay-Per-Last-N-Shares pays
// out of the block that was actually found, in proportion to the work in a
// trailing window — so the pool never owes more than it earned, and a miner that
// hops in just before a block is found does not get a full block's credit for a
// minute of work.
//
// Because miners work at different difficulties, a share's WEIGHT is its own
// difficulty, not one. Without that, a miner on an easy target would out-earn a
// miner doing ten times the work.

// pplnsWindow bounds how many recent shares the payout is computed over. It is a
// count rather than a duration because that is what makes the payout independent
// of how fast blocks happen to be arriving.
const pplnsWindow = 4096

// Vardiff tuning: aim for one share every vardiffTarget, reassess no more often
// than vardiffInterval, and never move the factor by more than 4x at a time so a
// brief stall cannot drop a miner to a target it will flood.
const (
	vardiffTarget   = 10 * time.Second
	vardiffInterval = 30 * time.Second
	vardiffMinShare = 4         // reassess only with at least this many samples
	minShareFactor  = uint32(1) // as hard as a block
	maxShareFactor  = uint32(1) << 22
)

// PoolShare is one accepted share, as payout accounting sees it.
type PoolShare struct {
	Address string  `json:"address"`
	Worker  string  `json:"worker,omitempty"`
	Weight  float64 `json:"weight"` // the share's own difficulty
	Height  uint64  `json:"height"`
	At      int64   `json:"at"`
}

// PoolPayout is one address's claim on the next block found.
type PoolPayout struct {
	Address   string  `json:"address"`
	Shares    int     `json:"shares"`
	Weight    float64 `json:"weight"`
	Percent   float64 `json:"percent"`
	Amount    uint64  `json:"amount"`
	AmountFmt string  `json:"amount_fmt"`
}

// PoolReport is the whole payout picture: what the next block is worth, who has
// a claim on it, and how much work the window holds.
type PoolReport struct {
	Window      int          `json:"window"`     // shares held
	WindowMax   int          `json:"window_max"` // capacity
	Weight      float64      `json:"weight"`     // total work in the window
	Reward      uint64       `json:"reward"`     // what the next block pays
	RewardFmt   string       `json:"reward_fmt"`
	Payouts     []PoolPayout `json:"payouts"`
	Connections int          `json:"connections"` // live stratum sessions
}

// shareWindow is the PPLNS ring.
type shareWindow struct {
	mu     sync.Mutex
	max    int
	shares []PoolShare
	total  float64
}

func newShareWindow(max int) *shareWindow {
	if max <= 0 {
		max = pplnsWindow
	}
	return &shareWindow{max: max}
}

func (w *shareWindow) add(s PoolShare) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.shares) >= w.max {
		w.total -= w.shares[0].Weight
		w.shares = w.shares[1:]
	}
	w.shares = append(w.shares, s)
	w.total += s.Weight
}

func (w *shareWindow) snapshot() []PoolShare {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]PoolShare(nil), w.shares...)
}

// restore replaces the window with a saved one, dropping anything beyond
// capacity (oldest first) so a file written by a build with a larger window
// cannot push this one over its bound.
func (w *shareWindow) restore(shares []PoolShare) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(shares) > w.max {
		shares = shares[len(shares)-w.max:]
	}
	w.shares = append([]PoolShare(nil), shares...)
	w.total = 0
	for _, s := range w.shares {
		w.total += s.Weight
	}
}

// payouts splits `reward` across the window in proportion to each address's
// weight. The split is exact: integer division leaves a remainder of at most
// one base unit per address, and the whole remainder goes to the largest claim,
// so the parts always sum back to the reward rather than quietly burning dust.
func (w *shareWindow) payouts(reward uint64) (float64, []PoolPayout) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.shares) == 0 || w.total <= 0 {
		return 0, nil
	}
	type acc struct {
		n int
		f float64
	}
	by := map[string]*acc{}
	for _, s := range w.shares {
		a := by[s.Address]
		if a == nil {
			a = &acc{}
			by[s.Address] = a
		}
		a.n++
		a.f += s.Weight
	}
	out := make([]PoolPayout, 0, len(by))
	var assigned uint64
	for addr, a := range by {
		amt := uint64(float64(reward) * a.f / w.total)
		assigned += amt
		out = append(out, PoolPayout{
			Address: addr, Shares: a.n, Weight: a.f,
			Percent: 100 * a.f / w.total, Amount: amt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Address < out[j].Address
	})
	if reward > assigned && len(out) > 0 {
		out[0].Amount += reward - assigned
	}
	for i := range out {
		out[i].AmountFmt = core.FormatAmount(out[i].Amount)
	}
	return w.total, out
}

// Pool reports what the pool owes on the next block it finds.
func (n *Node) Pool() PoolReport {
	reward := core.BlockReward(n.chain.Height() + 1)
	total, payouts := n.window.payouts(reward)
	r := PoolReport{
		Window:    len(n.window.snapshot()),
		WindowMax: n.window.max,
		Weight:    total,
		Reward:    reward,
		RewardFmt: core.FormatAmount(reward),
		Payouts:   payouts,
	}
	if n.stratum != nil {
		r.Connections = n.stratum.sessions()
	}
	return r
}

// vardiff holds one connection's difficulty state.
type vardiff struct {
	factor   uint32
	since    time.Time
	accepted int
}

func newVardiff(start uint32) *vardiff {
	if start < minShareFactor {
		start = core.DefaultShareFactor
	}
	return &vardiff{factor: start, since: time.Now()}
}

// observe records an accepted share and returns the new factor plus whether it
// changed, so the caller knows to push a set_difficulty.
//
// The adjustment is the ratio of observed to target spacing, clamped to 4x per
// step: a miner that stalls briefly (a dropped job, a restart) would otherwise be
// handed a factor so easy that it floods the pool when it comes back.
func (v *vardiff) observe(now time.Time) (uint32, bool) {
	v.accepted++
	elapsed := now.Sub(v.since)
	if elapsed < vardiffInterval || v.accepted < vardiffMinShare {
		return v.factor, false
	}
	actual := elapsed / time.Duration(v.accepted)
	ratio := float64(actual) / float64(vardiffTarget)
	switch {
	case ratio > 4:
		ratio = 4
	case ratio < 0.25:
		ratio = 0.25
	}
	// A share is `factor` times easier than a block, so submitting too OFTEN
	// (ratio < 1) means the factor must come down.
	next := uint32(float64(v.factor) * ratio)
	switch {
	case next < minShareFactor:
		next = minShareFactor
	case next > maxShareFactor:
		next = maxShareFactor
	}
	v.since, v.accepted = now, 0
	if next == v.factor {
		return v.factor, false
	}
	v.factor = next
	return next, true
}

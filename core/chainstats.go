package core

import (
	"math/big"
	"sort"
)

// Chain analytics: what the numbers in a header actually imply.
//
// A node reports its difficulty, but difficulty is a target ratio — it says
// nothing directly about how much hashpower is behind the chain, whether blocks
// are arriving at the interval the retarget aims for, or what people are paying
// to get into them. Those are the three questions you ask when you want to know
// if a chain is healthy, and none of them could be answered from the API before.
//
// All of it is derived from block headers and bodies already in memory, so it is
// reporting, never consensus: a node that computes it differently is not on a
// different chain.

// DefaultStatsWindow is how many recent blocks the stats cover when no window is
// given: enough to smooth out proof-of-work variance, short enough to describe
// the chain as it is now rather than its whole history.
const DefaultStatsWindow = 144

// ChainStats summarizes recent chain activity over a window of blocks.
type ChainStats struct {
	Window     int    `json:"window"`      // blocks actually covered
	FromHeight uint64 `json:"from_height"` // first block in the window
	ToHeight   uint64 `json:"to_height"`   // last block (the tip)
	Seconds    int64  `json:"seconds"`     // wall-clock span of the window

	// Hashrate is the estimated network hashrate in hashes per second: the
	// cumulative work of the window divided by the time it took. It is an estimate
	// in the strict sense — proof of work is a random process, so a short window
	// says more about luck than about hashpower.
	Hashrate    float64 `json:"hashrate"`
	HashrateFmt string  `json:"hashrate_fmt"`

	// Block intervals, in seconds, against the TargetBlockTime the retarget aims
	// for. A median far from the target means the retarget is not keeping up (or
	// the chain is running with NoRetarget).
	TargetInterval int64 `json:"target_interval"`
	MinInterval    int64 `json:"min_interval"`
	MedianInterval int64 `json:"median_interval"`
	MaxInterval    int64 `json:"max_interval"`
	MeanInterval   int64 `json:"mean_interval"`

	// Difficulty over the window, so a caller can see it moving rather than only
	// its current value.
	MinDifficulty float64 `json:"min_difficulty"`
	MaxDifficulty float64 `json:"max_difficulty"`

	// Fees and space. Burned is the base-fee portion (destroyed); Tips is what
	// miners actually kept.
	Txs         int    `json:"txs"`           // non-coinbase transactions in the window
	Bytes       int    `json:"bytes"`         // their total serialized size
	FullestPct  int    `json:"fullest_pct"`   // fullest block, as a % of MaxBlockBytes
	Fees        uint64 `json:"fees"`          // total fees paid
	FeesFmt     string `json:"fees_fmt"`      //
	Burned      uint64 `json:"burned"`        // of which destroyed by the base fee
	BurnedFmt   string `json:"burned_fmt"`    //
	Tips        uint64 `json:"tips"`          // of which kept by miners
	TipsFmt     string `json:"tips_fmt"`      //
	MeanFeeRate uint64 `json:"mean_fee_rate"` // base units per byte across the window

	// Miners is who mined the window's blocks, most blocks first. On a chain with
	// few participants this is the closest thing to a hashpower distribution.
	Miners []MinerBlocks `json:"miners"`
}

// MinerBlocks is one coinbase recipient's share of a window.
type MinerBlocks struct {
	Address string `json:"address"`
	Blocks  int    `json:"blocks"`
	Percent int    `json:"percent"`
}

// Stats summarizes the last `window` blocks (0 or less means
// DefaultStatsWindow, and the window is clamped to the chain's length).
//
// Intervals need two blocks to make one gap, so a window of N blocks yields N-1
// intervals; a chain with only genesis yields none and the interval fields stay
// zero rather than being invented.
func (bc *Blockchain) Stats(window int) ChainStats {
	if window <= 0 {
		window = DefaultStatsWindow
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	n := len(bc.blocks)
	if window > n {
		window = n
	}
	blocks := bc.blocks[n-window:]
	st := ChainStats{
		Window:         len(blocks),
		FromHeight:     blocks[0].Index,
		ToHeight:       blocks[len(blocks)-1].Index,
		TargetInterval: TargetBlockTime,
	}

	work := new(big.Int)
	var intervals []int64
	minersByAddr := map[string]int{}
	fullest := 0
	for i, b := range blocks {
		work.Add(work, BlockWork(b.Bits))
		if d := TargetDifficulty(b.Bits); st.MinDifficulty == 0 || d < st.MinDifficulty {
			st.MinDifficulty = d
		}
		if d := TargetDifficulty(b.Bits); d > st.MaxDifficulty {
			st.MaxDifficulty = d
		}
		if i > 0 {
			intervals = append(intervals, b.Timestamp-blocks[i-1].Timestamp)
		}
		if len(b.Transactions) == 0 {
			continue // a header-only placeholder below a snapshot: nothing to count
		}
		if cb := b.Transactions[0]; cb.IsCoinbase() && cb.To != "" {
			minersByAddr[cb.To]++
		}
		weight := 0
		for _, tx := range b.Transactions[1:] {
			size := tx.Size()
			st.Txs++
			st.Bytes += size
			weight += size
			st.Fees += tx.Fee
			if burn := BaseFeeFor(tx, b.BaseFee); tx.Fee > burn {
				st.Burned += burn
				st.Tips += tx.Fee - burn
			} else {
				st.Burned += tx.Fee
			}
		}
		if pct := weight * 100 / MaxBlockBytes; pct > fullest {
			fullest = pct
		}
	}
	st.FullestPct = fullest
	if st.Bytes > 0 {
		st.MeanFeeRate = st.Fees / uint64(st.Bytes)
	}
	st.FeesFmt = FormatAmount(st.Fees)
	st.BurnedFmt = FormatAmount(st.Burned)
	st.TipsFmt = FormatAmount(st.Tips)

	if len(intervals) > 0 {
		st.Seconds = blocks[len(blocks)-1].Timestamp - blocks[0].Timestamp
		sorted := append([]int64(nil), intervals...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		st.MinInterval = sorted[0]
		st.MaxInterval = sorted[len(sorted)-1]
		st.MedianInterval = sorted[len(sorted)/2]
		var total int64
		for _, v := range sorted {
			total += v
		}
		st.MeanInterval = total / int64(len(sorted))
		// Hashrate = work / time. A window whose blocks all share a timestamp (a
		// fast regtest run) has no elapsed time to divide by, so it is left at zero
		// rather than reported as infinite.
		if st.Seconds > 0 {
			rate, _ := new(big.Float).Quo(new(big.Float).SetInt(work), big.NewFloat(float64(st.Seconds))).Float64()
			st.Hashrate = rate
			st.HashrateFmt = FormatHashrate(rate)
		}
	}

	st.Miners = make([]MinerBlocks, 0, len(minersByAddr))
	for addr, count := range minersByAddr {
		st.Miners = append(st.Miners, MinerBlocks{
			Address: addr, Blocks: count, Percent: count * 100 / len(blocks),
		})
	}
	sort.Slice(st.Miners, func(i, j int) bool {
		if st.Miners[i].Blocks != st.Miners[j].Blocks {
			return st.Miners[i].Blocks > st.Miners[j].Blocks
		}
		return st.Miners[i].Address < st.Miners[j].Address
	})
	return st
}

// FormatHashrate renders hashes per second in the largest unit that keeps it
// readable.
func FormatHashrate(hps float64) string {
	units := []string{"H/s", "kH/s", "MH/s", "GH/s", "TH/s", "PH/s", "EH/s"}
	i := 0
	for hps >= 1000 && i < len(units)-1 {
		hps /= 1000
		i++
	}
	return trimFloat(hps) + " " + units[i]
}

// trimFloat renders a float with two decimals and no trailing zeros.
func trimFloat(v float64) string {
	s := big.NewFloat(v).Text('f', 2)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

// DefaultSeriesLimit is how many points a series returns when none is asked for,
// and MaxSeriesLimit the most it will return.
const (
	DefaultSeriesLimit = 144
	MaxSeriesLimit     = 2000
)

// ChainPoint is one block's worth of the quantities worth plotting over time.
//
// ChainStats summarizes a window into single numbers, which answers "what is the
// chain doing now" and cannot answer "what has it been doing" — a median interval
// of 60s is the same number whether every block took 60s or half took 5s and half
// took 115s. A per-height series is what a chart needs, and deriving it here
// rather than in each client is what stops four clients each reimplementing the
// compact-target arithmetic.
type ChainPoint struct {
	Height    uint64 `json:"height"`
	Timestamp int64  `json:"timestamp"`
	// Interval is seconds since the previous block; 0 at the first point of the
	// series, where there is no previous block to measure from.
	Interval   int64   `json:"interval"`
	Difficulty float64 `json:"difficulty"`
	BaseFee    uint64  `json:"base_fee"`

	// Body reports whether this height's transactions are still held. A pruned
	// height keeps its header, so everything above is exact and Txs/Bytes/Fees
	// below are zero because the body is gone — not because the block was empty.
	Body  bool   `json:"body"`
	Txs   int    `json:"txs"`
	Bytes int    `json:"bytes"`
	Fees  uint64 `json:"fees"`
}

// Series returns per-height chain metrics from `from`, at most `limit` points.
func (bc *Blockchain) Series(from uint64, limit int) []ChainPoint {
	if limit <= 0 {
		limit = DefaultSeriesLimit
	}
	if limit > MaxSeriesLimit {
		limit = MaxSeriesLimit
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()

	if int(from) >= len(bc.blocks) {
		return []ChainPoint{}
	}
	end := int(from) + limit
	if end > len(bc.blocks) {
		end = len(bc.blocks)
	}
	out := make([]ChainPoint, 0, end-int(from))
	for i := int(from); i < end; i++ {
		b := bc.blocks[i]
		p := ChainPoint{
			Height:     b.Index,
			Timestamp:  b.Timestamp,
			Difficulty: TargetDifficulty(b.Bits),
			BaseFee:    b.BaseFee,
			Body:       !b.IsPlaceholder(),
		}
		if i > 0 {
			p.Interval = b.Timestamp - bc.blocks[i-1].Timestamp
		}
		// Genesis carries no transactions at all — not even a coinbase — so it is
		// not a placeholder and still has nothing to count.
		if p.Body && len(b.Transactions) > 0 {
			p.Txs = len(b.Transactions) - 1 // the coinbase is not a payment
			for _, tx := range b.Transactions[1:] {
				p.Bytes += tx.Size()
				p.Fees += tx.Fee
			}
		}
		out = append(out, p)
	}
	return out
}

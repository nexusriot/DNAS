package node

import "sync"

// Ban scoring: peers accumulate points for misbehaviour and are cut off once
// they reach the threshold. The key depends on when the misbehaviour is
// detectable: failed handshakes happen before a peer proves an identity, so they
// are keyed by remote IP (loopback exempt, see bannableIP); protocol-level
// misbehaviour after authentication (bad headers, invalid blocks) is keyed by
// the peer's authenticated identity. Scores persist across a graceful restart
// (see persist.go) and otherwise last the process lifetime.
const (
	banThreshold    = 100
	banHandshake    = 20 // failed secure/identity handshake (keyed by IP)
	banBadHeaders   = 34 // served an internally-invalid header chain (keyed by identity)
	banInvalidBlock = 20 // served a block that fails PoW/structure (keyed by identity)
)

type banbook struct {
	mu        sync.Mutex
	score     map[string]int
	threshold int
}

func newBanbook(threshold int) *banbook {
	return &banbook{score: map[string]int{}, threshold: threshold}
}

// add increases key's score and reports whether it is now banned.
func (b *banbook) add(key string, points int) bool {
	if key == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.score[key] += points
	return b.score[key] >= b.threshold
}

// banned reports whether key has reached the ban threshold.
func (b *banbook) banned(key string) bool {
	if key == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.score[key] >= b.threshold
}

// scoreOf returns key's current score (for inspection/tests).
func (b *banbook) scoreOf(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.score[key]
}

// snapshot returns a copy of the current ban scores, for persistence.
func (b *banbook) snapshot() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int, len(b.score))
	for k, v := range b.score {
		out[k] = v
	}
	return out
}

// restore merges persisted ban scores back in (used on startup so bans survive a
// restart instead of resetting).
func (b *banbook) restore(scores map[string]int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, v := range scores {
		b.score[k] = v
	}
}

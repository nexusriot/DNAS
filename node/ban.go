package node

import "sync"

// Ban scoring: peers accumulate points for misbehaviour and are banned once they
// reach the threshold. Scores are keyed by remote IP (harder to rotate than a
// keypair) and last for the process lifetime.
const (
	banThreshold    = 100
	banHandshake    = 20 // failed secure/identity handshake
	banBadHeaders   = 34 // served an internally-invalid header chain
	banInvalidBlock = 20 // served a block that fails PoW/structure
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

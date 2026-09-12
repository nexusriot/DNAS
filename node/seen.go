package node

import "sync"

// seenSet is a bounded, FIFO set of hashes used to de-duplicate gossip so we
// never re-process or re-broadcast the same block/transaction. It is bounded so
// a long-running node's memory does not grow without limit; the oldest entries
// are evicted first.
type seenSet struct {
	mu    sync.Mutex
	max   int
	set   map[string]struct{}
	order []string
}

func newSeenSet(max int) *seenSet {
	if max <= 0 {
		max = 1
	}
	return &seenSet{max: max, set: make(map[string]struct{})}
}

// seen reports whether h was already present. If it was not, h is recorded
// (evicting the oldest entry when full) and false is returned.
func (s *seenSet) seen(h string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[h]; ok {
		return true
	}
	if len(s.order) >= s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.set, oldest)
	}
	s.set[h] = struct{}{}
	s.order = append(s.order, h)
	return false
}

func (s *seenSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.set)
}

// has reports whether h is present WITHOUT recording it. Announcement-based
// relay needs this distinction: seeing a transaction announced is not the same as
// having it, and marking it on the announcement would mean never fetching the
// body if the peer that announced it went away.
func (s *seenSet) has(h string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.set[h]
	return ok
}

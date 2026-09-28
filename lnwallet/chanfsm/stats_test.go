package chanfsm

import "sync"

// diffStats counts what randomized runs saw, so a test can insist that every
// rule, plan or crash site it cares about was exercised.
type diffStats struct {
	mu     sync.Mutex
	counts map[string]int
}

// hit counts one occurrence of the named event.
func (s *diffStats) hit(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.counts == nil {
		s.counts = make(map[string]int)
	}
	s.counts[name]++
}

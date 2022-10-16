package wtclient

import (
	"fmt"
	"sync"
)

// ClientStats is a collection of in-memory statistics of the actions the client
// has performed since its creation.
type ClientStats struct {
	mu sync.Mutex

	// NumTasksPending is the total number of backups that are pending to
	// be acknowledged by all active and exhausted watchtower sessions.
	NumTasksPending int

	// NumTasksAccepted is the total number of backups made to all active
	// and exhausted watchtower sessions.
	NumTasksAccepted int

	// NumTasksIneligible is the total number of backups that all active and
	// exhausted watchtower sessions have failed to acknowledge.
	NumTasksIneligible int

	// NumSessionsAcquired is the total number of new sessions made to
	// watchtowers.
	NumSessionsAcquired int

	// NumSessionsExhausted is the total number of watchtower sessions that
	// have been exhausted.
	NumSessionsExhausted int
}

// taskReceived increments the number of backup requests the client has received
// from active channels.
func (s *ClientStats) taskReceived() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NumTasksPending++
}

// taskAccepted increments the number of tasks that have been assigned to active
// session queues, and are awaiting upload to a tower.
func (s *ClientStats) taskAccepted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NumTasksAccepted++
	s.NumTasksPending--
}

// taskIneligible increments the number of tasks that were unable to satisfy the
// active session queue's policy. These can potentially be retried later, but
// typically this means that the balance created dust outputs, so it may not be
// worth backing up at all.
func (s *ClientStats) taskIneligible() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NumTasksIneligible++
}

// sessionAcquired increments the number of sessions that have been successfully
// negotiated by the client during this execution.
func (s *ClientStats) sessionAcquired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NumSessionsAcquired++
}

// sessionExhausted increments the number of session that have become full as a
// result of accepting backup tasks.
func (s *ClientStats) sessionExhausted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NumSessionsExhausted++
}

// String returns a human-readable summary of the client's metrics.
func (s *ClientStats) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("tasks(received=%d accepted=%d ineligible=%d) "+
		"sessions(acquired=%d exhausted=%d)", s.NumTasksPending,
		s.NumTasksAccepted, s.NumTasksIneligible, s.NumSessionsAcquired,
		s.NumSessionsExhausted)
}

// Copy returns a copy of the current stats.
func (s *ClientStats) Copy() ClientStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ClientStats{
		NumTasksPending:      s.NumTasksPending,
		NumTasksAccepted:     s.NumTasksAccepted,
		NumTasksIneligible:   s.NumTasksIneligible,
		NumSessionsAcquired:  s.NumSessionsAcquired,
		NumSessionsExhausted: s.NumSessionsExhausted,
	}
}

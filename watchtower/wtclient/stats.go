package wtclient

import (
	"fmt"
	"sync"
)

// ClientStats is a collection of in-memory statistics of the actions the client
// has performed since its creation.
type ClientStats struct {
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

// clientStats wraps ClientStats with a mutex so that it's members can be
// accessed in a thread safe manner.
type clientStats struct {
	mu sync.Mutex

	ClientStats
}

// taskReceived increments the number of backup requests the client has received
// from active channels.
func (s *clientStats) taskReceived() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.NumTasksPending++
}

// taskAccepted increments the number of tasks that have been assigned to active
// session queues, and are awaiting upload to a tower.
func (s *clientStats) taskAccepted() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.NumTasksAccepted++
	s.NumTasksPending--
}

// getStatsCopy returns a copy of the ClientStats.
func (s *clientStats) getStatsCopy() ClientStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ClientStats
}

// taskIneligible increments the number of tasks that were unable to satisfy the
// active session queue's policy. These can potentially be retried later, but
// typically this means that the balance created dust outputs, so it may not be
// worth backing up at all.
func (s *clientStats) taskIneligible() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.NumTasksIneligible++
}

// sessionAcquired increments the number of sessions that have been successfully
// negotiated by the client during this execution.
func (s *clientStats) sessionAcquired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.NumSessionsAcquired++
}

// sessionExhausted increments the number of session that have become full as a
// result of accepting backup tasks.
func (s *clientStats) sessionExhausted() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.NumSessionsExhausted++
}

// String returns a human-readable summary of the client's metrics.
func (s *clientStats) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return fmt.Sprintf("tasks(received=%d accepted=%d ineligible=%d) "+
		"sessions(acquired=%d exhausted=%d)", s.NumTasksPending,
		s.NumTasksAccepted, s.NumTasksIneligible, s.NumSessionsAcquired,
		s.NumSessionsExhausted)
}

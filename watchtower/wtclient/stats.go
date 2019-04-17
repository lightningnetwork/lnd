package wtclient

import "fmt"

type clientStats struct {
	numTasksReceived     int
	numTasksAccepted     int
	numTasksIneligible   int
	numSessionsAcquired  int
	numSessionsExhausted int
}

// taskReceived increments the number to backup requests the client has received
// from active channels.
func (s *clientStats) taskReceived() {
	s.numTasksReceived++
}

// taskAccepted increments the number of tasks that have been assigned to active
// session queues, and are awaiting upload to a tower.
func (s *clientStats) taskAccepted() {
	s.numTasksAccepted++
}

// taskIneligible increments the number of tasks that were unable to satisfy the
// active session queue's policy. These can potentially be retried later, but
// typically this means that the balance created dust outputs, so it may not be
// worth backing up at all.
func (s *clientStats) taskIneligible() {
	s.numTasksIneligible++
}

// sessionAcquired increments the number of sessions that have been successfully
// negotiated by the client during this execution.
func (s *clientStats) sessionAcquired() {
	s.numSessionsAcquired++
}

// sessionExhausted increments the number of session that have become full as a
// result of accepting backup tasks.
func (s *clientStats) sessionExhausted() {
	s.numSessionsExhausted++
}

// String returns a human readable summary of the client's metrics.
func (s clientStats) String() string {
	return fmt.Sprintf("tasks(received=%d accepted=%d ineligible=%d) "+
		"sessions(acquired=%d exhausted=%d)", s.numTasksReceived,
		s.numTasksAccepted, s.numTasksIneligible, s.numSessionsAcquired,
		s.numSessionsExhausted)
}

package descriptorsweep

import (
	"errors"
	"fmt"
	"time"
)

type retryableError struct {
	err error
}

type deterministicError struct {
	err error
}

func (e *deterministicError) Error() string { return e.err.Error() }
func (e *deterministicError) Unwrap() error { return e.err }

func deterministic(err error) error {
	if err == nil {
		return nil
	}

	return &deterministicError{err: err}
}

func isDeterministic(err error) bool {
	var target *deterministicError
	return errors.As(err, &target)
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func retryable(err error) error {
	if err == nil || isRetryable(err) {
		return err
	}

	return &retryableError{err: err}
}

func isRetryable(err error) bool {
	var target *retryableError
	return errors.As(err, &target)
}

func (s *Service) retryBounds() (time.Duration, time.Duration) {
	initial, maximum := s.retryInitial, s.retryMax
	if initial <= 0 {
		initial = defaultRetryInitial
	}
	if maximum < initial {
		maximum = initial
	}

	return initial, maximum
}

// scheduleRetry runs at most one retry worker for a registration and operation
// kind. Delays grow exponentially but are capped, and every wait can be
// interrupted by Stop.
func (s *Service) scheduleRetry(key retryKey, task func() error,
	onDeterministic func(error)) {

	s.mu.Lock()
	select {
	case <-s.quit:
		s.mu.Unlock()
		return
	default:
	}
	if s.retrying == nil {
		s.retrying = make(map[retryKey]bool)
	}
	if _, ok := s.retrying[key]; ok {
		// The active worker will run the task again even if its current
		// call succeeds. This closes the race where a newly attached
		// result stream fails before the worker that attached it exits.
		s.retrying[key] = true
		s.mu.Unlock()
		return
	}
	s.retrying[key] = false
	worker := func() {
		defer s.wg.Done()

		initial, maximum := s.retryBounds()
		backoff := initial
		for {
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-s.quit:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				s.mu.Lock()
				delete(s.retrying, key)
				s.mu.Unlock()

				return
			}

			err := task()
			if err == nil {
				s.mu.Lock()
				pending := s.retrying[key]
				if pending {
					s.retrying[key] = false
					s.mu.Unlock()
					backoff = initial

					continue
				}
				delete(s.retrying, key)
				s.mu.Unlock()

				return
			}
			if !isRetryable(err) {
				s.mu.Lock()
				delete(s.retrying, key)
				s.mu.Unlock()
				if onDeterministic != nil {
					onDeterministic(err)
				}

				return
			}
			s.mu.Lock()
			s.retrying[key] = false
			s.mu.Unlock()

			if backoff < maximum {
				backoff *= 2
				if backoff > maximum {
					backoff = maximum
				}
			}
		}
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go worker()
}

func (s *Service) handleRegistrationError(id RegistrationID, err error) {
	if err == nil {
		return
	}
	if isDeterministic(err) {
		s.failDurably(id, err)

		return
	}

	s.scheduleRetry(retryKey{id: id, kind: "resume"}, func() error {
		return s.resume(id)
	}, func(err error) {
		s.failDurably(id, err)
	})
}

func (s *Service) failDurably(id RegistrationID, failure error) {
	err := s.persistTransition(id, func(next *storedRecord) error {
		next.Status = StatusFailed
		next.Error = failure.Error()
		return nil
	})
	if err == nil {
		return
	}

	s.scheduleRetry(
		retryKey{id: id, kind: "persist-failure"}, func() error {
			return s.persistTransition(
				id, func(next *storedRecord) error {
					next.Status = StatusFailed
					next.Error = failure.Error()

					return nil
				},
			)
		}, nil,
	)
}

func (s *Service) persistTransition(id RegistrationID,
	mutate func(*storedRecord) error) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.updateRecordLocked(id, mutate)

	return err
}

func retryablef(format string, args ...interface{}) error {
	return retryable(fmt.Errorf(format, args...))
}

func (s *Service) launch(worker func()) bool {
	s.mu.Lock()
	select {
	case <-s.quit:
		s.mu.Unlock()
		return false
	default:
		s.wg.Add(1)
		s.mu.Unlock()
		go worker()

		return true
	}
}

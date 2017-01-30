package htlcswitch

import (
	"github.com/go-errors/errors"
	"sync"
	"sync/atomic"
)

// Service...
type Service struct {
	// started...
	started int32

	// shutdown...
	shutdown int32

	// wg..
	wg sync.WaitGroup

	// err...
	err error

	// Name...
	Name string

	// Quit...
	Quit chan bool
}

// NewService...
func NewService(name string) *Service {
	return &Service{
		Quit: make(chan bool, 1),
		Name: name,
	}
}

// Start...
func (s *Service) Start() error {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		return errors.Errorf("service %v already started", s.Name)
	}
	return nil
}

// Stop...
func (s *Service) Stop(err error) error {
	if !atomic.CompareAndSwapInt32(&s.shutdown, 0, 1) {
		return errors.Errorf("service %v already stoped", s.Name)
	}

	s.err = err
	close(s.Quit)

	return nil
}

// Wait...
func (s *Service) Wait() error {
	s.wg.Wait()
	return s.err
}

// Go...
func (s *Service) Go(f func()) {
	s.wg.Add(1)
	go f()
}

// Done...
func (s *Service) Done() {
	s.wg.Done()
}

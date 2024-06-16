package graph

import (
	"sync"
	"sync/atomic"
)

// Config holds the configuration required by the Builder.
type Config struct {
}

// Builder builds and maintains a view of the Lightning Network graph.
type Builder struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	cfg *Config

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewBuilder constructs a new Builder.
func NewBuilder(cfg *Config) (*Builder, error) {
	return &Builder{
		cfg:  cfg,
		quit: make(chan struct{}),
	}, nil
}

// Start launches all the goroutines the Builder requires to carry out its
// duties. If the builder has already been started, then this method is a noop.
func (b *Builder) Start() error {
	if !atomic.CompareAndSwapUint32(&b.started, 0, 1) {
		return nil
	}

	log.Info("Builder starting")

	return nil
}

// Stop signals to the Builder that it should halt all routines. This method
// will *block* until all goroutines have excited. If the builder has already
// stopped then this method will return immediately.
func (b *Builder) Stop() error {
	if !atomic.CompareAndSwapUint32(&b.stopped, 0, 1) {
		return nil
	}

	log.Info("Builder shutting down...")
	defer log.Debug("Builder shutdown complete")

	close(b.quit)
	b.wg.Wait()

	return nil
}

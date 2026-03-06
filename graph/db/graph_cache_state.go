package graphdb

import (
	"sync"
	"sync/atomic"
)

// graphCacheState tracks the in-memory graph cache together with its
// population state. The underlying GraphCache is independently thread-safe, so
// once reads are allowed to use it, they do not need to hold updateMtx.
type graphCacheState struct {
	cache  *GraphCache
	loaded atomic.Bool

	updateMtx sync.Mutex
	loading   bool

	pendingUpdates []func(*GraphCache)
}

// newGraphCacheState constructs a graph cache state with a new cache instance.
func newGraphCacheState(preAllocNumNodes int) *graphCacheState {
	return &graphCacheState{
		cache: NewGraphCache(preAllocNumNodes),
	}
}

// isLoaded reports whether the cache has finished its initial population and
// is safe to serve reads from.
func (s *graphCacheState) isLoaded() bool {
	return s.loaded.Load()
}

// stats returns the cache stats if the cache has finished its initial
// population.
func (s *graphCacheState) stats() (string, bool) {
	if !s.isLoaded() {
		return "", false
	}

	return s.cache.Stats(), true
}

// beginPopulation marks the cache as loading and starts buffering concurrent
// cache mutations until the population pass completes.
func (s *graphCacheState) beginPopulation() {
	s.updateMtx.Lock()
	defer s.updateMtx.Unlock()

	s.loading = true
	s.pendingUpdates = nil
}

// finishPopulation replays any buffered mutations and marks the cache as ready
// when the initial population completed successfully. Buffered mutations are
// always replayed so that the cache remains consistent with the DB for
// subsequent writes even if population failed.
func (s *graphCacheState) finishPopulation(loaded bool) {
	s.updateMtx.Lock()
	defer s.updateMtx.Unlock()

	for _, update := range s.pendingUpdates {
		update(s.cache)
	}

	if loaded {
		s.loaded.Store(true)
	}

	s.pendingUpdates = nil
	s.loading = false
}

// applyUpdate applies a cache mutation immediately or buffers it when the
// cache is still being populated.
func (s *graphCacheState) applyUpdate(update func(cache *GraphCache)) {
	s.updateMtx.Lock()
	defer s.updateMtx.Unlock()

	if s.loading {
		s.pendingUpdates = append(s.pendingUpdates, update)

		return
	}

	update(s.cache)
}

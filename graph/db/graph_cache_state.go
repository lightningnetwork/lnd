package graphdb

import (
	"sync"
	"sync/atomic"
)

// pendingUpdatesWarnThreshold is the number of buffered cache mutations at
// which a warning is logged. A large buffer indicates that cache population is
// taking a long time relative to the incoming gossip rate.
const pendingUpdatesWarnThreshold = 10_000

// graphCacheState tracks the in-memory graph cache together with its
// population state. The underlying GraphCache is independently thread-safe, so
// once reads are allowed to use it, they do not need to hold updateMtx.
type graphCacheState struct {
	graphCache *GraphCache
	loaded     atomic.Bool
	failed     atomic.Bool

	updateMtx sync.Mutex
	loading   bool

	pendingUpdates []func(*GraphCache)
}

// newGraphCacheState constructs a graph cache state with a new cache instance.
func newGraphCacheState(preAllocNumNodes int) *graphCacheState {
	return &graphCacheState{
		graphCache: NewGraphCache(preAllocNumNodes),
	}
}

// isLoaded reports whether the cache has finished its initial population and
// is safe to serve reads from.
func (s *graphCacheState) isLoaded() bool {
	return s.loaded.Load()
}

// isFailed reports whether the cache population attempt has failed.
func (s *graphCacheState) isFailed() bool {
	return s.failed.Load()
}

// stats returns the cache stats if the cache has finished its initial
// population.
func (s *graphCacheState) stats() (string, bool) {
	if !s.isLoaded() {
		return "", false
	}

	return s.graphCache.Stats(), true
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
// when the initial population completed successfully. If population failed,
// buffered mutations are discarded since the cache won't be used for reads.
func (s *graphCacheState) finishPopulation(loaded bool) {
	s.updateMtx.Lock()
	defer s.updateMtx.Unlock()

	if loaded {
		for _, update := range s.pendingUpdates {
			update(s.graphCache)
		}

		s.loaded.Store(true)
	} else {
		s.failed.Store(true)
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

		if len(s.pendingUpdates)%pendingUpdatesWarnThreshold == 0 {
			log.Warnf("Graph cache has %d pending updates "+
				"buffered during population",
				len(s.pendingUpdates))
		}

		return
	}

	update(s.graphCache)
}

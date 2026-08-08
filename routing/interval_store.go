package routing

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// DefaultMaxIntervalHistory is the default number of directed channels the
// interval store will remember. Entries are created in pairs, one per
// direction, so this is roughly half as many channels. Mission control bounds
// its own history the same way, for the same reason: a long lived node would
// otherwise accumulate an entry for every channel it has ever touched.
const DefaultMaxIntervalHistory = 10000

// DefaultIntervalFlushInterval is how often the store writes accumulated
// beliefs down when a persister is attached.
const DefaultIntervalFlushInterval = time.Second

// intervalEvictionFraction is the fraction of the store dropped when it grows
// past its bound. Evicting a batch rather than a single entry keeps the cost of
// eviction amortized rather than paid on every insert once the store is full.
const intervalEvictionFraction = 4

// intervalEntry is one directed channel's belief plus the bookkeeping the store
// needs to bound itself.
type intervalEntry struct {
	LiquidityInterval

	// seq is the value of the store's counter when this entry was last
	// written, which is what eviction orders on.
	seq uint64
}

// PersistedInterval is one belief as it is written to and read from durable
// storage.
type PersistedInterval struct {
	// Key identifies the directed channel the belief is about.
	Key IntervalKey

	// Interval is the belief itself.
	Interval LiquidityInterval
}

// IntervalPersister is the durable backing of an IntervalStore. It is an
// interface rather than a concrete store so that the routing package does not
// have to care which database is underneath, and so that a node with no SQL
// backend configured simply runs without one.
type IntervalPersister interface {
	// FetchIntervals returns at most limit of the most recently written
	// beliefs.
	FetchIntervals(ctx context.Context, limit int) ([]PersistedInterval,
		error)

	// StoreIntervals writes the given beliefs, replacing any already held
	// for the same directed channels.
	StoreIntervals(ctx context.Context, intervals []PersistedInterval) error

	// PruneIntervals drops all but the given number of most recently
	// written beliefs.
	PruneIntervals(ctx context.Context, keep int) error

	// PurgeIntervals drops every stored belief.
	PurgeIntervals(ctx context.Context) error
}

// IntervalStore holds the router's belief about the liquidity of every directed
// channel it has observed. It plays the role mission control plays for the
// stock router, with two differences that matter. It records amount intervals
// rather than penalties, and it never forgets anything on a timer: a bound
// moves when new evidence arrives, not when a half life elapses.
//
// The store lives for as long as the node does and is shared by every payment,
// which is what makes the beliefs one payment gathers available to the next.
// With a persister attached it also outlives the process, in which case the
// beliefs it reads back are marked restored, since a bound written down before
// a restart describes a network that has had every chance to move on.
type IntervalStore struct {
	started atomic.Bool
	stopped atomic.Bool

	mu sync.Mutex

	// entries holds one belief per directed channel.
	entries map[IntervalKey]*intervalEntry

	// maxEntries bounds the size of the store.
	maxEntries int

	// seq is a monotonic counter used to order entries for eviction.
	seq uint64

	// persister is the durable backing, or nil when the store is memory
	// only. Everything below it is unused in that case.
	persister IntervalPersister

	// flushInterval is how often accumulated changes are written down.
	flushInterval time.Duration

	// dirty holds the keys written since the last flush.
	dirty map[IntervalKey]struct{}

	// held tracks the amounts this node currently has committed on
	// directed channels through HTLCs it has sent and not yet seen
	// resolved. It is summed across every payment in flight, so that one
	// payment prices a corridor knowing what another payment is already
	// holding on it.
	//
	// This is not part of what the store believes about the network and is
	// never persisted. It records what we are doing to the network right
	// now, which is knowledge that expires the moment the HTLC does.
	held map[IntervalKey]lnwire.MilliSatoshi

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewIntervalStore builds an empty store bounded at the given number of
// directed channels. A non-positive bound selects the default. The store is
// memory only until a persister is attached.
func NewIntervalStore(maxEntries int) *IntervalStore {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxIntervalHistory
	}

	return &IntervalStore{
		entries:    make(map[IntervalKey]*intervalEntry),
		maxEntries: maxEntries,
		held:       make(map[IntervalKey]lnwire.MilliSatoshi),
		quit:       make(chan struct{}),
	}
}

// UsePersistence attaches durable storage to the store. It must be called
// before Start.
func (s *IntervalStore) UsePersistence(persister IntervalPersister,
	flushInterval time.Duration) {

	s.mu.Lock()
	defer s.mu.Unlock()

	if flushInterval <= 0 {
		flushInterval = DefaultIntervalFlushInterval
	}

	s.persister = persister
	s.flushInterval = flushInterval
	s.dirty = make(map[IntervalKey]struct{})
}

// Start loads whatever beliefs were written down before this process began and
// starts the goroutine that writes new ones. It is a no-op on a store with no
// persister, which is what a node running without a SQL backend has.
func (s *IntervalStore) Start(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return nil
	}

	s.mu.Lock()
	persister := s.persister
	limit := s.maxEntries
	s.mu.Unlock()

	if persister == nil {
		return nil
	}

	stored, err := persister.FetchIntervals(ctx, limit)
	if err != nil {
		return fmt.Errorf("unable to load liquidity intervals: %w", err)
	}

	for _, entry := range stored {
		s.Restore(entry.Key, entry.Interval)
	}

	// Reading beliefs in is not a reason to write them straight back out.
	s.mu.Lock()
	clear(s.dirty)
	s.mu.Unlock()

	log.Infof("Loaded %d liquidity interval beliefs", len(stored))

	s.wg.Add(1)
	go s.flusher()

	return nil
}

// Stop writes down anything still pending and stops the flush goroutine.
func (s *IntervalStore) Stop() error {
	if !s.started.Load() || !s.stopped.CompareAndSwap(false, true) {
		return nil
	}

	close(s.quit)
	s.wg.Wait()

	// A shutdown is the one moment we know there will be no further
	// observations, so it is worth paying for a last write.
	return s.flush(context.Background())
}

// flusher writes accumulated changes down on a ticker.
//
// NOTE: this must be run as a goroutine.
func (s *IntervalStore) flusher() {
	defer s.wg.Done()

	s.mu.Lock()
	interval := s.flushInterval
	s.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.flush(context.Background()); err != nil {
				log.Errorf("Unable to flush liquidity "+
					"intervals: %v", err)
			}

		case <-s.quit:
			return
		}
	}
}

// flush writes every belief changed since the last call.
//
// A ticker rather than a write on every observation is deliberate. One payment
// attempt writes both directions of every hop it touched, so a write through
// store would put a handful of database round trips on the path between an
// HTLC failing and the next route being chosen, which is the one path in the
// router that a user waits on. Nothing here needs to survive a crash to stay
// correct either: a belief that never reached disk is a belief the router
// rediscovers on its next attempt, at the cost of that attempt. Mission control
// batches its own writes for the same reasons.
func (s *IntervalStore) flush(ctx context.Context) error {
	s.mu.Lock()

	if s.persister == nil || len(s.dirty) == 0 {
		s.mu.Unlock()

		return nil
	}

	pending := make([]PersistedInterval, 0, len(s.dirty))
	for key := range s.dirty {
		entry, ok := s.entries[key]
		if !ok {
			continue
		}

		pending = append(pending, PersistedInterval{
			Key:      key,
			Interval: entry.LiquidityInterval,
		})
	}

	// Clear the dirty set before releasing the lock. An observation that
	// lands during the write below marks its key again, so the worst case
	// is that we write it twice rather than lose it.
	clear(s.dirty)

	persister := s.persister
	limit := s.maxEntries
	s.mu.Unlock()

	if err := persister.StoreIntervals(ctx, pending); err != nil {
		return err
	}

	return persister.PruneIntervals(ctx, limit)
}

// Get returns the belief held for the given directed channel, normalized
// against the given capacity. A channel that has never been observed returns
// the zero interval, which the probability model reads as "no evidence".
func (s *IntervalStore) Get(key IntervalKey,
	capacity lnwire.MilliSatoshi) LiquidityInterval {

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[key]
	if !ok {
		return LiquidityInterval{}
	}

	interval := entry.LiquidityInterval
	interval.normalize(capacity)

	return interval
}

// Probability returns the success probability of forwarding the given amount
// over the given directed channel.
func (s *IntervalStore) Probability(key IntervalKey,
	amt, capacity lnwire.MilliSatoshi) float64 {

	interval := s.Get(key, capacity)

	return interval.Probability(amt, capacity)
}

// RecordProbe records that the given directed channel forwarded the given
// amount, which we learn whenever a failure is reported by a node further along
// the route than this hop.
func (s *IntervalStore) RecordProbe(key IntervalKey,
	amt, capacity lnwire.MilliSatoshi) {

	s.update(key, amt, capacity, func(forward, reverse *LiquidityInterval,
		amt lnwire.MilliSatoshi) {

		forward.recordProbe(reverse, amt, capacity)
	})
}

// RecordFailure records that the given directed channel could not carry the
// given amount.
func (s *IntervalStore) RecordFailure(key IntervalKey,
	amt, capacity lnwire.MilliSatoshi) {

	s.update(key, amt, capacity, func(forward, reverse *LiquidityInterval,
		amt lnwire.MilliSatoshi) {

		forward.recordFailure(reverse, amt, capacity)
	})
}

// RecordSettlement records that the given directed channel actually moved the
// given amount, which shifts both directions of the interval rather than merely
// narrowing them.
func (s *IntervalStore) RecordSettlement(key IntervalKey,
	amt, capacity lnwire.MilliSatoshi) {

	s.update(key, amt, capacity, func(forward, reverse *LiquidityInterval,
		amt lnwire.MilliSatoshi) {

		forward.recordSettlement(reverse, amt, capacity)
	})
}

// update applies an observation to both directions of a channel under the
// store's lock. Observations of a zero amount, or of a channel whose capacity
// we do not know, carry no information the model can use and are dropped. The
// sanitized amount is handed to the callback, which must use it in place of the
// amount its caller was given.
func (s *IntervalStore) update(key IntervalKey, amt,
	capacity lnwire.MilliSatoshi,
	apply func(forward, reverse *LiquidityInterval,
		amt lnwire.MilliSatoshi)) {

	if amt == 0 || capacity == 0 {
		return
	}

	// An amount larger than the capacity cannot be a real observation about
	// this channel. It can still reach us, because the capacity we path
	// find against is a synthetic one when a peer has several channels to
	// the same node, so clamp rather than reject.
	if amt > capacity {
		amt = capacity
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	forward := s.entryLocked(key)
	reverse := s.entryLocked(key.Reverse())

	apply(&forward.LiquidityInterval, &reverse.LiquidityInterval, amt)

	// Every observation writes both directions, so both need writing down.
	s.markDirtyLocked(key)
	s.markDirtyLocked(key.Reverse())

	s.evictLocked()
}

// entryLocked returns the entry for a key, creating it if needed, and stamps it
// as the most recently written.
//
// NOTE: the store's mutex must be held.
func (s *IntervalStore) entryLocked(key IntervalKey) *intervalEntry {
	entry, ok := s.entries[key]
	if !ok {
		entry = &intervalEntry{}
		s.entries[key] = entry
	}

	s.seq++
	entry.seq = s.seq

	return entry
}

// markDirtyLocked records that a key needs writing down. It is a no-op on a
// store with no persister, which is what keeps the memory only path free of
// any bookkeeping it would never read.
//
// NOTE: the store's mutex must be held.
func (s *IntervalStore) markDirtyLocked(key IntervalKey) {
	if s.dirty == nil {
		return
	}

	s.dirty[key] = struct{}{}
}

// evictLocked drops the least recently written entries when the store has grown
// past its bound.
//
// NOTE: the store's mutex must be held.
func (s *IntervalStore) evictLocked() {
	if len(s.entries) <= s.maxEntries {
		return
	}

	keys := make([]IntervalKey, 0, len(s.entries))
	for key := range s.entries {
		keys = append(keys, key)
	}

	sort.Slice(keys, func(i, j int) bool {
		return s.entries[keys[i]].seq < s.entries[keys[j]].seq
	})

	drop := len(s.entries) / intervalEvictionFraction
	for _, key := range keys[:drop] {
		delete(s.entries, key)

		if s.dirty != nil {
			delete(s.dirty, key)
		}
	}
}

// Restore seeds the store with a belief that was held before this process
// started. The interval is taken as it was written down, but it is marked as
// restored, which stops the probability model from treating either of its
// bounds as a certainty until a fresh observation replaces it.
//
// An entry that has already been observed in this process is left alone, since
// what we have watched ourselves beats anything we read back.
func (s *IntervalStore) Restore(key IntervalKey,
	interval LiquidityInterval) {

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.entries[key]; ok && existing.Known &&
		!existing.Restored {

		return
	}

	entry := s.entryLocked(key)
	entry.LiquidityInterval = interval
	entry.Known = true
	entry.markRestored()

	s.evictLocked()

	// A restored belief is already on disk, and the halved confidence it
	// now carries is a reading of it rather than a new observation, so
	// there is nothing here worth writing back.
}

// ForEach hands every belief the store holds to the callback, which is how a
// persistence layer reads out what needs writing down.
func (s *IntervalStore) ForEach(cb func(IntervalKey, LiquidityInterval)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, entry := range s.entries {
		cb(key, entry.LiquidityInterval)
	}
}

// Held returns the amount this node currently has committed on the given
// directed channel through HTLCs it has sent and not yet seen resolved.
func (s *IntervalStore) Held(key IntervalKey) lnwire.MilliSatoshi {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.held[key]
}

// Hold records that we have committed the given amounts, one per directed
// channel of a route we are about to send over.
func (s *IntervalStore) Hold(amounts map[IntervalKey]lnwire.MilliSatoshi) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, amt := range amounts {
		s.held[key] += amt
	}
}

// Release gives back amounts recorded by an earlier call to Hold, which the
// caller makes once the HTLC that committed them has resolved.
//
// An amount larger than what is held would mean the caller has released
// something twice. That must not happen, but if it does we would rather forget
// a hold than carry a phantom one, because a hold nothing is behind depresses a
// channel for every payment and nothing but another release can lift it.
func (s *IntervalStore) Release(amounts map[IntervalKey]lnwire.MilliSatoshi) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, amt := range amounts {
		current, ok := s.held[key]
		if !ok {
			continue
		}

		if current <= amt {
			delete(s.held, key)

			continue
		}

		s.held[key] = current - amt
	}
}

// HeldLen returns the number of directed channels currently carrying a hold.
// It exists so that a test can assert that nothing leaked.
func (s *IntervalStore) HeldLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.held)
}

// Clear forgets everything the store has learned. It exists so that an operator
// can reset the router's beliefs the way mission control's history can be
// reset.
func (s *IntervalStore) Clear(ctx context.Context) error {
	s.mu.Lock()

	s.entries = make(map[IntervalKey]*intervalEntry)
	s.held = make(map[IntervalKey]lnwire.MilliSatoshi)
	s.seq = 0

	persister := s.persister
	if s.dirty != nil {
		clear(s.dirty)
	}
	s.mu.Unlock()

	if persister == nil {
		return nil
	}

	return persister.PurgeIntervals(ctx)
}

// Len returns the number of directed channels the store currently holds a
// belief for.
func (s *IntervalStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.entries)
}

package htlcswitch

import (
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
)

// defaultSequenceBatchSize specifies the window of sequence numbers that are
// allocated for each write to disk made by the sequencer.
const defaultSequenceBatchSize = 1000

// Sequencer emits sequence numbers for locally initiated HTLCs. These are
// only used internally for tracking pending payments, however they must be
// unique in order to avoid circuit key collision in the circuit map.
type Sequencer interface {
	// NextID returns a unique sequence number for each invocation.
	NextID() (uint64, error)
}

var (
	// nextPaymentIDKey identifies the bucket that will keep track of the
	// persistent sequence numbers for payments.
	nextPaymentIDKey = []byte("next-payment-id-key")

	// ErrSequencerCorrupted signals that the persistence engine was not
	// initialized, or has been corrupted since startup.
	ErrSequencerCorrupted = errors.New(
		"sequencer database has been corrupted")
)

// persistentSequencer is a concrete implementation of IDGenerator, that uses
// channeldb to allocate sequence numbers.
type persistentSequencer struct {
	db *channeldb.DB

	mu sync.Mutex

	nextID    uint64
	horizonID uint64
}

// NewPersistentSequencer initializes a new sequencer using a channeldb backend.
func NewPersistentSequencer(db *channeldb.DB) (Sequencer, error) {
	g := &persistentSequencer{
		db: db,
	}

	// Ensure the database bucket is created before any updates are
	// performed.
	if err := g.initDB(); err != nil {
		return nil, err
	}

	return g, nil
}

// NextID returns a unique sequence number for every invocation, persisting the
// assignment to avoid reuse.
func (s *persistentSequencer) NextID() (uint64, error) {

	// nextID will be the unique sequence number returned if no errors are
	// encountered.
	var nextID uint64

	// If our sequence batch has not been exhausted, we can allocate the
	// next identifier in the range.
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.nextID < s.horizonID {
		nextID = s.nextID
		s.nextID++

		return nextID, nil
	}

	// Otherwise, our sequence batch has been exhausted. We use the last
	// known sequence number on disk to mark the beginning of the next
	// sequence batch, and allocate defaultSequenceBatchSize (1000) at a
	// time.
	//
	// NOTE: This also will happen on the first invocation after startup,
	// i.e. when nextID and horizonID are both 0. The next sequence batch to be
	// allocated will start from the last known tip on disk, which is fine
	// as we only require uniqueness of the allocated numbers.
	var nextHorizonID uint64
	if err := kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		nextIDBkt := tx.ReadWriteBucket(nextPaymentIDKey)
		if nextIDBkt == nil {
			return ErrSequencerCorrupted
		}

		nextID = nextIDBkt.Sequence()
		nextHorizonID = nextID + defaultSequenceBatchSize

		// Cannot fail when used in Update.
		nextIDBkt.SetSequence(nextHorizonID)

		return nil
	}, func() {
		nextHorizonID = 0
	}); err != nil {
		return 0, err
	}

	// Never assign index zero, to avoid collisions with the EmptyKeystone.
	if nextID == 0 {
		nextID++
	}

	// If our batch sequence allocation succeed, update our in-memory values
	// so we can continue to allocate sequence numbers without hitting disk.
	// The nextID is incremented by one in memory so the in can be used
	// issued directly on the next invocation.
	s.nextID = nextID + 1
	s.horizonID = nextHorizonID

	return nextID, nil
}

// initDB populates the bucket used to generate payment sequence numbers.
func (s *persistentSequencer) initDB() error {
	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(nextPaymentIDKey)
		return err
	}, func() {})
}

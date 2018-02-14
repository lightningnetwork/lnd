package htlcswitch

import (
	"github.com/boltdb/bolt"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
)

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
func (g *persistentSequencer) NextID() (uint64, error) {
	var nextPaymentID uint64
	if err := g.db.Update(func(tx *bolt.Tx) error {
		nextIDBkt := tx.Bucket(nextPaymentIDKey)
		if nextIDBkt == nil {
			return ErrSequencerCorrupted
		}

		// Cannot fail when used in Update.
		nextPaymentID, _ = nextIDBkt.NextSequence()

		return nil
	}); err != nil {
		return 0, err
	}

	return nextPaymentID, nil
}

// initDB populates the bucket used to generate payment sequence numbers.
func (g *persistentSequencer) initDB() error {
	return g.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(nextPaymentIDKey)
		return err
	})
}

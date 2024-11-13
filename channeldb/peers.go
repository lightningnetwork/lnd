package channeldb

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// peersBucket is the name of a top level bucket in which we store
	// information about our peers. Information for different peers is
	// stored in buckets keyed by their public key.
	//
	//
	// peers-bucket
	//      |
	//      |-- <peer-pubkey>
	//      |        |--flap-count-key: <ts><flap count>
	//      |
	//      |-- <peer-pubkey>
	//      |        |--flap-count-key: <ts><flap count>
	peersBucket = []byte("peers-bucket")

	// flapCountKey is a key used in the peer pubkey sub-bucket that stores
	// the timestamp of a peer's last flap count and its all time flap
	// count.
	flapCountKey = []byte("flap-count")
)

var (
	// ErrNoPeerBucket is returned when we try to read entries for a peer
	// that is not tracked.
	ErrNoPeerBucket = errors.New("peer bucket not found")
)

// FlapCount contains information about a peer's flap count.
type FlapCount struct {
	// Count provides the total flap count for a peer.
	Count uint32

	// LastFlap is the timestamp of the last flap recorded for a peer.
	LastFlap time.Time
}

// WriteFlapCounts writes the flap count for a set of peers to disk, creating a
// bucket for the peer's pubkey if necessary. Note that this function overwrites
// the current value.
func (d *DB) WriteFlapCounts(flapCounts map[route.Vertex]*FlapCount) error {
	// Exit early if there are no updates.
	if len(flapCounts) == 0 {
		log.Debugf("No flap counts to write, skipped db update")
		return nil
	}

	return kvdb.Update(d, func(tx kvdb.RwTx) error {
		// Run through our set of flap counts and record them for
		// each peer, creating a bucket for the peer pubkey if required.
		for peer, flapCount := range flapCounts {
			peers := tx.ReadWriteBucket(peersBucket)

			peerBucket, err := peers.CreateBucketIfNotExists(
				peer[:],
			)
			if err != nil {
				return err
			}

			var b bytes.Buffer
			err = serializeTime(&b, flapCount.LastFlap)
			if err != nil {
				return err
			}

			if err = WriteElement(&b, flapCount.Count); err != nil {
				return err
			}

			err = peerBucket.Put(flapCountKey, b.Bytes())
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// ReadFlapCount attempts to read the flap count for a peer, failing if the
// peer is not found or we do not have flap count stored.
func (d *DB) ReadFlapCount(pubkey route.Vertex) (*FlapCount, error) {
	var flapCount FlapCount

	if err := kvdb.View(d, func(tx kvdb.RTx) error {
		peers := tx.ReadBucket(peersBucket)

		peerBucket := peers.NestedReadBucket(pubkey[:])
		if peerBucket == nil {
			return ErrNoPeerBucket
		}

		flapBytes := peerBucket.Get(flapCountKey)
		if flapBytes == nil {
			return fmt.Errorf("flap count not recorded for: %v",
				pubkey)
		}

		var (
			err error
			r   = bytes.NewReader(flapBytes)
		)

		flapCount.LastFlap, err = deserializeTime(r)
		if err != nil {
			return err
		}

		return ReadElements(r, &flapCount.Count)
	}, func() {
		flapCount = FlapCount{}
	}); err != nil {
		return nil, err
	}

	return &flapCount, nil
}

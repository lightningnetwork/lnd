package main

import (
	"bytes"
	"errors"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

// NurseryStore abstracts the persistent storage layer for the utxo nursery.
// Concretely, it stores commitment and htlc outputs that until any time-bounded
// constraints have fully matured. The store exposes methods for enumerating
// its contents, and persisting state transitions detected by the utxo nursery.
type NurseryStore interface {

	// Incubate registers a commitment output and a slice of htlc outputs to
	// be swept back into the user's wallet. The event is persisted to disk,
	// such that the nursery can resume the incubation process after a
	// potential crash.
	Incubate(*kidOutput, []babyOutput) error

	// CribToKinder atomically moves a babyOutput in the crib bucket to the
	// kindergarten bucket. The now mature kidOutput contained in the
	// babyOutput will be stored as it waits out the kidOutput's CSV delay.
	CribToKinder(*babyOutput) error

	// PreschoolToKinder atomically moves a kidOutput from the preschool
	// bucket to the kindergarten bucket. This transition should be executed
	// after receiving confirmation of the preschool output's commitment
	// transaction.
	PreschoolToKinder(*kidOutput) error

	// GraduateKinder accepts a slice of kidOutputs from the kindergarten
	// bucket, and removes their corresponding entries from the height and
	// channel indexes. If this method detects that all outputs for a
	// particular contract have been incubated, it returns the channel
	// points that are ready to be marked as fully closed.
	// TODO: make this handle one output at a time?
	GraduateKinder([]kidOutput) error

	// FinalizeHeight accepts a block height as a parameter and purges its
	// persistent state for all outputs at that height. During a restart,
	// the utxo nursery will begin it's recovery procedure from the next
	// height that has yet to be finalized. This block height should lag
	// beyond the best height for this chain as a measure of reorg
	// protection.
	FinalizeHeight(height uint32) error

	// LastFinalizedHeight returns the last block height for which the
	// nursery store has purged all persistent state.
	LastFinalizedHeight() (uint32, error)

	// FetchClass returns a list of babyOutputs in the crib bucket whose
	// CLTV delay expires at the provided block height.
	FetchClass(height uint32) ([]kidOutput, []babyOutput, error)

	// FetchPreschools returns a list of all outputs currently stored in the
	// preschool bucket.
	FetchPreschools() ([]kidOutput, error)

	// ForChanOutputs iterates over all outputs being incubated for a
	// particular channel point. This method accepts a callback that allows
	// the caller to process each key-value pair. The key will be a prefixed
	// outpoint, and the value will be the serialized bytes for an output,
	// whose type should be inferred from the key's prefix.
	ForChanOutputs(*wire.OutPoint, func([]byte, []byte) error) error

	// IsMatureChannel determines the whether or not all of the outputs in a
	// particular channel bucket have been marked as graduated.
	IsMatureChannel(*wire.OutPoint) (bool, error)

	// RemoveChannel channel erases all entries from the channel bucket for
	// the provided channel point.
	RemoveChannel(*wire.OutPoint) error
}

// prefixChainKey creates the root level keys for the nursery store. The keys
// are comprised of a nursery-specific prefix and the intended chain hash that
// this nursery store will be used for. This allows multiple nursery stores to
// isolate their state when operating on multiple chains or forks.
func prefixChainKey(sysPrefix []byte, hash *chainhash.Hash) ([]byte, error) {
	// Create a buffer to which we will write the system prefix, e.g.
	// "utxn", followed by the provided chain hash.
	var pfxChainBuffer bytes.Buffer
	if _, err := pfxChainBuffer.Write(sysPrefix); err != nil {
		return nil, err
	}

	if _, err := pfxChainBuffer.Write(hash[:]); err != nil {
		return nil, err
	}

	return pfxChainBuffer.Bytes(), nil
}

// prefixOutputKey creates a serialized key that prefixes the serialized
// outpoint with the provided state prefix. The returned bytes will be of the
// form <prefix><outpoint>.
func prefixOutputKey(statePrefix []byte,
	outpoint *wire.OutPoint) ([]byte, error) {

	// Create a buffer to which we will first write the state prefix,
	// followed by the outpoint.
	var pfxOutputBuffer bytes.Buffer
	if _, err := pfxOutputBuffer.Write(statePrefix); err != nil {
		return nil, err
	}

	err := writeOutpoint(&pfxOutputBuffer, outpoint)
	if err != nil {
		return nil, err
	}

	return pfxOutputBuffer.Bytes(), nil
}

var (
	// utxnChainPrefix is used to prefix a particular chain hash and create
	// the root-level, chain-segmented bucket for each nursery store.
	utxnChainPrefix = []byte("utxn")

	// lastFinalizedHeightKey is a static key used to locate nursery store's
	// last finalized height.
	lastFinalizedHeightKey = []byte("last-finalized-height")

	// channelIndexKey is a static key used to lookup the bucket containing
	// all of the nursery's active channels.
	channelIndexKey = []byte("channel-index")

	// channelIndexKey is a static key used to retrieve a directory
	// containing all heights for which the nursery will need to take
	// action.
	heightIndexKey = []byte("height-index")

	// cribPrefix is the state prefix given to htlc outputs waiting for
	// their first-stage, absolute locktime to elapse.
	cribPrefix = []byte("crib")

	// psclPrefix is the state prefix given to commitment outputs awaiting
	// the // confirmation of the commitment transaction, as this solidifies
	// the absolute height at which they can be spent.
	psclPrefix = []byte("pscl")

	// kndrPrefix is the state prefix given to all CSV delayed outputs,
	// either from the commitment transaction, or a stage-one htlc
	// transaction, whose maturity height has solidified. Outputs marked in
	// this state are in their final stage of incubation withn the nursery,
	// and will be swept into the wallet after waiting out the relative
	// timelock.
	kndrPrefix = []byte("kndr")

	// gradPrefix is the state prefix given to all outputs that have been
	// completely incubated. Once all outputs have been marked as graduated,
	// this serves as a persistent marker that the nursery should mark the
	// channel fully closed in the channeldb.
	gradPrefix = []byte("grad")
)

//	              Overview of Nursery Store Storage Hierarchy
//
//   CHAIN SEGMENTATION
//
//   The root directory of a nursery store is bucketed by the chain hash and
//   the 'utxn' prefix. This allows multiple utxo nurseries for distinct chains
//   to simultaneously use the same channel.DB instance. This is critical for
//   providing replay protection and more to isolate chain-specific data in the
//   multichain setting.
//
//   utxn<chain-hash>/
//   |
//   |   LAST FINALIZED HEIGHT
//   |
//   |   Each nursery store tracks a "last finalized height", which records the
//   |   most recent block height for which the nursery store has purged all
//   |   state. This value lags behind the best block height for reorg safety,
//   |   and serves as a starting height for rescans after a restart.
//   |
//   ├── last-finalized-height-key: <last-finalized-height>
//   |
//   |   CHANNEL INDEX
//   |
//   |   The channel index contains a directory for each channel that has a
//   |   non-zero number of outputs being tracked by the nursery store.
//   |   Inside each channel directory are files containing serialized spendable
//   |   outputs that are awaiting some state transition. The name of each file
//   |   contains the outpoint of the spendable output in the file, and is
//   |   prefixed with 4-byte state prefix, indicating whether the spendable
//   |   output is a crib, preschool, or kindergarten output. The nursery store
//   |   supports the ability to enumerate all outputs for a particular channel,
//   |   which is useful in constructing nursery reports.
//   |
//   ├── channel-index-key/
//   │   ├── <chan-point-1>/                      <- CHANNEL BUCKET
//   |   |   ├── <state-prefix><outpoint-1>: <spendable-output-1>
//   |   |   └── <state-prefix><outpoint-2>: <spendable-output-2>
//   │   ├── <chan-point-2>/
//   |   |   └── <state-prefix><outpoint-3>: <spendable-output-3>
//   │   └── <chan-point-3>/
//   |       ├── <state-prefix><outpoint-4>: <spendable-output-4>
//   |       └── <state-prefix><outpoint-5>: <spendable-output-5>
//   |
//   |   HEIGHT INDEX
//   |
//   |   The height index contains a directory for each height at which the
//   |   nursery still has uncompleted actions. If an output is a crib or
//   |   kindergarten output, it will have an associated entry in the height
//   |   index. Inside a particular height directory, the structure is similar
//   |   to that of the channel index, containing multiple channel directories,
//   |   each of which contains subdirectories named with a prefixed outpoint
//   | 	 belonging to the channel. Enumerating these combinations yields a
//   |   relative file path:
//   |     e.g. <chan-point-3>/<prefix><outpoint-2>/
//   |   that can be queried in the channel index to retrieve the serialized
//   |   output.
//   |
//   └── height-index-key/
//       ├── <height-1>/                             <- HEIGHT BUCKET
//       |   └── <chan-point-3>/                     <- HEIGHT-CHANNEL BUCKET
//       |   |    ├── <state-prefix><outpoint-4>: "" <- PREFIXED OUTPOINT
//       |   |    └── <state-prefix><outpoint-5>: ""
//       |   └── <chan-point-2>/
//       |        └── <state-prefix><outpoint-3>: ""
//       └── <height-2>/
//           └── <chan-point-1>/
//                └── <state-prefix><outpoint-1>: ""
//                └── <state-prefix><outpoint-2>: ""

// nurseryStore is a concrete instantiation of a NurseryStore that is backed by
// a channeldb.DB instance.
type nurseryStore struct {
	chainHash chainhash.Hash
	db        *channeldb.DB

	pfxChainKey []byte
}

// newNurseryStore accepts a chain hash and a channeldb.DB instance, returning
// an instance of nurseryStore who's database is properly segmented for the
// given chain.
func newNurseryStore(chainHash *chainhash.Hash,
	db *channeldb.DB) (*nurseryStore, error) {

	// Prefix the provided chain hash with "utxn" to create the key for the
	// nursery store's root bucket, ensuring each one has proper chain
	// segmentation.
	pfxChainKey, err := prefixChainKey(utxnChainPrefix, chainHash)
	if err != nil {
		return nil, err
	}

	return &nurseryStore{
		chainHash:   *chainHash,
		db:          db,
		pfxChainKey: pfxChainKey,
	}, nil
}

// Incubate initiates the incubation process for the CSV-delayed commitment
// output and any number of CLTV-delayed htlc outputs.
func (ns *nurseryStore) Incubate(kid *kidOutput, babies []babyOutput) error {
	return ns.db.Update(func(tx *bolt.Tx) error {
		// Store commitment output in preschool bucket if not nil.
		if kid != nil {
			if err := ns.enterPreschool(tx, kid); err != nil {
				return err
			}
		}

		// Add all htlc outputs to the crib bucket.
		for _, baby := range babies {
			if err := ns.enterCrib(tx, &baby); err != nil {
				return err
			}
		}

		return nil
	})
}

// CribToKinder atomically moves a babyOutput in the crib bucket to the
// kindergarten bucket. The now mature kidOutput contained in the babyOutput
// will be stored as it waits out the kidOutput's CSV delay.
func (ns *nurseryStore) CribToKinder(bby *babyOutput) error {
	return ns.db.Update(func(tx *bolt.Tx) error {

		// First, retrieve or create the channel bucket corresponding to
		// the baby output's origin channel point.
		chanPoint := bby.OriginChanPoint()
		chanBucket, err := ns.createChannelBucket(tx, chanPoint)
		if err != nil {
			return err
		}

		// The babyOutput should currently be stored in the crib bucket.
		// So, we create a key that prefixes the babyOutput's outpoint
		// with the crib prefix, allowing us to reference it in the
		// store.
		pfxOutputKey, err := prefixOutputKey(cribPrefix, bby.OutPoint())
		if err != nil {
			return err
		}

		// Since the babyOutput is being moved to the kindergarten
		// bucket, we remove the entry from the channel bucket under the
		// crib-prefixed outpoint key.
		if err := chanBucket.Delete(pfxOutputKey); err != nil {
			return err
		}

		// Next, retrieve the height-channel bucket located in the
		// height bucket corresponding to the baby output's CLTV expiry
		// height. This bucket should always exist, but if it doesn't
		// then we have nothing to clean up.
		hghtChanBucketCltv := ns.getHeightChanBucket(tx, bby.expiry,
			chanPoint)
		if hghtChanBucketCltv != nil {
			// We successfully located  an existing height chan
			// bucket at this babyOutput's expiry height, proceed by
			// removing it from the index.
			err := hghtChanBucketCltv.Delete(pfxOutputKey)
			if err != nil {
				return err
			}

			// Since we removed a crib output from the height index,
			// we opportunistically prune the height bucket
			// corresponding to the babyOutput's CLTV delay. This
			// allows us to clean up any persistent state as outputs
			// are progressed through the incubation process.
			pruned, err := ns.pruneHeight(tx, bby.expiry)
			if err != nil && err != ErrBucketNotEmpty {
				return err
			} else if err == nil && pruned {
				utxnLog.Infof("Height bucket %d pruned", bby.expiry)
			}
		}

		// Since we are moving this output from the crib bucket to the
		// kindergarten bucket, we overwrite the existing prefix of this
		// key with the kindergarten prefix.
		copy(pfxOutputKey, kndrPrefix)

		// Now, serialize babyOutput's encapsulated kidOutput such that
		// it can be written to the channel bucket under the new
		// kindergarten-prefixed key.
		var kidBuffer bytes.Buffer
		if err := bby.kidOutput.Encode(&kidBuffer); err != nil {
			return err
		}
		kidBytes := kidBuffer.Bytes()

		// Persist the serialized kidOutput under the
		// kindergarten-prefixed outpoint key.
		if err := chanBucket.Put(pfxOutputKey, kidBytes); err != nil {
			return err
		}

		// Now, compute the height at which this kidOutput's CSV delay
		// will expire.  This is done by adding the required delay to
		// the block height at which the output was confirmed.
		maturityHeight := bby.ConfHeight() + bby.BlocksToMaturity()

		// Retrive or create a height-channel bucket corresponding to
		// the kidOutput's maturity height.
		hghtChanBucketCsv, err := ns.createHeightChanBucket(tx,
			maturityHeight, chanPoint)
		if err != nil {
			return err
		}

		// Register the kindergarten output's prefixed output key in the
		// height-channel bucket corresponding to its maturity height.
		// This informs the utxo nursery that it should attempt to spend
		// this output when the blockchain reaches the maturity height.
		return hghtChanBucketCsv.Put(pfxOutputKey, []byte{})
	})
}

// PreschoolToKinder atomically moves a kidOutput from the preschool bucket to
// the kindergarten bucket. This transition should be executed after receiving
// confirmation of the preschool output's commitment transaction.
func (ns *nurseryStore) PreschoolToKinder(kid *kidOutput) error {
	return ns.db.Update(func(tx *bolt.Tx) error {

		// Create or retrieve the channel bucket corresponding to the
		// kid output's origin channel point.
		chanPoint := kid.OriginChanPoint()
		chanBucket, err := ns.createChannelBucket(tx, chanPoint)
		if err != nil {
			return err
		}

		// First, we will attempt to remove the existing serialized
		// output from the channel bucket, where the kid's outpoint will
		// be prefixed by a preschool prefix.

		// Generate the key of existing serialized kid output by
		// prefixing its outpoint with the preschool prefix...
		pfxOutputKey, err := prefixOutputKey(psclPrefix, kid.OutPoint())
		if err != nil {
			return err
		}

		// And remove the old serialized output from the database.
		if err := chanBucket.Delete(pfxOutputKey); err != nil {
			return err
		}

		// Next, we will write the provided kid outpoint to the channel
		// bucket, using a key prefixed by the kindergarten prefix.

		// Convert the preschool prefix key into a kindergarten key for
		// the same outpoint.
		copy(pfxOutputKey, kndrPrefix)

		// Reserialize the kid here to capture any differences in the
		// new and old kid output, such as the confirmation height.
		var kidBuffer bytes.Buffer
		if err := kid.Encode(&kidBuffer); err != nil {
			return err
		}
		kidBytes := kidBuffer.Bytes()

		// And store the kid output in its channel bucket using the
		// kindergarten prefixed key.
		if err := chanBucket.Put(pfxOutputKey, kidBytes); err != nil {
			return err
		}

		// Since the CSV delay on the kid output has now begun ticking,
		// we must insert a record of in the height index to remind us
		// to revisit this output once it has fully matured.

		// Compute the maturity height, by adding the output's CSV delay
		// to its confirmation height.
		maturityHeight := kid.ConfHeight() + kid.BlocksToMaturity()

		// Create or retrieve the height-channel bucket for this
		// channel. This method will first create a height bucket for
		// the given maturity height if none exists.
		hghtChanBucket, err := ns.createHeightChanBucket(tx,
			maturityHeight, chanPoint)
		if err != nil {
			return err
		}

		// Finally, we touch a bucket in the height-channel created
		// above.  The bucket is named using a kindergarten prefixed
		// key, signaling that this CSV delayed output will be ready to
		// broadcast at the maturity height, after a brief period of
		// incubation.
		_, err = hghtChanBucket.CreateBucketIfNotExists(pfxOutputKey)

		return err
	})
}

// GraduateKinder accepts a list of kidOutputs in the kindergarten bucket,
// removing their corresponding entries from the height and channel indexes.
// If this method detects that all outputs for a particular contract have been
// incubated, it returns the channel points that are ready to be marked as
// fully closed. This method will iterate through the provided kidOutputs and do
// the following:
// 1) Prune the kid height bucket at the kid's confirmation height, if it is
//     empty.
// 2) Prune the channel bucket belonging to the kid's origin channel point, if
//     it is empty.
func (ns *nurseryStore) GraduateKinder(kids []kidOutput) error {

	if err := ns.db.Update(func(tx *bolt.Tx) error {
		for _, kid := range kids {

			confHeight := kid.ConfHeight()

			outpoint := kid.OutPoint()
			chanPoint := kid.OriginChanPoint()

			// Construct the key under which the output is currently
			// stored height and channel indexes.
			pfxOutputKey, err := prefixOutputKey(kndrPrefix, outpoint)
			if err != nil {
				return err
			}

			// Load the height-channel bucket, remove this output,
			// and attempt to prune the bucket if it empty.
			hghtChanBucket := ns.getHeightChanBucket(tx, confHeight, chanPoint)
			if hghtChanBucket != nil {
				if err := hghtChanBucket.Delete(pfxOutputKey); err != nil {
					return err
				}

				// Attempt to prune the height bucket matching the kid
				// output's confirmation height if it contains no active
				// outputs.
				pruned, err := ns.pruneHeight(tx, confHeight)
				if err != nil && err != ErrBucketNotEmpty {
					return err
				} else if err == nil && pruned {
					utxnLog.Infof("Height bucket %d pruned", confHeight)
				}
			}

			chanBucket, err := ns.createChannelBucket(tx, chanPoint)
			if err != nil {
				return err
			}

			// Remove previous output with kindergarten prefix.
			if err := chanBucket.Delete(pfxOutputKey); err != nil {
				return err
			}

			// Convert kindergarten key to graduate key.
			copy(pfxOutputKey, gradPrefix)

			var gradBuffer bytes.Buffer
			if err := kid.Encode(&gradBuffer); err != nil {
				return err
			}

			// Insert serialized output into channel bucket using
			// kindergarten-prefixed key.
			err = chanBucket.Put(pfxOutputKey, gradBuffer.Bytes())
			if err != nil {
				return err
			}

		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

// FinalizeHeight accepts a block height as a parameter and purges its
// persistent state for all outputs at that height. During a restart, the utxo
// nursery will begin it's recovery procedure from the next height that has
// yet to be finalized.
func (ns *nurseryStore) FinalizeHeight(height uint32) error {
	return ns.db.Update(func(tx *bolt.Tx) error {
		if err := ns.deleteHeightBucket(tx, height); err != nil {
			return err
		}

		return ns.putLastFinalizedHeight(tx, height)
	})
}

// FetchClass returns a list of babyOutputs in the crib bucket whose CLTV
// delay expires at the provided block height.
func (ns *nurseryStore) FetchClass(height uint32) ([]kidOutput, []babyOutput, error) {
	// Construct list of all crib and kindergarten outputs that need TLC at
	// the provided block height.
	var kids []kidOutput
	var babies []babyOutput
	if err := ns.db.View(func(tx *bolt.Tx) error {
		if err := ns.forEachHeightPrefix(tx, cribPrefix, height, func(buf []byte) error {

			// We will attempt to deserialize all outputs
			// stored with the crib prefix into babyOutputs,
			// since this is the expected type that would
			// have been serialized previously.
			var baby babyOutput
			babyReader := bytes.NewReader(buf)
			if err := baby.Decode(babyReader); err != nil {
				return err
			}

			// Append the deserialized object to our list of
			// babyOutputs.
			babies = append(babies, baby)

			return nil

		}); err != nil {
			return err
		}

		return ns.forEachHeightPrefix(tx, kndrPrefix, height, func(buf []byte) error {
			// We will attempt to deserialize all outputs stored
			// with the kindergarten prefix into kidOutputs, since
			// this is the expected type that would have been
			// serialized previously.
			var kid kidOutput
			kidReader := bytes.NewReader(buf)
			if err := kid.Decode(kidReader); err != nil {
				return err
			}

			// Append the deserialized object to our list of
			// kidOutputs.
			kids = append(kids, kid)

			return nil

		})

	}); err != nil {
		return nil, nil, err
	}

	return kids, babies, nil
}

// FetchPreschools returns a list of all outputs currently stored in the
// preschool bucket.
func (ns *nurseryStore) FetchPreschools() ([]kidOutput, error) {
	var kids []kidOutput
	if err := ns.db.View(func(tx *bolt.Tx) error {

		// Retrieve the existing chain bucket for this nursery store.
		chainBucket := tx.Bucket(ns.pfxChainKey)
		if chainBucket == nil {
			return nil
		}

		// Load the existing channel index from the chain bucket.
		chanIndex := chainBucket.Bucket(channelIndexKey)
		if chanIndex == nil {
			return nil
		}

		// Construct a list of all channels in the channel index that
		// are currently being tracked by the nursery store.
		var activeChannels [][]byte
		if err := chanIndex.ForEach(func(chanBytes, _ []byte) error {
			activeChannels = append(activeChannels, chanBytes)
			return nil
		}); err != nil {
			return err
		}

		// Iterate over all of the accumulated channels, and do a prefix
		// scan inside of each channel bucket. Each output found that
		// has a preschool prefix will be deserialized into a kidOutput,
		// and added to our list of preschool outputs to return to the
		// caller.
		for _, chanBytes := range activeChannels {
			// Retrieve the channel bucket associated with this
			// channel.
			chanBucket := chanIndex.Bucket(chanBytes)
			if chanBucket == nil {
				continue
			}

			// All of the outputs of interest will start with the
			// "pscl" prefix. So, we will perform a prefix scan of
			// the channel bucket to efficiently enumerate all the
			// desired outputs.
			c := chanBucket.Cursor()

			// Seek and iterate over all outputs starting with the
			// prefix "pscl".
			pfxOutputKey, kidBytes := c.Seek(psclPrefix)
			for bytes.HasPrefix(pfxOutputKey, psclPrefix) {

				// Deserialize each output as a kidOutput, since
				// this should have been the type that was
				// serialized when it was written to disk.
				var psclOutput kidOutput
				psclReader := bytes.NewReader(kidBytes)
				err := psclOutput.Decode(psclReader)
				if err != nil {
					return err
				}

				// Add the deserialized output to our list of
				// preschool outputs.
				kids = append(kids, psclOutput)

				// Advance to the subsequent key-value pair of
				// the prefix scan.
				pfxOutputKey, kidBytes = c.Next()
			}
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return kids, nil
}

// ForChanOutputs iterates over all outputs being incubated for a particular
// channel point. This method accepts a callback that allows the caller to
// process each key-value pair. The key will be a prefixed outpoint, and the
// value will be the serialized bytes for an output, whose type should be
// inferred from the key's prefix.
// NOTE: The callback should be not modify the provided byte slices and is
// preferably non-blocking.
func (ns *nurseryStore) ForChanOutputs(chanPoint *wire.OutPoint,
	callback func([]byte, []byte) error) error {

	return ns.db.View(func(tx *bolt.Tx) error {
		return ns.forChanOutputs(tx, chanPoint, callback)
	})
}

// errImmatureChannel signals that not all outputs in a channel bucket have
// graduated.
var errImmatureChannel = errors.New("channel has non-graduated outputs")

// IsMatureChannel determines the whether or not all of the outputs in a
// particular channel bucket have been marked as graduated.
func (ns *nurseryStore) IsMatureChannel(chanPoint *wire.OutPoint) (bool, error) {
	if err := ns.db.View(func(tx *bolt.Tx) error {
		// Iterate over the contents of the channel bucket, computing
		// both total number of outputs, and those that have the grad
		// prefix.
		return ns.forChanOutputs(tx, chanPoint, func(pfxKey, _ []byte) error {
			if string(pfxKey[:4]) != string(gradPrefix) {
				utxnLog.Infof("Found non-graduated output: %x", pfxKey)
				return errImmatureChannel
			}
			return nil
		})

	}); err != nil && err != errImmatureChannel {
		return false, err
	} else {
		return err == nil, nil
	}

}

// RemoveChannel channel erases all entries from the channel bucket for the
// provided channel point.
func (ns *nurseryStore) RemoveChannel(chanPoint *wire.OutPoint) error {
	return ns.db.Update(func(tx *bolt.Tx) error {
		// Retrieve the existing chain bucket for this nursery store.
		chainBucket := tx.Bucket(ns.pfxChainKey)
		if chainBucket == nil {
			return nil
		}

		// Retrieve the channel index stored in the chain bucket.
		chanIndex := chainBucket.Bucket(channelIndexKey)
		if chanIndex == nil {
			return nil
		}

		// Serialize the provided channel point, such that we can delete
		// the mature channel bucket.
		var chanBuffer bytes.Buffer
		if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
			return err
		}

		err := chanIndex.DeleteBucket(chanBuffer.Bytes())
		if err != nil {
			return err
		}

		return nil
	})
}

// LastFinalizedHeight returns the last block height for which the nursery
// store has purged all persistent state. This occurs after a fixed interval
// for reorg safety.
func (ns *nurseryStore) LastFinalizedHeight() (uint32, error) {
	var lastFinalizedHeight uint32
	err := ns.db.View(func(tx *bolt.Tx) error {
		var err error
		lastFinalizedHeight, err = ns.getLastFinalizedHeight(tx)
		return err
	})

	return lastFinalizedHeight, err
}

// Helper Methods

// enterCrib accepts a new htlc output that the nursery will incubate through
// its two-stage process of sweeping funds back to the user's wallet. These
// outputs are persisted in the nursery store's crib bucket, and will be
// revisited after the output's CLTV has expired.
func (ns *nurseryStore) enterCrib(tx *bolt.Tx, baby *babyOutput) error {
	// First, retrieve or create the channel bucket corresponding to the
	// baby output's origin channel point.
	chanPoint := baby.OriginChanPoint()
	chanBucket, err := ns.createChannelBucket(tx, chanPoint)
	if err != nil {
		return err
	}

	// Next, retrieve or create the height-channel bucket located in the
	// height bucket corresponding to the baby output's CLTV expiry height.
	hghtChanBucket, err := ns.createHeightChanBucket(tx,
		baby.expiry, chanPoint)
	if err != nil {
		return err
	}

	// Since we are inserting this output into the crib bucket, we create a
	// key that prefixes the baby output's outpoint with the crib prefix.
	pfxOutputKey, err := prefixOutputKey(cribPrefix, baby.OutPoint())
	if err != nil {
		return err
	}

	// Serialize the baby output so that it can be written to the underlying
	// key-value store.
	var babyBuffer bytes.Buffer
	if err := baby.Encode(&babyBuffer); err != nil {
		return err
	}
	babyBytes := babyBuffer.Bytes()

	// Now, insert the serialized output into its channel bucket under the
	// prefixed key created above.
	if err := chanBucket.Put(pfxOutputKey, babyBytes); err != nil {
		return err
	}

	// Finally, create a corresponding bucket in the height-channel bucket
	// for this crib output. The existence of this bucket indicates that the
	// serialized output can be retrieved from the channel bucket using the
	// same prefix key.
	return hghtChanBucket.Put(pfxOutputKey, []byte{})
}

// enterPreschool accepts a new commitment output that the nursery will incubate
// through a single stage before sweeping. Outputs are stored in the preschool
// bucket until the commitment transaction has been confirmed, at which point
// they will be moved to the kindergarten bucket.
func (ns *nurseryStore) enterPreschool(tx *bolt.Tx, kid *kidOutput) error {
	// First, retrieve or create the channel bucket corresponding to the
	// baby output's origin channel point.
	chanPoint := kid.OriginChanPoint()
	chanBucket, err := ns.createChannelBucket(tx, chanPoint)
	if err != nil {
		return err
	}

	// Since the babyOutput is being inserted into the preschool bucket, we
	// create a key that prefixes its outpoint with the preschool prefix.
	pfxOutputKey, err := prefixOutputKey(psclPrefix, kid.OutPoint())
	if err != nil {
		return err
	}

	// Serialize the kidOutput and insert it into the channel bucket.
	var kidBuffer bytes.Buffer
	if err := kid.Encode(&kidBuffer); err != nil {
		return err
	}

	return chanBucket.Put(pfxOutputKey, kidBuffer.Bytes())
}

// createChannelBucket creates or retrieves a channel bucket for the provided
// channel point.
func (ns *nurseryStore) createChannelBucket(tx *bolt.Tx,
	chanPoint *wire.OutPoint) (*bolt.Bucket, error) {

	// Ensure that the chain bucket for this nursery store exists.
	chainBucket, err := tx.CreateBucketIfNotExists(ns.pfxChainKey)
	if err != nil {
		return nil, err
	}

	// Ensure that the channel index has been properly initialized for this
	// chain.
	chanIndex, err := chainBucket.CreateBucketIfNotExists(channelIndexKey)
	if err != nil {
		return nil, err
	}

	// Serialize the provided channel point, as this provides the name of
	// the channel bucket of interest.
	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return nil, err
	}

	// Finally, create or retrieve the channel bucket using the serialized
	// key.
	return chanIndex.CreateBucketIfNotExists(chanBuffer.Bytes())
}

// getChannelBucket retrieves an existing channel bucket from the nursery store,
// using the given channel point.  If the bucket does not exist, or any bucket
// along its path does not exist, a nil value is returned.
func (ns *nurseryStore) getChannelBucket(tx *bolt.Tx,
	chanPoint *wire.OutPoint) *bolt.Bucket {

	// Retrieve the existing chain bucket for this nursery store.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return nil
	}

	// Retrieve the existing channel index.
	chanIndex := chainBucket.Bucket(channelIndexKey)
	if chanIndex == nil {
		return nil
	}

	// Serialize the provided channel point and return the bucket matching
	// the serialized key.
	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return nil
	}

	return chanIndex.Bucket(chanBuffer.Bytes())
}

// createHeightBucket creates or retrieves an existing bucket from the height
// index, corresponding to the provided height.
func (ns *nurseryStore) createHeightBucket(tx *bolt.Tx,
	height uint32) (*bolt.Bucket, error) {

	// Ensure that the chain bucket for this nursery store exists.
	chainBucket, err := tx.CreateBucketIfNotExists(ns.pfxChainKey)
	if err != nil {
		return nil, err
	}

	// Ensure that the height index has been properly initialized for this
	// chain.
	hghtIndex, err := chainBucket.CreateBucketIfNotExists(heightIndexKey)
	if err != nil {
		return nil, err
	}

	// Serialize the provided height, as this will form the name of the
	// bucket.
	var heightBytes [4]byte
	byteOrder.PutUint32(heightBytes[:], height)

	// Finally, create or retrieve the bucket in question.
	return hghtIndex.CreateBucketIfNotExists(heightBytes[:])
}

// getHeightBucketPath retrieves an existing height bucket from the nursery store,
// using the provided block height. If the bucket does not exist, or any bucket
// along its path does not exist, a nil value is returned.
func (ns *nurseryStore) getHeightBucketPath(tx *bolt.Tx,
	height uint32) (*bolt.Bucket, *bolt.Bucket, *bolt.Bucket) {

	// Retrieve the existing chain bucket for this nursery store.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return nil, nil, nil
	}

	// Retrieve the existing channel index.
	hghtIndex := chainBucket.Bucket(heightIndexKey)
	if hghtIndex == nil {
		return nil, nil, nil
	}

	// Serialize the provided block height and return the bucket matching
	// the serialized key.
	var heightBytes [4]byte
	byteOrder.PutUint32(heightBytes[:], height)

	return chainBucket, hghtIndex, hghtIndex.Bucket(heightBytes[:])
}

// getHeightBucket retrieves an existing height bucket from the nursery store,
// using the provided block height. If the bucket does not exist, or any bucket
// along its path does not exist, a nil value is returned.
func (ns *nurseryStore) getHeightBucket(tx *bolt.Tx,
	height uint32) *bolt.Bucket {
	_, _, hghtBucket := ns.getHeightBucketPath(tx, height)

	return hghtBucket
}

// deleteHeightBucket ensures that the height bucket at the provided index is
// purged from the nursery store.
func (ns *nurseryStore) deleteHeightBucket(tx *bolt.Tx, height uint32) error {
	// Ensure that the chain bucket for this nursery store exists.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return nil
	}

	// Ensure that the height index has been properly initialized for this
	// chain.
	hghtIndex := chainBucket.Bucket(heightIndexKey)
	if hghtIndex == nil {
		return nil
	}

	// Serialize the provided height, as this will form the name of the
	// bucket.
	var heightBytes [4]byte
	byteOrder.PutUint32(heightBytes[:], height)

	// Finally, delete the bucket in question.
	err := hghtIndex.DeleteBucket(heightBytes[:])
	if err != nil && err != bolt.ErrBucketNotFound {
		return err
	}

	return nil
}

// createHeightChanBucket creates or retrieves an existing height-channel bucket
// for the provided block height and channel point. This method will attempt to
// instantiate all buckets along the path if required.
func (ns *nurseryStore) createHeightChanBucket(tx *bolt.Tx,
	height uint32, chanPoint *wire.OutPoint) (*bolt.Bucket, error) {

	// Ensure that the height bucket for this nursery store exists.
	hghtBucket, err := ns.createHeightBucket(tx, height)
	if err != nil {
		return nil, err
	}

	// Serialize the provided channel point, as this generates the name of
	// the subdirectory corresponding to the channel of interest.
	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return nil, err
	}
	chanBytes := chanBuffer.Bytes()

	// Finally, create or retrieve an existing height-channel bucket for
	// this channel point.
	return hghtBucket.CreateBucketIfNotExists(chanBytes)
}

// getHeightChanBucket retrieves an existing height-channel bucket from the
// nursery store, using the provided block height and channel point. if the
// bucket does not exist, or any bucket along its path does not exist, a nil
// value is returned.
func (ns *nurseryStore) getHeightChanBucket(tx *bolt.Tx,
	height uint32, chanPoint *wire.OutPoint) *bolt.Bucket {

	// Retrieve the existing height bucket from this nursery store.
	hghtBucket := ns.getHeightBucket(tx, height)
	if hghtBucket == nil {
		return nil
	}

	// Serialize the provided channel point, which generates the key for
	// looking up the proper height-channel bucket inside the height bucket.
	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return nil
	}
	chanBytes := chanBuffer.Bytes()

	// Finally, return the height bucket specified by the serialized channel
	// point.
	return hghtBucket.Bucket(chanBytes)
}

// forEachHeightPrefix enumerates all outputs at the given height whose state
// prefix matches that which is provided. This is used as a subroutine to help
// enumerate crib and kindergarten outputs at a particular height. The callback
// is invoked with serialized bytes retrieved for each output of interest,
// allowing the caller to deserialize them into the appropriate type.
func (ns *nurseryStore) forEachHeightPrefix(tx *bolt.Tx, prefix []byte,
	height uint32, callback func([]byte) error) error {

	// Start by retrieving the height bucket corresponding to the provided
	// block height.
	chainBucket, _, hghtBucket := ns.getHeightBucketPath(tx, height)
	if hghtBucket == nil {
		return nil
	}

	// Using the height bucket as a starting point, we will traverse its
	// entire two-tier directory structure, and filter for outputs that have
	// the provided prefix. The first layer of the height bucket contains
	// buckets identified by a channel point, thus we first create list of
	// channels contained in this height bucket.
	var channelsAtHeight [][]byte
	if err := hghtBucket.ForEach(func(chanBytes, _ []byte) error {
		channelsAtHeight = append(channelsAtHeight, chanBytes)
		return nil
	}); err != nil {
		return err
	}

	// Additionally, grab the chain index, which we will facilitate queries
	// for each of the channel buckets of each of the channels in the list
	// we assembled above.
	chanIndex := chainBucket.Bucket(channelIndexKey)
	if chanIndex == nil {
		return errors.New("unable to retrieve channel index")
	}

	// Now, we are ready to enumerate all outputs with the desired prefix at
	// this block height. We do so by iterating over our list of channels at
	// this height, filtering for outputs in each height-channel bucket that
	// begin with the given prefix, and then retrieving the serialized
	// outputs from the appropriate channel bucket.
	for _, chanBytes := range channelsAtHeight {
		// Retrieve the height-channel bucket for this channel, which
		// holds a sub-bucket for all outputs maturing at this height.
		hghtChanBucket := hghtBucket.Bucket(chanBytes)
		if hghtChanBucket == nil {
			return errors.New("unable to retrieve height-channel bucket")
		}

		// Load the appropriate channel bucket from the channel index,
		// this will allow us to retrieve the individual serialized
		// outputs.
		chanBucket := chanIndex.Bucket(chanBytes)
		if chanBucket == nil {
			return errors.New("unable to retrieve channel bucket")
		}

		// Since all of the outputs of interest will start with the same
		// prefix, we will perform a prefix scan of the buckets
		// contained in the height-channel bucket, efficiently
		// enumerating the desired outputs.
		c := hghtChanBucket.Cursor()

		// Seek to and iterate over all entries starting with the given
		// prefix.
		pfxOutputKey, _ := c.Seek(prefix)
		for bytes.HasPrefix(pfxOutputKey, prefix) {

			// Use the prefix output key emitted from our scan to
			// load the serialized babyOutput from the appropriate
			// channel bucket.
			outputBytes := chanBucket.Get(pfxOutputKey)
			if outputBytes == nil {
				return errors.New("unable to retrieve output")
			}

			// Present the serialized bytes to our call back
			// function, which is responsible for deserializing the
			// bytes into the appropriate type.
			if err := callback(outputBytes); err != nil {
				return err
			}

			// Lastly, advance our prefix output key for the next
			// iteration.
			pfxOutputKey, _ = c.Next()
		}
	}

	return nil
}

// forChanOutputs enumerates the outputs contained in a channel bucket to the
// provided callback. The callback accepts a key-value pair of byte slices
// corresponding to the prefixed-output key and the serialized output,
// respectively.
func (ns *nurseryStore) forChanOutputs(tx *bolt.Tx, chanPoint *wire.OutPoint,
	callback func([]byte, []byte) error) error {

	chanBucket := ns.getChannelBucket(tx, chanPoint)
	if chanBucket == nil {
		return ErrContractNotFound
	}

	return chanBucket.ForEach(callback)
}

// getLastFinalizedHeight is a helper method that retrieves the last height for
// which the database finalized its persistent state.
func (ns *nurseryStore) getLastFinalizedHeight(tx *bolt.Tx) (uint32, error) {
	// Retrieve the chain bucket associated with the given nursery store.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return 0, nil
	}

	// Lookup the last finalized height in the top-level chain bucket.
	heightBytes := chainBucket.Get(lastFinalizedHeightKey)

	// If the resulting bytes are not sized like a uint32, then we have
	// never finalized, so we return 0.
	if len(heightBytes) != 4 {
		return 0, nil
	}

	// Otherwise, parse the bytes and return the last finalized height.
	return byteOrder.Uint32(heightBytes), nil
}

// pubLastFinalizedHeight is a helper method that writes the provided height
// under the last finalized height key.
func (ns *nurseryStore) putLastFinalizedHeight(tx *bolt.Tx,
	height uint32) error {

	// Ensure that the chain bucket for this nursery store exists.
	chainBucket, err := tx.CreateBucketIfNotExists(ns.pfxChainKey)
	if err != nil {
		return err
	}

	// TODO(conner): purge all state below reorg depth.

	// Serialize the provided last-finalized height, and store it in the
	// top-level chain bucket for this nursery store.
	var lastHeightBytes [4]byte
	byteOrder.PutUint32(lastHeightBytes[:], height)

	return chainBucket.Put(lastFinalizedHeightKey, lastHeightBytes[:])
}

var (
	// ErrBucketDoesNotExist signals that a bucket has already been removed,
	// or was never created.
	ErrBucketDoesNotExist = errors.New("bucket does not exist")

	// ErrBucketNotEmpty signals that an attempt to prune a particular
	// bucket failed because it still has active outputs.
	ErrBucketNotEmpty = errors.New("bucket is not empty, cannot be pruned")
)

// pruneHeight removes the height bucket at the provided height if and only if
// all active outputs at this height have been removed from their respective
// height-channel buckets.
func (ns *nurseryStore) pruneHeight(tx *bolt.Tx, height uint32) (bool, error) {
	// Fetch the existing height index and height bucket.
	_, hghtIndex, hghtBucket := ns.getHeightBucketPath(tx, height)
	if hghtBucket == nil {
		return false, nil
	}

	// TODO(conner): fix this comment
	// this block height. We will attempt to remove each one if they are
	// empty, keeping track of the number of height-channel buckets that
	// still have active outputs.
	var nActiveBuckets int
	if err := hghtBucket.ForEach(func(chanBytes, _ []byte) error {
		// Attempt to each height-channel bucket from the height bucket
		// located above.
		_, err := ns.removeBucketIfEmpty(hghtBucket, chanBytes)
		if err != nil && err != ErrBucketNotEmpty {
			return err
		} else if err == ErrBucketNotEmpty {
			nActiveBuckets++
		}

		return nil

	}); err != nil {
		return false, err
	}

	// If we located any height-channel buckets that still have active
	// outputs, it is unsafe to delete this height bucket. Signal this event
	// to the caller so that they can determine the appropriate action.
	if nActiveBuckets > 0 {
		return false, ErrBucketNotEmpty
	}

	// Serialize the provided block height, such that it can be used as the
	// key to delete desired height bucket.
	var heightBytes [4]byte
	byteOrder.PutUint32(heightBytes[:], height)

	// All of the height-channel buckets are empty or have been previously
	// removed, proceed by removing the height bucket
	// altogether.
	if err := hghtIndex.DeleteBucket(heightBytes[:]); err != nil {
		return false, err
	}

	return true, nil
}

// removeBucketIfEmpty attempts to delete a bucket specified by name from the
// provided parent bucket.
func (ns *nurseryStore) removeBucketIfEmpty(parent *bolt.Bucket,
	bktName []byte) (bool, error) {

	// Attempt to fetch the named bucket from its parent.
	bkt := parent.Bucket(bktName)
	if bkt == nil {
		// No bucket was found, signal this to the caller.
		return false, nil
	}

	// The bucket exists, now compute how many children *it* has.
	nChildren, err := ns.numChildrenInBucket(bkt)
	if err != nil {
		return false, nil
	}

	// If the number of children is non-zero, alert the caller that the
	// named bucket is not being removed.
	if nChildren > 0 {
		return false, nil
	}

	// Otherwise, remove the empty bucket from its parent.
	err = parent.DeleteBucket(bktName)
	if err != nil {
		return false, err
	}

	return true, nil
}

// numChildrenInBucket computes the number of children contained in the given
// boltdb bucket.
func (ns *nurseryStore) numChildrenInBucket(parent *bolt.Bucket) (int, error) {
	var nChildren int
	if err := parent.ForEach(func(_, _ []byte) error {
		nChildren++
		return nil
	}); err != nil {
		return 0, err
	}

	return nChildren, nil
}

// Compile-time constraint to ensure nurseryStore implements NurseryStore.
var _ NurseryStore = (*nurseryStore)(nil)

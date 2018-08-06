package main

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
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
//   |   LAST PURGED + FINALIZED HEIGHTS
//   |
//   |   Each nursery store tracks a "last graduated height", which records the
//   |   most recent block height for which the nursery store has successfully
//   |   graduated all outputs. It also tracks a "last finalized height", which
//   |   records the last block height that the nursery attempted to graduate
//   |   If a finalized height has kindergarten outputs, the sweep txn for these
//   |   outputs will be stored in the height bucket. This ensure that the same
//   |   txid will be used after restarts. Otherwise, the nursery will be unable
//   |   to recover the txid of kindergarten sweep transaction it has already
//   |   broadcast.
//   |
//   ├── last-finalized-height-key: <last-finalized-height>
//   ├── last-graduated-height-key: <last-graduated-height>
//   |
//   |   CHANNEL INDEX
//   |
//   |   The channel index contains a directory for each channel that has a
//   |   non-zero number of outputs being tracked by the nursery store.
//   |   Inside each channel directory are files containing serialized spendable
//   |   outputs that are awaiting some state transition. The name of each file
//   |   contains the outpoint of the spendable output in the file, and is
//   |   prefixed with 4-byte state prefix, indicating whether the spendable
//   |   output is a crib, preschool, or kindergarten, or graduated output. The
//   |   nursery store supports the ability to enumerate all outputs for a
//   |   particular channel, which is useful in constructing nursery reports.
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
//   |   nursery still has scheduled actions. If an output is a crib or
//   |   kindergarten output, it will have an associated entry in the height
//   |   index. Inside a particular height directory, the structure is similar
//   |   to that of the channel index, containing multiple channel directories,
//   |   each of which contains subdirectories named with a prefixed outpoint
//   |   belonging to the channel. Enumerating these combinations yields a
//   |   relative file path:
//   |     e.g. <chan-point-3>/<prefix><outpoint-2>/
//   |   that can be queried in the channel index to retrieve the serialized
//   |   output. If a height bucket is less than or equal to the current last
//   |   finalized height and has a non-zero number of kindergarten outputs, a
//   |   height bucket will also contain the finalized kindergarten sweep txn
//   |   under the "finalized-kndr-txn" key.
//   |
//   └── height-index-key/
//       ├── <height-1>/                             <- HEIGHT BUCKET
//       |   ├── <chan-point-3>/                     <- HEIGHT-CHANNEL BUCKET
//       |   |    ├── <state-prefix><outpoint-4>: "" <- PREFIXED OUTPOINT
//       |   |    └── <state-prefix><outpoint-5>: ""
//       |   ├── <chan-point-2>/
//       |   |    └── <state-prefix><outpoint-3>: ""
//       |   └── finalized-kndr-txn:              "" | <kndr-sweep-tnx>
//       └── <height-2>/
//           └── <chan-point-1>/
//                └── <state-prefix><outpoint-1>: ""
//                └── <state-prefix><outpoint-2>: ""

// NurseryStore abstracts the persistent storage layer for the utxo nursery.
// Concretely, it stores commitment and htlc outputs until any time-bounded
// constraints have fully matured. The store exposes methods for enumerating its
// contents, and persisting state transitions detected by the utxo nursery.
type NurseryStore interface {
	// Incubate registers a set of CSV delayed outputs (incoming HTLC's on
	// our commitment transaction, or a commitment output), and a slice of
	// outgoing htlc outputs to be swept back into the user's wallet. The
	// event is persisted to disk, such that the nursery can resume the
	// incubation process after a potential crash.
	Incubate([]kidOutput, []babyOutput) error

	// CribToKinder atomically moves a babyOutput in the crib bucket to the
	// kindergarten bucket. Baby outputs are outgoing HTLC's which require
	// us to go to the second-layer to claim. The now mature kidOutput
	// contained in the babyOutput will be stored as it waits out the
	// kidOutput's CSV delay.
	CribToKinder(*babyOutput) error

	// PreschoolToKinder atomically moves a kidOutput from the preschool
	// bucket to the kindergarten bucket. This transition should be
	// executed after receiving confirmation of the preschool output.
	// Incoming HTLC's we need to go to the second-layer to claim, and also
	// our commitment outputs fall into this class.
	PreschoolToKinder(*kidOutput) error

	// GraduateKinder atomically moves the kindergarten class at the
	// provided height into the graduated status. This involves removing the
	// kindergarten entries from both the height and channel indexes, and
	// cleaning up the finalized kindergarten sweep txn. The height bucket
	// will be opportunistically pruned from the height index as outputs are
	// removed.
	GraduateKinder(height uint32) error

	// FetchPreschools returns a list of all outputs currently stored in
	// the preschool bucket.
	FetchPreschools() ([]kidOutput, error)

	// FetchClass returns a list of kindergarten and crib outputs whose
	// timelocks expire at the given height. If the kindergarten class at
	// this height hash been finalized previously, via FinalizeKinder, it
	// will also returns the finalized kindergarten sweep txn.
	FetchClass(height uint32) (*wire.MsgTx, []kidOutput, []babyOutput, error)

	// FinalizeKinder accepts a block height and the kindergarten sweep txn
	// computed for this height. Upon startup, we will rebroadcast any
	// finalized kindergarten txns instead of signing a new txn, as this
	// result in a different txid from a preceding broadcast.
	FinalizeKinder(height uint32, tx *wire.MsgTx) error

	// LastFinalizedHeight returns the last block height for which the
	// nursery store finalized a kindergarten class.
	LastFinalizedHeight() (uint32, error)

	// GraduateHeight records the provided height as the last height for
	// which the nursery store successfully graduated all outputs.
	GraduateHeight(height uint32) error

	// LastGraduatedHeight returns the last block height for which the
	// nursery store successfully graduated all outputs.
	LastGraduatedHeight() (uint32, error)

	// HeightsBelowOrEqual returns the lowest non-empty heights in the
	// height index, that exist at or below the provided upper bound.
	HeightsBelowOrEqual(height uint32) ([]uint32, error)

	// ForChanOutputs iterates over all outputs being incubated for a
	// particular channel point. This method accepts a callback that allows
	// the caller to process each key-value pair. The key will be a prefixed
	// outpoint, and the value will be the serialized bytes for an output,
	// whose type should be inferred from the key's prefix.
	ForChanOutputs(*wire.OutPoint, func([]byte, []byte) error) error

	// ListChannels returns all channels the nursery is currently tracking.
	ListChannels() ([]wire.OutPoint, error)

	// IsMatureChannel determines the whether or not all of the outputs in a
	// particular channel bucket have been marked as graduated.
	IsMatureChannel(*wire.OutPoint) (bool, error)

	// RemoveChannel channel erases all entries from the channel bucket for
	// the provided channel point, this method should only be called if
	// IsMatureChannel indicates the channel is ready for removal.
	RemoveChannel(*wire.OutPoint) error
}

var (
	// utxnChainPrefix is used to prefix a particular chain hash and create
	// the root-level, chain-segmented bucket for each nursery store.
	utxnChainPrefix = []byte("utxn")

	// lastFinalizedHeightKey is a static key used to locate nursery store's
	// last finalized height.
	lastFinalizedHeightKey = []byte("last-finalized-height")

	// lastGraduatedHeightKey is a static key used to retrieve the height of
	// the last bucket that successfully graduated all outputs.
	lastGraduatedHeightKey = []byte("last-graduated-height")

	// channelIndexKey is a static key used to lookup the bucket containing
	// all of the nursery's active channels.
	channelIndexKey = []byte("channel-index")

	// channelIndexKey is a static key used to retrieve a directory
	// containing all heights for which the nursery will need to take
	// action.
	heightIndexKey = []byte("height-index")

	// finalizedKndrTxnKey is a static key that can be used to locate a
	// finalized kindergarten sweep txn.
	finalizedKndrTxnKey = []byte("finalized-kndr-txn")
)

// Defines the state prefixes that will be used to persistently track an
// output's progress through the nursery.
// NOTE: Each state prefix MUST be exactly 4 bytes in length, the nursery logic
// depends on the ability to create keys for a different state by overwriting
// an existing state prefix.
var (
	// cribPrefix is the state prefix given to htlc outputs waiting for
	// their first-stage, absolute locktime to elapse.
	cribPrefix = []byte("crib")

	// psclPrefix is the state prefix given to commitment outputs awaiting
	// the confirmation of the commitment transaction, as this solidifies
	// the absolute height at which they can be spent.
	psclPrefix = []byte("pscl")

	// kndrPrefix is the state prefix given to all CSV delayed outputs,
	// either from the commitment transaction, or a stage-one htlc
	// transaction, whose maturity height has solidified. Outputs marked in
	// this state are in their final stage of incubation within the nursery,
	// and will be swept into the wallet after waiting out the relative
	// timelock.
	kndrPrefix = []byte("kndr")

	// gradPrefix is the state prefix given to all outputs that have been
	// completely incubated. Once all outputs have been marked as graduated,
	// this serves as a persistent marker that the nursery should mark the
	// channel fully closed in the channeldb.
	gradPrefix = []byte("grad")
)

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

// Incubate persists the beginning of the incubation process for the
// CSV-delayed outputs (commitment and incoming HTLC's), commitment output and
// a list of outgoing two-stage htlc outputs.
func (ns *nurseryStore) Incubate(kids []kidOutput, babies []babyOutput) error {
	return ns.db.Update(func(tx *bolt.Tx) error {
		// If we have any kid outputs to incubate, then we'll attempt
		// to add each of them to the nursery store. Any duplicate
		// outputs will be ignored.
		for _, kid := range kids {
			if err := ns.enterPreschool(tx, &kid); err != nil {
				return err
			}
		}

		// Next, we'll Add all htlc outputs to the crib bucket.
		// Similarly, we'll ignore any outputs that have already been
		// inserted.
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

		// Remove the crib output's entry in the height index.
		err = ns.removeOutputFromHeight(tx, bby.expiry, chanPoint,
			pfxOutputKey)
		if err != nil {
			return err
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

		// Retrieve or create a height-channel bucket corresponding to
		// the kidOutput's maturity height.
		hghtChanBucketCsv, err := ns.createHeightChanBucket(tx,
			maturityHeight, chanPoint)
		if err != nil {
			return err
		}

		utxnLog.Tracef("Transitioning (crib -> baby) output for "+
			"chan_point=%v at height_index=%v", chanPoint,
			maturityHeight)

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

		// If this output has an absolute time lock, then we'll set the
		// maturity height directly.
		var maturityHeight uint32
		if kid.BlocksToMaturity() == 0 {
			maturityHeight = kid.absoluteMaturity
		} else {
			// Otherwise, since the CSV delay on the kid output has
			// now begun ticking, we must insert a record of in the
			// height index to remind us to revisit this output
			// once it has fully matured.
			//
			// Compute the maturity height, by adding the output's
			// CSV delay to its confirmation height.
			maturityHeight = kid.ConfHeight() + kid.BlocksToMaturity()
		}

		// In the case of a Late Registration, we've already graduated
		// the class that this kid is destined for. So we'll bump its
		// height by one to ensure we don't forget to graduate it.
		lastGradHeight, err := ns.getLastGraduatedHeight(tx)
		if err != nil {
			return err
		}
		if maturityHeight <= lastGradHeight {
			utxnLog.Debugf("Late Registration for kid output=%v "+
				"detected: class_height=%v, "+
				"last_graduated_height=%v", kid.OutPoint(),
				maturityHeight, lastGradHeight)

			maturityHeight = lastGradHeight + 1
		}

		utxnLog.Infof("Transitioning (crib -> kid) output for "+
			"chan_point=%v at height_index=%v", chanPoint,
			maturityHeight)

		// Create or retrieve the height-channel bucket for this
		// channel. This method will first create a height bucket for
		// the given maturity height if none exists.
		hghtChanBucket, err := ns.createHeightChanBucket(tx,
			maturityHeight, chanPoint)
		if err != nil {
			return err
		}

		// Finally, we touch a key in the height-channel created above.
		// The key is named using a kindergarten prefixed key, signaling
		// that this CSV delayed output will be ready to broadcast at
		// the maturity height, after a brief period of incubation.
		return hghtChanBucket.Put(pfxOutputKey, []byte{})
	})
}

// GraduateKinder atomically moves the kindergarten class at the provided height
// into the graduated status. This involves removing the kindergarten entries
// from both the height and channel indexes, and cleaning up the finalized
// kindergarten sweep txn. The height bucket will be opportunistically pruned
// from the height index as outputs are removed.
func (ns *nurseryStore) GraduateKinder(height uint32) error {
	return ns.db.Update(func(tx *bolt.Tx) error {

		// Since all kindergarten outputs at a particular height are
		// swept in a single txn, we can now safely delete the finalized
		// txn, since it has already been broadcast and confirmed.
		hghtBucket := ns.getHeightBucket(tx, height)
		if hghtBucket == nil {
			// Nothing to delete, bucket has already been removed.
			return nil
		}

		// Remove the finalized kindergarten txn, we do this before
		// removing the outputs so that the extra entry doesn't prevent
		// the height bucket from being opportunistically pruned below.
		if err := hghtBucket.Delete(finalizedKndrTxnKey); err != nil {
			return err
		}

		// For each kindergarten found output, delete its entry from the
		// height and channel index, and create a new grad output in the
		// channel index.
		return ns.forEachHeightPrefix(tx, kndrPrefix, height,
			func(v []byte) error {
				var kid kidOutput
				err := kid.Decode(bytes.NewReader(v))
				if err != nil {
					return err
				}

				outpoint := kid.OutPoint()
				chanPoint := kid.OriginChanPoint()

				// Construct the key under which the output is
				// currently stored height and channel indexes.
				pfxOutputKey, err := prefixOutputKey(kndrPrefix,
					outpoint)
				if err != nil {
					return err
				}

				// Remove the grad output's entry in the height
				// index.
				err = ns.removeOutputFromHeight(tx, height,
					chanPoint, pfxOutputKey)
				if err != nil {
					return err
				}

				chanBucket := ns.getChannelBucket(tx,
					chanPoint)
				if chanBucket == nil {
					return ErrContractNotFound
				}

				// Remove previous output with kindergarten
				// prefix.
				err = chanBucket.Delete(pfxOutputKey)
				if err != nil {
					return err
				}

				// Convert kindergarten key to graduate key.
				copy(pfxOutputKey, gradPrefix)

				var gradBuffer bytes.Buffer
				if err := kid.Encode(&gradBuffer); err != nil {
					return err
				}

				// Insert serialized output into channel bucket
				// using graduate-prefixed key.
				return chanBucket.Put(pfxOutputKey,
					gradBuffer.Bytes())
			},
		)
	})
}

// FinalizeKinder accepts a block height and a finalized kindergarten sweep
// transaction, persisting the transaction at the appropriate height bucket. The
// nursery store's last finalized height is also updated with the provided
// height.
func (ns *nurseryStore) FinalizeKinder(height uint32,
	finalTx *wire.MsgTx) error {

	return ns.db.Update(func(tx *bolt.Tx) error {
		return ns.finalizeKinder(tx, height, finalTx)
	})
}

// GraduateHeight persists the provided height as the nursery store's last
// graduated height.
func (ns *nurseryStore) GraduateHeight(height uint32) error {

	return ns.db.Update(func(tx *bolt.Tx) error {
		return ns.putLastGraduatedHeight(tx, height)
	})
}

// FetchClass returns a list of babyOutputs in the crib bucket whose CLTV
// delay expires at the provided block height.
// FetchClass returns a list of the kindergarten and crib outputs whose timeouts
// are expiring
func (ns *nurseryStore) FetchClass(
	height uint32) (*wire.MsgTx, []kidOutput, []babyOutput, error) {

	// Construct list of all crib and kindergarten outputs that need to be
	// processed at the provided block height.
	var finalTx *wire.MsgTx
	var kids []kidOutput
	var babies []babyOutput
	if err := ns.db.View(func(tx *bolt.Tx) error {

		var err error
		finalTx, err = ns.getFinalizedTxn(tx, height)
		if err != nil {
			return err
		}

		// Append each crib output to our list of babyOutputs.
		if err = ns.forEachHeightPrefix(tx, cribPrefix, height,
			func(buf []byte) error {

				// We will attempt to deserialize all outputs
				// stored with the crib prefix into babyOutputs,
				// since this is the expected type that would
				// have been serialized previously.
				var baby babyOutput
				babyReader := bytes.NewReader(buf)
				if err := baby.Decode(babyReader); err != nil {
					return err
				}

				babies = append(babies, baby)

				return nil

			},
		); err != nil {
			return err
		}

		// Append each kindergarten output to our list of kidOutputs.
		return ns.forEachHeightPrefix(tx, kndrPrefix, height,
			func(buf []byte) error {
				// We will attempt to deserialize all outputs
				// stored with the kindergarten prefix into
				// kidOutputs, since this is the expected type
				// that would have been serialized previously.
				var kid kidOutput
				kidReader := bytes.NewReader(buf)
				if err := kid.Decode(kidReader); err != nil {
					return err
				}

				kids = append(kids, kid)

				return nil

			})

	}); err != nil {
		return nil, nil, nil, err
	}

	return finalTx, kids, babies, nil
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
			for k, v := c.Seek(psclPrefix); bytes.HasPrefix(
				k, psclPrefix); k, v = c.Next() {

				// Deserialize each output as a kidOutput, since
				// this should have been the type that was
				// serialized when it was written to disk.
				var psclOutput kidOutput
				psclReader := bytes.NewReader(v)
				err := psclOutput.Decode(psclReader)
				if err != nil {
					return err
				}

				// Add the deserialized output to our list of
				// preschool outputs.
				kids = append(kids, psclOutput)
			}
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return kids, nil
}

// HeightsBelowOrEqual returns a slice of all non-empty heights in the height
// index at or below the provided upper bound.
func (ns *nurseryStore) HeightsBelowOrEqual(height uint32) ([]uint32, error) {
	var activeHeights []uint32
	err := ns.db.View(func(tx *bolt.Tx) error {
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
		var lower, upper [4]byte
		byteOrder.PutUint32(upper[:], height)

		c := hghtIndex.Cursor()
		for k, _ := c.Seek(lower[:]); bytes.Compare(k, upper[:]) <= 0 &&
			len(k) == 4; k, _ = c.Next() {

			activeHeights = append(activeHeights, byteOrder.Uint32(k))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return activeHeights, nil
}

// ForChanOutputs iterates over all outputs being incubated for a particular
// channel point. This method accepts a callback that allows the caller to
// process each key-value pair. The key will be a prefixed outpoint, and the
// value will be the serialized bytes for an output, whose type should be
// inferred from the key's prefix.
// NOTE: The callback should not modify the provided byte slices and is
// preferably non-blocking.
func (ns *nurseryStore) ForChanOutputs(chanPoint *wire.OutPoint,
	callback func([]byte, []byte) error) error {

	return ns.db.View(func(tx *bolt.Tx) error {
		return ns.forChanOutputs(tx, chanPoint, callback)
	})
}

// ListChannels returns all channels the nursery is currently tracking.
func (ns *nurseryStore) ListChannels() ([]wire.OutPoint, error) {
	var activeChannels []wire.OutPoint
	if err := ns.db.View(func(tx *bolt.Tx) error {
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

		return chanIndex.ForEach(func(chanBytes, _ []byte) error {
			var chanPoint wire.OutPoint
			err := readOutpoint(bytes.NewReader(chanBytes), &chanPoint)
			if err != nil {
				return err
			}

			activeChannels = append(activeChannels, chanPoint)

			return nil
		})
	}); err != nil {
		return nil, err
	}

	return activeChannels, nil
}

// IsMatureChannel determines the whether or not all of the outputs in a
// particular channel bucket have been marked as graduated.
func (ns *nurseryStore) IsMatureChannel(chanPoint *wire.OutPoint) (bool, error) {
	err := ns.db.View(func(tx *bolt.Tx) error {
		// Iterate over the contents of the channel bucket, computing
		// both total number of outputs, and those that have the grad
		// prefix.
		return ns.forChanOutputs(tx, chanPoint,
			func(pfxKey, _ []byte) error {
				if !bytes.HasPrefix(pfxKey, gradPrefix) {
					return ErrImmatureChannel
				}
				return nil
			})

	})
	if err != nil && err != ErrImmatureChannel {
		return false, err
	}

	return err == nil, nil
}

// ErrImmatureChannel signals a channel cannot be removed because not all of its
// outputs have graduated.
var ErrImmatureChannel = errors.New("cannot remove immature channel, " +
	"still has ungraduated outputs")

// RemoveChannel channel erases all entries from the channel bucket for the
// provided channel point.
// NOTE: The channel's entries in the height index are assumed to be removed.
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
		chanBytes := chanBuffer.Bytes()

		err := ns.forChanOutputs(tx, chanPoint, func(k, v []byte) error {
			if !bytes.HasPrefix(k, gradPrefix) {
				return ErrImmatureChannel
			}

			// Construct a kindergarten prefixed key, since this
			// would have been the preceding state for a grad
			// output.
			kndrKey := make([]byte, len(k))
			copy(kndrKey, k)
			copy(kndrKey[:4], kndrPrefix)

			// Decode each to retrieve the output's maturity height.
			var kid kidOutput
			if err := kid.Decode(bytes.NewReader(v)); err != nil {
				return err
			}

			maturityHeight := kid.ConfHeight() + kid.BlocksToMaturity()

			hghtBucket := ns.getHeightBucket(tx, maturityHeight)
			if hghtBucket == nil {
				return nil
			}

			return removeBucketIfExists(hghtBucket, chanBytes)
		})
		if err != nil {
			return err
		}

		return removeBucketIfExists(chanIndex, chanBytes)
	})
}

// LastFinalizedHeight returns the last block height for which the nursery
// store has finalized a kindergarten class.
func (ns *nurseryStore) LastFinalizedHeight() (uint32, error) {
	var lastFinalizedHeight uint32
	err := ns.db.View(func(tx *bolt.Tx) error {
		var err error
		lastFinalizedHeight, err = ns.getLastFinalizedHeight(tx)
		return err
	})

	return lastFinalizedHeight, err
}

// LastGraduatedHeight returns the last block height for which the nursery
// store has successfully graduated all outputs.
func (ns *nurseryStore) LastGraduatedHeight() (uint32, error) {
	var lastGraduatedHeight uint32
	err := ns.db.View(func(tx *bolt.Tx) error {
		var err error
		lastGraduatedHeight, err = ns.getLastGraduatedHeight(tx)
		return err
	})

	return lastGraduatedHeight, err
}

// Helper Methods

// enterCrib accepts a new htlc output that the nursery will incubate through
// its two-stage process of sweeping funds back to the user's wallet. These
// outputs are persisted in the nursery store in the crib state, and will be
// revisited after the first-stage output's CLTV has expired.
func (ns *nurseryStore) enterCrib(tx *bolt.Tx, baby *babyOutput) error {
	// First, retrieve or create the channel bucket corresponding to the
	// baby output's origin channel point.
	chanPoint := baby.OriginChanPoint()
	chanBucket, err := ns.createChannelBucket(tx, chanPoint)
	if err != nil {
		return err
	}

	// Since we are inserting this output into the crib bucket, we create a
	// key that prefixes the baby output's outpoint with the crib prefix.
	pfxOutputKey, err := prefixOutputKey(cribPrefix, baby.OutPoint())
	if err != nil {
		return err
	}

	// We'll first check that we don't already have an entry for this
	// output. If we do, then we can exit early.
	if rawBytes := chanBucket.Get(pfxOutputKey); rawBytes != nil {
		return nil
	}

	// Next, retrieve or create the height-channel bucket located in the
	// height bucket corresponding to the baby output's CLTV expiry height.
	hghtChanBucket, err := ns.createHeightChanBucket(tx,
		baby.expiry, chanPoint)
	if err != nil {
		return err
	}

	// Serialize the baby output so that it can be written to the
	// underlying key-value store.
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
	// for this crib output. The existence of this bucket indicates that
	// the serialized output can be retrieved from the channel bucket using
	// the same prefix key.
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

	// Since the kidOutput is being inserted into the preschool bucket, we
	// create a key that prefixes its outpoint with the preschool prefix.
	pfxOutputKey, err := prefixOutputKey(psclPrefix, kid.OutPoint())
	if err != nil {
		return err
	}

	// We'll first check if an entry for this key is already stored. If so,
	// then we'll ignore this request, and return a nil error.
	if rawBytes := chanBucket.Get(pfxOutputKey); rawBytes != nil {
		return nil
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

// getHeightBucketPath retrieves an existing height bucket from the nursery
// store, using the provided block height. If the bucket does not exist, or any
// bucket along its path does not exist, a nil value is returned.
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
	if err := hghtBucket.ForEach(func(chanBytes, v []byte) error {
		if v == nil {
			channelsAtHeight = append(channelsAtHeight, chanBytes)
		}
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
			return fmt.Errorf("unable to retrieve height-channel "+
				"bucket at height %d for %x", height, chanBytes)
		}

		// Load the appropriate channel bucket from the channel index,
		// this will allow us to retrieve the individual serialized
		// outputs.
		chanBucket := chanIndex.Bucket(chanBytes)
		if chanBucket == nil {
			return fmt.Errorf("unable to retrieve channel "+
				"bucket: '%x'", chanBytes)
		}

		// Since all of the outputs of interest will start with the same
		// prefix, we will perform a prefix scan of the buckets
		// contained in the height-channel bucket, efficiently
		// enumerating the desired outputs.
		c := hghtChanBucket.Cursor()
		for k, _ := c.Seek(prefix); bytes.HasPrefix(
			k, prefix); k, _ = c.Next() {

			// Use the prefix output key emitted from our scan to
			// load the serialized babyOutput from the appropriate
			// channel bucket.
			outputBytes := chanBucket.Get(k)
			if outputBytes == nil {
				return errors.New("unable to retrieve output")
			}

			// Present the serialized bytes to our call back
			// function, which is responsible for deserializing the
			// bytes into the appropriate type.
			if err := callback(outputBytes); err != nil {
				return err
			}
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
	if heightBytes == nil {
		// We have never finalized, return height 0.
		return 0, nil
	}

	// If the resulting bytes are not sized like a uint32, then we have
	// never finalized, so we return 0.

	// Otherwise, parse the bytes and return the last finalized height.
	return byteOrder.Uint32(heightBytes), nil
}

// finalizeKinder records a finalized kindergarten sweep txn to the given height
// bucket. It also updates the nursery store's last finalized height, so that we
// do not finalize the same height twice. If the finalized txn is nil, i.e. if
// the height has no kindergarten outputs, the height will be marked as
// finalized, and we skip the process of writing the txn. When the class is
// loaded, a nil value will be returned if no txn has been written to a
// finalized height bucket.
func (ns *nurseryStore) finalizeKinder(tx *bolt.Tx, height uint32,
	finalTx *wire.MsgTx) error {

	// TODO(conner) ensure height is greater that current finalized height.

	// 1. Write the last finalized height to the chain bucket.

	// Ensure that the chain bucket for this nursery store exists.
	chainBucket, err := tx.CreateBucketIfNotExists(ns.pfxChainKey)
	if err != nil {
		return err
	}

	// Serialize the provided last-finalized height, and store it in the
	// top-level chain bucket for this nursery store.
	var lastHeightBytes [4]byte
	byteOrder.PutUint32(lastHeightBytes[:], height)

	err = chainBucket.Put(lastFinalizedHeightKey, lastHeightBytes[:])
	if err != nil {
		return err
	}

	// 2. Write the finalized txn in the appropriate height bucket.

	// If there is no finalized txn, we have nothing to do.
	if finalTx == nil {
		return nil
	}

	// Otherwise serialize the finalized txn and write it to the height
	// bucket.
	hghtBucket := ns.getHeightBucket(tx, height)
	if hghtBucket == nil {
		return nil
	}

	var finalTxnBuf bytes.Buffer
	if err := finalTx.Serialize(&finalTxnBuf); err != nil {
		return err
	}

	return hghtBucket.Put(finalizedKndrTxnKey, finalTxnBuf.Bytes())
}

// getFinalizedTxn retrieves the finalized kindergarten sweep txn at the given
// height, returning nil if one was not found.
func (ns *nurseryStore) getFinalizedTxn(tx *bolt.Tx,
	height uint32) (*wire.MsgTx, error) {

	hghtBucket := ns.getHeightBucket(tx, height)
	if hghtBucket == nil {
		// No class to finalize.
		return nil, nil
	}

	finalTxBytes := hghtBucket.Get(finalizedKndrTxnKey)
	if finalTxBytes == nil {
		// No finalized txn for this height.
		return nil, nil
	}

	// Otherwise, deserialize and return the finalized transaction.
	txn := &wire.MsgTx{}
	if err := txn.Deserialize(bytes.NewReader(finalTxBytes)); err != nil {
		return nil, err
	}

	return txn, nil
}

// getLastGraduatedHeight is a helper method that retrieves the last height for
// which the database graduated all outputs successfully.
func (ns *nurseryStore) getLastGraduatedHeight(tx *bolt.Tx) (uint32, error) {
	// Retrieve the chain bucket associated with the given nursery store.
	chainBucket := tx.Bucket(ns.pfxChainKey)
	if chainBucket == nil {
		return 0, nil
	}

	// Lookup the last graduated height in the top-level chain bucket.
	heightBytes := chainBucket.Get(lastGraduatedHeightKey)
	if heightBytes == nil {
		// We have never graduated before, return height 0.
		return 0, nil
	}

	// Otherwise, parse the bytes and return the last graduated height.
	return byteOrder.Uint32(heightBytes), nil
}

// pubLastGraduatedHeight is a helper method that writes the provided height under
// the last graduated height key.
func (ns *nurseryStore) putLastGraduatedHeight(tx *bolt.Tx, height uint32) error {

	// Ensure that the chain bucket for this nursery store exists.
	chainBucket, err := tx.CreateBucketIfNotExists(ns.pfxChainKey)
	if err != nil {
		return err
	}

	// Serialize the provided last-graduated height, and store it in the
	// top-level chain bucket for this nursery store.
	var lastHeightBytes [4]byte
	byteOrder.PutUint32(lastHeightBytes[:], height)

	return chainBucket.Put(lastGraduatedHeightKey, lastHeightBytes[:])
}

// errBucketNotEmpty signals that an attempt to prune a particular
// bucket failed because it still has active outputs.
var errBucketNotEmpty = errors.New("bucket is not empty, cannot be pruned")

// removeOutputFromHeight will delete the given output from the specified
// height-channel bucket, and attempt to prune the upstream directories if they
// are empty.
func (ns *nurseryStore) removeOutputFromHeight(tx *bolt.Tx, height uint32,
	chanPoint *wire.OutPoint, pfxKey []byte) error {

	// Retrieve the height-channel bucket and delete the prefixed output.
	hghtChanBucket := ns.getHeightChanBucket(tx, height, chanPoint)
	if hghtChanBucket == nil {
		// Height-channel bucket already removed.
		return nil
	}

	// Try to delete the prefixed output from the target height-channel
	// bucket.
	if err := hghtChanBucket.Delete(pfxKey); err != nil {
		return err
	}

	// Retrieve the height bucket that contains the height-channel bucket.
	hghtBucket := ns.getHeightBucket(tx, height)
	if hghtBucket == nil {
		return errors.New("height bucket not found")
	}

	var chanBuffer bytes.Buffer
	if err := writeOutpoint(&chanBuffer, chanPoint); err != nil {
		return err
	}

	// Try to remove the channel-height bucket if it this was the last
	// output in the bucket.
	err := removeBucketIfEmpty(hghtBucket, chanBuffer.Bytes())
	if err != nil && err != errBucketNotEmpty {
		return err
	} else if err == errBucketNotEmpty {
		return nil
	}

	// Attempt to prune the height bucket matching the kid output's
	// confirmation height in case that was the last height-chan bucket.
	pruned, err := ns.pruneHeight(tx, height)
	if err != nil && err != errBucketNotEmpty {
		return err
	} else if err == nil && pruned {
		utxnLog.Infof("Height bucket %d pruned", height)
	}

	return nil
}

// pruneHeight removes the height bucket at the provided height if and only if
// all active outputs at this height have been removed from their respective
// height-channel buckets. The returned boolean value indicated whether or not
// this invocation successfully pruned the height bucket.
func (ns *nurseryStore) pruneHeight(tx *bolt.Tx, height uint32) (bool, error) {
	// Fetch the existing height index and height bucket.
	_, hghtIndex, hghtBucket := ns.getHeightBucketPath(tx, height)
	if hghtBucket == nil {
		return false, nil
	}

	// Iterate over all channels stored at this block height. We will
	// attempt to remove each one if they are empty, keeping track of the
	// number of height-channel buckets that still have active outputs.
	if err := hghtBucket.ForEach(func(chanBytes, v []byte) error {
		// Skip the finalized txn key.
		if v != nil {
			return nil
		}

		// Attempt to each height-channel bucket from the height bucket
		// located above.
		hghtChanBucket := hghtBucket.Bucket(chanBytes)
		if hghtChanBucket == nil {
			return errors.New("unable to find height-channel bucket")
		}

		return isBucketEmpty(hghtChanBucket)

	}); err != nil {
		return false, err
	}

	// Serialize the provided block height, such that it can be used as the
	// key to delete desired height bucket.
	var heightBytes [4]byte
	byteOrder.PutUint32(heightBytes[:], height)

	// All of the height-channel buckets are empty or have been previously
	// removed, proceed by removing the height bucket
	// altogether.
	if err := removeBucketIfExists(hghtIndex, heightBytes[:]); err != nil {
		return false, err
	}

	return true, nil
}

// removeBucketIfEmpty attempts to delete a bucket specified by name from the
// provided parent bucket.
func removeBucketIfEmpty(parent *bolt.Bucket, bktName []byte) error {
	// Attempt to fetch the named bucket from its parent.
	bkt := parent.Bucket(bktName)
	if bkt == nil {
		// No bucket was found, already removed?
		return nil
	}

	// The bucket exists, fail if it still has children.
	if err := isBucketEmpty(bkt); err != nil {
		return err
	}

	return parent.DeleteBucket(bktName)
}

// removeBucketIfExists safely deletes the named bucket by first checking
// that it exists in the parent bucket.
func removeBucketIfExists(parent *bolt.Bucket, bktName []byte) error {
	// Attempt to fetch the named bucket from its parent.
	bkt := parent.Bucket(bktName)
	if bkt == nil {
		// No bucket was found, already removed?
		return nil
	}

	return parent.DeleteBucket(bktName)
}

// isBucketEmpty returns errBucketNotEmpty if the bucket has a non-zero number
// of children.
func isBucketEmpty(parent *bolt.Bucket) error {
	return parent.ForEach(func(_, _ []byte) error {
		return errBucketNotEmpty
	})
}

// Compile-time constraint to ensure nurseryStore implements NurseryStore.
var _ NurseryStore = (*nurseryStore)(nil)

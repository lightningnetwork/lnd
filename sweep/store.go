package sweep

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// txHashesBucketKey is the key that points to a bucket containing the
	// hashes of all sweep txes that were published successfully.
	//
	// maps: txHash -> empty slice
	txHashesBucketKey = []byte("sweeper-tx-hashes")

	// utxnChainPrefix is the bucket prefix for nursery buckets.
	utxnChainPrefix = []byte("utxn")

	// utxnHeightIndexKey is the sub bucket where the nursery stores the
	// height index.
	utxnHeightIndexKey = []byte("height-index")

	// utxnFinalizedKndrTxnKey is a static key that can be used to locate
	// the nursery finalized kindergarten sweep txn.
	utxnFinalizedKndrTxnKey = []byte("finalized-kndr-txn")

	byteOrder = binary.BigEndian

	errNoTxHashesBucket = errors.New("tx hashes bucket does not exist")
)

// SweeperStore stores published txes.
type SweeperStore interface {
	// IsOurTx determines whether a tx is published by us, based on its
	// hash.
	IsOurTx(hash chainhash.Hash) (bool, error)

	// NotifyPublishTx signals that we are about to publish a tx.
	NotifyPublishTx(*wire.MsgTx) error

	// ListSweeps lists all the sweeps we have successfully published.
	ListSweeps() ([]chainhash.Hash, error)
}

type sweeperStore struct {
	db kvdb.Backend
}

// NewSweeperStore returns a new store instance.
func NewSweeperStore(db kvdb.Backend, chainHash *chainhash.Hash) (
	SweeperStore, error) {

	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		if tx.ReadWriteBucket(txHashesBucketKey) != nil {
			return nil
		}

		txHashesBucket, err := tx.CreateTopLevelBucket(
			txHashesBucketKey,
		)
		if err != nil {
			return err
		}

		// Use non-existence of tx hashes bucket as a signal to migrate
		// nursery finalized txes.
		err = migrateTxHashes(tx, txHashesBucket, chainHash)

		return err
	}, func() {})
	if err != nil {
		return nil, err
	}

	return &sweeperStore{
		db: db,
	}, nil
}

// migrateTxHashes migrates nursery finalized txes to the tx hashes bucket. This
// is not implemented as a database migration, to keep the downgrade path open.
func migrateTxHashes(tx kvdb.RwTx, txHashesBucket kvdb.RwBucket,
	chainHash *chainhash.Hash) error {

	log.Infof("Migrating UTXO nursery finalized TXIDs")

	// Compose chain bucket key.
	var b bytes.Buffer
	if _, err := b.Write(utxnChainPrefix); err != nil {
		return err
	}

	if _, err := b.Write(chainHash[:]); err != nil {
		return err
	}

	// Get chain bucket if exists.
	chainBucket := tx.ReadWriteBucket(b.Bytes())
	if chainBucket == nil {
		return nil
	}

	// Retrieve the existing height index.
	hghtIndex := chainBucket.NestedReadWriteBucket(utxnHeightIndexKey)
	if hghtIndex == nil {
		return nil
	}

	// Retrieve all heights.
	err := hghtIndex.ForEach(func(k, v []byte) error {
		heightBucket := hghtIndex.NestedReadWriteBucket(k)
		if heightBucket == nil {
			return nil
		}

		// Get finalized tx for height.
		txBytes := heightBucket.Get(utxnFinalizedKndrTxnKey)
		if txBytes == nil {
			return nil
		}

		// Deserialize and skip tx if it cannot be deserialized.
		tx := &wire.MsgTx{}
		err := tx.Deserialize(bytes.NewReader(txBytes))
		if err != nil {
			log.Warnf("Cannot deserialize utxn tx")
			return nil
		}

		// Calculate hash.
		hash := tx.TxHash()

		// Insert utxn tx hash in hashes bucket.
		log.Debugf("Inserting nursery tx %v in hash list "+
			"(height=%v)", hash, byteOrder.Uint32(k))

		return txHashesBucket.Put(hash[:], []byte{})
	})
	if err != nil {
		return err
	}

	return nil
}

// NotifyPublishTx signals that we are about to publish a tx.
func (s *sweeperStore) NotifyPublishTx(sweepTx *wire.MsgTx) error {
	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {

		txHashesBucket := tx.ReadWriteBucket(txHashesBucketKey)
		if txHashesBucket == nil {
			return errNoTxHashesBucket
		}

		hash := sweepTx.TxHash()

		return txHashesBucket.Put(hash[:], []byte{})
	}, func() {})
}

// IsOurTx determines whether a tx is published by us, based on its
// hash.
func (s *sweeperStore) IsOurTx(hash chainhash.Hash) (bool, error) {
	var ours bool

	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		txHashesBucket := tx.ReadBucket(txHashesBucketKey)
		if txHashesBucket == nil {
			return errNoTxHashesBucket
		}

		ours = txHashesBucket.Get(hash[:]) != nil

		return nil
	}, func() {
		ours = false
	})
	if err != nil {
		return false, err
	}

	return ours, nil
}

// ListSweeps lists all the sweep transactions we have in the sweeper store.
func (s *sweeperStore) ListSweeps() ([]chainhash.Hash, error) {
	var sweepTxns []chainhash.Hash

	if err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		txHashesBucket := tx.ReadBucket(txHashesBucketKey)
		if txHashesBucket == nil {
			return errNoTxHashesBucket
		}

		return txHashesBucket.ForEach(func(resKey, _ []byte) error {
			txid, err := chainhash.NewHash(resKey)
			if err != nil {
				return err
			}

			sweepTxns = append(sweepTxns, *txid)

			return nil
		})
	}, func() {
		sweepTxns = nil
	}); err != nil {
		return nil, err
	}

	return sweepTxns, nil
}

// Compile-time constraint to ensure sweeperStore implements SweeperStore.
var _ SweeperStore = (*sweeperStore)(nil)

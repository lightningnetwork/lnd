package sweep

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
)

var (
	// lastTxBucketKey is the key that points to a bucket containing a
	// single item storing the last published tx.
	//
	// maps: lastTxKey -> serialized_tx
	lastTxBucketKey = []byte("sweeper-last-tx")

	// lastTxKey is the fixed key under which the serialized tx is stored.
	lastTxKey = []byte("last-tx")

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
)

// SweeperStore stores published txes.
type SweeperStore interface {
	// IsOurTx determines whether a tx is published by us, based on its
	// hash.
	IsOurTx(hash chainhash.Hash) (bool, error)

	// NotifyPublishTx signals that we are about to publish a tx.
	NotifyPublishTx(*wire.MsgTx) error

	// GetLastPublishedTx returns the last tx that we called NotifyPublishTx
	// for.
	GetLastPublishedTx() (*wire.MsgTx, error)
}

type sweeperStore struct {
	db *channeldb.DB
}

// NewSweeperStore returns a new store instance.
func NewSweeperStore(db *channeldb.DB, chainHash *chainhash.Hash) (
	SweeperStore, error) {

	err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(
			lastTxBucketKey,
		)
		if err != nil {
			return err
		}

		if tx.Bucket(txHashesBucketKey) != nil {
			return nil
		}

		txHashesBucket, err := tx.CreateBucket(txHashesBucketKey)
		if err != nil {
			return err
		}

		// Use non-existence of tx hashes bucket as a signal to migrate
		// nursery finalized txes.
		err = migrateTxHashes(tx, txHashesBucket, chainHash)

		return err
	})
	if err != nil {
		return nil, err
	}

	return &sweeperStore{
		db: db,
	}, nil
}

// migrateTxHashes migrates nursery finalized txes to the tx hashes bucket. This
// is not implemented as a database migration, to keep the downgrade path open.
func migrateTxHashes(tx *bbolt.Tx, txHashesBucket *bbolt.Bucket,
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
	chainBucket := tx.Bucket(b.Bytes())
	if chainBucket == nil {
		return nil
	}

	// Retrieve the existing height index.
	hghtIndex := chainBucket.Bucket(utxnHeightIndexKey)
	if hghtIndex == nil {
		return nil
	}

	// Retrieve all heights.
	err := hghtIndex.ForEach(func(k, v []byte) error {
		heightBucket := hghtIndex.Bucket(k)
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
	return s.db.Update(func(tx *bbolt.Tx) error {
		lastTxBucket := tx.Bucket(lastTxBucketKey)
		if lastTxBucket == nil {
			return errors.New("last tx bucket does not exist")
		}

		txHashesBucket := tx.Bucket(txHashesBucketKey)
		if txHashesBucket == nil {
			return errors.New("tx hashes bucket does not exist")
		}

		var b bytes.Buffer
		if err := sweepTx.Serialize(&b); err != nil {
			return err
		}

		if err := lastTxBucket.Put(lastTxKey, b.Bytes()); err != nil {
			return err
		}

		hash := sweepTx.TxHash()

		return txHashesBucket.Put(hash[:], []byte{})
	})
}

// GetLastPublishedTx returns the last tx that we called NotifyPublishTx
// for.
func (s *sweeperStore) GetLastPublishedTx() (*wire.MsgTx, error) {
	var sweepTx *wire.MsgTx

	err := s.db.View(func(tx *bbolt.Tx) error {
		lastTxBucket := tx.Bucket(lastTxBucketKey)
		if lastTxBucket == nil {
			return errors.New("last tx bucket does not exist")
		}

		sweepTxRaw := lastTxBucket.Get(lastTxKey)
		if sweepTxRaw == nil {
			return nil
		}

		sweepTx = &wire.MsgTx{}
		txReader := bytes.NewReader(sweepTxRaw)
		if err := sweepTx.Deserialize(txReader); err != nil {
			return fmt.Errorf("tx deserialize: %v", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return sweepTx, nil
}

// IsOurTx determines whether a tx is published by us, based on its
// hash.
func (s *sweeperStore) IsOurTx(hash chainhash.Hash) (bool, error) {
	var ours bool

	err := s.db.View(func(tx *bbolt.Tx) error {
		txHashesBucket := tx.Bucket(txHashesBucketKey)
		if txHashesBucket == nil {
			return errors.New("tx hashes bucket does not exist")
		}

		ours = txHashesBucket.Get(hash[:]) != nil

		return nil
	})
	if err != nil {
		return false, err
	}

	return ours, nil
}

// Compile-time constraint to ensure sweeperStore implements SweeperStore.
var _ SweeperStore = (*sweeperStore)(nil)

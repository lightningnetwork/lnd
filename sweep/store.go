package sweep

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// txHashesBucketKey is the key that points to a bucket containing the
	// hashes of all sweep txes that were published successfully.
	//
	// maps: txHash -> TxRecord
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

	// ErrTxNotFound is returned when querying using a txid that's not
	// found in our db.
	ErrTxNotFound = errors.New("tx not found")
)

// TxRecord specifies a record of a tx that's stored in the database.
type TxRecord struct {
	// Txid is the sweeping tx's txid, which is used as the key to store
	// the following values.
	Txid chainhash.Hash

	// FeeRate is the fee rate of the sweeping tx, unit is sats/kw.
	FeeRate uint64

	// Fee is the fee of the sweeping tx, unit is sat.
	Fee uint64

	// Published indicates whether the tx has been published.
	Published bool
}

// toTlvStream converts TxRecord into a tlv representation.
func (t *TxRecord) toTlvStream() (*tlv.Stream, error) {
	const (
		// A set of tlv type definitions used to serialize TxRecord.
		// We define it here instead of the head of the file to avoid
		// naming conflicts.
		//
		// NOTE: A migration should be added whenever the existing type
		// changes.
		//
		// NOTE: Txid is stored as the key, so it's not included here.
		feeRateType tlv.Type = 0
		feeType     tlv.Type = 1
		boolType    tlv.Type = 2
	)

	return tlv.NewStream(
		tlv.MakeBigSizeRecord(feeRateType, &t.FeeRate),
		tlv.MakeBigSizeRecord(feeType, &t.Fee),
		tlv.MakePrimitiveRecord(boolType, &t.Published),
	)
}

// serializeTxRecord serializes a TxRecord based on tlv format.
func serializeTxRecord(w io.Writer, tx *TxRecord) error {
	// Create the tlv stream.
	tlvStream, err := tx.toTlvStream()
	if err != nil {
		return err
	}

	// Encode the tlv stream.
	var buf bytes.Buffer
	if err := tlvStream.Encode(&buf); err != nil {
		return err
	}

	// Write the tlv stream.
	if _, err = w.Write(buf.Bytes()); err != nil {
		return err
	}

	return nil
}

// deserializeTxRecord deserializes a TxRecord based on tlv format.
func deserializeTxRecord(r io.Reader) (*TxRecord, error) {
	var tx TxRecord

	// Create the tlv stream.
	tlvStream, err := tx.toTlvStream()
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Decode(r); err != nil {
		return nil, err
	}

	return &tx, nil
}

// SweeperStore stores published txes.
type SweeperStore interface {
	// IsOurTx determines whether a tx is published by us, based on its
	// hash.
	IsOurTx(hash chainhash.Hash) bool

	// StoreTx stores a tx hash we are about to publish.
	StoreTx(*TxRecord) error

	// ListSweeps lists all the sweeps we have successfully published.
	ListSweeps() ([]chainhash.Hash, error)

	// GetTx queries the database to find the tx that matches the given
	// txid. Returns ErrTxNotFound if it cannot be found.
	GetTx(hash chainhash.Hash) (*TxRecord, error)

	// DeleteTx removes a tx specified by the hash from the store.
	DeleteTx(hash chainhash.Hash) error
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
//
// TODO(yy): delete this function once nursery is removed.
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

		// Create the transaction record. Since this is an old record,
		// we can assume it's already been published. Although it's
		// possible to calculate the fees and fee rate used here, we
		// skip it as it's unlikely we'd perform RBF on these old
		// sweeping transactions.
		tr := &TxRecord{
			Txid:      hash,
			Published: true,
		}

		// Serialize tx record.
		var b bytes.Buffer
		err = serializeTxRecord(&b, tr)
		if err != nil {
			return err
		}

		return txHashesBucket.Put(tr.Txid[:], b.Bytes())
	})
	if err != nil {
		return err
	}

	return nil
}

// StoreTx stores that we are about to publish a tx.
func (s *sweeperStore) StoreTx(tr *TxRecord) error {
	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		txHashesBucket := tx.ReadWriteBucket(txHashesBucketKey)
		if txHashesBucket == nil {
			return errNoTxHashesBucket
		}

		// Serialize tx record.
		var b bytes.Buffer
		err := serializeTxRecord(&b, tr)
		if err != nil {
			return err
		}

		return txHashesBucket.Put(tr.Txid[:], b.Bytes())
	}, func() {})
}

// IsOurTx determines whether a tx is published by us, based on its hash.
func (s *sweeperStore) IsOurTx(hash chainhash.Hash) bool {
	var ours bool

	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		txHashesBucket := tx.ReadBucket(txHashesBucketKey)
		// If the root bucket cannot be found, we consider the tx to be
		// not found in our db.
		if txHashesBucket == nil {
			log.Error("Tx hashes bucket not found in sweeper store")
			return nil
		}

		ours = txHashesBucket.Get(hash[:]) != nil

		return nil
	}, func() {
		ours = false
	})
	if err != nil {
		return false
	}

	return ours
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

// GetTx queries the database to find the tx that matches the given txid.
// Returns ErrTxNotFound if it cannot be found.
func (s *sweeperStore) GetTx(txid chainhash.Hash) (*TxRecord, error) {
	// Create a record.
	tr := &TxRecord{}

	var err error
	err = kvdb.View(s.db, func(tx kvdb.RTx) error {
		txHashesBucket := tx.ReadBucket(txHashesBucketKey)
		if txHashesBucket == nil {
			return errNoTxHashesBucket
		}

		txBytes := txHashesBucket.Get(txid[:])
		if txBytes == nil {
			return ErrTxNotFound
		}

		// For old records, we'd get an empty byte slice here. We can
		// assume it's already been published. Although it's possible
		// to calculate the fees and fee rate used here, we skip it as
		// it's unlikely we'd perform RBF on these old sweeping
		// transactions.
		//
		// TODO(yy): remove this check once migration is added.
		if len(txBytes) == 0 {
			tr.Published = true
			return nil
		}

		tr, err = deserializeTxRecord(bytes.NewReader(txBytes))
		if err != nil {
			return err
		}

		return nil
	}, func() {
		tr = &TxRecord{}
	})
	if err != nil {
		return nil, err
	}

	// Attach the txid to the record.
	tr.Txid = txid

	return tr, nil
}

// DeleteTx removes the given tx from db.
func (s *sweeperStore) DeleteTx(txid chainhash.Hash) error {
	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		txHashesBucket := tx.ReadWriteBucket(txHashesBucketKey)
		if txHashesBucket == nil {
			return errNoTxHashesBucket
		}

		return txHashesBucket.Delete(txid[:])
	}, func() {})
}

// Compile-time constraint to ensure sweeperStore implements SweeperStore.
var _ SweeperStore = (*sweeperStore)(nil)

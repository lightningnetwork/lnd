package migration16

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	paymentsRootBucket = []byte("payments-root-bucket")

	paymentSequenceKey = []byte("payment-sequence-key")

	duplicatePaymentsBucket = []byte("payment-duplicate-bucket")

	paymentsIndexBucket = []byte("payments-index-bucket")

	byteOrder = binary.BigEndian
)

// paymentIndexType indicates the type of index we have recorded in the payment
// indexes bucket.
type paymentIndexType uint8

// paymentIndexTypeHash is a payment index type which indicates that we have
// created an index of payment sequence number to payment hash.
const paymentIndexTypeHash paymentIndexType = 0

// paymentIndex stores all the information we require to create an index by
// sequence number for a payment.
type paymentIndex struct {
	// paymentHash is the hash of the payment, which is its key in the
	// payment root bucket.
	paymentHash []byte

	// sequenceNumbers is the set of sequence numbers associated with this
	// payment hash. There will be more than one sequence number in the
	// case where duplicate payments are present.
	sequenceNumbers [][]byte
}

// MigrateSequenceIndex migrates the payments db to contain a new bucket which
// provides an index from sequence number to payment hash. This is required
// for more efficient sequential lookup of payments, which are keyed by payment
// hash before this migration.
func MigrateSequenceIndex(tx kvdb.RwTx) error {
	log.Infof("Migrating payments to add sequence number index")

	// Get a list of indices we need to write.
	indexList, err := getPaymentIndexList(tx)
	if err != nil {
		return err
	}

	// Create the top level bucket that we will use to index payments in.
	bucket, err := tx.CreateTopLevelBucket(paymentsIndexBucket)
	if err != nil {
		return err
	}

	// Write an index for each of our payments.
	for _, index := range indexList {
		// Write indexes for each of our sequence numbers.
		for _, seqNr := range index.sequenceNumbers {
			err := putIndex(bucket, seqNr, index.paymentHash)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// putIndex performs a sanity check that ensures we are not writing duplicate
// indexes to disk then creates the index provided.
func putIndex(bucket kvdb.RwBucket, sequenceNr, paymentHash []byte) error {
	// Add a sanity check that we do not already have an entry with
	// this sequence number.
	existingEntry := bucket.Get(sequenceNr)
	if existingEntry != nil {
		return fmt.Errorf("sequence number: %x duplicated",
			sequenceNr)
	}

	bytes, err := serializePaymentIndexEntry(paymentHash)
	if err != nil {
		return err
	}

	return bucket.Put(sequenceNr, bytes)
}

// serializePaymentIndexEntry serializes a payment hash typed index. The value
// produced contains a payment index type (which can be used in future to
// signal different payment index types) and the payment hash.
func serializePaymentIndexEntry(hash []byte) ([]byte, error) {
	var b bytes.Buffer

	err := binary.Write(&b, byteOrder, paymentIndexTypeHash)
	if err != nil {
		return nil, err
	}

	if err := wire.WriteVarBytes(&b, 0, hash); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// getPaymentIndexList gets a list of indices we need to write for our current
// set of payments.
func getPaymentIndexList(tx kvdb.RTx) ([]paymentIndex, error) {
	// Iterate over all payments and store their indexing keys. This is
	// needed, because no modifications are allowed inside a Bucket.ForEach
	// loop.
	paymentsBucket := tx.ReadBucket(paymentsRootBucket)
	if paymentsBucket == nil {
		return nil, nil
	}

	var indexList []paymentIndex
	err := paymentsBucket.ForEach(func(k, v []byte) error {
		// Get the bucket which contains the payment, fail if the key
		// does not have a bucket.
		bucket := paymentsBucket.NestedReadBucket(k)
		if bucket == nil {
			return fmt.Errorf("non bucket element in " +
				"payments bucket")
		}
		seqBytes := bucket.Get(paymentSequenceKey)
		if seqBytes == nil {
			return fmt.Errorf("nil sequence number bytes")
		}

		seqNrs, err := fetchSequenceNumbers(bucket)
		if err != nil {
			return err
		}

		// Create an index object with our payment hash and sequence
		// numbers and append it to our set of indexes.
		index := paymentIndex{
			paymentHash:     k,
			sequenceNumbers: seqNrs,
		}

		indexList = append(indexList, index)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return indexList, nil
}

// fetchSequenceNumbers fetches all the sequence numbers associated with a
// payment, including those belonging to any duplicate payments.
func fetchSequenceNumbers(paymentBucket kvdb.RBucket) ([][]byte, error) {
	seqNum := paymentBucket.Get(paymentSequenceKey)
	if seqNum == nil {
		return nil, errors.New("expected sequence number")
	}

	sequenceNumbers := [][]byte{seqNum}

	// Get the duplicate payments bucket, if it has no duplicates, just
	// return early with the payment sequence number.
	duplicates := paymentBucket.NestedReadBucket(duplicatePaymentsBucket)
	if duplicates == nil {
		return sequenceNumbers, nil
	}

	// If we do have duplicated, they are keyed by sequence number, so we
	// iterate through the duplicates bucket and add them to our set of
	// sequence numbers.
	if err := duplicates.ForEach(func(k, v []byte) error {
		sequenceNumbers = append(sequenceNumbers, k)
		return nil
	}); err != nil {
		return nil, err
	}

	return sequenceNumbers, nil
}

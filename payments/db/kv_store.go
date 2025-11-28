package paymentsdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// paymentSeqBlockSize is the block size used when we batch allocate
	// payment sequences for future payments.
	paymentSeqBlockSize = 1000

	// paymentProgressLogInterval is the interval we use limiting the
	// logging output of payment processing.
	paymentProgressLogInterval = 30 * time.Second
)

//nolint:ll
var (
	// paymentsRootBucket is the name of the top-level bucket within the
	// database that stores all data related to payments. Within this
	// bucket, each payment hash its own sub-bucket keyed by its payment
	// hash.
	//
	// Bucket hierarchy:
	//
	// root-bucket
	//      |
	//      |-- <paymenthash>
	//      |        |--sequence-key: <sequence number>
	//      |        |--creation-info-key: <creation info>
	//      |        |--fail-info-key: <(optional) fail info>
	//      |        |
	//      |        |--payment-htlcs-bucket (shard-bucket)
	//      |        |        |
	//      |        |        |-- ai<htlc attempt ID>: <htlc attempt info>
	//      |        |        |-- si<htlc attempt ID>: <(optional) settle info>
	//      |        |        |-- fi<htlc attempt ID>: <(optional) fail info>
	//      |        |        |
	//      |        |       ...
	//      |        |
	//      |        |
	//      |        |--duplicate-bucket (only for old, completed payments)
	//      |                 |
	//      |                 |-- <seq-num>
	//      |                 |       |--sequence-key: <sequence number>
	//      |                 |       |--creation-info-key: <creation info>
	//      |                 |       |--ai: <attempt info>
	//      |                 |       |--si: <settle info>
	//      |                 |       |--fi: <fail info>
	//      |                 |
	//      |                 |-- <seq-num>
	//      |                 |       |
	//      |                ...     ...
	//      |
	//      |-- <paymenthash>
	//      |        |
	//      |       ...
	//     ...
	//
	paymentsRootBucket = []byte("payments-root-bucket")

	// paymentSequenceKey is a key used in the payment's sub-bucket to
	// store the sequence number of the payment.
	paymentSequenceKey = []byte("payment-sequence-key")

	// paymentCreationInfoKey is a key used in the payment's sub-bucket to
	// store the creation info of the payment.
	paymentCreationInfoKey = []byte("payment-creation-info")

	// paymentHtlcsBucket is a bucket where we'll store the information
	// about the HTLCs that were attempted for a payment.
	paymentHtlcsBucket = []byte("payment-htlcs-bucket")

	// htlcAttemptInfoKey is the key used as the prefix of an HTLC attempt
	// to store the info about the attempt that was done for the HTLC in
	// question. The HTLC attempt ID is concatenated at the end.
	htlcAttemptInfoKey = []byte("ai")

	// htlcSettleInfoKey is the key used as the prefix of an HTLC attempt
	// settle info, if any. The HTLC attempt ID is concatenated at the end.
	htlcSettleInfoKey = []byte("si")

	// htlcFailInfoKey is the key used as the prefix of an HTLC attempt
	// failure information, if any.The  HTLC attempt ID is concatenated at
	// the end.
	htlcFailInfoKey = []byte("fi")

	// paymentFailInfoKey is a key used in the payment's sub-bucket to
	// store information about the reason a payment failed.
	paymentFailInfoKey = []byte("payment-fail-info")

	// paymentsIndexBucket is the name of the top-level bucket within the
	// database that stores an index of payment sequence numbers to its
	// payment hash.
	// payments-sequence-index-bucket
	// 	|--<sequence-number>: <payment hash>
	// 	|--...
	// 	|--<sequence-number>: <payment hash>
	paymentsIndexBucket = []byte("payments-index-bucket")
)

// KVStore implements persistence for payments and payment attempts.
type KVStore struct {
	// Sequence management for the kv store.
	seqMu     sync.Mutex
	currSeq   uint64
	storedSeq uint64

	// db is the underlying database implementation.
	db kvdb.Backend
}

// A compile-time constraint to ensure KVStore implements DB.
var _ DB = (*KVStore)(nil)

// NewKVStore creates a new KVStore for payments.
func NewKVStore(db kvdb.Backend,
	options ...OptionModifier) (*KVStore, error) {

	opts := DefaultOptions()
	for _, applyOption := range options {
		applyOption(opts)
	}

	if !opts.NoMigration {
		if err := initKVStore(db); err != nil {
			return nil, err
		}
	}

	return &KVStore{
		db: db,
	}, nil
}

// paymentsTopLevelBuckets is a list of top-level buckets that are used for
// the payments database when using the kv store.
var paymentsTopLevelBuckets = [][]byte{
	paymentsRootBucket,
	paymentsIndexBucket,
}

// initKVStore creates and initializes the top-level buckets for the payment db.
func initKVStore(db kvdb.Backend) error {
	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		for _, tlb := range paymentsTopLevelBuckets {
			if _, err := tx.CreateTopLevelBucket(tlb); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	if err != nil {
		return fmt.Errorf("unable to create new payments db: %w", err)
	}

	return nil
}

// InitPayment checks or records the given PaymentCreationInfo with the DB,
// making sure it does not already exist as an in-flight payment. When this
// method returns successfully, the payment is guaranteed to be in the InFlight
// state.
func (p *KVStore) InitPayment(_ context.Context, paymentHash lntypes.Hash,
	info *PaymentCreationInfo) error {

	// Obtain a new sequence number for this payment. This is used
	// to sort the payments in order of creation, and also acts as
	// a unique identifier for each payment.
	sequenceNum, err := p.nextPaymentSequence()
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := serializePaymentCreationInfo(&b, info); err != nil {
		return err
	}
	infoBytes := b.Bytes()

	var updateErr error
	err = kvdb.Batch(p.db, func(tx kvdb.RwTx) error {
		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil

		prefetchPayment(tx, paymentHash)
		bucket, err := createPaymentBucket(tx, paymentHash)
		if err != nil {
			return err
		}

		// Get the existing status of this payment, if any.
		paymentStatus, err := fetchPaymentStatus(bucket)

		switch {
		// If no error is returned, it means we already have this
		// payment. We'll check the status to decide whether we allow
		// retrying the payment or return a specific error.
		case err == nil:
			if err := paymentStatus.initializable(); err != nil {
				updateErr = err
				return nil
			}

		// Otherwise, if the error is not `ErrPaymentNotInitiated`,
		// we'll return the error.
		case !errors.Is(err, ErrPaymentNotInitiated):
			return err
		}

		// Before we set our new sequence number, we check whether this
		// payment has a previously set sequence number and remove its
		// index entry if it exists. This happens in the case where we
		// have a previously attempted payment which was left in a state
		// where we can retry.
		seqBytes := bucket.Get(paymentSequenceKey)
		if seqBytes != nil {
			indexBucket := tx.ReadWriteBucket(paymentsIndexBucket)
			if err := indexBucket.Delete(seqBytes); err != nil {
				return err
			}
		}

		// Once we have obtained a sequence number, we add an entry
		// to our index bucket which will map the sequence number to
		// our payment identifier.
		err = createPaymentIndexEntry(
			tx, sequenceNum, info.PaymentIdentifier,
		)
		if err != nil {
			return err
		}

		err = bucket.Put(paymentSequenceKey, sequenceNum)
		if err != nil {
			return err
		}

		// Add the payment info to the bucket, which contains the
		// static information for this payment
		err = bucket.Put(paymentCreationInfoKey, infoBytes)
		if err != nil {
			return err
		}

		// We'll delete any lingering HTLCs to start with, in case we
		// are initializing a payment that was attempted earlier, but
		// left in a state where we could retry.
		err = bucket.DeleteNestedBucket(paymentHtlcsBucket)
		if err != nil && !errors.Is(err, kvdb.ErrBucketNotFound) {
			return err
		}

		// Also delete any lingering failure info now that we are
		// re-attempting.
		return bucket.Delete(paymentFailInfoKey)
	})
	if err != nil {
		return fmt.Errorf("unable to init payment: %w", err)
	}

	return updateErr
}

// DeleteFailedAttempts deletes all failed htlcs for a payment.
func (p *KVStore) DeleteFailedAttempts(ctx context.Context,
	hash lntypes.Hash) error {

	const failedHtlcsOnly = true
	err := p.DeletePayment(ctx, hash, failedHtlcsOnly)
	if err != nil {
		return err
	}

	return nil
}

// paymentIndexTypeHash is a payment index type which indicates that we have
// created an index of payment sequence number to payment hash.
type paymentIndexType uint8

// paymentIndexTypeHash is a payment index type which indicates that we have
// created an index of payment sequence number to payment hash.
const paymentIndexTypeHash paymentIndexType = 0

// createPaymentIndexEntry creates a payment hash typed index for a payment. The
// index produced contains a payment index type (which can be used in future to
// signal different payment index types) and the payment identifier.
func createPaymentIndexEntry(tx kvdb.RwTx, sequenceNumber []byte,
	id lntypes.Hash) error {

	var b bytes.Buffer
	if err := WriteElements(&b, paymentIndexTypeHash, id[:]); err != nil {
		return err
	}

	indexes := tx.ReadWriteBucket(paymentsIndexBucket)

	return indexes.Put(sequenceNumber, b.Bytes())
}

// deserializePaymentIndex deserializes a payment index entry. This function
// currently only supports deserialization of payment hash indexes, and will
// fail for other types.
func deserializePaymentIndex(r io.Reader) (lntypes.Hash, error) {
	var (
		indexType   paymentIndexType
		paymentHash []byte
	)

	if err := ReadElements(r, &indexType, &paymentHash); err != nil {
		return lntypes.Hash{}, err
	}

	// While we only have on payment index type, we do not need to use our
	// index type to deserialize the index. However, we sanity check that
	// this type is as expected, since we had to read it out anyway.
	if indexType != paymentIndexTypeHash {
		return lntypes.Hash{}, fmt.Errorf("unknown payment index "+
			"type: %v", indexType)
	}

	hash, err := lntypes.MakeHash(paymentHash)
	if err != nil {
		return lntypes.Hash{}, err
	}

	return hash, nil
}

// RegisterAttempt atomically records the provided HTLCAttemptInfo to the
// DB.
func (p *KVStore) RegisterAttempt(_ context.Context, paymentHash lntypes.Hash,
	attempt *HTLCAttemptInfo) (*MPPayment, error) {

	// Serialize the information before opening the db transaction.
	var a bytes.Buffer
	err := serializeHTLCAttemptInfo(&a, attempt)
	if err != nil {
		return nil, err
	}
	htlcInfoBytes := a.Bytes()

	htlcIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(htlcIDBytes, attempt.AttemptID)

	var payment *MPPayment
	err = kvdb.Batch(p.db, func(tx kvdb.RwTx) error {
		prefetchPayment(tx, paymentHash)
		bucket, err := fetchPaymentBucketUpdate(tx, paymentHash)
		if err != nil {
			return err
		}

		payment, err = fetchPayment(bucket)
		if err != nil {
			return err
		}

		// Check if registering a new attempt is allowed.
		if err := payment.Registrable(); err != nil {
			return err
		}

		// Verify the attempt is compatible with the existing payment.
		if err := verifyAttempt(payment, attempt); err != nil {
			return err
		}

		htlcsBucket, err := bucket.CreateBucketIfNotExists(
			paymentHtlcsBucket,
		)
		if err != nil {
			return err
		}

		err = htlcsBucket.Put(
			htlcBucketKey(htlcAttemptInfoKey, htlcIDBytes),
			htlcInfoBytes,
		)
		if err != nil {
			return err
		}

		// Retrieve attempt info for the notification.
		payment, err = fetchPayment(bucket)

		return err
	})
	if err != nil {
		return nil, err
	}

	return payment, err
}

// SettleAttempt marks the given attempt settled with the preimage. If this is
// a multi shard payment, this might implicitly mean that the full payment
// succeeded.
//
// After invoking this method, InitPayment should always return an error to
// prevent us from making duplicate payments to the same payment hash. The
// provided preimage is atomically saved to the DB for record keeping.
func (p *KVStore) SettleAttempt(_ context.Context, hash lntypes.Hash,
	attemptID uint64, settleInfo *HTLCSettleInfo) (*MPPayment, error) {

	var b bytes.Buffer
	if err := serializeHTLCSettleInfo(&b, settleInfo); err != nil {
		return nil, err
	}
	settleBytes := b.Bytes()

	return p.updateHtlcKey(hash, attemptID, htlcSettleInfoKey, settleBytes)
}

// FailAttempt marks the given payment attempt failed.
func (p *KVStore) FailAttempt(_ context.Context, hash lntypes.Hash,
	attemptID uint64, failInfo *HTLCFailInfo) (*MPPayment, error) {

	var b bytes.Buffer
	if err := serializeHTLCFailInfo(&b, failInfo); err != nil {
		return nil, err
	}
	failBytes := b.Bytes()

	return p.updateHtlcKey(hash, attemptID, htlcFailInfoKey, failBytes)
}

// updateHtlcKey updates a database key for the specified htlc.
func (p *KVStore) updateHtlcKey(paymentHash lntypes.Hash,
	attemptID uint64, key, value []byte) (*MPPayment, error) {

	aid := make([]byte, 8)
	binary.BigEndian.PutUint64(aid, attemptID)

	var payment *MPPayment
	err := kvdb.Batch(p.db, func(tx kvdb.RwTx) error {
		payment = nil

		prefetchPayment(tx, paymentHash)
		bucket, err := fetchPaymentBucketUpdate(tx, paymentHash)
		if err != nil {
			return err
		}

		p, err := fetchPayment(bucket)
		if err != nil {
			return err
		}

		// We can only update keys of in-flight payments. We allow
		// updating keys even if the payment has reached a terminal
		// condition, since the HTLC outcomes must still be updated.
		if err := p.Status.updatable(); err != nil {
			return err
		}

		htlcsBucket := bucket.NestedReadWriteBucket(paymentHtlcsBucket)
		if htlcsBucket == nil {
			return fmt.Errorf("htlcs bucket not found")
		}

		attemptKey := htlcBucketKey(htlcAttemptInfoKey, aid)
		if htlcsBucket.Get(attemptKey) == nil {
			return fmt.Errorf("HTLC with ID %v not registered",
				attemptID)
		}

		// Make sure the shard is not already failed or settled.
		failKey := htlcBucketKey(htlcFailInfoKey, aid)
		if htlcsBucket.Get(failKey) != nil {
			return ErrAttemptAlreadyFailed
		}

		settleKey := htlcBucketKey(htlcSettleInfoKey, aid)
		if htlcsBucket.Get(settleKey) != nil {
			return ErrAttemptAlreadySettled
		}

		// Add or update the key for this htlc.
		err = htlcsBucket.Put(htlcBucketKey(key, aid), value)
		if err != nil {
			return err
		}

		// Retrieve attempt info for the notification.
		payment, err = fetchPayment(bucket)

		return err
	})
	if err != nil {
		return nil, err
	}

	return payment, err
}

// Fail transitions a payment into the Failed state, and records the reason the
// payment failed. After invoking this method, InitPayment should return nil on
// its next call for this payment hash, allowing the switch to make a
// subsequent payment.
func (p *KVStore) Fail(_ context.Context, paymentHash lntypes.Hash,
	reason FailureReason) (*MPPayment, error) {

	var (
		updateErr error
		payment   *MPPayment
	)
	err := kvdb.Batch(p.db, func(tx kvdb.RwTx) error {
		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil
		payment = nil

		prefetchPayment(tx, paymentHash)
		bucket, err := fetchPaymentBucketUpdate(tx, paymentHash)
		if errors.Is(err, ErrPaymentNotInitiated) {
			updateErr = ErrPaymentNotInitiated
			return nil
		} else if err != nil {
			return err
		}

		// We mark the payment as failed as long as it is known. This
		// lets the last attempt to fail with a terminal write its
		// failure to the KVStore without synchronizing with
		// other attempts.
		_, err = fetchPaymentStatus(bucket)
		if errors.Is(err, ErrPaymentNotInitiated) {
			updateErr = ErrPaymentNotInitiated
			return nil
		} else if err != nil {
			return err
		}

		// Put the failure reason in the bucket for record keeping.
		v := []byte{byte(reason)}
		err = bucket.Put(paymentFailInfoKey, v)
		if err != nil {
			return err
		}

		// Retrieve attempt info for the notification, if available.
		payment, err = fetchPayment(bucket)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return payment, updateErr
}

// FetchPayment returns information about a payment from the database.
func (p *KVStore) FetchPayment(_ context.Context,
	paymentHash lntypes.Hash) (*MPPayment, error) {

	var payment *MPPayment
	err := kvdb.View(p.db, func(tx kvdb.RTx) error {
		prefetchPayment(tx, paymentHash)
		bucket, err := fetchPaymentBucket(tx, paymentHash)
		if err != nil {
			return err
		}

		payment, err = fetchPayment(bucket)

		return err
	}, func() {
		payment = nil
	})
	if err != nil {
		return nil, err
	}

	return payment, nil
}

// prefetchPayment attempts to prefetch as much of the payment as possible to
// reduce DB roundtrips.
func prefetchPayment(tx kvdb.RTx, paymentHash lntypes.Hash) {
	rb := kvdb.RootBucket(tx)
	kvdb.Prefetch(
		rb,
		[]string{
			// Prefetch all keys in the payment's bucket.
			string(paymentsRootBucket),
			string(paymentHash[:]),
		},
		[]string{
			// Prefetch all keys in the payment's htlc bucket.
			string(paymentsRootBucket),
			string(paymentHash[:]),
			string(paymentHtlcsBucket),
		},
	)
}

// createPaymentBucket creates or fetches the sub-bucket assigned to this
// payment hash.
func createPaymentBucket(tx kvdb.RwTx, paymentHash lntypes.Hash) (
	kvdb.RwBucket, error) {

	payments, err := tx.CreateTopLevelBucket(paymentsRootBucket)
	if err != nil {
		return nil, err
	}

	return payments.CreateBucketIfNotExists(paymentHash[:])
}

// fetchPaymentBucket fetches the sub-bucket assigned to this payment hash. If
// the bucket does not exist, it returns ErrPaymentNotInitiated.
func fetchPaymentBucket(tx kvdb.RTx, paymentHash lntypes.Hash) (
	kvdb.RBucket, error) {

	payments := tx.ReadBucket(paymentsRootBucket)
	if payments == nil {
		return nil, ErrPaymentNotInitiated
	}

	bucket := payments.NestedReadBucket(paymentHash[:])
	if bucket == nil {
		return nil, ErrPaymentNotInitiated
	}

	return bucket, nil
}

// fetchPaymentBucketUpdate is identical to fetchPaymentBucket, but it returns a
// bucket that can be written to.
func fetchPaymentBucketUpdate(tx kvdb.RwTx, paymentHash lntypes.Hash) (
	kvdb.RwBucket, error) {

	payments := tx.ReadWriteBucket(paymentsRootBucket)
	if payments == nil {
		return nil, ErrPaymentNotInitiated
	}

	bucket := payments.NestedReadWriteBucket(paymentHash[:])
	if bucket == nil {
		return nil, ErrPaymentNotInitiated
	}

	return bucket, nil
}

// nextPaymentSequence returns the next sequence number to store for a new
// payment.
func (p *KVStore) nextPaymentSequence() ([]byte, error) {
	p.seqMu.Lock()
	defer p.seqMu.Unlock()

	// Set a new upper bound in the DB every 1000 payments to avoid
	// conflicts on the sequence when using etcd.
	if p.currSeq == p.storedSeq {
		var currPaymentSeq, newUpperBound uint64
		if err := kvdb.Update(p.db, func(tx kvdb.RwTx) error {
			paymentsBucket, err := tx.CreateTopLevelBucket(
				paymentsRootBucket,
			)
			if err != nil {
				return err
			}

			currPaymentSeq = paymentsBucket.Sequence()
			newUpperBound = currPaymentSeq + paymentSeqBlockSize

			return paymentsBucket.SetSequence(newUpperBound)
		}, func() {}); err != nil {
			return nil, err
		}

		// We lazy initialize the cached currPaymentSeq here using the
		// first nextPaymentSequence() call. This if statement will auto
		// initialize our stored currPaymentSeq, since by default both
		// this variable and storedPaymentSeq are zero which in turn
		// will have us fetch the current values from the DB.
		if p.currSeq == 0 {
			p.currSeq = currPaymentSeq
		}

		p.storedSeq = newUpperBound
	}

	p.currSeq++
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, p.currSeq)

	return b, nil
}

// fetchPaymentStatus fetches the payment status of the payment. If the payment
// isn't found, it will return error `ErrPaymentNotInitiated`.
func fetchPaymentStatus(bucket kvdb.RBucket) (PaymentStatus, error) {
	// Creation info should be set for all payments, regardless of state.
	// If not, it is unknown.
	if bucket.Get(paymentCreationInfoKey) == nil {
		return 0, ErrPaymentNotInitiated
	}

	payment, err := fetchPayment(bucket)
	if err != nil {
		return 0, err
	}

	return payment.Status, nil
}

// FetchInFlightPayments returns all payments with status InFlight.
func (p *KVStore) FetchInFlightPayments(_ context.Context) ([]*MPPayment,
	error) {

	var (
		inFlights      []*MPPayment
		start          = time.Now()
		lastLogTime    = time.Now()
		processedCount int
	)

	err := kvdb.View(p.db, func(tx kvdb.RTx) error {
		payments := tx.ReadBucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		return payments.ForEach(func(k, _ []byte) error {
			bucket := payments.NestedReadBucket(k)
			if bucket == nil {
				return fmt.Errorf("non bucket element")
			}

			p, err := fetchPayment(bucket)
			if err != nil {
				return err
			}

			processedCount++
			if time.Since(lastLogTime) >=
				paymentProgressLogInterval {

				log.Debugf("Scanning inflight payments "+
					"(in progress), processed %d, last "+
					"processed payment: %v", processedCount,
					p.Info)

				lastLogTime = time.Now()
			}

			// Skip the payment if it's terminated.
			if p.Terminated() {
				return nil
			}

			inFlights = append(inFlights, p)

			return nil
		})
	}, func() {
		inFlights = nil
	})
	if err != nil {
		return nil, err
	}

	elapsed := time.Since(start)
	log.Debugf("Completed scanning for inflight payments: "+
		"total_processed=%d, found_inflight=%d, elapsed=%v",
		processedCount, len(inFlights),
		elapsed.Round(time.Millisecond))

	return inFlights, nil
}

// htlcBucketKey creates a composite key from prefix and id where the result is
// simply the two concatenated.
func htlcBucketKey(prefix, id []byte) []byte {
	key := make([]byte, len(prefix)+len(id))
	copy(key, prefix)
	copy(key[len(prefix):], id)

	return key
}

// FetchPayments returns all sent payments found in the DB.
func (p *KVStore) FetchPayments() ([]*MPPayment, error) {
	var payments []*MPPayment

	err := kvdb.View(p.db, func(tx kvdb.RTx) error {
		paymentsBucket := tx.ReadBucket(paymentsRootBucket)
		if paymentsBucket == nil {
			return nil
		}

		return paymentsBucket.ForEach(func(k, v []byte) error {
			bucket := paymentsBucket.NestedReadBucket(k)
			if bucket == nil {
				// We only expect sub-buckets to be found in
				// this top-level bucket.
				return fmt.Errorf("non bucket element in " +
					"payments bucket")
			}

			p, err := fetchPayment(bucket)
			if err != nil {
				return err
			}

			payments = append(payments, p)

			// For older versions of lnd, duplicate payments to a
			// payment has was possible. These will be found in a
			// sub-bucket indexed by their sequence number if
			// available.
			duplicatePayments, err := fetchDuplicatePayments(bucket)
			if err != nil {
				return err
			}

			payments = append(payments, duplicatePayments...)

			return nil
		})
	}, func() {
		payments = nil
	})
	if err != nil {
		return nil, err
	}

	// Before returning, sort the payments by their sequence number.
	sort.Slice(payments, func(i, j int) bool {
		return payments[i].SequenceNum < payments[j].SequenceNum
	})

	return payments, nil
}

func fetchCreationInfo(bucket kvdb.RBucket) (*PaymentCreationInfo, error) {
	b := bucket.Get(paymentCreationInfoKey)
	if b == nil {
		return nil, fmt.Errorf("creation info not found")
	}

	r := bytes.NewReader(b)

	return deserializePaymentCreationInfo(r)
}

func fetchPayment(bucket kvdb.RBucket) (*MPPayment, error) {
	seqBytes := bucket.Get(paymentSequenceKey)
	if seqBytes == nil {
		return nil, fmt.Errorf("sequence number not found")
	}

	sequenceNum := binary.BigEndian.Uint64(seqBytes)

	// Get the PaymentCreationInfo.
	creationInfo, err := fetchCreationInfo(bucket)
	if err != nil {
		return nil, err
	}

	var htlcs []HTLCAttempt
	htlcsBucket := bucket.NestedReadBucket(paymentHtlcsBucket)
	if htlcsBucket != nil {
		// Get the payment attempts. This can be empty.
		htlcs, err = fetchHtlcAttempts(htlcsBucket)
		if err != nil {
			return nil, err
		}
	}

	// Get failure reason if available.
	var failureReason *FailureReason
	b := bucket.Get(paymentFailInfoKey)
	if b != nil {
		reason := FailureReason(b[0])
		failureReason = &reason
	}

	// Create a new payment.
	payment := &MPPayment{
		SequenceNum:   sequenceNum,
		Info:          creationInfo,
		HTLCs:         htlcs,
		FailureReason: failureReason,
	}

	// Set its state and status.
	if err := payment.setState(); err != nil {
		return nil, err
	}

	return payment, nil
}

// fetchHtlcAttempts retrieves all htlc attempts made for the payment found in
// the given bucket.
func fetchHtlcAttempts(bucket kvdb.RBucket) ([]HTLCAttempt, error) {
	htlcsMap := make(map[uint64]*HTLCAttempt)

	attemptInfoCount := 0
	err := bucket.ForEach(func(k, v []byte) error {
		aid := byteOrder.Uint64(k[len(k)-8:])

		if _, ok := htlcsMap[aid]; !ok {
			htlcsMap[aid] = &HTLCAttempt{}
		}

		var err error
		switch {
		case bytes.HasPrefix(k, htlcAttemptInfoKey):
			attemptInfo, err := readHtlcAttemptInfo(v)
			if err != nil {
				return err
			}

			attemptInfo.AttemptID = aid
			htlcsMap[aid].HTLCAttemptInfo = *attemptInfo
			attemptInfoCount++

		case bytes.HasPrefix(k, htlcSettleInfoKey):
			htlcsMap[aid].Settle, err = readHtlcSettleInfo(v)
			if err != nil {
				return err
			}

		case bytes.HasPrefix(k, htlcFailInfoKey):
			htlcsMap[aid].Failure, err = readHtlcFailInfo(v)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("unknown htlc attempt key")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sanity check that all htlcs have an attempt info.
	if attemptInfoCount != len(htlcsMap) {
		return nil, ErrNoAttemptInfo
	}

	keys := make([]uint64, len(htlcsMap))
	i := 0
	for k := range htlcsMap {
		keys[i] = k
		i++
	}

	// Sort HTLC attempts by their attempt ID. This is needed because in the
	// DB we store the attempts with keys prefixed by their status which
	// changes order (groups them together by status).
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})

	htlcs := make([]HTLCAttempt, len(htlcsMap))
	for i, key := range keys {
		htlcs[i] = *htlcsMap[key]
	}

	return htlcs, nil
}

// readHtlcAttemptInfo reads the payment attempt info for this htlc.
func readHtlcAttemptInfo(b []byte) (*HTLCAttemptInfo, error) {
	r := bytes.NewReader(b)
	return deserializeHTLCAttemptInfo(r)
}

// readHtlcSettleInfo reads the settle info for the htlc. If the htlc isn't
// settled, nil is returned.
func readHtlcSettleInfo(b []byte) (*HTLCSettleInfo, error) {
	r := bytes.NewReader(b)
	return deserializeHTLCSettleInfo(r)
}

// readHtlcFailInfo reads the failure info for the htlc. If the htlc hasn't
// failed, nil is returned.
func readHtlcFailInfo(b []byte) (*HTLCFailInfo, error) {
	r := bytes.NewReader(b)
	return deserializeHTLCFailInfo(r)
}

// fetchFailedHtlcKeys retrieves the bucket keys of all failed HTLCs of a
// payment bucket.
func fetchFailedHtlcKeys(bucket kvdb.RBucket) ([][]byte, error) {
	htlcsBucket := bucket.NestedReadBucket(paymentHtlcsBucket)

	var htlcs []HTLCAttempt
	var err error
	if htlcsBucket != nil {
		htlcs, err = fetchHtlcAttempts(htlcsBucket)
		if err != nil {
			return nil, err
		}
	}

	// Now iterate though them and save the bucket keys for the failed
	// HTLCs.
	var htlcKeys [][]byte
	for _, h := range htlcs {
		if h.Failure == nil {
			continue
		}

		htlcKeyBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(htlcKeyBytes, h.AttemptID)

		htlcKeys = append(htlcKeys, htlcKeyBytes)
	}

	return htlcKeys, nil
}

// QueryPayments is a query to the payments database which is restricted
// to a subset of payments by the payments query, containing an offset
// index and a maximum number of returned payments.
func (p *KVStore) QueryPayments(_ context.Context,
	query Query) (Response, error) {

	var resp Response

	if err := kvdb.View(p.db, func(tx kvdb.RTx) error {
		// Get the root payments bucket.
		paymentsBucket := tx.ReadBucket(paymentsRootBucket)
		if paymentsBucket == nil {
			return nil
		}

		// Get the index bucket which maps sequence number -> payment
		// hash and duplicate bool. If we have a payments bucket, we
		// should have an indexes bucket as well.
		indexes := tx.ReadBucket(paymentsIndexBucket)
		if indexes == nil {
			return fmt.Errorf("index bucket does not exist")
		}

		// accumulatePayments gets payments with the sequence number
		// and hash provided and adds them to our list of payments if
		// they meet the criteria of our query. It returns the number
		// of payments that were added.
		accumulatePayments := func(sequenceKey, hash []byte) (bool,
			error) {

			r := bytes.NewReader(hash)
			paymentHash, err := deserializePaymentIndex(r)
			if err != nil {
				return false, err
			}

			payment, err := fetchPaymentWithSequenceNumber(
				tx, paymentHash, sequenceKey,
			)
			if err != nil {
				return false, err
			}

			// To keep compatibility with the old API, we only
			// return non-succeeded payments if requested.
			if payment.Status != StatusSucceeded &&
				!query.IncludeIncomplete {

				return false, err
			}

			// Get the creation time in Unix seconds, this always
			// rounds down the nanoseconds to full seconds.
			createTime := payment.Info.CreationTime.Unix()

			// Skip any payments that were created before the
			// specified time.
			if createTime < query.CreationDateStart {
				return false, nil
			}

			// Skip any payments that were created after the
			// specified time.
			if query.CreationDateEnd != 0 &&
				createTime > query.CreationDateEnd {

				return false, nil
			}

			// At this point, we've exhausted the offset, so we'll
			// begin collecting invoices found within the range.
			resp.Payments = append(resp.Payments, payment)

			return true, nil
		}

		// Create a paginator which reads from our sequence index bucket
		// with the parameters provided by the payments query.
		paginator := channeldb.NewPaginator(
			indexes.ReadCursor(), query.Reversed, query.IndexOffset,
			query.MaxPayments,
		)

		// Run a paginated query, adding payments to our response.
		if err := paginator.Query(accumulatePayments); err != nil {
			return err
		}

		// Counting the total number of payments is expensive, since we
		// literally have to traverse the cursor linearly, which can
		// take quite a while. So it's an optional query parameter.
		if query.CountTotal {
			var (
				totalPayments uint64
				err           error
			)
			countFn := func(_, _ []byte) error {
				totalPayments++

				return nil
			}

			// In non-boltdb database backends, there's a faster
			// ForAll query that allows for batch fetching items.
			fastBucket, ok := indexes.(kvdb.ExtendedRBucket)
			if ok {
				err = fastBucket.ForAll(countFn)
			} else {
				err = indexes.ForEach(countFn)
			}
			if err != nil {
				return fmt.Errorf("error counting payments: %w",
					err)
			}

			resp.TotalCount = totalPayments
		}

		return nil
	}, func() {
		resp = Response{}
	}); err != nil {
		return resp, err
	}

	// Need to swap the payments slice order if reversed order.
	if query.Reversed {
		for l, r := 0, len(resp.Payments)-1; l < r; l, r = l+1, r-1 {
			resp.Payments[l], resp.Payments[r] =
				resp.Payments[r], resp.Payments[l]
		}
	}

	// Set the first and last index of the returned payments so that the
	// caller can resume from this point later on.
	if len(resp.Payments) > 0 {
		resp.FirstIndexOffset = resp.Payments[0].SequenceNum
		resp.LastIndexOffset =
			resp.Payments[len(resp.Payments)-1].SequenceNum
	}

	return resp, nil
}

// fetchPaymentWithSequenceNumber get the payment which matches the payment hash
// *and* sequence number provided from the database. This is required because
// we previously had more than one payment per hash, so we have multiple indexes
// pointing to a single payment; we want to retrieve the correct one.
func fetchPaymentWithSequenceNumber(tx kvdb.RTx, paymentHash lntypes.Hash,
	sequenceNumber []byte) (*MPPayment, error) {

	// We can now lookup the payment keyed by its hash in
	// the payments root bucket.
	bucket, err := fetchPaymentBucket(tx, paymentHash)
	if err != nil {
		return nil, err
	}

	// A single payment hash can have multiple payments associated with it.
	// We lookup our sequence number first, to determine whether this is
	// the payment we are actually looking for.
	seqBytes := bucket.Get(paymentSequenceKey)
	if seqBytes == nil {
		return nil, ErrNoSequenceNumber
	}

	// If this top level payment has the sequence number we are looking for,
	// return it.
	if bytes.Equal(seqBytes, sequenceNumber) {
		return fetchPayment(bucket)
	}

	// If we were not looking for the top level payment, we are looking for
	// one of our duplicate payments. We need to iterate through the seq
	// numbers in this bucket to find the correct payments. If we do not
	// find a duplicate payments bucket here, something is wrong.
	dup := bucket.NestedReadBucket(duplicatePaymentsBucket)
	if dup == nil {
		return nil, ErrNoDuplicateBucket
	}

	var duplicatePayment *MPPayment
	err = dup.ForEach(func(k, v []byte) error {
		subBucket := dup.NestedReadBucket(k)
		if subBucket == nil {
			// We one bucket for each duplicate to be found.
			return ErrNoDuplicateNestedBucket
		}

		seqBytes := subBucket.Get(duplicatePaymentSequenceKey)
		if seqBytes == nil {
			return err
		}

		// If this duplicate payment is not the sequence number we are
		// looking for, we can continue.
		if !bytes.Equal(seqBytes, sequenceNumber) {
			return nil
		}

		duplicatePayment, err = fetchDuplicatePayment(subBucket)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// If none of the duplicate payments matched our sequence number, we
	// failed to find the payment with this sequence number; something is
	// wrong.
	if duplicatePayment == nil {
		return nil, ErrDuplicateNotFound
	}

	return duplicatePayment, nil
}

// DeletePayment deletes a payment from the DB given its payment hash. If
// failedHtlcsOnly is set, only failed HTLC attempts of the payment will be
// deleted.
func (p *KVStore) DeletePayment(_ context.Context, paymentHash lntypes.Hash,
	failedHtlcsOnly bool) error {

	return kvdb.Update(p.db, func(tx kvdb.RwTx) error {
		payments := tx.ReadWriteBucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		bucket := payments.NestedReadWriteBucket(paymentHash[:])
		if bucket == nil {
			return fmt.Errorf("non bucket element in payments " +
				"bucket")
		}

		// If the status is InFlight, we cannot safely delete
		// the payment information, so we return early.
		paymentStatus, err := fetchPaymentStatus(bucket)
		if err != nil {
			return err
		}

		// If the payment has inflight HTLCs, we cannot safely delete
		// the payment information, so we return an error.
		if err := paymentStatus.removable(); err != nil {
			return fmt.Errorf("payment '%v' has inflight HTLCs"+
				"and therefore cannot be deleted: %w",
				paymentHash.String(), err)
		}

		// Delete the failed HTLC attempts we found.
		if failedHtlcsOnly {
			toDelete, err := fetchFailedHtlcKeys(bucket)
			if err != nil {
				return err
			}

			htlcsBucket := bucket.NestedReadWriteBucket(
				paymentHtlcsBucket,
			)

			for _, htlcID := range toDelete {
				err = htlcsBucket.Delete(
					htlcBucketKey(
						htlcAttemptInfoKey, htlcID,
					),
				)
				if err != nil {
					return err
				}

				err = htlcsBucket.Delete(
					htlcBucketKey(htlcFailInfoKey, htlcID),
				)
				if err != nil {
					return err
				}

				err = htlcsBucket.Delete(
					htlcBucketKey(
						htlcSettleInfoKey, htlcID,
					),
				)
				if err != nil {
					return err
				}
			}

			return nil
		}

		seqNrs, err := fetchSequenceNumbers(bucket)
		if err != nil {
			return err
		}

		err = payments.DeleteNestedBucket(paymentHash[:])
		if err != nil {
			return err
		}

		indexBucket := tx.ReadWriteBucket(paymentsIndexBucket)
		for _, k := range seqNrs {
			if err := indexBucket.Delete(k); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// DeletePayments deletes all completed and failed payments from the DB. If
// failedOnly is set, only failed payments will be considered for deletion. If
// failedHtlcsOnly is set, the payment itself won't be deleted, only failed HTLC
// attempts. The method returns the number of deleted payments, which is always
// 0 if failedHtlcsOnly is set.
func (p *KVStore) DeletePayments(_ context.Context, failedOnly,
	failedHtlcsOnly bool) (int, error) {

	var numPayments int
	err := kvdb.Update(p.db, func(tx kvdb.RwTx) error {
		payments := tx.ReadWriteBucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		var (
			// deleteBuckets is the set of payment buckets we need
			// to delete.
			deleteBuckets [][]byte

			// deleteIndexes is the set of indexes pointing to these
			// payments that need to be deleted.
			deleteIndexes [][]byte

			// deleteHtlcs maps a payment hash to the HTLC IDs we
			// want to delete for that payment.
			deleteHtlcs = make(map[lntypes.Hash][][]byte)
		)
		err := payments.ForEach(func(k, _ []byte) error {
			bucket := payments.NestedReadBucket(k)
			if bucket == nil {
				// We only expect sub-buckets to be found in
				// this top-level bucket.
				return fmt.Errorf("non bucket element in " +
					"payments bucket")
			}

			// If the status is InFlight, we cannot safely delete
			// the payment information, so we return early.
			paymentStatus, err := fetchPaymentStatus(bucket)
			if err != nil {
				return err
			}

			// If the payment has inflight HTLCs, we cannot safely
			// delete the payment information, so we return an nil
			// to skip it.
			if err := paymentStatus.removable(); err != nil {
				return nil
			}

			// If we requested to only delete failed payments, we
			// can return if this one is not.
			if failedOnly && paymentStatus != StatusFailed {
				return nil
			}

			// If we are only deleting failed HTLCs, fetch them.
			if failedHtlcsOnly {
				toDelete, err := fetchFailedHtlcKeys(bucket)
				if err != nil {
					return err
				}

				hash, err := lntypes.MakeHash(k)
				if err != nil {
					return err
				}

				deleteHtlcs[hash] = toDelete

				// We return, we are only deleting attempts.
				return nil
			}

			// Add the bucket to the set of buckets we can delete.
			deleteBuckets = append(deleteBuckets, k)

			// Get all the sequence number associated with the
			// payment, including duplicates.
			seqNrs, err := fetchSequenceNumbers(bucket)
			if err != nil {
				return err
			}

			deleteIndexes = append(deleteIndexes, seqNrs...)
			numPayments++

			return nil
		})
		if err != nil {
			return err
		}

		// Delete the failed HTLC attempts we found.
		for hash, htlcIDs := range deleteHtlcs {
			bucket := payments.NestedReadWriteBucket(hash[:])
			htlcsBucket := bucket.NestedReadWriteBucket(
				paymentHtlcsBucket,
			)

			for _, aid := range htlcIDs {
				if err := htlcsBucket.Delete(
					htlcBucketKey(htlcAttemptInfoKey, aid),
				); err != nil {
					return err
				}

				if err := htlcsBucket.Delete(
					htlcBucketKey(htlcFailInfoKey, aid),
				); err != nil {
					return err
				}

				if err := htlcsBucket.Delete(
					htlcBucketKey(htlcSettleInfoKey, aid),
				); err != nil {
					return err
				}
			}
		}

		for _, k := range deleteBuckets {
			if err := payments.DeleteNestedBucket(k); err != nil {
				return err
			}
		}

		// Get our index bucket and delete all indexes pointing to the
		// payments we are deleting.
		indexBucket := tx.ReadWriteBucket(paymentsIndexBucket)
		for _, k := range deleteIndexes {
			if err := indexBucket.Delete(k); err != nil {
				return err
			}
		}

		return nil
	}, func() {
		numPayments = 0
	})
	if err != nil {
		return 0, err
	}

	return numPayments, nil
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

func serializePaymentCreationInfo(w io.Writer, c *PaymentCreationInfo) error {
	var scratch [8]byte

	if _, err := w.Write(c.PaymentIdentifier[:]); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(c.Value))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := serializeTime(w, c.CreationTime); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], uint32(len(c.PaymentRequest)))
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	if _, err := w.Write(c.PaymentRequest); err != nil {
		return err
	}

	// Any remaining bytes are TLV encoded records. Currently, these are
	// only the custom records provided by the user to be sent to the first
	// hop. But this can easily be extended with further records by merging
	// the records into a single TLV stream.
	err := c.FirstHopCustomRecords.SerializeTo(w)
	if err != nil {
		return err
	}

	return nil
}

func deserializePaymentCreationInfo(r io.Reader) (*PaymentCreationInfo,
	error) {

	var scratch [8]byte

	c := &PaymentCreationInfo{}

	if _, err := io.ReadFull(r, c.PaymentIdentifier[:]); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return nil, err
	}
	c.Value = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	creationTime, err := deserializeTime(r)
	if err != nil {
		return nil, err
	}
	c.CreationTime = creationTime

	if _, err := io.ReadFull(r, scratch[:4]); err != nil {
		return nil, err
	}

	reqLen := byteOrder.Uint32(scratch[:4])
	payReq := make([]byte, reqLen)
	if reqLen > 0 {
		if _, err := io.ReadFull(r, payReq); err != nil {
			return nil, err
		}
	}
	c.PaymentRequest = payReq

	// Any remaining bytes are TLV encoded records. Currently, these are
	// only the custom records provided by the user to be sent to the first
	// hop. But this can easily be extended with further records by merging
	// the records into a single TLV stream.
	c.FirstHopCustomRecords, err = lnwire.ParseCustomRecordsFrom(r)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func serializeHTLCAttemptInfo(w io.Writer, a *HTLCAttemptInfo) error {
	if err := WriteElements(w, a.sessionKey); err != nil {
		return err
	}

	if err := SerializeRoute(w, a.Route); err != nil {
		return err
	}

	if err := serializeTime(w, a.AttemptTime); err != nil {
		return err
	}

	// If the hash is nil we can just return.
	if a.Hash == nil {
		return nil
	}

	if _, err := w.Write(a.Hash[:]); err != nil {
		return err
	}

	// Merge the fixed/known records together with the custom records to
	// serialize them as a single blob. We can't do this in SerializeRoute
	// because we're in the middle of the byte stream there. We can only do
	// TLV serialization at the end of the stream, since EOF is allowed for
	// a stream if no more data is expected.
	producers := []tlv.RecordProducer{
		&a.Route.FirstHopAmount,
	}
	tlvData, err := lnwire.MergeAndEncode(
		producers, nil, a.Route.FirstHopWireCustomRecords,
	)
	if err != nil {
		return err
	}

	if _, err := w.Write(tlvData); err != nil {
		return err
	}

	return nil
}

func deserializeHTLCAttemptInfo(r io.Reader) (*HTLCAttemptInfo, error) {
	a := &HTLCAttemptInfo{}
	err := ReadElements(r, &a.sessionKey)
	if err != nil {
		return nil, err
	}

	a.Route, err = DeserializeRoute(r)
	if err != nil {
		return nil, err
	}

	a.AttemptTime, err = deserializeTime(r)
	if err != nil {
		return nil, err
	}

	hash := lntypes.Hash{}
	_, err = io.ReadFull(r, hash[:])

	switch {
	// Older payment attempts wouldn't have the hash set, in which case we
	// can just return.
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return a, nil

	case err != nil:
		return nil, err

	default:
	}

	a.Hash = &hash

	// Read any remaining data (if any) and parse it into the known records
	// and custom records.
	extraData, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	customRecords, _, _, err := lnwire.ParseAndExtractCustomRecords(
		extraData, &a.Route.FirstHopAmount,
	)
	if err != nil {
		return nil, err
	}

	a.Route.FirstHopWireCustomRecords = customRecords

	return a, nil
}

func serializeHop(w io.Writer, h *route.Hop) error {
	if err := WriteElements(w,
		h.PubKeyBytes[:],
		h.ChannelID,
		h.OutgoingTimeLock,
		h.AmtToForward,
	); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, h.LegacyPayload); err != nil {
		return err
	}

	// For legacy payloads, we don't need to write any TLV records, so
	// we'll write a zero indicating the our serialized TLV map has no
	// records.
	if h.LegacyPayload {
		return WriteElements(w, uint32(0))
	}

	// Gather all non-primitive TLV records so that they can be serialized
	// as a single blob.
	//
	// TODO(conner): add migration to unify all fields in a single TLV
	// blobs. The split approach will cause headaches down the road as more
	// fields are added, which we can avoid by having a single TLV stream
	// for all payload fields.
	var records []tlv.Record
	if h.MPP != nil {
		records = append(records, h.MPP.Record())
	}

	// Add blinding point and encrypted data if present.
	if h.EncryptedData != nil {
		records = append(records, record.NewEncryptedDataRecord(
			&h.EncryptedData,
		))
	}

	if h.BlindingPoint != nil {
		records = append(records, record.NewBlindingPointRecord(
			&h.BlindingPoint,
		))
	}

	if h.AMP != nil {
		records = append(records, h.AMP.Record())
	}

	if h.Metadata != nil {
		records = append(records, record.NewMetadataRecord(&h.Metadata))
	}

	if h.TotalAmtMsat != 0 {
		totalMsatInt := uint64(h.TotalAmtMsat)
		records = append(
			records, record.NewTotalAmtMsatBlinded(&totalMsatInt),
		)
	}

	// Final sanity check to absolutely rule out custom records that are not
	// custom and write into the standard range.
	if err := h.CustomRecords.Validate(); err != nil {
		return err
	}

	// Convert custom records to tlv and add to the record list.
	// MapToRecords sorts the list, so adding it here will keep the list
	// canonical.
	tlvRecords := tlv.MapToRecords(h.CustomRecords)
	records = append(records, tlvRecords...)

	// Otherwise, we'll transform our slice of records into a map of the
	// raw bytes, then serialize them in-line with a length (number of
	// elements) prefix.
	mapRecords, err := tlv.RecordsToMap(records)
	if err != nil {
		return err
	}

	numRecords := uint32(len(mapRecords))
	if err := WriteElements(w, numRecords); err != nil {
		return err
	}

	for recordType, rawBytes := range mapRecords {
		if err := WriteElements(w, recordType); err != nil {
			return err
		}

		if err := wire.WriteVarBytes(w, 0, rawBytes); err != nil {
			return err
		}
	}

	return nil
}

// maxOnionPayloadSize is the largest Sphinx payload possible, so we don't need
// to read/write a TLV stream larger than this.
const maxOnionPayloadSize = 1300

func deserializeHop(r io.Reader) (*route.Hop, error) {
	h := &route.Hop{}

	var pub []byte
	if err := ReadElements(r, &pub); err != nil {
		return nil, err
	}
	copy(h.PubKeyBytes[:], pub)

	if err := ReadElements(r,
		&h.ChannelID, &h.OutgoingTimeLock, &h.AmtToForward,
	); err != nil {
		return nil, err
	}

	// TODO(roasbeef): change field to allow LegacyPayload false to be the
	// legacy default?
	err := binary.Read(r, byteOrder, &h.LegacyPayload)
	if err != nil {
		return nil, err
	}

	var numElements uint32
	if err := ReadElements(r, &numElements); err != nil {
		return nil, err
	}

	// If there're no elements, then we can return early.
	if numElements == 0 {
		return h, nil
	}

	tlvMap := make(map[uint64][]byte)
	for i := uint32(0); i < numElements; i++ {
		var tlvType uint64
		if err := ReadElements(r, &tlvType); err != nil {
			return nil, err
		}

		rawRecordBytes, err := wire.ReadVarBytes(
			r, 0, maxOnionPayloadSize, "tlv",
		)
		if err != nil {
			return nil, err
		}

		tlvMap[tlvType] = rawRecordBytes
	}

	// If the MPP type is present, remove it from the generic TLV map and
	// parse it back into a proper MPP struct.
	//
	// TODO(conner): add migration to unify all fields in a single TLV
	// blobs. The split approach will cause headaches down the road as more
	// fields are added, which we can avoid by having a single TLV stream
	// for all payload fields.
	mppType := uint64(record.MPPOnionType)
	if mppBytes, ok := tlvMap[mppType]; ok {
		delete(tlvMap, mppType)

		var (
			mpp    = &record.MPP{}
			mppRec = mpp.Record()
			r      = bytes.NewReader(mppBytes)
		)
		err := mppRec.Decode(r, uint64(len(mppBytes)))
		if err != nil {
			return nil, err
		}
		h.MPP = mpp
	}

	// If encrypted data or blinding key are present, remove them from
	// the TLV map and parse into proper types.
	encryptedDataType := uint64(record.EncryptedDataOnionType)
	if data, ok := tlvMap[encryptedDataType]; ok {
		delete(tlvMap, encryptedDataType)
		h.EncryptedData = data
	}

	blindingType := uint64(record.BlindingPointOnionType)
	if blindingPoint, ok := tlvMap[blindingType]; ok {
		delete(tlvMap, blindingType)

		h.BlindingPoint, err = btcec.ParsePubKey(blindingPoint)
		if err != nil {
			return nil, fmt.Errorf("invalid blinding point: %w",
				err)
		}
	}

	ampType := uint64(record.AMPOnionType)
	if ampBytes, ok := tlvMap[ampType]; ok {
		delete(tlvMap, ampType)

		var (
			amp    = &record.AMP{}
			ampRec = amp.Record()
			r      = bytes.NewReader(ampBytes)
		)
		err := ampRec.Decode(r, uint64(len(ampBytes)))
		if err != nil {
			return nil, err
		}
		h.AMP = amp
	}

	// If the metadata type is present, remove it from the tlv map and
	// populate directly on the hop.
	metadataType := uint64(record.MetadataOnionType)
	if metadata, ok := tlvMap[metadataType]; ok {
		delete(tlvMap, metadataType)

		h.Metadata = metadata
	}

	totalAmtMsatType := uint64(record.TotalAmtMsatBlindedType)
	if totalAmtMsat, ok := tlvMap[totalAmtMsatType]; ok {
		delete(tlvMap, totalAmtMsatType)

		var (
			totalAmtMsatInt uint64
			buf             [8]byte
		)
		if err := tlv.DTUint64(
			bytes.NewReader(totalAmtMsat),
			&totalAmtMsatInt,
			&buf,
			uint64(len(totalAmtMsat)),
		); err != nil {
			return nil, err
		}

		h.TotalAmtMsat = lnwire.MilliSatoshi(totalAmtMsatInt)
	}

	h.CustomRecords = tlvMap

	return h, nil
}

// SerializeRoute serializes a route.
func SerializeRoute(w io.Writer, r route.Route) error {
	if err := WriteElements(w,
		r.TotalTimeLock, r.TotalAmount, r.SourcePubKey[:],
	); err != nil {
		return err
	}

	if err := WriteElements(w, uint32(len(r.Hops))); err != nil {
		return err
	}

	for _, h := range r.Hops {
		if err := serializeHop(w, h); err != nil {
			return err
		}
	}

	// Any new/extra TLV data is encoded in serializeHTLCAttemptInfo!

	return nil
}

// DeserializeRoute deserializes a route.
func DeserializeRoute(r io.Reader) (route.Route, error) {
	rt := route.Route{}
	if err := ReadElements(r,
		&rt.TotalTimeLock, &rt.TotalAmount,
	); err != nil {
		return rt, err
	}

	var pub []byte
	if err := ReadElements(r, &pub); err != nil {
		return rt, err
	}
	copy(rt.SourcePubKey[:], pub)

	var numHops uint32
	if err := ReadElements(r, &numHops); err != nil {
		return rt, err
	}

	var hops []*route.Hop
	for i := uint32(0); i < numHops; i++ {
		hop, err := deserializeHop(r)
		if err != nil {
			return rt, err
		}
		hops = append(hops, hop)
	}
	rt.Hops = hops

	// Any new/extra TLV data is decoded in deserializeHTLCAttemptInfo!

	return rt, nil
}

// serializeHTLCSettleInfo serializes the details of a settled htlc.
func serializeHTLCSettleInfo(w io.Writer, s *HTLCSettleInfo) error {
	if _, err := w.Write(s.Preimage[:]); err != nil {
		return err
	}

	if err := serializeTime(w, s.SettleTime); err != nil {
		return err
	}

	return nil
}

// deserializeHTLCSettleInfo deserializes the details of a settled htlc.
func deserializeHTLCSettleInfo(r io.Reader) (*HTLCSettleInfo, error) {
	s := &HTLCSettleInfo{}
	if _, err := io.ReadFull(r, s.Preimage[:]); err != nil {
		return nil, err
	}

	var err error
	s.SettleTime, err = deserializeTime(r)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// serializeHTLCFailInfo serializes the details of a failed htlc including the
// wire failure.
func serializeHTLCFailInfo(w io.Writer, f *HTLCFailInfo) error {
	if err := serializeTime(w, f.FailTime); err != nil {
		return err
	}

	// Write failure. If there is no failure message, write an empty
	// byte slice.
	var messageBytes bytes.Buffer
	if f.Message != nil {
		err := lnwire.EncodeFailureMessage(&messageBytes, f.Message, 0)
		if err != nil {
			return err
		}
	}
	if err := wire.WriteVarBytes(w, 0, messageBytes.Bytes()); err != nil {
		return err
	}

	return WriteElements(w, byte(f.Reason), f.FailureSourceIndex)
}

// deserializeHTLCFailInfo deserializes the details of a failed htlc including
// the wire failure.
func deserializeHTLCFailInfo(r io.Reader) (*HTLCFailInfo, error) {
	f := &HTLCFailInfo{}
	var err error
	f.FailTime, err = deserializeTime(r)
	if err != nil {
		return nil, err
	}

	// Read failure.
	failureBytes, err := wire.ReadVarBytes(
		r, 0, math.MaxUint16, "failure",
	)
	if err != nil {
		return nil, err
	}
	if len(failureBytes) > 0 {
		f.Message, err = lnwire.DecodeFailureMessage(
			bytes.NewReader(failureBytes), 0,
		)
		if err != nil &&
			!errors.Is(err, lnwire.ErrParsingExtraTLVBytes) {

			return nil, err
		}

		// In case we have an invalid TLV stream regarding the extra
		// tlv data we still continue with the decoding of the
		// HTLCFailInfo.
		if errors.Is(err, lnwire.ErrParsingExtraTLVBytes) {
			log.Warnf("Failed to decode extra TLV bytes for "+
				"failure message: %v", err)
		}
	}

	var reason byte
	err = ReadElements(r, &reason, &f.FailureSourceIndex)
	if err != nil {
		return nil, err
	}
	f.Reason = HTLCFailReason(reason)

	return f, nil
}

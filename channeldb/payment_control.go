package channeldb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	pymtpkg "github.com/lightningnetwork/lnd/payments"
)

const (
	// paymentSeqBlockSize is the block size used when we batch allocate
	// payment sequences for future payments.
	paymentSeqBlockSize = 1000

	// paymentProgressLogInterval is the interval we use limiting the
	// logging output of payment processing.
	paymentProgressLogInterval = 30 * time.Second
)

// KVPaymentDB implements persistence for payments and payment attempts.
type KVPaymentDB struct {
	paymentSeqMx     sync.Mutex
	currPaymentSeq   uint64
	storedPaymentSeq uint64

	// Move the methods which touch related to payment into this struct.
	// QueryPayments, DeletePayments, DeletePayment.
	db *DB
}

// NewKVPaymentDB creates a new instance of KVPaymentDB.
func NewKVPaymentDB(db *DB) *KVPaymentDB {
	return &KVPaymentDB{
		db: db,
	}
}

// InitPayment checks or records the given PaymentCreationInfo with the DB,
// making sure it does not already exist as an in-flight payment. When this
// method returns successfully, the payment is guaranteed to be in the InFlight
// state.
func (p *KVPaymentDB) InitPayment(paymentHash lntypes.Hash,
	info *pymtpkg.PaymentCreationInfo) error {

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
	err = kvdb.Batch(p.db.Backend, func(tx kvdb.RwTx) error {
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
			if err := paymentStatus.Initializable(); err != nil {
				updateErr = err
				return nil
			}

		// Otherwise, if the error is not `ErrPaymentNotInitiated`,
		// we'll return the error.
		case !errors.Is(err, pymtpkg.ErrPaymentNotInitiated):
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
		if err != nil && err != kvdb.ErrBucketNotFound {
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

// DeleteFailedAttempts deletes all failed htlcs for a payment if configured
// by the PaymentControl db.
func (p *KVPaymentDB) DeleteFailedAttempts(hash lntypes.Hash) error {
	if !p.db.keepFailedPaymentAttempts {
		const failedHtlcsOnly = true
		err := p.db.DeletePayment(hash, failedHtlcsOnly)
		if err != nil {
			return err
		}
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
func (p *KVPaymentDB) RegisterAttempt(paymentHash lntypes.Hash,
	attempt *pymtpkg.HTLCAttemptInfo) (*pymtpkg.MPPayment, error) {

	// Serialize the information before opening the db transaction.
	var a bytes.Buffer
	err := serializeHTLCAttemptInfo(&a, attempt)
	if err != nil {
		return nil, err
	}
	htlcInfoBytes := a.Bytes()

	htlcIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(htlcIDBytes, attempt.AttemptID)

	var payment *pymtpkg.MPPayment
	err = kvdb.Batch(p.db.Backend, func(tx kvdb.RwTx) error {
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

		// If the final hop has encrypted data, then we know this is a
		// blinded payment. In blinded payments, MPP records are not set
		// for split payments and the recipient is responsible for using
		// a consistent PathID across the various encrypted data
		// payloads that we received from them for this payment. All we
		// need to check is that the total amount field for each HTLC
		// in the split payment is correct.
		isBlinded := len(attempt.Route.FinalHop().EncryptedData) != 0

		// Make sure any existing shards match the new one with regards
		// to MPP options.
		mpp := attempt.Route.FinalHop().MPP

		// MPP records should not be set for attempts to blinded paths.
		if isBlinded && mpp != nil {
			return pymtpkg.ErrMPPRecordInBlindedPayment
		}

		for _, h := range payment.InFlightHTLCs() {
			hMpp := h.Route.FinalHop().MPP

			// If this is a blinded payment, then no existing HTLCs
			// should have MPP records.
			if isBlinded && hMpp != nil {
				return pymtpkg.ErrMPPRecordInBlindedPayment
			}

			// If this is a blinded payment, then we just need to
			// check that the TotalAmtMsat field for this shard
			// is equal to that of any other shard in the same
			// payment.
			if isBlinded {
				if attempt.Route.FinalHop().TotalAmtMsat !=
					h.Route.FinalHop().TotalAmtMsat {

					//nolint:ll
					return pymtpkg.ErrBlindedPaymentTotalAmountMismatch
				}

				continue
			}

			switch {
			// We tried to register a non-MPP attempt for a MPP
			// payment.
			case mpp == nil && hMpp != nil:
				return pymtpkg.ErrMPPayment

			// We tried to register a MPP shard for a non-MPP
			// payment.
			case mpp != nil && hMpp == nil:
				return pymtpkg.ErrNonMPPayment

			// Non-MPP payment, nothing more to validate.
			case mpp == nil:
				continue
			}

			// Check that MPP options match.
			if mpp.PaymentAddr() != hMpp.PaymentAddr() {
				return pymtpkg.ErrMPPPaymentAddrMismatch
			}

			if mpp.TotalMsat() != hMpp.TotalMsat() {
				return pymtpkg.ErrMPPTotalAmountMismatch
			}
		}

		// If this is a non-MPP attempt, it must match the total amount
		// exactly. Note that a blinded payment is considered an MPP
		// attempt.
		amt := attempt.Route.ReceiverAmt()
		if !isBlinded && mpp == nil && amt != payment.Info.Value {
			return pymtpkg.ErrValueMismatch
		}

		// Ensure we aren't sending more than the total payment amount.
		sentAmt, _ := payment.SentAmt()
		if sentAmt+amt > payment.Info.Value {
			return fmt.Errorf("%w: attempted=%v, payment amount="+
				"%v", pymtpkg.ErrValueExceedsAmt, sentAmt+amt,
				payment.Info.Value)
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
func (p *KVPaymentDB) SettleAttempt(hash lntypes.Hash,
	attemptID uint64, settleInfo *pymtpkg.HTLCSettleInfo) (
	*pymtpkg.MPPayment, error) {

	var b bytes.Buffer
	if err := serializeHTLCSettleInfo(&b, settleInfo); err != nil {
		return nil, err
	}
	settleBytes := b.Bytes()

	return p.updateHtlcKey(hash, attemptID, htlcSettleInfoKey, settleBytes)
}

// FailAttempt marks the given payment attempt failed.
func (p *KVPaymentDB) FailAttempt(hash lntypes.Hash,
	attemptID uint64, failInfo *pymtpkg.HTLCFailInfo) (
	*pymtpkg.MPPayment, error) {

	var b bytes.Buffer
	if err := serializeHTLCFailInfo(&b, failInfo); err != nil {
		return nil, err
	}
	failBytes := b.Bytes()

	return p.updateHtlcKey(hash, attemptID, htlcFailInfoKey, failBytes)
}

// DeletePayment deletes a payment from the database.
//
// TODO(ziggie): Remove the wrapper.
func (p *KVPaymentDB) DeletePayment(hash lntypes.Hash,
	keepFailedAttempts bool) error {

	return p.db.DeletePayment(hash, keepFailedAttempts)
}

// DeletePayments deletes payments from the database.
//
// TODO(ziggie): Remove the wrapper.
func (p *KVPaymentDB) DeletePayments(failedOnly, failedHtlcsOnly bool) (
	int, error) {

	return p.db.DeletePayments(failedOnly, failedHtlcsOnly)
}

// QueryPayments queries the database for payments.
//
// TODO(ziggie): Remove the wrapper.
func (p *KVPaymentDB) QueryPayments(_ context.Context,
	query pymtpkg.Query) (pymtpkg.Response, error) {

	return p.db.QueryPayments(query)
}

// updateHtlcKey updates a database key for the specified htlc.
func (p *KVPaymentDB) updateHtlcKey(paymentHash lntypes.Hash,
	attemptID uint64, key, value []byte) (*pymtpkg.MPPayment, error) {

	aid := make([]byte, 8)
	binary.BigEndian.PutUint64(aid, attemptID)

	var payment *pymtpkg.MPPayment
	err := kvdb.Batch(p.db.Backend, func(tx kvdb.RwTx) error {
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
		if err := p.Status.Updatable(); err != nil {
			return err
		}

		htlcsBucket := bucket.NestedReadWriteBucket(paymentHtlcsBucket)
		if htlcsBucket == nil {
			return fmt.Errorf("htlcs bucket not found")
		}

		if htlcsBucket.Get(htlcBucketKey(htlcAttemptInfoKey, aid)) == nil {
			return fmt.Errorf("HTLC with ID %v not registered",
				attemptID)
		}

		// Make sure the shard is not already failed or settled.
		if htlcsBucket.Get(htlcBucketKey(htlcFailInfoKey, aid)) != nil {
			return pymtpkg.ErrAttemptAlreadyFailed
		}

		if htlcsBucket.Get(htlcBucketKey(htlcSettleInfoKey, aid)) != nil {
			return pymtpkg.ErrAttemptAlreadySettled
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

// FailPayment transitions a payment into the Failed state, and records the
// reason the payment failed. After invoking this method, InitPayment should
// return nil on its next call for this payment hash, allowing the switch to
// make a subsequent payment.
func (p *KVPaymentDB) FailPayment(paymentHash lntypes.Hash,
	reason pymtpkg.FailureReason) (*pymtpkg.MPPayment, error) {

	var (
		updateErr error
		payment   *pymtpkg.MPPayment
	)
	err := kvdb.Batch(p.db.Backend, func(tx kvdb.RwTx) error {
		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil
		payment = nil

		prefetchPayment(tx, paymentHash)
		bucket, err := fetchPaymentBucketUpdate(tx, paymentHash)
		if err == pymtpkg.ErrPaymentNotInitiated {
			updateErr = pymtpkg.ErrPaymentNotInitiated
			return nil
		} else if err != nil {
			return err
		}

		// We mark the payment as failed as long as it is known. This
		// lets the last attempt to fail with a terminal write its
		// failure to the PaymentControl without synchronizing with
		// other attempts.
		_, err = fetchPaymentStatus(bucket)
		if errors.Is(err, pymtpkg.ErrPaymentNotInitiated) {
			updateErr = pymtpkg.ErrPaymentNotInitiated
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
func (p *KVPaymentDB) FetchPayment(paymentHash lntypes.Hash) (
	*pymtpkg.MPPayment, error) {

	var payment *pymtpkg.MPPayment
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
		return nil, pymtpkg.ErrPaymentNotInitiated
	}

	bucket := payments.NestedReadBucket(paymentHash[:])
	if bucket == nil {
		return nil, pymtpkg.ErrPaymentNotInitiated
	}

	return bucket, nil

}

// fetchPaymentBucketUpdate is identical to fetchPaymentBucket, but it returns a
// bucket that can be written to.
func fetchPaymentBucketUpdate(tx kvdb.RwTx, paymentHash lntypes.Hash) (
	kvdb.RwBucket, error) {

	payments := tx.ReadWriteBucket(paymentsRootBucket)
	if payments == nil {
		return nil, pymtpkg.ErrPaymentNotInitiated
	}

	bucket := payments.NestedReadWriteBucket(paymentHash[:])
	if bucket == nil {
		return nil, pymtpkg.ErrPaymentNotInitiated
	}

	return bucket, nil
}

// nextPaymentSequence returns the next sequence number to store for a new
// payment.
func (p *KVPaymentDB) nextPaymentSequence() ([]byte, error) {
	p.paymentSeqMx.Lock()
	defer p.paymentSeqMx.Unlock()

	// Set a new upper bound in the DB every 1000 payments to avoid
	// conflicts on the sequence when using etcd.
	if p.currPaymentSeq == p.storedPaymentSeq {
		var currPaymentSeq, newUpperBound uint64
		if err := kvdb.Update(p.db.Backend, func(tx kvdb.RwTx) error {
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
		if p.currPaymentSeq == 0 {
			p.currPaymentSeq = currPaymentSeq
		}

		p.storedPaymentSeq = newUpperBound
	}

	p.currPaymentSeq++
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, p.currPaymentSeq)

	return b, nil
}

// fetchPaymentStatus fetches the payment status of the payment. If the payment
// isn't found, it will return error `ErrPaymentNotInitiated`.
func fetchPaymentStatus(bucket kvdb.RBucket) (pymtpkg.PaymentStatus, error) {
	// Creation info should be set for all payments, regardless of state.
	// If not, it is unknown.
	if bucket.Get(paymentCreationInfoKey) == nil {
		return 0, pymtpkg.ErrPaymentNotInitiated
	}

	payment, err := fetchPayment(bucket)
	if err != nil {
		return 0, err
	}

	return payment.Status, nil
}

// FetchInFlightPayments returns all payments with status InFlight.
func (p *KVPaymentDB) FetchInFlightPayments() ([]*pymtpkg.MPPayment, error) {
	var (
		inFlights      []*pymtpkg.MPPayment
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

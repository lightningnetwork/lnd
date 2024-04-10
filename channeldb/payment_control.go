package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// paymentSeqBlockSize is the block size used when we batch allocate
	// payment sequences for future payments.
	paymentSeqBlockSize = 1000
)

var (
	// ErrAlreadyPaid signals we have already paid this payment hash.
	ErrAlreadyPaid = errors.New("invoice is already paid")

	// ErrPaymentInFlight signals that payment for this payment hash is
	// already "in flight" on the network.
	ErrPaymentInFlight = errors.New("payment is in transition")

	// ErrPaymentExists is returned when we try to initialize an already
	// existing payment that is not failed.
	ErrPaymentExists = errors.New("payment already exists")

	// ErrPaymentInternal is returned when performing the payment has a
	// conflicting state, such as,
	// - payment has StatusSucceeded but remaining amount is not zero.
	// - payment has StatusInitiated but remaining amount is zero.
	// - payment has StatusFailed but remaining amount is zero.
	ErrPaymentInternal = errors.New("internal error")

	// ErrPaymentNotInitiated is returned if the payment wasn't initiated.
	ErrPaymentNotInitiated = errors.New("payment isn't initiated")

	// ErrPaymentAlreadySucceeded is returned in the event we attempt to
	// change the status of a payment already succeeded.
	ErrPaymentAlreadySucceeded = errors.New("payment is already succeeded")

	// ErrPaymentAlreadyFailed is returned in the event we attempt to alter
	// a failed payment.
	ErrPaymentAlreadyFailed = errors.New("payment has already failed")

	// ErrUnknownPaymentStatus is returned when we do not recognize the
	// existing state of a payment.
	ErrUnknownPaymentStatus = errors.New("unknown payment status")

	// ErrPaymentTerminal is returned if we attempt to alter a payment that
	// already has reached a terminal condition.
	ErrPaymentTerminal = errors.New("payment has reached terminal " +
		"condition")

	// ErrAttemptAlreadySettled is returned if we try to alter an already
	// settled HTLC attempt.
	ErrAttemptAlreadySettled = errors.New("attempt already settled")

	// ErrAttemptAlreadyFailed is returned if we try to alter an already
	// failed HTLC attempt.
	ErrAttemptAlreadyFailed = errors.New("attempt already failed")

	// ErrValueMismatch is returned if we try to register a non-MPP attempt
	// with an amount that doesn't match the payment amount.
	ErrValueMismatch = errors.New("attempted value doesn't match payment" +
		"amount")

	// ErrValueExceedsAmt is returned if we try to register an attempt that
	// would take the total sent amount above the payment amount.
	ErrValueExceedsAmt = errors.New("attempted value exceeds payment" +
		"amount")

	// ErrNonMPPayment is returned if we try to register an MPP attempt for
	// a payment that already has a non-MPP attempt registered.
	ErrNonMPPayment = errors.New("payment has non-MPP attempts")

	// ErrMPPayment is returned if we try to register a non-MPP attempt for
	// a payment that already has an MPP attempt registered.
	ErrMPPayment = errors.New("payment has MPP attempts")

	// ErrMPPPaymentAddrMismatch is returned if we try to register an MPP
	// shard where the payment address doesn't match existing shards.
	ErrMPPPaymentAddrMismatch = errors.New("payment address mismatch")

	// ErrMPPTotalAmountMismatch is returned if we try to register an MPP
	// shard where the total amount doesn't match existing shards.
	ErrMPPTotalAmountMismatch = errors.New("mp payment total amount " +
		"mismatch")

	// ErrPaymentPendingSettled is returned when we try to add a new
	// attempt to a payment that has at least one of its HTLCs settled.
	ErrPaymentPendingSettled = errors.New("payment has settled htlcs")

	// ErrPaymentAlreadyFailed is returned when we try to add a new attempt
	// to a payment that already has a failure reason.
	ErrPaymentPendingFailed = errors.New("payment has failure reason")

	// ErrSentExceedsTotal is returned if the payment's current total sent
	// amount exceed the total amount.
	ErrSentExceedsTotal = errors.New("total sent exceeds total amount")

	// errNoAttemptInfo is returned when no attempt info is stored yet.
	errNoAttemptInfo = errors.New("unable to find attempt info for " +
		"inflight payment")

	// errNoSequenceNrIndex is returned when an attempt to lookup a payment
	// index is made for a sequence number that is not indexed.
	errNoSequenceNrIndex = errors.New("payment sequence number index " +
		"does not exist")
)

// PaymentControl implements persistence for payments and payment attempts.
type PaymentControl struct {
	paymentSeqMx     sync.Mutex
	currPaymentSeq   uint64
	storedPaymentSeq uint64
	db               *DB
}

// NewPaymentControl creates a new instance of the PaymentControl.
func NewPaymentControl(db *DB) *PaymentControl {
	return &PaymentControl{
		db: db,
	}
}

// InitPayment checks or records the given PaymentCreationInfo with the DB,
// making sure it does not already exist as an in-flight payment. When this
// method returns successfully, the payment is guaranteed to be in the InFlight
// state.
func (p *PaymentControl) InitPayment(paymentHash lntypes.Hash,
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
func (p *PaymentControl) DeleteFailedAttempts(hash lntypes.Hash) error {
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
func (p *PaymentControl) RegisterAttempt(paymentHash lntypes.Hash,
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

		// Make sure any existing shards match the new one with regards
		// to MPP options.
		mpp := attempt.Route.FinalHop().MPP
		for _, h := range payment.InFlightHTLCs() {
			hMpp := h.Route.FinalHop().MPP

			switch {
			// We tried to register a non-MPP attempt for a MPP
			// payment.
			case mpp == nil && hMpp != nil:
				return ErrMPPayment

			// We tried to register a MPP shard for a non-MPP
			// payment.
			case mpp != nil && hMpp == nil:
				return ErrNonMPPayment

			// Non-MPP payment, nothing more to validate.
			case mpp == nil:
				continue
			}

			// Check that MPP options match.
			if mpp.PaymentAddr() != hMpp.PaymentAddr() {
				return ErrMPPPaymentAddrMismatch
			}

			if mpp.TotalMsat() != hMpp.TotalMsat() {
				return ErrMPPTotalAmountMismatch
			}
		}

		// If this is a non-MPP attempt, it must match the total amount
		// exactly.
		amt := attempt.Route.ReceiverAmt()
		if mpp == nil && amt != payment.Info.Value {
			return ErrValueMismatch
		}

		// Ensure we aren't sending more than the total payment amount.
		sentAmt, _ := payment.SentAmt()
		if sentAmt+amt > payment.Info.Value {
			return ErrValueExceedsAmt
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
func (p *PaymentControl) SettleAttempt(hash lntypes.Hash,
	attemptID uint64, settleInfo *HTLCSettleInfo) (*MPPayment, error) {

	var b bytes.Buffer
	if err := serializeHTLCSettleInfo(&b, settleInfo); err != nil {
		return nil, err
	}
	settleBytes := b.Bytes()

	return p.updateHtlcKey(hash, attemptID, htlcSettleInfoKey, settleBytes)
}

// FailAttempt marks the given payment attempt failed.
func (p *PaymentControl) FailAttempt(hash lntypes.Hash,
	attemptID uint64, failInfo *HTLCFailInfo) (*MPPayment, error) {

	var b bytes.Buffer
	if err := serializeHTLCFailInfo(&b, failInfo); err != nil {
		return nil, err
	}
	failBytes := b.Bytes()

	return p.updateHtlcKey(hash, attemptID, htlcFailInfoKey, failBytes)
}

// updateHtlcKey updates a database key for the specified htlc.
func (p *PaymentControl) updateHtlcKey(paymentHash lntypes.Hash,
	attemptID uint64, key, value []byte) (*MPPayment, error) {

	aid := make([]byte, 8)
	binary.BigEndian.PutUint64(aid, attemptID)

	var payment *MPPayment
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
		if err := p.Status.updatable(); err != nil {
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
			return ErrAttemptAlreadyFailed
		}

		if htlcsBucket.Get(htlcBucketKey(htlcSettleInfoKey, aid)) != nil {
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
func (p *PaymentControl) Fail(paymentHash lntypes.Hash,
	reason FailureReason) (*MPPayment, error) {

	var (
		updateErr error
		payment   *MPPayment
	)
	err := kvdb.Batch(p.db.Backend, func(tx kvdb.RwTx) error {
		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil
		payment = nil

		prefetchPayment(tx, paymentHash)
		bucket, err := fetchPaymentBucketUpdate(tx, paymentHash)
		if err == ErrPaymentNotInitiated {
			updateErr = ErrPaymentNotInitiated
			return nil
		} else if err != nil {
			return err
		}

		// We mark the payment as failed as long as it is known. This
		// lets the last attempt to fail with a terminal write its
		// failure to the PaymentControl without synchronizing with
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
func (p *PaymentControl) FetchPayment(paymentHash lntypes.Hash) (
	*MPPayment, error) {

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
func (p *PaymentControl) nextPaymentSequence() ([]byte, error) {
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
func (p *PaymentControl) FetchInFlightPayments() ([]*MPPayment, error) {
	var inFlights []*MPPayment
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

	return inFlights, nil
}

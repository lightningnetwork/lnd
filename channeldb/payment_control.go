package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// ErrAlreadyPaid signals we have already paid this payment hash.
	ErrAlreadyPaid = errors.New("invoice is already paid")

	// ErrPaymentInFlight signals that payment for this payment hash is
	// already "in flight" on the network.
	ErrPaymentInFlight = errors.New("payment is in transition")

	// ErrPaymentNotInitiated is returned  if payment wasn't initiated in
	// switch.
	ErrPaymentNotInitiated = errors.New("payment isn't initiated")

	// ErrPaymentAlreadySucceeded is returned in the event we attempt to
	// change the status of a payment already succeeded.
	ErrPaymentAlreadySucceeded = errors.New("payment is already succeeded")

	// ErrPaymentAlreadyFailed is returned in the event we attempt to
	// re-fail a failed payment.
	ErrPaymentAlreadyFailed = errors.New("payment has already failed")

	// ErrUnknownPaymentStatus is returned when we do not recognize the
	// existing state of a payment.
	ErrUnknownPaymentStatus = errors.New("unknown payment status")

	// errNoAttemptInfo is returned when no attempt info is stored yet.
	errNoAttemptInfo = errors.New("unable to find attempt info for " +
		"inflight payment")
)

// PaymentControl implements persistence for payments and payment attempts.
type PaymentControl struct {
	db *DB
}

// NewPaymentControl creates a new instance of the PaymentControl.
func NewPaymentControl(db *DB) *PaymentControl {
	return &PaymentControl{
		db: db,
	}
}

// InitPayment checks or records the given PaymentCreationInfo with the DB,
// making sure it does not already exist as an in-flight payment. Then this
// method returns successfully, the payment is guranteeed to be in the InFlight
// state.
func (p *PaymentControl) InitPayment(paymentHash lntypes.Hash,
	info *PaymentCreationInfo) error {

	var b bytes.Buffer
	if err := serializePaymentCreationInfo(&b, info); err != nil {
		return err
	}
	infoBytes := b.Bytes()

	var updateErr error
	err := kvdb.Batch(p.db.Backend, func(tx kvdb.RwTx) error {
		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil

		bucket, err := createPaymentBucket(tx, paymentHash)
		if err != nil {
			return err
		}

		// Get the existing status of this payment, if any.
		paymentStatus, err := fetchPaymentStatus(bucket)
		if err != nil {
			return err
		}

		switch paymentStatus {

		// We allow retrying failed payments.
		case StatusFailed:

		// This is a new payment that is being initialized for the
		// first time.
		case StatusUnknown:

		// We already have an InFlight payment on the network. We will
		// disallow any new payments.
		case StatusInFlight:
			updateErr = ErrPaymentInFlight
			return nil

		// We've already succeeded a payment to this payment hash,
		// forbid the switch from sending another.
		case StatusSucceeded:
			updateErr = ErrAlreadyPaid
			return nil

		default:
			updateErr = ErrUnknownPaymentStatus
			return nil
		}

		// Obtain a new sequence number for this payment. This is used
		// to sort the payments in order of creation, and also acts as
		// a unique identifier for each payment.
		sequenceNum, err := nextPaymentSequence(tx)
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
		return err
	}

	return updateErr
}

// RegisterAttempt atomically records the provided HTLCAttemptInfo to the
// DB.
func (p *PaymentControl) RegisterAttempt(paymentHash lntypes.Hash,
	attempt *HTLCAttemptInfo) error {

	// Serialize the information before opening the db transaction.
	var a bytes.Buffer
	err := serializeHTLCAttemptInfo(&a, attempt)
	if err != nil {
		return err
	}
	htlcInfoBytes := a.Bytes()

	htlcIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(htlcIDBytes, attempt.AttemptID)

	return kvdb.Batch(p.db.Backend, func(tx kvdb.RwTx) error {
		bucket, err := fetchPaymentBucketUpdate(tx, paymentHash)
		if err != nil {
			return err
		}

		// We can only register attempts for payments that are
		// in-flight.
		if err := ensureInFlight(bucket); err != nil {
			return err
		}

		htlcsBucket, err := bucket.CreateBucketIfNotExists(
			paymentHtlcsBucket,
		)
		if err != nil {
			return err
		}

		// Create bucket for this attempt. Fail if the bucket already
		// exists.
		htlcBucket, err := htlcsBucket.CreateBucket(htlcIDBytes)
		if err != nil {
			return err
		}

		return htlcBucket.Put(htlcAttemptInfoKey, htlcInfoBytes)
	})
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
	attemptID uint64, failInfo *HTLCFailInfo) error {

	var b bytes.Buffer
	if err := serializeHTLCFailInfo(&b, failInfo); err != nil {
		return err
	}
	failBytes := b.Bytes()

	_, err := p.updateHtlcKey(hash, attemptID, htlcFailInfoKey, failBytes)
	return err
}

// updateHtlcKey updates a database key for the specified htlc.
func (p *PaymentControl) updateHtlcKey(paymentHash lntypes.Hash,
	attemptID uint64, key, value []byte) (*MPPayment, error) {

	htlcIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(htlcIDBytes, attemptID)

	var payment *MPPayment
	err := kvdb.Batch(p.db.Backend, func(tx kvdb.RwTx) error {
		payment = nil

		bucket, err := fetchPaymentBucketUpdate(tx, paymentHash)
		if err != nil {
			return err
		}

		// We can only update keys of in-flight payments.
		if err := ensureInFlight(bucket); err != nil {
			return err
		}

		htlcsBucket := bucket.NestedReadWriteBucket(paymentHtlcsBucket)
		if htlcsBucket == nil {
			return fmt.Errorf("htlcs bucket not found")
		}

		htlcBucket := htlcsBucket.NestedReadWriteBucket(htlcIDBytes)
		if htlcBucket == nil {
			return fmt.Errorf("HTLC with ID %v not registered",
				attemptID)
		}

		// Add or update the key for this htlc.
		err = htlcBucket.Put(key, value)
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

		bucket, err := fetchPaymentBucketUpdate(tx, paymentHash)
		if err == ErrPaymentNotInitiated {
			updateErr = ErrPaymentNotInitiated
			return nil
		} else if err != nil {
			return err
		}

		// We can only mark in-flight payments as failed.
		if err := ensureInFlight(bucket); err != nil {
			updateErr = err
			return nil
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

		// Final sanity check to see if there are no in-flight htlcs.
		for _, htlc := range payment.HTLCs {
			if htlc.Settle == nil && htlc.Failure == nil {
				return errors.New("payment failed with " +
					"in-flight htlc(s)")
			}
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
	err := kvdb.View(p.db, func(tx kvdb.ReadTx) error {
		bucket, err := fetchPaymentBucket(tx, paymentHash)
		if err != nil {
			return err
		}

		payment, err = fetchPayment(bucket)

		return err
	})
	if err != nil {
		return nil, err
	}

	return payment, nil
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
func fetchPaymentBucket(tx kvdb.ReadTx, paymentHash lntypes.Hash) (
	kvdb.ReadBucket, error) {

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
func nextPaymentSequence(tx kvdb.RwTx) ([]byte, error) {
	payments, err := tx.CreateTopLevelBucket(paymentsRootBucket)
	if err != nil {
		return nil, err
	}

	seq, err := payments.NextSequence()
	if err != nil {
		return nil, err
	}

	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, seq)
	return b, nil
}

// fetchPaymentStatus fetches the payment status of the payment. If the payment
// isn't found, it will default to "StatusUnknown".
func fetchPaymentStatus(bucket kvdb.ReadBucket) (PaymentStatus, error) {
	htlcsBucket := bucket.NestedReadBucket(paymentHtlcsBucket)
	if htlcsBucket != nil {
		htlcs, err := fetchHtlcAttempts(htlcsBucket)
		if err != nil {
			return 0, err
		}

		// Go through all HTLCs, and return StatusSucceeded if any of
		// them did succeed.
		for _, h := range htlcs {
			if h.Settle != nil {
				return StatusSucceeded, nil
			}
		}
	}

	if bucket.Get(paymentFailInfoKey) != nil {
		return StatusFailed, nil
	}

	if bucket.Get(paymentCreationInfoKey) != nil {
		return StatusInFlight, nil
	}

	return StatusUnknown, nil
}

// ensureInFlight checks whether the payment found in the given bucket has
// status InFlight, and returns an error otherwise. This should be used to
// ensure we only mark in-flight payments as succeeded or failed.
func ensureInFlight(bucket kvdb.ReadBucket) error {
	paymentStatus, err := fetchPaymentStatus(bucket)
	if err != nil {
		return err
	}

	switch {

	// The payment was indeed InFlight, return.
	case paymentStatus == StatusInFlight:
		return nil

	// Our records show the payment as unknown, meaning it never
	// should have left the switch.
	case paymentStatus == StatusUnknown:
		return ErrPaymentNotInitiated

	// The payment succeeded previously.
	case paymentStatus == StatusSucceeded:
		return ErrPaymentAlreadySucceeded

	// The payment was already failed.
	case paymentStatus == StatusFailed:
		return ErrPaymentAlreadyFailed

	default:
		return ErrUnknownPaymentStatus
	}
}

// InFlightPayment is a wrapper around a payment that has status InFlight.
type InFlightPayment struct {
	// Info is the PaymentCreationInfo of the in-flight payment.
	Info *PaymentCreationInfo

	// Attempts is the set of payment attempts that was made to this
	// payment hash.
	//
	// NOTE: Might be empty.
	Attempts []HTLCAttemptInfo
}

// FetchInFlightPayments returns all payments with status InFlight.
func (p *PaymentControl) FetchInFlightPayments() ([]*InFlightPayment, error) {
	var inFlights []*InFlightPayment
	err := kvdb.View(p.db, func(tx kvdb.ReadTx) error {
		payments := tx.ReadBucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		return payments.ForEach(func(k, _ []byte) error {
			bucket := payments.NestedReadBucket(k)
			if bucket == nil {
				return fmt.Errorf("non bucket element")
			}

			// If the status is not InFlight, we can return early.
			paymentStatus, err := fetchPaymentStatus(bucket)
			if err != nil {
				return err
			}

			if paymentStatus != StatusInFlight {
				return nil
			}

			inFlight := &InFlightPayment{}

			// Get the CreationInfo.
			b := bucket.Get(paymentCreationInfoKey)
			if b == nil {
				return fmt.Errorf("unable to find creation " +
					"info for inflight payment")
			}

			r := bytes.NewReader(b)
			inFlight.Info, err = deserializePaymentCreationInfo(r)
			if err != nil {
				return err
			}

			htlcsBucket := bucket.NestedReadBucket(
				paymentHtlcsBucket,
			)
			if htlcsBucket == nil {
				return nil
			}

			// Fetch all HTLCs attempted for this payment.
			htlcs, err := fetchHtlcAttempts(htlcsBucket)
			if err != nil {
				return err
			}

			// We only care about the static info for the HTLCs
			// still in flight, so convert the result to a slice of
			// HTLCAttemptInfos.
			for _, h := range htlcs {
				// Skip HTLCs not in flight.
				if h.Settle != nil || h.Failure != nil {
					continue
				}

				inFlight.Attempts = append(
					inFlight.Attempts, h.HTLCAttemptInfo,
				)
			}

			inFlights = append(inFlights, inFlight)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return inFlights, nil
}

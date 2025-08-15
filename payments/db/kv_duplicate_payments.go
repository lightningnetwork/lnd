package paymentsdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// duplicatePaymentsBucket is the name of a optional sub-bucket within
	// the payment hash bucket, that is used to hold duplicate payments to a
	// payment hash. This is needed to support information from earlier
	// versions of lnd, where it was possible to pay to a payment hash more
	// than once.
	duplicatePaymentsBucket = []byte("payment-duplicate-bucket")

	// duplicatePaymentSettleInfoKey is a key used in the payment's
	// sub-bucket to store the settle info of the payment.
	duplicatePaymentSettleInfoKey = []byte("payment-settle-info")

	// duplicatePaymentAttemptInfoKey is a key used in the payment's
	// sub-bucket to store the info about the latest attempt that was done
	// for the payment in question.
	duplicatePaymentAttemptInfoKey = []byte("payment-attempt-info")

	// duplicatePaymentCreationInfoKey is a key used in the payment's
	// sub-bucket to store the creation info of the payment.
	duplicatePaymentCreationInfoKey = []byte("payment-creation-info")

	// duplicatePaymentFailInfoKey is a key used in the payment's sub-bucket
	// to store information about the reason a payment failed.
	duplicatePaymentFailInfoKey = []byte("payment-fail-info")

	// duplicatePaymentSequenceKey is a key used in the payment's sub-bucket
	// to store the sequence number of the payment.
	duplicatePaymentSequenceKey = []byte("payment-sequence-key")
)

// duplicateHTLCAttemptInfo contains static information about a specific HTLC
// attempt for a payment. This information is used by the router to handle any
// errors coming back after an attempt is made, and to query the switch about
// the status of the attempt.
type duplicateHTLCAttemptInfo struct {
	// attemptID is the unique ID used for this attempt.
	attemptID uint64

	// sessionKey is the ephemeral key used for this attempt.
	sessionKey [btcec.PrivKeyBytesLen]byte

	// route is the route attempted to send the HTLC.
	route route.Route
}

// fetchDuplicatePaymentStatus fetches the payment status of the payment. If
// the payment isn't found, it will return error `ErrPaymentNotInitiated`.
func fetchDuplicatePaymentStatus(bucket kvdb.RBucket) (PaymentStatus, error) {
	if bucket.Get(duplicatePaymentSettleInfoKey) != nil {
		return StatusSucceeded, nil
	}

	if bucket.Get(duplicatePaymentFailInfoKey) != nil {
		return StatusFailed, nil
	}

	if bucket.Get(duplicatePaymentCreationInfoKey) != nil {
		return StatusInFlight, nil
	}

	return 0, ErrPaymentNotInitiated
}

func deserializeDuplicateHTLCAttemptInfo(r io.Reader) (
	*duplicateHTLCAttemptInfo, error) {

	a := &duplicateHTLCAttemptInfo{}
	err := ReadElements(r, &a.attemptID, &a.sessionKey)
	if err != nil {
		return nil, err
	}
	a.route, err = DeserializeRoute(r)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func deserializeDuplicatePaymentCreationInfo(r io.Reader) (
	*PaymentCreationInfo, error) {

	var scratch [8]byte

	c := &PaymentCreationInfo{}

	if _, err := io.ReadFull(r, c.PaymentIdentifier[:]); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return nil, err
	}
	c.Value = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return nil, err
	}
	c.CreationTime = time.Unix(int64(byteOrder.Uint64(scratch[:])), 0)

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

	return c, nil
}

func fetchDuplicatePayment(bucket kvdb.RBucket) (*MPPayment, error) {
	seqBytes := bucket.Get(duplicatePaymentSequenceKey)
	if seqBytes == nil {
		return nil, fmt.Errorf("sequence number not found")
	}

	sequenceNum := binary.BigEndian.Uint64(seqBytes)

	// Get the payment status.
	paymentStatus, err := fetchDuplicatePaymentStatus(bucket)
	if err != nil {
		return nil, err
	}

	// Get the PaymentCreationInfo.
	b := bucket.Get(duplicatePaymentCreationInfoKey)
	if b == nil {
		return nil, fmt.Errorf("creation info not found")
	}

	r := bytes.NewReader(b)
	creationInfo, err := deserializeDuplicatePaymentCreationInfo(r)
	if err != nil {
		return nil, err
	}

	// Get failure reason if available.
	var failureReason *FailureReason
	b = bucket.Get(duplicatePaymentFailInfoKey)
	if b != nil {
		reason := FailureReason(b[0])
		failureReason = &reason
	}

	payment := &MPPayment{
		SequenceNum:   sequenceNum,
		Info:          creationInfo,
		FailureReason: failureReason,
		Status:        paymentStatus,
	}

	// Get the HTLCAttemptInfo. It can be absent.
	b = bucket.Get(duplicatePaymentAttemptInfoKey)
	if b != nil {
		r = bytes.NewReader(b)
		attempt, err := deserializeDuplicateHTLCAttemptInfo(r)
		if err != nil {
			return nil, err
		}

		htlc := HTLCAttempt{
			HTLCAttemptInfo: HTLCAttemptInfo{
				AttemptID:  attempt.attemptID,
				Route:      attempt.route,
				sessionKey: attempt.sessionKey,
			},
		}

		// Get the payment preimage. This is only found for
		// successful payments.
		b = bucket.Get(duplicatePaymentSettleInfoKey)
		if b != nil {
			var preimg lntypes.Preimage
			copy(preimg[:], b)

			htlc.Settle = &HTLCSettleInfo{
				Preimage:   preimg,
				SettleTime: time.Time{},
			}
		} else {
			// Otherwise the payment must have failed.
			htlc.Failure = &HTLCFailInfo{
				FailTime: time.Time{},
			}
		}

		payment.HTLCs = []HTLCAttempt{htlc}
	}

	return payment, nil
}

func fetchDuplicatePayments(paymentHashBucket kvdb.RBucket) ([]*MPPayment,
	error) {

	var payments []*MPPayment

	// For older versions of lnd, duplicate payments to a payment has was
	// possible. These will be found in a sub-bucket indexed by their
	// sequence number if available.
	dup := paymentHashBucket.NestedReadBucket(duplicatePaymentsBucket)
	if dup == nil {
		return nil, nil
	}

	err := dup.ForEach(func(k, v []byte) error {
		subBucket := dup.NestedReadBucket(k)
		if subBucket == nil {
			// We one bucket for each duplicate to be found.
			return fmt.Errorf("non bucket element" +
				"in duplicate bucket")
		}

		p, err := fetchDuplicatePayment(subBucket)
		if err != nil {
			return err
		}

		payments = append(payments, p)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return payments, nil
}

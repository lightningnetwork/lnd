package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

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
	//      |        |--attempt-info-key: <attempt info>
	//      |        |--settle-info-key: <settle info>
	//      |        |--fail-info-key: <fail info>
	//      |        |
	//      |        |--duplicate-bucket (only for old, completed payments)
	//      |                 |
	//      |                 |-- <seq-num>
	//      |                 |       |--sequence-key: <sequence number>
	//      |                 |       |--creation-info-key: <creation info>
	//      |                 |       |--attempt-info-key: <attempt info>
	//      |                 |       |--settle-info-key: <settle info>
	//      |                 |       |--fail-info-key: <fail info>
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

	// paymentStatusKey is a key used in the payment's sub-bucket to store
	// the status of the payment.
	paymentStatusKey = []byte("payment-status-key")

	// paymentDublicateBucket is the name of a optional sub-bucket within
	// the payment hash bucket, that is used to hold duplicate payments to
	// a payment hash. This is needed to support information from earlier
	// versions of lnd, where it was possible to pay to a payment hash more
	// than once.
	paymentDuplicateBucket = []byte("payment-duplicate-bucket")

	// paymentSequenceKey is a key used in the payment's sub-bucket to
	// store the sequence number of the payment.
	paymentSequenceKey = []byte("payment-sequence-key")

	// paymentCreationInfoKey is a key used in the payment's sub-bucket to
	// store the creation info of the payment.
	paymentCreationInfoKey = []byte("payment-creation-info")

	// paymentAttemptInfoKey is a key used in the payment's sub-bucket to
	// store the info about the latest attempt that was done for the
	// payment in question.
	paymentAttemptInfoKey = []byte("payment-attempt-info")

	// paymentSettleInfoKey is a key used in the payment's sub-bucket to
	// store the settle info of the payment.
	paymentSettleInfoKey = []byte("payment-settle-info")

	// paymentBucket is the name of the bucket within the database that
	// stores all data related to payments.
	//
	// Within the payments bucket, each invoice is keyed by its invoice ID
	// which is a monotonically increasing uint64.  BoltDB's sequence
	// feature is used for generating monotonically increasing id.
	paymentBucket = []byte("payments")

	// paymentStatusBucket is the name of the bucket within the database that
	// stores the status of a payment indexed by the payment's preimage.
	paymentStatusBucket = []byte("payment-status")
)

// PaymentStatus represent current status of payment
type PaymentStatus byte

const (
	// StatusGrounded is the status where a payment has never been
	// initiated.
	StatusGrounded PaymentStatus = 0

	// StatusInFlight is the status where a payment has been initiated, but
	// a response has not been received.
	StatusInFlight PaymentStatus = 1

	// StatusCompleted is the status where a payment has been initiated and
	// the payment was completed successfully.
	StatusCompleted PaymentStatus = 2

	// StatusFailed is the status where a payment has been initiated and a
	// failure result has come back.
	StatusFailed PaymentStatus = 3

	// TODO(halseth): timeout/cancel state?
)

// Bytes returns status as slice of bytes.
func (ps PaymentStatus) Bytes() []byte {
	return []byte{byte(ps)}
}

// FromBytes sets status from slice of bytes.
func (ps *PaymentStatus) FromBytes(status []byte) error {
	if len(status) != 1 {
		return errors.New("payment status is empty")
	}

	switch PaymentStatus(status[0]) {
	case StatusGrounded, StatusInFlight, StatusCompleted, StatusFailed:
		*ps = PaymentStatus(status[0])
	default:
		return errors.New("unknown payment status")
	}

	return nil
}

// String returns readable representation of payment status.
func (ps PaymentStatus) String() string {
	switch ps {
	case StatusGrounded:
		return "Grounded"
	case StatusInFlight:
		return "In Flight"
	case StatusCompleted:
		return "Completed"
	case StatusFailed:
		return "Failed"
	default:
		return "Unknown"
	}
}

// OutgoingPayment represents a successful payment between the daemon and a
// remote node. Details such as the total fee paid, and the time of the payment
// are stored.
//
// NOTE: Deprecated. Kept around for migration purposes.
type OutgoingPayment struct {
	Invoice

	// Fee is the total fee paid for the payment in milli-satoshis.
	Fee lnwire.MilliSatoshi

	// TotalTimeLock is the total cumulative time-lock in the HTLC extended
	// from the second-to-last hop to the destination.
	TimeLockLength uint32

	// Path encodes the path the payment took through the network. The path
	// excludes the outgoing node and consists of the hex-encoded
	// compressed public key of each of the nodes involved in the payment.
	Path [][33]byte

	// PaymentPreimage is the preImage of a successful payment. This is used
	// to calculate the PaymentHash as well as serve as a proof of payment.
	PaymentPreimage [32]byte
}

// AddPayment saves a successful payment to the database. It is assumed that
// all payment are sent using unique payment hashes.
//
// NOTE: Deprecated. Kept around for migration purposes.
func (db *DB) AddPayment(payment *OutgoingPayment) error {
	// Validate the field of the inner voice within the outgoing payment,
	// these must also adhere to the same constraints as regular invoices.
	if err := validateInvoice(&payment.Invoice); err != nil {
		return err
	}

	// We first serialize the payment before starting the database
	// transaction so we can avoid creating a DB payment in the case of a
	// serialization error.
	var b bytes.Buffer
	if err := serializeOutgoingPayment(&b, payment); err != nil {
		return err
	}
	paymentBytes := b.Bytes()

	return db.Batch(func(tx *bbolt.Tx) error {
		payments, err := tx.CreateBucketIfNotExists(paymentBucket)
		if err != nil {
			return err
		}

		// Obtain the new unique sequence number for this payment.
		paymentID, err := payments.NextSequence()
		if err != nil {
			return err
		}

		// We use BigEndian for keys as it orders keys in
		// ascending order. This allows bucket scans to order payments
		// in the order in which they were created.
		paymentIDBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(paymentIDBytes, paymentID)

		return payments.Put(paymentIDBytes, paymentBytes)
	})
}

// FetchAllPayments returns all outgoing payments in DB.
//
// NOTE: Deprecated. Kept around for migration purposes.
func (db *DB) FetchAllPayments() ([]*OutgoingPayment, error) {
	var payments []*OutgoingPayment

	err := db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(paymentBucket)
		if bucket == nil {
			return ErrNoPaymentsCreated
		}

		return bucket.ForEach(func(k, v []byte) error {
			// If the value is nil, then we ignore it as it may be
			// a sub-bucket.
			if v == nil {
				return nil
			}

			r := bytes.NewReader(v)
			payment, err := deserializeOutgoingPayment(r)
			if err != nil {
				return err
			}

			payments = append(payments, payment)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return payments, nil
}

// DeleteAllPayments deletes all payments from DB.
//
// NOTE: Deprecated. Kept around for migration purposes.
func (db *DB) DeleteAllPayments() error {
	return db.Update(func(tx *bbolt.Tx) error {
		err := tx.DeleteBucket(paymentBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		_, err = tx.CreateBucket(paymentBucket)
		return err
	})
}

// FetchPaymentStatus returns the payment status for outgoing payment.
// If status of the payment isn't found, it will default to "StatusGrounded".
//
// NOTE: Deprecated. Kept around for migration purposes.
func (db *DB) FetchPaymentStatus(paymentHash [32]byte) (PaymentStatus, error) {
	var paymentStatus = StatusGrounded
	err := db.View(func(tx *bbolt.Tx) error {
		var err error
		paymentStatus, err = FetchPaymentStatusTx(tx, paymentHash)
		return err
	})
	if err != nil {
		return StatusGrounded, err
	}

	return paymentStatus, nil
}

// FetchPaymentStatusTx is a helper method that returns the payment status for
// outgoing payment.  If status of the payment isn't found, it will default to
// "StatusGrounded". It accepts the boltdb transactions such that this method
// can be composed into other atomic operations.
//
// NOTE: Deprecated. Kept around for migration purposes.
func FetchPaymentStatusTx(tx *bbolt.Tx, paymentHash [32]byte) (PaymentStatus, error) {
	// The default status for all payments that aren't recorded in database.
	var paymentStatus = StatusGrounded

	bucket := tx.Bucket(paymentStatusBucket)
	if bucket == nil {
		return paymentStatus, nil
	}

	paymentStatusBytes := bucket.Get(paymentHash[:])
	if paymentStatusBytes == nil {
		return paymentStatus, nil
	}

	paymentStatus.FromBytes(paymentStatusBytes)

	return paymentStatus, nil
}

func serializeOutgoingPayment(w io.Writer, p *OutgoingPayment) error {
	var scratch [8]byte

	if err := serializeInvoice(w, &p.Invoice); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(p.Fee))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	// First write out the length of the bytes to prefix the value.
	pathLen := uint32(len(p.Path))
	byteOrder.PutUint32(scratch[:4], pathLen)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	// Then with the path written, we write out the series of public keys
	// involved in the path.
	for _, hop := range p.Path {
		if _, err := w.Write(hop[:]); err != nil {
			return err
		}
	}

	byteOrder.PutUint32(scratch[:4], p.TimeLockLength)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	if _, err := w.Write(p.PaymentPreimage[:]); err != nil {
		return err
	}

	return nil
}

func deserializeOutgoingPayment(r io.Reader) (*OutgoingPayment, error) {
	var scratch [8]byte

	p := &OutgoingPayment{}

	inv, err := deserializeInvoice(r)
	if err != nil {
		return nil, err
	}
	p.Invoice = inv

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	p.Fee = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	if _, err = r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	pathLen := byteOrder.Uint32(scratch[:4])

	path := make([][33]byte, pathLen)
	for i := uint32(0); i < pathLen; i++ {
		if _, err := r.Read(path[i][:]); err != nil {
			return nil, err
		}
	}
	p.Path = path

	if _, err = r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	p.TimeLockLength = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(p.PaymentPreimage[:]); err != nil {
		return nil, err
	}

	return p, nil
}

// PaymentCreationInfo is the information necessary to have ready when
// initiating a payment, moving it into state InFlight.
type PaymentCreationInfo struct {
	// PaymentHash is the hash this payment is paying to.
	PaymentHash lntypes.Hash

	// Value is the amount we are paying.
	Value lnwire.MilliSatoshi

	// CreatingDate is the time when this payment was initiated.
	CreationDate time.Time

	// PaymentRequest is the full payment request, if any.
	PaymentRequest []byte
}

// PaymentAttemptInfo contains information about a specific payment attempt for
// a given payment. This information is used by the router to handle any errors
// coming back after an attempt is made, and to query the switch about the
// status of a payment. For settled payment this will be the information for
// the succeeding payment attempt.
type PaymentAttemptInfo struct {
	// PaymentID is the unique ID used for this attempt.
	PaymentID uint64

	// SessionKey is the ephemeral key used for this payment attempt.
	SessionKey *btcec.PrivateKey

	// Route is the route attempted to send the HTLC.
	Route route.Route
}

// Payment is a wrapper around a payment's PaymentCreationInfo,
// PaymentAttemptInfo, and preimage. All payments will have the
// PaymentCreationInfo set, the PaymentAttemptInfo will be set only if at least
// one payment attempt has been made, while only completed payments will have a
// non-zero payment preimage.
type Payment struct {
	// sequenceNum is a unique identifier used to sort the payments in
	// order of creation.
	sequenceNum uint64

	// Status is the current PaymentStatus of this payment.
	Status PaymentStatus

	// Info holds all static information about this payment, and is
	// populated when the payment is initiated.
	Info *PaymentCreationInfo

	// Attempt is the information about the last payment attempt made.
	//
	// NOTE: Can be nil if no attempt is yet made.
	Attempt *PaymentAttemptInfo

	// PaymentPreimage is the preimage of a successful payment. This serves
	// as a proof of payment. It will only be non-nil for settled payments.
	//
	// NOTE: Can be nil if payment is not settled.
	PaymentPreimage *lntypes.Preimage
}

// FetchPayments returns all sent payments found in the DB.
func (db *DB) FetchPayments() ([]*Payment, error) {
	var payments []*Payment

	err := db.View(func(tx *bbolt.Tx) error {
		paymentsBucket := tx.Bucket(paymentsRootBucket)
		if paymentsBucket == nil {
			return nil
		}

		return paymentsBucket.ForEach(func(k, v []byte) error {
			bucket := paymentsBucket.Bucket(k)
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
			dup := bucket.Bucket(paymentDuplicateBucket)
			if dup == nil {
				return nil
			}

			return dup.ForEach(func(k, v []byte) error {
				subBucket := dup.Bucket(k)
				if subBucket == nil {
					// We one bucket for each duplicate to
					// be found.
					return fmt.Errorf("non bucket element" +
						"in duplicate bucket")
				}

				p, err := fetchPayment(subBucket)
				if err != nil {
					return err
				}

				payments = append(payments, p)
				return nil
			})
		})
	})
	if err != nil {
		return nil, err
	}

	// Before returning, sort the payments by their sequence number.
	sort.Slice(payments, func(i, j int) bool {
		return payments[i].sequenceNum < payments[j].sequenceNum
	})

	return payments, nil
}

func fetchPayment(bucket *bbolt.Bucket) (*Payment, error) {
	var (
		err error
		p   = &Payment{}
	)

	seqBytes := bucket.Get(paymentSequenceKey)
	if seqBytes == nil {
		return nil, fmt.Errorf("sequence number not found")
	}

	p.sequenceNum = binary.BigEndian.Uint64(seqBytes)

	// Get the payment status.
	p.Status = fetchPaymentStatus(bucket)

	// Get the PaymentCreationInfo.
	b := bucket.Get(paymentCreationInfoKey)
	if b == nil {
		return nil, fmt.Errorf("creation info not found")
	}

	r := bytes.NewReader(b)
	p.Info, err = deserializePaymentCreationInfo(r)
	if err != nil {
		return nil, err

	}

	// Get the PaymentAttemptInfo. This can be unset.
	b = bucket.Get(paymentAttemptInfoKey)
	if b != nil {
		r = bytes.NewReader(b)
		p.Attempt, err = deserializePaymentAttemptInfo(r)
		if err != nil {
			return nil, err
		}
	}

	// Get the payment preimage. This is only found for
	// completed payments.
	b = bucket.Get(paymentSettleInfoKey)
	if b != nil {
		var preimg lntypes.Preimage
		copy(preimg[:], b[:])
		p.PaymentPreimage = &preimg
	}

	return p, nil
}

func serializePaymentCreationInfo(w io.Writer, c *PaymentCreationInfo) error {
	var scratch [8]byte

	if _, err := w.Write(c.PaymentHash[:]); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(c.Value))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(c.CreationDate.Unix()))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], uint32(len(c.PaymentRequest)))
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	if _, err := w.Write(c.PaymentRequest[:]); err != nil {
		return err
	}

	return nil
}

func deserializePaymentCreationInfo(r io.Reader) (*PaymentCreationInfo, error) {
	var scratch [8]byte

	c := &PaymentCreationInfo{}

	if _, err := io.ReadFull(r, c.PaymentHash[:]); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return nil, err
	}
	c.Value = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return nil, err
	}
	c.CreationDate = time.Unix(int64(byteOrder.Uint64(scratch[:])), 0)

	if _, err := io.ReadFull(r, scratch[:4]); err != nil {
		return nil, err
	}

	reqLen := uint32(byteOrder.Uint32(scratch[:4]))
	payReq := make([]byte, reqLen)
	if reqLen > 0 {
		if _, err := io.ReadFull(r, payReq[:]); err != nil {
			return nil, err
		}
	}
	c.PaymentRequest = payReq

	return c, nil
}

func serializePaymentAttemptInfo(w io.Writer, a *PaymentAttemptInfo) error {
	if err := WriteElements(w, a.PaymentID, a.SessionKey); err != nil {
		return err
	}

	if err := serializeRoute(w, a.Route); err != nil {
		return err
	}

	return nil
}

func deserializePaymentAttemptInfo(r io.Reader) (*PaymentAttemptInfo, error) {
	a := &PaymentAttemptInfo{}
	err := ReadElements(r, &a.PaymentID, &a.SessionKey)
	if err != nil {
		return nil, err
	}
	a.Route, err = deserializeRoute(r)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func serializeHop(w io.Writer, h *route.Hop) error {
	if err := WriteElements(w,
		h.PubKeyBytes[:], h.ChannelID, h.OutgoingTimeLock,
		h.AmtToForward,
	); err != nil {
		return err
	}

	return nil
}

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

	return h, nil
}

func serializeRoute(w io.Writer, r route.Route) error {
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

	return nil
}

func deserializeRoute(r io.Reader) (route.Route, error) {
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

	return rt, nil
}

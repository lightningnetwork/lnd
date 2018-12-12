package channeldb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"

	"fmt"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// paymentBucket is the name of the bucket within the database that
	// stores all data related to payments.
	//
	// Within the payments bucket, each invoice is keyed by its invoice ID
	// which is a monotonically increasing uint64.  BoltDB's sequence
	// feature is used for generating monotonically increasing id.
	paymentBucket = []byte("payments")

	// paymentIndexBucket is the name of the sub-bucket within the
	// paymentBucket which indexes all payments by their payment hash. The
	// payment hash is the sha256 of the payment's preimage. This
	// index is used to provide a fast path for looking up incoming HTLCs
	// to determine if we're able to settle them fully.
	//
	// maps: payHash => payment
	paymentIndexBucket = []byte("paymenthashes")

	// numPaymentKey is the name of key which houses the auto-incrementing
	// payment ID which is essentially used as a primary key. With each
	// payment inserted, the primary key is incremented by one. This key is
	// stored within the paymentIndexBucket. Within the paymentBucket
	// payments are uniquely identified by the payment ID.
	numPaymentsKey = []byte("nik")

	// addPaymentIndexBucket is an index bucket that we'll use to create a
	// monotonically increasing set of add indexes. Each time we add a new
	// payment, this sequence number will be incremented and then populated
	// within the new payment.
	//
	// In addition to this sequence number, we map:
	//
	//   addIndexNo => paymentKey
	addPaymentIndexBucket = []byte("payment-add-index")

	// paymentStatusBucket is the name of the bucket within the database that
	// stores the status of a payment indexed by the payment's preimage.
	paymentStatusBucket = []byte("payment-status")
)

// PaymentStatus represent current status of payment
type PaymentStatus byte

const (
	// StatusGrounded is the status where a payment has never been
	// initiated, or has been initiated and received an intermittent
	// failure.
	StatusGrounded PaymentStatus = 0

	// StatusInFlight is the status where a payment has been initiated, but
	// a response has not been received.
	StatusInFlight PaymentStatus = 1

	// StatusCompleted is the status where a payment has been initiated and
	// the payment was completed successfully.
	StatusCompleted PaymentStatus = 2
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
	case StatusGrounded, StatusInFlight, StatusCompleted:
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
	default:
		return "Unknown"
	}
}

// OutgoingPayment represents a successful payment between the daemon and a
// remote node. Details such as the total fee paid, and the time of the payment
// are stored.
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
func (db *DB) AddPayment(payment *OutgoingPayment) (uint64, error) {
	// Validate the field of the inner voice within the outgoing payment,
	// these must also adhere to the same constraints as regular invoices.
	if err := validateInvoice(&payment.Invoice); err != nil {
		return 0, err
	}

	// We first serialize the payment before starting the database
	// transaction so we can avoid creating a DB payment in the case of a
	// serialization error.
	var b bytes.Buffer
	if err := serializeOutgoingPayment(&b, payment); err != nil {
		return 0, err
	}
	paymentBytes := b.Bytes()

	var paymentAddIndex uint64
	err := db.Batch(func(tx *bbolt.Tx) error {
		payments, err := tx.CreateBucketIfNotExists(paymentBucket)
		if err != nil {
			return err
		}
		paymentIndex, err := payments.CreateBucketIfNotExists(
			paymentIndexBucket,
		)
		if err != nil {
			return err
		}
		addIndex, err := payments.CreateBucketIfNotExists(
			addPaymentIndexBucket,
		)
		if err != nil {
			return err
		}

		// If the current running payment ID counter hasn't yet been
		// created, then create it now.
		var paymentNum uint32
		paymentCounter := paymentIndex.Get(numPaymentsKey)
		if paymentCounter == nil {
			var scratch [4]byte
			byteOrder.PutUint32(scratch[:], paymentNum)
			err := paymentIndex.Put(numPaymentsKey, scratch[:])
			if err != nil {
				return nil
			}
		} else {
			paymentNum = byteOrder.Uint32(paymentCounter)
		}

		newIndex, err := putPayment(
			payments, paymentIndex, addIndex, payment, paymentBytes, paymentNum,
		)
		if err != nil {
			return err
		}

		paymentAddIndex = newIndex

		return nil
	})
	if err != nil {
		return 0, err
	}

	return paymentAddIndex, nil
}

// FetchAllPayments returns all outgoing payments in DB.
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

// LookupPayment attempts to look up an payment according to its 32 byte
// payment hash.
func (db *DB) LookupPayment(paymentHash [32]byte) (*OutgoingPayment, error) {
	var payment *OutgoingPayment
	var err error

	err = db.View(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(paymentBucket)
		if payments == nil {
			return ErrNoPaymentsCreated
		}

		paymentIndex := payments.Bucket(paymentIndexBucket)
		if paymentIndex == nil {
			return ErrNoPaymentsCreated
		}

		// Check the payment index to see if a payment to this
		// hash exists within the DB.
		paymentNum := paymentIndex.Get(paymentHash[:])
		if paymentNum == nil {
			return ErrPaymentNotFound
		}

		payment, err = fetchPayment(paymentNum, payments)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	fmt.Printf("\n\n%+v\n\n", payment)
	return payment, nil
}

// getPaymentByHash gets a paymentBucket and a paymentHash and returns the
// payment if matches paymentHash
func getPaymentByHash(payments *bbolt.Bucket, paymentHash [32]byte) (*OutgoingPayment, error) {
	var paymentResponse *OutgoingPayment
	var err error

	err = payments.ForEach(func(_, payment []byte) error {
		// If the value is nil, then we ignore it as it may be
		// a sub-bucket.
		if payment == nil {
			return nil
		}

		reader := bytes.NewReader(payment)
		paymentTemp, err := deserializeOutgoingPayment(reader)
		if err != nil {
			return err
		}

		isPayment := checkPaymentHash(paymentHash, paymentTemp.PaymentPreimage)
		if isPayment {
			paymentResponse = paymentTemp
		}

		return nil
	})

	if err != nil {
		return nil, err
	}
	return paymentResponse, nil

}

func checkPaymentHash(paymentHash, paymentPreimage [32]byte) bool {
	hashToCheck := sha256.Sum256(paymentPreimage[:])
	if bytes.Equal(paymentHash[:], hashToCheck[:]) {
		return true
	}

	return false
}

// DeleteAllPayments deletes all payments from DB.
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

// UpdatePaymentStatus sets the payment status for outgoing/finished payments in
// local database.
func (db *DB) UpdatePaymentStatus(paymentHash [32]byte, status PaymentStatus) error {
	return db.Batch(func(tx *bbolt.Tx) error {
		return UpdatePaymentStatusTx(tx, paymentHash, status)
	})
}

// UpdatePaymentStatusTx is a helper method that sets the payment status for
// outgoing/finished payments in the local database. This method accepts a
// boltdb transaction such that the operation can be composed into other
// database transactions.
func UpdatePaymentStatusTx(tx *bbolt.Tx,
	paymentHash [32]byte, status PaymentStatus) error {

	paymentStatuses, err := tx.CreateBucketIfNotExists(paymentStatusBucket)
	if err != nil {
		return err
	}

	return paymentStatuses.Put(paymentHash[:], status.Bytes())
}

// FetchPaymentStatus returns the payment status for outgoing payment.
// If status of the payment isn't found, it will default to "StatusGrounded".
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

func putPayment(payments, paymentIndex, addIndex *bbolt.Bucket, payment *OutgoingPayment,
	paymentBytes []byte, paymentNum uint32) (uint64, error) {

	// We use BigEndian for keys as it orders keys in
	// ascending order. This allows bucket scans to order payments
	// in the order in which they were created.
	paymentIDBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(paymentIDBytes, paymentNum)

	// Add the payment hash to the payment index. This will let us quickly
	// identify a settled payment.
	paymentHash := sha256.Sum256(payment.PaymentPreimage[:])
	err := paymentIndex.Put(paymentHash[:], paymentIDBytes[:])
	if err != nil {
		return 0, err
	}

	// Next, we'll obtain the next add payment index (sequence
	// number), so we can properly place this payment within this
	// event stream.
	nextAddSeqNo, err := addIndex.NextSequence()
	if err != nil {
		return 0, err
	}

	// With the next sequence obtained, we'll updating the event series in
	// the add index bucket to map this current add counter to the index of
	// this new invoice.
	var seqNoBytes [8]byte
	byteOrder.PutUint64(seqNoBytes[:], nextAddSeqNo)
	if err := addIndex.Put(seqNoBytes[:], paymentIDBytes[:]); err != nil {
		return 0, err
	}

	if err := payments.Put(paymentIDBytes[:], paymentBytes); err != nil {
		return 0, err
	}

	return nextAddSeqNo, nil
}

func fetchPayment(paymentNum []byte, payments *bbolt.Bucket) (*OutgoingPayment, error) {
	paymentBytes := payments.Get(paymentNum)
	if paymentBytes == nil {
		return nil, ErrPaymentNotFound
	}

	paymentReader := bytes.NewReader(paymentBytes)

	return deserializeOutgoingPayment(paymentReader)
}

package channeldb

import (
	"bytes"
	"encoding/binary"
	"io"

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
	//
	// NOTE: Deprecated. Kept around for migration purposes.
	paymentBucket = []byte("payments")

	// paymentStatusBucket is the name of the bucket within the database
	// that stores the status of a payment indexed by the payment's
	// preimage.
	//
	// NOTE: Deprecated. Kept around for migration purposes.
	paymentStatusBucket = []byte("payment-status")
)

// outgoingPayment represents a successful payment between the daemon and a
// remote node. Details such as the total fee paid, and the time of the payment
// are stored.
//
// NOTE: Deprecated. Kept around for migration purposes.
type outgoingPayment struct {
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

// addPayment saves a successful payment to the database. It is assumed that
// all payment are sent using unique payment hashes.
//
// NOTE: Deprecated. Kept around for migration purposes.
func (db *DB) addPayment(payment *outgoingPayment) error {
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

// fetchAllPayments returns all outgoing payments in DB.
//
// NOTE: Deprecated. Kept around for migration purposes.
func (db *DB) fetchAllPayments() ([]*outgoingPayment, error) {
	var payments []*outgoingPayment

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

// fetchPaymentStatus returns the payment status for outgoing payment.
// If status of the payment isn't found, it will default to "StatusUnknown".
//
// NOTE: Deprecated. Kept around for migration purposes.
func (db *DB) fetchPaymentStatus(paymentHash [32]byte) (PaymentStatus, error) {
	var paymentStatus = StatusUnknown
	err := db.View(func(tx *bbolt.Tx) error {
		var err error
		paymentStatus, err = fetchPaymentStatusTx(tx, paymentHash)
		return err
	})
	if err != nil {
		return StatusUnknown, err
	}

	return paymentStatus, nil
}

// fetchPaymentStatusTx is a helper method that returns the payment status for
// outgoing payment.  If status of the payment isn't found, it will default to
// "StatusUnknown". It accepts the boltdb transactions such that this method
// can be composed into other atomic operations.
//
// NOTE: Deprecated. Kept around for migration purposes.
func fetchPaymentStatusTx(tx *bbolt.Tx, paymentHash [32]byte) (PaymentStatus, error) {
	// The default status for all payments that aren't recorded in database.
	var paymentStatus = StatusUnknown

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

func serializeOutgoingPayment(w io.Writer, p *outgoingPayment) error {
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

func deserializeOutgoingPayment(r io.Reader) (*outgoingPayment, error) {
	var scratch [8]byte

	p := &outgoingPayment{}

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

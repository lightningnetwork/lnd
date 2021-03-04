package migration_01_to_11

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/lntypes"
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

	return kvdb.Update(db, func(tx kvdb.RwTx) error {
		payments, err := tx.CreateTopLevelBucket(paymentBucket)
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
	}, func() {})
}

// fetchAllPayments returns all outgoing payments in DB.
//
// NOTE: Deprecated. Kept around for migration purposes.
func (db *DB) fetchAllPayments() ([]*outgoingPayment, error) {
	var payments []*outgoingPayment

	err := kvdb.View(db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(paymentBucket)
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
	}, func() {
		payments = nil
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
	err := kvdb.View(db, func(tx kvdb.RTx) error {
		var err error
		paymentStatus, err = fetchPaymentStatusTx(tx, paymentHash)
		return err
	}, func() {
		paymentStatus = StatusUnknown
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
func fetchPaymentStatusTx(tx kvdb.RTx, paymentHash [32]byte) (PaymentStatus, error) {
	// The default status for all payments that aren't recorded in database.
	var paymentStatus = StatusUnknown

	bucket := tx.ReadBucket(paymentStatusBucket)
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

	if err := serializeInvoiceLegacy(w, &p.Invoice); err != nil {
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

	inv, err := deserializeInvoiceLegacy(r)
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

// serializePaymentAttemptInfoMigration9 is the serializePaymentAttemptInfo
// version as existed when migration #9 was created. We keep this around, along
// with the methods below to ensure that clients that upgrade will use the
// correct version of this method.
func serializePaymentAttemptInfoMigration9(w io.Writer, a *PaymentAttemptInfo) error {
	if err := WriteElements(w, a.PaymentID, a.SessionKey); err != nil {
		return err
	}

	if err := serializeRouteMigration9(w, a.Route); err != nil {
		return err
	}

	return nil
}

func serializeHopMigration9(w io.Writer, h *Hop) error {
	if err := WriteElements(w,
		h.PubKeyBytes[:], h.ChannelID, h.OutgoingTimeLock,
		h.AmtToForward,
	); err != nil {
		return err
	}

	return nil
}

func serializeRouteMigration9(w io.Writer, r Route) error {
	if err := WriteElements(w,
		r.TotalTimeLock, r.TotalAmount, r.SourcePubKey[:],
	); err != nil {
		return err
	}

	if err := WriteElements(w, uint32(len(r.Hops))); err != nil {
		return err
	}

	for _, h := range r.Hops {
		if err := serializeHopMigration9(w, h); err != nil {
			return err
		}
	}

	return nil
}

func deserializePaymentAttemptInfoMigration9(r io.Reader) (*PaymentAttemptInfo, error) {
	a := &PaymentAttemptInfo{}
	err := ReadElements(r, &a.PaymentID, &a.SessionKey)
	if err != nil {
		return nil, err
	}
	a.Route, err = deserializeRouteMigration9(r)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func deserializeRouteMigration9(r io.Reader) (Route, error) {
	rt := Route{}
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

	var hops []*Hop
	for i := uint32(0); i < numHops; i++ {
		hop, err := deserializeHopMigration9(r)
		if err != nil {
			return rt, err
		}
		hops = append(hops, hop)
	}
	rt.Hops = hops

	return rt, nil
}

func deserializeHopMigration9(r io.Reader) (*Hop, error) {
	h := &Hop{}

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

// fetchPaymentsMigration9 returns all sent payments found in the DB using the
// payment attempt info format that was present as of migration #9. We need
// this as otherwise, the current FetchPayments version will use the latest
// decoding format. Note that we only need this for the
// TestOutgoingPaymentsMigration migration test case.
func (db *DB) fetchPaymentsMigration9() ([]*Payment, error) {
	var payments []*Payment

	err := kvdb.View(db, func(tx kvdb.RTx) error {
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

			p, err := fetchPaymentMigration9(bucket)
			if err != nil {
				return err
			}

			payments = append(payments, p)

			// For older versions of lnd, duplicate payments to a
			// payment has was possible. These will be found in a
			// sub-bucket indexed by their sequence number if
			// available.
			dup := bucket.NestedReadBucket(paymentDuplicateBucket)
			if dup == nil {
				return nil
			}

			return dup.ForEach(func(k, v []byte) error {
				subBucket := dup.NestedReadBucket(k)
				if subBucket == nil {
					// We one bucket for each duplicate to
					// be found.
					return fmt.Errorf("non bucket element" +
						"in duplicate bucket")
				}

				p, err := fetchPaymentMigration9(subBucket)
				if err != nil {
					return err
				}

				payments = append(payments, p)
				return nil
			})
		})
	}, func() {
		payments = nil
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

func fetchPaymentMigration9(bucket kvdb.RBucket) (*Payment, error) {
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
		p.Attempt, err = deserializePaymentAttemptInfoMigration9(r)
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

	// Get failure reason if available.
	b = bucket.Get(paymentFailInfoKey)
	if b != nil {
		reason := FailureReason(b[0])
		p.Failure = &reason
	}

	return p, nil
}

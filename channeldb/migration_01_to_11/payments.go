package migration_01_to_11

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/tlv"
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

	// paymentFailInfoKey is a key used in the payment's sub-bucket to
	// store information about the reason a payment failed.
	paymentFailInfoKey = []byte("payment-fail-info")
)

// FailureReason encodes the reason a payment ultimately failed.
type FailureReason byte

const (
	// FailureReasonTimeout indicates that the payment did timeout before a
	// successful payment attempt was made.
	FailureReasonTimeout FailureReason = 0

	// FailureReasonNoRoute indicates no successful route to the
	// destination was found during path finding.
	FailureReasonNoRoute FailureReason = 1

	// FailureReasonError indicates that an unexpected error happened during
	// payment.
	FailureReasonError FailureReason = 2

	// FailureReasonIncorrectPaymentDetails indicates that either the hash
	// is unknown or the final cltv delta or amount is incorrect.
	FailureReasonIncorrectPaymentDetails FailureReason = 3

	// TODO(halseth): cancel state.

	// TODO(joostjager): Add failure reasons for:
	// LocalLiquidityInsufficient, RemoteCapacityInsufficient.
)

// String returns a human readable FailureReason
func (r FailureReason) String() string {
	switch r {
	case FailureReasonTimeout:
		return "timeout"
	case FailureReasonNoRoute:
		return "no_route"
	case FailureReasonError:
		return "error"
	case FailureReasonIncorrectPaymentDetails:
		return "incorrect_payment_details"
	}

	return "unknown"
}

// PaymentStatus represent current status of payment
type PaymentStatus byte

const (
	// StatusUnknown is the status where a payment has never been initiated
	// and hence is unknown.
	StatusUnknown PaymentStatus = 0

	// StatusInFlight is the status where a payment has been initiated, but
	// a response has not been received.
	StatusInFlight PaymentStatus = 1

	// StatusSucceeded is the status where a payment has been initiated and
	// the payment was completed successfully.
	StatusSucceeded PaymentStatus = 2

	// StatusFailed is the status where a payment has been initiated and a
	// failure result has come back.
	StatusFailed PaymentStatus = 3
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
	case StatusUnknown, StatusInFlight, StatusSucceeded, StatusFailed:
		*ps = PaymentStatus(status[0])
	default:
		return errors.New("unknown payment status")
	}

	return nil
}

// String returns readable representation of payment status.
func (ps PaymentStatus) String() string {
	switch ps {
	case StatusUnknown:
		return "Unknown"
	case StatusInFlight:
		return "In Flight"
	case StatusSucceeded:
		return "Succeeded"
	case StatusFailed:
		return "Failed"
	default:
		return "Unknown"
	}
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
	Route Route
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

	// Failure is a failure reason code indicating the reason the payment
	// failed. It is only non-nil for failed payments.
	//
	// NOTE: Can be nil if payment is not failed.
	Failure *FailureReason
}

// FetchPayments returns all sent payments found in the DB.
func (db *DB) FetchPayments() ([]*Payment, error) {
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

			p, err := fetchPayment(bucket)
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

				p, err := fetchPayment(subBucket)
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

func fetchPayment(bucket kvdb.RBucket) (*Payment, error) {
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

	// Get failure reason if available.
	b = bucket.Get(paymentFailInfoKey)
	if b != nil {
		reason := FailureReason(b[0])
		p.Failure = &reason
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

	if err := SerializeRoute(w, a.Route); err != nil {
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
	a.Route, err = DeserializeRoute(r)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func serializeHop(w io.Writer, h *Hop) error {
	if err := WriteElements(w,
		h.PubKeyBytes[:], h.ChannelID, h.OutgoingTimeLock,
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

	// Otherwise, we'll transform our slice of records into a map of the
	// raw bytes, then serialize them in-line with a length (number of
	// elements) prefix.
	mapRecords, err := tlv.RecordsToMap(h.TLVRecords)
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

func deserializeHop(r io.Reader) (*Hop, error) {
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

	h.TLVRecords = tlv.MapToRecords(tlvMap)

	return h, nil
}

// SerializeRoute serializes a route.
func SerializeRoute(w io.Writer, r Route) error {
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

// DeserializeRoute deserializes a route.
func DeserializeRoute(r io.Reader) (Route, error) {
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
		hop, err := deserializeHop(r)
		if err != nil {
			return rt, err
		}
		hops = append(hops, hop)
	}
	rt.Hops = hops

	return rt, nil
}

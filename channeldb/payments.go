package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
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

	// FailureReasonPaymentDetails indicates that either the hash is unknown
	// or the final cltv delta or amount is incorrect.
	FailureReasonPaymentDetails FailureReason = 3

	// FailureReasonInsufficientBalance indicates that we didn't have enough
	// balance to complete the payment.
	FailureReasonInsufficientBalance FailureReason = 4

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
	case FailureReasonPaymentDetails:
		return "incorrect_payment_details"
	case FailureReasonInsufficientBalance:
		return "insufficient_balance"
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

	// CreationTime is the time when this payment was initiated.
	CreationTime time.Time

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

	// Preimage is the preimage of a successful payment. This serves as a
	// proof of payment. It will only be non-nil for settled payments.
	//
	// NOTE: Can be nil if payment is not settled.
	Preimage *lntypes.Preimage

	// Failure is a failure reason code indicating the reason the payment
	// failed. It is only non-nil for failed payments.
	//
	// NOTE: Can be nil if payment is not failed.
	Failure *FailureReason
}

// ToMPPayment converts a legacy payment into an MPPayment.
func (p *Payment) ToMPPayment() *MPPayment {
	var (
		htlcs   []HTLCAttempt
		reason  *FailureReason
		settle  *HTLCSettleInfo
		failure *HTLCFailInfo
	)

	// Promote the payment failure to a proper fail struct, if it exists.
	if p.Failure != nil {
		// NOTE: FailTime is not set for legacy payments.
		failure = &HTLCFailInfo{}
		reason = p.Failure
	}

	// Promote the payment preimage to proper settle struct, if it exists.
	if p.Preimage != nil {
		// NOTE: SettleTime is not set for legacy payments.
		settle = &HTLCSettleInfo{
			Preimage: *p.Preimage,
		}
	}

	// Either a settle or a failure may be set, but not both.
	if settle != nil && failure != nil {
		panic("htlc attempt has both settle and failure info")
	}

	// Populate a single HTLC on the MPPayment if an attempt exists on the
	// legacy payment. If none exists we will leave the attempt info blank
	// since we cannot recover it.
	if p.Attempt != nil {
		// NOTE: AttemptTime is not set for legacy payments.
		htlcs = []HTLCAttempt{
			{
				PaymentID:  p.Attempt.PaymentID,
				SessionKey: p.Attempt.SessionKey,
				Route:      p.Attempt.Route,
				Settle:     settle,
				Failure:    failure,
			},
		}
	}

	return &MPPayment{
		sequenceNum: p.sequenceNum,
		Info: &PaymentCreationInfo{
			PaymentHash:    p.Info.PaymentHash,
			Value:          p.Info.Value,
			CreationTime:   p.Info.CreationTime,
			PaymentRequest: p.Info.PaymentRequest,
		},
		HTLCs:         htlcs,
		FailureReason: reason,
		Status:        p.Status,
	}
}

// FetchPayments returns all sent payments found in the DB.
//
// nolint: dupl
func (db *DB) FetchPayments() ([]*MPPayment, error) {
	var payments []*MPPayment

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

func fetchPayment(bucket *bbolt.Bucket) (*MPPayment, error) {
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
		p.Preimage = &preimg
	}

	// Get failure reason if available.
	b = bucket.Get(paymentFailInfoKey)
	if b != nil {
		reason := FailureReason(b[0])
		p.Failure = &reason
	}

	return p.ToMPPayment(), nil
}

// DeletePayments deletes all completed and failed payments from the DB.
func (db *DB) DeletePayments() error {
	return db.Update(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		var deleteBuckets [][]byte
		err := payments.ForEach(func(k, _ []byte) error {
			bucket := payments.Bucket(k)
			if bucket == nil {
				// We only expect sub-buckets to be found in
				// this top-level bucket.
				return fmt.Errorf("non bucket element in " +
					"payments bucket")
			}

			// If the status is InFlight, we cannot safely delete
			// the payment information, so we return early.
			paymentStatus := fetchPaymentStatus(bucket)
			if paymentStatus == StatusInFlight {
				return nil
			}

			deleteBuckets = append(deleteBuckets, k)
			return nil
		})
		if err != nil {
			return err
		}

		for _, k := range deleteBuckets {
			if err := payments.DeleteBucket(k); err != nil {
				return err
			}
		}

		return nil
	})
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

	byteOrder.PutUint64(scratch[:], uint64(c.CreationTime.Unix()))
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
	c.CreationTime = time.Unix(int64(byteOrder.Uint64(scratch[:])), 0)

	if _, err := io.ReadFull(r, scratch[:4]); err != nil {
		return nil, err
	}

	reqLen := uint32(byteOrder.Uint32(scratch[:4]))
	payReq := make([]byte, reqLen)
	if reqLen > 0 {
		if _, err := io.ReadFull(r, payReq); err != nil {
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

	return rt, nil
}

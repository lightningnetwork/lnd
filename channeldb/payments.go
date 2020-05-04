package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
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
	//      |        |--fail-info-key: <(optional) fail info>
	//      |        |
	//      |        |--payment-htlcs-bucket (shard-bucket)
	//      |        |        |
	//      |        |        |-- <htlc attempt ID>
	//      |        |        |       |--htlc-attempt-info-key: <htlc attempt info>
	//      |        |        |       |--htlc-settle-info-key: <(optional) settle info>
	//      |        |        |       |--htlc-fail-info-key: <(optional) fail info>
	//      |        |        |
	//      |        |        |-- <htlc attempt ID>
	//      |        |        |       |
	//      |        |       ...     ...
	//      |        |
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

	// paymentSequenceKey is a key used in the payment's sub-bucket to
	// store the sequence number of the payment.
	paymentSequenceKey = []byte("payment-sequence-key")

	// paymentCreationInfoKey is a key used in the payment's sub-bucket to
	// store the creation info of the payment.
	paymentCreationInfoKey = []byte("payment-creation-info")

	// paymentHtlcsBucket is a bucket where we'll store the information
	// about the HTLCs that were attempted for a payment.
	paymentHtlcsBucket = []byte("payment-htlcs-bucket")

	// htlcAttemptInfoKey is a key used in a HTLC's sub-bucket to store the
	// info about the attempt that was done for the HTLC in question.
	htlcAttemptInfoKey = []byte("htlc-attempt-info")

	// htlcSettleInfoKey is a key used in a HTLC's sub-bucket to store the
	// settle info, if any.
	htlcSettleInfoKey = []byte("htlc-settle-info")

	// htlcFailInfoKey is a key used in a HTLC's sub-bucket to store
	// failure information, if any.
	htlcFailInfoKey = []byte("htlc-fail-info")

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

// Error returns a human readable error string for the FailureReason.
func (r FailureReason) Error() string {
	return r.String()
}

// String returns a human readable FailureReason.
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

// FetchPayments returns all sent payments found in the DB.
//
// nolint: dupl
func (db *DB) FetchPayments() ([]*MPPayment, error) {
	var payments []*MPPayment

	err := kvdb.View(db, func(tx kvdb.ReadTx) error {
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
			duplicatePayments, err := fetchDuplicatePayments(bucket)
			if err != nil {
				return err
			}

			payments = append(payments, duplicatePayments...)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	// Before returning, sort the payments by their sequence number.
	sort.Slice(payments, func(i, j int) bool {
		return payments[i].SequenceNum < payments[j].SequenceNum
	})

	return payments, nil
}

func fetchCreationInfo(bucket kvdb.ReadBucket) (*PaymentCreationInfo, error) {
	b := bucket.Get(paymentCreationInfoKey)
	if b == nil {
		return nil, fmt.Errorf("creation info not found")
	}

	r := bytes.NewReader(b)
	return deserializePaymentCreationInfo(r)
}

func fetchPayment(bucket kvdb.ReadBucket) (*MPPayment, error) {
	seqBytes := bucket.Get(paymentSequenceKey)
	if seqBytes == nil {
		return nil, fmt.Errorf("sequence number not found")
	}

	sequenceNum := binary.BigEndian.Uint64(seqBytes)

	// Get the PaymentCreationInfo.
	creationInfo, err := fetchCreationInfo(bucket)
	if err != nil {
		return nil, err

	}

	var htlcs []HTLCAttempt
	htlcsBucket := bucket.NestedReadBucket(paymentHtlcsBucket)
	if htlcsBucket != nil {
		// Get the payment attempts. This can be empty.
		htlcs, err = fetchHtlcAttempts(htlcsBucket)
		if err != nil {
			return nil, err
		}
	}

	// Get failure reason if available.
	var failureReason *FailureReason
	b := bucket.Get(paymentFailInfoKey)
	if b != nil {
		reason := FailureReason(b[0])
		failureReason = &reason
	}

	// Go through all HTLCs for this payment, noting whether we have any
	// settled HTLC, and any still in-flight.
	var inflight, settled bool
	for _, h := range htlcs {
		if h.Failure != nil {
			continue
		}

		if h.Settle != nil {
			settled = true
			continue
		}

		// If any of the HTLCs are not failed nor settled, we
		// still have inflight HTLCs.
		inflight = true
	}

	// Use the DB state to determine the status of the payment.
	var paymentStatus PaymentStatus

	switch {

	// If any of the the HTLCs did succeed and there are no HTLCs in
	// flight, the payment succeeded.
	case !inflight && settled:
		paymentStatus = StatusSucceeded

	// If we have no in-flight HTLCs, and the payment failure is set, the
	// payment is considered failed.
	case !inflight && failureReason != nil:
		paymentStatus = StatusFailed

	// Otherwise it is still in flight.
	default:
		paymentStatus = StatusInFlight
	}

	return &MPPayment{
		SequenceNum:   sequenceNum,
		Info:          creationInfo,
		HTLCs:         htlcs,
		FailureReason: failureReason,
		Status:        paymentStatus,
	}, nil
}

// fetchHtlcAttempts retrives all htlc attempts made for the payment found in
// the given bucket.
func fetchHtlcAttempts(bucket kvdb.ReadBucket) ([]HTLCAttempt, error) {
	htlcs := make([]HTLCAttempt, 0)

	err := bucket.ForEach(func(k, _ []byte) error {
		aid := byteOrder.Uint64(k)
		htlcBucket := bucket.NestedReadBucket(k)

		attemptInfo, err := fetchHtlcAttemptInfo(
			htlcBucket,
		)
		if err != nil {
			return err
		}
		attemptInfo.AttemptID = aid

		htlc := HTLCAttempt{
			HTLCAttemptInfo: *attemptInfo,
		}

		// Settle info might be nil.
		htlc.Settle, err = fetchHtlcSettleInfo(htlcBucket)
		if err != nil {
			return err
		}

		// Failure info might be nil.
		htlc.Failure, err = fetchHtlcFailInfo(htlcBucket)
		if err != nil {
			return err
		}

		htlcs = append(htlcs, htlc)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return htlcs, nil
}

// fetchHtlcAttemptInfo fetches the payment attempt info for this htlc from the
// bucket.
func fetchHtlcAttemptInfo(bucket kvdb.ReadBucket) (*HTLCAttemptInfo, error) {
	b := bucket.Get(htlcAttemptInfoKey)
	if b == nil {
		return nil, errNoAttemptInfo
	}

	r := bytes.NewReader(b)
	return deserializeHTLCAttemptInfo(r)
}

// fetchHtlcSettleInfo retrieves the settle info for the htlc. If the htlc isn't
// settled, nil is returned.
func fetchHtlcSettleInfo(bucket kvdb.ReadBucket) (*HTLCSettleInfo, error) {
	b := bucket.Get(htlcSettleInfoKey)
	if b == nil {
		// Settle info is optional.
		return nil, nil
	}

	r := bytes.NewReader(b)
	return deserializeHTLCSettleInfo(r)
}

// fetchHtlcFailInfo retrieves the failure info for the htlc. If the htlc hasn't
// failed, nil is returned.
func fetchHtlcFailInfo(bucket kvdb.ReadBucket) (*HTLCFailInfo, error) {
	b := bucket.Get(htlcFailInfoKey)
	if b == nil {
		// Fail info is optional.
		return nil, nil
	}

	r := bytes.NewReader(b)
	return deserializeHTLCFailInfo(r)
}

// PaymentsQuery represents a query to the payments database starting or ending
// at a certain offset index. The number of retrieved records can be limited.
type PaymentsQuery struct {
	// IndexOffset determines the starting point of the payments query and
	// is always exclusive. In normal order, the query starts at the next
	// higher (available) index compared to IndexOffset. In reversed order,
	// the query ends at the next lower (available) index compared to the
	// IndexOffset. In the case of a zero index_offset, the query will start
	// with the oldest payment when paginating forwards, or will end with
	// the most recent payment when paginating backwards.
	IndexOffset uint64

	// MaxPayments is the maximal number of payments returned in the
	// payments query.
	MaxPayments uint64

	// Reversed gives a meaning to the IndexOffset. If reversed is set to
	// true, the query will fetch payments with indices lower than the
	// IndexOffset, otherwise, it will return payments with indices greater
	// than the IndexOffset.
	Reversed bool

	// If IncludeIncomplete is true, then return payments that have not yet
	// fully completed. This means that pending payments, as well as failed
	// payments will show up if this field is set to true.
	IncludeIncomplete bool
}

// PaymentsResponse contains the result of a query to the payments database.
// It includes the set of payments that match the query and integers which
// represent the index of the first and last item returned in the series of
// payments. These integers allow callers to resume their query in the event
// that the query's response exceeds the max number of returnable events.
type PaymentsResponse struct {
	// Payments is the set of payments returned from the database for the
	// PaymentsQuery.
	Payments []*MPPayment

	// FirstIndexOffset is the index of the first element in the set of
	// returned MPPayments. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response. The offset can be used to continue reverse pagination.
	FirstIndexOffset uint64

	// LastIndexOffset is the index of the last element in the set of
	// returned MPPayments. Callers can use this to resume their query
	// in the event that the slice has too many events to fit into a single
	// response. The offset can be used to continue forward pagination.
	LastIndexOffset uint64
}

// QueryPayments is a query to the payments database which is restricted
// to a subset of payments by the payments query, containing an offset
// index and a maximum number of returned payments.
func (db *DB) QueryPayments(query PaymentsQuery) (PaymentsResponse, error) {
	var resp PaymentsResponse

	allPayments, err := db.FetchPayments()
	if err != nil {
		return resp, err
	}

	if len(allPayments) == 0 {
		return resp, nil
	}

	indexExclusiveLimit := query.IndexOffset
	// In backward pagination, if the index limit is the default 0 value,
	// we set our limit to maxint to include all payments from the highest
	// sequence number on.
	if query.Reversed && indexExclusiveLimit == 0 {
		indexExclusiveLimit = math.MaxInt64
	}

	for i := range allPayments {
		var payment *MPPayment

		// If we have the max number of payments we want, exit.
		if uint64(len(resp.Payments)) == query.MaxPayments {
			break
		}

		if query.Reversed {
			payment = allPayments[len(allPayments)-1-i]

			// In the reversed direction, skip over all payments
			// that have sequence numbers greater than or equal to
			// the index offset. We skip payments with equal index
			// because the offset is exclusive.
			if payment.SequenceNum >= indexExclusiveLimit {
				continue
			}
		} else {
			payment = allPayments[i]

			// In the forward direction, skip over all payments that
			// have sequence numbers less than or equal to the index
			// offset. We skip payments with equal indexes because
			// the index offset is exclusive.
			if payment.SequenceNum <= indexExclusiveLimit {
				continue
			}
		}

		// To keep compatibility with the old API, we only return
		// non-succeeded payments if requested.
		if payment.Status != StatusSucceeded &&
			!query.IncludeIncomplete {

			continue
		}

		resp.Payments = append(resp.Payments, payment)
	}

	// Need to swap the payments slice order if reversed order.
	if query.Reversed {
		for l, r := 0, len(resp.Payments)-1; l < r; l, r = l+1, r-1 {
			resp.Payments[l], resp.Payments[r] =
				resp.Payments[r], resp.Payments[l]
		}
	}

	// Set the first and last index of the returned payments so that the
	// caller can resume from this point later on.
	if len(resp.Payments) > 0 {
		resp.FirstIndexOffset = resp.Payments[0].SequenceNum
		resp.LastIndexOffset =
			resp.Payments[len(resp.Payments)-1].SequenceNum
	}

	return resp, err
}

// DeletePayments deletes all completed and failed payments from the DB.
func (db *DB) DeletePayments() error {
	return kvdb.Update(db, func(tx kvdb.RwTx) error {
		payments := tx.ReadWriteBucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		var deleteBuckets [][]byte
		err := payments.ForEach(func(k, _ []byte) error {
			bucket := payments.NestedReadWriteBucket(k)
			if bucket == nil {
				// We only expect sub-buckets to be found in
				// this top-level bucket.
				return fmt.Errorf("non bucket element in " +
					"payments bucket")
			}

			// If the status is InFlight, we cannot safely delete
			// the payment information, so we return early.
			paymentStatus, err := fetchPaymentStatus(bucket)
			if err != nil {
				return err
			}

			// If the status is InFlight, we cannot safely delete
			// the payment information, so we return early.
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
			if err := payments.DeleteNestedBucket(k); err != nil {
				return err
			}
		}

		return nil
	})
}

// nolint: dupl
func serializePaymentCreationInfo(w io.Writer, c *PaymentCreationInfo) error {
	var scratch [8]byte

	if _, err := w.Write(c.PaymentHash[:]); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(c.Value))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := serializeTime(w, c.CreationTime); err != nil {
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

	creationTime, err := deserializeTime(r)
	if err != nil {
		return nil, err
	}
	c.CreationTime = creationTime

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

func serializeHTLCAttemptInfo(w io.Writer, a *HTLCAttemptInfo) error {
	if err := WriteElements(w, a.SessionKey); err != nil {
		return err
	}

	if err := SerializeRoute(w, a.Route); err != nil {
		return err
	}

	return serializeTime(w, a.AttemptTime)
}

func deserializeHTLCAttemptInfo(r io.Reader) (*HTLCAttemptInfo, error) {
	a := &HTLCAttemptInfo{}
	err := ReadElements(r, &a.SessionKey)
	if err != nil {
		return nil, err
	}
	a.Route, err = DeserializeRoute(r)
	if err != nil {
		return nil, err
	}

	a.AttemptTime, err = deserializeTime(r)
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

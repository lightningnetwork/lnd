package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb"
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
	//      |        |        |-- ai<htlc attempt ID>: <htlc attempt info>
	//      |        |        |-- si<htlc attempt ID>: <(optional) settle info>
	//      |        |        |-- fi<htlc attempt ID>: <(optional) fail info>
	//      |        |        |
	//      |        |       ...
	//      |        |
	//      |        |
	//      |        |--duplicate-bucket (only for old, completed payments)
	//      |                 |
	//      |                 |-- <seq-num>
	//      |                 |       |--sequence-key: <sequence number>
	//      |                 |       |--creation-info-key: <creation info>
	//      |                 |       |--ai: <attempt info>
	//      |                 |       |--si: <settle info>
	//      |                 |       |--fi: <fail info>
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

	// htlcAttemptInfoKey is the key used as the prefix of an HTLC attempt
	// to store the info about the attempt that was done for the HTLC in
	// question. The HTLC attempt ID is concatenated at the end.
	htlcAttemptInfoKey = []byte("ai")

	// htlcSettleInfoKey is the key used as the prefix of an HTLC attempt
	// settle info, if any. The HTLC attempt ID is concatenated at the end.
	htlcSettleInfoKey = []byte("si")

	// htlcFailInfoKey is the key used as the prefix of an HTLC attempt
	// failure information, if any.The  HTLC attempt ID is concatenated at
	// the end.
	htlcFailInfoKey = []byte("fi")

	// paymentFailInfoKey is a key used in the payment's sub-bucket to
	// store information about the reason a payment failed.
	paymentFailInfoKey = []byte("payment-fail-info")

	// paymentsIndexBucket is the name of the top-level bucket within the
	// database that stores an index of payment sequence numbers to its
	// payment hash.
	// payments-sequence-index-bucket
	// 	|--<sequence-number>: <payment hash>
	// 	|--...
	// 	|--<sequence-number>: <payment hash>
	paymentsIndexBucket = []byte("payments-index-bucket")
)

var (
	// ErrNoSequenceNumber is returned if we lookup a payment which does
	// not have a sequence number.
	ErrNoSequenceNumber = errors.New("sequence number not found")

	// ErrDuplicateNotFound is returned when we lookup a payment by its
	// index and cannot find a payment with a matching sequence number.
	ErrDuplicateNotFound = errors.New("duplicate payment not found")

	// ErrNoDuplicateBucket is returned when we expect to find duplicates
	// when looking up a payment from its index, but the payment does not
	// have any.
	ErrNoDuplicateBucket = errors.New("expected duplicate bucket")

	// ErrNoDuplicateNestedBucket is returned if we do not find duplicate
	// payments in their own sub-bucket.
	ErrNoDuplicateNestedBucket = errors.New("nested duplicate bucket not " +
		"found")
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

// PaymentCreationInfo is the information necessary to have ready when
// initiating a payment, moving it into state InFlight.
type PaymentCreationInfo struct {
	// PaymentIdentifier is the hash this payment is paying to in case of
	// non-AMP payments, and the SetID for AMP payments.
	PaymentIdentifier lntypes.Hash

	// Value is the amount we are paying.
	Value lnwire.MilliSatoshi

	// CreationTime is the time when this payment was initiated.
	CreationTime time.Time

	// PaymentRequest is the full payment request, if any.
	PaymentRequest []byte
}

// htlcBucketKey creates a composite key from prefix and id where the result is
// simply the two concatenated.
func htlcBucketKey(prefix, id []byte) []byte {
	key := make([]byte, len(prefix)+len(id))
	copy(key, prefix)
	copy(key[len(prefix):], id)
	return key
}

// FetchPayments returns all sent payments found in the DB.
//
// nolint: dupl
func (d *DB) FetchPayments() ([]*MPPayment, error) {
	var payments []*MPPayment

	err := kvdb.View(d, func(tx kvdb.RTx) error {
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
	}, func() {
		payments = nil
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

func fetchCreationInfo(bucket kvdb.RBucket) (*PaymentCreationInfo, error) {
	b := bucket.Get(paymentCreationInfoKey)
	if b == nil {
		return nil, fmt.Errorf("creation info not found")
	}

	r := bytes.NewReader(b)
	return deserializePaymentCreationInfo(r)
}

func fetchPayment(bucket kvdb.RBucket) (*MPPayment, error) {
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

// fetchHtlcAttempts retrieves all htlc attempts made for the payment found in
// the given bucket.
func fetchHtlcAttempts(bucket kvdb.RBucket) ([]HTLCAttempt, error) {
	htlcsMap := make(map[uint64]*HTLCAttempt)

	attemptInfoCount := 0
	err := bucket.ForEach(func(k, v []byte) error {
		aid := byteOrder.Uint64(k[len(k)-8:])

		if _, ok := htlcsMap[aid]; !ok {
			htlcsMap[aid] = &HTLCAttempt{}
		}

		var err error
		switch {
		case bytes.HasPrefix(k, htlcAttemptInfoKey):
			attemptInfo, err := readHtlcAttemptInfo(v)
			if err != nil {
				return err
			}

			attemptInfo.AttemptID = aid
			htlcsMap[aid].HTLCAttemptInfo = *attemptInfo
			attemptInfoCount++

		case bytes.HasPrefix(k, htlcSettleInfoKey):
			htlcsMap[aid].Settle, err = readHtlcSettleInfo(v)
			if err != nil {
				return err
			}

		case bytes.HasPrefix(k, htlcFailInfoKey):
			htlcsMap[aid].Failure, err = readHtlcFailInfo(v)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("unknown htlc attempt key")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sanity check that all htlcs have an attempt info.
	if attemptInfoCount != len(htlcsMap) {
		return nil, errNoAttemptInfo
	}

	keys := make([]uint64, len(htlcsMap))
	i := 0
	for k := range htlcsMap {
		keys[i] = k
		i++
	}

	// Sort HTLC attempts by their attempt ID. This is needed because in the
	// DB we store the attempts with keys prefixed by their status which
	// changes order (groups them together by status).
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})

	htlcs := make([]HTLCAttempt, len(htlcsMap))
	for i, key := range keys {
		htlcs[i] = *htlcsMap[key]
	}

	return htlcs, nil
}

// readHtlcAttemptInfo reads the payment attempt info for this htlc.
func readHtlcAttemptInfo(b []byte) (*HTLCAttemptInfo, error) {
	r := bytes.NewReader(b)
	return deserializeHTLCAttemptInfo(r)
}

// readHtlcSettleInfo reads the settle info for the htlc. If the htlc isn't
// settled, nil is returned.
func readHtlcSettleInfo(b []byte) (*HTLCSettleInfo, error) {
	r := bytes.NewReader(b)
	return deserializeHTLCSettleInfo(r)
}

// readHtlcFailInfo reads the failure info for the htlc. If the htlc hasn't
// failed, nil is returned.
func readHtlcFailInfo(b []byte) (*HTLCFailInfo, error) {
	r := bytes.NewReader(b)
	return deserializeHTLCFailInfo(r)
}

// fetchFailedHtlcKeys retrieves the bucket keys of all failed HTLCs of a
// payment bucket.
func fetchFailedHtlcKeys(bucket kvdb.RBucket) ([][]byte, error) {
	htlcsBucket := bucket.NestedReadBucket(paymentHtlcsBucket)

	var htlcs []HTLCAttempt
	var err error
	if htlcsBucket != nil {
		htlcs, err = fetchHtlcAttempts(htlcsBucket)
		if err != nil {
			return nil, err
		}
	}

	// Now iterate though them and save the bucket keys for the failed
	// HTLCs.
	var htlcKeys [][]byte
	for _, h := range htlcs {
		if h.Failure == nil {
			continue
		}

		htlcKeyBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(htlcKeyBytes, h.AttemptID)

		htlcKeys = append(htlcKeys, htlcKeyBytes)
	}

	return htlcKeys, nil
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

	// CountTotal indicates that all payments currently present in the
	// payment index (complete and incomplete) should be counted.
	CountTotal bool

	// CreationDateStart, if set, filters out all payments with a creation
	// date greater than or euqal to it.
	CreationDateStart time.Time

	// CreationDateEnd, if set, filters out all payments with a creation
	// date less than or euqal to it.
	CreationDateEnd time.Time
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

	// TotalCount represents the total number of payments that are currently
	// stored in the payment database. This will only be set if the
	// CountTotal field in the query was set to true.
	TotalCount uint64
}

// QueryPayments is a query to the payments database which is restricted
// to a subset of payments by the payments query, containing an offset
// index and a maximum number of returned payments.
func (d *DB) QueryPayments(query PaymentsQuery) (PaymentsResponse, error) {
	var (
		resp         PaymentsResponse
		startDateSet = !query.CreationDateStart.IsZero()
		endDateSet   = !query.CreationDateEnd.IsZero()
	)

	if err := kvdb.View(d, func(tx kvdb.RTx) error {
		// Get the root payments bucket.
		paymentsBucket := tx.ReadBucket(paymentsRootBucket)
		if paymentsBucket == nil {
			return nil
		}

		// Get the index bucket which maps sequence number -> payment
		// hash and duplicate bool. If we have a payments bucket, we
		// should have an indexes bucket as well.
		indexes := tx.ReadBucket(paymentsIndexBucket)
		if indexes == nil {
			return fmt.Errorf("index bucket does not exist")
		}

		// accumulatePayments gets payments with the sequence number
		// and hash provided and adds them to our list of payments if
		// they meet the criteria of our query. It returns the number
		// of payments that were added.
		accumulatePayments := func(sequenceKey, hash []byte) (bool,
			error) {

			r := bytes.NewReader(hash)
			paymentHash, err := deserializePaymentIndex(r)
			if err != nil {
				return false, err
			}

			payment, err := fetchPaymentWithSequenceNumber(
				tx, paymentHash, sequenceKey,
			)
			if err != nil {
				return false, err
			}

			// To keep compatibility with the old API, we only
			// return non-succeeded payments if requested.
			if payment.Status != StatusSucceeded &&
				!query.IncludeIncomplete {

				return false, err
			}

			// Skip any payments that were created before the
			// specified time.
			if startDateSet && payment.Info.CreationTime.Before(
				query.CreationDateStart,
			) {

				return false, nil
			}

			// Skip any payments that were created after the
			// specified time.
			if endDateSet && payment.Info.CreationTime.After(
				query.CreationDateEnd,
			) {

				return false, nil
			}

			// At this point, we've exhausted the offset, so we'll
			// begin collecting invoices found within the range.
			resp.Payments = append(resp.Payments, payment)
			return true, nil
		}

		// Create a paginator which reads from our sequence index bucket
		// with the parameters provided by the payments query.
		paginator := newPaginator(
			indexes.ReadCursor(), query.Reversed, query.IndexOffset,
			query.MaxPayments,
		)

		// Run a paginated query, adding payments to our response.
		if err := paginator.query(accumulatePayments); err != nil {
			return err
		}

		// Counting the total number of payments is expensive, since we
		// literally have to traverse the cursor linearly, which can
		// take quite a while. So it's an optional query parameter.
		if query.CountTotal {
			var (
				totalPayments uint64
				err           error
			)
			countFn := func(_, _ []byte) error {
				totalPayments++

				return nil
			}

			// In non-boltdb database backends, there's a faster
			// ForAll query that allows for batch fetching items.
			if fastBucket, ok := indexes.(kvdb.ExtendedRBucket); ok {
				err = fastBucket.ForAll(countFn)
			} else {
				err = indexes.ForEach(countFn)
			}
			if err != nil {
				return fmt.Errorf("error counting payments: %v",
					err)
			}

			resp.TotalCount = totalPayments
		}

		return nil
	}, func() {
		resp = PaymentsResponse{}
	}); err != nil {
		return resp, err
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

	return resp, nil
}

// fetchPaymentWithSequenceNumber get the payment which matches the payment hash
// *and* sequence number provided from the database. This is required because
// we previously had more than one payment per hash, so we have multiple indexes
// pointing to a single payment; we want to retrieve the correct one.
func fetchPaymentWithSequenceNumber(tx kvdb.RTx, paymentHash lntypes.Hash,
	sequenceNumber []byte) (*MPPayment, error) {

	// We can now lookup the payment keyed by its hash in
	// the payments root bucket.
	bucket, err := fetchPaymentBucket(tx, paymentHash)
	if err != nil {
		return nil, err
	}

	// A single payment hash can have multiple payments associated with it.
	// We lookup our sequence number first, to determine whether this is
	// the payment we are actually looking for.
	seqBytes := bucket.Get(paymentSequenceKey)
	if seqBytes == nil {
		return nil, ErrNoSequenceNumber
	}

	// If this top level payment has the sequence number we are looking for,
	// return it.
	if bytes.Equal(seqBytes, sequenceNumber) {
		return fetchPayment(bucket)
	}

	// If we were not looking for the top level payment, we are looking for
	// one of our duplicate payments. We need to iterate through the seq
	// numbers in this bucket to find the correct payments. If we do not
	// find a duplicate payments bucket here, something is wrong.
	dup := bucket.NestedReadBucket(duplicatePaymentsBucket)
	if dup == nil {
		return nil, ErrNoDuplicateBucket
	}

	var duplicatePayment *MPPayment
	err = dup.ForEach(func(k, v []byte) error {
		subBucket := dup.NestedReadBucket(k)
		if subBucket == nil {
			// We one bucket for each duplicate to be found.
			return ErrNoDuplicateNestedBucket
		}

		seqBytes := subBucket.Get(duplicatePaymentSequenceKey)
		if seqBytes == nil {
			return err
		}

		// If this duplicate payment is not the sequence number we are
		// looking for, we can continue.
		if !bytes.Equal(seqBytes, sequenceNumber) {
			return nil
		}

		duplicatePayment, err = fetchDuplicatePayment(subBucket)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// If none of the duplicate payments matched our sequence number, we
	// failed to find the payment with this sequence number; something is
	// wrong.
	if duplicatePayment == nil {
		return nil, ErrDuplicateNotFound
	}

	return duplicatePayment, nil
}

// DeletePayment deletes a payment from the DB given its payment hash. If
// failedHtlcsOnly is set, only failed HTLC attempts of the payment will be
// deleted.
func (d *DB) DeletePayment(paymentHash lntypes.Hash,
	failedHtlcsOnly bool) error {

	return kvdb.Update(d, func(tx kvdb.RwTx) error {
		payments := tx.ReadWriteBucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		bucket := payments.NestedReadWriteBucket(paymentHash[:])
		if bucket == nil {
			return fmt.Errorf("non bucket element in payments " +
				"bucket")
		}

		// If the status is InFlight, we cannot safely delete
		// the payment information, so we return early.
		paymentStatus, err := fetchPaymentStatus(bucket)
		if err != nil {
			return err
		}

		// If the status is InFlight, we cannot safely delete
		// the payment information, so we return an error.
		if paymentStatus == StatusInFlight {
			return fmt.Errorf("payment '%v' has status InFlight "+
				"and therefore cannot be deleted",
				paymentHash.String())
		}

		// Delete the failed HTLC attempts we found.
		if failedHtlcsOnly {
			toDelete, err := fetchFailedHtlcKeys(bucket)
			if err != nil {
				return err
			}

			htlcsBucket := bucket.NestedReadWriteBucket(
				paymentHtlcsBucket,
			)

			for _, htlcID := range toDelete {
				err = htlcsBucket.Delete(
					htlcBucketKey(htlcAttemptInfoKey, htlcID),
				)
				if err != nil {
					return err
				}

				err = htlcsBucket.Delete(
					htlcBucketKey(htlcFailInfoKey, htlcID),
				)
				if err != nil {
					return err
				}

				err = htlcsBucket.Delete(
					htlcBucketKey(htlcSettleInfoKey, htlcID),
				)
				if err != nil {
					return err
				}
			}

			return nil
		}

		seqNrs, err := fetchSequenceNumbers(bucket)
		if err != nil {
			return err
		}

		if err := payments.DeleteNestedBucket(paymentHash[:]); err != nil {
			return err
		}

		indexBucket := tx.ReadWriteBucket(paymentsIndexBucket)
		for _, k := range seqNrs {
			if err := indexBucket.Delete(k); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// DeletePayments deletes all completed and failed payments from the DB. If
// failedOnly is set, only failed payments will be considered for deletion. If
// failedHtlsOnly is set, the payment itself won't be deleted, only failed HTLC
// attempts.
func (d *DB) DeletePayments(failedOnly, failedHtlcsOnly bool) error {
	return kvdb.Update(d, func(tx kvdb.RwTx) error {
		payments := tx.ReadWriteBucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		var (
			// deleteBuckets is the set of payment buckets we need
			// to delete.
			deleteBuckets [][]byte

			// deleteIndexes is the set of indexes pointing to these
			// payments that need to be deleted.
			deleteIndexes [][]byte

			// deleteHtlcs maps a payment hash to the HTLC IDs we
			// want to delete for that payment.
			deleteHtlcs = make(map[lntypes.Hash][][]byte)
		)
		err := payments.ForEach(func(k, _ []byte) error {
			bucket := payments.NestedReadBucket(k)
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

			// If we requested to only delete failed payments, we
			// can return if this one is not.
			if failedOnly && paymentStatus != StatusFailed {
				return nil
			}

			// If we are only deleting failed HTLCs, fetch them.
			if failedHtlcsOnly {
				toDelete, err := fetchFailedHtlcKeys(bucket)
				if err != nil {
					return err
				}

				hash, err := lntypes.MakeHash(k)
				if err != nil {
					return err
				}

				deleteHtlcs[hash] = toDelete

				// We return, we are only deleting attempts.
				return nil
			}

			// Add the bucket to the set of buckets we can delete.
			deleteBuckets = append(deleteBuckets, k)

			// Get all the sequence number associated with the
			// payment, including duplicates.
			seqNrs, err := fetchSequenceNumbers(bucket)
			if err != nil {
				return err
			}

			deleteIndexes = append(deleteIndexes, seqNrs...)
			return nil
		})
		if err != nil {
			return err
		}

		// Delete the failed HTLC attempts we found.
		for hash, htlcIDs := range deleteHtlcs {
			bucket := payments.NestedReadWriteBucket(hash[:])
			htlcsBucket := bucket.NestedReadWriteBucket(
				paymentHtlcsBucket,
			)

			for _, aid := range htlcIDs {
				if err := htlcsBucket.Delete(
					htlcBucketKey(htlcAttemptInfoKey, aid),
				); err != nil {
					return err
				}

				if err := htlcsBucket.Delete(
					htlcBucketKey(htlcFailInfoKey, aid),
				); err != nil {
					return err
				}

				if err := htlcsBucket.Delete(
					htlcBucketKey(htlcSettleInfoKey, aid),
				); err != nil {
					return err
				}
			}
		}

		for _, k := range deleteBuckets {
			if err := payments.DeleteNestedBucket(k); err != nil {
				return err
			}
		}

		// Get our index bucket and delete all indexes pointing to the
		// payments we are deleting.
		indexBucket := tx.ReadWriteBucket(paymentsIndexBucket)
		for _, k := range deleteIndexes {
			if err := indexBucket.Delete(k); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// fetchSequenceNumbers fetches all the sequence numbers associated with a
// payment, including those belonging to any duplicate payments.
func fetchSequenceNumbers(paymentBucket kvdb.RBucket) ([][]byte, error) {
	seqNum := paymentBucket.Get(paymentSequenceKey)
	if seqNum == nil {
		return nil, errors.New("expected sequence number")
	}

	sequenceNumbers := [][]byte{seqNum}

	// Get the duplicate payments bucket, if it has no duplicates, just
	// return early with the payment sequence number.
	duplicates := paymentBucket.NestedReadBucket(duplicatePaymentsBucket)
	if duplicates == nil {
		return sequenceNumbers, nil
	}

	// If we do have duplicated, they are keyed by sequence number, so we
	// iterate through the duplicates bucket and add them to our set of
	// sequence numbers.
	if err := duplicates.ForEach(func(k, v []byte) error {
		sequenceNumbers = append(sequenceNumbers, k)
		return nil
	}); err != nil {
		return nil, err
	}

	return sequenceNumbers, nil
}

// nolint: dupl
func serializePaymentCreationInfo(w io.Writer, c *PaymentCreationInfo) error {
	var scratch [8]byte

	if _, err := w.Write(c.PaymentIdentifier[:]); err != nil {
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

	if _, err := io.ReadFull(r, c.PaymentIdentifier[:]); err != nil {
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
	if err := WriteElements(w, a.sessionKey); err != nil {
		return err
	}

	if err := SerializeRoute(w, a.Route); err != nil {
		return err
	}

	if err := serializeTime(w, a.AttemptTime); err != nil {
		return err
	}

	// If the hash is nil we can just return.
	if a.Hash == nil {
		return nil
	}

	if _, err := w.Write(a.Hash[:]); err != nil {
		return err
	}

	return nil
}

func deserializeHTLCAttemptInfo(r io.Reader) (*HTLCAttemptInfo, error) {
	a := &HTLCAttemptInfo{}
	err := ReadElements(r, &a.sessionKey)
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

	hash := lntypes.Hash{}
	_, err = io.ReadFull(r, hash[:])

	switch {
	// Older payment attempts wouldn't have the hash set, in which case we
	// can just return.
	case err == io.EOF, err == io.ErrUnexpectedEOF:
		return a, nil

	case err != nil:
		return nil, err

	default:
	}

	a.Hash = &hash

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

	if h.Metadata != nil {
		records = append(records, record.NewMetadataRecord(&h.Metadata))
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

	metadataType := uint64(record.MetadataOnionType)
	if metadata, ok := tlvMap[metadataType]; ok {
		delete(tlvMap, metadataType)

		h.Metadata = metadata
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

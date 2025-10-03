package paymentsdb

import "errors"

var (
	// ErrAlreadyPaid signals we have already paid this payment hash.
	ErrAlreadyPaid = errors.New("invoice is already paid")

	// ErrPaymentInFlight signals that payment for this payment hash is
	// already "in flight" on the network.
	ErrPaymentInFlight = errors.New("payment is in transition")

	// ErrPaymentExists is returned when we try to initialize an already
	// existing payment that is not failed.
	ErrPaymentExists = errors.New("payment already exists")

	// ErrPaymentInternal is returned when performing the payment has a
	// conflicting state, such as,
	// - payment has StatusSucceeded but remaining amount is not zero.
	// - payment has StatusInitiated but remaining amount is zero.
	// - payment has StatusFailed but remaining amount is zero.
	ErrPaymentInternal = errors.New("internal error")

	// ErrPaymentNotInitiated is returned if the payment wasn't initiated.
	ErrPaymentNotInitiated = errors.New("payment isn't initiated")

	// ErrPaymentAlreadySucceeded is returned in the event we attempt to
	// change the status of a payment already succeeded.
	ErrPaymentAlreadySucceeded = errors.New("payment is already succeeded")

	// ErrPaymentAlreadyFailed is returned in the event we attempt to alter
	// a failed payment.
	ErrPaymentAlreadyFailed = errors.New("payment has already failed")

	// ErrUnknownPaymentStatus is returned when we do not recognize the
	// existing state of a payment.
	ErrUnknownPaymentStatus = errors.New("unknown payment status")

	// ErrPaymentTerminal is returned if we attempt to alter a payment that
	// already has reached a terminal condition.
	ErrPaymentTerminal = errors.New("payment has reached terminal " +
		"condition")

	// ErrAttemptAlreadySettled is returned if we try to alter an already
	// settled HTLC attempt.
	ErrAttemptAlreadySettled = errors.New("attempt already settled")

	// ErrAttemptAlreadyFailed is returned if we try to alter an already
	// failed HTLC attempt.
	ErrAttemptAlreadyFailed = errors.New("attempt already failed")

	// ErrValueMismatch is returned if we try to register a non-MPP attempt
	// with an amount that doesn't match the payment amount.
	ErrValueMismatch = errors.New("attempted value doesn't match payment " +
		"amount")

	// ErrValueExceedsAmt is returned if we try to register an attempt that
	// would take the total sent amount above the payment amount.
	ErrValueExceedsAmt = errors.New("attempted value exceeds payment " +
		"amount")

	// ErrNonMPPayment is returned if we try to register an MPP attempt for
	// a payment that already has a non-MPP attempt registered.
	ErrNonMPPayment = errors.New("payment has non-MPP attempts")

	// ErrMPPayment is returned if we try to register a non-MPP attempt for
	// a payment that already has an MPP attempt registered.
	ErrMPPayment = errors.New("payment has MPP attempts")

	// ErrMPPRecordInBlindedPayment is returned if we try to register an
	// attempt with an MPP record for a payment to a blinded path.
	ErrMPPRecordInBlindedPayment = errors.New("blinded payment cannot " +
		"contain MPP records")

	// ErrBlindedPaymentTotalAmountMismatch is returned if we try to
	// register an HTLC shard to a blinded route where the total amount
	// doesn't match existing shards.
	ErrBlindedPaymentTotalAmountMismatch = errors.New("blinded path " +
		"total amount mismatch")

	// ErrMixedBlindedAndNonBlindedPayments is returned if we try to
	// register a non-blinded attempt to a payment which uses a blinded
	// paths or vice versa.
	ErrMixedBlindedAndNonBlindedPayments = errors.New("mixed blinded and " +
		"non-blinded payments")

	// ErrMPPPaymentAddrMismatch is returned if we try to register an MPP
	// shard where the payment address doesn't match existing shards.
	ErrMPPPaymentAddrMismatch = errors.New("payment address mismatch")

	// ErrMPPTotalAmountMismatch is returned if we try to register an MPP
	// shard where the total amount doesn't match existing shards.
	ErrMPPTotalAmountMismatch = errors.New("mp payment total amount " +
		"mismatch")

	// ErrPaymentPendingSettled is returned when we try to add a new
	// attempt to a payment that has at least one of its HTLCs settled.
	ErrPaymentPendingSettled = errors.New("payment has settled htlcs")

	// ErrPaymentPendingFailed is returned when we try to add a new attempt
	// to a payment that already has a failure reason.
	ErrPaymentPendingFailed = errors.New("payment has failure reason")

	// ErrSentExceedsTotal is returned if the payment's current total sent
	// amount exceed the total amount.
	ErrSentExceedsTotal = errors.New("total sent exceeds total amount")

	// ErrNoAttemptInfo is returned when no attempt info is stored yet.
	ErrNoAttemptInfo = errors.New("unable to find attempt info for " +
		"inflight payment")
)

// KV backend specific errors.
var (
	// ErrNoSequenceNumber is returned if we look up a payment which does
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

	// ErrNoSequenceNrIndex is returned when an attempt to lookup a payment
	// index is made for a sequence number that is not indexed.
	//
	// NOTE: Only used for the kv backend.
	ErrNoSequenceNrIndex = errors.New("payment sequence number index " +
		"does not exist")
)

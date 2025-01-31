package invoices

// acceptResolutionResult provides metadata which about a htlc that was
// accepted by the registry.
type acceptResolutionResult uint8

const (
	resultInvalidAccept acceptResolutionResult = iota

	// resultReplayToAccepted is returned when we replay an accepted
	// invoice.
	resultReplayToAccepted

	// resultDuplicateToAccepted is returned when we accept a duplicate
	// htlc.
	resultDuplicateToAccepted

	// resultAccepted is returned when we accept a hodl invoice.
	resultAccepted

	// resultPartialAccepted is returned when we have partially received
	// payment.
	resultPartialAccepted
)

// String returns a string representation of the result.
func (a acceptResolutionResult) String() string {
	switch a {
	case resultInvalidAccept:
		return "invalid accept result"

	case resultReplayToAccepted:
		return "replayed htlc to accepted invoice"

	case resultDuplicateToAccepted:
		return "accepting duplicate payment to accepted invoice"

	case resultAccepted:
		return "accepted"

	case resultPartialAccepted:
		return "partial payment accepted"

	default:
		return "unknown accept resolution result"
	}
}

// FailResolutionResult provides metadata about a htlc that was failed by
// the registry. It can be used to take custom actions on resolution of the
// htlc.
type FailResolutionResult uint8

const (
	resultInvalidFailure FailResolutionResult = iota

	// ResultReplayToCanceled is returned when we replay a canceled invoice.
	ResultReplayToCanceled

	// ResultInvoiceAlreadyCanceled is returned when trying to pay an
	// invoice that is already canceled.
	ResultInvoiceAlreadyCanceled

	// ResultInvoiceAlreadySettled is returned when trying to pay an invoice
	// that is already settled.
	ResultInvoiceAlreadySettled

	// ResultAmountTooLow is returned when an invoice is underpaid.
	ResultAmountTooLow

	// ResultExpiryTooSoon is returned when we do not accept an invoice
	// payment because it expires too soon.
	ResultExpiryTooSoon

	// ResultCanceled is returned when we cancel an invoice and its
	// associated htlcs.
	ResultCanceled

	// ResultInvoiceNotOpen is returned when a mpp invoice is not open.
	ResultInvoiceNotOpen

	// ResultMppTimeout is returned when an invoice paid with multiple
	// partial payments times out before it is fully paid.
	ResultMppTimeout

	// ResultAddressMismatch is returned when the payment address for a mpp
	// invoice does not match.
	ResultAddressMismatch

	// ResultHtlcSetTotalMismatch is returned when the amount paid by a
	// htlc does not match its set total.
	ResultHtlcSetTotalMismatch

	// ResultHtlcSetTotalTooLow is returned when a mpp set total is too low
	// for an invoice.
	ResultHtlcSetTotalTooLow

	// ResultHtlcSetOverpayment is returned when a mpp set is overpaid.
	ResultHtlcSetOverpayment

	// ResultInvoiceNotFound is returned when an attempt is made to pay an
	// invoice that is unknown to us.
	ResultInvoiceNotFound

	// ResultKeySendError is returned when we receive invalid keysend
	// parameters.
	ResultKeySendError

	// ResultMppInProgress is returned when we are busy receiving a mpp
	// payment.
	ResultMppInProgress

	// ResultHtlcInvoiceTypeMismatch is returned when an AMP HTLC targets a
	// non-AMP invoice and vice versa.
	ResultHtlcInvoiceTypeMismatch

	// ResultAmpError is returned when we receive invalid AMP parameters.
	ResultAmpError

	// ResultAmpReconstruction is returned when the derived child
	// hash/preimage pairs were invalid for at least one HTLC in the set.
	ResultAmpReconstruction

	// ExternalValidationFailed is returned when the external validation
	// failed.
	ExternalValidationFailed
)

// String returns a string representation of the result.
func (f FailResolutionResult) String() string {
	return f.FailureString()
}

// FailureString returns a string representation of the result.
//
// Note: it is part of the FailureDetail interface.
func (f FailResolutionResult) FailureString() string {
	switch f {
	case resultInvalidFailure:
		return "invalid failure result"

	case ResultReplayToCanceled:
		return "replayed htlc to canceled invoice"

	case ResultInvoiceAlreadyCanceled:
		return "invoice already canceled"

	case ResultInvoiceAlreadySettled:
		return "invoice already settled"

	case ResultAmountTooLow:
		return "amount too low"

	case ResultExpiryTooSoon:
		return "expiry too soon"

	case ResultCanceled:
		return "canceled"

	case ResultInvoiceNotOpen:
		return "invoice no longer open"

	case ResultMppTimeout:
		return "mpp timeout"

	case ResultAddressMismatch:
		return "payment address mismatch"

	case ResultHtlcSetTotalMismatch:
		return "htlc total amt doesn't match set total"

	case ResultHtlcSetTotalTooLow:
		return "set total too low for invoice"

	case ResultHtlcSetOverpayment:
		return "mpp is overpaying set total"

	case ResultInvoiceNotFound:
		return "invoice not found"

	case ResultKeySendError:
		return "invalid keysend parameters"

	case ResultMppInProgress:
		return "mpp reception in progress"

	case ResultHtlcInvoiceTypeMismatch:
		return "htlc invoice type mismatch"

	case ResultAmpError:
		return "invalid amp parameters"

	case ResultAmpReconstruction:
		return "amp reconstruction failed"

	case ExternalValidationFailed:
		return "external validation failed"

	default:
		return "unknown failure resolution result"
	}
}

// IsSetFailure returns true if this failure should result in the entire HTLC
// set being failed with the same result.
func (f FailResolutionResult) IsSetFailure() bool {
	switch f {
	case
		ResultAmpReconstruction,
		ResultHtlcSetTotalTooLow,
		ResultHtlcSetTotalMismatch,
		ResultHtlcSetOverpayment,
		ExternalValidationFailed:

		return true

	default:
		return false
	}
}

// SettleResolutionResult provides metadata which about a htlc that was failed
// by the registry. It can be used to take custom actions on resolution of the
// htlc.
type SettleResolutionResult uint8

const (
	resultInvalidSettle SettleResolutionResult = iota

	// ResultSettled is returned when we settle an invoice.
	ResultSettled

	// ResultReplayToSettled is returned when we replay a settled invoice.
	ResultReplayToSettled

	// ResultDuplicateToSettled is returned when we settle an invoice which
	// has already been settled at least once.
	ResultDuplicateToSettled
)

// String returns a string representation of the result.
func (s SettleResolutionResult) String() string {
	switch s {
	case resultInvalidSettle:
		return "invalid settle result"

	case ResultSettled:
		return "settled"

	case ResultReplayToSettled:
		return "replayed htlc to settled invoice"

	case ResultDuplicateToSettled:
		return "accepting duplicate payment to settled invoice"

	default:
		return "unknown settle resolution result"
	}
}

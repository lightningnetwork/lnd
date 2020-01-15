package htlcswitch

// FailureDetail is an enum which is used to enrich failures with
// additional information.
type FailureDetail int

const (
	// FailureDetailNone is returned when the wire message contains
	// sufficient information.
	FailureDetailNone = iota

	// FailureDetailOnionDecode indicates that we could not decode an
	// onion error.
	FailureDetailOnionDecode

	// FailureDetailLinkNotEligible indicates that a routing attempt was
	// made over a link that is not eligible for routing.
	FailureDetailLinkNotEligible

	// FailureDetailOnChainTimeout indicates that a payment had to be timed
	// out on chain before it got past the first hop by us or the remote
	// party.
	FailureDetailOnChainTimeout

	// FailureDetailHTLCExceedsMax is returned when a htlc exceeds our
	// policy's maximum htlc amount.
	FailureDetailHTLCExceedsMax

	// FailureDetailInsufficientBalance is returned when we cannot route a
	// htlc due to insufficient outgoing capacity.
	FailureDetailInsufficientBalance
)

// String returns the string representation of a failure detail.
func (fd FailureDetail) String() string {
	switch fd {
	case FailureDetailNone:
		return "no failure detail"

	case FailureDetailOnionDecode:
		return "could not decode onion"

	case FailureDetailLinkNotEligible:
		return "link not eligible"

	case FailureDetailOnChainTimeout:
		return "payment was resolved on-chain, then canceled back"

	case FailureDetailHTLCExceedsMax:
		return "htlc exceeds maximum policy amount"

	case FailureDetailInsufficientBalance:
		return "insufficient bandwidth to route htlc"

	default:
		return "unknown failure detail"
	}
}

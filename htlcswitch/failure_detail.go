package htlcswitch

// OutgoingFailure is an enum which is used to enrich failures which occur in
// the switch or on our outgoing link with additional metadata.
type OutgoingFailure int

const (
	// OutgoingFailureNone is returned when the wire message contains
	// sufficient information.
	OutgoingFailureNone OutgoingFailure = iota

	// OutgoingFailureDecodeError indicates that we could not decode the
	// failure reason provided for a failed payment.
	OutgoingFailureDecodeError

	// OutgoingFailureLinkNotEligible indicates that a routing attempt was
	// made over a link that is not eligible for routing.
	OutgoingFailureLinkNotEligible

	// OutgoingFailureOnChainTimeout indicates that a payment had to be
	// timed out on chain before it got past the first hop by us or the
	// remote party.
	OutgoingFailureOnChainTimeout

	// OutgoingFailureHTLCExceedsMax is returned when a htlc exceeds our
	// policy's maximum htlc amount.
	OutgoingFailureHTLCExceedsMax

	// OutgoingFailureInsufficientBalance is returned when we cannot route a
	// htlc due to insufficient outgoing capacity.
	OutgoingFailureInsufficientBalance

	// OutgoingFailureCircularRoute is returned when an attempt is made
	// to forward a htlc through our node which arrives and leaves on the
	// same channel.
	OutgoingFailureCircularRoute
)

// String returns the string representation of a failure detail.
func (fd OutgoingFailure) String() string {
	switch fd {
	case OutgoingFailureNone:
		return "no failure detail"

	case OutgoingFailureDecodeError:
		return "could not decode wire failure"

	case OutgoingFailureLinkNotEligible:
		return "link not eligible"

	case OutgoingFailureOnChainTimeout:
		return "payment was resolved on-chain, then canceled back"

	case OutgoingFailureHTLCExceedsMax:
		return "htlc exceeds maximum policy amount"

	case OutgoingFailureInsufficientBalance:
		return "insufficient bandwidth to route htlc"

	case OutgoingFailureCircularRoute:
		return "same incoming and outgoing channel"

	default:
		return "unknown failure detail"
	}
}

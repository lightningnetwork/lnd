package hodl

import "fmt"

// MaskNone represents the empty Mask, in which no breakpoints are
// active.
const MaskNone = Mask(0)

type (
	// Flag represents a single breakpoint where an HTLC should be dropped
	// during forwarding. Flags can be composed into a Mask to express more
	// complex combinations.
	Flag uint32

	// Mask is a bitvector combining multiple Flags that can be queried to
	// see which breakpoints are active.
	Mask uint32
)

const (
	// ExitSettle drops an incoming ADD for which we are the exit node,
	// before processing in the link.
	ExitSettle Flag = 1 << iota

	// AddIncoming drops an incoming ADD before processing if we are not
	// the exit node.
	AddIncoming

	// SettleIncoming drops an incoming SETTLE before processing if we
	// are not the exit node.
	SettleIncoming

	// FailIncoming drops an incoming FAIL before processing if we are
	// not the exit node.
	FailIncoming

	// TODO(conner): add  modes for switch breakpoints

	// AddOutgoing drops an outgoing ADD before it is added to the
	// in-memory commitment state of the link.
	AddOutgoing

	// SettleOutgoing drops an SETTLE before it is added to the
	// in-memory commitment state of the link.
	SettleOutgoing

	// FailOutgoing drops an outgoing FAIL before is is added to the
	// in-memory commitment state of the link.
	FailOutgoing

	// Commit drops all HTLC after any outgoing circuits have been
	// opened, but before the in-memory commitment state is persisted.
	Commit

	// BogusSettle attempts to settle back any incoming HTLC for which we
	// are the exit node with a bogus preimage.
	BogusSettle
)

// String returns a human-readable identifier for a given Flag.
func (f Flag) String() string {
	switch f {
	case ExitSettle:
		return "ExitSettle"
	case AddIncoming:
		return "AddIncoming"
	case SettleIncoming:
		return "SettleIncoming"
	case FailIncoming:
		return "FailIncoming"
	case AddOutgoing:
		return "AddOutgoing"
	case SettleOutgoing:
		return "SettleOutgoing"
	case FailOutgoing:
		return "FailOutgoing"
	case Commit:
		return "Commit"
	case BogusSettle:
		return "BogusSettle"
	default:
		return "UnknownHodlFlag"
	}
}

// Warning generates a warning message to log if a particular breakpoint is
// triggered during execution.
func (f Flag) Warning() string {
	var msg string
	switch f {
	case ExitSettle:
		msg = "will not attempt to settle ADD with sender"
	case AddIncoming:
		msg = "will not attempt to forward ADD to switch"
	case SettleIncoming:
		msg = "will not attempt to forward SETTLE to switch"
	case FailIncoming:
		msg = "will not attempt to forward FAIL to switch"
	case AddOutgoing:
		msg = "will not update channel state with downstream ADD"
	case SettleOutgoing:
		msg = "will not update channel state with downstream SETTLE"
	case FailOutgoing:
		msg = "will not update channel state with downstream FAIL"
	case Commit:
		msg = "will not commit pending channel updates"
	case BogusSettle:
		msg = "will settle HTLC with bogus preimage"
	default:
		msg = "incorrect hodl flag usage"
	}

	return fmt.Sprintf("%s mode enabled -- %s", f, msg)
}

// Mask returns the Mask consisting solely of this Flag.
func (f Flag) Mask() Mask {
	return Mask(f)
}

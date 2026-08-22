package peer

import "github.com/lightningnetwork/lnd/lnwire"

const (
	// maxQueuedMsgs caps fixed-cost message floods even when their retained
	// byte charge remains well below the independent memory budget.
	maxQueuedMsgs = 10000

	// maxQueuedBytes bounds explicitly charged per-peer backlog memory at
	// approximately 16 MiB before the owning connection is disconnected.
	maxQueuedBytes = 16 << 20

	// queuedMsgOverhead conservatively charges the Go message wrapper and
	// list element retained for every queued wire message.
	queuedMsgOverhead = 128
)

// queueLimits groups the count and retained-memory bounds applied to one
// peer's outgoing backlog. The values remain private because they protect
// internal resource ownership rather than define user-facing behavior.
type queueLimits struct {
	// maxMsgs prevents fixed-size messages from growing the queue without
	// bound even when their charged byte cost is small.
	maxMsgs int

	// maxBytes caps the explicitly charged retained memory. Message shapes
	// not included in the estimate remain protected by maxMsgs.
	maxBytes int

	// msgOverhead charges the message wrapper and list element even when a
	// payload aliases memory owned elsewhere, as Pongs do.
	msgOverhead int
}

// defaultQueueLimits returns the private resource bounds applied to each
// peer's outgoing backlog. Keeping them in one value gives the producer and
// queue owner the same immutable accounting policy.
func defaultQueueLimits() queueLimits {
	return queueLimits{
		// Bound both cheap-message floods and approximately 16 MiB
		// of explicitly charged retained queue memory.
		maxMsgs:  maxQueuedMsgs,
		maxBytes: maxQueuedBytes,

		// A retained Pong costs about 104 bytes across its wrapper
		// and list element, rounded up for accounting.
		msgOverhead: queuedMsgOverhead,
	}
}

// msgCost estimates memory retained by an outgoing message without
// serializing it. The independent count limit still bounds message shapes
// whose dynamic memory is not included in this targeted estimate. CommitSig
// signatures are the known material undercount, but commitment flow control
// bounds them by channel count rather than permitting a bulk peer flood.
func (l queueLimits) msgCost(msg lnwire.Message) int {
	switch msg := msg.(type) {
	// Pong payloads alias one server-wide buffer, so only their wrapper and
	// list storage contribute additional retained queue memory.
	case *lnwire.Pong:
		return l.msgOverhead

	// Failure reasons are preserved byte-for-byte when forwarded upstream
	// and are the largest variable payload a remote peer can drive in bulk.
	case *lnwire.UpdateFailHTLC:
		return l.msgOverhead + len(msg.Reason)

	// The onion packet is inline rather than a slice, so charge it together
	// with any separately retained extra data.
	case *lnwire.UpdateAddHTLC:
		return l.msgOverhead + lnwire.OnionPacketSize +
			len(msg.ExtraData)

	// Error and Warning retain peer-controlled diagnostic payloads, so
	// charge their backing bytes against the queue memory limit.
	case *lnwire.Error:
		return l.msgOverhead + len(msg.Data)

	case *lnwire.Warning:
		return l.msgOverhead + len(msg.Data)

	// Forwarded onion messages retain a fresh peer-controlled blob. Charge
	// those backing bytes so many maximum-sized messages cannot outgrow the
	// queue's retained-memory budget while paying only fixed overhead.
	case *lnwire.OnionMessage:
		return l.msgOverhead + len(msg.OnionBlob)

	// Other messages receive the fixed charge. Their total count is still
	// bounded even if they retain dynamic data not enumerated above.
	default:
		return l.msgOverhead
	}
}

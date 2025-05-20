//go:build !integration

package peer

import (
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
)

const (
	// redMinThreshold is the minimum queue length before RED starts dropping
	// messages.
	redMinThreshold = 10

	// redMaxThreshold is the queue length at or above which RED drops all
	// messages (that are not protected by type).
	redMaxThreshold = 40
)

// isProtectedMsgType checks if a message is of a type that should not be
// dropped by the predicate.
func isProtectedMsgType(msg lnwire.Message) bool {
	switch msg.(type) {
	// Never drop any messages that are heading to an active channel.
	case lnwire.LinkUpdater:
		return true

	// Make sure to never drop an incoming announcement signatures
	// message, as we need this to be able to advertise channels.
	//
	// TODO(roasbeef): don't drop any gossip if doing IGD?
	case *lnwire.AnnounceSignatures1:
		return true

	default:
		return false
	}
}

// getMsgStreamDropPredicate returns the drop predicate for the msgStream's
// BackpressureQueue. For non-integration builds, this combines a type-based
// check for critical messages with Random Early Detection (RED).
func getMsgStreamDropPredicate() queue.DropPredicate[lnwire.Message] {
	redPred := queue.RandomEarlyDrop[lnwire.Message](
		redMinThreshold, redMaxThreshold,
	)

	// We'll never dropped protected messages, for the rest we'll use the
	// RED predicate.
	return func(queueLen int, item lnwire.Message) bool {
		if isProtectedMsgType(item) {
			return false
		}

		return redPred(queueLen, item)
	}
}

//go:build integration

package peer

import (
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
)

// getMsgStreamDropPredicate returns the drop predicate for the msgStream's
// BackpressureQueue. For integration builds, this predicate never drops
// messages.
func getMsgStreamDropPredicate() queue.DropPredicate[lnwire.Message] {
	return func(queueLen int, item lnwire.Message) bool {
		return false
	}
}

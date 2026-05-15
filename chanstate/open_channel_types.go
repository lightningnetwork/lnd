package chanstate

import "net"

// ChannelShell is a shell of a channel that is meant to be used for channel
// recovery purposes. It contains a minimal OpenChannel instance along with
// addresses for that target node.
type ChannelShell struct {
	// NodeAddrs the set of addresses that this node has known to be
	// reachable at in the past.
	NodeAddrs []net.Addr

	// Chan is a shell of an OpenChannel, it contains only the items
	// required to restore the channel on disk.
	Chan *OpenChannel
}

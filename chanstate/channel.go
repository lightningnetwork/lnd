package chanstate

import "net"

// ChanCount is used by the server in determining access control.
type ChanCount struct {
	HasOpenOrClosedChan bool
	PendingOpenCount    uint64
}

// FinalHtlcInfo contains information about the final outcome of an htlc.
type FinalHtlcInfo struct {
	// Settled is true is the htlc was settled. If false, the htlc was
	// failed.
	Settled bool

	// Offchain indicates whether the htlc was resolved off-chain or
	// on-chain.
	Offchain bool
}

// ChannelShell contains the minimal channel state and peer addresses needed to
// restore a channel during recovery.
type ChannelShell[Channel any] struct {
	// NodeAddrs is the set of addresses that this node has known to be
	// reachable at in the past.
	NodeAddrs []net.Addr

	// Chan is the minimal channel state required to restore the channel on
	// disk.
	Chan Channel
}

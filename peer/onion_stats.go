package peer

// OnionMessageStats is the snapshot of per-peer onion-message
// byte and drop counters exposed to callers via
// Brontide.OnionStats. All values are cumulative over the
// lifetime of the current Brontide connection and reset when
// the peer reconnects.
type OnionMessageStats struct {
	// BytesRecv is the total on-the-wire size of onion
	// messages from this peer that were admitted past the
	// ingress pipeline.
	BytesRecv uint64

	// BytesSent is the total on-the-wire size of onion
	// messages we sent to this peer.
	BytesSent uint64

	// DroppedPeer is the count of onion messages from this
	// peer rejected by the per-peer byte bucket.
	DroppedPeer uint64

	// DroppedFreebie is the count of onion messages from
	// this peer rejected by the shared freebie bucket
	// (non-zero only for peers with no fully-open channel,
	// when the freebie slot is enabled).
	DroppedFreebie uint64

	// DroppedGlobal is the count of onion messages from
	// this peer rejected by the global byte bucket.
	DroppedGlobal uint64

	// DroppedNoChannel is the count of onion messages
	// from this peer rejected by the channel-presence
	// gate because the peer has no fully-open channel and
	// either freebie is disabled or its bucket was empty.
	DroppedNoChannel uint64
}

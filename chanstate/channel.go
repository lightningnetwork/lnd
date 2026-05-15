package chanstate

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

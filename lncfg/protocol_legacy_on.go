//go:build dev
// +build dev

package lncfg

// Legacy is a sub-config that houses all the legacy protocol options.  These
// are mostly used for integration tests as most modern nodes should always run
// with them on by default.
//
//nolint:ll
type LegacyProtocol struct {
	// LegacyOnionFormat if set to true, then we won't signal
	// TLVOnionPayloadOptional. As a result, nodes that include us in the
	// route won't use the new modern onion framing.
	LegacyOnionFormat bool `long:"onion" description:"force node to not advertise the new modern TLV onion format"`

	// CommitmentTweak is deprecated and no longer has any effect. The
	// legacy commitment type is refused for new channels, so a node that
	// stopped signalling StaticRemoteKeyOptional could no longer negotiate
	// any commitment type at all.
	CommitmentTweak bool `long:"committweak" hidden:"true" description:"deprecated: the legacy commitment format can no longer be used for new channels"`
}

// LegacyOnion returns true if the old legacy onion format should be used when
// we're an intermediate or final hop. This controls if we set the
// TLVOnionPayloadOptional bit or not.
func (l *LegacyProtocol) LegacyOnion() bool {
	return l.LegacyOnionFormat
}

// NoStaticRemoteKey returns true if the old commitment format with a tweaked
// remote key should be used for new funded channels. The legacy commitment
// type can no longer be used for new channels, so this always returns false.
// The CommitmentTweak field is only kept around so that configs which still
// set it continue to parse.
func (l *LegacyProtocol) NoStaticRemoteKey() bool {
	return false
}

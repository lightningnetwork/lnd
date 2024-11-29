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

	// CommitmentTweak guards if we should use the old legacy commitment
	// protocol, or the newer variant that doesn't have a tweak for the
	// remote party's output in the commitment. If set to true, then we
	// won't signal StaticRemoteKeyOptional.
	CommitmentTweak bool `long:"committweak" description:"force node to not advertise the new commitment format"`
}

// LegacyOnion returns true if the old legacy onion format should be used when
// we're an intermediate or final hop. This controls if we set the
// TLVOnionPayloadOptional bit or not.
func (l *LegacyProtocol) LegacyOnion() bool {
	return l.LegacyOnionFormat
}

// NoStaticRemoteKey returns true if the old commitment format with a tweaked
// remote key should be used for new funded channels.
func (l *LegacyProtocol) NoStaticRemoteKey() bool {
	return l.CommitmentTweak
}

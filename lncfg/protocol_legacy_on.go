// +build dev

package lncfg

// ProtocolOptions is a struct that we use to be able to test backwards
// compatibility of protocol additions, while defaulting to the latest within
// lnd, or to enable experimental protocol changes.
type ProtocolOptions struct {
	// LegacyOnionFormat if set to true, then we won't signal
	// TLVOnionPayloadOptional. As a result, nodes that include us in the
	// route won't use the new modern onion framing.
	LegacyOnionFormat bool `long:"legacyonion" description:"force node to not advertise the new modern TLV onion format"`

	// CommitmentTweak guards if we should use the old legacy commitment
	// protocol, or the newer variant that doesn't have a tweak for the
	// remote party's output in the commitment. If set to true, then we
	// won't signal StaticRemoteKeyOptional.
	CommitmentTweak bool `long:"committweak" description:"force node to not advertise the new commitment format"`

	// Anchors should be set if we want to support opening or accepting
	// channels having the anchor commitment type.
	Anchors bool `long:"anchors" description:"EXPERIMENTAL: enable experimental support for anchor commitments. Won't work with watchtowers or static channel backups"`
}

// LegacyOnion returns true if the old legacy onion format should be used when
// we're an intermediate or final hop. This controls if we set the
// TLVOnionPayloadOptional bit or not.
func (l *ProtocolOptions) LegacyOnion() bool {
	return l.LegacyOnionFormat
}

// NoStaticRemoteKey returns true if the old commitment format with a tweaked
// remote key should be used for new funded channels.
func (l *ProtocolOptions) NoStaticRemoteKey() bool {
	return l.CommitmentTweak
}

// AnchorCommitments returns true if support for the the anchor commitment type
// should be signaled.
func (l *ProtocolOptions) AnchorCommitments() bool {
	return l.Anchors
}

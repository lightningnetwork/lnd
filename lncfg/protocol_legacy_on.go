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
}

// LegacyOnion returns true if the old legacy onion format should be used when
// we're an intermediate or final hop. This controls if we set the
// TLVOnionPayloadOptional bit or not.
func (l *ProtocolOptions) LegacyOnion() bool {
	return l.LegacyOnionFormat
}

// LegacyCommitment returns true if the old commitment format should be used
// for new funded channels.
func (l *ProtocolOptions) LegacyCommitment() bool {
	return l.CommitmentTweak
}

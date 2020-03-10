// +build !dev

package lncfg

// ProtocolOptions is a struct that we use to be able to test backwards
// compatibility of protocol additions, while defaulting to the latest within
// lnd, or to enable experimental protocol changes.
type ProtocolOptions struct {
}

// LegacyOnion returns true if the old legacy onion format should be used when
// we're an intermediate or final hop. This controls if we set the
// TLVOnionPayloadOptional bit or not.
func (l *ProtocolOptions) LegacyOnion() bool {
	return false
}

// NoStaticRemoteKey returns true if the old commitment format with a tweaked
// remote key should be used for new funded channels.
func (l *ProtocolOptions) NoStaticRemoteKey() bool {
	return false
}

// AnchorCommitments returns true if support for the the anchor commitment type
// should be signaled.
func (l *ProtocolOptions) AnchorCommitments() bool {
	return false
}

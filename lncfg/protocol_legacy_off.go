// +build !dev

package lncfg

// LegacyProtocol is a struct that we use to be able to test backwards
// compatibility of protocol additions, while defaulting to the latest within
// lnd.
type LegacyProtocol struct {
}

// LegacyOnion returns true if the old legacy onion format should be used when
// we're an intermediate or final hop. This controls if we set the
// TLVOnionPayloadOptional bit or not.
func (l *LegacyProtocol) LegacyOnion() bool {
	return false
}

// LegacyOnion returns true if the old commitment format should be used for new
// funded channels.
func (l *LegacyProtocol) LegacyCommitment() bool {
	return false
}

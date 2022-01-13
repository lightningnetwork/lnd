//go:build !dev
// +build !dev

package lncfg

// Legacy is a sub-config that houses all the legacy protocol options.  These
// are mostly used for integration tests as most modern nodes should always run
// with them on by default.
type LegacyProtocol struct {
}

// LegacyOnion returns true if the old legacy onion format should be used when
// we're an intermediate or final hop. This controls if we set the
// TLVOnionPayloadOptional bit or not.
func (l *LegacyProtocol) LegacyOnion() bool {
	return false
}

// NoStaticRemoteKey returns true if the old commitment format with a tweaked
// remote key should be used for new funded channels.
func (l *LegacyProtocol) NoStaticRemoteKey() bool {
	return false
}

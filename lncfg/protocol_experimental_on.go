//go:build dev
// +build dev

package lncfg

import "github.com/lightningnetwork/lnd/lnwire"

// ExperimentalProtocol is a sub-config that houses any experimental protocol
// features that also require a build-tag to activate.
//
//nolint:lll
type ExperimentalProtocol struct {
	CustomMessage []uint16 `long:"custom-message" description:"allows the custom message apis to send and report messages with the protocol number provided that fall outside of the custom message number range."`

	CustomFeature []uint64 `long:"custom-nodeann-feature" description:"a protocol feature bit to set in our node's announcement"`
}

// CustomMessageOverrides returns the set of protocol messages that we override
// to allow custom handling.
func (p ExperimentalProtocol) CustomMessageOverrides() []uint16 {
	return p.CustomMessage
}

// CustomAnnFeatures returns a set of protocol feature bits to add to the node's
// announcement.
func (p ExperimentalProtocol) CustomAnnFeatures() []lnwire.FeatureBit {
	features := make([]lnwire.FeatureBit, len(p.CustomFeature))
	for i, f := range p.CustomFeature {
		features[i] = lnwire.FeatureBit(f)
	}

	return features
}

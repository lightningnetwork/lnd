//go:build !dev
// +build !dev

package lncfg

import "github.com/lightningnetwork/lnd/lnwire"

// ExperimentalProtocol is a sub-config that houses any experimental protocol
// features that also require a build-tag to activate.
type ExperimentalProtocol struct {
}

// CustomMessageOverrides returns the set of protocol messages that we override
// to allow custom handling.
func (p ExperimentalProtocol) CustomMessageOverrides() []uint16 {
	return nil
}

// CustomAnnFeatures returns a set of protocol feature bits to set in the node's
// announcement.
func (p ExperimentalProtocol) CustomAnnFeatures() []lnwire.FeatureBit {
	return nil
}

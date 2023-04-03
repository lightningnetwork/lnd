//go:build !dev
// +build !dev

package lncfg

import (
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ExperimentalProtocol is a sub-config that houses any experimental protocol
// features that also require a build-tag to activate.
type ExperimentalProtocol struct {
}

// CustomMessageOverrides returns the set of protocol messages that we override
// to allow custom handling.
func (p ExperimentalProtocol) CustomMessageOverrides() []uint16 {
	return nil
}

// CustomFeatures returns a custom set of feature bits to advertise.
func (p ExperimentalProtocol) CustomFeatures() map[feature.Set][]lnwire.FeatureBit {
	return map[feature.Set][]lnwire.FeatureBit{}
}

//go:build dev
// +build dev

package lncfg

import (
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ExperimentalProtocol is a sub-config that houses any experimental protocol
// features that also require a build-tag to activate.
//
//nolint:lll
type ExperimentalProtocol struct {
	CustomMessage []uint16 `long:"custom-message" description:"allows the custom message apis to send and report messages with the protocol number provided that fall outside of the custom message number range."`

	CustomInit []uint16 `long:"custom-init" description:"custom feature bits to advertise in the node's init message"`

	CustomNodeAnn []uint16 `long:"custom-nodeann" description:"custom feature bits to advertise in the node's announcement message"`

	CustomInvoice []uint16 `long:"custom-invoice" description:"custom feature bits to advertise in the node's invoices"`
}

// CustomMessageOverrides returns the set of protocol messages that we override
// to allow custom handling.
func (p ExperimentalProtocol) CustomMessageOverrides() []uint16 {
	return p.CustomMessage
}

// CustomFeatures returns a custom set of feature bits to advertise.
//
//nolint:lll
func (p ExperimentalProtocol) CustomFeatures() map[feature.Set][]lnwire.FeatureBit {
	customFeatures := make(map[feature.Set][]lnwire.FeatureBit)

	setFeatures := func(set feature.Set, bits []uint16) {
		for _, customFeature := range bits {
			customFeatures[set] = append(
				customFeatures[set],
				lnwire.FeatureBit(customFeature),
			)
		}
	}

	setFeatures(feature.SetInit, p.CustomInit)
	setFeatures(feature.SetNodeAnn, p.CustomNodeAnn)
	setFeatures(feature.SetInvoice, p.CustomInvoice)

	return customFeatures
}

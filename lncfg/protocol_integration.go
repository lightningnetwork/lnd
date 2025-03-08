//go:build integration

package lncfg

import (
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ProtocolOptions is a struct that we use to be able to test backwards
// compatibility of protocol additions, while defaulting to the latest within
// lnd, or to enable experimental protocol changes.
//
// TODO(yy): delete this build flag to unify with `lncfg/protocol.go`.
//
//nolint:ll
type ProtocolOptions struct {
	// LegacyProtocol is a sub-config that houses all the legacy protocol
	// options.  These are mostly used for integration tests as most modern
	// nodes should always run with them on by default.
	LegacyProtocol `group:"legacy" namespace:"legacy"`

	// ExperimentalProtocol is a sub-config that houses any experimental
	// protocol features that also require a build-tag to activate.
	ExperimentalProtocol

	// WumboChans should be set if we want to enable support for wumbo
	// (channels larger than 0.16 BTC) channels, which is the opposite of
	// mini.
	WumboChans bool `long:"wumbo-channels" description:"if set, then lnd will create and accept requests for channels larger chan 0.16 BTC"`

	// TaprootChans should be set if we want to enable support for the
	// experimental simple taproot chans commitment type.
	TaprootChans bool `long:"simple-taproot-chans" description:"if set, then lnd will create and accept requests for channels using the simple taproot commitment type"`

	// TaprootOverlayChans should be set if we want to enable support for
	// the experimental taproot overlay chan type.
	TaprootOverlayChans bool `long:"simple-taproot-overlay-chans" description:"if set, then lnd will create and accept requests for channels using the taproot overlay commitment type"`

	// Anchors enables anchor commitments.
	// TODO(halseth): transition itests to anchors instead!
	Anchors bool `long:"anchors" description:"enable support for anchor commitments"`

	// RbfCoopClose should be set if we want to signal that we support for
	// the new experimental RBF coop close feature.
	RbfCoopClose bool `long:"rbf-coop-close" description:"if set, then lnd will signal that it supports the new RBF based coop close protocol"`

	// ScriptEnforcedLease enables script enforced commitments for channel
	// leases.
	//
	// TODO: Move to experimental?
	ScriptEnforcedLease bool `long:"script-enforced-lease" description:"enable support for script enforced lease commitments"`

	// OptionScidAlias should be set if we want to signal the
	// option-scid-alias feature bit. This allows scid aliases and the
	// option-scid-alias channel-type.
	OptionScidAlias bool `long:"option-scid-alias" description:"enable support for option_scid_alias channels"`

	// OptionZeroConf should be set if we want to signal the zero-conf
	// feature bit.
	OptionZeroConf bool `long:"zero-conf" description:"enable support for zero-conf channels, must have option-scid-alias set also"`

	// NoOptionAnySegwit should be set to true if we don't want to use any
	// Taproot (and beyond) addresses for co-op closing.
	NoOptionAnySegwit bool `long:"no-any-segwit" description:"disallow using any segiwt witness version as a co-op close address"`

	// NoTimestampQueryOption should be set to true if we don't want our
	// syncing peers to also send us the timestamps of announcement messages
	// when we send them a channel range query. Setting this to true will
	// also mean that we won't respond with timestamps if requested by our
	// peers.
	NoTimestampQueryOption bool `long:"no-timestamp-query-option" description:"do not query syncing peers for announcement timestamps and do not respond with timestamps if requested"`

	// NoRouteBlindingOption disables forwarding of payments in blinded routes.
	NoRouteBlindingOption bool `long:"no-route-blinding" description:"do not forward payments that are a part of a blinded route"`

	// NoExperimentalEndorsementOption disables experimental endorsement.
	NoExperimentalEndorsementOption bool `long:"no-experimental-endorsement" description:"do not forward experimental endorsement signals"`

	// NoQuiescenceOption disables quiescence for all channels.
	NoQuiescenceOption bool `long:"no-quiescence" description:"do not allow or advertise quiescence for any channel"`

	// CustomMessage allows the custom message APIs to handle messages with
	// the provided protocol numbers, which fall outside the custom message
	// number range.
	CustomMessage []uint16 `long:"custom-message" description:"allows the custom message apis to send and report messages with the protocol number provided that fall outside of the custom message number range."`

	// CustomInit specifies feature bits to advertise in the node's init
	// message.
	CustomInit []uint16 `long:"custom-init" description:"custom feature bits to advertise in the node's init message"`

	// CustomNodeAnn specifies custom feature bits to advertise in the
	// node's announcement message.
	CustomNodeAnn []uint16 `long:"custom-nodeann" description:"custom feature bits to advertise in the node's announcement message"`

	// CustomInvoice specifies custom feature bits to advertise in the
	// node's invoices.
	CustomInvoice []uint16 `long:"custom-invoice" description:"custom feature bits to advertise in the node's invoices"`
}

// Wumbo returns true if lnd should permit the creation and acceptance of wumbo
// channels.
func (l *ProtocolOptions) Wumbo() bool {
	return l.WumboChans
}

// NoAnchorCommitments returns true if we have disabled support for the anchor
// commitment type.
func (l *ProtocolOptions) NoAnchorCommitments() bool {
	return !l.Anchors
}

// NoScriptEnforcementLease returns true if we have disabled support for the
// script enforcement commitment type for leased channels.
func (l *ProtocolOptions) NoScriptEnforcementLease() bool {
	return !l.ScriptEnforcedLease
}

// ScidAlias returns true if we have enabled the option-scid-alias feature bit.
func (l *ProtocolOptions) ScidAlias() bool {
	return l.OptionScidAlias
}

// ZeroConf returns true if we have enabled the zero-conf feature bit.
func (l *ProtocolOptions) ZeroConf() bool {
	return l.OptionZeroConf
}

// NoAnySegwit returns true if we don't signal that we understand other newer
// segwit witness versions for co-op close addresses.
func (l *ProtocolOptions) NoAnySegwit() bool {
	return l.NoOptionAnySegwit
}

// NoRouteBlinding returns true if forwarding of blinded payments is disabled.
func (l *ProtocolOptions) NoRouteBlinding() bool {
	return l.NoRouteBlindingOption
}

// NoExperimentalEndorsement returns true if experimental endorsement should
// be disabled.
func (l *ProtocolOptions) NoExperimentalEndorsement() bool {
	return l.NoExperimentalEndorsementOption
}

// NoQuiescence returns true if quiescence is disabled.
func (l *ProtocolOptions) NoQuiescence() bool {
	return l.NoQuiescenceOption
}

// CustomMessageOverrides returns the set of protocol messages that we override
// to allow custom handling.
func (l ProtocolOptions) CustomMessageOverrides() []uint16 {
	return l.CustomMessage
}

// CustomFeatures returns a custom set of feature bits to advertise.
func (l ProtocolOptions) CustomFeatures() map[feature.Set][]lnwire.FeatureBit {
	customFeatures := make(map[feature.Set][]lnwire.FeatureBit)

	setFeatures := func(set feature.Set, bits []uint16) {
		for _, customFeature := range bits {
			customFeatures[set] = append(
				customFeatures[set],
				lnwire.FeatureBit(customFeature),
			)
		}
	}

	setFeatures(feature.SetInit, l.CustomInit)
	setFeatures(feature.SetNodeAnn, l.CustomNodeAnn)
	setFeatures(feature.SetInvoice, l.CustomInvoice)

	return customFeatures
}

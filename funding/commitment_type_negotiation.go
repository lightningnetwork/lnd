package funding

import (
	"errors"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// errUnsupportedCommitmentType is an error returned when a specific
	// channel commitment type is being explicitly negotiated but either
	// peer of the channel does not support it.
	errUnsupportedChannelType = errors.New("requested channel type " +
		"not supported")
)

// negotiateCommitmentType negotiates the commitment type of a newly opened
// channel. If a desiredChanType is provided, explicit negotiation for said type
// will be attempted if the set of both local and remote features support it.
// Otherwise, a default type is selected based on feature compatibility,
// particularly when the RPC caller does not request a specific channel type.
//
// The returned ChannelType is always non-nil. An error is only returned if
// desiredChanType is not supported.
func negotiateCommitmentType(desiredChanType *lnwire.ChannelType, local,
	remote *lnwire.FeatureVector) (*lnwire.ChannelType,
	lnwallet.CommitmentType, error) {

	// If a specific channel type was provided, verify it's supported.
	if desiredChanType != nil {
		commitType, err := explicitNegotiateCommitmentType(
			*desiredChanType, local, remote,
		)

		return desiredChanType, commitType, err
	}

	// No specific channel type was requested. Select a default type based
	// on locally-known feature compatibility. This default is then sent
	// explicitly over the wire.
	defaultChanType, commitType := selectDefaultChannelType(local, remote)

	return defaultChanType, commitType, nil
}

// explicitNegotiateCommitmentType attempts to explicitly negotiate for a
// specific channel type. Since the channel type is comprised of a set of even
// feature bits, we also make sure each feature is supported by both peers. An
// error is returned if either peer does not support said channel type.
func explicitNegotiateCommitmentType(channelType lnwire.ChannelType, local,
	remote *lnwire.FeatureVector) (lnwallet.CommitmentType, error) {

	channelFeatures := lnwire.RawFeatureVector(channelType)

	switch {
	// Lease script enforcement + anchors zero fee + static remote key +
	// zero conf + scid alias features only.
	case channelFeatures.OnlyContains(
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
		lnwire.ScriptEnforcedLeaseRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key + zero conf + scid alias
	// features only.
	case channelFeatures.OnlyContains(
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Lease script enforcement + anchors zero fee + static remote key +
	// zero conf features only.
	case channelFeatures.OnlyContains(
		lnwire.ZeroConfRequired,
		lnwire.ScriptEnforcedLeaseRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key + zero conf features only.
	case channelFeatures.OnlyContains(
		lnwire.ZeroConfRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Lease script enforcement + anchors zero fee + static remote key +
	// option-scid-alias features only.
	case channelFeatures.OnlyContains(
		lnwire.ScidAliasRequired,
		lnwire.ScriptEnforcedLeaseRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ScidAliasOptional,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key + option-scid-alias features
	// only.
	case channelFeatures.OnlyContains(
		lnwire.ScidAliasRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ScidAliasOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Lease script enforcement + anchors zero fee + static remote key
	// features only.
	case channelFeatures.OnlyContains(
		lnwire.ScriptEnforcedLeaseRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key features only.
	case channelFeatures.OnlyContains(
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		if !hasFeatures(
			local, remote,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		) {

			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Static remote key feature only.
	case channelFeatures.OnlyContains(lnwire.StaticRemoteKeyRequired):
		if !hasFeatures(local, remote, lnwire.StaticRemoteKeyOptional) {
			return 0, errUnsupportedChannelType
		}
		return lnwallet.CommitmentTypeTweakless, nil

	// Simple taproot channels only (final feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredFinal,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalFinal,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootFinal, nil

	// Simple taproot channels only (staging feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels with scid only (final feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredFinal,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalFinal,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootFinal, nil

	// Simple taproot channels with scid only (staging feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels with zero conf only (final feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredFinal,
		lnwire.ZeroConfRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalFinal,
			lnwire.ZeroConfOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootFinal, nil

	// Simple taproot channels with zero conf only (staging feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ZeroConfRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ZeroConfOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels with scid and zero conf (final feature bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredFinal,
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalFinal,
			lnwire.ZeroConfOptional,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootFinal, nil

	// Simple taproot channels with scid and zero conf (staging feature
	// bits).
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ZeroConfOptional,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels overlay only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootOverlayChansRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootOverlayChansOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootOverlay, nil

	// Simple taproot overlay channels with scid only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootOverlayChansRequired,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootOverlayChansOptional,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootOverlay, nil

	// Simple taproot overlay channels with zero conf only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootOverlayChansRequired,
		lnwire.ZeroConfRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootOverlayChansOptional,
			lnwire.ZeroConfOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootOverlay, nil

	// Simple taproot overlay channels with scid and zero conf.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootOverlayChansRequired,
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootOverlayChansOptional,
			lnwire.ZeroConfOptional,
			lnwire.ScidAliasOptional,
		) {

			return 0, errUnsupportedChannelType
		}

		return lnwallet.CommitmentTypeSimpleTaprootOverlay, nil

	// No features, use legacy commitment type.
	case channelFeatures.IsEmpty():
		return lnwallet.CommitmentTypeLegacy, nil

	default:
		return 0, errUnsupportedChannelType
	}
}

// selectDefaultChannelType selects a default channel type by choosing the
// latest non-taproot type supported by the local and remote features.
// Taproot channels must be requested explicitly, keeping default selections
// on channel types that can be used for both public and private channels.
//
// TODO(yy): Revisit taproot channel selection once public taproot channel
// announcements are supported.
func selectDefaultChannelType(local,
	remote *lnwire.FeatureVector) (*lnwire.ChannelType,
	lnwallet.CommitmentType) {

	// If both peers are signalling support for anchor commitments with
	// zero-fee HTLC transactions, we'll use this type.
	if hasFeatures(local, remote, lnwire.AnchorsZeroFeeHtlcTxOptional) {
		chanType := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			lnwire.AnchorsZeroFeeHtlcTxRequired,
			lnwire.StaticRemoteKeyRequired,
		))

		return &chanType, lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx
	}

	// Since we don't want to support the "legacy" anchor type, we will fall
	// back to static remote key if the nodes don't support the zero fee
	// HTLC tx anchor type.
	//
	// If both nodes are signaling the proper feature bit for tweakless
	// commitments, we'll use that.
	if hasFeatures(local, remote, lnwire.StaticRemoteKeyOptional) {
		chanType := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			lnwire.StaticRemoteKeyRequired,
		))

		return &chanType, lnwallet.CommitmentTypeTweakless
	}

	// Otherwise we'll fall back to the legacy type.
	chanType := lnwire.ChannelType(*lnwire.NewRawFeatureVector())
	return &chanType, lnwallet.CommitmentTypeLegacy
}

// hasFeatures determines whether a set of features is supported by both the set
// of local and remote features.
func hasFeatures(local, remote *lnwire.FeatureVector,
	features ...lnwire.FeatureBit) bool {

	for _, feature := range features {
		if !local.HasFeature(feature) || !remote.HasFeature(feature) {
			return false
		}
	}
	return true
}

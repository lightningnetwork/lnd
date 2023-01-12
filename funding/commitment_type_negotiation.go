package funding

import (
	"errors"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// errUnsupportedExplicitNegotiation is an error returned when explicit
	// channel commitment negotiation is attempted but either peer of the
	// channel does not support it.
	errUnsupportedExplicitNegotiation = errors.New("explicit channel " +
		"type negotiation not supported")

	// errUnsupportedCommitmentType is an error returned when a specific
	// channel commitment type is being explicitly negotiated but either
	// peer of the channel does not support it.
	errUnsupportedChannelType = errors.New("requested channel type " +
		"not supported")
)

// negotiateCommitmentType negotiates the commitment type of a newly opened
// channel. If a channelType is provided, explicit negotiation for said type
// will be attempted if the set of both local and remote features support it.
// Otherwise, implicit negotiation will be attempted. Two booleans are
// returned letting the caller know if the option-scid-alias or zero-conf
// channel types were negotiated.
func negotiateCommitmentType(channelType *lnwire.ChannelType, local,
	remote *lnwire.FeatureVector, mustBeExplicit bool) (bool,
	*lnwire.ChannelType, lnwallet.CommitmentType, error) {

	if channelType != nil {
		// If the peer does know explicit negotiation, let's attempt
		// that now.
		if hasFeatures(
			local, remote, lnwire.ExplicitChannelTypeOptional,
		) {

			chanType, err := explicitNegotiateCommitmentType(
				*channelType, local, remote,
			)
			return true, channelType, chanType, err
		}

		// If we're the funder, and we are attempting to use an
		// explicit channel type, but the remote party doesn't signal
		// the bit, then we actually want to exit here, to ensure the
		// user doesn't end up with an unexpected channel type via
		// implicit negotiation.
		if mustBeExplicit {
			return false, nil, 0, errUnsupportedExplicitNegotiation
		}
	}

	chanType, commitType := implicitNegotiateCommitmentType(local, remote)
	return false, chanType, commitType, nil
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

	// No features, use legacy commitment type.
	case channelFeatures.IsEmpty():
		return lnwallet.CommitmentTypeLegacy, nil

	default:
		return 0, errUnsupportedChannelType
	}
}

// implicitNegotiateCommitmentType negotiates the commitment type of a channel
// implicitly by choosing the latest type supported by the local and remote
// features.
func implicitNegotiateCommitmentType(local,
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

package funding

import (
	"errors"
	"fmt"

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
// Otherwise, implicit negotiation will be attempted.
//
// The returned ChannelType is nil when implicit negotiation is used. An error
// is only returned if desiredChanType is not supported.
func negotiateCommitmentType(desiredChanType *lnwire.ChannelType, local,
	remote *lnwire.FeatureVector) (*lnwire.ChannelType,
	lnwallet.CommitmentType, error) {

	// BOLT#2 specifies we MUST use explicit negotiation if both peers
	// signal for it.
	explicitNegotiation, _ := checkFeaturesDiff(
		local, remote, lnwire.ExplicitChannelTypeOptional,
	)

	chanTypeRequested := desiredChanType != nil

	switch {
	case explicitNegotiation && chanTypeRequested:
		commitType, err := explicitNegotiateCommitmentType(
			*desiredChanType, local, remote,
		)

		return desiredChanType, commitType, err

	// We don't have a specific channel type requested, so we select a
	// default type as if implicit negotiation were used, and then we
	// explicitly signal that default type.
	case explicitNegotiation && !chanTypeRequested:
		defaultChanType, commitType := implicitNegotiateCommitmentType(
			local, remote,
		)

		return defaultChanType, commitType, nil

	// A specific channel type was requested, but we can't explicitly signal
	// it. So if implicit negotiation wouldn't select the desired channel
	// type, we must return an error.
	case !explicitNegotiation && chanTypeRequested:
		implicitChanType, commitType := implicitNegotiateCommitmentType(
			local, remote,
		)

		expected := lnwire.RawFeatureVector(*desiredChanType)
		actual := lnwire.RawFeatureVector(*implicitChanType)
		if !expected.Equals(&actual) {
			return nil, 0, errUnsupportedChannelType
		}

		return nil, commitType, nil

	default: // !explicitNegotiation && !chanTypeRequested
		_, commitType := implicitNegotiateCommitmentType(local, remote)

		return nil, commitType, nil
	}
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
		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		)
		if !ok {
			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
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
		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		)
		if !ok {
			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
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
		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		)
		if !ok {
			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key + zero conf features only.
	case channelFeatures.OnlyContains(
		lnwire.ZeroConfRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		ok, missings := checkFeaturesDiff(
			local, remote,
			lnwire.ZeroConfOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		)
		if !ok {

			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missings)
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
		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.ScidAliasOptional,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		)
		if !ok {

			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key + option-scid-alias features
	// only.
	case channelFeatures.OnlyContains(
		lnwire.ScidAliasRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.ScidAliasOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		)
		if !ok {

			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Lease script enforcement + anchors zero fee + static remote key
	// features only.
	case channelFeatures.OnlyContains(
		lnwire.ScriptEnforcedLeaseRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.ScriptEnforcedLeaseOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		)
		if !ok {

			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
		}
		return lnwallet.CommitmentTypeScriptEnforcedLease, nil

	// Anchors zero fee + static remote key features only.
	case channelFeatures.OnlyContains(
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.StaticRemoteKeyRequired,
	):
		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.StaticRemoteKeyOptional,
		)
		if !ok {

			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
		}
		return lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx, nil

	// Static remote key feature only.
	case channelFeatures.OnlyContains(lnwire.StaticRemoteKeyRequired):
		ok, missings := checkFeaturesDiff(local, remote, lnwire.StaticRemoteKeyOptional)
		if !ok {
			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missings)
		}
		return lnwallet.CommitmentTypeTweakless, nil

	// Simple taproot channels only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
	):

		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
		)
		if !ok {

			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels with scid only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ScidAliasRequired,
	):

		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ScidAliasOptional,
		)
		if !ok {

			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels with zero conf only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ZeroConfRequired,
	):

		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ZeroConfOptional,
		)
		if !ok {

			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels with scid and zero conf.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
	):

		ok, missing := checkFeaturesDiff(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ZeroConfOptional,
		)
		if !ok {

			return 0, fmt.Errorf("%w: missing features: %v",
				errUnsupportedChannelType, missing)
		}

		return lnwallet.CommitmentTypeSimpleTaproot, nil

	// Simple taproot channels overlay only.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootOverlayChansRequired,
	):

		if !checkFeaturesDiff(
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

		if !checkFeaturesDiff(
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

		if !checkFeaturesDiff(
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

		if !checkFeaturesDiff(
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

// implicitNegotiateCommitmentType negotiates the commitment type of a channel
// implicitly by choosing the latest type supported by the local and remote
// features.
func implicitNegotiateCommitmentType(local,
	remote *lnwire.FeatureVector) (*lnwire.ChannelType,
	lnwallet.CommitmentType) {

	// If both peers are signalling support for anchor commitments with
	// zero-fee HTLC transactions, we'll use this type.
	ok, _ := checkFeaturesDiff(local, remote, lnwire.AnchorsZeroFeeHtlcTxOptional)
	if ok {
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
	ok, _ = checkFeaturesDiff(local, remote, lnwire.StaticRemoteKeyOptional)
	if ok {
		chanType := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			lnwire.StaticRemoteKeyRequired,
		))

		return &chanType, lnwallet.CommitmentTypeTweakless
	}

	// Otherwise we'll fall back to the legacy type.
	chanType := lnwire.ChannelType(*lnwire.NewRawFeatureVector())
	return &chanType, lnwallet.CommitmentTypeLegacy
}

// keeps track of the feature bit mismatches between the local and remote nodes.
type featuresBitDiff struct {
	local  []lnwire.FeatureBit
	remote []lnwire.FeatureBit
}

// isEmpty returns true if the local and remote features are empty arrays.
func (f *featuresBitDiff) isEmpty() bool {
	return len(f.local) == 0 && len(f.remote) == 0
}

// String returns the string version of the struct FeaturesBitDiff.
func (f featuresBitDiff) String() string {
	return fmt.Sprintf("local %v - remote %v", f.local, f.remote)
}

// checkFeaturesDiff determines whether a set of features is supported by both the
// local and remote features. A FeaturesBitDiff struct is returned with
// the missing feature bits for each side.
func checkFeaturesDiff(local, remote *lnwire.FeatureVector,
	features ...lnwire.FeatureBit) (bool, featuresBitDiff) {

	missing := featuresBitDiff{
		local:  make([]lnwire.FeatureBit, 0),
		remote: make([]lnwire.FeatureBit, 0),
	}
	for _, feature := range features {
		if !local.HasFeature(feature) {
			missing.local = append(missing.local, feature)
		}

		if !remote.HasFeature(feature) {
			missing.remote = append(missing.remote, feature)
		}
	}

	return missing.isEmpty(), missing
}

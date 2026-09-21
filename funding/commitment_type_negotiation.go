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

	// ErrDeprecatedChanType is returned when settling on the legacy
	// commitment type is the only option left, either because the caller of
	// our own RPC asked for it or because automatic selection would have
	// fallen back to it. We keep operating the legacy channels we already
	// have, but no longer open new ones.
	//
	// Unlike lnwire.ErrChanTypeDeprecated, which we send to a peer whose
	// proposal we reject, this never goes on the wire. The audience is our
	// own operator, who can act on the answer, so it spells out what to use
	// instead.
	ErrDeprecatedChanType = errors.New("the legacy commitment type is " +
		"deprecated, new channels must use the static remote key " +
		"commitment type or later")
)

// negotiateCommitmentType negotiates the commitment type of a newly opened
// channel. If a desiredChanType is provided, explicit negotiation for said type
// will be attempted if the set of both local and remote features support it.
// Otherwise, implicit negotiation will be attempted.
//
// The legacy commitment type is never selected, whether it was requested
// explicitly or would only have been reached by falling back.
//
// The returned ChannelType is nil when implicit negotiation is used. An error
// is returned if desiredChanType is unsupported or deprecated, or if no
// supported type can be selected implicitly.
func negotiateCommitmentType(desiredChanType *lnwire.ChannelType, local,
	remote *lnwire.FeatureVector) (*lnwire.ChannelType,
	lnwallet.CommitmentType, error) {

	// BOLT#2 specifies we MUST use explicit negotiation if both peers
	// signal for it.
	explicitNegotiation := hasFeatures(
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
		defaultChanType, commitType, err := implicitNegotiateCommitmentType(
			local, remote,
		)
		if err != nil {
			return nil, 0, err
		}

		return defaultChanType, commitType, nil

	// A specific channel type was requested, but we can't explicitly signal
	// it. So if implicit negotiation wouldn't select the desired channel
	// type, we must return an error.
	case !explicitNegotiation && chanTypeRequested:
		implicitChanType, commitType, err := implicitNegotiateCommitmentType(
			local, remote,
		)
		if err != nil {
			return nil, 0, err
		}

		expected := lnwire.RawFeatureVector(*desiredChanType)
		actual := lnwire.RawFeatureVector(*implicitChanType)
		if !expected.Equals(&actual) {
			return nil, 0, errUnsupportedChannelType
		}

		return nil, commitType, nil

	default: // !explicitNegotiation && !chanTypeRequested
		_, commitType, err := implicitNegotiateCommitmentType(
			local, remote,
		)
		if err != nil {
			return nil, 0, err
		}

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

	// Simple taproot channels only.
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

	// Simple taproot channels with scid only.
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

	// Simple taproot channels with zero conf only.
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

	// Simple taproot channels with scid and zero conf.
	case channelFeatures.OnlyContains(
		lnwire.SimpleTaprootChannelsRequiredStaging,
		lnwire.ZeroConfRequired,
		lnwire.ScidAliasRequired,
	):

		if !hasFeatures(
			local, remote,
			lnwire.SimpleTaprootChannelsOptionalStaging,
			lnwire.ZeroConfOptional,
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

	// An empty channel type asks for the legacy commitment type, which was
	// removed from the spec in 2024 and which we refuse outright, not by
	// configuration. Note that this branch performs no feature check of its
	// own, since the legacy type predates feature bits entirely: any peer
	// sending an empty channel_type TLV used to get a legacy channel out of
	// us no matter what either side signalled.
	case channelFeatures.IsEmpty():
		return 0, lnwire.ErrChanTypeDeprecated

	default:
		return 0, errUnsupportedChannelType
	}
}

// implicitNegotiateCommitmentType negotiates the commitment type of a channel
// implicitly by choosing the latest type supported by the local and remote
// features.
//
// An error is returned if there is no mutually supported type above the legacy
// one, which we no longer open.
func implicitNegotiateCommitmentType(local,
	remote *lnwire.FeatureVector) (*lnwire.ChannelType,
	lnwallet.CommitmentType, error) {

	// If both peers are signalling support for anchor commitments with
	// zero-fee HTLC transactions, we'll use this type.
	if hasFeatures(local, remote, lnwire.AnchorsZeroFeeHtlcTxOptional) {
		chanType := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			lnwire.AnchorsZeroFeeHtlcTxRequired,
			lnwire.StaticRemoteKeyRequired,
		))

		return &chanType, lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			nil
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

		return &chanType, lnwallet.CommitmentTypeTweakless, nil
	}

	// Without a mutually supported type above it, the only one left to fall
	// back on is the legacy type, which we never open. Either side failing
	// to signal static remote key is enough to end up here.
	return nil, 0, ErrDeprecatedChanType
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

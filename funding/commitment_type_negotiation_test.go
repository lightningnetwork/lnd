package funding

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestCommitmentTypeNegotiation tests all of the possible paths of a channel
// commitment type negotiation.
func TestCommitmentTypeNegotiation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name              string
		channelFeatures   *lnwire.RawFeatureVector
		localFeatures     *lnwire.RawFeatureVector
		remoteFeatures    *lnwire.RawFeatureVector
		expectsCommitType lnwallet.CommitmentType
		expectsChanType   *lnwire.ChannelType
		zeroConf          bool
		scidAlias         bool
		expectsErr        error
	}{
		{
			name: "explicit missing remote negotiation feature",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			),
			//nolint:ll
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType:   nil,
			expectsErr:        nil,
		},
		{
			name: "explicit missing remote commitment feature",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsErr: errUnsupportedChannelType,
		},
		{
			name: "explicit zero-conf script enforced",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfRequired,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.ScriptEnforcedLeaseRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfOptional,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ScriptEnforcedLeaseOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ScriptEnforcedLeaseOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeScriptEnforcedLease,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.ZeroConfRequired,
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.ScriptEnforcedLeaseRequired,
				),
			),
			zeroConf:   true,
			expectsErr: nil,
		},
		{
			name: "explicit zero-conf anchors",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfRequired,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfOptional,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.ZeroConfOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.ZeroConfRequired,
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
				),
			),
			zeroConf:   true,
			expectsErr: nil,
		},
		{
			name: "explicit scid-alias script enforced",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasRequired,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.ScriptEnforcedLeaseRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasOptional,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ScriptEnforcedLeaseOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ScriptEnforcedLeaseOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeScriptEnforcedLease,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.ScidAliasRequired,
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.ScriptEnforcedLeaseRequired,
				),
			),
			scidAlias:  true,
			expectsErr: nil,
		},
		{
			name: "explicit scid-alias anchors",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasRequired,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasOptional,
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.ScidAliasOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.ScidAliasRequired,
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
				),
			),
			scidAlias:  true,
			expectsErr: nil,
		},
		{
			name: "explicit anchors",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
				),
			),
			expectsErr: nil,
		},
		{
			name: "explicit tweakless",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeTweakless,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.StaticRemoteKeyRequired,
				),
			),
			expectsErr: nil,
		},
		{
			name:            "explicit legacy",
			channelFeatures: lnwire.NewRawFeatureVector(),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeLegacy,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(),
			),
			expectsErr: nil,
		},
		// Both sides signal the explicit chan type bit, so we expect
		// that we return the corresponding chan type feature bits,
		// even though we didn't set a desired channel type.
		{
			name:            "default explicit anchors",
			channelFeatures: nil,
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: (*lnwire.ChannelType)(
				lnwire.NewRawFeatureVector(
					lnwire.StaticRemoteKeyRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
				),
			),
			expectsErr: nil,
		},
		{
			name:            "implicit tweakless",
			channelFeatures: nil,
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeTweakless,
			expectsChanType:   nil,
			expectsErr:        nil,
		},
		{
			name:            "implicit legacy",
			channelFeatures: nil,
			localFeatures:   lnwire.NewRawFeatureVector(),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeLegacy,
			expectsChanType:   nil,
			expectsErr:        nil,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		ok := t.Run(testCase.name, func(t *testing.T) {
			localFeatures := lnwire.NewFeatureVector(
				testCase.localFeatures, lnwire.Features,
			)
			remoteFeatures := lnwire.NewFeatureVector(
				testCase.remoteFeatures, lnwire.Features,
			)

			var channelType *lnwire.ChannelType
			if testCase.channelFeatures != nil {
				channelType = new(lnwire.ChannelType)
				*channelType = lnwire.ChannelType(
					*testCase.channelFeatures,
				)
			}

			lChan, lCommit, err := negotiateCommitmentType(
				channelType, localFeatures, remoteFeatures,
			)

			var (
				localZc    bool
				localScid  bool
				remoteZc   bool
				remoteScid bool
			)

			if lChan != nil {
				localFv := lnwire.RawFeatureVector(*lChan)
				localZc = localFv.IsSet(
					lnwire.ZeroConfRequired,
				)
				localScid = localFv.IsSet(
					lnwire.ScidAliasRequired,
				)
			}

			require.Equal(t, testCase.zeroConf, localZc)
			require.Equal(t, testCase.scidAlias, localScid)
			require.Equal(t, testCase.expectsErr, err)

			rChan, rCommit, err := negotiateCommitmentType(
				channelType, remoteFeatures, localFeatures,
			)

			if rChan != nil {
				remoteFv := lnwire.RawFeatureVector(*rChan)
				remoteZc = remoteFv.IsSet(
					lnwire.ZeroConfRequired,
				)
				remoteScid = remoteFv.IsSet(
					lnwire.ScidAliasRequired,
				)
			}

			require.Equal(t, testCase.zeroConf, remoteZc)
			require.Equal(t, testCase.scidAlias, remoteScid)
			require.Equal(t, testCase.expectsErr, err)

			if testCase.expectsErr != nil {
				return
			}

			require.Equal(
				t, testCase.expectsCommitType, lCommit,
				testCase.name,
			)
			require.Equal(
				t, testCase.expectsCommitType, rCommit,
				testCase.name,
			)

			require.Equal(
				t, testCase.expectsChanType, lChan,
				testCase.name,
			)
			require.Equal(
				t, testCase.expectsChanType, rChan,
				testCase.name,
			)
		})
		if !ok {
			return
		}
	}
}

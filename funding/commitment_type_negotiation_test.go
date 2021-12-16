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
		mustBeExplicit    bool
		channelFeatures   *lnwire.RawFeatureVector
		localFeatures     *lnwire.RawFeatureVector
		remoteFeatures    *lnwire.RawFeatureVector
		expectsCommitType lnwallet.CommitmentType
		expectsChanType   lnwire.ChannelType
		expectsErr        error
	}{
		{
			name: "explicit missing remote negotiation feature",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: lnwire.ChannelType(*lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			)),
			expectsErr: nil,
		},
		{
			name: "local funder wants explicit, remote doesn't " +
				"support so fall back",
			mustBeExplicit: true,
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			),
			expectsErr: errUnsupportedExplicitNegotiation,
		},
		{
			name: "explicit missing remote commitment feature",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
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
			name: "explicit anchors",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: lnwire.ChannelType(*lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			)),
			expectsErr: nil,
		},
		{
			name: "explicit tweakless",
			channelFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
			),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeTweakless,
			expectsChanType: lnwire.ChannelType(*lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
			)),
			expectsErr: nil,
		},
		{
			name:            "explicit legacy",
			channelFeatures: lnwire.NewRawFeatureVector(),
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeLegacy,
			expectsChanType:   lnwire.ChannelType(*lnwire.NewRawFeatureVector()),
			expectsErr:        nil,
		},
		// Both sides signal the explicit chan type bit, so we expect
		// that we return the corresponding chan type feature bits,
		// even though we didn't set an explicit channel type.
		{
			name:            "implicit anchors",
			channelFeatures: nil,
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.ExplicitChannelTypeOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeAnchorsZeroFeeHtlcTx,
			expectsChanType: lnwire.ChannelType(*lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
			)),
			expectsErr: nil,
		},
		{
			name:            "implicit tweakless",
			channelFeatures: nil,
			localFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
			),
			remoteFeatures: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyOptional,
			),
			expectsCommitType: lnwallet.CommitmentTypeTweakless,
			expectsChanType: lnwire.ChannelType(*lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
			)),
			expectsErr: nil,
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
			expectsChanType:   lnwire.ChannelType(*lnwire.NewRawFeatureVector()),
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
			_, localChanType, localCommitType, err := negotiateCommitmentType(
				channelType, localFeatures, remoteFeatures,
				testCase.mustBeExplicit,
			)
			require.Equal(t, testCase.expectsErr, err)

			_, remoteChanType, remoteCommitType, err := negotiateCommitmentType(
				channelType, remoteFeatures, localFeatures,
				testCase.mustBeExplicit,
			)
			require.Equal(t, testCase.expectsErr, err)

			if testCase.expectsErr != nil {
				return
			}

			require.Equal(
				t, testCase.expectsCommitType, localCommitType,
				testCase.name,
			)
			require.Equal(
				t, testCase.expectsCommitType, remoteCommitType,
				testCase.name,
			)

			require.Equal(
				t, testCase.expectsChanType, *localChanType,
				testCase.name,
			)
			require.Equal(
				t, testCase.expectsChanType, *remoteChanType,
				testCase.name,
			)
		})
		if !ok {
			return
		}
	}
}

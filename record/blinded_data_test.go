package record

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

//nolint:lll
const pubkeyStr = "02eec7245d6b7d2ccb30380bfbe2a3648cd7a942653f5aa340edcea1f283686619"

func pubkey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	nodeBytes, err := hex.DecodeString(pubkeyStr)
	require.NoError(t, err)

	nodePk, err := btcec.ParsePubKey(nodeBytes)
	require.NoError(t, err)

	return nodePk
}

// TestBlindedDataEncoding tests encoding and decoding of blinded data blobs.
// These tests specifically cover cases where the variable length encoded
// integers values have different numbers of leading zeros trimmed because
// these TLVs are the first composite records with variable length tlvs
// (previously, a variable length integer would take up the whole record).
func TestBlindedDataEncoding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		baseFee  uint32
		htlcMin  lnwire.MilliSatoshi
		features *lnwire.FeatureVector
	}{
		{
			name:    "zero variable values",
			baseFee: 0,
			htlcMin: 0,
		},
		{
			name:    "zeros trimmed",
			baseFee: math.MaxUint32 / 2,
			htlcMin: math.MaxUint64 / 2,
		},
		{
			name:    "no zeros trimmed",
			baseFee: math.MaxUint32,
			htlcMin: math.MaxUint64,
		},
		{
			name:     "nil feature vector",
			features: nil,
		},
		{
			name:     "non-nil, but empty feature vector",
			features: lnwire.EmptyFeatureVector(),
		},
		{
			name: "populated feature vector",
			features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(lnwire.AMPOptional),
				lnwire.Features,
			),
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			// Create a standard set of blinded route data, using
			// the values from our test case for the variable
			// length encoded values.
			channelID := lnwire.NewShortChanIDFromInt(1)
			encodedData := &BlindedRouteData{
				ShortChannelID: &channelID,
				NextNodeID:     pubkey(t),
				RelayInfo: &PaymentRelayInfo{
					FeeRate:         2,
					CltvExpiryDelta: 3,
					BaseFee:         testCase.baseFee,
				},
				Constraints: &PaymentConstraints{
					MaxCltvExpiry:   4,
					HtlcMinimumMsat: testCase.htlcMin,
				},
				Features: testCase.features,
			}

			encoded, err := EncodeBlindedRouteData(encodedData)
			require.NoError(t, err)

			// We fill a non-nil feature vector if there is no
			// features tlv, so we set our expected feature vector
			// to an empty one if that's what we expect
			if encodedData.Features == nil ||
				encodedData.Features.IsEmpty() {

				//nolint:lll
				encodedData.Features = lnwire.EmptyFeatureVector()
			}

			b := bytes.NewBuffer(encoded)
			decodedData, err := DecodeBlindedRouteData(b)
			require.NoError(t, err)

			require.Equal(t, encodedData, decodedData)
		})
	}
}

package record

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

//nolint:ll
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
		name        string
		baseFee     lnwire.MilliSatoshi
		htlcMin     lnwire.MilliSatoshi
		features    *lnwire.FeatureVector
		constraints bool
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
		{
			name:        "no payment constraints",
			constraints: true,
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
			info := PaymentRelayInfo{
				FeeRate:         2,
				CltvExpiryDelta: 3,
				BaseFee:         testCase.baseFee,
			}

			var constraints *PaymentConstraints
			if testCase.constraints {
				constraints = &PaymentConstraints{
					MaxCltvExpiry:   4,
					HtlcMinimumMsat: testCase.htlcMin,
				}
			}

			encodedData := NewNonFinalBlindedRouteData(
				channelID, pubkey(t), info, constraints,
				testCase.features,
			)

			encoded, err := EncodeBlindedRouteData(encodedData)
			require.NoError(t, err)

			b := bytes.NewBuffer(encoded)
			decodedData, err := DecodeBlindedRouteData(b)
			require.NoError(t, err)

			require.Equal(t, encodedData, decodedData)
		})
	}
}

// TestBlindedDataFinalHopEncoding tests the encoding and decoding of a blinded
// data blob intended for the final hop of a blinded path where only the pathID
// will potentially be set.
func TestBlindedDataFinalHopEncoding(t *testing.T) {
	tests := []struct {
		name        string
		pathID      []byte
		constraints bool
	}{
		{
			name:   "with path ID",
			pathID: []byte{1, 2, 3, 4, 5, 6},
		},
		{
			name:   "with no path ID",
			pathID: nil,
		},
		{
			name:        "with path ID and constraints",
			pathID:      []byte{1, 2, 3, 4, 5, 6},
			constraints: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var constraints *PaymentConstraints
			if test.constraints {
				constraints = &PaymentConstraints{
					MaxCltvExpiry:   4,
					HtlcMinimumMsat: 5,
				}
			}

			encodedData := NewFinalHopBlindedRouteData(
				constraints, test.pathID,
			)

			encoded, err := EncodeBlindedRouteData(encodedData)
			require.NoError(t, err)

			b := bytes.NewBuffer(encoded)
			decodedData, err := DecodeBlindedRouteData(b)
			require.NoError(t, err)

			require.Equal(t, encodedData, decodedData)
		})
	}
}

// TestDummyHopBlindedDataEncoding tests the encoding and decoding of a blinded
// data blob intended for hops preceding a dummy hop in a blinded path. These
// hops provide the reader with a signal that the next hop may be a dummy hop.
func TestDummyHopBlindedDataEncoding(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	info := PaymentRelayInfo{
		FeeRate:         2,
		CltvExpiryDelta: 3,
		BaseFee:         30,
	}

	constraints := PaymentConstraints{
		MaxCltvExpiry:   4,
		HtlcMinimumMsat: 100,
	}

	routeData := NewDummyHopRouteData(priv.PubKey(), info, constraints)

	encoded, err := EncodeBlindedRouteData(routeData)
	require.NoError(t, err)

	// Assert the size of an average dummy hop payload in case we need to
	// update this constant in future.
	require.Len(t, encoded, AverageDummyHopPayloadSize)

	b := bytes.NewBuffer(encoded)
	decodedData, err := DecodeBlindedRouteData(b)
	require.NoError(t, err)

	require.Equal(t, routeData, decodedData)
}

// TestBlindedRouteDataPadding tests the PadBy method of BlindedRouteData.
func TestBlindedRouteDataPadding(t *testing.T) {
	newBlindedRouteData := func() *BlindedRouteData {
		channelID := lnwire.NewShortChanIDFromInt(1)
		info := PaymentRelayInfo{
			FeeRate:         2,
			CltvExpiryDelta: 3,
			BaseFee:         30,
		}

		constraints := &PaymentConstraints{
			MaxCltvExpiry:   4,
			HtlcMinimumMsat: 100,
		}

		return NewNonFinalBlindedRouteData(
			channelID, pubkey(t), info, constraints, nil,
		)
	}

	tests := []struct {
		name                 string
		paddingSize          int
		expectedSizeIncrease uint64
	}{
		{
			// Calling PadBy with an n value of 0 in the case where
			// there is not yet a padding field will result in a
			// zero length TLV entry being added. This will add 2
			// bytes for the type and length fields.
			name:                 "no extra padding",
			expectedSizeIncrease: 2,
		},
		{
			name: "small padding (length " +
				"field of 1 byte)",
			paddingSize:          200,
			expectedSizeIncrease: 202,
		},
		{
			name: "medium padding (length field " +
				"of 3 bytes)",
			paddingSize:          256,
			expectedSizeIncrease: 260,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := newBlindedRouteData()

			prePaddingEncoding, err := EncodeBlindedRouteData(data)
			require.NoError(t, err)

			data.PadBy(test.paddingSize)

			postPaddingEncoding, err := EncodeBlindedRouteData(data)
			require.NoError(t, err)

			require.EqualValues(
				t, test.expectedSizeIncrease,
				len(postPaddingEncoding)-
					len(prePaddingEncoding),
			)
		})
	}
}

// TestBlindedRouteVectors tests encoding/decoding of the test vectors for
// blinded route data provided in the specification.
//
//nolint:ll
func TestBlindingSpecTestVectors(t *testing.T) {
	nextBlindingOverrideStr, err := hex.DecodeString("031b84c5567b126440995d3ed5aaba0565d71e1834604819ff9c17f5e9d5dd078f")
	require.NoError(t, err)
	nextBlindingOverride, err := btcec.ParsePubKey(nextBlindingOverrideStr)
	require.NoError(t, err)

	tests := []struct {
		encoded             string
		expectedPaymentData *BlindedRouteData
		expectedPadding     int
	}{
		{
			encoded: "011a0000000000000000000000000000000000000000000000000000020800000000000006c10a0800240000009627100c06000b69e505dc0e00fd023103123456",
			expectedPaymentData: NewNonFinalBlindedRouteData(
				lnwire.ShortChannelID{
					BlockHeight: 0,
					TxIndex:     0,
					TxPosition:  1729,
				},
				nil,
				PaymentRelayInfo{
					CltvExpiryDelta: 36,
					FeeRate:         150,
					BaseFee:         10000,
				},
				&PaymentConstraints{
					MaxCltvExpiry:   748005,
					HtlcMinimumMsat: 1500,
				},
				lnwire.NewFeatureVector(
					lnwire.NewRawFeatureVector(),
					lnwire.Features,
				),
			),
			expectedPadding: 26,
		},
		{
			encoded: "020800000000000004510821031b84c5567b126440995d3ed5aaba0565d71e1834604819ff9c17f5e9d5dd078f0a0800300000006401f40c06000b69c105dc0e00",
			expectedPaymentData: NewNonFinalBlindedRouteData(
				lnwire.ShortChannelID{
					TxPosition: 1105,
				},
				nextBlindingOverride,
				PaymentRelayInfo{
					CltvExpiryDelta: 48,
					FeeRate:         100,
					BaseFee:         500,
				},
				&PaymentConstraints{
					MaxCltvExpiry:   747969,
					HtlcMinimumMsat: 1500,
				},
				lnwire.NewFeatureVector(
					lnwire.NewRawFeatureVector(),
					lnwire.Features,
				),
			),
		},
	}

	for i, test := range tests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			route, err := hex.DecodeString(test.encoded)
			require.NoError(t, err)

			buff := bytes.NewBuffer(route)

			decodedRoute, err := DecodeBlindedRouteData(buff)
			require.NoError(t, err)

			if test.expectedPadding != 0 {
				test.expectedPaymentData.PadBy(
					test.expectedPadding,
				)
			}

			require.Equal(
				t, test.expectedPaymentData, decodedRoute,
			)
		})
	}
}

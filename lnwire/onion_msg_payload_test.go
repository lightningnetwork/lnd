package lnwire

import (
	"bytes"
	"fmt"
	"testing"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// makeBlindedPath creates a BlindedPath with the given number of hops for
// testing. Each hop has a random blinded node pub and some cipher text.
func makeBlindedPath(t *testing.T, numHops int) *sphinx.BlindedPath {
	t.Helper()

	introKey, err := randPubKey()
	require.NoError(t, err)

	blindingKey, err := randPubKey()
	require.NoError(t, err)

	hops := make([]*sphinx.BlindedHopInfo, numHops)
	for i := range hops {
		nodePub, err := randPubKey()
		require.NoError(t, err)

		hops[i] = &sphinx.BlindedHopInfo{
			BlindedNodePub: nodePub,
			CipherText:     bytes.Repeat([]byte{byte(i + 1)}, 32),
		}
	}

	return &sphinx.BlindedPath{
		IntroductionPoint: introKey,
		BlindingPoint:     blindingKey,
		BlindedHops:       hops,
	}
}

// assertBlindedPathEqual compares two BlindedPaths for equality, checking each
// field.
func assertBlindedPathEqual(t *testing.T, expected,
	actual *sphinx.BlindedPath) {

	t.Helper()

	require.True(
		t,
		expected.IntroductionPoint.IsEqual(actual.IntroductionPoint),
		"IntroductionPoint mismatch",
	)
	require.True(
		t, expected.BlindingPoint.IsEqual(actual.BlindingPoint),
		"BlindingPoint mismatch",
	)
	require.Len(t, actual.BlindedHops, len(expected.BlindedHops))

	for i, expectedHop := range expected.BlindedHops {
		actualHop := actual.BlindedHops[i]

		require.True(
			t,
			expectedHop.BlindedNodePub.IsEqual(
				actualHop.BlindedNodePub,
			),
			"hop %d: BlindedNodePub mismatch", i,
		)
		require.Equal(
			t, expectedHop.CipherText, actualHop.CipherText,
			"hop %d: CipherText mismatch", i,
		)
	}
}

// encodeAndDecode is a helper that encodes a payload and decodes it into a
// fresh OnionMessagePayload.
func encodeAndDecode(t *testing.T,
	original *OnionMessagePayload) *OnionMessagePayload {

	t.Helper()

	encoded, err := original.Encode()
	require.NoError(t, err)

	decoded := NewOnionMessagePayload()
	_, err = decoded.Decode(bytes.NewReader(encoded))
	require.NoError(t, err)

	return decoded
}

// TestOnionMessagePayloadRoundTrip tests encode/decode roundtrips for various
// payload configurations.
func TestOnionMessagePayloadRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("only reply path", func(t *testing.T) {
		t.Parallel()

		original := &OnionMessagePayload{
			ReplyPath: makeBlindedPath(t, 3),
		}

		decoded := encodeAndDecode(t, original)

		require.NotNil(t, decoded.ReplyPath)
		assertBlindedPathEqual(t, original.ReplyPath, decoded.ReplyPath)
		require.Empty(t, decoded.EncryptedData)
		require.Empty(t, decoded.FinalHopTLVs)
	})

	t.Run("only encrypted data", func(t *testing.T) {
		t.Parallel()

		original := &OnionMessagePayload{
			EncryptedData: []byte("encrypted-recipient-data"),
		}

		decoded := encodeAndDecode(t, original)

		require.Nil(t, decoded.ReplyPath)
		require.Equal(
			t, original.EncryptedData, decoded.EncryptedData,
		)
		require.Empty(t, decoded.FinalHopTLVs)
	})

	t.Run("reply path and encrypted data", func(t *testing.T) {
		t.Parallel()

		original := &OnionMessagePayload{
			ReplyPath:     makeBlindedPath(t, 2),
			EncryptedData: []byte("test-ciphertext"),
		}

		decoded := encodeAndDecode(t, original)

		require.NotNil(t, decoded.ReplyPath)
		assertBlindedPathEqual(t, original.ReplyPath, decoded.ReplyPath)
		require.Equal(
			t, original.EncryptedData, decoded.EncryptedData,
		)
		require.Empty(t, decoded.FinalHopTLVs)
	})

	t.Run("single hop reply path", func(t *testing.T) {
		t.Parallel()

		original := &OnionMessagePayload{
			ReplyPath: makeBlindedPath(t, 1),
		}

		decoded := encodeAndDecode(t, original)

		require.NotNil(t, decoded.ReplyPath)
		assertBlindedPathEqual(t, original.ReplyPath, decoded.ReplyPath)
	})

	t.Run("final hop TLVs", func(t *testing.T) {
		t.Parallel()

		original := &OnionMessagePayload{
			EncryptedData: []byte("ciphertext"),
			FinalHopTLVs: []*FinalHopTLV{
				{
					TLVType: InvoiceRequestNamespaceType,
					Value:   []byte("invoice-request"),
				},
			},
		}

		decoded := encodeAndDecode(t, original)

		require.Equal(
			t, original.EncryptedData, decoded.EncryptedData,
		)
		require.Len(t, decoded.FinalHopTLVs, 1)
		require.Equal(
			t, InvoiceRequestNamespaceType,
			decoded.FinalHopTLVs[0].TLVType,
		)
		require.Equal(
			t, original.FinalHopTLVs[0].Value,
			decoded.FinalHopTLVs[0].Value,
		)
	})

	t.Run("multiple final hop TLVs", func(t *testing.T) {
		t.Parallel()

		original := &OnionMessagePayload{
			FinalHopTLVs: []*FinalHopTLV{
				{
					TLVType: InvoiceRequestNamespaceType,
					Value:   []byte("request"),
				},
				{
					TLVType: InvoiceNamespaceType,
					Value:   []byte("invoice"),
				},
				{
					TLVType: InvoiceErrorNamespaceType,
					Value:   []byte("error"),
				},
			},
		}

		decoded := encodeAndDecode(t, original)

		require.Nil(t, decoded.ReplyPath)
		require.Len(t, decoded.FinalHopTLVs, 3)

		// Decoded TLVs should be sorted by type.
		require.Equal(
			t, InvoiceRequestNamespaceType,
			decoded.FinalHopTLVs[0].TLVType,
		)
		require.Equal(
			t, InvoiceNamespaceType,
			decoded.FinalHopTLVs[1].TLVType,
		)
		require.Equal(
			t, InvoiceErrorNamespaceType,
			decoded.FinalHopTLVs[2].TLVType,
		)
	})

	t.Run("all fields populated", func(t *testing.T) {
		t.Parallel()

		original := &OnionMessagePayload{
			ReplyPath:     makeBlindedPath(t, 2),
			EncryptedData: []byte("encrypted-data"),
			FinalHopTLVs: []*FinalHopTLV{
				{
					TLVType: InvoiceNamespaceType,
					Value:   []byte("invoice-data"),
				},
			},
		}

		decoded := encodeAndDecode(t, original)

		require.NotNil(t, decoded.ReplyPath)
		assertBlindedPathEqual(t, original.ReplyPath, decoded.ReplyPath)
		require.Equal(
			t, original.EncryptedData, decoded.EncryptedData,
		)
		require.Len(t, decoded.FinalHopTLVs, 1)
		require.Equal(
			t, original.FinalHopTLVs[0].Value,
			decoded.FinalHopTLVs[0].Value,
		)
	})

	t.Run("odd unknown final hop TLV", func(t *testing.T) {
		t.Parallel()

		// Odd TLV types >= 64 that we don't explicitly recognize
		// should be preserved as FinalHopTLVs.
		original := &OnionMessagePayload{
			FinalHopTLVs: []*FinalHopTLV{
				{
					TLVType: 65,
					Value:   []byte("custom-data"),
				},
			},
		}

		decoded := encodeAndDecode(t, original)

		require.Len(t, decoded.FinalHopTLVs, 1)
		require.Equal(t, tlv.Type(65), decoded.FinalHopTLVs[0].TLVType)
		require.Equal(
			t, []byte("custom-data"),
			decoded.FinalHopTLVs[0].Value,
		)
	})
}

// TestFinalHopTLVValidate tests that FinalHopTLV.Validate correctly rejects
// types below the final hop range and accepts types within it.
func TestFinalHopTLVValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		recordType tlv.Type
		wantErr    error
	}{
		{
			name:       "type 0 rejected",
			recordType: 0,
			wantErr:    ErrNotFinalPayload,
		},
		{
			name:       "type 2 rejected",
			recordType: 2,
			wantErr:    ErrNotFinalPayload,
		},
		{
			name:       "type 63 rejected",
			recordType: 63,
			wantErr:    ErrNotFinalPayload,
		},
		{
			name:       "type 64 accepted",
			recordType: 64,
		},
		{
			name:       "type 65 accepted",
			recordType: 65,
		},
		{
			name:       "type 255 accepted",
			recordType: 255,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := &FinalHopTLV{
				TLVType: tc.recordType,
				Value:   []byte("value"),
			}

			err := f.Validate()
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestOnionMessagePayloadEncodeReplyPathNoHops tests that encoding a reply path
// with zero hops returns an error.
func TestOnionMessagePayloadEncodeReplyPathNoHops(t *testing.T) {
	t.Parallel()

	introKey, err := randPubKey()
	require.NoError(t, err)

	blindingKey, err := randPubKey()
	require.NoError(t, err)

	payload := &OnionMessagePayload{
		ReplyPath: &sphinx.BlindedPath{
			IntroductionPoint: introKey,
			BlindingPoint:     blindingKey,
			BlindedHops:       nil,
		},
	}

	_, err = payload.Encode()
	require.ErrorIs(t, err, ErrNoHops)
}

// TestOnionMessagePayloadEmpty tests that an empty payload roundtrips
// correctly.
func TestOnionMessagePayloadEmpty(t *testing.T) {
	t.Parallel()

	original := NewOnionMessagePayload()
	decoded := encodeAndDecode(t, original)

	require.Nil(t, decoded.ReplyPath)
	require.Empty(t, decoded.EncryptedData)
	require.Empty(t, decoded.FinalHopTLVs)
}

// TestOnionMessagePayloadRoundTripQuickCheck uses property-based testing to
// verify that randomly generated OnionMessagePayload values survive
// encode/decode roundtrips.
func TestOnionMessagePayloadRoundTripQuickCheck(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		original := &OnionMessagePayload{}

		// Optionally include a reply path.
		if rapid.Bool().Draw(t, "hasReplyPath") {
			original.ReplyPath = RandBlindedPath(t)
		}

		// Optionally include encrypted data.
		if rapid.Bool().Draw(t, "hasEncryptedData") {
			dataLen := rapid.IntRange(1, 256).Draw(
				t, "encryptedDataLen",
			)
			original.EncryptedData = rapid.SliceOfN(
				rapid.Byte(), dataLen, dataLen,
			).Draw(t, "encryptedData")
		}

		// Optionally include final hop TLVs. We use the three known
		// even types (64, 66, 68) since unknown even types would cause
		// decode to fail.
		knownTypes := []tlv.Type{
			InvoiceRequestNamespaceType,
			InvoiceNamespaceType,
			InvoiceErrorNamespaceType,
		}
		numFinalTLVs := rapid.IntRange(0, len(knownTypes)).Draw(
			t, "numFinalTLVs",
		)
		for i := range numFinalTLVs {
			valLen := rapid.IntRange(1, 64).Draw(
				t, fmt.Sprintf("finalTLVLen-%d", i),
			)
			original.FinalHopTLVs = append(
				original.FinalHopTLVs,
				&FinalHopTLV{
					TLVType: knownTypes[i],
					Value: rapid.SliceOfN(
						rapid.Byte(), valLen, valLen,
					).Draw(
						t,
						fmt.Sprintf("finalTLV-%d", i),
					),
				},
			)
		}

		// Encode.
		encoded, err := original.Encode()
		require.NoError(t, err)

		// Decode.
		decoded := NewOnionMessagePayload()
		_, err = decoded.Decode(bytes.NewReader(encoded))
		require.NoError(t, err)

		// Verify reply path.
		if original.ReplyPath == nil {
			require.Nil(t, decoded.ReplyPath)
		} else {
			require.NotNil(t, decoded.ReplyPath)
			require.True(
				t,
				original.ReplyPath.IntroductionPoint.IsEqual(
					decoded.ReplyPath.IntroductionPoint,
				),
			)
			require.True(
				t,
				original.ReplyPath.BlindingPoint.IsEqual(
					decoded.ReplyPath.BlindingPoint,
				),
			)
			require.Len(
				t, decoded.ReplyPath.BlindedHops,
				len(original.ReplyPath.BlindedHops),
			)
			for i, hop := range original.ReplyPath.BlindedHops {
				dHop := decoded.ReplyPath.BlindedHops[i]
				require.True(
					t,
					hop.BlindedNodePub.IsEqual(
						dHop.BlindedNodePub,
					),
				)
				require.Equal(
					t, hop.CipherText,
					dHop.CipherText,
				)
			}
		}

		// Verify encrypted data.
		require.Equal(
			t, original.EncryptedData, decoded.EncryptedData,
		)

		// Verify final hop TLVs.
		require.Len(
			t, decoded.FinalHopTLVs,
			len(original.FinalHopTLVs),
		)
		for i, orig := range original.FinalHopTLVs {
			dec := decoded.FinalHopTLVs[i]
			require.Equal(t, orig.TLVType, dec.TLVType)
			require.Equal(t, orig.Value, dec.Value)
		}
	})
}

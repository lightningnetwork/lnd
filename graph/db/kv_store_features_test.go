package graphdb

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestDeserializeChanEdgeFeaturesEmpty tests that empty feature bytes are
// handled correctly for both legacy and new formats.
func TestDeserializeChanEdgeFeaturesEmpty(t *testing.T) {
	t.Parallel()

	// Empty bytes should result in empty features.
	features, err := deserializeChanEdgeFeatures(nil)
	require.NoError(t, err)
	require.True(t, features.IsEmpty())

	features, err = deserializeChanEdgeFeatures([]byte{})
	require.NoError(t, err)
	require.True(t, features.IsEmpty())

	// New format with zero-length features: [0x00, 0x00].
	features, err = deserializeChanEdgeFeatures([]byte{0x00, 0x00})
	require.NoError(t, err)
	require.True(t, features.IsEmpty())
}

// TestDeserializeChanEdgeFeaturesLegacyFormat tests deserialization of
// feature bytes written in the legacy format (pre-v0.20), which contains
// raw feature bits without a 2-byte length prefix.
func TestDeserializeChanEdgeFeaturesLegacyFormat(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		legacyBytes   []byte
		expectedFeats []lnwire.FeatureBit
	}{
		{
			name:          "single byte - bit 0",
			legacyBytes:   []byte{0x01}, // bit 0 set
			expectedFeats: []lnwire.FeatureBit{0},
		},
		{
			name:          "single byte - bit 7",
			legacyBytes:   []byte{0x80}, // bit 7 set
			expectedFeats: []lnwire.FeatureBit{7},
		},
		{
			name:          "single byte - multiple bits",
			legacyBytes:   []byte{0x25}, // bits 0, 2, 5 set
			expectedFeats: []lnwire.FeatureBit{0, 2, 5},
		},
		{
			name:          "two bytes - bit 8",
			legacyBytes:   []byte{0x01, 0x00}, // bit 8 set
			expectedFeats: []lnwire.FeatureBit{8},
		},
		{
			name:          "two bytes - bits 0 and 15",
			legacyBytes:   []byte{0x80, 0x01}, // bits 0 and 15 set
			expectedFeats: []lnwire.FeatureBit{0, 15},
		},
		{
			// bit 1 (DataLossProtectOptional).
			name:        "common features - data loss protect",
			legacyBytes: []byte{0x02},
			expectedFeats: []lnwire.FeatureBit{
				lnwire.DataLossProtectOptional,
			},
		},
		{
			// bits 1, 7, 9, 13, 15 = DataLossProtect,
			// GossipQueries, TLVOnion, StaticRemoteKey,
			// PaymentAddr.
			name:          "multiple common features",
			legacyBytes:   []byte{0xA2, 0x82},
			expectedFeats: []lnwire.FeatureBit{1, 7, 9, 13, 15},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			features, err := deserializeChanEdgeFeatures(
				tc.legacyBytes,
			)
			require.NoError(t, err)

			for _, bit := range tc.expectedFeats {
				require.True(t, features.IsSet(bit),
					"expected bit %d to be set", bit)
			}

			// Verify no extra bits are set by creating expected
			// feature vector and comparing.
			expectedRaw := lnwire.NewRawFeatureVector(
				tc.expectedFeats...,
			)
			require.True(t, expectedRaw.Equals(
				features.RawFeatureVector),
				"feature vectors don't match")
		})
	}
}

// TestDeserializeChanEdgeFeaturesNewFormat tests deserialization of
// feature bytes written in the new format (v0.20+), which contains
// a 2-byte big-endian length prefix followed by raw feature bits.
func TestDeserializeChanEdgeFeaturesNewFormat(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		expectedFeats []lnwire.FeatureBit
	}{
		{
			name:          "empty features",
			expectedFeats: nil,
		},
		{
			name:          "single feature bit 0",
			expectedFeats: []lnwire.FeatureBit{0},
		},
		{
			name:          "single feature bit 15",
			expectedFeats: []lnwire.FeatureBit{15},
		},
		{
			name:          "multiple features",
			expectedFeats: []lnwire.FeatureBit{1, 5, 9, 13, 17},
		},
		{
			name: "common lightning features",
			expectedFeats: []lnwire.FeatureBit{
				lnwire.DataLossProtectOptional,
				lnwire.GossipQueriesOptional,
				lnwire.TLVOnionPayloadOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.PaymentAddrOptional,
			},
		},
		{
			name: "high bit features",
			expectedFeats: []lnwire.FeatureBit{
				lnwire.AMPOptional,     // 31
				lnwire.KeysendOptional, // 55
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create feature vector and encode in new format.
			rawFeatures := lnwire.NewRawFeatureVector(
				tc.expectedFeats...,
			)
			fv := lnwire.NewFeatureVector(
				rawFeatures, lnwire.Features,
			)

			// Encode using the new format (with length prefix).
			var buf bytes.Buffer
			err := fv.Encode(&buf)
			require.NoError(t, err)

			// Deserialize and verify.
			features, err := deserializeChanEdgeFeatures(
				buf.Bytes(),
			)
			require.NoError(t, err)

			for _, bit := range tc.expectedFeats {
				require.True(t, features.IsSet(bit),
					"expected bit %d to be set", bit)
			}

			// Verify feature equality.
			require.True(t, rawFeatures.Equals(
				features.RawFeatureVector),
			)
		})
	}
}

// TestDeserializeChanEdgeFeaturesFormatDetection tests that the format
// detection correctly distinguishes between legacy and new formats.
func TestDeserializeChanEdgeFeaturesFormatDetection(t *testing.T) {
	t.Parallel()

	// Test that legacy format bytes that could theoretically be confused
	// with new format are handled correctly. This shouldn't happen in
	// practice because in legacy format the first byte always has at least
	// one bit set (the highest feature bit determines the byte length).

	// Create a feature vector with bit 8 set (requires 2 bytes in legacy).
	// Legacy format: [0x01, 0x00] (big-endian, high byte first).
	// As a length, 0x0100 = 256, which != 0 (len-2), so correctly detected
	// as legacy.
	legacyBit8 := []byte{0x01, 0x00}
	features, err := deserializeChanEdgeFeatures(legacyBit8)
	require.NoError(t, err)
	require.True(t, features.IsSet(8))
	require.False(t, features.IsSet(0))

	// New format with bit 8: [0x00, 0x02, 0x01, 0x00]
	// Length prefix 0x0002 = 2, remaining 2 bytes = feature bits.
	newFormatBit8 := []byte{0x00, 0x02, 0x01, 0x00}
	features, err = deserializeChanEdgeFeatures(newFormatBit8)
	require.NoError(t, err)
	require.True(t, features.IsSet(8))
	require.False(t, features.IsSet(0))

	// Test single byte legacy format - cannot be confused with new format
	// since new format minimum is 2 bytes (the length prefix).
	legacyBit0 := []byte{0x01}
	features, err = deserializeChanEdgeFeatures(legacyBit0)
	require.NoError(t, err)
	require.True(t, features.IsSet(0))
}

// TestDeserializeChanEdgeFeaturesRoundTrip tests that features can be
// serialized and deserialized correctly using the new format.
func TestDeserializeChanEdgeFeaturesRoundTrip(t *testing.T) {
	t.Parallel()

	testFeatureSets := [][]lnwire.FeatureBit{
		{},
		{0},
		{7},
		{8},
		{15},
		{0, 1, 2, 3, 4, 5, 6, 7},
		{8, 9, 10, 11, 12, 13, 14, 15},
		{0, 8, 16, 24, 32},
		{
			lnwire.DataLossProtectOptional,
			lnwire.GossipQueriesOptional,
			lnwire.TLVOnionPayloadOptional,
			lnwire.StaticRemoteKeyOptional,
			lnwire.PaymentAddrOptional,
			lnwire.MPPOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
		},
	}

	for _, featureBits := range testFeatureSets {
		// Create and encode.
		rawFeatures := lnwire.NewRawFeatureVector(featureBits...)
		fv := lnwire.NewFeatureVector(rawFeatures, lnwire.Features)

		var buf bytes.Buffer
		err := fv.Encode(&buf)
		require.NoError(t, err)

		// Deserialize.
		decoded, err := deserializeChanEdgeFeatures(buf.Bytes())
		require.NoError(t, err)

		// Verify equality.
		require.True(t, rawFeatures.Equals(decoded.RawFeatureVector),
			"mismatch for features %v", featureBits)
	}
}

// TestDeserializeChanEdgeFeaturesPropertyBased uses property-based testing
// to verify that the deserialization works correctly for arbitrary feature
// combinations in both legacy and new formats.
func TestDeserializeChanEdgeFeaturesPropertyBased(t *testing.T) {
	t.Parallel()

	// Test legacy format: raw feature bytes without length prefix.
	rapid.Check(t, func(t *rapid.T) {
		// Generate random feature bits (max 256 to keep reasonable).
		numFeatures := rapid.IntRange(0, 20).Draw(t, "numFeatures")
		featureBits := make([]lnwire.FeatureBit, numFeatures)
		for i := 0; i < numFeatures; i++ {
			featureBits[i] = lnwire.FeatureBit(
				rapid.IntRange(0, 255).Draw(t, "featureBit"),
			)
		}

		// Create feature vector.
		rawFeatures := lnwire.NewRawFeatureVector(featureBits...)

		// Encode without length prefix (legacy format).
		var buf bytes.Buffer
		err := rawFeatures.EncodeBase256(&buf)
		require.NoError(t, err)

		// Deserialize.
		decoded, err := deserializeChanEdgeFeatures(buf.Bytes())
		require.NoError(t, err)

		// Verify equality.
		require.True(t, rawFeatures.Equals(decoded.RawFeatureVector))
	})

	// Test new format: with length prefix.
	rapid.Check(t, func(t *rapid.T) {
		// Generate random feature bits.
		numFeatures := rapid.IntRange(0, 20).Draw(t, "numFeatures")
		featureBits := make([]lnwire.FeatureBit, numFeatures)
		for i := 0; i < numFeatures; i++ {
			featureBits[i] = lnwire.FeatureBit(
				rapid.IntRange(0, 255).Draw(t, "featureBit"),
			)
		}

		// Create feature vector.
		rawFeatures := lnwire.NewRawFeatureVector(featureBits...)
		fv := lnwire.NewFeatureVector(rawFeatures, lnwire.Features)

		// Encode with length prefix (new format).
		var buf bytes.Buffer
		err := fv.Encode(&buf)
		require.NoError(t, err)

		// Deserialize.
		decoded, err := deserializeChanEdgeFeatures(buf.Bytes())
		require.NoError(t, err)

		// Verify equality.
		require.True(t, rawFeatures.Equals(decoded.RawFeatureVector))
	})
}

// TestDeserializeChanEdgeFeaturesLegacyFormatNoCollision verifies that
// the format detection cannot have false positives where legacy format
// bytes are incorrectly detected as new format.
func TestDeserializeChanEdgeFeaturesLegacyFormatNoCollision(t *testing.T) {
	t.Parallel()

	// The detection works through canonical encoding verification.
	// Even if a legacy vector accidentally matches the length check
	// (e.g., a 258-byte vector starting with 0x01, 0x00), the decode/
	// re-encode check will fail because the legacy encoding won't be
	// canonical when interpreted as new format payload.

	rapid.Check(t, func(t *rapid.T) {
		// Generate feature bits with higher range to catch more edge
		// cases, including vectors that could match the length check.
		maxBit := rapid.IntRange(0, 2200).Draw(t, "maxBit")
		numExtra := rapid.IntRange(0, 10).Draw(t, "numExtra")

		featureBits := []lnwire.FeatureBit{lnwire.FeatureBit(maxBit)}
		for i := 0; i < numExtra; i++ {
			bit := rapid.IntRange(0, maxBit).Draw(t, "extraBit")
			featureBits = append(featureBits,
				lnwire.FeatureBit(bit))
		}

		rawFeatures := lnwire.NewRawFeatureVector(featureBits...)

		// Encode in legacy format.
		var buf bytes.Buffer
		err := rawFeatures.EncodeBase256(&buf)
		require.NoError(t, err)

		legacyBytes := buf.Bytes()
		if len(legacyBytes) < 2 {
			// Single byte can't be confused with new format.
			return
		}

		// Verify deserialization still works correctly regardless of
		// whether the length check happens to match.
		decoded, err := deserializeChanEdgeFeatures(legacyBytes)
		require.NoError(t, err)
		require.True(t, rawFeatures.Equals(decoded.RawFeatureVector),
			"mismatch for legacy bytes %x with maxBit %d",
			legacyBytes, maxBit)
	})
}

// TestDeserializeChanEdgeFeaturesLengthCheckCollision specifically tests the
// edge case where a legacy feature vector accidentally satisfies the length
// check condition (first 2 bytes as uint16 == len - 2). This can happen with
// a 258-byte vector starting with 0x01, 0x00, where 256 == 258 - 2.
// The canonical encoding verification should correctly identify this as legacy
// format.
func TestDeserializeChanEdgeFeaturesLengthCheckCollision(t *testing.T) {
	t.Parallel()

	// Create a legacy feature vector that will produce bytes where the
	// first two bytes, interpreted as a length, equal len - 2.
	//
	// To get 258 bytes in legacy format, we need bit 2063 set (258*8-1).
	// The first byte will be 0x01 (bit 2056 is in byte 0, and we need
	// bit 2063 which is 0x80, but the bytes are big-endian so byte 0
	// contains the high bits). Actually let's work this out:
	//
	// For 258 bytes, bits 2056-2063 are in byte 0.
	// Setting bit 2056 gives byte[0] = 0x01.
	// If byte[0] = 0x01 and byte[1] = 0x00, then as uint16 = 256 = 258-2.
	//
	// So we need: bit 2056 set (gives 0x01 in byte 0), and no bits in
	// byte 1 set (bits 2048-2055), and at least one bit set below to
	// ensure we have full 258 bytes (bit 0 to ensure byte 257 is non-zero
	// won't work since it affects the last byte...).
	//
	// Actually the encoding is that the first byte contains the HIGHEST
	// bits. So for 258 bytes:
	// - byte[0] contains bits 2056-2063
	// - byte[1] contains bits 2048-2055
	// - ...
	// - byte[257] contains bits 0-7
	//
	// To get byte[0] = 0x01 and byte[1] = 0x00:
	// - Set bit 2056 (gives 0x01 in byte 0)
	// - Don't set bits 2048-2055 (keeps byte 1 = 0x00)
	//
	// We also need to set some lower bit to have meaningful features.
	featureBits := []lnwire.FeatureBit{
		2056, // This gives 0x01 in first byte (258 bytes total)
		0,    // Set bit 0 for a meaningful feature
	}

	rawFeatures := lnwire.NewRawFeatureVector(featureBits...)

	// Encode in legacy format.
	var buf bytes.Buffer
	err := rawFeatures.EncodeBase256(&buf)
	require.NoError(t, err)

	legacyBytes := buf.Bytes()
	require.Len(t, legacyBytes, 258, "expected 258 bytes for bit 2056")

	// Verify the collision condition: first 2 bytes as uint16 == len - 2.
	encodedLen := binary.BigEndian.Uint16(legacyBytes[:2])
	require.Equal(t, uint16(256), encodedLen,
		"expected first 2 bytes to encode 256")
	require.Equal(t, 256, len(legacyBytes)-2,
		"expected length check to match")

	// Despite the length check matching, deserialization should still
	// correctly identify this as legacy format (via canonical encoding
	// verification) and decode it properly.
	decoded, err := deserializeChanEdgeFeatures(legacyBytes)
	require.NoError(t, err)
	require.True(t, decoded.IsSet(2056), "bit 2056 should be set")
	require.True(t, decoded.IsSet(0), "bit 0 should be set")
	require.True(t, rawFeatures.Equals(decoded.RawFeatureVector),
		"feature vectors should match")
}

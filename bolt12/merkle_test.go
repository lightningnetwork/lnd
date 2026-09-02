package bolt12

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestMerkleRootVectors verifies the Merkle root computation against every test
// case in signature-test.json.
func TestMerkleRootVectors(t *testing.T) {
	t.Parallel()

	vectors := loadSignatureVectors(t)
	require.NotEmpty(t, vectors)

	for _, tc := range vectors {
		t.Run(tc.Comment, func(t *testing.T) {
			t.Parallel()

			var records []tlv.Record

			switch {
			case tc.Bolt12 != "":
				// Decode the bech32 string to get TLV bytes,
				// then convert into the record view merkleRoot
				// consumes.
				_, tlvBytes, err := Decode(tc.Bolt12)
				require.NoError(t, err)

				records = streamToRecords(t, tlvBytes)

			case tc.TLV == "n1":
				// Build records from the leaf descriptions. The
				// n1 namespace is synthetic. There is no bech32
				// representation, so we recover each record
				// from its hex prefix.
				records = buildN1Records(t, tc)

			default:
				t.Fatalf("vector %q: neither bolt12 nor "+
					"n1: refusing to assume the "+
					"wrong synthesis path", tc.Comment)
			}

			// Keep only the records that participate in the
			// signature root.
			filtered := signableTLVs(records)

			root, err := merkleRoot(filtered)
			require.NoError(t, err)

			expectedRoot, err := hex.DecodeString(tc.Merkle)
			require.NoError(t, err)
			require.Equal(
				t, expectedRoot, root[:],
				"merkle root mismatch",
			)
		})
	}
}

// buildN1Records constructs tlv.Record entries for the simple n1 test vectors
// by parsing the leaf hex values from the test JSON.
func buildN1Records(t *testing.T, tc sigTestVector) []tlv.Record {
	t.Helper()

	var result []tlv.Record

	for _, leafJSON := range tc.Leaves {
		var leafMap map[string]string
		require.NoError(t, json.Unmarshal(leafJSON, &leafMap))

		// Find the LnLeaf key to extract the TLV bytes.
		// Key format: H(`LnLeaf`,<hex>)
		prefix := "H(`LnLeaf`,"
		for key := range leafMap {
			if len(key) <= len(prefix) ||
				key[:len(prefix)] != prefix {

				continue
			}

			// Extract hex between the comma and closing
			// paren.
			hexStr := key[len(prefix) : len(key)-1]
			fullBytes, err := hex.DecodeString(hexStr)
			require.NoError(t, err)

			result = append(
				result,
				recordFromWireBytes(t, fullBytes),
			)

			break
		}
	}

	return result
}

// TestLeafHash verifies individual leaf hash computations from the test
// vectors.
func TestLeafHash(t *testing.T) {
	t.Parallel()

	const (
		// From the first test vector: H("LnLeaf", 010203e8).
		inputStr    = "010203e8"
		expectedStr = "67a2a995433890d8fe0c18a1765ad19e98f1fc" +
			"feff14c13a45bbc80964a78cf7"
	)

	input, err := hex.DecodeString(inputStr)
	require.NoError(t, err)

	expected, err := hex.DecodeString(expectedStr)
	require.NoError(t, err)

	got := leafHash(input)
	require.Equal(t, expected, got[:])
}

// TestNonceHash verifies the nonce hash computation. The type 1001 case pins
// the multi-byte BigSize encoding of the type, which none of the vendored
// vectors exercise.
func TestNonceHash(t *testing.T) {
	t.Parallel()

	firstTLV, err := hex.DecodeString("010203e8")
	require.NoError(t, err)

	tests := []struct {
		name     string
		tlvType  tlv.Type
		expected string
	}{
		{
			name:    "type 1 nonce",
			tlvType: 1,
			expected: "255a95f5b6b3c6997e2838dc4d9348807fb6da" +
				"8eb7bbc02d30662d144718b6aa",
		},
		{
			name:    "type 2 nonce",
			tlvType: 2,
			expected: "12bc15565410d8e3251a6fb1c53a2d360f39a9" +
				"f65afb8403ef875016e34ff678",
		},
		{
			name:    "type 1001 nonce multi-byte bigsize",
			tlvType: 1001,
			expected: "793dc046489a1260fd133c5048591f6b59f192" +
				"8cbb7f9190219beeabc2b45f4d",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expected, err := hex.DecodeString(tc.expected)
			require.NoError(t, err)

			got := nonceHash("LnNonce"+string(firstTLV), tc.tlvType)
			require.Equal(t, expected, got[:])
		})
	}
}

// TestBranchHash verifies the branch hash computation.
func TestBranchHash(t *testing.T) {
	t.Parallel()

	const (
		// From test vector 2: combining the tlv1+nonce and
		// tlv2+nonce branches.
		aStr = "19d6ecfa3be88d29c30e56167f58526d7695df" +
			"ac9cb95e1256deb222c92db4d0"
		bStr = "b013756c8fee86503a0b4abdab4cddeb1af5d3" +
			"44ca6fc2fa8b6c08938caa6f93"
		expectedStr = "c3774abbf4815aa54ccaa026bff6581f01f3be" +
			"5fe814c620a252534f434bc0d1"
	)

	a, err := hex.DecodeString(aStr)
	require.NoError(t, err)
	b, err := hex.DecodeString(bStr)
	require.NoError(t, err)

	var aArr, bArr [32]byte
	copy(aArr[:], a)
	copy(bArr[:], b)

	expected, err := hex.DecodeString(expectedStr)
	require.NoError(t, err)

	got := branchHash(aArr, bArr)
	require.Equal(t, expected, got[:])
}

// TestMerkleVectorIntermediateHashes asserts every named LnLeaf, LnNonce, and
// LnBranch entry from each signature-test.json vector matches the hash this
// implementation produces. The root test alone cannot distinguish an encoding
// bug from a hash-construction bug. Feeding the primitives the spec-stated
// bytes directly localizes a vector failure to a single pipeline stage.
func TestMerkleVectorIntermediateHashes(t *testing.T) {
	t.Parallel()

	for _, tc := range loadSignatureVectors(t) {
		// The n1 vectors are synthesised. The pubkey-bearing
		// invoice_request leaves are recoverable from the bech32
		// string. In both cases the leaf hex appears in the JSON
		// `H('LnLeaf', <hex>)` keys, so we walk those directly.
		t.Run(tc.Comment, func(t *testing.T) {
			t.Parallel()

			firstTLV, err := hex.DecodeString(tc.FirstTLV)
			require.NoError(t, err)

			for i, leafJSON := range tc.Leaves {
				assertLeafEntry(t, leafJSON, firstTLV, i)
			}

			// Branch entries each carry exactly one
			// H('LnBranch', <hashA||hashB>) key.
			for i, branchJSON := range tc.Branches {
				assertBranchEntry(t, branchJSON, i)
			}
		})
	}
}

// assertLeafEntry checks the hashes a vector leaf records for a single TLV
// against the values this implementation derives from the leaf bytes and the
// stream's first TLV.
func assertLeafEntry(t *testing.T, leafJSON json.RawMessage, firstTLV []byte,
	idx int) {

	t.Helper()

	var entries map[string]string
	require.NoError(t, json.Unmarshal(leafJSON, &entries))

	const (
		leafPrefix   = "H(`LnLeaf`,"
		noncePrefix  = "H(`LnNonce`|first-tlv,"
		branchPrefix = "H(`LnBranch`,"
	)

	var (
		leafKey, leafExpected     string
		nonceKey, nonceExpected   string
		branchKey, branchExpected string
	)
	for k, v := range entries {
		switch {
		case len(k) > len(leafPrefix) &&
			k[:len(leafPrefix)] == leafPrefix:
			leafKey, leafExpected = k, v
		case len(k) > len(noncePrefix) &&
			k[:len(noncePrefix)] == noncePrefix:
			nonceKey, nonceExpected = k, v
		case len(k) > len(branchPrefix) &&
			k[:len(branchPrefix)] == branchPrefix:
			branchKey, branchExpected = k, v
		}
	}
	require.NotEmpty(t, leafKey,
		"leaf %d: missing LnLeaf key", idx)
	require.NotEmpty(t, nonceKey,
		"leaf %d: missing LnNonce key", idx)
	require.NotEmpty(t, branchKey,
		"leaf %d: missing LnBranch key", idx)

	leafHex := leafKey[len(leafPrefix) : len(leafKey)-1]
	leafBytes, err := hex.DecodeString(leafHex)
	require.NoError(t, err)

	gotLeaf := leafHash(leafBytes)
	wantLeaf, err := hex.DecodeString(leafExpected)
	require.NoError(t, err)
	require.Equal(
		t, wantLeaf, gotLeaf[:], "leaf %d: LnLeaf hash mismatch", idx,
	)

	// The nonce key encodes a per-leaf type identifier as the final segment
	// after the comma. For older vectors the segment is the type name
	// ("tlv1-type"). Newer ones use a raw type number ("1"). We extract the
	// leaf's leading TLV type from its hex prefix and use that. The spec
	// says the nonce binds to the first TLV plus the leaf's own type.
	leafType := leafTypeFromHex(t, leafBytes)
	gotNonce := nonceHash("LnNonce"+string(firstTLV), leafType)
	wantNonce, err := hex.DecodeString(nonceExpected)
	require.NoError(t, err)
	require.Equal(t, wantNonce, gotNonce[:],
		"leaf %d: LnNonce hash mismatch", idx)

	gotBranch := branchHash(gotLeaf, gotNonce)
	wantBranch, err := hex.DecodeString(branchExpected)
	require.NoError(t, err)
	require.Equal(t, wantBranch, gotBranch[:],
		"leaf %d: LnBranch hash mismatch", idx)
}

// assertBranchEntry validates the branch hash for one entry in the vector's
// `branches` array. Each entry's H('LnBranch', <hashA||hashB>) key carries the
// two child hashes concatenated. The value is the expected combined hash.
func assertBranchEntry(t *testing.T, branchJSON json.RawMessage, idx int) {
	t.Helper()

	var entries map[string]string
	require.NoError(t, json.Unmarshal(branchJSON, &entries))

	const branchPrefix = "H(`LnBranch`,"

	var key, expected string
	for k, v := range entries {
		if len(k) > len(branchPrefix) &&
			k[:len(branchPrefix)] == branchPrefix {

			key, expected = k, v
		}
	}
	require.NotEmpty(t, key, "branch %d: missing LnBranch key", idx)

	hexConcat := key[len(branchPrefix) : len(key)-1]
	concat, err := hex.DecodeString(hexConcat)
	require.NoError(t, err)
	require.Equal(
		t, 64, len(concat), "branch %d: expected 64 bytes of "+
			"child hashes", idx,
	)

	var a, b [32]byte
	copy(a[:], concat[:32])
	copy(b[:], concat[32:])

	got := branchHash(a, b)
	want, err := hex.DecodeString(expected)
	require.NoError(t, err)
	require.Equal(t, want, got[:], "branch %d: LnBranch hash mismatch", idx)
}

// leafTypeFromHex parses the leading varint of a TLV-encoded leaf to recover
// its type number. The signature-test.json LnNonce entries bind the nonce to
// this type, so we must reproduce the parse here to compute the same nonce
// hash.
func leafTypeFromHex(t *testing.T, leafBytes []byte) tlv.Type {
	t.Helper()

	var buf [8]byte
	r := bytes.NewReader(leafBytes)
	typ, err := tlv.ReadVarInt(r, &buf)
	require.NoError(t, err)

	return tlv.Type(typ)
}

// TestPropertyMerkleOrderSensitivity asserts that for any non-trivial raw TLV
// sequence the Merkle tree rejects a permuted input. The receiver-to-sender
// invoice flow signs a tree built over a type-sorted stream. If a permutation
// were accepted, an attacker could permute fields without invalidating the
// signature.
func TestPropertyMerkleOrderSensitivity(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		// Need at least two leaves with distinct types. Types are
		// tagged into the nonce hash, so identical types would produce
		// identical leaves and a swap would be a no-op.
		n := rapid.IntRange(2, 8).Draw(t, "leafCount")
		records := make([]tlv.Record, n)
		for i := range n {
			v := drawTLVValue(t)
			records[i] = tlv.MakePrimitiveRecord(tlv.Type(i+1), &v)
		}

		_, err := merkleRoot(records)
		require.NoError(t, err)

		swapped := make([]tlv.Record, len(records))
		copy(swapped, records)
		swapped[0], swapped[1] = swapped[1], swapped[0]

		_, err = merkleRoot(swapped)
		require.ErrorIs(t, err, ErrUnsortedMerkleInput,
			"swapping two distinct leaves did not reject the input")
	})
}

// drawTLVValue synthesises the value-side payload for a single TLV record. Used
// by the Merkle order-sensitivity property to build leaves that the hash
// functions can ingest.
func drawTLVValue(t *rapid.T) []byte {
	payloadLen := rapid.IntRange(1, 8).Draw(t, "payloadLen")

	return rapid.SliceOfN(rapid.Byte(), payloadLen, payloadLen).
		Draw(t, "payload")
}

// TestMerkleRootEmptyInput pins the contract for an empty leaf set: merkleRoot
// returns ErrEmptyMerkleInput, never the all-zero digest. The all-zero hash is
// a valid SHA-256 output that could collide with a legitimately computed root,
// so a verifier accepting it could be tricked by a forged-but-empty message.
func TestMerkleRootEmptyInput(t *testing.T) {
	t.Parallel()

	t.Run("nil slice", func(t *testing.T) {
		t.Parallel()

		root, err := merkleRoot(nil)
		require.ErrorIs(t, err, ErrEmptyMerkleInput)
		require.Equal(t, [32]byte{}, root)
	})

	t.Run("empty slice", func(t *testing.T) {
		t.Parallel()

		root, err := merkleRoot([]tlv.Record{})
		require.ErrorIs(t, err, ErrEmptyMerkleInput)
		require.Equal(t, [32]byte{}, root)
	})
}

// TestMerkleRootUnsortedInput pins the ordering precondition: merkleRoot
// returns ErrUnsortedMerkleInput for unsorted or duplicated input instead of
// producing an incorrect root silently.
func TestMerkleRootUnsortedInput(t *testing.T) {
	t.Parallel()

	newRecord := func(typ tlv.Type) tlv.Record {
		v := []byte{0x01}

		return tlv.MakePrimitiveRecord(typ, &v)
	}

	t.Run("unsorted", func(t *testing.T) {
		t.Parallel()

		_, err := merkleRoot([]tlv.Record{newRecord(2), newRecord(1)})
		require.ErrorIs(t, err, ErrUnsortedMerkleInput)
	})

	t.Run("duplicate", func(t *testing.T) {
		t.Parallel()

		_, err := merkleRoot([]tlv.Record{newRecord(1), newRecord(1)})
		require.ErrorIs(t, err, ErrUnsortedMerkleInput)
	})

	t.Run("sorted accepted", func(t *testing.T) {
		t.Parallel()

		_, err := merkleRoot([]tlv.Record{newRecord(1), newRecord(2)})
		require.NoError(t, err)
	})
}

// TestSignableTLVsFilteringBoundaries pins the inclusion rule for the Merkle
// input. The spec excludes types in [240, 1000]. Everything outside that range
// contributes. Drift here would either include type 240 (the signature itself,
// breaking commit-to-tree-root semantics) or exclude experimental types > 1000
// (silently dropping fields the writer expected to commit to).
func TestSignableTLVsFilteringBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		typ      tlv.Type
		included bool
	}{
		{typ: 0, included: true},
		{typ: 239, included: true},
		{typ: 240, included: false},
		{typ: 500, included: false},
		{typ: 1000, included: false},
		{typ: 1001, included: true},
		{typ: 1_000_000_000, included: true},
	}

	records := make([]tlv.Record, 0, len(tests))
	for _, tc := range tests {
		// An empty value blob is enough. The filter only inspects each
		// record's Type.
		var v []byte
		records = append(records, tlv.MakePrimitiveRecord(tc.typ, &v))
	}

	got := signableTLVs(records)
	gotTypes := make(map[tlv.Type]bool, len(got))
	for _, r := range got {
		gotTypes[r.Type()] = true
	}

	for _, tc := range tests {
		require.Equal(
			t, tc.included, gotTypes[tc.typ],
			"type %d inclusion mismatch", tc.typ,
		)
	}
}

package contractcourt

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func randResolverCtrlBlocks(t *testing.T) resolverCtrlBlocks {
	numBlocks := rand.Int() % 256
	blocks := make(resolverCtrlBlocks, numBlocks)

	for i := 0; i < numBlocks; i++ {
		var id resolverID
		_, err := rand.Read(id[:])
		require.NoError(t, err)

		var block [200]byte
		_, err = rand.Read(block[:])
		require.NoError(t, err)

		blocks[id] = block[:]
	}

	return blocks
}

func randHtlcTweaks(t *testing.T) htlcTapTweaks {
	numTweaks := rand.Int() % 256

	// If the numTweaks happens to be zero, we return a nil to avoid
	// initializing the map.
	if numTweaks == 0 {
		return nil
	}

	tweaks := make(htlcTapTweaks, numTweaks)
	for i := 0; i < numTweaks; i++ {
		var id resolverID
		_, err := rand.Read(id[:])
		require.NoError(t, err)

		var tweak [32]byte
		_, err = rand.Read(tweak[:])
		require.NoError(t, err)

		tweaks[id] = tweak
	}

	return tweaks
}

func randHtlcAuxBlobs(t *testing.T) htlcAuxBlobs {
	numBlobs := rand.Int() % 256
	blobs := make(htlcAuxBlobs, numBlobs)

	for i := 0; i < numBlobs; i++ {
		var id resolverID
		_, err := rand.Read(id[:])
		require.NoError(t, err)

		var blob [100]byte
		_, err = rand.Read(blob[:])
		require.NoError(t, err)

		blobs[id] = blob[:]
	}

	return blobs
}

// TestTaprootBriefcase tests the encode/decode methods of the taproot
// briefcase extension.
func TestTaprootBriefcase(t *testing.T) {
	t.Parallel()

	var sweepCtrlBlock [200]byte
	_, err := rand.Read(sweepCtrlBlock[:])
	require.NoError(t, err)

	var revokeCtrlBlock [200]byte
	_, err = rand.Read(revokeCtrlBlock[:])
	require.NoError(t, err)

	var anchorTweak [32]byte
	_, err = rand.Read(anchorTweak[:])
	require.NoError(t, err)

	var commitBlob [100]byte
	_, err = rand.Read(commitBlob[:])
	require.NoError(t, err)

	testCase := &taprootBriefcase{
		CtrlBlocks: tlv.NewRecordT[tlv.TlvType0](ctrlBlocks{
			CommitSweepCtrlBlock:   sweepCtrlBlock[:],
			RevokeSweepCtrlBlock:   revokeCtrlBlock[:],
			OutgoingHtlcCtrlBlocks: randResolverCtrlBlocks(t),
			IncomingHtlcCtrlBlocks: randResolverCtrlBlocks(t),
			SecondLevelCtrlBlocks:  randResolverCtrlBlocks(t),
		}),
		TapTweaks: tlv.NewRecordT[tlv.TlvType1](tapTweaks{
			AnchorTweak:                   anchorTweak[:],
			BreachedHtlcTweaks:            randHtlcTweaks(t),
			BreachedSecondLevelHltcTweaks: randHtlcTweaks(t),
		}),
		SettledCommitBlob: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType2](commitBlob[:]),
		),
		BreachedCommitBlob: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType3](commitBlob[:]),
		),
		HtlcBlobs: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType4](randHtlcAuxBlobs(t)),
		),
	}

	var b bytes.Buffer
	require.NoError(t, testCase.Encode(&b))

	var decodedCase taprootBriefcase
	require.NoError(t, decodedCase.Decode(&b))

	require.Equal(t, testCase, &decodedCase)
}

// testHtlcAuxBlobProperties is a rapid property that verifies the encoding and
// decoding of the HTLC aux blobs.
func testHtlcAuxBlobProperties(t *rapid.T) {
	htlcBlobs := rapid.Make[htlcAuxBlobs]().Draw(t, "htlcAuxBlobs")

	var b bytes.Buffer
	require.NoError(t, htlcBlobs.Encode(&b))

	decodedBlobs := newAuxHtlcBlobs()
	require.NoError(t, decodedBlobs.Decode(&b))

	require.Equal(t, htlcBlobs, decodedBlobs)
}

// TestHtlcAuxBlobEncodeDecode tests the encode/decode methods of the HTLC aux
// blobs.
func TestHtlcAuxBlobEncodeDecode(t *testing.T) {
	t.Parallel()

	rapid.Check(t, testHtlcAuxBlobProperties)
}

// FuzzHtlcAuxBlobEncodeDecodeFuzz tests the encode/decode methods of the HTLC
// aux blobs using the rapid derived fuzzer.
func FuzzHtlcAuxBlobEncodeDecode(f *testing.F) {
	f.Fuzz(rapid.MakeFuzz(testHtlcAuxBlobProperties))
}

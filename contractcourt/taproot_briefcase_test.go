package contractcourt

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
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

	testCase := &taprootBriefcase{
		CtrlBlocks: &ctrlBlocks{
			CommitSweepCtrlBlock:   sweepCtrlBlock[:],
			RevokeSweepCtrlBlock:   revokeCtrlBlock[:],
			OutgoingHtlcCtrlBlocks: randResolverCtrlBlocks(t),
			IncomingHtlcCtrlBlocks: randResolverCtrlBlocks(t),
			SecondLevelCtrlBlocks:  randResolverCtrlBlocks(t),
		},
		TapTweaks: &tapTweaks{
			AnchorTweak:                   anchorTweak[:],
			BreachedHtlcTweaks:            randHtlcTweaks(t),
			BreachedSecondLevelHltcTweaks: randHtlcTweaks(t),
		},
	}

	var b bytes.Buffer
	require.NoError(t, testCase.Encode(&b))

	var decodedCase taprootBriefcase
	require.NoError(t, decodedCase.Decode(&b))

	require.Equal(t, testCase, &decodedCase)
}

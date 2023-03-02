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

// TestTaprootBriefcase tests the encode/decode methods of the taproot
// briefcase extension.
func TestTaprootBriefcaseEncode(t *testing.T) {
	t.Parallel()

	var sweepCtrlBlock [200]byte
	_, err := rand.Read(sweepCtrlBlock[:])
	require.NoError(t, err)

	var anchorTweak [32]byte
	_, err = rand.Read(anchorTweak[:])
	require.NoError(t, err)

	testCase := &taprootBriefcase{
		CtrlBlocks: &ctrlBlocks{
			CommitSweepCtrlBlock:   sweepCtrlBlock[:],
			OutgoingHtlcCtrlBlocks: randResolverCtrlBlocks(t),
			IncomingHtlcCtrlBlocks: randResolverCtrlBlocks(t),
			SecondLevelCtrlBlocks:  randResolverCtrlBlocks(t),
		},
		TapTweaks: &tapTweaks{
			AnchorTweak: anchorTweak[:],
		},
	}

	var b bytes.Buffer
	require.NoError(t, testCase.Encode(&b))

	var decodedCase taprootBriefcase
	require.NoError(t, decodedCase.Decode(&b))

	require.Equal(t, testCase, &decodedCase)
}

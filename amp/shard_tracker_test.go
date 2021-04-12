package amp_test

import (
	"crypto/rand"
	"testing"

	"github.com/lightningnetwork/lnd/amp"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/shards"
	"github.com/stretchr/testify/require"
)

// TestAMPShardTracker tests that we can derive and cancel shards at will using
// the AMP shard tracker.
func TestAMPShardTracker(t *testing.T) {
	var root, setID, payAddr [32]byte
	_, err := rand.Read(root[:])
	require.NoError(t, err)

	_, err = rand.Read(setID[:])
	require.NoError(t, err)

	_, err = rand.Read(payAddr[:])
	require.NoError(t, err)

	var totalAmt lnwire.MilliSatoshi = 1000

	// Create an AMP shard tracker using the random data we just generated.
	tracker := amp.NewShardTracker(root, setID, payAddr, totalAmt)

	// Trying to retrieve a hash for id 0 should result in an error.
	_, err = tracker.GetHash(0)
	require.Error(t, err)

	// We start by creating 20 shards.
	const numShards = 20

	var shards []shards.PaymentShard
	for i := uint64(0); i < numShards; i++ {
		s, err := tracker.NewShard(i, i == numShards-1)
		require.NoError(t, err)

		// Check that the shards have their payloads set as expected.
		require.Equal(t, setID, s.AMP().SetID())
		require.Equal(t, totalAmt, s.MPP().TotalMsat())
		require.Equal(t, payAddr, s.MPP().PaymentAddr())

		shards = append(shards, s)
	}

	// Make sure we can retrieve the hash for all of them.
	for i := uint64(0); i < numShards; i++ {
		hash, err := tracker.GetHash(i)
		require.NoError(t, err)
		require.Equal(t, shards[i].Hash(), hash)
	}

	// Now cancel half of the shards.
	j := 0
	for i := uint64(0); i < numShards; i++ {
		if i%2 == 0 {
			err := tracker.CancelShard(i)
			require.NoError(t, err)
			continue
		}

		// Keep shard.
		shards[j] = shards[i]
		j++
	}
	shards = shards[:j]

	// Get a new last shard.
	s, err := tracker.NewShard(numShards, true)
	require.NoError(t, err)
	shards = append(shards, s)

	// Finally make sure these shards together can be used to reconstruct
	// the children.
	childDescs := make([]amp.ChildDesc, len(shards))
	for i, s := range shards {
		childDescs[i] = amp.ChildDesc{
			Share: s.AMP().RootShare(),
			Index: s.AMP().ChildIndex(),
		}
	}

	// Using the child descriptors, reconstruct the children.
	children := amp.ReconstructChildren(childDescs...)

	// Validate that the derived child preimages match the hash of each shard.
	for i, child := range children {
		require.Equal(t, shards[i].Hash(), child.Hash)
	}
}

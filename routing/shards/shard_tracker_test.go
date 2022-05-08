package shards_test

import (
	"crypto/rand"
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/shards"
	"github.com/stretchr/testify/require"
)

// TestSimpleShardTracker tests that the simple tracker that keeps a map from
// attemptID-> payment hash works.
func TestSimpleShardTracker(t *testing.T) {
	var testHashes [2]lntypes.Hash
	for i := range testHashes {
		_, err := rand.Read(testHashes[i][:])
		require.NoError(t, err)
	}

	startAttempts := map[uint64]lntypes.Hash{
		1: testHashes[1],
	}

	tracker := shards.NewSimpleShardTracker(testHashes[0], startAttempts)

	// Trying to retrieve a hash for id 0 should result in an error.
	_, err := tracker.GetHash(0)
	require.Error(t, err)

	// Getting id 1 should work.
	hash1, err := tracker.GetHash(1)
	require.NoError(t, err)

	require.Equal(t, testHashes[1], hash1)

	// Finally, create a new shard and immediately retrieve the hash.
	shard, err := tracker.NewShard(2, false)
	require.NoError(t, err)

	// It's hash should be the tracker's overall payment hash.
	hash2, err := tracker.GetHash(2)
	require.NoError(t, err)

	require.Equal(t, testHashes[0], shard.Hash())
	require.Equal(t, testHashes[0], hash2)
}

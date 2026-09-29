//go:build !test_db_sqlite && !test_db_postgres

package graphdb

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestPruneGraphNodesTakesCacheMutex asserts that PruneGraphNodes acquires the
// store's cache mutex before it opens its write transaction, just like every
// other mutator of the graph does.
//
// Beyond guarding the caches, that mutex is also the in-process serialization
// point against the batched channel edge insertion path. That path reads a
// node's row without writing it, so a node prune that ran concurrently with it
// could delete a node that the edge being added still references, leaving a
// dangling edge behind.
func TestPruneGraphNodesTakesCacheMutex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	backend, backendCleanup, err := kvdb.GetTestBackend(t.TempDir(), "cgr")
	require.NoError(t, err)
	t.Cleanup(backendCleanup)

	store, err := NewKVStore(backend)
	require.NoError(t, err)

	// The prune walks the graph starting from the source node, so we need
	// one to be set for the prune to succeed.
	sourceNode := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, store.SetSourceNode(ctx, sourceNode))

	// With the cache mutex held, a prune must not be able to make any
	// progress.
	store.cacheMu.Lock()

	pruneErr := make(chan error, 1)
	go func() {
		_, err := store.PruneGraphNodes(ctx)
		pruneErr <- err
	}()

	select {
	case <-pruneErr:
		t.Fatal("PruneGraphNodes did not wait for the cache mutex")

	case <-time.After(250 * time.Millisecond):
	}

	// Once we release the mutex, the prune should be able to run to
	// completion.
	store.cacheMu.Unlock()

	select {
	case err := <-pruneErr:
		require.NoError(t, err)

	case <-time.After(time.Minute):
		t.Fatal("PruneGraphNodes did not complete")
	}
}

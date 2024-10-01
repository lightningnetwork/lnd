package migration33

import (
	"bytes"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// resultsKey is the fixed key under which the attempt results are
	// stored.
	resultsKey = []byte("missioncontrol-results")

	// defaultMCNamespaceKey is the key of the default mission control store
	// namespace.
	defaultMCNamespaceKey = []byte("default")
)

// MigrateMCStoreNameSpacedResults reads in all the current mission control
// entries and re-writes them under a new default namespace.
func MigrateMCStoreNameSpacedResults(tx kvdb.RwTx) error {
	log.Infof("Migrating Mission Control store to use namespaced results")

	// Get the top level bucket. All the MC results are currently stored
	// as KV pairs in this bucket
	topLevelBucket := tx.ReadWriteBucket(resultsKey)

	// If the results bucket does not exist then there are no entries in
	// the mission control store yet and so there is nothing to migrate.
	if topLevelBucket == nil {
		return nil
	}

	// Create a new default namespace bucket under the top-level bucket.
	defaultNSBkt, err := topLevelBucket.CreateBucket(defaultMCNamespaceKey)
	if err != nil {
		return err
	}

	// Iterate through each of the existing result pairs, write them to the
	// new namespaced bucket. Also collect the set of keys so that we can
	// later delete them from the top level bucket.
	var keys [][]byte
	err = topLevelBucket.ForEach(func(k, v []byte) error {
		// Skip the new default namespace key.
		if bytes.Equal(k, defaultMCNamespaceKey) {
			return nil
		}

		// Collect the key.
		keys = append(keys, k)

		// Write the pair under the default namespace.
		return defaultNSBkt.Put(k, v)
	})
	if err != nil {
		return err
	}

	// Finally, iterate through the set of keys and delete them from the
	// top level bucket.
	for _, k := range keys {
		if err := topLevelBucket.Delete(k); err != nil {
			return err
		}
	}

	return err
}

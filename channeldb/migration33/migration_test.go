package migration33

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// before represents the structure of the MC store before the migration.
	before = map[string]interface{}{
		"key1": "result1",
		"key2": "result2",
		"key3": "result3",
		"key4": "result4",
	}

	// after represents the expected structure of the store after the
	// migration. It should be identical to before except all the kv pairs
	// are now under a new default namespace key.
	after = map[string]interface{}{
		string(defaultMCNamespaceKey): before,
	}
)

// TestMigrateMCStoreNameSpacedResults tests that the MC store results are
// correctly moved to be under a new default namespace bucket.
func TestMigrateMCStoreNameSpacedResults(t *testing.T) {
	before := func(tx kvdb.RwTx) error {
		return migtest.RestoreDB(tx, resultsKey, before)
	}

	after := func(tx kvdb.RwTx) error {
		return migtest.VerifyDB(tx, resultsKey, after)
	}

	migtest.ApplyMigration(
		t, before, after, MigrateMCStoreNameSpacedResults, false,
	)
}

package migration31

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

var (
	hexStr = migtest.Hex

	// lastTxBefore is the "sweeper-last-tx" bucket before the migration.
	// We fill the last-tx value with a dummy hex string because the actual
	// value is not important when deleting the bucket.
	lastTxBefore = map[string]interface{}{
		"sweeper-last-tx": hexStr("0000"),
	}
)

// TestDeleteLastPublishTxTLP asserts that the sweeper-last-tx bucket is
// properly deleted.
func TestDeleteLastPublishTxTLP(t *testing.T) {
	t.Parallel()

	// Prime the database with the populated sweeper-last-tx bucket.
	before := func(tx kvdb.RwTx) error {
		return migtest.RestoreDB(tx, lastTxBucketKey, lastTxBefore)
	}

	// After the migration, ensure that the sweeper-last-tx bucket was
	// properly deleted.
	after := func(tx kvdb.RwTx) error {
		err := migtest.VerifyDB(tx, lastTxBucketKey, nil)
		require.ErrorContains(
			t, err,
			fmt.Sprintf("bucket %s not found", lastTxBucketKey),
		)

		return nil
	}

	migtest.ApplyMigration(
		t, before, after, DeleteLastPublishedTxTLB, false,
	)
}

package migration_test

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/migration"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
)

// TestCreateTLB asserts that a CreateTLB properly initializes a new top-level
// bucket, and that it succeeds even if the bucket already exists. It would
// probably be better if the latter failed, but the kvdb abstraction doesn't
// support this.
func TestCreateTLB(t *testing.T) {
	newBucket := []byte("hello")

	tests := []struct {
		name            string
		beforeMigration func(kvdb.RwTx) error
		shouldFail      bool
	}{
		{
			name: "already exists",
			beforeMigration: func(tx kvdb.RwTx) error {
				_, err := tx.CreateTopLevelBucket(newBucket)
				return err
			},
			shouldFail: true,
		},
		{
			name:            "does not exist",
			beforeMigration: func(_ kvdb.RwTx) error { return nil },
			shouldFail:      false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			migtest.ApplyMigration(
				t,
				test.beforeMigration,
				func(tx kvdb.RwTx) error {
					if tx.ReadBucket(newBucket) != nil {
						return nil
					}
					return fmt.Errorf("bucket \"%s\" not "+
						"created", newBucket)
				},
				migration.CreateTLB(newBucket),
				test.shouldFail,
			)
		})
	}
}

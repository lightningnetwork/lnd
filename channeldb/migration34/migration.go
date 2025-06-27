package migration34

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
)

// Migration34 is an optional migration that garbage collects the decayed log
// in particular the `batch-replay` bucket. However we did choose to use an
// optional migration which defaults to true because the decayed log db is
// separate from the channeldb and if we would have implemented it as a
// required migration, then it would have required a bigger change to the
// codebase.
//
// Most of the decayed log db will shrink significantly after this migration
// because the other bucket called `shared-secrets` is garbage collected
// continuously and the `batch-replay` bucket will be deleted.

var (
	// batchReplayBucket is a bucket that maps batch identifiers to
	// serialized ReplaySets. This is used to give idempotency in the event
	// that a batch is processed more than once.
	batchReplayBucket = []byte("batch-replay")
)

// MigrationConfig is the interface for the migration configuration.
type MigrationConfig interface {
	GetDecayedLog() kvdb.Backend
}

// MigrationConfigImpl is the implementation of the migration configuration.
type MigrationConfigImpl struct {
	DecayedLog kvdb.Backend
}

// GetDecayedLog returns the decayed log backend.
func (c *MigrationConfigImpl) GetDecayedLog() kvdb.Backend {
	return c.DecayedLog
}

// MigrateDecayedLog migrates the decayed log. The migration deletes the
// `batch-replay` bucket, which is no longer used.
//
// NOTE: This migration is idempotent. If the bucket does not exist, then this
// migration is a no-op.
func MigrateDecayedLog(db kvdb.Backend, cfg MigrationConfig) error {
	decayedLog := cfg.GetDecayedLog()

	// Make sure we have a reference to the decayed log.
	if decayedLog == nil {
		return fmt.Errorf("decayed log backend is not available")
	}

	log.Info("Migrating decayed log...")
	err := decayedLog.Update(func(tx kvdb.RwTx) error {
		err := tx.DeleteTopLevelBucket(batchReplayBucket)
		if err != nil && !errors.Is(err, kvdb.ErrBucketNotFound) {
			return fmt.Errorf("deleting top level bucket %s: %w",
				batchReplayBucket, err)
		}

		log.Debugf("top level bucket %s deleted", batchReplayBucket)

		return nil
	}, func() {})

	if err != nil {
		return fmt.Errorf("failed to migrate decayed log: %w", err)
	}

	log.Info("Decayed log migrated successfully")

	return nil
}

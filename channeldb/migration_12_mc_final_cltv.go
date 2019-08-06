package channeldb

import "github.com/coreos/bbolt"

// migrateMcFinalCltv clears mission control to start with a clean slate on
// which final cltv delta values are stored too. Because mission control data is
// not critical we opt for simply clearing the existing data rather than a
// read/update/write migration.
func migrateMcFinalCltv(tx *bbolt.Tx) error {
	log.Infof("Migrating to new mission control store by clearing " +
		"existing data")

	resultsKey := []byte("missioncontrol-results")
	if err := tx.DeleteBucket(resultsKey); err != nil {
		return err
	}

	log.Infof("Migration to new mission control completed!")
	return nil
}

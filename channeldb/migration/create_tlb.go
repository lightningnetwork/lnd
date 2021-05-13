package migration

import (
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
)

// CreateTLB creates a new top-level bucket with the passed bucket identifier.
func CreateTLB(bucket []byte) func(kvdb.RwTx) error {
	return func(tx kvdb.RwTx) error {
		log.Infof("Creating top-level bucket: \"%s\" ...", bucket)

		if tx.ReadBucket(bucket) != nil {
			return fmt.Errorf("top-level bucket \"%s\" "+
				"already exists", bucket)
		}

		_, err := tx.CreateTopLevelBucket(bucket)
		if err != nil {
			return err
		}

		log.Infof("Created top-level bucket: \"%s\"", bucket)
		return nil
	}
}

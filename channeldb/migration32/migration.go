package migration32

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
)

// MigrateMCRouteSerialisation reads all the mission control store entries and
// re-serializes them using a minimal route serialisation so that only the parts
// of the route that are actually required for mission control are persisted.
func MigrateMCRouteSerialisation(tx kvdb.RwTx) error {
	log.Infof("Migrating Mission Control store to use a more minimal " +
		"encoding for routes")

	resultBucket := tx.ReadWriteBucket(resultsKey)

	// If the results bucket does not exist then there are no entries in
	// the mission control store yet and so there is nothing to migrate.
	if resultBucket == nil {
		return nil
	}

	// For each entry, read it into memory using the old encoding. Then,
	// extract the more minimal route, re-encode and persist the entry.
	return resultBucket.ForEach(func(k, v []byte) error {
		// Read the entry using the old encoding.
		resultOld, err := deserializeOldResult(k, v)
		if err != nil {
			return err
		}

		// Convert to the new payment result format with the minimal
		// route.
		resultNew := convertPaymentResult(resultOld)

		// Serialise the new payment result using the new encoding.
		key, resultNewBytes, err := serializeNewResult(resultNew)
		if err != nil {
			return err
		}

		// Make sure that the derived key is the same.
		if !bytes.Equal(key, k) {
			return fmt.Errorf("new payment result key (%v) is "+
				"not the same as the old key (%v)", key, k)
		}

		// Finally, overwrite the previous value with the new encoding.
		return resultBucket.Put(k, resultNewBytes)
	})
}

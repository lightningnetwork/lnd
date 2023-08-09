package migration31

import (
	"errors"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
)

// DeleteLastPublishedTxTLB deletes the top level bucket with the key
// "sweeper-last-tx".
func DeleteLastPublishedTxTLB(tx kvdb.RwTx) error {
	log.Infof("Deleting top-level bucket: %x ...", lastTxBucketKey)

	err := tx.DeleteTopLevelBucket(lastTxBucketKey)
	if err != nil && !errors.Is(err, walletdb.ErrBucketNotFound) {
		return err
	}

	log.Infof("Deleted top-level bucket: %x", lastTxBucketKey)

	return nil
}

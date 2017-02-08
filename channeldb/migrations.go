package channeldb

import (
	"bytes"

	"github.com/boltdb/bolt"
	"github.com/roasbeef/btcd/wire"
)

// deliveryScriptBugMigration is a database migration that patches an incorrect
// version of the database due to a typo in the key when fetching, putting,
// deleting the delivery scripts for a channel. As of database version 1, the
// querying logic expects delivery scripts to be in the proper place within the
// node's channel schema. This migration fixes the issue in the older version
// of the database so channels can properly be read from the database.
func deliveryScriptBugMigration(tx *bolt.Tx) error {
	// Get the bucket dedicated to storing the metadata for open channels.
	openChanBucket := tx.Bucket(openChannelBucket)
	if openChanBucket == nil {
		return nil
	}

	// Next, fetch the bucket dedicated to storing metadata related to all
	// nodes. All keys within this bucket are the serialized public keys of
	// all our direct counterparties.
	nodeMetaBucket := tx.Bucket(nodeInfoBucket)
	if nodeMetaBucket == nil {
		return nil
	}

	log.Infof("Migration database from legacy channel delivery script " +
		"schema")

	// Finally for each node public key in the bucket, fetch all the
	// channels related to this particular node.
	return nodeMetaBucket.ForEach(func(k, v []byte) error {
		// Within the meta node meta bucket, each key is the node's
		// serialized public key. Knowing this key allows us to fetch
		// the node's open channel bucket.
		nodeChanBucket := openChanBucket.Bucket(k)
		if nodeChanBucket == nil {
			return nil
		}

		// Once we have the open channel bucket for this particular
		// node, we then access another sub-bucket which is an index
		// that stores the channel points (chanID's) for all active
		// channels with the node.
		nodeChanIDBucket := nodeChanBucket.Bucket(chanIDBucket[:])
		if nodeChanIDBucket == nil {
			return nil
		}
		err := nodeChanIDBucket.ForEach(func(k, v []byte) error {
			if k == nil {
				return nil
			}

			// The old delivery script key within a node's channel
			// bucket for each channel was just the prefix key
			// itself. So we'll check if this key stores any data,
			// if not, then we don't need to migrate this channel.
			oldDeliverykey := deliveryScriptsKey
			deliveryScripts := nodeChanBucket.Get(oldDeliverykey)
			if deliveryScripts == nil {
				return nil
			}

			// Decode the stored outpoint so we can log our
			// progress.
			outBytes := bytes.NewReader(k)
			chanID := &wire.OutPoint{}
			if err := readOutpoint(outBytes, chanID); err != nil {
				return err
			}

			log.Debugf("Migration delivery scripts of "+
				"ChannelPoint(%v)", chanID)

			// Next we manually construct the _proper_ key which
			// uses the key prefix in conjunction with the chanID
			// to create the final key.
			deliveryKey := make([]byte, len(deliveryScriptsKey)+len(k))
			copy(deliveryKey[:3], deliveryScriptsKey)
			copy(deliveryKey[3:], k)

			// To complete the migration for this channel, we now
			// store the delivery scripts in their proper place.
			return nodeChanBucket.Put(deliveryKey, deliveryScripts)
		})
		if err != nil {
			return err
		}

		// Before we conclude, we'll also delete value of the incorrect
		// delivery storage.
		return nodeChanBucket.Delete(deliveryScriptsKey)
	})
}

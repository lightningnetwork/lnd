package migration26

import (
	"fmt"

	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// openChanBucket stores all the currently open channels. This bucket
	// has a second, nested bucket which is keyed by a node's ID. Within
	// that node ID bucket, all attributes required to track, update, and
	// close a channel are stored.
	openChannelBucket = []byte("open-chan-bucket")

	// ErrNoChanDBExists is returned when a channel bucket hasn't been
	// created.
	ErrNoChanDBExists = fmt.Errorf("channel db has not yet been created")

	// ErrNoActiveChannels  is returned when there is no active (open)
	// channels within the database.
	ErrNoActiveChannels = fmt.Errorf("no active channels exist")

	// ErrChannelNotFound is returned when we attempt to locate a channel
	// for a specific chain, but it is not found.
	ErrChannelNotFound = fmt.Errorf("channel not found")
)

// MigrateBalancesToTlvRecords migrates the balance fields into tlv records. It
// does so by first reading a list of open channels, then rewriting the channel
// info with the updated tlv stream.
func MigrateBalancesToTlvRecords(tx kvdb.RwTx) error {
	log.Infof("Migrating local and remote balances into tlv records...")

	openChanBucket := tx.ReadWriteBucket(openChannelBucket)

	// If no bucket is found, we can exit early.
	if openChanBucket == nil {
		return nil
	}

	// Read a list of open channels.
	channels, err := findOpenChannels(openChanBucket)
	if err != nil {
		return err
	}

	// Migrate the balances.
	for _, c := range channels {
		if err := migrateBalances(tx, c); err != nil {
			return err
		}
	}

	return err
}

// findOpenChannels finds all open channels.
func findOpenChannels(openChanBucket kvdb.RBucket) ([]*OpenChannel, error) {
	channels := []*OpenChannel{}

	// readChannel is a helper closure that reads the channel info from the
	// channel bucket.
	readChannel := func(chainBucket kvdb.RBucket, cp []byte) error {
		c := &OpenChannel{}

		// Read the sub-bucket level 3.
		chanBucket := chainBucket.NestedReadBucket(
			cp,
		)
		if chanBucket == nil {
			log.Errorf("unable to read bucket for chanPoint=%x", cp)
			return nil
		}

		// Get the old channel info.
		if err := FetchChanInfo(chanBucket, c, true); err != nil {
			return fmt.Errorf("unable to fetch chan info: %w", err)
		}

		channels = append(channels, c)

		return nil
	}

	// Iterate the root bucket.
	err := openChanBucket.ForEach(func(nodePub, v []byte) error {
		// Ensure that this is a key the same size as a pubkey, and
		// also that it leads directly to a bucket.
		if len(nodePub) != 33 || v != nil {
			return nil
		}

		// Read the sub-bucket level 1.
		nodeChanBucket := openChanBucket.NestedReadBucket(nodePub)
		if nodeChanBucket == nil {
			log.Errorf("no bucket for node %x", nodePub)
			return nil
		}

		// Iterate the bucket.
		return nodeChanBucket.ForEach(func(chainHash, _ []byte) error {
			// Read the sub-bucket level 2.
			chainBucket := nodeChanBucket.NestedReadBucket(
				chainHash,
			)
			if chainBucket == nil {
				log.Errorf("unable to read bucket for chain=%x",
					chainHash)
				return nil
			}

			// Iterate the bucket.
			return chainBucket.ForEach(func(cp, _ []byte) error {
				return readChannel(chainBucket, cp)
			})
		})
	})

	if err != nil {
		return nil, err
	}

	return channels, nil
}

// migrateBalances creates a new tlv stream which adds two more records to hold
// the balances info.
func migrateBalances(tx kvdb.RwTx, c *OpenChannel) error {
	// Get the bucket.
	chanBucket, err := mig25.FetchChanBucket(tx, &c.OpenChannel)
	if err != nil {
		return err
	}

	// Update the channel info. There isn't much to do here as the
	// `PutChanInfo` will read the values from `c.InitialLocalBalance` and
	// `c.InitialRemoteBalance` then create the new tlv stream as
	// requested.
	if err := PutChanInfo(chanBucket, c, false); err != nil {
		return fmt.Errorf("unable to put chan info: %w", err)
	}

	return nil
}

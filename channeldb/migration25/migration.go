package migration25

import (
	"bytes"
	"fmt"

	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// openChanBucket stores all the currently open channels. This bucket
	// has a second, nested bucket which is keyed by a node's ID. Within
	// that node ID bucket, all attributes required to track, update, and
	// close a channel are stored.
	openChannelBucket = []byte("open-chan-bucket")

	// chanInfoKey can be accessed within the bucket for a channel
	// (identified by its chanPoint). This key stores all the static
	// information for a channel which is decided at the end of  the
	// funding flow.
	chanInfoKey = []byte("chan-info-key")

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

// MigrateInitialBalances patches the two new fields, InitialLocalBalance and
// InitialRemoteBalance, for all the open channels. It does so by reading the
// revocation log at height 0 to learn the initial balances and then updates
// the channel's info.
// The channel info is saved in the nested bucket which is accessible via
// nodePub:chainHash:chanPoint. If any of the sub-buckets turns out to be nil,
// we will log the error and continue to process the rest.
func MigrateInitialBalances(tx kvdb.RwTx) error {
	log.Infof("Migrating initial local and remote balances...")

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
		if err := fetchChanInfo(chanBucket, c, true); err != nil {
			return fmt.Errorf("unable to fetch chan info: %w", err)
		}

		// Fetch the channel commitments, which are useful for freshly
		// open channels as they don't have any revocation logs and
		// their current commitments reflect the initial balances.
		if err := FetchChanCommitments(chanBucket, c); err != nil {
			return fmt.Errorf("unable to fetch chan commits: %w",
				err)
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

// migrateBalances queries the revocation log at height 0 to find the initial
// balances and save them to the channel info.
func migrateBalances(tx kvdb.RwTx, c *OpenChannel) error {
	// Get the bucket.
	chanBucket, err := FetchChanBucket(tx, c)
	if err != nil {
		return err
	}
	// Get the initial balances.
	localAmt, remoteAmt, err := c.balancesAtHeight(chanBucket, 0)
	if err != nil {
		return fmt.Errorf("unable to get initial balances: %w", err)
	}

	c.InitialLocalBalance = localAmt
	c.InitialRemoteBalance = remoteAmt

	// Update the channel info.
	if err := putChanInfo(chanBucket, c, false); err != nil {
		return fmt.Errorf("unable to put chan info: %w", err)
	}

	return nil
}

// FetchChanBucket is a helper function that returns the bucket where a
// channel's data resides in given: the public key for the node, the outpoint,
// and the chainhash that the channel resides on.
func FetchChanBucket(tx kvdb.RwTx, c *OpenChannel) (kvdb.RwBucket, error) {
	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	openChanBucket := tx.ReadWriteBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, ErrNoChanDBExists
	}

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := c.IdentityPub.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadWriteBucket(nodePub)
	if nodeChanBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket := nodeChanBucket.NestedReadWriteBucket(c.ChainHash[:])
	if chainBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for this channel itself.
	var chanPointBuf bytes.Buffer
	err := mig.WriteOutpoint(&chanPointBuf, &c.FundingOutpoint)
	if err != nil {
		return nil, err
	}

	chanBucket := chainBucket.NestedReadWriteBucket(chanPointBuf.Bytes())
	if chanBucket == nil {
		return nil, ErrChannelNotFound
	}

	return chanBucket, nil
}

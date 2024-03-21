package migration27

import (
	"bytes"
	"fmt"

	mig26 "github.com/lightningnetwork/lnd/channeldb/migration26"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// historicalChannelBucket stores all channels that have seen their
	// commitment tx confirm. All information from their previous open state
	// is retained.
	historicalChannelBucket = []byte("historical-chan-bucket")
)

// MigrateHistoricalBalances patches the two new fields, `InitialLocalBalance`
// and `InitialRemoteBalance`, for all the open channels saved in historical
// channel bucket. Unlike migration 25, it will only read the old channel info
// first and then patch the new tlv records with empty values. For historical
// channels, we previously didn't save the initial balances anywhere and since
// it's corresponding open channel bucket is deleted after closure, we have
// lost that balance info.
func MigrateHistoricalBalances(tx kvdb.RwTx) error {
	log.Infof("Migrating historical local and remote balances...")

	// First fetch the top level bucket which stores all data related to
	// historically stored channels.
	rootBucket := tx.ReadWriteBucket(historicalChannelBucket)

	// If no bucket is found, we can exit early.
	if rootBucket == nil {
		return nil
	}

	// Read a list of historical channels.
	channels, err := findHistoricalChannels(rootBucket)
	if err != nil {
		return err
	}

	// Migrate the balances.
	for _, c := range channels {
		if err := migrateBalances(rootBucket, c); err != nil {
			return err
		}
	}

	return err
}

// findHistoricalChannels finds all historical channels.
func findHistoricalChannels(historicalBucket kvdb.RBucket) ([]*OpenChannel,
	error) {

	channels := []*OpenChannel{}

	// readChannel is a helper closure that reads the channel info from the
	// historical sub-bucket.
	readChannel := func(rootBucket kvdb.RBucket, cp []byte) error {
		c := &OpenChannel{}

		chanPointBuf := bytes.NewBuffer(cp)
		err := mig.ReadOutpoint(chanPointBuf, &c.FundingOutpoint)
		if err != nil {
			return fmt.Errorf("read funding outpoint got: %w", err)
		}

		// Read the sub-bucket.
		chanBucket := rootBucket.NestedReadBucket(cp)
		if chanBucket == nil {
			log.Errorf("unable to read bucket for chanPoint=%s",
				c.FundingOutpoint)
			return nil
		}

		// Try to fetch channel info in old format.
		err = fetchChanInfoCompatible(chanBucket, c, true)
		if err != nil {
			return fmt.Errorf("%s: fetch chan info got: %w",
				c.FundingOutpoint, err)
		}

		channels = append(channels, c)

		return nil
	}

	// Iterate the root bucket.
	err := historicalBucket.ForEach(func(cp, _ []byte) error {
		return readChannel(historicalBucket, cp)
	})

	if err != nil {
		return nil, err
	}

	return channels, nil
}

// fetchChanInfoCompatible tries to fetch the channel info for a historical
// channel. It will first fetch the info assuming `InitialLocalBalance` and
// `InitialRemoteBalance` are not serialized. Upon receiving an error, it will
// then fetch it again assuming the two fields are present in db.
func fetchChanInfoCompatible(chanBucket kvdb.RBucket, c *OpenChannel,
	legacy bool) error {

	// Try to fetch the channel info assuming the historical channel in in
	// the old format, where the two fields, `InitialLocalBalance` and
	// `InitialRemoteBalance` are not saved to db.
	err := FetchChanInfo(chanBucket, c, legacy)
	if err == nil {
		return err
	}

	// If we got an error above, the historical channel may already have
	// the new fields saved. This could happen when a channel is closed
	// after applying migration 25. In this case, we'll borrow the
	// `FetchChanInfo` info method from migration 26 where we assume the
	// two fields are saved.
	return mig26.FetchChanInfo(chanBucket, &c.OpenChannel, legacy)
}

// migrateBalances serializes the channel info using the new tlv format where
// the two fields, `InitialLocalBalance` and `InitialRemoteBalance` are patched
// with empty values.
func migrateBalances(rootBucket kvdb.RwBucket, c *OpenChannel) error {
	var chanPointBuf bytes.Buffer
	err := mig.WriteOutpoint(&chanPointBuf, &c.FundingOutpoint)
	if err != nil {
		return err
	}

	// Get the channel bucket.
	chanBucket := rootBucket.NestedReadWriteBucket(chanPointBuf.Bytes())
	if chanBucket == nil {
		return fmt.Errorf("empty historical chan bucket")
	}

	// Update the channel info.
	if err := PutChanInfo(chanBucket, c, false); err != nil {
		return fmt.Errorf("unable to put chan info: %w", err)
	}

	return nil
}

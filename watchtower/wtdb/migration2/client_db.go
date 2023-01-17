package migration2

import (
	"errors"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// cChanSummaryBkt is a top-level bucket storing:
	//   channel-id -> encoded ClientChanSummary.
	cChanSummaryBkt = []byte("client-channel-summary-bucket")

	// cChanDetailsBkt is a top-level bucket storing:
	//   channel-id => cChannelSummary -> encoded ClientChanSummary.
	cChanDetailsBkt = []byte("client-channel-detail-bucket")

	// cChannelSummary is a key used in cChanDetailsBkt to store the encoded
	// body of ClientChanSummary.
	cChannelSummary = []byte("client-channel-summary")

	// ErrUninitializedDB signals that top-level buckets for the database
	// have not been initialized.
	ErrUninitializedDB = errors.New("db not initialized")

	// ErrCorruptChanSummary signals that the clients channel summary's
	// on-disk structure deviates from what is expected.
	ErrCorruptChanSummary = errors.New("channel summary corrupted")
)

// MigrateClientChannelDetails creates a new channel-details bucket that uses
// channel IDs as sub-buckets where the channel summaries are moved to from the
// channel summary bucket. If the migration is successful then the channel
// summary bucket is deleted.
func MigrateClientChannelDetails(tx kvdb.RwTx) error {
	log.Infof("Migrating the tower client db to move the channel " +
		"summaries to the new channel-details bucket")

	// Create the new top level cChanDetailsBkt.
	chanDetailsBkt, err := tx.CreateTopLevelBucket(cChanDetailsBkt)
	if err != nil {
		return err
	}

	// Get the top-level channel summaries bucket.
	chanSummaryBkt := tx.ReadWriteBucket(cChanSummaryBkt)
	if chanSummaryBkt == nil {
		return ErrUninitializedDB
	}

	// Iterate over the cChanSummaryBkt's keys. Each key is a channel-id.
	// For each of these, create a new sub-bucket with this key in
	// cChanDetailsBkt. In this sub-bucket, add the cChannelSummary key with
	// the encoded ClientChanSummary as the value.
	err = chanSummaryBkt.ForEach(func(chanID, summary []byte) error {
		// Force the migration to fail if the summary is empty. This
		// should never be the case, but it is added so that we can
		// force the migration to fail in a test so that we can test
		// that the db remains unaffected if a migration failure takes
		// place.
		if len(summary) == 0 {
			return ErrCorruptChanSummary
		}

		// Create a new sub-bucket in the channel details bucket using
		// this channel ID.
		channelBkt, err := chanDetailsBkt.CreateBucket(chanID)
		if err != nil {
			return err
		}

		// Add the encoded channel summary in the new bucket under the
		// channel-summary key.
		return channelBkt.Put(cChannelSummary, summary)
	})
	if err != nil {
		return err
	}

	// Now delete the cChanSummaryBkt from the DB.
	return tx.DeleteTopLevelBucket(cChanSummaryBkt)
}

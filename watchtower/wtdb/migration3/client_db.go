package migration3

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// cChanDetailsBkt is a top-level bucket storing:
	//   channel-id => cChannelSummary -> encoded ClientChanSummary.
	// 		=> cChanDBID -> db-assigned-id
	cChanDetailsBkt = []byte("client-channel-detail-bucket")

	// cChanDBID is a key used in the cChanDetailsBkt to store the
	// db-assigned-id of a channel.
	cChanDBID = []byte("client-channel-db-id")

	// cChanIDIndexBkt is a top-level bucket storing:
	//    db-assigned-id -> channel-ID
	cChanIDIndexBkt = []byte("client-channel-id-index")

	// cChannelSummary is a key used in cChanDetailsBkt to store the encoded
	// body of ClientChanSummary.
	cChannelSummary = []byte("client-channel-summary")

	// ErrUninitializedDB signals that top-level buckets for the database
	// have not been initialized.
	ErrUninitializedDB = errors.New("db not initialized")

	// ErrCorruptChanDetails signals that the clients channel detail's
	// on-disk structure deviates from what is expected.
	ErrCorruptChanDetails = errors.New("channel details corrupted")

	byteOrder = binary.BigEndian
)

// MigrateChannelIDIndex adds a new channel ID index to the tower client db.
// This index is a mapping from db-assigned ID (a uint64 encoded using BigSize
// encoding) to real channel ID (32 bytes). This mapping will allow us to
// persist channel pointers with fewer bytes in the future.
func MigrateChannelIDIndex(tx kvdb.RwTx) error {
	log.Infof("Migrating the tower client db to add a new channel ID " +
		"index which stores a mapping from db-assigned ID to real " +
		"channel ID")

	// Create a new top-level bucket for the new index.
	indexBkt, err := tx.CreateTopLevelBucket(cChanIDIndexBkt)
	if err != nil {
		return err
	}

	// Get the top-level channel-details bucket. The keys of this bucket
	// are the real channel IDs.
	chanDetailsBkt := tx.ReadWriteBucket(cChanDetailsBkt)
	if chanDetailsBkt == nil {
		return ErrUninitializedDB
	}

	// Iterate over the keys of the channel-details bucket.
	return chanDetailsBkt.ForEach(func(chanID, _ []byte) error {
		// Ask the db for a new, unique, ID for the index bucket.
		nextSeq, err := indexBkt.NextSequence()
		if err != nil {
			return err
		}

		// Encode the sequence number using BigSize encoding.
		var newIndex bytes.Buffer
		err = tlv.WriteVarInt(&newIndex, nextSeq, &[8]byte{})
		if err != nil {
			return err
		}

		// Add the mapping from the db-assigned ID to the channel ID
		// to the new index.
		newIndexBytes := newIndex.Bytes()
		err = indexBkt.Put(newIndexBytes, chanID)
		if err != nil {
			return err
		}

		chanDetails := chanDetailsBkt.NestedReadWriteBucket(chanID)
		if chanDetails == nil {
			return ErrCorruptChanDetails
		}

		// Here we ensure that the channel-details bucket includes a
		// channel summary. The only reason we do this is so that we can
		// simulate a migration fail in a test to ensure that a
		// migration fail results in an untouched db.
		chanSummaryBytes := chanDetails.Get(cChannelSummary)
		if chanSummaryBytes == nil {
			return ErrCorruptChanDetails
		}

		// In the channel-details sub-bucket for this channel, add the
		// new DB-assigned ID for this channel under the cChanDBID key.
		return chanDetails.Put(cChanDBID, newIndexBytes)
	})
}

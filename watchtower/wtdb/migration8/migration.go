package migration8

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// cSessionBkt is a top-level bucket storing:
	//   session-id => cSessionBody -> encoded ClientSessionBody
	// 		=> cSessionDBID -> db-assigned-id
	//              => cSessionCommits => seqnum -> encoded CommittedUpdate
	//              => cSessionAckRangeIndex => db-chan-id => start -> end
	// 		=> cSessionRogueUpdateCount -> count
	cSessionBkt = []byte("client-session-bucket")

	// cChanIDIndexBkt is a top-level bucket storing:
	//    db-assigned-id -> channel-ID
	cChanIDIndexBkt = []byte("client-channel-id-index")

	// cSessionAckRangeIndex is a sub-bucket of cSessionBkt storing
	//    chan-id => start -> end
	cSessionAckRangeIndex = []byte("client-session-ack-range-index")

	// cSessionBody is a sub-bucket of cSessionBkt storing:
	//    seqnum -> encoded CommittedUpdate.
	cSessionCommits = []byte("client-session-commits")

	// cChanDetailsBkt is a top-level bucket storing:
	//   channel-id => cChannelSummary -> encoded ClientChanSummary.
	//  		=> cChanDBID -> db-assigned-id
	// 		=> cChanSessions => db-session-id -> 1
	// 		=> cChanClosedHeight -> block-height
	// 		=> cChanMaxCommitmentHeight -> commitment-height
	cChanDetailsBkt = []byte("client-channel-detail-bucket")

	cChanMaxCommitmentHeight = []byte(
		"client-channel-max-commitment-height",
	)

	// ErrUninitializedDB signals that top-level buckets for the database
	// have not been initialized.
	ErrUninitializedDB = errors.New("db not initialized")

	byteOrder = binary.BigEndian
)

// MigrateChannelMaxHeights migrates the tower client db by collecting all the
// max commitment heights that have been backed up for each channel and then
// storing those heights alongside the channel info.
func MigrateChannelMaxHeights(tx kvdb.RwTx) error {
	log.Infof("Migrating the tower client DB for quick channel max " +
		"commitment height lookup")

	heights, err := collectChanMaxHeights(tx)
	if err != nil {
		return err
	}

	return writeChanMaxHeights(tx, heights)
}

// writeChanMaxHeights iterates over the given channel ID to height map and
// writes an entry under the cChanMaxCommitmentHeight key for each channel.
func writeChanMaxHeights(tx kvdb.RwTx, heights map[ChannelID]uint64) error {
	chanDetailsBkt := tx.ReadWriteBucket(cChanDetailsBkt)
	if chanDetailsBkt == nil {
		return ErrUninitializedDB
	}

	for chanID, maxHeight := range heights {
		chanDetails := chanDetailsBkt.NestedReadWriteBucket(chanID[:])

		// If the details bucket for this channel ID does not exist,
		// it is probably a channel that has been closed and deleted
		// already. So we can skip this height.
		if chanDetails == nil {
			continue
		}

		b, err := writeBigSize(maxHeight)
		if err != nil {
			return err
		}

		err = chanDetails.Put(cChanMaxCommitmentHeight, b)
		if err != nil {
			return err
		}
	}

	return nil
}

// collectChanMaxHeights iterates over all the sessions in the DB. For each
// session, it iterates over all the Acked updates and the committed updates
// to collect the maximum commitment height for each channel.
func collectChanMaxHeights(tx kvdb.RwTx) (map[ChannelID]uint64, error) {
	sessionsBkt := tx.ReadBucket(cSessionBkt)
	if sessionsBkt == nil {
		return nil, ErrUninitializedDB
	}

	chanIDIndexBkt := tx.ReadBucket(cChanIDIndexBkt)
	if chanIDIndexBkt == nil {
		return nil, ErrUninitializedDB
	}

	heights := make(map[ChannelID]uint64)

	// For each update we consider, we will only update the heights map if
	// the commitment height for the channel is larger than the current
	// max height stored for the channel.
	cb := func(chanID ChannelID, commitHeight uint64) {
		if commitHeight > heights[chanID] {
			heights[chanID] = commitHeight
		}
	}

	err := sessionsBkt.ForEach(func(sessIDBytes, _ []byte) error {
		sessBkt := sessionsBkt.NestedReadBucket(sessIDBytes)
		if sessBkt == nil {
			return fmt.Errorf("bucket not found for session %x",
				sessIDBytes)
		}

		err := forEachCommittedUpdate(sessBkt, cb)
		if err != nil {
			return err
		}

		return forEachAckedUpdate(sessBkt, chanIDIndexBkt, cb)
	})
	if err != nil {
		return nil, err
	}

	return heights, nil
}

// forEachCommittedUpdate iterates over all the given session's committed
// updates and calls the call-back for each.
func forEachCommittedUpdate(sessBkt kvdb.RBucket,
	cb func(chanID ChannelID, commitHeight uint64)) error {

	sessionCommits := sessBkt.NestedReadBucket(cSessionCommits)
	if sessionCommits == nil {
		return nil
	}

	return sessionCommits.ForEach(func(k, v []byte) error {
		var update CommittedUpdate
		err := update.Decode(bytes.NewReader(v))
		if err != nil {
			return err
		}

		cb(update.BackupID.ChanID, update.BackupID.CommitHeight)

		return nil
	})
}

// forEachAckedUpdate iterates over all the given session's acked update range
// indices and calls the call-back for each.
func forEachAckedUpdate(sessBkt, chanIDIndexBkt kvdb.RBucket,
	cb func(chanID ChannelID, commitHeight uint64)) error {

	sessionAcksRanges := sessBkt.NestedReadBucket(cSessionAckRangeIndex)
	if sessionAcksRanges == nil {
		return nil
	}

	return sessionAcksRanges.ForEach(func(dbChanID, _ []byte) error {
		rangeBkt := sessionAcksRanges.NestedReadBucket(dbChanID)
		if rangeBkt == nil {
			return nil
		}

		index, err := readRangeIndex(rangeBkt)
		if err != nil {
			return err
		}

		chanIDBytes := chanIDIndexBkt.Get(dbChanID)
		var chanID ChannelID
		copy(chanID[:], chanIDBytes)

		cb(chanID, index.MaxHeight())

		return nil
	})
}

// readRangeIndex reads a persisted RangeIndex from the passed bucket and into
// a new in-memory RangeIndex.
func readRangeIndex(rangesBkt kvdb.RBucket) (*RangeIndex, error) {
	ranges := make(map[uint64]uint64)
	err := rangesBkt.ForEach(func(k, v []byte) error {
		start, err := readBigSize(k)
		if err != nil {
			return err
		}

		end, err := readBigSize(v)
		if err != nil {
			return err
		}

		ranges[start] = end

		return nil
	})
	if err != nil {
		return nil, err
	}

	return NewRangeIndex(ranges, WithSerializeUint64Fn(writeBigSize))
}

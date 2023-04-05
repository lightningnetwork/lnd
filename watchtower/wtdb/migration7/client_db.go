package migration7

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// cSessionBkt is a top-level bucket storing:
	//   session-id => cSessionBody -> encoded ClientSessionBody
	// 		=> cSessionDBID -> db-assigned-id
	//              => cSessionCommits => seqnum -> encoded CommittedUpdate
	//              => cSessionAckRangeIndex => chan-id => acked-index-range
	cSessionBkt = []byte("client-session-bucket")

	// cChanDetailsBkt is a top-level bucket storing:
	//   channel-id => cChannelSummary -> encoded ClientChanSummary.
	//  		=> cChanDBID -> db-assigned-id
	// 		=> cChanSessions => db-session-id -> 1
	cChanDetailsBkt = []byte("client-channel-detail-bucket")

	// cChannelSummary is a sub-bucket of cChanDetailsBkt which stores the
	// encoded body of ClientChanSummary.
	cChannelSummary = []byte("client-channel-summary")

	// cChanSessions is a sub-bucket of cChanDetailsBkt which stores:
	//    session-id -> 1
	cChanSessions = []byte("client-channel-sessions")

	// cSessionAckRangeIndex is a sub-bucket of cSessionBkt storing:
	//    chan-id => start -> end
	cSessionAckRangeIndex = []byte("client-session-ack-range-index")

	// cSessionDBID is a key used in the cSessionBkt to store the
	// db-assigned-d of a session.
	cSessionDBID = []byte("client-session-db-id")

	// cChanIDIndexBkt is a top-level bucket storing:
	//    db-assigned-id -> channel-ID
	cChanIDIndexBkt = []byte("client-channel-id-index")

	// ErrUninitializedDB signals that top-level buckets for the database
	// have not been initialized.
	ErrUninitializedDB = errors.New("db not initialized")

	// ErrCorruptClientSession signals that the client session's on-disk
	// structure deviates from what is expected.
	ErrCorruptClientSession = errors.New("client session corrupted")

	// byteOrder is the default endianness used when serializing integers.
	byteOrder = binary.BigEndian
)

// MigrateChannelToSessionIndex migrates the tower client DB to add an index
// from channel-to-session. This will make it easier in future to check which
// sessions have updates for which channels.
func MigrateChannelToSessionIndex(tx kvdb.RwTx) error {
	log.Infof("Migrating the tower client DB to build a new " +
		"channel-to-session index")

	sessionsBkt := tx.ReadBucket(cSessionBkt)
	if sessionsBkt == nil {
		return ErrUninitializedDB
	}

	chanDetailsBkt := tx.ReadWriteBucket(cChanDetailsBkt)
	if chanDetailsBkt == nil {
		return ErrUninitializedDB
	}

	chanIDsBkt := tx.ReadBucket(cChanIDIndexBkt)
	if chanIDsBkt == nil {
		return ErrUninitializedDB
	}

	// First gather all the new channel-to-session pairs that we want to
	// add.
	index, err := collectIndex(sessionsBkt)
	if err != nil {
		return err
	}

	// Then persist those pairs to the db.
	return persistIndex(chanDetailsBkt, chanIDsBkt, index)
}

// collectIndex iterates through all the sessions and uses the keys in the
// cSessionAckRangeIndex bucket to collect all the channels that the session
// has updates for. The function returns a map from channel ID to session ID
// (using the db-assigned IDs for both).
func collectIndex(sessionsBkt kvdb.RBucket) (map[uint64]map[uint64]bool,
	error) {

	index := make(map[uint64]map[uint64]bool)
	err := sessionsBkt.ForEach(func(sessID, _ []byte) error {
		sessionBkt := sessionsBkt.NestedReadBucket(sessID)
		if sessionBkt == nil {
			return ErrCorruptClientSession
		}

		ackedRanges := sessionBkt.NestedReadBucket(
			cSessionAckRangeIndex,
		)
		if ackedRanges == nil {
			return nil
		}

		sessDBIDBytes := sessionBkt.Get(cSessionDBID)
		if sessDBIDBytes == nil {
			return ErrCorruptClientSession
		}

		sessDBID, err := readUint64(sessDBIDBytes)
		if err != nil {
			return err
		}

		return ackedRanges.ForEach(func(dbChanIDBytes, _ []byte) error {
			dbChanID, err := readUint64(dbChanIDBytes)
			if err != nil {
				return err
			}

			if _, ok := index[dbChanID]; !ok {
				index[dbChanID] = make(map[uint64]bool)
			}

			index[dbChanID][sessDBID] = true

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return index, nil
}

// persistIndex adds the channel-to-session mapping in each channel's details
// bucket.
func persistIndex(chanDetailsBkt kvdb.RwBucket, chanIDsBkt kvdb.RBucket,
	index map[uint64]map[uint64]bool) error {

	for dbChanID, sessIDs := range index {
		dbChanIDBytes, err := writeUint64(dbChanID)
		if err != nil {
			return err
		}

		realChanID := chanIDsBkt.Get(dbChanIDBytes)

		chanBkt := chanDetailsBkt.NestedReadWriteBucket(realChanID)
		if chanBkt == nil {
			return fmt.Errorf("channel not found")
		}

		sessIDsBkt, err := chanBkt.CreateBucket(cChanSessions)
		if err != nil {
			return err
		}

		for id := range sessIDs {
			sessID, err := writeUint64(id)
			if err != nil {
				return err
			}

			err = sessIDsBkt.Put(sessID, []byte{1})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func writeUint64(i uint64) ([]byte, error) {
	var b bytes.Buffer
	err := tlv.WriteVarInt(&b, i, &[8]byte{})
	if err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func readUint64(b []byte) (uint64, error) {
	r := bytes.NewReader(b)
	i, err := tlv.ReadVarInt(r, &[8]byte{})
	if err != nil {
		return 0, err
	}

	return i, nil
}

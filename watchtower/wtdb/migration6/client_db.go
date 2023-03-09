package migration6

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// cSessionBkt is a top-level bucket storing:
	//   session-id => cSessionBody -> encoded ClientSessionBody
	// 		=> cSessionDBID -> db-assigned-id
	//              => cSessionCommits => seqnum -> encoded CommittedUpdate
	//              => cSessionAcks => seqnum -> encoded BackupID
	cSessionBkt = []byte("client-session-bucket")

	// cSessionDBID is a key used in the cSessionBkt to store the
	// db-assigned-id of a session.
	cSessionDBID = []byte("client-session-db-id")

	// cSessionIDIndexBkt is a top-level bucket storing:
	//    db-assigned-id -> session-id
	cSessionIDIndexBkt = []byte("client-session-id-index")

	// cSessionBody is a sub-bucket of cSessionBkt storing only the body of
	// the ClientSession.
	cSessionBody = []byte("client-session-body")

	// ErrUninitializedDB signals that top-level buckets for the database
	// have not been initialized.
	ErrUninitializedDB = errors.New("db not initialized")

	// ErrCorruptClientSession signals that the client session's on-disk
	// structure deviates from what is expected.
	ErrCorruptClientSession = errors.New("client session corrupted")

	byteOrder = binary.BigEndian
)

// MigrateSessionIDIndex adds a new session ID index to the tower client db.
// This index is a mapping from db-assigned ID (a uint64 encoded using BigSize)
// to real session ID (33 bytes). This mapping will allow us to persist session
// pointers with fewer bytes in the future.
func MigrateSessionIDIndex(tx kvdb.RwTx) error {
	log.Infof("Migrating the tower client db to add a new session ID " +
		"index which stores a mapping from db-assigned ID to real " +
		"session ID")

	// Create a new top-level bucket for the index.
	indexBkt, err := tx.CreateTopLevelBucket(cSessionIDIndexBkt)
	if err != nil {
		return err
	}

	// Get the existing top-level sessions bucket.
	sessionsBkt := tx.ReadWriteBucket(cSessionBkt)
	if sessionsBkt == nil {
		return ErrUninitializedDB
	}

	// Iterate over the sessions bucket where each key is a session-ID.
	return sessionsBkt.ForEach(func(sessionID, _ []byte) error {
		// Ask the DB for a new, unique, id for the index bucket.
		nextSeq, err := indexBkt.NextSequence()
		if err != nil {
			return err
		}

		newIndex, err := writeBigSize(nextSeq)
		if err != nil {
			return err
		}

		// Add the new db-assigned-ID to real-session-ID pair to the
		// new index bucket.
		err = indexBkt.Put(newIndex, sessionID)
		if err != nil {
			return err
		}

		// Get the sub-bucket for this specific session ID.
		sessionBkt := sessionsBkt.NestedReadWriteBucket(sessionID)
		if sessionBkt == nil {
			return ErrCorruptClientSession
		}

		// Here we ensure that the session bucket includes a session
		// body. The only reason we do this is so that we can simulate
		// a migration fail in a test to ensure that a migration fail
		// results in an untouched db.
		sessionBodyBytes := sessionBkt.Get(cSessionBody)
		if sessionBodyBytes == nil {
			return ErrCorruptClientSession
		}

		// Add the db-assigned ID of the session to the session under
		// the cSessionDBID key.
		return sessionBkt.Put(cSessionDBID, newIndex)
	})
}

// writeBigSize will encode the given uint64 as a BigSize byte slice.
func writeBigSize(i uint64) ([]byte, error) {
	var b bytes.Buffer
	err := tlv.WriteVarInt(&b, i, &[8]byte{})
	if err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

package migration4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

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

	// cSessionBkt is a top-level bucket storing:
	//   session-id => cSessionBody -> encoded ClientSessionBody
	//              => cSessionCommits => seqnum -> encoded CommittedUpdate
	//              => cSessionAcks => seqnum -> encoded BackupID
	cSessionBkt = []byte("client-session-bucket")

	// cSessionAcks is a sub-bucket of cSessionBkt storing:
	//    seqnum -> encoded BackupID.
	cSessionAcks = []byte("client-session-acks")

	// cSessionAckRangeIndex is a sub-bucket of cSessionBkt storing:
	//    chan-id => start -> end
	cSessionAckRangeIndex = []byte("client-session-ack-range-index")

	// ErrUninitializedDB signals that top-level buckets for the database
	// have not been initialized.
	ErrUninitializedDB = errors.New("db not initialized")

	// ErrClientSessionNotFound signals that the requested client session
	// was not found in the database.
	ErrClientSessionNotFound = errors.New("client session not found")

	// ErrCorruptChanDetails signals that the clients channel detail's
	// on-disk structure deviates from what is expected.
	ErrCorruptChanDetails = errors.New("channel details corrupted")

	// ErrChannelNotRegistered signals a channel has not yet been registered
	// in the client database.
	ErrChannelNotRegistered = errors.New("channel not registered")

	// byteOrder is the default endianness used when serializing integers.
	byteOrder = binary.BigEndian

	// errExit is an error used to signal that the sessionIterator should
	// exit.
	errExit = errors.New("the exit condition has been met")
)

// DefaultSessionsPerTx is the default number of sessions that should be
// migrated per db transaction.
const DefaultSessionsPerTx = 5000

// MigrateAckedUpdates migrates the tower client DB. It takes the individual
// Acked Updates that are stored for each session and re-stores them using the
// RangeIndex representation.
func MigrateAckedUpdates(sessionsPerTx int) func(kvdb.Backend) error {
	return func(db kvdb.Backend) error {
		log.Infof("Migrating the tower client db to move all Acked " +
			"Updates to the new Range Index representation.")

		// Migrate the old acked-updates.
		err := migrateAckedUpdates(db, sessionsPerTx)
		if err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}

		log.Infof("Migrating old session acked updates finished, now " +
			"checking the migration results...")

		// Before we can safety delete the old buckets, we perform a
		// check to make sure the sessions have been migrated as
		// expected.
		err = kvdb.View(db, validateMigration, func() {})
		if err != nil {
			return fmt.Errorf("validate migration failed: %w", err)
		}

		// Delete old acked updates.
		err = kvdb.Update(db, deleteOldAckedUpdates, func() {})
		if err != nil {
			return fmt.Errorf("failed to delete old acked "+
				"updates: %w", err)
		}

		return nil
	}
}

// migrateAckedUpdates migrates the acked updates of each session in the
// wtclient db into the new RangeIndex form. This is done over multiple db
// transactions in order to prevent the migration from taking up too much RAM.
// The sessionsPerTx parameter can be used to set the maximum number of sessions
// that should be migrated per transaction.
func migrateAckedUpdates(db kvdb.Backend, sessionsPerTx int) error {
	// Get migration progress stats.
	total, migrated, err := logMigrationStats(db)
	if err != nil {
		return err
	}
	log.Infof("Total sessions=%d, migrated=%d", total, migrated)

	// Exit early if the old session acked updates have already been
	// migrated and deleted.
	if total == 0 {
		log.Info("Migration already finished!")
		return nil
	}

	var (
		finished bool
		startKey []byte
	)
	for {
		// Process the migration.
		err = kvdb.Update(db, func(tx kvdb.RwTx) error {
			startKey, finished, err = processMigration(
				tx, startKey, sessionsPerTx,
			)

			return err
		}, func() {})
		if err != nil {
			return err
		}

		if finished {
			break
		}

		// Each time we finished the above process, we'd read the stats
		// again to understand the current progress.
		total, migrated, err = logMigrationStats(db)
		if err != nil {
			return err
		}

		// Calculate and log the progress if the progress is less than
		// one hundred percent.
		progress := float64(migrated) / float64(total) * 100
		if progress >= 100 {
			break
		}

		log.Infof("Migration progress: %.3f%%, still have: %d",
			progress, total-migrated)
	}

	return nil
}

func validateMigration(tx kvdb.RTx) error {
	mainSessionsBkt := tx.ReadBucket(cSessionBkt)
	if mainSessionsBkt == nil {
		return ErrUninitializedDB
	}

	chanDetailsBkt := tx.ReadBucket(cChanDetailsBkt)
	if chanDetailsBkt == nil {
		return ErrUninitializedDB
	}

	return mainSessionsBkt.ForEach(func(sessID, _ []byte) error {
		// Get the bucket for this particular session.
		sessionBkt := mainSessionsBkt.NestedReadBucket(sessID)
		if sessionBkt == nil {
			return ErrClientSessionNotFound
		}

		// Get the bucket where any old acked updates would be stored.
		oldAcksBucket := sessionBkt.NestedReadBucket(cSessionAcks)

		// Get the bucket where any new acked updates would be stored.
		newAcksBucket := sessionBkt.NestedReadBucket(
			cSessionAckRangeIndex,
		)

		switch {
		// If both the old and new acked updates buckets are nil, then
		// we can safely skip this session.
		case oldAcksBucket == nil && newAcksBucket == nil:
			return nil

		case oldAcksBucket == nil:
			return fmt.Errorf("no old acks but do have new acks")

		case newAcksBucket == nil:
			return fmt.Errorf("no new acks but have old acks")

		default:
		}

		// Collect acked ranges for this session.
		ackedRanges := make(map[uint64]*RangeIndex)
		err := newAcksBucket.ForEach(func(dbChanID, _ []byte) error {
			rangeIndexBkt := newAcksBucket.NestedReadBucket(
				dbChanID,
			)
			if rangeIndexBkt == nil {
				return fmt.Errorf("no acked updates bucket "+
					"found for channel %x", dbChanID)
			}

			// Read acked ranges from new bucket.
			ri, err := readRangeIndex(rangeIndexBkt)
			if err != nil {
				return err
			}

			dbChanIDNum, err := readBigSize(dbChanID)
			if err != nil {
				return err
			}

			ackedRanges[dbChanIDNum] = ri

			return nil
		})
		if err != nil {
			return err
		}

		// Now we will iterate through each of the old acked updates and
		// make sure that the update appears in the new bucket.
		return oldAcksBucket.ForEach(func(_, v []byte) error {
			var backupID BackupID
			err := backupID.Decode(bytes.NewReader(v))
			if err != nil {
				return err
			}

			dbChanID, _, err := getDBChanID(
				chanDetailsBkt, backupID.ChanID,
			)
			if err != nil {
				return err
			}

			index, ok := ackedRanges[dbChanID]
			if !ok {
				return fmt.Errorf("no index found for this " +
					"channel")
			}

			if !index.IsInIndex(backupID.CommitHeight) {
				return fmt.Errorf("commit height not found " +
					"in index")
			}

			return nil
		})
	})
}

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

func deleteOldAckedUpdates(tx kvdb.RwTx) error {
	mainSessionsBkt := tx.ReadWriteBucket(cSessionBkt)
	if mainSessionsBkt == nil {
		return ErrUninitializedDB
	}

	return mainSessionsBkt.ForEach(func(sessID, _ []byte) error {
		// Get the bucket for this particular session.
		sessionBkt := mainSessionsBkt.NestedReadWriteBucket(
			sessID,
		)
		if sessionBkt == nil {
			return ErrClientSessionNotFound
		}

		// Get the bucket where any old acked updates would be stored.
		oldAcksBucket := sessionBkt.NestedReadBucket(cSessionAcks)
		if oldAcksBucket == nil {
			return nil
		}

		// Now that we have read everything that we need to from
		// the cSessionAcks sub-bucket, we can delete it.
		return sessionBkt.DeleteNestedBucket(cSessionAcks)
	})
}

// processMigration uses the given transaction to perform a maximum of
// sessionsPerTx session migrations. If startKey is non-nil, it is used to
// determine the first session to start the migration at. The first return
// item is the key of the last session that was migrated successfully and the
// boolean is true if there are no more sessions left to migrate.
func processMigration(tx kvdb.RwTx, startKey []byte, sessionsPerTx int) ([]byte,
	bool, error) {

	chanDetailsBkt := tx.ReadWriteBucket(cChanDetailsBkt)
	if chanDetailsBkt == nil {
		return nil, false, ErrUninitializedDB
	}

	// sessionCount keeps track of the number of sessions that have been
	// migrated under the current db transaction.
	var sessionCount int

	// migrateSessionCB is a callback function that calls migrateSession
	// in order to migrate a single session. Upon success, the sessionCount
	// is incremented and is then compared against sessionsPerTx to
	// determine if we should continue migrating more sessions in this db
	// transaction.
	migrateSessionCB := func(sessionBkt kvdb.RwBucket) error {
		err := migrateSession(chanDetailsBkt, sessionBkt)
		if err != nil {
			return err
		}

		sessionCount++

		// If we have migrated sessionsPerTx sessions in this tx, then
		// we return errExit in order to signal that this tx should be
		// committed and the migration should be continued in a new
		// transaction.
		if sessionCount >= sessionsPerTx {
			return errExit
		}

		return nil
	}

	// Starting at startKey, iterate over the sessions in the db and migrate
	// them until either all are migrated or until the errExit signal is
	// received.
	lastKey, err := sessionIterator(tx, startKey, migrateSessionCB)
	if err != nil && errors.Is(err, errExit) {
		return lastKey, false, nil
	} else if err != nil {
		return nil, false, err
	}

	// The migration is complete.
	return nil, true, nil
}

// migrateSession migrates a single session's acked-updates to the new
// RangeIndex form.
func migrateSession(chanDetailsBkt kvdb.RBucket,
	sessionBkt kvdb.RwBucket) error {

	// Get the existing cSessionAcks bucket. If there is no such bucket,
	// then there are no acked-updates to migrate for this session.
	sessionAcks := sessionBkt.NestedReadBucket(cSessionAcks)
	if sessionAcks == nil {
		return nil
	}

	// If there is already a new cSessionAckedRangeIndex bucket, then this
	// session has already been migrated.
	sessionAckRangesBkt := sessionBkt.NestedReadBucket(
		cSessionAckRangeIndex,
	)
	if sessionAckRangesBkt != nil {
		return nil
	}

	// Otherwise, we will iterate over each of the acked-updates, and we
	// will construct a new RangeIndex for each channel.
	m := make(map[ChannelID]*RangeIndex)
	if err := sessionAcks.ForEach(func(_, v []byte) error {
		var backupID BackupID
		err := backupID.Decode(bytes.NewReader(v))
		if err != nil {
			return err
		}

		if _, ok := m[backupID.ChanID]; !ok {
			index, err := NewRangeIndex(nil)
			if err != nil {
				return err
			}

			m[backupID.ChanID] = index
		}

		return m[backupID.ChanID].Add(backupID.CommitHeight, nil)
	}); err != nil {
		return err
	}

	// Create a new sub-bucket that will be used to store the new RangeIndex
	// representation of the acked updates.
	ackRangeBkt, err := sessionBkt.CreateBucket(cSessionAckRangeIndex)
	if err != nil {
		return err
	}

	// Iterate over each of the new range indexes that we will add for this
	// session.
	for chanID, rangeIndex := range m {
		// Get db chanID.
		chanDetails := chanDetailsBkt.NestedReadBucket(chanID[:])
		if chanDetails == nil {
			return ErrCorruptChanDetails
		}

		// Create a sub-bucket for this channel using the db-assigned ID
		// for the channel.
		dbChanID := chanDetails.Get(cChanDBID)
		chanAcksBkt, err := ackRangeBkt.CreateBucket(dbChanID)
		if err != nil {
			return err
		}

		// Iterate over the range pairs that we need to add to the DB.
		for k, v := range rangeIndex.GetAllRanges() {
			start, err := writeBigSize(k)
			if err != nil {
				return err
			}

			end, err := writeBigSize(v)
			if err != nil {
				return err
			}

			err = chanAcksBkt.Put(start, end)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// logMigrationStats reads the buckets to provide stats over current migration
// progress. The returned values are the numbers of total records and already
// migrated records.
func logMigrationStats(db kvdb.Backend) (uint64, uint64, error) {
	var (
		err        error
		total      uint64
		unmigrated uint64
	)

	err = kvdb.View(db, func(tx kvdb.RTx) error {
		total, unmigrated, err = getMigrationStats(tx)

		return err
	}, func() {})

	log.Debugf("Total sessions=%d, unmigrated=%d", total, unmigrated)

	return total, total - unmigrated, err
}

// getMigrationStats iterates over all sessions. It counts the total number of
// sessions as well as the total number of unmigrated sessions.
func getMigrationStats(tx kvdb.RTx) (uint64, uint64, error) {
	var (
		total      uint64
		unmigrated uint64
	)

	// Get sessions bucket.
	mainSessionsBkt := tx.ReadBucket(cSessionBkt)
	if mainSessionsBkt == nil {
		return 0, 0, ErrUninitializedDB
	}

	// Iterate over each session ID in the bucket.
	err := mainSessionsBkt.ForEach(func(sessID, _ []byte) error {
		// Get the bucket for this particular session.
		sessionBkt := mainSessionsBkt.NestedReadBucket(sessID)
		if sessionBkt == nil {
			return ErrClientSessionNotFound
		}

		total++

		// Get the cSessionAckRangeIndex bucket.
		sessionAcksBkt := sessionBkt.NestedReadBucket(cSessionAcks)

		// Get the cSessionAckRangeIndex bucket.
		sessionAckRangesBkt := sessionBkt.NestedReadBucket(
			cSessionAckRangeIndex,
		)

		// If both buckets do not exist, then this session is empty and
		// does not need to be migrated.
		if sessionAckRangesBkt == nil && sessionAcksBkt == nil {
			return nil
		}

		// If the sessionAckRangesBkt is not nil, then the session has
		// already been migrated.
		if sessionAckRangesBkt != nil {
			return nil
		}

		// Else the session has not yet been migrated.
		unmigrated++

		return nil
	})
	if err != nil {
		return 0, 0, err
	}

	return total, unmigrated, nil
}

// getDBChanID returns the db-assigned channel ID for the given real channel ID.
// It returns both the uint64 and byte representation.
func getDBChanID(chanDetailsBkt kvdb.RBucket, chanID ChannelID) (uint64,
	[]byte, error) {

	chanDetails := chanDetailsBkt.NestedReadBucket(chanID[:])
	if chanDetails == nil {
		return 0, nil, ErrChannelNotRegistered
	}

	idBytes := chanDetails.Get(cChanDBID)
	if len(idBytes) == 0 {
		return 0, nil, fmt.Errorf("no db-assigned ID found for "+
			"channel ID %s", chanID)
	}

	id, err := readBigSize(idBytes)
	if err != nil {
		return 0, nil, err
	}

	return id, idBytes, nil
}

// callback defines a type that's used by the sessionIterator.
type callback func(bkt kvdb.RwBucket) error

// sessionIterator is a helper function that iterates over the main sessions
// bucket and performs the callback function on each individual session. If a
// seeker is specified, it will move the cursor to the given position otherwise
// it will start from the first item.
func sessionIterator(tx kvdb.RwTx, seeker []byte, cb callback) ([]byte, error) {
	// Get sessions bucket.
	mainSessionsBkt := tx.ReadWriteBucket(cSessionBkt)
	if mainSessionsBkt == nil {
		return nil, ErrUninitializedDB
	}

	c := mainSessionsBkt.ReadCursor()
	k, _ := c.First()

	// Move the cursor to the specified position if seeker is non-nil.
	if seeker != nil {
		k, _ = c.Seek(seeker)
	}

	// Start the iteration and exit on condition.
	for k := k; k != nil; k, _ = c.Next() {
		// Get the bucket for this particular session.
		bkt := mainSessionsBkt.NestedReadWriteBucket(k)
		if bkt == nil {
			return nil, ErrClientSessionNotFound
		}

		// Call the callback function with the session's bucket.
		if err := cb(bkt); err != nil {
			// return k, err
			lastIndex := make([]byte, len(k))
			copy(lastIndex, k)
			return lastIndex, err
		}
	}

	return nil, nil
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

// readBigSize converts the given byte slice into a uint64 and assumes that the
// bytes slice is using BigSize encoding.
func readBigSize(b []byte) (uint64, error) {
	r := bytes.NewReader(b)
	i, err := tlv.ReadVarInt(r, &[8]byte{})
	if err != nil {
		return 0, err
	}

	return i, nil
}

package wtdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
)

const (
	// dbName is the filename of tower database.
	dbName = "watchtower.db"

	// dbFilePermission requests read+write access to the db file.
	dbFilePermission = 0600
)

var (
	// sessionsBkt is a bucket containing all negotiated client sessions.
	//  session id -> session
	sessionsBkt = []byte("sessions-bucket")

	// updatesBkt is a bucket containing all state updates sent by clients.
	// The updates are further bucketed by session id to prevent clients
	// from overwrite each other.
	//   hint => session id -> update
	updatesBkt = []byte("updates-bucket")

	// updateIndexBkt is a bucket that indexes all state updates by their
	// overarching session id. This allows for efficient lookup of updates
	// by their session id, which is currently used to aide deletion
	// performance.
	//  session id => hint1 -> []byte{}
	//             => hint2 -> []byte{}
	updateIndexBkt = []byte("update-index-bucket")

	// lookoutTipBkt is a bucket containing the last block epoch processed
	// by the lookout subsystem. It has one key, lookoutTipKey.
	//   lookoutTipKey -> block epoch
	lookoutTipBkt = []byte("lookout-tip-bucket")

	// lookoutTipKey is a static key used to retrieve lookout tip's block
	// epoch from the lookoutTipBkt.
	lookoutTipKey = []byte("lookout-tip")

	// metadataBkt stores all the meta information concerning the state of
	// the database.
	metadataBkt = []byte("metadata-bucket")

	// dbVersionKey is a static key used to retrieve the database version
	// number from the metadataBkt.
	dbVersionKey = []byte("version")

	// ErrUninitializedDB signals that top-level buckets for the database
	// have not been initialized.
	ErrUninitializedDB = errors.New("tower db not initialized")

	// ErrNoDBVersion signals that the database contains no version info.
	ErrNoDBVersion = errors.New("tower db has no version")

	// ErrNoSessionHintIndex signals that an active session does not have an
	// initialized index for tracking its own state updates.
	ErrNoSessionHintIndex = errors.New("session hint index missing")

	byteOrder = binary.BigEndian
)

// TowerDB is single database providing a persistent storage engine for the
// wtserver and lookout subsystems.
type TowerDB struct {
	db     *bbolt.DB
	dbPath string
}

// OpenTowerDB opens the tower database given the path to the database's
// directory. If no such database exists, this method will initialize a fresh
// one using the latest version number and bucket structure. If a database
// exists but has a lower version number than the current version, any necessary
// migrations will be applied before returning. Any attempt to open a database
// with a version number higher that the latest version will fail to prevent
// accidental reversion.
func OpenTowerDB(dbPath string) (*TowerDB, error) {
	path := filepath.Join(dbPath, dbName)

	// If the database file doesn't exist, this indicates we much initialize
	// a fresh database with the latest version.
	firstInit := !fileExists(path)
	if firstInit {
		// Ensure all parent directories are initialized.
		err := os.MkdirAll(dbPath, 0700)
		if err != nil {
			return nil, err
		}
	}

	bdb, err := bbolt.Open(path, dbFilePermission, nil)
	if err != nil {
		return nil, err
	}

	// If the file existed previously, we'll now check to see that the
	// metadata bucket is properly initialized. It could be the case that
	// the database was created, but we failed to actually populate any
	// metadata. If the metadata bucket does not actually exist, we'll
	// set firstInit to true so that we can treat is initialize the bucket.
	if !firstInit {
		var metadataExists bool
		err = bdb.View(func(tx *bbolt.Tx) error {
			metadataExists = tx.Bucket(metadataBkt) != nil
			return nil
		})
		if err != nil {
			return nil, err
		}

		if !metadataExists {
			firstInit = true
		}
	}

	towerDB := &TowerDB{
		db:     bdb,
		dbPath: dbPath,
	}

	if firstInit {
		// If the database has not yet been created, we'll initialize
		// the database version with the latest known version.
		err = towerDB.db.Update(func(tx *bbolt.Tx) error {
			return initDBVersion(tx, getLatestDBVersion(dbVersions))
		})
		if err != nil {
			bdb.Close()
			return nil, err
		}
	} else {
		// Otherwise, ensure that any migrations are applied to ensure
		// the data is in the format expected by the latest version.
		err = towerDB.syncVersions(dbVersions)
		if err != nil {
			bdb.Close()
			return nil, err
		}
	}

	// Now that the database version fully consistent with our latest known
	// version, ensure that all top-level buckets known to this version are
	// initialized. This allows us to assume their presence throughout all
	// operations. If an known top-level bucket is expected to exist but is
	// missing, this will trigger a ErrUninitializedDB error.
	err = towerDB.db.Update(initTowerDBBuckets)
	if err != nil {
		bdb.Close()
		return nil, err
	}

	return towerDB, nil
}

// fileExists returns true if the file exists, and false otherwise.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

// initTowerDBBuckets creates all top-level buckets required to handle database
// operations required by the latest version.
func initTowerDBBuckets(tx *bbolt.Tx) error {
	buckets := [][]byte{
		sessionsBkt,
		updateIndexBkt,
		updatesBkt,
		lookoutTipBkt,
	}

	for _, bucket := range buckets {
		_, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
	}

	return nil
}

// syncVersions ensures the database version is consistent with the highest
// known database version, applying any migrations that have not been made. If
// the highest known version number is lower than the database's version, this
// method will fail to prevent accidental reversions.
func (t *TowerDB) syncVersions(versions []version) error {
	curVersion, err := t.Version()
	if err != nil {
		return err
	}

	latestVersion := getLatestDBVersion(versions)
	switch {

	// Current version is higher than any known version, fail to prevent
	// reversion.
	case curVersion > latestVersion:
		return channeldb.ErrDBReversion

	// Current version matches highest known version, nothing to do.
	case curVersion == latestVersion:
		return nil
	}

	// Otherwise, apply any migrations in order to bring the database
	// version up to the highest known version.
	updates := getMigrations(versions, curVersion)
	return t.db.Update(func(tx *bbolt.Tx) error {
		for _, update := range updates {
			if update.migration == nil {
				continue
			}

			log.Infof("Applying migration #%d", update.number)

			err := update.migration(tx)
			if err != nil {
				log.Errorf("Unable to apply migration #%d: %v",
					err)
				return err
			}
		}

		return putDBVersion(tx, latestVersion)
	})
}

// Version returns the database's current version number.
func (t *TowerDB) Version() (uint32, error) {
	var version uint32
	err := t.db.View(func(tx *bbolt.Tx) error {
		var err error
		version, err = getDBVersion(tx)
		return err
	})
	if err != nil {
		return 0, err
	}

	return version, nil
}

// Close closes the underlying database.
func (t *TowerDB) Close() error {
	return t.db.Close()
}

// GetSessionInfo retrieves the session for the passed session id. An error is
// returned if the session could not be found.
func (t *TowerDB) GetSessionInfo(id *SessionID) (*SessionInfo, error) {
	var session *SessionInfo
	err := t.db.View(func(tx *bbolt.Tx) error {
		sessions := tx.Bucket(sessionsBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		var err error
		session, err = getSession(sessions, id[:])
		return err
	})
	if err != nil {
		return nil, err
	}

	return session, nil
}

// InsertSessionInfo records a negotiated session in the tower database. An
// error is returned if the session already exists.
func (t *TowerDB) InsertSessionInfo(session *SessionInfo) error {
	return t.db.Update(func(tx *bbolt.Tx) error {
		sessions := tx.Bucket(sessionsBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		updateIndex := tx.Bucket(updateIndexBkt)
		if updateIndex == nil {
			return ErrUninitializedDB
		}

		dbSession, err := getSession(sessions, session.ID[:])
		switch {
		case err == ErrSessionNotFound:
			// proceed.

		case err != nil:
			return err

		case dbSession.LastApplied > 0:
			return ErrSessionAlreadyExists
		}

		err = putSession(sessions, session)
		if err != nil {
			return err
		}

		// Initialize the session-hint index which will be used to track
		// all updates added for this session. Upon deletion, we will
		// consult the index to determine exactly which updates should
		// be deleted without needing to iterate over the entire
		// database.
		return touchSessionHintBkt(updateIndex, &session.ID)
	})
}

// InsertStateUpdate stores an update sent by the client after validating that
// the update is well-formed in the context of other updates sent for the same
// session. This include verifying that the sequence number is incremented
// properly and the last applied values echoed by the client are sane.
func (t *TowerDB) InsertStateUpdate(update *SessionStateUpdate) (uint16, error) {
	var lastApplied uint16
	err := t.db.Update(func(tx *bbolt.Tx) error {
		sessions := tx.Bucket(sessionsBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		updates := tx.Bucket(updatesBkt)
		if updates == nil {
			return ErrUninitializedDB
		}

		updateIndex := tx.Bucket(updateIndexBkt)
		if updateIndex == nil {
			return ErrUninitializedDB
		}

		// Fetch the session corresponding to the update's session id.
		// This will be used to validate that the update's sequence
		// number and last applied values are sane.
		session, err := getSession(sessions, update.ID[:])
		if err != nil {
			return err
		}

		// Validate the update against the current state of the session.
		err = session.AcceptUpdateSequence(
			update.SeqNum, update.LastApplied,
		)
		if err != nil {
			return err
		}

		// Validation succeeded, therefore the update is committed and
		// the session's last applied value is equal to the update's
		// sequence number.
		lastApplied = session.LastApplied

		// Store the updated session to persist the updated last applied
		// values.
		err = putSession(sessions, session)
		if err != nil {
			return err
		}

		// Create or load the hint bucket for this state update's hint
		// and write the given update.
		hints, err := updates.CreateBucketIfNotExists(update.Hint[:])
		if err != nil {
			return err
		}

		var b bytes.Buffer
		err = update.Encode(&b)
		if err != nil {
			return err
		}

		err = hints.Put(update.ID[:], b.Bytes())
		if err != nil {
			return err
		}

		// Finally, create an entry in the update index to track this
		// hint under its session id. This will allow us to delete the
		// entries efficiently if the session is ever removed.
		return putHintForSession(updateIndex, &update.ID, update.Hint)
	})
	if err != nil {
		return 0, err
	}

	return lastApplied, nil
}

// DeleteSession removes all data associated with a particular session id from
// the tower's database.
func (t *TowerDB) DeleteSession(target SessionID) error {
	return t.db.Update(func(tx *bbolt.Tx) error {
		sessions := tx.Bucket(sessionsBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		updates := tx.Bucket(updatesBkt)
		if updates == nil {
			return ErrUninitializedDB
		}

		updateIndex := tx.Bucket(updateIndexBkt)
		if updateIndex == nil {
			return ErrUninitializedDB
		}

		// Fail if the session doesn't exit.
		_, err := getSession(sessions, target[:])
		if err != nil {
			return err
		}

		// Remove the target session.
		err = sessions.Delete(target[:])
		if err != nil {
			return err
		}

		// Next, check the update index for any hints that were added
		// under this session.
		hints, err := getHintsForSession(updateIndex, &target)
		if err != nil {
			return err
		}

		for _, hint := range hints {
			// Remove the state updates for any blobs stored under
			// the target session identifier.
			updatesForHint := updates.Bucket(hint[:])
			if updatesForHint == nil {
				continue
			}

			update := updatesForHint.Get(target[:])
			if update == nil {
				continue
			}

			err := updatesForHint.Delete(target[:])
			if err != nil {
				return err
			}

			// If this was the last state update, we can also remove
			// the hint that would map to an empty set.
			err = isBucketEmpty(updatesForHint)
			switch {

			// Other updates exist for this hint, keep the bucket.
			case err == errBucketNotEmpty:
				continue

			// Unexpected error.
			case err != nil:
				return err

			// No more updates for this hint, prune hint bucket.
			default:
				err = updates.DeleteBucket(hint[:])
				if err != nil {
					return err
				}
			}
		}

		// Finally, remove this session from the update index, which
		// also removes any of the indexed hints beneath it.
		return removeSessionHintBkt(updateIndex, &target)
	})
}

// QueryMatches searches against all known state updates for any that match the
// passed breachHints. More than one Match will be returned for a given hint if
// they exist in the database.
func (t *TowerDB) QueryMatches(breachHints []BreachHint) ([]Match, error) {
	var matches []Match
	err := t.db.View(func(tx *bbolt.Tx) error {
		sessions := tx.Bucket(sessionsBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		updates := tx.Bucket(updatesBkt)
		if updates == nil {
			return ErrUninitializedDB
		}

		// Iterate through the target breach hints, appending any
		// matching updates to the set of matches.
		for _, hint := range breachHints {
			// If a bucket does not exist for this hint, no matches
			// are known.
			updatesForHint := updates.Bucket(hint[:])
			if updatesForHint == nil {
				continue
			}

			// Otherwise, iterate through all (session id, update)
			// pairs, creating a Match for each.
			err := updatesForHint.ForEach(func(k, v []byte) error {
				// Load the session via the session id for this
				// update. The session info contains further
				// instructions for how to process the state
				// update.
				session, err := getSession(sessions, k)
				switch {
				case err == ErrSessionNotFound:
					log.Warnf("Missing session=%x for "+
						"matched state update hint=%x",
						k, hint)
					return nil

				case err != nil:
					return err
				}

				// Decode the state update containing the
				// encrypted blob.
				update := &SessionStateUpdate{}
				err = update.Decode(bytes.NewReader(v))
				if err != nil {
					return err
				}

				var id SessionID
				copy(id[:], k)

				// Construct the final match using the found
				// update and its session info.
				match := Match{
					ID:            id,
					SeqNum:        update.SeqNum,
					Hint:          hint,
					EncryptedBlob: update.EncryptedBlob,
					SessionInfo:   session,
				}

				matches = append(matches, match)

				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return matches, nil
}

// SetLookoutTip stores the provided epoch as the latest lookout tip epoch in
// the tower database.
func (t *TowerDB) SetLookoutTip(epoch *chainntnfs.BlockEpoch) error {
	return t.db.Update(func(tx *bbolt.Tx) error {
		lookoutTip := tx.Bucket(lookoutTipBkt)
		if lookoutTip == nil {
			return ErrUninitializedDB
		}

		return putLookoutEpoch(lookoutTip, epoch)
	})
}

// GetLookoutTip retrieves the current lookout tip block epoch from the tower
// database.
func (t *TowerDB) GetLookoutTip() (*chainntnfs.BlockEpoch, error) {
	var epoch *chainntnfs.BlockEpoch
	err := t.db.View(func(tx *bbolt.Tx) error {
		lookoutTip := tx.Bucket(lookoutTipBkt)
		if lookoutTip == nil {
			return ErrUninitializedDB
		}

		epoch = getLookoutEpoch(lookoutTip)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return epoch, nil
}

// getSession retrieves the session info from the sessions bucket identified by
// its session id. An error is returned if the session is not found or a
// deserialization error occurs.
func getSession(sessions *bbolt.Bucket, id []byte) (*SessionInfo, error) {
	sessionBytes := sessions.Get(id)
	if sessionBytes == nil {
		return nil, ErrSessionNotFound
	}

	var session SessionInfo
	err := session.Decode(bytes.NewReader(sessionBytes))
	if err != nil {
		return nil, err
	}

	return &session, nil
}

// putSession stores the session info in the sessions bucket identified by its
// session id. An error is returned if a serialization error occurs.
func putSession(sessions *bbolt.Bucket, session *SessionInfo) error {
	var b bytes.Buffer
	err := session.Encode(&b)
	if err != nil {
		return err
	}

	return sessions.Put(session.ID[:], b.Bytes())
}

// touchSessionHintBkt initializes the session-hint bucket for a particular
// session id. This ensures that future calls to getHintsForSession or
// putHintForSession can rely on the bucket already being created, and fail if
// index has not been initialized as this points to improper usage.
func touchSessionHintBkt(updateIndex *bbolt.Bucket, id *SessionID) error {
	_, err := updateIndex.CreateBucketIfNotExists(id[:])
	return err
}

// removeSessionHintBkt prunes the session-hint bucket for the given session id
// and all of the hints contained inside. This should be used to clean up the
// index upon session deletion.
func removeSessionHintBkt(updateIndex *bbolt.Bucket, id *SessionID) error {
	return updateIndex.DeleteBucket(id[:])
}

// getHintsForSession returns all known hints belonging to the given session id.
// If the index for the session has not been initialized, this method returns
// ErrNoSessionHintIndex.
func getHintsForSession(updateIndex *bbolt.Bucket,
	id *SessionID) ([]BreachHint, error) {

	sessionHints := updateIndex.Bucket(id[:])
	if sessionHints == nil {
		return nil, ErrNoSessionHintIndex
	}

	var hints []BreachHint
	err := sessionHints.ForEach(func(k, _ []byte) error {
		if len(k) != BreachHintSize {
			return nil
		}

		var hint BreachHint
		copy(hint[:], k)
		hints = append(hints, hint)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return hints, nil
}

// putHintForSession inserts a record into the update index for a given
// (session, hint) pair. The hints are coalesced under a bucket for the target
// session id, and used to perform efficient removal of updates. If the index
// for the session has not been initialized, this method returns
// ErrNoSessionHintIndex.
func putHintForSession(updateIndex *bbolt.Bucket, id *SessionID,
	hint BreachHint) error {

	sessionHints := updateIndex.Bucket(id[:])
	if sessionHints == nil {
		return ErrNoSessionHintIndex
	}

	return sessionHints.Put(hint[:], []byte{})
}

// putLookoutEpoch stores the given lookout tip block epoch in provided bucket.
func putLookoutEpoch(bkt *bbolt.Bucket, epoch *chainntnfs.BlockEpoch) error {
	epochBytes := make([]byte, 36)
	copy(epochBytes, epoch.Hash[:])
	byteOrder.PutUint32(epochBytes[32:], uint32(epoch.Height))

	return bkt.Put(lookoutTipKey, epochBytes)
}

// getLookoutEpoch retrieves the lookout tip block epoch from the given bucket.
// A nil epoch is returned if no update exists.
func getLookoutEpoch(bkt *bbolt.Bucket) *chainntnfs.BlockEpoch {
	epochBytes := bkt.Get(lookoutTipKey)
	if len(epochBytes) != 36 {
		return nil
	}

	var hash chainhash.Hash
	copy(hash[:], epochBytes[:32])
	height := byteOrder.Uint32(epochBytes[32:])

	return &chainntnfs.BlockEpoch{
		Hash:   &hash,
		Height: int32(height),
	}
}

// errBucketNotEmpty is a helper error returned when testing whether a bucket is
// empty or not.
var errBucketNotEmpty = errors.New("bucket not empty")

// isBucketEmpty returns errBucketNotEmpty if the bucket is not empty.
func isBucketEmpty(bkt *bbolt.Bucket) error {
	return bkt.ForEach(func(_, _ []byte) error {
		return errBucketNotEmpty
	})
}

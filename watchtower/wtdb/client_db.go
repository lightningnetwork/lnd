package wtdb

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/watchtower/blob"
)

var (
	// cSessionKeyIndexBkt is a top-level bucket storing:
	//   tower-id -> reserved-session-key-index (uint32).
	cSessionKeyIndexBkt = []byte("client-session-key-index-bucket")

	// cChanDetailsBkt is a top-level bucket storing:
	//   channel-id => cChannelSummary -> encoded ClientChanSummary.
	//  		=> cChanDBID -> db-assigned-id
	// 		=> cChanSessions => db-session-id -> 1
	// 		=> cChanClosedHeight -> block-height
	// 		=> cChanMaxCommitmentHeight -> commitment-height
	cChanDetailsBkt = []byte("client-channel-detail-bucket")

	// cChanSessions is a sub-bucket of cChanDetailsBkt which stores:
	//    db-session-id -> 1
	cChanSessions = []byte("client-channel-sessions")

	// cChanDBID is a key used in the cChanDetailsBkt to store the
	// db-assigned-id of a channel.
	cChanDBID = []byte("client-channel-db-id")

	// cChanClosedHeight is a key used in the cChanDetailsBkt to store the
	// block height at which the channel's closing transaction was mined in.
	// If this there is no associated value for this key, then the channel
	// has not yet been marked as closed.
	cChanClosedHeight = []byte("client-channel-closed-height")

	// cChannelSummary is a key used in cChanDetailsBkt to store the encoded
	// body of ClientChanSummary.
	cChannelSummary = []byte("client-channel-summary")

	// cChanMaxCommitmentHeight is a key used in the cChanDetailsBkt used
	// to store the highest commitment height for this channel that the
	// tower has been handed.
	cChanMaxCommitmentHeight = []byte(
		"client-channel-max-commitment-height",
	)

	// cSessionBkt is a top-level bucket storing:
	//   session-id => cSessionBody -> encoded ClientSessionBody
	// 		=> cSessionDBID -> db-assigned-id
	//              => cSessionCommits => seqnum -> encoded CommittedUpdate
	//              => cSessionAckRangeIndex => db-chan-id => start -> end
	// 		=> cSessionRogueUpdateCount -> count
	cSessionBkt = []byte("client-session-bucket")

	// cSessionDBID is a key used in the cSessionBkt to store the
	// db-assigned-id of a session.
	cSessionDBID = []byte("client-session-db-id")

	// cSessionBody is a sub-bucket of cSessionBkt storing only the body of
	// the ClientSession.
	cSessionBody = []byte("client-session-body")

	// cSessionBody is a sub-bucket of cSessionBkt storing:
	//    seqnum -> encoded CommittedUpdate.
	cSessionCommits = []byte("client-session-commits")

	// cSessionAckRangeIndex is a sub-bucket of cSessionBkt storing
	//    chan-id => start -> end
	cSessionAckRangeIndex = []byte("client-session-ack-range-index")

	// cSessionRogueUpdateCount is a key in the cSessionBkt bucket storing
	// the number of rogue updates that were backed up using the session.
	// Rogue updates are updates for channels that have been closed already
	// at the time of the back-up.
	cSessionRogueUpdateCount = []byte("client-session-rogue-update-count")

	// cChanIDIndexBkt is a top-level bucket storing:
	//    db-assigned-id -> channel-ID
	cChanIDIndexBkt = []byte("client-channel-id-index")

	// cSessionIDIndexBkt is a top-level bucket storing:
	//    db-assigned-id -> session-id
	cSessionIDIndexBkt = []byte("client-session-id-index")

	// cTowerBkt is a top-level bucket storing:
	//    tower-id -> encoded Tower.
	cTowerBkt = []byte("client-tower-bucket")

	// cTowerIndexBkt is a top-level bucket storing:
	//    tower-pubkey -> tower-id.
	cTowerIndexBkt = []byte("client-tower-index-bucket")

	// cTowerToSessionIndexBkt is a top-level bucket storing:
	// 	tower-id -> session-id -> 1
	cTowerToSessionIndexBkt = []byte(
		"client-tower-to-session-index-bucket",
	)

	// cClosableSessionsBkt is a top-level bucket storing:
	// 	db-session-id -> last-channel-close-height
	cClosableSessionsBkt = []byte("client-closable-sessions-bucket")

	// cTaskQueue is a top-level bucket where the disk queue may store its
	// content.
	cTaskQueue = []byte("client-task-queue")

	// ErrTowerNotFound signals that the target tower was not found in the
	// database.
	ErrTowerNotFound = errors.New("tower not found")

	// ErrTowerUnackedUpdates is an error returned when we attempt to mark a
	// tower's sessions as inactive, but one of its sessions has unacked
	// updates.
	ErrTowerUnackedUpdates = errors.New("tower has unacked updates")

	// ErrCorruptClientSession signals that the client session's on-disk
	// structure deviates from what is expected.
	ErrCorruptClientSession = errors.New("client session corrupted")

	// ErrCorruptChanDetails signals that the clients channel detail's
	// on-disk structure deviates from what is expected.
	ErrCorruptChanDetails = errors.New("channel details corrupted")

	// ErrClientSessionAlreadyExists signals an attempt to reinsert a client
	// session that has already been created.
	ErrClientSessionAlreadyExists = errors.New(
		"client session already exists",
	)

	// ErrChannelAlreadyRegistered signals a duplicate attempt to register a
	// channel with the client database.
	ErrChannelAlreadyRegistered = errors.New("channel already registered")

	// ErrChannelNotRegistered signals a channel has not yet been registered
	// in the client database.
	ErrChannelNotRegistered = errors.New("channel not registered")

	// ErrClientSessionNotFound signals that the requested client session
	// was not found in the database.
	ErrClientSessionNotFound = errors.New("client session not found")

	// ErrUpdateAlreadyCommitted signals that the chosen sequence number has
	// already been committed to an update with a different breach hint.
	ErrUpdateAlreadyCommitted = errors.New("update already committed")

	// ErrCommitUnorderedUpdate signals the client tried to commit a
	// sequence number other than the next unallocated sequence number.
	ErrCommitUnorderedUpdate = errors.New("update seqnum not monotonic")

	// ErrCommittedUpdateNotFound signals that the tower tried to ACK a
	// sequence number that has not yet been allocated by the client.
	ErrCommittedUpdateNotFound = errors.New("committed update not found")

	// ErrUnallocatedLastApplied signals that the tower tried to provide a
	// LastApplied value greater than any allocated sequence number.
	ErrUnallocatedLastApplied = errors.New("tower echoed last appiled " +
		"greater than allocated seqnum")

	// ErrNoReservedKeyIndex signals that a client session could not be
	// created because no session key index was reserved.
	ErrNoReservedKeyIndex = errors.New("key index not reserved")

	// ErrIncorrectKeyIndex signals that the client session could not be
	// created because session key index differs from the reserved key
	// index.
	ErrIncorrectKeyIndex = errors.New("incorrect key index")

	// ErrLastTowerAddr is an error returned when the last address of a
	// watchtower is attempted to be removed.
	ErrLastTowerAddr = errors.New("cannot remove last tower address")

	// ErrNoRangeIndexFound is returned when there is no persisted
	// range-index found for the given session ID to channel ID pair.
	ErrNoRangeIndexFound = errors.New("no range index found for the " +
		"given session-channel pair")

	// ErrSessionFailedFilterFn indicates that a particular session did
	// not pass the filter func provided by the caller.
	ErrSessionFailedFilterFn = errors.New("session failed filter func")

	// ErrSessionNotClosable is returned when a session is not found in the
	// closable list.
	ErrSessionNotClosable = errors.New("session is not closable")

	// errSessionHasOpenChannels is an error used to indicate that a
	// session has updates for channels that are still open.
	errSessionHasOpenChannels = errors.New("session has open channels")

	// ErrSessionHasUnackedUpdates is an error used to indicate that a
	// session has un-acked updates.
	ErrSessionHasUnackedUpdates = errors.New("session has un-acked updates")

	// errChannelHasMoreSessions is an error used to indicate that a channel
	// has updates in other non-closed sessions.
	errChannelHasMoreSessions = errors.New("channel has updates in " +
		"other sessions")
)

// NewBoltBackendCreator returns a function that creates a new bbolt backend for
// the watchtower database.
func NewBoltBackendCreator(active bool, dbPath,
	dbFileName string) func(boltCfg *kvdb.BoltConfig) (kvdb.Backend,
	error) {

	// If the watchtower client isn't active, we return a function that
	// always returns a nil DB to make sure we don't create empty database
	// files.
	if !active {
		return func(_ *kvdb.BoltConfig) (kvdb.Backend, error) {
			return nil, nil
		}
	}

	return func(boltCfg *kvdb.BoltConfig) (kvdb.Backend, error) {
		cfg := &kvdb.BoltBackendConfig{
			DBPath:            dbPath,
			DBFileName:        dbFileName,
			NoFreelistSync:    boltCfg.NoFreelistSync,
			AutoCompact:       boltCfg.AutoCompact,
			AutoCompactMinAge: boltCfg.AutoCompactMinAge,
			DBTimeout:         boltCfg.DBTimeout,
		}

		db, err := kvdb.GetBoltBackend(cfg)
		if err != nil {
			return nil, fmt.Errorf("could not open boltdb: %w", err)
		}

		return db, nil
	}
}

// ClientDB is single database providing a persistent storage engine for the
// wtclient.
type ClientDB struct {
	db kvdb.Backend

	// ackedRangeIndex is a map from session ID to channel ID to a
	// RangeIndex which represents the backups that have been acked for that
	// channel using that session.
	ackedRangeIndex   map[SessionID]map[lnwire.ChannelID]*RangeIndex
	ackedRangeIndexMu sync.Mutex
}

// OpenClientDB opens the client database given the path to the database's
// directory. If no such database exists, this method will initialize a fresh
// one using the latest version number and bucket structure. If a database
// exists but has a lower version number than the current version, any necessary
// migrations will be applied before returning. Any attempt to open a database
// with a version number higher that the latest version will fail to prevent
// accidental reversion.
func OpenClientDB(db kvdb.Backend) (*ClientDB, error) {
	firstInit, err := isFirstInit(db)
	if err != nil {
		return nil, err
	}

	clientDB := &ClientDB{
		db: db,
		ackedRangeIndex: make(
			map[SessionID]map[lnwire.ChannelID]*RangeIndex,
		),
	}

	err = initOrSyncVersions(clientDB, firstInit, clientDBVersions)
	if err != nil {
		db.Close()
		return nil, err
	}

	// Now that the database version fully consistent with our latest known
	// version, ensure that all top-level buckets known to this version are
	// initialized. This allows us to assume their presence throughout all
	// operations. If an known top-level bucket is expected to exist but is
	// missing, this will trigger a ErrUninitializedDB error.
	err = kvdb.Update(clientDB.db, initClientDBBuckets, func() {})
	if err != nil {
		db.Close()
		return nil, err
	}

	return clientDB, nil
}

// initClientDBBuckets creates all top-level buckets required to handle database
// operations required by the latest version.
func initClientDBBuckets(tx kvdb.RwTx) error {
	buckets := [][]byte{
		cSessionKeyIndexBkt,
		cChanDetailsBkt,
		cSessionBkt,
		cTowerBkt,
		cTowerIndexBkt,
		cTowerToSessionIndexBkt,
		cChanIDIndexBkt,
		cSessionIDIndexBkt,
		cClosableSessionsBkt,
	}

	for _, bucket := range buckets {
		_, err := tx.CreateTopLevelBucket(bucket)
		if err != nil {
			return err
		}
	}

	return nil
}

// bdb returns the backing bbolt.DB instance.
//
// NOTE: Part of the versionedDB interface.
func (c *ClientDB) bdb() kvdb.Backend {
	return c.db
}

// Version returns the database's current version number.
//
// NOTE: Part of the versionedDB interface.
func (c *ClientDB) Version() (uint32, error) {
	var version uint32
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		var err error
		version, err = getDBVersion(tx)
		return err
	}, func() {
		version = 0
	})
	if err != nil {
		return 0, err
	}

	return version, nil
}

// Close closes the underlying database.
func (c *ClientDB) Close() error {
	return c.db.Close()
}

// CreateTower initialize an address record used to communicate with a
// watchtower. Each Tower is assigned a unique ID, that is used to amortize
// storage costs of the public key when used by multiple sessions. If the tower
// already exists, the address is appended to the list of all addresses used to
// that tower previously and its corresponding sessions are marked as active.
func (c *ClientDB) CreateTower(lnAddr *lnwire.NetAddress) (*Tower, error) {
	var towerPubKey [33]byte
	copy(towerPubKey[:], lnAddr.IdentityKey.SerializeCompressed())

	var tower *Tower
	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		towerIndex := tx.ReadWriteBucket(cTowerIndexBkt)
		if towerIndex == nil {
			return ErrUninitializedDB
		}

		towers := tx.ReadWriteBucket(cTowerBkt)
		if towers == nil {
			return ErrUninitializedDB
		}

		towerToSessionIndex := tx.ReadWriteBucket(
			cTowerToSessionIndexBkt,
		)
		if towerToSessionIndex == nil {
			return ErrUninitializedDB
		}

		// Check if the tower index already knows of this pubkey.
		towerIDBytes := towerIndex.Get(towerPubKey[:])
		if len(towerIDBytes) == 8 {
			// The tower already exists, deserialize the existing
			// record.
			var err error
			tower, err = getTower(towers, towerIDBytes)
			if err != nil {
				return err
			}

			// Set its status to active.
			tower.Status = TowerStatusActive

			// Add the new address to the existing tower. If the
			// address is a duplicate, this will result in no
			// change.
			tower.AddAddress(lnAddr.Address)
		} else {
			// No such tower exists, create a new tower id for our
			// new tower. The error is unhandled since NextSequence
			// never fails in an Update.
			towerID, _ := towerIndex.NextSequence()

			tower = &Tower{
				ID:          TowerID(towerID),
				IdentityKey: lnAddr.IdentityKey,
				Addresses:   []net.Addr{lnAddr.Address},
				Status:      TowerStatusActive,
			}

			towerIDBytes = tower.ID.Bytes()

			// Since this tower is new, record the mapping from
			// tower pubkey to tower id in the tower index.
			err := towerIndex.Put(towerPubKey[:], towerIDBytes)
			if err != nil {
				return err
			}

			// Create a new bucket for this tower in the
			// tower-to-sessions index.
			_, err = towerToSessionIndex.CreateBucket(towerIDBytes)
			if err != nil {
				return err
			}
		}

		// Store the new or updated tower under its tower id.
		return putTower(towers, tower)
	}, func() {
		tower = nil
	})
	if err != nil {
		return nil, err
	}

	return tower, nil
}

// RemoveTower modifies a tower's record within the database. If an address is
// provided, then _only_ the address record should be removed from the tower's
// persisted state. Otherwise, we'll attempt to mark the tower as inactive. If
// any of its sessions has unacked updates, then ErrTowerUnackedUpdates is
// returned. If the tower doesn't have any sessions at all, it'll be completely
// removed from the database.
//
// NOTE: An error is not returned if the tower doesn't exist.
func (c *ClientDB) RemoveTower(pubKey *btcec.PublicKey, addr net.Addr) error {
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		towers := tx.ReadWriteBucket(cTowerBkt)
		if towers == nil {
			return ErrUninitializedDB
		}

		towerIndex := tx.ReadWriteBucket(cTowerIndexBkt)
		if towerIndex == nil {
			return ErrUninitializedDB
		}

		towersToSessionsIndex := tx.ReadWriteBucket(
			cTowerToSessionIndexBkt,
		)
		if towersToSessionsIndex == nil {
			return ErrUninitializedDB
		}

		chanIDIndexBkt := tx.ReadBucket(cChanIDIndexBkt)
		if chanIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		// Don't return an error if the watchtower doesn't exist to act
		// as a NOP.
		pubKeyBytes := pubKey.SerializeCompressed()
		towerIDBytes := towerIndex.Get(pubKeyBytes)
		if towerIDBytes == nil {
			return nil
		}

		tower, err := getTower(towers, towerIDBytes)
		if err != nil {
			return err
		}

		// If an address is provided, then we should _only_ remove the
		// address record from the database.
		if addr != nil {
			// Towers should always have at least one address saved.
			tower.RemoveAddress(addr)
			if len(tower.Addresses) == 0 {
				return ErrLastTowerAddr
			}

			return putTower(towers, tower)
		}

		// Otherwise, we should attempt to mark the tower's sessions as
		// inactive.
		sessions := tx.ReadWriteBucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}
		towerID := TowerIDFromBytes(towerIDBytes)

		committedUpdateCount := make(map[SessionID]uint16)
		perCommittedUpdate := func(s *ClientSession,
			_ *CommittedUpdate) {

			committedUpdateCount[s.ID]++
		}

		towerSessions, err := c.listTowerSessions(
			towerID, sessions, chanIDIndexBkt,
			towersToSessionsIndex,
			WithPerCommittedUpdate(perCommittedUpdate),
		)
		if err != nil {
			return err
		}

		// If it doesn't have any, we can completely remove it from the
		// database.
		if len(towerSessions) == 0 {
			if err := towerIndex.Delete(pubKeyBytes); err != nil {
				return err
			}

			if err := towers.Delete(towerIDBytes); err != nil {
				return err
			}

			return towersToSessionsIndex.DeleteNestedBucket(
				towerIDBytes,
			)
		}

		// Otherwise, we mark the tower as inactive.
		tower.Status = TowerStatusInactive
		err = putTower(towers, tower)
		if err != nil {
			return err
		}

		// We'll do a check to ensure that the tower's sessions don't
		// have any pending back-ups.
		for _, session := range towerSessions {
			if committedUpdateCount[session.ID] > 0 {
				return ErrTowerUnackedUpdates
			}
		}

		return nil
	}, func() {})
}

// DeactivateTower sets the given tower's status to inactive. This means that
// this tower's sessions won't be loaded and used for backups. CreateTower can
// be used to reactivate the tower again.
func (c *ClientDB) DeactivateTower(pubKey *btcec.PublicKey) error {
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		towers := tx.ReadWriteBucket(cTowerBkt)
		if towers == nil {
			return ErrUninitializedDB
		}

		towerIndex := tx.ReadWriteBucket(cTowerIndexBkt)
		if towerIndex == nil {
			return ErrUninitializedDB
		}

		towersToSessionsIndex := tx.ReadWriteBucket(
			cTowerToSessionIndexBkt,
		)
		if towersToSessionsIndex == nil {
			return ErrUninitializedDB
		}

		chanIDIndexBkt := tx.ReadBucket(cChanIDIndexBkt)
		if chanIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		pubKeyBytes := pubKey.SerializeCompressed()
		towerIDBytes := towerIndex.Get(pubKeyBytes)
		if towerIDBytes == nil {
			return ErrTowerNotFound
		}

		tower, err := getTower(towers, towerIDBytes)
		if err != nil {
			return err
		}

		// If the tower already has the desired status, then we can exit
		// here.
		if tower.Status == TowerStatusInactive {
			return nil
		}

		// Otherwise, we update the status and re-store the tower.
		tower.Status = TowerStatusInactive

		return putTower(towers, tower)
	}, func() {})
}

// LoadTowerByID retrieves a tower by its tower ID.
func (c *ClientDB) LoadTowerByID(towerID TowerID) (*Tower, error) {
	var tower *Tower
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		towers := tx.ReadBucket(cTowerBkt)
		if towers == nil {
			return ErrUninitializedDB
		}

		var err error
		tower, err = getTower(towers, towerID.Bytes())
		return err
	}, func() {
		tower = nil
	})
	if err != nil {
		return nil, err
	}

	return tower, nil
}

// LoadTower retrieves a tower by its public key.
func (c *ClientDB) LoadTower(pubKey *btcec.PublicKey) (*Tower, error) {
	var tower *Tower
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		towers := tx.ReadBucket(cTowerBkt)
		if towers == nil {
			return ErrUninitializedDB
		}
		towerIndex := tx.ReadBucket(cTowerIndexBkt)
		if towerIndex == nil {
			return ErrUninitializedDB
		}

		towerIDBytes := towerIndex.Get(pubKey.SerializeCompressed())
		if towerIDBytes == nil {
			return ErrTowerNotFound
		}

		var err error
		tower, err = getTower(towers, towerIDBytes)
		return err
	}, func() {
		tower = nil
	})
	if err != nil {
		return nil, err
	}

	return tower, nil
}

// TowerFilterFn is the signature of a call-back function that can be used to
// skip certain towers in the ListTowers method.
type TowerFilterFn func(*Tower) bool

// ListTowers retrieves the list of towers available within the database that
// have a status matching the given status. The filter function may be set in
// order to filter out the towers to be returned.
func (c *ClientDB) ListTowers(filter TowerFilterFn) ([]*Tower, error) {
	var towers []*Tower
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		towerBucket := tx.ReadBucket(cTowerBkt)
		if towerBucket == nil {
			return ErrUninitializedDB
		}

		return towerBucket.ForEach(func(towerIDBytes, _ []byte) error {
			tower, err := getTower(towerBucket, towerIDBytes)
			if err != nil {
				return err
			}

			if filter != nil && !filter(tower) {
				return nil
			}

			towers = append(towers, tower)

			return nil
		})
	}, func() {
		towers = nil
	})
	if err != nil {
		return nil, err
	}

	return towers, nil
}

// NextSessionKeyIndex reserves a new session key derivation index for a
// particular tower id. The index is reserved for that tower until
// CreateClientSession is invoked for that tower and index, at which point a new
// index for that tower can be reserved. Multiple calls to this method before
// CreateClientSession is invoked should return the same index unless forceNext
// is true.
func (c *ClientDB) NextSessionKeyIndex(towerID TowerID,
	blobType blob.Type, forceNext bool) (uint32, error) {

	var index uint32
	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		keyIndex := tx.ReadWriteBucket(cSessionKeyIndexBkt)
		if keyIndex == nil {
			return ErrUninitializedDB
		}

		var err error
		if !forceNext {
			// Check the session key index to see if a key has
			// already been reserved for this tower. If so, we'll
			// deserialize and return the index directly.
			index, err = getSessionKeyIndex(
				keyIndex, towerID, blobType,
			)
			if err == nil {
				return nil
			}
		}

		// By default, we use the next available bucket sequence as the
		// key index. But if forceNext is true, then it is assumed that
		// some data loss occurred and so the sequence is incremented a
		// by a jump of 1000 so that we can arrive at a brand new key
		// index quicker.
		currentSequence := keyIndex.Sequence()
		nextIndex := currentSequence + 1
		if forceNext {
			nextIndex = currentSequence + 1000
		}

		if err = keyIndex.SetSequence(nextIndex); err != nil {
			return fmt.Errorf("could not set next bucket "+
				"sequence: %w", err)
		}

		// As a sanity check, assert that the index is still in the
		// valid range of unhardened pubkeys. In the future, we should
		// move to only using hardened keys, and this will prevent any
		// overlap from occurring until then. This also prevents us from
		// overflowing uint32s.
		if nextIndex > math.MaxInt32 {
			return fmt.Errorf("exhausted session key indexes")
		}

		// Create the key that will used to be store the reserved index.
		keyBytes := createSessionKeyIndexKey(towerID, blobType)

		index = uint32(nextIndex)

		var indexBuf [4]byte
		byteOrder.PutUint32(indexBuf[:], index)

		// Record the reserved session key index under this tower's id.
		return keyIndex.Put(keyBytes, indexBuf[:])
	}, func() {
		index = 0
	})
	if err != nil {
		return 0, err
	}

	return index, nil
}

// CreateClientSession records a newly negotiated client session in the set of
// active sessions. The session can be identified by its SessionID.
func (c *ClientDB) CreateClientSession(session *ClientSession) error {
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		keyIndexes := tx.ReadWriteBucket(cSessionKeyIndexBkt)
		if keyIndexes == nil {
			return ErrUninitializedDB
		}

		sessions := tx.ReadWriteBucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		towers := tx.ReadBucket(cTowerBkt)
		if towers == nil {
			return ErrUninitializedDB
		}

		towerToSessionIndex := tx.ReadWriteBucket(
			cTowerToSessionIndexBkt,
		)
		if towerToSessionIndex == nil {
			return ErrUninitializedDB
		}

		// Check that  client session with this session id doesn't
		// already exist.
		existingSessionBytes := sessions.NestedReadWriteBucket(
			session.ID[:],
		)
		if existingSessionBytes != nil {
			return ErrClientSessionAlreadyExists
		}

		// Ensure that a tower with the given ID actually exists in the
		// DB.
		towerID := session.TowerID
		if _, err := getTower(towers, towerID.Bytes()); err != nil {
			return err
		}

		blobType := session.Policy.BlobType

		// Check that this tower has a reserved key index.
		index, err := getSessionKeyIndex(keyIndexes, towerID, blobType)
		if err != nil {
			return err
		}

		// Assert that the key index of the inserted session matches the
		// reserved session key index.
		if index != session.KeyIndex {
			return ErrIncorrectKeyIndex
		}

		// Remove the key index reservation. For altruist commit
		// sessions, we'll also purge under the old legacy key format.
		key := createSessionKeyIndexKey(towerID, blobType)
		err = keyIndexes.Delete(key)
		if err != nil {
			return err
		}
		if blobType == blob.TypeAltruistCommit {
			err = keyIndexes.Delete(towerID.Bytes())
			if err != nil {
				return err
			}
		}

		// Get the session-ID index bucket.
		dbIDIndex := tx.ReadWriteBucket(cSessionIDIndexBkt)
		if dbIDIndex == nil {
			return ErrUninitializedDB
		}

		// Get a new, unique, ID for this session from the session-ID
		// index bucket.
		nextSeq, err := dbIDIndex.NextSequence()
		if err != nil {
			return err
		}

		// Add the new entry to the dbID-to-SessionID index.
		newIndex, err := writeBigSize(nextSeq)
		if err != nil {
			return err
		}

		err = dbIDIndex.Put(newIndex, session.ID[:])
		if err != nil {
			return err
		}

		// Also add the db-assigned-id to the session bucket under the
		// cSessionDBID key.
		sessionBkt, err := sessions.CreateBucket(session.ID[:])
		if err != nil {
			return err
		}

		err = sessionBkt.Put(cSessionDBID, newIndex)
		if err != nil {
			return err
		}

		// TODO(elle): migrate the towerID-to-SessionID to use the
		// new db-assigned sessionID's rather.

		// Add the new entry to the towerID-to-SessionID index.
		towerSessions := towerToSessionIndex.NestedReadWriteBucket(
			towerID.Bytes(),
		)
		if towerSessions == nil {
			return ErrTowerNotFound
		}

		err = towerSessions.Put(session.ID[:], []byte{1})
		if err != nil {
			return err
		}

		// Finally, write the client session's body in the sessions
		// bucket.
		return putClientSessionBody(sessionBkt, session)
	}, func() {})
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

// getRangeIndex checks the ClientDB's in-memory range index map to see if it
// has an entry for the given session and channel ID. If it does, this is
// returned, otherwise the range index is loaded from the DB. An optional db
// transaction parameter may be provided. If one is provided then it will be
// used to query the DB for the range index, otherwise, a new transaction will
// be created and used.
func (c *ClientDB) getRangeIndex(tx kvdb.RTx, sID SessionID,
	chanID lnwire.ChannelID) (*RangeIndex, error) {

	c.ackedRangeIndexMu.Lock()
	defer c.ackedRangeIndexMu.Unlock()

	if _, ok := c.ackedRangeIndex[sID]; !ok {
		c.ackedRangeIndex[sID] = make(map[lnwire.ChannelID]*RangeIndex)
	}

	// If the in-memory range-index map already includes an entry for this
	// session ID and channel ID pair, then return it.
	if index, ok := c.ackedRangeIndex[sID][chanID]; ok {
		return index, nil
	}

	// readRangeIndexFromBkt is a helper that is used to read in a
	// RangeIndex structure from the passed in bucket and store it in the
	// ackedRangeIndex map.
	readRangeIndexFromBkt := func(rangesBkt kvdb.RBucket) (*RangeIndex,
		error) {

		// Create a new in-memory RangeIndex by reading in ranges from
		// the DB.
		rangeIndex, err := readRangeIndex(rangesBkt)
		if err != nil {
			return nil, err
		}

		c.ackedRangeIndex[sID][chanID] = rangeIndex

		return rangeIndex, nil
	}

	// If a DB transaction is provided then use it to fetch the ranges
	// bucket from the DB.
	if tx != nil {
		rangesBkt, err := getRangesReadBucket(tx, sID, chanID)
		if err != nil {
			return nil, err
		}

		return readRangeIndexFromBkt(rangesBkt)
	}

	// No DB transaction was provided. So create and use a new one.
	var index *RangeIndex
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		rangesBkt, err := getRangesReadBucket(tx, sID, chanID)
		if err != nil {
			return err
		}

		index, err = readRangeIndexFromBkt(rangesBkt)

		return err
	}, func() {})
	if err != nil {
		return nil, err
	}

	return index, nil
}

// getRangesReadBucket gets the range index bucket where the range index for the
// given session-channel pair is stored. If any sub-buckets along the way do not
// exist, then an error is returned. If the sub-buckets should be created
// instead, then use getRangesWriteBucket.
func getRangesReadBucket(tx kvdb.RTx, sID SessionID, chanID lnwire.ChannelID) (
	kvdb.RBucket, error) {

	sessions := tx.ReadBucket(cSessionBkt)
	if sessions == nil {
		return nil, ErrUninitializedDB
	}

	chanDetailsBkt := tx.ReadBucket(cChanDetailsBkt)
	if chanDetailsBkt == nil {
		return nil, ErrUninitializedDB
	}

	sessionBkt := sessions.NestedReadBucket(sID[:])
	if sessionsBkt == nil {
		return nil, ErrNoRangeIndexFound
	}

	// Get the DB representation of the channel-ID.
	_, dbChanIDBytes, err := getDBChanID(chanDetailsBkt, chanID)
	if err != nil {
		return nil, err
	}

	sessionAckRanges := sessionBkt.NestedReadBucket(cSessionAckRangeIndex)
	if sessionAckRanges == nil {
		return nil, ErrNoRangeIndexFound
	}

	return sessionAckRanges.NestedReadBucket(dbChanIDBytes), nil
}

// getRangesWriteBucket gets the range index bucket where the range index for
// the given session-channel pair is stored. If any sub-buckets along the way do
// not exist, then they are created.
func getRangesWriteBucket(sessionBkt kvdb.RwBucket, dbChanIDBytes []byte) (
	kvdb.RwBucket, error) {

	sessionAckRanges, err := sessionBkt.CreateBucketIfNotExists(
		cSessionAckRangeIndex,
	)
	if err != nil {
		return nil, err
	}

	return sessionAckRanges.CreateBucketIfNotExists(dbChanIDBytes)
}

// createSessionKeyIndexKey returns the identifier used in the
// session-key-index index, created as tower-id||blob-type.
//
// NOTE: The original serialization only used tower-id, which prevents
// concurrent client types from reserving sessions with the same tower.
func createSessionKeyIndexKey(towerID TowerID, blobType blob.Type) []byte {
	towerIDBytes := towerID.Bytes()

	// Session key indexes are stored under as tower-id||blob-type.
	var keyBytes [6]byte
	copy(keyBytes[:4], towerIDBytes)
	byteOrder.PutUint16(keyBytes[4:], uint16(blobType))

	return keyBytes[:]
}

// getSessionKeyIndex is a helper method.
func getSessionKeyIndex(keyIndexes kvdb.RwBucket, towerID TowerID,
	blobType blob.Type) (uint32, error) {

	// Session key indexes are store under as tower-id||blob-type. The
	// original serialization only used tower-id, which prevents concurrent
	// client types from reserving sessions with the same tower.
	keyBytes := createSessionKeyIndexKey(towerID, blobType)

	// Retrieve the index using the key bytes. If the key wasn't found, we
	// will fall back to the legacy format that only uses the tower id, but
	// _only_ if the blob type is for altruist commit sessions since that
	// was the only operational session type prior to changing the key
	// format.
	keyIndexBytes := keyIndexes.Get(keyBytes)
	if keyIndexBytes == nil && blobType == blob.TypeAltruistCommit {
		keyIndexBytes = keyIndexes.Get(towerID.Bytes())
	}

	// All session key indexes should be serialized uint32's. If no key
	// index was found, the length of keyIndexBytes will be 0.
	if len(keyIndexBytes) != 4 {
		return 0, ErrNoReservedKeyIndex
	}

	return byteOrder.Uint32(keyIndexBytes), nil
}

// GetClientSession loads the ClientSession with the given ID from the DB.
func (c *ClientDB) GetClientSession(id SessionID,
	opts ...ClientSessionListOption) (*ClientSession, error) {

	var sess *ClientSession
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		sessionsBkt := tx.ReadBucket(cSessionBkt)
		if sessionsBkt == nil {
			return ErrUninitializedDB
		}

		chanIDIndexBkt := tx.ReadBucket(cChanIDIndexBkt)
		if chanIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		session, err := c.getClientSession(
			sessionsBkt, chanIDIndexBkt, id[:], opts...,
		)
		if err != nil {
			return err
		}

		sess = session

		return nil
	}, func() {})

	return sess, err
}

// ListClientSessions returns the set of all client sessions known to the db. An
// optional tower ID can be used to filter out any client sessions in the
// response that do not correspond to this tower.
func (c *ClientDB) ListClientSessions(id *TowerID,
	opts ...ClientSessionListOption) (map[SessionID]*ClientSession, error) {

	var clientSessions map[SessionID]*ClientSession
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		sessions := tx.ReadBucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		chanIDIndexBkt := tx.ReadBucket(cChanIDIndexBkt)
		if chanIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		// If no tower ID is specified, then fetch all the sessions
		// known to the db.
		var err error
		if id == nil {
			clientSessions, err = c.listClientAllSessions(
				sessions, chanIDIndexBkt, opts...,
			)
			return err
		}

		// Otherwise, fetch the sessions for the given tower.
		towerToSessionIndex := tx.ReadBucket(cTowerToSessionIndexBkt)
		if towerToSessionIndex == nil {
			return ErrUninitializedDB
		}

		clientSessions, err = c.listTowerSessions(
			*id, sessions, chanIDIndexBkt, towerToSessionIndex,
			opts...,
		)
		return err
	}, func() {
		clientSessions = nil
	})
	if err != nil {
		return nil, err
	}

	return clientSessions, nil
}

// listClientAllSessions returns the set of all client sessions known to the db.
func (c *ClientDB) listClientAllSessions(sessions, chanIDIndexBkt kvdb.RBucket,
	opts ...ClientSessionListOption) (map[SessionID]*ClientSession, error) {

	clientSessions := make(map[SessionID]*ClientSession)
	err := sessions.ForEach(func(k, _ []byte) error {
		// We'll load the full client session since the client will need
		// the CommittedUpdates and AckedUpdates on startup to resume
		// committed updates and compute the highest known commit height
		// for each channel.
		session, err := c.getClientSession(
			sessions, chanIDIndexBkt, k, opts...,
		)
		if errors.Is(err, ErrSessionFailedFilterFn) {
			return nil
		} else if err != nil {
			return err
		}

		clientSessions[session.ID] = session

		return nil
	})
	if err != nil {
		return nil, err
	}

	return clientSessions, nil
}

// listTowerSessions returns the set of all client sessions known to the db
// that are associated with the given tower id.
func (c *ClientDB) listTowerSessions(id TowerID, sessionsBkt, chanIDIndexBkt,
	towerToSessionIndex kvdb.RBucket, opts ...ClientSessionListOption) (
	map[SessionID]*ClientSession, error) {

	towerIndexBkt := towerToSessionIndex.NestedReadBucket(id.Bytes())
	if towerIndexBkt == nil {
		return nil, ErrTowerNotFound
	}

	clientSessions := make(map[SessionID]*ClientSession)
	err := towerIndexBkt.ForEach(func(k, _ []byte) error {
		// We'll load the full client session since the client will need
		// the CommittedUpdates and AckedUpdates on startup to resume
		// committed updates and compute the highest known commit height
		// for each channel.
		session, err := c.getClientSession(
			sessionsBkt, chanIDIndexBkt, k, opts...,
		)
		if errors.Is(err, ErrSessionFailedFilterFn) {
			return nil
		} else if err != nil {
			return err
		}

		clientSessions[session.ID] = session
		return nil
	})
	if err != nil {
		return nil, err
	}

	return clientSessions, nil
}

// FetchSessionCommittedUpdates retrieves the current set of un-acked updates
// of the given session.
func (c *ClientDB) FetchSessionCommittedUpdates(id *SessionID) (
	[]CommittedUpdate, error) {

	var committedUpdates []CommittedUpdate
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		sessions := tx.ReadBucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		sessionBkt := sessions.NestedReadBucket(id[:])
		if sessionBkt == nil {
			return ErrClientSessionNotFound
		}

		var err error
		committedUpdates, err = getClientSessionCommits(
			sessionBkt, nil, nil,
		)
		return err
	}, func() {})
	if err != nil {
		return nil, err
	}

	return committedUpdates, nil
}

// IsAcked returns true if the given backup has been backed up using the given
// session.
func (c *ClientDB) IsAcked(id *SessionID, backupID *BackupID) (bool, error) {
	index, err := c.getRangeIndex(nil, *id, backupID.ChanID)
	if errors.Is(err, ErrNoRangeIndexFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return index.IsInIndex(backupID.CommitHeight), nil
}

// NumAckedUpdates returns the number of backups that have been successfully
// backed up using the given session.
func (c *ClientDB) NumAckedUpdates(id *SessionID) (uint64, error) {
	var numAcked uint64
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		sessions := tx.ReadBucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		chanIDIndexBkt := tx.ReadBucket(cChanIDIndexBkt)
		if chanIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		sessionBkt := sessions.NestedReadBucket(id[:])
		if sessionBkt == nil {
			return nil
		}

		// First, account for any rogue updates.
		rogueCountBytes := sessionBkt.Get(cSessionRogueUpdateCount)
		if len(rogueCountBytes) != 0 {
			rogueCount, err := readBigSize(rogueCountBytes)
			if err != nil {
				return err
			}

			numAcked += rogueCount
		}

		// Then, check if the session-ack-ranges contains any entries
		// to account for.
		sessionAckRanges := sessionBkt.NestedReadBucket(
			cSessionAckRangeIndex,
		)
		if sessionAckRanges == nil {
			return nil
		}

		// Iterate over the channel ID's in the sessionAckRanges
		// bucket.
		return sessionAckRanges.ForEach(func(dbChanID, _ []byte) error {
			// Get the range index for the session-channel pair.
			chanIDBytes := chanIDIndexBkt.Get(dbChanID)
			var chanID lnwire.ChannelID
			copy(chanID[:], chanIDBytes)

			index, err := c.getRangeIndex(tx, *id, chanID)
			if err != nil {
				return err
			}

			numAcked += index.NumInSet()

			return nil
		})
	}, func() {
		numAcked = 0
	})
	if err != nil {
		return 0, err
	}

	return numAcked, nil
}

// FetchChanInfos loads a mapping from all registered channels to their
// ChannelInfo. Only the channels that have not yet been marked as closed will
// be loaded.
func (c *ClientDB) FetchChanInfos() (ChannelInfos, error) {
	var infos ChannelInfos

	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		chanDetailsBkt := tx.ReadBucket(cChanDetailsBkt)
		if chanDetailsBkt == nil {
			return ErrUninitializedDB
		}

		return chanDetailsBkt.ForEach(func(k, _ []byte) error {
			chanDetails := chanDetailsBkt.NestedReadBucket(k)
			if chanDetails == nil {
				return ErrCorruptChanDetails
			}
			// If this channel has already been marked as closed,
			// then its summary does not need to be loaded.
			closedHeight := chanDetails.Get(cChanClosedHeight)
			if len(closedHeight) > 0 {
				return nil
			}
			var chanID lnwire.ChannelID
			copy(chanID[:], k)
			summary, err := getChanSummary(chanDetails)
			if err != nil {
				return err
			}

			info := &ChannelInfo{
				ClientChanSummary: *summary,
			}

			maxHeightBytes := chanDetails.Get(
				cChanMaxCommitmentHeight,
			)
			if len(maxHeightBytes) != 0 {
				height, err := readBigSize(maxHeightBytes)
				if err != nil {
					return err
				}

				info.MaxHeight = fn.Some(height)
			}

			infos[chanID] = info

			return nil
		})
	}, func() {
		infos = make(ChannelInfos)
	})
	if err != nil {
		return nil, err
	}

	return infos, nil
}

// RegisterChannel registers a channel for use within the client database. For
// now, all that is stored in the channel summary is the sweep pkscript that
// we'd like any tower sweeps to pay into. In the future, this will be extended
// to contain more info to allow the client efficiently request historical
// states to be backed up under the client's active policy.
func (c *ClientDB) RegisterChannel(chanID lnwire.ChannelID,
	sweepPkScript []byte) error {

	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		chanDetailsBkt := tx.ReadWriteBucket(cChanDetailsBkt)
		if chanDetailsBkt == nil {
			return ErrUninitializedDB
		}

		chanDetails := chanDetailsBkt.NestedReadWriteBucket(chanID[:])
		if chanDetails != nil {
			// Channel is already registered.
			return ErrChannelAlreadyRegistered
		}

		chanDetails, err := chanDetailsBkt.CreateBucket(chanID[:])
		if err != nil {
			return err
		}

		// Get the channel-id-index bucket.
		indexBkt := tx.ReadWriteBucket(cChanIDIndexBkt)
		if indexBkt == nil {
			return ErrUninitializedDB
		}

		// Request the next unique id from the bucket.
		nextSeq, err := indexBkt.NextSequence()
		if err != nil {
			return err
		}

		// Use BigSize encoding to encode the db-assigned index.
		newIndex, err := writeBigSize(nextSeq)
		if err != nil {
			return err
		}

		// Add the new db-assigned ID to channel-ID pair.
		err = indexBkt.Put(newIndex, chanID[:])
		if err != nil {
			return err
		}

		// Add the db-assigned ID to the channel's channel details
		// bucket under the cChanDBID key.
		err = chanDetails.Put(cChanDBID, newIndex)
		if err != nil {
			return err
		}

		summary := ClientChanSummary{
			SweepPkScript: sweepPkScript,
		}

		return putChanSummary(chanDetails, &summary)
	}, func() {})
}

// MarkBackupIneligible records that the state identified by the (channel id,
// commit height) tuple was ineligible for being backed up under the current
// policy. This state can be retried later under a different policy.
func (c *ClientDB) MarkBackupIneligible(chanID lnwire.ChannelID,
	commitHeight uint64) error {

	return nil
}

// ListClosableSessions fetches and returns the IDs for all sessions marked as
// closable.
func (c *ClientDB) ListClosableSessions() (map[SessionID]uint32, error) {
	sessions := make(map[SessionID]uint32)
	err := kvdb.View(c.db, func(tx kvdb.RTx) error {
		csBkt := tx.ReadBucket(cClosableSessionsBkt)
		if csBkt == nil {
			return ErrUninitializedDB
		}

		sessIDIndexBkt := tx.ReadBucket(cSessionIDIndexBkt)
		if sessIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		return csBkt.ForEach(func(dbIDBytes, heightBytes []byte) error {
			dbID, err := readBigSize(dbIDBytes)
			if err != nil {
				return err
			}

			sessID, err := getRealSessionID(sessIDIndexBkt, dbID)
			if err != nil {
				return err
			}

			sessions[*sessID] = byteOrder.Uint32(heightBytes)

			return nil
		})
	}, func() {
		sessions = make(map[SessionID]uint32)
	})
	if err != nil {
		return nil, err
	}

	return sessions, nil
}

// DeleteSession can be called when a session should be deleted from the DB.
// All references to the session will also be deleted from the DB. Note that a
// session will only be deleted if was previously marked as closable.
func (c *ClientDB) DeleteSession(id SessionID) error {
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		sessionsBkt := tx.ReadWriteBucket(cSessionBkt)
		if sessionsBkt == nil {
			return ErrUninitializedDB
		}

		closableBkt := tx.ReadWriteBucket(cClosableSessionsBkt)
		if closableBkt == nil {
			return ErrUninitializedDB
		}

		chanDetailsBkt := tx.ReadWriteBucket(cChanDetailsBkt)
		if chanDetailsBkt == nil {
			return ErrUninitializedDB
		}

		sessIDIndexBkt := tx.ReadWriteBucket(cSessionIDIndexBkt)
		if sessIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		chanIDIndexBkt := tx.ReadWriteBucket(cChanIDIndexBkt)
		if chanIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		towerToSessBkt := tx.ReadWriteBucket(cTowerToSessionIndexBkt)
		if towerToSessBkt == nil {
			return ErrUninitializedDB
		}

		// Get the sub-bucket for this session ID. If it does not exist
		// then the session has already been deleted and so our work is
		// done.
		sessionBkt := sessionsBkt.NestedReadBucket(id[:])
		if sessionBkt == nil {
			return nil
		}

		_, dbIDBytes, err := getDBSessionID(sessionsBkt, id)
		if err != nil {
			return err
		}

		// First we check if the session has actually been marked as
		// closable.
		if closableBkt.Get(dbIDBytes) == nil {
			return ErrSessionNotClosable
		}

		sess, err := getClientSessionBody(sessionsBkt, id[:])
		if err != nil {
			return err
		}

		// Delete from the tower-to-sessionID index.
		towerIndexBkt := towerToSessBkt.NestedReadWriteBucket(
			sess.TowerID.Bytes(),
		)
		if towerIndexBkt == nil {
			return fmt.Errorf("no entry in the tower-to-session "+
				"index found for tower ID %v", sess.TowerID)
		}

		err = towerIndexBkt.Delete(id[:])
		if err != nil {
			return err
		}

		// Delete entry from session ID index.
		err = sessIDIndexBkt.Delete(dbIDBytes)
		if err != nil {
			return err
		}

		// Delete the entry from the closable sessions index.
		err = closableBkt.Delete(dbIDBytes)
		if err != nil {
			return err
		}

		ackRanges := sessionBkt.NestedReadBucket(cSessionAckRangeIndex)

		// There is a small chance that the session only contains rogue
		// updates. In that case, there will be no ack-ranges index but
		// the rogue update count will be equal the MaxUpdates.
		rogueCountBytes := sessionBkt.Get(cSessionRogueUpdateCount)
		if len(rogueCountBytes) != 0 {
			rogueCount, err := readBigSize(rogueCountBytes)
			if err != nil {
				return err
			}

			maxUpdates := sess.ClientSessionBody.Policy.MaxUpdates
			if rogueCount == uint64(maxUpdates) {
				// Do a sanity check to ensure that the acked
				// ranges bucket does not exist in this case.
				if ackRanges != nil {
					return fmt.Errorf("acked updates "+
						"exist for session with a "+
						"max-updates(%d) rogue count",
						rogueCount)
				}

				return sessionsBkt.DeleteNestedBucket(id[:])
			}
		}

		// A session would only be considered closable if it was
		// exhausted. Meaning that it should not be the case that it has
		// no acked-updates.
		if ackRanges == nil {
			return fmt.Errorf("cannot delete session %s since it "+
				"is not yet exhausted", id)
		}

		// For each of the channels, delete the session ID entry.
		err = ackRanges.ForEach(func(chanDBID, _ []byte) error {
			chanDBIDInt, err := readBigSize(chanDBID)
			if err != nil {
				return err
			}

			chanID, err := getRealChannelID(
				chanIDIndexBkt, chanDBIDInt,
			)
			if err != nil {
				return err
			}

			chanDetails := chanDetailsBkt.NestedReadWriteBucket(
				chanID[:],
			)
			if chanDetails == nil {
				return ErrChannelNotRegistered
			}

			chanSessions := chanDetails.NestedReadWriteBucket(
				cChanSessions,
			)
			if chanSessions == nil {
				return fmt.Errorf("no session list found for "+
					"channel %s", chanID)
			}

			// Check that this session was actually listed in the
			// session list for this channel.
			if len(chanSessions.Get(dbIDBytes)) == 0 {
				return fmt.Errorf("session %s not found in "+
					"the session list for channel %s", id,
					chanID)
			}

			// If it was, then delete it.
			err = chanSessions.Delete(dbIDBytes)
			if err != nil {
				return err
			}

			// If this was the last session for this channel, we can
			// now delete the channel details for this channel
			// completely.
			err = chanSessions.ForEach(func(_, _ []byte) error {
				return errChannelHasMoreSessions
			})
			if errors.Is(err, errChannelHasMoreSessions) {
				return nil
			} else if err != nil {
				return err
			}

			// Delete the channel's entry from the channel-id-index.
			dbID := chanDetails.Get(cChanDBID)
			err = chanIDIndexBkt.Delete(dbID)
			if err != nil {
				return err
			}

			// Delete the channel details.
			return chanDetailsBkt.DeleteNestedBucket(chanID[:])
		})
		if err != nil {
			return err
		}

		// Delete the actual session.
		return sessionsBkt.DeleteNestedBucket(id[:])
	}, func() {})
}

// MarkChannelClosed will mark a registered channel as closed by setting its
// closed-height as the given block height. It returns a list of session IDs for
// sessions that are now considered closable due to the close of this channel.
// The details for this channel will be deleted from the DB if there are no more
// sessions in the DB that contain updates for this channel.
func (c *ClientDB) MarkChannelClosed(chanID lnwire.ChannelID,
	blockHeight uint32) ([]SessionID, error) {

	var closableSessions []SessionID
	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		sessionsBkt := tx.ReadBucket(cSessionBkt)
		if sessionsBkt == nil {
			return ErrUninitializedDB
		}

		chanDetailsBkt := tx.ReadWriteBucket(cChanDetailsBkt)
		if chanDetailsBkt == nil {
			return ErrUninitializedDB
		}

		closableSessBkt := tx.ReadWriteBucket(cClosableSessionsBkt)
		if closableSessBkt == nil {
			return ErrUninitializedDB
		}

		chanIDIndexBkt := tx.ReadBucket(cChanIDIndexBkt)
		if chanIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		sessIDIndexBkt := tx.ReadBucket(cSessionIDIndexBkt)
		if sessIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		chanDetails := chanDetailsBkt.NestedReadWriteBucket(chanID[:])
		if chanDetails == nil {
			return ErrChannelNotRegistered
		}

		// If there are no sessions for this channel, the channel
		// details can be deleted.
		chanSessIDsBkt := chanDetails.NestedReadBucket(cChanSessions)
		if chanSessIDsBkt == nil {
			return chanDetailsBkt.DeleteNestedBucket(chanID[:])
		}

		// Otherwise, mark the channel as closed.
		var height [4]byte
		byteOrder.PutUint32(height[:], blockHeight)

		err := chanDetails.Put(cChanClosedHeight, height[:])
		if err != nil {
			return err
		}

		// Now iterate through all the sessions of the channel to check
		// if any of them are closeable.
		return chanSessIDsBkt.ForEach(func(sessDBID, _ []byte) error {
			sessDBIDInt, err := readBigSize(sessDBID)
			if err != nil {
				return err
			}

			// Use the session-ID index to get the real session ID.
			sID, err := getRealSessionID(
				sessIDIndexBkt, sessDBIDInt,
			)
			if err != nil {
				return err
			}

			isClosable, err := isSessionClosable(
				sessionsBkt, chanDetailsBkt, chanIDIndexBkt,
				sID,
			)
			if err != nil {
				return err
			}

			if !isClosable {
				return nil
			}

			// Add session to "closableSessions" list and add the
			// block height that this last channel was closed in.
			// This will be used in future to determine when we
			// should delete the session.
			var height [4]byte
			byteOrder.PutUint32(height[:], blockHeight)
			err = closableSessBkt.Put(sessDBID, height[:])
			if err != nil {
				return err
			}

			closableSessions = append(closableSessions, *sID)

			return nil
		})
	}, func() {
		closableSessions = nil
	})
	if err != nil {
		return nil, err
	}

	return closableSessions, nil
}

// isSessionClosable returns true if a session is considered closable. A session
// is considered closable only if all the following points are true:
//  1. It has no un-acked updates.
//  2. It is exhausted (ie it can't accept any more updates) OR it has been
//     marked as terminal.
//  3. All the channels that it has acked updates for are closed.
func isSessionClosable(sessionsBkt, chanDetailsBkt, chanIDIndexBkt kvdb.RBucket,
	id *SessionID) (bool, error) {

	sessBkt := sessionsBkt.NestedReadBucket(id[:])
	if sessBkt == nil {
		return false, ErrSessionNotFound
	}

	// Since the DeleteCommittedUpdates method deletes the cSessionCommits
	// bucket in one go, it is possible for the session to be closable even
	// if this bucket no longer exists.
	commitsBkt := sessBkt.NestedReadBucket(cSessionCommits)
	if commitsBkt != nil {
		// If the session has any un-acked updates, then it is not yet
		// closable.
		err := commitsBkt.ForEach(func(_, _ []byte) error {
			return ErrSessionHasUnackedUpdates
		})
		if errors.Is(err, ErrSessionHasUnackedUpdates) {
			return false, nil
		} else if err != nil {
			return false, err
		}
	}

	session, err := getClientSessionBody(sessionsBkt, id[:])
	if err != nil {
		return false, err
	}

	isTerminal := session.Status == CSessionTerminal

	// We have already checked that the session has no more committed
	// updates. So now we can check if the session is exhausted or has a
	// terminal state.
	if !isTerminal && session.SeqNum < session.Policy.MaxUpdates {
		// If the session is not yet exhausted, and it is not yet in a
		// terminal state then it is not yet closable.
		return false, nil
	}

	// Either the acked-update bucket should exist _or_ the rogue update
	// count must be equal to the session's MaxUpdates value, otherwise
	// something is wrong because the above check ensures that the session
	// has been exhausted.
	rogueCountBytes := sessBkt.Get(cSessionRogueUpdateCount)
	if len(rogueCountBytes) != 0 {
		rogueCount, err := readBigSize(rogueCountBytes)
		if err != nil {
			return false, err
		}

		if rogueCount == uint64(session.Policy.MaxUpdates) {
			return true, nil
		}
	}

	ackedRangeBkt := sessBkt.NestedReadBucket(cSessionAckRangeIndex)
	if ackedRangeBkt == nil {
		if isTerminal {
			return true, nil
		}

		// If the session has no acked-updates, and it is not in a
		// terminal state then something is wrong since the above check
		// ensures that this session has been exhausted meaning that it
		// should have MaxUpdates acked updates.
		return false, fmt.Errorf("no acked-updates found for "+
			"exhausted session %s", id)
	}

	// Iterate over each of the channels that the session has acked-updates
	// for. If any of those channels are not closed, then the session is
	// not yet closable.
	err = ackedRangeBkt.ForEach(func(dbChanID, _ []byte) error {
		dbChanIDInt, err := readBigSize(dbChanID)
		if err != nil {
			return err
		}

		chanID, err := getRealChannelID(chanIDIndexBkt, dbChanIDInt)
		if err != nil {
			return err
		}

		// Get the channel details bucket for the channel.
		chanDetails := chanDetailsBkt.NestedReadBucket(chanID[:])
		if chanDetails == nil {
			return fmt.Errorf("no channel details found for "+
				"channel %s referenced by session %s", chanID,
				id)
		}

		// If a closed height has been set, then the channel is closed.
		closedHeight := chanDetails.Get(cChanClosedHeight)
		if len(closedHeight) > 0 {
			return nil
		}

		// Otherwise, the channel is not yet closed meaning that the
		// session is not yet closable. We break the ForEach by
		// returning an error to indicate this.
		return errSessionHasOpenChannels
	})
	if errors.Is(err, errSessionHasOpenChannels) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}

// CommitUpdate persists the CommittedUpdate provided in the slot for (session,
// seqNum). This allows the client to retransmit this update on startup.
func (c *ClientDB) CommitUpdate(id *SessionID,
	update *CommittedUpdate) (uint16, error) {

	var lastApplied uint16
	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		sessions := tx.ReadWriteBucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		// We'll only load the ClientSession body for performance, since
		// we primarily need to inspect its SeqNum and TowerLastApplied
		// fields. The CommittedUpdates will be modified on disk
		// directly.
		session, err := getClientSessionBody(sessions, id[:])
		if err != nil {
			return err
		}

		// Can't fail if the above didn't fail.
		sessionBkt := sessions.NestedReadWriteBucket(id[:])

		// Ensure the session commits sub-bucket is initialized.
		sessionCommits, err := sessionBkt.CreateBucketIfNotExists(
			cSessionCommits,
		)
		if err != nil {
			return err
		}

		var seqNumBuf [2]byte
		byteOrder.PutUint16(seqNumBuf[:], update.SeqNum)

		// Check to see if a committed update already exists for this
		// sequence number.
		committedUpdateBytes := sessionCommits.Get(seqNumBuf[:])
		if committedUpdateBytes != nil {
			var dbUpdate CommittedUpdate
			err := dbUpdate.Decode(
				bytes.NewReader(committedUpdateBytes),
			)
			if err != nil {
				return err
			}

			// If an existing committed update has a different hint,
			// we'll reject this newer update.
			if dbUpdate.Hint != update.Hint {
				return ErrUpdateAlreadyCommitted
			}

			// Otherwise, capture the last applied value and
			// succeed.
			lastApplied = session.TowerLastApplied
			return nil
		}

		// There's no committed update for this sequence number, ensure
		// that we are committing the next unallocated one.
		if update.SeqNum != session.SeqNum+1 {
			return ErrCommitUnorderedUpdate
		}

		// Increment the session's sequence number and store the updated
		// client session.
		//
		// TODO(conner): split out seqnum and last applied own bucket to
		// eliminate serialization of full struct during CommitUpdate?
		// Can also read/write directly to byes [:2] without migration.
		session.SeqNum++
		err = putClientSessionBody(sessionBkt, session)
		if err != nil {
			return err
		}

		// Encode and store the committed update in the sessionCommits
		// sub-bucket under the requested sequence number.
		var b bytes.Buffer
		err = update.Encode(&b)
		if err != nil {
			return err
		}

		err = sessionCommits.Put(seqNumBuf[:], b.Bytes())
		if err != nil {
			return err
		}

		// Update the channel's max commitment height if needed.
		err = maybeUpdateMaxCommitHeight(tx, update.BackupID)
		if err != nil {
			return err
		}

		// Finally, capture the session's last applied value so it can
		// be sent in the next state update to the tower.
		lastApplied = session.TowerLastApplied

		return nil
	}, func() {
		lastApplied = 0
	})
	if err != nil {
		return 0, err
	}

	return lastApplied, nil
}

// AckUpdate persists an acknowledgment for a given (session, seqnum) pair. This
// removes the update from the set of committed updates, and validates the
// lastApplied value returned from the tower.
func (c *ClientDB) AckUpdate(id *SessionID, seqNum uint16,
	lastApplied uint16) error {

	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		sessions := tx.ReadWriteBucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		chanDetailsBkt := tx.ReadWriteBucket(cChanDetailsBkt)
		if chanDetailsBkt == nil {
			return ErrUninitializedDB
		}

		// We'll only load the ClientSession body for performance, since
		// we primarily need to inspect its SeqNum and TowerLastApplied
		// fields. The CommittedUpdates and AckedUpdates will be
		// modified on disk directly.
		session, err := getClientSessionBody(sessions, id[:])
		if err != nil {
			return err
		}

		// If the tower has acked a sequence number beyond our highest
		// sequence number, fail.
		if lastApplied > session.SeqNum {
			return ErrUnallocatedLastApplied
		}

		// If the tower acked with a lower sequence number than it gave
		// us prior, fail.
		if lastApplied < session.TowerLastApplied {
			return ErrLastAppliedReversion
		}

		// TODO(conner): split out seqnum and last applied own bucket to
		// eliminate serialization of full struct during AckUpdate?  Can
		// also read/write directly to byes [2:4] without migration.
		session.TowerLastApplied = lastApplied

		// Can't fail because getClientSession succeeded.
		sessionBkt := sessions.NestedReadWriteBucket(id[:])

		// Write the client session with the updated last applied value.
		err = putClientSessionBody(sessionBkt, session)
		if err != nil {
			return err
		}

		// If the commits sub-bucket doesn't exist, there can't possibly
		// be a corresponding committed update to remove.
		sessionCommits := sessionBkt.NestedReadWriteBucket(
			cSessionCommits,
		)
		if sessionCommits == nil {
			return ErrCommittedUpdateNotFound
		}

		var seqNumBuf [2]byte
		byteOrder.PutUint16(seqNumBuf[:], seqNum)

		// Assert that a committed update exists for this sequence
		// number.
		committedUpdateBytes := sessionCommits.Get(seqNumBuf[:])
		if committedUpdateBytes == nil {
			return ErrCommittedUpdateNotFound
		}

		var committedUpdate CommittedUpdate
		err = committedUpdate.Decode(
			bytes.NewReader(committedUpdateBytes),
		)
		if err != nil {
			return err
		}

		// Remove the corresponding committed update.
		err = sessionCommits.Delete(seqNumBuf[:])
		if err != nil {
			return err
		}

		dbSessionID, dbSessIDBytes, err := getDBSessionID(sessions, *id)
		if err != nil {
			return err
		}

		chanID := committedUpdate.BackupID.ChanID
		height := committedUpdate.BackupID.CommitHeight

		// Get the DB representation of the channel-ID. There is a
		// chance that the channel corresponding to this update has been
		// closed and that the details for this channel no longer exist
		// in the tower client DB. In that case, we consider this a
		// rogue update and all we do is make sure to keep track of the
		// number of rogue updates for this session.
		_, dbChanIDBytes, err := getDBChanID(chanDetailsBkt, chanID)
		if errors.Is(err, ErrChannelNotRegistered) {
			var (
				count uint64
				err   error
			)

			rogueCountBytes := sessionBkt.Get(
				cSessionRogueUpdateCount,
			)
			if len(rogueCountBytes) != 0 {
				count, err = readBigSize(rogueCountBytes)
				if err != nil {
					return err
				}
			}

			rogueCount := count + 1
			countBytes, err := writeBigSize(rogueCount)
			if err != nil {
				return err
			}

			err = sessionBkt.Put(
				cSessionRogueUpdateCount, countBytes,
			)
			if err != nil {
				return err
			}

			// In the rare chance that this session only has rogue
			// updates, we check here if the count is equal to the
			// MaxUpdate of the session. If it is, then we mark the
			// session as closable.
			if rogueCount != uint64(session.Policy.MaxUpdates) {
				return nil
			}

			// Before we mark the session as closable, we do a
			// sanity check to ensure that this session has no
			// acked-update index.
			sessionAckRanges := sessionBkt.NestedReadBucket(
				cSessionAckRangeIndex,
			)
			if sessionAckRanges != nil {
				return fmt.Errorf("session(%s) has an "+
					"acked ranges index but has a rogue "+
					"count indicating saturation",
					session.ID)
			}

			closableSessBkt := tx.ReadWriteBucket(
				cClosableSessionsBkt,
			)
			if closableSessBkt == nil {
				return ErrUninitializedDB
			}

			var height [4]byte
			byteOrder.PutUint32(height[:], 0)

			return closableSessBkt.Put(dbSessIDBytes, height[:])
		} else if err != nil {
			return err
		}

		// Get the ranges write bucket before getting the range index to
		// ensure that the session acks sub-bucket is initialized, so
		// that we can insert an entry.
		rangesBkt, err := getRangesWriteBucket(
			sessionBkt, dbChanIDBytes,
		)
		if err != nil {
			return err
		}

		chanDetails := chanDetailsBkt.NestedReadWriteBucket(
			committedUpdate.BackupID.ChanID[:],
		)
		if chanDetails == nil {
			return ErrChannelNotRegistered
		}

		err = putChannelToSessionMapping(chanDetails, dbSessionID)
		if err != nil {
			return err
		}

		// Get the range index for the given session-channel pair.
		index, err := c.getRangeIndex(tx, *id, chanID)
		if err != nil {
			return err
		}

		return index.Add(height, rangesBkt)
	}, func() {})
}

// GetDBQueue returns a BackupID Queue instance under the given namespace.
func (c *ClientDB) GetDBQueue(namespace []byte) Queue[*BackupID] {
	return NewQueueDB(
		c.db, namespace, func() *BackupID {
			return &BackupID{}
		}, func(tx kvdb.RwTx, item *BackupID) error {
			return maybeUpdateMaxCommitHeight(tx, *item)
		},
	)
}

// TerminateSession sets the given session's status to CSessionTerminal meaning
// that it will not be usable again. An error will be returned if the given
// session still has un-acked updates that should be attended to.
func (c *ClientDB) TerminateSession(id SessionID) error {
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		sessions := tx.ReadWriteBucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		sessionsBkt := tx.ReadBucket(cSessionBkt)
		if sessionsBkt == nil {
			return ErrUninitializedDB
		}

		chanIDIndexBkt := tx.ReadBucket(cChanIDIndexBkt)
		if chanIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		// Collect any un-acked updates for this session.
		committedUpdateCount := make(map[SessionID]uint16)
		perCommittedUpdate := func(s *ClientSession,
			_ *CommittedUpdate) {

			committedUpdateCount[s.ID]++
		}

		session, err := c.getClientSession(
			sessionsBkt, chanIDIndexBkt, id[:],
			WithPerCommittedUpdate(perCommittedUpdate),
		)
		if err != nil {
			return err
		}

		// If there are any un-acked updates for this session then
		// we don't allow the change of status as these updates must
		// first be dealt with somehow.
		if committedUpdateCount[id] > 0 {
			return ErrSessionHasUnackedUpdates
		}

		return markSessionStatus(sessions, session, CSessionTerminal)
	}, func() {})
}

// DeleteCommittedUpdates deletes all the committed updates for the given
// session.
func (c *ClientDB) DeleteCommittedUpdates(id *SessionID) error {
	return kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		sessions := tx.ReadWriteBucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		sessionBkt := sessions.NestedReadWriteBucket(id[:])
		if sessionBkt == nil {
			return fmt.Errorf("session bucket %s not found",
				id.String())
		}

		// If the commits sub-bucket doesn't exist, there can't possibly
		// be corresponding updates to remove.
		sessionCommits := sessionBkt.NestedReadWriteBucket(
			cSessionCommits,
		)
		if sessionCommits == nil {
			return nil
		}

		// errFoundUpdates is an error we will use to exit early from
		// the ForEach loop. The return of this error means that at
		// least one committed update exists.
		var errFoundUpdates = fmt.Errorf("found committed updates")
		err := sessionCommits.ForEach(func(k, v []byte) error {
			return errFoundUpdates
		})
		switch {
		// If the errFoundUpdates signal error was returned then there
		// are some updates that need to be deleted.
		case errors.Is(err, errFoundUpdates):

		// If no error is returned then the ForEach call back was never
		// entered meaning that there are no un-acked committed updates.
		// So we can exit now as there is nothing left to do.
		case err == nil:
			return nil

		// If an expected error is returned, return that error.
		default:
			return err
		}

		session, err := getClientSessionBody(sessions, id[:])
		if err != nil {
			return err
		}

		// Once we delete a committed update from the session, the
		// SeqNum of the session will be incorrect and so the session
		// should be marked as terminal.
		session.Status = CSessionTerminal
		err = putClientSessionBody(sessionBkt, session)
		if err != nil {
			return err
		}

		// Delete all the committed updates in one go by deleting the
		// session commits bucket.
		return sessionBkt.DeleteNestedBucket(cSessionCommits)
	}, func() {})
}

// putChannelToSessionMapping adds the given session ID to a channel's
// cChanSessions bucket.
func putChannelToSessionMapping(chanDetails kvdb.RwBucket,
	dbSessID uint64) error {

	chanSessIDsBkt, err := chanDetails.CreateBucketIfNotExists(
		cChanSessions,
	)
	if err != nil {
		return err
	}

	b, err := writeBigSize(dbSessID)
	if err != nil {
		return err
	}

	return chanSessIDsBkt.Put(b, []byte{1})
}

// getClientSessionBody loads the body of a ClientSession from the sessions
// bucket corresponding to the serialized session id. This does not deserialize
// the CommittedUpdates, AckUpdates or the Tower associated with the session.
// If the caller requires this info, use getClientSession.
func getClientSessionBody(sessions kvdb.RBucket,
	idBytes []byte) (*ClientSession, error) {

	sessionBkt := sessions.NestedReadBucket(idBytes)
	if sessionBkt == nil {
		return nil, ErrClientSessionNotFound
	}

	// Should never have a sessionBkt without also having its body.
	sessionBody := sessionBkt.Get(cSessionBody)
	if sessionBody == nil {
		return nil, ErrCorruptClientSession
	}

	var session ClientSession
	copy(session.ID[:], idBytes)

	err := session.Decode(bytes.NewReader(sessionBody))
	if err != nil {
		return nil, err
	}

	return &session, nil
}

// ClientSessionFilterFn describes the signature of a callback function that can
// be used to filter the sessions that are returned in any of the DB methods
// that read sessions from the DB.
type ClientSessionFilterFn func(*ClientSession) bool

// ClientSessWithNumCommittedUpdatesFilterFn describes the signature of a
// callback function that can be used to filter out a session based on the
// contents of ClientSession along with the number of un-acked committed updates
// that the session has.
type ClientSessWithNumCommittedUpdatesFilterFn func(*ClientSession, uint16) bool

// PerMaxHeightCB describes the signature of a callback function that can be
// called for each channel that a session has updates for to communicate the
// maximum commitment height that the session has backed up for the channel.
type PerMaxHeightCB func(*ClientSession, lnwire.ChannelID, uint64)

// PerNumAckedUpdatesCB describes the signature of a callback function that can
// be called for each channel that a session has updates for to communicate the
// number of updates that the session has for the channel.
type PerNumAckedUpdatesCB func(*ClientSession, lnwire.ChannelID, uint16)

// PerRogueUpdateCountCB describes the signature of a callback function that can
// be called for each session with the number of rogue updates that the session
// has.
type PerRogueUpdateCountCB func(*ClientSession, uint16)

// PerAckedUpdateCB describes the signature of a callback function that can be
// called for each of a session's acked updates.
type PerAckedUpdateCB func(*ClientSession, uint16, BackupID)

// PerCommittedUpdateCB describes the signature of a callback function that can
// be called for each of a session's committed updates (updates that the client
// has not yet received an ACK for).
type PerCommittedUpdateCB func(*ClientSession, *CommittedUpdate)

// ClientSessionListOption describes the signature of a functional option that
// can be used when listing client sessions in order to provide any extra
// instruction to the query.
type ClientSessionListOption func(cfg *ClientSessionListCfg)

// ClientSessionListCfg defines various query parameters that will be used when
// querying the DB for client sessions.
type ClientSessionListCfg struct {
	// PerNumAckedUpdates will, if set, be called for each of the session's
	// channels to communicate the number of updates stored for that
	// channel.
	PerNumAckedUpdates PerNumAckedUpdatesCB

	// PerRogueUpdateCount will, if set, be called with the number of rogue
	// updates that the session has backed up.
	PerRogueUpdateCount PerRogueUpdateCountCB

	// PerMaxHeight will, if set, be called for each of the session's
	// channels to communicate the highest commit height of updates stored
	// for that channel.
	PerMaxHeight PerMaxHeightCB

	// PerCommittedUpdate will, if set, be called for each of the session's
	// committed (un-acked) updates.
	PerCommittedUpdate PerCommittedUpdateCB

	// PreEvaluateFilterFn will be run after loading a session from the DB
	// and _before_ any of the other call-back functions in
	// ClientSessionListCfg. Therefore, if a session fails this filter
	// function, then it will not be passed to any of the other call backs
	// and won't be included in the return list.
	PreEvaluateFilterFn ClientSessionFilterFn

	// PostEvaluateFilterFn will be run _after_ all the other call-back
	// functions in ClientSessionListCfg. If a session fails this filter
	// function then all it means is that it won't be included in the list
	// of sessions to return.
	PostEvaluateFilterFn ClientSessWithNumCommittedUpdatesFilterFn
}

// NewClientSessionCfg constructs a new ClientSessionListCfg.
func NewClientSessionCfg() *ClientSessionListCfg {
	return &ClientSessionListCfg{}
}

// WithPerMaxHeight constructs a functional option that will set a call-back
// function to be called for each of a session's channels to communicate the
// maximum commitment height that the session has stored for the channel.
func WithPerMaxHeight(cb PerMaxHeightCB) ClientSessionListOption {
	return func(cfg *ClientSessionListCfg) {
		cfg.PerMaxHeight = cb
	}
}

// WithPerNumAckedUpdates constructs a functional option that will set a
// call-back function to be called for each of a session's channels to
// communicate the number of updates that the session has stored for the
// channel.
func WithPerNumAckedUpdates(cb PerNumAckedUpdatesCB) ClientSessionListOption {
	return func(cfg *ClientSessionListCfg) {
		cfg.PerNumAckedUpdates = cb
	}
}

// WithPerRogueUpdateCount constructs a functional option that will set a
// call-back function to be called with the number of rogue updates that the
// session has backed up.
func WithPerRogueUpdateCount(cb PerRogueUpdateCountCB) ClientSessionListOption {
	return func(cfg *ClientSessionListCfg) {
		cfg.PerRogueUpdateCount = cb
	}
}

// WithPerCommittedUpdate constructs a functional option that will set a
// call-back function to be called for each of a client's un-acked updates.
func WithPerCommittedUpdate(cb PerCommittedUpdateCB) ClientSessionListOption {
	return func(cfg *ClientSessionListCfg) {
		cfg.PerCommittedUpdate = cb
	}
}

// WithPreEvalFilterFn constructs a functional option that will set a call-back
// function that will be called immediately after loading a session. If the
// session fails this filter function, then it will not be passed to any of the
// other evaluation call-back functions.
func WithPreEvalFilterFn(fn ClientSessionFilterFn) ClientSessionListOption {
	return func(cfg *ClientSessionListCfg) {
		cfg.PreEvaluateFilterFn = fn
	}
}

// WithPostEvalFilterFn constructs a functional option that will set a call-back
// function that will be used to determine if a session should be included in
// the returned list. This differs from WithPreEvalFilterFn since that call-back
// is used to determine if the session should be evaluated at all (and thus
// run against the other ClientSessionListCfg call-backs) whereas the session
// will only reach the PostEvalFilterFn call-back once it has already been
// evaluated by all the other call-backs.
func WithPostEvalFilterFn(
	fn ClientSessWithNumCommittedUpdatesFilterFn) ClientSessionListOption {

	return func(cfg *ClientSessionListCfg) {
		cfg.PostEvaluateFilterFn = fn
	}
}

// getClientSession loads the full ClientSession associated with the serialized
// session id. This method populates the CommittedUpdates, AckUpdates and Tower
// in addition to the ClientSession's body.
func (c *ClientDB) getClientSession(sessionsBkt, chanIDIndexBkt kvdb.RBucket,
	idBytes []byte, opts ...ClientSessionListOption) (*ClientSession,
	error) {

	cfg := NewClientSessionCfg()
	for _, o := range opts {
		o(cfg)
	}

	session, err := getClientSessionBody(sessionsBkt, idBytes)
	if err != nil {
		return nil, err
	}

	if cfg.PreEvaluateFilterFn != nil && !cfg.PreEvaluateFilterFn(session) {
		return nil, ErrSessionFailedFilterFn
	}

	// Can't fail because client session body has already been read.
	sessionBkt := sessionsBkt.NestedReadBucket(idBytes)

	// Pass the session's committed (un-acked) updates through the call-back
	// if one is provided.
	numCommittedUpdates, err := filterClientSessionCommits(
		sessionBkt, session, cfg.PerCommittedUpdate,
	)
	if err != nil {
		return nil, err
	}

	// Pass the session's acked updates through the call-back if one is
	// provided.
	err = c.filterClientSessionAcks(
		sessionBkt, chanIDIndexBkt, session, cfg.PerMaxHeight,
		cfg.PerNumAckedUpdates, cfg.PerRogueUpdateCount,
	)
	if err != nil {
		return nil, err
	}

	if cfg.PostEvaluateFilterFn != nil &&
		!cfg.PostEvaluateFilterFn(session, numCommittedUpdates) {

		return nil, ErrSessionFailedFilterFn
	}

	return session, nil
}

// getClientSessionCommits retrieves all committed updates for the session
// identified by the serialized session id. If a PerCommittedUpdateCB is
// provided, then it will be called for each of the session's committed updates.
func getClientSessionCommits(sessionBkt kvdb.RBucket, s *ClientSession,
	cb PerCommittedUpdateCB) ([]CommittedUpdate, error) {

	// Initialize committedUpdates so that we can return an initialized map
	// if no committed updates exist.
	committedUpdates := make([]CommittedUpdate, 0)

	sessionCommits := sessionBkt.NestedReadBucket(cSessionCommits)
	if sessionCommits == nil {
		return committedUpdates, nil
	}

	err := sessionCommits.ForEach(func(k, v []byte) error {
		var committedUpdate CommittedUpdate
		err := committedUpdate.Decode(bytes.NewReader(v))
		if err != nil {
			return err
		}
		committedUpdate.SeqNum = byteOrder.Uint16(k)

		committedUpdates = append(committedUpdates, committedUpdate)

		if cb != nil {
			cb(s, &committedUpdate)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return committedUpdates, nil
}

// filterClientSessionAcks retrieves all acked updates for the session
// identified by the serialized session id and passes them to the provided
// call back if one is provided.
func (c *ClientDB) filterClientSessionAcks(sessionBkt,
	chanIDIndexBkt kvdb.RBucket, s *ClientSession, perMaxCb PerMaxHeightCB,
	perNumAckedUpdates PerNumAckedUpdatesCB,
	perRogueUpdateCount PerRogueUpdateCountCB) error {

	if perRogueUpdateCount != nil {
		var (
			count uint64
			err   error
		)
		rogueCountBytes := sessionBkt.Get(cSessionRogueUpdateCount)
		if len(rogueCountBytes) != 0 {
			count, err = readBigSize(rogueCountBytes)
			if err != nil {
				return err
			}
		}

		perRogueUpdateCount(s, uint16(count))
	}

	if perMaxCb == nil && perNumAckedUpdates == nil {
		return nil
	}

	sessionAcksRanges := sessionBkt.NestedReadBucket(cSessionAckRangeIndex)
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
		var chanID lnwire.ChannelID
		copy(chanID[:], chanIDBytes)

		if perMaxCb != nil {
			perMaxCb(s, chanID, index.MaxHeight())
		}

		if perNumAckedUpdates != nil {
			perNumAckedUpdates(s, chanID, uint16(index.NumInSet()))
		}
		return nil
	})
}

// filterClientSessionCommits retrieves all committed updates for the session
// identified by the serialized session id and passes them to the given
// PerCommittedUpdateCB callback.
func filterClientSessionCommits(sessionBkt kvdb.RBucket, s *ClientSession,
	cb PerCommittedUpdateCB) (uint16, error) {

	sessionCommits := sessionBkt.NestedReadBucket(cSessionCommits)
	if sessionCommits == nil {
		return 0, nil
	}

	var numUpdates uint16
	err := sessionCommits.ForEach(func(k, v []byte) error {
		numUpdates++

		if cb == nil {
			return nil
		}

		var committedUpdate CommittedUpdate
		err := committedUpdate.Decode(bytes.NewReader(v))
		if err != nil {
			return err
		}
		committedUpdate.SeqNum = byteOrder.Uint16(k)

		cb(s, &committedUpdate)

		return nil
	})
	if err != nil {
		return 0, err
	}

	return numUpdates, nil
}

// putClientSessionBody stores the body of the ClientSession (everything but the
// CommittedUpdates and AckedUpdates).
func putClientSessionBody(sessionBkt kvdb.RwBucket,
	session *ClientSession) error {

	var b bytes.Buffer
	err := session.Encode(&b)
	if err != nil {
		return err
	}

	return sessionBkt.Put(cSessionBody, b.Bytes())
}

// markSessionStatus updates the persisted state of the session to the new
// status.
func markSessionStatus(sessions kvdb.RwBucket, session *ClientSession,
	status CSessionStatus) error {

	sessionBkt, err := sessions.CreateBucketIfNotExists(session.ID[:])
	if err != nil {
		return err
	}

	session.Status = status

	return putClientSessionBody(sessionBkt, session)
}

// getChanSummary loads a ClientChanSummary for the passed chanID.
func getChanSummary(chanDetails kvdb.RBucket) (*ClientChanSummary, error) {
	chanSummaryBytes := chanDetails.Get(cChannelSummary)
	if chanSummaryBytes == nil {
		return nil, ErrChannelNotRegistered
	}

	var summary ClientChanSummary
	err := summary.Decode(bytes.NewReader(chanSummaryBytes))
	if err != nil {
		return nil, err
	}

	return &summary, nil
}

// putChanSummary stores a ClientChanSummary for the passed chanID.
func putChanSummary(chanDetails kvdb.RwBucket,
	summary *ClientChanSummary) error {

	var b bytes.Buffer
	err := summary.Encode(&b)
	if err != nil {
		return err
	}

	return chanDetails.Put(cChannelSummary, b.Bytes())
}

// getTower loads a Tower identified by its serialized tower id.
func getTower(towers kvdb.RBucket, id []byte) (*Tower, error) {
	towerBytes := towers.Get(id)
	if towerBytes == nil {
		return nil, ErrTowerNotFound
	}

	var tower Tower
	err := tower.Decode(bytes.NewReader(towerBytes))
	if err != nil {
		return nil, err
	}

	tower.ID = TowerIDFromBytes(id)

	return &tower, nil
}

// putTower stores a Tower identified by its serialized tower id.
func putTower(towers kvdb.RwBucket, tower *Tower) error {
	var b bytes.Buffer
	err := tower.Encode(&b)
	if err != nil {
		return err
	}

	return towers.Put(tower.ID.Bytes(), b.Bytes())
}

// getDBChanID returns the db-assigned channel ID for the given real channel ID.
// It returns both the uint64 and byte representation.
func getDBChanID(chanDetailsBkt kvdb.RBucket, chanID lnwire.ChannelID) (uint64,
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

// getDBSessionID returns the db-assigned session ID for the given real session
// ID. It returns both the uint64 and byte representation.
func getDBSessionID(sessionsBkt kvdb.RBucket, sessionID SessionID) (uint64,
	[]byte, error) {

	sessionBkt := sessionsBkt.NestedReadBucket(sessionID[:])
	if sessionBkt == nil {
		return 0, nil, ErrClientSessionNotFound
	}

	idBytes := sessionBkt.Get(cSessionDBID)
	if len(idBytes) == 0 {
		return 0, nil, fmt.Errorf("no db-assigned ID found for "+
			"session ID %s", sessionID)
	}

	id, err := readBigSize(idBytes)
	if err != nil {
		return 0, nil, err
	}

	return id, idBytes, nil
}

// maybeUpdateMaxCommitHeight updates the given channel details bucket with the
// given height if it is larger than the current max height stored for the
// channel.
func maybeUpdateMaxCommitHeight(tx kvdb.RwTx, backupID BackupID) error {
	chanDetailsBkt := tx.ReadWriteBucket(cChanDetailsBkt)
	if chanDetailsBkt == nil {
		return ErrUninitializedDB
	}

	// If an entry for this channel does not exist in the channel details
	// bucket then we exit here as this means that the channel has been
	// closed.
	chanDetails := chanDetailsBkt.NestedReadWriteBucket(backupID.ChanID[:])
	if chanDetails == nil {
		return nil
	}

	putHeight := func() error {
		b, err := writeBigSize(backupID.CommitHeight)
		if err != nil {
			return err
		}

		return chanDetails.Put(
			cChanMaxCommitmentHeight, b,
		)
	}

	// Get current height.
	heightBytes := chanDetails.Get(cChanMaxCommitmentHeight)

	// The height might have not been set yet, in which case
	// we can just write the new height.
	if len(heightBytes) == 0 {
		return putHeight()
	}

	// Otherwise, read in the current max commitment height for the channel.
	currentHeight, err := readBigSize(heightBytes)
	if err != nil {
		return err
	}

	// If the new height is not larger than the current persisted height,
	// then there is nothing left for us to do.
	if backupID.CommitHeight <= currentHeight {
		return nil
	}

	return putHeight()
}

func getRealSessionID(sessIDIndexBkt kvdb.RBucket, dbID uint64) (*SessionID,
	error) {

	dbIDBytes, err := writeBigSize(dbID)
	if err != nil {
		return nil, err
	}

	sessIDBytes := sessIDIndexBkt.Get(dbIDBytes)
	if len(sessIDBytes) != SessionIDSize {
		return nil, fmt.Errorf("session ID not found")
	}

	var sessID SessionID
	copy(sessID[:], sessIDBytes)

	return &sessID, nil
}

func getRealChannelID(chanIDIndexBkt kvdb.RBucket,
	dbID uint64) (*lnwire.ChannelID, error) {

	dbIDBytes, err := writeBigSize(dbID)
	if err != nil {
		return nil, err
	}

	chanIDBytes := chanIDIndexBkt.Get(dbIDBytes)
	if len(chanIDBytes) != 32 {
		return nil, fmt.Errorf("channel ID not found")
	}

	var chanIDS lnwire.ChannelID
	copy(chanIDS[:], chanIDBytes)

	return &chanIDS, nil
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

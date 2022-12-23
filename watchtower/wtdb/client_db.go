package wtdb

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
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
	cChanDetailsBkt = []byte("client-channel-detail-bucket")

	// cChanDBID is a key used in the cChanDetailsBkt to store the
	// db-assigned-id of a channel.
	cChanDBID = []byte("client-channel-db-id")

	// cChannelSummary is a key used in cChanDetailsBkt to store the encoded
	// body of ClientChanSummary.
	cChannelSummary = []byte("client-channel-summary")

	// cSessionBkt is a top-level bucket storing:
	//   session-id => cSessionBody -> encoded ClientSessionBody
	//              => cSessionCommits => seqnum -> encoded CommittedUpdate
	//              => cSessionAckRangeIndex => db-chan-id => start -> end
	cSessionBkt = []byte("client-session-bucket")

	// cSessionBody is a sub-bucket of cSessionBkt storing only the body of
	// the ClientSession.
	cSessionBody = []byte("client-session-body")

	// cSessionBody is a sub-bucket of cSessionBkt storing:
	//    seqnum -> encoded CommittedUpdate.
	cSessionCommits = []byte("client-session-commits")

	// cSessionAckRangeIndex is a sub-bucket of cSessionBkt storing
	//    chan-id => start -> end
	cSessionAckRangeIndex = []byte("client-session-ack-range-index")

	// cChanIDIndexBkt is a top-level bucket storing:
	//    db-assigned-id -> channel-ID
	cChanIDIndexBkt = []byte("client-channel-id-index")

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
			return nil, fmt.Errorf("could not open boltdb: %v", err)
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

			// Add the new address to the existing tower. If the
			// address is a duplicate, this will result in no
			// change.
			tower.AddAddress(lnAddr.Address)

			// If there are any client sessions that correspond to
			// this tower, we'll mark them as active to ensure we
			// load them upon restarts.
			towerSessIndex := towerToSessionIndex.NestedReadBucket(
				tower.ID.Bytes(),
			)
			if towerSessIndex == nil {
				return ErrTowerNotFound
			}

			sessions := tx.ReadWriteBucket(cSessionBkt)
			if sessions == nil {
				return ErrUninitializedDB
			}

			err = towerSessIndex.ForEach(func(k, _ []byte) error {
				session, err := getClientSessionBody(
					sessions, k,
				)
				if err != nil {
					return err
				}

				return markSessionStatus(
					sessions, session, CSessionActive,
				)
			})
			if err != nil {
				return err
			}
		} else {
			// No such tower exists, create a new tower id for our
			// new tower. The error is unhandled since NextSequence
			// never fails in an Update.
			towerID, _ := towerIndex.NextSequence()

			tower = &Tower{
				ID:          TowerID(towerID),
				IdentityKey: lnAddr.IdentityKey,
				Addresses:   []net.Addr{lnAddr.Address},
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
// persisted state. Otherwise, we'll attempt to mark the tower as inactive by
// marking all of its sessions inactive. If any of its sessions has unacked
// updates, then ErrTowerUnackedUpdates is returned. If the tower doesn't have
// any sessions at all, it'll be completely removed from the database.
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

		// If an address is provided, then we should _only_ remove the
		// address record from the database.
		if addr != nil {
			tower, err := getTower(towers, towerIDBytes)
			if err != nil {
				return err
			}

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

		// We'll mark its sessions as inactive as long as they don't
		// have any pending updates to ensure we don't load them upon
		// restarts.
		for _, session := range towerSessions {
			if committedUpdateCount[session.ID] > 0 {
				return ErrTowerUnackedUpdates
			}
			err := markSessionStatus(
				sessions, session, CSessionInactive,
			)
			if err != nil {
				return err
			}
		}

		return nil
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

// ListTowers retrieves the list of towers available within the database.
func (c *ClientDB) ListTowers() ([]*Tower, error) {
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
// CreateClientSession is invoked should return the same index.
func (c *ClientDB) NextSessionKeyIndex(towerID TowerID,
	blobType blob.Type) (uint32, error) {

	var index uint32
	err := kvdb.Update(c.db, func(tx kvdb.RwTx) error {
		keyIndex := tx.ReadWriteBucket(cSessionKeyIndexBkt)
		if keyIndex == nil {
			return ErrUninitializedDB
		}

		// Check the session key index to see if a key has already been
		// reserved for this tower. If so, we'll deserialize and return
		// the index directly.
		var err error
		index, err = getSessionKeyIndex(keyIndex, towerID, blobType)
		if err == nil {
			return nil
		}

		// Otherwise, generate a new session key index since the node
		// doesn't already have reserved index. The error is ignored
		// since NextSequence can't fail inside Update.
		index64, _ := keyIndex.NextSequence()

		// As a sanity check, assert that the index is still in the
		// valid range of unhardened pubkeys. In the future, we should
		// move to only using hardened keys, and this will prevent any
		// overlap from occurring until then. This also prevents us from
		// overflowing uint32s.
		if index64 > math.MaxInt32 {
			return fmt.Errorf("exhausted session key indexes")
		}

		// Create the key that will used to be store the reserved index.
		keyBytes := createSessionKeyIndexKey(towerID, blobType)

		index = uint32(index64)

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

		// Add the new entry to the towerID-to-SessionID index.
		indexBkt := towerToSessionIndex.NestedReadWriteBucket(
			towerID.Bytes(),
		)
		if indexBkt == nil {
			return ErrTowerNotFound
		}

		err = indexBkt.Put(session.ID[:], []byte{1})
		if err != nil {
			return err
		}

		sessionBkt, err := sessions.CreateBucket(session.ID[:])
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
func getRangesWriteBucket(tx kvdb.RwTx, sID SessionID,
	chanID lnwire.ChannelID) (kvdb.RwBucket, error) {

	sessions := tx.ReadWriteBucket(cSessionBkt)
	if sessions == nil {
		return nil, ErrUninitializedDB
	}

	chanDetailsBkt := tx.ReadBucket(cChanDetailsBkt)
	if chanDetailsBkt == nil {
		return nil, ErrUninitializedDB
	}

	sessionBkt, err := sessions.CreateBucketIfNotExists(sID[:])
	if err != nil {
		return nil, err
	}

	// Get the DB representation of the channel-ID.
	_, dbChanIDBytes, err := getDBChanID(chanDetailsBkt, chanID)
	if err != nil {
		return nil, err
	}

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

		towers := tx.ReadBucket(cTowerBkt)
		if towers == nil {
			return ErrUninitializedDB
		}

		chanIDIndexBkt := tx.ReadBucket(cChanIDIndexBkt)
		if chanIDIndexBkt == nil {
			return ErrUninitializedDB
		}

		var err error

		// If no tower ID is specified, then fetch all the sessions
		// known to the db.
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
		if err != nil {
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
		if err != nil {
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
		if sessionsBkt == nil {
			return nil
		}

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

// FetchChanSummaries loads a mapping from all registered channels to their
// channel summaries.
func (c *ClientDB) FetchChanSummaries() (ChannelSummaries, error) {
	var summaries map[lnwire.ChannelID]ClientChanSummary

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

			var chanID lnwire.ChannelID
			copy(chanID[:], k)

			summary, err := getChanSummary(chanDetails)
			if err != nil {
				return err
			}

			summaries[chanID] = *summary

			return nil
		})
	}, func() {
		summaries = make(map[lnwire.ChannelID]ClientChanSummary)
	})
	if err != nil {
		return nil, err
	}

	return summaries, nil
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

		chanDetailsBkt := tx.ReadBucket(cChanDetailsBkt)
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

		chanID := committedUpdate.BackupID.ChanID
		height := committedUpdate.BackupID.CommitHeight

		// Get the ranges write bucket before getting the range index to
		// ensure that the session acks sub-bucket is initialized, so
		// that we can insert an entry.
		rangesBkt, err := getRangesWriteBucket(tx, *id, chanID)
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

// PerMaxHeightCB describes the signature of a callback function that can be
// called for each channel that a session has updates for to communicate the
// maximum commitment height that the session has backed up for the channel.
type PerMaxHeightCB func(*ClientSession, lnwire.ChannelID, uint64)

// PerNumAckedUpdatesCB describes the signature of a callback function that can
// be called for each channel that a session has updates for to communicate the
// number of updates that the session has for the channel.
type PerNumAckedUpdatesCB func(*ClientSession, lnwire.ChannelID, uint16)

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

	// PerMaxHeight will, if set, be called for each of the session's
	// channels to communicate the highest commit height of updates stored
	// for that channel.
	PerMaxHeight PerMaxHeightCB

	// PerCommittedUpdate will, if set, be called for each of the session's
	// committed (un-acked) updates.
	PerCommittedUpdate PerCommittedUpdateCB
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

// WithPerCommittedUpdate constructs a functional option that will set a
// call-back function to be called for each of a client's un-acked updates.
func WithPerCommittedUpdate(cb PerCommittedUpdateCB) ClientSessionListOption {
	return func(cfg *ClientSessionListCfg) {
		cfg.PerCommittedUpdate = cb
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

	// Can't fail because client session body has already been read.
	sessionBkt := sessionsBkt.NestedReadBucket(idBytes)

	// Pass the session's committed (un-acked) updates through the call-back
	// if one is provided.
	err = filterClientSessionCommits(
		sessionBkt, session, cfg.PerCommittedUpdate,
	)
	if err != nil {
		return nil, err
	}

	// Pass the session's acked updates through the call-back if one is
	// provided.
	err = c.filterClientSessionAcks(
		sessionBkt, chanIDIndexBkt, session, cfg.PerMaxHeight,
		cfg.PerNumAckedUpdates,
	)
	if err != nil {
		return nil, err
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
	perNumAckedUpdates PerNumAckedUpdatesCB) error {

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
	cb PerCommittedUpdateCB) error {

	if cb == nil {
		return nil
	}

	sessionCommits := sessionBkt.NestedReadBucket(cSessionCommits)
	if sessionCommits == nil {
		return nil
	}

	err := sessionCommits.ForEach(func(k, v []byte) error {
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
		return err
	}

	return nil
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

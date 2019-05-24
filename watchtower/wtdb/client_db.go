package wtdb

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// clientDBName is the filename of client database.
	clientDBName = "wtclient.db"
)

var (
	// cSessionKeyIndexBkt is a top-level bucket storing:
	//   tower-id -> reserved-session-key-index (uint32).
	cSessionKeyIndexBkt = []byte("client-session-key-index-bucket")

	// cChanSummaryBkt is a top-level bucket storing:
	//   channel-id -> encoded ClientChanSummary.
	cChanSummaryBkt = []byte("client-channel-summary-bucket")

	// cSessionBkt is a top-level bucket storing:
	//   session-id => cSessionBody -> encoded ClientSessionBody
	//              => cSessionCommits => seqnum -> encoded CommittedUpdate
	//              => cSessionAcks => seqnum -> encoded BackupID
	cSessionBkt = []byte("client-session-bucket")

	// cSessionBody is a sub-bucket of cSessionBkt storing only the body of
	// the ClientSession.
	cSessionBody = []byte("client-session-body")

	// cSessionBody is a sub-bucket of cSessionBkt storing:
	//    seqnum -> encoded CommittedUpdate.
	cSessionCommits = []byte("client-session-commits")

	// cSessionAcks is a sub-bucket of cSessionBkt storing:
	//    seqnum -> encoded BackupID.
	cSessionAcks = []byte("client-session-acks")

	// cTowerBkt is a top-level bucket storing:
	//    tower-id -> encoded Tower.
	cTowerBkt = []byte("client-tower-bucket")

	// cTowerIndexBkt is a top-level bucket storing:
	//    tower-pubkey -> tower-id.
	cTowerIndexBkt = []byte("client-tower-index-bucket")

	// ErrTowerNotFound signals that the target tower was not found in the
	// database.
	ErrTowerNotFound = errors.New("tower not found")

	// ErrCorruptClientSession signals that the client session's on-disk
	// structure deviates from what is expected.
	ErrCorruptClientSession = errors.New("client session corrupted")

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
)

// ClientDB is single database providing a persistent storage engine for the
// wtclient.
type ClientDB struct {
	db     *bbolt.DB
	dbPath string
}

// OpenClientDB opens the client database given the path to the database's
// directory. If no such database exists, this method will initialize a fresh
// one using the latest version number and bucket structure. If a database
// exists but has a lower version number than the current version, any necessary
// migrations will be applied before returning. Any attempt to open a database
// with a version number higher that the latest version will fail to prevent
// accidental reversion.
func OpenClientDB(dbPath string) (*ClientDB, error) {
	bdb, firstInit, err := createDBIfNotExist(dbPath, clientDBName)
	if err != nil {
		return nil, err
	}

	clientDB := &ClientDB{
		db:     bdb,
		dbPath: dbPath,
	}

	err = initOrSyncVersions(clientDB, firstInit, clientDBVersions)
	if err != nil {
		bdb.Close()
		return nil, err
	}

	// Now that the database version fully consistent with our latest known
	// version, ensure that all top-level buckets known to this version are
	// initialized. This allows us to assume their presence throughout all
	// operations. If an known top-level bucket is expected to exist but is
	// missing, this will trigger a ErrUninitializedDB error.
	err = clientDB.db.Update(initClientDBBuckets)
	if err != nil {
		bdb.Close()
		return nil, err
	}

	return clientDB, nil
}

// initClientDBBuckets creates all top-level buckets required to handle database
// operations required by the latest version.
func initClientDBBuckets(tx *bbolt.Tx) error {
	buckets := [][]byte{
		cSessionKeyIndexBkt,
		cChanSummaryBkt,
		cSessionBkt,
		cTowerBkt,
		cTowerIndexBkt,
	}

	for _, bucket := range buckets {
		_, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
	}

	return nil
}

// bdb returns the backing bbolt.DB instance.
//
// NOTE: Part of the versionedDB interface.
func (c *ClientDB) bdb() *bbolt.DB {
	return c.db
}

// Version returns the database's current version number.
//
// NOTE: Part of the versionedDB interface.
func (c *ClientDB) Version() (uint32, error) {
	var version uint32
	err := c.db.View(func(tx *bbolt.Tx) error {
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
func (c *ClientDB) Close() error {
	return c.db.Close()
}

// CreateTower initializes a database entry with the given lightning address. If
// the tower exists, the address is append to the list of all addresses used to
// that tower previously.
func (c *ClientDB) CreateTower(lnAddr *lnwire.NetAddress) (*Tower, error) {
	var towerPubKey [33]byte
	copy(towerPubKey[:], lnAddr.IdentityKey.SerializeCompressed())

	var tower *Tower
	err := c.db.Update(func(tx *bbolt.Tx) error {
		towerIndex := tx.Bucket(cTowerIndexBkt)
		if towerIndex == nil {
			return ErrUninitializedDB
		}

		towers := tx.Bucket(cTowerBkt)
		if towers == nil {
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
		}

		// Store the new or updated tower under its tower id.
		return putTower(towers, tower)
	})
	if err != nil {
		return nil, err
	}

	return tower, nil
}

// LoadTower retrieves a tower by its tower ID.
func (c *ClientDB) LoadTower(towerID TowerID) (*Tower, error) {
	var tower *Tower
	err := c.db.View(func(tx *bbolt.Tx) error {
		towers := tx.Bucket(cTowerBkt)
		if towers == nil {
			return ErrUninitializedDB
		}

		var err error
		tower, err = getTower(towers, towerID.Bytes())
		return err
	})
	if err != nil {
		return nil, err
	}

	return tower, nil
}

// NextSessionKeyIndex reserves a new session key derivation index for a
// particular tower id. The index is reserved for that tower until
// CreateClientSession is invoked for that tower and index, at which point a new
// index for that tower can be reserved. Multiple calls to this method before
// CreateClientSession is invoked should return the same index.
func (c *ClientDB) NextSessionKeyIndex(towerID TowerID) (uint32, error) {
	var index uint32
	err := c.db.Update(func(tx *bbolt.Tx) error {
		keyIndex := tx.Bucket(cSessionKeyIndexBkt)
		if keyIndex == nil {
			return ErrUninitializedDB
		}

		// Check the session key index to see if a key has already been
		// reserved for this tower. If so, we'll deserialize and return
		// the index directly.
		towerIDBytes := towerID.Bytes()
		indexBytes := keyIndex.Get(towerIDBytes)
		if len(indexBytes) == 4 {
			index = byteOrder.Uint32(indexBytes)
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

		index = uint32(index64)

		var indexBuf [4]byte
		byteOrder.PutUint32(indexBuf[:], index)

		// Record the reserved session key index under this tower's id.
		return keyIndex.Put(towerIDBytes, indexBuf[:])
	})
	if err != nil {
		return 0, err
	}

	return index, nil
}

// CreateClientSession records a newly negotiated client session in the set of
// active sessions. The session can be identified by its SessionID.
func (c *ClientDB) CreateClientSession(session *ClientSession) error {
	return c.db.Update(func(tx *bbolt.Tx) error {
		keyIndexes := tx.Bucket(cSessionKeyIndexBkt)
		if keyIndexes == nil {
			return ErrUninitializedDB
		}

		sessions := tx.Bucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		// Check that  client session with this session id doesn't
		// already exist.
		existingSessionBytes := sessions.Bucket(session.ID[:])
		if existingSessionBytes != nil {
			return ErrClientSessionAlreadyExists
		}

		// Check that this tower has a reserved key index.
		towerIDBytes := session.TowerID.Bytes()
		keyIndexBytes := keyIndexes.Get(towerIDBytes)
		if len(keyIndexBytes) != 4 {
			return ErrNoReservedKeyIndex
		}

		// Assert that the key index of the inserted session matches the
		// reserved session key index.
		index := byteOrder.Uint32(keyIndexBytes)
		if index != session.KeyIndex {
			return ErrIncorrectKeyIndex
		}

		// Remove the key index reservation.
		err := keyIndexes.Delete(towerIDBytes)
		if err != nil {
			return err
		}

		// Finally, write the client session's body in the sessions
		// bucket.
		return putClientSessionBody(sessions, session)
	})
}

// ListClientSessions returns the set of all client sessions known to the db.
func (c *ClientDB) ListClientSessions() (map[SessionID]*ClientSession, error) {
	clientSessions := make(map[SessionID]*ClientSession)
	err := c.db.View(func(tx *bbolt.Tx) error {
		sessions := tx.Bucket(cSessionBkt)
		if sessions == nil {
			return ErrUninitializedDB
		}

		return sessions.ForEach(func(k, _ []byte) error {
			// We'll load the full client session since the client
			// will need the CommittedUpdates and AckedUpdates on
			// startup to resume committed updates and compute the
			// highest known commit height for each channel.
			session, err := getClientSession(sessions, k)
			if err != nil {
				return err
			}

			clientSessions[session.ID] = session

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return clientSessions, nil
}

// FetchChanSummaries loads a mapping from all registered channels to their
// channel summaries.
func (c *ClientDB) FetchChanSummaries() (ChannelSummaries, error) {
	summaries := make(map[lnwire.ChannelID]ClientChanSummary)
	err := c.db.View(func(tx *bbolt.Tx) error {
		chanSummaries := tx.Bucket(cChanSummaryBkt)
		if chanSummaries == nil {
			return ErrUninitializedDB
		}

		return chanSummaries.ForEach(func(k, v []byte) error {
			var chanID lnwire.ChannelID
			copy(chanID[:], k)

			var summary ClientChanSummary
			err := summary.Decode(bytes.NewReader(v))
			if err != nil {
				return err
			}

			summaries[chanID] = summary

			return nil
		})
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

	return c.db.Update(func(tx *bbolt.Tx) error {
		chanSummaries := tx.Bucket(cChanSummaryBkt)
		if chanSummaries == nil {
			return ErrUninitializedDB
		}

		_, err := getChanSummary(chanSummaries, chanID)
		switch {

		// Summary already exists.
		case err == nil:
			return ErrChannelAlreadyRegistered

		// Channel is not registered, proceed with registration.
		case err == ErrChannelNotRegistered:

		// Unexpected error.
		case err != nil:
			return err
		}

		summary := ClientChanSummary{
			SweepPkScript: sweepPkScript,
		}

		return putChanSummary(chanSummaries, chanID, &summary)
	})
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
	err := c.db.Update(func(tx *bbolt.Tx) error {
		sessions := tx.Bucket(cSessionBkt)
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
		sessionBkt := sessions.Bucket(id[:])

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
		err = putClientSessionBody(sessions, session)
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

	return c.db.Update(func(tx *bbolt.Tx) error {
		sessions := tx.Bucket(cSessionBkt)
		if sessions == nil {
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

		// Write the client session with the updated last applied value.
		err = putClientSessionBody(sessions, session)
		if err != nil {
			return err
		}

		// Can't fail because of getClientSession succeeded.
		sessionBkt := sessions.Bucket(id[:])

		// If the commits sub-bucket doesn't exist, there can't possibly
		// be a corresponding committed update to remove.
		sessionCommits := sessionBkt.Bucket(cSessionCommits)
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

		// Ensure that the session acks sub-bucket is initialized so we
		// can insert an entry.
		sessionAcks, err := sessionBkt.CreateBucketIfNotExists(
			cSessionAcks,
		)
		if err != nil {
			return err
		}

		// The session acks only need to track the backup id of the
		// update, so we can discard the blob and hint.
		var b bytes.Buffer
		err = committedUpdate.BackupID.Encode(&b)
		if err != nil {
			return err
		}

		// Finally, insert the ack into the sessionAcks sub-bucket.
		return sessionAcks.Put(seqNumBuf[:], b.Bytes())
	})
}

// getClientSessionBody loads the body of a ClientSession from the sessions
// bucket corresponding to the serialized session id. This does not deserialize
// the CommittedUpdates or AckUpdates associated with the session. If the caller
// requires this info, use getClientSession.
func getClientSessionBody(sessions *bbolt.Bucket,
	idBytes []byte) (*ClientSession, error) {

	sessionBkt := sessions.Bucket(idBytes)
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

// getClientSession loads the full ClientSession associated with the serialized
// session id. This method populates the CommittedUpdates and AckUpdates in
// addition to the ClientSession's body.
func getClientSession(sessions *bbolt.Bucket,
	idBytes []byte) (*ClientSession, error) {

	session, err := getClientSessionBody(sessions, idBytes)
	if err != nil {
		return nil, err
	}

	// Fetch the committed updates for this session.
	commitedUpdates, err := getClientSessionCommits(sessions, idBytes)
	if err != nil {
		return nil, err
	}

	// Fetch the acked updates for this session.
	ackedUpdates, err := getClientSessionAcks(sessions, idBytes)
	if err != nil {
		return nil, err
	}

	session.CommittedUpdates = commitedUpdates
	session.AckedUpdates = ackedUpdates

	return session, nil
}

// getClientSessionCommits retrieves all committed updates for the session
// identified by the serialized session id.
func getClientSessionCommits(sessions *bbolt.Bucket,
	idBytes []byte) ([]CommittedUpdate, error) {

	// Can't fail because client session body has already been read.
	sessionBkt := sessions.Bucket(idBytes)

	// Initialize commitedUpdates so that we can return an initialized map
	// if no committed updates exist.
	committedUpdates := make([]CommittedUpdate, 0)

	sessionCommits := sessionBkt.Bucket(cSessionCommits)
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

		return nil
	})
	if err != nil {
		return nil, err
	}

	return committedUpdates, nil
}

// getClientSessionAcks retrieves all acked updates for the session identified
// by the serialized session id.
func getClientSessionAcks(sessions *bbolt.Bucket,
	idBytes []byte) (map[uint16]BackupID, error) {

	// Can't fail because client session body has already been read.
	sessionBkt := sessions.Bucket(idBytes)

	// Initialize ackedUpdates so that we can return an initialized map if
	// no acked updates exist.
	ackedUpdates := make(map[uint16]BackupID)

	sessionAcks := sessionBkt.Bucket(cSessionAcks)
	if sessionAcks == nil {
		return ackedUpdates, nil
	}

	err := sessionAcks.ForEach(func(k, v []byte) error {
		seqNum := byteOrder.Uint16(k)

		var backupID BackupID
		err := backupID.Decode(bytes.NewReader(v))
		if err != nil {
			return err
		}

		ackedUpdates[seqNum] = backupID

		return nil
	})
	if err != nil {
		return nil, err
	}

	return ackedUpdates, nil
}

// putClientSessionBody stores the body of the ClientSession (everything but the
// CommittedUpdates and AckedUpdates).
func putClientSessionBody(sessions *bbolt.Bucket,
	session *ClientSession) error {

	sessionBkt, err := sessions.CreateBucketIfNotExists(session.ID[:])
	if err != nil {
		return err
	}

	var b bytes.Buffer
	err = session.Encode(&b)
	if err != nil {
		return err
	}

	return sessionBkt.Put(cSessionBody, b.Bytes())
}

// getChanSummary loads a ClientChanSummary for the passed chanID.
func getChanSummary(chanSummaries *bbolt.Bucket,
	chanID lnwire.ChannelID) (*ClientChanSummary, error) {

	chanSummaryBytes := chanSummaries.Get(chanID[:])
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
func putChanSummary(chanSummaries *bbolt.Bucket, chanID lnwire.ChannelID,
	summary *ClientChanSummary) error {

	var b bytes.Buffer
	err := summary.Encode(&b)
	if err != nil {
		return err
	}

	return chanSummaries.Put(chanID[:], b.Bytes())
}

// getTower loads a Tower identified by its serialized tower id.
func getTower(towers *bbolt.Bucket, id []byte) (*Tower, error) {
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
func putTower(towers *bbolt.Bucket, tower *Tower) error {
	var b bytes.Buffer
	err := tower.Encode(&b)
	if err != nil {
		return err
	}

	return towers.Put(tower.ID.Bytes(), b.Bytes())
}

package wtmock

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

type towerPK [33]byte

type keyIndexKey struct {
	towerID  wtdb.TowerID
	blobType blob.Type
}

// ClientDB is a mock, in-memory database or testing the watchtower client
// behavior.
type ClientDB struct {
	nextTowerID uint64 // to be used atomically

	mu             sync.Mutex
	summaries      map[lnwire.ChannelID]wtdb.ClientChanSummary
	activeSessions map[wtdb.SessionID]wtdb.ClientSession
	towerIndex     map[towerPK]wtdb.TowerID
	towers         map[wtdb.TowerID]*wtdb.Tower

	nextIndex     uint32
	indexes       map[keyIndexKey]uint32
	legacyIndexes map[wtdb.TowerID]uint32
}

// NewClientDB initializes a new mock ClientDB.
func NewClientDB() *ClientDB {
	return &ClientDB{
		summaries:      make(map[lnwire.ChannelID]wtdb.ClientChanSummary),
		activeSessions: make(map[wtdb.SessionID]wtdb.ClientSession),
		towerIndex:     make(map[towerPK]wtdb.TowerID),
		towers:         make(map[wtdb.TowerID]*wtdb.Tower),
		indexes:        make(map[keyIndexKey]uint32),
		legacyIndexes:  make(map[wtdb.TowerID]uint32),
	}
}

// CreateTower initialize an address record used to communicate with a
// watchtower. Each Tower is assigned a unique ID, that is used to amortize
// storage costs of the public key when used by multiple sessions. If the tower
// already exists, the address is appended to the list of all addresses used to
// that tower previously and its corresponding sessions are marked as active.
func (m *ClientDB) CreateTower(lnAddr *lnwire.NetAddress) (*wtdb.Tower, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var towerPubKey towerPK
	copy(towerPubKey[:], lnAddr.IdentityKey.SerializeCompressed())

	var tower *wtdb.Tower
	towerID, ok := m.towerIndex[towerPubKey]
	if ok {
		tower = m.towers[towerID]
		tower.AddAddress(lnAddr.Address)

		towerSessions, err := m.listClientSessions(&towerID)
		if err != nil {
			return nil, err
		}
		for id, session := range towerSessions {
			session.Status = wtdb.CSessionActive
			m.activeSessions[id] = *session
		}
	} else {
		towerID = wtdb.TowerID(atomic.AddUint64(&m.nextTowerID, 1))
		tower = &wtdb.Tower{
			ID:          wtdb.TowerID(towerID),
			IdentityKey: lnAddr.IdentityKey,
			Addresses:   []net.Addr{lnAddr.Address},
		}
	}

	m.towerIndex[towerPubKey] = towerID
	m.towers[towerID] = tower

	return copyTower(tower), nil
}

// RemoveTower modifies a tower's record within the database. If an address is
// provided, then _only_ the address record should be removed from the tower's
// persisted state. Otherwise, we'll attempt to mark the tower as inactive by
// marking all of its sessions inactive. If any of its sessions has unacked
// updates, then ErrTowerUnackedUpdates is returned. If the tower doesn't have
// any sessions at all, it'll be completely removed from the database.
//
// NOTE: An error is not returned if the tower doesn't exist.
func (m *ClientDB) RemoveTower(pubKey *btcec.PublicKey, addr net.Addr) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tower, err := m.loadTower(pubKey)
	if err == wtdb.ErrTowerNotFound {
		return nil
	}
	if err != nil {
		return err
	}

	if addr != nil {
		tower.RemoveAddress(addr)
		if len(tower.Addresses) == 0 {
			return wtdb.ErrLastTowerAddr
		}
		m.towers[tower.ID] = tower
		return nil
	}

	towerSessions, err := m.listClientSessions(&tower.ID)
	if err != nil {
		return err
	}
	if len(towerSessions) == 0 {
		var towerPK towerPK
		copy(towerPK[:], pubKey.SerializeCompressed())
		delete(m.towerIndex, towerPK)
		delete(m.towers, tower.ID)
		return nil
	}

	for id, session := range towerSessions {
		if len(session.CommittedUpdates) > 0 {
			return wtdb.ErrTowerUnackedUpdates
		}
		session.Status = wtdb.CSessionInactive
		m.activeSessions[id] = *session
	}

	return nil
}

// LoadTower retrieves a tower by its public key.
func (m *ClientDB) LoadTower(pubKey *btcec.PublicKey) (*wtdb.Tower, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadTower(pubKey)
}

// loadTower retrieves a tower by its public key.
//
// NOTE: This method requires the database's lock to be acquired.
func (m *ClientDB) loadTower(pubKey *btcec.PublicKey) (*wtdb.Tower, error) {
	var towerPK towerPK
	copy(towerPK[:], pubKey.SerializeCompressed())

	towerID, ok := m.towerIndex[towerPK]
	if !ok {
		return nil, wtdb.ErrTowerNotFound
	}
	tower, ok := m.towers[towerID]
	if !ok {
		return nil, wtdb.ErrTowerNotFound
	}

	return copyTower(tower), nil
}

// LoadTowerByID retrieves a tower by its tower ID.
func (m *ClientDB) LoadTowerByID(towerID wtdb.TowerID) (*wtdb.Tower, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if tower, ok := m.towers[towerID]; ok {
		return copyTower(tower), nil
	}

	return nil, wtdb.ErrTowerNotFound
}

// ListTowers retrieves the list of towers available within the database.
func (m *ClientDB) ListTowers() ([]*wtdb.Tower, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	towers := make([]*wtdb.Tower, 0, len(m.towers))
	for _, tower := range m.towers {
		towers = append(towers, copyTower(tower))
	}

	return towers, nil
}

// MarkBackupIneligible records that particular commit height is ineligible for
// backup. This allows the client to track which updates it should not attempt
// to retry after startup.
func (m *ClientDB) MarkBackupIneligible(chanID lnwire.ChannelID, commitHeight uint64) error {
	return nil
}

// ListClientSessions returns the set of all client sessions known to the db. An
// optional tower ID can be used to filter out any client sessions in the
// response that do not correspond to this tower.
func (m *ClientDB) ListClientSessions(
	tower *wtdb.TowerID) (map[wtdb.SessionID]*wtdb.ClientSession, error) {

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listClientSessions(tower)
}

// listClientSessions returns the set of all client sessions known to the db. An
// optional tower ID can be used to filter out any client sessions in the
// response that do not correspond to this tower.
func (m *ClientDB) listClientSessions(
	tower *wtdb.TowerID) (map[wtdb.SessionID]*wtdb.ClientSession, error) {

	sessions := make(map[wtdb.SessionID]*wtdb.ClientSession)
	for _, session := range m.activeSessions {
		session := session
		if tower != nil && *tower != session.TowerID {
			continue
		}
		sessions[session.ID] = &session
	}

	return sessions, nil
}

// CreateClientSession records a newly negotiated client session in the set of
// active sessions. The session can be identified by its SessionID.
func (m *ClientDB) CreateClientSession(session *wtdb.ClientSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Ensure that we aren't overwriting an existing session.
	if _, ok := m.activeSessions[session.ID]; ok {
		return wtdb.ErrClientSessionAlreadyExists
	}

	key := keyIndexKey{
		towerID:  session.TowerID,
		blobType: session.Policy.BlobType,
	}

	// Ensure that a session key index has been reserved for this tower.
	keyIndex, err := m.getSessionKeyIndex(key)
	if err != nil {
		return err
	}

	// Ensure that the session's index matches the reserved index.
	if keyIndex != session.KeyIndex {
		return wtdb.ErrIncorrectKeyIndex
	}

	// Remove the key index reservation for this tower. Once committed, this
	// permits us to create another session with this tower.
	delete(m.indexes, key)
	if key.blobType == blob.TypeAltruistCommit {
		delete(m.legacyIndexes, key.towerID)
	}

	m.activeSessions[session.ID] = wtdb.ClientSession{
		ID: session.ID,
		ClientSessionBody: wtdb.ClientSessionBody{
			SeqNum:           session.SeqNum,
			TowerLastApplied: session.TowerLastApplied,
			TowerID:          session.TowerID,
			KeyIndex:         session.KeyIndex,
			Policy:           session.Policy,
			RewardPkScript:   cloneBytes(session.RewardPkScript),
		},
		CommittedUpdates: make([]wtdb.CommittedUpdate, 0),
		AckedUpdates:     make(map[uint16]wtdb.BackupID),
	}

	return nil
}

// NextSessionKeyIndex reserves a new session key derivation index for a
// particular tower id. The index is reserved for that tower until
// CreateClientSession is invoked for that tower and index, at which point a new
// index for that tower can be reserved. Multiple calls to this method before
// CreateClientSession is invoked should return the same index.
func (m *ClientDB) NextSessionKeyIndex(towerID wtdb.TowerID,
	blobType blob.Type) (uint32, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	key := keyIndexKey{
		towerID:  towerID,
		blobType: blobType,
	}

	if index, err := m.getSessionKeyIndex(key); err == nil {
		return index, nil
	}

	m.nextIndex++
	index := m.nextIndex
	m.indexes[key] = index

	return index, nil
}

func (m *ClientDB) getSessionKeyIndex(key keyIndexKey) (uint32, error) {
	if index, ok := m.indexes[key]; ok {
		return index, nil
	}

	if key.blobType == blob.TypeAltruistCommit {
		if index, ok := m.legacyIndexes[key.towerID]; ok {
			return index, nil
		}
	}

	return 0, wtdb.ErrNoReservedKeyIndex
}

// CommitUpdate persists the CommittedUpdate provided in the slot for (session,
// seqNum). This allows the client to retransmit this update on startup.
func (m *ClientDB) CommitUpdate(id *wtdb.SessionID,
	update *wtdb.CommittedUpdate) (uint16, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	// Fail if session doesn't exist.
	session, ok := m.activeSessions[*id]
	if !ok {
		return 0, wtdb.ErrClientSessionNotFound
	}

	// Check if an update has already been committed for this state.
	for _, dbUpdate := range session.CommittedUpdates {
		if dbUpdate.SeqNum == update.SeqNum {
			// If the breach hint matches, we'll just return the
			// last applied value so the client can retransmit.
			if dbUpdate.Hint == update.Hint {
				return session.TowerLastApplied, nil
			}

			// Otherwise, fail since the breach hint doesn't match.
			return 0, wtdb.ErrUpdateAlreadyCommitted
		}
	}

	// Sequence number must increment.
	if update.SeqNum != session.SeqNum+1 {
		return 0, wtdb.ErrCommitUnorderedUpdate
	}

	// Save the update and increment the sequence number.
	session.CommittedUpdates = append(session.CommittedUpdates, *update)
	session.SeqNum++
	m.activeSessions[*id] = session

	return session.TowerLastApplied, nil
}

// AckUpdate persists an acknowledgment for a given (session, seqnum) pair. This
// removes the update from the set of committed updates, and validates the
// lastApplied value returned from the tower.
func (m *ClientDB) AckUpdate(id *wtdb.SessionID, seqNum, lastApplied uint16) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Fail if session doesn't exist.
	session, ok := m.activeSessions[*id]
	if !ok {
		return wtdb.ErrClientSessionNotFound
	}

	// Ensure the returned last applied value does not exceed the highest
	// allocated sequence number.
	if lastApplied > session.SeqNum {
		return wtdb.ErrUnallocatedLastApplied
	}

	// Ensure the last applied value isn't lower than a previous one sent by
	// the tower.
	if lastApplied < session.TowerLastApplied {
		return wtdb.ErrLastAppliedReversion
	}

	// Retrieve the committed update, failing if none is found. We should
	// only receive acks for state updates that we send.
	updates := session.CommittedUpdates
	for i, update := range updates {
		if update.SeqNum != seqNum {
			continue
		}

		// Remove the committed update from disk and mark the update as
		// acked. The tower last applied value is also recorded to send
		// along with the next update.
		copy(updates[:i], updates[i+1:])
		updates[len(updates)-1] = wtdb.CommittedUpdate{}
		session.CommittedUpdates = updates[:len(updates)-1]

		session.AckedUpdates[seqNum] = update.BackupID
		session.TowerLastApplied = lastApplied

		m.activeSessions[*id] = session
		return nil
	}

	return wtdb.ErrCommittedUpdateNotFound
}

// FetchChanSummaries loads a mapping from all registered channels to their
// channel summaries.
func (m *ClientDB) FetchChanSummaries() (wtdb.ChannelSummaries, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	summaries := make(map[lnwire.ChannelID]wtdb.ClientChanSummary)
	for chanID, summary := range m.summaries {
		summaries[chanID] = wtdb.ClientChanSummary{
			SweepPkScript: cloneBytes(summary.SweepPkScript),
		}
	}

	return summaries, nil
}

// RegisterChannel registers a channel for use within the client database. For
// now, all that is stored in the channel summary is the sweep pkscript that
// we'd like any tower sweeps to pay into. In the future, this will be extended
// to contain more info to allow the client efficiently request historical
// states to be backed up under the client's active policy.
func (m *ClientDB) RegisterChannel(chanID lnwire.ChannelID,
	sweepPkScript []byte) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.summaries[chanID]; ok {
		return wtdb.ErrChannelAlreadyRegistered
	}

	m.summaries[chanID] = wtdb.ClientChanSummary{
		SweepPkScript: cloneBytes(sweepPkScript),
	}

	return nil
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}

	bb := make([]byte, len(b))
	copy(bb, b)

	return bb
}

func copyTower(tower *wtdb.Tower) *wtdb.Tower {
	t := &wtdb.Tower{
		ID:          tower.ID,
		IdentityKey: tower.IdentityKey,
		Addresses:   make([]net.Addr, len(tower.Addresses)),
	}
	copy(t.Addresses, tower.Addresses)

	return t
}

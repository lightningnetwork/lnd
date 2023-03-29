package wtmock

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

var byteOrder = binary.BigEndian

type towerPK [33]byte

type keyIndexKey struct {
	towerID  wtdb.TowerID
	blobType blob.Type
}

type rangeIndexArrayMap map[wtdb.SessionID]map[lnwire.ChannelID]*wtdb.RangeIndex

type rangeIndexKVStore map[wtdb.SessionID]map[lnwire.ChannelID]*mockKVStore

type channel struct {
	summary      *wtdb.ClientChanSummary
	closedHeight uint32
	sessions     map[wtdb.SessionID]bool
}

// ClientDB is a mock, in-memory database or testing the watchtower client
// behavior.
type ClientDB struct {
	nextTowerID uint64 // to be used atomically

	mu                    sync.Mutex
	channels              map[lnwire.ChannelID]*channel
	activeSessions        map[wtdb.SessionID]wtdb.ClientSession
	ackedUpdates          rangeIndexArrayMap
	persistedAckedUpdates rangeIndexKVStore
	committedUpdates      map[wtdb.SessionID][]wtdb.CommittedUpdate
	towerIndex            map[towerPK]wtdb.TowerID
	towers                map[wtdb.TowerID]*wtdb.Tower
	closableSessions      map[wtdb.SessionID]uint32

	nextIndex     uint32
	indexes       map[keyIndexKey]uint32
	legacyIndexes map[wtdb.TowerID]uint32
}

// NewClientDB initializes a new mock ClientDB.
func NewClientDB() *ClientDB {
	return &ClientDB{
		channels: make(map[lnwire.ChannelID]*channel),
		activeSessions: make(
			map[wtdb.SessionID]wtdb.ClientSession,
		),
		ackedUpdates:          make(rangeIndexArrayMap),
		persistedAckedUpdates: make(rangeIndexKVStore),
		committedUpdates: make(
			map[wtdb.SessionID][]wtdb.CommittedUpdate,
		),
		towerIndex:       make(map[towerPK]wtdb.TowerID),
		towers:           make(map[wtdb.TowerID]*wtdb.Tower),
		indexes:          make(map[keyIndexKey]uint32),
		legacyIndexes:    make(map[wtdb.TowerID]uint32),
		closableSessions: make(map[wtdb.SessionID]uint32),
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
			ID:          towerID,
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
		if len(m.committedUpdates[session.ID]) > 0 {
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
func (m *ClientDB) MarkBackupIneligible(_ lnwire.ChannelID, _ uint64) error {
	return nil
}

// ListClientSessions returns the set of all client sessions known to the db. An
// optional tower ID can be used to filter out any client sessions in the
// response that do not correspond to this tower.
func (m *ClientDB) ListClientSessions(tower *wtdb.TowerID,
	opts ...wtdb.ClientSessionListOption) (
	map[wtdb.SessionID]*wtdb.ClientSession, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.listClientSessions(tower, opts...)
}

// listClientSessions returns the set of all client sessions known to the db. An
// optional tower ID can be used to filter out any client sessions in the
// response that do not correspond to this tower.
func (m *ClientDB) listClientSessions(tower *wtdb.TowerID,
	opts ...wtdb.ClientSessionListOption) (
	map[wtdb.SessionID]*wtdb.ClientSession, error) {

	cfg := wtdb.NewClientSessionCfg()
	for _, o := range opts {
		o(cfg)
	}

	sessions := make(map[wtdb.SessionID]*wtdb.ClientSession)
	for _, session := range m.activeSessions {
		session := session
		if tower != nil && *tower != session.TowerID {
			continue
		}

		if cfg.PreEvaluateFilterFn != nil &&
			!cfg.PreEvaluateFilterFn(&session) {

			continue
		}

		if cfg.PerMaxHeight != nil {
			for chanID, index := range m.ackedUpdates[session.ID] {
				cfg.PerMaxHeight(
					&session, chanID, index.MaxHeight(),
				)
			}
		}

		if cfg.PerNumAckedUpdates != nil {
			for chanID, index := range m.ackedUpdates[session.ID] {
				cfg.PerNumAckedUpdates(
					&session, chanID,
					uint16(index.NumInSet()),
				)
			}
		}

		if cfg.PerCommittedUpdate != nil {
			for _, update := range m.committedUpdates[session.ID] {
				update := update
				cfg.PerCommittedUpdate(&session, &update)
			}
		}

		if cfg.PostEvaluateFilterFn != nil &&
			!cfg.PostEvaluateFilterFn(&session) {

			continue
		}

		sessions[session.ID] = &session
	}

	return sessions, nil
}

// FetchSessionCommittedUpdates retrieves the current set of un-acked updates
// of the given session.
func (m *ClientDB) FetchSessionCommittedUpdates(id *wtdb.SessionID) (
	[]wtdb.CommittedUpdate, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	updates, ok := m.committedUpdates[*id]
	if !ok {
		return nil, wtdb.ErrClientSessionNotFound
	}

	return updates, nil
}

// IsAcked returns true if the given backup has been backed up using the given
// session.
func (m *ClientDB) IsAcked(id *wtdb.SessionID, backupID *wtdb.BackupID) (bool,
	error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	index, ok := m.ackedUpdates[*id][backupID.ChanID]
	if !ok {
		return false, nil
	}

	return index.IsInIndex(backupID.CommitHeight), nil
}

// NumAckedUpdates returns the number of backups that have been successfully
// backed up using the given session.
func (m *ClientDB) NumAckedUpdates(id *wtdb.SessionID) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var numAcked uint64

	for _, index := range m.ackedUpdates[*id] {
		numAcked += index.NumInSet()
	}

	return numAcked, nil
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
	}
	m.ackedUpdates[session.ID] = make(map[lnwire.ChannelID]*wtdb.RangeIndex)
	m.persistedAckedUpdates[session.ID] = make(
		map[lnwire.ChannelID]*mockKVStore,
	)
	m.committedUpdates[session.ID] = make([]wtdb.CommittedUpdate, 0)

	return nil
}

// NextSessionKeyIndex reserves a new session key derivation index for a
// particular tower id. The index is reserved for that tower until
// CreateClientSession is invoked for that tower and index, at which point a new
// index for that tower can be reserved. Multiple calls to this method before
// CreateClientSession is invoked should return the same index unless forceNext
// is set to true.
func (m *ClientDB) NextSessionKeyIndex(towerID wtdb.TowerID, blobType blob.Type,
	forceNext bool) (uint32, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	key := keyIndexKey{
		towerID:  towerID,
		blobType: blobType,
	}

	if !forceNext {
		if index, err := m.getSessionKeyIndex(key); err == nil {
			return index, nil
		}
	}

	// By default, we use the next available bucket sequence as the key
	// index. But if forceNext is true, then it is assumed that some data
	// loss occurred and so the sequence is incremented a by a jump of 1000
	// so that we can arrive at a brand new key index quicker.
	nextIndex := m.nextIndex + 1
	if forceNext {
		nextIndex = m.nextIndex + 1000
	}
	m.nextIndex = nextIndex
	m.indexes[key] = nextIndex

	return nextIndex, nil
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
	for _, dbUpdate := range m.committedUpdates[session.ID] {
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
	m.committedUpdates[session.ID] = append(
		m.committedUpdates[session.ID], *update,
	)
	session.SeqNum++
	m.activeSessions[*id] = session

	return session.TowerLastApplied, nil
}

// AckUpdate persists an acknowledgment for a given (session, seqnum) pair. This
// removes the update from the set of committed updates, and validates the
// lastApplied value returned from the tower.
func (m *ClientDB) AckUpdate(id *wtdb.SessionID, seqNum,
	lastApplied uint16) error {

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
	updates := m.committedUpdates[session.ID]
	for i, update := range updates {
		if update.SeqNum != seqNum {
			continue
		}

		// Add sessionID to channel.
		channel, ok := m.channels[update.BackupID.ChanID]
		if !ok {
			return wtdb.ErrChannelNotRegistered
		}
		channel.sessions[*id] = true

		// Remove the committed update from disk and mark the update as
		// acked. The tower last applied value is also recorded to send
		// along with the next update.
		copy(updates[:i], updates[i+1:])
		updates[len(updates)-1] = wtdb.CommittedUpdate{}
		m.committedUpdates[session.ID] = updates[:len(updates)-1]

		chanID := update.BackupID.ChanID
		if _, ok := m.ackedUpdates[*id][update.BackupID.ChanID]; !ok {
			index, err := wtdb.NewRangeIndex(nil)
			if err != nil {
				return err
			}

			m.ackedUpdates[*id][chanID] = index
			m.persistedAckedUpdates[*id][chanID] = newMockKVStore()
		}

		err := m.ackedUpdates[*id][chanID].Add(
			update.BackupID.CommitHeight,
			m.persistedAckedUpdates[*id][chanID],
		)
		if err != nil {
			return err
		}

		session.TowerLastApplied = lastApplied

		m.activeSessions[*id] = session
		return nil
	}

	return wtdb.ErrCommittedUpdateNotFound
}

// ListClosableSessions fetches and returns the IDs for all sessions marked as
// closable.
func (m *ClientDB) ListClosableSessions() (map[wtdb.SessionID]uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cs := make(map[wtdb.SessionID]uint32, len(m.closableSessions))
	for id, height := range m.closableSessions {
		cs[id] = height
	}

	return cs, nil
}

// FetchChanSummaries loads a mapping from all registered channels to their
// channel summaries. Only the channels that have not yet been marked as closed
// will be loaded.
func (m *ClientDB) FetchChanSummaries() (wtdb.ChannelSummaries, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	summaries := make(map[lnwire.ChannelID]wtdb.ClientChanSummary)
	for chanID, channel := range m.channels {
		// Don't load the channel if it has been marked as closed.
		if channel.closedHeight > 0 {
			continue
		}

		summaries[chanID] = wtdb.ClientChanSummary{
			SweepPkScript: cloneBytes(
				channel.summary.SweepPkScript,
			),
		}
	}

	return summaries, nil
}

// MarkChannelClosed will mark a registered channel as closed by setting
// its closed-height as the given block height. It returns a list of
// session IDs for sessions that are now considered closable due to the
// close of this channel.
func (m *ClientDB) MarkChannelClosed(chanID lnwire.ChannelID,
	blockHeight uint32) ([]wtdb.SessionID, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	channel, ok := m.channels[chanID]
	if !ok {
		return nil, wtdb.ErrChannelNotRegistered
	}

	// If there are no sessions for this channel, the channel details can be
	// deleted.
	if len(channel.sessions) == 0 {
		delete(m.channels, chanID)
		return nil, nil
	}

	// Mark the channel as closed.
	channel.closedHeight = blockHeight

	// Now iterate through all the sessions of the channel to check if any
	// of them are closeable.
	var closableSessions []wtdb.SessionID
	for sessID := range channel.sessions {
		isClosable, err := m.isSessionClosable(sessID)
		if err != nil {
			return nil, err
		}

		if !isClosable {
			continue
		}

		closableSessions = append(closableSessions, sessID)

		// Add session to "closableSessions" list and add the block
		// height that this last channel was closed in. This will be
		// used in future to determine when we should delete the
		// session.
		m.closableSessions[sessID] = blockHeight
	}

	return closableSessions, nil
}

// isSessionClosable returns true if a session is considered closable. A session
// is considered closable only if:
// 1) It has no un-acked updates
// 2) It is exhausted (ie it cant accept any more updates)
// 3) All the channels that it has acked-updates for are closed.
func (m *ClientDB) isSessionClosable(id wtdb.SessionID) (bool, error) {
	// The session is not closable if it has un-acked updates.
	if len(m.committedUpdates[id]) > 0 {
		return false, nil
	}

	sess, ok := m.activeSessions[id]
	if !ok {
		return false, wtdb.ErrClientSessionNotFound
	}

	// The session is not closable if it is not yet exhausted.
	if sess.SeqNum != sess.Policy.MaxUpdates {
		return false, nil
	}

	// Iterate over each of the channels that the session has acked-updates
	// for. If any of those channels are not closed, then the session is
	// not yet closable.
	for chanID := range m.ackedUpdates[id] {
		channel, ok := m.channels[chanID]
		if !ok {
			continue
		}

		// Channel is not yet closed, and so we can not yet delete the
		// session.
		if channel.closedHeight == 0 {
			return false, nil
		}
	}

	return true, nil
}

// GetClientSession loads the ClientSession with the given ID from the DB.
func (m *ClientDB) GetClientSession(id wtdb.SessionID,
	opts ...wtdb.ClientSessionListOption) (*wtdb.ClientSession, error) {

	cfg := wtdb.NewClientSessionCfg()
	for _, o := range opts {
		o(cfg)
	}

	session, ok := m.activeSessions[id]
	if !ok {
		return nil, wtdb.ErrClientSessionNotFound
	}

	if cfg.PerMaxHeight != nil {
		for chanID, index := range m.ackedUpdates[session.ID] {
			cfg.PerMaxHeight(&session, chanID, index.MaxHeight())
		}
	}

	if cfg.PerCommittedUpdate != nil {
		for _, update := range m.committedUpdates[session.ID] {
			update := update
			cfg.PerCommittedUpdate(&session, &update)
		}
	}

	return &session, nil
}

// DeleteSession can be called when a session should be deleted from the DB.
// All references to the session will also be deleted from the DB. Note that a
// session will only be deleted if it is considered closable.
func (m *ClientDB) DeleteSession(id wtdb.SessionID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.closableSessions[id]
	if !ok {
		return wtdb.ErrSessionNotClosable
	}

	// For each of the channels, delete the session ID entry.
	for chanID := range m.ackedUpdates[id] {
		c, ok := m.channels[chanID]
		if !ok {
			return wtdb.ErrChannelNotRegistered
		}

		delete(c.sessions, id)
	}

	delete(m.closableSessions, id)
	delete(m.activeSessions, id)

	return nil
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

	if _, ok := m.channels[chanID]; ok {
		return wtdb.ErrChannelAlreadyRegistered
	}

	m.channels[chanID] = &channel{
		summary: &wtdb.ClientChanSummary{
			SweepPkScript: cloneBytes(sweepPkScript),
		},
		sessions: make(map[wtdb.SessionID]bool),
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

type mockKVStore struct {
	kv map[uint64]uint64

	err error
}

func newMockKVStore() *mockKVStore {
	return &mockKVStore{
		kv: make(map[uint64]uint64),
	}
}

func (m *mockKVStore) Put(key, value []byte) error {
	if m.err != nil {
		return m.err
	}

	k := byteOrder.Uint64(key)
	v := byteOrder.Uint64(value)

	m.kv[k] = v

	return nil
}

func (m *mockKVStore) Delete(key []byte) error {
	if m.err != nil {
		return m.err
	}

	k := byteOrder.Uint64(key)
	delete(m.kv, k)

	return nil
}

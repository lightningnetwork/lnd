package wtdb_test

import (
	crand "crypto/rand"
	"io"
	"math/rand"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/stretchr/testify/require"
)

const blobType = blob.TypeAltruistCommit

// pseudoAddr is a fake network address to be used for testing purposes.
var pseudoAddr = &net.TCPAddr{IP: []byte{0x01, 0x00, 0x00, 0x00}, Port: 9911}

// clientDBInit is a closure used to initialize a wtclient.DB instance.
type clientDBInit func(t *testing.T) wtclient.DB

type clientDBHarness struct {
	t  *testing.T
	db wtclient.DB
}

func newClientDBHarness(t *testing.T, init clientDBInit) *clientDBHarness {
	db := init(t)

	h := &clientDBHarness{
		t:  t,
		db: db,
	}

	return h
}

func (h *clientDBHarness) insertSession(session *wtdb.ClientSession,
	expErr error) {

	h.t.Helper()

	err := h.db.CreateClientSession(session)
	require.ErrorIs(h.t, err, expErr)
}

func (h *clientDBHarness) listSessions(id *wtdb.TowerID,
	opts ...wtdb.ClientSessionListOption) map[wtdb.SessionID]*wtdb.ClientSession {

	h.t.Helper()

	sessions, err := h.db.ListClientSessions(id, opts...)
	require.NoError(h.t, err, "unable to list client sessions")

	return sessions
}

func (h *clientDBHarness) nextKeyIndex(id wtdb.TowerID, blobType blob.Type,
	forceNext bool) uint32 {

	h.t.Helper()

	index, err := h.db.NextSessionKeyIndex(id, blobType, forceNext)
	require.NoError(h.t, err, "unable to create next session key index")
	require.NotZero(h.t, index, "next key index should never be 0")

	return index
}

func (h *clientDBHarness) createTower(lnAddr *lnwire.NetAddress,
	expErr error) *wtdb.Tower {

	h.t.Helper()

	tower, err := h.db.CreateTower(lnAddr)
	require.ErrorIs(h.t, err, expErr)
	require.NotZero(h.t, tower.ID, "tower id should never be 0")

	for _, session := range h.listSessions(&tower.ID) {
		require.Equal(h.t, wtdb.CSessionActive, session.Status)
	}

	return tower
}

func (h *clientDBHarness) deactivateTower(pubKey *btcec.PublicKey,
	expErr error) {

	h.t.Helper()

	err := h.db.DeactivateTower(pubKey)
	require.ErrorIs(h.t, err, expErr)
}

func (h *clientDBHarness) listTowers(filterFn wtdb.TowerFilterFn,
	expErr error) []*wtdb.Tower {

	h.t.Helper()

	towers, err := h.db.ListTowers(filterFn)
	require.ErrorIs(h.t, err, expErr)

	return towers
}

func (h *clientDBHarness) removeTower(pubKey *btcec.PublicKey, addr net.Addr,
	hasSessions bool, expErr error) {

	h.t.Helper()

	err := h.db.RemoveTower(pubKey, addr)
	require.ErrorIs(h.t, err, expErr)

	if expErr != nil {
		return
	}

	pubKeyStr := pubKey.SerializeCompressed()

	if addr != nil {
		tower, err := h.db.LoadTower(pubKey)
		require.NoErrorf(h.t, err, "expected tower %x to still exist",
			pubKeyStr)

		removedAddr := addr.String()
		for _, towerAddr := range tower.Addresses {
			require.NotEqualf(h.t, removedAddr, towerAddr,
				"address %v not removed for tower %x",
				removedAddr, pubKeyStr)
		}
	} else {
		tower, err := h.db.LoadTower(pubKey)
		if hasSessions {
			require.NoError(h.t, err, "expected tower %x with "+
				"sessions to still exist", pubKeyStr)
		} else {
			require.Errorf(h.t, err, "expected tower %x with no "+
				"sessions to not exist", pubKeyStr)
			return
		}

		require.EqualValues(
			h.t, wtdb.TowerStatusInactive, tower.Status,
		)
	}
}

func (h *clientDBHarness) loadTower(pubKey *btcec.PublicKey,
	expErr error) *wtdb.Tower {

	h.t.Helper()

	tower, err := h.db.LoadTower(pubKey)
	require.ErrorIs(h.t, err, expErr)

	return tower
}

func (h *clientDBHarness) loadTowerByID(id wtdb.TowerID,
	expErr error) *wtdb.Tower {

	h.t.Helper()

	tower, err := h.db.LoadTowerByID(id)
	require.ErrorIs(h.t, err, expErr)

	return tower
}

func (h *clientDBHarness) terminateSession(id wtdb.SessionID, expErr error) {
	h.t.Helper()

	err := h.db.TerminateSession(id)
	require.ErrorIs(h.t, err, expErr)
}

func (h *clientDBHarness) getClientSession(id wtdb.SessionID,
	expErr error) *wtdb.ClientSession {

	h.t.Helper()

	session, err := h.db.GetClientSession(id)
	require.ErrorIs(h.t, err, expErr)

	return session
}

func (h *clientDBHarness) fetchChanInfos() wtdb.ChannelInfos {
	h.t.Helper()

	infos, err := h.db.FetchChanInfos()
	require.NoError(h.t, err)

	return infos
}

func (h *clientDBHarness) registerChan(chanID lnwire.ChannelID,
	sweepPkScript []byte, expErr error) {

	h.t.Helper()

	err := h.db.RegisterChannel(chanID, sweepPkScript)
	require.ErrorIs(h.t, err, expErr)
}

func (h *clientDBHarness) commitUpdate(id *wtdb.SessionID,
	update *wtdb.CommittedUpdate, expErr error) uint16 {

	h.t.Helper()

	lastApplied, err := h.db.CommitUpdate(id, update)
	require.ErrorIs(h.t, err, expErr)

	return lastApplied
}

func (h *clientDBHarness) ackUpdate(id *wtdb.SessionID, seqNum uint16,
	lastApplied uint16, expErr error) {

	h.t.Helper()

	err := h.db.AckUpdate(id, seqNum, lastApplied)
	require.ErrorIs(h.t, err, expErr)
}

func (h *clientDBHarness) deleteCommittedUpdates(id *wtdb.SessionID,
	expErr error) {

	h.t.Helper()

	err := h.db.DeleteCommittedUpdates(id)
	require.ErrorIs(h.t, err, expErr)
}

func (h *clientDBHarness) markChannelClosed(id lnwire.ChannelID,
	blockHeight uint32, expErr error) []wtdb.SessionID {

	h.t.Helper()

	closableSessions, err := h.db.MarkChannelClosed(id, blockHeight)
	require.ErrorIs(h.t, err, expErr)

	return closableSessions
}

func (h *clientDBHarness) listClosableSessions(
	expErr error) map[wtdb.SessionID]uint32 {

	h.t.Helper()

	closableSessions, err := h.db.ListClosableSessions()
	require.ErrorIs(h.t, err, expErr)

	return closableSessions
}

func (h *clientDBHarness) deleteSession(id wtdb.SessionID, expErr error) {
	h.t.Helper()

	err := h.db.DeleteSession(id)
	require.ErrorIs(h.t, err, expErr)
}

// newTower is a helper function that creates a new tower with a randomly
// generated public key and inserts it into the client DB.
func (h *clientDBHarness) newTower() *wtdb.Tower {
	h.t.Helper()

	pk, err := randPubKey()
	require.NoError(h.t, err)

	// Insert a random tower into the database.
	return h.createTower(&lnwire.NetAddress{
		IdentityKey: pk,
		Address:     pseudoAddr,
	}, nil)
}

func (h *clientDBHarness) fetchSessionCommittedUpdates(id *wtdb.SessionID,
	expErr error) []wtdb.CommittedUpdate {

	h.t.Helper()

	updates, err := h.db.FetchSessionCommittedUpdates(id)
	if err != expErr {
		h.t.Fatalf("expected fetch session committed updates error: "+
			"%v, got: %v", expErr, err)
	}

	return updates
}

func (h *clientDBHarness) isAcked(id *wtdb.SessionID, backupID *wtdb.BackupID,
	expErr error) bool {

	h.t.Helper()

	isAcked, err := h.db.IsAcked(id, backupID)
	require.ErrorIs(h.t, err, expErr)

	return isAcked
}

func (h *clientDBHarness) numAcked(id *wtdb.SessionID, expErr error) uint64 {
	h.t.Helper()

	numAcked, err := h.db.NumAckedUpdates(id)
	require.ErrorIs(h.t, err, expErr)

	return numAcked
}

// testCreateClientSession asserts various conditions regarding the creation of
// a new ClientSession. The test asserts:
//   - client sessions can only be created if a session key index is reserved.
//   - client sessions cannot be created with an incorrect session key index .
//   - inserting duplicate sessions fails.
func testCreateClientSession(h *clientDBHarness) {
	const blobType = blob.TypeAltruistAnchorCommit

	tower := h.newTower()

	// Create a test client session to insert.
	session := &wtdb.ClientSession{
		ClientSessionBody: wtdb.ClientSessionBody{
			TowerID: tower.ID,
			Policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType: blobType,
				},
				MaxUpdates: 100,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
		},
		ID: wtdb.SessionID([33]byte{0x01}),
	}

	// First, assert that this session is not already present in the
	// database.
	_, ok := h.listSessions(nil)[session.ID]
	require.Falsef(h.t, ok, "session for id %x should not exist yet",
		session.ID)

	// Attempting to insert the client session without reserving a session
	// key index should fail.
	h.insertSession(session, wtdb.ErrNoReservedKeyIndex)

	// Now, reserve a session key for this tower.
	keyIndex := h.nextKeyIndex(session.TowerID, blobType, false)

	// The client session hasn't been updated with the reserved key index
	// (since it's still zero). Inserting should fail due to the mismatch.
	h.insertSession(session, wtdb.ErrIncorrectKeyIndex)

	// Reserve another key for the same index. Since no session has been
	// successfully created, it should return the same index to maintain
	// idempotency across restarts.
	keyIndex2 := h.nextKeyIndex(session.TowerID, blobType, false)
	require.Equalf(h.t, keyIndex, keyIndex2, "next key index should "+
		"be idempotent: want: %v, got %v", keyIndex, keyIndex2)

	// Now, set the client session's key index so that it is proper and
	// insert it. This should succeed.
	session.KeyIndex = keyIndex
	h.insertSession(session, nil)

	// Verify that the session now exists in the database.
	_, ok = h.listSessions(nil)[session.ID]
	require.Truef(h.t, ok, "session for id %x should exist now", session.ID)

	// Attempt to insert the session again, which should fail due to the
	// session already existing.
	h.insertSession(session, wtdb.ErrClientSessionAlreadyExists)

	// Assert that reserving another key index succeeds with a different key
	// index, now that the first one has been finalized.
	keyIndex3 := h.nextKeyIndex(session.TowerID, blobType, false)
	require.NotEqualf(h.t, keyIndex, keyIndex3, "key index still "+
		"reserved after creating session")

	// Show that calling NextSessionKeyIndex again now will result in the
	// same key being returned as long as forceNext remains false.
	keyIndex4 := h.nextKeyIndex(session.TowerID, blobType, false)
	require.Equal(h.t, keyIndex3, keyIndex4)

	// Finally, assert that if the forceNext param of the
	// NextSessionKeyIndex method is true, then the key index returned will
	// be different.
	keyIndex5 := h.nextKeyIndex(session.TowerID, blobType, true)
	require.NotEqual(h.t, keyIndex5, keyIndex4)
	require.Equal(h.t, keyIndex3+1000, keyIndex5)
}

// testFilterClientSessions asserts that we can correctly filter client sessions
// for a specific tower.
func testFilterClientSessions(h *clientDBHarness) {
	// We'll create three client sessions, the first two belonging to one
	// tower, and the last belonging to another one.
	const numSessions = 3
	const blobType = blob.TypeAltruistCommit
	towerSessions := make(map[wtdb.TowerID][]wtdb.SessionID)
	for i := 0; i < numSessions; i++ {
		tower := h.newTower()
		keyIndex := h.nextKeyIndex(tower.ID, blobType, false)
		sessionID := wtdb.SessionID([33]byte{byte(i)})
		h.insertSession(&wtdb.ClientSession{
			ClientSessionBody: wtdb.ClientSessionBody{
				TowerID: tower.ID,
				Policy: wtpolicy.Policy{
					TxPolicy: wtpolicy.TxPolicy{
						BlobType: blobType,
					},
					MaxUpdates: 100,
				},
				RewardPkScript: []byte{0x01, 0x02, 0x03},
				KeyIndex:       keyIndex,
			},
			ID: sessionID,
		}, nil)
		towerSessions[tower.ID] = append(
			towerSessions[tower.ID], sessionID,
		)
	}

	// We should see the expected sessions for each tower when filtering
	// them.
	for towerID, expectedSessions := range towerSessions {
		sessions := h.listSessions(&towerID)
		require.Len(h.t, sessions, len(expectedSessions))

		for _, expectedSession := range expectedSessions {
			_, ok := sessions[expectedSession]
			require.Truef(h.t, ok, "expected session %v for "+
				"tower %v", expectedSession, towerID)
		}
	}
}

// testCreateTower asserts the behavior of creating new Tower objects within the
// database, and that the latest address is always prepended to the list of
// known addresses for the tower.
func testCreateTower(h *clientDBHarness) {
	// Test that loading a tower with an arbitrary tower id fails.
	h.loadTowerByID(20, wtdb.ErrTowerNotFound)

	tower := h.newTower()
	require.Len(h.t, tower.Addresses, 1)
	towerAddr := &lnwire.NetAddress{
		IdentityKey: tower.IdentityKey,
		Address:     tower.Addresses[0],
	}

	// Load the tower from the database and assert that it matches the tower
	// we created.
	tower2 := h.loadTowerByID(tower.ID, nil)
	require.Equal(h.t, tower, tower2)

	tower2 = h.loadTower(tower.IdentityKey, nil)
	require.Equal(h.t, tower, tower2)

	// Insert the address again into the database. Since the address is the
	// same, this should result in an unmodified tower record.
	towerDupAddr := h.createTower(towerAddr, nil)
	require.Lenf(h.t, towerDupAddr.Addresses, 1, "duplicate address "+
		"should be deduped")

	require.Equal(h.t, tower, towerDupAddr)

	// Generate a new address for this tower.
	addr2 := &net.TCPAddr{IP: []byte{0x02, 0x00, 0x00, 0x00}, Port: 9911}

	lnAddr2 := &lnwire.NetAddress{
		IdentityKey: tower.IdentityKey,
		Address:     addr2,
	}

	// Insert the updated address, which should produce a tower with a new
	// address.
	towerNewAddr := h.createTower(lnAddr2, nil)

	// Load the tower from the database, and assert that it matches the
	// tower returned from creation.
	towerNewAddr2 := h.loadTowerByID(tower.ID, nil)
	require.Equal(h.t, towerNewAddr, towerNewAddr2)

	towerNewAddr2 = h.loadTower(tower.IdentityKey, nil)
	require.Equal(h.t, towerNewAddr, towerNewAddr2)

	// Assert that there are now two addresses on the tower object.
	require.Lenf(h.t, towerNewAddr.Addresses, 2, "new address should be "+
		"added")

	// Finally, assert that the new address was prepended since it is deemed
	// fresher.
	require.Equal(h.t, tower.Addresses, towerNewAddr.Addresses[1:])
}

// testRemoveTower asserts the behavior of removing Tower objects as a whole and
// removing addresses from Tower objects within the database.
func testRemoveTower(h *clientDBHarness) {
	// Generate a random public key we'll use for our tower.
	pk, err := randPubKey()
	require.NoError(h.t, err)

	// Removing a tower that does not exist within the database should
	// result in a NOP.
	h.removeTower(pk, nil, false, nil)

	// We'll create a tower with two addresses.
	addr1 := &net.TCPAddr{IP: []byte{0x01, 0x00, 0x00, 0x00}, Port: 9911}
	addr2 := &net.TCPAddr{IP: []byte{0x02, 0x00, 0x00, 0x00}, Port: 9911}
	h.createTower(&lnwire.NetAddress{
		IdentityKey: pk,
		Address:     addr1,
	}, nil)
	h.createTower(&lnwire.NetAddress{
		IdentityKey: pk,
		Address:     addr2,
	}, nil)

	// We'll then remove the second address. We should now only see the
	// first.
	h.removeTower(pk, addr2, false, nil)

	// We'll then remove the first address. We should now see that the tower
	// has no addresses left.
	h.removeTower(pk, addr1, false, wtdb.ErrLastTowerAddr)

	// Removing the tower as a whole from the database should succeed since
	// there aren't any active sessions for it.
	h.removeTower(pk, nil, false, nil)

	// We'll then recreate the tower, but this time we'll create a session
	// for it.
	tower := h.createTower(&lnwire.NetAddress{
		IdentityKey: pk,
		Address:     addr1,
	}, nil)

	const blobType = blob.TypeAltruistCommit
	session := &wtdb.ClientSession{
		ClientSessionBody: wtdb.ClientSessionBody{
			TowerID: tower.ID,
			Policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType: blobType,
				},
				MaxUpdates: 100,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
			KeyIndex: h.nextKeyIndex(
				tower.ID, blobType, false,
			),
		},
		ID: wtdb.SessionID([33]byte{0x01}),
	}
	h.insertSession(session, nil)
	update := randCommittedUpdate(h.t, 1)
	h.registerChan(update.BackupID.ChanID, nil, nil)
	h.commitUpdate(&session.ID, update, nil)

	// We should not be able to fully remove it from the database since
	// there's a session, and it has unacked updates.
	h.removeTower(pk, nil, true, wtdb.ErrTowerUnackedUpdates)

	// Removing the tower after all sessions no longer have unacked updates
	// should succeed.
	h.ackUpdate(&session.ID, 1, 1, nil)
	h.removeTower(pk, nil, true, nil)
}

func testTerminateSession(h *clientDBHarness) {
	const blobType = blob.TypeAltruistCommit

	tower := h.newTower()

	// Create a new session that the updates in this will be tied to.
	session := &wtdb.ClientSession{
		ClientSessionBody: wtdb.ClientSessionBody{
			TowerID: tower.ID,
			Policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType: blobType,
				},
				MaxUpdates: 100,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
		},
		ID: wtdb.SessionID([33]byte{0x03}),
	}

	// Reserve a session key and insert the client session.
	session.KeyIndex = h.nextKeyIndex(session.TowerID, blobType, false)
	h.insertSession(session, nil)

	// Commit to a random update at seqnum 1.
	update1 := randCommittedUpdate(h.t, 1)
	h.registerChan(update1.BackupID.ChanID, nil, nil)
	lastApplied := h.commitUpdate(&session.ID, update1, nil)
	require.Zero(h.t, lastApplied)

	// Terminating the session now should fail since the session has an
	// un-acked update.
	h.terminateSession(session.ID, wtdb.ErrSessionHasUnackedUpdates)

	// Fetch the session and assert that the status is still active.
	sess := h.getClientSession(session.ID, nil)
	require.Equal(h.t, wtdb.CSessionActive, sess.Status)

	// Delete the update.
	h.deleteCommittedUpdates(&session.ID, nil)

	// Terminating the session now should succeed.
	h.terminateSession(session.ID, nil)

	// Fetch the session again and assert that its status is now Terminal.
	sess = h.getClientSession(session.ID, nil)
	require.Equal(h.t, wtdb.CSessionTerminal, sess.Status)
}

// testTowerStatusChange tests that the Tower status is updated accordingly
// given a variety of commands.
func testTowerStatusChange(h *clientDBHarness) {
	// Create a new tower.
	pk, err := randPubKey()
	require.NoError(h.t, err)

	towerAddr := &lnwire.NetAddress{
		IdentityKey: pk,
		Address: &net.TCPAddr{
			IP: []byte{0x01, 0x00, 0x00, 0x00}, Port: 9911,
		},
	}

	tower := h.createTower(towerAddr, nil)

	// Add a new session.
	session := h.randSession(h.t, tower.ID, 100)
	h.insertSession(session, nil)

	// assertTowerStatus is a helper function that will assert that the
	// tower's status is as expected.
	assertTowerStatus := func(status wtdb.TowerStatus) {
		activeFilter := func(tower *wtdb.Tower) bool {
			return tower.Status == status
		}

		towers := h.listTowers(activeFilter, nil)
		require.Len(h.t, towers, 1)
		require.EqualValues(h.t, towers[0].Status, status)
	}

	// assertSessionStatus is a helper that will assert that the session's
	// status is as expected
	assertSessionStatus := func(status wtdb.CSessionStatus) {
		sessions := h.listSessions(&tower.ID)
		require.Len(h.t, sessions, 1)
		for _, sess := range sessions {
			require.EqualValues(h.t, sess.Status, status)
		}
	}

	// Initially, the tower and session should be active.
	assertTowerStatus(wtdb.TowerStatusActive)
	assertSessionStatus(wtdb.CSessionActive)

	// Removing the tower should change its status but its session
	// status should remain active.
	h.removeTower(tower.IdentityKey, nil, true, nil)
	assertTowerStatus(wtdb.TowerStatusInactive)
	assertSessionStatus(wtdb.CSessionActive)

	// Re-adding the tower in some way should re-active it and its session.
	h.createTower(towerAddr, nil)
	assertTowerStatus(wtdb.TowerStatusActive)
	assertSessionStatus(wtdb.CSessionActive)

	// Deactivating the tower should change its status but its session
	// status should remain active.
	h.deactivateTower(tower.IdentityKey, nil)
	assertTowerStatus(wtdb.TowerStatusInactive)
	assertSessionStatus(wtdb.CSessionActive)
}

// testChanSummaries tests the process of a registering a channel and its
// associated sweep pkscript.
func testChanSummaries(h *clientDBHarness) {
	// First, assert that this channel is not already registered.
	var chanID lnwire.ChannelID
	_, ok := h.fetchChanInfos()[chanID]
	require.Falsef(h.t, ok, "pkscript for channel %x should not exist yet",
		chanID)

	// Generate a random sweep pkscript and register it for this channel.
	expPkScript := make([]byte, 22)
	_, err := io.ReadFull(crand.Reader, expPkScript)
	require.NoError(h.t, err)

	h.registerChan(chanID, expPkScript, nil)

	// Assert that the channel exists and that its sweep pkscript matches
	// the one we registered.
	summary, ok := h.fetchChanInfos()[chanID]
	require.Truef(h.t, ok, "pkscript for channel %x should not exist yet",
		chanID)
	require.Equal(h.t, expPkScript, summary.SweepPkScript)

	// Finally, assert that re-registering the same channel produces a
	// failure.
	h.registerChan(chanID, expPkScript, wtdb.ErrChannelAlreadyRegistered)
}

// testCommitUpdate tests the behavior of CommitUpdate and
// DeleteCommittedUpdate.
func testCommitUpdate(h *clientDBHarness) {
	const blobType = blob.TypeAltruistCommit

	tower := h.newTower()
	session := &wtdb.ClientSession{
		ClientSessionBody: wtdb.ClientSessionBody{
			TowerID: tower.ID,
			Policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType: blobType,
				},
				MaxUpdates: 100,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
		},
		ID: wtdb.SessionID([33]byte{0x02}),
	}

	// Generate a random update and try to commit before inserting the
	// session, which should fail.
	update1 := randCommittedUpdate(h.t, 1)
	h.commitUpdate(&session.ID, update1, wtdb.ErrClientSessionNotFound)
	h.fetchSessionCommittedUpdates(
		&session.ID, wtdb.ErrClientSessionNotFound,
	)

	// Reserve a session key index and insert the session.
	session.KeyIndex = h.nextKeyIndex(session.TowerID, blobType, false)
	h.insertSession(session, nil)

	// Now, try to commit the update that failed initially which should
	// succeed. The lastApplied value should be 0 since we have not received
	// an ack from the tower.
	lastApplied := h.commitUpdate(&session.ID, update1, nil)
	require.Zero(h.t, lastApplied)

	// Assert that the committed update appears in the client session's
	// CommittedUpdates map when loaded from disk and that there are no
	// AckedUpdates.
	h.assertUpdates(session.ID, []wtdb.CommittedUpdate{*update1}, nil)

	// Try to commit the same update, which should succeed due to
	// idempotency (which is preserved when the breach hint is identical to
	// the on-disk update's hint). The lastApplied value should remain
	// unchanged.
	lastApplied2 := h.commitUpdate(&session.ID, update1, nil)
	require.Equal(h.t, lastApplied, lastApplied2)

	// Assert that the loaded ClientSession is the same as before.
	h.assertUpdates(session.ID, []wtdb.CommittedUpdate{*update1}, nil)

	// Generate another random update and try to commit it at the identical
	// sequence number. Since the breach hint has changed, this should fail.
	update2 := randCommittedUpdate(h.t, 1)
	h.commitUpdate(&session.ID, update2, wtdb.ErrUpdateAlreadyCommitted)

	// Next, insert the new update at the next unallocated sequence number
	// which should succeed.
	update2.SeqNum = 2
	lastApplied3 := h.commitUpdate(&session.ID, update2, nil)
	require.Equal(h.t, lastApplied, lastApplied3)

	// Check that both updates now appear as committed on the ClientSession
	// loaded from disk.
	h.assertUpdates(session.ID, []wtdb.CommittedUpdate{
		*update1,
		*update2,
	}, nil)

	// Finally, create one more random update and try to commit it at index
	// 4, which should be rejected since 3 is the next slot the database
	// expects.
	update4 := randCommittedUpdate(h.t, 4)
	h.commitUpdate(&session.ID, update4, wtdb.ErrCommitUnorderedUpdate)

	// Assert that the ClientSession loaded from disk remains unchanged.
	h.assertUpdates(session.ID, []wtdb.CommittedUpdate{
		*update1,
		*update2,
	}, nil)

	// We will now also test that the DeleteCommittedUpdates method also
	// works.
	h.deleteCommittedUpdates(&session.ID, nil)
	h.assertUpdates(session.ID, []wtdb.CommittedUpdate{}, nil)
}

// testRogueUpdates asserts that rogue updates (updates for channels that are
// backed up after the channel has been closed and the channel details deleted
// from the DB) are handled correctly.
func testRogueUpdates(h *clientDBHarness) {
	const maxUpdates = 5

	tower := h.newTower()

	// Create and insert a new session.
	session1 := h.randSession(h.t, tower.ID, maxUpdates)
	h.insertSession(session1, nil)

	// Create a new channel and register it.
	chanID1 := randChannelID(h.t)
	h.registerChan(chanID1, nil, nil)

	// Num acked updates should be 0.
	require.Zero(h.t, h.numAcked(&session1.ID, nil))

	// Commit and ACK enough updates for this channel to fill the session.
	for i := 1; i <= maxUpdates; i++ {
		update := randCommittedUpdateForChanWithHeight(
			h.t, chanID1, uint16(i), uint64(i),
		)
		lastApplied := h.commitUpdate(&session1.ID, update, nil)
		h.ackUpdate(&session1.ID, uint16(i), lastApplied, nil)
	}

	// Num acked updates should now be 5.
	require.EqualValues(h.t, maxUpdates, h.numAcked(&session1.ID, nil))

	// Commit one more update for the channel but this time do not ACK it.
	// This update will be put in a new session since the previous one has
	// been exhausted.
	session2 := h.randSession(h.t, tower.ID, maxUpdates)
	sess2Seq := 1
	h.insertSession(session2, nil)
	update := randCommittedUpdateForChanWithHeight(
		h.t, chanID1, uint16(sess2Seq), uint64(maxUpdates+1),
	)
	lastApplied := h.commitUpdate(&session2.ID, update, nil)

	// Session 2 should not have any acked updates yet.
	require.Zero(h.t, h.numAcked(&session2.ID, nil))

	// There should currently be no closable sessions.
	require.Empty(h.t, h.listClosableSessions(nil))

	// Now mark the channel as closed.
	h.markChannelClosed(chanID1, 1, nil)

	// Assert that session 1 is now seen as closable.
	closableSessionsMap := h.listClosableSessions(nil)
	require.Len(h.t, closableSessionsMap, 1)
	_, ok := closableSessionsMap[session1.ID]
	require.True(h.t, ok)

	// Delete session 1.
	h.deleteSession(session1.ID, nil)

	// Now try to ACK the update for the channel. This should succeed and
	// the update should be considered a rogue update.
	h.ackUpdate(&session2.ID, uint16(sess2Seq), lastApplied, nil)

	// Show that the number of acked updates is now 1.
	require.EqualValues(h.t, 1, h.numAcked(&session2.ID, nil))

	// We also want to test the extreme case where all the updates for a
	// particular session are rogue updates. In this case, the session
	// should be seen as closable if it is saturated.

	// First show that the session is not yet considered closable.
	require.Empty(h.t, h.listClosableSessions(nil))

	// Then, let's continue adding rogue updates for the closed channel to
	// session 2.
	for i := maxUpdates + 2; i <= maxUpdates*2; i++ {
		sess2Seq++

		update := randCommittedUpdateForChanWithHeight(
			h.t, chanID1, uint16(sess2Seq), uint64(i),
		)
		lastApplied := h.commitUpdate(&session2.ID, update, nil)
		h.ackUpdate(&session2.ID, uint16(sess2Seq), lastApplied, nil)
	}

	// At this point, session 2 is saturated with rogue updates. Assert that
	// it is now closable.
	closableSessionsMap = h.listClosableSessions(nil)
	require.Len(h.t, closableSessionsMap, 1)
}

// testMaxCommitmentHeights tests that the max known commitment height of a
// channel is properly persisted.
func testMaxCommitmentHeights(h *clientDBHarness) {
	const maxUpdates = 5
	t := h.t

	// Initially, we expect no channels.
	infos := h.fetchChanInfos()
	require.Empty(t, infos)

	// Create a new tower.
	tower := h.newTower()

	// Create and insert a new session.
	session1 := h.randSession(t, tower.ID, maxUpdates)
	h.insertSession(session1, nil)

	// Create a new channel and register it.
	chanID1 := randChannelID(t)
	h.registerChan(chanID1, nil, nil)

	// At this point, we expect one channel to be returned from
	// fetchChanInfos but with an unset max height.
	infos = h.fetchChanInfos()
	require.Len(t, infos, 1)

	info, ok := infos[chanID1]
	require.True(t, ok)
	require.True(t, info.MaxHeight.IsNone())

	// Commit and ACK some updates for this channel.
	for i := 1; i <= maxUpdates; i++ {
		update := randCommittedUpdateForChanWithHeight(
			t, chanID1, uint16(i), uint64(i-1),
		)
		lastApplied := h.commitUpdate(&session1.ID, update, nil)
		h.ackUpdate(&session1.ID, uint16(i), lastApplied, nil)
	}

	// Assert that the max height has now been set accordingly for this
	// channel.
	infos = h.fetchChanInfos()
	require.Len(t, infos, 1)

	info, ok = infos[chanID1]
	require.True(t, ok)
	require.True(t, info.MaxHeight.IsSome())
	info.MaxHeight.WhenSome(func(u uint64) {
		require.EqualValues(t, maxUpdates-1, u)
	})
}

// testMarkChannelClosed asserts the behaviour of MarkChannelClosed.
func testMarkChannelClosed(h *clientDBHarness) {
	tower := h.newTower()

	// Create channel 1.
	chanID1 := randChannelID(h.t)

	// Since we have not yet registered the channel, we expect an error
	// when attempting to mark it as closed.
	h.markChannelClosed(chanID1, 1, wtdb.ErrChannelNotRegistered)

	// Now register the channel.
	h.registerChan(chanID1, nil, nil)

	// Since there are still no sessions that would have updates for the
	// channel, marking it as closed now should succeed.
	h.markChannelClosed(chanID1, 1, nil)

	// Register channel 2.
	chanID2 := randChannelID(h.t)
	h.registerChan(chanID2, nil, nil)

	// Create session1 with MaxUpdates set to 5.
	session1 := h.randSession(h.t, tower.ID, 5)
	h.insertSession(session1, nil)

	// Add an update for channel 2 in session 1 and ack it too.
	update := randCommittedUpdateForChannel(h.t, chanID2, 1)
	lastApplied := h.commitUpdate(&session1.ID, update, nil)
	require.Zero(h.t, lastApplied)
	h.ackUpdate(&session1.ID, 1, 1, nil)

	// Marking channel 2 now should not result in any closable sessions
	// since session 1 is not yet exhausted.
	sl := h.markChannelClosed(chanID2, 1, nil)
	require.Empty(h.t, sl)

	// Create channel 3 and 4.
	chanID3 := randChannelID(h.t)
	h.registerChan(chanID3, nil, nil)

	chanID4 := randChannelID(h.t)
	h.registerChan(chanID4, nil, nil)

	// Add an update for channel 4 and ack it.
	update = randCommittedUpdateForChannel(h.t, chanID4, 2)
	lastApplied = h.commitUpdate(&session1.ID, update, nil)
	require.EqualValues(h.t, 1, lastApplied)
	h.ackUpdate(&session1.ID, 2, 2, nil)

	// Add an update for channel 3 in session 1. But dont ack it yet.
	update = randCommittedUpdateForChannel(h.t, chanID2, 3)
	lastApplied = h.commitUpdate(&session1.ID, update, nil)
	require.EqualValues(h.t, 2, lastApplied)

	// Mark channel 4 as closed & assert that session 1 is not seen as
	// closable since it still has committed updates.
	sl = h.markChannelClosed(chanID4, 1, nil)
	require.Empty(h.t, sl)

	// Now ack the update we added above.
	h.ackUpdate(&session1.ID, 3, 3, nil)

	// Mark channel 3 as closed & assert that session 1 is still not seen as
	// closable since it is not yet exhausted.
	sl = h.markChannelClosed(chanID3, 1, nil)
	require.Empty(h.t, sl)

	// Create channel 5 and 6.
	chanID5 := randChannelID(h.t)
	h.registerChan(chanID5, nil, nil)

	chanID6 := randChannelID(h.t)
	h.registerChan(chanID6, nil, nil)

	// Add an update for channel 5 and ack it.
	update = randCommittedUpdateForChannel(h.t, chanID5, 4)
	lastApplied = h.commitUpdate(&session1.ID, update, nil)
	require.EqualValues(h.t, 3, lastApplied)
	h.ackUpdate(&session1.ID, 4, 4, nil)

	// Add an update for channel 6 and ack it.
	update = randCommittedUpdateForChannel(h.t, chanID6, 5)
	lastApplied = h.commitUpdate(&session1.ID, update, nil)
	require.EqualValues(h.t, 4, lastApplied)
	h.ackUpdate(&session1.ID, 5, 5, nil)

	// The session is now exhausted.
	// If we now close channel 5, session 1 should still not be closable
	// since it has an update for channel 6 which is still open.
	sl = h.markChannelClosed(chanID5, 1, nil)
	require.Empty(h.t, sl)
	require.Empty(h.t, h.listClosableSessions(nil))

	// Also check that attempting to delete the session will fail since it
	// is not yet considered closable.
	h.deleteSession(session1.ID, wtdb.ErrSessionNotClosable)

	// Finally, if we close channel 6, session 1 _should_ be in the closable
	// list.
	sl = h.markChannelClosed(chanID6, 100, nil)
	require.ElementsMatch(h.t, sl, []wtdb.SessionID{session1.ID})
	slMap := h.listClosableSessions(nil)
	require.InDeltaMapValues(h.t, slMap, map[wtdb.SessionID]uint32{
		session1.ID: 100,
	}, 0)

	// Assert that we now can delete the session.
	h.deleteSession(session1.ID, nil)
	require.Empty(h.t, h.listClosableSessions(nil))

	// We also want to test that a session can be deleted if it has been
	// marked as terminal.

	// Create session2 with MaxUpdates set to 5.
	session2 := h.randSession(h.t, tower.ID, 5)
	h.insertSession(session2, nil)

	// Create and register channel 7.
	chanID7 := randChannelID(h.t)
	h.registerChan(chanID7, nil, nil)

	// Add two updates for channel 7 in session 2. Ack one of them so that
	// the mapping from this channel to this session is created but don't
	// ack the other since we want to delete the committed update later.
	update = randCommittedUpdateForChannel(h.t, chanID7, 1)
	h.commitUpdate(&session2.ID, update, nil)
	h.ackUpdate(&session2.ID, 1, 1, nil)

	update = randCommittedUpdateForChannel(h.t, chanID7, 2)
	h.commitUpdate(&session2.ID, update, nil)

	// Check that attempting to delete the session will fail since it is not
	// yet considered closable.
	h.deleteSession(session2.ID, wtdb.ErrSessionNotClosable)

	// Now delete the added committed updates. This should put the
	// session in the terminal state after which we should be able to
	// delete the session.
	h.deleteCommittedUpdates(&session2.ID, nil)

	// Marking channel 7 as closed will now return session 2 since it has
	// been marked as terminal.
	sl = h.markChannelClosed(chanID7, 1, nil)
	require.ElementsMatch(h.t, sl, []wtdb.SessionID{session2.ID})

	// We should now be able to delete the session.
	h.deleteSession(session2.ID, nil)
}

// testAckUpdate asserts the behavior of AckUpdate.
func testAckUpdate(h *clientDBHarness) {
	const blobType = blob.TypeAltruistCommit

	tower := h.newTower()

	// Create a new session that the updates in this will be tied to.
	session := &wtdb.ClientSession{
		ClientSessionBody: wtdb.ClientSessionBody{
			TowerID: tower.ID,
			Policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType: blobType,
				},
				MaxUpdates: 100,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
		},
		ID: wtdb.SessionID([33]byte{0x03}),
	}

	// Try to ack an update before inserting the client session, which
	// should fail.
	h.ackUpdate(&session.ID, 1, 0, wtdb.ErrClientSessionNotFound)

	// Reserve a session key and insert the client session.
	session.KeyIndex = h.nextKeyIndex(session.TowerID, blobType, false)
	h.insertSession(session, nil)

	// Now, try to ack update 1. This should fail since update 1 was never
	// committed.
	h.ackUpdate(&session.ID, 1, 0, wtdb.ErrCommittedUpdateNotFound)

	// Commit to a random update at seqnum 1.
	update1 := randCommittedUpdate(h.t, 1)

	h.registerChan(update1.BackupID.ChanID, nil, nil)
	lastApplied := h.commitUpdate(&session.ID, update1, nil)
	require.Zero(h.t, lastApplied)

	// Acking seqnum 1 should succeed.
	h.ackUpdate(&session.ID, 1, 1, nil)

	// Acking seqnum 1 again should fail.
	h.ackUpdate(&session.ID, 1, 1, wtdb.ErrCommittedUpdateNotFound)

	// Acking a valid seqnum with a reverted last applied value should fail.
	h.ackUpdate(&session.ID, 1, 0, wtdb.ErrLastAppliedReversion)

	// Acking with a last applied greater than any allocated seqnum should
	// fail.
	h.ackUpdate(&session.ID, 4, 3, wtdb.ErrUnallocatedLastApplied)

	// Assert that the ClientSession loaded from disk has one update in it's
	// AckedUpdates map, and that the committed update has been removed.
	h.assertUpdates(session.ID, nil, map[uint16]wtdb.BackupID{
		1: update1.BackupID,
	})

	// Commit to another random update, and assert that the last applied
	// value is 1, since this was what was provided in the last successful
	// ack.
	update2 := randCommittedUpdate(h.t, 2)
	h.registerChan(update2.BackupID.ChanID, nil, nil)
	lastApplied = h.commitUpdate(&session.ID, update2, nil)
	require.EqualValues(h.t, 1, lastApplied)

	// Ack seqnum 2.
	h.ackUpdate(&session.ID, 2, 2, nil)

	// Assert that both updates exist as AckedUpdates when loaded from disk.
	h.assertUpdates(session.ID, nil, map[uint16]wtdb.BackupID{
		1: update1.BackupID,
		2: update2.BackupID,
	})

	// Acking again with a lower last applied should fail.
	h.ackUpdate(&session.ID, 2, 1, wtdb.ErrLastAppliedReversion)

	// Acking an unallocated seqnum should fail.
	h.ackUpdate(&session.ID, 4, 2, wtdb.ErrCommittedUpdateNotFound)

	// Acking with a last applied greater than any allocated seqnum should
	// fail.
	h.ackUpdate(&session.ID, 4, 3, wtdb.ErrUnallocatedLastApplied)
}

func (h *clientDBHarness) assertUpdates(id wtdb.SessionID,
	expectedPending []wtdb.CommittedUpdate,
	expectedAcked map[uint16]wtdb.BackupID) {

	committedUpdates := h.fetchSessionCommittedUpdates(&id, nil)
	checkCommittedUpdates(h.t, committedUpdates, expectedPending)

	// Check acked updates.
	numAcked := h.numAcked(&id, nil)
	require.EqualValues(h.t, len(expectedAcked), numAcked)
	for _, backupID := range expectedAcked {
		isAcked := h.isAcked(&id, &backupID, nil)
		require.True(h.t, isAcked)
	}
}

// checkCommittedUpdates asserts that the CommittedUpdates on session match the
// expUpdates provided.
func checkCommittedUpdates(t *testing.T, actualUpdates,
	expUpdates []wtdb.CommittedUpdate) {

	t.Helper()

	// We promote nil expUpdates to an initialized slice since the database
	// should never return a nil slice. This promotion is done purely out of
	// convenience for the testing framework.
	if expUpdates == nil {
		expUpdates = make([]wtdb.CommittedUpdate, 0)
	}

	require.Equal(t, expUpdates, actualUpdates)
}

// TestClientDB asserts the behavior of a fresh client db, a reopened client db,
// and the mock implementation. This ensures that all databases function
// identically, especially in the negative paths.
func TestClientDB(t *testing.T) {
	dbCfg := &kvdb.BoltConfig{DBTimeout: kvdb.DefaultDBTimeout}
	dbs := []struct {
		name string
		init clientDBInit
	}{
		{
			name: "fresh clientdb",
			init: func(t *testing.T) wtclient.DB {
				bdb, err := wtdb.NewBoltBackendCreator(
					true, t.TempDir(), "wtclient.db",
				)(dbCfg)
				require.NoError(t, err)

				db, err := wtdb.OpenClientDB(bdb)
				require.NoError(t, err)

				t.Cleanup(func() {
					db.Close()
				})

				return db
			},
		},
		{
			name: "reopened clientdb",
			init: func(t *testing.T) wtclient.DB {
				path := t.TempDir()

				bdb, err := wtdb.NewBoltBackendCreator(
					true, path, "wtclient.db",
				)(dbCfg)
				require.NoError(t, err)

				db, err := wtdb.OpenClientDB(bdb)
				require.NoError(t, err)
				db.Close()

				bdb, err = wtdb.NewBoltBackendCreator(
					true, path, "wtclient.db",
				)(dbCfg)
				require.NoError(t, err)

				db, err = wtdb.OpenClientDB(bdb)
				require.NoError(t, err)

				t.Cleanup(func() {
					db.Close()
				})

				return db
			},
		},
	}

	tests := []struct {
		name string
		run  func(*clientDBHarness)
	}{
		{
			name: "create client session",
			run:  testCreateClientSession,
		},
		{
			name: "filter client sessions",
			run:  testFilterClientSessions,
		},
		{
			name: "create tower",
			run:  testCreateTower,
		},
		{
			name: "remove tower",
			run:  testRemoveTower,
		},
		{
			name: "chan summaries",
			run:  testChanSummaries,
		},
		{
			name: "commit update",
			run:  testCommitUpdate,
		},
		{
			name: "ack update",
			run:  testAckUpdate,
		},
		{
			name: "mark channel closed",
			run:  testMarkChannelClosed,
		},
		{
			name: "rogue updates",
			run:  testRogueUpdates,
		},
		{
			name: "max commitment heights",
			run:  testMaxCommitmentHeights,
		},
		{
			name: "test tower status change",
			run:  testTowerStatusChange,
		},
		{
			name: "terminate session",
			run:  testTerminateSession,
		},
	}

	for _, database := range dbs {
		db := database
		t.Run(db.name, func(t *testing.T) {
			t.Parallel()

			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					h := newClientDBHarness(t, db.init)

					test.run(h)
				})
			}
		})
	}
}

// randCommittedUpdate generates a random committed update.
func randCommittedUpdate(t *testing.T, seqNum uint16) *wtdb.CommittedUpdate {
	t.Helper()

	chanID := randChannelID(t)

	return randCommittedUpdateForChannel(t, chanID, seqNum)
}

func randChannelID(t *testing.T) lnwire.ChannelID {
	t.Helper()

	var chanID lnwire.ChannelID
	_, err := io.ReadFull(crand.Reader, chanID[:])
	require.NoError(t, err)

	return chanID
}

// randCommittedUpdateForChannel generates a random committed update for the
// given channel ID.
func randCommittedUpdateForChannel(t *testing.T, chanID lnwire.ChannelID,
	seqNum uint16) *wtdb.CommittedUpdate {

	t.Helper()

	var hint blob.BreachHint
	_, err := io.ReadFull(crand.Reader, hint[:])
	require.NoError(t, err)

	kit, err := blob.AnchorCommitment.EmptyJusticeKit()
	require.NoError(t, err)

	encBlob := make([]byte, blob.Size(kit))
	_, err = io.ReadFull(crand.Reader, encBlob)
	require.NoError(t, err)

	return &wtdb.CommittedUpdate{
		SeqNum: seqNum,
		CommittedUpdateBody: wtdb.CommittedUpdateBody{
			BackupID: wtdb.BackupID{
				ChanID:       chanID,
				CommitHeight: 666,
			},
			Hint:          hint,
			EncryptedBlob: encBlob,
		},
	}
}

// randCommittedUpdateForChanWithHeight generates a random committed update for
// the given channel ID using the given commit height.
func randCommittedUpdateForChanWithHeight(t *testing.T, chanID lnwire.ChannelID,
	seqNum uint16, height uint64) *wtdb.CommittedUpdate {

	t.Helper()

	var hint blob.BreachHint
	_, err := io.ReadFull(crand.Reader, hint[:])
	require.NoError(t, err)

	kit, err := blob.AnchorCommitment.EmptyJusticeKit()
	require.NoError(t, err)

	encBlob := make([]byte, blob.Size(kit))
	_, err = io.ReadFull(crand.Reader, encBlob)
	require.NoError(t, err)

	return &wtdb.CommittedUpdate{
		SeqNum: seqNum,
		CommittedUpdateBody: wtdb.CommittedUpdateBody{
			BackupID: wtdb.BackupID{
				ChanID:       chanID,
				CommitHeight: height,
			},
			Hint:          hint,
			EncryptedBlob: encBlob,
		},
	}
}

func (h *clientDBHarness) randSession(t *testing.T,
	towerID wtdb.TowerID, maxUpdates uint16) *wtdb.ClientSession {

	t.Helper()

	var id wtdb.SessionID
	rand.Read(id[:])

	return &wtdb.ClientSession{
		ClientSessionBody: wtdb.ClientSessionBody{
			TowerID: towerID,
			Policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType: blobType,
				},
				MaxUpdates: maxUpdates,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
			KeyIndex: h.nextKeyIndex(
				towerID, blobType, false,
			),
		},
		ID: id,
	}
}

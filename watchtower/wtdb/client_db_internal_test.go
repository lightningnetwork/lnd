package wtdb

import (
	"net"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/stretchr/testify/require"
)

// newTestClientDB opens a bolt backed client DB for testing.
func newTestClientDB(t *testing.T) *ClientDB {
	t.Helper()

	backend, err := NewBoltBackendCreator(
		true, t.TempDir(), "wtclient.db",
	)(&kvdb.BoltConfig{DBTimeout: kvdb.DefaultDBTimeout})
	require.NoError(t, err)

	db, err := OpenClientDB(backend)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db
}

// newTestClientDBOnTestBackend opens a client DB on whichever kvdb backend the
// current build selects. Unlike the bolt backed helper above, this gives the
// SQL backends a chance to run the test, which is where concurrent
// transactions actually interleave.
func newTestClientDBOnTestBackend(t *testing.T) *ClientDB {
	t.Helper()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "wtclient")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	db, err := OpenClientDB(backend)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db
}

// newTestSession registers a tower and a session with the given max updates
// against it, and returns the session.
func newTestSession(t *testing.T, db *ClientDB,
	maxUpdates uint16) *ClientSession {

	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	tower, err := db.CreateTower(&lnwire.NetAddress{
		IdentityKey: privKey.PubKey(),
		Address:     &net.TCPAddr{IP: []byte{0x01, 0x00, 0x00, 0x00}},
	})
	require.NoError(t, err)

	const blobType = blob.TypeAltruistCommit

	keyIndex, err := db.NextSessionKeyIndex(tower.ID, blobType, false)
	require.NoError(t, err)

	sessionPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	session := &ClientSession{
		ID: NewSessionIDFromPubKey(sessionPriv.PubKey()),
		ClientSessionBody: ClientSessionBody{
			TowerID:  tower.ID,
			KeyIndex: keyIndex,
			Policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType: blobType,
				},
				MaxUpdates: maxUpdates,
			},
			RewardPkScript: []byte{0x01, 0x02, 0x03},
		},
	}
	require.NoError(t, db.CreateClientSession(session))

	return session
}

// TestAckUpdateRangeIndexEviction asserts that the in-memory range index of a
// session-channel pair can be dropped, so that it is read back from the
// database the next time it is needed.
//
// AckUpdate relies on this: RangeIndex.Add mutates the in-memory index as part
// of AckUpdate's transaction, but that mutation is not rolled back along with
// the transaction. An attempt that is retried on top of the mutated index would
// find the height covered already, apply nothing to the database, and leave the
// ack persisted nowhere while the rest of the transaction committed.
func TestAckUpdateRangeIndexEviction(t *testing.T) {
	t.Parallel()

	db := newTestClientDB(t)
	session := newTestSession(t, db, 5)

	var chanID lnwire.ChannelID
	copy(chanID[:], []byte{0x01, 0x02, 0x03})
	require.NoError(t, db.RegisterChannel(chanID, []byte{0x01}))

	// Commit and ack a single update, which leaves the range index for this
	// session-channel pair both on disk and in memory.
	const ackedHeight = 5
	update := &CommittedUpdate{
		SeqNum: 1,
		CommittedUpdateBody: CommittedUpdateBody{
			BackupID: BackupID{
				ChanID:       chanID,
				CommitHeight: ackedHeight,
			},
			EncryptedBlob: []byte{0x01, 0x02, 0x03},
		},
	}
	_, err := db.CommitUpdate(&session.ID, update)
	require.NoError(t, err)
	require.NoError(t, db.AckUpdate(&session.ID, 1, 1))

	// readFromDisk reads the range index of the pair straight from the
	// database, bypassing the in-memory copy of it entirely.
	readFromDisk := func() *RangeIndex {
		var index *RangeIndex
		err := kvdb.View(db.db, func(tx kvdb.RTx) error {
			rangesBkt, err := getRangesReadBucket(
				tx, session.ID, chanID,
			)
			if err != nil {
				return err
			}

			index, err = readRangeIndex(rangesBkt)

			return err
		}, func() {})
		require.NoError(t, err)

		return index
	}

	// Both copies agree at this point.
	index, err := db.getRangeIndex(nil, session.ID, chanID)
	require.NoError(t, err)
	require.True(t, index.IsInIndex(ackedHeight))
	require.True(t, readFromDisk().IsInIndex(ackedHeight))

	// Now mutate the in-memory index without touching the database, which
	// is the state that an attempt of AckUpdate that was rolled back leaves
	// behind.
	const rolledBackHeight = 9
	require.NoError(t, index.Add(rolledBackHeight, nil))
	require.True(t, index.IsInIndex(rolledBackHeight))
	require.False(t, readFromDisk().IsInIndex(rolledBackHeight))

	// Evicting the index is what makes the next read of it agree with the
	// database again.
	db.evictRangeIndex(session.ID, chanID)

	index, err = db.getRangeIndex(nil, session.ID, chanID)
	require.NoError(t, err)
	require.True(t, index.IsInIndex(ackedHeight))
	require.False(t, index.IsInIndex(rolledBackHeight))

	// With the stale height gone, acking an update at that height actually
	// makes it to disk. Were the index still holding on to it, the ack
	// would be a no-op against the database.
	update = &CommittedUpdate{
		SeqNum: 2,
		CommittedUpdateBody: CommittedUpdateBody{
			BackupID: BackupID{
				ChanID:       chanID,
				CommitHeight: rolledBackHeight,
			},
			EncryptedBlob: []byte{0x04, 0x05, 0x06},
		},
	}
	_, err = db.CommitUpdate(&session.ID, update)
	require.NoError(t, err)
	require.NoError(t, db.AckUpdate(&session.ID, 2, 2))

	require.True(t, readFromDisk().IsInIndex(rolledBackHeight))
}

// TestAckUpdateRacesChannelClose asserts that a session that acks its first
// update for a channel at the very same time as that channel is being marked
// closed still ends up being evaluated for closability, no matter which of the
// two transactions gets there first.
//
// The two halves of that invariant live in different transactions:
// MarkChannelClosed evaluates the sessions it can see in the channel's set of
// sessions, and AckUpdate evaluates its own session when it finds the channel
// already closed. What ties them together is that they both write the channel's
// db-ID row, so the database can't let both of them commit on a view of the
// world the other one has already invalidated.
func TestAckUpdateRacesChannelClose(t *testing.T) {
	t.Parallel()

	db := newTestClientDBOnTestBackend(t)

	var chanID lnwire.ChannelID
	copy(chanID[:], []byte{0x0a, 0x0b, 0x0c})
	require.NoError(t, db.RegisterChannel(chanID, []byte{0x01}))

	// The first session acks an update for the channel up front, which is
	// what keeps the channel's details around once it is closed. It is far
	// from exhausted, so it never becomes closable itself.
	keeper := newTestSession(t, db, 5)
	keeperUpdate := &CommittedUpdate{
		SeqNum: 1,
		CommittedUpdateBody: CommittedUpdateBody{
			BackupID: BackupID{
				ChanID:       chanID,
				CommitHeight: 1,
			},
			EncryptedBlob: []byte{0x01},
		},
	}
	_, err := db.CommitUpdate(&keeper.ID, keeperUpdate)
	require.NoError(t, err)
	require.NoError(t, db.AckUpdate(&keeper.ID, 1, 1))

	// The racing session has a single update, so acking it both adds the
	// session to the channel's set of sessions for the very first time and
	// exhausts the session.
	racer := newTestSession(t, db, 1)
	racerUpdate := &CommittedUpdate{
		SeqNum: 1,
		CommittedUpdateBody: CommittedUpdateBody{
			BackupID: BackupID{
				ChanID:       chanID,
				CommitHeight: 2,
			},
			EncryptedBlob: []byte{0x02},
		},
	}
	_, err = db.CommitUpdate(&racer.ID, racerUpdate)
	require.NoError(t, err)

	// Now run the ack and the channel close at the same time.
	const closeHeight = 100

	var (
		wg                sync.WaitGroup
		ackErr, closeErr  error
		closedByMarkClose []SessionID
	)

	wg.Add(2)
	go func() {
		defer wg.Done()

		ackErr = db.AckUpdate(&racer.ID, 1, 1)
	}()
	go func() {
		defer wg.Done()

		closedByMarkClose, closeErr = db.MarkChannelClosed(
			chanID, closeHeight,
		)
	}()
	wg.Wait()

	require.NoError(t, ackErr)
	require.NoError(t, closeErr)

	// Whichever of the two won, the racing session must have been found
	// closable by exactly one of them, and the height it is recorded under
	// is the height the channel closed at either way.
	closable, err := db.ListClosableSessions()
	require.NoError(t, err)
	require.Contains(t, closable, racer.ID)
	require.EqualValues(t, closeHeight, closable[racer.ID])

	// The session that is still far from exhausted must not have been
	// swept up along with it.
	require.NotContains(t, closable, keeper.ID)

	// If the close was the one that saw the session, it hands it back to
	// its caller as well.
	if len(closedByMarkClose) > 0 {
		require.Equal(t, []SessionID{racer.ID}, closedByMarkClose)
	}
}

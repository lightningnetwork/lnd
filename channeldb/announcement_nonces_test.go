package channeldb

import (
	"math/rand"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestAnnouncementNonces tests the various announcement nonce pair CRUD
// operations.
func TestAnnouncementNonces(t *testing.T) {
	cdb, err := MakeTestDB(t)
	require.NoError(t, err)

	db := cdb.ChannelStateDB()

	// Show that the set of nonces is currently empty.
	nonceSet, err := db.GetAllAnnouncementNonces()
	require.NoError(t, err)
	require.Empty(t, nonceSet)

	// Generate a random channel ID.
	chanID1 := randChannelID(t)

	// Assert that there is no entry for this channel yet.
	_, err = db.GetAnnouncementNonces(chanID1)
	require.ErrorIs(t, err, ErrChannelNotFound)

	// Insert an entry.
	nonces1 := AnnouncementNonces{
		Btc:  randNonce(t),
		Node: randNonce(t),
	}

	err = db.SaveAnnouncementNonces(chanID1, &nonces1)
	require.NoError(t, err)

	// Assert that the entry is now returned.
	n, err := db.GetAnnouncementNonces(chanID1)
	require.NoError(t, err)
	require.Equal(t, &nonces1, n)

	nonceSet, err = db.GetAllAnnouncementNonces()
	require.NoError(t, err)
	require.EqualValues(t, map[lnwire.ChannelID]AnnouncementNonces{
		chanID1: nonces1,
	}, nonceSet)

	// Add another entry.
	chanID2 := randChannelID(t)
	nonces2 := AnnouncementNonces{
		Btc:  randNonce(t),
		Node: randNonce(t),
	}

	err = db.SaveAnnouncementNonces(chanID2, &nonces2)
	require.NoError(t, err)

	n, err = db.GetAnnouncementNonces(chanID2)
	require.NoError(t, err)
	require.Equal(t, &nonces2, n)

	nonceSet, err = db.GetAllAnnouncementNonces()
	require.NoError(t, err)
	require.EqualValues(t, map[lnwire.ChannelID]AnnouncementNonces{
		chanID1: nonces1,
		chanID2: nonces2,
	}, nonceSet)

	// Now, assert that deletion works.
	err = db.DeleteAnnouncementNonces(chanID1)
	require.NoError(t, err)

	_, err = db.GetAnnouncementNonces(chanID1)
	require.ErrorIs(t, err, ErrChannelNotFound)

	nonceSet, err = db.GetAllAnnouncementNonces()
	require.NoError(t, err)
	require.EqualValues(t, map[lnwire.ChannelID]AnnouncementNonces{
		chanID2: nonces2,
	}, nonceSet)

	err = db.DeleteAnnouncementNonces(chanID2)
	require.NoError(t, err)

	nonceSet, err = db.GetAllAnnouncementNonces()
	require.NoError(t, err)
	require.Empty(t, nonceSet)
}

func randChannelID(t *testing.T) lnwire.ChannelID {
	var chanID lnwire.ChannelID

	_, err := rand.Read(chanID[:])
	require.NoError(t, err)

	return chanID
}

func randNonce(t *testing.T) [66]byte {
	var b [66]byte

	_, err := rand.Read(b[:])
	require.NoError(t, err)

	return b
}

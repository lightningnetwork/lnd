package channeldb

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestPeerStorage tests store, fetch, overwrite, and delete of peer storage
// blobs.
func TestPeerStorage(t *testing.T) {
	db, err := MakeTestDB(t)
	require.NoError(t, err)

	peer := route.Vertex{1, 1, 1}

	// Fetching from a peer with no bucket should fail.
	_, err = db.FetchPeerStorage(peer)
	require.ErrorIs(t, err, ErrNoPeerBucket)

	// Store a blob and fetch it back.
	blob := []byte("encrypted-backup-data")
	err = db.StorePeerStorage(peer, blob)
	require.NoError(t, err)

	fetched, err := db.FetchPeerStorage(peer)
	require.NoError(t, err)
	require.Equal(t, blob, fetched)

	// Overwrite with a new blob.
	newBlob := []byte("updated-backup-data")
	err = db.StorePeerStorage(peer, newBlob)
	require.NoError(t, err)

	fetched, err = db.FetchPeerStorage(peer)
	require.NoError(t, err)
	require.Equal(t, newBlob, fetched)

	// Delete the blob.
	err = db.DeletePeerStorage(peer)
	require.NoError(t, err)

	// Fetch should now fail.
	_, err = db.FetchPeerStorage(peer)
	require.Error(t, err)

	// Delete on a non-existent blob should not error.
	err = db.DeletePeerStorage(peer)
	require.NoError(t, err)
}

// TestFlapCount tests lookup and writing of flap count to disk.
func TestFlapCount(t *testing.T) {
	db, err := MakeTestDB(t)
	require.NoError(t, err)

	// Try to read flap count for a peer that we have no records for.
	_, err = db.ReadFlapCount(testPub)
	require.Equal(t, ErrNoPeerBucket, err)

	var (
		testPub2       = route.Vertex{2, 2, 2}
		peer1FlapCount = &FlapCount{
			Count:    20,
			LastFlap: time.Unix(100, 23),
		}
		peer2FlapCount = &FlapCount{
			Count:    39,
			LastFlap: time.Unix(200, 23),
		}
	)

	peers := map[route.Vertex]*FlapCount{
		testPub:  peer1FlapCount,
		testPub2: peer2FlapCount,
	}

	err = db.WriteFlapCounts(peers)
	require.NoError(t, err)

	// Lookup flap count for our first pubkey.
	count, err := db.ReadFlapCount(testPub)
	require.NoError(t, err)
	require.Equal(t, peer1FlapCount, count)

	// Lookup our flap count for the second peer.
	count, err = db.ReadFlapCount(testPub2)
	require.NoError(t, err)
	require.Equal(t, peer2FlapCount, count)
}

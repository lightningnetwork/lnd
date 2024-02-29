package peer

import (
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// TestPeerStorageDB tests the PeerStorageDB functionality.
func TestPeerStorageDB(t *testing.T) {
	rt := require.New(t)

	file, err := os.CreateTemp("", "*.db")
	rt.NoError(err)
	t.Cleanup(func() {
		rt.NoError(file.Close())
		rt.NoError(os.Remove(file.Name()))
	})

	dbPath := file.Name()
	db, err := kvdb.Open(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
	)
	rt.NoError(err)
	t.Cleanup(func() {
		rt.NoError(db.Close())
	})

	peerStoreProducer, err := NewPeerStorageProducer(db)
	rt.NoError(err)

	// Create a sample private key for testing.
	samplePrivKey, _ := btcec.NewPrivateKey()
	pubKey := samplePrivKey.PubKey()
	pubKeyBytes := pubKey.SerializeCompressed()

	// Create a PeerStorageDB instance.
	peerDataStore := peerStoreProducer.NewPeerStorageDB(pubKeyBytes)
	rt.NotNil(peerDataStore)

	// Sample byte data.
	samplePeerData := []byte("sample data")

	// Test store.
	err = peerDataStore.Store(samplePeerData)
	rt.NoError(err)

	// Test Retrieve.
	retrievedData, err := peerDataStore.Retrieve()
	rt.NoError(err)
	rt.NotNil(retrievedData)

	// Retrieved data should be same as the one earlier stored.
	rt.Equal(retrievedData, samplePeerData)

	// Test Delete.
	err = peerDataStore.Delete()
	rt.NoError(err)

	// Test that there is no data stored for that peer after delete.
	retrievedData, err = peerDataStore.Retrieve()
	rt.NoError(err)
	rt.Nil(retrievedData)
}

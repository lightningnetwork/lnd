package persistlog

import (
	"crypto/sha256"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"math"
	"os"
	"testing"
	"time"
)

var (
	// Bytes of a private key
	key = [32]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
)

type mockNotifier struct {
	confChannel chan *chainntnfs.TxConfirmation
	epochChan   chan *chainntnfs.BlockEpoch
}

func (m *mockNotifier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	return &chainntnfs.BlockEpochEvent{
		Epochs: m.epochChan,
		Cancel: func() {},
	}, nil
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash, numConfs,
	heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return nil, nil
}

func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {
	return nil, nil
}

func (m *mockNotifier) Start() error {
	return nil
}

func (m *mockNotifier) Stop() error {
	return nil
}

// generateSharedSecret generates a shared secret given a public key and a
// private key. It is directly copied from sphinx.go.
func generateSharedSecret(pub *btcec.PublicKey, priv *btcec.PrivateKey) [32]byte {
	s := &btcec.PublicKey{}
	x, y := btcec.S256().ScalarMult(pub.X, pub.Y, priv.D.Bytes())
	s.X = x
	s.Y = y

	return sha256.Sum256(s.SerializeCompressed())
}

// shutdown stops the DecayedLog and deletes the folder enclosing the
// temporary channel database.
func shutdown(d *DecayedLog) {
	os.RemoveAll("tempdir")
	d.Stop()
}

// TestDecayedLogGarbageCollector tests the ability of the garbage collector
// to delete expired cltv values every time a block is received. Expired cltv
// values are cltv values that are <= current block height.
func TestDecayedLogGarbageCollector(t *testing.T) {
	// Random (TO-BE-EXPIRED) cltv value
	cltv := uint32(2)

	// Create the MockNotifier which triggers the garbage collector
	MockNotifier := &mockNotifier{
		epochChan: make(chan *chainntnfs.BlockEpoch, 1),
	}

	// Create a DecayedLog object
	d := DecayedLog{Notifier: MockNotifier}

	// Open the channeldb (start the garbage collector)
	err := d.Start("tempdir")
	if err != nil {
		t.Fatalf("Unable to start / open DecayedLog")
	}
	defer shutdown(&d)

	// Create a new private key on elliptic curve secp256k1
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("Unable to create new private key")
	}

	// Generate a public key from the key bytes
	_, testPub := btcec.PrivKeyFromBytes(btcec.S256(), key[:])

	// Generate a shared secret with the public and private keys we made
	secret := generateSharedSecret(testPub, priv)

	// Create the hashedSecret given the shared secret we just generated.
	// This is the first 20 bytes of the Sha-256 hash of the shared secret.
	// This is used as a key to retrieve the cltv value.
	hashedSecret := HashSharedSecret(secret)

	// Store <hashedSecret, cltv> in the sharedHashBucket.
	err = d.Put(hashedSecret[:], cltv)
	if err != nil {
		t.Fatalf("Unable to store in channeldb")
	}

	// Send Block notification to garbage collector. The garbage collector
	// should remove the entry we just added to sharedHashBucket as it is
	// now expired.
	MockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: int32(101),
	}
	MockNotifier.epochChan <- &chainntnfs.BlockEpoch{
		Height: int32(102),
	}

	// Wait for database write (GC is in a goroutine)
	time.Sleep(500 * time.Millisecond)

	// Assert that hashedSecret is not in the sharedHashBucket
	val, err := d.Get(hashedSecret[:])
	if err != nil {
		t.Fatalf("Delete failed - received an error upon Get")
	}

	if val != math.MaxUint32 {
		t.Fatalf("cltv was not deleted")
	}
}

// TestDecayedLogInsertionAndRetrieval inserts a cltv value into the nested
// sharedHashBucket and then deletes it and finally asserts that we can no
// longer retrieve it.
func TestDecayedLogInsertionAndDeletion(t *testing.T) {
	// Random cltv value
	cltv := uint32(5)

	// Create a DecayedLog object
	d := DecayedLog{}

	// Open the channeldb
	err := d.Start("tempdir")
	if err != nil {
		t.Fatalf("Unable to start / open DecayedLog")
	}
	defer shutdown(&d)

	// Create a new private key on elliptic curve secp256k1
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("Unable to create new private key")
	}

	// Generate a public key from the key bytes
	_, testPub := btcec.PrivKeyFromBytes(btcec.S256(), key[:])

	// Generate a shared secret with the public and private keys we made
	secret := generateSharedSecret(testPub, priv)

	// Create the hashedSecret given the shared secret we just generated.
	// This is the first 20 bytes of the Sha-256 hash of the shared secret.
	// This is used as a key to retrieve the cltv value.
	hashedSecret := HashSharedSecret(secret)

	// Store <hashedSecret, cltv> in the sharedHashBucket.
	err = d.Put(hashedSecret[:], cltv)
	if err != nil {
		t.Fatalf("Unable to store in channeldb")
	}

	// Delete hashedSecret from the sharedHashBucket.
	err = d.Delete(hashedSecret[:])
	if err != nil {
		t.Fatalf("Unable to delete from channeldb")
	}

	// Assert that hashedSecret is not in the sharedHashBucket
	val, err := d.Get(hashedSecret[:])
	if err != nil {
		t.Fatalf("Delete failed - received the wrong error message")
	}

	if val != math.MaxUint32 {
		t.Fatalf("cltv was not deleted")
	}

}

// TestDecayedLogStartAndStop tests for persistence. The DecayedLog is started,
// a cltv value is stored in the sharedHashBucket, and then it the DecayedLog
// is stopped. The DecayedLog is then started up again and we test that the
// cltv value is indeed still stored in the sharedHashBucket. We then delete
// the cltv value and check that it persists upon startup.
func TestDecayedLogStartAndStop(t *testing.T) {
	// Random cltv value
	cltv := uint32(6)

	// Create a DecayedLog object
	d := DecayedLog{}

	// Open the channeldb
	err := d.Start("tempdir")
	if err != nil {
		t.Fatalf("Unable to start / open DecayedLog")
	}
	defer shutdown(&d)

	// Create a new private key on elliptic curve secp256k1
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("Unable to create new private key")
	}

	// Generate a public key from the key bytes
	_, testPub := btcec.PrivKeyFromBytes(btcec.S256(), key[:])

	// Generate a shared secret with the public and private keys we made
	secret := generateSharedSecret(testPub, priv)

	// Create the hashedSecret given the shared secret we just generated.
	// This is the first 20 bytes of the Sha-256 hash of the shared secret.
	// This is used as a key to retrieve the cltv value.
	hashedSecret := HashSharedSecret(secret)

	// Store <hashedSecret, cltv> in the sharedHashBucket.
	err = d.Put(hashedSecret[:], cltv)
	if err != nil {
		t.Fatalf("Unable to store in channeldb")
	}

	// Shutdown the DecayedLog's channeldb
	d.Stop()

	// Startup the DecayedLog's channeldb
	err = d.Start("tempdir")
	if err != nil {
		t.Fatalf("Unable to start / open DecayedLog")
	}

	// Retrieve the stored cltv value given the hashedSecret key.
	value, err := d.Get(hashedSecret[:])
	if err != nil {
		t.Fatalf("Unable to retrieve from channeldb")
	}

	// Check that the original cltv value matches the retrieved cltv
	// value.
	if cltv != value {
		t.Fatalf("Value retrieved doesn't match value stored")
	}

	// Delete hashedSecret from sharedHashBucket
	err = d.Delete(hashedSecret[:])
	if err != nil {
		t.Fatalf("Unable to delete from channeldb")
	}

	// Shutdown the DecayedLog's channeldb
	d.Stop()

	// Startup the DecayedLog's channeldb
	err = d.Start("tempdir")
	if err != nil {
		t.Fatalf("Unable to start / open DecayedLog")
	}

	// Assert that hashedSecret is not in the sharedHashBucket
	val, err := d.Get(hashedSecret[:])
	if err != nil {
		t.Fatalf("Delete failed")
	}

	if val != math.MaxUint32 {
		t.Fatalf("cltv was not deleted")
	}

}

// TestDecayedLogStorageAndRetrieval stores a cltv value and then retrieves it
// via the nested sharedHashBucket and finally asserts that the original stored
// and retrieved cltv values are equal.
func TestDecayedLogStorageAndRetrieval(t *testing.T) {
	// Random cltv value
	cltv := uint32(7)

	// Create a DecayedLog object
	d := DecayedLog{}

	// Open the channeldb
	err := d.Start("tempdir")
	if err != nil {
		t.Fatalf("Unable to start / open DecayedLog")
	}
	defer shutdown(&d)

	// Create a new private key on elliptic curve secp256k1
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("Unable to create new private key")
	}

	// Generate a public key from the key bytes
	_, testPub := btcec.PrivKeyFromBytes(btcec.S256(), key[:])

	// Generate a shared secret with the public and private keys we made
	secret := generateSharedSecret(testPub, priv)

	// Create the hashedSecret given the shared secret we just generated.
	// This is the first 20 bytes of the Sha-256 hash of the shared secret.
	// This is used as a key to retrieve the cltv value.
	hashedSecret := HashSharedSecret(secret)

	// Store <hashedSecret, cltv> in the sharedHashBucket
	err = d.Put(hashedSecret[:], cltv)
	if err != nil {
		t.Fatalf("Unable to store in channeldb")
	}

	// Retrieve the stored cltv value given the hashedSecret key.
	value, err := d.Get(hashedSecret[:])
	if err != nil {
		t.Fatalf("Unable to retrieve from channeldb")
	}

	// If the original cltv value does not match the value retrieved,
	// then the test failed.
	if cltv != value {
		t.Fatalf("Value retrieved doesn't match value stored")
	}

}

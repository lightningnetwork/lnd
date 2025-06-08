package decayedlog

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/stretchr/testify/require"
)

const (
	cltv uint32 = 100000
)

// startup sets up the DecayedLog and possibly the garbage collector.
func startup(dbPath string, notifier bool) (sphinx.ReplayLog, kvdb.Backend,
	*mock.ChainNotifier, *sphinx.HashPrefix, func(), error) {

	cfg := &kvdb.BoltConfig{
		DBTimeout: time.Second,
	}
	backend, err := NewBoltBackendCreator(dbPath, "sphinxreplay.db")(cfg)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("unable to create "+
			"temporary decayed log db: %v", err)
	}

	var log sphinx.ReplayLog
	var chainNotifier *mock.ChainNotifier
	if notifier {

		// Create the MockNotifier which triggers the garbage collector
		chainNotifier = &mock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		}

		// Initialize the DecayedLog object
		log, err = NewDecayedLog(backend, chainNotifier)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
	} else {
		// Initialize the DecayedLog object
		log, err = NewDecayedLog(backend, nil)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
	}

	// Open the channeldb (start the garbage collector)
	err = log.Start()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	// Create a HashPrefix identifier for a packet. Instead of actually
	// generating an ECDH secret and hashing it, simulate with random bytes.
	// This is used as a key to retrieve the cltv value.
	var hashedSecret sphinx.HashPrefix
	_, err = rand.Read(hashedSecret[:])
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	stop := func() {
		_ = log.Stop()
		backend.Close()
	}

	return log, backend, chainNotifier, &hashedSecret, stop, nil
}

// TestDecayedLogGarbageCollector tests the ability of the garbage collector
// to delete expired cltv values every time a block is received. Expired cltv
// values are cltv values that are < current block height.
func TestDecayedLogGarbageCollector(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir()

	d, backend, notifier, hashedSecret, _, err := startup(dbPath, true)
	require.NoError(t, err, "Unable to start up DecayedLog")
	t.Cleanup(func() {
		require.NoError(t, d.Stop())
	})

	// Store <hashedSecret, cltv> in the sharedHashBucket.
	err = d.Put(hashedSecret, cltv)
	require.NoError(t, err, "Unable to store in channeldb")

	// Also add a batch replay set to the batchReplayBucket.
	batchReplaySet := sphinx.NewReplaySet()
	batchReplaySet.Add(0, cltv)
	batchID := make([]byte, 8)
	binary.BigEndian.PutUint64(batchID, 0)
	batch := sphinx.NewBatch(batchID)
	batch.Put(0, hashedSecret, cltv)
	_, err = d.PutBatch(batch)
	require.NoError(t, err, "Unable to store batch in channeldb")

	// Wait for database write (GC is in a goroutine)
	time.Sleep(500 * time.Millisecond)

	// Send block notifications to garbage collector. The garbage collector
	// should remove the entry by block 100001.

	// Send block 100000
	notifier.EpochChan <- &chainntnfs.BlockEpoch{
		Height: 100000,
	}

	// Assert that hashedSecret is still in the sharedHashBucket
	val, err := d.Get(hashedSecret)
	require.NoError(t, err, "Get failed - received an error upon Get")

	if val != cltv {
		t.Fatalf("GC incorrectly deleted CLTV")
	}

	// Assert that the batch replay set is still in the batchReplayBucket.
	replaySets := []*sphinx.ReplaySet{}
	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		batchReplaySetBytes := tx.ReadBucket(batchReplayBucket)
		batchReplaySetBytes.ForEach(func(k, v []byte) error {
			replaySet := sphinx.NewReplaySet()
			err := replaySet.Decode(bytes.NewReader(v))
			if err != nil {
				return err
			}
			replaySets = append(replaySets, replaySet)
			return nil
		})
		return nil
	}, func() {})
	require.NoError(t, err, "Unable to get batch from channeldb")

	require.Equal(t, len(replaySets), 1)
	require.Equal(t, replaySets[0], batchReplaySet)

	// Send block 100001 (expiry block)
	notifier.EpochChan <- &chainntnfs.BlockEpoch{
		Height: 100001,
	}

	// Wait for database write (GC is in a goroutine)
	time.Sleep(500 * time.Millisecond)

	// Assert that hashedSecret is not in the sharedHashBucket
	_, err = d.Get(hashedSecret)
	if err == nil {
		t.Fatalf("CLTV was not deleted")
	}
	if err != sphinx.ErrLogEntryNotFound {
		t.Fatalf("Get failed - received unexpected error upon Get: %v",
			err)
	}

	// Assert that the batch replay set is not in the batchReplayBucket.
	replaySets = []*sphinx.ReplaySet{}
	err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
		batchReplaySetBytes := tx.ReadBucket(batchReplayBucket)
		batchReplaySetBytes.ForEach(func(k, v []byte) error {
			replaySet := sphinx.NewReplaySet()
			err := replaySet.Decode(bytes.NewReader(v))
			if err != nil {
				return err
			}
			replaySets = append(replaySets, replaySet)
			return nil
		})
		return nil
	}, func() {})
	require.NoError(t, err, "Unable to get batch from channeldb")

	require.Equal(t, len(replaySets), 0)
}

// TestDecayedLogPersistentGarbageCollector tests the persistence property of
// the garbage collector. The garbage collector will be restarted immediately and
// a block that expires the stored CLTV value will be sent to the ChainNotifier.
// We test that this causes the <hashedSecret, CLTV> pair to be deleted even
// on GC restarts.
func TestDecayedLogPersistentGarbageCollector(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir()

	d, _, _, hashedSecret, stop, err := startup(dbPath, true)
	require.NoError(t, err, "Unable to start up DecayedLog")
	t.Cleanup(func() {
		require.NoError(t, d.Stop())
	})

	// Store <hashedSecret, cltv> in the sharedHashBucket
	if err = d.Put(hashedSecret, cltv); err != nil {
		t.Fatalf("Unable to store in channeldb: %v", err)
	}

	// The hash prefix should be retrievable from the decayed log.
	_, err = d.Get(hashedSecret)
	if err != nil {
		t.Fatalf("Get failed - received unexpected error upon Get: %v", err)
	}

	// Shut down DecayedLog and the garbage collector along with it.
	stop()

	d2, _, notifier2, _, _, err := startup(dbPath, true)
	require.NoError(t, err, "Unable to restart DecayedLog")
	t.Cleanup(func() {
		require.NoError(t, d2.Stop())
	})

	// Check that the hash prefix still exists in the new db instance.
	_, err = d2.Get(hashedSecret)
	if err != nil {
		t.Fatalf("Get failed - received unexpected error upon Get: %v", err)
	}

	// Send a block notification to the garbage collector that expires
	// the stored CLTV.
	notifier2.EpochChan <- &chainntnfs.BlockEpoch{
		Height: int32(100001),
	}

	// Wait for database write (GC is in a goroutine)
	time.Sleep(500 * time.Millisecond)

	// Assert that hashedSecret is not in the sharedHashBucket
	_, err = d2.Get(hashedSecret)
	if err != sphinx.ErrLogEntryNotFound {
		t.Fatalf("Get failed - received unexpected error upon Get: %v", err)
	}
}

// TestDecayedLogInsertionAndRetrieval inserts a cltv value into the
// sharedHashBucket and then deletes it and finally asserts that we can no
// longer retrieve it.
func TestDecayedLogInsertionAndDeletion(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir()

	d, _, _, hashedSecret, _, err := startup(dbPath, false)
	require.NoError(t, err, "Unable to start up DecayedLog")
	t.Cleanup(func() {
		require.NoError(t, d.Stop())
	})

	// Store <hashedSecret, cltv> in the sharedHashBucket.
	err = d.Put(hashedSecret, cltv)
	require.NoError(t, err, "Unable to store in channeldb")

	// Delete hashedSecret from the sharedHashBucket.
	err = d.Delete(hashedSecret)
	require.NoError(t, err, "Unable to delete from channeldb")

	// Assert that hashedSecret is not in the sharedHashBucket
	_, err = d.Get(hashedSecret)
	if err == nil {
		t.Fatalf("CLTV was not deleted")
	}
	if err != sphinx.ErrLogEntryNotFound {
		t.Fatalf("Get failed - received unexpected error upon Get: %v", err)
	}
}

// TestDecayedLogStartAndStop tests for persistence. The DecayedLog is started,
// a cltv value is stored in the sharedHashBucket, and then it the DecayedLog
// is stopped. The DecayedLog is then started up again and we test that the
// cltv value is indeed still stored in the sharedHashBucket. We then delete
// the cltv value and check that it persists upon startup.
func TestDecayedLogStartAndStop(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir()

	d, _, _, hashedSecret, stop, err := startup(dbPath, false)
	require.NoError(t, err, "Unable to start up DecayedLog")
	t.Cleanup(func() {
		require.NoError(t, d.Stop())
	})

	// Store <hashedSecret, cltv> in the sharedHashBucket.
	err = d.Put(hashedSecret, cltv)
	require.NoError(t, err, "Unable to store in channeldb")

	// Shutdown the DecayedLog's channeldb
	stop()

	d2, _, _, hashedSecret2, stop, err := startup(dbPath, false)
	require.NoError(t, err, "Unable to restart DecayedLog")
	t.Cleanup(func() {
		require.NoError(t, d2.Stop())
	})

	// Retrieve the stored cltv value given the hashedSecret key.
	value, err := d2.Get(hashedSecret)
	require.NoError(t, err, "Unable to retrieve from channeldb")

	// Check that the original cltv value matches the retrieved cltv
	// value.
	if cltv != value {
		t.Fatalf("Value retrieved doesn't match value stored")
	}

	// Delete hashedSecret from sharedHashBucket
	err = d2.Delete(hashedSecret2)
	require.NoError(t, err, "Unable to delete from channeldb")

	// Shutdown the DecayedLog's channeldb
	stop()

	d3, _, _, hashedSecret3, _, err := startup(dbPath, false)
	require.NoError(t, err, "Unable to restart DecayedLog")
	t.Cleanup(func() {
		require.NoError(t, d3.Stop())
	})

	// Assert that hashedSecret is not in the sharedHashBucket
	_, err = d3.Get(hashedSecret3)
	if err == nil {
		t.Fatalf("CLTV was not deleted")
	}
	if err != sphinx.ErrLogEntryNotFound {
		t.Fatalf("Get failed - received unexpected error upon Get: %v", err)
	}
}

// TestDecayedLogStorageAndRetrieval stores a cltv value and then retrieves it
// via the nested sharedHashBucket and finally asserts that the original stored
// and retrieved cltv values are equal.
func TestDecayedLogStorageAndRetrieval(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir()

	d, _, _, hashedSecret, _, err := startup(dbPath, false)
	require.NoError(t, err, "Unable to start up DecayedLog")
	t.Cleanup(func() {
		require.NoError(t, d.Stop())
	})

	// Store <hashedSecret, cltv> in the sharedHashBucket
	err = d.Put(hashedSecret, cltv)
	require.NoError(t, err, "Unable to store in channeldb")

	// Retrieve the stored cltv value given the hashedSecret key.
	value, err := d.Get(hashedSecret)
	require.NoError(t, err, "Unable to retrieve from channeldb")

	// If the original cltv value does not match the value retrieved,
	// then the test failed.
	if cltv != value {
		t.Fatalf("Value retrieved doesn't match value stored")
	}
}

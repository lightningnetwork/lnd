package aliasmgr

import (
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const channelReadTimeout = time.Second * 10

// TestAliasStorePeerAlias tests that putting and retrieving a peer's alias
// works properly.
func TestAliasStorePeerAlias(t *testing.T) {
	t.Parallel()

	// Create the backend database and use this to create the aliasStore.
	dbPath := filepath.Join(t.TempDir(), "testdb")
	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
		false,
	)
	require.NoError(t, err)
	defer db.Close()

	linkUpdater := func(shortID lnwire.ShortChannelID) error {
		return nil
	}

	aliasStore, err := NewManager(db, linkUpdater)
	require.NoError(t, err)

	var chanID1 [32]byte
	_, err = rand.Read(chanID1[:])
	require.NoError(t, err)

	// Test that we can put the (chanID, alias) mapping in the database.
	// Also check that we retrieve exactly what we put in.
	err = aliasStore.PutPeerAlias(chanID1, StartingAlias)
	require.NoError(t, err)

	storedAlias, err := aliasStore.GetPeerAlias(chanID1)
	require.NoError(t, err)
	require.Equal(t, StartingAlias, storedAlias)
}

// TestAliasStoreRequest tests that the aliasStore delivers the expected SCID.
func TestAliasStoreRequest(t *testing.T) {
	t.Parallel()

	// Create the backend database and use this to create the aliasStore.
	dbPath := filepath.Join(t.TempDir(), "testdb")
	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
		false,
	)
	require.NoError(t, err)
	defer db.Close()

	linkUpdater := func(shortID lnwire.ShortChannelID) error {
		return nil
	}

	aliasStore, err := NewManager(db, linkUpdater)
	require.NoError(t, err)

	// We'll assert that the very first alias we receive is StartingAlias.
	alias1, err := aliasStore.RequestAlias()
	require.NoError(t, err)
	require.Equal(t, StartingAlias, alias1)

	// The next alias should be the result of passing in StartingAlias to
	// getNextScid.
	nextAlias := getNextScid(alias1)
	alias2, err := aliasStore.RequestAlias()
	require.NoError(t, err)
	require.Equal(t, nextAlias, alias2)
}

// TestAliasLifecycle tests that the aliases can be created and deleted.
func TestAliasLifecycle(t *testing.T) {
	t.Parallel()

	// Create the backend database and use this to create the aliasStore.
	dbPath := filepath.Join(t.TempDir(), "testdb")
	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
		false,
	)
	require.NoError(t, err)
	defer db.Close()

	updateChan := make(chan struct{}, 1)

	linkUpdater := func(shortID lnwire.ShortChannelID) error {
		updateChan <- struct{}{}
		return nil
	}

	aliasStore, err := NewManager(db, linkUpdater)
	require.NoError(t, err)

	const (
		base  = uint64(123123123)
		alias = uint64(456456456)
	)

	// Parse the aliases and base to short channel ID format.
	baseScid := lnwire.NewShortChanIDFromInt(base)
	aliasScid := lnwire.NewShortChanIDFromInt(alias)
	aliasScid2 := lnwire.NewShortChanIDFromInt(alias + 1)

	// Add the first alias.
	err = aliasStore.AddLocalAlias(aliasScid, baseScid, false, true)
	require.NoError(t, err)

	// The link updater should be called.
	require.True(t, readOrTimeout(updateChan, channelReadTimeout))

	// Query the aliases and verify the results.
	aliasList := aliasStore.GetAliases(baseScid)
	require.Len(t, aliasList, 1)
	require.Contains(t, aliasList, aliasScid)

	// Add the second alias.
	err = aliasStore.AddLocalAlias(aliasScid2, baseScid, false, true)
	require.NoError(t, err)

	// The link updater should be called.
	require.True(t, readOrTimeout(updateChan, channelReadTimeout))

	// Query the aliases and verify the results.
	aliasList = aliasStore.GetAliases(baseScid)
	require.Len(t, aliasList, 2)
	require.Contains(t, aliasList, aliasScid)
	require.Contains(t, aliasList, aliasScid2)

	// Delete the first alias.
	err = aliasStore.DeleteLocalAlias(aliasScid, baseScid)
	require.NoError(t, err)

	// The link updater should be called.
	require.True(t, readOrTimeout(updateChan, channelReadTimeout))

	// We expect to get an error if we attempt to delete the same alias
	// again.
	err = aliasStore.DeleteLocalAlias(aliasScid, baseScid)
	require.ErrorIs(t, err, ErrAliasNotFound)

	// The link updater should _not_ be called.
	select {
	case <-updateChan:
		t.Fatal("link alias updater should not have been called")
	default:
	}

	// Query the aliases and verify that first one doesn't exist anymore.
	aliasList = aliasStore.GetAliases(baseScid)
	require.Len(t, aliasList, 1)
	require.Contains(t, aliasList, aliasScid2)
	require.NotContains(t, aliasList, aliasScid)

	// Delete the second alias.
	err = aliasStore.DeleteLocalAlias(aliasScid2, baseScid)
	require.NoError(t, err)

	// The link updater should be called.
	require.True(t, readOrTimeout(updateChan, channelReadTimeout))

	// Query the aliases and verify that none exists.
	aliasList = aliasStore.GetAliases(baseScid)
	require.Len(t, aliasList, 0)

	// We now request an alias generated by the aliasStore. This should give
	// the first from the pre-defined list of allocated aliases.
	firstRequested, err := aliasStore.RequestAlias()
	require.NoError(t, err)
	require.Equal(t, StartingAlias, firstRequested)

	// We now manually add the next alias from the range as a custom alias.
	// This time we also use the base lookup option, in order to be able to
	// go from alias back to the base scid. The WithBaseLookup option also
	// marks this alias as persistent.
	secondAlias := getNextScid(firstRequested)
	err = aliasStore.AddLocalAlias(
		secondAlias, baseScid, false, true, WithBaseLookup(),
	)
	require.NoError(t, err)

	baseLookup, err := aliasStore.FindBaseSCID(secondAlias)
	require.NoError(t, err)

	require.Equal(t, baseScid, baseLookup)

	// When we now request another alias from the allocation list, we expect
	// the third one (tx position 2) to be returned.
	thirdRequested, err := aliasStore.RequestAlias()
	require.NoError(t, err)
	require.Equal(t, getNextScid(secondAlias), thirdRequested)
	require.EqualValues(t, 2, thirdRequested.TxPosition)
}

// TestGetNextScid tests that given a current lnwire.ShortChannelID,
// getNextScid returns the expected alias to use next.
func TestGetNextScid(t *testing.T) {
	tests := []struct {
		name     string
		current  lnwire.ShortChannelID
		expected lnwire.ShortChannelID
	}{
		{
			name:    "starting alias",
			current: StartingAlias,
			expected: lnwire.ShortChannelID{
				BlockHeight: AliasStartBlockHeight,
				TxIndex:     0,
				TxPosition:  1,
			},
		},
		{
			name: "txposition rollover",
			current: lnwire.ShortChannelID{
				BlockHeight: 16_100_000,
				TxIndex:     15,
				TxPosition:  65535,
			},
			expected: lnwire.ShortChannelID{
				BlockHeight: 16_100_000,
				TxIndex:     16,
				TxPosition:  0,
			},
		},
		{
			name: "txindex max no rollover",
			current: lnwire.ShortChannelID{
				BlockHeight: 16_100_000,
				TxIndex:     16777215,
				TxPosition:  15,
			},
			expected: lnwire.ShortChannelID{
				BlockHeight: 16_100_000,
				TxIndex:     16777215,
				TxPosition:  16,
			},
		},
		{
			name: "txindex rollover",
			current: lnwire.ShortChannelID{
				BlockHeight: 16_100_000,
				TxIndex:     16777215,
				TxPosition:  65535,
			},
			expected: lnwire.ShortChannelID{
				BlockHeight: 16_100_001,
				TxIndex:     0,
				TxPosition:  0,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			nextScid := getNextScid(test.current)
			require.Equal(t, test.expected, nextScid)
		})
	}
}

// TestPersistentAlias tests that aliases marked as persistent remain in the
// aliasToBase map even after the base SCID is confirmed.
func TestPersistentAlias(t *testing.T) {
	t.Parallel()

	// Create the backend database and use this to create the aliasStore.
	dbPath := filepath.Join(t.TempDir(), "testdb")
	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
		false,
	)
	require.NoError(t, err)
	defer db.Close()

	linkUpdater := func(shortID lnwire.ShortChannelID) error {
		return nil
	}

	aliasStore, err := NewManager(db, linkUpdater)
	require.NoError(t, err)

	const (
		base               = uint64(123123123)
		nonPersistentAlias = uint64(456456456)
		persistentAlias    = uint64(789789789)
	)

	baseScid := lnwire.NewShortChanIDFromInt(base)
	nonPersistentScid := lnwire.NewShortChanIDFromInt(nonPersistentAlias)
	persistentScid := lnwire.NewShortChanIDFromInt(persistentAlias)

	// Add a non-persistent alias (without WithBaseLookup option).
	err = aliasStore.AddLocalAlias(
		nonPersistentScid, baseScid, true, false,
	)
	require.NoError(t, err)

	// Add a persistent alias (with WithBaseLookup option).
	err = aliasStore.AddLocalAlias(
		persistentScid, baseScid, true, false, WithBaseLookup(),
	)
	require.NoError(t, err)

	// Both aliases should be in aliasToBase before confirmation.
	_, err = aliasStore.FindBaseSCID(nonPersistentScid)
	require.NoError(t, err)

	_, err = aliasStore.FindBaseSCID(persistentScid)
	require.NoError(t, err)

	// Mark the base as confirmed (simulating 6 confirmations).
	err = aliasStore.DeleteSixConfs(baseScid)
	require.NoError(t, err)

	// Non-persistent alias should no longer be findable.
	_, err = aliasStore.FindBaseSCID(nonPersistentScid)
	require.Error(t, err)

	// Persistent alias should still be findable.
	foundBase, err := aliasStore.FindBaseSCID(persistentScid)
	require.NoError(t, err)
	require.Equal(t, baseScid, foundBase)

	// Restart: create a new manager with the same database.
	aliasStore2, err := NewManager(db, linkUpdater)
	require.NoError(t, err)

	// After restart, non-persistent alias should still not be findable.
	_, err = aliasStore2.FindBaseSCID(nonPersistentScid)
	require.Error(t, err)

	// Persistent alias should still be findable after restart.
	foundBase, err = aliasStore2.FindBaseSCID(persistentScid)
	require.NoError(t, err)
	require.Equal(t, baseScid, foundBase)

	// Both aliases should still be in baseToSet.
	aliases := aliasStore2.GetAliases(baseScid)
	require.Len(t, aliases, 2)
	require.Contains(t, aliases, nonPersistentScid)
	require.Contains(t, aliases, persistentScid)
}

// TestBackwardCompatibility tests that old aliases (without flags) are still
// loaded correctly.
func TestBackwardCompatibility(t *testing.T) {
	t.Parallel()

	// Create the backend database and use this to create the aliasStore.
	dbPath := filepath.Join(t.TempDir(), "testdb")
	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
		false,
	)
	require.NoError(t, err)
	defer db.Close()

	linkUpdater := func(shortID lnwire.ShortChannelID) error {
		return nil
	}

	// Manually write an old-format alias (8 bytes, no flags).
	const (
		base  = uint64(111111111)
		alias = uint64(222222222)
	)

	err = kvdb.Update(db, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(aliasBucket)
		if err != nil {
			return err
		}

		var aliasBytes [8]byte
		var baseBytes [8]byte
		byteOrder.PutUint64(aliasBytes[:], alias)
		byteOrder.PutUint64(baseBytes[:], base)

		// Write only 8 bytes (old format, no flags).
		return bucket.Put(aliasBytes[:], baseBytes[:])
	}, func() {})
	require.NoError(t, err)

	// Create the manager - it should load the old entry.
	aliasStore, err := NewManager(db, linkUpdater)
	require.NoError(t, err)

	baseScid := lnwire.NewShortChanIDFromInt(base)
	aliasScid := lnwire.NewShortChanIDFromInt(alias)

	// The alias should be in baseToSet.
	aliases := aliasStore.GetAliases(baseScid)
	require.Len(t, aliases, 1)
	require.Contains(t, aliases, aliasScid)

	// The alias should be findable (since it's not confirmed).
	foundBase, err := aliasStore.FindBaseSCID(aliasScid)
	require.NoError(t, err)
	require.Equal(t, baseScid, foundBase)

	// Mark as confirmed - old aliases should not persist.
	err = aliasStore.DeleteSixConfs(baseScid)
	require.NoError(t, err)

	// Should no longer be findable after confirmation.
	_, err = aliasStore.FindBaseSCID(aliasScid)
	require.Error(t, err)
}

// TestDeletePersistentAlias tests that persistent aliases can be manually
// deleted via DeleteLocalAlias.
func TestDeletePersistentAlias(t *testing.T) {
	t.Parallel()

	// Create the backend database and use this to create the aliasStore.
	dbPath := filepath.Join(t.TempDir(), "testdb")
	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
		false,
	)
	require.NoError(t, err)
	defer db.Close()

	updateChan := make(chan struct{}, 1)
	linkUpdater := func(shortID lnwire.ShortChannelID) error {
		updateChan <- struct{}{}
		return nil
	}

	aliasStore, err := NewManager(db, linkUpdater)
	require.NoError(t, err)

	const (
		base            = uint64(123123123)
		persistentAlias = uint64(789789789)
	)

	baseScid := lnwire.NewShortChanIDFromInt(base)
	persistentScid := lnwire.NewShortChanIDFromInt(persistentAlias)

	// Add a persistent alias (with WithBaseLookup option).
	err = aliasStore.AddLocalAlias(
		persistentScid, baseScid, true, true, WithBaseLookup(),
	)
	require.NoError(t, err)

	// Link updater should be called.
	require.True(t, readOrTimeout(updateChan, channelReadTimeout))

	// Alias should be findable.
	foundBase, err := aliasStore.FindBaseSCID(persistentScid)
	require.NoError(t, err)
	require.Equal(t, baseScid, foundBase)

	// Mark as confirmed - persistent alias should survive.
	err = aliasStore.DeleteSixConfs(baseScid)
	require.NoError(t, err)

	// Persistent alias should still be findable after confirmation.
	foundBase, err = aliasStore.FindBaseSCID(persistentScid)
	require.NoError(t, err)
	require.Equal(t, baseScid, foundBase)

	// Now manually delete the persistent alias.
	err = aliasStore.DeleteLocalAlias(persistentScid, baseScid)
	require.NoError(t, err)

	// Link updater should be called.
	require.True(t, readOrTimeout(updateChan, channelReadTimeout))

	// Alias should no longer be findable.
	_, err = aliasStore.FindBaseSCID(persistentScid)
	require.Error(t, err)

	// Alias should not be in baseToSet.
	aliases := aliasStore.GetAliases(baseScid)
	require.Len(t, aliases, 0)

	// Verify it's deleted from database by restarting.
	aliasStore2, err := NewManager(db, linkUpdater)
	require.NoError(t, err)

	// Should still not be findable after restart.
	_, err = aliasStore2.FindBaseSCID(persistentScid)
	require.Error(t, err)

	// Should not be in baseToSet after restart.
	aliases = aliasStore2.GetAliases(baseScid)
	require.Len(t, aliases, 0)
}

// readOrTimeout attempts to read an element from the given channel of type T.
// If the item is read successfully then true is returned. If the timeout is
// reached then it returns false.
func readOrTimeout[T any](ch <-chan T, timeout time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

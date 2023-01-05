package aliasmgr

import (
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestAliasStorePeerAlias tests that putting and retrieving a peer's alias
// works properly.
func TestAliasStorePeerAlias(t *testing.T) {
	t.Parallel()

	// Create the backend database and use this to create the aliasStore.
	dbPath := filepath.Join(t.TempDir(), "testdb")
	db, err := kvdb.Create(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
	)
	require.NoError(t, err)
	defer db.Close()

	aliasStore, err := NewManager(db)
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
	)
	require.NoError(t, err)
	defer db.Close()

	aliasStore, err := NewManager(db)
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
				BlockHeight: uint32(startingBlockHeight),
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

package lncfg_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/stretchr/testify/require"
)

// TestDBDefaultConfig tests that the default DB config is created as expected.
func TestDBDefaultConfig(t *testing.T) {
	defaultConfig := lncfg.DefaultDB()

	require.Equal(t, lncfg.BoltBackend, defaultConfig.Backend)
	require.Equal(
		t, kvdb.DefaultBoltAutoCompactMinAge,
		defaultConfig.Bolt.AutoCompactMinAge,
	)
	require.Equal(t, kvdb.DefaultDBTimeout, defaultConfig.Bolt.DBTimeout)
	// Implicitly, the following fields are default to false.
	require.False(t, defaultConfig.Bolt.AutoCompact)
	require.True(t, defaultConfig.Bolt.NoFreelistSync)
}

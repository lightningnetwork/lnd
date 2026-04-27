//go:build test_db_postgres || test_db_sqlite

package chainparams

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

// TestValidateNetworkMismatch verifies that ValidateNetwork persists the first
// network and fails when a different network is used later.
func TestValidateNetworkMismatch(t *testing.T) {
	t.Parallel()

	store := NewStore(newTestDB(t))

	// First call: persists regtest into the store.
	err := store.ValidateNetwork(t.Context(), &chaincfg.RegressionNetParams)
	require.NoError(t, err)

	// Second call: a different network — must fail with ErrNetworkMismatch.
	err = store.ValidateNetwork(t.Context(), &chaincfg.SimNetParams)
	require.ErrorIs(t, err, ErrNetworkMismatch)
}

// TestValidateNetworkSameNetwork verifies that ValidateNetwork succeeds when
// called repeatedly with the same network (idempotent-match path).
func TestValidateNetworkSameNetwork(t *testing.T) {
	t.Parallel()

	store := NewStore(newTestDB(t))

	// First call: persists the network.
	err := store.ValidateNetwork(t.Context(), &chaincfg.RegressionNetParams)
	require.NoError(t, err)

	// Second call: same network again — reads the stored value and must
	// succeed (idempotent match).
	err = store.ValidateNetwork(t.Context(), &chaincfg.RegressionNetParams)
	require.NoError(t, err)
}

// TestValidateNetworkNormalizesTestnet verifies that network aliases collapse
// to the same persisted value.
func TestValidateNetworkNormalizesTestnet(t *testing.T) {
	t.Parallel()

	store := NewStore(newTestDB(t))

	// First call: persists canonical testnet3 parameters.
	err := store.ValidateNetwork(t.Context(), &chaincfg.TestNet3Params)
	require.NoError(t, err)

	// Second call: same logical network, different Name field — still
	// matches after normalization (not ErrNetworkMismatch).
	testnetAlias := chaincfg.TestNet3Params
	testnetAlias.Name = "testnet"

	err = store.ValidateNetwork(t.Context(), &testnetAlias)
	require.NoError(t, err)
}

// TestValidateNetworkRejectsEmptyName verifies that malformed network params
// are rejected before touching the database.
func TestValidateNetworkRejectsEmptyName(t *testing.T) {
	t.Parallel()

	store := NewStore(newTestDB(t))

	// Empty Params.Name — rejected before any database read or write.
	err := store.ValidateNetwork(t.Context(), &chaincfg.Params{})
	require.ErrorContains(t, err, "must define a network")
}

// TestValidateNetworkRejectsNilParams verifies that callers provide network
// parameters.
func TestValidateNetworkRejectsNilParams(t *testing.T) {
	t.Parallel()

	store := NewStore(newTestDB(t))

	// Nil params — rejected before any database read or write.
	err := store.ValidateNetwork(t.Context(), nil)
	require.ErrorContains(t, err, "must not be nil")
}

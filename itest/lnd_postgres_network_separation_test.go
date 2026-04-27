package itest

import (
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/chainparams"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// testPostgresNetworkSeparation verifies that lnd refuses to start when the
// active Bitcoin network does not match the network stored in the postgres
// chain_params table. This prevents silent data corruption that would occur if
// a user accidentally reused the same postgres DSN across different networks.
//
// Note: the equivalent SQLite scenario (reusing the same .db file across
// networks) is not covered here because the itest harness does not provide a
// direct path to inject an existing SQLite file into a new node. The feature
// is exercised for SQLite at the unit-test level in chainparams/store_test.go.
//
// The test flow is:
//  1. Start lnd on regtest with native SQL enabled → first startup writes
//     "regtest" into the chain_params table.
//  2. Restart with the same postgres DSN and regtest → must succeed, proving
//     ValidateNetwork passes when the active network matches the stored one.
//  3. Stop the node.
//  4. Restart lnd with the same postgres DSN but switch to simnet → lnd must
//     detect the network mismatch and exit with an error.
func testPostgresNetworkSeparation(ht *lntest.HarnessTest) {
	// This test is only relevant for the postgres backend with native SQL.
	// The SQLite equivalent is covered at the unit-test level.
	if !ht.IsPostgresBackend() {
		ht.Skip("node not running with postgres backend")
	}

	// First startup: native SQL applies migrations and persists regtest in
	// chain_params.
	alice := ht.NewNodeWithCoins("Alice", []string{"--db.use-native-sql"})

	// Second startup: same DSN and network — ValidateNetwork must succeed;
	// proves the matching path against a real DB, not only unit tests.
	ht.RestartNode(alice)

	require.NoError(ht, alice.Stop())

	// Direct store check: simnet vs stored regtest must be
	// ErrNetworkMismatch, independent of whether lnd's process failed for
	// the right reason.
	store, err := sqldb.NewPostgresStore(&sqldb.PostgresConfig{
		Dsn:     alice.Cfg.PostgresDsn,
		Timeout: defaultTimeout,
	})
	require.NoError(ht, err)
	defer store.Close()

	chainParamsStore := chainparams.NewStore(store.GetBaseDB())
	err = chainParamsStore.ValidateNetwork(
		ht.Context(), &chaincfg.SimNetParams,
	)
	require.ErrorIs(ht, err, chainparams.ErrNetworkMismatch)

	// Process-level check: restart lnd with simnet while the DB still says
	// regtest — must exit early (ValidateNetwork during startup).
	//
	// Now restart alice but override the network to simnet. The DSN still
	// points at the same postgres database, so lnd should detect the
	// mismatch and refuse to start.
	//
	// ExtraArgs are appended last when building the lnd command line and
	// therefore take precedence over the generated --bitcoin.regtest flag,
	// effectively switching the node to simnet.
	alice.Cfg.NetParams = &chaincfg.SimNetParams
	alice.SetExtraArgs([]string{
		"--db.use-native-sql",
		"--bitcoin.simnet",
		"--bitcoin.node=neutrino",
	})

	// StartLndCmd launches the process without waiting for it to become
	// ready, which is what we want since we expect it to exit early.
	require.NoError(ht, alice.StartLndCmd(ht.Context()))

	// The process should exit with a non-zero status due to the network
	// mismatch error returned by ValidateNetwork. We only assert that the
	// error is non-nil rather than matching the exact OS exit-code string,
	// because WaitForProcessExit may return a harness-level shutdown error
	// when the node exits before writing "Shutdown complete" to its log.
	require.Error(ht, alice.WaitForProcessExit())
}

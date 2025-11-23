//go:build integration && kvdb_postgres

package itest

import "github.com/lightningnetwork/lnd/lntest"

// postgresTestCases is a list of tests that are only run when using postgres
// backend. These tests verify postgres-specific behavior like deferred cleanup.
var postgresTestCases = []*lntest.TestCase{
	{
		Name:     "wipe forwarding packages",
		TestFunc: testWipeForwardingPackagesPostgres,
	},
}

func init() {
	allTestCases = append(allTestCases, postgresTestCases...)
}

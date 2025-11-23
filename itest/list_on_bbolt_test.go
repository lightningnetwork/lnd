//go:build integration && !kvdb_postgres

package itest

import "github.com/lightningnetwork/lnd/lntest"

// immediateCleanupTestCases is a list of tests that are only run when using
// bbolt or sqlite backends. These backends perform immediate cleanup during
// channel close, unlike postgres which defers cleanup to startup.
var immediateCleanupTestCases = []*lntest.TestCase{
	{
		Name:     "wipe forwarding packages",
		TestFunc: testWipeForwardingPackages,
	},
}

func init() {
	allTestCases = append(allTestCases, immediateCleanupTestCases...)
}

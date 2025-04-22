//go:build integration

package itest

import (
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntest"
)

// excludedTestsWindows is a list of tests that are flaky on Windows and should
// be excluded from the test suite atm.
//
// TODO(yy): fix these tests and remove them from this list.
var excludedTestsWindows = append(append([]string{
	"batch channel funding",
	"zero conf channel open",
	"open channel with unstable utxos",
	"funding flow persistence",

	// Gives "channel link not found" error.
	"zero conf-channel policy update public zero conf",

	"listsweeps",
	"sweep htlcs",
	"sweep cpfp anchor incoming timeout",
	"payment succeeded htlc remote swept",
	"3rd party anchor spend",

	"send payment amp",
	"async payments benchmark",
	"async bidirectional payments",

	"multihop-htlc aggregation leased",
	"multihop-htlc aggregation leased zero conf",
	"multihop-htlc aggregation anchor",
	"multihop-htlc aggregation anchor zero conf",
	"multihop-htlc aggregation simple taproot",
	"multihop-htlc aggregation simple taproot zero conf",

	"channel force close-anchor",
	"channel force close-simple taproot",
	"channel backup restore force close",
	"wipe forwarding packages",

	"coop close with htlcs",

	"forward interceptor restart",
	"forward interceptor dedup htlcs",
	"invoice HTLC modifier basic",
	"lookup htlc resolution",

	"on chain to blinded",
	"query blinded route",

	"data loss protection",

	// The following restart cases will fail in windows due to aggregation
	// can sometimes miss grouping one or two inputs in the same sweeping
	// tx, which is likely caused by how the blocks are notified in windows,
	// more investigation is needed.
	"channel force close-anchor restart",
	"channel force close-simple taproot restart",
}, extractNames(
	"coop close with external delivery",
	coopCloseWithExternalTestCases)...,
),
	extractNames("remote signer", remoteSignerTestCases)...,
)

// filterWindowsFlakyTests filters out the flaky tests that are excluded from
// the test suite on Windows.
func filterWindowsFlakyTests() []*lntest.TestCase {
	// filteredTestCases is a substest of allTestCases that excludes the
	// above flaky tests.
	filteredTestCases := make([]*lntest.TestCase, 0, len(allTestCases))

	// Create a set for the excluded test cases for fast lookup.
	excludedSet := fn.NewSet(excludedTestsWindows...)

	// Remove the tests from the excludedSet if it's found in the list of
	// all test cases. This is done to ensure the excluded tests are not
	// pointing to a test case that doesn't exist.
	for _, tc := range allTestCases {
		if excludedSet.Contains(tc.Name) {
			excludedSet.Remove(tc.Name)

			continue
		}

		filteredTestCases = append(filteredTestCases, tc)
	}

	// Exit early if all the excluded tests are found in allTestCases.
	if excludedSet.IsEmpty() {
		return filteredTestCases
	}

	// Otherwise, print out the tests that are not found in allTestCases.
	errStr := "\nThe following tests are not found, please make sure the " +
		"test names are correct in `excludedTestsWindows`.\n"
	for _, name := range excludedSet.ToSlice() {
		errStr += fmt.Sprintf("Test not found in test suite: %v\n",
			name)
	}

	panic(errStr)
}

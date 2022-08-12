//go:build integration

package funding

import "time"

func init() {
	// For itest, we will use a much shorter checking interval here as
	// local communications are very fast.
	checkPeerFundingLockInterval = 10 * time.Millisecond
}

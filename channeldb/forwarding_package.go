package channeldb

import (
	cstate "github.com/lightningnetwork/lnd/chanstate"
)

var (
	// fwdPackagesKey is retained while the root channeldb bucket setup
	// remains in this package.
	fwdPackagesKey = cstate.FwdPackagesBucketKey()
)

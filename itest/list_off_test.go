//go:build !integration && !bolt12

package itest

import "github.com/lightningnetwork/lnd/lntest"

var allTestCases = []*lntest.TestCase{}

//go:build integration

package aezeed

import "github.com/btcsuite/btcwallet/waddrmgr"

func init() {
	// For the purposes of our itest, we'll crank down the scrypt params a
	// bit.
	scryptN = waddrmgr.FastScryptOptions.N
	scryptR = waddrmgr.FastScryptOptions.R
	scryptP = waddrmgr.FastScryptOptions.P
}

//go:build integration

package macaroons

import "github.com/btcsuite/btcwallet/waddrmgr"

func init() {
	// Below are the reduced scrypt parameters that are used when creating
	// the encryption key for the macaroon database with snacl.NewSecretKey.
	// We use very low values for our itest/rpctest to speed things up.
	scryptN = waddrmgr.FastScryptOptions.N
	scryptR = waddrmgr.FastScryptOptions.R
	scryptP = waddrmgr.FastScryptOptions.P
}

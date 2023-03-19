package macaroons

import "github.com/btcsuite/btcwallet/snacl"

var (
	// Below are the default scrypt parameters that are used when creating
	// the encryption key for the macaroon database with snacl.NewSecretKey.
	scryptN = snacl.DefaultN
	scryptR = snacl.DefaultR
	scryptP = snacl.DefaultP
)

//go:build integration
// +build integration

package lnwallet

import "github.com/btcsuite/btcd/btcutil"

// CloseConfsForCapacity returns the number of confirmations to wait
// before signaling a cooperative close. Under integration tests, we
// always return 1 to keep tests fast and deterministic.
func CloseConfsForCapacity(capacity btcutil.Amount) uint32 { //nolint:revive
	return 1
}
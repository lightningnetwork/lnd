//go:build !integration
// +build !integration

package lnwallet

import "github.com/btcsuite/btcd/btcutil"

// CloseConfsForCapacity returns the number of confirmations to wait
// before signaling a cooperative close, scaled by channel capacity.
// This uses the ScaleNumConfs function for consistent scaling with
// funding confirmations, but without considering push amounts.
func CloseConfsForCapacity(cap btcutil.Amount) uint32 { //nolint:revive
	// For cooperative closes, we don't have a push amount to consider,
	// so we pass 0 for the pushAmt parameter.
	return uint32(ScaleNumConfs(cap, 0))
}
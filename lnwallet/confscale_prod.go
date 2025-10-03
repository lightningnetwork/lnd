//go:build !integration
// +build !integration

package lnwallet

import "github.com/btcsuite/btcd/btcutil"

// CloseConfsForCapacity returns the number of confirmations to wait
// before signaling a cooperative close, scaled by channel capacity.
// We enforce a minimum of 3 confirmations to provide better reorg protection,
// even for small channels.
func CloseConfsForCapacity(capacity btcutil.Amount) uint32 { //nolint:revive
	// For cooperative closes, we don't have a push amount to consider,
	// so we pass 0 for the pushAmt parameter.
	scaledConfs := uint32(ScaleNumConfs(capacity, 0))
	
	// Enforce a minimum of 3 confirmations for reorg safety.
	// This protects against shallow reorgs which are more common.
	const minCoopCloseConfs = 3
	if scaledConfs < minCoopCloseConfs {
		return minCoopCloseConfs
	}
	
	return scaledConfs
}
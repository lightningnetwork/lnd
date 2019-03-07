package lncfg

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// DefaultBitcoinTimeLockDelta Default TLD for channel policy updates
	DefaultBitcoinTimeLockDelta = 144
	// DefaultBitcoinFeeRate Default fee rate for channel policy updates
	DefaultBitcoinFeeRate = lnwire.MilliSatoshi(1)
	// DefaultBitcoinBaseFeeMSat Default base fee for channel policy updates
	DefaultBitcoinBaseFeeMSat   = lnwire.MilliSatoshi(1000)
)

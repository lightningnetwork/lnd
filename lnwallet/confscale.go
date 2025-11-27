package lnwallet

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// minRequiredConfs is the minimum number of confirmations we'll
	// require for channel operations.
	minRequiredConfs = 1

	// maxRequiredConfs is the maximum number of confirmations we'll
	// require for channel operations.
	maxRequiredConfs = 6

	// maxChannelSize is the maximum expected channel size in satoshis.
	// This matches MaxBtcFundingAmount (0.16777215 BTC).
	maxChannelSize = 16777215
)

// ScaleNumConfs returns a linearly scaled number of confirmations based on the
// provided channel amount and push amount (for funding transactions). The push
// amount represents additional risk when receiving funds.
func ScaleNumConfs(chanAmt btcutil.Amount, pushAmt lnwire.MilliSatoshi) uint16 {
	// For wumbo channels, always require maximum confirmations.
	if chanAmt > maxChannelSize {
		return maxRequiredConfs
	}

	// Calculate total stake: channel amount + push amount. The push amount
	// represents value at risk for the receiver.
	maxChannelSizeMsat := lnwire.NewMSatFromSatoshis(maxChannelSize)
	stake := lnwire.NewMSatFromSatoshis(chanAmt) + pushAmt

	// Scale confirmations linearly based on stake.
	conf := uint64(maxRequiredConfs) * uint64(stake) /
		uint64(maxChannelSizeMsat)

	// Bound the result between minRequiredConfs and maxRequiredConfs.
	if conf < minRequiredConfs {
		conf = minRequiredConfs
	}
	if conf > maxRequiredConfs {
		conf = maxRequiredConfs
	}

	return uint16(conf)
}

// FundingConfsForAmounts returns the number of confirmations to wait for a
// funding transaction, taking into account both the channel amount and any
// pushed amount (which represents additional risk).
func FundingConfsForAmounts(chanAmt btcutil.Amount,
	pushAmt lnwire.MilliSatoshi) uint16 {

	return ScaleNumConfs(chanAmt, pushAmt)
}


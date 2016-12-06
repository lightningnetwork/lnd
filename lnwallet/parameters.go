package lnwallet

import (
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/wallet/txrules"
)

// DefaultDustLimit is used to calculate the dust HTLC amount which will be
// send to other node during funding process.
func DefaultDustLimit() btcutil.Amount {
	return txrules.GetDustThreshold(P2WSHSize, txrules.DefaultRelayFeePerKb)
}

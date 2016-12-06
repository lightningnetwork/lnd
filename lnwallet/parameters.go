package lnwallet

import (
	"github.com/roasbeef/btcwallet/wallet/txrules"
	"github.com/roasbeef/btcutil"
)

// DefaultDustLimit is used to calculate the dust HTLC amount which will be
// proposed to other node during channel creation.
func DefaultDustLimit() btcutil.Amount {
	return txrules.GetDustThreshold(P2WSHSize, txrules.DefaultRelayFeePerKb)
}

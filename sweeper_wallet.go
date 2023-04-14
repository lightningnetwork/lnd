package lnd

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// sweeperWallet is a wrapper around the LightningWallet that implements the
// sweeper's Wallet interface.
type sweeperWallet struct {
	*lnwallet.LightningWallet
}

// newSweeperWallet creates a new sweeper wallet from the given
// LightningWallet.
func newSweeperWallet(w *lnwallet.LightningWallet) *sweeperWallet {
	return &sweeperWallet{
		LightningWallet: w,
	}
}

// CancelRebroadcast cancels the rebroadcast of the given transaction.
func (s *sweeperWallet) CancelRebroadcast(txid chainhash.Hash) {
	s.Cfg.Rebroadcaster.MarkAsConfirmed(txid)
}

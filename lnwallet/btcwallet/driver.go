package btcwallet

import (
	"fmt"

	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	walletType = "btcwallet"
)

// createNewWallet creates a new instance of BtcWallet given the proper list of
// initialization parameters. This function is the factory function required to
// properly create an instance of the lnwallet.WalletDriver struct for
// BtcWallet.
func createNewWallet(args ...interface{}) (lnwallet.WalletController, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("incorrect number of arguments to .New(...), "+
			"expected 1, instead passed %v", len(args))
	}

	config, ok := args[0].(*Config)
	if !ok {
		return nil, fmt.Errorf("first argument to btcdnotifier.New is " +
			"incorrect, expected a *rpcclient.ConnConfig")
	}

	return New(*config)
}

// init registers a driver for the BtcWallet concrete implementation of the
// lnwallet.WalletController interface.
func init() {
	// Register the driver.
	driver := &lnwallet.WalletDriver{
		WalletType: walletType,
		New:        createNewWallet,
		BackEnds:   chain.BackEnds,
	}

	if err := lnwallet.RegisterWallet(driver); err != nil {
		panic(fmt.Sprintf("failed to register wallet driver '%s': %v",
			walletType, err))
	}
}

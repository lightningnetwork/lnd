package bitcoindnotify

import (
	"fmt"

	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// createNewNotifier creates a new instance of the ChainNotifier interface
// implemented by BitcoindNotifier.
func createNewNotifier(args ...interface{}) (chainntnfs.ChainNotifier, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("incorrect number of arguments to "+
			".New(...), expected 1, instead passed %v", len(args))
	}

	chainConn, ok := args[0].(*chain.BitcoindConn)
	if !ok {
		return nil, fmt.Errorf("first argument to bitcoindnotify.New " +
			"is incorrect, expected a *chain.BitcoindConn")
	}

	return New(chainConn), nil
}

// init registers a driver for the BtcdNotifier concrete implementation of the
// chainntnfs.ChainNotifier interface.
func init() {
	// Register the driver.
	notifier := &chainntnfs.NotifierDriver{
		NotifierType: notifierType,
		New:          createNewNotifier,
	}

	if err := chainntnfs.RegisterNotifier(notifier); err != nil {
		panic(fmt.Sprintf("failed to register notifier driver '%s': %v",
			notifierType, err))
	}
}

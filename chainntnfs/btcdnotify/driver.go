package btcdnotify

import (
	"fmt"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/roasbeef/btcrpcclient"
)

// createNewNotifier creates a new instance of the ChainNotifier interface
// implemented by BtcdNotifier.
func createNewNotifier(args ...interface{}) (chainntnfs.ChainNotifier, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("incorrect number of arguments to .New(...), "+
			"expected 1, instead passed %v", len(args))
	}

	config, ok := args[0].(*btcrpcclient.ConnConfig)
	if !ok {
		return nil, fmt.Errorf("first argument to btcdnotifier.New is " +
			"incorrect, expected a *btcrpcclient.ConnConfig")
	}

	return New(config)
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

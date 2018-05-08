package bitcoindnotify

import (
	"fmt"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/rpcclient"
)

// createNewNotifier creates a new instance of the ChainNotifier interface
// implemented by BitcoindNotifier.
func createNewNotifier(args ...interface{}) (chainntnfs.ChainNotifier, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("incorrect number of arguments to "+
			".New(...), expected 3, instead passed %v", len(args))
	}

	config, ok := args[0].(*rpcclient.ConnConfig)
	if !ok {
		return nil, fmt.Errorf("first argument to bitcoindnotifier." +
			"New is incorrect, expected a *rpcclient.ConnConfig")
	}

	zmqConnect, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("second argument to bitcoindnotifier." +
			"New is incorrect, expected a string")
	}

	params, ok := args[2].(chaincfg.Params)
	if !ok {
		return nil, fmt.Errorf("third argument to bitcoindnotifier." +
			"New is incorrect, expected a chaincfg.Params")
	}

	return New(config, zmqConnect, params)
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

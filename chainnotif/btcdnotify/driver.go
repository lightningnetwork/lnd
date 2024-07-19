package btcdnotify

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainnotif"
)

// createNewNotifier creates a new instance of the ChainNotifier interface
// implemented by BtcdNotifier.
func createNewNotifier(args ...interface{}) (chainnotif.ChainNotifier, error) {
	if len(args) != 5 {
		return nil, fmt.Errorf("incorrect number of arguments to "+
			".New(...), expected 5, instead passed %v", len(args))
	}

	config, ok := args[0].(*rpcclient.ConnConfig)
	if !ok {
		return nil, errors.New("first argument to btcdnotify.New " +
			"is incorrect, expected a *rpcclient.ConnConfig")
	}

	chainParams, ok := args[1].(*chaincfg.Params)
	if !ok {
		return nil, errors.New("second argument to btcdnotify.New " +
			"is incorrect, expected a *chaincfg.Params")
	}

	spendHintCache, ok := args[2].(chainnotif.SpendHintCache)
	if !ok {
		return nil, errors.New("third argument to btcdnotify.New " +
			"is incorrect, expected a chainnotif.SpendHintCache")
	}

	confirmHintCache, ok := args[3].(chainnotif.ConfirmHintCache)
	if !ok {
		return nil, errors.New("fourth argument to btcdnotify.New " +
			"is incorrect, expected a chainnotif.ConfirmHintCache")
	}

	blockCache, ok := args[4].(*blockcache.BlockCache)
	if !ok {
		return nil, errors.New("fifth argument to btcdnotify.New " +
			"is incorrect, expected a *blockcache.BlockCache")
	}

	return New(
		config, chainParams, spendHintCache, confirmHintCache, blockCache,
	)
}

// init registers a driver for the BtcdNotifier concrete implementation of the
// chainnotif.ChainNotifier interface.
func init() {
	// Register the driver.
	notifier := &chainnotif.NotifierDriver{
		NotifierType: notifierType,
		New:          createNewNotifier,
	}

	if err := chainnotif.RegisterNotifier(notifier); err != nil {
		panic(fmt.Sprintf("failed to register notifier driver '%s': %v",
			notifierType, err))
	}
}

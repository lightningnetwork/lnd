package electrumnotify

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/electrum"
)

// createNewNotifier creates a new instance of the ChainNotifier interface
// implemented by ElectrumNotifier.
func createNewNotifier(args ...interface{}) (chainntnfs.ChainNotifier, error) {
	if len(args) != 5 {
		return nil, fmt.Errorf("incorrect number of arguments to "+
			".New(...), expected 5, instead passed %v", len(args))
	}

	client, ok := args[0].(*electrum.Client)
	if !ok {
		return nil, errors.New("first argument to electrumnotify.New " +
			"is incorrect, expected a *electrum.Client")
	}

	chainParams, ok := args[1].(*chaincfg.Params)
	if !ok {
		return nil, errors.New("second argument to electrumnotify.New " +
			"is incorrect, expected a *chaincfg.Params")
	}

	spendHintCache, ok := args[2].(chainntnfs.SpendHintCache)
	if !ok {
		return nil, errors.New("third argument to electrumnotify.New " +
			"is incorrect, expected a chainntnfs.SpendHintCache")
	}

	confirmHintCache, ok := args[3].(chainntnfs.ConfirmHintCache)
	if !ok {
		return nil, errors.New("fourth argument to electrumnotify.New " +
			"is incorrect, expected a chainntnfs.ConfirmHintCache")
	}

	blockCache, ok := args[4].(*blockcache.BlockCache)
	if !ok {
		return nil, errors.New("fifth argument to electrumnotify.New " +
			"is incorrect, expected a *blockcache.BlockCache")
	}

	return New(client, chainParams, spendHintCache,
		confirmHintCache, blockCache), nil
}

// init registers a driver for the ElectrumNotifier concrete implementation of
// the chainntnfs.ChainNotifier interface.
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

package neutrinonotify

import (
	"errors"
	"fmt"

	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainnotif"
)

// createNewNotifier creates a new instance of the ChainNotifier interface
// implemented by NeutrinoNotifier.
func createNewNotifier(args ...interface{}) (chainnotif.ChainNotifier, error) {
	if len(args) != 4 {
		return nil, fmt.Errorf("incorrect number of arguments to "+
			".New(...), expected 4, instead passed %v", len(args))
	}

	config, ok := args[0].(*neutrino.ChainService)
	if !ok {
		return nil, errors.New("first argument to neutrinonotify.New " +
			"is incorrect, expected a *neutrino.ChainService")
	}

	spendHintCache, ok := args[1].(chainnotif.SpendHintCache)
	if !ok {
		return nil, errors.New("second argument to neutrinonotify.New" +
			" is  incorrect, expected a chainntfs.SpendHintCache")
	}

	confirmHintCache, ok := args[2].(chainnotif.ConfirmHintCache)
	if !ok {
		return nil, errors.New("third argument to neutrinonotify.New " +
			"is  incorrect, expected a chainntfs.ConfirmHintCache")
	}

	blockCache, ok := args[3].(*blockcache.BlockCache)
	if !ok {
		return nil, errors.New("fourth argument to neutrinonotify.New" +
			" is incorrect, expected a *blockcache.BlockCache")
	}

	return New(config, spendHintCache, confirmHintCache, blockCache), nil
}

// init registers a driver for the NeutrinoNotify concrete implementation of
// the chainnotif.ChainNotifier interface.
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

package bitcoindnotify

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainnotif"
)

// createNewNotifier creates a new instance of the ChainNotifier interface
// implemented by BitcoindNotifier.
func createNewNotifier(args ...interface{}) (chainnotif.ChainNotifier, error) {
	if len(args) != 5 {
		return nil, fmt.Errorf("incorrect number of arguments to "+
			".New(...), expected 5, instead passed %v", len(args))
	}

	chainConn, ok := args[0].(*chain.BitcoindConn)
	if !ok {
		return nil, errors.New("first argument to bitcoindnotify.New " +
			"is incorrect, expected a *chain.BitcoindConn")
	}

	chainParams, ok := args[1].(*chaincfg.Params)
	if !ok {
		return nil, errors.New("second argument to bitcoindnotify.New " +
			"is incorrect, expected a *chaincfg.Params")
	}

	spendHintCache, ok := args[2].(chainnotif.SpendHintCache)
	if !ok {
		return nil, errors.New("third argument to bitcoindnotify.New " +
			"is incorrect, expected a chainnotif.SpendHintCache")
	}

	confirmHintCache, ok := args[3].(chainnotif.ConfirmHintCache)
	if !ok {
		return nil, errors.New("fourth argument to bitcoindnotify.New " +
			"is incorrect, expected a chainnotif.ConfirmHintCache")
	}

	blockCache, ok := args[4].(*blockcache.BlockCache)
	if !ok {
		return nil, errors.New("fifth argument to bitcoindnotify.New " +
			"is incorrect, expected a *blockcache.BlockCache")
	}

	return New(chainConn, chainParams, spendHintCache,
		confirmHintCache, blockCache), nil
}

// init registers a driver for the BtcdNotifier concrete implementation of the
// chainnotif.ChainNotifier interface.
func init() {
	// Register the driver.
	notifierZMQ := &chainnotif.NotifierDriver{
		NotifierType: notifierTypeZMQ,
		New:          createNewNotifier,
	}
	if err := chainnotif.RegisterNotifier(notifierZMQ); err != nil {
		panic(fmt.Sprintf("failed to register notifier driver '%s': %v",
			notifierTypeZMQ, err))
	}

	notifierRPC := &chainnotif.NotifierDriver{
		NotifierType: notifierTypeRPCPolling,
		New:          createNewNotifier,
	}
	if err := chainnotif.RegisterNotifier(notifierRPC); err != nil {
		panic(fmt.Sprintf("failed to register notifier driver '%s': %v",
			notifierTypeRPCPolling, err))
	}
}

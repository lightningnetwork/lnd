package esploranotify

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/esplora"
)

// createNewNotifier creates a new instance of the EsploraNotifier from a
// config.
func createNewNotifier(args ...interface{}) (chainntnfs.ChainNotifier, error) {
	if len(args) != 5 {
		return nil, fmt.Errorf("incorrect number of arguments to "+
			"createNewNotifier, expected 5, got %d", len(args))
	}

	client, ok := args[0].(*esplora.Client)
	if !ok {
		return nil, fmt.Errorf("first argument must be an " +
			"*esplora.Client")
	}

	chainParams, ok := args[1].(*chaincfg.Params)
	if !ok {
		return nil, fmt.Errorf("second argument must be a " +
			"*chaincfg.Params")
	}

	spendHintCache, ok := args[2].(chainntnfs.SpendHintCache)
	if !ok {
		return nil, fmt.Errorf("third argument must be a " +
			"chainntnfs.SpendHintCache")
	}

	confirmHintCache, ok := args[3].(chainntnfs.ConfirmHintCache)
	if !ok {
		return nil, fmt.Errorf("fourth argument must be a " +
			"chainntnfs.ConfirmHintCache")
	}

	blockCache, ok := args[4].(*blockcache.BlockCache)
	if !ok {
		return nil, fmt.Errorf("fifth argument must be a " +
			"*blockcache.BlockCache")
	}

	return New(client, chainParams, spendHintCache, confirmHintCache,
		blockCache), nil
}

// init registers a driver for the EsploraNotifier.
func init() {
	chainntnfs.RegisterNotifier(&chainntnfs.NotifierDriver{
		NotifierType: notifierType,
		New:          createNewNotifier,
	})
}

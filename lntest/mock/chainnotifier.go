package mock

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// ChainNotifier is a mock implementation of the ChainNotifier interface.
type ChainNotifier struct {
	SpendChan chan *chainntnfs.SpendDetail
	EpochChan chan *chainntnfs.BlockEpoch
	ConfChan  chan *chainntnfs.TxConfirmation
}

// RegisterConfirmationsNtfn returns a ConfirmationEvent that contains a channel
// that the tx confirmation will go over.
func (c *ChainNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent, error) {

	return &chainntnfs.ConfirmationEvent{
		Confirmed: c.ConfChan,
		Cancel:    func() {},
	}, nil
}

// RegisterSpendNtfn returns a SpendEvent that contains a channel that the spend
// details will go over.
func (c *ChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	return &chainntnfs.SpendEvent{
		Spend:  c.SpendChan,
		Cancel: func() {},
	}, nil
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent that contains a channel that
// block epochs will go over.
func (c *ChainNotifier) RegisterBlockEpochNtfn(blockEpoch *chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: c.EpochChan,
		Cancel: func() {},
	}, nil
}

// Start currently returns a dummy value.
func (c *ChainNotifier) Start() error {
	return nil
}

// Started currently returns a dummy value.
func (c *ChainNotifier) Started() bool {
	return true
}

// Stop currently returns a dummy value.
func (c *ChainNotifier) Stop() error {
	return nil
}

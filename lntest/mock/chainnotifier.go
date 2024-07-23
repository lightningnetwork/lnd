package mock

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainnotif"
)

// ChainNotifier is a mock implementation of the ChainNotifier interface.
type ChainNotifier struct {
	SpendChan chan *chainnotif.SpendDetail
	EpochChan chan *chainnotif.BlockEpoch
	ConfChan  chan *chainnotif.TxConfirmation
}

// RegisterConfirmationsNtfn returns a ConfirmationEvent that contains a channel
// that the tx confirmation will go over.
func (c *ChainNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainnotif.NotifierOption) (*chainnotif.ConfirmationEvent,
	error) {

	return &chainnotif.ConfirmationEvent{
		Confirmed: c.ConfChan,
		Cancel:    func() {},
	}, nil
}

// RegisterSpendNtfn returns a SpendEvent that contains a channel that the spend
// details will go over.
func (c *ChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainnotif.SpendEvent, error) {

	return &chainnotif.SpendEvent{
		Spend:  c.SpendChan,
		Cancel: func() {},
	}, nil
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent that contains a channel that
// block epochs will go over.
func (c *ChainNotifier) RegisterBlockEpochNtfn(
	blockEpoch *chainnotif.BlockEpoch) (
	*chainnotif.BlockEpochEvent, error) {

	return &chainnotif.BlockEpochEvent{
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

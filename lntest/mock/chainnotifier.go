package mock

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// ChainNotifier is a mock implementation of the ChainNotifier interface.
type ChainNotifier struct {
	SpendChan      chan *chainntnfs.SpendDetail
	EpochChan      chan *chainntnfs.BlockEpoch
	ConfChan       chan *chainntnfs.TxConfirmation
	ConfRegistered chan struct{}
}

// RegisterConfirmationsNtfn returns a ConfirmationEvent that contains a channel
// that the tx confirmation will go over.
func (c *ChainNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent, error) {

	// Signal that a confirmation registration occurred.
	if c.ConfRegistered != nil {
		select {
		case c.ConfRegistered <- struct{}{}:
		default:
		}
	}

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

// WaitForConfRegistrationAndSend waits for a confirmation registration to
// occur and then sends a confirmation notification. This is a helper function
// for tests that need to ensure the chain watcher has registered for
// confirmations before sending the confirmation.
func (c *ChainNotifier) WaitForConfRegistrationAndSend(t *testing.T) {
	t.Helper()

	// Wait for the chain watcher to register for confirmations.
	select {
	case <-c.ConfRegistered:
	case <-time.After(time.Second * 2):
		t.Fatalf("timeout waiting for conf registration")
	}

	// Send the confirmation to satisfy the confirmation requirement.
	select {
	case c.ConfChan <- &chainntnfs.TxConfirmation{}:
	case <-time.After(time.Second * 1):
		t.Fatalf("unable to send confirmation")
	}
}

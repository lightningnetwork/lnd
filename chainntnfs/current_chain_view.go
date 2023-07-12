package chainntnfs

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/wire"
)

// CurrentChainStateView is an interface that allows the querying of the most
// up-to-date blockchain state with low overhead. Valid implementations of this
// interface must track the latest chain state.
type CurrentChainStateView interface {
	// Gets the most recent block height known to the view
	BestHeight() uint32
	// Gets the most recent block header known to the view
	BestBlockHeader() *wire.BlockHeader
}

// CurrentChainStateTracker is a tiny subsystem that tracks the blockchain tip
// and saves the most recent tip information in memory for querying. It is a
// valid implementation of CurrentChainStateView and additionally includes
// methods for starting and stopping the system.
type CurrentChainStateTracker struct {
	quit         chan struct{}
	wg           sync.WaitGroup
	notifier     *ChainNotifier
	changeStream *BlockEpochEvent
	current      atomic.Pointer[BlockEpoch]
}

// This gets the most recent block height known to the CurrentChainStateTracker.
func (t *CurrentChainStateTracker) BestHeight() uint32 {
	epoch := t.current.Load()
	if epoch == nil {
		return 0
	}

	return uint32(epoch.Height)
}

// This gets the most recent block header known to the CurrentChainStateTracker.
func (t *CurrentChainStateTracker) BestBlockHeader() *wire.BlockHeader {
	epoch := t.current.Load()
	if epoch == nil {
		return nil
	}

	return epoch.BlockHeader
}

// This method is a helper that subscribes to the underlying BlockEpochEvent
// stream and updates the internal values to match the new BlockEpochs that
// are discovered.
//
// MUST be run as a goroutine.
func (t *CurrentChainStateTracker) updateLoop() {
	defer t.wg.Done()
	for {
		select {
		case epoch, ok := <-t.changeStream.Epochs:
			if ok {
				t.current.Store(epoch)
			} else {
				Log.Error(
					"dead epoch stream in " +
						"CurrentChainStateTracker",
				)

				return
			}
		case <-t.quit:
			t.current.Store(nil)
			return
		}
	}
}

// This method starts the CurrentChainStateTracker. It is an error to start it
// if it is already started.
func (t *CurrentChainStateTracker) Start() error {
	if t.changeStream != nil {
		return fmt.Errorf("CurrentChainStateTracker is already started")
	}
	changeStream, err := (*t.notifier).RegisterBlockEpochNtfn(nil)
	if err != nil {
		return err
	}
	t.changeStream = changeStream
	t.quit = make(chan struct{})
	t.wg.Add(1)
	go t.updateLoop()

	return nil
}

// This method stops the CurrentChainStateTracker. It is an error to stop it
// if it has not been started or if it has already been stopped.
func (t *CurrentChainStateTracker) Stop() error {
	if t.changeStream == nil {
		return fmt.Errorf("CurrentChainStateTracker is not running")
	}
	close(t.quit)
	t.wg.Wait()
	t.changeStream.Cancel()
	t.changeStream = nil

	return nil
}

// This function creates a new ChainStateTracker. It will not provide up to date
// information unless it has been started. The ChainNotifier parameter must also
// be started.
func NewChainStateTracker(
	chainNotifier *ChainNotifier,
) *CurrentChainStateTracker {

	return &CurrentChainStateTracker{
		notifier:     chainNotifier,
		changeStream: nil,
	}
}

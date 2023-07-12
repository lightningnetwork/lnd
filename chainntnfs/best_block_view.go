package chainntnfs

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/wire"
)

// BestBlockView is an interface that allows the querying of the most
// up-to-date blockchain state with low overhead. Valid implementations of this
// interface must track the latest chain state.
type BestBlockView interface {
	// BestHeight gets the most recent block height known to the view.
	BestHeight() (uint32, error)

	// BestBlockHeader gets the most recent block header known to the view.
	BestBlockHeader() (*wire.BlockHeader, error)
}

// BestBlockTracker is a tiny subsystem that tracks the blockchain tip
// and saves the most recent tip information in memory for querying. It is a
// valid implementation of BestBlockView and additionally includes
// methods for starting and stopping the system.
type BestBlockTracker struct {
	notifier        ChainNotifier
	blockNtfnStream *BlockEpochEvent
	current         atomic.Pointer[BlockEpoch]
	mu              sync.Mutex
	quit            chan struct{}
	wg              sync.WaitGroup
}

// This is a compile time check to ensure that BestBlockTracker implements
// BestBlockView.
var _ BestBlockView = (*BestBlockTracker)(nil)

// NewBestBlockTracker creates a new BestBlockTracker that isn't running yet.
// It will not provide up to date information unless it has been started. The
// ChainNotifier parameter must also be started prior to starting the
// BestBlockTracker.
func NewBestBlockTracker(chainNotifier ChainNotifier) *BestBlockTracker {
	return &BestBlockTracker{
		notifier:        chainNotifier,
		blockNtfnStream: nil,
		quit:            make(chan struct{}),
	}
}

// BestHeight gets the most recent block height known to the
// BestBlockTracker.
func (t *BestBlockTracker) BestHeight() (uint32, error) {
	epoch := t.current.Load()
	if epoch == nil {
		return 0, errors.New("best block height not yet known")
	}

	return uint32(epoch.Height), nil
}

// BestBlockHeader gets the most recent block header known to the
// BestBlockTracker.
func (t *BestBlockTracker) BestBlockHeader() (*wire.BlockHeader, error) {
	epoch := t.current.Load()
	if epoch == nil {
		return nil, errors.New("best block header not yet known")
	}

	return epoch.BlockHeader, nil
}

// updateLoop is a helper that subscribes to the underlying BlockEpochEvent
// stream and updates the internal values to match the new BlockEpochs that
// are discovered.
//
// MUST be run as a goroutine.
func (t *BestBlockTracker) updateLoop() {
	defer t.wg.Done()
	for {
		select {
		case epoch, ok := <-t.blockNtfnStream.Epochs:
			if !ok {
				Log.Error("dead epoch stream in " +
					"BestBlockTracker")

				return
			}
			t.current.Store(epoch)
		case <-t.quit:
			t.current.Store(nil)
			return
		}
	}
}

// Start starts the BestBlockTracker. It is an error to start it if it
// is already started.
func (t *BestBlockTracker) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.blockNtfnStream != nil {
		return fmt.Errorf("BestBlockTracker is already started")
	}

	var err error
	t.blockNtfnStream, err = t.notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return err
	}

	t.wg.Add(1)
	go t.updateLoop()

	return nil
}

// Stop stops the BestBlockTracker. It is an error to stop it if it has
// not been started or if it has already been stopped.
func (t *BestBlockTracker) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.blockNtfnStream == nil {
		return fmt.Errorf("BestBlockTracker is not running")
	}
	close(t.quit)
	t.wg.Wait()
	t.blockNtfnStream.Cancel()
	t.blockNtfnStream = nil

	return nil
}

package contractcourt

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// Sweeper is responsible for sweeping outputs back into the wallet
type Sweeper struct {
	Notifier chainntnfs.ChainNotifier
	quit     chan struct{}
}

// Start starts the process of constructing and publish sweep txes.
func (s *Sweeper) Start() error {
	s.quit = make(chan struct{})

	newBlockChan, err := s.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return err
	}

	go s.collector(newBlockChan)

	return nil
}

func (s *Sweeper) collector(newBlockChan *chainntnfs.BlockEpochEvent) {
	defer newBlockChan.Cancel()

	for {
		select {
		case _, ok := <-newBlockChan.Epochs:
			// If the epoch channel has been closed, then the
			// ChainNotifier is exiting which means the daemon is
			// as well. Therefore, we exit early also in order to
			// ensure the daemon shuts down gracefully, yet
			// swiftly.
			if !ok {
				return
			}

			// Fetch all input channel from priority queue that have
			// min target height <= epoch + 1

			// Read outpoints from input channels. If channel is
			// closed, ignore. This means the sweep for that output
			// has been cancelled.

			// Construct sweep tx with the outputs.

			// Publish sweep tx.

			// Process publish error (double spend) -> ?

			// Remove from priority queue

			// Log in database outputs that are currently part of an
			// unconfirmed sweep tx. This is to prevent republishing
			// in a new sweep tx after restart.

		case <-s.quit:
			return
		}
	}

}

// AnnounceSweep schedules a sweep for a specific minimum target height. The
// return value is a channel that is expected to provide the outpoint to sweep
// by the time the block height is high enough. The reason to use a blocking
// channel is that we want to make sure we collected all inputs before
// constructing the sweep tx. Otherwise, if block events would reach sweeper and
// the providers of sweep inputs (like resolvers) in the wrong order, inputs
// would be unnecessarily delayed until the next block.
//
// TODO: Maybe we can already pass in the outpoint at this moment?
func (s *Sweeper) AnnounceSweep(minTargetHeight int32) chan wire.OutPoint {
	inputChan := make(chan wire.OutPoint)

	// Insert channel into priority queue keyed with minTargetHeight

	// TODO: publish sweep immediately sometimes?

	return inputChan
}

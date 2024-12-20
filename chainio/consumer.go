package chainio

// BeatConsumer defines a supplementary component that should be used by
// subsystems which implement the `Consumer` interface. It partially implements
// the `Consumer` interface by providing the method `ProcessBlock` such that
// subsystems don't need to re-implement it.
//
// While inheritance is not commonly used in Go, subsystems embedding this
// struct cannot pass the interface check for `Consumer` because the `Name`
// method is not implemented, which gives us a "mortise and tenon" structure.
// In addition to reducing code duplication, this design allows `ProcessBlock`
// to work on the concrete type `Beat` to access its internal states.
type BeatConsumer struct {
	// BlockbeatChan is a channel to receive blocks from Blockbeat. The
	// received block contains the best known height and the txns confirmed
	// in this block.
	BlockbeatChan chan Blockbeat

	// name is the name of the consumer which embeds the BlockConsumer.
	name string

	// quit is a channel that closes when the BlockConsumer is shutting
	// down.
	//
	// NOTE: this quit channel should be mounted to the same quit channel
	// used by the subsystem.
	quit chan struct{}

	// errChan is a buffered chan that receives an error returned from
	// processing this block.
	errChan chan error
}

// NewBeatConsumer creates a new BlockConsumer.
func NewBeatConsumer(quit chan struct{}, name string) BeatConsumer {
	// Refuse to start `lnd` if the quit channel is not initialized. We
	// treat this case as if we are facing a nil pointer dereference, as
	// there's no point to return an error here, which will cause the node
	// to fail to be started anyway.
	if quit == nil {
		panic("quit channel is nil")
	}

	b := BeatConsumer{
		BlockbeatChan: make(chan Blockbeat),
		name:          name,
		errChan:       make(chan error, 1),
		quit:          quit,
	}

	return b
}

// ProcessBlock takes a blockbeat and sends it to the consumer's blockbeat
// channel. It will send it to the subsystem's BlockbeatChan, and block until
// the processed result is received from the subsystem. The subsystem must call
// `NotifyBlockProcessed` after it has finished processing the block.
//
// NOTE: part of the `chainio.Consumer` interface.
func (b *BeatConsumer) ProcessBlock(beat Blockbeat) error {
	// Update the current height.
	beat.logger().Tracef("set current height for [%s]", b.name)

	select {
	// Send the beat to the blockbeat channel. It's expected that the
	// consumer will read from this channel and process the block. Once
	// processed, it should return the error or nil to the beat.Err chan.
	case b.BlockbeatChan <- beat:
		beat.logger().Tracef("Sent blockbeat to [%s]", b.name)

	case <-b.quit:
		beat.logger().Debugf("[%s] received shutdown before sending "+
			"beat", b.name)

		return nil
	}

	// Check the consumer's err chan. We expect the consumer to call
	// `beat.NotifyBlockProcessed` to send the error back here.
	select {
	case err := <-b.errChan:
		beat.logger().Tracef("[%s] processed beat: err=%v", b.name, err)

		return err

	case <-b.quit:
		beat.logger().Debugf("[%s] received shutdown", b.name)
	}

	return nil
}

// NotifyBlockProcessed signals that the block has been processed. It takes the
// blockbeat being processed and an error resulted from processing it. This
// error is then sent back to the consumer's err chan to unblock
// `ProcessBlock`.
//
// NOTE: This method must be called by the subsystem after it has finished
// processing the block.
func (b *BeatConsumer) NotifyBlockProcessed(beat Blockbeat, err error) {
	// Update the current height.
	beat.logger().Tracef("[%s]: notifying beat processed", b.name)

	select {
	case b.errChan <- err:
		beat.logger().Tracef("[%s]: notified beat processed, err=%v",
			b.name, err)

	case <-b.quit:
		beat.logger().Debugf("[%s] received shutdown before notifying "+
			"beat processed", b.name)
	}
}

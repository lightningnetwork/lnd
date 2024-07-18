package chainio

// Blockbeat defines an interface that can be used by subsystems to retrieve
// block data. It is sent by the BlockbeatDispatcher whenever a new block is
// received. Once the subsystem finishes processing the block, it must signal
// it by calling NotifyBlockProcessed.
//
// The blockchain is a state machine - whenever there's a state change, it's
// manifested in a block. The blockbeat is a way to notify subsystems of this
// state change, and to provide them with the data they need to process it. In
// other words, subsystems must react to this state change and should consider
// being driven by the blockbeat in their own state machines.
type Blockbeat interface {
	// NotifyBlockProcessed signals that the block has been processed. It
	// takes an error resulted from processing the block, and a quit chan
	// of the subsystem.
	//
	// NOTE: This method must be called by the subsystem after it has
	// finished processing the block. Extreme caution must be taken when
	// returning an error as it will shutdown lnd.
	//
	// TODO(yy): Define fatal and non-fatal errors.
	NotifyBlockProcessed(err error, quitChan chan struct{})

	// Height returns the current block height.
	Height() int32

	// DispatchConcurrent sends the blockbeat to the specified consumers
	// concurrently.
	DispatchConcurrent(consumers []Consumer) error

	// DispatchConcurrent sends the blockbeat to the specified consumers
	// sequentially.
	DispatchSequential(consumers []Consumer) error
}

// Consumer defines a blockbeat consumer interface. Subsystems that need block
// info must implement it.
type Consumer interface {
	// Name returns a human-readable string for this subsystem.
	Name() string

	// ProcessBlock takes a blockbeat and processes it. A receive-only
	// error chan must be returned.
	//
	// NOTE: When implementing this, it's very important to send back the
	// error or nil to the channel `b.errChan` immediately, otherwise
	// BlockbeatDispatcher will timeout and lnd will shutdown.
	ProcessBlock(b Beat) <-chan error

	// SetCurrentBeat sets the current beat of the consumer.
	//
	// NOTE: This method is used by the BlockbeatDispatcher to set the
	// initial blockbeat for the consumer.
	SetCurrentBeat(b Beat)
}

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

	// currentBeat is the current beat of the consumer.
	currentBeat Beat
}

// NewBeatConsumer creates a new BlockConsumer.
func NewBeatConsumer(quit chan struct{}, name string) BeatConsumer {
	return BeatConsumer{
		BlockbeatChan: make(chan Blockbeat),
		quit:          quit,
		name:          name,
	}
}

// ProcessBlock takes a blockbeat and sends it to the blockbeat channel.
//
// NOTE: part of the `chainio.Consumer` interface.
func (b *BeatConsumer) ProcessBlock(beat Beat) <-chan error {
	// Update the current height.
	b.SetCurrentBeat(beat)

	select {
	// Send the beat to the blockbeat channel. It's expected that the
	// consumer will read from this channel and process the block. Once
	// processed, it should return the error or nil to the beat.Err chan.
	case b.BlockbeatChan <- beat:
		beat.log.Tracef("Sent blockbeat to %v", b.name)

	case <-b.quit:
		beat.log.Debugf("[%s] received shutdown", b.name)

		select {
		case beat.errChan <- nil:
		default:
		}

		return beat.errChan
	}

	return beat.errChan
}

// SetCurrentBeat sets the current beat of the consumer.
//
// NOTE: part of the `chainio.Consumer` interface.
func (b *BeatConsumer) SetCurrentBeat(beat Beat) {
	beat.log.Tracef("set current height for [%s]", b.name)
	b.currentBeat = beat
}

// CurrentBeat returns the current blockbeat of the consumer. This is used by
// subsystems to retrieve the current blockbeat during their startup.
func (b *BeatConsumer) CurrentBeat() Blockbeat {
	return b.currentBeat
}

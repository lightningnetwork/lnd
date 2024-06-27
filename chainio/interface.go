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
	// Height returns the current block height.
	Height() int32
}

// Consumer defines a blockbeat consumer interface. Subsystems that need block
// info must implement it.
type Consumer interface {
	// TODO(yy): We should also define the start methods used by the
	// consumers such that when implementing the interface, the consumer
	// will always be started with a blockbeat. This cannot be enforced at
	// the moment as we need refactor all the start methods to only take a
	// beat.
	//
	// Start(beat Blockbeat) error

	// Name returns a human-readable string for this subsystem.
	Name() string

	// ProcessBlock takes a blockbeat and processes it. It should not
	// return until the subsystem has updated its state based on the block
	// data.
	//
	// NOTE: The consumer must try its best to NOT return an error. If an
	// error is returned from processing the block, it means the subsystem
	// cannot react to onchain state changes and lnd will shutdown.
	ProcessBlock(b Blockbeat) error
}

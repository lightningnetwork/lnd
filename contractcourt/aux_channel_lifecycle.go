package contractcourt

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
)

var errNoAuxChannelLifecycle = errors.New("auxiliary channel lifecycle is " +
	"not configured")

// ChainWatchOwner identifies the subsystem responsible for watching a
// channel's funding lifecycle.
type ChainWatchOwner uint8

const (
	// ChainWatchOwnerLnd means lnd must start its normal chain watcher and
	// channel arbitrator for the channel.
	ChainWatchOwnerLnd ChainWatchOwner = iota

	// ChainWatchOwnerAux means an auxiliary lifecycle is responsible for
	// observing the channel until it can be handed to lnd. Implementations
	// must monitor any external funding ancestry while ownership is
	// deferred.
	ChainWatchOwnerAux
)

// String returns a human-readable chain watch owner.
func (o ChainWatchOwner) String() string {
	switch o {
	case ChainWatchOwnerLnd:
		return "lnd"

	case ChainWatchOwnerAux:
		return "auxiliary lifecycle"

	default:
		return fmt.Sprintf("unknown chain watch owner <%d>", o)
	}
}

// AuxChannelLifecycle coordinates channels whose funding lifecycle is partly
// owned by an external subsystem. Implementations must honor context
// cancellation so lnd can shut down without waiting indefinitely. A lifecycle
// that owns any channel must be installed on every restart until ownership is
// returned to lnd or the channel is fully resolved.
type AuxChannelLifecycle interface {
	// ChainWatchOwner returns the subsystem currently responsible for
	// watching a channel. Returning ChainWatchOwnerAux prevents lnd from
	// watching the funding outpoint until WatchNewChannel is called again
	// after ownership transfers to lnd.
	ChainWatchOwner(context.Context, *chanstate.OpenChannel) (
		ChainWatchOwner, error)

	// PrepareCommitmentPublish makes the channel funding outpoint
	// publishable before lnd records or publishes a local commitment. It
	// must be idempotent because interrupted force closes can be resumed.
	PrepareCommitmentPublish(context.Context, wire.OutPoint) error

	// WaitForChannelFinalization blocks until the auxiliary lifecycle has
	// durably recorded that lnd fully resolved the channel. Implementations
	// own any retry policy, must tolerate replay after restart, and must
	// return when the context is canceled.
	WaitForChannelFinalization(context.Context, wire.OutPoint) error
}

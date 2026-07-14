package peer

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// rbfCloseMessage is a message type that is used to trigger a cooperative fee
// bump, or initiate a close for the first time.
type rbfCloseMessage struct {
	actor.BaseMessage

	// Ctx is the context of the caller that initiated the RBF close. This
	// is propagated to the underlying close request so that cancellation
	// of the caller (e.g. RPC stream disconnect) tears down the associated
	// observer goroutine. The caller's context is distinct from the
	// actor's own lifecycle context that is passed to Receive.
	Ctx context.Context //nolint:containedctx

	// ChanPoint is the channel point of the channel to be closed.
	ChanPoint wire.OutPoint

	// FeeRate is the fee rate to use for the transaction.
	FeeRate chainfee.SatPerKWeight

	// DeliveryScript is the script to use for the transaction.
	DeliveryScript lnwire.DeliveryAddress
}

// MessageType returns the type of the message.
//
// NOTE: This is part of the actor.Message interface.
func (r rbfCloseMessage) MessageType() string {
	return fmt.Sprintf("RbfCloseMessage(%v)", r.ChanPoint)
}

// NewRbfBumpCloseMsg returns a message that can be sent to the RBF actor to
// initiate a new fee bump.
func NewRbfBumpCloseMsg(ctx context.Context, op wire.OutPoint,
	feeRate chainfee.SatPerKWeight,
	deliveryScript lnwire.DeliveryAddress) rbfCloseMessage {

	return rbfCloseMessage{
		Ctx:            ctx,
		ChanPoint:      op,
		FeeRate:        feeRate,
		DeliveryScript: deliveryScript,
	}
}

// RbfCloseActorServiceKey is a service key that can be used to reach an RBF
// chan closer.
type RbfCloseActorServiceKey = actor.ServiceKey[
	rbfCloseMessage, *CoopCloseUpdates,
]

// NewRbfCloserPeerServiceKey returns a new service key that can be used to
// reach an RBF chan closer, via an active peer.
func NewRbfCloserPeerServiceKey(op wire.OutPoint) RbfCloseActorServiceKey {
	opStr := op.String()

	// Just using the channel point here is enough, as we have a unique
	// type here rbfCloseMessage which will handle the final actor
	// selection.
	actorKey := fmt.Sprintf("Peer(RbfChanCloser(%v))", opStr)

	return actor.NewServiceKey[rbfCloseMessage, *CoopCloseUpdates](actorKey)
}

// rbfCloseActor is a wrapper around the Brontide peer to expose the internal
// RBF close state machine as an actor. This is intended for callers that need
// to obtain streaming close updates related to the RBF close process.
type rbfCloseActor struct {
	chanPeer    *Brontide
	actorSystem *actor.ActorSystem
	chanPoint   wire.OutPoint
}

// newRbfCloseActor creates a new instance of the RBF close wrapper actor.
func newRbfCloseActor(chanPoint wire.OutPoint,
	chanPeer *Brontide, actorSystem *actor.ActorSystem) *rbfCloseActor {

	return &rbfCloseActor{
		chanPeer:    chanPeer,
		actorSystem: actorSystem,
		chanPoint:   chanPoint,
	}
}

// registerActor registers a new RBF close actor with the actor system. If an
// instance with the same service key and types are registered, we'll
// unregister before proceeding.
func (r *rbfCloseActor) registerActor() error {
	// First, we'll make the service key of this RBF actor. This'll allow
	// us to spawn the actor in the actor system.
	actorKey := NewRbfCloserPeerServiceKey(r.chanPoint)

	// We only want to have a single actor instance for this rbf closer,
	// so we'll now attempt to unregister any other instances.
	actorKey.UnregisterAll(r.actorSystem)

	// Now that we know that no instances of the actor are present, let's
	// register a new instance. We don't actually need the ref though, as
	// any interested parties can look up the actor via the service key.
	actorID := fmt.Sprintf(
		"PeerWrapper(RbfChanCloser(%s))", r.chanPoint,
	)
	if _, err := actorKey.Spawn(r.actorSystem, actorID, r); err != nil {
		return fmt.Errorf("unable to spawn RBF close actor for "+
			"channel %v: %w", r.chanPoint, err)
	}

	return nil
}

// Receive implements the actor.ActorBehavior interface for the rbf closer
// wrapper. This allows us to expose our specific processes around the coop
// close flow as an actor.
//
// NOTE: This implements the actor.ActorBehavior interface.
func (r *rbfCloseActor) Receive(_ context.Context,
	msg rbfCloseMessage) fn.Result[*CoopCloseUpdates] {

	type retType = *CoopCloseUpdates

	// If RBF coop close isn't permitted, then we'll return an error.
	if !r.chanPeer.rbfCoopCloseAllowed() {
		return fn.Errf[retType]("rbf coop close not enabled for " +
			"channel")
	}

	closeUpdates := &CoopCloseUpdates{
		UpdateChan: make(chan interface{}, 1),
		ErrChan:    make(chan error, 1),
	}

	// We'll re-use the existing switch struct here, even though we're
	// bypassing the switch entirely. We use the caller's context from the
	// message so that canceling the caller (e.g., RPC stream close) also
	// tears down the observer goroutine.
	closeReq := htlcswitch.ChanClose{
		CloseType:      contractcourt.CloseRegular,
		ChanPoint:      &msg.ChanPoint,
		TargetFeePerKw: msg.FeeRate,
		DeliveryScript: msg.DeliveryScript,
		Updates:        closeUpdates.UpdateChan,
		Err:            closeUpdates.ErrChan,
		Ctx:            msg.Ctx,
	}

	err := r.chanPeer.startRbfChanCloser(
		newRPCShutdownInit(&closeReq), msg.ChanPoint,
	)
	if err != nil {
		peerLog.Errorf("unable to start RBF chan closer for "+
			"channel %v: %v", msg.ChanPoint, err)

		return fn.Errf[retType]("unable to start RBF chan "+
			"closer: %w", err)
	}

	return fn.Ok(closeUpdates)
}

// RbfChanCloseActor is a router that will route messages to the relevant RBF
// chan closer.
type RbfChanCloseActor = actor.Router[rbfCloseMessage, *CoopCloseUpdates]

// RbfChanCloserRouter creates a new router that will route messages to the
// relevant RBF chan closer.
func RbfChanCloserRouter(actorSystem *actor.ActorSystem,
	serviceKey RbfCloseActorServiceKey) *RbfChanCloseActor {

	strategy := actor.NewRoundRobinStrategy[
		rbfCloseMessage, *CoopCloseUpdates,
	]()

	return actor.NewRouter(
		actorSystem.Receptionist(), serviceKey, strategy, nil,
	)
}

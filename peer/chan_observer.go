package peer

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
)

// channelView is a view into the current active/global channel state machine
// for a given link.
type channelView interface {
	// OweCommitment returns a boolean value reflecting whether we need to
	// send out a commitment signature because there are outstanding local
	// updates and/or updates in the local commit tx that aren't reflected
	// in the remote commit tx yet.
	OweCommitment() bool

	// IsChannelClean returns true if neither side has pending commitments,
	// neither side has HTLC's, and all updates are locked in irrevocably.
	IsChannelClean() bool

	// MarkCoopBroadcasted persistently marks that the channel close
	// transaction has been broadcast.
	MarkCoopBroadcasted(*wire.MsgTx, lntypes.ChannelParty) error

	// StateSnapshot returns a snapshot of the current fully committed
	// state within the channel.
	StateSnapshot() *channeldb.ChannelSnapshot

	// MarkShutdownSent persists the given ShutdownInfo. The existence of
	// the ShutdownInfo represents the fact that the Shutdown message has
	// been sent by us and so should be re-sent on re-establish.
	MarkShutdownSent(info *channeldb.ShutdownInfo) error
}

// linkController is capable of controlling the flow out incoming/outgoing
// HTLCs to/from the link.
type linkController interface {
	// DisableAdds sets the ChannelUpdateHandler state to allow/reject
	// UpdateAddHtlc's in the specified direction. It returns true if the
	// state was changed and false if the desired state was already set
	// before the method was called.
	DisableAdds(outgoing bool) bool

	// IsFlushing returns true when UpdateAddHtlc's are disabled in the
	// direction of the argument.
	IsFlushing(direction bool) bool
}

// linkNetworkController is an interface that represents an object capable of
// managing interactions with the active channel links from the PoV of the
// gossip network.
type linkNetworkController interface {
	// RequestDisable disables a channel by its channel point.
	RequestDisable(wire.OutPoint, bool) error
}

// chanObserver implements the chancloser.ChanObserver interface for the
// existing LightningChannel struct/instance.
type chanObserver struct {
	chanView    channelView
	link        linkController
	linkNetwork linkNetworkController
}

// newChanObserver creates a new instance of a chanObserver from an active
// channelView.
func newChanObserver(chanView channelView,
	link linkController, linkNetwork linkNetworkController) *chanObserver {

	return &chanObserver{
		chanView:    chanView,
		link:        link,
		linkNetwork: linkNetwork,
	}
}

// NoDanglingUpdates returns true if there are no dangling updates in the
// channel. In other words, there are no active update messages that haven't
// already been covered by a commit sig.
func (l *chanObserver) NoDanglingUpdates() bool {
	return !l.chanView.OweCommitment()
}

// DisableIncomingAdds instructs the channel link to disable process new
// incoming add messages.
func (l *chanObserver) DisableIncomingAdds() error {
	// If there's no link, then we don't need to disable any adds.
	if l.link == nil {
		return nil
	}

	disabled := l.link.DisableAdds(htlcswitch.Incoming)
	if disabled {
		chanPoint := l.chanView.StateSnapshot().ChannelPoint
		peerLog.Debugf("ChannelPoint(%v): link already disabled",
			chanPoint)
	}

	return nil
}

// DisableOutgoingAdds instructs the channel link to disable process new
// outgoing add messages.
func (l *chanObserver) DisableOutgoingAdds() error {
	// If there's no link, then we don't need to disable any adds.
	if l.link == nil {
		return nil
	}

	_ = l.link.DisableAdds(htlcswitch.Outgoing)

	return nil
}

// MarkCoopBroadcasted persistently marks that the channel close transaction
// has been broadcast.
func (l *chanObserver) MarkCoopBroadcasted(tx *wire.MsgTx, local bool) error {
	return l.chanView.MarkCoopBroadcasted(tx, lntypes.Local)
}

// MarkShutdownSent persists the given ShutdownInfo. The existence of the
// ShutdownInfo represents the fact that the Shutdown message has been sent by
// us and so should be re-sent on re-establish.
func (l *chanObserver) MarkShutdownSent(deliveryAddr []byte,
	isInitiator bool) error {

	shutdownInfo := channeldb.NewShutdownInfo(deliveryAddr, isInitiator)
	return l.chanView.MarkShutdownSent(shutdownInfo)
}

// FinalBalances is the balances of the channel once it has been flushed. If
// Some, then this indicates that the channel is now in a state where it's
// always flushed, so we can accelerate the state transitions.
func (l *chanObserver) FinalBalances() fn.Option[chancloser.ShutdownBalances] {
	chanClean := l.chanView.IsChannelClean()

	switch {
	// If we have a link, then the balances are final if both the incoming
	// and outgoing adds are disabled _and_ the channel is clean.
	case l.link != nil && l.link.IsFlushing(htlcswitch.Incoming) &&
		l.link.IsFlushing(htlcswitch.Outgoing) && chanClean:

		fallthrough

	// If we don't have a link, then this is a restart case, so the
	// balances are final.
	case l.link == nil:
		snapshot := l.chanView.StateSnapshot()

		return fn.Some(chancloser.ShutdownBalances{
			LocalBalance:  snapshot.LocalBalance,
			RemoteBalance: snapshot.RemoteBalance,
		})

	// Otherwise, the link is still active and not flushed, so the balances
	// aren't yet final.
	default:
		return fn.None[chancloser.ShutdownBalances]()
	}
}

// DisableChannel disables the target channel.
func (l *chanObserver) DisableChannel() error {
	op := l.chanView.StateSnapshot().ChannelPoint
	return l.linkNetwork.RequestDisable(op, false)
}

// A compile-time assertion to ensure that chanObserver meets the
// chancloser.ChanStateObserver interface.
var _ chancloser.ChanStateObserver = (*chanObserver)(nil)

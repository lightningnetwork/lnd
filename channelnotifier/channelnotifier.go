package channelnotifier

import (
	"sync"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/subscribe"
)

// ChannelNotifier is a subsystem which all active, inactive, and closed channel
// events pipe through. It takes subscriptions for its events, and whenever
// it receives a new event it notifies its subscribers over the proper channel.
type ChannelNotifier struct {
	started sync.Once
	stopped sync.Once

	ntfnServer *subscribe.Server

	chanDB *channeldb.ChannelStateDB
}

// PendingOpenChannelEvent represents a new event where a new channel has
// entered a pending open state.
type PendingOpenChannelEvent struct {
	// ChannelPoint is the channel outpoint for the new channel.
	ChannelPoint *wire.OutPoint

	// PendingChannel is the channel configuration for the newly created
	// channel. This might not have been persisted to the channel DB yet
	// because we are still waiting for the final message from the remote
	// peer.
	PendingChannel *channeldb.OpenChannel
}

// OpenChannelEvent represents a new event where a channel goes from pending
// open to open.
type OpenChannelEvent struct {
	// Channel is the channel that has become open.
	Channel *channeldb.OpenChannel
}

// ActiveLinkEvent represents a new event where the link becomes active in the
// switch. This happens before the ActiveChannelEvent.
type ActiveLinkEvent struct {
	// ChannelPoint is the channel point for the newly active channel.
	ChannelPoint *wire.OutPoint
}

// InactiveLinkEvent represents a new event where the link becomes inactive in
// the switch.
type InactiveLinkEvent struct {
	// ChannelPoint is the channel point for the inactive channel.
	ChannelPoint *wire.OutPoint
}

// ActiveChannelEvent represents a new event where a channel becomes active.
type ActiveChannelEvent struct {
	// ChannelPoint is the channelpoint for the newly active channel.
	ChannelPoint *wire.OutPoint
}

// InactiveChannelEvent represents a new event where a channel becomes inactive.
type InactiveChannelEvent struct {
	// ChannelPoint is the channelpoint for the newly inactive channel.
	ChannelPoint *wire.OutPoint
}

// ClosedChannelEvent represents a new event where a channel becomes closed.
type ClosedChannelEvent struct {
	// CloseSummary is the summary of the channel close that has occurred.
	CloseSummary *channeldb.ChannelCloseSummary
}

// FullyResolvedChannelEvent represents a new event where a channel becomes
// fully resolved.
type FullyResolvedChannelEvent struct {
	// ChannelPoint is the channelpoint for the newly fully resolved
	// channel.
	ChannelPoint *wire.OutPoint
}

// FundingTimeoutEvent represents a new event where a pending-open channel has
// timed out from the PoV of the funding manager because the funding tx
// has not confirmed in the allotted time.
type FundingTimeoutEvent struct {
	// ChannelPoint is the channelpoint for the newly inactive channel.
	ChannelPoint *wire.OutPoint
}

// New creates a new channel notifier. The ChannelNotifier gets channel
// events from peers and from the chain arbitrator, and dispatches them to
// its clients.
func New(chanDB *channeldb.ChannelStateDB) *ChannelNotifier {
	return &ChannelNotifier{
		ntfnServer: subscribe.NewServer(),
		chanDB:     chanDB,
	}
}

// Start starts the ChannelNotifier and all goroutines it needs to carry out its task.
func (c *ChannelNotifier) Start() error {
	var err error
	c.started.Do(func() {
		log.Info("ChannelNotifier starting")
		err = c.ntfnServer.Start()
	})
	return err
}

// Stop signals the notifier for a graceful shutdown.
func (c *ChannelNotifier) Stop() error {
	var err error
	c.stopped.Do(func() {
		log.Info("ChannelNotifier shutting down...")
		defer log.Debug("ChannelNotifier shutdown complete")

		err = c.ntfnServer.Stop()
	})
	return err
}

// SubscribeChannelEvents returns a subscribe.Client that will receive updates
// any time the Server is made aware of a new event. The subscription provides
// channel events from the point of subscription onwards.
//
// TODO(carlaKC): update  to allow subscriptions to specify a block height from
// which we would like to subscribe to events.
func (c *ChannelNotifier) SubscribeChannelEvents() (*subscribe.Client, error) {
	return c.ntfnServer.Subscribe()
}

// NotifyPendingOpenChannelEvent notifies the channelEventNotifier goroutine
// that a new channel is pending. The pending channel is passed as a parameter
// instead of read from the database because it might not yet have been
// persisted to the DB because we still wait for the final message from the
// remote peer.
func (c *ChannelNotifier) NotifyPendingOpenChannelEvent(chanPoint wire.OutPoint,
	pendingChan *channeldb.OpenChannel) {

	event := PendingOpenChannelEvent{
		ChannelPoint:   &chanPoint,
		PendingChannel: pendingChan,
	}

	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send pending open channel update: %v", err)
	}
}

// NotifyOpenChannelEvent notifies the channelEventNotifier goroutine that a
// channel has gone from pending open to open.
func (c *ChannelNotifier) NotifyOpenChannelEvent(chanPoint wire.OutPoint) {
	// Fetch the relevant channel from the database.
	channel, err := c.chanDB.FetchChannel(chanPoint)
	if err != nil {
		log.Warnf("Unable to fetch open channel from the db: %v", err)
	}

	// Send the open event to all channel event subscribers.
	event := OpenChannelEvent{Channel: channel}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send open channel update: %v", err)
	}
}

// NotifyClosedChannelEvent notifies the channelEventNotifier goroutine that a
// channel has closed.
func (c *ChannelNotifier) NotifyClosedChannelEvent(chanPoint wire.OutPoint) {
	// Fetch the relevant closed channel from the database.
	closeSummary, err := c.chanDB.FetchClosedChannel(&chanPoint)
	if err != nil {
		log.Warnf("Unable to fetch closed channel summary from the db: %v", err)
	}

	// Send the closed event to all channel event subscribers.
	event := ClosedChannelEvent{CloseSummary: closeSummary}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send closed channel update: %v", err)
	}
}

// NotifyFullyResolvedChannelEvent notifies the channelEventNotifier goroutine
// that a channel was fully resolved on chain.
func (c *ChannelNotifier) NotifyFullyResolvedChannelEvent(
	chanPoint wire.OutPoint) {

	// Send the resolved event to all channel event subscribers.
	event := FullyResolvedChannelEvent{ChannelPoint: &chanPoint}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send resolved channel update: %v", err)
	}
}

// NotifyFundingTimeout notifies the channelEventNotifier goroutine that
// a funding timeout has occurred for a certain channel point.
func (c *ChannelNotifier) NotifyFundingTimeout(chanPoint wire.OutPoint) {
	// Send this event to all channel event subscribers.
	event := FundingTimeoutEvent{ChannelPoint: &chanPoint}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send funding timeout update: %v for "+
			"ChanPoint(%v)", err, chanPoint)
	}
}

// NotifyActiveLinkEvent notifies the channelEventNotifier goroutine that a
// link has been added to the switch.
func (c *ChannelNotifier) NotifyActiveLinkEvent(chanPoint wire.OutPoint) {
	event := ActiveLinkEvent{ChannelPoint: &chanPoint}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send active link update: %v", err)
	}
}

// NotifyActiveChannelEvent notifies the channelEventNotifier goroutine that a
// channel is active.
func (c *ChannelNotifier) NotifyActiveChannelEvent(chanPoint wire.OutPoint) {
	event := ActiveChannelEvent{ChannelPoint: &chanPoint}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send active channel update: %v", err)
	}
}

// NotifyInactiveLinkEvent notifies the channelEventNotifier goroutine that a
// link has been removed from the switch.
func (c *ChannelNotifier) NotifyInactiveLinkEvent(chanPoint wire.OutPoint) {
	event := InactiveLinkEvent{ChannelPoint: &chanPoint}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send inactive link update: %v", err)
	}
}

// NotifyInactiveChannelEvent notifies the channelEventNotifier goroutine that a
// channel is inactive.
func (c *ChannelNotifier) NotifyInactiveChannelEvent(chanPoint wire.OutPoint) {
	event := InactiveChannelEvent{ChannelPoint: &chanPoint}
	if err := c.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send inactive channel update: %v", err)
	}
}

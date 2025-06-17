package lnd

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
)

// channelNotifier is an implementation of the chanbackup.ChannelNotifier
// interface using the existing channelnotifier.ChannelNotifier struct. This
// implementation allows us to satisfy all the dependencies of the
// chanbackup.SubSwapper struct.
type channelNotifier struct {
	// chanNotifier is the based channel notifier that we'll proxy requests
	// from.
	chanNotifier *channelnotifier.ChannelNotifier

	// addrs is an implementation of the addrSource interface that allows
	// us to get the latest set of addresses for a given node. We'll need
	// this to be able to create an SCB for new channels.
	addrs channeldb.AddrSource
}

// SubscribeChans requests a new channel subscription relative to the initial
// set of known channels. We use the knownChans as a synchronization point to
// ensure that the chanbackup.SubSwapper does not miss any channel open or
// close events in the period between when it's created, and when it requests
// the channel subscription.
//
// NOTE: This is part of the chanbackup.ChannelNotifier interface.
func (c *channelNotifier) SubscribeChans(ctx context.Context,
	startingChans map[wire.OutPoint]struct{}) (
	*chanbackup.ChannelSubscription, error) {

	ltndLog.Infof("Channel backup proxy channel notifier starting")

	// TODO(roasbeef): read existing set of chans and diff

	quit := make(chan struct{})
	chanUpdates := make(chan chanbackup.ChannelEvent, 1)

	// sendChanOpenUpdate is a closure that sends a ChannelEvent to the
	// chanUpdates channel to inform subscribers about new pending or
	// confirmed channels.
	sendChanOpenUpdate := func(newOrPendingChan *channeldb.OpenChannel) {
		_, nodeAddrs, err := c.addrs.AddrsForNode(
			ctx, newOrPendingChan.IdentityPub,
		)
		if err != nil {
			pub := newOrPendingChan.IdentityPub
			ltndLog.Errorf("unable to fetch addrs for %x: %v",
				pub.SerializeCompressed(), err)
		}

		chanEvent := chanbackup.ChannelEvent{
			NewChans: []chanbackup.ChannelWithAddrs{
				{
					OpenChannel: newOrPendingChan,
					Addrs:       nodeAddrs,
				},
			},
		}

		select {
		case chanUpdates <- chanEvent:
		case <-quit:
			return
		}
	}

	// In order to adhere to the interface, we'll proxy the events from the
	// channel notifier to the sub-swapper in a format it understands.
	go func() {
		// First, we'll subscribe to the primary channel notifier so we can
		// obtain events for new opened/closed channels.
		chanSubscription, err := c.chanNotifier.SubscribeChannelEvents()
		if err != nil {
			panic(fmt.Sprintf("unable to subscribe to chans: %v",
				err))
		}

		defer chanSubscription.Cancel()

		for {
			select {

			// A new event has been sent by the chanNotifier, we'll
			// filter out the events we actually care about and
			// send them to the sub-swapper.
			case e := <-chanSubscription.Updates():
				// TODO(roasbeef): batch dispatch ntnfs

				switch event := e.(type) {
				// A new channel has been opened and is still
				// pending. We can still create a backup, even
				// if the final channel ID is not yet available.
				case channelnotifier.PendingOpenChannelEvent:
					pendingChan := event.PendingChannel
					sendChanOpenUpdate(pendingChan)

				// A new channel has been confirmed, we'll
				// obtain the node address, then send to the
				// sub-swapper.
				case channelnotifier.OpenChannelEvent:
					sendChanOpenUpdate(event.Channel)

				// An existing channel has been closed, we'll
				// send only the chanPoint of the closed
				// channel to the sub-swapper.
				case channelnotifier.ClosedChannelEvent:
					chanPoint := event.CloseSummary.ChanPoint
					closeType := event.CloseSummary.CloseType

					// Because we see the contract as closed
					// once our local force close TX
					// confirms, the channel arbitrator
					// already fires on this event. But
					// because our funds can be in limbo for
					// up to 2 weeks worst case we don't
					// want to remove the crucial info we
					// need for sweeping that time locked
					// output before we've actually done so.
					if closeType == channeldb.LocalForceClose {
						ltndLog.Debugf("Channel %v "+
							"was force closed by "+
							"us, not removing "+
							"from channel backup "+
							"until fully resolved",
							chanPoint)

						continue
					}

					chanEvent := chanbackup.ChannelEvent{
						ClosedChans: []wire.OutPoint{
							chanPoint,
						},
					}

					select {
					case chanUpdates <- chanEvent:
					case <-quit:
						return
					}

				// A channel was fully resolved on chain. This
				// should only really interest us if it was a
				// locally force closed channel where we didn't
				// remove the channel already when the close
				// event was fired.
				case channelnotifier.FullyResolvedChannelEvent:
					chanEvent := chanbackup.ChannelEvent{
						ClosedChans: []wire.OutPoint{
							*event.ChannelPoint,
						},
					}

					select {
					case chanUpdates <- chanEvent:
					case <-quit:
						return
					}
				}

			// The cancel method has been called, signalling us to
			// exit
			case <-quit:
				return
			}
		}
	}()

	return &chanbackup.ChannelSubscription{
		ChanUpdates: chanUpdates,
		Cancel: func() {
			close(quit)
		},
	}, nil
}

// A compile-time constraint to ensure channelNotifier implements
// chanbackup.ChannelNotifier.
var _ chanbackup.ChannelNotifier = (*channelNotifier)(nil)

package main

import (
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/channelnotifier"
)

// addrSource is an interface that allow us to get the addresses for a target
// node. We'll need this in order to be able to properly proxy the
// notifications to create SCBs.
type addrSource interface {
	// AddrsForNode returns all known addresses for the target node public
	// key.
	AddrsForNode(nodePub *btcec.PublicKey) ([]net.Addr, error)
}

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
	addrs addrSource
}

// SubscribeChans requests a new channel subscription relative to the initial
// set of known channels. We use the knownChans as a synchronization point to
// ensure that the chanbackup.SubSwapper does not miss any channel open or
// close events in the period between when it's created, and when it requests
// the channel subscription.
//
// NOTE: This is part of the chanbackup.ChannelNotifier interface.
func (c *channelNotifier) SubscribeChans(startingChans map[wire.OutPoint]struct{}) (
	*chanbackup.ChannelSubscription, error) {

	ltndLog.Infof("Channel backup proxy channel notifier starting")

	// TODO(roasbeef): read existing set of chans and diff

	quit := make(chan struct{})
	chanUpdates := make(chan chanbackup.ChannelEvent, 1)

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

				// A new channel has been opened, we'll obtain
				// the node address, then send to the
				// sub-swapper.
				case channelnotifier.OpenChannelEvent:
					nodeAddrs, err := c.addrs.AddrsForNode(
						event.Channel.IdentityPub,
					)
					if err != nil {
						pub := event.Channel.IdentityPub
						ltndLog.Errorf("unable to "+
							"fetch addrs for %x: %v",
							pub.SerializeCompressed(),
							err)
					}

					channel := event.Channel
					chanEvent := chanbackup.ChannelEvent{
						NewChans: []chanbackup.ChannelWithAddrs{
							{
								OpenChannel: channel,
								Addrs:       nodeAddrs,
							},
						},
					}

					select {
					case chanUpdates <- chanEvent:
					case <-quit:
						return
					}

				// An existing channel has been closed, we'll
				// send only the chanPoint of the closed
				// channel to the sub-swapper.
				case channelnotifier.ClosedChannelEvent:
					chanPoint := event.CloseSummary.ChanPoint
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

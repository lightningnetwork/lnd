package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// bandwidthHints provides hints about the currently available balance in our
// channels.
type bandwidthHints interface {
	// availableChanBandwidth returns the total available bandwidth for a
	// channel and a bool indicating whether the channel hint was found.
	// The amount parameter is used to validate the outgoing htlc amount
	// that we wish to add to the channel against its flow restrictions. If
	// a zero amount is provided, the minimum htlc value for the channel
	// will be used. If the channel is unavailable, a zero amount is
	// returned.
	availableChanBandwidth(channelID uint64,
		amount lnwire.MilliSatoshi) (lnwire.MilliSatoshi, bool)
}

// getLinkQuery is the function signature used to lookup a link.
type getLinkQuery func(lnwire.ShortChannelID) (
	htlcswitch.ChannelLink, error)

// bandwidthManager is an implementation of the bandwidthHints interface which
// uses the link lookup provided to query the link for our latest local channel
// balances.
type bandwidthManager struct {
	getLink    getLinkQuery
	localChans map[lnwire.ShortChannelID]struct{}
}

// newBandwidthManager creates a bandwidth manager for the source node provided
// which is used to obtain hints from the lower layer w.r.t the available
// bandwidth of edges on the network. Currently, we'll only obtain bandwidth
// hints for the edges we directly have open ourselves. Obtaining these hints
// allows us to reduce the number of extraneous attempts as we can skip channels
// that are inactive, or just don't have enough bandwidth to carry the payment.
func newBandwidthManager(graph routingGraph, sourceNode route.Vertex,
	linkQuery getLinkQuery) (*bandwidthManager, error) {

	manager := &bandwidthManager{
		getLink:    linkQuery,
		localChans: make(map[lnwire.ShortChannelID]struct{}),
	}

	// First, we'll collect the set of outbound edges from the target
	// source node and add them to our bandwidth manager's map of channels.
	err := graph.forEachNodeChannel(sourceNode,
		func(channel *channeldb.DirectedChannel) error {
			shortID := lnwire.NewShortChanIDFromInt(
				channel.ChannelID,
			)
			manager.localChans[shortID] = struct{}{}

			return nil
		})

	if err != nil {
		return nil, err
	}

	return manager, nil
}

// getBandwidth queries the current state of a link and gets its currently
// available bandwidth. Note that this function assumes that the channel being
// queried is one of our local channels, so any failure to retrieve the link
// is interpreted as the link being offline.
func (b *bandwidthManager) getBandwidth(cid lnwire.ShortChannelID,
	amount lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	link, err := b.getLink(cid)
	if err != nil {
		// If the link isn't online, then we'll report that it has
		// zero bandwidth.
		log.Warnf("ShortChannelID=%v: link not found: %v", cid, err)
		return 0
	}

	// If the link is found within the switch, but it isn't yet eligible
	// to forward any HTLCs, then we'll treat it as if it isn't online in
	// the first place.
	if !link.EligibleToForward() {
		log.Warnf("ShortChannelID=%v: not eligible to forward", cid)
		return 0
	}

	// If our link isn't currently in a state where it can  add another
	// outgoing htlc, treat the link as unusable.
	if err := link.MayAddOutgoingHtlc(amount); err != nil {
		log.Warnf("ShortChannelID=%v: cannot add outgoing htlc: %v",
			cid, err)
		return 0
	}

	// Otherwise, we'll return the current best estimate for the available
	// bandwidth for the link.
	return link.Bandwidth()
}

// availableChanBandwidth returns the total available bandwidth for a channel
// and a bool indicating whether the channel hint was found. If the channel is
// unavailable, a zero amount is returned.
func (b *bandwidthManager) availableChanBandwidth(channelID uint64,
	amount lnwire.MilliSatoshi) (lnwire.MilliSatoshi, bool) {

	shortID := lnwire.NewShortChanIDFromInt(channelID)
	_, ok := b.localChans[shortID]
	if !ok {
		return 0, false
	}

	return b.getBandwidth(shortID, amount), true
}

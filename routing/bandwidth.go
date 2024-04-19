package routing

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
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
		amount lnwire.MilliSatoshi,
		htlcBlob fn.Option[tlv.Blob]) (lnwire.MilliSatoshi, bool)
}

// TlvTrafficShaper is an interface that allows the sender to determine if a
// payment should be carried by a channel based on the TLV records that may be
// present in the `update_add_htlc` message or the channel commitment itself.
type TlvTrafficShaper interface {
	// ShouldCarryPayment returns true if the provided tlv records indicate
	// that this channel may carry out the payment by utilizing external
	// mechanisms.
	ShouldCarryPayment(amt lnwire.MilliSatoshi, htlcTLV,
		channelBlob fn.Option[tlv.Blob]) bool

	// HandleTraffic is called in order to check if the channel identified
	// by the provided channel ID may have external mechanisms that would
	// allow it to carry out the payment.
	HandleTraffic(cid lnwire.ShortChannelID) bool

	AuxHtlcModifier
}

// AuxHtlcModifier is an interface that allows the sender to modify the outgoing
// HTLC of a payment by changing the amount or the wire message tlv records.
type AuxHtlcModifier interface {
	// ProduceHtlcExtraData is a function that, based on the previous extra
	// data blob of an htlc, may produce a different blob or modify the
	// amount of bitcoin this htlc should carry.
	ProduceHtlcExtraData(htlcBlob tlv.Blob,
		chanID uint64) (btcutil.Amount, tlv.Blob, error)
}

// getLinkQuery is the function signature used to lookup a link.
type getLinkQuery func(lnwire.ShortChannelID) (
	htlcswitch.ChannelLink, error)

// bandwidthManager is an implementation of the bandwidthHints interface which
// uses the link lookup provided to query the link for our latest local channel
// balances.
type bandwidthManager struct {
	getLink       getLinkQuery
	localChans    map[lnwire.ShortChannelID]struct{}
	trafficShaper fn.Option[TlvTrafficShaper]
}

// newBandwidthManager creates a bandwidth manager for the source node provided
// which is used to obtain hints from the lower layer w.r.t the available
// bandwidth of edges on the network. Currently, we'll only obtain bandwidth
// hints for the edges we directly have open ourselves. Obtaining these hints
// allows us to reduce the number of extraneous attempts as we can skip channels
// that are inactive, or just don't have enough bandwidth to carry the payment.
func newBandwidthManager(graph routingGraph, sourceNode route.Vertex,
	linkQuery getLinkQuery,
	trafficShaper fn.Option[TlvTrafficShaper]) (*bandwidthManager, error) {

	manager := &bandwidthManager{
		getLink:       linkQuery,
		localChans:    make(map[lnwire.ShortChannelID]struct{}),
		trafficShaper: trafficShaper,
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
	amount lnwire.MilliSatoshi,
	htlcBlob fn.Option[tlv.Blob]) lnwire.MilliSatoshi {

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

	res := fn.MapOptionZ(b.trafficShaper, func(ts TlvTrafficShaper) bool {
		return ts.HandleTraffic(cid)
	})

	// If response is no and we do have a traffic shaper, we can't handle
	// the traffic and return early.
	if !res && b.trafficShaper.IsSome() {
		log.Warnf("ShortChannelID=%v: can't handle traffic", cid)
		return 0
	}

	channelBlob := link.ChannelCustomBlob()

	// Run the wrapped traffic if it exists, otherwise return false.
	res = fn.MapOptionZ(b.trafficShaper, func(ts TlvTrafficShaper) bool {
		return ts.ShouldCarryPayment(amount, htlcBlob, channelBlob)
	})

	// If the traffic shaper indicates that this channel can route the
	// payment, we immediatelly select this channel and return maximum
	// bandwidth as response.
	if res {
		// If the amount is zero, but the traffic shaper signaled that
		// the channel can carry the payment, we'll return the maximum
		// amount. A zero amount is used when we try to figure out if
		// enough balance exists for the payment to be carried out, but
		// at that point we don't know the payment amount in order to
		// return an exact value, so we signal a value that will
		// certainly satisfy the payment amount.
		if amount == 0 {
			// We don't want to just return the max uint64 as this
			// will overflow when further amounts are added
			// together.
			return lnwire.MaxMilliSatoshi / 2
		}

		return amount
	}

	// If the traffic shaper is present and it returned false, we want to
	// skip this channel.
	if b.trafficShaper.IsSome() {
		log.Warnf("ShortChannelID=%v: payment not allowed by traffic "+
			"shaper", cid)
		return 0
	}

	// If our link isn't currently in a state where it can add
	// another outgoing htlc, treat the link as unusable.
	if err := link.MayAddOutgoingHtlc(amount); err != nil {
		log.Warnf("ShortChannelID=%v: cannot add outgoing "+
			"htlc: %v", cid, err)
		return 0
	}

	// Otherwise, we'll return the current best estimate for the
	// available bandwidth for the link.
	return link.Bandwidth()
}

// availableChanBandwidth returns the total available bandwidth for a channel
// and a bool indicating whether the channel hint was found. If the channel is
// unavailable, a zero amount is returned.
func (b *bandwidthManager) availableChanBandwidth(channelID uint64,
	amount lnwire.MilliSatoshi,
	htlcBlob fn.Option[tlv.Blob]) (lnwire.MilliSatoshi, bool) {

	shortID := lnwire.NewShortChanIDFromInt(channelID)
	_, ok := b.localChans[shortID]
	if !ok {
		return 0, false
	}

	return b.getBandwidth(shortID, amount, htlcBlob), true
}

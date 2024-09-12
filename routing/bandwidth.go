package routing

import (
	"fmt"

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
		amount lnwire.MilliSatoshi) (lnwire.MilliSatoshi, bool)

	// firstHopCustomBlob returns the custom blob for the first hop of the
	// payment, if available.
	firstHopCustomBlob() fn.Option[tlv.Blob]
}

// TlvTrafficShaper is an interface that allows the sender to determine if a
// payment should be carried by a channel based on the TLV records that may be
// present in the `update_add_htlc` message or the channel commitment itself.
type TlvTrafficShaper interface {
	AuxHtlcModifier

	// ShouldHandleTraffic is called in order to check if the channel
	// identified by the provided channel ID may have external mechanisms
	// that would allow it to carry out the payment.
	ShouldHandleTraffic(cid lnwire.ShortChannelID,
		fundingBlob fn.Option[tlv.Blob]) (bool, error)

	// PaymentBandwidth returns the available bandwidth for a custom channel
	// decided by the given channel aux blob and HTLC blob. A return value
	// of 0 means there is no bandwidth available. To find out if a channel
	// is a custom channel that should be handled by the traffic shaper, the
	// HandleTraffic method should be called first.
	PaymentBandwidth(htlcBlob, commitmentBlob fn.Option[tlv.Blob],
		linkBandwidth,
		htlcAmt lnwire.MilliSatoshi) (lnwire.MilliSatoshi, error)
}

// AuxHtlcModifier is an interface that allows the sender to modify the outgoing
// HTLC of a payment by changing the amount or the wire message tlv records.
type AuxHtlcModifier interface {
	// ProduceHtlcExtraData is a function that, based on the previous extra
	// data blob of an HTLC, may produce a different blob or modify the
	// amount of bitcoin this htlc should carry.
	ProduceHtlcExtraData(totalAmount lnwire.MilliSatoshi,
		htlcCustomRecords lnwire.CustomRecords) (lnwire.MilliSatoshi,
		lnwire.CustomRecords, error)
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
	firstHopBlob  fn.Option[tlv.Blob]
	trafficShaper fn.Option[TlvTrafficShaper]
}

// newBandwidthManager creates a bandwidth manager for the source node provided
// which is used to obtain hints from the lower layer w.r.t the available
// bandwidth of edges on the network. Currently, we'll only obtain bandwidth
// hints for the edges we directly have open ourselves. Obtaining these hints
// allows us to reduce the number of extraneous attempts as we can skip channels
// that are inactive, or just don't have enough bandwidth to carry the payment.
func newBandwidthManager(graph Graph, sourceNode route.Vertex,
	linkQuery getLinkQuery, firstHopBlob fn.Option[tlv.Blob],
	trafficShaper fn.Option[TlvTrafficShaper]) (*bandwidthManager, error) {

	manager := &bandwidthManager{
		getLink:       linkQuery,
		localChans:    make(map[lnwire.ShortChannelID]struct{}),
		firstHopBlob:  firstHopBlob,
		trafficShaper: trafficShaper,
	}

	// First, we'll collect the set of outbound edges from the target
	// source node and add them to our bandwidth manager's map of channels.
	err := graph.ForEachNodeChannel(sourceNode,
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

	// bandwidthResult is an inline type that we'll use to pass the
	// bandwidth result from the external traffic shaper to the main logic
	// below.
	type bandwidthResult struct {
		// bandwidth is the available bandwidth for the channel as
		// reported by the external traffic shaper. If the external
		// traffic shaper is not handling the channel, this value will
		// be fn.None
		bandwidth fn.Option[lnwire.MilliSatoshi]

		// htlcAmount is the amount we're going to use to check if we
		// can add another HTLC to the channel. If the external traffic
		// shaper is handling the channel, we'll use 0 to just sanity
		// check the number of HTLCs on the channel, since we don't know
		// the actual HTLC amount that will be sent.
		htlcAmount fn.Option[lnwire.MilliSatoshi]
	}

	var (
		// We will pass the link bandwidth to the external traffic
		// shaper. This is the current best estimate for the available
		// bandwidth for the link.
		linkBandwidth = link.Bandwidth()

		bandwidthErr = func(err error) fn.Result[bandwidthResult] {
			return fn.Err[bandwidthResult](err)
		}
	)

	result, err := fn.MapOptionZ(
		b.trafficShaper,
		func(ts TlvTrafficShaper) fn.Result[bandwidthResult] {
			fundingBlob := link.FundingCustomBlob()
			shouldHandle, err := ts.ShouldHandleTraffic(
				cid, fundingBlob,
			)
			if err != nil {
				return bandwidthErr(fmt.Errorf("traffic "+
					"shaper failed to decide whether to "+
					"handle traffic: %w", err))
			}

			log.Debugf("ShortChannelID=%v: external traffic "+
				"shaper is handling traffic: %v", cid,
				shouldHandle)

			// If this channel isn't handled by the external traffic
			// shaper, we'll return early.
			if !shouldHandle {
				return fn.Ok(bandwidthResult{})
			}

			// Ask for a specific bandwidth to be used for the
			// channel.
			commitmentBlob := link.CommitmentCustomBlob()
			auxBandwidth, err := ts.PaymentBandwidth(
				b.firstHopBlob, commitmentBlob, linkBandwidth,
				amount,
			)
			if err != nil {
				return bandwidthErr(fmt.Errorf("failed to get "+
					"bandwidth from external traffic "+
					"shaper: %w", err))
			}

			log.Debugf("ShortChannelID=%v: external traffic "+
				"shaper reported available bandwidth: %v", cid,
				auxBandwidth)

			// We don't know the actual HTLC amount that will be
			// sent using the custom channel. But we'll still want
			// to make sure we can add another HTLC, using the
			// MayAddOutgoingHtlc method below. Passing 0 into that
			// method will use the minimum HTLC value for the
			// channel, which is okay to just check we don't exceed
			// the max number of HTLCs on the channel. A proper
			// balance check is done elsewhere.
			return fn.Ok(bandwidthResult{
				bandwidth:  fn.Some(auxBandwidth),
				htlcAmount: fn.Some[lnwire.MilliSatoshi](0),
			})
		},
	).Unpack()
	if err != nil {
		log.Errorf("ShortChannelID=%v: failed to get bandwidth from "+
			"external traffic shaper: %v", cid, err)

		return 0
	}

	htlcAmount := result.htlcAmount.UnwrapOr(amount)

	// If our link isn't currently in a state where it can add another
	// outgoing htlc, treat the link as unusable.
	if err := link.MayAddOutgoingHtlc(htlcAmount); err != nil {
		log.Warnf("ShortChannelID=%v: cannot add outgoing "+
			"htlc with amount %v: %v", cid, htlcAmount, err)
		return 0
	}

	// If the external traffic shaper determined the bandwidth, we'll return
	// that value, even if it is zero (which would mean no bandwidth is
	// available on that channel).
	reportedBandwidth := result.bandwidth.UnwrapOr(linkBandwidth)

	return reportedBandwidth
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

// firstHopCustomBlob returns the custom blob for the first hop of the payment,
// if available.
func (b *bandwidthManager) firstHopCustomBlob() fn.Option[tlv.Blob] {
	return b.firstHopBlob
}

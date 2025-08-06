package routing

import (
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
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

	// isCustomHTLCPayment returns true if this payment is a custom payment.
	// For custom payments policy checks might not be needed.
	isCustomHTLCPayment() bool
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
	trafficShaper fn.Option[htlcswitch.AuxTrafficShaper]
}

// newBandwidthManager creates a bandwidth manager for the source node provided
// which is used to obtain hints from the lower layer w.r.t the available
// bandwidth of edges on the network. Currently, we'll only obtain bandwidth
// hints for the edges we directly have open ourselves. Obtaining these hints
// allows us to reduce the number of extraneous attempts as we can skip channels
// that are inactive, or just don't have enough bandwidth to carry the payment.
func newBandwidthManager(graph Graph, sourceNode route.Vertex,
	linkQuery getLinkQuery, firstHopBlob fn.Option[tlv.Blob],
	ts fn.Option[htlcswitch.AuxTrafficShaper]) (*bandwidthManager,
	error) {

	manager := &bandwidthManager{
		getLink:       linkQuery,
		localChans:    make(map[lnwire.ShortChannelID]struct{}),
		firstHopBlob:  firstHopBlob,
		trafficShaper: ts,
	}

	// First, we'll collect the set of outbound edges from the target
	// source node and add them to our bandwidth manager's map of channels.
	err := graph.ForEachNodeDirectedChannel(
		sourceNode, func(channel *graphdb.DirectedChannel) error {
			shortID := lnwire.NewShortChanIDFromInt(
				channel.ChannelID,
			)
			manager.localChans[shortID] = struct{}{}

			return nil
		}, func() {
			clear(manager.localChans)
		},
	)
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
	amount lnwire.MilliSatoshi) (lnwire.MilliSatoshi, error) {

	link, err := b.getLink(cid)
	if err != nil {
		return 0, fmt.Errorf("error querying switch for link: %w", err)
	}

	// If the link is found within the switch, but it isn't yet eligible
	// to forward any HTLCs, then we'll treat it as if it isn't online in
	// the first place.
	if !link.EligibleToForward() {
		return 0, fmt.Errorf("link not eligible to forward")
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
		func(s htlcswitch.AuxTrafficShaper) fn.Result[bandwidthResult] {
			auxBandwidth, err := link.AuxBandwidth(
				amount, cid, b.firstHopBlob, s,
			).Unpack()
			if err != nil {
				return bandwidthErr(fmt.Errorf("failed to get "+
					"auxiliary bandwidth: %w", err))
			}

			// If the external traffic shaper is not handling the
			// channel, we'll just return the original bandwidth and
			// no custom amount.
			if !auxBandwidth.IsHandled {
				return fn.Ok(bandwidthResult{})
			}

			// We don't know the actual HTLC amount that will be
			// sent using the custom channel. But we'll still want
			// to make sure we can add another HTLC, using the
			// MayAddOutgoingHtlc method below. Passing 0 into that
			// method will use the minimum HTLC value for the
			// channel, which is okay to just check we don't exceed
			// the max number of HTLCs on the channel. A proper
			// balance check is done elsewhere.
			return fn.Ok(bandwidthResult{
				bandwidth:  auxBandwidth.Bandwidth,
				htlcAmount: fn.Some[lnwire.MilliSatoshi](0),
			})
		},
	).Unpack()
	if err != nil {
		return 0, fmt.Errorf("failed to consult external traffic "+
			"shaper: %w", err)
	}

	htlcAmount := result.htlcAmount.UnwrapOr(amount)

	// If our link isn't currently in a state where it can add another
	// outgoing htlc, treat the link as unusable.
	if err := link.MayAddOutgoingHtlc(htlcAmount); err != nil {
		return 0, fmt.Errorf("cannot add outgoing htlc to channel %v "+
			"with amount %v: %w", cid, htlcAmount, err)
	}

	// If the external traffic shaper determined the bandwidth, we'll return
	// that value, even if it is zero (which would mean no bandwidth is
	// available on that channel).
	reportedBandwidth := result.bandwidth.UnwrapOr(linkBandwidth)

	return reportedBandwidth, nil
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

	bandwidth, err := b.getBandwidth(shortID, amount)
	if err != nil {
		log.Warnf("failed to get bandwidth for channel %v: %v",
			shortID, err)

		return 0, true
	}

	return bandwidth, true
}

// isCustomHTLCPayment returns true if this payment is a custom payment.
// For custom payments policy checks might not be needed.
func (b *bandwidthManager) isCustomHTLCPayment() bool {
	return fn.MapOptionZ(b.firstHopBlob, func(blob tlv.Blob) bool {
		customRecords, err := lnwire.ParseCustomRecords(blob)
		if err != nil {
			log.Warnf("failed to parse custom records when "+
				"checking if payment is custom: %v", err)

			return false
		}

		return fn.MapOptionZ(
			b.trafficShaper,
			func(s htlcswitch.AuxTrafficShaper) bool {
				return s.IsCustomHTLC(customRecords)
			},
		)
	})
}

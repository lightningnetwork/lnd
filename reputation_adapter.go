package lnd

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/reputation"
)

// reputationChannelSource adapts the channel database and channel notifier to
// the read-only reputation.ChannelSource seam. It exposes the node's active
// channels (with their holder-side capacity limits, used to size resource
// buckets) and a stream of open/close events. It is strictly read-only.
type reputationChannelSource struct {
	chanDB   chanstate.Store
	notifier *channelnotifier.ChannelNotifier
}

// newReputationChannelSource builds a reputation.ChannelSource backed by the
// channel state db and channel notifier.
func newReputationChannelSource(chanDB chanstate.Store,
	notifier *channelnotifier.ChannelNotifier) reputation.ChannelSource {

	return &reputationChannelSource{
		chanDB:   chanDB,
		notifier: notifier,
	}
}

// channelInfo extracts the reputation-relevant capacity limits from an open
// channel, reading the holder (local) side limits (matching LDK's
// get_holder_max_*).
func channelInfo(c *channeldb.OpenChannel) reputation.ChannelInfo {
	return reputation.ChannelInfo{
		SCID:             c.ShortChanID(),
		MaxAcceptedHTLCs: c.LocalChanCfg.MaxAcceptedHtlcs,
		MaxInFlightMsat:  c.LocalChanCfg.MaxPendingAmount,
	}
}

// ActiveChannels returns the node's currently open channels and their limits.
func (r *reputationChannelSource) ActiveChannels() ([]reputation.ChannelInfo,
	error) {

	channels, err := r.chanDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	infos := make([]reputation.ChannelInfo, 0, len(channels))
	for _, c := range channels {
		infos = append(infos, channelInfo(c))
	}

	return infos, nil
}

// SubscribeChannelEvents returns a stream of open/close events and a cancel
// func. A goroutine translates the channel notifier's events into the
// reputation seam's ChannelEvent type until cancelled.
func (r *reputationChannelSource) SubscribeChannelEvents() (
	<-chan reputation.ChannelEvent, func(), error) {

	client, err := r.notifier.SubscribeChannelEvents()
	if err != nil {
		return nil, nil, err
	}

	out := make(chan reputation.ChannelEvent, 16)
	quit := make(chan struct{})

	go func() {
		defer close(out)
		for {
			select {
			case <-quit:
				return

			case e, ok := <-client.Updates():
				if !ok {
					return
				}

				ev, relay := translateChannelEvent(e)
				if !relay {
					continue
				}

				select {
				case out <- ev:
				case <-quit:
					return
				}
			}
		}
	}()

	cancel := func() {
		close(quit)
		client.Cancel()
	}

	return out, cancel, nil
}

// circuitSource exposes the switch's open-circuit snapshot to the in-flight
// reconstruction helper. Implemented by *htlcswitch.Switch (ActiveCircuits),
// kept as a seam so the helper is unit-testable with a fake.
type circuitSource interface {
	// ActiveCircuits returns a snapshot of all open (keystoned) payment
	// circuits currently tracked by the switch.
	ActiveCircuits() []*htlcswitch.PaymentCircuit
}

// openChannelSource exposes the node's open channels (with their restored
// commitment HTLC sets) to the in-flight reconstruction helper. Implemented by
// chanstate.Store (FetchAllOpenChannels), kept as a seam for testing.
type openChannelSource interface {
	// FetchAllOpenChannels returns all currently open channels.
	FetchAllOpenChannels() ([]*channeldb.OpenChannel, error)
}

// incomingHTLCMeta carries the two fields that the switch circuit map does not
// retain (cltv expiry and the accountable signal), recovered from the live
// incoming-channel commitment HTLC.
type incomingHTLCMeta struct {
	cltv        uint32
	accountable bool
}

// htlcAccountable extracts the experimental accountable signal (TLV 106823,
// value 7) from a restored commitment HTLC's custom records. The accountable
// bit IS persisted: incoming update_add_htlc custom records are written to the
// commitment HTLC set (lnwallet) and round-tripped through OpenChannel's
// ExtraData on restart, so it is recoverable after a restart.
func htlcAccountable(records lnwire.CustomRecords) bool {
	key := uint64(lnwire.ExperimentalAccountableType)
	rec, ok := records[key]

	return ok && len(rec) > 0 && rec[0] == lnwire.ExperimentalAccountable
}

// buildIncomingHTLCIndex builds a map from incoming circuit key to the
// cltv/accountable metadata recovered from the live incoming-channel commitment
// HTLC sets. It only indexes incoming HTLCs, keyed by (short chan id, htlc
// index) — which matches a circuit's Incoming key (incomingChanID =
// ShortChanID(), incomingHTLCID = the incoming add's HTLC index).
func buildIncomingHTLCIndex(chans []*channeldb.OpenChannel) map[models.CircuitKey]incomingHTLCMeta { //nolint:ll
	index := make(map[models.CircuitKey]incomingHTLCMeta)
	for _, c := range chans {
		scid := c.ShortChanID()
		for _, htlc := range c.ActiveHtlcs() {
			// Circuit incoming keys reference the incoming add;
			// skip HTLCs we offered (outgoing) on this channel.
			if !htlc.Incoming {
				continue
			}

			key := models.CircuitKey{
				ChanID: scid,
				HtlcID: htlc.HtlcIndex,
			}
			accountable := htlcAccountable(htlc.CustomRecords)
			index[key] = incomingHTLCMeta{
				cltv:        htlc.RefundTimeout,
				accountable: accountable,
			}
		}
	}

	return index
}

// assembleInFlightHTLCs joins the switch's open circuit snapshot with the live
// incoming-channel commitment HTLC sets to reconstruct the in-flight HTLCs that
// the reputation manager must replay on restart. Only real forwards are
// included (the incoming side is not our own switch and the keystone is set).
//
// The switch circuit map only retains keys + amounts; the incoming cltv expiry
// and the accountable custom record are recovered from the matching incoming
// commitment HTLC (both persisted and restored). If no matching incoming HTLC
// is found (e.g. the channel resolved/closed concurrently with startup), the
// circuit is skipped — replaying it without its true cltv/accountable would
// poison the reconstructed state.
func assembleInFlightHTLCs(circuits []*htlcswitch.PaymentCircuit,
	chans []*channeldb.OpenChannel) []reputation.InFlightHTLC {

	index := buildIncomingHTLCIndex(chans)

	htlcs := make([]reputation.InFlightHTLC, 0, len(circuits))
	for _, c := range circuits {
		// Filter to real forwards: the incoming side must not be our
		// own switch (locally-initiated payment) and the keystone must
		// be set (the HTLC was actually forwarded out).
		if c.Incoming.ChanID == hop.Source {
			continue
		}
		if c.Outgoing == nil {
			continue
		}

		meta, ok := index[c.Incoming]
		if !ok {
			// The incoming HTLC is no longer live on the
			// commitment; we can't recover its cltv/accountable,
			// so skip it.
			continue
		}

		htlcs = append(htlcs, reputation.InFlightHTLC{
			Incoming:     c.Incoming,
			Outgoing:     *c.Outgoing,
			IncomingAmt:  c.IncomingAmount,
			OutgoingAmt:  c.OutgoingAmount,
			IncomingCltv: meta.cltv,
			Accountable:  meta.accountable,
		})
	}

	return htlcs
}

// reconstructInFlightHTLCs assembles the node's currently in-flight forwarded
// HTLCs from the switch circuit map joined with the live channel commitment
// HTLC sets, for replay into the reputation manager on startup.
func reconstructInFlightHTLCs(switchSrc circuitSource,
	chanSrc openChannelSource) ([]reputation.InFlightHTLC, error) {

	chans, err := chanSrc.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	return assembleInFlightHTLCs(switchSrc.ActiveCircuits(), chans), nil
}

// translateChannelEvent maps a channel notifier event to the reputation seam's
// ChannelEvent, returning relay=false for events that are not open/close.
func translateChannelEvent(e interface{}) (reputation.ChannelEvent, bool) {
	switch ev := e.(type) {
	case channelnotifier.OpenChannelEvent:
		return reputation.ChannelEvent{
			Info: channelInfo(ev.Channel),
			Open: true,
		}, true

	case channelnotifier.ClosedChannelEvent:
		return reputation.ChannelEvent{
			Info: reputation.ChannelInfo{
				SCID: ev.CloseSummary.ShortChanID,
			},
			Open: false,
		}, true

	default:
		return reputation.ChannelEvent{}, false
	}
}

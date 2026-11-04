package lnd

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/chanstate"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/reputation"
	"github.com/lightningnetwork/lnd/subscribe"
)

// circuitSource exposes the switch's open circuits. It is implemented by the
// switch and kept as a seam so the reconstruction can be tested with a fake.
type circuitSource interface {
	// ActiveCircuits returns a snapshot of the open circuits.
	ActiveCircuits() []*htlcswitch.PaymentCircuit
}

// openChannelSource exposes the node's open channels with their commitment
// HTLC sets. It is implemented by the channel state db and kept as a seam so
// the reconstruction can be tested with a fake.
type openChannelSource interface {
	// FetchAllOpenChannels returns all currently open channels.
	FetchAllOpenChannels() ([]*chanstate.OpenChannel, error)
}

// policySource exposes the channel policies in the graph. It is implemented
// by the channel graph and kept as a seam so the reconstruction can be tested
// with a fake.
type policySource interface {
	// FetchChannelEdgesByOutpoint returns the channel's edge info and
	// both directional policies, or graphdb.ErrEdgeNotFound.
	FetchChannelEdgesByOutpoint(ctx context.Context,
		op *wire.OutPoint) (*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error)
}

// fetchSelfPolicies returns this node's own forwarding policy for each of the
// given channels, keyed by funding outpoint. A channel without a policy in the
// graph is left out, the same way the link falls back to defaults for it.
func fetchSelfPolicies(ctx context.Context, graph policySource,
	selfKey [33]byte, chans []*chanstate.OpenChannel) (
	map[wire.OutPoint]*models.ChannelEdgePolicy, error) {

	policies := make(
		map[wire.OutPoint]*models.ChannelEdgePolicy, len(chans),
	)
	for _, c := range chans {
		op := c.FundingOutpoint
		info, p1, p2, err := graph.FetchChannelEdgesByOutpoint(ctx, &op)
		if errors.Is(err, graphdb.ErrEdgeNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}

		// The policy whose edge connects to the other node is ours.
		policy := p2
		if info != nil &&
			bytes.Equal(info.NodeKey1Bytes[:], selfKey[:]) {

			policy = p1
		}
		if policy != nil {
			policies[op] = policy
		}
	}

	return policies, nil
}

// incomingHTLCInfo carries what the circuit map does not retain about an
// in-flight HTLC: the channel_id of the incoming channel, the incoming cltv
// expiry and the accountable signal, the latter two read back from the live
// incoming commitment HTLC.
type incomingHTLCInfo struct {
	chanID      lnwire.ChannelID
	cltv        uint32
	accountable bool
}

// channelIndex joins the open channels to the circuit map. Circuits key both
// of their ends by the short channel id the link reports, which is the one the
// channel state holds (for a zero-conf channel that is its alias, before and
// after confirmation), while the reputation manager keys channels by their
// channel_id.
type channelIndex struct {
	// incoming maps the incoming circuit key of every live incoming
	// commitment HTLC to what the replay needs to know about it. An
	// incoming HTLC is keyed by (short channel id, htlc index), which is
	// exactly a circuit's incoming key.
	incoming map[models.CircuitKey]incomingHTLCInfo

	// chanIDs maps each open channel's short channel id to its channel_id.
	chanIDs map[lnwire.ShortChannelID]lnwire.ChannelID

	// nextHtlcIndex maps each open channel's short channel id to the
	// index its next outgoing HTLC will get. Outgoing HTLCs at or above it
	// never made it into a commitment.
	nextHtlcIndex map[lnwire.ShortChannelID]uint64

	// policies maps each open channel's short channel id to this node's
	// forwarding policy on it, for the channels that have one.
	policies map[lnwire.ShortChannelID]*models.ChannelEdgePolicy
}

// channelInfo is what the reconstruction needs to know about an open channel
// beyond its channel state: this node's policy on it, if any, and the index of
// its next outgoing HTLC. Both are keyed by funding outpoint.
type channelInfo struct {
	policies      map[wire.OutPoint]*models.ChannelEdgePolicy
	nextHtlcIndex map[wire.OutPoint]uint64
}

// indexChannels builds the channelIndex of the given open channels.
func indexChannels(chans []*chanstate.OpenChannel,
	info channelInfo) channelIndex {

	index := channelIndex{
		incoming: make(map[models.CircuitKey]incomingHTLCInfo),
		chanIDs: make(
			map[lnwire.ShortChannelID]lnwire.ChannelID, len(chans),
		),
		nextHtlcIndex: make(
			map[lnwire.ShortChannelID]uint64, len(chans),
		),
		policies: make(
			map[lnwire.ShortChannelID]*models.ChannelEdgePolicy,
			len(info.policies),
		),
	}

	for _, c := range chans {
		scid := c.ShortChanID()
		chanID := lnwire.NewChanIDFromOutPoint(c.FundingOutpoint)
		index.chanIDs[scid] = chanID
		index.nextHtlcIndex[scid] =
			info.nextHtlcIndex[c.FundingOutpoint]

		if policy, ok := info.policies[c.FundingOutpoint]; ok {
			index.policies[scid] = policy
		}

		for _, htlc := range c.ActiveHtlcs() {
			// Circuits reference the incoming add; HTLCs we offered
			// on this channel are the outgoing side of some other
			// circuit.
			if !htlc.Incoming {
				continue
			}

			key := models.CircuitKey{
				ChanID: scid,
				HtlcID: htlc.HtlcIndex,
			}
			index.incoming[key] = incomingHTLCInfo{
				chanID: chanID,
				cltv:   htlc.RefundTimeout,
				accountable: htlcswitch.AccountableFromRecords(
					htlc.CustomRecords,
				),
			}
		}
	}

	return index
}

// requiredFee returns the fee this node requires to forward amtToForward from
// the incoming to the outgoing channel under its current policies, computed
// the same way the switch does for a live forward: the outgoing channel's
// outbound fee plus the incoming channel's inbound fee, clamped at zero. The
// second return value is false if either channel has no policy in the graph.
func (i channelIndex) requiredFee(incoming, outgoing lnwire.ShortChannelID,
	amtToForward lnwire.MilliSatoshi) (lnwire.MilliSatoshi, bool) {

	inPolicy, ok := i.policies[incoming]
	if !ok {
		return 0, false
	}
	outPolicy, ok := i.policies[outgoing]
	if !ok {
		return 0, false
	}

	outFee := htlcswitch.ExpectedFee(models.ForwardingPolicy{
		BaseFee: outPolicy.FeeBaseMSat,
		FeeRate: outPolicy.FeeProportionalMillionths,
	}, amtToForward)

	var inboundFee models.InboundFee
	inPolicy.InboundFee.WhenSome(func(fee lnwire.Fee) {
		inboundFee = models.NewInboundFeeFromWire(fee)
	})

	total := htlcswitch.TotalForwardingFee(amtToForward, outFee, inboundFee)
	if total < 0 {
		total = 0
	}

	return lnwire.MilliSatoshi(total), true
}

// assembleInFlightHTLCs joins the open circuits with the live incoming
// commitment HTLCs into the in-flight HTLCs the reputation manager replays on
// startup. Only forwards are included: the incoming side must not be this
// node's own switch, and the circuit must have been opened on an outgoing
// link. A circuit whose incoming HTLC is no longer live on the commitment is
// skipped, since its cltv and accountable signal cannot be recovered, as is a
// circuit whose outgoing channel is no longer open. A circuit whose outgoing
// HTLC never made it into a commitment is skipped as well: the switch trims
// such circuits when the outgoing link starts and forwards the HTLC again,
// which reports it through the live hook.
//
// The circuit map does not retain the fee the node required for the forward,
// only the amounts. Like the live forward path, the replayed HTLC is scored on
// the fee the node's own policy requires rather than on the fee the sender
// offered, so that over-paying cannot inflate reputation. The policy may have
// changed since the forward was accepted, so the required fee is capped at the
// offered fee, which the node can never earn more than. If either channel has
// no policy in the graph, the offered fee is used.
func assembleInFlightHTLCs(circuits []*htlcswitch.PaymentCircuit,
	chans []*chanstate.OpenChannel,
	info channelInfo) []reputation.InFlightHTLC {

	index := indexChannels(chans, info)

	htlcs := make([]reputation.InFlightHTLC, 0, len(circuits))
	for _, c := range circuits {
		if c.Incoming.ChanID == hop.Source || c.Outgoing == nil {
			continue
		}

		info, ok := index.incoming[c.Incoming]
		if !ok {
			srvrLog.Debugf("Skipping in-flight htlc %v for "+
				"reputation replay: incoming htlc no longer "+
				"live", c.Incoming)

			continue
		}

		outgoingChan, ok := index.chanIDs[c.Outgoing.ChanID]
		if !ok {
			srvrLog.Debugf("Skipping in-flight htlc %v for "+
				"reputation replay: outgoing channel %v no "+
				"longer open", c.Incoming, c.Outgoing.ChanID)

			continue
		}

		if c.Outgoing.HtlcID >= index.nextHtlcIndex[c.Outgoing.ChanID] {
			srvrLog.Debugf("Skipping in-flight htlc %v for "+
				"reputation replay: outgoing htlc %v not "+
				"committed, will be forwarded again",
				c.Incoming, c.Outgoing)

			continue
		}

		var offered lnwire.MilliSatoshi
		if c.IncomingAmount > c.OutgoingAmount {
			offered = c.IncomingAmount - c.OutgoingAmount
		}

		fee := offered
		required, ok := index.requiredFee(
			c.Incoming.ChanID, c.Outgoing.ChanID, c.OutgoingAmount,
		)
		switch {
		case !ok:
			srvrLog.Debugf("No policy for in-flight htlc %v, "+
				"replaying with the offered fee %v",
				c.Incoming, offered)

		case required < offered:
			fee = required
		}

		htlcs = append(htlcs, reputation.InFlightHTLC{
			Incoming:     c.Incoming,
			IncomingChan: info.chanID,
			OutgoingChan: outgoingChan,
			Fee:          fee,
			IncomingCltv: info.cltv,
			Accountable:  info.accountable,
		})
	}

	return htlcs
}

// nextHtlcIndexFunc returns the index an open channel's next outgoing HTLC
// will get. It is chanstate.OpenChannel.NextLocalHtlcIndex in production and
// kept as a seam since that reads the channel database.
type nextHtlcIndexFunc func(*chanstate.OpenChannel) (uint64, error)

// reconstructInFlightHTLCs assembles the forwarded HTLCs that are in flight
// from the switch's open circuits, the live channel commitments and this
// node's policies on the channels.
func reconstructInFlightHTLCs(ctx context.Context, circuits circuitSource,
	chans openChannelSource, graph policySource, selfKey [33]byte,
	nextHtlcIndex nextHtlcIndexFunc) ([]reputation.InFlightHTLC, error) {

	openChans, err := chans.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	policies, err := fetchSelfPolicies(ctx, graph, selfKey, openChans)
	if err != nil {
		return nil, err
	}

	nextIndexes := make(map[wire.OutPoint]uint64, len(openChans))
	for _, c := range openChans {
		next, err := nextHtlcIndex(c)
		if err != nil {
			return nil, fmt.Errorf("next htlc index of channel "+
				"%v: %w", c.FundingOutpoint, err)
		}

		nextIndexes[c.FundingOutpoint] = next
	}

	return assembleInFlightHTLCs(
		circuits.ActiveCircuits(), openChans, channelInfo{
			policies:      policies,
			nextHtlcIndex: nextIndexes,
		},
	), nil
}

// channelRemover removes a channel's state from the reputation manager. It is
// implemented by the manager and kept as a seam for tests.
type channelRemover interface {
	RemoveChannel(chanID lnwire.ChannelID) error
}

// removeClosedChannel drops the channel of a closed channel event from the
// reputation manager. Any other channel event is ignored, as is a closed
// channel event without a summary, which the notifier sends when it cannot
// load the close from the database. A failure to remove is logged: the
// manager is log-only and must not disturb the close.
func removeClosedChannel(mgr channelRemover, event any) {
	closed, ok := event.(channelnotifier.ClosedChannelEvent)
	if !ok || closed.CloseSummary == nil {
		return
	}

	chanID := lnwire.NewChanIDFromOutPoint(closed.CloseSummary.ChanPoint)
	if err := mgr.RemoveChannel(chanID); err != nil {
		srvrLog.Warnf("Unable to remove channel %v from reputation "+
			"manager: %v", chanID, err)
	}
}

// forwardChannelClosesToReputation removes closed channels from the reputation
// manager until the server shuts down. The subscription is taken by the
// caller, before the subsystems that emit close events start, so that none is
// missed.
func (s *server) forwardChannelClosesToReputation(
	client *subscribe.Client) {

	defer s.wg.Done()
	defer client.Cancel()

	for {
		select {
		case event := <-client.Updates():
			removeClosedChannel(s.reputationMgr, event)

		case <-s.quit:
			return
		}
	}
}

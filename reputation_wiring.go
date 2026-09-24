package lnd

import (
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/reputation"
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

// incomingHTLCInfo carries what the circuit map does not retain about an
// in-flight HTLC: the incoming cltv expiry and the accountable signal, both
// read back from the live incoming commitment HTLC.
type incomingHTLCInfo struct {
	cltv        uint32
	accountable bool
}

// indexIncomingHTLCs maps the incoming circuit key of every live incoming
// commitment HTLC to its cltv expiry and accountable signal. An incoming HTLC
// is keyed by (short channel id, htlc index), which is exactly a circuit's
// incoming key.
func indexIncomingHTLCs(
	chans []*chanstate.OpenChannel) map[models.CircuitKey]incomingHTLCInfo {

	index := make(map[models.CircuitKey]incomingHTLCInfo)
	for _, c := range chans {
		scid := c.ShortChanID()
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
			index[key] = incomingHTLCInfo{
				cltv: htlc.RefundTimeout,
				accountable: htlcswitch.AccountableFromRecords(
					htlc.CustomRecords,
				),
			}
		}
	}

	return index
}

// assembleInFlightHTLCs joins the open circuits with the live incoming
// commitment HTLCs into the in-flight HTLCs the reputation manager replays on
// startup. Only forwards are included: the incoming side must not be this
// node's own switch, and the circuit must have been opened on an outgoing
// link. A circuit whose incoming HTLC is no longer live on the commitment is
// skipped, since its cltv and accountable signal cannot be recovered.
//
// The circuit map does not retain the fee the node charged for the forward,
// only the amounts, so the replayed fee is the fee the sender offered. That
// is at least the fee the node advertised, since the forward was accepted,
// and it only affects HTLCs that spanned a restart.
func assembleInFlightHTLCs(circuits []*htlcswitch.PaymentCircuit,
	chans []*chanstate.OpenChannel) []reputation.InFlightHTLC {

	index := indexIncomingHTLCs(chans)

	htlcs := make([]reputation.InFlightHTLC, 0, len(circuits))
	for _, c := range circuits {
		if c.Incoming.ChanID == hop.Source || c.Outgoing == nil {
			continue
		}

		info, ok := index[c.Incoming]
		if !ok {
			srvrLog.Debugf("Skipping in-flight htlc %v for "+
				"reputation replay: incoming htlc no longer "+
				"live", c.Incoming)

			continue
		}

		var fee lnwire.MilliSatoshi
		if c.IncomingAmount > c.OutgoingAmount {
			fee = c.IncomingAmount - c.OutgoingAmount
		}

		htlcs = append(htlcs, reputation.InFlightHTLC{
			Incoming:     c.Incoming,
			Outgoing:     c.Outgoing.ChanID,
			Fee:          fee,
			IncomingCltv: info.cltv,
			Accountable:  info.accountable,
		})
	}

	return htlcs
}

// reconstructInFlightHTLCs assembles the forwarded HTLCs that are in flight
// from the switch's open circuits and the live channel commitments.
func reconstructInFlightHTLCs(circuits circuitSource,
	chans openChannelSource) ([]reputation.InFlightHTLC, error) {

	openChans, err := chans.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	return assembleInFlightHTLCs(circuits.ActiveCircuits(), openChans), nil
}

// forwardChannelClosesToReputation removes closed channels from the reputation
// manager until the server shuts down.
func (s *server) forwardChannelClosesToReputation() {
	defer s.wg.Done()

	client, err := s.channelNotifier.SubscribeChannelEvents()
	if err != nil {
		srvrLog.Errorf("Unable to subscribe reputation manager to "+
			"channel events: %v", err)

		return
	}
	defer client.Cancel()

	for {
		select {
		case event := <-client.Updates():
			closed, ok := event.(channelnotifier.ClosedChannelEvent)
			if !ok {
				continue
			}

			scid := closed.CloseSummary.ShortChanID.ToUint64()
			err := s.reputationMgr.RemoveChannel(scid)
			if err != nil {
				srvrLog.Warnf("Unable to remove channel %d "+
					"from reputation manager: %v",
					scid, err)
			}

		case <-s.quit:
			return
		}
	}
}

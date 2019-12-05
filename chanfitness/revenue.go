package chanfitness

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwire"
)

// RevenueReport provides a pairwise report on channel revenue. It maps a
// target channel to a map of channels that it has forwarded HTLCs with to
// a record of the revenue produced by each channel in the map. These revenue
// records report revenue and volume with direction relative to the target
// channel.
type RevenueReport struct {
	ChannelPairs map[wire.OutPoint]map[wire.OutPoint]Revenue
}

// Revenue describes the volume of forwards that a channel has been a part of
// and fees that a channel has generated as a result forward events. Volume
// and fee are reported by direction, where incoming means that the forward
// arrived at our node via the channel, and outgoing meaning that the forward
// left our node via the channel.
type Revenue struct {
	// AmountOutgoing is the amount in msat that was sent out over the channel
	// as part of forwards with its peer channel.
	AmountOutgoing lnwire.MilliSatoshi

	// AmountIncoming is the amount in msat that arrived on the channel to be
	// forwarded onwards by the peer channel.
	AmountIncoming lnwire.MilliSatoshi

	// FeesOutgoing is the amount in msat of fees that we attribute to the
	// channel for its role as the outgoing channel channel in forwards.
	FeesOutgoing lnwire.MilliSatoshi

	// FeesIncoming is the amount in msat of fees that we attribute to the
	// channel for its role as the incoming channel in forwards.
	FeesIncoming lnwire.MilliSatoshi
}

// getRevenue gets a revenue record for a given target channel and its
// forwarding pair. If map entries do not exist at any stage, they are created.
func (r RevenueReport) getRevenue(targetChan,
	pairChan wire.OutPoint) Revenue {

	// If we do not have an entry in our revenue report for the target channel
	// create one.
	record, ok := r.ChannelPairs[targetChan]
	if !ok {
		record = make(map[wire.OutPoint]Revenue)
		r.ChannelPairs[targetChan] = record
	}

	// Get the revenue we have with the pair channel channel, if there is no
	// revenue record, return an empty one.
	revenue, ok := record[pairChan]
	if !ok {
		return Revenue{}
	}

	return revenue
}

// setRevenue sets the revenue value for a target channel and its forwarding
// pair. This function expects both maps to be initialized.
func (r RevenueReport) setRevenue(targetChan, pairChan wire.OutPoint,
	revenue Revenue) {

	r.ChannelPairs[targetChan][pairChan] = revenue
}

// addIncoming gets the existing revenue record for the incoming channel with
// the outgoing channel, adds the incoming volume and fees and updates the
// Revenue Report.
func (r RevenueReport) addIncoming(incomingChannel,
	outgoingChannel wire.OutPoint, amount, fees lnwire.MilliSatoshi) {
	revenue := r.getRevenue(incomingChannel, outgoingChannel)

	// Add the fees and revenue that have been earned to the existing revenue
	// record.
	revenue.AmountIncoming += amount
	revenue.FeesIncoming += fees

	log.Trace("Incoming channel: %v recording incoming volume: %v and fee: %v"+
		" with outgoing channel: %v", incomingChannel, amount, fees, outgoingChannel)

	// Set the new revenue record in the revenue report.
	r.setRevenue(incomingChannel, outgoingChannel, revenue)
}

// addOutgoing gets the existing revenue record for the outgoing channel with
// the incoming channel, adds the outgoing volume and fees and updates the
// Revenue Report.
func (r RevenueReport) addOutgoing(outgoingChannel,
	incomingChannel wire.OutPoint, amount, fees lnwire.MilliSatoshi) {
	revenue := r.getRevenue(outgoingChannel, incomingChannel)

	// Add the fees and revenue that have been earned to the existing revenue
	// record.
	revenue.AmountOutgoing += amount
	revenue.FeesOutgoing += fees

	log.Trace("Outgoing channel: %v recording outgoing volume: %v and fee: %v"+
		" with incoming channel: %v", outgoingChannel, amount, fees, incomingChannel)

	// Set the new revenue record in the revenue report.
	r.setRevenue(outgoingChannel, incomingChannel, revenue)
}

// revenueEvent provides the information captured by ForwardingEvents with
// channel outpoints rather than short channel ids.
type revenueEvent struct {
	incomingChannel wire.OutPoint
	outgoingChannel wire.OutPoint
	incomingAmt     lnwire.MilliSatoshi
	outgoingAmt     lnwire.MilliSatoshi
}

// revenueReport creates a revenue report for the set of events provided. It
// takes an attribute incoming float which determines the fee split between
// incoming and outgoing channels.
func revenueReport(events []revenueEvent, attributeIncoming float64) *RevenueReport {
	record := &RevenueReport{
		ChannelPairs: make(map[wire.OutPoint]map[wire.OutPoint]Revenue),
	}

	for _, event := range events {
		// Calculate total fees earned for this event.
		fee := event.incomingAmt - event.outgoingAmt

		// Calculate fees earned by the incoming channel in this event.
		incomingFees := lnwire.MilliSatoshi(float64(fee) * attributeIncoming)

		// Update the revenue record for the incoming channel.
		record.addIncoming(event.incomingChannel, event.outgoingChannel,
			event.incomingAmt, incomingFees)

		// Calculate the revenue record for the outgoing channel.
		outgoingFees := lnwire.MilliSatoshi(float64(fee) *
			(1 - attributeIncoming))

		// Update the revenue record for the downstream channel.
		record.addOutgoing(event.outgoingChannel, event.incomingChannel,
			event.outgoingAmt, outgoingFees)
	}

	return record
}

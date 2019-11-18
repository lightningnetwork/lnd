package chanfitness

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// errZero end time is returned when a query over a range does not have an
	// end time.
	errZeroEndTime = fmt.Errorf("zero end time")

	// errEndBeforeStart is returned when a query over a range has an end time
	// which is before the start time.
	errEndBeforeStart = fmt.Errorf("end time before start time")
)

type eventType int

const (
	peerOnlineEvent eventType = iota
	peerOfflineEvent
)

// String provides string representations of channel events.
func (e eventType) String() string {
	switch e {
	case peerOnlineEvent:
		return "peer_online"

	case peerOfflineEvent:
		return "peer_offline"
	}

	return "unknown"
}

// channelEvent is a a timestamped event which is observed on a per channel
// basis.
type channelEvent struct {
	timestamp time.Time
	eventType eventType
}

// chanEventLog stores all events that have occurred over a channel's lifetime.
type chanEventLog struct {
	// id is the uint64 of the short channel ID.
	id uint64

	// peer is the compressed public key of the peer being monitored.
	peer route.Vertex

	// events is a log of timestamped events observed for the channel.
	events []*channelEvent

	// ChannelTimestamps contains the opened, closed and monitored since times
	// for the channel. It also contains a now function which is used for time
	// mocking in unit tests.
	ChannelTimestamps
}

// ChannelTimestamps provides open and close timestamps for a channel, as well
// as a timestamp which indicates when monitoring of the channel began. A
// MonitoredUntil variable which indicates when monitoring ended is not given
// because monitoring ends when the channel is closed.
type ChannelTimestamps struct {
	// Opened at is the time that the channel was opened on chain. Note that
	// this value is set from block height when channels exist at startup.
	// Block timestamps can be up to two hours in the future, so this value
	// should not be relied upon for any strict calculations, see
	// https://en.bitcoin.it/wiki/Block_timestamp for details.
	OpenedAt time.Time

	// Closed at is the time that the channel was closed on chain. If it is
	// still open, this value is zero.
	ClosedAt time.Time

	// monitoredSince tracks the first time this channel was seen. This is not
	// necessarily the time that it confirmed on chain because channel events
	// are not persisted at present.
	MonitoredSince time.Time

	// now is expected to return the current time. It is supplied as an
	// external function to enable deterministic unit tests.
	now func() time.Time
}

// Lifetime returns the amount of time that the channel was open for. If the
// channel is not closed yet, the value is calculated until the present.
func (c ChannelTimestamps) Lifetime() time.Duration {
	endTime := c.ClosedAt
	if endTime.IsZero() {
		endTime = c.now()
	}

	return endTime.Sub(c.OpenedAt)
}

// Monitored returns the amount of time that the channel has been monitored.
// If the channel is closed, this value is calculated until close time, and if
// it is still opened, it is calculated until the present.
func (c ChannelTimestamps) Monitored() time.Duration {
	endTime := c.ClosedAt
	if endTime.IsZero() {
		endTime = c.now()
	}

	return endTime.Sub(c.MonitoredSince)
}

// newEventLog returns a channel event log for a channel id, peer vertex and
// channel open time. It also takes a now function which allows for easier
// time mocking during unit testing. This function is used to set the monitored
// since field, which indicates the point at which monitoring of the channel
// began.
func newEventLog(id uint64, peer route.Vertex, openedAt time.Time,
	now func() time.Time) *chanEventLog {

	return &chanEventLog{
		id:   id,
		peer: peer,
		ChannelTimestamps: ChannelTimestamps{
			OpenedAt:       openedAt,
			MonitoredSince: now(),
			now:            now,
		},
	}
}

// close sets the closing time for an event log.
func (e *chanEventLog) close() {
	e.ClosedAt = e.now()
}

// add appends an event with the given type and current time to the event log.
// The open time for the eventLog will be set to the event's timestamp if it is
// not set yet.
func (e *chanEventLog) add(eventType eventType) {
	// If the channel is already closed, return early without adding an event.
	if !e.ClosedAt.IsZero() {
		return
	}

	// Add the event to the eventLog with the current timestamp.
	event := &channelEvent{
		timestamp: e.now(),
		eventType: eventType,
	}
	e.events = append(e.events, event)

	log.Debugf("Channel %v recording event: %v", e.id, eventType)
}

// onlinePeriod represents a period of time over which a peer was online.
type onlinePeriod struct {
	start, end time.Time
}

// getOnlinePeriods returns a list of all the periods that the event log has
// recorded the remote peer as being online. In the unexpected case where there
// are no events, the function returns early. Online periods are defined as a
// peer online event which is terminated by a peer offline event. This function
// expects the event log provided to be ordered by ascending timestamp.
func (e *chanEventLog) getOnlinePeriods() []*onlinePeriod {
	// Return early if there are no events, there are no online periods.
	if len(e.events) == 0 {
		return nil
	}

	var (
		lastOnline    time.Time
		offline       bool
		onlinePeriods []*onlinePeriod
	)

	// Loop through all events to build a list of periods that the peer was
	// online. Online periods are added when they are terminated with a peer
	// offline event. If the log ends on an online event, the period between
	// the online event and the present is not tracked. The type of the most
	// recent event is tracked using the offline bool so that we can add a
	// final online period if necessary.
	for _, event := range e.events {

		switch event.eventType {
		case peerOnlineEvent:
			lastOnline = event.timestamp
			offline = false

		case peerOfflineEvent:
			offline = true

			// Do not add to uptime if there is no previous online timestamp,
			// the event log has started with an offline event
			if lastOnline.IsZero() {
				continue
			}

			// The eventLog has recorded an offline event, having previously
			// been online so we add an online period to to set of online periods.
			onlinePeriods = append(onlinePeriods, &onlinePeriod{
				start: lastOnline,
				end:   event.timestamp,
			})
		}
	}

	// If the last event was an peer offline event, we do not need to calculate
	// a final online period and can return online periods as is.
	if offline {
		return onlinePeriods
	}

	// The log ended on an online event, so we need to add a final online event.
	// If the channel is closed, this period is until channel closure. It it is
	// still open, we calculate it until the present.
	endTime := e.ClosedAt
	if endTime.IsZero() {
		endTime = e.now()
	}

	// Add the final online period to the set and return.
	return append(onlinePeriods, &onlinePeriod{
		start: lastOnline,
		end:   endTime,
	})
}

// uptime calculates the total uptime we have recorded for a channel over the
// inclusive range specified. An error is returned if the end of the range is
// before the start or a zero end time is returned.
func (e *chanEventLog) uptime(start, end time.Time) (time.Duration, error) {
	// Error if we are provided with an invalid range to calculate uptime for.
	if err := validateRange(start, end); err != nil {
		return 0, err
	}

	var uptime time.Duration

	for _, p := range e.getOnlinePeriods() {
		// The online period ends before the range we're looking at, so we can
		// skip over it.
		if p.end.Before(start) {
			continue
		}
		// The online period starts after the range we're looking at, so can
		// stop calculating uptime.
		if p.start.After(end) {
			break
		}

		// If the online period starts before our range, shift the start time up
		// so that we only calculate uptime from the start of our range.
		if p.start.Before(start) {
			p.start = start
		}

		// If the online period ends before our range, shift the end time
		// forward so that we only calculate uptime until the end of the range.
		if p.end.After(end) {
			p.end = end
		}

		uptime += p.end.Sub(p.start)
	}

	return uptime, nil
}

type eventsFunc func(startTime, endTime time.Time) ([]channeldb.ForwardingEvent, error)

func (e *chanEventLog) revenue(startTime, endTime time.Time, events eventsFunc,
	attributeIncoming float64) (lnwire.MilliSatoshi, error) {

	log.Debugf("Calculating revenue for channel: %v from %v to %v", e.id,
		startTime, endTime)

	// Error if the range provided is invalid.
	if err := validateRange(startTime, endTime); err != nil {
		return 0, err
	}

	// If a start time is not specified, use the open time of the channel,
	// adjusted by the block time offset.
	if startTime.IsZero() {
		startTime = e.OpenedAt
	}

	eventList, err := events(startTime, endTime)
	if err != nil {
		return 0, err
	}

	// Revenue is the total revenue earned by this channel in millisatoshis.
	// This value is accumulated as a float then converted to millisatoshis so
	// that we can minimize the amount of rounding from float -> int.
	var revenue float64

	for _, event := range eventList {
		// Calculate fees earned for this event.
		fee := event.AmtIn - event.AmtOut

		// If our target channel was the incoming channel for this event,
		// attribute the incomingAttribute percentage of the fees to it.
		if event.IncomingChanID.ToUint64() == e.id {
			earned := float64(fee) * attributeIncoming

			log.Debugf("Channel: %v is outgoing channel, incrementing "+
				"revenue by: %v msat", e.id, earned)

			revenue += earned
		}

		// If our target channel was the outgoing channel for this event,
		// attribute (1 - percentage for incoming) percent of the fees to it.
		if event.OutgoingChanID.ToUint64() == e.id {
			earned := float64(fee) * (1 - attributeIncoming)

			log.Debugf("Channel: %v is outgoing channel, incrementing "+
				"revenue by: %v msat", e.id, earned)

			revenue += earned
		}
	}

	return lnwire.MilliSatoshi(uint64(revenue)), nil
}

// validateRange returns an error if the endtime provided is zero or before the
// start time provided.
func validateRange(startTime, endTime time.Time) error {
	if endTime.IsZero() {
		return errZeroEndTime
	}

	if endTime.Before(startTime) {
		return errEndBeforeStart
	}

	return nil
}

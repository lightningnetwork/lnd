package chanfitness

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/routing/route"
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
	// channelPoint is the outpoint for the channel's funding transaction.
	channelPoint wire.OutPoint

	// peer is the compressed public key of the peer being monitored.
	peer route.Vertex

	// events is a log of timestamped events observed for the channel.
	events []*channelEvent

	// now is expected to return the current time. It is supplied as an
	// external function to enable deterministic unit tests.
	now func() time.Time

	// openedAt tracks the first time this channel was seen. This is not
	// necessarily the time that it confirmed on chain because channel events
	// are not persisted at present.
	openedAt time.Time

	// closedAt is the time that the channel was closed. If the channel has not
	// been closed yet, it is zero.
	closedAt time.Time
}

// newEventLog creates an event log for a channel with the openedAt time set.
func newEventLog(channelPoint wire.OutPoint, peer route.Vertex,
	now func() time.Time) *chanEventLog {

	eventlog := &chanEventLog{
		channelPoint: channelPoint,
		peer:         peer,
		now:          now,
		openedAt:     now(),
	}

	return eventlog
}

// close sets the closing time for an event log.
func (e *chanEventLog) close() {
	e.closedAt = e.now()
}

// add appends an event with the given type and current time to the event log.
// The open time for the eventLog will be set to the event's timestamp if it is
// not set yet.
func (e *chanEventLog) add(eventType eventType) {
	// If the channel is already closed, return early without adding an event.
	if !e.closedAt.IsZero() {
		return
	}

	// Add the event to the eventLog with the current timestamp.
	event := &channelEvent{
		timestamp: e.now(),
		eventType: eventType,
	}
	e.events = append(e.events, event)

	log.Debugf("Channel %v recording event: %v", e.channelPoint, eventType)
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
	endTime := e.closedAt
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
	if end.Before(start) {
		return 0, fmt.Errorf("end time: %v before start time: %v",
			end, start)
	}
	if end.IsZero() {
		return 0, fmt.Errorf("zero end time")
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

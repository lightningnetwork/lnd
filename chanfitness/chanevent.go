package chanfitness

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/clock"
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

type event struct {
	timestamp time.Time
	eventType eventType
}

// peerLog tracks events for a peer and its channels. If we currently have no
// channels with the peer, it will simply track its current online state. If we
// do have channels open with the peer, it will track the peer's online and
// offline events so that we can calculate uptime for our channels. A single
// event log is used for these online and offline events, and uptime for a
// channel is calculated by examining a subsection of this log.
type peerLog struct {
	// online stores whether the peer is currently online.
	online bool

	// onlineEvents is a log of timestamped events observed for the peer
	// that we have committed to allocating memory to.
	onlineEvents []*event

	// stagedEvent represents an event that is pending addition to the
	// events list. It has not yet been added because we rate limit the
	// frequency that we store events at. We need to store this value
	// in the log (rather than just ignore events) so that we can flush the
	// aggregate outcome to our event log once the rate limiting period has
	// ended.
	//
	// Take the following example:
	// - Peer online event recorded
	// - Peer offline event, not recorded due to rate limit
	// - No more events, we incorrectly believe our peer to be online
	// Instead of skipping events, we stage the most recent event during the
	// rate limited period so that we know what happened (on aggregate)
	// while we were rate limiting events.
	//
	// Note that we currently only store offline/online events so we can
	// use this field to track our online state. With the addition of other
	// event types, we need to only stage online/offline events, or split
	// them out.
	stagedEvent *event

	// flapCount is the number of times this peer has been observed as
	// going offline.
	flapCount int

	// lastFlap is the timestamp of the last flap we recorded for the peer.
	// This value will be nil if we have never recorded a flap for the peer.
	lastFlap *time.Time

	// clock allows creation of deterministic unit tests.
	clock clock.Clock

	// channels contains a set of currently open channels. Channels will be
	// added and removed from this map as they are opened and closed.
	channels map[wire.OutPoint]*channelInfo
}

// newPeerLog creates a log for a peer, taking its historical flap count and
// last flap time as parameters. These values may be zero/nil if we have no
// record of historical flap count for the peer.
func newPeerLog(clock clock.Clock, flapCount int,
	lastFlap *time.Time) *peerLog {

	return &peerLog{
		clock:     clock,
		flapCount: flapCount,
		lastFlap:  lastFlap,
		channels:  make(map[wire.OutPoint]*channelInfo),
	}
}

// channelInfo contains information about a channel.
type channelInfo struct {
	// openedAt tracks the first time this channel was seen. This is not
	// necessarily the time that it confirmed on chain because channel
	// events are not persisted at present.
	openedAt time.Time
}

func newChannelInfo(openedAt time.Time) *channelInfo {
	return &channelInfo{
		openedAt: openedAt,
	}
}

// onlineEvent records a peer online or offline event in the log and increments
// the peer's flap count.
func (p *peerLog) onlineEvent(online bool) {
	eventTime := p.clock.Now()

	// If we have a non-nil last flap time, potentially apply a cooldown
	// factor to the peer's flap count before we rate limit it. This allows
	// us to decrease the penalty for historical flaps over time, provided
	// the peer has not flapped for a while.
	if p.lastFlap != nil {
		p.flapCount = cooldownFlapCount(
			p.clock.Now(), p.flapCount, *p.lastFlap,
		)
	}

	// Record flap count information and online state regardless of whether
	// we have any channels open with this peer.
	p.flapCount++
	p.lastFlap = &eventTime
	p.online = online

	// If we have no channels currently open with the peer, we do not want
	// to commit resources to tracking their online state beyond a simple
	// online boolean, so we exit early.
	if p.channelCount() == 0 {
		return
	}

	p.addEvent(online, eventTime)
}

// addEvent records an online or offline event in our event log. and increments
// the peer's flap count.
func (p *peerLog) addEvent(online bool, time time.Time) {
	eventType := peerOnlineEvent
	if !online {
		eventType = peerOfflineEvent
	}

	event := &event{
		timestamp: time,
		eventType: eventType,
	}

	// If we have no staged events, we can just stage this event and return.
	if p.stagedEvent == nil {
		p.stagedEvent = event
		return
	}

	// We get the amount of time we require between events according to
	// peer flap count.
	aggregation := getRateLimit(p.flapCount)
	nextRecordTime := p.stagedEvent.timestamp.Add(aggregation)
	flushEvent := nextRecordTime.Before(event.timestamp)

	// If enough time has passed since our last staged event, we add our
	// event to our in-memory list.
	if flushEvent {
		p.onlineEvents = append(p.onlineEvents, p.stagedEvent)
	}

	// Finally, we replace our staged event with the new event we received.
	p.stagedEvent = event
}

// addChannel adds a channel to our log. If we have not tracked any online
// events for our peer yet, we create one with our peer's current online state
// so that we know the state that the peer had at channel start, which is
// required to calculate uptime over the channel's lifetime.
func (p *peerLog) addChannel(channelPoint wire.OutPoint) error {
	_, ok := p.channels[channelPoint]
	if ok {
		return fmt.Errorf("channel: %v already present", channelPoint)
	}

	openTime := p.clock.Now()
	p.channels[channelPoint] = newChannelInfo(openTime)

	// If we do not have any online events tracked for our peer (which is
	// the case when we have no other channels open with the peer), we add
	// an event with the peer's current online state so that we know that
	// starting state for this peer when a channel was connected (which
	// allows us to calculate uptime over the lifetime of the channel).
	if len(p.onlineEvents) == 0 {
		p.addEvent(p.online, openTime)
	}

	return nil
}

// removeChannel removes a channel from our log. If we have no more channels
// with the peer after removing this one, we clear our list of events.
func (p *peerLog) removeChannel(channelPoint wire.OutPoint) error {
	_, ok := p.channels[channelPoint]
	if !ok {
		return fmt.Errorf("channel: %v not present", channelPoint)
	}

	delete(p.channels, channelPoint)

	// If we have no more channels in our event log, we can discard all of
	// our online events in memory, since we don't need them anymore.
	// TODO(carla): this could be done on a per channel basis.
	if p.channelCount() == 0 {
		p.onlineEvents = nil
		p.stagedEvent = nil
	}

	return nil
}

// channelCount returns the number of channels that we currently have
// with the peer.
func (p *peerLog) channelCount() int {
	return len(p.channels)
}

// channelUptime looks up a channel and returns the amount of time that the
// channel has been monitored for and its uptime over this period.
func (p *peerLog) channelUptime(channelPoint wire.OutPoint) (time.Duration,
	time.Duration, error) {

	channel, ok := p.channels[channelPoint]
	if !ok {
		return 0, 0, ErrChannelNotFound
	}

	now := p.clock.Now()

	uptime, err := p.uptime(channel.openedAt, now)
	if err != nil {
		return 0, 0, err
	}

	return now.Sub(channel.openedAt), uptime, nil
}

// getFlapCount returns the peer's flap count and the timestamp that we last
// recorded a flap.
func (p *peerLog) getFlapCount() (int, *time.Time) {
	return p.flapCount, p.lastFlap
}

// listEvents returns all of the events that our event log has tracked,
// including events that are staged for addition to our set of events but have
// not yet been committed to (because we rate limit and store only the aggregate
// outcome over a period).
func (p *peerLog) listEvents() []*event {
	if p.stagedEvent == nil {
		return p.onlineEvents
	}

	return append(p.onlineEvents, p.stagedEvent)
}

// onlinePeriod represents a period of time over which a peer was online.
type onlinePeriod struct {
	start, end time.Time
}

// getOnlinePeriods returns a list of all the periods that the event log has
// recorded the remote peer as being online. In the unexpected case where there
// are no events, the function returns early. Online periods are defined as a
// peer online event which is terminated by a peer offline event. If the event
// log ends on a peer online event, it appends a final period which is
// calculated until the present. This function expects the event log provided
// to be ordered by ascending timestamp, and can tolerate multiple consecutive
// online or offline events.
func (p *peerLog) getOnlinePeriods() []*onlinePeriod {
	events := p.listEvents()

	// Return early if there are no events, there are no online periods.
	if len(events) == 0 {
		return nil
	}

	var (
		// lastEvent tracks the last event that we had that was of
		// a different type to our own. It is used to determine the
		// start time of our online periods when we experience an
		// offline event, and to track our last recorded state.
		lastEvent     *event
		onlinePeriods []*onlinePeriod
	)

	// Loop through all events to build a list of periods that the peer was
	// online. Online periods are added when they are terminated with a peer
	// offline event. If the log ends on an online event, the period between
	// the online event and the present is not tracked. The type of the most
	// recent event is tracked using the offline bool so that we can add a
	// final online period if necessary.
	for _, event := range events {
		switch event.eventType {
		case peerOnlineEvent:
			// If our previous event is nil, we just set it and
			// break out of the switch.
			if lastEvent == nil {
				lastEvent = event
				break
			}

			// If our previous event was an offline event, we update
			// it to this event. We do not do this if it was an
			// online event because duplicate online events would
			// progress our online timestamp forward (rather than
			// keep it at our earliest online event timestamp).
			if lastEvent.eventType == peerOfflineEvent {
				lastEvent = event
			}

		case peerOfflineEvent:
			// If our previous event is nil, we just set it and
			// break out of the switch since we cannot record an
			// online period from this single event.
			if lastEvent == nil {
				lastEvent = event
				break
			}

			// If the last event we saw was an online event, we
			// add an online period to our set and progress our
			// previous event to this offline event. We do not
			// do this if we have had duplicate offline events
			// because we would be tracking the most recent offline
			// event (rather than keep it at our earliest offline
			// event timestamp).
			if lastEvent.eventType == peerOnlineEvent {
				onlinePeriods = append(
					onlinePeriods, &onlinePeriod{
						start: lastEvent.timestamp,
						end:   event.timestamp,
					},
				)

				lastEvent = event
			}
		}
	}

	// If the last event was an peer offline event, we do not need to
	// calculate a final online period and can return online periods as is.
	if lastEvent.eventType == peerOfflineEvent {
		return onlinePeriods
	}

	// The log ended on an online event, so we need to add a final online
	// period which terminates at the present.
	finalEvent := &onlinePeriod{
		start: lastEvent.timestamp,
		end:   p.clock.Now(),
	}

	// Add the final online period to the set and return.
	return append(onlinePeriods, finalEvent)
}

// uptime calculates the total uptime we have recorded for a peer over the
// inclusive range specified. An error is returned if the end of the range is
// before the start or a zero end time is returned.
func (p *peerLog) uptime(start, end time.Time) (time.Duration, error) {
	// Error if we are provided with an invalid range to calculate uptime
	// for.
	if end.Before(start) {
		return 0, fmt.Errorf("end time: %v before start time: %v",
			end, start)
	}
	if end.IsZero() {
		return 0, fmt.Errorf("zero end time")
	}

	var uptime time.Duration

	for _, p := range p.getOnlinePeriods() {
		// The online period ends before the range we're looking at, so
		// we can skip over it.
		if p.end.Before(start) {
			continue
		}
		// The online period starts after the range we're looking at, so
		// can stop calculating uptime.
		if p.start.After(end) {
			break
		}

		// If the online period starts before our range, shift the start
		// time up so that we only calculate uptime from the start of
		// our range.
		if p.start.Before(start) {
			p.start = start
		}

		// If the online period ends after our range, shift the end
		// time forward so that we only calculate uptime until the end
		// of the range.
		if p.end.After(end) {
			p.end = end
		}

		uptime += p.end.Sub(p.start)
	}

	return uptime, nil
}

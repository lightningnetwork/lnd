// Package chanfitness monitors the behaviour of channels to provide insight
// into the health and performance of a channel. This is achieved by maintaining
// an event store which tracks events for each channel.
//
// Lifespan: the period that the channel has been known to the scoring system.
// Note that lifespan may not equal the channel's full lifetime because data is
// not currently persisted.
//
// Uptime: the total time within a given period that the channel's remote peer
// has been online.
package chanfitness

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/peernotifier"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/ticker"
)

const (
	// FlapCountFlushRate determines how often we write peer total flap
	// count to disk.
	FlapCountFlushRate = time.Hour
)

var (
	// errShuttingDown is returned when the store cannot respond to a query
	// because it has received the shutdown signal.
	errShuttingDown = errors.New("channel event store shutting down")

	// ErrChannelNotFound is returned when a query is made for a channel
	// that the event store does not have knowledge of.
	ErrChannelNotFound = errors.New("channel not found in event store")

	// ErrPeerNotFound is returned when a query is made for a channel
	// that has a peer that the event store is not currently tracking.
	ErrPeerNotFound = errors.New("peer not found in event store")
)

// ChannelEventStore maintains a set of event logs for the node's channels to
// provide insight into the performance and health of channels.
type ChannelEventStore struct {
	started atomic.Bool
	stopped atomic.Bool

	cfg *Config

	// peers tracks all of our currently monitored peers and their channels.
	peers map[route.Vertex]peerMonitor

	// chanInfoRequests serves requests for information about our channel.
	chanInfoRequests chan channelInfoRequest

	// peerRequests serves requests for information about a peer.
	peerRequests chan peerRequest

	quit chan struct{}

	wg sync.WaitGroup
}

// Config provides the event store with functions required to monitor channel
// activity. All elements of the config must be non-nil for the event store to
// operate.
type Config struct {
	// SubscribeChannelEvents provides a subscription client which provides
	// a stream of channel events.
	SubscribeChannelEvents func() (subscribe.Subscription, error)

	// SubscribePeerEvents provides a subscription client which provides a
	// stream of peer online/offline events.
	SubscribePeerEvents func() (subscribe.Subscription, error)

	// GetOpenChannels provides a list of existing open channels which is
	// used to populate the ChannelEventStore with a set of channels on
	// startup.
	GetOpenChannels func() ([]*channeldb.OpenChannel, error)

	// Clock is the time source that the subsystem uses, provided here
	// for ease of testing.
	Clock clock.Clock

	// WriteFlapCount records the flap count for a set of peers on disk.
	WriteFlapCount func(map[route.Vertex]*channeldb.FlapCount) error

	// ReadFlapCount gets the flap count for a peer on disk.
	ReadFlapCount func(route.Vertex) (*channeldb.FlapCount, error)

	// FlapCountTicker is a ticker which controls how often we flush our
	// peer's flap count to disk.
	FlapCountTicker ticker.Ticker
}

// peerFlapCountMap is the map used to map peers to flap counts, declared here
// to allow shorter function signatures.
type peerFlapCountMap map[route.Vertex]*channeldb.FlapCount

type channelInfoRequest struct {
	peer         route.Vertex
	channelPoint wire.OutPoint
	responseChan chan channelInfoResponse
}

type channelInfoResponse struct {
	info *ChannelInfo
	err  error
}

type peerRequest struct {
	peer         route.Vertex
	responseChan chan peerResponse
}

type peerResponse struct {
	flapCount int
	ts        *time.Time
	err       error
}

// NewChannelEventStore initializes an event store with the config provided.
// Note that this function does not start the main event loop, Start() must be
// called.
func NewChannelEventStore(config *Config) *ChannelEventStore {
	store := &ChannelEventStore{
		cfg:              config,
		peers:            make(map[route.Vertex]peerMonitor),
		chanInfoRequests: make(chan channelInfoRequest),
		peerRequests:     make(chan peerRequest),
		quit:             make(chan struct{}),
	}

	return store
}

// Start adds all existing open channels to the event store and starts the main
// loop which records channel and peer events, and serves requests for
// information from the store. If this function fails, it cancels its existing
// subscriptions and returns an error.
func (c *ChannelEventStore) Start() error {
	log.Info("ChannelEventStore starting...")

	if c.started.Swap(true) {
		return fmt.Errorf("ChannelEventStore started more than once")
	}

	// Create a subscription to channel events.
	channelClient, err := c.cfg.SubscribeChannelEvents()
	if err != nil {
		return err
	}

	// Create a subscription to peer events. If an error occurs, cancel the
	// existing subscription to channel events and return.
	peerClient, err := c.cfg.SubscribePeerEvents()
	if err != nil {
		channelClient.Cancel()
		return err
	}

	// cancel should be called to cancel all subscriptions if an error
	// occurs.
	cancel := func() {
		channelClient.Cancel()
		peerClient.Cancel()
	}

	// Add the existing set of channels to the event store. This is required
	// because channel events will not be triggered for channels that exist
	// at startup time.
	channels, err := c.cfg.GetOpenChannels()
	if err != nil {
		cancel()
		return err
	}

	log.Infof("Adding %v channels to event store", len(channels))

	for _, ch := range channels {
		peerKey, err := route.NewVertexFromBytes(
			ch.IdentityPub.SerializeCompressed(),
		)
		if err != nil {
			cancel()
			return err
		}

		// Add existing channels to the channel store with an initial
		// peer online or offline event.
		c.addChannel(ch.FundingOutpoint, peerKey)
	}

	// Start a goroutine that consumes events from all subscriptions.
	c.wg.Add(1)
	go c.consume(&subscriptions{
		channelUpdates: channelClient.Updates(),
		peerUpdates:    peerClient.Updates(),
		cancel:         cancel,
	})

	log.Debug("ChannelEventStore started")

	return nil
}

// Stop terminates all goroutines started by the event store.
func (c *ChannelEventStore) Stop() error {
	log.Info("ChannelEventStore shutting down...")

	if c.stopped.Swap(true) {
		return fmt.Errorf("ChannelEventStore stopped more than once")
	}

	// Stop the consume goroutine.
	close(c.quit)
	c.wg.Wait()

	// Stop the ticker after the goroutine reading from it has exited, to
	// avoid a race.
	var err error
	if c.cfg.FlapCountTicker == nil {
		err = fmt.Errorf("ChannelEventStore FlapCountTicker not " +
			"initialized")
	} else {
		c.cfg.FlapCountTicker.Stop()
	}

	log.Debugf("ChannelEventStore shutdown complete")

	return err
}

// addChannel checks whether we are already tracking a channel's peer, creates a
// new peer log to track it if we are not yet monitoring it, and adds the
// channel.
func (c *ChannelEventStore) addChannel(channelPoint wire.OutPoint,
	peer route.Vertex) {

	peerMonitor, err := c.getOrCreatePeerMonitor(peer)
	if err != nil {
		log.Error("could not create monitor: %v", err)
		return
	}

	if err := peerMonitor.addChannel(channelPoint); err != nil {
		log.Errorf("could not add channel: %v", err)
	}
}

// getOrCreatePeerMonitor tries to get an existing peer monitor from our in
// memory list. If the peer is not yet known to us, it will create a new
// monitor and add it to the list. When a new monitor is created, we also send
// an initial online event for the peer.
func (c *ChannelEventStore) getOrCreatePeerMonitor(
	peer route.Vertex) (peerMonitor, error) {

	peerMonitor, ok := c.peers[peer]
	if ok {
		return peerMonitor, nil
	}

	var (
		flapCount int
		lastFlap  *time.Time
	)

	historicalFlap, err := c.cfg.ReadFlapCount(peer)
	switch err {
	// If we do not have any records for this peer we set a 0 flap count
	// and timestamp.
	case channeldb.ErrNoPeerBucket:

	case nil:
		flapCount = int(historicalFlap.Count)
		lastFlap = &historicalFlap.LastFlap

	// Return if we get an unexpected error.
	default:
		return nil, err
	}

	peerMonitor = newPeerLog(c.cfg.Clock, flapCount, lastFlap)
	c.peers[peer] = peerMonitor

	// Send an online event given it's the first time we see this peer.
	peerMonitor.onlineEvent(true)

	return peerMonitor, nil
}

// closeChannel records a closed time for a channel, and returns early is the
// channel is not known to the event store. We log warnings (rather than errors)
// when we cannot find a peer/channel because channels that we restore from a
// static channel backup do not have their open notified, so the event store
// never learns about them, but they are closed using the regular flow so we
// will try to remove them on close. At present, we cannot easily distinguish
// between these closes and others.
func (c *ChannelEventStore) closeChannel(channelPoint wire.OutPoint,
	peer route.Vertex) {

	peerMonitor, ok := c.peers[peer]
	if !ok {
		log.Warnf("peer not known to store: %v", peer)
		return
	}

	if err := peerMonitor.removeChannel(channelPoint); err != nil {
		log.Warnf("could not remove channel: %v", err)
	}
}

// peerEvent adds an online event to a peer's monitor. If the peer is not
// yet known to the event store, the event is ignored. A peer is only known to
// the event store if we have an open channel with them.
func (c *ChannelEventStore) peerEvent(peer route.Vertex, online bool) {
	peerMonitor, ok := c.peers[peer]
	if !ok {
		log.Tracef("Ignore peer event (online=%v) from non-channel "+
			"peer: %v", online, peer)

		return
	}

	peerMonitor.onlineEvent(online)
}

// subscriptions abstracts away from subscription clients to allow for mocking.
type subscriptions struct {
	channelUpdates <-chan interface{}
	peerUpdates    <-chan interface{}
	cancel         func()
}

// consume is the event store's main loop. It consumes subscriptions to update
// the event store with channel and peer events, and serves requests for channel
// uptime and lifespan.
func (c *ChannelEventStore) consume(subscriptions *subscriptions) {
	// Start our flap count ticker.
	c.cfg.FlapCountTicker.Resume()

	// On exit, we will cancel our subscriptions and write our most recent
	// flap counts to disk. This ensures that we have consistent data in
	// the case of a graceful shutdown. If we do not shutdown gracefully,
	// our worst case is data from our last flap count tick (1H).
	defer func() {
		subscriptions.cancel()

		if err := c.recordFlapCount(); err != nil {
			log.Errorf("error recording flap on shutdown: %v", err)
		}

		c.wg.Done()
	}()

	// Consume events until the channel is closed.
	for {
		select {
		// Process channel opened and closed events.
		case e := <-subscriptions.channelUpdates:
			switch event := e.(type) {
			// A new channel has been opened, we must add the
			// channel to the store and record a channel open event.
			case channelnotifier.OpenChannelEvent:
				compressed := event.Channel.IdentityPub.SerializeCompressed()
				peerKey, err := route.NewVertexFromBytes(
					compressed,
				)
				if err != nil {
					log.Errorf("Could not get vertex "+
						"from: %v", compressed)
				}

				op := event.Channel.FundingOutpoint

				log.Tracef("Received OpenChannelEvent(%v) "+
					"from %v", op, peerKey)

				c.addChannel(op, peerKey)

			// A channel has been closed, we must remove the channel
			// from the store and record a channel closed event.
			case channelnotifier.ClosedChannelEvent:
				compressed := event.CloseSummary.RemotePub.SerializeCompressed()
				peerKey, err := route.NewVertexFromBytes(
					compressed,
				)
				if err != nil {
					log.Errorf("Could not get vertex "+
						"from: %v", compressed)
					continue
				}

				c.closeChannel(
					event.CloseSummary.ChanPoint, peerKey,
				)
			}

		// Process peer online and offline events.
		case e := <-subscriptions.peerUpdates:
			switch event := e.(type) {
			// We have reestablished a connection with our peer,
			// and should record an online event for any channels
			// with that peer.
			case peernotifier.PeerOnlineEvent:
				c.peerEvent(event.PubKey, true)

			// We have lost a connection with our peer, and should
			// record an offline event for any channels with that
			// peer.
			case peernotifier.PeerOfflineEvent:
				c.peerEvent(event.PubKey, false)
			}

		// Serve all requests for channel lifetime.
		case req := <-c.chanInfoRequests:
			var resp channelInfoResponse

			resp.info, resp.err = c.getChanInfo(req)
			req.responseChan <- resp

		// Serve all requests for information about our peer.
		case req := <-c.peerRequests:
			var resp peerResponse

			resp.flapCount, resp.ts, resp.err = c.flapCount(
				req.peer,
			)
			req.responseChan <- resp

		case <-c.cfg.FlapCountTicker.Ticks():
			if err := c.recordFlapCount(); err != nil {
				log.Errorf("could not record flap "+
					"count: %v", err)
			}

		// Exit if the store receives the signal to shutdown.
		case <-c.quit:
			return
		}
	}
}

// ChannelInfo provides the set of information that the event store has recorded
// for a channel.
type ChannelInfo struct {
	// Lifetime is the total amount of time we have monitored the channel
	// for.
	Lifetime time.Duration

	// Uptime is the total amount of time that the channel peer has been
	// observed as online during the monitored lifespan.
	Uptime time.Duration
}

// GetChanInfo gets all the information we have on a channel in the event store.
func (c *ChannelEventStore) GetChanInfo(channelPoint wire.OutPoint,
	peer route.Vertex) (*ChannelInfo, error) {

	request := channelInfoRequest{
		peer:         peer,
		channelPoint: channelPoint,
		responseChan: make(chan channelInfoResponse),
	}

	// Send a request for the channel's information to the main event loop,
	// or return early with an error if the store has already received a
	// shutdown signal.
	select {
	case c.chanInfoRequests <- request:
	case <-c.quit:
		return nil, errShuttingDown
	}

	// Return the response we receive on the response channel or exit early
	// if the store is instructed to exit.
	select {
	case resp := <-request.responseChan:
		return resp.info, resp.err

	case <-c.quit:
		return nil, errShuttingDown
	}
}

// getChanInfo collects channel information for a channel. It gets uptime over
// the full lifetime of the channel.
func (c *ChannelEventStore) getChanInfo(req channelInfoRequest) (*ChannelInfo,
	error) {

	peerMonitor, ok := c.peers[req.peer]
	if !ok {
		return nil, fmt.Errorf("%w: %v", ErrPeerNotFound, req.peer)
	}

	lifetime, uptime, err := peerMonitor.channelUptime(req.channelPoint)
	if err != nil {
		return nil, err
	}

	return &ChannelInfo{
		Lifetime: lifetime,
		Uptime:   uptime,
	}, nil
}

// FlapCount returns the flap count we have for a peer and the timestamp of its
// last flap. If we do not have any flaps recorded for the peer, the last flap
// timestamp will be nil.
func (c *ChannelEventStore) FlapCount(peer route.Vertex) (int, *time.Time,
	error) {

	request := peerRequest{
		peer:         peer,
		responseChan: make(chan peerResponse),
	}

	// Send a request for the peer's information to the main event loop,
	// or return early with an error if the store has already received a
	// shutdown signal.
	select {
	case c.peerRequests <- request:
	case <-c.quit:
		return 0, nil, errShuttingDown
	}

	// Return the response we receive on the response channel or exit early
	// if the store is instructed to exit.
	select {
	case resp := <-request.responseChan:
		return resp.flapCount, resp.ts, resp.err

	case <-c.quit:
		return 0, nil, errShuttingDown
	}
}

// flapCount gets our peer flap count and last flap timestamp from our in memory
// record of a peer, falling back to on disk if we are not currently tracking
// the peer. If we have no flap count recorded for the peer, a nil last flap
// time will be returned.
func (c *ChannelEventStore) flapCount(peer route.Vertex) (int, *time.Time,
	error) {

	// First check whether we are tracking this peer in memory, because this
	// record will have the most accurate flap count. We do not fail if we
	// can't find the peer in memory, because we may have previously
	// recorded its flap count on disk.
	peerMonitor, ok := c.peers[peer]
	if ok {
		count, ts := peerMonitor.getFlapCount()
		return count, ts, nil
	}

	// Try to get our flap count from the database. If this value is not
	// recorded, we return a nil last flap time to indicate that we have no
	// record of the peer's flap count.
	flapCount, err := c.cfg.ReadFlapCount(peer)
	switch err {
	case channeldb.ErrNoPeerBucket:
		return 0, nil, nil

	case nil:
		return int(flapCount.Count), &flapCount.LastFlap, nil

	default:
		return 0, nil, err
	}
}

// recordFlapCount will record our flap count for each peer that we are
// currently tracking, skipping peers that have a 0 flap count.
func (c *ChannelEventStore) recordFlapCount() error {
	updates := make(peerFlapCountMap)

	for peer, monitor := range c.peers {
		flapCount, lastFlap := monitor.getFlapCount()
		if lastFlap == nil {
			continue
		}

		updates[peer] = &channeldb.FlapCount{
			Count:    uint32(flapCount),
			LastFlap: *lastFlap,
		}
	}

	log.Debugf("recording flap count for: %v peers", len(updates))

	return c.cfg.WriteFlapCount(updates)
}

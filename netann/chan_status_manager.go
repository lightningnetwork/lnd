package netann

import (
	"errors"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrChanStatusManagerExiting signals that a shutdown of the
	// ChanStatusManager has already been requested.
	ErrChanStatusManagerExiting = errors.New("chan status manager exiting")

	// ErrInvalidTimeoutConstraints signals that the ChanStatusManager could
	// not be initialized because the timeouts and sample intervals were
	// malformed.
	ErrInvalidTimeoutConstraints = errors.New("active_timeout + " +
		"sample_interval must be less than or equal to " +
		"inactive_timeout and be positive integers")

	// ErrEnableInactiveChan signals that a request to enable a channel
	// could not be completed because the channel isn't actually active at
	// the time of the request.
	ErrEnableInactiveChan = errors.New("unable to enable channel which " +
		"is not currently active")
)

// ChanStatusConfig holds parameters and resources required by the
// ChanStatusManager to perform its duty.
type ChanStatusConfig struct {
	// OurPubKey is the public key identifying this node on the network.
	OurPubKey *btcec.PublicKey

	// MessageSigner signs messages that validate under OurPubKey.
	MessageSigner lnwallet.MessageSigner

	// IsChannelActive checks whether the channel identified by the provided
	// ChannelID is considered active. This should only return true if the
	// channel has been sufficiently confirmed, the channel has received
	// FundingLocked, and the remote peer is online.
	IsChannelActive func(lnwire.ChannelID) bool

	// ApplyChannelUpdate processes new ChannelUpdates signed by our node by
	// updating our local routing table and broadcasting the update to our
	// peers.
	ApplyChannelUpdate func(*lnwire.ChannelUpdate) error

	// DB stores the set of channels that are to be monitored.
	DB DB

	// Graph stores the channel info and policies for channels in DB.
	Graph ChannelGraph

	// ChanEnableTimeout is the duration a peer's connect must remain stable
	// before attempting to reenable the channel.
	//
	// NOTE: This value is only used to verify that the relation between
	// itself, ChanDisableTimeout, and ChanStatusSampleInterval is correct.
	// The user is still responsible for ensuring that the same duration
	// elapses before attempting to reenable a channel.
	ChanEnableTimeout time.Duration

	// ChanDisableTimeout is the duration the manager will wait after
	// detecting that a channel has become inactive before broadcasting an
	// update to disable the channel.
	ChanDisableTimeout time.Duration

	// ChanStatusSampleInterval is the long-polling interval used by the
	// manager to check if the channels being monitored have become
	// inactive.
	ChanStatusSampleInterval time.Duration
}

// ChanStatusManager facilitates requests to enable or disable a channel via a
// network announcement that sets the disable bit on the ChannelUpdate
// accordingly. The manager will periodically sample to detect cases where a
// link has become inactive, and facilitate the process of disabling the channel
// passively. The ChanStatusManager state machine is designed to reduce the
// likelihood of spamming the network with updates for flapping peers.
type ChanStatusManager struct {
	started sync.Once
	stopped sync.Once

	cfg *ChanStatusConfig

	// ourPubKeyBytes is the serialized compressed pubkey of our node.
	ourPubKeyBytes []byte

	// chanStates contains the set of channels being monitored for status
	// updates. Access to the map is serialized by the statusManager's event
	// loop.
	chanStates channelStates

	// enableRequests pipes external requests to enable a channel into the
	// primary event loop.
	enableRequests chan statusRequest

	// disableRequests pipes external requests to disable a channel into the
	// primary event loop.
	disableRequests chan statusRequest

	// statusSampleTicker fires at the interval prescribed by
	// ChanStatusSampleInterval to check if channels in chanStates have
	// become inactive.
	statusSampleTicker *time.Ticker

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewChanStatusManager initializes a new ChanStatusManager using the given
// configuration. An error is returned if the timeouts and sample interval fail
// to meet do not satisfy the equation:
//   ChanEnableTimeout + ChanStatusSampleInterval > ChanDisableTimeout.
func NewChanStatusManager(cfg *ChanStatusConfig) (*ChanStatusManager, error) {
	// Assert that the config timeouts are properly formed. We require the
	// enable_timeout + sample_interval to be less than or equal to the
	// disable_timeout and that all are positive values. A peer that
	// disconnects and reconnects quickly may cause a disable update to be
	// sent, shortly followed by a reenable. Ensuring a healthy separation
	// helps dampen the possibility of spamming updates that toggle the
	// disable bit for such events.
	if cfg.ChanStatusSampleInterval <= 0 {
		return nil, ErrInvalidTimeoutConstraints
	}
	if cfg.ChanEnableTimeout <= 0 {
		return nil, ErrInvalidTimeoutConstraints
	}
	if cfg.ChanDisableTimeout <= 0 {
		return nil, ErrInvalidTimeoutConstraints
	}
	if cfg.ChanEnableTimeout+cfg.ChanStatusSampleInterval >
		cfg.ChanDisableTimeout {
		return nil, ErrInvalidTimeoutConstraints

	}

	return &ChanStatusManager{
		cfg:                cfg,
		ourPubKeyBytes:     cfg.OurPubKey.SerializeCompressed(),
		chanStates:         make(channelStates),
		statusSampleTicker: time.NewTicker(cfg.ChanStatusSampleInterval),
		enableRequests:     make(chan statusRequest),
		disableRequests:    make(chan statusRequest),
		quit:               make(chan struct{}),
	}, nil
}

// Start safely starts the ChanStatusManager.
func (m *ChanStatusManager) Start() error {
	var err error
	m.started.Do(func() {
		err = m.start()
	})
	return err
}

func (m *ChanStatusManager) start() error {
	channels, err := m.fetchChannels()
	if err != nil {
		return err
	}

	// Populate the initial states of all confirmed, public channels.
	for _, c := range channels {
		_, err := m.getOrInitChanStatus(c.FundingOutpoint)
		switch {

		// If we can't retrieve the edge info for this channel, it may
		// have been pruned from the channel graph but not yet from our
		// set of channels. We'll skip it as we can't determine its
		// initial state.
		case err == channeldb.ErrEdgeNotFound:
			log.Warnf("Unable to find channel policies for %v, "+
				"skipping. This is typical if the channel is "+
				"in the process of closing.", c.FundingOutpoint)
			continue

		case err != nil:
			return err
		}
	}

	m.wg.Add(1)
	go m.statusManager()

	return nil
}

// Stop safely shuts down the ChanStatusManager.
func (m *ChanStatusManager) Stop() error {
	m.stopped.Do(func() {
		close(m.quit)
		m.wg.Wait()
	})
	return nil
}

// RequestEnable submits a request to immediately enable a channel identified by
// the provided outpoint. If the channel is already enabled, no action will be
// taken. If the channel is marked pending-disable the channel will be returned
// to an active status as the scheduled disable was never sent. Otherwise if the
// channel is found to be disabled, a new announcement will be signed with the
// disabled bit cleared and broadcast to the network.
//
// NOTE: RequestEnable should only be called after a stable connection with the
// channel's peer has lasted at least the ChanEnableTimeout. Failure to do so
// may result in behavior that deviates from the expected behavior of the state
// machine.
func (m *ChanStatusManager) RequestEnable(outpoint wire.OutPoint) error {
	return m.submitRequest(m.enableRequests, outpoint)
}

// RequestDisable submits a request to immediately disable a channel identified
// by the provided outpoint. If the channel is already disabled, no action will
// be taken. Otherwise, a new announcement will be signed with the disabled bit
// set and broadcast to the network.
func (m *ChanStatusManager) RequestDisable(outpoint wire.OutPoint) error {
	return m.submitRequest(m.disableRequests, outpoint)
}

// statusRequest is passed to the statusManager to request a change in status
// for a particular channel point.  The exact action is governed by passing the
// request through one of the enableRequests or disableRequests channels.
type statusRequest struct {
	outpoint wire.OutPoint
	errChan  chan error
}

// submitRequest sends a request for either enabling or disabling a particular
// outpoint and awaits an error response. The request type is dictated by the
// reqChan passed in, which can be either of the enableRequests or
// disableRequests channels.
func (m *ChanStatusManager) submitRequest(reqChan chan statusRequest,
	outpoint wire.OutPoint) error {

	req := statusRequest{
		outpoint: outpoint,
		errChan:  make(chan error, 1),
	}

	select {
	case reqChan <- req:
	case <-m.quit:
		return ErrChanStatusManagerExiting
	}

	select {
	case err := <-req.errChan:
		return err
	case <-m.quit:
		return ErrChanStatusManagerExiting
	}
}

// statusManager is the primary event loop for the ChanStatusManager, providing
// the necessary synchronization primitive to protect access to the chanStates
// map. All requests to explicitly enable or disable a channel are processed
// within this method. The statusManager will also periodically poll the active
// status of channels within the htlcswitch to see if a disable announcement
// should be scheduled or broadcast.
//
// NOTE: This method MUST be run as a goroutine.
func (m *ChanStatusManager) statusManager() {
	defer m.wg.Done()

	for {
		select {

		// Process any requests to mark channel as enabled.
		case req := <-m.enableRequests:
			req.errChan <- m.processEnableRequest(req.outpoint)

		// Process any requests to mark channel as disabled.
		case req := <-m.disableRequests:
			req.errChan <- m.processDisableRequest(req.outpoint)

		// Use long-polling to detect when channels become inactive.
		case <-m.statusSampleTicker.C:
			// First, do a sweep and mark any ChanStatusEnabled
			// channels that are not active within the htlcswitch as
			// ChanStatusPendingDisabled. The channel will then be
			// disabled if no request to enable is received before
			// the ChanDisableTimeout expires.
			m.markPendingInactiveChannels()

			// Now, do another sweep to disable any channels that
			// were marked in a prior iteration as pending inactive
			// if the inactive chan timeout has elapsed.
			m.disableInactiveChannels()

		case <-m.quit:
			return
		}
	}
}

// processEnableRequest attempts to enable the given outpoint. If the method
// returns nil, the status of the channel in chanStates will be
// ChanStatusEnabled. If the channel is not active at the time of the request,
// ErrEnableInactiveChan will be returned. An update will be broadcast only if
// the channel is currently disabled, otherwise no update will be sent on the
// network.
func (m *ChanStatusManager) processEnableRequest(outpoint wire.OutPoint) error {
	curState, err := m.getOrInitChanStatus(outpoint)
	if err != nil {
		return err
	}

	// Quickly check to see if the requested channel is active within the
	// htlcswitch and return an error if it isn't.
	chanID := lnwire.NewChanIDFromOutPoint(&outpoint)
	if !m.cfg.IsChannelActive(chanID) {
		return ErrEnableInactiveChan
	}

	switch curState.Status {

	// Channel is already enabled, nothing to do.
	case ChanStatusEnabled:
		return nil

	// The channel is enabled, though we are now canceling the scheduled
	// disable.
	case ChanStatusPendingDisabled:
		log.Debugf("Channel(%v) already enabled, canceling scheduled "+
			"disable", outpoint)

	// We'll sign a new update if the channel is still disabled.
	case ChanStatusDisabled:
		log.Infof("Announcing channel(%v) enabled", outpoint)

		err := m.signAndSendNextUpdate(outpoint, false)
		if err != nil {
			return err
		}
	}

	m.chanStates.markEnabled(outpoint)

	return nil
}

// processDisableRequest attempts to disable the given outpoint. If the method
// returns nil, the status of the channel in chanStates will be
// ChanStatusDisabled. An update will only be sent if the channel has a status
// other than ChanStatusEnabled, otherwise no update will be sent on the
// network.
func (m *ChanStatusManager) processDisableRequest(outpoint wire.OutPoint) error {
	curState, err := m.getOrInitChanStatus(outpoint)
	if err != nil {
		return err
	}

	switch curState.Status {

	// Channel is already disabled, nothing to do.
	case ChanStatusDisabled:
		return nil

	// We'll sign a new update disabling the channel if the current status
	// is enabled or pending-inactive.
	case ChanStatusEnabled, ChanStatusPendingDisabled:
		log.Infof("Announcing channel(%v) disabled [requested]",
			outpoint)

		err := m.signAndSendNextUpdate(outpoint, true)
		if err != nil {
			return err
		}
	}

	// If the disable was requested via the manager's public interface, we
	// will remove the output from our map of channel states. Typically this
	// signals that the channel is being closed, so this frees up the space
	// in the map. If for some reason the channel isn't closed, the state
	// will be repopulated on subsequent calls to RequestEnable or
	// RequestDisable via a db lookup, or on startup.
	delete(m.chanStates, outpoint)

	return nil
}

// markPendingInactiveChannels performs a sweep of the database's active
// channels and determines which, if any, should have a disable announcement
// scheduled. Once an active channel is determined to be pending-inactive, one
// of two transitions can follow. Either the channel is disabled because no
// request to enable is received before the scheduled disable is broadcast, or
// the channel is successfully reenabled and channel is returned to an active
// state from the POV of the ChanStatusManager.
func (m *ChanStatusManager) markPendingInactiveChannels() {
	channels, err := m.fetchChannels()
	if err != nil {
		log.Errorf("Unable to load active channels: %v", err)
		return
	}

	for _, c := range channels {
		// Determine the initial status of the active channel, and
		// populate the entry in the chanStates map.
		curState, err := m.getOrInitChanStatus(c.FundingOutpoint)
		if err != nil {
			log.Errorf("Unable to retrieve chan status for "+
				"Channel(%v): %v", c.FundingOutpoint, err)
			continue
		}

		// If the channel's status is not ChanStatusEnabled, we are
		// done.  Either it is already disabled, or it has been marked
		// ChanStatusPendingDisable meaning that we have already
		// scheduled the time at which it will be disabled.
		if curState.Status != ChanStatusEnabled {
			continue
		}

		// If our bookkeeping shows the channel as active, sample the
		// htlcswitch to see if it believes the link is also active. If
		// so, we will skip marking it as ChanStatusPendingDisabled.
		chanID := lnwire.NewChanIDFromOutPoint(&c.FundingOutpoint)
		if m.cfg.IsChannelActive(chanID) {
			continue
		}

		// Otherwise, we discovered that this link was inactive within
		// the switch. Compute the time at which we will send out a
		// disable if the peer is unable to reestablish a stable
		// connection.
		disableTime := time.Now().Add(m.cfg.ChanDisableTimeout)

		log.Debugf("Marking channel(%v) pending-inactive",
			c.FundingOutpoint)

		m.chanStates.markPendingDisabled(c.FundingOutpoint, disableTime)
	}
}

// disableInactiveChannels scans through the set of monitored channels, and
// broadcast a disable update for any pending inactive channels whose
// SendDisableTime has been superseded by the current time.
func (m *ChanStatusManager) disableInactiveChannels() {
	// Now, disable any channels whose inactive chan timeout has elapsed.
	now := time.Now()
	for outpoint, state := range m.chanStates {
		// Ignore statuses that are not in the pending-inactive state.
		if state.Status != ChanStatusPendingDisabled {
			continue
		}

		// Ignore statuses for which the disable timeout has not
		// expired.
		if state.SendDisableTime.After(now) {
			continue
		}

		log.Infof("Announcing channel(%v) disabled "+
			"[detected]", outpoint)

		// Sign an update disabling the channel.
		err := m.signAndSendNextUpdate(outpoint, true)
		if err != nil {
			log.Errorf("Unable to sign update disabling "+
				"channel(%v): %v", outpoint, err)

			// If the edge was not found, this is a likely indicator
			// that the channel has been closed. Thus we remove the
			// outpoint from the set of tracked outpoints to prevent
			// further attempts.
			if err == channeldb.ErrEdgeNotFound {
				log.Debugf("Removing channel(%v) from "+
					"consideration for passive disabling",
					outpoint)
				delete(m.chanStates, outpoint)
			}

			continue
		}

		// Record that the channel has now been disabled.
		m.chanStates.markDisabled(outpoint)
	}
}

// fetchChannels returns the working set of channels managed by the
// ChanStatusManager. The returned channels are filtered to only contain public
// channels.
func (m *ChanStatusManager) fetchChannels() ([]*channeldb.OpenChannel, error) {
	allChannels, err := m.cfg.DB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	// Filter out private channels.
	var channels []*channeldb.OpenChannel
	for _, c := range allChannels {
		// We'll skip any private channels, as they aren't used for
		// routing within the network by other nodes.
		if c.ChannelFlags&lnwire.FFAnnounceChannel == 0 {
			continue
		}

		channels = append(channels, c)
	}

	return channels, nil
}

// signAndSendNextUpdate computes and signs a valid update for the passed
// outpoint, with the ability to toggle the disabled bit. The new update will
// use the current time as the update's timestamp, or increment the old
// timestamp by 1 to ensure the update can propagate. If signing is successful,
// the new update will be sent out on the network.
func (m *ChanStatusManager) signAndSendNextUpdate(outpoint wire.OutPoint,
	disabled bool) error {

	// Retrieve the latest update for this channel. We'll use this
	// as our starting point to send the new update.
	chanUpdate, err := m.fetchLastChanUpdateByOutPoint(outpoint)
	if err != nil {
		return err
	}

	err = SignChannelUpdate(
		m.cfg.MessageSigner, m.cfg.OurPubKey, chanUpdate,
		ChannelUpdateSetDisable(disabled),
	)
	if err != nil {
		return err
	}

	return m.cfg.ApplyChannelUpdate(chanUpdate)
}

// fetchLastChanUpdateByOutPoint fetches the latest policy for our direction of
// a channel, and crafts a new ChannelUpdate with this policy. Returns an error
// in case our ChannelEdgePolicy is not found in the database.
func (m *ChanStatusManager) fetchLastChanUpdateByOutPoint(op wire.OutPoint) (
	*lnwire.ChannelUpdate, error) {

	// Get the edge info and policies for this channel from the graph.
	info, edge1, edge2, err := m.cfg.Graph.FetchChannelEdgesByOutpoint(&op)
	if err != nil {
		return nil, err
	}

	return ExtractChannelUpdate(m.ourPubKeyBytes, info, edge1, edge2)
}

// loadInitialChanState determines the initial ChannelState for a particular
// outpoint. The initial ChanStatus for a given outpoint will either be
// ChanStatusEnabled or ChanStatusDisabled, determined by inspecting the bits on
// the most recent announcement. An error is returned if the latest update could
// not be retrieved.
func (m *ChanStatusManager) loadInitialChanState(
	outpoint *wire.OutPoint) (ChannelState, error) {

	lastUpdate, err := m.fetchLastChanUpdateByOutPoint(*outpoint)
	if err != nil {
		return ChannelState{}, err
	}

	// Determine the channel's starting status by inspecting the disable bit
	// on last announcement we sent out.
	var initialStatus ChanStatus
	if lastUpdate.ChannelFlags&lnwire.ChanUpdateDisabled == 0 {
		initialStatus = ChanStatusEnabled
	} else {
		initialStatus = ChanStatusDisabled
	}

	return ChannelState{
		Status: initialStatus,
	}, nil
}

// getOrInitChanStatus retrieves the current ChannelState for a particular
// outpoint. If the chanStates map already contains an entry for the outpoint,
// the value in the map is returned. Otherwise, the outpoint's initial status is
// computed and updated in the chanStates map before being returned.
func (m *ChanStatusManager) getOrInitChanStatus(
	outpoint wire.OutPoint) (ChannelState, error) {

	// Return the current ChannelState from the chanStates map if it is
	// already known to the ChanStatusManager.
	if curState, ok := m.chanStates[outpoint]; ok {
		return curState, nil
	}

	// Otherwise, determine the initial state based on the last update we
	// sent for the outpoint.
	initialState, err := m.loadInitialChanState(&outpoint)
	if err != nil {
		return ChannelState{}, err
	}

	// Finally, store the initial state in the chanStates map. This will
	// serve as are up-to-date view of the outpoint's current status, in
	// addition to making the channel eligible for detecting inactivity.
	m.chanStates[outpoint] = initialState

	return initialState, nil
}

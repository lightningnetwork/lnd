package wtclient

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

const (
	// DefaultReadTimeout specifies the default duration we will wait during
	// a read before breaking out of a blocking read.
	DefaultReadTimeout = 15 * time.Second

	// DefaultWriteTimeout specifies the default duration we will wait during
	// a write before breaking out of a blocking write.
	DefaultWriteTimeout = 15 * time.Second

	// DefaultStatInterval specifies the default interval between logging
	// metrics about the client's operation.
	DefaultStatInterval = time.Minute

	// DefaultForceQuitDelay specifies the default duration after which the
	// client should abandon any pending updates or session negotiations
	// before terminating.
	DefaultForceQuitDelay = 10 * time.Second
)

// RegisteredTower encompasses information about a registered watchtower with
// the client.
type RegisteredTower struct {
	*wtdb.Tower

	// Sessions is the set of sessions corresponding to the watchtower.
	Sessions map[wtdb.SessionID]*wtdb.ClientSession

	// ActiveSessionCandidate determines whether the watchtower is currently
	// being considered for new sessions.
	ActiveSessionCandidate bool
}

// Client is the primary interface used by the daemon to control a client's
// lifecycle and backup revoked states.
type Client interface {
	// AddTower adds a new watchtower reachable at the given address and
	// considers it for new sessions. If the watchtower already exists, then
	// any new addresses included will be considered when dialing it for
	// session negotiations and backups.
	AddTower(*lnwire.NetAddress) error

	// RemoveTower removes a watchtower from being considered for future
	// session negotiations and from being used for any subsequent backups
	// until it's added again. If an address is provided, then this call
	// only serves as a way of removing the address from the watchtower
	// instead.
	RemoveTower(*btcec.PublicKey, net.Addr) error

	// RegisteredTowers retrieves the list of watchtowers registered with
	// the client.
	RegisteredTowers() ([]*RegisteredTower, error)

	// LookupTower retrieves a registered watchtower through its public key.
	LookupTower(*btcec.PublicKey) (*RegisteredTower, error)

	// Stats returns the in-memory statistics of the client since startup.
	Stats() ClientStats

	// Policy returns the active client policy configuration.
	Policy() wtpolicy.Policy

	// RegisterChannel persistently initializes any channel-dependent
	// parameters within the client. This should be called during link
	// startup to ensure that the client is able to support the link during
	// operation.
	RegisterChannel(lnwire.ChannelID) error

	// BackupState initiates a request to back up a particular revoked
	// state. If the method returns nil, the backup is guaranteed to be
	// successful unless the client is force quit, or the justice
	// transaction would create dust outputs when trying to abide by the
	// negotiated policy. If the channel we're trying to back up doesn't
	// have a tweak for the remote party's output, then isTweakless should
	// be true.
	BackupState(*lnwire.ChannelID, *lnwallet.BreachRetribution, bool) error

	// Start initializes the watchtower client, allowing it process requests
	// to backup revoked channel states.
	Start() error

	// Stop attempts a graceful shutdown of the watchtower client. In doing
	// so, it will attempt to flush the pipeline and deliver any queued
	// states to the tower before exiting.
	Stop() error

	// ForceQuit will forcibly shutdown the watchtower client. Calling this
	// may lead to queued states being dropped.
	ForceQuit()
}

// Config provides the TowerClient with access to the resources it requires to
// perform its duty. All nillable fields must be non-nil for the tower to be
// initialized properly.
type Config struct {
	// Signer provides access to the wallet so that the client can sign
	// justice transactions that spend from a remote party's commitment
	// transaction.
	Signer input.Signer

	// NewAddress generates a new on-chain sweep pkscript.
	NewAddress func() ([]byte, error)

	// SecretKeyRing is used to derive the session keys used to communicate
	// with the tower. The client only stores the KeyLocators internally so
	// that we never store private keys on disk.
	SecretKeyRing SecretKeyRing

	// Dial connects to an addr using the specified net and returns the
	// connection object.
	Dial Dial

	// AuthDialer establishes a brontide connection over an onion or clear
	// network.
	AuthDial AuthDialer

	// DB provides access to the client's stable storage medium.
	DB DB

	// Policy is the session policy the client will propose when creating
	// new sessions with the tower. If the policy differs from any active
	// sessions recorded in the database, those sessions will be ignored and
	// new sessions will be requested immediately.
	Policy wtpolicy.Policy

	// ChainHash identifies the chain that the client is on and for which
	// the tower must be watching to monitor for breaches.
	ChainHash chainhash.Hash

	// ForceQuitDelay is the duration after attempting to shutdown that the
	// client will automatically abort any pending backups if an unclean
	// shutdown is detected. If the value is less than or equal to zero, a
	// call to Stop may block indefinitely. The client can always be
	// ForceQuit externally irrespective of the chosen parameter.
	ForceQuitDelay time.Duration

	// ReadTimeout is the duration we will wait during a read before
	// breaking out of a blocking read. If the value is less than or equal
	// to zero, the default will be used instead.
	ReadTimeout time.Duration

	// WriteTimeout is the duration we will wait during a write before
	// breaking out of a blocking write. If the value is less than or equal
	// to zero, the default will be used instead.
	WriteTimeout time.Duration

	// MinBackoff defines the initial backoff applied to connections with
	// watchtowers. Subsequent backoff durations will grow exponentially up
	// until MaxBackoff.
	MinBackoff time.Duration

	// MaxBackoff defines the maximum backoff applied to connections with
	// watchtowers. If the exponential backoff produces a timeout greater
	// than this value, the backoff will be clamped to MaxBackoff.
	MaxBackoff time.Duration
}

// newTowerMsg is an internal message we'll use within the TowerClient to signal
// that a new tower can be considered.
type newTowerMsg struct {
	// addr is the tower's reachable address that we'll use to establish a
	// connection with.
	addr *lnwire.NetAddress

	// errChan is the channel through which we'll send a response back to
	// the caller when handling their request.
	//
	// NOTE: This channel must be buffered.
	errChan chan error
}

// staleTowerMsg is an internal message we'll use within the TowerClient to
// signal that a tower should no longer be considered.
type staleTowerMsg struct {
	// pubKey is the identifying public key of the watchtower.
	pubKey *btcec.PublicKey

	// addr is an optional field that when set signals that the address
	// should be removed from the watchtower's set of addresses, indicating
	// that it is stale. If it's not set, then the watchtower should be
	// no longer be considered for new sessions.
	addr net.Addr

	// errChan is the channel through which we'll send a response back to
	// the caller when handling their request.
	//
	// NOTE: This channel must be buffered.
	errChan chan error
}

// TowerClient is a concrete implementation of the Client interface, offering a
// non-blocking, reliable subsystem for backing up revoked states to a specified
// private tower.
type TowerClient struct {
	started sync.Once
	stopped sync.Once
	forced  sync.Once

	cfg *Config

	pipeline *taskPipeline

	negotiator        SessionNegotiator
	candidateTowers   TowerCandidateIterator
	candidateSessions map[wtdb.SessionID]*wtdb.ClientSession
	activeSessions    sessionQueueSet

	sessionQueue *sessionQueue
	prevTask     *backupTask

	backupMu          sync.Mutex
	summaries         wtdb.ChannelSummaries
	chanCommitHeights map[lnwire.ChannelID]uint64

	statTicker *time.Ticker
	stats      *ClientStats

	newTowers   chan *newTowerMsg
	staleTowers chan *staleTowerMsg

	wg        sync.WaitGroup
	forceQuit chan struct{}
}

// Compile-time constraint to ensure *TowerClient implements the Client
// interface.
var _ Client = (*TowerClient)(nil)

// New initializes a new TowerClient from the provide Config. An error is
// returned if the client could not initialized.
func New(config *Config) (*TowerClient, error) {
	// Copy the config to prevent side-effects from modifying both the
	// internal and external version of the Config.
	cfg := new(Config)
	*cfg = *config

	// Set the read timeout to the default if none was provided.
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = DefaultReadTimeout
	}

	// Set the write timeout to the default if none was provided.
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}

	// Next, load all candidate sessions and towers from the database into
	// the client. We will use any of these session if their policies match
	// the current policy of the client, otherwise they will be ignored and
	// new sessions will be requested.
	sessions, err := cfg.DB.ListClientSessions(nil)
	if err != nil {
		return nil, err
	}

	candidateSessions := make(map[wtdb.SessionID]*wtdb.ClientSession)
	sessionTowers := make(map[wtdb.TowerID]*wtdb.Tower)
	for _, s := range sessions {
		// Candidate sessions must be in an active state.
		if s.Status != wtdb.CSessionActive {
			continue
		}

		// Reload the tower from disk using the tower ID contained in
		// each candidate session. We will also rederive any session
		// keys needed to be able to communicate with the towers and
		// authenticate session requests. This prevents us from having
		// to store the private keys on disk.
		tower, ok := sessionTowers[s.TowerID]
		if !ok {
			var err error
			tower, err = cfg.DB.LoadTowerByID(s.TowerID)
			if err != nil {
				return nil, err
			}
		}
		s.Tower = tower

		sessionKey, err := DeriveSessionKey(cfg.SecretKeyRing, s.KeyIndex)
		if err != nil {
			return nil, err
		}
		s.SessionPrivKey = sessionKey

		candidateSessions[s.ID] = s
		sessionTowers[tower.ID] = tower
	}

	var candidateTowers []*wtdb.Tower
	for _, tower := range sessionTowers {
		log.Infof("Using private watchtower %s, offering policy %s",
			tower, cfg.Policy)
		candidateTowers = append(candidateTowers, tower)
	}

	// Load the sweep pkscripts that have been generated for all previously
	// registered channels.
	chanSummaries, err := cfg.DB.FetchChanSummaries()
	if err != nil {
		return nil, err
	}

	c := &TowerClient{
		cfg:               cfg,
		pipeline:          newTaskPipeline(),
		candidateTowers:   newTowerListIterator(candidateTowers...),
		candidateSessions: candidateSessions,
		activeSessions:    make(sessionQueueSet),
		summaries:         chanSummaries,
		statTicker:        time.NewTicker(DefaultStatInterval),
		stats:             new(ClientStats),
		newTowers:         make(chan *newTowerMsg),
		staleTowers:       make(chan *staleTowerMsg),
		forceQuit:         make(chan struct{}),
	}
	c.negotiator = newSessionNegotiator(&NegotiatorConfig{
		DB:            cfg.DB,
		SecretKeyRing: cfg.SecretKeyRing,
		Policy:        cfg.Policy,
		ChainHash:     cfg.ChainHash,
		SendMessage:   c.sendMessage,
		ReadMessage:   c.readMessage,
		Dial:          c.dial,
		Candidates:    c.candidateTowers,
		MinBackoff:    cfg.MinBackoff,
		MaxBackoff:    cfg.MaxBackoff,
	})

	// Reconstruct the highest commit height processed for each channel
	// under the client's current policy.
	c.buildHighestCommitHeights()

	return c, nil
}

// buildHighestCommitHeights inspects the full set of candidate client sessions
// loaded from disk, and determines the highest known commit height for each
// channel. This allows the client to reject backups that it has already
// processed for it's active policy.
func (c *TowerClient) buildHighestCommitHeights() {
	chanCommitHeights := make(map[lnwire.ChannelID]uint64)
	for _, s := range c.candidateSessions {
		// We only want to consider accepted updates that have been
		// accepted under an identical policy to the client's current
		// policy.
		if s.Policy != c.cfg.Policy {
			continue
		}

		// Take the highest commit height found in the session's
		// committed updates.
		for _, committedUpdate := range s.CommittedUpdates {
			bid := committedUpdate.BackupID

			height, ok := chanCommitHeights[bid.ChanID]
			if !ok || bid.CommitHeight > height {
				chanCommitHeights[bid.ChanID] = bid.CommitHeight
			}
		}

		// Take the heights commit height found in the session's acked
		// updates.
		for _, bid := range s.AckedUpdates {
			height, ok := chanCommitHeights[bid.ChanID]
			if !ok || bid.CommitHeight > height {
				chanCommitHeights[bid.ChanID] = bid.CommitHeight
			}
		}
	}

	c.chanCommitHeights = chanCommitHeights
}

// Start initializes the watchtower client by loading or negotiating an active
// session and then begins processing backup tasks from the request pipeline.
func (c *TowerClient) Start() error {
	var err error
	c.started.Do(func() {
		log.Infof("Starting watchtower client")

		// First, restart a session queue for any sessions that have
		// committed but unacked state updates. This ensures that these
		// sessions will be able to flush the committed updates after a
		// restart.
		for _, session := range c.candidateSessions {
			if len(session.CommittedUpdates) > 0 {
				log.Infof("Starting session=%s to process "+
					"%d committed backups", session.ID,
					len(session.CommittedUpdates))
				c.initActiveQueue(session)
			}
		}

		// Now start the session negotiator, which will allow us to
		// request new session as soon as the backupDispatcher starts
		// up.
		err = c.negotiator.Start()
		if err != nil {
			return
		}

		// Start the task pipeline to which new backup tasks will be
		// submitted from active links.
		c.pipeline.Start()

		c.wg.Add(1)
		go c.backupDispatcher()

		log.Infof("Watchtower client started successfully")
	})
	return err
}

// Stop idempotently initiates a graceful shutdown of the watchtower client.
func (c *TowerClient) Stop() error {
	c.stopped.Do(func() {
		log.Debugf("Stopping watchtower client")

		// 1. Shutdown the backup queue, which will prevent any further
		// updates from being accepted. In practice, the links should be
		// shutdown before the client has been stopped, so all updates
		// would have been added prior.
		c.pipeline.Stop()

		// 2. To ensure we don't hang forever on shutdown due to
		// unintended failures, we'll delay a call to force quit the
		// pipeline if a ForceQuitDelay is specified. This will have no
		// effect if the pipeline shuts down cleanly before the delay
		// fires.
		//
		// For full safety, this can be set to 0 and wait out
		// indefinitely.  However for mobile clients which may have a
		// limited amount of time to exit before the background process
		// is killed, this offers a way to ensure the process
		// terminates.
		if c.cfg.ForceQuitDelay > 0 {
			time.AfterFunc(c.cfg.ForceQuitDelay, c.ForceQuit)
		}

		// 3. Once the backup queue has shutdown, wait for the main
		// dispatcher to exit. The backup queue will signal it's
		// completion to the dispatcher, which releases the wait group
		// after all tasks have been assigned to session queues.
		c.wg.Wait()

		// 4. Since all valid tasks have been assigned to session
		// queues, we no longer need to negotiate sessions.
		c.negotiator.Stop()

		log.Debugf("Waiting for active session queues to finish "+
			"draining, stats: %s", c.stats)

		// 5. Shutdown all active session queues in parallel. These will
		// exit once all updates have been acked by the watchtower.
		c.activeSessions.ApplyAndWait(func(s *sessionQueue) func() {
			return s.Stop
		})

		// Skip log if force quitting.
		select {
		case <-c.forceQuit:
			return
		default:
		}

		log.Debugf("Client successfully stopped, stats: %s", c.stats)
	})
	return nil
}

// ForceQuit idempotently initiates an unclean shutdown of the watchtower
// client. This should only be executed if Stop is unable to exit cleanly.
func (c *TowerClient) ForceQuit() {
	c.forced.Do(func() {
		log.Infof("Force quitting watchtower client")

		// 1. Shutdown the backup queue, which will prevent any further
		// updates from being accepted. In practice, the links should be
		// shutdown before the client has been stopped, so all updates
		// would have been added prior.
		c.pipeline.ForceQuit()

		// 2. Once the backup queue has shutdown, wait for the main
		// dispatcher to exit. The backup queue will signal it's
		// completion to the dispatcher, which releases the wait group
		// after all tasks have been assigned to session queues.
		close(c.forceQuit)
		c.wg.Wait()

		// 3. Since all valid tasks have been assigned to session
		// queues, we no longer need to negotiate sessions.
		c.negotiator.Stop()

		// 4. Force quit all active session queues in parallel. These
		// will exit once all updates have been acked by the watchtower.
		c.activeSessions.ApplyAndWait(func(s *sessionQueue) func() {
			return s.ForceQuit
		})

		log.Infof("Watchtower client unclean shutdown complete, "+
			"stats: %s", c.stats)
	})
}

// RegisterChannel persistently initializes any channel-dependent parameters
// within the client. This should be called during link startup to ensure that
// the client is able to support the link during operation.
func (c *TowerClient) RegisterChannel(chanID lnwire.ChannelID) error {
	c.backupMu.Lock()
	defer c.backupMu.Unlock()

	// If a pkscript for this channel already exists, the channel has been
	// previously registered.
	if _, ok := c.summaries[chanID]; ok {
		return nil
	}

	// Otherwise, generate a new sweep pkscript used to sweep funds for this
	// channel.
	pkScript, err := c.cfg.NewAddress()
	if err != nil {
		return err
	}

	// Persist the sweep pkscript so that restarts will not introduce
	// address inflation when the channel is reregistered after a restart.
	err = c.cfg.DB.RegisterChannel(chanID, pkScript)
	if err != nil {
		return err
	}

	// Finally, cache the pkscript in our in-memory cache to avoid db
	// lookups for the remainder of the daemon's execution.
	c.summaries[chanID] = wtdb.ClientChanSummary{
		SweepPkScript: pkScript,
	}

	return nil
}

// BackupState initiates a request to back up a particular revoked state. If the
// method returns nil, the backup is guaranteed to be successful unless the:
//  - client is force quit,
//  - justice transaction would create dust outputs when trying to abide by the
//    negotiated policy, or
//  - breached outputs contain too little value to sweep at the target sweep fee
//    rate.
func (c *TowerClient) BackupState(chanID *lnwire.ChannelID,
	breachInfo *lnwallet.BreachRetribution, isTweakless bool) error {

	// Retrieve the cached sweep pkscript used for this channel.
	c.backupMu.Lock()
	summary, ok := c.summaries[*chanID]
	if !ok {
		c.backupMu.Unlock()
		return ErrUnregisteredChannel
	}

	// Ignore backups that have already been presented to the client.
	height, ok := c.chanCommitHeights[*chanID]
	if ok && breachInfo.RevokedStateNum <= height {
		c.backupMu.Unlock()
		log.Debugf("Ignoring duplicate backup for chanid=%v at height=%d",
			chanID, breachInfo.RevokedStateNum)
		return nil
	}

	// This backup has a higher commit height than any known backup for this
	// channel. We'll update our tip so that we won't accept it again if the
	// link flaps.
	c.chanCommitHeights[*chanID] = breachInfo.RevokedStateNum
	c.backupMu.Unlock()

	task := newBackupTask(
		chanID, breachInfo, summary.SweepPkScript, isTweakless,
	)

	return c.pipeline.QueueBackupTask(task)
}

// nextSessionQueue attempts to fetch an active session from our set of
// candidate sessions. Candidate sessions with a differing policy from the
// active client's advertised policy will be ignored, but may be resumed if the
// client is restarted with a matching policy. If no candidates were found, nil
// is returned to signal that we need to request a new policy.
func (c *TowerClient) nextSessionQueue() *sessionQueue {
	// Select any candidate session at random, and remove it from the set of
	// candidate sessions.
	var candidateSession *wtdb.ClientSession
	for id, sessionInfo := range c.candidateSessions {
		delete(c.candidateSessions, id)

		// Skip any sessions with policies that don't match the current
		// TxPolicy, as they would result in different justice
		// transactions from what is requested. These can be used again
		// if the client changes their configuration and restarting.
		if sessionInfo.Policy.TxPolicy != c.cfg.Policy.TxPolicy {
			continue
		}

		candidateSession = sessionInfo
		break
	}

	// If none of the sessions could be used or none were found, we'll
	// return nil to signal that we need another session to be negotiated.
	if candidateSession == nil {
		return nil
	}

	// Initialize the session queue and spin it up so it can begin handling
	// updates. If the queue was already made active on startup, this will
	// simply return the existing session queue from the set.
	return c.getOrInitActiveQueue(candidateSession)
}

// backupDispatcher processes events coming from the taskPipeline and is
// responsible for detecting when the client needs to renegotiate a session to
// fulfill continuing demand. The event loop exits after all tasks have been
// received from the upstream taskPipeline, or the taskPipeline is force quit.
//
// NOTE: This method MUST be run as a goroutine.
func (c *TowerClient) backupDispatcher() {
	defer c.wg.Done()

	log.Tracef("Starting backup dispatcher")
	defer log.Tracef("Stopping backup dispatcher")

	for {
		switch {

		// No active session queue and no additional sessions.
		case c.sessionQueue == nil && len(c.candidateSessions) == 0:
			log.Infof("Requesting new session.")

			// Immediately request a new session.
			c.negotiator.RequestSession()

			// Wait until we receive the newly negotiated session.
			// All backups sent in the meantime are queued in the
			// revoke queue, as we cannot process them.
		awaitSession:
			select {
			case session := <-c.negotiator.NewSessions():
				log.Infof("Acquired new session with id=%s",
					session.ID)
				c.candidateSessions[session.ID] = session
				c.stats.sessionAcquired()

				// We'll continue to choose the newly negotiated
				// session as our active session queue.
				continue

			case <-c.statTicker.C:
				log.Infof("Client stats: %s", c.stats)

			// A new tower has been requested to be added. We'll
			// update our persisted and in-memory state and consider
			// its corresponding sessions, if any, as new
			// candidates.
			case msg := <-c.newTowers:
				msg.errChan <- c.handleNewTower(msg)

			// A tower has been requested to be removed. We'll
			// immediately return an error as we want to avoid the
			// possibility of a new session being negotiated with
			// this request's tower.
			case msg := <-c.staleTowers:
				msg.errChan <- errors.New("removing towers " +
					"is disallowed while a new session " +
					"negotiation is in progress")

			case <-c.forceQuit:
				return
			}

			// Instead of looping, we'll jump back into the select
			// case and await the delivery of the session to prevent
			// us from re-requesting additional sessions.
			goto awaitSession

		// No active session queue but have additional sessions.
		case c.sessionQueue == nil && len(c.candidateSessions) > 0:
			// We've exhausted the prior session, we'll pop another
			// from the remaining sessions and continue processing
			// backup tasks.
			c.sessionQueue = c.nextSessionQueue()
			if c.sessionQueue != nil {
				log.Debugf("Loaded next candidate session "+
					"queue id=%s", c.sessionQueue.ID())
			}

		// Have active session queue, process backups.
		case c.sessionQueue != nil:
			if c.prevTask != nil {
				c.processTask(c.prevTask)

				// Continue to ensure the sessionQueue is
				// properly initialized before attempting to
				// process more tasks from the pipeline.
				continue
			}

			// Normal operation where new tasks are read from the
			// pipeline.
			select {

			// If any sessions are negotiated while we have an
			// active session queue, queue them for future use.
			// This shouldn't happen with the current design, so
			// it doesn't hurt to select here just in case. In the
			// future, we will likely allow more asynchrony so that
			// we can request new sessions before the session is
			// fully empty, which this case would handle.
			case session := <-c.negotiator.NewSessions():
				log.Warnf("Acquired new session with id=%s "+
					"while processing tasks", session.ID)
				c.candidateSessions[session.ID] = session
				c.stats.sessionAcquired()

			case <-c.statTicker.C:
				log.Infof("Client stats: %s", c.stats)

			// Process each backup task serially from the queue of
			// revoked states.
			case task, ok := <-c.pipeline.NewBackupTasks():
				// All backups in the pipeline have been
				// processed, it is now safe to exit.
				if !ok {
					return
				}

				log.Debugf("Processing %v", task.id)

				c.stats.taskReceived()
				c.processTask(task)

			// A new tower has been requested to be added. We'll
			// update our persisted and in-memory state and consider
			// its corresponding sessions, if any, as new
			// candidates.
			case msg := <-c.newTowers:
				msg.errChan <- c.handleNewTower(msg)

			// A tower has been removed, so we'll remove certain
			// information that's persisted and also in our
			// in-memory state depending on the request, and set any
			// of its corresponding candidate sessions as inactive.
			case msg := <-c.staleTowers:
				msg.errChan <- c.handleStaleTower(msg)
			}
		}
	}
}

// processTask attempts to schedule the given backupTask on the active
// sessionQueue. The task will either be accepted or rejected, afterwhich the
// appropriate modifications to the client's state machine will be made. After
// every invocation of processTask, the caller should ensure that the
// sessionQueue hasn't been exhausted before proceeding to the next task. Tasks
// that are rejected because the active sessionQueue is full will be cached as
// the prevTask, and should be reprocessed after obtaining a new sessionQueue.
func (c *TowerClient) processTask(task *backupTask) {
	status, accepted := c.sessionQueue.AcceptTask(task)
	if accepted {
		c.taskAccepted(task, status)
	} else {
		c.taskRejected(task, status)
	}
}

// taskAccepted processes the acceptance of a task by a sessionQueue depending
// on the state the sessionQueue is in *after* the task is added. The client's
// prevTask is always removed as a result of this call. The client's
// sessionQueue will be removed if accepting the task left the sessionQueue in
// an exhausted state.
func (c *TowerClient) taskAccepted(task *backupTask, newStatus reserveStatus) {
	log.Infof("Queued %v successfully for session %v",
		task.id, c.sessionQueue.ID())

	c.stats.taskAccepted()

	// If this task was accepted, we discard anything held in the prevTask.
	// Either it was nil before, or is the task which was just accepted.
	c.prevTask = nil

	switch newStatus {

	// The sessionQueue still has capacity after accepting this task.
	case reserveAvailable:

	// The sessionQueue is full after accepting this task, so we will need
	// to request a new one before proceeding.
	case reserveExhausted:
		c.stats.sessionExhausted()

		log.Debugf("Session %s exhausted", c.sessionQueue.ID())

		// This task left the session exhausted, set it to nil and
		// proceed to the next loop so we can consume another
		// pre-negotiated session or request another.
		c.sessionQueue = nil
	}
}

// taskRejected process the rejection of a task by a sessionQueue depending on
// the state the was in *before* the task was rejected. The client's prevTask
// will cache the task if the sessionQueue was exhausted before hand, and nil
// the sessionQueue to find a new session. If the sessionQueue was not
// exhausted, the client marks the task as ineligible, as this implies we
// couldn't construct a valid justice transaction given the session's policy.
func (c *TowerClient) taskRejected(task *backupTask, curStatus reserveStatus) {
	switch curStatus {

	// The sessionQueue has available capacity but the task was rejected,
	// this indicates that the task was ineligible for backup.
	case reserveAvailable:
		c.stats.taskIneligible()

		log.Infof("Ignoring ineligible %v", task.id)

		err := c.cfg.DB.MarkBackupIneligible(
			task.id.ChanID, task.id.CommitHeight,
		)
		if err != nil {
			log.Errorf("Unable to mark %v ineligible: %v",
				task.id, err)

			// It is safe to not handle this error, even if we could
			// not persist the result. At worst, this task may be
			// reprocessed on a subsequent start up, and will either
			// succeed do a change in session parameters or fail in
			// the same manner.
		}

		// If this task was rejected *and* the session had available
		// capacity, we discard anything held in the prevTask. Either it
		// was nil before, or is the task which was just rejected.
		c.prevTask = nil

	// The sessionQueue rejected the task because it is full, we will stash
	// this task and try to add it to the next available sessionQueue.
	case reserveExhausted:
		c.stats.sessionExhausted()

		log.Debugf("Session %v exhausted, %v queued for next session",
			c.sessionQueue.ID(), task.id)

		// Cache the task that we pulled off, so that we can process it
		// once a new session queue is available.
		c.sessionQueue = nil
		c.prevTask = task
	}
}

// dial connects the peer at addr using privKey as our secret key for the
// connection. The connection will use the configured Net's resolver to resolve
// the address for either Tor or clear net connections.
func (c *TowerClient) dial(privKey *btcec.PrivateKey,
	addr *lnwire.NetAddress) (wtserver.Peer, error) {

	return c.cfg.AuthDial(privKey, addr, c.cfg.Dial)
}

// readMessage receives and parses the next message from the given Peer. An
// error is returned if a message is not received before the server's read
// timeout, the read off the wire failed, or the message could not be
// deserialized.
func (c *TowerClient) readMessage(peer wtserver.Peer) (wtwire.Message, error) {
	// Set a read timeout to ensure we drop the connection if nothing is
	// received in a timely manner.
	err := peer.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout))
	if err != nil {
		err = fmt.Errorf("unable to set read deadline: %v", err)
		log.Errorf("Unable to read msg: %v", err)
		return nil, err
	}

	// Pull the next message off the wire,
	rawMsg, err := peer.ReadNextMessage()
	if err != nil {
		err = fmt.Errorf("unable to read message: %v", err)
		log.Errorf("Unable to read msg: %v", err)
		return nil, err
	}

	// Parse the received message according to the watchtower wire
	// specification.
	msgReader := bytes.NewReader(rawMsg)
	msg, err := wtwire.ReadMessage(msgReader, 0)
	if err != nil {
		err = fmt.Errorf("unable to parse message: %v", err)
		log.Errorf("Unable to read msg: %v", err)
		return nil, err
	}

	logMessage(peer, msg, true)

	return msg, nil
}

// sendMessage sends a watchtower wire message to the target peer.
func (c *TowerClient) sendMessage(peer wtserver.Peer, msg wtwire.Message) error {
	// Encode the next wire message into the buffer.
	// TODO(conner): use buffer pool
	var b bytes.Buffer
	_, err := wtwire.WriteMessage(&b, msg, 0)
	if err != nil {
		err = fmt.Errorf("Unable to encode msg: %v", err)
		log.Errorf("Unable to send msg: %v", err)
		return err
	}

	// Set the write deadline for the connection, ensuring we drop the
	// connection if nothing is sent in a timely manner.
	err = peer.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout))
	if err != nil {
		err = fmt.Errorf("unable to set write deadline: %v", err)
		log.Errorf("Unable to send msg: %v", err)
		return err
	}

	logMessage(peer, msg, false)

	// Write out the full message to the remote peer.
	_, err = peer.Write(b.Bytes())
	if err != nil {
		log.Errorf("Unable to send msg: %v", err)
	}
	return err
}

// newSessionQueue creates a sessionQueue from a ClientSession loaded from the
// database and supplying it with the resources needed by the client.
func (c *TowerClient) newSessionQueue(s *wtdb.ClientSession) *sessionQueue {
	return newSessionQueue(&sessionQueueConfig{
		ClientSession: s,
		ChainHash:     c.cfg.ChainHash,
		Dial:          c.dial,
		ReadMessage:   c.readMessage,
		SendMessage:   c.sendMessage,
		Signer:        c.cfg.Signer,
		DB:            c.cfg.DB,
		MinBackoff:    c.cfg.MinBackoff,
		MaxBackoff:    c.cfg.MaxBackoff,
	})
}

// getOrInitActiveQueue checks the activeSessions set for a sessionQueue for the
// passed ClientSession. If it exists, the active sessionQueue is returned.
// Otherwise a new sessionQueue is initialized and added to the set.
func (c *TowerClient) getOrInitActiveQueue(s *wtdb.ClientSession) *sessionQueue {
	if sq, ok := c.activeSessions[s.ID]; ok {
		return sq
	}

	return c.initActiveQueue(s)
}

// initActiveQueue creates a new sessionQueue from the passed ClientSession,
// adds the sessionQueue to the activeSessions set, and starts the sessionQueue
// so that it can deliver any committed updates or begin accepting newly
// assigned tasks.
func (c *TowerClient) initActiveQueue(s *wtdb.ClientSession) *sessionQueue {
	// Initialize the session queue, providing it with all of the resources
	// it requires from the client instance.
	sq := c.newSessionQueue(s)

	// Add the session queue as an active session so that we remember to
	// stop it on shutdown.
	c.activeSessions.Add(sq)

	// Start the queue so that it can be active in processing newly assigned
	// tasks or to upload previously committed updates.
	sq.Start()

	return sq
}

// AddTower adds a new watchtower reachable at the given address and considers
// it for new sessions. If the watchtower already exists, then any new addresses
// included will be considered when dialing it for session negotiations and
// backups.
func (c *TowerClient) AddTower(addr *lnwire.NetAddress) error {
	errChan := make(chan error, 1)

	select {
	case c.newTowers <- &newTowerMsg{
		addr:    addr,
		errChan: errChan,
	}:
	case <-c.pipeline.quit:
		return ErrClientExiting
	case <-c.pipeline.forceQuit:
		return ErrClientExiting
	}

	select {
	case err := <-errChan:
		return err
	case <-c.pipeline.quit:
		return ErrClientExiting
	case <-c.pipeline.forceQuit:
		return ErrClientExiting
	}
}

// handleNewTower handles a request for a new tower to be added. If the tower
// already exists, then its corresponding sessions, if any, will be set
// considered as candidates.
func (c *TowerClient) handleNewTower(msg *newTowerMsg) error {
	// We'll start by updating our persisted state, followed by our
	// in-memory state, with the new tower. This might not actually be a new
	// tower, but it might include a new address at which it can be reached.
	tower, err := c.cfg.DB.CreateTower(msg.addr)
	if err != nil {
		return err
	}
	c.candidateTowers.AddCandidate(tower)

	// Include all of its corresponding sessions to our set of candidates.
	sessions, err := c.cfg.DB.ListClientSessions(&tower.ID)
	if err != nil {
		return fmt.Errorf("unable to determine sessions for tower %x: "+
			"%v", tower.IdentityKey.SerializeCompressed(), err)
	}
	for id, session := range sessions {
		c.candidateSessions[id] = session
	}

	return nil
}

// RemoveTower removes a watchtower from being considered for future session
// negotiations and from being used for any subsequent backups until it's added
// again. If an address is provided, then this call only serves as a way of
// removing the address from the watchtower instead.
func (c *TowerClient) RemoveTower(pubKey *btcec.PublicKey, addr net.Addr) error {
	errChan := make(chan error, 1)

	select {
	case c.staleTowers <- &staleTowerMsg{
		pubKey:  pubKey,
		addr:    addr,
		errChan: errChan,
	}:
	case <-c.pipeline.quit:
		return ErrClientExiting
	case <-c.pipeline.forceQuit:
		return ErrClientExiting
	}

	select {
	case err := <-errChan:
		return err
	case <-c.pipeline.quit:
		return ErrClientExiting
	case <-c.pipeline.forceQuit:
		return ErrClientExiting
	}
}

// handleNewTower handles a request for an existing tower to be removed. If none
// of the tower's sessions have pending updates, then they will become inactive
// and removed as candidates. If the active session queue corresponds to any of
// these sessions, a new one will be negotiated.
func (c *TowerClient) handleStaleTower(msg *staleTowerMsg) error {
	// We'll load the tower before potentially removing it in order to
	// retrieve its ID within the database.
	tower, err := c.cfg.DB.LoadTower(msg.pubKey)
	if err != nil {
		return err
	}

	// We'll update our persisted state, followed by our in-memory state,
	// with the stale tower.
	if err := c.cfg.DB.RemoveTower(msg.pubKey, msg.addr); err != nil {
		return err
	}
	c.candidateTowers.RemoveCandidate(tower.ID, msg.addr)

	// If an address was provided, then we're only meant to remove the
	// address from the tower, so there's nothing left for us to do.
	if msg.addr != nil {
		return nil
	}

	// Otherwise, the tower should no longer be used for future session
	// negotiations and backups.
	pubKey := msg.pubKey.SerializeCompressed()
	sessions, err := c.cfg.DB.ListClientSessions(&tower.ID)
	if err != nil {
		return fmt.Errorf("unable to retrieve sessions for tower %x: "+
			"%v", pubKey, err)
	}
	for sessionID := range sessions {
		delete(c.candidateSessions, sessionID)
	}

	// If our active session queue corresponds to the stale tower, we'll
	// proceed to negotiate a new one.
	if c.sessionQueue != nil {
		activeTower := c.sessionQueue.towerAddr.IdentityKey.SerializeCompressed()
		if bytes.Equal(pubKey, activeTower) {
			c.sessionQueue = nil
		}
	}

	return nil
}

// RegisteredTowers retrieves the list of watchtowers registered with the
// client.
func (c *TowerClient) RegisteredTowers() ([]*RegisteredTower, error) {
	// Retrieve all of our towers along with all of our sessions.
	towers, err := c.cfg.DB.ListTowers()
	if err != nil {
		return nil, err
	}
	clientSessions, err := c.cfg.DB.ListClientSessions(nil)
	if err != nil {
		return nil, err
	}

	// Construct a lookup map that coalesces all of the sessions for a
	// specific watchtower.
	towerSessions := make(
		map[wtdb.TowerID]map[wtdb.SessionID]*wtdb.ClientSession,
	)
	for id, s := range clientSessions {
		sessions, ok := towerSessions[s.TowerID]
		if !ok {
			sessions = make(map[wtdb.SessionID]*wtdb.ClientSession)
			towerSessions[s.TowerID] = sessions
		}
		sessions[id] = s
	}

	registeredTowers := make([]*RegisteredTower, 0, len(towerSessions))
	for _, tower := range towers {
		isActive := c.candidateTowers.IsActive(tower.ID)
		registeredTowers = append(registeredTowers, &RegisteredTower{
			Tower:                  tower,
			Sessions:               towerSessions[tower.ID],
			ActiveSessionCandidate: isActive,
		})
	}

	return registeredTowers, nil
}

// LookupTower retrieves a registered watchtower through its public key.
func (c *TowerClient) LookupTower(pubKey *btcec.PublicKey) (*RegisteredTower, error) {
	tower, err := c.cfg.DB.LoadTower(pubKey)
	if err != nil {
		return nil, err
	}

	towerSessions, err := c.cfg.DB.ListClientSessions(&tower.ID)
	if err != nil {
		return nil, err
	}

	return &RegisteredTower{
		Tower:                  tower,
		Sessions:               towerSessions,
		ActiveSessionCandidate: c.candidateTowers.IsActive(tower.ID),
	}, nil
}

// Stats returns the in-memory statistics of the client since startup.
func (c *TowerClient) Stats() ClientStats {
	return c.stats.Copy()
}

// Policy returns the active client policy configuration.
func (c *TowerClient) Policy() wtpolicy.Policy {
	return c.cfg.Policy
}

// logMessage writes information about a message received from a remote peer,
// using directional prepositions to signal whether the message was sent or
// received.
func logMessage(peer wtserver.Peer, msg wtwire.Message, read bool) {
	var action = "Received"
	var preposition = "from"
	if !read {
		action = "Sending"
		preposition = "to"
	}

	summary := wtwire.MessageSummary(msg)
	if len(summary) > 0 {
		summary = "(" + summary + ")"
	}

	log.Debugf("%s %s%v %s %x@%s", action, msg.MsgType(), summary,
		preposition, peer.RemotePub().SerializeCompressed(),
		peer.RemoteAddr())
}

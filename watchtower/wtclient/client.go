package wtclient

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
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

	// DefaultWriteTimeout specifies the default duration we will wait
	// during a write before breaking out of a blocking write.
	DefaultWriteTimeout = 15 * time.Second

	// DefaultStatInterval specifies the default interval between logging
	// metrics about the client's operation.
	DefaultStatInterval = time.Minute

	// DefaultSessionCloseRange is the range over which we will generate a
	// random number of blocks to delay closing a session after its last
	// channel has been closed.
	DefaultSessionCloseRange = 288

	// DefaultMaxTasksInMemQueue is the maximum number of items to be held
	// in the in-memory queue.
	DefaultMaxTasksInMemQueue = 2000
)

// genSessionFilter constructs a filter that can be used to select sessions only
// if they match the policy of the client (namely anchor vs legacy). If
// activeOnly is set, then only active sessions will be returned.
func (c *client) genSessionFilter(
	activeOnly bool) wtdb.ClientSessionFilterFn {

	return func(session *wtdb.ClientSession) bool {
		if c.cfg.Policy.TxPolicy != session.Policy.TxPolicy {
			return false
		}

		if !activeOnly {
			return true
		}

		return session.Status == wtdb.CSessionActive
	}
}

// ExhaustedSessionFilter constructs a wtdb.ClientSessionFilterFn filter
// function that will filter out any sessions that have been exhausted. A
// session is considered exhausted only if it has no un-acked updates and the
// sequence number of the session is equal to the max updates of the session
// policy.
func ExhaustedSessionFilter() wtdb.ClientSessWithNumCommittedUpdatesFilterFn {
	return func(session *wtdb.ClientSession, numUnAcked uint16) bool {
		return session.SeqNum < session.Policy.MaxUpdates ||
			numUnAcked > 0
	}
}

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

// BreachRetributionBuilder is a function that can be used to construct a
// BreachRetribution from a channel ID and a commitment height.
type BreachRetributionBuilder func(id lnwire.ChannelID,
	commitHeight uint64) (*lnwallet.BreachRetribution,
	channeldb.ChannelType, error)

// newTowerMsg is an internal message we'll use within the client to signal
// that a new tower can be considered.
type newTowerMsg struct {
	// tower holds the info about the new Tower or new tower address
	// required to connect to it.
	tower *Tower

	// errChan is the channel through which we'll send a response back to
	// the caller when handling their request.
	//
	// NOTE: This channel must be buffered.
	errChan chan error
}

// staleTowerMsg is an internal message we'll use within the client to
// signal that a tower should no longer be considered.
type staleTowerMsg struct {
	// id is the unique database identifier for the tower.
	id wtdb.TowerID

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

// deactivateTowerMsg is an internal message we'll use within the TowerClient
// to signal that a tower should be marked as inactive.
type deactivateTowerMsg struct {
	// id is the unique database identifier for the tower.
	id wtdb.TowerID

	// pubKey is the identifying public key of the watchtower.
	pubKey *btcec.PublicKey

	// errChan is the channel through which we'll send a response back to
	// the caller when handling their request.
	//
	// NOTE: This channel must be buffered.
	errChan chan error
}

// terminateSessMsg is an internal message we'll use within the TowerClient to
// signal that a session should be terminated.
type terminateSessMsg struct {
	// id is the session identifier.
	id wtdb.SessionID

	// errChan is the channel through which we'll send a response back to
	// the caller when handling their request.
	//
	// NOTE: This channel must be buffered.
	errChan chan error
}

// clientCfg holds the configuration values required by a client.
type clientCfg struct {
	*Config

	// Policy is the session policy the client will propose when creating
	// new sessions with the tower. If the policy differs from any active
	// sessions recorded in the database, those sessions will be ignored and
	// new sessions will be requested immediately.
	Policy wtpolicy.Policy

	getSweepScript func(lnwire.ChannelID) ([]byte, bool)
}

// client manages backing up revoked states for all states that fall under a
// specific policy type.
type client struct {
	cfg *clientCfg

	log btclog.Logger

	pipeline *DiskOverflowQueue[*wtdb.BackupID]

	negotiator        SessionNegotiator
	candidateTowers   TowerCandidateIterator
	candidateSessions map[wtdb.SessionID]*ClientSession
	activeSessions    *sessionQueueSet

	sessionQueue *sessionQueue
	prevTask     *wtdb.BackupID

	statTicker *time.Ticker
	stats      *clientStats

	newTowers         chan *newTowerMsg
	staleTowers       chan *staleTowerMsg
	deactivateTowers  chan *deactivateTowerMsg
	terminateSessions chan *terminateSessMsg

	wg   sync.WaitGroup
	quit chan struct{}
}

// newClient initializes a new client from the provided clientCfg. An error is
// returned if the client could not be initialized.
func newClient(cfg *clientCfg) (*client, error) {
	identifier, err := cfg.Policy.BlobType.Identifier()
	if err != nil {
		return nil, err
	}
	plog := log.WithPrefix(fmt.Sprintf("(%s)", identifier))

	queueDB := cfg.DB.GetDBQueue([]byte(identifier))
	queue, err := NewDiskOverflowQueue[*wtdb.BackupID](
		queueDB, cfg.MaxTasksInMemQueue, plog,
	)
	if err != nil {
		return nil, err
	}

	c := &client{
		cfg:               cfg,
		log:               plog,
		pipeline:          queue,
		activeSessions:    newSessionQueueSet(),
		statTicker:        time.NewTicker(DefaultStatInterval),
		stats:             new(clientStats),
		newTowers:         make(chan *newTowerMsg),
		staleTowers:       make(chan *staleTowerMsg),
		deactivateTowers:  make(chan *deactivateTowerMsg),
		terminateSessions: make(chan *terminateSessMsg),
		quit:              make(chan struct{}),
	}

	candidateTowers := newTowerListIterator()
	perActiveTower := func(tower *Tower) {
		// If the tower has already been marked as active, then there is
		// no need to add it to the iterator again.
		if candidateTowers.IsActive(tower.ID) {
			return
		}

		c.log.Infof("Using private watchtower %x, offering policy %s",
			tower.IdentityKey.SerializeCompressed(), cfg.Policy)

		// Add the tower to the set of candidate towers.
		candidateTowers.AddCandidate(tower)
	}

	// Load all candidate sessions and towers from the database into the
	// client. We will use any of these sessions if their policies match the
	// current policy of the client, otherwise they will be ignored and new
	// sessions will be requested.
	candidateSessions, err := getTowerAndSessionCandidates(
		cfg.DB, cfg.SecretKeyRing, perActiveTower,
		wtdb.WithPreEvalFilterFn(c.genSessionFilter(true)),
		wtdb.WithPostEvalFilterFn(ExhaustedSessionFilter()),
	)
	if err != nil {
		return nil, err
	}

	c.candidateTowers = candidateTowers
	c.candidateSessions = candidateSessions

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
		Log:           plog,
	})

	return c, nil
}

// getTowerAndSessionCandidates loads all the towers from the DB and then
// fetches the sessions for each of tower. Sessions are only collected if they
// pass the sessionFilter check. If a tower has a session that does pass the
// sessionFilter check then the perActiveTower call-back will be called on that
// tower.
func getTowerAndSessionCandidates(db DB, keyRing ECDHKeyRing,
	perActiveTower func(tower *Tower),
	opts ...wtdb.ClientSessionListOption) (
	map[wtdb.SessionID]*ClientSession, error) {

	// Fetch all active towers from the DB.
	towers, err := db.ListTowers(func(tower *wtdb.Tower) bool {
		return tower.Status == wtdb.TowerStatusActive
	})
	if err != nil {
		return nil, err
	}

	candidateSessions := make(map[wtdb.SessionID]*ClientSession)
	for _, dbTower := range towers {
		tower, err := NewTowerFromDBTower(dbTower)
		if err != nil {
			return nil, err
		}

		sessions, err := db.ListClientSessions(&tower.ID, opts...)
		if err != nil {
			return nil, err
		}

		for _, s := range sessions {
			cs, err := NewClientSessionFromDBSession(
				s, tower, keyRing,
			)
			if err != nil {
				return nil, err
			}

			// Add the session to the set of candidate sessions.
			candidateSessions[s.ID] = cs
		}

		perActiveTower(tower)
	}

	return candidateSessions, nil
}

// getClientSessions retrieves the client sessions for a particular tower if
// specified, otherwise all client sessions for all towers are retrieved. An
// optional filter can be provided to filter out any undesired client sessions.
//
// NOTE: This method should only be used when deserialization of a
// ClientSession's SessionPrivKey field is desired, otherwise, the existing
// ListClientSessions method should be used.
func getClientSessions(db DB, keyRing ECDHKeyRing, forTower *wtdb.TowerID,
	opts ...wtdb.ClientSessionListOption) (
	map[wtdb.SessionID]*ClientSession, error) {

	dbSessions, err := db.ListClientSessions(forTower, opts...)
	if err != nil {
		return nil, err
	}

	// Reload the tower from disk using the tower ID contained in each
	// candidate session. We will also rederive any session keys needed to
	// be able to communicate with the towers and authenticate session
	// requests. This prevents us from having to store the private keys on
	// disk.
	sessions := make(map[wtdb.SessionID]*ClientSession)
	for _, s := range dbSessions {
		dbTower, err := db.LoadTowerByID(s.TowerID)
		if err != nil {
			return nil, err
		}

		towerKeyDesc, err := keyRing.DeriveKey(keychain.KeyLocator{
			Family: keychain.KeyFamilyTowerSession,
			Index:  s.KeyIndex,
		})
		if err != nil {
			return nil, err
		}

		sessionKeyECDH := keychain.NewPubKeyECDH(towerKeyDesc, keyRing)

		tower, err := NewTowerFromDBTower(dbTower)
		if err != nil {
			return nil, err
		}

		sessions[s.ID] = &ClientSession{
			ID:                s.ID,
			ClientSessionBody: s.ClientSessionBody,
			Tower:             tower,
			SessionKeyECDH:    sessionKeyECDH,
		}
	}

	return sessions, nil
}

// start initializes the watchtower client by loading or negotiating an active
// session and then begins processing backup tasks from the request pipeline.
func (c *client) start() error {
	c.log.Infof("Watchtower client starting")

	// First, restart a session queue for any sessions that have
	// committed but unacked state updates. This ensures that these
	// sessions will be able to flush the committed updates after a
	// restart.
	fetchCommittedUpdates := c.cfg.DB.FetchSessionCommittedUpdates
	for _, session := range c.candidateSessions {
		committedUpdates, err := fetchCommittedUpdates(
			&session.ID,
		)
		if err != nil {
			return err
		}

		if len(committedUpdates) > 0 {
			c.log.Infof("Starting session=%s to process "+
				"%d committed backups", session.ID,
				len(committedUpdates))

			c.initActiveQueue(session, committedUpdates)
		}
	}

	// Now start the session negotiator, which will allow us to request new
	// session as soon as the backupDispatcher starts up.
	err := c.negotiator.Start()
	if err != nil {
		return err
	}

	// Start the task pipeline to which new backup tasks will be
	// submitted from active links.
	err = c.pipeline.Start()
	if err != nil {
		return err
	}

	c.wg.Add(1)
	go c.backupDispatcher()

	c.log.Infof("Watchtower client started successfully")

	return nil
}

// stop idempotently initiates a graceful shutdown of the watchtower client.
func (c *client) stop() error {
	var returnErr error
	c.log.Debugf("Stopping watchtower client")

	// 1. Stop the session negotiator.
	err := c.negotiator.Stop()
	if err != nil {
		returnErr = err
	}

	// 2. Stop the backup dispatcher and any other goroutines.
	close(c.quit)
	c.wg.Wait()

	// 3. If there was a left over 'prevTask' from the backup
	// dispatcher, replay that onto the pipeline.
	if c.prevTask != nil {
		err = c.pipeline.QueueBackupID(c.prevTask)
		if err != nil {
			returnErr = err
		}
	}

	// 4. Shutdown all active session queues in parallel. These will
	// exit once all unhandled updates have been replayed to the
	// task pipeline.
	c.activeSessions.ApplyAndWait(func(s *sessionQueue) func() {
		return func() {
			err := s.Stop(false)
			if err != nil {
				c.log.Errorf("could not stop session "+
					"queue: %s: %v", s.ID(), err)

				returnErr = err
			}
		}
	})

	// 5. Shutdown the backup queue, which will prevent any further
	// updates from being accepted.
	if err = c.pipeline.Stop(); err != nil {
		returnErr = err
	}

	c.log.Debugf("Client successfully stopped, stats: %s", c.stats)

	return returnErr
}

// backupState initiates a request to back up a particular revoked state. If the
// method returns nil, the backup is guaranteed to be successful unless the:
//   - justice transaction would create dust outputs when trying to abide by the
//     negotiated policy, or
//   - breached outputs contain too little value to sweep at the target sweep
//     fee rate.
func (c *client) backupState(chanID *lnwire.ChannelID,
	stateNum uint64) error {

	id := &wtdb.BackupID{
		ChanID:       *chanID,
		CommitHeight: stateNum,
	}

	return c.pipeline.QueueBackupID(id)
}

// nextSessionQueue attempts to fetch an active session from our set of
// candidate sessions. Candidate sessions with a differing policy from the
// active client's advertised policy will be ignored, but may be resumed if the
// client is restarted with a matching policy. If no candidates were found, nil
// is returned to signal that we need to request a new policy.
func (c *client) nextSessionQueue() (*sessionQueue, error) {
	// Select any candidate session at random, and remove it from the set of
	// candidate sessions.
	var candidateSession *ClientSession
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
		return nil, nil
	}

	updates, err := c.cfg.DB.FetchSessionCommittedUpdates(
		&candidateSession.ID,
	)
	if err != nil {
		return nil, err
	}

	// Initialize the session queue and spin it up, so it can begin handling
	// updates. If the queue was already made active on startup, this will
	// simply return the existing session queue from the set.
	return c.getOrInitActiveQueue(candidateSession, updates), nil
}

// stopAndRemoveSession stops the session with the given ID and removes it from
// the in-memory active sessions set.
func (c *client) stopAndRemoveSession(id wtdb.SessionID, final bool) error {
	return c.activeSessions.StopAndRemove(id, final)
}

// deleteSessionFromTower dials the tower that we created the session with and
// attempts to send the tower the DeleteSession message.
func (c *client) deleteSessionFromTower(sess *wtdb.ClientSession) error {
	// First, we check if we have already loaded this tower in our
	// candidate towers iterator.
	tower, err := c.candidateTowers.GetTower(sess.TowerID)
	if errors.Is(err, ErrTowerNotInIterator) {
		// If not, then we attempt to load it from the DB.
		dbTower, err := c.cfg.DB.LoadTowerByID(sess.TowerID)
		if err != nil {
			return err
		}

		tower, err = NewTowerFromDBTower(dbTower)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	session, err := NewClientSessionFromDBSession(
		sess, tower, c.cfg.SecretKeyRing,
	)
	if err != nil {
		return err
	}

	localInit := wtwire.NewInitMessage(
		lnwire.NewRawFeatureVector(wtwire.AltruistSessionsRequired),
		c.cfg.ChainHash,
	)

	var (
		conn wtserver.Peer

		// addrIterator is a copy of the tower's address iterator.
		// We use this copy so that iterating through the addresses does
		// not affect any other threads using this iterator.
		addrIterator = tower.Addresses.Copy()
		towerAddr    = addrIterator.Peek()
	)
	// Attempt to dial the tower with its available addresses.
	for {
		conn, err = c.dial(
			session.SessionKeyECDH, &lnwire.NetAddress{
				IdentityKey: tower.IdentityKey,
				Address:     towerAddr,
			},
		)
		if err != nil {
			// If there are more addrs available, immediately try
			// those.
			nextAddr, iteratorErr := addrIterator.Next()
			if iteratorErr == nil {
				towerAddr = nextAddr
				continue
			}

			// Otherwise, if we have exhausted the address list,
			// exit.
			addrIterator.Reset()

			return fmt.Errorf("failed to dial tower(%x) at any "+
				"available addresses",
				tower.IdentityKey.SerializeCompressed())
		}

		break
	}
	defer conn.Close()

	// Send Init to tower.
	err = c.sendMessage(conn, localInit)
	if err != nil {
		return err
	}

	// Receive Init from tower.
	remoteMsg, err := c.readMessage(conn)
	if err != nil {
		return err
	}

	remoteInit, ok := remoteMsg.(*wtwire.Init)
	if !ok {
		return fmt.Errorf("watchtower %s responded with %T to Init",
			towerAddr, remoteMsg)
	}

	// Validate Init.
	err = localInit.CheckRemoteInit(remoteInit, wtwire.FeatureNames)
	if err != nil {
		return err
	}

	// Send DeleteSession to tower.
	err = c.sendMessage(conn, &wtwire.DeleteSession{})
	if err != nil {
		return err
	}

	// Receive DeleteSessionReply from tower.
	remoteMsg, err = c.readMessage(conn)
	if err != nil {
		return err
	}

	deleteSessionReply, ok := remoteMsg.(*wtwire.DeleteSessionReply)
	if !ok {
		return fmt.Errorf("watchtower %s responded with %T to "+
			"DeleteSession", towerAddr, remoteMsg)
	}

	switch deleteSessionReply.Code {
	case wtwire.CodeOK, wtwire.DeleteSessionCodeNotFound:
		return nil
	default:
		return fmt.Errorf("received error code %v in "+
			"DeleteSessionReply when attempting to delete "+
			"session from tower", deleteSessionReply.Code)
	}
}

// backupDispatcher processes events coming from the taskPipeline and is
// responsible for detecting when the client needs to renegotiate a session to
// fulfill continuing demand. The event loop exits if the client is quit.
//
// NOTE: This method MUST be run as a goroutine.
func (c *client) backupDispatcher() {
	defer c.wg.Done()

	c.log.Tracef("Starting backup dispatcher")
	defer c.log.Tracef("Stopping backup dispatcher")

	for {
		switch {

		// No active session queue and no additional sessions.
		case c.sessionQueue == nil && len(c.candidateSessions) == 0:
			c.log.Infof("Requesting new session.")

			// Immediately request a new session.
			c.negotiator.RequestSession()

			// Wait until we receive the newly negotiated session.
			// All backups sent in the meantime are queued in the
			// revoke queue, as we cannot process them.
		awaitSession:
			select {
			case session := <-c.negotiator.NewSessions():
				c.log.Infof("Acquired new session with id=%s",
					session.ID)
				c.candidateSessions[session.ID] = session
				c.stats.sessionAcquired()

				// We'll continue to choose the newly negotiated
				// session as our active session queue.
				continue

			case <-c.statTicker.C:
				c.log.Infof("Client stats: %s", c.stats)

			// A new tower has been requested to be added. We'll
			// update our persisted and in-memory state and consider
			// its corresponding sessions, if any, as new
			// candidates.
			case msg := <-c.newTowers:
				msg.errChan <- c.handleNewTower(msg.tower)

			// A tower has been requested to be removed. We'll
			// only allow removal of it if the address in question
			// is not currently being used for session negotiation.
			case msg := <-c.staleTowers:
				msg.errChan <- c.handleStaleTower(msg)

			// A tower has been requested to be de-activated. We'll
			// only allow this if the tower is not currently being
			// used for session negotiation.
			case msg := <-c.deactivateTowers:
				msg.errChan <- c.handleDeactivateTower(msg)

			// A request has come through to terminate a session.
			case msg := <-c.terminateSessions:
				msg.errChan <- c.handleTerminateSession(msg)

			case <-c.quit:
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
			var err error
			c.sessionQueue, err = c.nextSessionQueue()
			if err != nil {
				c.log.Errorf("error fetching next session "+
					"queue: %v", err)
			}

			if c.sessionQueue != nil {
				c.log.Debugf("Loaded next candidate session "+
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
				c.log.Warnf("Acquired new session with id=%s "+
					"while processing tasks", session.ID)
				c.candidateSessions[session.ID] = session
				c.stats.sessionAcquired()

			case <-c.statTicker.C:
				c.log.Infof("Client stats: %s", c.stats)

			// Process each backup task serially from the queue of
			// revoked states.
			case task, ok := <-c.pipeline.NextBackupID():
				// All backups in the pipeline have been
				// processed, it is now safe to exit.
				if !ok {
					return
				}

				c.log.Debugf("Processing %v", task)

				c.stats.taskReceived()
				c.processTask(task)

			// A new tower has been requested to be added. We'll
			// update our persisted and in-memory state and consider
			// its corresponding sessions, if any, as new
			// candidates.
			case msg := <-c.newTowers:
				msg.errChan <- c.handleNewTower(msg.tower)

			// A tower has been removed, so we'll remove certain
			// information that's persisted and also in our
			// in-memory state depending on the request, and set any
			// of its corresponding candidate sessions as inactive.
			case msg := <-c.staleTowers:
				msg.errChan <- c.handleStaleTower(msg)

			// A tower has been requested to be de-activated.
			case msg := <-c.deactivateTowers:
				msg.errChan <- c.handleDeactivateTower(msg)

			// A request has come through to terminate a session.
			case msg := <-c.terminateSessions:
				msg.errChan <- c.handleTerminateSession(msg)

			case <-c.quit:
				return
			}
		}
	}
}

// processTask attempts to schedule the given backupTask on the active
// sessionQueue. The task will either be accepted or rejected, after which the
// appropriate modifications to the client's state machine will be made. After
// every invocation of processTask, the caller should ensure that the
// sessionQueue hasn't been exhausted before proceeding to the next task. Tasks
// that are rejected because the active sessionQueue is full will be cached as
// the prevTask, and should be reprocessed after obtaining a new sessionQueue.
func (c *client) processTask(task *wtdb.BackupID) {
	script, ok := c.cfg.getSweepScript(task.ChanID)
	if !ok {
		log.Infof("not processing task for unregistered channel: %s",
			task.ChanID)

		return
	}

	backupTask := newBackupTask(*task, script)

	status, accepted := c.sessionQueue.AcceptTask(backupTask)
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
func (c *client) taskAccepted(task *wtdb.BackupID,
	newStatus sessionQueueStatus) {

	c.log.Infof("Queued %v successfully for session %v", task,
		c.sessionQueue.ID())

	c.stats.taskAccepted()

	// If this task was accepted, we discard anything held in the prevTask.
	// Either it was nil before, or is the task which was just accepted.
	c.prevTask = nil

	switch newStatus {

	// The sessionQueue still has capacity after accepting this task.
	case sessionQueueAvailable:

	// The sessionQueue is full after accepting this task, so we will need
	// to request a new one before proceeding.
	case sessionQueueExhausted:
		c.stats.sessionExhausted()

		c.log.Debugf("Session %s exhausted", c.sessionQueue.ID())

		// This task left the session exhausted, set it to nil and
		// proceed to the next loop, so we can consume another
		// pre-negotiated session or request another.
		c.sessionQueue = nil
	}
}

// taskRejected process the rejection of a task by a sessionQueue depending on
// the state the was in *before* the task was rejected. The client's prevTask
// will cache the task if the sessionQueue was exhausted beforehand, and nil
// the sessionQueue to find a new session. If the sessionQueue was not
// exhausted and not shutting down, the client marks the task as ineligible, as
// this implies we couldn't construct a valid justice transaction given the
// session's policy.
func (c *client) taskRejected(task *wtdb.BackupID,
	curStatus sessionQueueStatus) {

	switch curStatus {

	// The sessionQueue has available capacity but the task was rejected,
	// this indicates that the task was ineligible for backup.
	case sessionQueueAvailable:
		c.stats.taskIneligible()

		c.log.Infof("Ignoring ineligible %v", task)

		err := c.cfg.DB.MarkBackupIneligible(
			task.ChanID, task.CommitHeight,
		)
		if err != nil {
			c.log.Errorf("Unable to mark %v ineligible: %v",
				task, err)

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
	case sessionQueueExhausted:
		c.stats.sessionExhausted()

		c.log.Debugf("Session %v exhausted, %v queued for next session",
			c.sessionQueue.ID(), task)

		// Cache the task that we pulled off, so that we can process it
		// once a new session queue is available.
		c.sessionQueue = nil
		c.prevTask = task

	// The sessionQueue rejected the task because it is shutting down. We
	// will stash this task and try to add it to the next available
	// sessionQueue.
	case sessionQueueShuttingDown:
		c.log.Debugf("Session %v is shutting down, %v queued for "+
			"next session", c.sessionQueue.ID(), task)

		// Cache the task that we pulled off, so that we can process it
		// once a new session queue is available.
		c.sessionQueue = nil
		c.prevTask = task
	}
}

// dial connects the peer at addr using privKey as our secret key for the
// connection. The connection will use the configured Net's resolver to resolve
// the address for either Tor or clear net connections.
func (c *client) dial(localKey keychain.SingleKeyECDH,
	addr *lnwire.NetAddress) (wtserver.Peer, error) {

	return c.cfg.AuthDial(localKey, addr, c.cfg.Dial)
}

// readMessage receives and parses the next message from the given Peer. An
// error is returned if a message is not received before the server's read
// timeout, the read off the wire failed, or the message could not be
// deserialized.
func (c *client) readMessage(peer wtserver.Peer) (wtwire.Message, error) {
	// Set a read timeout to ensure we drop the connection if nothing is
	// received in a timely manner.
	err := peer.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout))
	if err != nil {
		err = fmt.Errorf("unable to set read deadline: %w", err)
		c.log.Errorf("Unable to read msg: %v", err)
		return nil, err
	}

	// Pull the next message off the wire,
	rawMsg, err := peer.ReadNextMessage()
	if err != nil {
		err = fmt.Errorf("unable to read message: %w", err)
		c.log.Errorf("Unable to read msg: %v", err)
		return nil, err
	}

	// Parse the received message according to the watchtower wire
	// specification.
	msgReader := bytes.NewReader(rawMsg)
	msg, err := wtwire.ReadMessage(msgReader, 0)
	if err != nil {
		err = fmt.Errorf("unable to parse message: %w", err)
		c.log.Errorf("Unable to read msg: %v", err)
		return nil, err
	}

	c.logMessage(peer, msg, true)

	return msg, nil
}

// sendMessage sends a watchtower wire message to the target peer.
func (c *client) sendMessage(peer wtserver.Peer,
	msg wtwire.Message) error {

	// Encode the next wire message into the buffer.
	// TODO(conner): use buffer pool
	var b bytes.Buffer
	_, err := wtwire.WriteMessage(&b, msg, 0)
	if err != nil {
		err = fmt.Errorf("unable to encode msg: %w", err)
		c.log.Errorf("Unable to send msg: %v", err)
		return err
	}

	// Set the write deadline for the connection, ensuring we drop the
	// connection if nothing is sent in a timely manner.
	err = peer.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout))
	if err != nil {
		err = fmt.Errorf("unable to set write deadline: %w", err)
		c.log.Errorf("Unable to send msg: %v", err)
		return err
	}

	c.logMessage(peer, msg, false)

	// Write out the full message to the remote peer.
	_, err = peer.Write(b.Bytes())
	if err != nil {
		c.log.Errorf("Unable to send msg: %v", err)
	}
	return err
}

// newSessionQueue creates a sessionQueue from a ClientSession loaded from the
// database and supplying it with the resources needed by the client.
func (c *client) newSessionQueue(s *ClientSession,
	updates []wtdb.CommittedUpdate) *sessionQueue {

	return newSessionQueue(&sessionQueueConfig{
		ClientSession:          s,
		ChainHash:              c.cfg.ChainHash,
		Dial:                   c.dial,
		ReadMessage:            c.readMessage,
		SendMessage:            c.sendMessage,
		Signer:                 c.cfg.Signer,
		DB:                     c.cfg.DB,
		MinBackoff:             c.cfg.MinBackoff,
		MaxBackoff:             c.cfg.MaxBackoff,
		Log:                    c.log,
		BuildBreachRetribution: c.cfg.BuildBreachRetribution,
		TaskPipeline:           c.pipeline,
	}, updates)
}

// getOrInitActiveQueue checks the activeSessions set for a sessionQueue for the
// passed ClientSession. If it exists, the active sessionQueue is returned.
// Otherwise, a new sessionQueue is initialized and added to the set.
func (c *client) getOrInitActiveQueue(s *ClientSession,
	updates []wtdb.CommittedUpdate) *sessionQueue {

	if sq, ok := c.activeSessions.Get(s.ID); ok {
		return sq
	}

	return c.initActiveQueue(s, updates)
}

// initActiveQueue creates a new sessionQueue from the passed ClientSession,
// adds the sessionQueue to the activeSessions set, and starts the sessionQueue
// so that it can deliver any committed updates or begin accepting newly
// assigned tasks.
func (c *client) initActiveQueue(s *ClientSession,
	updates []wtdb.CommittedUpdate) *sessionQueue {

	// Initialize the session queue, providing it with all the resources it
	// requires from the client instance.
	sq := c.newSessionQueue(s, updates)

	// Add the session queue as an active session so that we remember to
	// stop it on shutdown. This method will also start the queue so that it
	// can be active in processing newly assigned tasks or to upload
	// previously committed updates.
	c.activeSessions.AddAndStart(sq)

	return sq
}

// terminateSession sets the given session's status to CSessionTerminal meaning
// that it will not be used again.
func (c *client) terminateSession(id wtdb.SessionID) error {
	errChan := make(chan error, 1)

	select {
	case c.terminateSessions <- &terminateSessMsg{
		id:      id,
		errChan: errChan,
	}:
	case <-c.pipeline.quit:
		return ErrClientExiting
	}

	select {
	case err := <-errChan:
		return err
	case <-c.pipeline.quit:
		return ErrClientExiting
	}
}

// handleTerminateSession handles a request to terminate a session. It will
// first shut down the session if it is part of the active session set, then
// it will ensure that the active session queue is set reset if it is using the
// session in question. Finally, the session's status in the DB will be updated.
func (c *client) handleTerminateSession(msg *terminateSessMsg) error {
	id := msg.id

	delete(c.candidateSessions, id)

	err := c.activeSessions.StopAndRemove(id, true)
	if err != nil {
		return fmt.Errorf("could not stop session %s: %w", id, err)
	}

	// If our active session queue corresponds to the session being
	// terminated, then we'll proceed to negotiate a new one.
	if c.sessionQueue != nil {
		if bytes.Equal(c.sessionQueue.ID()[:], id[:]) {
			c.sessionQueue = nil
		}
	}

	return nil
}

// deactivateTower sends a tower deactivation request to the backupDispatcher
// where it will be handled synchronously. The request should result in all the
// sessions that we have with the given tower being shutdown and removed from
// our in-memory set of active sessions.
func (c *client) deactivateTower(id wtdb.TowerID,
	pubKey *btcec.PublicKey) error {

	errChan := make(chan error, 1)

	select {
	case c.deactivateTowers <- &deactivateTowerMsg{
		id:      id,
		pubKey:  pubKey,
		errChan: errChan,
	}:
	case <-c.pipeline.quit:
		return ErrClientExiting
	}

	select {
	case err := <-errChan:
		return err
	case <-c.pipeline.quit:
		return ErrClientExiting
	}
}

// handleDeactivateTower handles a request to deactivate a tower. We will remove
// it from the in-memory candidate set, and we will also stop any active
// sessions we have with this tower.
func (c *client) handleDeactivateTower(msg *deactivateTowerMsg) error {
	// Remove the tower from our in-memory candidate set so that it is not
	// used for any new session negotiations.
	err := c.candidateTowers.RemoveCandidate(msg.id, nil)
	if err != nil {
		return err
	}

	pubKey := msg.pubKey.SerializeCompressed()
	sessions, err := c.cfg.DB.ListClientSessions(&msg.id)
	if err != nil {
		return fmt.Errorf("unable to retrieve sessions for tower %x: "+
			"%v", pubKey, err)
	}

	// Iterate over all the sessions we have for this tower and remove them
	// from our candidate set and also from our set of started, active
	// sessions.
	for sessionID := range sessions {
		delete(c.candidateSessions, sessionID)

		err = c.activeSessions.StopAndRemove(sessionID, false)
		if err != nil {
			return fmt.Errorf("could not stop session %s: %w",
				sessionID, err)
		}
	}

	// If our active session queue corresponds to the stale tower, we'll
	// proceed to negotiate a new one.
	if c.sessionQueue != nil {
		towerKey := c.sessionQueue.tower.IdentityKey

		if bytes.Equal(pubKey, towerKey.SerializeCompressed()) {
			c.sessionQueue = nil
		}
	}

	return nil
}

// addTower adds a new watchtower reachable at the given address and considers
// it for new sessions. If the watchtower already exists, then any new addresses
// included will be considered when dialing it for session negotiations and
// backups.
func (c *client) addTower(tower *Tower) error {
	errChan := make(chan error, 1)

	select {
	case c.newTowers <- &newTowerMsg{
		tower:   tower,
		errChan: errChan,
	}:
	case <-c.pipeline.quit:
		return ErrClientExiting
	}

	select {
	case err := <-errChan:
		return err
	case <-c.pipeline.quit:
		return ErrClientExiting
	}
}

// handleNewTower handles a request for a new tower to be added. If the tower
// already exists, then its corresponding sessions, if any, will be set
// considered as candidates.
func (c *client) handleNewTower(tower *Tower) error {
	c.candidateTowers.AddCandidate(tower)

	// Include all of its corresponding sessions to our set of candidates.
	sessions, err := getClientSessions(
		c.cfg.DB, c.cfg.SecretKeyRing, &tower.ID,
		wtdb.WithPreEvalFilterFn(c.genSessionFilter(true)),
		wtdb.WithPostEvalFilterFn(ExhaustedSessionFilter()),
	)
	if err != nil {
		return fmt.Errorf("unable to determine sessions for tower %x: "+
			"%v", tower.IdentityKey.SerializeCompressed(), err)
	}
	maps.Copy(c.candidateSessions, sessions)

	return nil
}

// removeTower removes a watchtower from being considered for future session
// negotiations and from being used for any subsequent backups until it's added
// again. If an address is provided, then this call only serves as a way of
// removing the address from the watchtower instead.
func (c *client) removeTower(id wtdb.TowerID, pubKey *btcec.PublicKey,
	addr net.Addr) error {

	errChan := make(chan error, 1)

	select {
	case c.staleTowers <- &staleTowerMsg{
		id:      id,
		pubKey:  pubKey,
		addr:    addr,
		errChan: errChan,
	}:
	case <-c.pipeline.quit:
		return ErrClientExiting
	}

	select {
	case err := <-errChan:
		return err
	case <-c.pipeline.quit:
		return ErrClientExiting
	}
}

// handleStaleTower handles a request for an existing tower to be removed. If
// none of the tower's sessions have pending updates, then they will become
// inactive and removed as candidates. If the active session queue corresponds
// to any of these sessions, a new one will be negotiated.
func (c *client) handleStaleTower(msg *staleTowerMsg) error {
	// We'll first update our in-memory state.
	err := c.candidateTowers.RemoveCandidate(msg.id, msg.addr)
	if err != nil {
		return err
	}

	// If an address was provided, then we're only meant to remove the
	// address from the tower.
	if msg.addr != nil {
		return nil
	}

	// Otherwise, the tower should no longer be used for future session
	// negotiations and backups.

	pubKey := msg.pubKey.SerializeCompressed()
	sessions, err := c.cfg.DB.ListClientSessions(&msg.id)
	if err != nil {
		return fmt.Errorf("unable to retrieve sessions for tower %x: "+
			"%v", pubKey, err)
	}
	for sessionID := range sessions {
		delete(c.candidateSessions, sessionID)

		// Shutdown the session so that any pending updates are
		// replayed back onto the main task pipeline.
		err = c.activeSessions.StopAndRemove(sessionID, true)
		if err != nil {
			c.log.Errorf("could not stop session %s: %w", sessionID,
				err)
		}
	}

	// If our active session queue corresponds to the stale tower, we'll
	// proceed to negotiate a new one.
	if c.sessionQueue != nil {
		towerKey := c.sessionQueue.tower.IdentityKey

		if bytes.Equal(pubKey, towerKey.SerializeCompressed()) {
			c.sessionQueue = nil
		}
	}

	return nil
}

// registeredTowers retrieves the list of watchtowers registered with the
// client.
func (c *client) registeredTowers(towers []*wtdb.Tower,
	opts ...wtdb.ClientSessionListOption) ([]*RegisteredTower, error) {

	// Generate a filter that will fetch all the client's sessions
	// regardless of if they are active or not.
	opts = append(opts, wtdb.WithPreEvalFilterFn(c.genSessionFilter(false)))

	clientSessions, err := c.cfg.DB.ListClientSessions(nil, opts...)
	if err != nil {
		return nil, err
	}

	// Construct a lookup map that coalesces all the sessions for a specific
	// watchtower.
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

// lookupTower retrieves the info of sessions held with the given tower handled
// by this client.
func (c *client) lookupTower(tower *wtdb.Tower,
	opts ...wtdb.ClientSessionListOption) (*RegisteredTower, error) {

	opts = append(opts, wtdb.WithPreEvalFilterFn(c.genSessionFilter(false)))

	towerSessions, err := c.cfg.DB.ListClientSessions(&tower.ID, opts...)
	if err != nil {
		return nil, err
	}

	return &RegisteredTower{
		Tower:                  tower,
		Sessions:               towerSessions,
		ActiveSessionCandidate: c.candidateTowers.IsActive(tower.ID),
	}, nil
}

// getStats returns the in-memory statistics of the client since startup.
func (c *client) getStats() ClientStats {
	return c.stats.getStatsCopy()
}

// policy returns the active client policy configuration.
func (c *client) policy() wtpolicy.Policy {
	return c.cfg.Policy
}

// logMessage writes information about a message received from a remote peer,
// using directional prepositions to signal whether the message was sent or
// received.
func (c *client) logMessage(
	peer wtserver.Peer, msg wtwire.Message, read bool) {

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

	c.log.Debugf("%s %s%v %s %x@%s", action, msg.MsgType(), summary,
		preposition, peer.RemotePub().SerializeCompressed(),
		peer.RemoteAddr())
}

func newRandomDelay(max uint32) (uint32, error) {
	var maxDelay big.Int
	maxDelay.SetUint64(uint64(max))

	randDelay, err := rand.Int(rand.Reader, &maxDelay)
	if err != nil {
		return 0, err
	}

	return uint32(randDelay.Uint64()), nil
}

package wtclient

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
)

// ClientManager is the primary interface used by the daemon to control a
// client's lifecycle and backup revoked states.
type ClientManager interface {
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

	// DeactivateTower sets the given tower's status to inactive so that it
	// is not considered for session negotiation. Its sessions will also not
	// be used while the tower is inactive.
	DeactivateTower(pubKey *btcec.PublicKey) error

	// TerminateSession sets the given session's status to CSessionTerminal
	// meaning that it will not be used again.
	TerminateSession(id wtdb.SessionID) error

	// Stats returns the in-memory statistics of the client since startup.
	Stats() ClientStats

	// Policy returns the active client policy configuration.
	Policy(blob.Type) (wtpolicy.Policy, error)

	// RegisteredTowers retrieves the list of watchtowers registered with
	// the client. It returns a set of registered towers per client policy
	// type.
	RegisteredTowers(opts ...wtdb.ClientSessionListOption) (
		map[blob.Type][]*RegisteredTower, error)

	// LookupTower retrieves a registered watchtower through its public key.
	LookupTower(*btcec.PublicKey, ...wtdb.ClientSessionListOption) (
		map[blob.Type]*RegisteredTower, error)

	// RegisterChannel persistently initializes any channel-dependent
	// parameters within the client. This should be called during link
	// startup to ensure that the client is able to support the link during
	// operation.
	RegisterChannel(lnwire.ChannelID, channeldb.ChannelType) error

	// BackupState initiates a request to back up a particular revoked
	// state. If the method returns nil, the backup is guaranteed to be
	// successful unless the justice transaction would create dust outputs
	// when trying to abide by the negotiated policy.
	BackupState(chanID *lnwire.ChannelID, stateNum uint64) error
}

// Config provides the client with access to the resources it requires to
// perform its duty. All nillable fields must be non-nil for the tower to be
// initialized properly.
type Config struct {
	// Signer provides access to the wallet so that the client can sign
	// justice transactions that spend from a remote party's commitment
	// transaction.
	Signer input.Signer

	// SubscribeChannelEvents can be used to subscribe to channel event
	// notifications.
	SubscribeChannelEvents func() (subscribe.Subscription, error)

	// FetchClosedChannel can be used to fetch the info about a closed
	// channel. If the channel is not found or not yet closed then
	// channeldb.ErrClosedChannelNotFound will be returned.
	FetchClosedChannel func(cid lnwire.ChannelID) (
		*channeldb.ChannelCloseSummary, error)

	// ChainNotifier can be used to subscribe to block notifications.
	ChainNotifier chainntnfs.ChainNotifier

	// BuildBreachRetribution is a function closure that allows the client
	// fetch the breach retribution info for a certain channel at a certain
	// revoked commitment height.
	BuildBreachRetribution BreachRetributionBuilder

	// NewAddress generates a new on-chain sweep pkscript.
	NewAddress func() ([]byte, error)

	// SecretKeyRing is used to derive the session keys used to communicate
	// with the tower. The client only stores the KeyLocators internally so
	// that we never store private keys on disk.
	SecretKeyRing ECDHKeyRing

	// Dial connects to an addr using the specified net and returns the
	// connection object.
	Dial tor.DialFunc

	// AuthDialer establishes a brontide connection over an onion or clear
	// network.
	AuthDial AuthDialer

	// DB provides access to the client's stable storage medium.
	DB DB

	// ChainHash identifies the chain that the client is on and for which
	// the tower must be watching to monitor for breaches.
	ChainHash chainhash.Hash

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

	// SessionCloseRange is the range over which we will generate a random
	// number of blocks to delay closing a session after its last channel
	// has been closed.
	SessionCloseRange uint32

	// MaxTasksInMemQueue is the maximum number of backup tasks that should
	// be kept in-memory. Any more tasks will overflow to disk.
	MaxTasksInMemQueue uint64
}

// Manager manages the various tower clients that are active. A client is
// required for each different commitment transaction type. The Manager acts as
// a tower client multiplexer.
type Manager struct {
	started sync.Once
	stopped sync.Once

	cfg *Config

	clients   map[blob.Type]*client
	clientsMu sync.Mutex

	backupMu     sync.Mutex
	chanInfos    wtdb.ChannelInfos
	chanBlobType map[lnwire.ChannelID]blob.Type

	closableSessionQueue *sessionCloseMinHeap

	wg   sync.WaitGroup
	quit chan struct{}
}

var _ ClientManager = (*Manager)(nil)

// NewManager constructs a new Manager.
func NewManager(config *Config, policies ...wtpolicy.Policy) (*Manager, error) {
	// Copy the config to prevent side effects from modifying both the
	// internal and external version of the Config.
	cfg := *config

	// Set the read timeout to the default if none was provided.
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = DefaultReadTimeout
	}

	// Set the write timeout to the default if none was provided.
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}

	chanInfos, err := cfg.DB.FetchChanInfos()
	if err != nil {
		return nil, err
	}

	m := &Manager{
		cfg:                  &cfg,
		clients:              make(map[blob.Type]*client),
		chanBlobType:         make(map[lnwire.ChannelID]blob.Type),
		chanInfos:            chanInfos,
		closableSessionQueue: newSessionCloseMinHeap(),
		quit:                 make(chan struct{}),
	}

	for _, policy := range policies {
		if err = policy.Validate(); err != nil {
			return nil, err
		}

		if err = m.newClient(policy); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// newClient constructs a new client and adds it to the set of clients that
// the Manager is keeping track of.
func (m *Manager) newClient(policy wtpolicy.Policy) error {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	_, ok := m.clients[policy.BlobType]
	if ok {
		return fmt.Errorf("a client with blob type %s has "+
			"already been registered", policy.BlobType)
	}

	cfg := &clientCfg{
		Config:         m.cfg,
		Policy:         policy,
		getSweepScript: m.getSweepScript,
	}

	client, err := newClient(cfg)
	if err != nil {
		return err
	}

	m.clients[policy.BlobType] = client

	return nil
}

// Start starts all the clients that have been registered with the Manager.
func (m *Manager) Start() error {
	var returnErr error
	m.started.Do(func() {
		chanSub, err := m.cfg.SubscribeChannelEvents()
		if err != nil {
			returnErr = err

			return
		}

		// Iterate over the list of registered channels and check if any
		// of them can be marked as closed.
		for id := range m.chanInfos {
			isClosed, closedHeight, err := m.isChannelClosed(id)
			if err != nil {
				returnErr = err

				return
			}

			if !isClosed {
				continue
			}

			_, err = m.cfg.DB.MarkChannelClosed(id, closedHeight)
			if err != nil {
				log.Errorf("could not mark channel(%s) as "+
					"closed: %v", id, err)

				continue
			}

			// Since the channel has been marked as closed, we can
			// also remove it from the channel summaries map.
			delete(m.chanInfos, id)
		}

		// Load all closable sessions.
		closableSessions, err := m.cfg.DB.ListClosableSessions()
		if err != nil {
			returnErr = err

			return
		}

		err = m.trackClosableSessions(closableSessions)
		if err != nil {
			returnErr = err

			return
		}

		m.wg.Add(1)
		go m.handleChannelCloses(chanSub)

		// Subscribe to new block events.
		blockEvents, err := m.cfg.ChainNotifier.RegisterBlockEpochNtfn(
			nil,
		)
		if err != nil {
			returnErr = err

			return
		}

		m.wg.Add(1)
		go m.handleClosableSessions(blockEvents)

		m.clientsMu.Lock()
		defer m.clientsMu.Unlock()

		for _, client := range m.clients {
			if err := client.start(); err != nil {
				returnErr = err
				return
			}
		}
	})

	return returnErr
}

// Stop stops all the clients that the Manger is managing.
func (m *Manager) Stop() error {
	var returnErr error
	m.stopped.Do(func() {
		m.clientsMu.Lock()
		defer m.clientsMu.Unlock()

		close(m.quit)
		m.wg.Wait()

		for _, client := range m.clients {
			if err := client.stop(); err != nil {
				returnErr = err
			}
		}
	})

	return returnErr
}

// AddTower adds a new watchtower reachable at the given address and considers
// it for new sessions. If the watchtower already exists, then any new addresses
// included will be considered when dialing it for session negotiations and
// backups.
func (m *Manager) AddTower(address *lnwire.NetAddress) error {
	// We'll start by updating our persisted state, followed by the
	// in-memory state of each client, with the new tower. This might not
	// actually be a new tower, but it might include a new address at which
	// it can be reached.
	dbTower, err := m.cfg.DB.CreateTower(address)
	if err != nil {
		return err
	}

	tower, err := NewTowerFromDBTower(dbTower)
	if err != nil {
		return err
	}

	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	for blobType, client := range m.clients {
		clientType, err := blobType.Identifier()
		if err != nil {
			return err
		}

		if err := client.addTower(tower); err != nil {
			return fmt.Errorf("could not add tower(%x) to the %s "+
				"tower client: %w",
				tower.IdentityKey.SerializeCompressed(),
				clientType, err)
		}
	}

	return nil
}

// RemoveTower removes a watchtower from being considered for future session
// negotiations and from being used for any subsequent backups until it's added
// again. If an address is provided, then this call only serves as a way of
// removing the address from the watchtower instead.
func (m *Manager) RemoveTower(key *btcec.PublicKey, addr net.Addr) error {
	// We'll load the tower before potentially removing it in order to
	// retrieve its ID within the database.
	dbTower, err := m.cfg.DB.LoadTower(key)
	if err != nil {
		return err
	}

	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	for _, client := range m.clients {
		err := client.removeTower(dbTower.ID, key, addr)
		if err != nil {
			return err
		}
	}

	if err := m.cfg.DB.RemoveTower(key, addr); err != nil {
		// If the persisted state update fails, re-add the address to
		// our client's in-memory state.
		tower, newTowerErr := NewTowerFromDBTower(dbTower)
		if newTowerErr != nil {
			log.Errorf("could not create new in-memory tower: %v",
				newTowerErr)

			return err
		}

		for _, client := range m.clients {
			addTowerErr := client.addTower(tower)
			if addTowerErr != nil {
				log.Errorf("could not re-add tower: %v",
					addTowerErr)
			}
		}

		return err
	}

	return nil
}

// TerminateSession sets the given session's status to CSessionTerminal meaning
// that it will not be used again.
func (m *Manager) TerminateSession(id wtdb.SessionID) error {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	for _, client := range m.clients {
		err := client.terminateSession(id)
		if err != nil {
			return err
		}
	}

	// Finally, mark the session as terminated in the DB.
	return m.cfg.DB.TerminateSession(id)
}

// DeactivateTower sets the given tower's status to inactive so that it is not
// considered for session negotiation. Its sessions will also not be used while
// the tower is inactive.
func (m *Manager) DeactivateTower(key *btcec.PublicKey) error {
	// We'll load the tower in order to retrieve its ID within the database.
	tower, err := m.cfg.DB.LoadTower(key)
	if err != nil {
		return err
	}

	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	for _, client := range m.clients {
		err := client.deactivateTower(tower.ID, tower.IdentityKey)
		if err != nil {
			return err
		}
	}

	// Finally, mark the tower as inactive in the DB.
	err = m.cfg.DB.DeactivateTower(key)
	if err != nil {
		log.Errorf("Could not deactivate the tower. Re-activating. %v",
			err)

		// If the persisted state update fails, re-add the address to
		// our client's in-memory state.
		tower, newTowerErr := NewTowerFromDBTower(tower)
		if newTowerErr != nil {
			log.Errorf("Could not create new in-memory tower: %v",
				newTowerErr)

			return err
		}

		for _, client := range m.clients {
			addTowerErr := client.addTower(tower)
			if addTowerErr != nil {
				log.Errorf("Could not re-add tower: %v",
					addTowerErr)
			}
		}

		return err
	}

	return nil
}

// Stats returns the in-memory statistics of the clients managed by the Manager
// since startup.
func (m *Manager) Stats() ClientStats {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	var resp ClientStats
	for _, client := range m.clients {
		stats := client.getStats()
		resp.NumTasksAccepted += stats.NumTasksAccepted
		resp.NumTasksIneligible += stats.NumTasksIneligible
		resp.NumTasksPending += stats.NumTasksPending
		resp.NumSessionsAcquired += stats.NumSessionsAcquired
		resp.NumSessionsExhausted += stats.NumSessionsExhausted
	}

	return resp
}

// RegisteredTowers retrieves the list of watchtowers being used by the various
// clients.
func (m *Manager) RegisteredTowers(opts ...wtdb.ClientSessionListOption) (
	map[blob.Type][]*RegisteredTower, error) {

	towers, err := m.cfg.DB.ListTowers(nil)
	if err != nil {
		return nil, err
	}

	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	resp := make(map[blob.Type][]*RegisteredTower)
	for _, client := range m.clients {
		towers, err := client.registeredTowers(towers, opts...)
		if err != nil {
			return nil, err
		}

		resp[client.policy().BlobType] = towers
	}

	return resp, nil
}

// LookupTower retrieves a registered watchtower through its public key.
func (m *Manager) LookupTower(key *btcec.PublicKey,
	opts ...wtdb.ClientSessionListOption) (map[blob.Type]*RegisteredTower,
	error) {

	tower, err := m.cfg.DB.LoadTower(key)
	if err != nil {
		return nil, err
	}

	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	resp := make(map[blob.Type]*RegisteredTower)
	for _, client := range m.clients {
		tower, err := client.lookupTower(tower, opts...)
		if err != nil {
			return nil, err
		}

		resp[client.policy().BlobType] = tower
	}

	return resp, nil
}

// Policy returns the active client policy configuration for the client using
// the given blob type.
func (m *Manager) Policy(blobType blob.Type) (wtpolicy.Policy, error) {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	var policy wtpolicy.Policy
	client, ok := m.clients[blobType]
	if !ok {
		return policy, fmt.Errorf("no client for the given blob type")
	}

	return client.policy(), nil
}

// RegisterChannel persistently initializes any channel-dependent parameters
// within the client. This should be called during link startup to ensure that
// the client is able to support the link during operation.
func (m *Manager) RegisterChannel(id lnwire.ChannelID,
	chanType channeldb.ChannelType) error {

	blobType := blob.TypeFromChannel(chanType)

	m.clientsMu.Lock()
	if _, ok := m.clients[blobType]; !ok {
		m.clientsMu.Unlock()

		return fmt.Errorf("no client registered for blob type %s",
			blobType)
	}
	m.clientsMu.Unlock()

	m.backupMu.Lock()
	defer m.backupMu.Unlock()

	// If a pkscript for this channel already exists, the channel has been
	// previously registered.
	if _, ok := m.chanInfos[id]; ok {
		// Keep track of which blob type this channel will use for
		// updates.
		m.chanBlobType[id] = blobType

		return nil
	}

	// Otherwise, generate a new sweep pkscript used to sweep funds for this
	// channel.
	pkScript, err := m.cfg.NewAddress()
	if err != nil {
		return err
	}

	// Persist the sweep pkscript so that restarts will not introduce
	// address inflation when the channel is reregistered after a restart.
	err = m.cfg.DB.RegisterChannel(id, pkScript)
	if err != nil {
		return err
	}

	// Finally, cache the pkscript in our in-memory cache to avoid db
	// lookups for the remainder of the daemon's execution.
	m.chanInfos[id] = &wtdb.ChannelInfo{
		ClientChanSummary: wtdb.ClientChanSummary{
			SweepPkScript: pkScript,
		},
	}

	// Keep track of which blob type this channel will use for updates.
	m.chanBlobType[id] = blobType

	return nil
}

// BackupState initiates a request to back up a particular revoked state. If the
// method returns nil, the backup is guaranteed to be successful unless the
// justice transaction would create dust outputs when trying to abide by the
// negotiated policy.
func (m *Manager) BackupState(chanID *lnwire.ChannelID, stateNum uint64) error {
	select {
	case <-m.quit:
		return ErrClientExiting
	default:
	}

	// Make sure that this channel is registered with the tower client.
	m.backupMu.Lock()
	info, ok := m.chanInfos[*chanID]
	if !ok {
		m.backupMu.Unlock()

		return ErrUnregisteredChannel
	}

	// Ignore backups that have already been presented to the client.
	var duplicate bool
	info.MaxHeight.WhenSome(func(maxHeight uint64) {
		if stateNum <= maxHeight {
			duplicate = true
		}
	})
	if duplicate {
		m.backupMu.Unlock()

		log.Debugf("Ignoring duplicate backup for chanid=%v at "+
			"height=%d", chanID, stateNum)

		return nil
	}

	// This backup has a higher commit height than any known backup for this
	// channel. We'll update our tip so that we won't accept it again if the
	// link flaps.
	m.chanInfos[*chanID].MaxHeight = fn.Some(stateNum)

	blobType, ok := m.chanBlobType[*chanID]
	if !ok {
		m.backupMu.Unlock()

		return ErrUnregisteredChannel
	}
	m.backupMu.Unlock()

	m.clientsMu.Lock()
	client, ok := m.clients[blobType]
	if !ok {
		m.clientsMu.Unlock()

		return fmt.Errorf("no client registered for blob type %s",
			blobType)
	}
	m.clientsMu.Unlock()

	return client.backupState(chanID, stateNum)
}

// isChanClosed can be used to check if the channel with the given ID has been
// closed. If it has been, the block height in which its closing transaction was
// mined will also be returned.
func (m *Manager) isChannelClosed(id lnwire.ChannelID) (bool, uint32,
	error) {

	chanSum, err := m.cfg.FetchClosedChannel(id)
	if errors.Is(err, channeldb.ErrClosedChannelNotFound) {
		return false, 0, nil
	} else if err != nil {
		return false, 0, err
	}

	return true, chanSum.CloseHeight, nil
}

// trackClosableSessions takes in a map of session IDs to the earliest block
// height at which the session should be deleted. For each of the sessions,
// a random delay is added to the block height and the session is added to the
// closableSessionQueue.
func (m *Manager) trackClosableSessions(
	sessions map[wtdb.SessionID]uint32) error {

	// For each closable session, add a random delay to its close
	// height and add it to the closableSessionQueue.
	for sID, blockHeight := range sessions {
		delay, err := newRandomDelay(m.cfg.SessionCloseRange)
		if err != nil {
			return err
		}

		deleteHeight := blockHeight + delay

		m.closableSessionQueue.Push(&sessionCloseItem{
			sessionID:    sID,
			deleteHeight: deleteHeight,
		})
	}

	return nil
}

// handleChannelCloses listens for channel close events and marks channels as
// closed in the DB.
//
// NOTE: This method MUST be run as a goroutine.
func (m *Manager) handleChannelCloses(chanSub subscribe.Subscription) {
	defer m.wg.Done()

	log.Debugf("Starting channel close handler")
	defer log.Debugf("Stopping channel close handler")

	for {
		select {
		case update, ok := <-chanSub.Updates():
			if !ok {
				log.Debugf("Channel notifier has exited")
				return
			}

			// We only care about channel-close events.
			event, ok := update.(channelnotifier.ClosedChannelEvent)
			if !ok {
				continue
			}

			chanID := lnwire.NewChanIDFromOutPoint(
				event.CloseSummary.ChanPoint,
			)

			log.Debugf("Received ClosedChannelEvent for "+
				"channel: %s", chanID)

			err := m.handleClosedChannel(
				chanID, event.CloseSummary.CloseHeight,
			)
			if err != nil {
				log.Errorf("Could not handle channel close "+
					"event for channel(%s): %v", chanID,
					err)
			}

		case <-m.quit:
			return
		}
	}
}

// handleClosedChannel handles the closure of a single channel. It will mark the
// channel as closed in the DB, then it will handle all the sessions that are
// now closable due to the channel closure.
func (m *Manager) handleClosedChannel(chanID lnwire.ChannelID,
	closeHeight uint32) error {

	m.backupMu.Lock()
	defer m.backupMu.Unlock()

	// We only care about channels registered with the tower client.
	if _, ok := m.chanInfos[chanID]; !ok {
		return nil
	}

	log.Debugf("Marking channel(%s) as closed", chanID)

	sessions, err := m.cfg.DB.MarkChannelClosed(chanID, closeHeight)
	if err != nil {
		return fmt.Errorf("could not mark channel(%s) as closed: %w",
			chanID, err)
	}

	closableSessions := make(map[wtdb.SessionID]uint32, len(sessions))
	for _, sess := range sessions {
		closableSessions[sess] = closeHeight
	}

	log.Debugf("Tracking %d new closable sessions as a result of "+
		"closing channel %s", len(closableSessions), chanID)

	err = m.trackClosableSessions(closableSessions)
	if err != nil {
		return fmt.Errorf("could not track closable sessions: %w", err)
	}

	delete(m.chanInfos, chanID)

	return nil
}

// handleClosableSessions listens for new block notifications. For each block,
// it checks the closableSessionQueue to see if there is a closable session with
// a delete-height smaller than or equal to the new block, if there is then the
// tower is informed that it can delete the session, and then we also delete it
// from our DB.
func (m *Manager) handleClosableSessions(
	blocksChan *chainntnfs.BlockEpochEvent) {

	defer m.wg.Done()

	log.Debug("Starting closable sessions handler")
	defer log.Debug("Stopping closable sessions handler")

	for {
		select {
		case newBlock := <-blocksChan.Epochs:
			if newBlock == nil {
				return
			}

			height := uint32(newBlock.Height)
			for {
				select {
				case <-m.quit:
					return
				default:
				}

				// If there are no closable sessions that we
				// need to handle, then we are done and can
				// reevaluate when the next block comes.
				item := m.closableSessionQueue.Top()
				if item == nil {
					break
				}

				// If there is closable session but the delete
				// height we have set for it is after the
				// current block height, then our work is done.
				if item.deleteHeight > height {
					break
				}

				// Otherwise, we pop this item from the heap
				// and handle it.
				m.closableSessionQueue.Pop()

				// Fetch the session from the DB so that we can
				// extract the Tower info.
				sess, err := m.cfg.DB.GetClientSession(
					item.sessionID,
				)
				if err != nil {
					log.Errorf("error calling "+
						"GetClientSession for "+
						"session %s: %v",
						item.sessionID, err)

					continue
				}

				// get appropriate client.
				m.clientsMu.Lock()
				client, ok := m.clients[sess.Policy.BlobType]
				if !ok {
					m.clientsMu.Unlock()
					log.Errorf("no client currently " +
						"active for the session type")

					continue
				}
				m.clientsMu.Unlock()

				clientName, err := client.policy().BlobType.
					Identifier()
				if err != nil {
					log.Errorf("could not get client "+
						"identifier: %v", err)

					continue
				}

				// Stop the session and remove it from the
				// in-memory set.
				err = client.stopAndRemoveSession(
					item.sessionID, true,
				)
				if err != nil {
					log.Errorf("could not remove "+
						"session(%s) from in-memory "+
						"set of the %s client: %v",
						item.sessionID, clientName, err)

					continue
				}

				err = client.deleteSessionFromTower(sess)
				if err != nil {
					log.Errorf("error deleting "+
						"session %s from tower: %v",
						sess.ID, err)

					continue
				}

				err = m.cfg.DB.DeleteSession(item.sessionID)
				if err != nil {
					log.Errorf("could not delete "+
						"session(%s) from DB: %w",
						sess.ID, err)

					continue
				}
			}

		case <-m.quit:
			return
		}
	}
}

func (m *Manager) getSweepScript(id lnwire.ChannelID) ([]byte, bool) {
	m.backupMu.Lock()
	defer m.backupMu.Unlock()

	summary, ok := m.chanInfos[id]
	if !ok {
		return nil, false
	}

	return summary.SweepPkScript, true
}

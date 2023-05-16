package wtclient

import (
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
)

// TowerClientManager is the primary interface used by the daemon to control a
// client's lifecycle and backup revoked states.
type TowerClientManager interface {
	// AddTower adds a new watchtower reachable at the given address and
	// considers it for new sessions. If the watchtower already exists, then
	// any new addresses included will be considered when dialing it for
	// session negotiations and backups.
	AddTower(*lnwire.NetAddress) error
}

// Config provides the TowerClient with access to the resources it requires to
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

	clients   map[blob.Type]*TowerClient
	clientsMu sync.Mutex
}

var _ TowerClientManager = (*Manager)(nil)

// NewManager constructs a new Manager.
func NewManager(config *Config) (*Manager, error) {
	// Copy the config to prevent side effects from modifying both the
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

	return &Manager{
		cfg:     cfg,
		clients: make(map[blob.Type]*TowerClient),
	}, nil
}

// NewClient constructs a new TowerClient and adds it to the set of clients that
// the Manager is keeping track of.
func (m *Manager) NewClient(policy wtpolicy.Policy) (*TowerClient, error) {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()

	_, ok := m.clients[policy.BlobType]
	if ok {
		return nil, fmt.Errorf("a client with blob type %s has "+
			"already been registered", policy.BlobType)
	}

	cfg := &towerClientCfg{
		Config: m.cfg,
		Policy: policy,
	}

	client, err := newTowerClient(cfg)
	if err != nil {
		return nil, err
	}

	m.clients[policy.BlobType] = client

	return client, nil
}

// Start starts all the clients that have been registered with the Manager.
func (m *Manager) Start() error {
	var returnErr error
	m.started.Do(func() {
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

	for _, client := range m.clients {
		if err := client.addTower(tower); err != nil {
			return err
		}
	}

	return nil
}

package watchtower

import (
	"net"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/watchtower/lookout"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
)

// Standalone encapsulates the server-side functionality required by watchtower
// clients. A Standalone couples the two primary subsystems such that, as a
// unit, this instance can negotiate sessions with clients, accept state updates
// for active sessions, monitor the chain for breaches matching known breach
// hints, publish reconstructed justice transactions on behalf of tower clients.
type Standalone struct {
	started uint32 // to be used atomically
	stopped uint32 // to be used atomically

	cfg *Config

	// server is the client endpoint, used for negotiating sessions and
	// uploading state updates.
	server wtserver.Interface

	// lookout is a service that monitors the chain and inspects the
	// transactions found in new blocks against the state updates received
	// by the server.
	lookout lookout.Service
}

// New validates the passed Config and returns a fresh Standalone instance if
// the tower's subsystems could be properly initialized.
func New(cfg *Config) (*Standalone, error) {
	// The tower must have listening address in order to accept new updates
	// from clients.
	if len(cfg.ListenAddrs) == 0 {
		return nil, ErrNoListeners
	}

	// Assign the default read timeout if none is provided.
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = DefaultReadTimeout
	}

	// Assign the default write timeout if none is provided.
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}

	punisher := lookout.NewBreachPunisher(&lookout.PunisherConfig{
		PublishTx: cfg.PublishTx,
	})

	// Initialize the lookout service with its required resources.
	lookout := lookout.New(&lookout.Config{
		BlockFetcher:   cfg.BlockFetcher,
		DB:             cfg.DB,
		EpochRegistrar: cfg.EpochRegistrar,
		Punisher:       punisher,
	})

	// Create a brontide listener on each of the provided listening
	// addresses. Client should be able to connect to any of open ports to
	// communicate with this Standalone instance.
	listeners := make([]net.Listener, 0, len(cfg.ListenAddrs))
	for _, listenAddr := range cfg.ListenAddrs {
		listener, err := brontide.NewListener(
			cfg.NodePrivKey, listenAddr.String(),
		)
		if err != nil {
			return nil, err
		}

		listeners = append(listeners, listener)
	}

	// Initialize the server with its required resources.
	server, err := wtserver.New(&wtserver.Config{
		ChainHash:    cfg.ChainHash,
		DB:           cfg.DB,
		NodePrivKey:  cfg.NodePrivKey,
		Listeners:    listeners,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		NewAddress:   cfg.NewAddress,
	})
	if err != nil {
		return nil, err
	}

	return &Standalone{
		cfg:     cfg,
		server:  server,
		lookout: lookout,
	}, nil
}

// Start idempotently starts the Standalone, an error is returned if the
// subsystems could not be initialized.
func (w *Standalone) Start() error {
	if !atomic.CompareAndSwapUint32(&w.started, 0, 1) {
		return nil
	}

	log.Infof("Starting watchtower")

	if err := w.lookout.Start(); err != nil {
		return err
	}
	if err := w.server.Start(); err != nil {
		w.lookout.Stop()
		return err
	}

	log.Infof("Watchtower started successfully")

	return nil
}

// Stop idempotently stops the Standalone and blocks until the subsystems have
// completed their shutdown.
func (w *Standalone) Stop() error {
	if !atomic.CompareAndSwapUint32(&w.stopped, 0, 1) {
		return nil
	}

	log.Infof("Stopping watchtower")

	w.server.Stop()
	w.lookout.Stop()

	log.Infof("Watchtower stopped successfully")

	return nil
}

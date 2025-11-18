package lnd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"math/big"
	prand "math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/chanfitness"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/cluster"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/graph"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/healthcheck"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/lnwallet/rpcwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/nat"
	"github.com/lightningnetwork/lnd/netann"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/peer"
	"github.com/lightningnetwork/lnd/peernotifier"
	"github.com/lightningnetwork/lnd/pool"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/localchans"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
)

const (
	// defaultMinPeers is the minimum number of peers nodes should always be
	// connected to.
	defaultMinPeers = 3

	// defaultStableConnDuration is a floor under which all reconnection
	// attempts will apply exponential randomized backoff. Connections
	// durations exceeding this value will be eligible to have their
	// backoffs reduced.
	defaultStableConnDuration = 10 * time.Minute

	// numInstantInitReconnect specifies how many persistent peers we should
	// always attempt outbound connections to immediately. After this value
	// is surpassed, the remaining peers will be randomly delayed using
	// maxInitReconnectDelay.
	numInstantInitReconnect = 10

	// maxInitReconnectDelay specifies the maximum delay in seconds we will
	// apply in attempting to reconnect to persistent peers on startup. The
	// value used or a particular peer will be chosen between 0s and this
	// value.
	maxInitReconnectDelay = 30

	// multiAddrConnectionStagger is the number of seconds to wait between
	// attempting to a peer with each of its advertised addresses.
	multiAddrConnectionStagger = 10 * time.Second
)

var (
	// ErrPeerNotConnected signals that the server has no connection to the
	// given peer.
	ErrPeerNotConnected = errors.New("peer is not connected")

	// ErrServerNotActive indicates that the server has started but hasn't
	// fully finished the startup process.
	ErrServerNotActive = errors.New("server is still in the process of " +
		"starting")

	// ErrServerShuttingDown indicates that the server is in the process of
	// gracefully exiting.
	ErrServerShuttingDown = errors.New("server is shutting down")

	// MaxFundingAmount is a soft-limit of the maximum channel size
	// currently accepted within the Lightning Protocol. This is
	// defined in BOLT-0002, and serves as an initial precautionary limit
	// while implementations are battle tested in the real world.
	//
	// At the moment, this value depends on which chain is active. It is set
	// to the value under the Bitcoin chain as default.
	//
	// TODO(roasbeef): add command line param to modify.
	MaxFundingAmount = funding.MaxBtcFundingAmount

	// EndorsementExperimentEnd is the time after which nodes should stop
	// propagating experimental endorsement signals.
	//
	// Per blip04: January 1, 2026 12:00:00 AM UTC in unix seconds.
	EndorsementExperimentEnd = time.Unix(1767225600, 0)

	// ErrGossiperBan is one of the errors that can be returned when we
	// attempt to finalize a connection to a remote peer.
	ErrGossiperBan = errors.New("gossiper has banned remote's key")

	// ErrNoMoreRestrictedAccessSlots is one of the errors that can be
	// returned when we attempt to finalize a connection. It means that
	// this peer has no pending-open, open, or closed channels with us and
	// are already at our connection ceiling for a peer with this access
	// status.
	ErrNoMoreRestrictedAccessSlots = errors.New("no more restricted slots")

	// ErrNoPeerScore is returned when we expect to find a score in
	// peerScores, but one does not exist.
	ErrNoPeerScore = errors.New("peer score not found")

	// ErrNoPendingPeerInfo is returned when we couldn't find any pending
	// peer info.
	ErrNoPendingPeerInfo = errors.New("no pending peer info")
)

// errPeerAlreadyConnected is an error returned by the server when we're
// commanded to connect to a peer, but they're already connected.
type errPeerAlreadyConnected struct {
	peer *peer.Brontide
}

// Error returns the human readable version of this error type.
//
// NOTE: Part of the error interface.
func (e *errPeerAlreadyConnected) Error() string {
	return fmt.Sprintf("already connected to peer: %v", e.peer)
}

// peerAccessStatus denotes the p2p access status of a given peer. This will be
// used to assign peer ban scores that determine an action the server will
// take.
type peerAccessStatus int

const (
	// peerStatusRestricted indicates that the peer only has access to the
	// limited number of "free" reserved slots.
	peerStatusRestricted peerAccessStatus = iota

	// peerStatusTemporary indicates that the peer only has temporary p2p
	// access to the server.
	peerStatusTemporary

	// peerStatusProtected indicates that the peer has been granted
	// permanent p2p access to the server. The peer can still have its
	// access revoked.
	peerStatusProtected
)

// String returns a human-readable representation of the status code.
func (p peerAccessStatus) String() string {
	switch p {
	case peerStatusRestricted:
		return "restricted"

	case peerStatusTemporary:
		return "temporary"

	case peerStatusProtected:
		return "protected"

	default:
		return "unknown"
	}
}

// peerSlotStatus determines whether a peer gets access to one of our free
// slots or gets to bypass this safety mechanism.
type peerSlotStatus struct {
	// state determines which privileges the peer has with our server.
	state peerAccessStatus
}

// server is the main server of the Lightning Network Daemon. The server houses
// global state pertaining to the wallet, database, and the rpcserver.
// Additionally, the server is also used as a central messaging bus to interact
// with any of its companion objects.
type server struct {
	active   int32 // atomic
	stopping int32 // atomic

	start sync.Once
	stop  sync.Once

	cfg *Config

	implCfg *ImplementationCfg

	// identityECDH is an ECDH capable wrapper for the private key used
	// to authenticate any incoming connections.
	identityECDH keychain.SingleKeyECDH

	// identityKeyLoc is the key locator for the above wrapped identity key.
	identityKeyLoc keychain.KeyLocator

	// nodeSigner is an implementation of the MessageSigner implementation
	// that's backed by the identity private key of the running lnd node.
	nodeSigner *netann.NodeSigner

	chanStatusMgr *netann.ChanStatusManager

	// listenAddrs is the list of addresses the server is currently
	// listening on.
	listenAddrs []net.Addr

	// torController is a client that will communicate with a locally
	// running Tor server. This client will handle initiating and
	// authenticating the connection to the Tor server, automatically
	// creating and setting up onion services, etc.
	torController *tor.Controller

	// natTraversal is the specific NAT traversal technique used to
	// automatically set up port forwarding rules in order to advertise to
	// the network that the node is accepting inbound connections.
	natTraversal nat.Traversal

	// lastDetectedIP is the last IP detected by the NAT traversal technique
	// above. This IP will be watched periodically in a goroutine in order
	// to handle dynamic IP changes.
	lastDetectedIP net.IP

	mu sync.RWMutex

	// peersByPub is a map of the active peers.
	//
	// NOTE: The key used here is the raw bytes of the peer's public key to
	// string conversion, which means it cannot be printed using `%s` as it
	// will just print the binary.
	//
	// TODO(yy): Use the hex string instead.
	peersByPub map[string]*peer.Brontide

	inboundPeers  map[string]*peer.Brontide
	outboundPeers map[string]*peer.Brontide

	peerConnectedListeners    map[string][]chan<- lnpeer.Peer
	peerDisconnectedListeners map[string][]chan<- struct{}

	// TODO(yy): the Brontide.Start doesn't know this value, which means it
	// will continue to send messages even if there are no active channels
	// and the value below is false. Once it's pruned, all its connections
	// will be closed, thus the Brontide.Start will return an error.
	persistentPeers        map[string]bool
	persistentPeersBackoff map[string]time.Duration
	persistentPeerAddrs    map[string][]*lnwire.NetAddress
	persistentConnReqs     map[string][]*connmgr.ConnReq
	persistentRetryCancels map[string]chan struct{}

	// peerErrors keeps a set of peer error buffers for peers that have
	// disconnected from us. This allows us to track historic peer errors
	// over connections. The string of the peer's compressed pubkey is used
	// as a key for this map.
	peerErrors map[string]*queue.CircularBuffer

	// ignorePeerTermination tracks peers for which the server has initiated
	// a disconnect. Adding a peer to this map causes the peer termination
	// watcher to short circuit in the event that peers are purposefully
	// disconnected.
	ignorePeerTermination map[*peer.Brontide]struct{}

	// scheduledPeerConnection maps a pubkey string to a callback that
	// should be executed in the peerTerminationWatcher the prior peer with
	// the same pubkey exits.  This allows the server to wait until the
	// prior peer has cleaned up successfully, before adding the new peer
	// intended to replace it.
	scheduledPeerConnection map[string]func()

	// pongBuf is a shared pong reply buffer we'll use across all active
	// peer goroutines. We know the max size of a pong message
	// (lnwire.MaxPongBytes), so we can allocate this ahead of time, and
	// avoid allocations each time we need to send a pong message.
	pongBuf []byte

	cc *chainreg.ChainControl

	fundingMgr *funding.Manager

	graphDB *graphdb.ChannelGraph

	chanStateDB *channeldb.ChannelStateDB

	addrSource channeldb.AddrSource

	// miscDB is the DB that contains all "other" databases within the main
	// channel DB that haven't been separated out yet.
	miscDB *channeldb.DB

	invoicesDB invoices.InvoiceDB

	// paymentsDB is the DB that contains all functions for managing
	// payments.
	paymentsDB paymentsdb.DB

	aliasMgr *aliasmgr.Manager

	htlcSwitch *htlcswitch.Switch

	interceptableSwitch *htlcswitch.InterceptableSwitch

	invoices *invoices.InvoiceRegistry

	invoiceHtlcModifier *invoices.HtlcModificationInterceptor

	channelNotifier *channelnotifier.ChannelNotifier

	peerNotifier *peernotifier.PeerNotifier

	htlcNotifier *htlcswitch.HtlcNotifier

	witnessBeacon contractcourt.WitnessBeacon

	breachArbitrator *contractcourt.BreachArbitrator

	missionController *routing.MissionController
	defaultMC         *routing.MissionControl

	graphBuilder *graph.Builder

	chanRouter *routing.ChannelRouter

	controlTower routing.ControlTower

	authGossiper *discovery.AuthenticatedGossiper

	localChanMgr *localchans.Manager

	utxoNursery *contractcourt.UtxoNursery

	sweeper *sweep.UtxoSweeper

	chainArb *contractcourt.ChainArbitrator

	sphinx *hop.OnionProcessor

	towerClientMgr *wtclient.Manager

	connMgr *connmgr.ConnManager

	sigPool *lnwallet.SigPool

	writePool *pool.Write

	readPool *pool.Read

	tlsManager *TLSManager

	// featureMgr dispatches feature vectors for various contexts within the
	// daemon.
	featureMgr *feature.Manager

	// currentNodeAnn is the node announcement that has been broadcast to
	// the network upon startup, if the attributes of the node (us) has
	// changed since last start.
	currentNodeAnn *lnwire.NodeAnnouncement1

	// chansToRestore is the set of channels that upon starting, the server
	// should attempt to restore/recover.
	chansToRestore walletunlocker.ChannelsToRecover

	// chanSubSwapper is a sub-system that will ensure our on-disk channel
	// backups are consistent at all times. It interacts with the
	// channelNotifier to be notified of newly opened and closed channels.
	chanSubSwapper *chanbackup.SubSwapper

	// chanEventStore tracks the behaviour of channels and their remote peers to
	// provide insights into their health and performance.
	chanEventStore *chanfitness.ChannelEventStore

	hostAnn *netann.HostAnnouncer

	// livenessMonitor monitors that lnd has access to critical resources.
	livenessMonitor *healthcheck.Monitor

	customMessageServer *subscribe.Server

	onionMessageServer *subscribe.Server

	// txPublisher is a publisher with fee-bumping capability.
	txPublisher *sweep.TxPublisher

	// blockbeatDispatcher is a block dispatcher that notifies subscribers
	// of new blocks.
	blockbeatDispatcher *chainio.BlockbeatDispatcher

	// peerAccessMan implements peer access controls.
	peerAccessMan *accessMan

	quit chan struct{}

	wg sync.WaitGroup
}

// updatePersistentPeerAddrs subscribes to topology changes and stores
// advertised addresses for any NodeAnnouncements from our persisted peers.
func (s *server) updatePersistentPeerAddrs() error {
	graphSub, err := s.graphDB.SubscribeTopology()
	if err != nil {
		return err
	}

	s.wg.Add(1)
	go func() {
		defer func() {
			graphSub.Cancel()
			s.wg.Done()
		}()

		for {
			select {
			case <-s.quit:
				return

			case topChange, ok := <-graphSub.TopologyChanges:
				// If the router is shutting down, then we will
				// as well.
				if !ok {
					return
				}

				for _, update := range topChange.NodeUpdates {
					pubKeyStr := string(
						update.IdentityKey.
							SerializeCompressed(),
					)

					// We only care about updates from
					// our persistentPeers.
					s.mu.RLock()
					_, ok := s.persistentPeers[pubKeyStr]
					s.mu.RUnlock()
					if !ok {
						continue
					}

					addrs := make([]*lnwire.NetAddress, 0,
						len(update.Addresses))

					for _, addr := range update.Addresses {
						addrs = append(addrs,
							&lnwire.NetAddress{
								IdentityKey: update.IdentityKey,
								Address:     addr,
								ChainNet:    s.cfg.ActiveNetParams.Net,
							},
						)
					}

					s.mu.Lock()

					// Update the stored addresses for this
					// to peer to reflect the new set.
					s.persistentPeerAddrs[pubKeyStr] = addrs

					// If there are no outstanding
					// connection requests for this peer
					// then our work is done since we are
					// not currently trying to connect to
					// them.
					if len(s.persistentConnReqs[pubKeyStr]) == 0 {
						s.mu.Unlock()
						continue
					}

					s.mu.Unlock()

					s.connectToPersistentPeer(pubKeyStr)
				}
			}
		}
	}()

	return nil
}

// CustomMessage is a custom message that is received from a peer.
type CustomMessage struct {
	// Peer is the peer pubkey
	Peer [33]byte

	// Msg is the custom wire message.
	Msg *lnwire.Custom
}

// parseAddr parses an address from its string format to a net.Addr.
func parseAddr(address string, netCfg tor.Net) (net.Addr, error) {
	var (
		host string
		port int
	)

	// Split the address into its host and port components.
	h, p, err := net.SplitHostPort(address)
	if err != nil {
		// If a port wasn't specified, we'll assume the address only
		// contains the host so we'll use the default port.
		host = address
		port = defaultPeerPort
	} else {
		// Otherwise, we'll note both the host and ports.
		host = h
		portNum, err := strconv.Atoi(p)
		if err != nil {
			return nil, err
		}
		port = portNum
	}

	if tor.IsOnionHost(host) {
		return &tor.OnionAddr{OnionService: host, Port: port}, nil
	}

	// If the host is part of a TCP address, we'll use the network
	// specific ResolveTCPAddr function in order to resolve these
	// addresses over Tor in order to prevent leaking your real IP
	// address.
	hostPort := net.JoinHostPort(host, strconv.Itoa(port))
	return netCfg.ResolveTCPAddr("tcp", hostPort)
}

// noiseDial is a factory function which creates a connmgr compliant dialing
// function by returning a closure which includes the server's identity key.
func noiseDial(idKey keychain.SingleKeyECDH,
	netCfg tor.Net, timeout time.Duration) func(net.Addr) (net.Conn, error) {

	return func(a net.Addr) (net.Conn, error) {
		lnAddr := a.(*lnwire.NetAddress)
		return brontide.Dial(idKey, lnAddr, timeout, netCfg.Dial)
	}
}

// newServer creates a new instance of the server which is to listen using the
// passed listener address.
//
//nolint:funlen
func newServer(ctx context.Context, cfg *Config, listenAddrs []net.Addr,
	dbs *DatabaseInstances, cc *chainreg.ChainControl,
	nodeKeyDesc *keychain.KeyDescriptor,
	chansToRestore walletunlocker.ChannelsToRecover,
	chanPredicate chanacceptor.ChannelAcceptor,
	torController *tor.Controller, tlsManager *TLSManager,
	leaderElector cluster.LeaderElector,
	implCfg *ImplementationCfg) (*server, error) {

	var (
		err         error
		nodeKeyECDH = keychain.NewPubKeyECDH(*nodeKeyDesc, cc.KeyRing)

		// We just derived the full descriptor, so we know the public
		// key is set on it.
		nodeKeySigner = keychain.NewPubKeyMessageSigner(
			nodeKeyDesc.PubKey, nodeKeyDesc.KeyLocator, cc.KeyRing,
		)
	)

	netParams := cfg.ActiveNetParams.Params

	// Initialize the sphinx router.
	replayLog := htlcswitch.NewDecayedLog(
		dbs.DecayedLogDB, cc.ChainNotifier,
	)
	sphinxRouter := sphinx.NewRouter(nodeKeyECDH, replayLog)

	writeBufferPool := pool.NewWriteBuffer(
		pool.DefaultWriteBufferGCInterval,
		pool.DefaultWriteBufferExpiryInterval,
	)

	writePool := pool.NewWrite(
		writeBufferPool, cfg.Workers.Write, pool.DefaultWorkerTimeout,
	)

	readBufferPool := pool.NewReadBuffer(
		pool.DefaultReadBufferGCInterval,
		pool.DefaultReadBufferExpiryInterval,
	)

	readPool := pool.NewRead(
		readBufferPool, cfg.Workers.Read, pool.DefaultWorkerTimeout,
	)

	// If the taproot overlay flag is set, but we don't have an aux funding
	// controller, then we'll exit as this is incompatible.
	if cfg.ProtocolOptions.TaprootOverlayChans &&
		implCfg.AuxFundingController.IsNone() {

		return nil, fmt.Errorf("taproot overlay flag set, but " +
			"overlay channels are not supported " +
			"in a standalone lnd build")
	}

	//nolint:ll
	featureMgr, err := feature.NewManager(feature.Config{
		NoTLVOnion:                cfg.ProtocolOptions.LegacyOnion(),
		NoStaticRemoteKey:         cfg.ProtocolOptions.NoStaticRemoteKey(),
		NoAnchors:                 cfg.ProtocolOptions.NoAnchorCommitments(),
		NoWumbo:                   !cfg.ProtocolOptions.Wumbo(),
		NoScriptEnforcementLease:  cfg.ProtocolOptions.NoScriptEnforcementLease(),
		NoKeysend:                 !cfg.AcceptKeySend,
		NoOptionScidAlias:         !cfg.ProtocolOptions.ScidAlias(),
		NoZeroConf:                !cfg.ProtocolOptions.ZeroConf(),
		NoAnySegwit:               cfg.ProtocolOptions.NoAnySegwit(),
		CustomFeatures:            cfg.ProtocolOptions.CustomFeatures(),
		NoTaprootChans:            !cfg.ProtocolOptions.TaprootChans,
		NoTaprootOverlay:          !cfg.ProtocolOptions.TaprootOverlayChans,
		NoRouteBlinding:           cfg.ProtocolOptions.NoRouteBlinding(),
		NoExperimentalEndorsement: cfg.ProtocolOptions.NoExperimentalEndorsement(),
		NoQuiescence:              cfg.ProtocolOptions.NoQuiescence(),
		NoRbfCoopClose:            !cfg.ProtocolOptions.RbfCoopClose,
	})
	if err != nil {
		return nil, err
	}

	invoiceHtlcModifier := invoices.NewHtlcModificationInterceptor()
	registryConfig := invoices.RegistryConfig{
		FinalCltvRejectDelta:        lncfg.DefaultFinalCltvRejectDelta,
		HtlcHoldDuration:            invoices.DefaultHtlcHoldDuration,
		Clock:                       clock.NewDefaultClock(),
		AcceptKeySend:               cfg.AcceptKeySend,
		AcceptAMP:                   cfg.AcceptAMP,
		GcCanceledInvoicesOnStartup: cfg.GcCanceledInvoicesOnStartup,
		GcCanceledInvoicesOnTheFly:  cfg.GcCanceledInvoicesOnTheFly,
		KeysendHoldTime:             cfg.KeysendHoldTime,
		HtlcInterceptor:             invoiceHtlcModifier,
	}

	addrSource := channeldb.NewMultiAddrSource(dbs.ChanStateDB, dbs.GraphDB)

	s := &server{
		cfg:            cfg,
		implCfg:        implCfg,
		graphDB:        dbs.GraphDB,
		chanStateDB:    dbs.ChanStateDB.ChannelStateDB(),
		addrSource:     addrSource,
		miscDB:         dbs.ChanStateDB,
		invoicesDB:     dbs.InvoiceDB,
		paymentsDB:     dbs.PaymentsDB,
		cc:             cc,
		sigPool:        lnwallet.NewSigPool(cfg.Workers.Sig, cc.Signer),
		writePool:      writePool,
		readPool:       readPool,
		chansToRestore: chansToRestore,

		blockbeatDispatcher: chainio.NewBlockbeatDispatcher(
			cc.ChainNotifier,
		),
		channelNotifier: channelnotifier.New(
			dbs.ChanStateDB.ChannelStateDB(),
		),

		identityECDH:   nodeKeyECDH,
		identityKeyLoc: nodeKeyDesc.KeyLocator,
		nodeSigner:     netann.NewNodeSigner(nodeKeySigner),

		listenAddrs: listenAddrs,

		// TODO(roasbeef): derive proper onion key based on rotation
		// schedule
		sphinx: hop.NewOnionProcessor(sphinxRouter),

		torController: torController,

		persistentPeers:         make(map[string]bool),
		persistentPeersBackoff:  make(map[string]time.Duration),
		persistentConnReqs:      make(map[string][]*connmgr.ConnReq),
		persistentPeerAddrs:     make(map[string][]*lnwire.NetAddress),
		persistentRetryCancels:  make(map[string]chan struct{}),
		peerErrors:              make(map[string]*queue.CircularBuffer),
		ignorePeerTermination:   make(map[*peer.Brontide]struct{}),
		scheduledPeerConnection: make(map[string]func()),
		pongBuf:                 make([]byte, lnwire.MaxPongBytes),

		peersByPub:                make(map[string]*peer.Brontide),
		inboundPeers:              make(map[string]*peer.Brontide),
		outboundPeers:             make(map[string]*peer.Brontide),
		peerConnectedListeners:    make(map[string][]chan<- lnpeer.Peer),
		peerDisconnectedListeners: make(map[string][]chan<- struct{}),

		invoiceHtlcModifier: invoiceHtlcModifier,

		customMessageServer: subscribe.NewServer(),

		onionMessageServer: subscribe.NewServer(),

		tlsManager: tlsManager,

		featureMgr: featureMgr,
		quit:       make(chan struct{}),
	}

	currentHash, currentHeight, err := s.cc.ChainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}

	expiryWatcher := invoices.NewInvoiceExpiryWatcher(
		clock.NewDefaultClock(), cfg.Invoices.HoldExpiryDelta,
		uint32(currentHeight), currentHash, cc.ChainNotifier,
	)
	s.invoices = invoices.NewRegistry(
		dbs.InvoiceDB, expiryWatcher, &registryConfig,
	)

	s.htlcNotifier = htlcswitch.NewHtlcNotifier(time.Now)

	thresholdSats := btcutil.Amount(cfg.MaxFeeExposure)
	thresholdMSats := lnwire.NewMSatFromSatoshis(thresholdSats)

	linkUpdater := func(shortID lnwire.ShortChannelID) error {
		link, err := s.htlcSwitch.GetLinkByShortID(shortID)
		if err != nil {
			return err
		}

		s.htlcSwitch.UpdateLinkAliases(link)

		return nil
	}

	s.aliasMgr, err = aliasmgr.NewManager(dbs.ChanStateDB, linkUpdater)
	if err != nil {
		return nil, err
	}

	s.htlcSwitch, err = htlcswitch.New(htlcswitch.Config{
		DB:                   dbs.ChanStateDB,
		FetchAllOpenChannels: s.chanStateDB.FetchAllOpenChannels,
		FetchAllChannels:     s.chanStateDB.FetchAllChannels,
		FetchClosedChannels:  s.chanStateDB.FetchClosedChannels,
		LocalChannelClose: func(pubKey []byte,
			request *htlcswitch.ChanClose) {

			peer, err := s.FindPeerByPubStr(string(pubKey))
			if err != nil {
				srvrLog.Errorf("unable to close channel, peer"+
					" with %v id can't be found: %v",
					pubKey, err,
				)
				return
			}

			peer.HandleLocalCloseChanReqs(request)
		},
		FwdingLog:              dbs.ChanStateDB.ForwardingLog(),
		SwitchPackager:         channeldb.NewSwitchPackager(),
		ExtractErrorEncrypter:  s.sphinx.ExtractErrorEncrypter,
		FetchLastChannelUpdate: s.fetchLastChanUpdate(),
		Notifier:               s.cc.ChainNotifier,
		HtlcNotifier:           s.htlcNotifier,
		FwdEventTicker:         ticker.New(htlcswitch.DefaultFwdEventInterval),
		LogEventTicker:         ticker.New(htlcswitch.DefaultLogInterval),
		AckEventTicker:         ticker.New(htlcswitch.DefaultAckInterval),
		AllowCircularRoute:     cfg.AllowCircularRoute,
		RejectHTLC:             cfg.RejectHTLC,
		Clock:                  clock.NewDefaultClock(),
		MailboxDeliveryTimeout: cfg.Htlcswitch.MailboxDeliveryTimeout,
		MaxFeeExposure:         thresholdMSats,
		SignAliasUpdate:        s.signAliasUpdate,
		IsAlias:                aliasmgr.IsAlias,
	}, uint32(currentHeight))
	if err != nil {
		return nil, err
	}
	s.interceptableSwitch, err = htlcswitch.NewInterceptableSwitch(
		&htlcswitch.InterceptableSwitchConfig{
			Switch:             s.htlcSwitch,
			CltvRejectDelta:    lncfg.DefaultFinalCltvRejectDelta,
			CltvInterceptDelta: lncfg.DefaultCltvInterceptDelta,
			RequireInterceptor: s.cfg.RequireInterceptor,
			Notifier:           s.cc.ChainNotifier,
		},
	)
	if err != nil {
		return nil, err
	}

	s.witnessBeacon = newPreimageBeacon(
		dbs.ChanStateDB.NewWitnessCache(),
		s.interceptableSwitch.ForwardPacket,
	)

	chanStatusMgrCfg := &netann.ChanStatusConfig{
		ChanStatusSampleInterval: cfg.ChanStatusSampleInterval,
		ChanEnableTimeout:        cfg.ChanEnableTimeout,
		ChanDisableTimeout:       cfg.ChanDisableTimeout,
		OurPubKey:                nodeKeyDesc.PubKey,
		OurKeyLoc:                nodeKeyDesc.KeyLocator,
		MessageSigner:            s.nodeSigner,
		IsChannelActive:          s.htlcSwitch.HasActiveLink,
		ApplyChannelUpdate:       s.applyChannelUpdate,
		DB:                       s.chanStateDB,
		Graph:                    dbs.GraphDB,
	}

	chanStatusMgr, err := netann.NewChanStatusManager(chanStatusMgrCfg)
	if err != nil {
		return nil, err
	}
	s.chanStatusMgr = chanStatusMgr

	// If enabled, use either UPnP or NAT-PMP to automatically configure
	// port forwarding for users behind a NAT.
	if cfg.NAT {
		srvrLog.Info("Scanning local network for a UPnP enabled device")

		discoveryTimeout := time.Duration(10 * time.Second)

		ctx, cancel := context.WithTimeout(
			context.Background(), discoveryTimeout,
		)
		defer cancel()
		upnp, err := nat.DiscoverUPnP(ctx)
		if err == nil {
			s.natTraversal = upnp
		} else {
			// If we were not able to discover a UPnP enabled device
			// on the local network, we'll fall back to attempting
			// to discover a NAT-PMP enabled device.
			srvrLog.Errorf("Unable to discover a UPnP enabled "+
				"device on the local network: %v", err)

			srvrLog.Info("Scanning local network for a NAT-PMP " +
				"enabled device")

			pmp, err := nat.DiscoverPMP(discoveryTimeout)
			if err != nil {
				err := fmt.Errorf("unable to discover a "+
					"NAT-PMP enabled device on the local "+
					"network: %v", err)
				srvrLog.Error(err)
				return nil, err
			}

			s.natTraversal = pmp
		}
	}

	nodePubKey := route.NewVertex(nodeKeyDesc.PubKey)
	// Set the self node which represents our node in the graph.
	err = s.setSelfNode(ctx, nodePubKey, listenAddrs)
	if err != nil {
		return nil, err
	}

	// The router will get access to the payment ID sequencer, such that it
	// can generate unique payment IDs.
	sequencer, err := htlcswitch.NewPersistentSequencer(dbs.ChanStateDB)
	if err != nil {
		return nil, err
	}

	// Instantiate mission control with config from the sub server.
	//
	// TODO(joostjager): When we are further in the process of moving to sub
	// servers, the mission control instance itself can be moved there too.
	routingConfig := routerrpc.GetRoutingConfig(cfg.SubRPCServers.RouterRPC)

	// We only initialize a probability estimator if there's no custom one.
	var estimator routing.Estimator
	if cfg.Estimator != nil {
		estimator = cfg.Estimator
	} else {
		switch routingConfig.ProbabilityEstimatorType {
		case routing.AprioriEstimatorName:
			aCfg := routingConfig.AprioriConfig
			aprioriConfig := routing.AprioriConfig{
				AprioriHopProbability: aCfg.HopProbability,
				PenaltyHalfLife:       aCfg.PenaltyHalfLife,
				AprioriWeight:         aCfg.Weight,
				CapacityFraction:      aCfg.CapacityFraction,
			}

			estimator, err = routing.NewAprioriEstimator(
				aprioriConfig,
			)
			if err != nil {
				return nil, err
			}

		case routing.BimodalEstimatorName:
			bCfg := routingConfig.BimodalConfig
			bimodalConfig := routing.BimodalConfig{
				BimodalNodeWeight: bCfg.NodeWeight,
				BimodalScaleMsat: lnwire.MilliSatoshi(
					bCfg.Scale,
				),
				BimodalDecayTime: bCfg.DecayTime,
			}

			estimator, err = routing.NewBimodalEstimator(
				bimodalConfig,
			)
			if err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("unknown estimator type %v",
				routingConfig.ProbabilityEstimatorType)
		}
	}

	mcCfg := &routing.MissionControlConfig{
		OnConfigUpdate:          fn.Some(s.UpdateRoutingConfig),
		Estimator:               estimator,
		MaxMcHistory:            routingConfig.MaxMcHistory,
		McFlushInterval:         routingConfig.McFlushInterval,
		MinFailureRelaxInterval: routing.DefaultMinFailureRelaxInterval,
	}

	s.missionController, err = routing.NewMissionController(
		dbs.ChanStateDB, nodePubKey, mcCfg,
	)
	if err != nil {
		return nil, fmt.Errorf("can't create mission control "+
			"manager: %w", err)
	}
	s.defaultMC, err = s.missionController.GetNamespacedStore(
		routing.DefaultMissionControlNamespace,
	)
	if err != nil {
		return nil, fmt.Errorf("can't create mission control in the "+
			"default namespace: %w", err)
	}

	srvrLog.Debugf("Instantiating payment session source with config: "+
		"AttemptCost=%v + %v%%, MinRouteProbability=%v",
		int64(routingConfig.AttemptCost),
		float64(routingConfig.AttemptCostPPM)/10000,
		routingConfig.MinRouteProbability)

	pathFindingConfig := routing.PathFindingConfig{
		AttemptCost: lnwire.NewMSatFromSatoshis(
			routingConfig.AttemptCost,
		),
		AttemptCostPPM: routingConfig.AttemptCostPPM,
		MinProbability: routingConfig.MinRouteProbability,
	}

	sourceNode, err := dbs.GraphDB.SourceNode(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting source node: %w", err)
	}
	paymentSessionSource := &routing.SessionSource{
		GraphSessionFactory: dbs.GraphDB,
		SourceNode:          sourceNode,
		MissionControl:      s.defaultMC,
		GetLink:             s.htlcSwitch.GetLinkByShortID,
		PathFindingConfig:   pathFindingConfig,
	}

	s.controlTower = routing.NewControlTower(dbs.PaymentsDB)

	strictPruning := cfg.Bitcoin.Node == "neutrino" ||
		cfg.Routing.StrictZombiePruning

	s.graphBuilder, err = graph.NewBuilder(&graph.Config{
		SelfNode:            nodePubKey,
		Graph:               dbs.GraphDB,
		Chain:               cc.ChainIO,
		ChainView:           cc.ChainView,
		Notifier:            cc.ChainNotifier,
		ChannelPruneExpiry:  graph.DefaultChannelPruneExpiry,
		GraphPruneInterval:  time.Hour,
		FirstTimePruneDelay: graph.DefaultFirstTimePruneDelay,
		AssumeChannelValid:  cfg.Routing.AssumeChannelValid,
		StrictZombiePruning: strictPruning,
		IsAlias:             aliasmgr.IsAlias,
	})
	if err != nil {
		return nil, fmt.Errorf("can't create graph builder: %w", err)
	}

	s.chanRouter, err = routing.New(routing.Config{
		SelfNode:                  nodePubKey,
		RoutingGraph:              dbs.GraphDB,
		Chain:                     cc.ChainIO,
		Payer:                     s.htlcSwitch,
		Control:                   s.controlTower,
		MissionControl:            s.defaultMC,
		SessionSource:             paymentSessionSource,
		GetLink:                   s.htlcSwitch.GetLinkByShortID,
		NextPaymentID:             sequencer.NextID,
		PathFindingConfig:         pathFindingConfig,
		Clock:                     clock.NewDefaultClock(),
		ApplyChannelUpdate:        s.graphBuilder.ApplyChannelUpdate,
		ClosedSCIDs:               s.fetchClosedChannelSCIDs(),
		TrafficShaper:             implCfg.TrafficShaper,
		KeepFailedPaymentAttempts: cfg.KeepFailedPaymentAttempts,
	})
	if err != nil {
		return nil, fmt.Errorf("can't create router: %w", err)
	}

	chanSeries := discovery.NewChanSeries(s.graphDB)
	gossipMessageStore, err := discovery.NewMessageStore(dbs.ChanStateDB)
	if err != nil {
		return nil, err
	}
	waitingProofStore, err := channeldb.NewWaitingProofStore(dbs.ChanStateDB)
	if err != nil {
		return nil, err
	}

	scidCloserMan := discovery.NewScidCloserMan(s.graphDB, s.chanStateDB)

	s.authGossiper = discovery.New(discovery.Config{
		Graph:                 s.graphBuilder,
		ChainIO:               s.cc.ChainIO,
		Notifier:              s.cc.ChainNotifier,
		ChainParams:           s.cfg.ActiveNetParams.Params,
		Broadcast:             s.BroadcastMessage,
		ChanSeries:            chanSeries,
		NotifyWhenOnline:      s.NotifyWhenOnline,
		NotifyWhenOffline:     s.NotifyWhenOffline,
		FetchSelfAnnouncement: s.getNodeAnnouncement,
		UpdateSelfAnnouncement: func() (lnwire.NodeAnnouncement1,
			error) {

			return s.genNodeAnnouncement(nil)
		},
		ProofMatureDelta:        cfg.Gossip.AnnouncementConf,
		TrickleDelay:            time.Millisecond * time.Duration(cfg.TrickleDelay),
		RetransmitTicker:        ticker.New(time.Minute * 30),
		RebroadcastInterval:     time.Hour * 24,
		WaitingProofStore:       waitingProofStore,
		MessageStore:            gossipMessageStore,
		AnnSigner:               s.nodeSigner,
		RotateTicker:            ticker.New(discovery.DefaultSyncerRotationInterval),
		HistoricalSyncTicker:    ticker.New(cfg.HistoricalSyncInterval),
		NumActiveSyncers:        cfg.NumGraphSyncPeers,
		NoTimestampQueries:      cfg.ProtocolOptions.NoTimestampQueryOption, //nolint:ll
		MinimumBatchSize:        10,
		SubBatchDelay:           cfg.Gossip.SubBatchDelay,
		IgnoreHistoricalFilters: cfg.IgnoreHistoricalGossipFilters,
		PinnedSyncers:           cfg.Gossip.PinnedSyncers,
		MaxChannelUpdateBurst:   cfg.Gossip.MaxChannelUpdateBurst,
		ChannelUpdateInterval:   cfg.Gossip.ChannelUpdateInterval,
		IsAlias:                 aliasmgr.IsAlias,
		SignAliasUpdate:         s.signAliasUpdate,
		FindBaseByAlias:         s.aliasMgr.FindBaseSCID,
		GetAlias:                s.aliasMgr.GetPeerAlias,
		FindChannel:             s.findChannel,
		IsStillZombieChannel:    s.graphBuilder.IsZombieChannel,
		ScidCloser:              scidCloserMan,
		AssumeChannelValid:      cfg.Routing.AssumeChannelValid,
		MsgRateBytes:            cfg.Gossip.MsgRateBytes,
		MsgBurstBytes:           cfg.Gossip.MsgBurstBytes,
		FilterConcurrency:       cfg.Gossip.FilterConcurrency,
		BanThreshold:            cfg.Gossip.BanThreshold,
		PeerMsgRateBytes:        cfg.Gossip.PeerMsgRateBytes,
	}, nodeKeyDesc)

	accessCfg := &accessManConfig{
		initAccessPerms: func() (map[string]channeldb.ChanCount,
			error) {

			genesisHash := *s.cfg.ActiveNetParams.GenesisHash
			return s.chanStateDB.FetchPermAndTempPeers(
				genesisHash[:],
			)
		},
		shouldDisconnect:   s.authGossiper.ShouldDisconnect,
		maxRestrictedSlots: int64(s.cfg.NumRestrictedSlots),
	}

	peerAccessMan, err := newAccessMan(accessCfg)
	if err != nil {
		return nil, err
	}

	s.peerAccessMan = peerAccessMan

	selfVertex := route.Vertex(nodeKeyDesc.PubKey.SerializeCompressed())
	//nolint:ll
	s.localChanMgr = &localchans.Manager{
		SelfPub:              nodeKeyDesc.PubKey,
		DefaultRoutingPolicy: cc.RoutingPolicy,
		ForAllOutgoingChannels: func(ctx context.Context,
			cb func(*models.ChannelEdgeInfo,
				*models.ChannelEdgePolicy) error,
			reset func()) error {

			return s.graphDB.ForEachNodeChannel(ctx, selfVertex,
				func(c *models.ChannelEdgeInfo,
					e *models.ChannelEdgePolicy,
					_ *models.ChannelEdgePolicy) error {

					// NOTE: The invoked callback here may
					// receive a nil channel policy.
					return cb(c, e)
				}, reset,
			)
		},
		PropagateChanPolicyUpdate: s.authGossiper.PropagateChanPolicyUpdate,
		UpdateForwardingPolicies:  s.htlcSwitch.UpdateForwardingPolicies,
		FetchChannel:              s.chanStateDB.FetchChannel,
		AddEdge: func(ctx context.Context,
			edge *models.ChannelEdgeInfo) error {

			return s.graphBuilder.AddEdge(ctx, edge)
		},
	}

	utxnStore, err := contractcourt.NewNurseryStore(
		s.cfg.ActiveNetParams.GenesisHash, dbs.ChanStateDB,
	)
	if err != nil {
		srvrLog.Errorf("unable to create nursery store: %v", err)
		return nil, err
	}

	sweeperStore, err := sweep.NewSweeperStore(
		dbs.ChanStateDB, s.cfg.ActiveNetParams.GenesisHash,
	)
	if err != nil {
		srvrLog.Errorf("unable to create sweeper store: %v", err)
		return nil, err
	}

	aggregator := sweep.NewBudgetAggregator(
		cc.FeeEstimator, sweep.DefaultMaxInputsPerTx,
		s.implCfg.AuxSweeper,
	)

	s.txPublisher = sweep.NewTxPublisher(sweep.TxPublisherConfig{
		Signer:     cc.Wallet.Cfg.Signer,
		Wallet:     cc.Wallet,
		Estimator:  cc.FeeEstimator,
		Notifier:   cc.ChainNotifier,
		AuxSweeper: s.implCfg.AuxSweeper,
	})

	s.sweeper = sweep.New(&sweep.UtxoSweeperConfig{
		FeeEstimator: cc.FeeEstimator,
		GenSweepScript: newSweepPkScriptGen(
			cc.Wallet, s.cfg.ActiveNetParams.Params,
		),
		Signer:               cc.Wallet.Cfg.Signer,
		Wallet:               newSweeperWallet(cc.Wallet),
		Mempool:              cc.MempoolNotifier,
		Notifier:             cc.ChainNotifier,
		Store:                sweeperStore,
		MaxInputsPerTx:       sweep.DefaultMaxInputsPerTx,
		MaxFeeRate:           cfg.Sweeper.MaxFeeRate,
		Aggregator:           aggregator,
		Publisher:            s.txPublisher,
		NoDeadlineConfTarget: cfg.Sweeper.NoDeadlineConfTarget,
	})

	s.utxoNursery = contractcourt.NewUtxoNursery(&contractcourt.NurseryConfig{
		ChainIO:             cc.ChainIO,
		ConfDepth:           1,
		FetchClosedChannels: s.chanStateDB.FetchClosedChannels,
		FetchClosedChannel:  s.chanStateDB.FetchClosedChannel,
		Notifier:            cc.ChainNotifier,
		PublishTransaction:  cc.Wallet.PublishTransaction,
		Store:               utxnStore,
		SweepInput:          s.sweeper.SweepInput,
		Budget:              s.cfg.Sweeper.Budget,
	})

	// Construct a closure that wraps the htlcswitch's CloseLink method.
	closeLink := func(chanPoint *wire.OutPoint,
		closureType contractcourt.ChannelCloseType) {
		// TODO(conner): Properly respect the update and error channels
		// returned by CloseLink.

		// Instruct the switch to close the channel.  Provide no close out
		// delivery script or target fee per kw because user input is not
		// available when the remote peer closes the channel.
		s.htlcSwitch.CloseLink(
			context.Background(), chanPoint, closureType, 0, 0, nil,
		)
	}

	// We will use the following channel to reliably hand off contract
	// breach events from the ChannelArbitrator to the BreachArbitrator,
	contractBreaches := make(chan *contractcourt.ContractBreachEvent, 1)

	s.breachArbitrator = contractcourt.NewBreachArbitrator(
		&contractcourt.BreachConfig{
			CloseLink: closeLink,
			DB:        s.chanStateDB,
			Estimator: s.cc.FeeEstimator,
			GenSweepScript: newSweepPkScriptGen(
				cc.Wallet, s.cfg.ActiveNetParams.Params,
			),
			Notifier:           cc.ChainNotifier,
			PublishTransaction: cc.Wallet.PublishTransaction,
			ContractBreaches:   contractBreaches,
			Signer:             cc.Wallet.Cfg.Signer,
			Store: contractcourt.NewRetributionStore(
				dbs.ChanStateDB,
			),
			AuxSweeper: s.implCfg.AuxSweeper,
		},
	)

	//nolint:ll
	s.chainArb = contractcourt.NewChainArbitrator(contractcourt.ChainArbitratorConfig{
		ChainHash:              *s.cfg.ActiveNetParams.GenesisHash,
		IncomingBroadcastDelta: lncfg.DefaultIncomingBroadcastDelta,
		OutgoingBroadcastDelta: lncfg.DefaultOutgoingBroadcastDelta,
		NewSweepAddr: func() ([]byte, error) {
			addr, err := newSweepPkScriptGen(
				cc.Wallet, netParams,
			)().Unpack()
			if err != nil {
				return nil, err
			}

			return addr.DeliveryAddress, nil
		},
		PublishTx: cc.Wallet.PublishTransaction,
		DeliverResolutionMsg: func(msgs ...contractcourt.ResolutionMsg) error {
			for _, msg := range msgs {
				err := s.htlcSwitch.ProcessContractResolution(msg)
				if err != nil {
					return err
				}
			}
			return nil
		},
		IncubateOutputs: func(chanPoint wire.OutPoint,
			outHtlcRes fn.Option[lnwallet.OutgoingHtlcResolution],
			inHtlcRes fn.Option[lnwallet.IncomingHtlcResolution],
			broadcastHeight uint32,
			deadlineHeight fn.Option[int32]) error {

			return s.utxoNursery.IncubateOutputs(
				chanPoint, outHtlcRes, inHtlcRes,
				broadcastHeight, deadlineHeight,
			)
		},
		PreimageDB:   s.witnessBeacon,
		Notifier:     cc.ChainNotifier,
		Mempool:      cc.MempoolNotifier,
		Signer:       cc.Wallet.Cfg.Signer,
		FeeEstimator: cc.FeeEstimator,
		ChainIO:      cc.ChainIO,
		MarkLinkInactive: func(chanPoint wire.OutPoint) error {
			chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
			s.htlcSwitch.RemoveLink(chanID)
			return nil
		},
		IsOurAddress: cc.Wallet.IsOurAddress,
		ContractBreach: func(chanPoint wire.OutPoint,
			breachRet *lnwallet.BreachRetribution) error {

			// processACK will handle the BreachArbitrator ACKing
			// the event.
			finalErr := make(chan error, 1)
			processACK := func(brarErr error) {
				if brarErr != nil {
					finalErr <- brarErr
					return
				}

				// If the BreachArbitrator successfully handled
				// the event, we can signal that the handoff
				// was successful.
				finalErr <- nil
			}

			event := &contractcourt.ContractBreachEvent{
				ChanPoint:         chanPoint,
				ProcessACK:        processACK,
				BreachRetribution: breachRet,
			}

			// Send the contract breach event to the
			// BreachArbitrator.
			select {
			case contractBreaches <- event:
			case <-s.quit:
				return ErrServerShuttingDown
			}

			// We'll wait for a final error to be available from
			// the BreachArbitrator.
			select {
			case err := <-finalErr:
				return err
			case <-s.quit:
				return ErrServerShuttingDown
			}
		},
		DisableChannel: func(chanPoint wire.OutPoint) error {
			return s.chanStatusMgr.RequestDisable(chanPoint, false)
		},
		Sweeper:                       s.sweeper,
		Registry:                      s.invoices,
		NotifyClosedChannel:           s.channelNotifier.NotifyClosedChannelEvent,
		NotifyFullyResolvedChannel:    s.channelNotifier.NotifyFullyResolvedChannelEvent,
		OnionProcessor:                s.sphinx,
		PaymentsExpirationGracePeriod: cfg.PaymentsExpirationGracePeriod,
		IsForwardedHTLC:               s.htlcSwitch.IsForwardedHTLC,
		Clock:                         clock.NewDefaultClock(),
		SubscribeBreachComplete:       s.breachArbitrator.SubscribeBreachComplete,
		PutFinalHtlcOutcome:           s.chanStateDB.PutOnchainFinalHtlcOutcome,
		HtlcNotifier:                  s.htlcNotifier,
		Budget:                        *s.cfg.Sweeper.Budget,

		// TODO(yy): remove this hack once PaymentCircuit is interfaced.
		QueryIncomingCircuit: func(
			circuit models.CircuitKey) *models.CircuitKey {

			// Get the circuit map.
			circuits := s.htlcSwitch.CircuitLookup()

			// Lookup the outgoing circuit.
			pc := circuits.LookupOpenCircuit(circuit)
			if pc == nil {
				return nil
			}

			return &pc.Incoming
		},
		AuxLeafStore: implCfg.AuxLeafStore,
		AuxSigner:    implCfg.AuxSigner,
		AuxResolver:  implCfg.AuxContractResolver,
	}, dbs.ChanStateDB)

	// Select the configuration and funding parameters for Bitcoin.
	chainCfg := cfg.Bitcoin
	minRemoteDelay := funding.MinBtcRemoteDelay
	maxRemoteDelay := funding.MaxBtcRemoteDelay

	var chanIDSeed [32]byte
	if _, err := rand.Read(chanIDSeed[:]); err != nil {
		return nil, err
	}

	// Wrap the DeleteChannelEdges method so that the funding manager can
	// use it without depending on several layers of indirection.
	deleteAliasEdge := func(scid lnwire.ShortChannelID) (
		*models.ChannelEdgePolicy, error) {

		info, e1, e2, err := s.graphDB.FetchChannelEdgesByID(
			scid.ToUint64(),
		)
		if errors.Is(err, graphdb.ErrEdgeNotFound) {
			// This is unlikely but there is a slim chance of this
			// being hit if lnd was killed via SIGKILL and the
			// funding manager was stepping through the delete
			// alias edge logic.
			return nil, nil
		} else if err != nil {
			return nil, err
		}

		// Grab our key to find our policy.
		var ourKey [33]byte
		copy(ourKey[:], nodeKeyDesc.PubKey.SerializeCompressed())

		var ourPolicy *models.ChannelEdgePolicy
		if info != nil && info.NodeKey1Bytes == ourKey {
			ourPolicy = e1
		} else {
			ourPolicy = e2
		}

		if ourPolicy == nil {
			// Something is wrong, so return an error.
			return nil, fmt.Errorf("we don't have an edge")
		}

		err = s.graphDB.DeleteChannelEdges(
			false, false, scid.ToUint64(),
		)
		return ourPolicy, err
	}

	// For the reservationTimeout and the zombieSweeperInterval different
	// values are set in case we are in a dev environment so enhance test
	// capacilities.
	reservationTimeout := chanfunding.DefaultReservationTimeout
	zombieSweeperInterval := lncfg.DefaultZombieSweeperInterval

	// Get the development config for funding manager. If we are not in
	// development mode, this would be nil.
	var devCfg *funding.DevConfig
	if lncfg.IsDevBuild() {
		devCfg = &funding.DevConfig{
			ProcessChannelReadyWait: cfg.Dev.ChannelReadyWait(),
			MaxWaitNumBlocksFundingConf: cfg.Dev.
				GetMaxWaitNumBlocksFundingConf(),
		}

		reservationTimeout = cfg.Dev.GetReservationTimeout()
		zombieSweeperInterval = cfg.Dev.GetZombieSweeperInterval()

		srvrLog.Debugf("Using the dev config for the fundingMgr: %v, "+
			"reservationTimeout=%v, zombieSweeperInterval=%v",
			devCfg, reservationTimeout, zombieSweeperInterval)
	}

	// Attempt to parse the provided upfront-shutdown address (if any).
	script, err := chancloser.ParseUpfrontShutdownAddress(
		cfg.UpfrontShutdownAddr, cfg.ActiveNetParams.Params,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing upfront shutdown: %w",
			err)
	}

	//nolint:ll
	s.fundingMgr, err = funding.NewFundingManager(funding.Config{
		Dev:                devCfg,
		NoWumboChans:       !cfg.ProtocolOptions.Wumbo(),
		IDKey:              nodeKeyDesc.PubKey,
		IDKeyLoc:           nodeKeyDesc.KeyLocator,
		Wallet:             cc.Wallet,
		PublishTransaction: cc.Wallet.PublishTransaction,
		UpdateLabel: func(hash chainhash.Hash, label string) error {
			return cc.Wallet.LabelTransaction(hash, label, true)
		},
		Notifier:     cc.ChainNotifier,
		ChannelDB:    s.chanStateDB,
		FeeEstimator: cc.FeeEstimator,
		SignMessage:  cc.MsgSigner.SignMessage,
		CurrentNodeAnnouncement: func() (lnwire.NodeAnnouncement1,
			error) {

			return s.genNodeAnnouncement(nil)
		},
		SendAnnouncement:     s.authGossiper.ProcessLocalAnnouncement,
		NotifyWhenOnline:     s.NotifyWhenOnline,
		TempChanIDSeed:       chanIDSeed,
		FindChannel:          s.findChannel,
		DefaultRoutingPolicy: cc.RoutingPolicy,
		DefaultMinHtlcIn:     cc.MinHtlcIn,
		NumRequiredConfs: func(chanAmt btcutil.Amount,
			pushAmt lnwire.MilliSatoshi) uint16 {
			// For large channels we increase the number
			// of confirmations we require for the
			// channel to be considered open. As it is
			// always the responder that gets to choose
			// value, the pushAmt is value being pushed
			// to us. This means we have more to lose
			// in the case this gets re-orged out, and
			// we will require more confirmations before
			// we consider it open.

			// In case the user has explicitly specified
			// a default value for the number of
			// confirmations, we use it.
			defaultConf := uint16(chainCfg.DefaultNumChanConfs)
			if defaultConf != 0 {
				return defaultConf
			}

			minConf := uint64(3)
			maxConf := uint64(6)

			// If this is a wumbo channel, then we'll require the
			// max amount of confirmations.
			if chanAmt > MaxFundingAmount {
				return uint16(maxConf)
			}

			// If not we return a value scaled linearly
			// between 3 and 6, depending on channel size.
			// TODO(halseth): Use 1 as minimum?
			maxChannelSize := uint64(
				lnwire.NewMSatFromSatoshis(MaxFundingAmount))
			stake := lnwire.NewMSatFromSatoshis(chanAmt) + pushAmt
			conf := maxConf * uint64(stake) / maxChannelSize
			if conf < minConf {
				conf = minConf
			}
			if conf > maxConf {
				conf = maxConf
			}
			return uint16(conf)
		},
		RequiredRemoteDelay: func(chanAmt btcutil.Amount) uint16 {
			// We scale the remote CSV delay (the time the
			// remote have to claim funds in case of a unilateral
			// close) linearly from minRemoteDelay blocks
			// for small channels, to maxRemoteDelay blocks
			// for channels of size MaxFundingAmount.

			// In case the user has explicitly specified
			// a default value for the remote delay, we
			// use it.
			defaultDelay := uint16(chainCfg.DefaultRemoteDelay)
			if defaultDelay > 0 {
				return defaultDelay
			}

			// If this is a wumbo channel, then we'll require the
			// max value.
			if chanAmt > MaxFundingAmount {
				return maxRemoteDelay
			}

			// If not we scale according to channel size.
			delay := uint16(btcutil.Amount(maxRemoteDelay) *
				chanAmt / MaxFundingAmount)
			if delay < minRemoteDelay {
				delay = minRemoteDelay
			}
			if delay > maxRemoteDelay {
				delay = maxRemoteDelay
			}
			return delay
		},
		WatchNewChannel: func(channel *channeldb.OpenChannel,
			peerKey *btcec.PublicKey) error {

			// First, we'll mark this new peer as a persistent peer
			// for re-connection purposes. If the peer is not yet
			// tracked or the user hasn't requested it to be perm,
			// we'll set false to prevent the server from continuing
			// to connect to this peer even if the number of
			// channels with this peer is zero.
			s.mu.Lock()
			pubStr := string(peerKey.SerializeCompressed())
			if _, ok := s.persistentPeers[pubStr]; !ok {
				s.persistentPeers[pubStr] = false
			}
			s.mu.Unlock()

			// With that taken care of, we'll send this channel to
			// the chain arb so it can react to on-chain events.
			return s.chainArb.WatchNewChannel(channel)
		},
		ReportShortChanID: func(chanPoint wire.OutPoint) error {
			cid := lnwire.NewChanIDFromOutPoint(chanPoint)
			return s.htlcSwitch.UpdateShortChanID(cid)
		},
		RequiredRemoteChanReserve: func(chanAmt,
			dustLimit btcutil.Amount) btcutil.Amount {

			// By default, we'll require the remote peer to maintain
			// at least 1% of the total channel capacity at all
			// times. If this value ends up dipping below the dust
			// limit, then we'll use the dust limit itself as the
			// reserve as required by BOLT #2.
			reserve := chanAmt / 100
			if reserve < dustLimit {
				reserve = dustLimit
			}

			return reserve
		},
		RequiredRemoteMaxValue: func(chanAmt btcutil.Amount) lnwire.MilliSatoshi {
			// By default, we'll allow the remote peer to fully
			// utilize the full bandwidth of the channel, minus our
			// required reserve.
			reserve := lnwire.NewMSatFromSatoshis(chanAmt / 100)
			return lnwire.NewMSatFromSatoshis(chanAmt) - reserve
		},
		RequiredRemoteMaxHTLCs: func(chanAmt btcutil.Amount) uint16 {
			if cfg.DefaultRemoteMaxHtlcs > 0 {
				return cfg.DefaultRemoteMaxHtlcs
			}

			// By default, we'll permit them to utilize the full
			// channel bandwidth.
			return uint16(input.MaxHTLCNumber / 2)
		},
		ZombieSweeperInterval:         zombieSweeperInterval,
		ReservationTimeout:            reservationTimeout,
		MinChanSize:                   btcutil.Amount(cfg.MinChanSize),
		MaxChanSize:                   btcutil.Amount(cfg.MaxChanSize),
		MaxPendingChannels:            cfg.MaxPendingChannels,
		RejectPush:                    cfg.RejectPush,
		MaxLocalCSVDelay:              chainCfg.MaxLocalDelay,
		NotifyOpenChannelEvent:        s.notifyOpenChannelPeerEvent,
		OpenChannelPredicate:          chanPredicate,
		NotifyPendingOpenChannelEvent: s.notifyPendingOpenChannelPeerEvent,
		NotifyFundingTimeout:          s.notifyFundingTimeoutPeerEvent,
		EnableUpfrontShutdown:         cfg.EnableUpfrontShutdown,
		MaxAnchorsCommitFeeRate: chainfee.SatPerKVByte(
			s.cfg.MaxCommitFeeRateAnchors * 1000).FeePerKWeight(),
		DeleteAliasEdge:      deleteAliasEdge,
		AliasManager:         s.aliasMgr,
		IsSweeperOutpoint:    s.sweeper.IsSweeperOutpoint,
		AuxFundingController: implCfg.AuxFundingController,
		AuxSigner:            implCfg.AuxSigner,
		AuxResolver:          implCfg.AuxContractResolver,
		AuxChannelNegotiator: implCfg.AuxChannelNegotiator,
		ShutdownScript:       peer.ChooseAddr(script),
	})
	if err != nil {
		return nil, err
	}

	// Next, we'll assemble the sub-system that will maintain an on-disk
	// static backup of the latest channel state.
	chanNotifier := &channelNotifier{
		chanNotifier: s.channelNotifier,
		addrs:        s.addrSource,
	}
	backupFile := chanbackup.NewMultiFile(
		cfg.BackupFilePath, cfg.NoBackupArchive,
	)
	startingChans, err := chanbackup.FetchStaticChanBackups(
		ctx, s.chanStateDB, s.addrSource,
	)
	if err != nil {
		return nil, err
	}
	s.chanSubSwapper, err = chanbackup.NewSubSwapper(
		ctx, startingChans, chanNotifier, s.cc.KeyRing, backupFile,
	)
	if err != nil {
		return nil, err
	}

	// Assemble a peer notifier which will provide clients with subscriptions
	// to peer online and offline events.
	s.peerNotifier = peernotifier.New()

	// Create a channel event store which monitors all open channels.
	s.chanEventStore = chanfitness.NewChannelEventStore(&chanfitness.Config{
		SubscribeChannelEvents: func() (subscribe.Subscription, error) {
			return s.channelNotifier.SubscribeChannelEvents()
		},
		SubscribePeerEvents: func() (subscribe.Subscription, error) {
			return s.peerNotifier.SubscribePeerEvents()
		},
		GetOpenChannels: s.chanStateDB.FetchAllOpenChannels,
		Clock:           clock.NewDefaultClock(),
		ReadFlapCount:   s.miscDB.ReadFlapCount,
		WriteFlapCount:  s.miscDB.WriteFlapCounts,
		FlapCountTicker: ticker.New(chanfitness.FlapCountFlushRate),
	})

	if cfg.WtClient.Active {
		policy := wtpolicy.DefaultPolicy()
		policy.MaxUpdates = cfg.WtClient.MaxUpdates

		// We expose the sweep fee rate in sat/vbyte, but the tower
		// protocol operations on sat/kw.
		sweepRateSatPerVByte := chainfee.SatPerKVByte(
			1000 * cfg.WtClient.SweepFeeRate,
		)

		policy.SweepFeeRate = sweepRateSatPerVByte.FeePerKWeight()

		if err := policy.Validate(); err != nil {
			return nil, err
		}

		// authDial is the wrapper around the btrontide.Dial for the
		// watchtower.
		authDial := func(localKey keychain.SingleKeyECDH,
			netAddr *lnwire.NetAddress,
			dialer tor.DialFunc) (wtserver.Peer, error) {

			return brontide.Dial(
				localKey, netAddr, cfg.ConnectionTimeout, dialer,
			)
		}

		// buildBreachRetribution is a call-back that can be used to
		// query the BreachRetribution info and channel type given a
		// channel ID and commitment height.
		buildBreachRetribution := func(chanID lnwire.ChannelID,
			commitHeight uint64) (*lnwallet.BreachRetribution,
			channeldb.ChannelType, error) {

			channel, err := s.chanStateDB.FetchChannelByID(
				nil, chanID,
			)
			if err != nil {
				return nil, 0, err
			}

			br, err := lnwallet.NewBreachRetribution(
				channel, commitHeight, 0, nil,
				implCfg.AuxLeafStore,
				implCfg.AuxContractResolver,
			)
			if err != nil {
				return nil, 0, err
			}

			return br, channel.ChanType, nil
		}

		fetchClosedChannel := s.chanStateDB.FetchClosedChannelForID

		// Copy the policy for legacy channels and set the blob flag
		// signalling support for anchor channels.
		anchorPolicy := policy
		anchorPolicy.BlobType |= blob.Type(blob.FlagAnchorChannel)

		// Copy the policy for legacy channels and set the blob flag
		// signalling support for taproot channels.
		taprootPolicy := policy
		taprootPolicy.TxPolicy.BlobType |= blob.Type(
			blob.FlagTaprootChannel,
		)

		s.towerClientMgr, err = wtclient.NewManager(&wtclient.Config{
			FetchClosedChannel:     fetchClosedChannel,
			BuildBreachRetribution: buildBreachRetribution,
			SessionCloseRange:      cfg.WtClient.SessionCloseRange,
			ChainNotifier:          s.cc.ChainNotifier,
			SubscribeChannelEvents: func() (subscribe.Subscription,
				error) {

				return s.channelNotifier.
					SubscribeChannelEvents()
			},
			Signer: cc.Wallet.Cfg.Signer,
			NewAddress: func() ([]byte, error) {
				addr, err := newSweepPkScriptGen(
					cc.Wallet, netParams,
				)().Unpack()
				if err != nil {
					return nil, err
				}

				return addr.DeliveryAddress, nil
			},
			SecretKeyRing:      s.cc.KeyRing,
			Dial:               cfg.net.Dial,
			AuthDial:           authDial,
			DB:                 dbs.TowerClientDB,
			ChainHash:          *s.cfg.ActiveNetParams.GenesisHash,
			MinBackoff:         10 * time.Second,
			MaxBackoff:         5 * time.Minute,
			MaxTasksInMemQueue: cfg.WtClient.MaxTasksInMemQueue,
		}, policy, anchorPolicy, taprootPolicy)
		if err != nil {
			return nil, err
		}
	}

	if len(cfg.ExternalHosts) != 0 {
		advertisedIPs := make(map[string]struct{})
		for _, addr := range s.currentNodeAnn.Addresses {
			advertisedIPs[addr.String()] = struct{}{}
		}

		s.hostAnn = netann.NewHostAnnouncer(netann.HostAnnouncerConfig{
			Hosts:         cfg.ExternalHosts,
			RefreshTicker: ticker.New(defaultHostSampleInterval),
			LookupHost: func(host string) (net.Addr, error) {
				return lncfg.ParseAddressString(
					host, strconv.Itoa(defaultPeerPort),
					cfg.net.ResolveTCPAddr,
				)
			},
			AdvertisedIPs: advertisedIPs,
			AnnounceNewIPs: netann.IPAnnouncer(
				func(modifier ...netann.NodeAnnModifier) (
					lnwire.NodeAnnouncement1, error) {

					return s.genNodeAnnouncement(
						nil, modifier...,
					)
				}),
		})
	}

	// Create liveness monitor.
	s.createLivenessMonitor(cfg, cc, leaderElector)

	listeners := make([]net.Listener, len(listenAddrs))
	for i, listenAddr := range listenAddrs {
		// Note: though brontide.NewListener uses ResolveTCPAddr, it
		// doesn't need to call the general lndResolveTCP function
		// since we are resolving a local address.

		// RESOLVE: We are actually partially accepting inbound
		// connection requests when we call NewListener.
		listeners[i], err = brontide.NewListener(
			nodeKeyECDH, listenAddr.String(),
			// TODO(yy): remove this check and unify the inbound
			// connection check inside `InboundPeerConnected`.
			s.peerAccessMan.checkAcceptIncomingConn,
		)
		if err != nil {
			return nil, err
		}
	}

	// Create the connection manager which will be responsible for
	// maintaining persistent outbound connections and also accepting new
	// incoming connections
	cmgr, err := connmgr.New(&connmgr.Config{
		Listeners:      listeners,
		OnAccept:       s.InboundPeerConnected,
		RetryDuration:  time.Second * 5,
		TargetOutbound: 100,
		Dial: noiseDial(
			nodeKeyECDH, s.cfg.net, s.cfg.ConnectionTimeout,
		),
		OnConnection: s.OutboundPeerConnected,
	})
	if err != nil {
		return nil, err
	}
	s.connMgr = cmgr

	// Finally, register the subsystems in blockbeat.
	s.registerBlockConsumers()

	return s, nil
}

// UpdateRoutingConfig is a callback function to update the routing config
// values in the main cfg.
func (s *server) UpdateRoutingConfig(cfg *routing.MissionControlConfig) {
	routerCfg := s.cfg.SubRPCServers.RouterRPC

	switch c := cfg.Estimator.Config().(type) {
	case routing.AprioriConfig:
		routerCfg.ProbabilityEstimatorType =
			routing.AprioriEstimatorName

		targetCfg := routerCfg.AprioriConfig
		targetCfg.PenaltyHalfLife = c.PenaltyHalfLife
		targetCfg.Weight = c.AprioriWeight
		targetCfg.CapacityFraction = c.CapacityFraction
		targetCfg.HopProbability = c.AprioriHopProbability

	case routing.BimodalConfig:
		routerCfg.ProbabilityEstimatorType =
			routing.BimodalEstimatorName

		targetCfg := routerCfg.BimodalConfig
		targetCfg.Scale = int64(c.BimodalScaleMsat)
		targetCfg.NodeWeight = c.BimodalNodeWeight
		targetCfg.DecayTime = c.BimodalDecayTime
	}

	routerCfg.MaxMcHistory = cfg.MaxMcHistory
}

// registerBlockConsumers registers the subsystems that consume block events.
// By calling `RegisterQueue`, a list of subsystems are registered in the
// blockbeat for block notifications. When a new block arrives, the subsystems
// in the same queue are notified sequentially, and different queues are
// notified concurrently.
//
// NOTE: To put a subsystem in a different queue, create a slice and pass it to
// a new `RegisterQueue` call.
func (s *server) registerBlockConsumers() {
	// In this queue, when a new block arrives, it will be received and
	// processed in this order: chainArb -> sweeper -> txPublisher.
	consumers := []chainio.Consumer{
		s.chainArb,
		s.sweeper,
		s.txPublisher,
	}
	s.blockbeatDispatcher.RegisterQueue(consumers)
}

// signAliasUpdate takes a ChannelUpdate and returns the signature. This is
// used for option_scid_alias channels where the ChannelUpdate to be sent back
// may differ from what is on disk.
func (s *server) signAliasUpdate(u *lnwire.ChannelUpdate1) (*ecdsa.Signature,
	error) {

	data, err := u.DataToSign()
	if err != nil {
		return nil, err
	}

	return s.cc.MsgSigner.SignMessage(s.identityKeyLoc, data, true)
}

// createLivenessMonitor creates a set of health checks using our configured
// values and uses these checks to create a liveness monitor. Available
// health checks,
//   - chainHealthCheck (will be disabled for --nochainbackend mode)
//   - diskCheck
//   - tlsHealthCheck
//   - torController, only created when tor is enabled.
//
// If a health check has been disabled by setting attempts to 0, our monitor
// will not run it.
func (s *server) createLivenessMonitor(cfg *Config, cc *chainreg.ChainControl,
	leaderElector cluster.LeaderElector) {

	chainBackendAttempts := cfg.HealthChecks.ChainCheck.Attempts
	if cfg.Bitcoin.Node == "nochainbackend" {
		srvrLog.Info("Disabling chain backend checks for " +
			"nochainbackend mode")

		chainBackendAttempts = 0
	}

	chainHealthCheck := healthcheck.NewObservation(
		"chain backend",
		cc.HealthCheck,
		cfg.HealthChecks.ChainCheck.Interval,
		cfg.HealthChecks.ChainCheck.Timeout,
		cfg.HealthChecks.ChainCheck.Backoff,
		chainBackendAttempts,
	)

	diskCheck := healthcheck.NewObservation(
		"disk space",
		func() error {
			free, err := healthcheck.AvailableDiskSpaceRatio(
				cfg.LndDir,
			)
			if err != nil {
				return err
			}

			// If we have more free space than we require,
			// we return a nil error.
			if free > cfg.HealthChecks.DiskCheck.RequiredRemaining {
				return nil
			}

			return fmt.Errorf("require: %v free space, got: %v",
				cfg.HealthChecks.DiskCheck.RequiredRemaining,
				free)
		},
		cfg.HealthChecks.DiskCheck.Interval,
		cfg.HealthChecks.DiskCheck.Timeout,
		cfg.HealthChecks.DiskCheck.Backoff,
		cfg.HealthChecks.DiskCheck.Attempts,
	)

	tlsHealthCheck := healthcheck.NewObservation(
		"tls",
		func() error {
			expired, expTime, err := s.tlsManager.IsCertExpired(
				s.cc.KeyRing,
			)
			if err != nil {
				return err
			}
			if expired {
				return fmt.Errorf("TLS certificate is "+
					"expired as of %v", expTime)
			}

			// If the certificate is not outdated, no error needs
			// to be returned
			return nil
		},
		cfg.HealthChecks.TLSCheck.Interval,
		cfg.HealthChecks.TLSCheck.Timeout,
		cfg.HealthChecks.TLSCheck.Backoff,
		cfg.HealthChecks.TLSCheck.Attempts,
	)

	checks := []*healthcheck.Observation{
		chainHealthCheck, diskCheck, tlsHealthCheck,
	}

	// If Tor is enabled, add the healthcheck for tor connection.
	if s.torController != nil {
		torConnectionCheck := healthcheck.NewObservation(
			"tor connection",
			func() error {
				return healthcheck.CheckTorServiceStatus(
					s.torController,
					func() error {
						return s.createNewHiddenService(
							context.TODO(),
						)
					},
				)
			},
			cfg.HealthChecks.TorConnection.Interval,
			cfg.HealthChecks.TorConnection.Timeout,
			cfg.HealthChecks.TorConnection.Backoff,
			cfg.HealthChecks.TorConnection.Attempts,
		)
		checks = append(checks, torConnectionCheck)
	}

	// If remote signing is enabled, add the healthcheck for the remote
	// signing RPC interface.
	if s.cfg.RemoteSigner != nil && s.cfg.RemoteSigner.Enable {
		// Because we have two cascading timeouts here, we need to add
		// some slack to the "outer" one of them in case the "inner"
		// returns exactly on time.
		overhead := time.Millisecond * 10

		remoteSignerConnectionCheck := healthcheck.NewObservation(
			"remote signer connection",
			rpcwallet.HealthCheck(
				s.cfg.RemoteSigner,

				// For the health check we might to be even
				// stricter than the initial/normal connect, so
				// we use the health check timeout here.
				cfg.HealthChecks.RemoteSigner.Timeout,
			),
			cfg.HealthChecks.RemoteSigner.Interval,
			cfg.HealthChecks.RemoteSigner.Timeout+overhead,
			cfg.HealthChecks.RemoteSigner.Backoff,
			cfg.HealthChecks.RemoteSigner.Attempts,
		)
		checks = append(checks, remoteSignerConnectionCheck)
	}

	// If we have a leader elector, we add a health check to ensure we are
	// still the leader. During normal operation, we should always be the
	// leader, but there are circumstances where this may change, such as
	// when we lose network connectivity for long enough expiring out lease.
	if leaderElector != nil {
		leaderCheck := healthcheck.NewObservation(
			"leader status",
			func() error {
				// Check if we are still the leader. Note that
				// we don't need to use a timeout context here
				// as the healthcheck observer will handle the
				// timeout case for us.
				timeoutCtx, cancel := context.WithTimeout(
					context.Background(),
					cfg.HealthChecks.LeaderCheck.Timeout,
				)
				defer cancel()

				leader, err := leaderElector.IsLeader(
					timeoutCtx,
				)
				if err != nil {
					return fmt.Errorf("unable to check if "+
						"still leader: %v", err)
				}

				if !leader {
					srvrLog.Debug("Not the current leader")
					return fmt.Errorf("not the current " +
						"leader")
				}

				return nil
			},
			cfg.HealthChecks.LeaderCheck.Interval,
			cfg.HealthChecks.LeaderCheck.Timeout,
			cfg.HealthChecks.LeaderCheck.Backoff,
			cfg.HealthChecks.LeaderCheck.Attempts,
		)

		checks = append(checks, leaderCheck)
	}

	// If we have not disabled all of our health checks, we create a
	// liveness monitor with our configured checks.
	s.livenessMonitor = healthcheck.NewMonitor(
		&healthcheck.Config{
			Checks:   checks,
			Shutdown: srvrLog.Criticalf,
		},
	)
}

// Started returns true if the server has been started, and false otherwise.
// NOTE: This function is safe for concurrent access.
func (s *server) Started() bool {
	return atomic.LoadInt32(&s.active) != 0
}

// cleaner is used to aggregate "cleanup" functions during an operation that
// starts several subsystems. In case one of the subsystem fails to start
// and a proper resource cleanup is required, the "run" method achieves this
// by running all these added "cleanup" functions.
type cleaner []func() error

// add is used to add a cleanup function to be called when
// the run function is executed.
func (c cleaner) add(cleanup func() error) cleaner {
	return append(c, cleanup)
}

// run is used to run all the previousely added cleanup functions.
func (c cleaner) run() {
	for i := len(c) - 1; i >= 0; i-- {
		if err := c[i](); err != nil {
			srvrLog.Errorf("Cleanup failed: %v", err)
		}
	}
}

// Start starts the main daemon server, all requested listeners, and any helper
// goroutines.
// NOTE: This function is safe for concurrent access.
//
//nolint:funlen
func (s *server) Start(ctx context.Context) error {
	var startErr error

	// If one sub system fails to start, the following code ensures that the
	// previous started ones are stopped. It also ensures a proper wallet
	// shutdown which is important for releasing its resources (boltdb, etc...)
	cleanup := cleaner{}

	s.start.Do(func() {
		cleanup = cleanup.add(s.customMessageServer.Stop)
		if err := s.customMessageServer.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.onionMessageServer.Stop)
		if err := s.onionMessageServer.Start(); err != nil {
			startErr = err
			return
		}

		if s.hostAnn != nil {
			cleanup = cleanup.add(s.hostAnn.Stop)
			if err := s.hostAnn.Start(); err != nil {
				startErr = err
				return
			}
		}

		if s.livenessMonitor != nil {
			cleanup = cleanup.add(s.livenessMonitor.Stop)
			if err := s.livenessMonitor.Start(); err != nil {
				startErr = err
				return
			}
		}

		// Start the notification server. This is used so channel
		// management goroutines can be notified when a funding
		// transaction reaches a sufficient number of confirmations, or
		// when the input for the funding transaction is spent in an
		// attempt at an uncooperative close by the counterparty.
		cleanup = cleanup.add(s.sigPool.Stop)
		if err := s.sigPool.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.writePool.Stop)
		if err := s.writePool.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.readPool.Stop)
		if err := s.readPool.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.cc.ChainNotifier.Stop)
		if err := s.cc.ChainNotifier.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.cc.BestBlockTracker.Stop)
		if err := s.cc.BestBlockTracker.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.channelNotifier.Stop)
		if err := s.channelNotifier.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(func() error {
			return s.peerNotifier.Stop()
		})
		if err := s.peerNotifier.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.htlcNotifier.Stop)
		if err := s.htlcNotifier.Start(); err != nil {
			startErr = err
			return
		}

		if s.towerClientMgr != nil {
			cleanup = cleanup.add(s.towerClientMgr.Stop)
			if err := s.towerClientMgr.Start(); err != nil {
				startErr = err
				return
			}
		}

		beat, err := s.getStartingBeat()
		if err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.txPublisher.Stop)
		if err := s.txPublisher.Start(beat); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.sweeper.Stop)
		if err := s.sweeper.Start(beat); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.utxoNursery.Stop)
		if err := s.utxoNursery.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.breachArbitrator.Stop)
		if err := s.breachArbitrator.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.fundingMgr.Stop)
		if err := s.fundingMgr.Start(); err != nil {
			startErr = err
			return
		}

		// htlcSwitch must be started before chainArb since the latter
		// relies on htlcSwitch to deliver resolution message upon
		// start.
		cleanup = cleanup.add(s.htlcSwitch.Stop)
		if err := s.htlcSwitch.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.interceptableSwitch.Stop)
		if err := s.interceptableSwitch.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.invoiceHtlcModifier.Stop)
		if err := s.invoiceHtlcModifier.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.chainArb.Stop)
		if err := s.chainArb.Start(beat); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.graphDB.Stop)
		if err := s.graphDB.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.graphBuilder.Stop)
		if err := s.graphBuilder.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.chanRouter.Stop)
		if err := s.chanRouter.Start(); err != nil {
			startErr = err
			return
		}
		// The authGossiper depends on the chanRouter and therefore
		// should be started after it.
		cleanup = cleanup.add(s.authGossiper.Stop)
		if err := s.authGossiper.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.invoices.Stop)
		if err := s.invoices.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.sphinx.Stop)
		if err := s.sphinx.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.chanStatusMgr.Stop)
		if err := s.chanStatusMgr.Start(); err != nil {
			startErr = err
			return
		}

		cleanup = cleanup.add(s.chanEventStore.Stop)
		if err := s.chanEventStore.Start(); err != nil {
			startErr = err
			return
		}

		cleanup.add(func() error {
			s.missionController.StopStoreTickers()
			return nil
		})
		s.missionController.RunStoreTickers()

		// Before we start the connMgr, we'll check to see if we have
		// any backups to recover. We do this now as we want to ensure
		// that have all the information we need to handle channel
		// recovery _before_ we even accept connections from any peers.
		chanRestorer := &chanDBRestorer{
			db:         s.chanStateDB,
			secretKeys: s.cc.KeyRing,
			chainArb:   s.chainArb,
		}
		if len(s.chansToRestore.PackedSingleChanBackups) != 0 {
			_, err := chanbackup.UnpackAndRecoverSingles(
				s.chansToRestore.PackedSingleChanBackups,
				s.cc.KeyRing, chanRestorer, s,
			)
			if err != nil {
				startErr = fmt.Errorf("unable to unpack single "+
					"backups: %v", err)
				return
			}
		}
		if len(s.chansToRestore.PackedMultiChanBackup) != 0 {
			_, err := chanbackup.UnpackAndRecoverMulti(
				s.chansToRestore.PackedMultiChanBackup,
				s.cc.KeyRing, chanRestorer, s,
			)
			if err != nil {
				startErr = fmt.Errorf("unable to unpack chan "+
					"backup: %v", err)
				return
			}
		}

		// chanSubSwapper must be started after the `channelNotifier`
		// because it depends on channel events as a synchronization
		// point.
		cleanup = cleanup.add(s.chanSubSwapper.Stop)
		if err := s.chanSubSwapper.Start(); err != nil {
			startErr = err
			return
		}

		if s.torController != nil {
			cleanup = cleanup.add(s.torController.Stop)
			if err := s.createNewHiddenService(ctx); err != nil {
				startErr = err
				return
			}
		}

		if s.natTraversal != nil {
			s.wg.Add(1)
			go s.watchExternalIP()
		}

		// Start connmgr last to prevent connections before init.
		cleanup = cleanup.add(func() error {
			s.connMgr.Stop()
			return nil
		})

		// RESOLVE: s.connMgr.Start() is called here, but
		// brontide.NewListener() is called in newServer. This means
		// that we are actually listening and partially accepting
		// inbound connections even before the connMgr starts.
		//
		// TODO(yy): move the log into the connMgr's `Start` method.
		srvrLog.Info("connMgr starting...")
		s.connMgr.Start()
		srvrLog.Debug("connMgr started")

		// If peers are specified as a config option, we'll add those
		// peers first.
		for _, peerAddrCfg := range s.cfg.AddPeers {
			parsedPubkey, parsedHost, err := lncfg.ParseLNAddressPubkey(
				peerAddrCfg,
			)
			if err != nil {
				startErr = fmt.Errorf("unable to parse peer "+
					"pubkey from config: %v", err)
				return
			}
			addr, err := parseAddr(parsedHost, s.cfg.net)
			if err != nil {
				startErr = fmt.Errorf("unable to parse peer "+
					"address provided as a config option: "+
					"%v", err)
				return
			}

			peerAddr := &lnwire.NetAddress{
				IdentityKey: parsedPubkey,
				Address:     addr,
				ChainNet:    s.cfg.ActiveNetParams.Net,
			}

			err = s.ConnectToPeer(
				peerAddr, true,
				s.cfg.ConnectionTimeout,
			)
			if err != nil {
				startErr = fmt.Errorf("unable to connect to "+
					"peer address provided as a config "+
					"option: %v", err)
				return
			}
		}

		// Subscribe to NodeAnnouncements that advertise new addresses
		// our persistent peers.
		if err := s.updatePersistentPeerAddrs(); err != nil {
			srvrLog.Errorf("Failed to update persistent peer "+
				"addr: %v", err)

			startErr = err
			return
		}

		// With all the relevant sub-systems started, we'll now attempt
		// to establish persistent connections to our direct channel
		// collaborators within the network. Before doing so however,
		// we'll prune our set of link nodes found within the database
		// to ensure we don't reconnect to any nodes we no longer have
		// open channels with.
		if err := s.chanStateDB.PruneLinkNodes(); err != nil {
			srvrLog.Errorf("Failed to prune link nodes: %v", err)

			startErr = err
			return
		}

		if err := s.establishPersistentConnections(ctx); err != nil {
			srvrLog.Errorf("Failed to establish persistent "+
				"connections: %v", err)
		}

		// setSeedList is a helper function that turns multiple DNS seed
		// server tuples from the command line or config file into the
		// data structure we need and does a basic formal sanity check
		// in the process.
		setSeedList := func(tuples []string, genesisHash chainhash.Hash) {
			if len(tuples) == 0 {
				return
			}

			result := make([][2]string, len(tuples))
			for idx, tuple := range tuples {
				tuple = strings.TrimSpace(tuple)
				if len(tuple) == 0 {
					return
				}

				servers := strings.Split(tuple, ",")
				if len(servers) > 2 || len(servers) == 0 {
					srvrLog.Warnf("Ignoring invalid DNS "+
						"seed tuple: %v", servers)
					return
				}

				copy(result[idx][:], servers)
			}

			chainreg.ChainDNSSeeds[genesisHash] = result
		}

		// Let users overwrite the DNS seed nodes. We only allow them
		// for bitcoin mainnet/testnet/signet.
		if s.cfg.Bitcoin.MainNet {
			setSeedList(
				s.cfg.Bitcoin.DNSSeeds,
				chainreg.BitcoinMainnetGenesis,
			)
		}
		if s.cfg.Bitcoin.TestNet3 {
			setSeedList(
				s.cfg.Bitcoin.DNSSeeds,
				chainreg.BitcoinTestnetGenesis,
			)
		}
		if s.cfg.Bitcoin.TestNet4 {
			setSeedList(
				s.cfg.Bitcoin.DNSSeeds,
				chainreg.BitcoinTestnet4Genesis,
			)
		}
		if s.cfg.Bitcoin.SigNet {
			setSeedList(
				s.cfg.Bitcoin.DNSSeeds,
				chainreg.BitcoinSignetGenesis,
			)
		}

		// If network bootstrapping hasn't been disabled, then we'll
		// configure the set of active bootstrappers, and launch a
		// dedicated goroutine to maintain a set of persistent
		// connections.
		if !s.cfg.NoNetBootstrap {
			bootstrappers, err := initNetworkBootstrappers(s)
			if err != nil {
				startErr = err
				return
			}

			s.wg.Add(1)
			go s.peerBootstrapper(
				ctx, defaultMinPeers, bootstrappers,
			)
		} else {
			srvrLog.Infof("Auto peer bootstrapping is disabled")
		}

		// Start the blockbeat after all other subsystems have been
		// started so they are ready to receive new blocks.
		cleanup = cleanup.add(func() error {
			s.blockbeatDispatcher.Stop()
			return nil
		})
		if err := s.blockbeatDispatcher.Start(); err != nil {
			startErr = err
			return
		}

		// Set the active flag now that we've completed the full
		// startup.
		atomic.StoreInt32(&s.active, 1)
	})

	if startErr != nil {
		cleanup.run()
	}
	return startErr
}

// Stop gracefully shutsdown the main daemon server. This function will signal
// any active goroutines, or helper objects to exit, then blocks until they've
// all successfully exited. Additionally, any/all listeners are closed.
// NOTE: This function is safe for concurrent access.
func (s *server) Stop() error {
	s.stop.Do(func() {
		atomic.StoreInt32(&s.stopping, 1)

		ctx := context.Background()

		close(s.quit)

		// Shutdown connMgr first to prevent conns during shutdown.
		s.connMgr.Stop()

		// Stop dispatching blocks to other systems immediately.
		s.blockbeatDispatcher.Stop()

		// Shutdown the wallet, funding manager, and the rpc server.
		if err := s.chanStatusMgr.Stop(); err != nil {
			srvrLog.Warnf("failed to stop chanStatusMgr: %v", err)
		}
		if err := s.htlcSwitch.Stop(); err != nil {
			srvrLog.Warnf("failed to stop htlcSwitch: %v", err)
		}
		if err := s.sphinx.Stop(); err != nil {
			srvrLog.Warnf("failed to stop sphinx: %v", err)
		}
		if err := s.invoices.Stop(); err != nil {
			srvrLog.Warnf("failed to stop invoices: %v", err)
		}
		if err := s.interceptableSwitch.Stop(); err != nil {
			srvrLog.Warnf("failed to stop interceptable "+
				"switch: %v", err)
		}
		if err := s.invoiceHtlcModifier.Stop(); err != nil {
			srvrLog.Warnf("failed to stop htlc invoices "+
				"modifier: %v", err)
		}
		if err := s.chanRouter.Stop(); err != nil {
			srvrLog.Warnf("failed to stop chanRouter: %v", err)
		}
		if err := s.graphBuilder.Stop(); err != nil {
			srvrLog.Warnf("failed to stop graphBuilder %v", err)
		}
		if err := s.graphDB.Stop(); err != nil {
			srvrLog.Warnf("failed to stop graphDB %v", err)
		}
		if err := s.chainArb.Stop(); err != nil {
			srvrLog.Warnf("failed to stop chainArb: %v", err)
		}
		if err := s.fundingMgr.Stop(); err != nil {
			srvrLog.Warnf("failed to stop fundingMgr: %v", err)
		}
		if err := s.breachArbitrator.Stop(); err != nil {
			srvrLog.Warnf("failed to stop breachArbitrator: %v",
				err)
		}
		if err := s.utxoNursery.Stop(); err != nil {
			srvrLog.Warnf("failed to stop utxoNursery: %v", err)
		}
		if err := s.authGossiper.Stop(); err != nil {
			srvrLog.Warnf("failed to stop authGossiper: %v", err)
		}
		if err := s.sweeper.Stop(); err != nil {
			srvrLog.Warnf("failed to stop sweeper: %v", err)
		}
		if err := s.txPublisher.Stop(); err != nil {
			srvrLog.Warnf("failed to stop txPublisher: %v", err)
		}
		if err := s.channelNotifier.Stop(); err != nil {
			srvrLog.Warnf("failed to stop channelNotifier: %v", err)
		}
		if err := s.peerNotifier.Stop(); err != nil {
			srvrLog.Warnf("failed to stop peerNotifier: %v", err)
		}
		if err := s.htlcNotifier.Stop(); err != nil {
			srvrLog.Warnf("failed to stop htlcNotifier: %v", err)
		}

		// Update channel.backup file. Make sure to do it before
		// stopping chanSubSwapper.
		singles, err := chanbackup.FetchStaticChanBackups(
			ctx, s.chanStateDB, s.addrSource,
		)
		if err != nil {
			srvrLog.Warnf("failed to fetch channel states: %v",
				err)
		} else {
			err := s.chanSubSwapper.ManualUpdate(singles)
			if err != nil {
				srvrLog.Warnf("Manual update of channel "+
					"backup failed: %v", err)
			}
		}

		if err := s.chanSubSwapper.Stop(); err != nil {
			srvrLog.Warnf("failed to stop chanSubSwapper: %v", err)
		}
		if err := s.cc.ChainNotifier.Stop(); err != nil {
			srvrLog.Warnf("Unable to stop ChainNotifier: %v", err)
		}
		if err := s.cc.BestBlockTracker.Stop(); err != nil {
			srvrLog.Warnf("Unable to stop BestBlockTracker: %v",
				err)
		}
		if err := s.chanEventStore.Stop(); err != nil {
			srvrLog.Warnf("Unable to stop ChannelEventStore: %v",
				err)
		}
		s.missionController.StopStoreTickers()

		// Disconnect from each active peers to ensure that
		// peerTerminationWatchers signal completion to each peer.
		for _, peer := range s.Peers() {
			err := s.DisconnectPeer(peer.IdentityKey())
			if err != nil {
				srvrLog.Warnf("could not disconnect peer: %v"+
					"received error: %v", peer.IdentityKey(),
					err,
				)
			}
		}

		// Now that all connections have been torn down, stop the tower
		// client which will reliably flush all queued states to the
		// tower. If this is halted for any reason, the force quit timer
		// will kick in and abort to allow this method to return.
		if s.towerClientMgr != nil {
			if err := s.towerClientMgr.Stop(); err != nil {
				srvrLog.Warnf("Unable to shut down tower "+
					"client manager: %v", err)
			}
		}

		if s.hostAnn != nil {
			if err := s.hostAnn.Stop(); err != nil {
				srvrLog.Warnf("unable to shut down host "+
					"annoucner: %v", err)
			}
		}

		if s.livenessMonitor != nil {
			if err := s.livenessMonitor.Stop(); err != nil {
				srvrLog.Warnf("unable to shutdown liveness "+
					"monitor: %v", err)
			}
		}

		// Wait for all lingering goroutines to quit.
		srvrLog.Debug("Waiting for server to shutdown...")
		s.wg.Wait()

		srvrLog.Debug("Stopping buffer pools...")
		s.sigPool.Stop()
		s.writePool.Stop()
		s.readPool.Stop()
	})

	return nil
}

// Stopped returns true if the server has been instructed to shutdown.
// NOTE: This function is safe for concurrent access.
func (s *server) Stopped() bool {
	return atomic.LoadInt32(&s.stopping) != 0
}

// configurePortForwarding attempts to set up port forwarding for the different
// ports that the server will be listening on.
//
// NOTE: This should only be used when using some kind of NAT traversal to
// automatically set up forwarding rules.
func (s *server) configurePortForwarding(ports ...uint16) ([]string, error) {
	ip, err := s.natTraversal.ExternalIP()
	if err != nil {
		return nil, err
	}
	s.lastDetectedIP = ip

	externalIPs := make([]string, 0, len(ports))
	for _, port := range ports {
		if err := s.natTraversal.AddPortMapping(port); err != nil {
			srvrLog.Debugf("Unable to forward port %d: %v", port, err)
			continue
		}

		hostIP := fmt.Sprintf("%v:%d", ip, port)
		externalIPs = append(externalIPs, hostIP)
	}

	return externalIPs, nil
}

// removePortForwarding attempts to clear the forwarding rules for the different
// ports the server is currently listening on.
//
// NOTE: This should only be used when using some kind of NAT traversal to
// automatically set up forwarding rules.
func (s *server) removePortForwarding() {
	forwardedPorts := s.natTraversal.ForwardedPorts()
	for _, port := range forwardedPorts {
		if err := s.natTraversal.DeletePortMapping(port); err != nil {
			srvrLog.Errorf("Unable to remove forwarding rules for "+
				"port %d: %v", port, err)
		}
	}
}

// watchExternalIP continuously checks for an updated external IP address every
// 15 minutes. Once a new IP address has been detected, it will automatically
// handle port forwarding rules and send updated node announcements to the
// currently connected peers.
//
// NOTE: This MUST be run as a goroutine.
func (s *server) watchExternalIP() {
	defer s.wg.Done()

	// Before exiting, we'll make sure to remove the forwarding rules set
	// up by the server.
	defer s.removePortForwarding()

	// Keep track of the external IPs set by the user to avoid replacing
	// them when detecting a new IP.
	ipsSetByUser := make(map[string]struct{})
	for _, ip := range s.cfg.ExternalIPs {
		ipsSetByUser[ip.String()] = struct{}{}
	}

	forwardedPorts := s.natTraversal.ForwardedPorts()

	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
out:
	for {
		select {
		case <-ticker.C:
			// We'll start off by making sure a new IP address has
			// been detected.
			ip, err := s.natTraversal.ExternalIP()
			if err != nil {
				srvrLog.Debugf("Unable to retrieve the "+
					"external IP address: %v", err)
				continue
			}

			// Periodically renew the NAT port forwarding.
			for _, port := range forwardedPorts {
				err := s.natTraversal.AddPortMapping(port)
				if err != nil {
					srvrLog.Warnf("Unable to automatically "+
						"re-create port forwarding using %s: %v",
						s.natTraversal.Name(), err)
				} else {
					srvrLog.Debugf("Automatically re-created "+
						"forwarding for port %d using %s to "+
						"advertise external IP",
						port, s.natTraversal.Name())
				}
			}

			if ip.Equal(s.lastDetectedIP) {
				continue
			}

			srvrLog.Infof("Detected new external IP address %s", ip)

			// Next, we'll craft the new addresses that will be
			// included in the new node announcement and advertised
			// to the network. Each address will consist of the new
			// IP detected and one of the currently advertised
			// ports.
			var newAddrs []net.Addr
			for _, port := range forwardedPorts {
				hostIP := fmt.Sprintf("%v:%d", ip, port)
				addr, err := net.ResolveTCPAddr("tcp", hostIP)
				if err != nil {
					srvrLog.Debugf("Unable to resolve "+
						"host %v: %v", addr, err)
					continue
				}

				newAddrs = append(newAddrs, addr)
			}

			// Skip the update if we weren't able to resolve any of
			// the new addresses.
			if len(newAddrs) == 0 {
				srvrLog.Debug("Skipping node announcement " +
					"update due to not being able to " +
					"resolve any new addresses")
				continue
			}

			// Now, we'll need to update the addresses in our node's
			// announcement in order to propagate the update
			// throughout the network. We'll only include addresses
			// that have a different IP from the previous one, as
			// the previous IP is no longer valid.
			currentNodeAnn := s.getNodeAnnouncement()

			for _, addr := range currentNodeAnn.Addresses {
				host, _, err := net.SplitHostPort(addr.String())
				if err != nil {
					srvrLog.Debugf("Unable to determine "+
						"host from address %v: %v",
						addr, err)
					continue
				}

				// We'll also make sure to include external IPs
				// set manually by the user.
				_, setByUser := ipsSetByUser[addr.String()]
				if setByUser || host != s.lastDetectedIP.String() {
					newAddrs = append(newAddrs, addr)
				}
			}

			// Then, we'll generate a new timestamped node
			// announcement with the updated addresses and broadcast
			// it to our peers.
			newNodeAnn, err := s.genNodeAnnouncement(
				nil, netann.NodeAnnSetAddrs(newAddrs),
			)
			if err != nil {
				srvrLog.Debugf("Unable to generate new node "+
					"announcement: %v", err)
				continue
			}

			err = s.BroadcastMessage(nil, &newNodeAnn)
			if err != nil {
				srvrLog.Debugf("Unable to broadcast new node "+
					"announcement to peers: %v", err)
				continue
			}

			// Finally, update the last IP seen to the current one.
			s.lastDetectedIP = ip
		case <-s.quit:
			break out
		}
	}
}

// initNetworkBootstrappers initializes a set of network peer bootstrappers
// based on the server, and currently active bootstrap mechanisms as defined
// within the current configuration.
func initNetworkBootstrappers(s *server) ([]discovery.NetworkPeerBootstrapper, error) {
	srvrLog.Infof("Initializing peer network bootstrappers!")

	var bootStrappers []discovery.NetworkPeerBootstrapper

	// First, we'll create an instance of the ChannelGraphBootstrapper as
	// this can be used by default if we've already partially seeded the
	// network.
	chanGraph := autopilot.ChannelGraphFromDatabase(s.graphDB)
	graphBootstrapper, err := discovery.NewGraphBootstrapper(
		chanGraph, s.cfg.Bitcoin.IsLocalNetwork(),
	)
	if err != nil {
		return nil, err
	}
	bootStrappers = append(bootStrappers, graphBootstrapper)

	// If this isn't using simnet or regtest mode, then one of our
	// additional bootstrapping sources will be the set of running DNS
	// seeds.
	if !s.cfg.Bitcoin.IsLocalNetwork() {
		//nolint:ll
		dnsSeeds, ok := chainreg.ChainDNSSeeds[*s.cfg.ActiveNetParams.GenesisHash]

		// If we have a set of DNS seeds for this chain, then we'll add
		// it as an additional bootstrapping source.
		if ok {
			srvrLog.Infof("Creating DNS peer bootstrapper with "+
				"seeds: %v", dnsSeeds)

			dnsBootStrapper := discovery.NewDNSSeedBootstrapper(
				dnsSeeds, s.cfg.net, s.cfg.ConnectionTimeout,
			)
			bootStrappers = append(bootStrappers, dnsBootStrapper)
		}
	}

	return bootStrappers, nil
}

// createBootstrapIgnorePeers creates a map of peers that the bootstrap process
// needs to ignore, which is made of three parts,
//   - the node itself needs to be skipped as it doesn't make sense to connect
//     to itself.
//   - the peers that already have connections with, as in s.peersByPub.
//   - the peers that we are attempting to connect, as in s.persistentPeers.
func (s *server) createBootstrapIgnorePeers() map[autopilot.NodeID]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ignore := make(map[autopilot.NodeID]struct{})

	// We should ignore ourselves from bootstrapping.
	selfKey := autopilot.NewNodeID(s.identityECDH.PubKey())
	ignore[selfKey] = struct{}{}

	// Ignore all connected peers.
	for _, peer := range s.peersByPub {
		nID := autopilot.NewNodeID(peer.IdentityKey())
		ignore[nID] = struct{}{}
	}

	// Ignore all persistent peers as they have a dedicated reconnecting
	// process.
	for pubKeyStr := range s.persistentPeers {
		var nID autopilot.NodeID
		copy(nID[:], []byte(pubKeyStr))
		ignore[nID] = struct{}{}
	}

	return ignore
}

// peerBootstrapper is a goroutine which is tasked with attempting to establish
// and maintain a target minimum number of outbound connections. With this
// invariant, we ensure that our node is connected to a diverse set of peers
// and that nodes newly joining the network receive an up to date network view
// as soon as possible.
func (s *server) peerBootstrapper(ctx context.Context, numTargetPeers uint32,
	bootstrappers []discovery.NetworkPeerBootstrapper) {

	defer s.wg.Done()

	// Before we continue, init the ignore peers map.
	ignoreList := s.createBootstrapIgnorePeers()

	// We'll start off by aggressively attempting connections to peers in
	// order to be a part of the network as soon as possible.
	s.initialPeerBootstrap(ctx, ignoreList, numTargetPeers, bootstrappers)

	// Once done, we'll attempt to maintain our target minimum number of
	// peers.
	//
	// We'll use a 15 second backoff, and double the time every time an
	// epoch fails up to a ceiling.
	backOff := time.Second * 15

	// We'll create a new ticker to wake us up every 15 seconds so we can
	// see if we've reached our minimum number of peers.
	sampleTicker := time.NewTicker(backOff)
	defer sampleTicker.Stop()

	// We'll use the number of attempts and errors to determine if we need
	// to increase the time between discovery epochs.
	var epochErrors uint32 // To be used atomically.
	var epochAttempts uint32

	for {
		select {
		// The ticker has just woken us up, so we'll need to check if
		// we need to attempt to connect our to any more peers.
		case <-sampleTicker.C:
			// Obtain the current number of peers, so we can gauge
			// if we need to sample more peers or not.
			s.mu.RLock()
			numActivePeers := uint32(len(s.peersByPub))
			s.mu.RUnlock()

			// If we have enough peers, then we can loop back
			// around to the next round as we're done here.
			if numActivePeers >= numTargetPeers {
				continue
			}

			// If all of our attempts failed during this last back
			// off period, then will increase our backoff to 5
			// minute ceiling to avoid an excessive number of
			// queries
			//
			// TODO(roasbeef): add reverse policy too?

			if epochAttempts > 0 &&
				atomic.LoadUint32(&epochErrors) >= epochAttempts {

				sampleTicker.Stop()

				backOff *= 2
				if backOff > bootstrapBackOffCeiling {
					backOff = bootstrapBackOffCeiling
				}

				srvrLog.Debugf("Backing off peer bootstrapper to "+
					"%v", backOff)
				sampleTicker = time.NewTicker(backOff)
				continue
			}

			atomic.StoreUint32(&epochErrors, 0)
			epochAttempts = 0

			// Since we know need more peers, we'll compute the
			// exact number we need to reach our threshold.
			numNeeded := numTargetPeers - numActivePeers

			srvrLog.Debugf("Attempting to obtain %v more network "+
				"peers", numNeeded)

			// With the number of peers we need calculated, we'll
			// query the network bootstrappers to sample a set of
			// random addrs for us.
			//
			// Before we continue, get a copy of the ignore peers
			// map.
			ignoreList = s.createBootstrapIgnorePeers()

			peerAddrs, err := discovery.MultiSourceBootstrap(
				ctx, ignoreList, numNeeded*2, bootstrappers...,
			)
			if err != nil {
				srvrLog.Errorf("Unable to retrieve bootstrap "+
					"peers: %v", err)
				continue
			}

			// Finally, we'll launch a new goroutine for each
			// prospective peer candidates.
			for _, addr := range peerAddrs {
				epochAttempts++

				go func(a *lnwire.NetAddress) {
					// TODO(roasbeef): can do AS, subnet,
					// country diversity, etc
					errChan := make(chan error, 1)
					s.connectToPeer(
						a, errChan,
						s.cfg.ConnectionTimeout,
					)
					select {
					case err := <-errChan:
						if err == nil {
							return
						}

						srvrLog.Errorf("Unable to "+
							"connect to %v: %v",
							a, err)
						atomic.AddUint32(&epochErrors, 1)
					case <-s.quit:
					}
				}(addr)
			}
		case <-s.quit:
			return
		}
	}
}

// bootstrapBackOffCeiling is the maximum amount of time we'll wait between
// failed attempts to locate a set of bootstrap peers. We'll slowly double our
// query back off each time we encounter a failure.
const bootstrapBackOffCeiling = time.Minute * 5

// initialPeerBootstrap attempts to continuously connect to peers on startup
// until the target number of peers has been reached. This ensures that nodes
// receive an up to date network view as soon as possible.
func (s *server) initialPeerBootstrap(ctx context.Context,
	ignore map[autopilot.NodeID]struct{}, numTargetPeers uint32,
	bootstrappers []discovery.NetworkPeerBootstrapper) {

	srvrLog.Debugf("Init bootstrap with targetPeers=%v, bootstrappers=%v, "+
		"ignore=%v", numTargetPeers, len(bootstrappers), len(ignore))

	// We'll start off by waiting 2 seconds between failed attempts, then
	// double each time we fail until we hit the bootstrapBackOffCeiling.
	var delaySignal <-chan time.Time
	delayTime := time.Second * 2

	// As want to be more aggressive, we'll use a lower back off celling
	// then the main peer bootstrap logic.
	backOffCeiling := bootstrapBackOffCeiling / 5

	for attempts := 0; ; attempts++ {
		// Check if the server has been requested to shut down in order
		// to prevent blocking.
		if s.Stopped() {
			return
		}

		// We can exit our aggressive initial peer bootstrapping stage
		// if we've reached out target number of peers.
		s.mu.RLock()
		numActivePeers := uint32(len(s.peersByPub))
		s.mu.RUnlock()

		if numActivePeers >= numTargetPeers {
			return
		}

		if attempts > 0 {
			srvrLog.Debugf("Waiting %v before trying to locate "+
				"bootstrap peers (attempt #%v)", delayTime,
				attempts)

			// We've completed at least one iterating and haven't
			// finished, so we'll start to insert a delay period
			// between each attempt.
			delaySignal = time.After(delayTime)
			select {
			case <-delaySignal:
			case <-s.quit:
				return
			}

			// After our delay, we'll double the time we wait up to
			// the max back off period.
			delayTime *= 2
			if delayTime > backOffCeiling {
				delayTime = backOffCeiling
			}
		}

		// Otherwise, we'll request for the remaining number of peers
		// in order to reach our target.
		peersNeeded := numTargetPeers - numActivePeers
		bootstrapAddrs, err := discovery.MultiSourceBootstrap(
			ctx, ignore, peersNeeded, bootstrappers...,
		)
		if err != nil {
			srvrLog.Errorf("Unable to retrieve initial bootstrap "+
				"peers: %v", err)
			continue
		}

		// Then, we'll attempt to establish a connection to the
		// different peer addresses retrieved by our bootstrappers.
		var wg sync.WaitGroup
		for _, bootstrapAddr := range bootstrapAddrs {
			wg.Add(1)
			go func(addr *lnwire.NetAddress) {
				defer wg.Done()

				errChan := make(chan error, 1)
				go s.connectToPeer(
					addr, errChan, s.cfg.ConnectionTimeout,
				)

				// We'll only allow this connection attempt to
				// take up to 3 seconds. This allows us to move
				// quickly by discarding peers that are slowing
				// us down.
				select {
				case err := <-errChan:
					if err == nil {
						return
					}
					srvrLog.Errorf("Unable to connect to "+
						"%v: %v", addr, err)
				// TODO: tune timeout? 3 seconds might be *too*
				// aggressive but works well.
				case <-time.After(3 * time.Second):
					srvrLog.Tracef("Skipping peer %v due "+
						"to not establishing a "+
						"connection within 3 seconds",
						addr)
				case <-s.quit:
				}
			}(bootstrapAddr)
		}

		wg.Wait()
	}
}

// createNewHiddenService automatically sets up a v2 or v3 onion service in
// order to listen for inbound connections over Tor.
func (s *server) createNewHiddenService(ctx context.Context) error {
	// Determine the different ports the server is listening on. The onion
	// service's virtual port will map to these ports and one will be picked
	// at random when the onion service is being accessed.
	listenPorts := make([]int, 0, len(s.listenAddrs))
	for _, listenAddr := range s.listenAddrs {
		port := listenAddr.(*net.TCPAddr).Port
		listenPorts = append(listenPorts, port)
	}

	encrypter, err := lnencrypt.KeyRingEncrypter(s.cc.KeyRing)
	if err != nil {
		return err
	}

	// Once the port mapping has been set, we can go ahead and automatically
	// create our onion service. The service's private key will be saved to
	// disk in order to regain access to this service when restarting `lnd`.
	onionCfg := tor.AddOnionConfig{
		VirtualPort: defaultPeerPort,
		TargetPorts: listenPorts,
		Store: tor.NewOnionFile(
			s.cfg.Tor.PrivateKeyPath, 0600, s.cfg.Tor.EncryptKey,
			encrypter,
		),
	}

	switch {
	case s.cfg.Tor.V2:
		onionCfg.Type = tor.V2
	case s.cfg.Tor.V3:
		onionCfg.Type = tor.V3
	}

	addr, err := s.torController.AddOnion(onionCfg)
	if err != nil {
		return err
	}

	// Now that the onion service has been created, we'll add the onion
	// address it can be reached at to our list of advertised addresses.
	newNodeAnn, err := s.genNodeAnnouncement(
		nil, func(currentAnn *lnwire.NodeAnnouncement1) {
			currentAnn.Addresses = append(currentAnn.Addresses, addr)
		},
	)
	if err != nil {
		return fmt.Errorf("unable to generate new node "+
			"announcement: %v", err)
	}

	// Finally, we'll update the on-disk version of our announcement so it
	// will eventually propagate to nodes in the network.
	selfNode := models.NewV1Node(
		route.NewVertex(s.identityECDH.PubKey()), &models.NodeV1Fields{
			Addresses:    newNodeAnn.Addresses,
			Features:     newNodeAnn.Features,
			AuthSigBytes: newNodeAnn.Signature.ToSignatureBytes(),
			Color:        newNodeAnn.RGBColor,
			Alias:        newNodeAnn.Alias.String(),
			LastUpdate:   time.Unix(int64(newNodeAnn.Timestamp), 0),
		},
	)

	if err := s.graphDB.SetSourceNode(ctx, selfNode); err != nil {
		return fmt.Errorf("can't set self node: %w", err)
	}

	return nil
}

// findChannel finds a channel given a public key and ChannelID. It is an
// optimization that is quicker than seeking for a channel given only the
// ChannelID.
func (s *server) findChannel(node *btcec.PublicKey, chanID lnwire.ChannelID) (
	*channeldb.OpenChannel, error) {

	nodeChans, err := s.chanStateDB.FetchOpenChannels(node)
	if err != nil {
		return nil, err
	}

	for _, channel := range nodeChans {
		if chanID.IsChanPoint(&channel.FundingOutpoint) {
			return channel, nil
		}
	}

	return nil, fmt.Errorf("unable to find channel")
}

// getNodeAnnouncement fetches the current, fully signed node announcement.
func (s *server) getNodeAnnouncement() lnwire.NodeAnnouncement1 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return *s.currentNodeAnn
}

// genNodeAnnouncement generates and returns the current fully signed node
// announcement. The time stamp of the announcement will be updated in order
// to ensure it propagates through the network.
func (s *server) genNodeAnnouncement(features *lnwire.RawFeatureVector,
	modifiers ...netann.NodeAnnModifier) (lnwire.NodeAnnouncement1, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a shallow copy of the current node announcement to work on.
	// This ensures the original announcement remains unchanged
	// until the new announcement is fully signed and valid.
	newNodeAnn := *s.currentNodeAnn

	// First, try to update our feature manager with the updated set of
	// features.
	if features != nil {
		proposedFeatures := map[feature.Set]*lnwire.RawFeatureVector{
			feature.SetNodeAnn: features,
		}
		err := s.featureMgr.UpdateFeatureSets(proposedFeatures)
		if err != nil {
			return lnwire.NodeAnnouncement1{}, err
		}

		// If we could successfully update our feature manager, add
		// an update modifier to include these new features to our
		// set.
		modifiers = append(
			modifiers, netann.NodeAnnSetFeatures(features),
		)
	}

	// Always update the timestamp when refreshing to ensure the update
	// propagates.
	modifiers = append(modifiers, netann.NodeAnnSetTimestamp)

	// Apply the requested changes to the node announcement.
	for _, modifier := range modifiers {
		modifier(&newNodeAnn)
	}

	// Sign a new update after applying all of the passed modifiers.
	err := netann.SignNodeAnnouncement(
		s.nodeSigner, s.identityKeyLoc, &newNodeAnn,
	)
	if err != nil {
		return lnwire.NodeAnnouncement1{}, err
	}

	// If signing succeeds, update the current announcement.
	*s.currentNodeAnn = newNodeAnn

	return *s.currentNodeAnn, nil
}

// updateAndBroadcastSelfNode generates a new node announcement
// applying the giving modifiers and updating the time stamp
// to ensure it propagates through the network. Then it broadcasts
// it to the network.
func (s *server) updateAndBroadcastSelfNode(ctx context.Context,
	features *lnwire.RawFeatureVector,
	modifiers ...netann.NodeAnnModifier) error {

	newNodeAnn, err := s.genNodeAnnouncement(features, modifiers...)
	if err != nil {
		return fmt.Errorf("unable to generate new node "+
			"announcement: %v", err)
	}

	// Update the on-disk version of our announcement.
	// Load and modify self node istead of creating anew instance so we
	// don't risk overwriting any existing values.
	selfNode, err := s.graphDB.SourceNode(ctx)
	if err != nil {
		return fmt.Errorf("unable to get current source node: %w", err)
	}

	selfNode.LastUpdate = time.Unix(int64(newNodeAnn.Timestamp), 0)
	selfNode.Addresses = newNodeAnn.Addresses
	selfNode.Alias = fn.Some(newNodeAnn.Alias.String())
	selfNode.Features = s.featureMgr.Get(feature.SetNodeAnn)
	selfNode.Color = fn.Some(newNodeAnn.RGBColor)
	selfNode.AuthSigBytes = newNodeAnn.Signature.ToSignatureBytes()

	copy(selfNode.PubKeyBytes[:], s.identityECDH.PubKey().SerializeCompressed())

	if err := s.graphDB.SetSourceNode(ctx, selfNode); err != nil {
		return fmt.Errorf("can't set self node: %w", err)
	}

	// Finally, propagate it to the nodes in the network.
	err = s.BroadcastMessage(nil, &newNodeAnn)
	if err != nil {
		rpcsLog.Debugf("Unable to broadcast new node "+
			"announcement to peers: %v", err)
		return err
	}

	return nil
}

type nodeAddresses struct {
	pubKey    *btcec.PublicKey
	addresses []net.Addr
}

// establishPersistentConnections attempts to establish persistent connections
// to all our direct channel collaborators. In order to promote liveness of our
// active channels, we instruct the connection manager to attempt to establish
// and maintain persistent connections to all our direct channel counterparties.
func (s *server) establishPersistentConnections(ctx context.Context) error {
	// nodeAddrsMap stores the combination of node public keys and addresses
	// that we'll attempt to reconnect to. PubKey strings are used as keys
	// since other PubKey forms can't be compared.
	nodeAddrsMap := make(map[string]*nodeAddresses)

	// Iterate through the list of LinkNodes to find addresses we should
	// attempt to connect to based on our set of previous connections. Set
	// the reconnection port to the default peer port.
	linkNodes, err := s.chanStateDB.LinkNodeDB().FetchAllLinkNodes()
	if err != nil && !errors.Is(err, channeldb.ErrLinkNodesNotFound) {
		return fmt.Errorf("failed to fetch all link nodes: %w", err)
	}

	for _, node := range linkNodes {
		pubStr := string(node.IdentityPub.SerializeCompressed())
		nodeAddrs := &nodeAddresses{
			pubKey:    node.IdentityPub,
			addresses: node.Addresses,
		}
		nodeAddrsMap[pubStr] = nodeAddrs
	}

	// After checking our previous connections for addresses to connect to,
	// iterate through the nodes in our channel graph to find addresses
	// that have been added via NodeAnnouncement1 messages.
	// TODO(roasbeef): instead iterate over link nodes and query graph for
	// each of the nodes.
	graphAddrs := make(map[string]*nodeAddresses)
	forEachSrcNodeChan := func(chanPoint wire.OutPoint,
		havePolicy bool, channelPeer *models.Node) error {

		// If the remote party has announced the channel to us, but we
		// haven't yet, then we won't have a policy. However, we don't
		// need this to connect to the peer, so we'll log it and move on.
		if !havePolicy {
			srvrLog.Warnf("No channel policy found for "+
				"ChannelPoint(%v): ", chanPoint)
		}

		pubStr := string(channelPeer.PubKeyBytes[:])

		// Add all unique addresses from channel
		// graph/NodeAnnouncements to the list of addresses we'll
		// connect to for this peer.
		addrSet := make(map[string]net.Addr)
		for _, addr := range channelPeer.Addresses {
			switch addr.(type) {
			case *net.TCPAddr:
				addrSet[addr.String()] = addr

			// We'll only attempt to connect to Tor addresses if Tor
			// outbound support is enabled.
			case *tor.OnionAddr:
				if s.cfg.Tor.Active {
					addrSet[addr.String()] = addr
				}
			}
		}

		// If this peer is also recorded as a link node, we'll add any
		// additional addresses that have not already been selected.
		linkNodeAddrs, ok := nodeAddrsMap[pubStr]
		if ok {
			for _, lnAddress := range linkNodeAddrs.addresses {
				switch lnAddress.(type) {
				case *net.TCPAddr:
					addrSet[lnAddress.String()] = lnAddress

				// We'll only attempt to connect to Tor
				// addresses if Tor outbound support is enabled.
				case *tor.OnionAddr:
					if s.cfg.Tor.Active {
						//nolint:ll
						addrSet[lnAddress.String()] = lnAddress
					}
				}
			}
		}

		// Construct a slice of the deduped addresses.
		var addrs []net.Addr
		for _, addr := range addrSet {
			addrs = append(addrs, addr)
		}

		n := &nodeAddresses{
			addresses: addrs,
		}
		n.pubKey, err = channelPeer.PubKey()
		if err != nil {
			return err
		}

		graphAddrs[pubStr] = n
		return nil
	}
	err = s.graphDB.ForEachSourceNodeChannel(
		ctx, forEachSrcNodeChan, func() {
			clear(graphAddrs)
		},
	)
	if err != nil {
		srvrLog.Errorf("Failed to iterate over source node channels: "+
			"%v", err)

		if !errors.Is(err, graphdb.ErrGraphNoEdgesFound) &&
			!errors.Is(err, graphdb.ErrEdgeNotFound) {

			return err
		}
	}

	// Combine the addresses from the link nodes and the channel graph.
	for pubStr, nodeAddr := range graphAddrs {
		nodeAddrsMap[pubStr] = nodeAddr
	}

	srvrLog.Debugf("Establishing %v persistent connections on start",
		len(nodeAddrsMap))

	// Acquire and hold server lock until all persistent connection requests
	// have been recorded and sent to the connection manager.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Iterate through the combined list of addresses from prior links and
	// node announcements and attempt to reconnect to each node.
	var numOutboundConns int
	for pubStr, nodeAddr := range nodeAddrsMap {
		// Add this peer to the set of peers we should maintain a
		// persistent connection with. We set the value to false to
		// indicate that we should not continue to reconnect if the
		// number of channels returns to zero, since this peer has not
		// been requested as perm by the user.
		s.persistentPeers[pubStr] = false
		if _, ok := s.persistentPeersBackoff[pubStr]; !ok {
			s.persistentPeersBackoff[pubStr] = s.cfg.MinBackoff
		}

		for _, address := range nodeAddr.addresses {
			// Create a wrapper address which couples the IP and
			// the pubkey so the brontide authenticated connection
			// can be established.
			lnAddr := &lnwire.NetAddress{
				IdentityKey: nodeAddr.pubKey,
				Address:     address,
			}

			s.persistentPeerAddrs[pubStr] = append(
				s.persistentPeerAddrs[pubStr], lnAddr)
		}

		// We'll connect to the first 10 peers immediately, then
		// randomly stagger any remaining connections if the
		// stagger initial reconnect flag is set. This ensures
		// that mobile nodes or nodes with a small number of
		// channels obtain connectivity quickly, but larger
		// nodes are able to disperse the costs of connecting to
		// all peers at once.
		if numOutboundConns < numInstantInitReconnect ||
			!s.cfg.StaggerInitialReconnect {

			go s.connectToPersistentPeer(pubStr)
		} else {
			go s.delayInitialReconnect(pubStr)
		}

		numOutboundConns++
	}

	return nil
}

// delayInitialReconnect will attempt a reconnection to the given peer after
// sampling a value for the delay between 0s and the maxInitReconnectDelay.
//
// NOTE: This method MUST be run as a goroutine.
func (s *server) delayInitialReconnect(pubStr string) {
	delay := time.Duration(prand.Intn(maxInitReconnectDelay)) * time.Second
	select {
	case <-time.After(delay):
		s.connectToPersistentPeer(pubStr)
	case <-s.quit:
	}
}

// prunePersistentPeerConnection removes all internal state related to
// persistent connections to a peer within the server. This is used to avoid
// persistent connection retries to peers we do not have any open channels with.
func (s *server) prunePersistentPeerConnection(compressedPubKey [33]byte) {
	pubKeyStr := string(compressedPubKey[:])

	s.mu.Lock()
	if perm, ok := s.persistentPeers[pubKeyStr]; ok && !perm {
		delete(s.persistentPeers, pubKeyStr)
		delete(s.persistentPeersBackoff, pubKeyStr)
		delete(s.persistentPeerAddrs, pubKeyStr)
		s.cancelConnReqs(pubKeyStr, nil)
		s.mu.Unlock()

		srvrLog.Infof("Pruned peer %x from persistent connections, "+
			"peer has no open channels", compressedPubKey)

		return
	}
	s.mu.Unlock()
}

// bannedPersistentPeerConnection does not actually "ban" a persistent peer. It
// is instead used to remove persistent peer state for a peer that has been
// disconnected for good cause by the server. Currently, a gossip ban from
// sending garbage and the server running out of restricted-access
// (i.e. "free") connection slots are the only way this logic gets hit. In the
// future, this function may expand when more ban criteria is added.
//
// NOTE: The server's write lock MUST be held when this is called.
func (s *server) bannedPersistentPeerConnection(remotePub string) {
	if perm, ok := s.persistentPeers[remotePub]; ok && !perm {
		delete(s.persistentPeers, remotePub)
		delete(s.persistentPeersBackoff, remotePub)
		delete(s.persistentPeerAddrs, remotePub)
		s.cancelConnReqs(remotePub, nil)
	}
}

// BroadcastMessage sends a request to the server to broadcast a set of
// messages to all peers other than the one specified by the `skips` parameter.
// All messages sent via BroadcastMessage will be queued for lazy delivery to
// the target peers.
//
// NOTE: This function is safe for concurrent access.
func (s *server) BroadcastMessage(skips map[route.Vertex]struct{},
	msgs ...lnwire.Message) error {

	// Filter out peers found in the skips map. We synchronize access to
	// peersByPub throughout this process to ensure we deliver messages to
	// exact set of peers present at the time of invocation.
	s.mu.RLock()
	peers := make([]*peer.Brontide, 0, len(s.peersByPub))
	for pubStr, sPeer := range s.peersByPub {
		if skips != nil {
			if _, ok := skips[sPeer.PubKey()]; ok {
				srvrLog.Tracef("Skipping %x in broadcast with "+
					"pubStr=%x", sPeer.PubKey(), pubStr)
				continue
			}
		}

		peers = append(peers, sPeer)
	}
	s.mu.RUnlock()

	// Iterate over all known peers, dispatching a go routine to enqueue
	// all messages to each of peers.
	var wg sync.WaitGroup
	for _, sPeer := range peers {
		srvrLog.Debugf("Sending %v messages to peer %x", len(msgs),
			sPeer.PubKey())

		// Dispatch a go routine to enqueue all messages to this peer.
		wg.Add(1)
		s.wg.Add(1)
		go func(p lnpeer.Peer) {
			defer s.wg.Done()
			defer wg.Done()

			p.SendMessageLazy(false, msgs...)
		}(sPeer)
	}

	// Wait for all messages to have been dispatched before returning to
	// caller.
	wg.Wait()

	return nil
}

// NotifyWhenOnline can be called by other subsystems to get notified when a
// particular peer comes online. The peer itself is sent across the peerChan.
//
// NOTE: This function is safe for concurrent access.
func (s *server) NotifyWhenOnline(peerKey [33]byte,
	peerChan chan<- lnpeer.Peer) {

	s.mu.Lock()

	// Compute the target peer's identifier.
	pubStr := string(peerKey[:])

	// Check if peer is connected.
	peer, ok := s.peersByPub[pubStr]
	if ok {
		// Unlock here so that the mutex isn't held while we are
		// waiting for the peer to become active.
		s.mu.Unlock()

		// Wait until the peer signals that it is actually active
		// rather than only in the server's maps.
		select {
		case <-peer.ActiveSignal():
		case <-peer.QuitSignal():
			// The peer quit, so we'll add the channel to the slice
			// and return.
			s.mu.Lock()
			s.peerConnectedListeners[pubStr] = append(
				s.peerConnectedListeners[pubStr], peerChan,
			)
			s.mu.Unlock()
			return
		}

		// Connected, can return early.
		srvrLog.Debugf("Notifying that peer %x is online", peerKey)

		select {
		case peerChan <- peer:
		case <-s.quit:
		}

		return
	}

	// Not connected, store this listener such that it can be notified when
	// the peer comes online.
	s.peerConnectedListeners[pubStr] = append(
		s.peerConnectedListeners[pubStr], peerChan,
	)
	s.mu.Unlock()
}

// NotifyWhenOffline delivers a notification to the caller of when the peer with
// the given public key has been disconnected. The notification is signaled by
// closing the channel returned.
func (s *server) NotifyWhenOffline(peerPubKey [33]byte) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := make(chan struct{})

	// If the peer is already offline, we can immediately trigger the
	// notification.
	peerPubKeyStr := string(peerPubKey[:])
	if _, ok := s.peersByPub[peerPubKeyStr]; !ok {
		srvrLog.Debugf("Notifying that peer %x is offline", peerPubKey)
		close(c)
		return c
	}

	// Otherwise, the peer is online, so we'll keep track of the channel to
	// trigger the notification once the server detects the peer
	// disconnects.
	s.peerDisconnectedListeners[peerPubKeyStr] = append(
		s.peerDisconnectedListeners[peerPubKeyStr], c,
	)

	return c
}

// FindPeer will return the peer that corresponds to the passed in public key.
// This function is used by the funding manager, allowing it to update the
// daemon's local representation of the remote peer.
//
// NOTE: This function is safe for concurrent access.
func (s *server) FindPeer(peerKey *btcec.PublicKey) (*peer.Brontide, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pubStr := string(peerKey.SerializeCompressed())

	return s.findPeerByPubStr(pubStr)
}

// FindPeerByPubStr will return the peer that corresponds to the passed peerID,
// which should be a string representation of the peer's serialized, compressed
// public key.
//
// NOTE: This function is safe for concurrent access.
func (s *server) FindPeerByPubStr(pubStr string) (*peer.Brontide, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.findPeerByPubStr(pubStr)
}

// findPeerByPubStr is an internal method that retrieves the specified peer from
// the server's internal state using.
func (s *server) findPeerByPubStr(pubStr string) (*peer.Brontide, error) {
	peer, ok := s.peersByPub[pubStr]
	if !ok {
		return nil, ErrPeerNotConnected
	}

	return peer, nil
}

// nextPeerBackoff computes the next backoff duration for a peer's pubkey using
// exponential backoff. If no previous backoff was known, the default is
// returned.
func (s *server) nextPeerBackoff(pubStr string,
	startTime time.Time) time.Duration {

	// Now, determine the appropriate backoff to use for the retry.
	backoff, ok := s.persistentPeersBackoff[pubStr]
	if !ok {
		// If an existing backoff was unknown, use the default.
		return s.cfg.MinBackoff
	}

	// If the peer failed to start properly, we'll just use the previous
	// backoff to compute the subsequent randomized exponential backoff
	// duration. This will roughly double on average.
	if startTime.IsZero() {
		return computeNextBackoff(backoff, s.cfg.MaxBackoff)
	}

	// The peer succeeded in starting. If the connection didn't last long
	// enough to be considered stable, we'll continue to back off retries
	// with this peer.
	connDuration := time.Since(startTime)
	if connDuration < defaultStableConnDuration {
		return computeNextBackoff(backoff, s.cfg.MaxBackoff)
	}

	// The peer succeed in starting and this was stable peer, so we'll
	// reduce the timeout duration by the length of the connection after
	// applying randomized exponential backoff. We'll only apply this in the
	// case that:
	//   reb(curBackoff) - connDuration > cfg.MinBackoff
	relaxedBackoff := computeNextBackoff(backoff, s.cfg.MaxBackoff) - connDuration
	if relaxedBackoff > s.cfg.MinBackoff {
		return relaxedBackoff
	}

	// Lastly, if reb(currBackoff) - connDuration <= cfg.MinBackoff, meaning
	// the stable connection lasted much longer than our previous backoff.
	// To reward such good behavior, we'll reconnect after the default
	// timeout.
	return s.cfg.MinBackoff
}

// shouldDropLocalConnection determines if our local connection to a remote peer
// should be dropped in the case of concurrent connection establishment. In
// order to deterministically decide which connection should be dropped, we'll
// utilize the ordering of the local and remote public key. If we didn't use
// such a tie breaker, then we risk _both_ connections erroneously being
// dropped.
func shouldDropLocalConnection(local, remote *btcec.PublicKey) bool {
	localPubBytes := local.SerializeCompressed()
	remotePubPbytes := remote.SerializeCompressed()

	// The connection that comes from the node with a "smaller" pubkey
	// should be kept. Therefore, if our pubkey is "greater" than theirs, we
	// should drop our established connection.
	return bytes.Compare(localPubBytes, remotePubPbytes) > 0
}

// InboundPeerConnected initializes a new peer in response to a new inbound
// connection.
//
// NOTE: This function is safe for concurrent access.
func (s *server) InboundPeerConnected(conn net.Conn) {
	// Exit early if we have already been instructed to shutdown, this
	// prevents any delayed callbacks from accidentally registering peers.
	if s.Stopped() {
		return
	}

	nodePub := conn.(*brontide.Conn).RemotePub()
	pubSer := nodePub.SerializeCompressed()
	pubStr := string(pubSer)

	var pubBytes [33]byte
	copy(pubBytes[:], pubSer)

	s.mu.Lock()
	defer s.mu.Unlock()

	// If we already have an outbound connection to this peer, then ignore
	// this new connection.
	if p, ok := s.outboundPeers[pubStr]; ok {
		srvrLog.Debugf("Already have outbound connection for %v, "+
			"ignoring inbound connection from local=%v, remote=%v",
			p, conn.LocalAddr(), conn.RemoteAddr())

		conn.Close()
		return
	}

	// If we already have a valid connection that is scheduled to take
	// precedence once the prior peer has finished disconnecting, we'll
	// ignore this connection.
	if p, ok := s.scheduledPeerConnection[pubStr]; ok {
		srvrLog.Debugf("Ignoring connection from %v, peer %v already "+
			"scheduled", conn.RemoteAddr(), p)
		conn.Close()
		return
	}

	srvrLog.Infof("New inbound connection from %v", conn.RemoteAddr())

	// Check to see if we already have a connection with this peer. If so,
	// we may need to drop our existing connection. This prevents us from
	// having duplicate connections to the same peer. We forgo adding a
	// default case as we expect these to be the only error values returned
	// from findPeerByPubStr.
	connectedPeer, err := s.findPeerByPubStr(pubStr)
	switch err {
	case ErrPeerNotConnected:
		// We were unable to locate an existing connection with the
		// target peer, proceed to connect.
		s.cancelConnReqs(pubStr, nil)
		s.peerConnected(conn, nil, true)

	case nil:
		ctx := btclog.WithCtx(
			context.TODO(),
			lnutils.LogPubKey("peer", connectedPeer.IdentityKey()),
		)

		// We already have a connection with the incoming peer. If the
		// connection we've already established should be kept and is
		// not of the same type of the new connection (inbound), then
		// we'll close out the new connection s.t there's only a single
		// connection between us.
		localPub := s.identityECDH.PubKey()
		if !connectedPeer.Inbound() &&
			!shouldDropLocalConnection(localPub, nodePub) {

			srvrLog.WarnS(ctx, "Received inbound connection from "+
				"peer, but already have outbound "+
				"connection, dropping conn",
				fmt.Errorf("already have outbound conn"))
			conn.Close()
			return
		}

		// Otherwise, if we should drop the connection, then we'll
		// disconnect our already connected peer.
		srvrLog.DebugS(ctx, "Disconnecting stale connection")

		s.cancelConnReqs(pubStr, nil)

		// Remove the current peer from the server's internal state and
		// signal that the peer termination watcher does not need to
		// execute for this peer.
		s.removePeerUnsafe(ctx, connectedPeer)
		s.ignorePeerTermination[connectedPeer] = struct{}{}
		s.scheduledPeerConnection[pubStr] = func() {
			s.peerConnected(conn, nil, true)
		}
	}
}

// OutboundPeerConnected initializes a new peer in response to a new outbound
// connection.
// NOTE: This function is safe for concurrent access.
func (s *server) OutboundPeerConnected(connReq *connmgr.ConnReq, conn net.Conn) {
	// Exit early if we have already been instructed to shutdown, this
	// prevents any delayed callbacks from accidentally registering peers.
	if s.Stopped() {
		return
	}

	nodePub := conn.(*brontide.Conn).RemotePub()
	pubSer := nodePub.SerializeCompressed()
	pubStr := string(pubSer)

	var pubBytes [33]byte
	copy(pubBytes[:], pubSer)

	s.mu.Lock()
	defer s.mu.Unlock()

	// If we already have an inbound connection to this peer, then ignore
	// this new connection.
	if p, ok := s.inboundPeers[pubStr]; ok {
		srvrLog.Debugf("Already have inbound connection for %v, "+
			"ignoring outbound connection from local=%v, remote=%v",
			p, conn.LocalAddr(), conn.RemoteAddr())

		if connReq != nil {
			s.connMgr.Remove(connReq.ID())
		}
		conn.Close()
		return
	}
	if _, ok := s.persistentConnReqs[pubStr]; !ok && connReq != nil {
		srvrLog.Debugf("Ignoring canceled outbound connection")
		s.connMgr.Remove(connReq.ID())
		conn.Close()
		return
	}

	// If we already have a valid connection that is scheduled to take
	// precedence once the prior peer has finished disconnecting, we'll
	// ignore this connection.
	if _, ok := s.scheduledPeerConnection[pubStr]; ok {
		srvrLog.Debugf("Ignoring connection, peer already scheduled")

		if connReq != nil {
			s.connMgr.Remove(connReq.ID())
		}

		conn.Close()
		return
	}

	srvrLog.Infof("Established outbound connection to: %x@%v", pubStr,
		conn.RemoteAddr())

	if connReq != nil {
		// A successful connection was returned by the connmgr.
		// Immediately cancel all pending requests, excluding the
		// outbound connection we just established.
		ignore := connReq.ID()
		s.cancelConnReqs(pubStr, &ignore)
	} else {
		// This was a successful connection made by some other
		// subsystem. Remove all requests being managed by the connmgr.
		s.cancelConnReqs(pubStr, nil)
	}

	// If we already have a connection with this peer, decide whether or not
	// we need to drop the stale connection. We forgo adding a default case
	// as we expect these to be the only error values returned from
	// findPeerByPubStr.
	connectedPeer, err := s.findPeerByPubStr(pubStr)
	switch err {
	case ErrPeerNotConnected:
		// We were unable to locate an existing connection with the
		// target peer, proceed to connect.
		s.peerConnected(conn, connReq, false)

	case nil:
		ctx := btclog.WithCtx(
			context.TODO(),
			lnutils.LogPubKey("peer", connectedPeer.IdentityKey()),
		)

		// We already have a connection with the incoming peer. If the
		// connection we've already established should be kept and is
		// not of the same type of the new connection (outbound), then
		// we'll close out the new connection s.t there's only a single
		// connection between us.
		localPub := s.identityECDH.PubKey()
		if connectedPeer.Inbound() &&
			shouldDropLocalConnection(localPub, nodePub) {

			srvrLog.WarnS(ctx, "Established outbound connection "+
				"to peer, but already have inbound "+
				"connection, dropping conn",
				fmt.Errorf("already have inbound conn"))
			if connReq != nil {
				s.connMgr.Remove(connReq.ID())
			}
			conn.Close()
			return
		}

		// Otherwise, _their_ connection should be dropped. So we'll
		// disconnect the peer and send the now obsolete peer to the
		// server for garbage collection.
		srvrLog.DebugS(ctx, "Disconnecting stale connection")

		// Remove the current peer from the server's internal state and
		// signal that the peer termination watcher does not need to
		// execute for this peer.
		s.removePeerUnsafe(ctx, connectedPeer)
		s.ignorePeerTermination[connectedPeer] = struct{}{}
		s.scheduledPeerConnection[pubStr] = func() {
			s.peerConnected(conn, connReq, false)
		}
	}
}

// UnassignedConnID is the default connection ID that a request can have before
// it actually is submitted to the connmgr.
// TODO(conner): move into connmgr package, or better, add connmgr method for
// generating atomic IDs
const UnassignedConnID uint64 = 0

// cancelConnReqs stops all persistent connection requests for a given pubkey.
// Any attempts initiated by the peerTerminationWatcher are canceled first.
// Afterwards, each connection request removed from the connmgr. The caller can
// optionally specify a connection ID to ignore, which prevents us from
// canceling a successful request. All persistent connreqs for the provided
// pubkey are discarded after the operationjw.
func (s *server) cancelConnReqs(pubStr string, skip *uint64) {
	// First, cancel any lingering persistent retry attempts, which will
	// prevent retries for any with backoffs that are still maturing.
	if cancelChan, ok := s.persistentRetryCancels[pubStr]; ok {
		close(cancelChan)
		delete(s.persistentRetryCancels, pubStr)
	}

	// Next, check to see if we have any outstanding persistent connection
	// requests to this peer. If so, then we'll remove all of these
	// connection requests, and also delete the entry from the map.
	connReqs, ok := s.persistentConnReqs[pubStr]
	if !ok {
		return
	}

	for _, connReq := range connReqs {
		srvrLog.Tracef("Canceling %s:", connReqs)

		// Atomically capture the current request identifier.
		connID := connReq.ID()

		// Skip any zero IDs, this indicates the request has not
		// yet been schedule.
		if connID == UnassignedConnID {
			continue
		}

		// Skip a particular connection ID if instructed.
		if skip != nil && connID == *skip {
			continue
		}

		s.connMgr.Remove(connID)
	}

	delete(s.persistentConnReqs, pubStr)
}

// handleCustomMessage dispatches an incoming custom peers message to
// subscribers.
func (s *server) handleCustomMessage(peer [33]byte, msg *lnwire.Custom) error {
	srvrLog.Debugf("Custom message received: peer=%x, type=%d",
		peer, msg.Type)

	return s.customMessageServer.SendUpdate(&CustomMessage{
		Peer: peer,
		Msg:  msg,
	})
}

// SubscribeCustomMessages subscribes to a stream of incoming custom peer
// messages.
func (s *server) SubscribeCustomMessages() (*subscribe.Client, error) {
	return s.customMessageServer.Subscribe()
}

// SubscribeOnionMessages subscribes to a stream of incoming onion messages.
func (s *server) SubscribeOnionMessages() (*subscribe.Client, error) {
	return s.onionMessageServer.Subscribe()
}

// notifyOpenChannelPeerEvent updates the access manager's maps and then calls
// the channelNotifier's NotifyOpenChannelEvent.
func (s *server) notifyOpenChannelPeerEvent(op wire.OutPoint,
	remotePub *btcec.PublicKey) {

	// Call newOpenChan to update the access manager's maps for this peer.
	if err := s.peerAccessMan.newOpenChan(remotePub); err != nil {
		srvrLog.Errorf("Failed to update peer[%x] access status after "+
			"channel[%v] open", remotePub.SerializeCompressed(), op)
	}

	// Notify subscribers about this open channel event.
	s.channelNotifier.NotifyOpenChannelEvent(op)
}

// notifyPendingOpenChannelPeerEvent updates the access manager's maps and then
// calls the channelNotifier's NotifyPendingOpenChannelEvent.
func (s *server) notifyPendingOpenChannelPeerEvent(op wire.OutPoint,
	pendingChan *channeldb.OpenChannel, remotePub *btcec.PublicKey) {

	// Call newPendingOpenChan to update the access manager's maps for this
	// peer.
	if err := s.peerAccessMan.newPendingOpenChan(remotePub); err != nil {
		srvrLog.Errorf("Failed to update peer[%x] access status after "+
			"channel[%v] pending open",
			remotePub.SerializeCompressed(), op)
	}

	// Notify subscribers about this event.
	s.channelNotifier.NotifyPendingOpenChannelEvent(op, pendingChan)
}

// notifyFundingTimeoutPeerEvent updates the access manager's maps and then
// calls the channelNotifier's NotifyFundingTimeout.
func (s *server) notifyFundingTimeoutPeerEvent(op wire.OutPoint,
	remotePub *btcec.PublicKey) {

	// Call newPendingCloseChan to potentially demote the peer.
	err := s.peerAccessMan.newPendingCloseChan(remotePub)
	if err != nil {
		srvrLog.Errorf("Failed to update peer[%x] access status after "+
			"channel[%v] pending close",
			remotePub.SerializeCompressed(), op)
	}

	if errors.Is(err, ErrNoMoreRestrictedAccessSlots) {
		// If we encounter an error while attempting to disconnect the
		// peer, log the error.
		if dcErr := s.DisconnectPeer(remotePub); dcErr != nil {
			srvrLog.Errorf("Unable to disconnect peer: %v\n", err)
		}
	}

	// Notify subscribers about this event.
	s.channelNotifier.NotifyFundingTimeout(op)
}

// peerConnected is a function that handles initialization a newly connected
// peer by adding it to the server's global list of all active peers, and
// starting all the goroutines the peer needs to function properly. The inbound
// boolean should be true if the peer initiated the connection to us.
func (s *server) peerConnected(conn net.Conn, connReq *connmgr.ConnReq,
	inbound bool) {

	brontideConn := conn.(*brontide.Conn)
	addr := conn.RemoteAddr()
	pubKey := brontideConn.RemotePub()

	// Only restrict access for inbound connections, which means if the
	// remote node's public key is banned or the restricted slots are used
	// up, we will drop the connection.
	//
	// TODO(yy): Consider perform this check in
	// `peerAccessMan.addPeerAccess`.
	access, err := s.peerAccessMan.assignPeerPerms(pubKey)
	if inbound && err != nil {
		pubSer := pubKey.SerializeCompressed()

		// Clean up the persistent peer maps if we're dropping this
		// connection.
		s.bannedPersistentPeerConnection(string(pubSer))

		srvrLog.Debugf("Dropping connection for %x since we are out "+
			"of restricted-access connection slots: %v.", pubSer,
			err)

		conn.Close()

		return
	}

	srvrLog.Infof("Finalizing connection to %x@%s, inbound=%v",
		pubKey.SerializeCompressed(), addr, inbound)

	peerAddr := &lnwire.NetAddress{
		IdentityKey: pubKey,
		Address:     addr,
		ChainNet:    s.cfg.ActiveNetParams.Net,
	}

	// With the brontide connection established, we'll now craft the feature
	// vectors to advertise to the remote node.
	initFeatures := s.featureMgr.Get(feature.SetInit)
	legacyFeatures := s.featureMgr.Get(feature.SetLegacyGlobal)

	// Lookup past error caches for the peer in the server. If no buffer is
	// found, create a fresh buffer.
	pkStr := string(peerAddr.IdentityKey.SerializeCompressed())
	errBuffer, ok := s.peerErrors[pkStr]
	if !ok {
		var err error
		errBuffer, err = queue.NewCircularBuffer(peer.ErrorBufferSize)
		if err != nil {
			srvrLog.Errorf("unable to create peer %v", err)
			return
		}
	}

	// If we directly set the peer.Config TowerClient member to the
	// s.towerClientMgr then in the case that the s.towerClientMgr is nil,
	// the peer.Config's TowerClient member will not evaluate to nil even
	// though the underlying value is nil. To avoid this gotcha which can
	// cause a panic, we need to explicitly pass nil to the peer.Config's
	// TowerClient if needed.
	var towerClient wtclient.ClientManager
	if s.towerClientMgr != nil {
		towerClient = s.towerClientMgr
	}

	thresholdSats := btcutil.Amount(s.cfg.MaxFeeExposure)
	thresholdMSats := lnwire.NewMSatFromSatoshis(thresholdSats)

	// Now that we've established a connection, create a peer, and it to the
	// set of currently active peers. Configure the peer with the incoming
	// and outgoing broadcast deltas to prevent htlcs from being accepted or
	// offered that would trigger channel closure. In case of outgoing
	// htlcs, an extra block is added to prevent the channel from being
	// closed when the htlc is outstanding and a new block comes in.
	pCfg := peer.Config{
		Conn:                    brontideConn,
		ConnReq:                 connReq,
		Addr:                    peerAddr,
		Inbound:                 inbound,
		Features:                initFeatures,
		LegacyFeatures:          legacyFeatures,
		OutgoingCltvRejectDelta: lncfg.DefaultOutgoingCltvRejectDelta,
		ChanActiveTimeout:       s.cfg.ChanEnableTimeout,
		ErrorBuffer:             errBuffer,
		WritePool:               s.writePool,
		ReadPool:                s.readPool,
		Switch:                  s.htlcSwitch,
		InterceptSwitch:         s.interceptableSwitch,
		ChannelDB:               s.chanStateDB,
		ChannelGraph:            s.graphDB,
		ChainArb:                s.chainArb,
		AuthGossiper:            s.authGossiper,
		ChanStatusMgr:           s.chanStatusMgr,
		ChainIO:                 s.cc.ChainIO,
		FeeEstimator:            s.cc.FeeEstimator,
		Signer:                  s.cc.Wallet.Cfg.Signer,
		SigPool:                 s.sigPool,
		Wallet:                  s.cc.Wallet,
		ChainNotifier:           s.cc.ChainNotifier,
		BestBlockView:           s.cc.BestBlockTracker,
		RoutingPolicy:           s.cc.RoutingPolicy,
		Sphinx:                  s.sphinx,
		WitnessBeacon:           s.witnessBeacon,
		Invoices:                s.invoices,
		ChannelNotifier:         s.channelNotifier,
		HtlcNotifier:            s.htlcNotifier,
		TowerClient:             towerClient,
		DisconnectPeer:          s.DisconnectPeer,
		OnionMessageServer:      s.onionMessageServer,
		GenNodeAnnouncement: func(...netann.NodeAnnModifier) (
			lnwire.NodeAnnouncement1, error) {

			return s.genNodeAnnouncement(nil)
		},

		PongBuf: s.pongBuf,

		PrunePersistentPeerConnection: s.prunePersistentPeerConnection,

		FetchLastChanUpdate: s.fetchLastChanUpdate(),

		FundingManager: s.fundingMgr,

		Hodl:                    s.cfg.Hodl,
		UnsafeReplay:            s.cfg.UnsafeReplay,
		MaxOutgoingCltvExpiry:   s.cfg.MaxOutgoingCltvExpiry,
		MaxChannelFeeAllocation: s.cfg.MaxChannelFeeAllocation,
		CoopCloseTargetConfs:    s.cfg.CoopCloseTargetConfs,
		MaxAnchorsCommitFeeRate: chainfee.SatPerKVByte(
			s.cfg.MaxCommitFeeRateAnchors * 1000).FeePerKWeight(),
		ChannelCommitInterval:  s.cfg.ChannelCommitInterval,
		PendingCommitInterval:  s.cfg.PendingCommitInterval,
		ChannelCommitBatchSize: s.cfg.ChannelCommitBatchSize,
		HandleCustomMessage:    s.handleCustomMessage,
		GetAliases:             s.aliasMgr.GetAliases,
		RequestAlias:           s.aliasMgr.RequestAlias,
		AddLocalAlias:          s.aliasMgr.AddLocalAlias,
		DisallowRouteBlinding:  s.cfg.ProtocolOptions.NoRouteBlinding(),
		DisallowQuiescence:     s.cfg.ProtocolOptions.NoQuiescence(),
		QuiescenceTimeout:      s.cfg.Htlcswitch.QuiescenceTimeout,
		MaxFeeExposure:         thresholdMSats,
		Quit:                   s.quit,
		AuxLeafStore:           s.implCfg.AuxLeafStore,
		AuxSigner:              s.implCfg.AuxSigner,
		MsgRouter:              s.implCfg.MsgRouter,
		AuxChanCloser:          s.implCfg.AuxChanCloser,
		AuxResolver:            s.implCfg.AuxContractResolver,
		AuxTrafficShaper:       s.implCfg.TrafficShaper,
		AuxChannelNegotiator:   s.implCfg.AuxChannelNegotiator,
		ShouldFwdExpEndorsement: func() bool {
			if s.cfg.ProtocolOptions.NoExperimentalEndorsement() {
				return false
			}

			return clock.NewDefaultClock().Now().Before(
				EndorsementExperimentEnd,
			)
		},
		NoDisconnectOnPongFailure: s.cfg.NoDisconnectOnPongFailure,
	}

	copy(pCfg.PubKeyBytes[:], peerAddr.IdentityKey.SerializeCompressed())
	copy(pCfg.ServerPubKey[:], s.identityECDH.PubKey().SerializeCompressed())

	p := peer.NewBrontide(pCfg)

	// Update the access manager with the access permission for this peer.
	s.peerAccessMan.addPeerAccess(pubKey, access, inbound)

	// TODO(roasbeef): update IP address for link-node
	//  * also mark last-seen, do it one single transaction?

	s.addPeer(p)

	// Once we have successfully added the peer to the server, we can
	// delete the previous error buffer from the server's map of error
	// buffers.
	delete(s.peerErrors, pkStr)

	// Dispatch a goroutine to asynchronously start the peer. This process
	// includes sending and receiving Init messages, which would be a DOS
	// vector if we held the server's mutex throughout the procedure.
	s.wg.Add(1)
	go s.peerInitializer(p)
}

// addPeer adds the passed peer to the server's global state of all active
// peers.
func (s *server) addPeer(p *peer.Brontide) {
	if p == nil {
		return
	}

	pubBytes := p.IdentityKey().SerializeCompressed()

	// Ignore new peers if we're shutting down.
	if s.Stopped() {
		srvrLog.Infof("Server stopped, skipped adding peer=%x",
			pubBytes)
		p.Disconnect(ErrServerShuttingDown)

		return
	}

	// Track the new peer in our indexes so we can quickly look it up either
	// according to its public key, or its peer ID.
	// TODO(roasbeef): pipe all requests through to the
	// queryHandler/peerManager

	// NOTE: This pubStr is a raw bytes to string conversion and will NOT
	// be human-readable.
	pubStr := string(pubBytes)

	s.peersByPub[pubStr] = p

	if p.Inbound() {
		s.inboundPeers[pubStr] = p
	} else {
		s.outboundPeers[pubStr] = p
	}

	// Inform the peer notifier of a peer online event so that it can be reported
	// to clients listening for peer events.
	var pubKey [33]byte
	copy(pubKey[:], pubBytes)
}

// peerInitializer asynchronously starts a newly connected peer after it has
// been added to the server's peer map. This method sets up a
// peerTerminationWatcher for the given peer, and ensures that it executes even
// if the peer failed to start. In the event of a successful connection, this
// method reads the negotiated, local feature-bits and spawns the appropriate
// graph synchronization method. Any registered clients of NotifyWhenOnline will
// be signaled of the new peer once the method returns.
//
// NOTE: This MUST be launched as a goroutine.
func (s *server) peerInitializer(p *peer.Brontide) {
	defer s.wg.Done()

	pubBytes := p.IdentityKey().SerializeCompressed()

	// Avoid initializing peers while the server is exiting.
	if s.Stopped() {
		srvrLog.Infof("Server stopped, skipped initializing peer=%x",
			pubBytes)
		return
	}

	// Create a channel that will be used to signal a successful start of
	// the link. This prevents the peer termination watcher from beginning
	// its duty too early.
	ready := make(chan struct{})

	// Before starting the peer, launch a goroutine to watch for the
	// unexpected termination of this peer, which will ensure all resources
	// are properly cleaned up, and re-establish persistent connections when
	// necessary. The peer termination watcher will be short circuited if
	// the peer is ever added to the ignorePeerTermination map, indicating
	// that the server has already handled the removal of this peer.
	s.wg.Add(1)
	go s.peerTerminationWatcher(p, ready)

	// Start the peer! If an error occurs, we Disconnect the peer, which
	// will unblock the peerTerminationWatcher.
	if err := p.Start(); err != nil {
		srvrLog.Warnf("Starting peer=%x got error: %v", pubBytes, err)

		p.Disconnect(fmt.Errorf("unable to start peer: %w", err))
		return
	}

	// Otherwise, signal to the peerTerminationWatcher that the peer startup
	// was successful, and to begin watching the peer's wait group.
	close(ready)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if there are listeners waiting for this peer to come online.
	srvrLog.Debugf("Notifying that peer %v is online", p)

	// TODO(guggero): Do a proper conversion to a string everywhere, or use
	// route.Vertex as the key type of peerConnectedListeners.
	pubStr := string(pubBytes)
	for _, peerChan := range s.peerConnectedListeners[pubStr] {
		select {
		case peerChan <- p:
		case <-s.quit:
			return
		}
	}
	delete(s.peerConnectedListeners, pubStr)

	// Since the peer has been fully initialized, now it's time to notify
	// the RPC about the peer online event.
	s.peerNotifier.NotifyPeerOnline([33]byte(pubBytes))
}

// peerTerminationWatcher waits until a peer has been disconnected unexpectedly,
// and then cleans up all resources allocated to the peer, notifies relevant
// sub-systems of its demise, and finally handles re-connecting to the peer if
// it's persistent. If the server intentionally disconnects a peer, it should
// have a corresponding entry in the ignorePeerTermination map which will cause
// the cleanup routine to exit early. The passed `ready` chan is used to
// synchronize when WaitForDisconnect should begin watching on the peer's
// waitgroup. The ready chan should only be signaled if the peer starts
// successfully, otherwise the peer should be disconnected instead.
//
// NOTE: This MUST be launched as a goroutine.
func (s *server) peerTerminationWatcher(p *peer.Brontide, ready chan struct{}) {
	defer s.wg.Done()

	ctx := btclog.WithCtx(
		context.TODO(), lnutils.LogPubKey("peer", p.IdentityKey()),
	)

	p.WaitForDisconnect(ready)

	srvrLog.DebugS(ctx, "Peer has been disconnected")

	// If the server is exiting then we can bail out early ourselves as all
	// the other sub-systems will already be shutting down.
	if s.Stopped() {
		srvrLog.DebugS(ctx, "Server quitting, exit early for peer")
		return
	}

	// Next, we'll cancel all pending funding reservations with this node.
	// If we tried to initiate any funding flows that haven't yet finished,
	// then we need to unlock those committed outputs so they're still
	// available for use.
	s.fundingMgr.CancelPeerReservations(p.PubKey())

	pubKey := p.IdentityKey()

	// We'll also inform the gossiper that this peer is no longer active,
	// so we don't need to maintain sync state for it any longer.
	s.authGossiper.PruneSyncState(p.PubKey())

	// Tell the switch to remove all links associated with this peer.
	// Passing nil as the target link indicates that all links associated
	// with this interface should be closed.
	//
	// TODO(roasbeef): instead add a PurgeInterfaceLinks function?
	links, err := s.htlcSwitch.GetLinksByInterface(p.PubKey())
	if err != nil && err != htlcswitch.ErrNoLinksFound {
		srvrLog.Errorf("Unable to get channel links for %v: %v", p, err)
	}

	for _, link := range links {
		s.htlcSwitch.RemoveLink(link.ChanID())
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// If there were any notification requests for when this peer
	// disconnected, we can trigger them now.
	srvrLog.DebugS(ctx, "Notifying that peer is offline")
	pubStr := string(pubKey.SerializeCompressed())
	for _, offlineChan := range s.peerDisconnectedListeners[pubStr] {
		close(offlineChan)
	}
	delete(s.peerDisconnectedListeners, pubStr)

	// If the server has already removed this peer, we can short circuit the
	// peer termination watcher and skip cleanup.
	if _, ok := s.ignorePeerTermination[p]; ok {
		delete(s.ignorePeerTermination, p)

		pubKey := p.PubKey()
		pubStr := string(pubKey[:])

		// If a connection callback is present, we'll go ahead and
		// execute it now that previous peer has fully disconnected. If
		// the callback is not present, this likely implies the peer was
		// purposefully disconnected via RPC, and that no reconnect
		// should be attempted.
		connCallback, ok := s.scheduledPeerConnection[pubStr]
		if ok {
			delete(s.scheduledPeerConnection, pubStr)
			connCallback()
		}
		return
	}

	// First, cleanup any remaining state the server has regarding the peer
	// in question.
	s.removePeerUnsafe(ctx, p)

	// Next, check to see if this is a persistent peer or not.
	if _, ok := s.persistentPeers[pubStr]; !ok {
		return
	}

	// Get the last address that we used to connect to the peer.
	addrs := []net.Addr{
		p.NetAddress().Address,
	}

	// We'll ensure that we locate all the peers advertised addresses for
	// reconnection purposes.
	advertisedAddrs, err := s.fetchNodeAdvertisedAddrs(ctx, pubKey)
	switch {
	// We found advertised addresses, so use them.
	case err == nil:
		addrs = advertisedAddrs

	// The peer doesn't have an advertised address.
	case err == errNoAdvertisedAddr:
		// If it is an outbound peer then we fall back to the existing
		// peer address.
		if !p.Inbound() {
			break
		}

		// Fall back to the existing peer address if
		// we're not accepting connections over Tor.
		if s.torController == nil {
			break
		}

		// If we are, the peer's address won't be known
		// to us (we'll see a private address, which is
		// the address used by our onion service to dial
		// to lnd), so we don't have enough information
		// to attempt a reconnect.
		srvrLog.DebugS(ctx, "Ignoring reconnection attempt "+
			"to inbound peer without advertised address")
		return

	// We came across an error retrieving an advertised
	// address, log it, and fall back to the existing peer
	// address.
	default:
		srvrLog.ErrorS(ctx, "Unable to retrieve advertised "+
			"address for peer", err)
	}

	// Make an easy lookup map so that we can check if an address
	// is already in the address list that we have stored for this peer.
	existingAddrs := make(map[string]bool)
	for _, addr := range s.persistentPeerAddrs[pubStr] {
		existingAddrs[addr.String()] = true
	}

	// Add any missing addresses for this peer to persistentPeerAddr.
	for _, addr := range addrs {
		if existingAddrs[addr.String()] {
			continue
		}

		s.persistentPeerAddrs[pubStr] = append(
			s.persistentPeerAddrs[pubStr],
			&lnwire.NetAddress{
				IdentityKey: p.IdentityKey(),
				Address:     addr,
				ChainNet:    p.NetAddress().ChainNet,
			},
		)
	}

	// Record the computed backoff in the backoff map.
	backoff := s.nextPeerBackoff(pubStr, p.StartTime())
	s.persistentPeersBackoff[pubStr] = backoff

	// Initialize a retry canceller for this peer if one does not
	// exist.
	cancelChan, ok := s.persistentRetryCancels[pubStr]
	if !ok {
		cancelChan = make(chan struct{})
		s.persistentRetryCancels[pubStr] = cancelChan
	}

	// We choose not to wait group this go routine since the Connect
	// call can stall for arbitrarily long if we shutdown while an
	// outbound connection attempt is being made.
	go func() {
		srvrLog.DebugS(ctx, "Scheduling connection "+
			"re-establishment to persistent peer",
			"reconnecting_in", backoff)

		select {
		case <-time.After(backoff):
		case <-cancelChan:
			return
		case <-s.quit:
			return
		}

		srvrLog.DebugS(ctx, "Attempting to re-establish persistent "+
			"connection")

		s.connectToPersistentPeer(pubStr)
	}()
}

// connectToPersistentPeer uses all the stored addresses for a peer to attempt
// to connect to the peer. It creates connection requests if there are
// currently none for a given address and it removes old connection requests
// if the associated address is no longer in the latest address list for the
// peer.
func (s *server) connectToPersistentPeer(pubKeyStr string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create an easy lookup map of the addresses we have stored for the
	// peer. We will remove entries from this map if we have existing
	// connection requests for the associated address and then any leftover
	// entries will indicate which addresses we should create new
	// connection requests for.
	addrMap := make(map[string]*lnwire.NetAddress)
	for _, addr := range s.persistentPeerAddrs[pubKeyStr] {
		addrMap[addr.String()] = addr
	}

	// Go through each of the existing connection requests and
	// check if they correspond to the latest set of addresses. If
	// there is a connection requests that does not use one of the latest
	// advertised addresses then remove that connection request.
	var updatedConnReqs []*connmgr.ConnReq
	for _, connReq := range s.persistentConnReqs[pubKeyStr] {
		lnAddr := connReq.Addr.(*lnwire.NetAddress).Address.String()

		switch _, ok := addrMap[lnAddr]; ok {
		// If the existing connection request is using one of the
		// latest advertised addresses for the peer then we add it to
		// updatedConnReqs and remove the associated address from
		// addrMap so that we don't recreate this connReq later on.
		case true:
			updatedConnReqs = append(
				updatedConnReqs, connReq,
			)
			delete(addrMap, lnAddr)

		// If the existing connection request is using an address that
		// is not one of the latest advertised addresses for the peer
		// then we remove the connecting request from the connection
		// manager.
		case false:
			srvrLog.Info(
				"Removing conn req:", connReq.Addr.String(),
			)
			s.connMgr.Remove(connReq.ID())
		}
	}

	s.persistentConnReqs[pubKeyStr] = updatedConnReqs

	cancelChan, ok := s.persistentRetryCancels[pubKeyStr]
	if !ok {
		cancelChan = make(chan struct{})
		s.persistentRetryCancels[pubKeyStr] = cancelChan
	}

	// Any addresses left in addrMap are new ones that we have not made
	// connection requests for. So create new connection requests for those.
	// If there is more than one address in the address map, stagger the
	// creation of the connection requests for those.
	go func() {
		ticker := time.NewTicker(multiAddrConnectionStagger)
		defer ticker.Stop()

		for _, addr := range addrMap {
			// Send the persistent connection request to the
			// connection manager, saving the request itself so we
			// can cancel/restart the process as needed.
			connReq := &connmgr.ConnReq{
				Addr:      addr,
				Permanent: true,
			}

			s.mu.Lock()
			s.persistentConnReqs[pubKeyStr] = append(
				s.persistentConnReqs[pubKeyStr], connReq,
			)
			s.mu.Unlock()

			srvrLog.Debugf("Attempting persistent connection to "+
				"channel peer %v", addr)

			go s.connMgr.Connect(connReq)

			select {
			case <-s.quit:
				return
			case <-cancelChan:
				return
			case <-ticker.C:
			}
		}
	}()
}

// removePeerUnsafe removes the passed peer from the server's state of all
// active peers.
//
// NOTE: Server mutex must be held when calling this function.
func (s *server) removePeerUnsafe(ctx context.Context, p *peer.Brontide) {
	if p == nil {
		return
	}

	srvrLog.DebugS(ctx, "Removing peer")

	// Exit early if we have already been instructed to shutdown, the peers
	// will be disconnected in the server shutdown process.
	if s.Stopped() {
		return
	}

	// Capture the peer's public key and string representation.
	pKey := p.PubKey()
	pubSer := pKey[:]
	pubStr := string(pubSer)

	delete(s.peersByPub, pubStr)

	if p.Inbound() {
		delete(s.inboundPeers, pubStr)
	} else {
		delete(s.outboundPeers, pubStr)
	}

	// When removing the peer we make sure to disconnect it asynchronously
	// to avoid blocking the main server goroutine because it is holding the
	// server's mutex. Disconnecting the peer might block and wait until the
	// peer has fully started up. This can happen if an inbound and outbound
	// race condition occurs.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		p.Disconnect(fmt.Errorf("server: disconnecting peer %v", p))

		// If this peer had an active persistent connection request,
		// remove it.
		if p.ConnReq() != nil {
			s.connMgr.Remove(p.ConnReq().ID())
		}

		// Remove the peer's access permission from the access manager.
		peerPubStr := string(p.IdentityKey().SerializeCompressed())
		s.peerAccessMan.removePeerAccess(ctx, peerPubStr)

		// Copy the peer's error buffer across to the server if it has
		// any items in it so that we can restore peer errors across
		// connections. We need to look up the error after the peer has
		// been disconnected because we write the error in the
		// `Disconnect` method.
		s.mu.Lock()
		if p.ErrorBuffer().Total() > 0 {
			s.peerErrors[pubStr] = p.ErrorBuffer()
		}
		s.mu.Unlock()

		// Inform the peer notifier of a peer offline event so that it
		// can be reported to clients listening for peer events.
		var pubKey [33]byte
		copy(pubKey[:], pubSer)

		s.peerNotifier.NotifyPeerOffline(pubKey)
	}()
}

// ConnectToPeer requests that the server connect to a Lightning Network peer
// at the specified address. This function will *block* until either a
// connection is established, or the initial handshake process fails.
//
// NOTE: This function is safe for concurrent access.
func (s *server) ConnectToPeer(addr *lnwire.NetAddress,
	perm bool, timeout time.Duration) error {

	targetPub := string(addr.IdentityKey.SerializeCompressed())

	// Acquire mutex, but use explicit unlocking instead of defer for
	// better granularity.  In certain conditions, this method requires
	// making an outbound connection to a remote peer, which requires the
	// lock to be released, and subsequently reacquired.
	s.mu.Lock()

	// Ensure we're not already connected to this peer.
	peer, err := s.findPeerByPubStr(targetPub)

	// When there's no error it means we already have a connection with this
	// peer. If this is a dev environment with the `--unsafeconnect` flag
	// set, we will ignore the existing connection and continue.
	if err == nil && !s.cfg.Dev.GetUnsafeConnect() {
		s.mu.Unlock()
		return &errPeerAlreadyConnected{peer: peer}
	}

	// Peer was not found, continue to pursue connection with peer.

	// If there's already a pending connection request for this pubkey,
	// then we ignore this request to ensure we don't create a redundant
	// connection.
	if reqs, ok := s.persistentConnReqs[targetPub]; ok {
		srvrLog.Warnf("Already have %d persistent connection "+
			"requests for %v, connecting anyway.", len(reqs), addr)
	}

	// If there's not already a pending or active connection to this node,
	// then instruct the connection manager to attempt to establish a
	// persistent connection to the peer.
	srvrLog.Debugf("Connecting to %v", addr)
	if perm {
		connReq := &connmgr.ConnReq{
			Addr:      addr,
			Permanent: true,
		}

		// Since the user requested a permanent connection, we'll set
		// the entry to true which will tell the server to continue
		// reconnecting even if the number of channels with this peer is
		// zero.
		s.persistentPeers[targetPub] = true
		if _, ok := s.persistentPeersBackoff[targetPub]; !ok {
			s.persistentPeersBackoff[targetPub] = s.cfg.MinBackoff
		}
		s.persistentConnReqs[targetPub] = append(
			s.persistentConnReqs[targetPub], connReq,
		)
		s.mu.Unlock()

		go s.connMgr.Connect(connReq)

		return nil
	}
	s.mu.Unlock()

	// If we're not making a persistent connection, then we'll attempt to
	// connect to the target peer. If the we can't make the connection, or
	// the crypto negotiation breaks down, then return an error to the
	// caller.
	errChan := make(chan error, 1)
	s.connectToPeer(addr, errChan, timeout)

	select {
	case err := <-errChan:
		return err
	case <-s.quit:
		return ErrServerShuttingDown
	}
}

// connectToPeer establishes a connection to a remote peer. errChan is used to
// notify the caller if the connection attempt has failed. Otherwise, it will be
// closed.
func (s *server) connectToPeer(addr *lnwire.NetAddress,
	errChan chan<- error, timeout time.Duration) {

	conn, err := brontide.Dial(
		s.identityECDH, addr, timeout, s.cfg.net.Dial,
	)
	if err != nil {
		srvrLog.Errorf("Unable to connect to %v: %v", addr, err)
		select {
		case errChan <- err:
		case <-s.quit:
		}
		return
	}

	close(errChan)

	srvrLog.Tracef("Brontide dialer made local=%v, remote=%v",
		conn.LocalAddr(), conn.RemoteAddr())

	s.OutboundPeerConnected(nil, conn)
}

// DisconnectPeer sends the request to server to close the connection with peer
// identified by public key.
//
// NOTE: This function is safe for concurrent access.
func (s *server) DisconnectPeer(pubKey *btcec.PublicKey) error {
	pubBytes := pubKey.SerializeCompressed()
	pubStr := string(pubBytes)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check that were actually connected to this peer. If not, then we'll
	// exit in an error as we can't disconnect from a peer that we're not
	// currently connected to.
	peer, err := s.findPeerByPubStr(pubStr)
	if err == ErrPeerNotConnected {
		return fmt.Errorf("peer %x is not connected", pubBytes)
	}

	srvrLog.Infof("Disconnecting from %v", peer)

	s.cancelConnReqs(pubStr, nil)

	// If this peer was formerly a persistent connection, then we'll remove
	// them from this map so we don't attempt to re-connect after we
	// disconnect.
	delete(s.persistentPeers, pubStr)
	delete(s.persistentPeersBackoff, pubStr)

	// Remove the peer by calling Disconnect. Previously this was done with
	// removePeerUnsafe, which bypassed the peerTerminationWatcher.
	//
	// NOTE: We call it in a goroutine to avoid blocking the main server
	// goroutine because we might hold the server's mutex.
	go peer.Disconnect(fmt.Errorf("server: DisconnectPeer called"))

	return nil
}

// OpenChannel sends a request to the server to open a channel to the specified
// peer identified by nodeKey with the passed channel funding parameters.
//
// NOTE: This function is safe for concurrent access.
func (s *server) OpenChannel(
	req *funding.InitFundingMsg) (chan *lnrpc.OpenStatusUpdate, chan error) {

	// The updateChan will have a buffer of 2, since we expect a ChanPending
	// + a ChanOpen update, and we want to make sure the funding process is
	// not blocked if the caller is not reading the updates.
	req.Updates = make(chan *lnrpc.OpenStatusUpdate, 2)
	req.Err = make(chan error, 1)

	// First attempt to locate the target peer to open a channel with, if
	// we're unable to locate the peer then this request will fail.
	pubKeyBytes := req.TargetPubkey.SerializeCompressed()
	s.mu.RLock()
	peer, ok := s.peersByPub[string(pubKeyBytes)]
	if !ok {
		s.mu.RUnlock()

		req.Err <- fmt.Errorf("peer %x is not online", pubKeyBytes)
		return req.Updates, req.Err
	}
	req.Peer = peer
	s.mu.RUnlock()

	// We'll wait until the peer is active before beginning the channel
	// opening process.
	select {
	case <-peer.ActiveSignal():
	case <-peer.QuitSignal():
		req.Err <- fmt.Errorf("peer %x disconnected", pubKeyBytes)
		return req.Updates, req.Err
	case <-s.quit:
		req.Err <- ErrServerShuttingDown
		return req.Updates, req.Err
	}

	// If the fee rate wasn't specified at this point we fail the funding
	// because of the missing fee rate information. The caller of the
	// `OpenChannel` method needs to make sure that default values for the
	// fee rate are set beforehand.
	if req.FundingFeePerKw == 0 {
		req.Err <- fmt.Errorf("no FundingFeePerKw specified for " +
			"the channel opening transaction")

		return req.Updates, req.Err
	}

	// Spawn a goroutine to send the funding workflow request to the funding
	// manager. This allows the server to continue handling queries instead
	// of blocking on this request which is exported as a synchronous
	// request to the outside world.
	go s.fundingMgr.InitFundingWorkflow(req)

	return req.Updates, req.Err
}

// Peers returns a slice of all active peers.
//
// NOTE: This function is safe for concurrent access.
func (s *server) Peers() []*peer.Brontide {
	s.mu.RLock()
	defer s.mu.RUnlock()

	peers := make([]*peer.Brontide, 0, len(s.peersByPub))
	for _, peer := range s.peersByPub {
		peers = append(peers, peer)
	}

	return peers
}

// computeNextBackoff uses a truncated exponential backoff to compute the next
// backoff using the value of the exiting backoff. The returned duration is
// randomized in either direction by 1/20 to prevent tight loops from
// stabilizing.
func computeNextBackoff(currBackoff, maxBackoff time.Duration) time.Duration {
	// Double the current backoff, truncating if it exceeds our maximum.
	nextBackoff := 2 * currBackoff
	if nextBackoff > maxBackoff {
		nextBackoff = maxBackoff
	}

	// Using 1/10 of our duration as a margin, compute a random offset to
	// avoid the nodes entering connection cycles.
	margin := nextBackoff / 10

	var wiggle big.Int
	wiggle.SetUint64(uint64(margin))
	if _, err := rand.Int(rand.Reader, &wiggle); err != nil {
		// Randomizing is not mission critical, so we'll just return the
		// current backoff.
		return nextBackoff
	}

	// Otherwise add in our wiggle, but subtract out half of the margin so
	// that the backoff can tweaked by 1/20 in either direction.
	return nextBackoff + (time.Duration(wiggle.Uint64()) - margin/2)
}

// errNoAdvertisedAddr is an error returned when we attempt to retrieve the
// advertised address of a node, but they don't have one.
var errNoAdvertisedAddr = errors.New("no advertised address found")

// fetchNodeAdvertisedAddrs attempts to fetch the advertised addresses of a node.
func (s *server) fetchNodeAdvertisedAddrs(ctx context.Context,
	pub *btcec.PublicKey) ([]net.Addr, error) {

	vertex, err := route.NewVertexFromBytes(pub.SerializeCompressed())
	if err != nil {
		return nil, err
	}

	node, err := s.graphDB.FetchNode(ctx, vertex)
	if err != nil {
		return nil, err
	}

	if len(node.Addresses) == 0 {
		return nil, errNoAdvertisedAddr
	}

	return node.Addresses, nil
}

// fetchLastChanUpdate returns a function which is able to retrieve our latest
// channel update for a target channel.
func (s *server) fetchLastChanUpdate() func(lnwire.ShortChannelID) (
	*lnwire.ChannelUpdate1, error) {

	ourPubKey := s.identityECDH.PubKey().SerializeCompressed()
	return func(cid lnwire.ShortChannelID) (*lnwire.ChannelUpdate1, error) {
		info, edge1, edge2, err := s.graphBuilder.GetChannelByID(cid)
		if err != nil {
			return nil, err
		}

		return netann.ExtractChannelUpdate(
			ourPubKey[:], info, edge1, edge2,
		)
	}
}

// applyChannelUpdate applies the channel update to the different sub-systems of
// the server. The useAlias boolean denotes whether or not to send an alias in
// place of the real SCID.
func (s *server) applyChannelUpdate(update *lnwire.ChannelUpdate1,
	op *wire.OutPoint, useAlias bool) error {

	var (
		peerAlias    *lnwire.ShortChannelID
		defaultAlias lnwire.ShortChannelID
	)

	chanID := lnwire.NewChanIDFromOutPoint(*op)

	// Fetch the peer's alias from the lnwire.ChannelID so it can be used
	// in the ChannelUpdate if it hasn't been announced yet.
	if useAlias {
		foundAlias, _ := s.aliasMgr.GetPeerAlias(chanID)
		if foundAlias != defaultAlias {
			peerAlias = &foundAlias
		}
	}

	errChan := s.authGossiper.ProcessLocalAnnouncement(
		update, discovery.RemoteAlias(peerAlias),
	)
	select {
	case err := <-errChan:
		return err
	case <-s.quit:
		return ErrServerShuttingDown
	}
}

// SendCustomMessage sends a custom message to the peer with the specified
// pubkey.
func (s *server) SendCustomMessage(ctx context.Context, peerPub [33]byte,
	msgType lnwire.MessageType, data []byte) error {

	peer, err := s.FindPeerByPubStr(string(peerPub[:]))
	if err != nil {
		return err
	}

	// We'll wait until the peer is active, but also listen for
	// cancellation.
	select {
	case <-peer.ActiveSignal():
	case <-peer.QuitSignal():
		return fmt.Errorf("peer %x disconnected", peerPub)
	case <-s.quit:
		return ErrServerShuttingDown
	case <-ctx.Done():
		return ctx.Err()
	}

	msg, err := lnwire.NewCustom(msgType, data)
	if err != nil {
		return err
	}

	// Send the message as low-priority. For now we assume that all
	// application-defined message are low priority.
	return peer.SendMessageLazy(true, msg)
}

// SendOnionMessage sends a custom message to the peer with the specified
// pubkey.
// TODO(gijs): change this message to include path finding.
func (s *server) SendOnionMessage(ctx context.Context, peerPub [33]byte,
	pathKey *btcec.PublicKey, onion []byte) error {

	peer, err := s.FindPeerByPubStr(string(peerPub[:]))
	if err != nil {
		return err
	}

	// We'll wait until the peer is active, but also listen for
	// cancellation.
	select {
	case <-peer.ActiveSignal():
	case <-peer.QuitSignal():
		return fmt.Errorf("peer %x disconnected", peerPub)
	case <-s.quit:
		return ErrServerShuttingDown
	case <-ctx.Done():
		return ctx.Err()
	}

	msg := lnwire.NewOnionMessage(pathKey, onion)

	// Send the message as low-priority. For now we assume that all
	// application-defined message are low priority.
	return peer.SendMessageLazy(true, msg)
}

// newSweepPkScriptGen creates closure that generates a new public key script
// which should be used to sweep any funds into the on-chain wallet.
// Specifically, the script generated is a version 0, pay-to-witness-pubkey-hash
// (p2wkh) output.
func newSweepPkScriptGen(
	wallet lnwallet.WalletController,
	netParams *chaincfg.Params) func() fn.Result[lnwallet.AddrWithKey] {

	return func() fn.Result[lnwallet.AddrWithKey] {
		sweepAddr, err := wallet.NewAddress(
			lnwallet.TaprootPubkey, false,
			lnwallet.DefaultAccountName,
		)
		if err != nil {
			return fn.Err[lnwallet.AddrWithKey](err)
		}

		addr, err := txscript.PayToAddrScript(sweepAddr)
		if err != nil {
			return fn.Err[lnwallet.AddrWithKey](err)
		}

		internalKeyDesc, err := lnwallet.InternalKeyForAddr(
			wallet, netParams, addr,
		)
		if err != nil {
			return fn.Err[lnwallet.AddrWithKey](err)
		}

		return fn.Ok(lnwallet.AddrWithKey{
			DeliveryAddress: addr,
			InternalKey:     internalKeyDesc,
		})
	}
}

// fetchClosedChannelSCIDs returns a set of SCIDs that have their force closing
// finished.
func (s *server) fetchClosedChannelSCIDs() map[lnwire.ShortChannelID]struct{} {
	// Get a list of closed channels.
	channels, err := s.chanStateDB.FetchClosedChannels(false)
	if err != nil {
		srvrLog.Errorf("Failed to fetch closed channels: %v", err)
		return nil
	}

	// Save the SCIDs in a map.
	closedSCIDs := make(map[lnwire.ShortChannelID]struct{}, len(channels))
	for _, c := range channels {
		// If the channel is not pending, its FC has been finalized.
		if !c.IsPending {
			closedSCIDs[c.ShortChanID] = struct{}{}
		}
	}

	// Double check whether the reported closed channel has indeed finished
	// closing.
	//
	// NOTE: There are misalignments regarding when a channel's FC is
	// marked as finalized. We double check the pending channels to make
	// sure the returned SCIDs are indeed terminated.
	//
	// TODO(yy): fix the misalignments in `FetchClosedChannels`.
	pendings, err := s.chanStateDB.FetchPendingChannels()
	if err != nil {
		srvrLog.Errorf("Failed to fetch pending channels: %v", err)
		return nil
	}

	for _, c := range pendings {
		if _, ok := closedSCIDs[c.ShortChannelID]; !ok {
			continue
		}

		// If the channel is still reported as pending, remove it from
		// the map.
		delete(closedSCIDs, c.ShortChannelID)

		srvrLog.Warnf("Channel=%v is prematurely marked as finalized",
			c.ShortChannelID)
	}

	return closedSCIDs
}

// getStartingBeat returns the current beat. This is used during the startup to
// initialize blockbeat consumers.
func (s *server) getStartingBeat() (*chainio.Beat, error) {
	// beat is the current blockbeat.
	var beat *chainio.Beat

	// If the node is configured with nochainbackend mode (remote signer),
	// we will skip fetching the best block.
	if s.cfg.Bitcoin.Node == "nochainbackend" {
		srvrLog.Info("Skipping block notification for nochainbackend " +
			"mode")

		return &chainio.Beat{}, nil
	}

	// We should get a notification with the current best block immediately
	// by passing a nil block.
	blockEpochs, err := s.cc.ChainNotifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return beat, fmt.Errorf("register block epoch ntfn: %w", err)
	}
	defer blockEpochs.Cancel()

	// We registered for the block epochs with a nil request. The notifier
	// should send us the current best block immediately. So we need to
	// wait for it here because we need to know the current best height.
	select {
	case bestBlock := <-blockEpochs.Epochs:
		srvrLog.Infof("Received initial block %v at height %d",
			bestBlock.Hash, bestBlock.Height)

		// Update the current blockbeat.
		beat = chainio.NewBeat(*bestBlock)

	case <-s.quit:
		srvrLog.Debug("LND shutting down")
	}

	return beat, nil
}

// ChanHasRbfCoopCloser returns true if the channel as identifier by the channel
// point has an active RBF chan closer.
func (s *server) ChanHasRbfCoopCloser(peerPub *btcec.PublicKey,
	chanPoint wire.OutPoint) bool {

	pubBytes := peerPub.SerializeCompressed()

	s.mu.RLock()
	targetPeer, ok := s.peersByPub[string(pubBytes)]
	s.mu.RUnlock()
	if !ok {
		return false
	}

	return targetPeer.ChanHasRbfCoopCloser(chanPoint)
}

// attemptCoopRbfFeeBump attempts to look up the active chan closer for a
// channel given the outpoint. If found, we'll attempt to do a fee bump,
// returning channels used for updates. If the channel isn't currently active
// (p2p connection established), then his function will return an error.
func (s *server) attemptCoopRbfFeeBump(ctx context.Context,
	chanPoint wire.OutPoint, feeRate chainfee.SatPerKWeight,
	deliveryScript lnwire.DeliveryAddress) (*peer.CoopCloseUpdates, error) {

	// First, we'll attempt to look up the channel based on it's
	// ChannelPoint.
	channel, err := s.chanStateDB.FetchChannel(chanPoint)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channel: %w", err)
	}

	// From the channel, we can now get the pubkey of the peer, then use
	// that to eventually get the chan closer.
	peerPub := channel.IdentityPub.SerializeCompressed()

	// Now that we have the peer pub, we can look up the peer itself.
	s.mu.RLock()
	targetPeer, ok := s.peersByPub[string(peerPub)]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("peer for ChannelPoint(%v) is "+
			"not online", chanPoint)
	}

	closeUpdates, err := targetPeer.TriggerCoopCloseRbfBump(
		ctx, chanPoint, feeRate, deliveryScript,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to trigger coop rbf fee bump: "+
			"%w", err)
	}

	return closeUpdates, nil
}

// AttemptRBFCloseUpdate attempts to trigger a new RBF iteration for a co-op
// close update. This route it to be used only if the target channel in question
// is no longer active in the link. This can happen when we restart while we
// already have done a single RBF co-op close iteration.
func (s *server) AttemptRBFCloseUpdate(ctx context.Context,
	chanPoint wire.OutPoint, feeRate chainfee.SatPerKWeight,
	deliveryScript lnwire.DeliveryAddress) (*peer.CoopCloseUpdates, error) {

	// If the channel is present in the switch, then the request should flow
	// through the switch instead.
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
	if _, err := s.htlcSwitch.GetLink(chanID); err == nil {
		return nil, fmt.Errorf("ChannelPoint(%v) is active in link, "+
			"invalid request", chanPoint)
	}

	// At this point, we know that the channel isn't present in the link, so
	// we'll check to see if we have an entry in the active chan closer map.
	updates, err := s.attemptCoopRbfFeeBump(
		ctx, chanPoint, feeRate, deliveryScript,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to attempt coop rbf fee bump "+
			"ChannelPoint(%v)", chanPoint)
	}

	return updates, nil
}

// setSelfNode configures and sets the server's self node. It sets the node
// announcement, signs it, and updates the source node in the graph. When
// determining values such as color and alias, the method prioritizes values
// set in the config, then values previously persisted on disk, and finally
// falls back to the defaults.
func (s *server) setSelfNode(ctx context.Context, nodePub route.Vertex,
	listenAddrs []net.Addr) error {

	// If we were requested to automatically configure port forwarding,
	// we'll use the ports that the server will be listening on.
	externalIPStrings := make([]string, 0, len(s.cfg.ExternalIPs))
	for _, ip := range s.cfg.ExternalIPs {
		externalIPStrings = append(externalIPStrings, ip.String())
	}
	if s.natTraversal != nil {
		listenPorts := make([]uint16, 0, len(listenAddrs))
		for _, listenAddr := range listenAddrs {
			// At this point, the listen addresses should have
			// already been normalized, so it's safe to ignore the
			// errors.
			_, portStr, _ := net.SplitHostPort(listenAddr.String())
			port, _ := strconv.Atoi(portStr)

			listenPorts = append(listenPorts, uint16(port))
		}

		ips, err := s.configurePortForwarding(listenPorts...)
		if err != nil {
			srvrLog.Errorf("Unable to automatically set up port "+
				"forwarding using %s: %v",
				s.natTraversal.Name(), err)
		} else {
			srvrLog.Infof("Automatically set up port forwarding "+
				"using %s to advertise external IP",
				s.natTraversal.Name())
			externalIPStrings = append(externalIPStrings, ips...)
		}
	}

	// Normalize the external IP strings to net.Addr.
	addrs, err := lncfg.NormalizeAddresses(
		externalIPStrings, strconv.Itoa(defaultPeerPort),
		s.cfg.net.ResolveTCPAddr,
	)
	if err != nil {
		return fmt.Errorf("unable to normalize addresses: %w", err)
	}

	// Parse the color from config. We will update this later if the config
	// color is not changed from default (#3399FF) and we have a value in
	// the source node.
	nodeColor, err := lncfg.ParseHexColor(s.cfg.Color)
	if err != nil {
		return fmt.Errorf("unable to parse color: %w", err)
	}

	var (
		alias          = s.cfg.Alias
		nodeLastUpdate = time.Now()
	)

	srcNode, err := s.graphDB.SourceNode(ctx)
	switch {
	case err == nil:
		// If we have a source node persisted in the DB already, then we
		// just need to make sure that the new LastUpdate time is at
		// least one second after the last update time.
		if srcNode.LastUpdate.Second() >= nodeLastUpdate.Second() {
			nodeLastUpdate = srcNode.LastUpdate.Add(time.Second)
		}

		// If the color is not changed from default, it means that we
		// didn't specify a different color in the config. We'll use the
		// source node's color.
		if s.cfg.Color == defaultColor {
			srcNode.Color.WhenSome(func(rgba color.RGBA) {
				nodeColor = rgba
			})
		}

		// If an alias is not specified in the config, we'll use the
		// source node's alias.
		if alias == "" {
			srcNode.Alias.WhenSome(func(s string) {
				alias = s
			})
		}

		// If the `externalip` is not specified in the config, it means
		// `addrs` will be empty, we'll use the source node's addresses.
		if len(s.cfg.ExternalIPs) == 0 {
			addrs = srcNode.Addresses
		}

	case errors.Is(err, graphdb.ErrSourceNodeNotSet):
		// If an alias is not specified in the config, we'll use the
		// default, which is the first 10 bytes of the serialized
		// pubkey.
		if alias == "" {
			alias = hex.EncodeToString(nodePub[:10])
		}

	// If the above cases are not matched, then we have an unhandled non
	// nil error.
	default:
		return fmt.Errorf("unable to fetch source node: %w", err)
	}

	nodeAlias, err := lnwire.NewNodeAlias(alias)
	if err != nil {
		return err
	}

	// TODO(abdulkbk): potentially find a way to use the source node's
	// features in the self node.
	selfNode := models.NewV1Node(
		nodePub, &models.NodeV1Fields{
			Alias:      nodeAlias.String(),
			Color:      nodeColor,
			LastUpdate: nodeLastUpdate,
			Addresses:  addrs,
			Features:   s.featureMgr.GetRaw(feature.SetNodeAnn),
		},
	)

	// Based on the disk representation of the node announcement generated
	// above, we'll generate a node announcement that can go out on the
	// network so we can properly sign it.
	nodeAnn, err := selfNode.NodeAnnouncement(false)
	if err != nil {
		return fmt.Errorf("unable to gen self node ann: %w", err)
	}

	// With the announcement generated, we'll sign it to properly
	// authenticate the message on the network.
	authSig, err := netann.SignAnnouncement(
		s.nodeSigner, s.identityKeyLoc, nodeAnn,
	)
	if err != nil {
		return fmt.Errorf("unable to generate signature for self node "+
			"announcement: %v", err)
	}

	selfNode.AuthSigBytes = authSig.Serialize()
	nodeAnn.Signature, err = lnwire.NewSigFromECDSARawSignature(
		selfNode.AuthSigBytes,
	)
	if err != nil {
		return err
	}

	// Finally, we'll update the representation on disk, and update our
	// cached in-memory version as well.
	if err := s.graphDB.SetSourceNode(ctx, selfNode); err != nil {
		return fmt.Errorf("can't set self node: %w", err)
	}

	s.currentNodeAnn = nodeAnn

	return nil
}

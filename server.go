package lnd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/chanfitness"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/healthcheck"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/rpcwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/nat"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/peer"
	"github.com/lightningnetwork/lnd/peerconn"
	"github.com/lightningnetwork/lnd/pool"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/localchans"
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
)

var (
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
)

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

	// pongBuf is a shared pong reply buffer we'll use across all active
	// peer goroutines. We know the max size of a pong message
	// (lnwire.MaxPongBytes), so we can allocate this ahead of time, and
	// avoid allocations each time we need to send a pong message.
	pongBuf []byte

	cc *chainreg.ChainControl

	fundingMgr *funding.Manager

	graphDB *channeldb.ChannelGraph

	chanStateDB *channeldb.ChannelStateDB

	addrSource chanbackup.AddressSource

	// miscDB is the DB that contains all "other" databases within the main
	// channel DB that haven't been separated out yet.
	miscDB *channeldb.DB

	aliasMgr *aliasmgr.Manager

	htlcSwitch *htlcswitch.Switch

	interceptableSwitch *htlcswitch.InterceptableSwitch

	invoices *invoices.InvoiceRegistry

	channelNotifier *channelnotifier.ChannelNotifier

	htlcNotifier *htlcswitch.HtlcNotifier

	witnessBeacon contractcourt.WitnessBeacon

	breachArbiter *contractcourt.BreachArbiter

	missionControl *routing.MissionControl

	chanRouter *routing.ChannelRouter

	controlTower routing.ControlTower

	authGossiper *discovery.AuthenticatedGossiper

	localChanMgr *localchans.Manager

	utxoNursery *contractcourt.UtxoNursery

	sweeper *sweep.UtxoSweeper

	chainArb *contractcourt.ChainArbitrator

	sphinx *hop.OnionProcessor

	towerClient wtclient.Client

	anchorTowerClient wtclient.Client

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
	currentNodeAnn *lnwire.NodeAnnouncement

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

	quit chan struct{}

	wg sync.WaitGroup

	pcm *peerconn.Manager
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

// newServer creates a new instance of the server which is to listen using the
// passed listener address.
func newServer(cfg *Config, listenAddrs []net.Addr,
	dbs *DatabaseInstances, cc *chainreg.ChainControl,
	nodeKeyDesc *keychain.KeyDescriptor,
	chansToRestore walletunlocker.ChannelsToRecover,
	chanPredicate chanacceptor.ChannelAcceptor,
	torController *tor.Controller, tlsManager *TLSManager) (*server,
	error) {

	var (
		err         error
		nodeKeyECDH = keychain.NewPubKeyECDH(*nodeKeyDesc, cc.KeyRing)

		// We just derived the full descriptor, so we know the public
		// key is set on it.
		nodeKeySigner = keychain.NewPubKeyMessageSigner(
			nodeKeyDesc.PubKey, nodeKeyDesc.KeyLocator, cc.KeyRing,
		)
	)

	listeners := make([]net.Listener, len(listenAddrs))
	for i, listenAddr := range listenAddrs {
		// Note: though brontide.NewListener uses ResolveTCPAddr, it
		// doesn't need to call the general lndResolveTCP function
		// since we are resolving a local address.
		listeners[i], err = brontide.NewListener(
			nodeKeyECDH, listenAddr.String(),
		)
		if err != nil {
			return nil, err
		}
	}

	var serializedPubKey [33]byte
	copy(serializedPubKey[:], nodeKeyDesc.PubKey.SerializeCompressed())

	// Initialize the sphinx router.
	replayLog := htlcswitch.NewDecayedLog(
		dbs.DecayedLogDB, cc.ChainNotifier,
	)
	sphinxRouter := sphinx.NewRouter(
		nodeKeyECDH, cfg.ActiveNetParams.Params, replayLog,
	)

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

	//nolint:lll
	featureMgr, err := feature.NewManager(feature.Config{
		NoTLVOnion:               cfg.ProtocolOptions.LegacyOnion(),
		NoStaticRemoteKey:        cfg.ProtocolOptions.NoStaticRemoteKey(),
		NoAnchors:                cfg.ProtocolOptions.NoAnchorCommitments(),
		NoWumbo:                  !cfg.ProtocolOptions.Wumbo(),
		NoScriptEnforcementLease: cfg.ProtocolOptions.NoScriptEnforcementLease(),
		NoKeysend:                !cfg.AcceptKeySend,
		NoOptionScidAlias:        !cfg.ProtocolOptions.ScidAlias(),
		NoZeroConf:               !cfg.ProtocolOptions.ZeroConf(),
		NoAnySegwit:              cfg.ProtocolOptions.NoAnySegwit(),
		CustomFeatures:           cfg.ProtocolOptions.ExperimentalProtocol.CustomFeatures(),
		NoTaprootChans:           !cfg.ProtocolOptions.TaprootChans,
	})
	if err != nil {
		return nil, err
	}

	registryConfig := invoices.RegistryConfig{
		FinalCltvRejectDelta:        lncfg.DefaultFinalCltvRejectDelta,
		HtlcHoldDuration:            invoices.DefaultHtlcHoldDuration,
		Clock:                       clock.NewDefaultClock(),
		AcceptKeySend:               cfg.AcceptKeySend,
		AcceptAMP:                   cfg.AcceptAMP,
		GcCanceledInvoicesOnStartup: cfg.GcCanceledInvoicesOnStartup,
		GcCanceledInvoicesOnTheFly:  cfg.GcCanceledInvoicesOnTheFly,
		KeysendHoldTime:             cfg.KeysendHoldTime,
	}

	s := &server{
		cfg:            cfg,
		graphDB:        dbs.GraphDB.ChannelGraph(),
		chanStateDB:    dbs.ChanStateDB.ChannelStateDB(),
		addrSource:     dbs.ChanStateDB,
		miscDB:         dbs.ChanStateDB,
		cc:             cc,
		sigPool:        lnwallet.NewSigPool(cfg.Workers.Sig, cc.Signer),
		writePool:      writePool,
		readPool:       readPool,
		chansToRestore: chansToRestore,

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

		customMessageServer: subscribe.NewServer(),

		tlsManager: tlsManager,

		pongBuf: make([]byte, lnwire.MaxPongBytes),

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

	thresholdSats := btcutil.Amount(cfg.DustThreshold)
	thresholdMSats := lnwire.NewMSatFromSatoshis(thresholdSats)

	s.aliasMgr, err = aliasmgr.NewManager(dbs.ChanStateDB)
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

			peer, err := s.pcm.FindPeerByPubStr(string(pubKey))
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
		DustThreshold:          thresholdMSats,
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
		Graph:                    dbs.GraphDB.ChannelGraph(),
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

	// If we were requested to automatically configure port forwarding,
	// we'll use the ports that the server will be listening on.
	externalIPStrings := make([]string, len(cfg.ExternalIPs))
	for idx, ip := range cfg.ExternalIPs {
		externalIPStrings[idx] = ip.String()
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

	// If external IP addresses have been specified, add those to the list
	// of this server's addresses.
	externalIPs, err := lncfg.NormalizeAddresses(
		externalIPStrings, strconv.Itoa(defaultPeerPort),
		cfg.net.ResolveTCPAddr,
	)
	if err != nil {
		return nil, err
	}

	selfAddrs := make([]net.Addr, 0, len(externalIPs))
	selfAddrs = append(selfAddrs, externalIPs...)

	// As the graph can be obtained at anytime from the network, we won't
	// replicate it, and instead it'll only be stored locally.
	chanGraph := dbs.GraphDB.ChannelGraph()

	// We'll now reconstruct a node announcement based on our current
	// configuration so we can send it out as a sort of heart beat within
	// the network.
	//
	// We'll start by parsing the node color from configuration.
	color, err := lncfg.ParseHexColor(cfg.Color)
	if err != nil {
		srvrLog.Errorf("unable to parse color: %v\n", err)
		return nil, err
	}

	// If no alias is provided, default to first 10 characters of public
	// key.
	alias := cfg.Alias
	if alias == "" {
		alias = hex.EncodeToString(serializedPubKey[:10])
	}
	nodeAlias, err := lnwire.NewNodeAlias(alias)
	if err != nil {
		return nil, err
	}
	selfNode := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Now(),
		Addresses:            selfAddrs,
		Alias:                nodeAlias.String(),
		Features:             s.featureMgr.Get(feature.SetNodeAnn),
		Color:                color,
	}
	copy(selfNode.PubKeyBytes[:], nodeKeyDesc.PubKey.SerializeCompressed())

	// Based on the disk representation of the node announcement generated
	// above, we'll generate a node announcement that can go out on the
	// network so we can properly sign it.
	nodeAnn, err := selfNode.NodeAnnouncement(false)
	if err != nil {
		return nil, fmt.Errorf("unable to gen self node ann: %v", err)
	}

	// With the announcement generated, we'll sign it to properly
	// authenticate the message on the network.
	authSig, err := netann.SignAnnouncement(
		s.nodeSigner, nodeKeyDesc.KeyLocator, nodeAnn,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to generate signature for "+
			"self node announcement: %v", err)
	}
	selfNode.AuthSigBytes = authSig.Serialize()
	nodeAnn.Signature, err = lnwire.NewSigFromECDSARawSignature(
		selfNode.AuthSigBytes,
	)
	if err != nil {
		return nil, err
	}

	// Finally, we'll update the representation on disk, and update our
	// cached in-memory version as well.
	if err := chanGraph.SetSourceNode(selfNode); err != nil {
		return nil, fmt.Errorf("can't set self node: %v", err)
	}
	s.currentNodeAnn = nodeAnn

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
		Estimator:               estimator,
		MaxMcHistory:            routingConfig.MaxMcHistory,
		McFlushInterval:         routingConfig.McFlushInterval,
		MinFailureRelaxInterval: routing.DefaultMinFailureRelaxInterval,
	}
	s.missionControl, err = routing.NewMissionControl(
		dbs.ChanStateDB, selfNode.PubKeyBytes, mcCfg,
	)
	if err != nil {
		return nil, fmt.Errorf("can't create mission control: %v", err)
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

	sourceNode, err := chanGraph.SourceNode()
	if err != nil {
		return nil, fmt.Errorf("error getting source node: %v", err)
	}
	paymentSessionSource := &routing.SessionSource{
		Graph:             chanGraph,
		SourceNode:        sourceNode,
		MissionControl:    s.missionControl,
		GetLink:           s.htlcSwitch.GetLinkByShortID,
		PathFindingConfig: pathFindingConfig,
	}

	paymentControl := channeldb.NewPaymentControl(dbs.ChanStateDB)

	s.controlTower = routing.NewControlTower(paymentControl)

	strictPruning := (cfg.Bitcoin.Node == "neutrino" ||
		cfg.Routing.StrictZombiePruning)
	s.chanRouter, err = routing.New(routing.Config{
		Graph:               chanGraph,
		Chain:               cc.ChainIO,
		ChainView:           cc.ChainView,
		Notifier:            cc.ChainNotifier,
		Payer:               s.htlcSwitch,
		Control:             s.controlTower,
		MissionControl:      s.missionControl,
		SessionSource:       paymentSessionSource,
		ChannelPruneExpiry:  routing.DefaultChannelPruneExpiry,
		GraphPruneInterval:  time.Hour,
		FirstTimePruneDelay: routing.DefaultFirstTimePruneDelay,
		GetLink:             s.htlcSwitch.GetLinkByShortID,
		AssumeChannelValid:  cfg.Routing.AssumeChannelValid,
		NextPaymentID:       sequencer.NextID,
		PathFindingConfig:   pathFindingConfig,
		Clock:               clock.NewDefaultClock(),
		StrictZombiePruning: strictPruning,
		IsAlias:             aliasmgr.IsAlias,
	})
	if err != nil {
		return nil, fmt.Errorf("can't create router: %v", err)
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

	// Create peer conn manager.
	s.pcm = peerconn.NewPeerConnManager(nodeKeyECDH, s.torController)

	s.authGossiper = discovery.New(discovery.Config{
		Router:                s.chanRouter,
		Notifier:              s.cc.ChainNotifier,
		ChainHash:             *s.cfg.ActiveNetParams.GenesisHash,
		Broadcast:             s.pcm.BroadcastMessage,
		ChanSeries:            chanSeries,
		NotifyWhenOnline:      s.pcm.NotifyWhenOnline,
		NotifyWhenOffline:     s.pcm.NotifyWhenOffline,
		FetchSelfAnnouncement: s.getNodeAnnouncement,
		UpdateSelfAnnouncement: func() (lnwire.NodeAnnouncement,
			error) {

			return s.genNodeAnnouncement(nil)
		},
		ProofMatureDelta:        0,
		TrickleDelay:            time.Millisecond * time.Duration(cfg.TrickleDelay),
		RetransmitTicker:        ticker.New(time.Minute * 30),
		RebroadcastInterval:     time.Hour * 24,
		WaitingProofStore:       waitingProofStore,
		MessageStore:            gossipMessageStore,
		AnnSigner:               s.nodeSigner,
		RotateTicker:            ticker.New(discovery.DefaultSyncerRotationInterval),
		HistoricalSyncTicker:    ticker.New(cfg.HistoricalSyncInterval),
		NumActiveSyncers:        cfg.NumGraphSyncPeers,
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
	}, nodeKeyDesc)

	s.localChanMgr = &localchans.Manager{
		ForAllOutgoingChannels:    s.chanRouter.ForAllOutgoingChannels,
		PropagateChanPolicyUpdate: s.authGossiper.PropagateChanPolicyUpdate,
		UpdateForwardingPolicies:  s.htlcSwitch.UpdateForwardingPolicies,
		FetchChannel:              s.chanStateDB.FetchChannel,
	}

	utxnStore, err := contractcourt.NewNurseryStore(
		s.cfg.ActiveNetParams.GenesisHash, dbs.ChanStateDB,
	)
	if err != nil {
		srvrLog.Errorf("unable to create nursery store: %v", err)
		return nil, err
	}

	srvrLog.Debugf("Sweeper batch window duration: %v",
		cfg.Sweeper.BatchWindowDuration)

	sweeperStore, err := sweep.NewSweeperStore(
		dbs.ChanStateDB, s.cfg.ActiveNetParams.GenesisHash,
	)
	if err != nil {
		srvrLog.Errorf("unable to create sweeper store: %v", err)
		return nil, err
	}

	s.sweeper = sweep.New(&sweep.UtxoSweeperConfig{
		FeeEstimator:   cc.FeeEstimator,
		GenSweepScript: newSweepPkScriptGen(cc.Wallet),
		Signer:         cc.Wallet.Cfg.Signer,
		Wallet:         newSweeperWallet(cc.Wallet),
		NewBatchTimer: func() <-chan time.Time {
			return time.NewTimer(cfg.Sweeper.BatchWindowDuration).C
		},
		Notifier:             cc.ChainNotifier,
		Store:                sweeperStore,
		MaxInputsPerTx:       sweep.DefaultMaxInputsPerTx,
		MaxSweepAttempts:     sweep.DefaultMaxSweepAttempts,
		NextAttemptDeltaFunc: sweep.DefaultNextAttemptDeltaFunc,
		MaxFeeRate:           cfg.Sweeper.MaxFeeRate,
		FeeRateBucketSize:    sweep.DefaultFeeRateBucketSize,
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
	})

	// Construct a closure that wraps the htlcswitch's CloseLink method.
	closeLink := func(chanPoint *wire.OutPoint,
		closureType contractcourt.ChannelCloseType) {
		// TODO(conner): Properly respect the update and error channels
		// returned by CloseLink.

		// Instruct the switch to close the channel.  Provide no close out
		// delivery script or target fee per kw because user input is not
		// available when the remote peer closes the channel.
		s.htlcSwitch.CloseLink(chanPoint, closureType, 0, 0, nil)
	}

	// We will use the following channel to reliably hand off contract
	// breach events from the ChannelArbitrator to the breachArbiter,
	contractBreaches := make(chan *contractcourt.ContractBreachEvent, 1)

	s.breachArbiter = contractcourt.NewBreachArbiter(&contractcourt.BreachConfig{
		CloseLink:          closeLink,
		DB:                 s.chanStateDB,
		Estimator:          s.cc.FeeEstimator,
		GenSweepScript:     newSweepPkScriptGen(cc.Wallet),
		Notifier:           cc.ChainNotifier,
		PublishTransaction: cc.Wallet.PublishTransaction,
		ContractBreaches:   contractBreaches,
		Signer:             cc.Wallet.Cfg.Signer,
		Store: contractcourt.NewRetributionStore(
			dbs.ChanStateDB,
		),
	})

	s.chainArb = contractcourt.NewChainArbitrator(contractcourt.ChainArbitratorConfig{
		ChainHash:              *s.cfg.ActiveNetParams.GenesisHash,
		IncomingBroadcastDelta: lncfg.DefaultIncomingBroadcastDelta,
		OutgoingBroadcastDelta: lncfg.DefaultOutgoingBroadcastDelta,
		NewSweepAddr:           newSweepPkScriptGen(cc.Wallet),
		PublishTx:              cc.Wallet.PublishTransaction,
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
			outHtlcRes *lnwallet.OutgoingHtlcResolution,
			inHtlcRes *lnwallet.IncomingHtlcResolution,
			broadcastHeight uint32) error {

			var (
				inRes  []lnwallet.IncomingHtlcResolution
				outRes []lnwallet.OutgoingHtlcResolution
			)
			if inHtlcRes != nil {
				inRes = append(inRes, *inHtlcRes)
			}
			if outHtlcRes != nil {
				outRes = append(outRes, *outHtlcRes)
			}

			return s.utxoNursery.IncubateOutputs(
				chanPoint, outRes, inRes,
				broadcastHeight,
			)
		},
		PreimageDB:   s.witnessBeacon,
		Notifier:     cc.ChainNotifier,
		Mempool:      cc.MempoolNotifier,
		Signer:       cc.Wallet.Cfg.Signer,
		FeeEstimator: cc.FeeEstimator,
		ChainIO:      cc.ChainIO,
		MarkLinkInactive: func(chanPoint wire.OutPoint) error {
			chanID := lnwire.NewChanIDFromOutPoint(&chanPoint)
			s.htlcSwitch.RemoveLink(chanID)
			return nil
		},
		IsOurAddress: cc.Wallet.IsOurAddress,
		ContractBreach: func(chanPoint wire.OutPoint,
			breachRet *lnwallet.BreachRetribution) error {

			// processACK will handle the breachArbiter ACKing the
			// event.
			finalErr := make(chan error, 1)
			processACK := func(brarErr error) {
				if brarErr != nil {
					finalErr <- brarErr
					return
				}

				// If the breachArbiter successfully handled
				// the event, we can signal that the handoff
				// was successful.
				finalErr <- nil
			}

			event := &contractcourt.ContractBreachEvent{
				ChanPoint:         chanPoint,
				ProcessACK:        processACK,
				BreachRetribution: breachRet,
			}

			// Send the contract breach event to the breachArbiter.
			select {
			case contractBreaches <- event:
			case <-s.quit:
				return ErrServerShuttingDown
			}

			// We'll wait for a final error to be available from
			// the breachArbiter.
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
		SubscribeBreachComplete:       s.breachArbiter.SubscribeBreachComplete,
		PutFinalHtlcOutcome:           s.chanStateDB.PutOnchainFinalHtlcOutcome, //nolint: lll
		HtlcNotifier:                  s.htlcNotifier,
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
		*channeldb.ChannelEdgePolicy, error) {

		info, e1, e2, err := s.graphDB.FetchChannelEdgesByID(
			scid.ToUint64(),
		)
		if err == channeldb.ErrEdgeNotFound {
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

		var ourPolicy *channeldb.ChannelEdgePolicy
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

	// Get the development config for funding manager. If we are not in
	// development mode, this would be nil.
	var devCfg *funding.DevConfig
	if lncfg.IsDevBuild() {
		devCfg = &funding.DevConfig{
			ProcessChannelReadyWait: cfg.Dev.ChannelReadyWait(),
		}
	}

	//nolint:lll
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
		CurrentNodeAnnouncement: func() (lnwire.NodeAnnouncement,
			error) {

			return s.genNodeAnnouncement(nil)
		},
		SendAnnouncement:     s.authGossiper.ProcessLocalAnnouncement,
		NotifyWhenOnline:     s.pcm.NotifyWhenOnline,
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
			s.pcm.AddPersistentPeer(peerKey)

			// With that taken care of, we'll send this channel to
			// the chain arb so it can react to on-chain events.
			return s.chainArb.WatchNewChannel(channel)
		},
		ReportShortChanID: func(chanPoint wire.OutPoint) error {
			cid := lnwire.NewChanIDFromOutPoint(&chanPoint)
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
		ZombieSweeperInterval:         1 * time.Minute,
		ReservationTimeout:            10 * time.Minute,
		MinChanSize:                   btcutil.Amount(cfg.MinChanSize),
		MaxChanSize:                   btcutil.Amount(cfg.MaxChanSize),
		MaxPendingChannels:            cfg.MaxPendingChannels,
		RejectPush:                    cfg.RejectPush,
		MaxLocalCSVDelay:              chainCfg.MaxLocalDelay,
		NotifyOpenChannelEvent:        s.channelNotifier.NotifyOpenChannelEvent,
		OpenChannelPredicate:          chanPredicate,
		NotifyPendingOpenChannelEvent: s.channelNotifier.NotifyPendingOpenChannelEvent,
		EnableUpfrontShutdown:         cfg.EnableUpfrontShutdown,
		MaxAnchorsCommitFeeRate: chainfee.SatPerKVByte(
			s.cfg.MaxCommitFeeRateAnchors * 1000).FeePerKWeight(),
		DeleteAliasEdge: deleteAliasEdge,
		AliasManager:    s.aliasMgr,
	})
	if err != nil {
		return nil, err
	}

	// Next, we'll assemble the sub-system that will maintain an on-disk
	// static backup of the latest channel state.
	chanNotifier := &channelNotifier{
		chanNotifier: s.channelNotifier,
		addrs:        dbs.ChanStateDB,
	}
	backupFile := chanbackup.NewMultiFile(cfg.BackupFilePath)
	startingChans, err := chanbackup.FetchStaticChanBackups(
		s.chanStateDB, s.addrSource,
	)
	if err != nil {
		return nil, err
	}
	s.chanSubSwapper, err = chanbackup.NewSubSwapper(
		startingChans, chanNotifier, s.cc.KeyRing, backupFile,
	)
	if err != nil {
		return nil, err
	}

	// Create a channel event store which monitors all open channels.
	s.chanEventStore = chanfitness.NewChannelEventStore(&chanfitness.Config{
		SubscribeChannelEvents: func() (subscribe.Subscription, error) {
			return s.channelNotifier.SubscribeChannelEvents()
		},
		SubscribePeerEvents: func() (subscribe.Subscription, error) {
			return s.pcm.PeerNotifier.SubscribePeerEvents()
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
			)
			if err != nil {
				return nil, 0, err
			}

			return br, channel.ChanType, nil
		}

		fetchClosedChannel := s.chanStateDB.FetchClosedChannelForID

		s.towerClient, err = wtclient.New(&wtclient.Config{
			FetchClosedChannel:     fetchClosedChannel,
			BuildBreachRetribution: buildBreachRetribution,
			SessionCloseRange:      cfg.WtClient.SessionCloseRange,
			ChainNotifier:          s.cc.ChainNotifier,
			SubscribeChannelEvents: func() (subscribe.Subscription,
				error) {

				return s.channelNotifier.
					SubscribeChannelEvents()
			},
			Signer:             cc.Wallet.Cfg.Signer,
			NewAddress:         newSweepPkScriptGen(cc.Wallet),
			SecretKeyRing:      s.cc.KeyRing,
			Dial:               cfg.net.Dial,
			AuthDial:           authDial,
			DB:                 dbs.TowerClientDB,
			Policy:             policy,
			ChainHash:          *s.cfg.ActiveNetParams.GenesisHash,
			MinBackoff:         10 * time.Second,
			MaxBackoff:         5 * time.Minute,
			MaxTasksInMemQueue: cfg.WtClient.MaxTasksInMemQueue,
		})
		if err != nil {
			return nil, err
		}

		// Copy the policy for legacy channels and set the blob flag
		// signalling support for anchor channels.
		anchorPolicy := policy
		anchorPolicy.TxPolicy.BlobType |=
			blob.Type(blob.FlagAnchorChannel)

		s.anchorTowerClient, err = wtclient.New(&wtclient.Config{
			FetchClosedChannel:     fetchClosedChannel,
			BuildBreachRetribution: buildBreachRetribution,
			SessionCloseRange:      cfg.WtClient.SessionCloseRange,
			ChainNotifier:          s.cc.ChainNotifier,
			SubscribeChannelEvents: func() (subscribe.Subscription,
				error) {

				return s.channelNotifier.
					SubscribeChannelEvents()
			},
			Signer:             cc.Wallet.Cfg.Signer,
			NewAddress:         newSweepPkScriptGen(cc.Wallet),
			SecretKeyRing:      s.cc.KeyRing,
			Dial:               cfg.net.Dial,
			AuthDial:           authDial,
			DB:                 dbs.TowerClientDB,
			Policy:             anchorPolicy,
			ChainHash:          *s.cfg.ActiveNetParams.GenesisHash,
			MinBackoff:         10 * time.Second,
			MaxBackoff:         5 * time.Minute,
			MaxTasksInMemQueue: cfg.WtClient.MaxTasksInMemQueue,
		})
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
					lnwire.NodeAnnouncement, error) {

					return s.genNodeAnnouncement(
						nil, modifier...,
					)
				}),
		})
	}

	// Create liveness monitor.
	s.createLivenessMonitor(cfg, cc)

	s.pcm.Config = &peerconn.ManagerConfig{
		PartialPeerConfig:       s.createPartialPeerConfig(),
		FeatureMgr:              s.featureMgr,
		Net:                     s.cfg.net,
		TorActive:               s.cfg.Tor.Active,
		StaggerInitialReconnect: s.cfg.StaggerInitialReconnect,
		ConnectionTimeout:       s.cfg.ConnectionTimeout,
		Listeners:               listeners,
		NetParams:               s.cfg.ActiveNetParams.Net,
		MinBackoff:              s.cfg.MinBackoff,
		MaxBackoff:              s.cfg.MaxBackoff,

		SubscribeTopology:      s.chanRouter.SubscribeTopology,
		CancelPeerReservations: s.fundingMgr.CancelPeerReservations,
	}

	return s, nil
}

// signAliasUpdate takes a ChannelUpdate and returns the signature. This is
// used for option_scid_alias channels where the ChannelUpdate to be sent back
// may differ from what is on disk.
func (s *server) signAliasUpdate(u *lnwire.ChannelUpdate) (*ecdsa.Signature,
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
func (s *server) createLivenessMonitor(cfg *Config, cc *chainreg.ChainControl) {
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
					s.createNewHiddenService,
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
			srvrLog.Infof("Cleanup failed: %v", err)
		}
	}
}

// Start starts the main daemon server, all requested listeners, and any helper
// goroutines.
// NOTE: This function is safe for concurrent access.
func (s *server) Start() error {
	var startErr error

	// If one sub system fails to start, the following code ensures that the
	// previous started ones are stopped. It also ensures a proper wallet
	// shutdown which is important for releasing its resources (boltdb, etc...)
	cleanup := cleaner{}

	s.start.Do(func() {
		if err := s.customMessageServer.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.customMessageServer.Stop)

		if s.hostAnn != nil {
			if err := s.hostAnn.Start(); err != nil {
				startErr = err
				return
			}
			cleanup = cleanup.add(s.hostAnn.Stop)
		}

		if s.livenessMonitor != nil {
			if err := s.livenessMonitor.Start(); err != nil {
				startErr = err
				return
			}
			cleanup = cleanup.add(s.livenessMonitor.Stop)
		}

		// Start the notification server. This is used so channel
		// management goroutines can be notified when a funding
		// transaction reaches a sufficient number of confirmations, or
		// when the input for the funding transaction is spent in an
		// attempt at an uncooperative close by the counterparty.
		if err := s.sigPool.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.sigPool.Stop)

		if err := s.writePool.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.writePool.Stop)

		if err := s.readPool.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.readPool.Stop)

		if err := s.cc.ChainNotifier.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.cc.ChainNotifier.Stop)

		if err := s.channelNotifier.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.channelNotifier.Stop)

		if err := s.htlcNotifier.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.htlcNotifier.Stop)

		if s.towerClient != nil {
			if err := s.towerClient.Start(); err != nil {
				startErr = err
				return
			}
			cleanup = cleanup.add(s.towerClient.Stop)
		}
		if s.anchorTowerClient != nil {
			if err := s.anchorTowerClient.Start(); err != nil {
				startErr = err
				return
			}
			cleanup = cleanup.add(s.anchorTowerClient.Stop)
		}

		if err := s.sweeper.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.sweeper.Stop)

		if err := s.utxoNursery.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.utxoNursery.Stop)

		if err := s.breachArbiter.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.breachArbiter.Stop)

		if err := s.fundingMgr.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.fundingMgr.Stop)

		// htlcSwitch must be started before chainArb since the latter
		// relies on htlcSwitch to deliver resolution message upon
		// start.
		if err := s.htlcSwitch.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.htlcSwitch.Stop)

		if err := s.interceptableSwitch.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.interceptableSwitch.Stop)

		if err := s.chainArb.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.chainArb.Stop)

		if err := s.authGossiper.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.authGossiper.Stop)

		if err := s.chanRouter.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.chanRouter.Stop)

		if err := s.invoices.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.invoices.Stop)

		if err := s.sphinx.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.sphinx.Stop)

		if err := s.chanStatusMgr.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.chanStatusMgr.Stop)

		// Start peer conn manager last to prevent connections before
		// init.
		if err := s.pcm.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(func() error {
			return s.pcm.Stop()
		})

		if err := s.chanEventStore.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(func() error {
			s.chanEventStore.Stop()
			return nil
		})

		s.missionControl.RunStoreTicker()
		cleanup.add(func() error {
			s.missionControl.StopStoreTicker()
			return nil
		})

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
			err := chanbackup.UnpackAndRecoverSingles(
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
			err := chanbackup.UnpackAndRecoverMulti(
				s.chansToRestore.PackedMultiChanBackup,
				s.cc.KeyRing, chanRestorer, s,
			)
			if err != nil {
				startErr = fmt.Errorf("unable to unpack chan "+
					"backup: %v", err)
				return
			}
		}

		if err := s.chanSubSwapper.Start(); err != nil {
			startErr = err
			return
		}
		cleanup = cleanup.add(s.chanSubSwapper.Stop)

		if s.torController != nil {
			if err := s.createNewHiddenService(); err != nil {
				startErr = err
				return
			}
			cleanup = cleanup.add(s.torController.Stop)
		}

		if s.natTraversal != nil {
			s.wg.Add(1)
			go s.watchExternalIP()
		}

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

			err = s.pcm.ConnectToPeer(
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
		if err := s.pcm.UpdatePersistentPeerAddrs(); err != nil {
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
			startErr = err
			return
		}
		if err := s.pcm.EstablishPersistentConnections(); err != nil {
			startErr = err
			return
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
		if shouldPeerBootstrap(s.cfg) {
			bootstrappers, err := initNetworkBootstrappers(s)
			if err != nil {
				startErr = err
				return
			}

			s.wg.Add(1)
			go s.pcm.PeerBootstrapper(
				defaultMinPeers, bootstrappers,
			)
		} else {
			srvrLog.Infof("Auto peer bootstrapping is disabled")
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

		close(s.quit)

		// Shutdown peer conn manager first to prevent conns during
		// shutdown.
		if err := s.pcm.Stop(); err != nil {
			srvrLog.Warnf("failed to stop PeerConnManager: %v", err)
		}

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
		if err := s.chanRouter.Stop(); err != nil {
			srvrLog.Warnf("failed to stop chanRouter: %v", err)
		}
		if err := s.chainArb.Stop(); err != nil {
			srvrLog.Warnf("failed to stop chainArb: %v", err)
		}
		if err := s.fundingMgr.Stop(); err != nil {
			srvrLog.Warnf("failed to stop fundingMgr: %v", err)
		}
		if err := s.breachArbiter.Stop(); err != nil {
			srvrLog.Warnf("failed to stop breachArbiter: %v", err)
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
		if err := s.channelNotifier.Stop(); err != nil {
			srvrLog.Warnf("failed to stop channelNotifier: %v", err)
		}
		if err := s.htlcNotifier.Stop(); err != nil {
			srvrLog.Warnf("failed to stop htlcNotifier: %v", err)
		}
		if err := s.chanSubSwapper.Stop(); err != nil {
			srvrLog.Warnf("failed to stop chanSubSwapper: %v", err)
		}
		if err := s.cc.ChainNotifier.Stop(); err != nil {
			srvrLog.Warnf("Unable to stop ChainNotifier: %v", err)
		}
		s.chanEventStore.Stop()
		s.missionControl.StopStoreTicker()

		// Now that all connections have been torn down, stop the tower
		// client which will reliably flush all queued states to the
		// tower. If this is halted for any reason, the force quit timer
		// will kick in and abort to allow this method to return.
		if s.towerClient != nil {
			if err := s.towerClient.Stop(); err != nil {
				srvrLog.Warnf("Unable to shut down tower "+
					"client: %v", err)
			}
		}
		if s.anchorTowerClient != nil {
			if err := s.anchorTowerClient.Stop(); err != nil {
				srvrLog.Warnf("Unable to shut down anchor "+
					"tower client: %v", err)
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

			err = s.pcm.BroadcastMessage(nil, &newNodeAnn)
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
	graphBootstrapper, err := discovery.NewGraphBootstrapper(chanGraph)
	if err != nil {
		return nil, err
	}
	bootStrappers = append(bootStrappers, graphBootstrapper)

	// If this isn't simnet mode, then one of our additional bootstrapping
	// sources will be the set of running DNS seeds.
	if !s.cfg.Bitcoin.SimNet {
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

// createNewHiddenService automatically sets up a v2 or v3 onion service in
// order to listen for inbound connections over Tor.
func (s *server) createNewHiddenService() error {
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
		nil, func(currentAnn *lnwire.NodeAnnouncement) {
			currentAnn.Addresses = append(currentAnn.Addresses, addr)
		},
	)
	if err != nil {
		return fmt.Errorf("unable to generate new node "+
			"announcement: %v", err)
	}

	// Finally, we'll update the on-disk version of our announcement so it
	// will eventually propagate to nodes in the network.
	selfNode := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Unix(int64(newNodeAnn.Timestamp), 0),
		Addresses:            newNodeAnn.Addresses,
		Alias:                newNodeAnn.Alias.String(),
		Features: lnwire.NewFeatureVector(
			newNodeAnn.Features, lnwire.Features,
		),
		Color:        newNodeAnn.RGBColor,
		AuthSigBytes: newNodeAnn.Signature.ToSignatureBytes(),
	}
	copy(selfNode.PubKeyBytes[:], s.identityECDH.PubKey().SerializeCompressed())
	if err := s.graphDB.SetSourceNode(selfNode); err != nil {
		return fmt.Errorf("can't set self node: %v", err)
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
func (s *server) getNodeAnnouncement() lnwire.NodeAnnouncement {
	s.mu.Lock()
	defer s.mu.Unlock()

	return *s.currentNodeAnn
}

// genNodeAnnouncement generates and returns the current fully signed node
// announcement. The time stamp of the announcement will be updated in order
// to ensure it propagates through the network.
func (s *server) genNodeAnnouncement(features *lnwire.RawFeatureVector,
	modifiers ...netann.NodeAnnModifier) (lnwire.NodeAnnouncement, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	// First, try to update our feature manager with the updated set of
	// features.
	if features != nil {
		proposedFeatures := map[feature.Set]*lnwire.RawFeatureVector{
			feature.SetNodeAnn: features,
		}
		err := s.featureMgr.UpdateFeatureSets(proposedFeatures)
		if err != nil {
			return lnwire.NodeAnnouncement{}, err
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
		modifier(s.currentNodeAnn)
	}

	// Sign a new update after applying all of the passed modifiers.
	err := netann.SignNodeAnnouncement(
		s.nodeSigner, s.identityKeyLoc, s.currentNodeAnn,
	)
	if err != nil {
		return lnwire.NodeAnnouncement{}, err
	}

	return *s.currentNodeAnn, nil
}

// updateAndBrodcastSelfNode generates a new node announcement
// applying the giving modifiers and updating the time stamp
// to ensure it propagates through the network. Then it brodcasts
// it to the network.
func (s *server) updateAndBrodcastSelfNode(features *lnwire.RawFeatureVector,
	modifiers ...netann.NodeAnnModifier) error {

	newNodeAnn, err := s.genNodeAnnouncement(features, modifiers...)
	if err != nil {
		return fmt.Errorf("unable to generate new node "+
			"announcement: %v", err)
	}

	// Update the on-disk version of our announcement.
	// Load and modify self node istead of creating anew instance so we
	// don't risk overwriting any existing values.
	selfNode, err := s.graphDB.SourceNode()
	if err != nil {
		return fmt.Errorf("unable to get current source node: %v", err)
	}

	selfNode.HaveNodeAnnouncement = true
	selfNode.LastUpdate = time.Unix(int64(newNodeAnn.Timestamp), 0)
	selfNode.Addresses = newNodeAnn.Addresses
	selfNode.Alias = newNodeAnn.Alias.String()
	selfNode.Features = s.featureMgr.Get(feature.SetNodeAnn)
	selfNode.Color = newNodeAnn.RGBColor
	selfNode.AuthSigBytes = newNodeAnn.Signature.ToSignatureBytes()

	copy(selfNode.PubKeyBytes[:], s.identityECDH.PubKey().SerializeCompressed())

	if err := s.graphDB.SetSourceNode(selfNode); err != nil {
		return fmt.Errorf("can't set self node: %v", err)
	}

	// Finally, propagate it to the nodes in the network.
	err = s.pcm.BroadcastMessage(nil, &newNodeAnn)
	if err != nil {
		rpcsLog.Debugf("Unable to broadcast new node "+
			"announcement to peers: %v", err)
		return err
	}

	return nil
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

// createPartialPeerConfig creates a partially filled peer config that will be
// used by peer conn manager when making new peers.
func (s *server) createPartialPeerConfig() peer.Config {
	const feeFactor = 1000

	return peer.Config{
		OutgoingCltvRejectDelta: lncfg.DefaultOutgoingCltvRejectDelta,
		ChanActiveTimeout:       s.cfg.ChanEnableTimeout,

		WritePool:         s.writePool,
		ReadPool:          s.readPool,
		Switch:            s.htlcSwitch,
		InterceptSwitch:   s.interceptableSwitch,
		ChannelDB:         s.chanStateDB,
		ChannelGraph:      s.graphDB,
		ChainArb:          s.chainArb,
		AuthGossiper:      s.authGossiper,
		ChanStatusMgr:     s.chanStatusMgr,
		ChainIO:           s.cc.ChainIO,
		FeeEstimator:      s.cc.FeeEstimator,
		Signer:            s.cc.Wallet.Cfg.Signer,
		SigPool:           s.sigPool,
		Wallet:            s.cc.Wallet,
		ChainNotifier:     s.cc.ChainNotifier,
		RoutingPolicy:     s.cc.RoutingPolicy,
		Sphinx:            s.sphinx,
		WitnessBeacon:     s.witnessBeacon,
		Invoices:          s.invoices,
		ChannelNotifier:   s.channelNotifier,
		HtlcNotifier:      s.htlcNotifier,
		TowerClient:       s.towerClient,
		AnchorTowerClient: s.anchorTowerClient,
		GenNodeAnnouncement: func(...netann.NodeAnnModifier) (
			lnwire.NodeAnnouncement, error) {

			return s.genNodeAnnouncement(nil)
		},

		PongBuf: s.pongBuf,

		DisconnectPeer: s.pcm.DisconnectPeer,
		PrunePersistentPeerConnection: s.pcm.
			PrunePersistentPeerConnection,

		FetchLastChanUpdate: s.fetchLastChanUpdate(),

		FundingManager: s.fundingMgr,

		Hodl:                    s.cfg.Hodl,
		UnsafeReplay:            s.cfg.UnsafeReplay,
		MaxOutgoingCltvExpiry:   s.cfg.MaxOutgoingCltvExpiry,
		MaxChannelFeeAllocation: s.cfg.MaxChannelFeeAllocation,
		CoopCloseTargetConfs:    s.cfg.CoopCloseTargetConfs,
		MaxAnchorsCommitFeeRate: chainfee.SatPerKVByte(
			s.cfg.MaxCommitFeeRateAnchors * feeFactor).
			FeePerKWeight(),
		ChannelCommitInterval:  s.cfg.ChannelCommitInterval,
		PendingCommitInterval:  s.cfg.PendingCommitInterval,
		ChannelCommitBatchSize: s.cfg.ChannelCommitBatchSize,

		HandleCustomMessage: s.handleCustomMessage,

		// TODO(yy): interface alias manager.
		GetAliases:    s.aliasMgr.GetAliases,
		RequestAlias:  s.aliasMgr.RequestAlias,
		AddLocalAlias: s.aliasMgr.AddLocalAlias,
	}
}

// OpenChannel sends a request to the server to open a channel to the specified
// peer identified by nodeKey with the passed channel funding parameters.
//
// NOTE: This function is safe for concurrent access.
func (s *server) OpenChannel(
	req *funding.InitFundingMsg) (chan *lnrpc.OpenStatusUpdate,
	chan error) {

	// The updateChan will have a buffer of 2, since we expect a ChanPending
	// + a ChanOpen update, and we want to make sure the funding process is
	// not blocked if the caller is not reading the updates.
	req.Updates = make(chan *lnrpc.OpenStatusUpdate, 2)
	req.Err = make(chan error, 1)

	// First attempt to locate the target peer to open a channel with, if
	// we're unable to locate the peer then this request will fail.
	pubKeyBytes := req.TargetPubkey.SerializeCompressed()
	peer, err := s.pcm.FindPeer(req.TargetPubkey)
	if err != nil {
		req.Err <- fmt.Errorf("peer %x is not online", pubKeyBytes)
		return req.Updates, req.Err
	}
	req.Peer = peer

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

	// If the fee rate wasn't specified, then we'll use a default
	// confirmation target.
	const defaultConfTarget = 6
	if req.FundingFeePerKw == 0 {
		estimator := s.cc.FeeEstimator
		feeRate, err := estimator.EstimateFeePerKW(defaultConfTarget)
		if err != nil {
			req.Err <- err
			return req.Updates, req.Err
		}
		req.FundingFeePerKw = feeRate
	}

	// Spawn a goroutine to send the funding workflow request to the funding
	// manager. This allows the server to continue handling queries instead
	// of blocking on this request which is exported as a synchronous
	// request to the outside world.
	go s.fundingMgr.InitFundingWorkflow(req)

	return req.Updates, req.Err
}

// fetchLastChanUpdate returns a function which is able to retrieve our latest
// channel update for a target channel.
func (s *server) fetchLastChanUpdate() func(lnwire.ShortChannelID) (
	*lnwire.ChannelUpdate, error) {

	ourPubKey := s.identityECDH.PubKey().SerializeCompressed()
	return func(cid lnwire.ShortChannelID) (*lnwire.ChannelUpdate, error) {
		info, edge1, edge2, err := s.chanRouter.GetChannelByID(cid)
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
func (s *server) applyChannelUpdate(update *lnwire.ChannelUpdate,
	op *wire.OutPoint, useAlias bool) error {

	var (
		peerAlias    *lnwire.ShortChannelID
		defaultAlias lnwire.ShortChannelID
	)

	chanID := lnwire.NewChanIDFromOutPoint(op)

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

// newSweepPkScriptGen creates closure that generates a new public key script
// which should be used to sweep any funds into the on-chain wallet.
// Specifically, the script generated is a version 0, pay-to-witness-pubkey-hash
// (p2wkh) output.
func newSweepPkScriptGen(
	wallet lnwallet.WalletController) func() ([]byte, error) {

	return func() ([]byte, error) {
		sweepAddr, err := wallet.NewAddress(
			lnwallet.TaprootPubkey, false,
			lnwallet.DefaultAccountName,
		)
		if err != nil {
			return nil, err
		}

		return txscript.PayToAddrScript(sweepAddr)
	}
}

// shouldPeerBootstrap returns true if we should attempt to perform peer
// bootstrapping to actively seek our peers using the set of active network
// bootstrappers.
func shouldPeerBootstrap(cfg *Config) bool {
	isSimnet := cfg.Bitcoin.SimNet
	isSignet := cfg.Bitcoin.SigNet
	isRegtest := cfg.Bitcoin.RegTest
	isDevNetwork := isSimnet || isSignet || isRegtest

	// TODO(yy): remove the check on simnet/regtest such that the itest is
	// covering the bootstrapping process.
	return !cfg.NoNetBootstrap && !isDevNetwork
}

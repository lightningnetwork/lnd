package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/color"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/connmgr"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/htlcswitch"
)

var (
	// ErrPeerNotFound signals that the server has no connection to the
	// given peer.
	ErrPeerNotFound = errors.New("unable to find peer")

	// ErrServerShuttingDown indicates that the server is in the process of
	// gracefully exiting.
	ErrServerShuttingDown = errors.New("server is shutting down")
)

// server is the main server of the Lightning Network Daemon. The server houses
// global state pertaining to the wallet, database, and the rpcserver.
// Additionally, the server is also used as a central messaging bus to interact
// with any of its companion objects.
type server struct {
	started  int32 // atomic
	shutdown int32 // atomic

	// identityPriv is the private key used to authenticate any incoming
	// connections.
	identityPriv *btcec.PrivateKey

	// nodeSigner is an implementation of the MessageSigner implementation
	// that's backed by the identity private key of the running lnd node.
	nodeSigner *nodeSigner

	// lightningID is the sha256 of the public key corresponding to our
	// long-term identity private key.
	lightningID [32]byte

	mu         sync.Mutex
	peersByID  map[int32]*peer
	peersByPub map[string]*peer

	inboundPeers  map[string]*peer
	outboundPeers map[string]*peer

	peerConnectedListeners map[string][]chan<- struct{}

	persistentPeers    map[string]struct{}
	persistentConnReqs map[string][]*connmgr.ConnReq

	// ignorePeerTermination tracks peers for which the server has initiated
	// a disconnect. Adding a peer to this map causes the peer termination
	// watcher to short circuit in the event that peers are purposefully
	// disconnected.
	ignorePeerTermination map[*peer]struct{}

	cc *chainControl

	fundingMgr *fundingManager

	chanDB *channeldb.DB

	htlcSwitch    *htlcswitch.Switch
	invoices      *invoiceRegistry
	breachArbiter *breachArbiter

	chanRouter *routing.ChannelRouter

	authGossiper *discovery.AuthenticatedGossiper

	utxoNursery *utxoNursery

	sphinx *htlcswitch.OnionProcessor

	connMgr *connmgr.ConnManager

	// globalFeatures feature vector which affects HTLCs and thus are also
	// advertised to other nodes.
	globalFeatures *lnwire.FeatureVector

	// currentNodeAnn is the node announcement that has been broadcast to
	// the network upon startup, if the attributes of the node (us) has
	// changed since last start.
	currentNodeAnn *lnwire.NodeAnnouncement

	quit chan struct{}

	wg sync.WaitGroup
}

// newServer creates a new instance of the server which is to listen using the
// passed listener address.
func newServer(listenAddrs []string, chanDB *channeldb.DB, cc *chainControl,
	privKey *btcec.PrivateKey) (*server, error) {

	var err error

	listeners := make([]net.Listener, len(listenAddrs))
	for i, addr := range listenAddrs {
		listeners[i], err = brontide.NewListener(privKey, addr)
		if err != nil {
			return nil, err
		}
	}

	globalFeatures := lnwire.NewRawFeatureVector()

	serializedPubKey := privKey.PubKey().SerializeCompressed()
	s := &server{
		chanDB: chanDB,
		cc:     cc,

		invoices: newInvoiceRegistry(chanDB),

		identityPriv: privKey,
		nodeSigner:   newNodeSigner(privKey),

		// TODO(roasbeef): derive proper onion key based on rotation
		// schedule
		sphinx: htlcswitch.NewOnionProcessor(
			sphinx.NewRouter(privKey, activeNetParams.Params)),
		lightningID: sha256.Sum256(serializedPubKey),

		persistentPeers:       make(map[string]struct{}),
		persistentConnReqs:    make(map[string][]*connmgr.ConnReq),
		ignorePeerTermination: make(map[*peer]struct{}),

		peersByID:              make(map[int32]*peer),
		peersByPub:             make(map[string]*peer),
		inboundPeers:           make(map[string]*peer),
		outboundPeers:          make(map[string]*peer),
		peerConnectedListeners: make(map[string][]chan<- struct{}),

		globalFeatures: lnwire.NewFeatureVector(globalFeatures,
			lnwire.GlobalFeatures),
		quit: make(chan struct{}),
	}

	// If the debug HTLC flag is on, then we invoice a "master debug"
	// invoice which all outgoing payments will be sent and all incoming
	// HTLCs with the debug R-Hash immediately settled.
	if cfg.DebugHTLC {
		kiloCoin := btcutil.Amount(btcutil.SatoshiPerBitcoin * 1000)
		s.invoices.AddDebugInvoice(kiloCoin, *debugPre)
		srvrLog.Debugf("Debug HTLC invoice inserted, preimage=%x, hash=%x",
			debugPre[:], debugHash[:])
	}

	s.htlcSwitch = htlcswitch.New(htlcswitch.Config{
		SelfKey: s.identityPriv.PubKey(),
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

			select {
			case peer.localCloseChanReqs <- request:
				srvrLog.Infof("Local close channel request "+
					"delivered to peer: %x", pubKey[:])
			case <-peer.quit:
				srvrLog.Errorf("Unable to deliver local close "+
					"channel request to peer %x, err: %v",
					pubKey[:], err)
			}
		},
	})

	// If external IP addresses have been specified, add those to the list
	// of this server's addresses.
	selfAddrs := make([]net.Addr, 0, len(cfg.ExternalIPs))
	for _, ip := range cfg.ExternalIPs {
		var addr string
		_, _, err = net.SplitHostPort(ip)
		if err != nil {
			addr = net.JoinHostPort(ip, strconv.Itoa(defaultPeerPort))
		} else {
			addr = ip
		}

		lnAddr, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			return nil, err
		}

		selfAddrs = append(selfAddrs, lnAddr)
	}

	chanGraph := chanDB.ChannelGraph()

	defaultColor := color.RGBA{ // #3399FF
		R: 51,
		G: 153,
		B: 255,
	}

	// TODO(roasbeef): make alias configurable
	alias, err := lnwire.NewNodeAlias(hex.EncodeToString(serializedPubKey[:10]))
	if err != nil {
		return nil, err
	}
	selfNode := &channeldb.LightningNode{
		HaveNodeAnnouncement: true,
		LastUpdate:           time.Now(),
		Addresses:            selfAddrs,
		PubKey:               privKey.PubKey(),
		Alias:                alias.String(),
		Features:             s.globalFeatures,
		Color:                defaultColor,
	}

	// If our information has changed since our last boot, then we'll
	// re-sign our node announcement so a fresh authenticated version of it
	// can be propagated throughout the network upon startup.
	//
	// TODO(roasbeef): don't always set timestamp above to _now.
	nodeAnn := &lnwire.NodeAnnouncement{
		Timestamp: uint32(selfNode.LastUpdate.Unix()),
		Addresses: selfNode.Addresses,
		NodeID:    selfNode.PubKey,
		Alias:     alias,
		Features:  selfNode.Features.RawFeatureVector,
		RGBColor:  defaultColor,
	}
	selfNode.AuthSig, err = discovery.SignAnnouncement(s.nodeSigner,
		s.identityPriv.PubKey(), nodeAnn,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to generate signature for "+
			"self node announcement: %v", err)
	}

	if err := chanGraph.SetSourceNode(selfNode); err != nil {
		return nil, fmt.Errorf("can't set self node: %v", err)
	}

	nodeAnn.Signature = selfNode.AuthSig
	s.currentNodeAnn = nodeAnn

	s.chanRouter, err = routing.New(routing.Config{
		Graph:     chanGraph,
		Chain:     cc.chainIO,
		ChainView: cc.chainView,
		SendToSwitch: func(firstHop *btcec.PublicKey,
			htlcAdd *lnwire.UpdateAddHTLC,
			circuit *sphinx.Circuit) ([32]byte, error) {

			// Using the created circuit, initialize the error
			// decrypter so we can parse+decode any failures
			// incurred by this payment within the switch.
			errorDecryptor := &htlcswitch.SphinxErrorDecrypter{
				OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(circuit),
			}

			var firstHopPub [33]byte
			copy(firstHopPub[:], firstHop.SerializeCompressed())

			return s.htlcSwitch.SendHTLC(firstHopPub, htlcAdd, errorDecryptor)
		},
		ChannelPruneExpiry: time.Duration(time.Hour * 24 * 14),
		GraphPruneInterval: time.Duration(time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("can't create router: %v", err)
	}

	s.authGossiper, err = discovery.New(discovery.Config{
		Router:           s.chanRouter,
		Notifier:         s.cc.chainNotifier,
		ChainHash:        *activeNetParams.GenesisHash,
		Broadcast:        s.BroadcastMessage,
		SendToPeer:       s.SendToPeer,
		ProofMatureDelta: 0,
		TrickleDelay:     time.Millisecond * time.Duration(cfg.TrickleDelay),
		RetransmitDelay:  time.Minute * 30,
		DB:               chanDB,
		AnnSigner:        s.nodeSigner,
	},
		s.identityPriv.PubKey(),
	)
	if err != nil {
		return nil, err
	}

	utxnStore, err := newNurseryStore(&bitcoinGenesis, chanDB)
	if err != nil {
		srvrLog.Errorf("unable to create nursery store: %v", err)
		return nil, err
	}

	s.utxoNursery = newUtxoNursery(&NurseryConfig{
		ChainIO:   cc.chainIO,
		ConfDepth: 1,
		DB:        chanDB,
		Estimator: cc.feeEstimator,
		GenSweepScript: func() ([]byte, error) {
			return newSweepPkScript(cc.wallet)
		},
		Notifier:           cc.chainNotifier,
		PublishTransaction: cc.wallet.PublishTransaction,
		Signer:             cc.wallet.Cfg.Signer,
		Store:              utxnStore,
	})

	// Construct a closure that wraps the htlcswitch's CloseLink method.
	closeLink := func(chanPoint *wire.OutPoint,
		closureType htlcswitch.ChannelCloseType) {
		// TODO(conner): Properly respect the update and error channels
		// returned by CloseLink.
		s.htlcSwitch.CloseLink(chanPoint, closureType, 0)
	}

	s.breachArbiter = newBreachArbiter(&BreachConfig{
		ChainIO:   s.cc.chainIO,
		CloseLink: closeLink,
		DB:        chanDB,
		Estimator: s.cc.feeEstimator,
		GenSweepScript: func() ([]byte, error) {
			return newSweepPkScript(cc.wallet)
		},
		Notifier:           cc.chainNotifier,
		PublishTransaction: cc.wallet.PublishTransaction,
		Signer:             cc.wallet.Cfg.Signer,
		Store:              newRetributionStore(chanDB),
	})

	// Create the connection manager which will be responsible for
	// maintaining persistent outbound connections and also accepting new
	// incoming connections
	cmgr, err := connmgr.New(&connmgr.Config{
		Listeners:      listeners,
		OnAccept:       s.InboundPeerConnected,
		RetryDuration:  time.Second * 5,
		TargetOutbound: 100,
		GetNewAddress:  nil,
		Dial:           noiseDial(s.identityPriv),
		OnConnection:   s.OutboundPeerConnected,
	})
	if err != nil {
		return nil, err
	}
	s.connMgr = cmgr

	return s, nil
}

// Started returns true if the server has been started, and false otherwise.
// NOTE: This function is safe for concurrent access.
func (s *server) Started() bool {
	return atomic.LoadInt32(&s.started) != 0
}

// Start starts the main daemon server, all requested listeners, and any helper
// goroutines.
// NOTE: This function is safe for concurrent access.
func (s *server) Start() error {
	// Already running?
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		return nil
	}

	// Start the notification server. This is used so channel management
	// goroutines can be notified when a funding transaction reaches a
	// sufficient number of confirmations, or when the input for the
	// funding transaction is spent in an attempt at an uncooperative close
	// by the counterparty.
	if err := s.cc.chainNotifier.Start(); err != nil {
		return err
	}

	if err := s.htlcSwitch.Start(); err != nil {
		return err
	}
	if err := s.utxoNursery.Start(); err != nil {
		return err
	}
	if err := s.breachArbiter.Start(); err != nil {
		return err
	}
	if err := s.authGossiper.Start(); err != nil {
		return err
	}
	if err := s.chanRouter.Start(); err != nil {
		return err
	}

	// With all the relevant sub-systems started, we'll now attempt to
	// establish persistent connections to our direct channel collaborators
	// within the network.
	if err := s.establishPersistentConnections(); err != nil {
		return err
	}

	go s.connMgr.Start()

	// If network bootstrapping hasn't been disabled, then we'll configure
	// the set of active bootstrappers, and launch a dedicated goroutine to
	// maintain a set of persistent connections.
	if !cfg.NoNetBootstrap && !(cfg.Bitcoin.SimNet || cfg.Litecoin.SimNet) &&
		!(cfg.Bitcoin.RegTest || cfg.Litecoin.RegTest) {
		networkBootStrappers, err := initNetworkBootstrappers(s)
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go s.peerBootstrapper(3, networkBootStrappers)
	} else {
		srvrLog.Infof("Auto peer bootstrapping is disabled")
	}

	return nil
}

// Stop gracefully shutsdown the main daemon server. This function will signal
// any active goroutines, or helper objects to exit, then blocks until they've
// all successfully exited. Additionally, any/all listeners are closed.
// NOTE: This function is safe for concurrent access.
func (s *server) Stop() error {
	// Bail if we're already shutting down.
	if !atomic.CompareAndSwapInt32(&s.shutdown, 0, 1) {
		return nil
	}

	close(s.quit)

	// Shutdown the wallet, funding manager, and the rpc server.
	s.cc.chainNotifier.Stop()
	s.chanRouter.Stop()
	s.htlcSwitch.Stop()
	s.utxoNursery.Stop()
	s.breachArbiter.Stop()
	s.authGossiper.Stop()
	s.cc.wallet.Shutdown()
	s.cc.chainView.Stop()
	s.connMgr.Stop()
	s.cc.feeEstimator.Stop()

	// Disconnect from each active peers to ensure that
	// peerTerminationWatchers signal completion to each peer.
	for _, peer := range s.Peers() {
		s.DisconnectPeer(peer.addr.IdentityKey)
	}

	// Wait for all lingering goroutines to quit.
	s.wg.Wait()

	return nil
}

// Stopped returns true if the server has been instructed to shutdown.
// NOTE: This function is safe for concurrent access.
func (s *server) Stopped() bool {
	return atomic.LoadInt32(&s.shutdown) != 0
}

// WaitForShutdown blocks until all goroutines have been stopped.
func (s *server) WaitForShutdown() {
	s.wg.Wait()
}

// initNetworkBootstrappers initializes a set of network peer bootstrappers
// based on the server, and currently active bootstrap mechanisms as defined
// within the current configuration.
func initNetworkBootstrappers(s *server) ([]discovery.NetworkPeerBootstrapper, error) {
	srvrLog.Infof("Initializing peer network boostrappers!")

	var bootStrappers []discovery.NetworkPeerBootstrapper

	// First, we'll create an instance of the ChannelGraphBootstrapper as
	// this can be used by default if we've already partially seeded the
	// network.
	chanGraph := autopilot.ChannelGraphFromDatabase(s.chanDB.ChannelGraph())
	graphBootstrapper, err := discovery.NewGraphBootstrapper(chanGraph)
	if err != nil {
		return nil, err
	}
	bootStrappers = append(bootStrappers, graphBootstrapper)

	// If this isn't simnet mode, then one of our additional bootstrapping
	// sources will be the set of running DNS seeds.
	if !cfg.Bitcoin.SimNet || !cfg.Litecoin.SimNet {
		dnsSeeds, ok := chainDNSSeeds[*activeNetParams.GenesisHash]

		// If we have a set of DNS seeds for this chain, then we'll add
		// it as an additional boostrapping source.
		if ok {
			srvrLog.Infof("Creating DNS peer boostrapper with "+
				"seeds: %v", dnsSeeds)

			dnsBootStrapper, err := discovery.NewDNSSeedBootstrapper(
				dnsSeeds,
			)
			if err != nil {
				return nil, err
			}

			bootStrappers = append(bootStrappers, dnsBootStrapper)
		}
	}

	return bootStrappers, nil
}

// peerBootstrapper is a goroutine which is tasked with attempting to establish
// and maintain a target min number of outbound connections. With this
// invariant, we ensure that our node is connected to a diverse set of peers
// and that nodes newly joining the network receive an up to date network view
// as soon as possible.
func (s *server) peerBootstrapper(numTargetPeers uint32,
	bootStrappers []discovery.NetworkPeerBootstrapper) {

	defer s.wg.Done()

	// To kick things off, we'll attempt to first query the set of
	// bootstrappers for enough address to fill our quot.
	bootStrapAddrs, err := discovery.MultiSourceBootstrap(
		nil, numTargetPeers, bootStrappers...,
	)
	if err != nil {
		// TODO(roasbeef): panic?
		srvrLog.Errorf("Unable to retrieve initial bootstrap "+
			"peers: %v", err)
		return
	}

	srvrLog.Debugf("Attempting to bootstrap connectivity with %v initial "+
		"peers", len(bootStrapAddrs))

	// With our initial set of peers obtained, we'll launch a goroutine to
	// attempt to connect out to each of them. We'll be waking up shortly
	// below to sample how many of these connections succeeded.
	for _, addr := range bootStrapAddrs {
		go func(a *lnwire.NetAddress) {
			conn, err := brontide.Dial(s.identityPriv, a)
			if err != nil {
				srvrLog.Errorf("unable to connect to %v: %v",
					a, err)
				return
			}

			s.OutboundPeerConnected(nil, conn)
		}(addr)
	}

	// We'll start with a 15 second backoff, and double the time every time
	// an epoch fails up to a ceiling.
	const backOffCeliing = time.Minute * 5
	backOff := time.Second * 15

	// We'll create a new ticker to wake us up every 15 seconds so we can
	// see if we've reached our minimum number of peers.
	sampleTicker := time.NewTicker(backOff)
	defer sampleTicker.Stop()

	// We'll use the number of attempts and errors to determine if we need
	// to increase the time between discovery epochs.
	var epochErrors, epochAttempts uint32

	for {
		select {
		// The ticker has just woken us up, so we'll need to check if
		// we need to attempt to connect our to any more peers.
		case <-sampleTicker.C:
			// Obtain the current number of peers, so we can gauge
			// if we need to sample more peers or not.
			s.mu.Lock()
			numActivePeers := uint32(len(s.peersByPub))
			s.mu.Unlock()

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
				if backOff > backOffCeliing {
					backOff = backOffCeliing
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
			s.mu.Lock()
			ignoreList := make(map[autopilot.NodeID]struct{})
			for _, peer := range s.peersByPub {
				nID := autopilot.NewNodeID(peer.addr.IdentityKey)
				ignoreList[nID] = struct{}{}
			}
			s.mu.Unlock()

			peerAddrs, err := discovery.MultiSourceBootstrap(
				ignoreList, numNeeded*2, bootStrappers...,
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
					conn, err := brontide.Dial(s.identityPriv, a)
					if err != nil {
						srvrLog.Errorf("unable to connect "+
							"to %v: %v", a, err)
						atomic.AddUint32(&epochErrors, 1)
						return
					}
					s.OutboundPeerConnected(nil, conn)
				}(addr)
			}
		case <-s.quit:
			return
		}
	}
}

// genNodeAnnouncement generates and returns the current fully signed node
// announcement. If refresh is true, then the time stamp of the announcement
// will be updated in order to ensure it propagates through the network.
func (s *server) genNodeAnnouncement(
	refresh bool) (lnwire.NodeAnnouncement, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	if !refresh {
		return *s.currentNodeAnn, nil
	}

	var err error

	newStamp := uint32(time.Now().Unix())
	if newStamp <= s.currentNodeAnn.Timestamp {
		newStamp = s.currentNodeAnn.Timestamp + 1
	}

	s.currentNodeAnn.Timestamp = newStamp
	s.currentNodeAnn.Signature, err = discovery.SignAnnouncement(
		s.nodeSigner, s.identityPriv.PubKey(), s.currentNodeAnn,
	)

	return *s.currentNodeAnn, err
}

type nodeAddresses struct {
	pubKey    *btcec.PublicKey
	addresses []*net.TCPAddr
}

// establishPersistentConnections attempts to establish persistent connections
// to all our direct channel collaborators.  In order to promote liveness of
// our active channels, we instruct the connection manager to attempt to
// establish and maintain persistent connections to all our direct channel
// counterparties.
func (s *server) establishPersistentConnections() error {
	// nodeAddrsMap stores the combination of node public keys and
	// addresses that we'll attempt to reconnect to. PubKey strings are
	// used as keys since other PubKey forms can't be compared.
	nodeAddrsMap := map[string]*nodeAddresses{}

	// Iterate through the list of LinkNodes to find addresses we should
	// attempt to connect to based on our set of previous connections. Set
	// the reconnection port to the default peer port.
	linkNodes, err := s.chanDB.FetchAllLinkNodes()
	if err != nil && err != channeldb.ErrLinkNodesNotFound {
		return err
	}
	for _, node := range linkNodes {
		for _, address := range node.Addresses {
			if address.Port == 0 {
				address.Port = defaultPeerPort
			}
		}
		pubStr := string(node.IdentityPub.SerializeCompressed())

		nodeAddrs := &nodeAddresses{
			pubKey:    node.IdentityPub,
			addresses: node.Addresses,
		}
		nodeAddrsMap[pubStr] = nodeAddrs
	}

	// After checking our previous connections for addresses to connect to,
	// iterate through the nodes in our channel graph to find addresses
	// that have been added via NodeAnnouncement messages.
	chanGraph := s.chanDB.ChannelGraph()
	sourceNode, err := chanGraph.SourceNode()
	if err != nil {
		return err
	}
	// TODO(roasbeef): instead iterate over link nodes and query graph for
	// each of the nodes.
	err = sourceNode.ForEachChannel(nil, func(
		_ *bolt.Tx,
		_ *channeldb.ChannelEdgeInfo,
		policy, _ *channeldb.ChannelEdgePolicy) error {

		pubStr := string(policy.Node.PubKey.SerializeCompressed())

		// Add addresses from channel graph/NodeAnnouncements to the
		// list of addresses we'll connect to. If there are duplicates
		// that have different ports specified, the port from the
		// channel graph should supersede the port from the link node.
		var addrs []*net.TCPAddr
		linkNodeAddrs, ok := nodeAddrsMap[pubStr]
		if ok {
			for _, lnAddress := range linkNodeAddrs.addresses {
				var addrMatched bool
				for _, polAddress := range policy.Node.Addresses {
					polTCPAddr, ok := polAddress.(*net.TCPAddr)
					if ok && polTCPAddr.IP.Equal(lnAddress.IP) {
						addrMatched = true
						addrs = append(addrs, polTCPAddr)
					}
				}
				if !addrMatched {
					addrs = append(addrs, lnAddress)
				}
			}
		} else {
			for _, addr := range policy.Node.Addresses {
				polTCPAddr, ok := addr.(*net.TCPAddr)
				if ok {
					addrs = append(addrs, polTCPAddr)
				}
			}
		}

		nodeAddrsMap[pubStr] = &nodeAddresses{
			pubKey:    policy.Node.PubKey,
			addresses: addrs,
		}

		return nil
	})
	if err != nil && err != channeldb.ErrGraphNoEdgesFound {
		return err
	}

	// Iterate through the combined list of addresses from prior links and
	// node announcements and attempt to reconnect to each node.
	for pubStr, nodeAddr := range nodeAddrsMap {
		// Add this peer to the set of peers we should maintain a
		// persistent connection with.
		s.persistentPeers[pubStr] = struct{}{}

		for _, address := range nodeAddr.addresses {
			// Create a wrapper address which couples the IP and
			// the pubkey so the brontide authenticated connection
			// can be established.
			lnAddr := &lnwire.NetAddress{
				IdentityKey: nodeAddr.pubKey,
				Address:     address,
			}
			srvrLog.Debugf("Attempting persistent connection to "+
				"channel peer %v", lnAddr)

			// Send the persistent connection request to the
			// connection manager, saving the request itself so we
			// can cancel/restart the process as needed.
			connReq := &connmgr.ConnReq{
				Addr:      lnAddr,
				Permanent: true,
			}

			s.persistentConnReqs[pubStr] = append(
				s.persistentConnReqs[pubStr], connReq)

			go s.connMgr.Connect(connReq)
		}
	}

	return nil
}

// BroadcastMessage sends a request to the server to broadcast a set of
// messages to all peers other than the one specified by the `skip` parameter.
//
// NOTE: This function is safe for concurrent access.
func (s *server) BroadcastMessage(skip *btcec.PublicKey,
	msgs ...lnwire.Message) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.broadcastMessages(skip, msgs)
}

// broadcastMessages is an internal method that delivers messages to all active
// peers except the one specified by `skip`.
//
// NOTE: This method MUST be called while the server's mutex is locked.
func (s *server) broadcastMessages(
	skip *btcec.PublicKey,
	msgs []lnwire.Message) error {

	srvrLog.Debugf("Broadcasting %v messages", len(msgs))

	// Iterate over all known peers, dispatching a go routine to enqueue
	// all messages to each of peers.  We synchronize access to peersByPub
	// throughout this process to ensure we deliver messages to exact set
	// of peers present at the time of invocation.
	var wg sync.WaitGroup
	for pubStr, sPeer := range s.peersByPub {
		if skip != nil && sPeer.addr.IdentityKey.IsEqual(skip) {
			srvrLog.Debugf("Skipping %v in broadcast", pubStr)
			continue
		}

		// Dispatch a go routine to enqueue all messages to this peer.
		wg.Add(1)
		s.wg.Add(1)
		go s.sendPeerMessages(sPeer, msgs, &wg)
	}

	// Wait for all messages to have been dispatched before returning to
	// caller.
	wg.Wait()

	return nil
}

// SendToPeer send a message to the server telling it to send the specific set
// of message to a particular peer. If the peer connect be found, then this
// method will return a non-nil error.
//
// NOTE: This function is safe for concurrent access.
func (s *server) SendToPeer(target *btcec.PublicKey,
	msgs ...lnwire.Message) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.sendToPeer(target, msgs)
}

// NotifyWhenOnline can be called by other subsystems to get notified when a
// particular peer comes online.
//
// NOTE: This function is safe for concurrent access.
func (s *server) NotifyWhenOnline(peer *btcec.PublicKey,
	connectedChan chan<- struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Compute the target peer's identifier.
	pubStr := string(peer.SerializeCompressed())

	// Check if peer is connected.
	_, ok := s.peersByPub[pubStr]
	if ok {
		// Connected, can return early.
		srvrLog.Debugf("Notifying that peer %v is online", pubStr)
		close(connectedChan)
		return
	}

	// Not connected, store this listener such that it can be notified when
	// the peer comes online.
	s.peerConnectedListeners[pubStr] = append(
		s.peerConnectedListeners[pubStr], connectedChan)
}

// sendToPeer is an internal method that delivers messages to the specified
// `target` peer.
func (s *server) sendToPeer(target *btcec.PublicKey,
	msgs []lnwire.Message) error {

	// Compute the target peer's identifier.
	targetPubBytes := target.SerializeCompressed()

	srvrLog.Infof("Attempting to send msgs %v to: %x",
		len(msgs), targetPubBytes)

	// Lookup intended target in peersByPub, returning an error to the
	// caller if the peer is unknown.  Access to peersByPub is synchronized
	// here to ensure we consider the exact set of peers present at the
	// time of invocation.
	targetPeer, err := s.findPeerByPubStr(string(targetPubBytes))
	if err == ErrPeerNotFound {
		srvrLog.Errorf("unable to send message to %x, "+
			"peer not found", targetPubBytes)
		return err
	}

	s.sendPeerMessages(targetPeer, msgs, nil)

	return nil
}

// sendPeerMessages enqueues a list of messages into the outgoingQueue of the
// `targetPeer`.  This method supports additional broadcast-level
// synchronization by using the additional `wg` to coordinate a particular
// broadcast.
//
// NOTE: This method must be invoked with a non-nil `wg` if it is spawned as a
// go routine--both `wg` and the server's WaitGroup should be incremented
// beforehand.  If this method is not spawned as a go routine, the provided
// `wg` should be nil, and the server's WaitGroup should not be tracking this
// invocation.
func (s *server) sendPeerMessages(
	targetPeer *peer,
	msgs []lnwire.Message,
	wg *sync.WaitGroup) {

	// If a WaitGroup is provided, we assume that this method was spawned
	// as a go routine, and that it is being tracked by both the server's
	// WaitGroup, as well as the broadcast-level WaitGroup `wg`. In this
	// event, we defer a call to Done on both WaitGroups to 1) ensure that
	// server will be able to shutdown after its go routines exit, and 2)
	// so the server can return to the caller of BroadcastMessage.
	if wg != nil {
		defer s.wg.Done()
		defer wg.Done()
	}

	for _, msg := range msgs {
		targetPeer.queueMsg(msg, nil)
	}
}

// FindPeer will return the peer that corresponds to the passed in public key.
// This function is used by the funding manager, allowing it to update the
// daemon's local representation of the remote peer.
//
// NOTE: This function is safe for concurrent access.
func (s *server) FindPeer(peerKey *btcec.PublicKey) (*peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pubStr := string(peerKey.SerializeCompressed())

	return s.findPeerByPubStr(pubStr)
}

// FindPeerByPubStr will return the peer that corresponds to the passed peerID,
// which should be a string representation of the peer's serialized, compressed
// public key.
//
// NOTE: This function is safe for concurrent access.
func (s *server) FindPeerByPubStr(pubStr string) (*peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.findPeerByPubStr(pubStr)
}

// findPeerByPubStr is an internal method that retrieves the specified peer from
// the server's internal state using.
func (s *server) findPeerByPubStr(pubStr string) (*peer, error) {
	peer, ok := s.peersByPub[pubStr]
	if !ok {
		return nil, ErrPeerNotFound
	}

	return peer, nil
}

// peerTerminationWatcher waits until a peer has been disconnected unexpectedly,
// and then cleans up all resources allocated to the peer, notifies relevant
// sub-systems of its demise, and finally handles re-connecting to the peer if
// it's persistent. If the server intentionally disconnects a peer, it should
// have a corresponding entry in the ignorePeerTermination map which will cause
// the cleanup routine to exit early.
//
// NOTE: This MUST be launched as a goroutine.
func (s *server) peerTerminationWatcher(p *peer) {
	defer s.wg.Done()

	p.WaitForDisconnect()

	srvrLog.Debugf("Peer %v has been disconnected", p)

	// If the server is exiting then we can bail out early ourselves as all
	// the other sub-systems will already be shutting down.
	if s.Stopped() {
		return
	}

	// Next, we'll cancel all pending funding reservations with this node.
	// If we tried to initiate any funding flows that haven't yet finished,
	// then we need to unlock those committed outputs so they're still
	// available for use.
	s.fundingMgr.CancelPeerReservations(p.PubKey())

	// Tell the switch to remove all links associated with this peer.
	// Passing nil as the target link indicates that all links associated
	// with this interface should be closed.
	//
	// TODO(roasbeef): instead add a PurgeInterfaceLinks function?
	links, err := p.server.htlcSwitch.GetLinksByInterface(p.pubKeyBytes)
	if err != nil {
		srvrLog.Errorf("unable to get channel links: %v", err)
	}

	for _, link := range links {
		err := p.server.htlcSwitch.RemoveLink(link.ChanID())
		if err != nil {
			srvrLog.Errorf("unable to remove channel link: %v",
				err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// If the server has already removed this peer, we can short circuit the
	// peer termination watcher and skip cleanup.
	if _, ok := s.ignorePeerTermination[p]; ok {
		delete(s.ignorePeerTermination, p)
		return
	}

	// First, cleanup any remaining state the server has regarding the peer
	// in question.
	s.removePeer(p)

	// Next, check to see if this is a persistent peer or not.
	pubStr := string(p.addr.IdentityKey.SerializeCompressed())
	_, ok := s.persistentPeers[pubStr]
	if ok {
		srvrLog.Debugf("Attempting to re-establish persistent "+
			"connection to peer %v", p)

		// If so, then we'll attempt to re-establish a persistent
		// connection to the peer.
		// TODO(roasbeef): look up latest info for peer in database
		connReq := &connmgr.ConnReq{
			Addr:      p.addr,
			Permanent: true,
		}

		// We'll only need to re-launch a connection requests if one
		// isn't already currently pending.
		if _, ok := s.persistentConnReqs[pubStr]; ok {
			return
		}

		// Otherwise, we'll launch a new connection requests in order
		// to attempt to maintain a persistent connection with this
		// peer.
		s.persistentConnReqs[pubStr] = append(
			s.persistentConnReqs[pubStr], connReq)

		go s.connMgr.Connect(connReq)
	}
}

// shouldRequestGraphSync returns true if the servers deems it necessary that
// we sync channel graph state with the remote peer. This method is used to
// avoid _always_ syncing channel graph state with each peer that connects.
//
// NOTE: This MUST be called with the server's mutex held.
func (s *server) shouldRequestGraphSync() bool {
	// Initially, we'll only request a graph sync iff we have less than two
	// peers.
	return len(s.peersByPub) <= 2
}

// peerConnected is a function that handles initialization a newly connected
// peer by adding it to the server's global list of all active peers, and
// starting all the goroutines the peer needs to function properly.
func (s *server) peerConnected(conn net.Conn, connReq *connmgr.ConnReq,
	inbound bool) {

	brontideConn := conn.(*brontide.Conn)
	peerAddr := &lnwire.NetAddress{
		IdentityKey: brontideConn.RemotePub(),
		Address:     conn.RemoteAddr().(*net.TCPAddr),
		ChainNet:    activeNetParams.Net,
	}

	// With the brontide connection established, we'll now craft the local
	// feature vector to advertise to the remote node.
	localFeatures := lnwire.NewRawFeatureVector()

	// We'll only request a full channel graph sync if we detect that that
	// we aren't fully synced yet.
	if s.shouldRequestGraphSync() {
		localFeatures.Set(lnwire.InitialRoutingSync)
	}

	// Now that we've established a connection, create a peer, and it to
	// the set of currently active peers.
	p, err := newPeer(conn, connReq, s, peerAddr, inbound, localFeatures)
	if err != nil {
		srvrLog.Errorf("unable to create peer %v", err)
		return
	}

	// TODO(roasbeef): update IP address for link-node
	//  * also mark last-seen, do it one single transaction?

	// Attempt to start the peer, if we're unable to do so, then disconnect
	// this peer.
	if err := p.Start(); err != nil {
		p.Disconnect(errors.Errorf("unable to start peer: %v", err))
		return
	}

	s.addPeer(p)
}

// shouldDropConnection determines if our local connection to a remote peer
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
	pubStr := string(nodePub.SerializeCompressed())

	s.mu.Lock()
	defer s.mu.Unlock()

	// If we already have an inbound connection to this peer, then ignore
	// this new connection.
	if _, ok := s.inboundPeers[pubStr]; ok {
		srvrLog.Debugf("Ignoring duplicate inbound connection")
		conn.Close()
		return
	}

	srvrLog.Infof("New inbound connection from %v", conn.RemoteAddr())

	localPub := s.identityPriv.PubKey()

	// Check to see if we already have a connection with this peer. If so,
	// we may need to drop our existing connection. This prevents us from
	// having duplicate connections to the same peer. We forgo adding a
	// default case as we expect these to be the only error values returned
	// from findPeerByPubStr.
	connectedPeer, err := s.findPeerByPubStr(pubStr)
	switch err {
	case ErrPeerNotFound:
		// We were unable to locate an existing connection with the
		// target peer, proceed to connect.

	case nil:
		// We already have a connection with the incoming peer. If the
		// connection we've already established should be kept, then
		// we'll close out this connection s.t there's only a single
		// connection between us.
		if !shouldDropLocalConnection(localPub, nodePub) {
			srvrLog.Warnf("Received inbound connection from "+
				"peer %x, but already connected, dropping conn",
				nodePub.SerializeCompressed())
			conn.Close()
			return
		}

		// Otherwise, if we should drop the connection, then we'll
		// disconnect our already connected peer.
		srvrLog.Debugf("Disconnecting stale connection to %v",
			connectedPeer)

		// Remove the current peer from the server's internal state and
		// signal that the peer termination watcher does not need to
		// execute for this peer.
		s.removePeer(connectedPeer)
		s.ignorePeerTermination[connectedPeer] = struct{}{}
	}

	// Next, check to see if we have any outstanding persistent connection
	// requests to this peer. If so, then we'll remove all of these
	// connection requests, and also delete the entry from the map.
	if connReqs, ok := s.persistentConnReqs[pubStr]; ok {
		for _, connReq := range connReqs {
			s.connMgr.Remove(connReq.ID())
		}
		delete(s.persistentConnReqs, pubStr)
	}

	s.peerConnected(conn, nil, false)
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

	localPub := s.identityPriv.PubKey()
	nodePub := conn.(*brontide.Conn).RemotePub()
	pubStr := string(nodePub.SerializeCompressed())

	s.mu.Lock()
	defer s.mu.Unlock()

	// If we already have an outbound connection to this peer, then ignore
	// this new connection.
	if _, ok := s.outboundPeers[pubStr]; ok {
		srvrLog.Debugf("Ignoring duplicate outbound connection")
		conn.Close()
		return
	}
	if _, ok := s.persistentConnReqs[pubStr]; !ok && connReq != nil {
		srvrLog.Debugf("Ignoring cancelled outbound connection")
		conn.Close()
		return
	}

	srvrLog.Infof("Established connection to: %v", conn.RemoteAddr())

	// As we've just established an outbound connection to this peer, we'll
	// cancel all other persistent connection requests and eliminate the
	// entry for this peer from the map.
	if connReqs, ok := s.persistentConnReqs[pubStr]; ok {
		for _, pConnReq := range connReqs {
			if connReq != nil &&
				pConnReq.ID() != connReq.ID() {

				s.connMgr.Remove(pConnReq.ID())
			}
		}
		delete(s.persistentConnReqs, pubStr)
	}

	// If we already have a connection with this peer, decide whether or not
	// we need to drop the stale connection. We forgo adding a default case
	// as we expect these to be the only error values returned from
	// findPeerByPubStr.
	connectedPeer, err := s.findPeerByPubStr(pubStr)
	switch err {
	case ErrPeerNotFound:
		// We were unable to locate an existing connection with the
		// target peer, proceed to connect.

	case nil:
		// We already have a connection open with the target peer.
		// If our (this) connection should be dropped, then we'll do
		// so, in order to ensure we don't have any duplicate
		// connections.
		if shouldDropLocalConnection(localPub, nodePub) {
			srvrLog.Warnf("Established outbound connection to "+
				"peer %x, but already connected, dropping conn",
				nodePub.SerializeCompressed())
			if connReq != nil {
				s.connMgr.Remove(connReq.ID())
			}
			conn.Close()
			return
		}

		// Otherwise, _their_ connection should be dropped. So we'll
		// disconnect the peer and send the now obsolete peer to the
		// server for garbage collection.
		srvrLog.Debugf("Disconnecting stale connection to %v",
			connectedPeer)

		// Remove the current peer from the server's internal state and
		// signal that the peer termination watcher does not need to
		// execute for this peer.
		s.removePeer(connectedPeer)
		s.ignorePeerTermination[connectedPeer] = struct{}{}
	}

	s.peerConnected(conn, connReq, true)
}

// addPeer adds the passed peer to the server's global state of all active
// peers.
func (s *server) addPeer(p *peer) {
	if p == nil {
		return
	}

	// Ignore new peers if we're shutting down.
	if s.Stopped() {
		p.Disconnect(ErrServerShuttingDown)
		return
	}

	// Track the new peer in our indexes so we can quickly look it up either
	// according to its public key, or it's peer ID.
	// TODO(roasbeef): pipe all requests through to the
	// queryHandler/peerManager

	pubStr := string(p.addr.IdentityKey.SerializeCompressed())

	s.peersByID[p.id] = p
	s.peersByPub[pubStr] = p

	if p.inbound {
		s.inboundPeers[pubStr] = p
	} else {
		s.outboundPeers[pubStr] = p
	}

	// Launch a goroutine to watch for the unexpected termination of this
	// peer, which will ensure all resources are properly cleaned up, and
	// re-establish persistent connections when necessary. The peer
	// termination watcher will be short circuited if the peer is ever
	// added to the ignorePeerTermination map, indicating that the server
	// has already handled the removal of this peer.
	s.wg.Add(1)
	go s.peerTerminationWatcher(p)

	// If the remote peer has the initial sync feature bit set, then we'll
	// being the synchronization protocol to exchange authenticated channel
	// graph edges/vertexes
	if p.remoteLocalFeatures.HasFeature(lnwire.InitialRoutingSync) {
		go s.authGossiper.SynchronizeNode(p.addr.IdentityKey)
	}

	// Check if there are listeners waiting for this peer to come online.
	for _, con := range s.peerConnectedListeners[pubStr] {
		close(con)
	}
	delete(s.peerConnectedListeners, pubStr)
}

// removePeer removes the passed peer from the server's state of all active
// peers.
func (s *server) removePeer(p *peer) {
	if p == nil {
		return
	}

	srvrLog.Debugf("removing peer %v", p)

	// As the peer is now finished, ensure that the TCP connection is
	// closed and all of its related goroutines have exited.
	p.Disconnect(fmt.Errorf("server: disconnecting peer %v", p))

	// If this peer had an active persistent connection request, remove it.
	if p.connReq != nil {
		s.connMgr.Remove(p.connReq.ID())
	}

	// Ignore deleting peers if we're shutting down.
	if s.Stopped() {
		return
	}

	pubStr := string(p.addr.IdentityKey.SerializeCompressed())

	delete(s.peersByID, p.id)
	delete(s.peersByPub, pubStr)

	if p.inbound {
		delete(s.inboundPeers, pubStr)
	} else {
		delete(s.outboundPeers, pubStr)
	}
}

// openChanReq is a message sent to the server in order to request the
// initiation of a channel funding workflow to the peer with either the
// specified relative peer ID, or a global lightning  ID.
type openChanReq struct {
	targetPeerID int32
	targetPubkey *btcec.PublicKey

	chainHash chainhash.Hash

	localFundingAmt  btcutil.Amount
	remoteFundingAmt btcutil.Amount

	pushAmt lnwire.MilliSatoshi

	fundingFeePerWeight btcutil.Amount

	// TODO(roasbeef): add ability to specify channel constraints as well

	updates chan *lnrpc.OpenStatusUpdate
	err     chan error
}

// ConnectToPeer requests that the server connect to a Lightning Network peer
// at the specified address. This function will *block* until either a
// connection is established, or the initial handshake process fails.
//
// NOTE: This function is safe for concurrent access.
func (s *server) ConnectToPeer(addr *lnwire.NetAddress, perm bool) error {

	targetPub := string(addr.IdentityKey.SerializeCompressed())

	// Acquire mutex, but use explicit unlocking instead of defer for
	// better granularity.  In certain conditions, this method requires
	// making an outbound connection to a remote peer, which requires the
	// lock to be released, and subsequently reacquired.
	s.mu.Lock()

	// Ensure we're not already connected to this peer.
	peer, err := s.findPeerByPubStr(targetPub)
	if err == nil {
		s.mu.Unlock()
		return fmt.Errorf("already connected to peer: %v", peer)
	}

	// Peer was not found, continue to pursue connection with peer.

	// If there's already a pending connection request for this pubkey,
	// then we ignore this request to ensure we don't create a redundant
	// connection.
	if _, ok := s.persistentConnReqs[targetPub]; ok {
		s.mu.Unlock()
		return fmt.Errorf("connection attempt to %v is pending", addr)
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

		s.persistentPeers[targetPub] = struct{}{}
		s.persistentConnReqs[targetPub] = append(
			s.persistentConnReqs[targetPub], connReq)
		s.mu.Unlock()

		go s.connMgr.Connect(connReq)

		return nil
	}
	s.mu.Unlock()

	// If we're not making a persistent connection, then we'll attempt to
	// connect to the target peer. If the we can't make the connection, or
	// the crypto negotiation breaks down, then return an error to the
	// caller.
	conn, err := brontide.Dial(s.identityPriv, addr)
	if err != nil {
		return err
	}

	// Once the connection has been made, we can notify the server of the
	// new connection via our public endpoint, which will require the lock
	// an add the peer to the server's internal state.
	s.OutboundPeerConnected(nil, conn)

	return nil
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
	if err == ErrPeerNotFound {
		return fmt.Errorf("unable to find peer %x", pubBytes)
	}

	srvrLog.Infof("Disconnecting from %v", peer)

	// If this peer was formerly a persistent connection, then we'll remove
	// them from this map so we don't attempt to re-connect after we
	// disconnect.
	delete(s.persistentPeers, pubStr)

	// Remove the current peer from the server's internal state and signal
	// that the peer termination watcher does not need to execute for this
	// peer.
	s.removePeer(peer)
	s.ignorePeerTermination[peer] = struct{}{}

	return nil
}

// OpenChannel sends a request to the server to open a channel to the specified
// peer identified by ID with the passed channel funding parameters.
//
// NOTE: This function is safe for concurrent access.
func (s *server) OpenChannel(peerID int32, nodeKey *btcec.PublicKey,
	localAmt btcutil.Amount, pushAmt lnwire.MilliSatoshi,
	fundingFeePerByte btcutil.Amount) (chan *lnrpc.OpenStatusUpdate, chan error) {

	updateChan := make(chan *lnrpc.OpenStatusUpdate, 1)
	errChan := make(chan error, 1)

	var (
		targetPeer  *peer
		pubKeyBytes []byte
	)

	// If the user is targeting the peer by public key, then we'll need to
	// convert that into a string for our map. Otherwise, we expect them to
	// target by peer ID instead.
	if nodeKey != nil {
		pubKeyBytes = nodeKey.SerializeCompressed()
	}

	// First attempt to locate the target peer to open a channel with, if
	// we're unable to locate the peer then this request will fail.
	s.mu.Lock()
	if peer, ok := s.peersByID[peerID]; ok {
		targetPeer = peer
	} else if peer, ok := s.peersByPub[string(pubKeyBytes)]; ok {
		targetPeer = peer
	}
	s.mu.Unlock()

	if targetPeer == nil {
		errChan <- fmt.Errorf("unable to find peer nodeID(%x), "+
			"peerID(%v)", pubKeyBytes, peerID)
		return updateChan, errChan
	}

	// We'll scale the sat/byte set as the fee  rate to sat/weight as this
	// is what's used internally when deciding upon coin selection.
	fundingFeePerWeight := fundingFeePerByte / blockchain.WitnessScaleFactor

	// Spawn a goroutine to send the funding workflow request to the
	// funding manager. This allows the server to continue handling queries
	// instead of blocking on this request which is exported as a
	// synchronous request to the outside world.
	req := &openChanReq{
		targetPeerID:        peerID,
		targetPubkey:        nodeKey,
		chainHash:           *activeNetParams.GenesisHash,
		localFundingAmt:     localAmt,
		fundingFeePerWeight: fundingFeePerWeight,
		pushAmt:             pushAmt,
		updates:             updateChan,
		err:                 errChan,
	}

	// TODO(roasbeef): pass in chan that's closed if/when funding succeeds
	// so can track as persistent peer?
	go s.fundingMgr.initFundingWorkflow(targetPeer.addr, req)

	return updateChan, errChan
}

// Peers returns a slice of all active peers.
//
// NOTE: This function is safe for concurrent access.
func (s *server) Peers() []*peer {
	s.mu.Lock()
	defer s.mu.Unlock()

	peers := make([]*peer, 0, len(s.peersByID))
	for _, peer := range s.peersByID {
		peers = append(peers, peer)
	}

	return peers
}

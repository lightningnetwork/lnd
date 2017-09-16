package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/connmgr"
	"github.com/roasbeef/btcutil"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/htlcswitch"
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

	persistentPeers    map[string]struct{}
	persistentConnReqs map[string][]*connmgr.ConnReq

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

	// localFeatures is an feature vector which represent the features
	// which only affect the protocol between these two nodes.
	localFeatures *lnwire.FeatureVector

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

	serializedPubKey := privKey.PubKey().SerializeCompressed()
	s := &server{
		chanDB: chanDB,
		cc:     cc,

		invoices: newInvoiceRegistry(chanDB),

		utxoNursery: newUtxoNursery(chanDB, cc.chainNotifier, cc.wallet),

		identityPriv: privKey,
		nodeSigner:   newNodeSigner(privKey),

		// TODO(roasbeef): derive proper onion key based on rotation
		// schedule
		sphinx: htlcswitch.NewOnionProcessor(
			sphinx.NewRouter(privKey, activeNetParams.Params)),
		lightningID: sha256.Sum256(serializedPubKey),

		persistentPeers:    make(map[string]struct{}),
		persistentConnReqs: make(map[string][]*connmgr.ConnReq),

		peersByID:     make(map[int32]*peer),
		peersByPub:    make(map[string]*peer),
		inboundPeers:  make(map[string]*peer),
		outboundPeers: make(map[string]*peer),

		globalFeatures: globalFeatures,
		localFeatures:  localFeatures,

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
		UpdateTopology: func(msg *lnwire.ChannelUpdate) error {
			s.authGossiper.ProcessRemoteAnnouncement(msg, nil)
			return nil
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

	// TODO(roasbeef): make alias configurable
	alias, err := lnwire.NewNodeAlias(hex.EncodeToString(serializedPubKey[:10]))
	if err != nil {
		return nil, err
	}

	timeStamp := time.Now()
	nodeAnn := &lnwire.NodeAnnouncement{
		Timestamp: uint32(timeStamp.Unix()),
		Addresses: selfAddrs,
		NodeID:    privKey.PubKey(),
		Alias:     alias,
		Features:  globalFeatures,
	}

	updateNodeAnn := true

	chanGraph := chanDB.ChannelGraph()

	// Try to retrieve the old source node from disk. During startup it
	// is possible for there to be no source node, and this should not be
	// treated as an error.
	oldNode, err := chanGraph.SourceNode()
	if err != nil && err != channeldb.ErrSourceNodeNotSet {
		return nil, fmt.Errorf("unable to read old source node from disk")
	}

	if oldNode != nil {
		oldAlias, err := lnwire.NewNodeAlias(oldNode.Alias)
		if err != nil {
			return nil, err
		}

		oldNodeAnn := &lnwire.NodeAnnouncement{
			Timestamp: uint32(oldNode.LastUpdate.Unix()),
			Addresses: oldNode.Addresses,
			NodeID:    oldNode.PubKey,
			Alias:     oldAlias,
			Features:  oldNode.Features,
		}

		// If the nodes are not equal than there have been config changes
		// and we should propagate the updated node.
		updateNodeAnn = !nodeAnn.CompareNodes(oldNodeAnn)
	}

	// If our information has changed since our last boot, then we'll
	// re-sign our node announcement so a fresh authenticated version of it
	// can be propagated throughout the network upon startup.
	if updateNodeAnn {
		selfNode := &channeldb.LightningNode{
			HaveNodeAnnouncement: true,
			LastUpdate:           timeStamp,
			Addresses:            selfAddrs,
			PubKey:               privKey.PubKey(),
			Alias:                alias.String(),
			Features:             globalFeatures,
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
	}

	s.currentNodeAnn = nodeAnn

	s.chanRouter, err = routing.New(routing.Config{
		Graph:     chanGraph,
		Chain:     cc.chainIO,
		ChainView: cc.chainView,
		SendToSwitch: func(firstHop *btcec.PublicKey,
			htlcAdd *lnwire.UpdateAddHTLC,
			circuit *sphinx.Circuit) ([32]byte, error) {

			// Using the created circuit, initialize the error
			// decryptor so we can parse+decode any failures
			// incurred by this payment within the switch.
			errorDecryptor := &htlcswitch.FailureDeobfuscator{
				OnionDeobfuscator: sphinx.NewOnionDeobfuscator(circuit),
			}

			var firstHopPub [33]byte
			copy(firstHopPub[:], firstHop.SerializeCompressed())

			return s.htlcSwitch.SendHTLC(firstHopPub, htlcAdd, errorDecryptor)
		},
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
		TrickleDelay:     time.Millisecond * 300,
		DB:               chanDB,
		AnnSigner:        s.nodeSigner,
	},
		s.identityPriv.PubKey(),
	)
	if err != nil {
		return nil, err
	}

	s.breachArbiter = newBreachArbiter(cc.wallet, chanDB, cc.chainNotifier,
		s.htlcSwitch, s.cc.chainIO, s.cc.feeEstimator)

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
	if !cfg.NoNetBootstrap && !(cfg.Bitcoin.SimNet || cfg.Litecoin.SimNet) {
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

	// Disconnect from each active peers to ensure that
	// peerTerminationWatchers signal completion to each peer.
	peers := s.Peers()
	for _, peer := range peers {
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
		chainHash := reverseChainMap[registeredChains.PrimaryChain()]
		dnsSeeds, ok := chainDNSSeeds[chainHash]

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

	srvrLog.Debug("Attempting to bootstrap connectivity with %v initial "+
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
			srvrLog.Infof("e=%v, a=%v",
				atomic.LoadUint32(&epochErrors), epochAttempts)

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

			// Since we know need more peers, we'll compute the
			// exact number we need to reach our threshold.
			numNeeded := numTargetPeers - numActivePeers

			srvrLog.Debug("Attempting to obtain %v more network "+
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
	for _, sPeer := range s.peersByPub {
		if skip != nil &&
			sPeer.addr.IdentityKey.IsEqual(skip) {

			srvrLog.Debugf("Skipping %v in broadcast",
				skip.SerializeCompressed())

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
	targetPeer, ok := s.peersByPub[string(targetPubBytes)]
	if !ok {
		srvrLog.Errorf("unable to send message to %x, "+
			"peer not found", targetPubBytes)

		return errors.New("peer not found")
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

	serializedIDKey := string(peerKey.SerializeCompressed())

	return s.findPeer(serializedIDKey)
}

// FindPeerByPubStr will return the peer that corresponds to the passed peerID,
// which should be a string representation of the peer's serialized, compressed
// public key.
//
// NOTE: This function is safe for concurrent access.
func (s *server) FindPeerByPubStr(peerID string) (*peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.findPeer(peerID)
}

// findPeer is an internal method that retrieves the specified peer from the
// server's internal state.
func (s *server) findPeer(peerID string) (*peer, error) {
	peer := s.peersByPub[peerID]
	if peer == nil {
		return nil, errors.New("Peer not found. Pubkey: " + peerID)
	}

	return peer, nil
}

// peerTerminationWatcher waits until a peer has been disconnected, and then
// cleans up all resources allocated to the peer, notifies relevant sub-systems
// of its demise, and finally handles re-connecting to the peer if it's
// persistent.
//
// NOTE: This MUST be launched as a goroutine AND the _peer's_ WaitGroup should
// be incremented before spawning this method, as it will signal to the peer's
// WaitGroup upon completion.
func (s *server) peerTerminationWatcher(p *peer) {
	defer p.wg.Done()

	p.WaitForDisconnect()

	srvrLog.Debugf("Peer %v has been disconnected", p)

	// If the server is exiting then we can bail out early ourselves as all
	// the other sub-systems will already be shutting down.
	if s.Stopped() {
		return
	}

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

	// Send the peer to be garbage collected by the server.
	s.removePeer(p)

	// If this peer had an active persistent connection request, then we
	// can remove this as we manually decide below if we should attempt to
	// re-connect.
	if p.connReq != nil {
		s.connMgr.Remove(p.connReq.ID())
	}

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

	// Now that we've established a connection, create a peer, and
	// it to the set of currently active peers.
	p, err := newPeer(conn, connReq, s, peerAddr, inbound)
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

	// Check to see if we should drop our connection, if not, then we'll
	// close out this connection with the remote peer. This
	// prevents us from having duplicate connections, or none.
	if connectedPeer, ok := s.peersByPub[pubStr]; ok {
		// If the connection we've already established should be kept,
		// then we'll close out this connection s.t there's only a
		// single connection between us.
		if !shouldDropLocalConnection(localPub, nodePub) {
			srvrLog.Warnf("Received inbound connection from "+
				"peer %x, but already connected, dropping conn",
				nodePub.SerializeCompressed())
			conn.Close()
			return
		}

		// Otherwise, if we should drop the connection, then we'll
		// disconnect our already connected peer, and also send the
		// peer to the peer garbage collection goroutine.
		srvrLog.Debugf("Disconnecting stale connection to %v",
			connectedPeer)
		connectedPeer.Disconnect(errors.New("remove stale connection"))

		s.removePeer(connectedPeer)
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

	// If we already have an inbound connection from this peer, then we'll
	// check to see _which_ of our connections should be dropped.
	if connectedPeer, ok := s.peersByPub[pubStr]; ok {
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
		connectedPeer.Disconnect(errors.New("remove stale connection"))

		s.removePeer(connectedPeer)
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
	if atomic.LoadInt32(&s.shutdown) != 0 {
		p.Disconnect(errors.New("server is shutting down"))
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

	// Launch a goroutine to watch for the termination of this peer so we
	// can ensure all resources are properly cleaned up and if need be
	// connections are re-established.  The go routine is tracked by the
	// _peer's_ WaitGroup so that a call to Disconnect will block until the
	// `peerTerminationWatcher` has exited.
	p.wg.Add(1)
	go s.peerTerminationWatcher(p)

	// Once the peer has been added to our indexes, send a message to the
	// channel router so we can synchronize our view of the channel graph
	// with this new peer.
	go s.authGossiper.SynchronizeNode(p.addr.IdentityKey)
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
	p.Disconnect(errors.New("remove peer"))

	// Ignore deleting peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
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
	peer, ok := s.peersByPub[targetPub]
	if ok {
		s.mu.Unlock()

		return fmt.Errorf("already connected to peer: %v", peer)
	}

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
	peer, ok := s.peersByPub[pubStr]
	if !ok {
		return fmt.Errorf("unable to find peer %x", pubBytes)
	}

	// If this peer was formerly a persistent connection, then we'll remove
	// them from this map so we don't attempt to re-connect after we
	// disconnect.
	if _, ok := s.persistentPeers[pubStr]; ok {
		delete(s.persistentPeers, pubStr)
	}

	// Now that we know the peer is actually connected, we'll disconnect
	// from the peer.  The lock is held until after Disconnect to ensure
	// that the peer's `peerTerminationWatcher` has fully exited before
	// returning to the caller.
	srvrLog.Infof("Disconnecting from %v", peer)
	peer.Disconnect(
		errors.New("received user command to disconnect the peer"),
	)

	return nil
}

// OpenChannel sends a request to the server to open a channel to the specified
// peer identified by ID with the passed channel funding parameters.
//
// NOTE: This function is safe for concurrent access.
func (s *server) OpenChannel(peerID int32, nodeKey *btcec.PublicKey,
	localAmt btcutil.Amount,
	pushAmt lnwire.MilliSatoshi) (chan *lnrpc.OpenStatusUpdate, chan error) {

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

	// Spawn a goroutine to send the funding workflow request to the
	// funding manager. This allows the server to continue handling queries
	// instead of blocking on this request which is exported as a
	// synchronous request to the outside world.
	req := &openChanReq{
		targetPeerID:    peerID,
		targetPubkey:    nodeKey,
		chainHash:       *activeNetParams.GenesisHash,
		localFundingAmt: localAmt,
		pushAmt:         pushAmt,
		updates:         updateChan,
		err:             errChan,
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

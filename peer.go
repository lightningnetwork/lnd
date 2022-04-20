package lnd

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/connmgr"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/nat"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/peer"
	"github.com/lightningnetwork/lnd/peernotifier"
	"github.com/lightningnetwork/lnd/pool"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/lightningnetwork/lnd/tor"
)

type PeerPackageConfig struct {
	chanRouter *routing.ChannelRouter
	cfg        *Config

	listenAddrs []net.Addr
}

type PeerPackage struct {
	active int32 // atomic
	start  sync.Once
	stop   sync.Once

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

	chanRouter *routing.ChannelRouter

	quit chan struct{}

	wg sync.WaitGroup

	mu sync.RWMutex

	cfg *Config

	connMgr *connmgr.ConnManager

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

	// identityECDH is an ECDH capable wrapper for the private key used
	// to authenticate any incoming connections.
	identityECDH keychain.SingleKeyECDH

	// identityKeyLoc is the key locator for the above wrapped identity key.
	identityKeyLoc keychain.KeyLocator

	nodeKeyDesc *keychain.KeyDescriptor

	writePool *pool.Write

	readPool *pool.Read

	// featureMgr dispatches feature vectors for various contexts within the
	// daemon.
	featureMgr *feature.Manager

	fundingMgr *funding.Manager

	graphDB *channeldb.ChannelGraph

	htlcSwitch *htlcswitch.Switch

	chanStateDB *channeldb.ChannelStateDB

	authGossiper *discovery.AuthenticatedGossiper

	peerNotifier *peernotifier.PeerNotifier

	// nodeSigner is an implementation of the MessageSigner implementation
	// that's backed by the identity private key of the running lnd node.
	nodeSigner *netann.NodeSigner

	customMessageServer *subscribe.Server

	stopping int32 // atomic

	// currentNodeAnn is the node announcement that has been broadcast to
	// the network upon startup, if the attributes of the node (us) has
	// changed since last start.
	currentNodeAnn *lnwire.NodeAnnouncement

	hostAnn *netann.HostAnnouncer

	peerConfig *peer.Config
}

func newPeerPackage(cfg *Config) (*PeerPackage, error) {
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

	s := &PeerPackage{
		writePool: writePool,
		readPool:  readPool,
	}

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
		listenPorts := make([]uint16, 0, len(s.listenAddrs))
		for _, listenAddr := range s.listenAddrs {
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
	chanGraph := s.graphDB

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
		var serializedPubKey [33]byte
		copy(serializedPubKey[:], s.nodeKeyDesc.PubKey.SerializeCompressed())

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
	copy(selfNode.PubKeyBytes[:], s.nodeKeyDesc.PubKey.SerializeCompressed())

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
		s.nodeSigner, s.nodeKeyDesc.KeyLocator, nodeAnn,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to generate signature for "+
			"self node announcement: %v", err)
	}
	selfNode.AuthSigBytes = authSig.Serialize()
	nodeAnn.Signature, err = lnwire.NewSigFromRawSignature(
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
			AdvertisedIPs:  advertisedIPs,
			AnnounceNewIPs: netann.IPAnnouncer(s.genNodeAnnouncement),
		})
	}

	listeners := make([]net.Listener, len(s.listenAddrs))
	for i, listenAddr := range s.listenAddrs {
		// Note: though brontide.NewListener uses ResolveTCPAddr, it
		// doesn't need to call the general lndResolveTCP function
		// since we are resolving a local address.
		listeners[i], err = brontide.NewListener(
			s.identityECDH, listenAddr.String(),
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
			s.identityECDH, s.cfg.net, s.cfg.ConnectionTimeout,
		),
		OnConnection: s.OutboundPeerConnected,
	})
	if err != nil {
		return nil, err
	}
	s.connMgr = cmgr

	return s, nil
}

func (s *PeerPackage) MarkNonPersistent(pubStr string) {
	s.mu.Lock()
	if _, ok := s.persistentPeers[pubStr]; !ok {
		s.persistentPeers[pubStr] = false
	}
	s.mu.Unlock()
}

// Start starts the main daemon server, all requested listeners, and any helper
// goroutines.
// NOTE: This function is safe for concurrent access.
func (s *PeerPackage) Start() error {
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

		// Start connmgr last to prevent connections before init.
		s.connMgr.Start()
		cleanup = cleanup.add(func() error {
			s.connMgr.Stop()
			return nil
		})

		// Subscribe to NodeAnnouncements that advertise new addresses
		// our persistent peers.
		if err := s.updatePersistentPeerAddrs(); err != nil {
			startErr = err
			return
		}

		if err := s.establishPersistentConnections(); err != nil {
			startErr = err
			return
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
			go s.peerBootstrapper(defaultMinPeers, bootstrappers)
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
func (s *PeerPackage) Stop() error {
	s.stop.Do(func() {
		atomic.StoreInt32(&s.stopping, 1)

		close(s.quit)

		// Shutdown connMgr first to prevent conns during shutdown.
		s.connMgr.Stop()

		if s.hostAnn != nil {
			if err := s.hostAnn.Stop(); err != nil {
				srvrLog.Warnf("unable to shut down host "+
					"annoucner: %v", err)
			}
		}

		// Wait for all lingering goroutines to quit.
		s.wg.Wait()

		s.writePool.Stop()
		s.readPool.Stop()
	})

	return nil
}

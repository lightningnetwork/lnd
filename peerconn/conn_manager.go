package peerconn

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	prand "math/rand"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/peer"
	"github.com/lightningnetwork/lnd/peernotifier"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tor"
)

const (
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

	// ErrServerShuttingDown indicates that the server is in the process of
	// gracefully exiting.
	ErrServerShuttingDown = errors.New("server is shutting down")
)

// ErrPeerAlreadyConnected is an error returned by the server when we're
// commanded to connect to a peer, but they're already connected.
type ErrPeerAlreadyConnected struct {
	peer *peer.Brontide
}

// Error returns the human readable version of this error type.
//
// NOTE: Part of the error interface.
func (e *ErrPeerAlreadyConnected) Error() string {
	return fmt.Sprintf("already connected to peer: %v", e.peer)
}

// PeerConnManagerConfig holds config info for the peer conn manager.
type PeerConnManagerConfig struct {
	// PartialPeerConfig holds a peer config that's not yet completed and
	// will be finished by the peer conn manager when making a new peer.
	PartialPeerConfig peer.Config

	// featureMgr dispatches feature vectors for various contexts within
	// the daemon.
	FeatureMgr *feature.Manager

	// Net is the named network specified during the startup of lnd.
	Net tor.Net

	// TorActive specifies whether we are using tor.
	TorActive bool

	// StaggerInitialReconnect specify whether we will apply a randomized
	// staggering between 0s and 30s when reconnecting to persistent peers
	// on startup. The first 10 reconnections will be attempted instantly,
	// regardless of the flag's value
	StaggerInitialReconnect bool

	//The timeout value for network connections.
	ConnectionTimeout time.Duration

	// Listeners is a list of addresses that's specified during the startup
	// of lnd.
	Listeners []net.Listener

	// NetParams specifies the bitcoin network used.
	NetParams wire.BitcoinNet

	// MinBackoff defines the initial backoff applied to connections with
	// watchtowers. Subsequent backoff durations will grow exponentially up
	// until MaxBackoff.
	MinBackoff time.Duration

	// MaxBackoff defines the maximum backoff applied to connections with
	// watchtowers. If the exponential backoff produces a timeout greater
	// than this value, the backoff will be clamped to MaxBackoff.
	MaxBackoff time.Duration

	// SubscribeTopology is used to get a subscription for topology changes
	// on the network.
	SubscribeTopology func() (*routing.TopologyClient, error)

	// CancelPeerReservations mounts the method from funding manager which
	// cancels all active reservations associated with the passed node.
	CancelPeerReservations func(nodePub [33]byte)
}

// PeerConnManager is responsible for managing peer connections.
type PeerConnManager struct {
	Config *PeerConnManagerConfig

	// IdentityECDH is an ECDH capable wrapper for the private key used
	// to authenticate any incoming connections.
	IdentityECDH keychain.SingleKeyECDH

	mu sync.RWMutex

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

	// PeerNotifier is responsible for providing clients with subscriptions
	// to peer online and offline events.
	PeerNotifier *peernotifier.PeerNotifier

	connMgr *connmgr.ConnManager

	active   int32
	stopping int32
	start    sync.Once
	stop     sync.Once
	quit     chan struct{}

	wg sync.WaitGroup

	// PruneSyncState mounts the method from auth gossiper and is called
	// once a peer that we were previously connected to has been
	// disconnected. In this case we can stop the existing GossipSyncer
	// assigned to the peer and free up resources.
	PruneSyncState func(peer route.Vertex)

	// torController is a client that will communicate with a locally
	// running Tor server. This client will handle initiating and
	// authenticating the connection to the Tor server, automatically
	// creating and setting up onion services, etc.
	torController *tor.Controller
}

// Start will start the peer conn manager.
func (p *PeerConnManager) Start() error {
	// Create the connection manager which will be responsible for
	// maintaining persistent outbound connections and also accepting new
	// incoming connections
	cmgr, err := connmgr.New(&connmgr.Config{
		Listeners:      p.Config.Listeners,
		OnAccept:       p.InboundPeerConnected,
		RetryDuration:  time.Second * 5,
		TargetOutbound: 100,
		Dial: noiseDial(
			p.IdentityECDH, p.Config.Net, p.Config.ConnectionTimeout,
		),
		OnConnection: p.OutboundPeerConnected,
	})
	if err != nil {
		return fmt.Errorf("Creating conn manager failed: %w", err)
	}
	p.connMgr = cmgr

	p.start.Do(func() {
		connLog.Info("PeerConnManager starting...")

		p.connMgr.Start()

		if err := p.PeerNotifier.Start(); err != nil {
			err = fmt.Errorf("PeerNotifier failed to start %w", err)
		}

		atomic.StoreInt32(&p.active, 1)
	})

	return err
}

// Stop will stop the peer conn manager.
func (p *PeerConnManager) Stop() error {
	// Before we shutdown the manager, disconnect from each active peers to
	// ensure that peerTerminationWatchers signal completion to each peer.
	for _, peer := range p.Peers() {
		err := p.DisconnectPeer(peer.IdentityKey())
		if err != nil {
			connLog.Warnf("could not disconnect peer: %v"+
				"received error: %v", peer.IdentityKey(),
				err,
			)
		}
	}

	var err error
	p.stop.Do(func() {
		connLog.Info("PeerConnManager shutting down")

		// Shutdown the connMgr.
		p.connMgr.Stop()

		atomic.StoreInt32(&p.stopping, 1)
		close(p.quit)
		p.wg.Wait()

		if err := p.PeerNotifier.Stop(); err != nil {
			err = fmt.Errorf("PeerNotifier failed to stop: %w", err)
		}
	})

	return err
}

// Stopped returns true if the peer conn manager has been instructed to
// shutdown.
func (p *PeerConnManager) Stopped() bool {
	return atomic.LoadInt32(&p.stopping) != 0
}

// NewPeerConnManager creates and returns a new peer conn manager.
func NewPeerConnManager(nodeKey keychain.SingleKeyECDH,
	tc *tor.Controller) *PeerConnManager {

	return &PeerConnManager{
		IdentityECDH: nodeKey,

		// Assemble a peer notifier which will provide clients with
		// subscriptions to peer online and offline events.
		PeerNotifier: peernotifier.New(),

		persistentPeers:         make(map[string]bool),
		persistentPeersBackoff:  make(map[string]time.Duration),
		persistentConnReqs:      make(map[string][]*connmgr.ConnReq),
		persistentPeerAddrs:     make(map[string][]*lnwire.NetAddress),
		persistentRetryCancels:  make(map[string]chan struct{}),
		peerErrors:              make(map[string]*queue.CircularBuffer),
		ignorePeerTermination:   make(map[*peer.Brontide]struct{}),
		scheduledPeerConnection: make(map[string]func()),

		peersByPub:    make(map[string]*peer.Brontide),
		inboundPeers:  make(map[string]*peer.Brontide),
		outboundPeers: make(map[string]*peer.Brontide),
		peerConnectedListeners: make(
			map[string][]chan<- lnpeer.Peer,
		),
		peerDisconnectedListeners: make(map[string][]chan<- struct{}),

		torController: tc,
		quit:          make(chan struct{}),
	}
}

// UpdatePersistentPeerAddrs subscribes to topology changes and stores
// advertised addresses for any NodeAnnouncements from our persisted peers.
func (p *PeerConnManager) UpdatePersistentPeerAddrs() error {
	graphSub, err := p.Config.SubscribeTopology()
	if err != nil {
		return err
	}

	p.wg.Add(1)
	go func() {
		defer func() {
			graphSub.Cancel()
			p.wg.Done()
		}()

		for {
			select {
			case <-p.quit:
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
					p.mu.RLock()
					_, ok := p.persistentPeers[pubKeyStr]
					p.mu.RUnlock()
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
								ChainNet:    p.Config.NetParams,
							},
						)
					}

					p.mu.Lock()

					// Update the stored addresses for this
					// to peer to reflect the new set.
					p.persistentPeerAddrs[pubKeyStr] = addrs

					// If there are no outstanding
					// connection requests for this peer
					// then our work is done since we are
					// not currently trying to connect to
					// them.
					if len(p.persistentConnReqs[pubKeyStr]) == 0 {
						p.mu.Unlock()
						continue
					}

					p.mu.Unlock()

					p.connectToPersistentPeer(pubKeyStr)
				}
			}
		}
	}()

	return nil
}

type IgnoredPeers map[autopilot.NodeID]struct{}

// CreateBootstrapIgnorePeers creates a map of peers that the bootstrap process
// needs to ignore, which is made of three parts,
//   - the node itself needs to be skipped as it doesn't make sense to connect
//     to itself.
//   - the peers that already have connections with, as in s.peersByPub.
//   - the peers that we are attempting to connect, as in s.persistentPeers.
func (p *PeerConnManager) CreateBootstrapIgnorePeers() IgnoredPeers {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ignore := make(map[autopilot.NodeID]struct{})

	// We should ignore ourselves from bootstrapping.
	selfKey := autopilot.NewNodeID(p.IdentityECDH.PubKey())
	ignore[selfKey] = struct{}{}

	// Ignore all connected peers.
	for _, peer := range p.peersByPub {
		nID := autopilot.NewNodeID(peer.IdentityKey())
		ignore[nID] = struct{}{}
	}

	// Ignore all persistent peers as they have a dedicated reconnecting
	// process.
	for pubKeyStr := range p.persistentPeers {
		var nID autopilot.NodeID
		copy(nID[:], []byte(pubKeyStr))
		ignore[nID] = struct{}{}
	}

	return ignore
}

// PeerBootstrapper is a goroutine which is tasked with attempting to establish
// and maintain a target minimum number of outbound connections. With this
// invariant, we ensure that our node is connected to a diverse set of peers
// and that nodes newly joining the network receive an up to date network view
// as soon as possible.
func (p *PeerConnManager) PeerBootstrapper(numTargetPeers uint32,
	bootstrappers []discovery.NetworkPeerBootstrapper) {

	defer p.wg.Done()

	// Before we continue, init the ignore peers map.
	ignoreList := p.CreateBootstrapIgnorePeers()

	// We'll start off by aggressively attempting connections to peers in
	// order to be a part of the network as soon as possible.
	p.initialPeerBootstrap(ignoreList, numTargetPeers, bootstrappers)

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
			p.mu.RLock()
			numActivePeers := uint32(len(p.peersByPub))
			p.mu.RUnlock()

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

				connLog.Debugf("Backing off peer bootstrapper to "+
					"%v", backOff)
				sampleTicker = time.NewTicker(backOff)
				continue
			}

			atomic.StoreUint32(&epochErrors, 0)
			epochAttempts = 0

			// Since we know need more peers, we'll compute the
			// exact number we need to reach our threshold.
			numNeeded := numTargetPeers - numActivePeers

			connLog.Debugf("Attempting to obtain %v more network "+
				"peers", numNeeded)

			// With the number of peers we need calculated, we'll
			// query the network bootstrappers to sample a set of
			// random addrs for us.
			//
			// Before we continue, get a copy of the ignore peers
			// map.
			ignoreList = p.CreateBootstrapIgnorePeers()

			peerAddrs, err := discovery.MultiSourceBootstrap(
				ignoreList, numNeeded*2, bootstrappers...,
			)
			if err != nil {
				connLog.Errorf("Unable to retrieve bootstrap "+
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
					p.connectToPeer(
						a, errChan,
						p.Config.ConnectionTimeout,
					)
					select {
					case err := <-errChan:
						if err == nil {
							return
						}

						connLog.Errorf("Unable to "+
							"connect to %v: %v",
							a, err)
						atomic.AddUint32(&epochErrors, 1)
					case <-p.quit:
					}
				}(addr)
			}
		case <-p.quit:
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
func (p *PeerConnManager) initialPeerBootstrap(ignore map[autopilot.NodeID]struct{},
	numTargetPeers uint32,
	bootstrappers []discovery.NetworkPeerBootstrapper) {

	connLog.Debugf("Init bootstrap with targetPeers=%v, bootstrappers=%v, "+
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
		if p.Stopped() {
			return
		}

		// We can exit our aggressive initial peer bootstrapping stage
		// if we've reached out target number of peers.
		p.mu.RLock()
		numActivePeers := uint32(len(p.peersByPub))
		p.mu.RUnlock()

		if numActivePeers >= numTargetPeers {
			return
		}

		if attempts > 0 {
			connLog.Debugf("Waiting %v before trying to locate "+
				"bootstrap peers (attempt #%v)", delayTime,
				attempts)

			// We've completed at least one iterating and haven't
			// finished, so we'll start to insert a delay period
			// between each attempt.
			delaySignal = time.After(delayTime)
			select {
			case <-delaySignal:
			case <-p.quit:
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
			ignore, peersNeeded, bootstrappers...,
		)
		if err != nil {
			connLog.Errorf("Unable to retrieve initial bootstrap "+
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
				go p.connectToPeer(
					addr, errChan,
					p.Config.ConnectionTimeout,
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
					connLog.Errorf("Unable to connect to "+
						"%v: %v", addr, err)
				// TODO: tune timeout? 3 seconds might be *too*
				// aggressive but works well.
				case <-time.After(3 * time.Second):
					connLog.Tracef("Skipping peer %v due "+
						"to not establishing a "+
						"connection within 3 seconds",
						addr)
				case <-p.quit:
				}
			}(bootstrapAddr)
		}

		wg.Wait()
	}
}

type nodeAddresses struct {
	pubKey    *btcec.PublicKey
	addresses []net.Addr
}

// EstablishPersistentConnections attempts to establish persistent connections
// to all our direct channel collaborators. In order to promote liveness of our
// active channels, we instruct the connection manager to attempt to establish
// and maintain persistent connections to all our direct channel counterparties.
func (p *PeerConnManager) EstablishPersistentConnections() error {
	// nodeAddrsMap stores the combination of node public keys and addresses
	// that we'll attempt to reconnect to. PubKey strings are used as keys
	// since other PubKey forms can't be compared.
	nodeAddrsMap := map[string]*nodeAddresses{}

	// Iterate through the list of LinkNodes to find addresses we should
	// attempt to connect to based on our set of previous connections. Set
	// the reconnection port to the default peer port.
	linkNodes, err := p.Config.PartialPeerConfig.ChannelDB.LinkNodeDB().
		FetchAllLinkNodes()
	if err != nil && !errors.Is(err, channeldb.ErrLinkNodesNotFound) {
		return err
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
	// that have been added via NodeAnnouncement messages.
	sourceNode, err := p.Config.PartialPeerConfig.ChannelGraph.SourceNode()
	if err != nil {
		return err
	}

	// TODO(roasbeef): instead iterate over link nodes and query graph for
	// each of the nodes.
	selfPub := p.IdentityECDH.PubKey().SerializeCompressed()
	err = sourceNode.ForEachChannel(nil, func(
		tx kvdb.RTx,
		chanInfo *channeldb.ChannelEdgeInfo,
		policy, _ *channeldb.ChannelEdgePolicy) error {

		// If the remote party has announced the channel to us, but we
		// haven't yet, then we won't have a policy. However, we don't
		// need this to connect to the peer, so we'll log it and move on.
		if policy == nil {
			connLog.Warnf("No channel policy found for "+
				"ChannelPoint(%v): ", chanInfo.ChannelPoint)
		}

		// We'll now fetch the peer opposite from us within this
		// channel so we can queue up a direct connection to them.
		channelPeer, err := chanInfo.FetchOtherNode(tx, selfPub)
		if err != nil {
			return fmt.Errorf("unable to fetch channel peer for "+
				"ChannelPoint(%v): %v", chanInfo.ChannelPoint,
				err)
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
				if p.Config.TorActive {
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
					if p.Config.TorActive {
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

		nodeAddrsMap[pubStr] = n
		return nil
	})
	if err != nil && !errors.Is(err, channeldb.ErrGraphNoEdgesFound) {
		return err
	}

	connLog.Debugf("Establishing %v persistent connections on start",
		len(nodeAddrsMap))

	// Acquire and hold server lock until all persistent connection requests
	// have been recorded and sent to the connection manager.
	p.mu.Lock()
	defer p.mu.Unlock()

	// Iterate through the combined list of addresses from prior links and
	// node announcements and attempt to reconnect to each node.
	var numOutboundConns int
	for pubStr, nodeAddr := range nodeAddrsMap {
		// Add this peer to the set of peers we should maintain a
		// persistent connection with. We set the value to false to
		// indicate that we should not continue to reconnect if the
		// number of channels returns to zero, since this peer has not
		// been requested as perm by the user.
		p.persistentPeers[pubStr] = false
		if _, ok := p.persistentPeersBackoff[pubStr]; !ok {
			p.persistentPeersBackoff[pubStr] = p.Config.MinBackoff
		}

		for _, address := range nodeAddr.addresses {
			// Create a wrapper address which couples the IP and
			// the pubkey so the brontide authenticated connection
			// can be established.
			lnAddr := &lnwire.NetAddress{
				IdentityKey: nodeAddr.pubKey,
				Address:     address,
			}

			p.persistentPeerAddrs[pubStr] = append(
				p.persistentPeerAddrs[pubStr], lnAddr)
		}

		// We'll connect to the first 10 peers immediately, then
		// randomly stagger any remaining connections if the
		// stagger initial reconnect flag is set. This ensures
		// that mobile nodes or nodes with a small number of
		// channels obtain connectivity quickly, but larger
		// nodes are able to disperse the costs of connecting to
		// all peers at once.
		if numOutboundConns < numInstantInitReconnect ||
			!p.Config.StaggerInitialReconnect {

			go p.connectToPersistentPeer(pubStr)
		} else {
			go p.delayInitialReconnect(pubStr)
		}

		numOutboundConns++
	}

	return nil
}

// delayInitialReconnect will attempt a reconnection to the given peer after
// sampling a value for the delay between 0s and the maxInitReconnectDelay.
//
// NOTE: This method MUST be run as a goroutine.
func (p *PeerConnManager) delayInitialReconnect(pubStr string) {
	delay := time.Duration(prand.Intn(maxInitReconnectDelay)) * time.Second
	select {
	case <-time.After(delay):
		p.connectToPersistentPeer(pubStr)
	case <-p.quit:
	}
}

// PrunePersistentPeerConnection removes all internal state related to
// persistent connections to a peer within the server. This is used to avoid
// persistent connection retries to peers we do not have any open channels with.
func (p *PeerConnManager) PrunePersistentPeerConnection(
	compressedPubKey [33]byte) {

	pubKeyStr := string(compressedPubKey[:])

	p.mu.Lock()
	if perm, ok := p.persistentPeers[pubKeyStr]; ok && !perm {
		delete(p.persistentPeers, pubKeyStr)
		delete(p.persistentPeersBackoff, pubKeyStr)
		delete(p.persistentPeerAddrs, pubKeyStr)
		p.cancelConnReqs(pubKeyStr, nil)
		p.mu.Unlock()

		connLog.Infof("Pruned peer %x from persistent connections, "+
			"peer has no open channels", compressedPubKey)

		return
	}
	p.mu.Unlock()
}

// BroadcastMessage sends a request to the server to broadcast a set of
// messages to all peers other than the one specified by the `skips` parameter.
// All messages sent via BroadcastMessage will be queued for lazy delivery to
// the target peers.
//
// NOTE: This function is safe for concurrent access.
func (p *PeerConnManager) BroadcastMessage(skips map[route.Vertex]struct{},
	msgs ...lnwire.Message) error {

	connLog.Debugf("Broadcasting %v messages", len(msgs))

	// Filter out peers found in the skips map. We synchronize access to
	// peersByPub throughout this process to ensure we deliver messages to
	// exact set of peers present at the time of invocation.
	p.mu.RLock()
	peers := make([]*peer.Brontide, 0, len(p.peersByPub))
	for _, sPeer := range p.peersByPub {
		if skips != nil {
			if _, ok := skips[sPeer.PubKey()]; ok {
				connLog.Tracef("Skipping %x in broadcast",
					sPeer.PubKey())
				continue
			}
		}

		peers = append(peers, sPeer)
	}
	p.mu.RUnlock()

	// Iterate over all known peers, dispatching a go routine to enqueue
	// all messages to each of peers.
	var wg sync.WaitGroup
	for _, sPeer := range peers {
		// Dispatch a go routine to enqueue all messages to this peer.
		wg.Add(1)
		p.wg.Add(1)
		go func(peer lnpeer.Peer) {
			defer p.wg.Done()
			defer wg.Done()

			peer.SendMessageLazy(false, msgs...)
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
func (p *PeerConnManager) NotifyWhenOnline(peerKey [33]byte,
	peerChan chan<- lnpeer.Peer) {

	p.mu.Lock()

	// Compute the target peer's identifier.
	pubStr := string(peerKey[:])

	// Check if peer is connected.
	peer, ok := p.peersByPub[pubStr]
	if ok {
		// Unlock here so that the mutex isn't held while we are
		// waiting for the peer to become active.
		p.mu.Unlock()

		// Wait until the peer signals that it is actually active
		// rather than only in the server's maps.
		select {
		case <-peer.ActiveSignal():
		case <-peer.QuitSignal():
			// The peer quit, so we'll add the channel to the slice
			// and return.
			p.mu.Lock()
			p.peerConnectedListeners[pubStr] = append(
				p.peerConnectedListeners[pubStr], peerChan,
			)
			p.mu.Unlock()
			return
		}

		// Connected, can return early.
		connLog.Debugf("Notifying that peer %x is online", peerKey)

		select {
		case peerChan <- peer:
		case <-p.quit:
		}

		return
	}

	// Not connected, store this listener such that it can be notified when
	// the peer comes online.
	p.peerConnectedListeners[pubStr] = append(
		p.peerConnectedListeners[pubStr], peerChan,
	)
	p.mu.Unlock()
}

// NotifyWhenOffline delivers a notification to the caller of when the peer with
// the given public key has been disconnected. The notification is signaled by
// closing the channel returned.
func (p *PeerConnManager) NotifyWhenOffline(
	peerPubKey [33]byte) <-chan struct{} {

	p.mu.Lock()
	defer p.mu.Unlock()

	c := make(chan struct{})

	// If the peer is already offline, we can immediately trigger the
	// notification.
	peerPubKeyStr := string(peerPubKey[:])
	if _, ok := p.peersByPub[peerPubKeyStr]; !ok {
		connLog.Debugf("Notifying that peer %x is offline", peerPubKey)
		close(c)
		return c
	}

	// Otherwise, the peer is online, so we'll keep track of the channel to
	// trigger the notification once the server detects the peer
	// disconnects.
	p.peerDisconnectedListeners[peerPubKeyStr] = append(
		p.peerDisconnectedListeners[peerPubKeyStr], c,
	)

	return c
}

// FindPeer will return the peer that corresponds to the passed in public key.
// This function is used by the funding manager, allowing it to update the
// daemon's local representation of the remote peer.
//
// NOTE: This function is safe for concurrent access.
func (p *PeerConnManager) FindPeer(
	peerKey *btcec.PublicKey) (*peer.Brontide, error) {

	p.mu.RLock()
	defer p.mu.RUnlock()

	pubStr := string(peerKey.SerializeCompressed())

	return p.findPeerByPubStr(pubStr)
}

// FindPeerByPubStr will return the peer that corresponds to the passed peerID,
// which should be a string representation of the peer's serialized, compressed
// public key.
//
// NOTE: This function is safe for concurrent access.
func (p *PeerConnManager) FindPeerByPubStr(
	pubStr string) (*peer.Brontide, error) {

	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.findPeerByPubStr(pubStr)
}

// findPeerByPubStr is an internal method that retrieves the specified peer from
// the server's internal state using.
func (p *PeerConnManager) findPeerByPubStr(pubStr string) (*peer.Brontide, error) {
	peer, ok := p.peersByPub[pubStr]
	if !ok {
		return nil, ErrPeerNotConnected
	}

	return peer, nil
}

// nextPeerBackoff computes the next backoff duration for a peer's pubkey using
// exponential backoff. If no previous backoff was known, the default is
// returned.
func (p *PeerConnManager) nextPeerBackoff(pubStr string,
	startTime time.Time) time.Duration {

	// Now, determine the appropriate backoff to use for the retry.
	backoff, ok := p.persistentPeersBackoff[pubStr]
	if !ok {
		// If an existing backoff was unknown, use the default.
		return p.Config.MinBackoff
	}

	// If the peer failed to start properly, we'll just use the previous
	// backoff to compute the subsequent randomized exponential backoff
	// duration. This will roughly double on average.
	if startTime.IsZero() {
		return computeNextBackoff(backoff, p.Config.MaxBackoff)
	}

	// The peer succeeded in starting. If the connection didn't last long
	// enough to be considered stable, we'll continue to back off retries
	// with this peer.
	connDuration := time.Since(startTime)
	if connDuration < defaultStableConnDuration {
		return computeNextBackoff(backoff, p.Config.MaxBackoff)
	}

	// The peer succeed in starting and this was stable peer, so we'll
	// reduce the timeout duration by the length of the connection after
	// applying randomized exponential backoff. We'll only apply this in the
	// case that:
	//   reb(curBackoff) - connDuration > cfg.MinBackoff
	relaxedBackoff := computeNextBackoff(backoff, p.Config.MaxBackoff) -
		connDuration
	if relaxedBackoff > p.Config.MinBackoff {
		return relaxedBackoff
	}

	// Lastly, if reb(currBackoff) - connDuration <= cfg.MinBackoff, meaning
	// the stable connection lasted much longer than our previous backoff.
	// To reward such good behavior, we'll reconnect after the default
	// timeout.
	return p.Config.MinBackoff
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
func (p *PeerConnManager) InboundPeerConnected(conn net.Conn) {
	// Exit early if we have already been instructed to shutdown, this
	// prevents any delayed callbacks from accidentally registering peers.
	if p.Stopped() {
		return
	}

	brontideConn, ok := conn.(*brontide.Conn)
	if !ok {
		connLog.Errorf("Unexpected conn type found in "+
			"InboundPeerConnected: %v", conn)
		return
	}

	nodePub := brontideConn.RemotePub()
	pubStr := string(nodePub.SerializeCompressed())

	p.mu.Lock()
	defer p.mu.Unlock()

	// If we already have an outbound connection to this peer, then ignore
	// this new connection.
	if p, ok := p.outboundPeers[pubStr]; ok {
		connLog.Debugf("Already have outbound connection for %v, "+
			"ignoring inbound connection from local=%v, remote=%v",
			p, conn.LocalAddr(), conn.RemoteAddr())

		conn.Close()
		return
	}

	// If we already have a valid connection that is scheduled to take
	// precedence once the prior peer has finished disconnecting, we'll
	// ignore this connection.
	if p, ok := p.scheduledPeerConnection[pubStr]; ok {
		connLog.Debugf("Ignoring connection from %v, peer %v already "+
			"scheduled", conn.RemoteAddr(), p)
		conn.Close()
		return
	}

	connLog.Infof("New inbound connection from %v", conn.RemoteAddr())

	// Check to see if we already have a connection with this peer. If so,
	// we may need to drop our existing connection. This prevents us from
	// having duplicate connections to the same peer. We forgo adding a
	// default case as we expect these to be the only error values returned
	// from findPeerByPubStr.
	connectedPeer, err := p.findPeerByPubStr(pubStr)
	switch {
	case errors.Is(err, ErrPeerNotConnected):
		// We were unable to locate an existing connection with the
		// target peer, proceed to connect.
		p.cancelConnReqs(pubStr, nil)
		p.peerConnected(conn, nil, true)

	case err == nil:
		// We already have a connection with the incoming peer. If the
		// connection we've already established should be kept and is
		// not of the same type of the new connection (inbound), then
		// we'll close out the new connection s.t there's only a single
		// connection between us.
		localPub := p.IdentityECDH.PubKey()
		if !connectedPeer.Inbound() &&
			!shouldDropLocalConnection(localPub, nodePub) {

			connLog.Warnf("Received inbound connection from "+
				"peer %v, but already have outbound "+
				"connection, dropping conn", connectedPeer)
			conn.Close()
			return
		}

		// Otherwise, if we should drop the connection, then we'll
		// disconnect our already connected peer.
		connLog.Debugf("Disconnecting stale connection to %v",
			connectedPeer)

		p.cancelConnReqs(pubStr, nil)

		// Remove the current peer from the server's internal state and
		// signal that the peer termination watcher does not need to
		// execute for this peer.
		p.removePeer(connectedPeer)
		p.ignorePeerTermination[connectedPeer] = struct{}{}
		p.scheduledPeerConnection[pubStr] = func() {
			p.peerConnected(conn, nil, true)
		}
	}
}

// OutboundPeerConnected initializes a new peer in response to a new outbound
// connection.
// NOTE: This function is safe for concurrent access.
func (p *PeerConnManager) OutboundPeerConnected(connReq *connmgr.ConnReq,
	conn net.Conn) {

	// Exit early if we have already been instructed to shutdown, this
	// prevents any delayed callbacks from accidentally registering peers.
	if p.Stopped() {
		return
	}

	brontideConn, ok := conn.(*brontide.Conn)
	if !ok {
		connLog.Errorf("Unexpected conn type found in "+
			"OutboundPeerConnected: %v", conn)
		return
	}

	nodePub := brontideConn.RemotePub()
	pubStr := string(nodePub.SerializeCompressed())

	p.mu.Lock()
	defer p.mu.Unlock()

	// If we already have an inbound connection to this peer, then ignore
	// this new connection.
	if peer, ok := p.inboundPeers[pubStr]; ok {
		connLog.Debugf("Already have inbound connection for %v, "+
			"ignoring outbound connection from local=%v, remote=%v",
			peer, conn.LocalAddr(), conn.RemoteAddr())

		if connReq != nil {
			p.connMgr.Remove(connReq.ID())
		}
		conn.Close()
		return
	}
	if _, ok := p.persistentConnReqs[pubStr]; !ok && connReq != nil {
		connLog.Debugf("Ignoring canceled outbound connection")
		p.connMgr.Remove(connReq.ID())
		conn.Close()
		return
	}

	// If we already have a valid connection that is scheduled to take
	// precedence once the prior peer has finished disconnecting, we'll
	// ignore this connection.
	if _, ok := p.scheduledPeerConnection[pubStr]; ok {
		connLog.Debugf("Ignoring connection, peer already scheduled")

		if connReq != nil {
			p.connMgr.Remove(connReq.ID())
		}

		conn.Close()
		return
	}

	connLog.Infof("Established connection to: %x@%v", pubStr,
		conn.RemoteAddr())

	if connReq != nil {
		// A successful connection was returned by the connmgr.
		// Immediately cancel all pending requests, excluding the
		// outbound connection we just established.
		ignore := connReq.ID()
		p.cancelConnReqs(pubStr, &ignore)
	} else {
		// This was a successful connection made by some other
		// subsystem. Remove all requests being managed by the connmgr.
		p.cancelConnReqs(pubStr, nil)
	}

	// If we already have a connection with this peer, decide whether or not
	// we need to drop the stale connection. We forgo adding a default case
	// as we expect these to be the only error values returned from
	// findPeerByPubStr.
	connectedPeer, err := p.findPeerByPubStr(pubStr)
	switch {
	case errors.Is(err, ErrPeerNotConnected):
		// We were unable to locate an existing connection with the
		// target peer, proceed to connect.
		p.peerConnected(conn, connReq, false)

	case err == nil:
		// We already have a connection with the incoming peer. If the
		// connection we've already established should be kept and is
		// not of the same type of the new connection (outbound), then
		// we'll close out the new connection s.t there's only a single
		// connection between us.
		localPub := p.IdentityECDH.PubKey()
		if connectedPeer.Inbound() &&
			shouldDropLocalConnection(localPub, nodePub) {

			connLog.Warnf("Established outbound connection to "+
				"peer %v, but already have inbound "+
				"connection, dropping conn", connectedPeer)
			if connReq != nil {
				p.connMgr.Remove(connReq.ID())
			}
			conn.Close()
			return
		}

		// Otherwise, _their_ connection should be dropped. So we'll
		// disconnect the peer and send the now obsolete peer to the
		// server for garbage collection.
		connLog.Debugf("Disconnecting stale connection to %v",
			connectedPeer)

		// Remove the current peer from the server's internal state and
		// signal that the peer termination watcher does not need to
		// execute for this peer.
		p.removePeer(connectedPeer)
		p.ignorePeerTermination[connectedPeer] = struct{}{}
		p.scheduledPeerConnection[pubStr] = func() {
			p.peerConnected(conn, connReq, false)
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
func (p *PeerConnManager) cancelConnReqs(pubStr string, skip *uint64) {
	// First, cancel any lingering persistent retry attempts, which will
	// prevent retries for any with backoffs that are still maturing.
	if cancelChan, ok := p.persistentRetryCancels[pubStr]; ok {
		close(cancelChan)
		delete(p.persistentRetryCancels, pubStr)
	}

	// Next, check to see if we have any outstanding persistent connection
	// requests to this peer. If so, then we'll remove all of these
	// connection requests, and also delete the entry from the map.
	connReqs, ok := p.persistentConnReqs[pubStr]
	if !ok {
		return
	}

	for _, connReq := range connReqs {
		connLog.Tracef("Canceling %s:", connReqs)

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

		p.connMgr.Remove(connID)
	}

	delete(p.persistentConnReqs, pubStr)
}

// peerConnected is a function that handles initialization a newly connected
// peer by adding it to the server's global list of all active peers, and
// starting all the goroutines the peer needs to function properly. The inbound
// boolean should be true if the peer initiated the connection to us.
func (p *PeerConnManager) peerConnected(conn net.Conn, connReq *connmgr.ConnReq,
	inbound bool) {

	brontideConn, ok := conn.(*brontide.Conn)
	if !ok {
		connLog.Errorf("Unexpected conn type found in peerConnected: "+
			"%v", conn)
		return
	}

	addr := conn.RemoteAddr()
	pubKey := brontideConn.RemotePub()

	connLog.Infof("Finalizing connection to %x@%s, inbound=%v",
		pubKey.SerializeCompressed(), addr, inbound)

	peerAddr := &lnwire.NetAddress{
		IdentityKey: pubKey,
		Address:     addr,
		ChainNet:    p.Config.NetParams,
	}

	// With the brontide connection established, we'll now craft the feature
	// vectors to advertise to the remote node.
	initFeatures := p.Config.FeatureMgr.Get(feature.SetInit)
	legacyFeatures := p.Config.FeatureMgr.Get(feature.SetLegacyGlobal)

	// Lookup past error caches for the peer in the server. If no buffer is
	// found, create a fresh buffer.
	pkStr := string(peerAddr.IdentityKey.SerializeCompressed())
	errBuffer, ok := p.peerErrors[pkStr]
	if !ok {
		var err error
		errBuffer, err = queue.NewCircularBuffer(peer.ErrorBufferSize)
		if err != nil {
			connLog.Errorf("unable to create peer %v", err)
			return
		}
	}

	// Now that we've established a connection, create a peer, and it to the
	// set of currently active peers. Configure the peer with the incoming
	// and outgoing broadcast deltas to prevent htlcs from being accepted or
	// offered that would trigger channel closure. In case of outgoing
	// htlcs, an extra block is added to prevent the channel from being
	// closed when the htlc is outstanding and a new block comes in.
	pCfg := p.Config.PartialPeerConfig

	// Finish the partial peer config.
	pCfg.Conn = brontideConn
	pCfg.ConnReq = connReq
	pCfg.Addr = peerAddr
	pCfg.Inbound = inbound
	pCfg.Features = initFeatures
	pCfg.LegacyFeatures = legacyFeatures
	pCfg.ErrorBuffer = errBuffer
	pCfg.Quit = p.quit

	copy(pCfg.PubKeyBytes[:], peerAddr.IdentityKey.SerializeCompressed())
	selfPub := p.IdentityECDH.PubKey().SerializeCompressed()
	copy(pCfg.ServerPubKey[:], selfPub)

	peer := peer.NewBrontide(pCfg)

	// TODO(roasbeef): update IP address for link-node
	//  * also mark last-seen, do it one single transaction?

	p.addPeer(peer)

	// Once we have successfully added the peer to the server, we can
	// delete the previous error buffer from the server's map of error
	// buffers.
	delete(p.peerErrors, pkStr)

	// Dispatch a goroutine to asynchronously start the peer. This process
	// includes sending and receiving Init messages, which would be a DOS
	// vector if we held the server's mutex throughout the procedure.
	p.wg.Add(1)
	go p.peerInitializer(peer)
}

// addPeer adds the passed peer to the server's global state of all active
// peers.
func (p *PeerConnManager) addPeer(peer *peer.Brontide) {
	if peer == nil {
		return
	}

	// Ignore new peers if we're shutting down.
	if p.Stopped() {
		peer.Disconnect(ErrServerShuttingDown)
		return
	}

	// Track the new peer in our indexes so we can quickly look it up either
	// according to its public key, or its peer ID.
	// TODO(roasbeef): pipe all requests through to the
	// queryHandler/peerManager

	pubSer := peer.IdentityKey().SerializeCompressed()
	pubStr := string(pubSer)

	p.peersByPub[pubStr] = peer

	if peer.Inbound() {
		p.inboundPeers[pubStr] = peer
	} else {
		p.outboundPeers[pubStr] = peer
	}

	// Inform the peer notifier of a peer online event so that it can be
	// reported to clients listening for peer events.
	var pubKey [33]byte
	copy(pubKey[:], pubSer)

	p.PeerNotifier.NotifyPeerOnline(pubKey)
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
func (p *PeerConnManager) peerInitializer(peer *peer.Brontide) {
	defer p.wg.Done()

	// Avoid initializing peers while the server is exiting.
	if p.Stopped() {
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
	p.wg.Add(1)
	go p.peerTerminationWatcher(peer, ready)

	// Start the peer! If an error occurs, we Disconnect the peer, which
	// will unblock the peerTerminationWatcher.
	if err := peer.Start(); err != nil {
		peer.Disconnect(fmt.Errorf("unable to start peer: %w", err))
		return
	}

	// Otherwise, signal to the peerTerminationWatcher that the peer startup
	// was successful, and to begin watching the peer's wait group.
	close(ready)

	pubStr := string(peer.IdentityKey().SerializeCompressed())

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if there are listeners waiting for this peer to come online.
	connLog.Debugf("Notifying that peer %v is online", peer)
	for _, peerChan := range p.peerConnectedListeners[pubStr] {
		select {
		case peerChan <- peer:
		case <-p.quit:
			return
		}
	}
	delete(p.peerConnectedListeners, pubStr)
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
func (p *PeerConnManager) peerTerminationWatcher(peer *peer.Brontide,
	ready chan struct{}) {

	defer p.wg.Done()

	peer.WaitForDisconnect(ready)

	connLog.Debugf("Peer %v has been disconnected", peer)

	// If the server is exiting then we can bail out early ourselves as all
	// the other sub-systems will already be shutting down.
	if p.Stopped() {
		return
	}

	// Next, we'll cancel all pending funding reservations with this node.
	// If we tried to initiate any funding flows that haven't yet finished,
	// then we need to unlock those committed outputs so they're still
	// available for use.
	p.Config.CancelPeerReservations(peer.PubKey())

	pubKey := peer.IdentityKey()

	// We'll also inform the gossiper that this peer is no longer active,
	// so we don't need to maintain sync state for it any longer.
	p.Config.PartialPeerConfig.AuthGossiper.PruneSyncState(peer.PubKey())

	// Tell the switch to remove all links associated with this peer.
	// Passing nil as the target link indicates that all links associated
	// with this interface should be closed.
	//
	// TODO(roasbeef): instead add a PurgeInterfaceLinks function?
	links, err := p.Config.PartialPeerConfig.Switch.GetLinksByInterface(
		peer.PubKey(),
	)
	if err != nil && !errors.Is(err, htlcswitch.ErrNoLinksFound) {
		connLog.Errorf("Unable to get channel links for %v: %v",
			peer, err)
	}

	for _, link := range links {
		p.Config.PartialPeerConfig.Switch.RemoveLink(link.ChanID())
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// If there were any notification requests for when this peer
	// disconnected, we can trigger them now.
	connLog.Debugf("Notifying that peer %v is offline", peer)
	pubStr := string(pubKey.SerializeCompressed())
	for _, offlineChan := range p.peerDisconnectedListeners[pubStr] {
		close(offlineChan)
	}
	delete(p.peerDisconnectedListeners, pubStr)

	// If the server has already removed this peer, we can short circuit the
	// peer termination watcher and skip cleanup.
	if _, ok := p.ignorePeerTermination[peer]; ok {
		delete(p.ignorePeerTermination, peer)

		pubKey := peer.PubKey()
		pubStr := string(pubKey[:])

		// If a connection callback is present, we'll go ahead and
		// execute it now that previous peer has fully disconnected. If
		// the callback is not present, this likely implies the peer was
		// purposefully disconnected via RPC, and that no reconnect
		// should be attempted.
		connCallback, ok := p.scheduledPeerConnection[pubStr]
		if ok {
			delete(p.scheduledPeerConnection, pubStr)
			connCallback()
		}
		return
	}

	// First, cleanup any remaining state the server has regarding the peer
	// in question.
	p.removePeer(peer)

	// Next, check to see if this is a persistent peer or not.
	if _, ok := p.persistentPeers[pubStr]; !ok {
		return
	}

	// Get the last address that we used to connect to the peer.
	addrs := []net.Addr{
		peer.NetAddress().Address,
	}

	// We'll ensure that we locate all the peers advertised addresses for
	// reconnection purposes.
	advertisedAddrs, err := p.fetchNodeAdvertisedAddrs(pubKey)
	switch {
	// We found advertised addresses, so use them.
	case err == nil:
		addrs = advertisedAddrs

	// The peer doesn't have an advertised address.
	case errors.Is(err, errNoAdvertisedAddr):
		// If it is an outbound peer then we fall back to the existing
		// peer address.
		if !peer.Inbound() {
			break
		}

		// Fall back to the existing peer address if
		// we're not accepting connections over Tor.
		if p.torController == nil {
			break
		}

		// If we are, the peer's address won't be known
		// to us (we'll see a private address, which is
		// the address used by our onion service to dial
		// to lnd), so we don't have enough information
		// to attempt a reconnect.
		connLog.Debugf("Ignoring reconnection attempt "+
			"to inbound peer %v without "+
			"advertised address", peer)
		return

	// We came across an error retrieving an advertised
	// address, log it, and fall back to the existing peer
	// address.
	default:
		connLog.Errorf("Unable to retrieve advertised "+
			"address for node %x: %v", peer.PubKey(),
			err)
	}

	// Make an easy lookup map so that we can check if an address
	// is already in the address list that we have stored for this peer.
	existingAddrs := make(map[string]bool)
	for _, addr := range p.persistentPeerAddrs[pubStr] {
		existingAddrs[addr.String()] = true
	}

	// Add any missing addresses for this peer to persistentPeerAddr.
	for _, addr := range addrs {
		if existingAddrs[addr.String()] {
			continue
		}

		p.persistentPeerAddrs[pubStr] = append(
			p.persistentPeerAddrs[pubStr],
			&lnwire.NetAddress{
				IdentityKey: peer.IdentityKey(),
				Address:     addr,
				ChainNet:    peer.NetAddress().ChainNet,
			},
		)
	}

	// Record the computed backoff in the backoff map.
	backoff := p.nextPeerBackoff(pubStr, peer.StartTime())
	p.persistentPeersBackoff[pubStr] = backoff

	// Initialize a retry canceller for this peer if one does not
	// exist.
	cancelChan, ok := p.persistentRetryCancels[pubStr]
	if !ok {
		cancelChan = make(chan struct{})
		p.persistentRetryCancels[pubStr] = cancelChan
	}

	// We choose not to wait group this go routine since the Connect
	// call can stall for arbitrarily long if we shutdown while an
	// outbound connection attempt is being made.
	go func() {
		connLog.Debugf("Scheduling connection re-establishment to "+
			"persistent peer %x in %s",
			peer.IdentityKey().SerializeCompressed(), backoff)

		select {
		case <-time.After(backoff):
		case <-cancelChan:
			return
		case <-p.quit:
			return
		}

		connLog.Debugf("Attempting to re-establish persistent "+
			"connection to peer %x",
			peer.IdentityKey().SerializeCompressed())

		p.connectToPersistentPeer(pubStr)
	}()
}

// connectToPersistentPeer uses all the stored addresses for a peer to attempt
// to connect to the peer. It creates connection requests if there are
// currently none for a given address and it removes old connection requests
// if the associated address is no longer in the latest address list for the
// peer.
func (p *PeerConnManager) connectToPersistentPeer(pubKeyStr string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Create an easy lookup map of the addresses we have stored for the
	// peer. We will remove entries from this map if we have existing
	// connection requests for the associated address and then any leftover
	// entries will indicate which addresses we should create new
	// connection requests for.
	addrMap := make(map[string]*lnwire.NetAddress)
	for _, addr := range p.persistentPeerAddrs[pubKeyStr] {
		addrMap[addr.String()] = addr
	}

	// Go through each of the existing connection requests and
	// check if they correspond to the latest set of addresses. If
	// there is a connection requests that does not use one of the latest
	// advertised addresses then remove that connection request.
	var updatedConnReqs []*connmgr.ConnReq
	for _, connReq := range p.persistentConnReqs[pubKeyStr] {
		wireAddr, ok := connReq.Addr.(*lnwire.NetAddress)
		if !ok {
			connLog.Errorf("Unexpected network address type %v",
				connReq.Addr)
		}

		lnAddr := wireAddr.Address.String()

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
			connLog.Info(
				"Removing conn req:", connReq.Addr.String(),
			)
			p.connMgr.Remove(connReq.ID())
		}
	}

	p.persistentConnReqs[pubKeyStr] = updatedConnReqs

	cancelChan, ok := p.persistentRetryCancels[pubKeyStr]
	if !ok {
		cancelChan = make(chan struct{})
		p.persistentRetryCancels[pubKeyStr] = cancelChan
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

			p.mu.Lock()
			p.persistentConnReqs[pubKeyStr] = append(
				p.persistentConnReqs[pubKeyStr], connReq,
			)
			p.mu.Unlock()

			connLog.Debugf("Attempting persistent connection to "+
				"channel peer %v", addr)

			go p.connMgr.Connect(connReq)

			select {
			case <-p.quit:
				return
			case <-cancelChan:
				return
			case <-ticker.C:
			}
		}
	}()
}

// removePeer removes the passed peer from the server's state of all active
// peers.
func (p *PeerConnManager) removePeer(peer *peer.Brontide) {
	if peer == nil {
		return
	}

	connLog.Debugf("removing peer %v", peer)

	// As the peer is now finished, ensure that the TCP connection is
	// closed and all of its related goroutines have exited.
	peer.Disconnect(fmt.Errorf("server: disconnecting peer %v", peer))

	// If this peer had an active persistent connection request, remove it.
	if peer.ConnReq() != nil {
		p.connMgr.Remove(peer.ConnReq().ID())
	}

	// Ignore deleting peers if we're shutting down.
	if p.Stopped() {
		return
	}

	pKey := peer.PubKey()
	pubSer := pKey[:]
	pubStr := string(pubSer)

	delete(p.peersByPub, pubStr)

	if peer.Inbound() {
		delete(p.inboundPeers, pubStr)
	} else {
		delete(p.outboundPeers, pubStr)
	}

	// Copy the peer's error buffer across to the server if it has any items
	// in it so that we can restore peer errors across connections.
	if peer.ErrorBuffer().Total() > 0 {
		p.peerErrors[pubStr] = peer.ErrorBuffer()
	}

	// Inform the peer notifier of a peer offline event so that it can be
	// reported to clients listening for peer events.
	var pubKey [33]byte
	copy(pubKey[:], pubSer)

	p.PeerNotifier.NotifyPeerOffline(pubKey)
}

// ConnectToPeer requests that the server connect to a Lightning Network peer
// at the specified address. This function will *block* until either a
// connection is established, or the initial handshake process fails.
//
// NOTE: This function is safe for concurrent access.
func (p *PeerConnManager) ConnectToPeer(addr *lnwire.NetAddress,
	perm bool, timeout time.Duration) error {

	targetPub := string(addr.IdentityKey.SerializeCompressed())

	// Acquire mutex, but use explicit unlocking instead of defer for
	// better granularity.  In certain conditions, this method requires
	// making an outbound connection to a remote peer, which requires the
	// lock to be released, and subsequently reacquired.
	p.mu.Lock()

	// Ensure we're not already connected to this peer.
	peer, err := p.findPeerByPubStr(targetPub)
	if err == nil {
		p.mu.Unlock()
		return &ErrPeerAlreadyConnected{peer: peer}
	}

	// Peer was not found, continue to pursue connection with peer.

	// If there's already a pending connection request for this pubkey,
	// then we ignore this request to ensure we don't create a redundant
	// connection.
	if reqs, ok := p.persistentConnReqs[targetPub]; ok {
		connLog.Warnf("Already have %d persistent connection "+
			"requests for %v, connecting anyway.", len(reqs), addr)
	}

	// If there's not already a pending or active connection to this node,
	// then instruct the connection manager to attempt to establish a
	// persistent connection to the peer.
	connLog.Debugf("Connecting to %v", addr)
	if perm {
		connReq := &connmgr.ConnReq{
			Addr:      addr,
			Permanent: true,
		}

		// Since the user requested a permanent connection, we'll set
		// the entry to true which will tell the server to continue
		// reconnecting even if the number of channels with this peer is
		// zero.
		p.persistentPeers[targetPub] = true
		if _, ok := p.persistentPeersBackoff[targetPub]; !ok {
			p.persistentPeersBackoff[targetPub] = p.Config.MinBackoff
		}
		p.persistentConnReqs[targetPub] = append(
			p.persistentConnReqs[targetPub], connReq,
		)
		p.mu.Unlock()

		go p.connMgr.Connect(connReq)

		return nil
	}
	p.mu.Unlock()

	// If we're not making a persistent connection, then we'll attempt to
	// connect to the target peer. If the we can't make the connection, or
	// the crypto negotiation breaks down, then return an error to the
	// caller.
	errChan := make(chan error, 1)
	p.connectToPeer(addr, errChan, timeout)

	select {
	case err := <-errChan:
		return err
	case <-p.quit:
		return ErrServerShuttingDown
	}
}

// connectToPeer establishes a connection to a remote peer. errChan is used to
// notify the caller if the connection attempt has failed. Otherwise, it will be
// closed.
func (p *PeerConnManager) connectToPeer(addr *lnwire.NetAddress,
	errChan chan<- error, timeout time.Duration) {

	conn, err := brontide.Dial(
		p.IdentityECDH, addr, timeout, p.Config.Net.Dial,
	)
	if err != nil {
		connLog.Errorf("Unable to connect to %v: %v", addr, err)
		select {
		case errChan <- err:
		case <-p.quit:
		}
		return
	}

	close(errChan)

	connLog.Tracef("Brontide dialer made local=%v, remote=%v",
		conn.LocalAddr(), conn.RemoteAddr())

	p.OutboundPeerConnected(nil, conn)
}

// DisconnectPeer sends the request to server to close the connection with peer
// identified by public key.
//
// NOTE: This function is safe for concurrent access.
func (p *PeerConnManager) DisconnectPeer(pubKey *btcec.PublicKey) error {
	pubBytes := pubKey.SerializeCompressed()
	pubStr := string(pubBytes)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check that were actually connected to this peer. If not, then we'll
	// exit in an error as we can't disconnect from a peer that we're not
	// currently connected to.
	peer, err := p.findPeerByPubStr(pubStr)
	if errors.Is(err, ErrPeerNotConnected) {
		return fmt.Errorf("peer %x is not connected", pubBytes)
	}

	connLog.Infof("Disconnecting from %v", peer)

	p.cancelConnReqs(pubStr, nil)

	// If this peer was formerly a persistent connection, then we'll remove
	// them from this map so we don't attempt to re-connect after we
	// disconnect.
	delete(p.persistentPeers, pubStr)
	delete(p.persistentPeersBackoff, pubStr)

	// Remove the peer by calling Disconnect. Previously this was done with
	// removePeer, which bypassed the peerTerminationWatcher.
	peer.Disconnect(fmt.Errorf("server: DisconnectPeer called"))

	return nil
}

// SendCustomMessage sends a custom message to the peer with the specified
// pubkey.
func (p *PeerConnManager) SendCustomMessage(peerPub [33]byte,
	msgType lnwire.MessageType, data []byte) error {

	peer, err := p.FindPeerByPubStr(string(peerPub[:]))
	if err != nil {
		return err
	}

	// We'll wait until the peer is active.
	select {
	case <-peer.ActiveSignal():
	case <-peer.QuitSignal():
		return fmt.Errorf("peer %x disconnected", peerPub)
	case <-p.quit:
		return ErrServerShuttingDown
	}

	msg, err := lnwire.NewCustom(msgType, data)
	if err != nil {
		return err
	}

	// Send the message as low-priority. For now we assume that all
	// application-defined message are low priority.
	return peer.SendMessageLazy(true, msg)
}

// AddPersistentPeer adds a peer's public key to the persistentPeers map.
func (p *PeerConnManager) AddPersistentPeer(peerKey *btcec.PublicKey) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pubStr := string(peerKey.SerializeCompressed())
	if _, ok := p.persistentPeers[pubStr]; !ok {
		p.persistentPeers[pubStr] = false
	}
}

// Peers returns a slice of all active peers.
//
// NOTE: This function is safe for concurrent access.
func (p *PeerConnManager) Peers() []*peer.Brontide {
	p.mu.RLock()
	defer p.mu.RUnlock()

	peers := make([]*peer.Brontide, 0, len(p.peersByPub))
	for _, peer := range p.peersByPub {
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

// fetchNodeAdvertisedAddrs attempts to fetch the advertised addresses of a
// node.
func (p *PeerConnManager) fetchNodeAdvertisedAddrs(
	pub *btcec.PublicKey) ([]net.Addr, error) {

	vertex, err := route.NewVertexFromBytes(pub.SerializeCompressed())
	if err != nil {
		return nil, err
	}

	node, err := p.Config.PartialPeerConfig.ChannelGraph.FetchLightningNode(
		vertex,
	)
	if err != nil {
		return nil, err
	}

	if len(node.Addresses) == 0 {
		return nil, errNoAdvertisedAddr
	}

	return node.Addresses, nil
}

// noiseDial is a factory function which creates a connmgr compliant dialing
// function by returning a closure which includes the server's identity key.
func noiseDial(idKey keychain.SingleKeyECDH,
	netCfg tor.Net, timeout time.Duration) func(net.Addr) (net.Conn, error) {

	return func(a net.Addr) (net.Conn, error) {
		lnAddr, ok := a.(*lnwire.NetAddress)
		if !ok {
			return nil, fmt.Errorf("Unexpected network address "+
				"type %v", a)
		}

		return brontide.Dial(idKey, lnAddr, timeout, netCfg.Dial)
	}
}

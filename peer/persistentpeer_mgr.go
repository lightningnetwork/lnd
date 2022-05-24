package peer

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// defaultStableConnDuration is a floor under which all reconnection
	// attempts will apply exponential randomized backoff. Connections
	// durations exceeding this value will be eligible to have their
	// backoffs reduced.
	defaultStableConnDuration = 10 * time.Minute

	// multiAddrConnectionStagger is the number of seconds to wait between
	// attempting to a peer with each of its advertised addresses.
	multiAddrConnectionStagger = 10 * time.Second
)

// PersistentPeerMgrConfig holds the config of the PersistentPeerManager.
type PersistentPeerMgrConfig struct {
	// ConnMgr is used to manage the creation and removal of connection
	// requests. It handles the actual connection to a peer.
	ConnMgr connMgr

	// SubscribeTopology will be used to listen for updates to a persistent
	// peer's advertised addresses.
	SubscribeTopology func() (*routing.TopologyClient, error)

	// ChainNet is the Bitcoin network this node is associated with.
	ChainNet wire.BitcoinNet

	// MinBackoff is the shortest backoff when reconnecting to a persistent
	// peer.
	MinBackoff time.Duration

	// MaxBackoff is the longest backoff when reconnecting to a persistent
	// peer.
	MaxBackoff time.Duration

	// AddrTypeIsSupported returns true if we can connect to this type of
	// address.
	AddrTypeIsSupported func(addr net.Addr) bool

	// FetchNodeAdvertisedAddrs can be used to fetch the advertised
	// addresses of a node that we have persisted. This should only ge used
	// to fetch an initial set of addresses for the peer.
	FetchNodeAdvertisedAddrs func(route.Vertex) ([]net.Addr, error)
}

// PersistentPeerManager manages persistent peers.
type PersistentPeerManager struct {
	// cfg holds the config of the manager.
	cfg *PersistentPeerMgrConfig

	// conns maps a peer's public key to a persistentPeer object.
	conns map[route.Vertex]*persistentPeer

	quit chan struct{}
	wg   sync.WaitGroup
	sync.RWMutex
}

// persistentPeer holds all the info about a peer that the
// PersistentPeerManager needs.
type persistentPeer struct {
	// pubKey is the public key identifier of the peer.
	pubKey *btcec.PublicKey

	// perm indicates if a connection to the peer should be maintained even
	// if there are no channels with the peer.
	perm bool

	// addrs is all the addresses we know about for this peer. It is a map
	// from the address string to the address object.
	addrs map[string]*lnwire.NetAddress

	// connReqs holds all the active connection requests that we have for
	// the peer.
	connReqs []*connmgr.ConnReq

	// backoff is the time to wait before trying to reconnect to a peer.
	backoff time.Duration

	// retryCanceller is used to cancel any retry attempt with backoff
	// that is still maturing.
	retryCanceller *chan struct{}
}

// cancelRetries closes the retry canceller channel of the peer.
func (p *persistentPeer) cancelRetries() {
	if p.retryCanceller == nil {
		return
	}

	// Cancel any lingering persistent retry attempts, which will
	// prevent retries for any with backoffs that are still maturing.
	close(*p.retryCanceller)
	p.retryCanceller = nil
}

// getRetryCanceller cancels any existing retry canceler and then creates and
// returns a new one.
func (p *persistentPeer) getRetryCanceller() chan struct{} {
	p.cancelRetries()

	cancelChan := make(chan struct{})
	p.retryCanceller = &cancelChan

	return cancelChan
}

// connMgr is what the PersistentPeerManager will use to create and remove
// connection requests. The purpose of this interface is to make testing easier.
type connMgr interface {
	Connect(c *connmgr.ConnReq)
	Remove(id uint64)
}

// NewPersistentPeerManager creates a new PersistentPeerManager instance.
func NewPersistentPeerManager(
	cfg *PersistentPeerMgrConfig) *PersistentPeerManager {

	return &PersistentPeerManager{
		cfg:   cfg,
		conns: make(map[route.Vertex]*persistentPeer),
		quit:  make(chan struct{}),
	}
}

// Start begins the processes of PersistentPeerManager. It subscribes to graph
// updates and listens for any NodeAnnouncement messages that indicate that the
// addresses of one of the persistent peers has changed and then updates the
// peer's addresses and connection requests accordingly.
func (m *PersistentPeerManager) Start() error {
	graphSub, err := m.cfg.SubscribeTopology()
	if err != nil {
		return fmt.Errorf("could not subscribe to graph: %v", err)
	}

	m.wg.Add(1)
	go func() {
		defer func() {
			graphSub.Cancel()
			m.wg.Done()
		}()

		for {
			select {
			case topChange, ok := <-graphSub.TopologyChanges:
				// If the router is shutting down, then we will
				// as well.
				if !ok {
					peerLog.Errorf("graph subscription " +
						"channel has been closed")
					return
				}

				for _, update := range topChange.NodeUpdates {
					if !m.processSingleNodeUpdate(update) {
						continue
					}

					m.ConnectPeer(update.IdentityKey)
				}

			case <-m.quit:
				return
			}
		}
	}()

	return nil
}

// Stop closes the quit channel of the PersistentPeerManager and waits for all
// goroutines to exit.
func (m *PersistentPeerManager) Stop() {
	close(m.quit)
	m.wg.Wait()
}

// AddPeer adds a new persistent peer for the PersistentPeerManager to keep
// track of. The peer may be initialised with an initial set of addresses.
func (m *PersistentPeerManager) AddPeer(pubKey *btcec.PublicKey, perm bool,
	addrs ...net.Addr) {

	peerKey := route.NewVertex(pubKey)

	// Fetch any stored addresses we may have for this peer.
	advertisedAddrs, err := m.cfg.FetchNodeAdvertisedAddrs(peerKey)
	if err != nil {
		peerLog.Errorf("Unable to retrieve advertised address for "+
			"node %s: %v", peerKey, err)
	}

	m.Lock()
	defer m.Unlock()

	backoff := m.cfg.MinBackoff
	if peer, ok := m.conns[peerKey]; ok {
		backoff = peer.backoff
	}

	m.conns[peerKey] = &persistentPeer{
		pubKey:  pubKey,
		perm:    perm,
		backoff: backoff,
	}

	m.setPeerAddrsUnsafe(peerKey, append(advertisedAddrs, addrs...))
}

// IsPersistentPeer returns true if the given peer is a peer that the
// PersistentPeerManager manages.
func (m *PersistentPeerManager) IsPersistentPeer(pubKey *btcec.PublicKey) bool {
	m.RLock()
	defer m.RUnlock()

	_, ok := m.conns[route.NewVertex(pubKey)]
	return ok
}

// IsNonPermPersistentPeer returns true if the peer is a persistent peer but
// has been marked as non-permanent.
func (m *PersistentPeerManager) IsNonPermPersistentPeer(
	pubKey *btcec.PublicKey) bool {

	m.RLock()
	defer m.RUnlock()

	peer, ok := m.conns[route.NewVertex(pubKey)]
	if !ok {
		return false
	}

	return !peer.perm
}

// DelPeer removes a peer from the list of persistent peers that the
// PersistentPeerManager will manage.
func (m *PersistentPeerManager) DelPeer(pubKey *btcec.PublicKey) {
	m.Lock()
	defer m.Unlock()

	m.cancelConnReqsUnsafe(pubKey, nil)

	delete(m.conns, route.NewVertex(pubKey))
}

// PersistentPeers returns the list of public keys of the peers it is currently
// keeping track of.
func (m *PersistentPeerManager) PersistentPeers() []*btcec.PublicKey {
	m.RLock()
	defer m.RUnlock()

	peers := make([]*btcec.PublicKey, 0, len(m.conns))
	for _, p := range m.conns {
		peers = append(peers, p.pubKey)
	}

	return peers
}

// NumPeerConnReqs returns the number of connection requests for the given peer.
func (m *PersistentPeerManager) NumPeerConnReqs(pubKey *btcec.PublicKey) int {
	m.RLock()
	defer m.RUnlock()

	peer, ok := m.conns[route.NewVertex(pubKey)]
	if !ok {
		return 0
	}

	return len(peer.connReqs)
}

// nextPeerBackoff computes the next backoff duration for a peer using
// exponential backoff. If no previous backoff was known, the default is
// returned.
func nextPeerBackoff(currentBackoff, minBackoff, maxBackoff time.Duration,
	startTime time.Time) time.Duration {

	// If the peer failed to start properly, we'll just use the previous
	// backoff to compute the subsequent randomized exponential backoff
	// duration. This will roughly double on average.
	if startTime.IsZero() {
		return computeNextBackoff(currentBackoff, maxBackoff)
	}

	// The peer succeeded in starting. If the connection didn't last long
	// enough to be considered stable, we'll continue to back off retries
	// with this peer.
	connDuration := time.Since(startTime)
	if connDuration < defaultStableConnDuration {
		return computeNextBackoff(currentBackoff, maxBackoff)
	}

	// The peer succeed in starting and this was stable peer, so we'll
	// reduce the timeout duration by the length of the connection after
	// applying randomized exponential backoff. We'll only apply this in the
	// case that:
	//   reb(curBackoff) - connDuration > cfg.MinBackoff
	relaxedBackoff := computeNextBackoff(currentBackoff, maxBackoff)
	relaxedBackoff -= connDuration

	if relaxedBackoff > maxBackoff {
		return relaxedBackoff
	}

	// Lastly, if reb(currBackoff) - connDuration <= cfg.MinBackoff, meaning
	// the stable connection lasted much longer than our previous backoff.
	// To reward such good behavior, we'll reconnect after the default
	// timeout.
	return minBackoff
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

// ConnectPeer uses all the stored addresses for a peer to attempt to connect
// to the peer. It creates connection requests if there are currently none for
// a given address, and it removes old connection requests if the associated
// address is no longer in the latest address list for the peer.
func (m *PersistentPeerManager) ConnectPeer(pubKey *btcec.PublicKey) {
	m.Lock()
	defer m.Unlock()

	peer, ok := m.conns[route.NewVertex(pubKey)]
	if !ok {
		peerLog.Debugf("Peer %x is not a persistent peer. Ignoring "+
			"connection attempt", pubKey.SerializeCompressed())
		return
	}

	if len(peer.addrs) == 0 {
		peerLog.Debugf("Ignoring connection attempt to peer %s "+
			"without any stored address",
			pubKey.SerializeCompressed())
		return
	}

	// Create an easy lookup map of the addresses we have stored for the
	// peer. We will remove entries from this map if we have existing
	// connection requests for the associated address and then any leftover
	// entries will indicate which addresses we should create new
	// connection requests for.
	addrMap := make(map[string]*lnwire.NetAddress)
	for key, addr := range peer.addrs {
		addrMap[key] = addr
	}

	// Go through each of the existing connection requests and
	// check if they correspond to the latest set of addresses. If there is
	// a connection requests that does not use one of the latest advertised
	// addresses then remove that connection request.
	var updatedConnReqs []*connmgr.ConnReq
	for _, connReq := range peer.connReqs {
		lnAddr := connReq.Addr.(*lnwire.NetAddress).String()

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
			peerLog.Info(
				"Removing conn req:", connReq.Addr.String(),
			)
			m.cfg.ConnMgr.Remove(connReq.ID())
		}
	}

	peer.connReqs = updatedConnReqs
	cancelChan := peer.getRetryCanceller()

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

			m.Lock()
			peer.connReqs = append(peer.connReqs, connReq)
			m.Unlock()

			peerLog.Debugf("Attempting persistent connection to "+
				"channel peer %v", addr)

			go m.cfg.ConnMgr.Connect(connReq)

			select {
			case <-m.quit:
				return
			case <-cancelChan:
				return
			case <-ticker.C:
			}
		}
	}()
}

// UnassignedConnID is the default connection ID that a request can have before
// it actually is submitted to the connmgr.
// TODO(conner): move into connmgr package, or better, add connmgr method for
// generating atomic IDs.
const UnassignedConnID uint64 = 0

// CancelConnReqs stops all persistent connection requests for a given pubKey.
// Any retry canceller channel is canceled first. Afterwards, each connection
// request is removed from the connmgr. The caller can optionally specify a
// connection ID to ignore, which prevents us from canceling a successful
// request. All persistent connReqs for the provided pubKey are discarded after
// the operation.
func (m *PersistentPeerManager) CancelConnReqs(pubKey *btcec.PublicKey,
	skip *uint64) {

	m.Lock()
	defer m.Unlock()

	m.cancelConnReqsUnsafe(pubKey, skip)
}

// cancelConnReqsUnsafe cancels all the connection requests for a peer with the
// given pubKey. An optional skip ID can be specified if any connReqs should be
// ignored. This function will also cancel the retry canceler of the peer.
// NOTE: that this is the method is not thread safe and should only be called if
// the PersistentPeerManager mutex lock is held.
func (m *PersistentPeerManager) cancelConnReqsUnsafe(pubKey *btcec.PublicKey,
	skip *uint64) {

	peer, ok := m.conns[route.NewVertex(pubKey)]
	if !ok {
		return
	}

	// First, cancel any lingering persistent retry attempts, which will
	// prevent retries for any with backoffs that are still maturing.
	peer.cancelRetries()

	// Next, check to see if we have any outstanding persistent connection
	// requests to this peer. If so, then we'll remove all of these
	// connection requests, and also delete the entry from the map.
	if len(peer.connReqs) == 0 {
		return
	}

	for _, connReq := range peer.connReqs {
		peerLog.Tracef("Canceling %s:", connReq)

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

		m.cfg.ConnMgr.Remove(connID)
	}

	peer.connReqs = nil
}

// processSingeNodeUpdate processes a single network node update. It updates
// our persistent peer addresses if the node update is relevant. It returns
// true if we should attempt to update our connection to this node.
func (m *PersistentPeerManager) processSingleNodeUpdate(
	update *routing.NetworkNodeUpdate) bool {

	m.Lock()
	defer m.Unlock()

	// We only care about updates from the persistent peers that we
	// are keeping track of.
	peerKey := route.NewVertex(update.IdentityKey)
	peer, ok := m.conns[peerKey]
	if !ok {
		return false
	}

	m.setPeerAddrsUnsafe(peerKey, update.Addresses)

	// If there are no outstanding connection requests for this peer then
	// our work is done since we are not currently trying to connect to
	// them.
	return len(peer.connReqs) != 0
}

// setPeerAddrsUnsafe can be used to set the addresses of a peer. It only adds
// supported addresses.
// NOTE: that this is the method is not thread safe and should only be called
// if the PersistentPeerManager mutex lock is held.
func (m *PersistentPeerManager) setPeerAddrsUnsafe(peerKey route.Vertex,
	addrs []net.Addr) {

	peer := m.conns[peerKey]

	peer.addrs = make(map[string]*lnwire.NetAddress)
	for _, addr := range addrs {
		if !m.cfg.AddrTypeIsSupported(addr) {
			continue
		}

		lnAddr := &lnwire.NetAddress{
			IdentityKey: peer.pubKey,
			Address:     addr,
			ChainNet:    m.cfg.ChainNet,
		}

		peer.addrs[lnAddr.String()] = lnAddr
	}
}

// ConnectPeerWithBackoff starts a connection attempt to the given peer after
// the peer's backoff time has passed.
func (m *PersistentPeerManager) ConnectPeerWithBackoff(pubKey *btcec.PublicKey,
	startTime time.Time) {

	m.Lock()
	defer m.Unlock()

	peer, ok := m.conns[route.NewVertex(pubKey)]
	if !ok {
		return
	}

	backoff := nextPeerBackoff(
		peer.backoff, m.cfg.MinBackoff, m.cfg.MaxBackoff, startTime,
	)
	peer.backoff = backoff

	// Initialize a retry canceller for this peer if one does not
	// exist.
	cancelChan := peer.getRetryCanceller()

	// We choose not to wait group this go routine since the Connect call
	// can stall for arbitrarily long if we shutdown while an outbound
	// connection attempt is being made.
	go func() {
		peerLog.Debugf("Scheduling connection re-establishment to "+
			"persistent peer %x in %s",
			pubKey)

		select {
		case <-time.After(backoff):
		case <-cancelChan:
			return
		case <-m.quit:
			return
		}

		peerLog.Debugf("Attempting to re-establish persistent "+
			"connection to peer %x", pubKey)

		m.ConnectPeer(pubKey)
	}()
}

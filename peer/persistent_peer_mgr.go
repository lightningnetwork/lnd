package peer

import (
	"crypto/rand"
	"math/big"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/routing"

	"github.com/lightningnetwork/lnd/lnwire"

	"github.com/btcsuite/btcd/connmgr"
)

const (
	// defaultStableConnDuration is a floor under which all reconnection
	// attempts will apply exponential randomized backoff. Connections
	// durations exceeding this value will be eligible to have their
	// backoffs reduced.
	defaultStableConnDuration = 10 * time.Minute
)

// PersistentPeerManager manages persistent peers and the active connection
// requests we may have to them.
type PersistentPeerManager struct {
	// connMgr is used to manage the creation and removal of connection
	// requests. It handles the actual connection to a peer.
	connMgr connMgr

	// conns maps a peer's public key string to a persistentPeer object.
	conns map[string]*persistentPeer

	// connsMu is used to guard the conns map.
	connsMu sync.RWMutex

	// subscribeTopology will be used to listen for updates to a persistent
	// peer's advertised addresses.
	subscribeTopology func() (*routing.TopologyClient, error)

	minBackoff time.Duration
	maxBackoff time.Duration

	wg   sync.WaitGroup
	quit chan struct{}
}

// persistentPeer holds the addresses we stored for a persistent peer along
// with any active connection requests that we have for them. A persistentPeer
// is permanent if we want to maintain a connection to them even when we have
// no open channels with them.
type persistentPeer struct {
	// addrs is all the addresses we know about for this peer.
	addrs map[string]*lnwire.NetAddress

	// connReqs holds all the active connection requests that we have
	// for the peer.
	connReqs []*connmgr.ConnReq

	// perm indicates if we should maintain a connection with a peer even
	// if we have no channels with the peer.
	perm bool

	// backoff is the time that we should wait before trying to reconnect
	// to a peer.
	backoff time.Duration

	// retryCanceller is used to cancel any retry attempts with backoffs
	// that are still maturing.
	retryCanceller chan struct{}
}

// connMgr is what the PersistentPeerManager will use to create and remove
// connection requests. The purpose of this interface is to make testing easier.
type connMgr interface {
	Connect(c *connmgr.ConnReq)
	Remove(id uint64)
}

// NewPersistentPeerManager creates a new PersistentPeerManager instance.
func NewPersistentPeerManager(manager connMgr,
	subscribeUpdates func() (*routing.TopologyClient, error), minBackoff,
	maxBackoff time.Duration) *PersistentPeerManager {

	return &PersistentPeerManager{
		connMgr:           manager,
		subscribeTopology: subscribeUpdates,
		minBackoff:        minBackoff,
		maxBackoff:        maxBackoff,
		conns:             make(map[string]*persistentPeer),
		quit:              make(chan struct{}),
	}
}

// Start begins the processes of PersistentPeerManager. It subscribes to
// graph updates and listens for any NodeAnnouncement messages that indicate
// that the addresses of one of the persistent peers has changed and then
// updates the peer's addresses and connection requests accordingly.
func (m *PersistentPeerManager) Start() error {
	graphSub, err := m.subscribeTopology()
	if err != nil {
		return err
	}

	m.wg.Add(1)
	go func() {
		defer graphSub.Cancel()
		defer m.wg.Done()

		for {
			select {
			case topChange, ok := <-graphSub.TopologyChanges:
				// If the router is shutting down, then we will
				// as well.
				if !ok {
					return
				}

				for _, update := range topChange.NodeUpdates {
					pubKeyStr := string(update.IdentityKey.
						SerializeCompressed())

					m.connsMu.RLock()
					_, ok := m.conns[pubKeyStr]
					m.connsMu.RUnlock()
					if !ok {
						continue
					}

					lnAddrs := make([]*lnwire.NetAddress, 0,
						len(update.Addresses))

					for _, addr := range update.Addresses {
						lnAddrs = append(lnAddrs,
							&lnwire.NetAddress{
								IdentityKey: update.IdentityKey,
								Address:     addr,
							})
					}

					m.updatePeerConn(pubKeyStr, lnAddrs)
				}

			case <-m.quit:
				return
			}
		}
	}()

	return nil
}

// Stop closes the quit channel of the PersistentPeerManager and waits for
// all goroutines to exit.
func (m *PersistentPeerManager) Stop() {
	close(m.quit)
	m.wg.Wait()
}

// updatePeerConn updates the stored addresses we have for a persistent peer
// to the new address set if the set has changed. It also checks to see if
// there are any outstanding connection requests to the peer and if so, it
// updates those to reflect the new address set.
func (m *PersistentPeerManager) updatePeerConn(pubKeyStr string,
	addrs []*lnwire.NetAddress) {

	m.connsMu.Lock()
	defer m.connsMu.Unlock()

	// Exit if this is not a persistent peer.
	peer, ok := m.conns[pubKeyStr]
	if !ok {
		return
	}

	// Create an easy lookup map of the new addresses. We will remove
	// entries from this map if we have existing connection requests for
	// the associated address and then any leftover entries will indicate
	// which addresses we should create new connection requests for.
	addrMap := make(map[string]*lnwire.NetAddress)
	for _, addr := range addrs {
		addrMap[addr.String()] = addr
	}

	// Update the stored addresses to reflect the new set.
	peer.addrs = addrMap

	// If there are no outstanding connection requests for this peer then
	// our work is done.
	if len(peer.connReqs) == 0 {
		return
	}

	// Otherwise, go through each of the existing connection requests and
	// check if they correspond to the latest advertised addresses. If
	// there is a connection requests that does not use one of the latest
	// advertised addresses then remove that connection request.
	var updatedConnReqs []*connmgr.ConnReq
	for _, connReq := range peer.connReqs {
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
			peerLog.Info(
				"Removing conn req:", connReq.Addr.String(),
			)
			m.connMgr.Remove(connReq.ID())
		}
	}

	peer.connReqs = updatedConnReqs

	// Any addresses left in addrMap are new ones that we have not made
	// connection requests for. So create new connection requests for those.
	for _, addr := range addrs {
		if _, ok := addrMap[addr.String()]; !ok {
			continue
		}

		connReq := &connmgr.ConnReq{
			Addr:      addr,
			Permanent: true,
		}

		peer.connReqs = append(peer.connReqs, connReq)
		go m.connMgr.Connect(connReq)
	}
}

// AddPeer adds a new persistent peer for which address updates will be kept
// track of. The peer can be initialised with an initial set of addresses.
func (m *PersistentPeerManager) AddPeer(pubKeyStr string,
	addrs []*lnwire.NetAddress, perm bool) {

	m.connsMu.Lock()
	defer m.connsMu.Unlock()

	addrMap := make(map[string]*lnwire.NetAddress)
	for _, addr := range addrs {
		addrMap[addr.String()] = addr
	}

	backoff := m.minBackoff
	peer, ok := m.conns[pubKeyStr]
	if ok {
		backoff = peer.backoff
	}

	m.conns[pubKeyStr] = &persistentPeer{
		addrs:   addrMap,
		perm:    perm,
		backoff: backoff,
	}
}

// DelPeer removes a peer from the list of persistent peers that the
// PersistentPeerManager will manage.
func (m *PersistentPeerManager) DelPeer(pubKeyStr string) {
	m.connsMu.Lock()
	defer m.connsMu.Unlock()

	delete(m.conns, pubKeyStr)
}

// IsPersistentPeer returns true if the given peer is a persistent peer that the
// PersistentPeerManager manages.
func (m *PersistentPeerManager) IsPersistentPeer(pubKeyStr string) bool {
	m.connsMu.RLock()
	defer m.connsMu.RUnlock()

	_, ok := m.conns[pubKeyStr]
	return ok
}

// IsPermPeer returns true if a connection should be kept with the given peer
// even in the case when we have no channels with the peer.
func (m *PersistentPeerManager) IsPermPeer(pubKeyStr string) bool {
	m.connsMu.RLock()
	defer m.connsMu.RUnlock()

	peer, ok := m.conns[pubKeyStr]
	if !ok {
		return false
	}

	return peer.perm
}

// NumConnReq the number of active connection requests for a peer.
func (m *PersistentPeerManager) NumConnReq(pubKeyStr string) int {
	m.connsMu.RLock()
	defer m.connsMu.RUnlock()

	peer, ok := m.conns[pubKeyStr]
	if !ok {
		return 0
	}

	return len(peer.connReqs)
}

// ConnectPeer adds the peer if it hasn't been added yet and initialises
// connection requests to this peer using all the stored addresses for the peer.
func (m *PersistentPeerManager) ConnectPeer(pubKeyStr string) {
	m.connsMu.Lock()
	defer m.connsMu.Unlock()

	peer, ok := m.conns[pubKeyStr]
	if !ok {
		peerLog.Debugf(
			"Peer %x is not a persistent peer."+
				"Ignoring connection attempt", pubKeyStr,
		)
		return
	}

	if len(peer.addrs) == 0 {
		peerLog.Debugf("Ignoring connection attempt "+
			"to peer %x without any stored address", pubKeyStr)
		return
	}

	for _, addr := range peer.addrs {
		connReq := &connmgr.ConnReq{
			Addr:      addr,
			Permanent: true,
		}

		peer.connReqs = append(peer.connReqs, connReq)

		peerLog.Debugf("Attempting persistent connection to "+
			"channel peer %v", addr)

		go m.connMgr.Connect(connReq)
	}
}

// ConnectPeerWithBackoff starts a connection attempt to the given peer after
// the peer's backoff time has passed.
func (m *PersistentPeerManager) ConnectPeerWithBackoff(pubKeyStr string,
	startTime time.Time) {

	m.connsMu.Lock()
	defer m.connsMu.Unlock()

	peer, ok := m.conns[pubKeyStr]
	if !ok {
		peerLog.Debugf(
			"Peer %x is not a persistent peer."+
				"Ignoring connection attempt", pubKeyStr,
		)
		return
	}

	// Initialize a retry canceller for this peer if one does not exist.
	var cancelChan chan struct{}
	if peer.retryCanceller == nil {
		cancelChan = make(chan struct{})
		peer.retryCanceller = cancelChan
	} else {
		cancelChan = peer.retryCanceller
	}

	peer.backoff = m.nextBackoff(peer.backoff, startTime)

	// We choose not to wait group this go routine since the Connect call
	// can stall for arbitrarily long if we shutdown while an outbound
	// connection attempt is being made.
	go func() {
		peerLog.Debugf("Scheduling connection "+
			"re-establishment to persistent peer %x in %s",
			pubKeyStr, peer.backoff)

		select {
		case <-time.After(peer.backoff):
		case <-cancelChan:
			return
		case <-m.quit:
			return
		}

		peerLog.Debugf("Attempting to re-establish persistent "+
			"connection to peer %x", pubKeyStr)

		m.ConnectPeer(pubKeyStr)
	}()
}

// UnassignedConnID is the default connection ID that a request can have before
// it actually is submitted to the connmgr.
// TODO(conner): move into connmgr package, or better, add connmgr method for
// generating atomic IDs
const UnassignedConnID uint64 = 0

// RemovePeerConns removes any connection requests that are active for the
// given peer. An optional skip id  can be provided to exclude removing the
// successful connection request.
func (m *PersistentPeerManager) RemovePeerConns(pubKeyStr string,
	skip *uint64) {

	m.connsMu.Lock()
	defer m.connsMu.Unlock()

	peer, ok := m.conns[pubKeyStr]
	if !ok {
		return
	}

	// First, cancel any lingering persistent retry attempts, which will
	// prevent retries for any with backoffs that are still maturing.
	select {
	case _, ok := <-peer.retryCanceller:
		if !ok {
			break
		}
		close(peer.retryCanceller)
	default:
	}

	// Next, check to see if we have any outstanding persistent connection
	// requests to this peer. If so, then we'll remove all of these
	// connection requests.
	for _, cr := range peer.connReqs {
		// Atomically capture the current request identifier.
		connID := cr.ID()

		// Skip any zero IDs, this indicates the request has not
		// yet been schedule.
		if connID == UnassignedConnID {
			continue
		}

		// Skip a particular connection ID if instructed.
		if skip != nil && connID == *skip {
			continue
		}

		m.connMgr.Remove(cr.ID())
	}

	peer.connReqs = nil
}

// nextBackoff computes the next backoff duration for a peer's pubkey using
// exponential backoff. If no previous backoff was known, the default is
// returned.
func (m *PersistentPeerManager) nextBackoff(backoff time.Duration,
	startTime time.Time) time.Duration {

	// Now, determine the appropriate backoff to use for the retry.
	if backoff < m.minBackoff {
		// If an existing backoff was unknown, use the default.
		return m.minBackoff
	}

	// If the peer failed to start properly, we'll just use the previous
	// backoff to compute the subsequent randomized exponential backoff
	// duration. This will roughly double on average.
	if startTime.IsZero() {
		return computeNextBackoff(backoff, m.maxBackoff)
	}

	// The peer succeeded in starting. If the connection didn't last long
	// enough to be considered stable, we'll continue to back off retries
	// with this peer.
	connDuration := time.Since(startTime)
	if connDuration < defaultStableConnDuration {
		return computeNextBackoff(backoff, m.maxBackoff)
	}

	// The peer succeed in starting and this was stable peer, so we'll
	// reduce the timeout duration by the length of the connection after
	// applying randomized exponential backoff. We'll only apply this in the
	// case that:
	//   reb(curBackoff) - connDuration > m.minBackoff
	relaxedBackoff := computeNextBackoff(backoff, m.maxBackoff) - connDuration
	if relaxedBackoff > m.minBackoff {
		return relaxedBackoff
	}

	// Lastly, if reb(currBackoff) - connDuration <= m.minBackoff, meaning
	// the stable connection lasted much longer than our previous backoff.
	// To reward such good behavior, we'll reconnect after the default
	// timeout.
	return m.minBackoff
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
	if _, err := rand.Int(rand.Reader, &wiggle); err != nil { // nolint: gosec
		// Randomizing is not mission critical, so we'll just return the
		// current backoff.
		return nextBackoff
	}

	// Otherwise add in our wiggle, but subtract out half of the margin so
	// that the backoff can tweaked by 1/20 in either direction.
	return nextBackoff + (time.Duration(wiggle.Uint64()) - margin/2)
}

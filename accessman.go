package lnd

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/channeldb"
)

// accessMan is responsible for managing the server's access permissions.
type accessMan struct {
	cfg *accessManConfig

	// banScoreMtx is used for the server's ban tracking. If the server
	// mutex is also going to be locked, ensure that this is locked after
	// the server mutex.
	banScoreMtx sync.RWMutex

	// peerCounts is a mapping from remote public key to {bool, uint64}
	// where the bool indicates that we have an open/closed channel with
	// the peer and where the uint64 indicates the number of pending-open
	// channels we currently have with them. This mapping will be used to
	// determine access permissions for the peer. The map key is the
	// string-version of the serialized public key.
	//
	// NOTE: This MUST be accessed with the banScoreMtx held.
	peerCounts map[string]channeldb.ChanCount

	// peerScores stores each connected peer's access status. The map key
	// is the string-version of the serialized public key.
	//
	// NOTE: This MUST be accessed with the banScoreMtx held.
	peerScores map[string]peerSlotStatus

	// numRestricted tracks the number of peers with restricted access in
	// peerScores. This MUST be accessed with the banScoreMtx held.
	numRestricted uint64
}

type accessManConfig struct {
	// initAccessPerms checks the channeldb for initial access permissions
	// and then populates the peerCounts and peerScores maps.
	initAccessPerms func() (map[string]channeldb.ChanCount, error)

	// shouldDisconnect determines whether we should disconnect a peer or
	// not.
	shouldDisconnect func(*btcec.PublicKey) (bool, error)

	// maxRestrictedSlots is the number of restricted slots we'll allocate.
	maxRestrictedSlots uint64
}

func newAccessMan(cfg *accessManConfig) (*accessMan, error) {
	a := &accessMan{
		cfg:        cfg,
		peerCounts: make(map[string]channeldb.ChanCount),
		peerScores: make(map[string]peerSlotStatus),
	}

	counts, err := a.cfg.initAccessPerms()
	if err != nil {
		return nil, err
	}

	// We'll populate the server's peerCounts map with the counts fetched
	// via initAccessPerms. Also note that we haven't yet connected to the
	// peers.
	for peerPub, count := range counts {
		a.peerCounts[peerPub] = count
	}

	return a, nil
}

// assignPeerPerms assigns a new peer its permissions. This does not track the
// access in the maps. This is intentional.
func (a *accessMan) assignPeerPerms(remotePub *btcec.PublicKey) (
	peerAccessStatus, error) {

	// Default is restricted unless the below filters say otherwise.
	access := peerStatusRestricted

	shouldDisconnect, err := a.cfg.shouldDisconnect(remotePub)
	if err != nil {
		// Access is restricted here.
		return access, err
	}

	if shouldDisconnect {
		// Access is restricted here.
		return access, ErrGossiperBan
	}

	peerMapKey := string(remotePub.SerializeCompressed())

	// Lock banScoreMtx for reading so that we can update the banning maps
	// below.
	a.banScoreMtx.RLock()
	defer a.banScoreMtx.RUnlock()

	if count, found := a.peerCounts[peerMapKey]; found {
		if count.HasOpenOrClosedChan {
			access = peerStatusProtected
		} else if count.PendingOpenCount != 0 {
			access = peerStatusTemporary
		}
	}

	// If we've reached this point and access hasn't changed from
	// restricted, then we need to check if we even have a slot for this
	// peer.
	if a.numRestricted >= a.cfg.maxRestrictedSlots &&
		access == peerStatusRestricted {

		return access, ErrNoMoreRestrictedAccessSlots
	}

	return access, nil
}

// newPendingOpenChan is called after the pending-open channel has been
// committed to the database. This may transition a restricted-access peer to a
// temporary-access peer.
func (a *accessMan) newPendingOpenChan(remotePub *btcec.PublicKey) error {
	a.banScoreMtx.Lock()
	defer a.banScoreMtx.Unlock()

	peerMapKey := string(remotePub.SerializeCompressed())

	// Fetch the peer's access status from peerScores.
	status, found := a.peerScores[peerMapKey]
	if !found {
		// If we didn't find the peer, we'll return an error.
		return ErrNoPeerScore
	}

	switch status.state {
	case peerStatusProtected:
		// If this peer's access status is protected, we don't need to
		// do anything.
		return nil

	case peerStatusTemporary:
		// If this peer's access status is temporary, we'll need to
		// update the peerCounts map. The peer's access status will
		// stay temporary.
		peerCount, found := a.peerCounts[peerMapKey]
		if !found {
			// Error if we did not find any info in peerCounts.
			return ErrNoPendingPeerInfo
		}

		// Increment the pending channel amount.
		peerCount.PendingOpenCount += 1
		a.peerCounts[peerMapKey] = peerCount

	case peerStatusRestricted:
		// If the peer's access status is restricted, then we can
		// transition it to a temporary-access peer. We'll need to
		// update numRestricted and also peerScores. We'll also need to
		// update peerCounts.
		peerCount := channeldb.ChanCount{
			HasOpenOrClosedChan: false,
			PendingOpenCount:    1,
		}

		a.peerCounts[peerMapKey] = peerCount

		// A restricted-access slot has opened up.
		a.numRestricted -= 1

		a.peerScores[peerMapKey] = peerSlotStatus{
			state: peerStatusTemporary,
		}

	default:
		// This should not be possible.
		return fmt.Errorf("invalid peer access status")
	}

	return nil
}

// newPendingCloseChan is called when a pending-open channel prematurely closes
// before the funding transaction has confirmed. This potentially demotes a
// temporary-access peer to a restricted-access peer. If no restricted-access
// slots are available, the peer will be disconnected.
func (a *accessMan) newPendingCloseChan(remotePub *btcec.PublicKey) error {
	a.banScoreMtx.Lock()
	defer a.banScoreMtx.Unlock()

	peerMapKey := string(remotePub.SerializeCompressed())

	// Fetch the peer's access status from peerScores.
	status, found := a.peerScores[peerMapKey]
	if !found {
		return ErrNoPeerScore
	}

	switch status.state {
	case peerStatusProtected:
		// If this peer is protected, we don't do anything.
		return nil

	case peerStatusTemporary:
		// If this peer is temporary, we need to check if it will
		// revert to a restricted-access peer.
		peerCount, found := a.peerCounts[peerMapKey]
		if !found {
			// Error if we did not find any info in peerCounts.
			return ErrNoPendingPeerInfo
		}

		currentNumPending := peerCount.PendingOpenCount - 1
		if currentNumPending == 0 {
			// Remove the entry from peerCounts.
			delete(a.peerCounts, peerMapKey)

			// If this is the only pending-open channel for this
			// peer and it's getting removed, attempt to demote
			// this peer to a restricted peer.
			if a.numRestricted == a.cfg.maxRestrictedSlots {
				// There are no available restricted slots, so
				// we need to disconnect this peer. We leave
				// this up to the caller.
				return ErrNoMoreRestrictedAccessSlots
			}

			// Otherwise, there is an available restricted-access
			// slot, so we can demote this peer.
			a.peerScores[peerMapKey] = peerSlotStatus{
				state: peerStatusRestricted,
			}

			// Update numRestricted.
			a.numRestricted++

			return nil
		}

		// Else, we don't need to demote this peer since it has other
		// pending-open channels with us.
		peerCount.PendingOpenCount = currentNumPending
		a.peerCounts[peerMapKey] = peerCount

		return nil

	case peerStatusRestricted:
		// This should not be possible. This indicates an error.
		return fmt.Errorf("invalid peer access state transition")

	default:
		// This should not be possible.
		return fmt.Errorf("invalid peer access status")
	}
}

// newOpenChan is called when a pending-open channel becomes an open channel
// (i.e. the funding transaction has confirmed). If the remote peer is a
// temporary-access peer, it will be promoted to a protected-access peer.
func (a *accessMan) newOpenChan(remotePub *btcec.PublicKey) error {
	a.banScoreMtx.Lock()
	defer a.banScoreMtx.Unlock()

	peerMapKey := string(remotePub.SerializeCompressed())

	// Fetch the peer's access status from peerScores.
	status, found := a.peerScores[peerMapKey]
	if !found {
		// If we didn't find the peer, we'll return an error.
		return ErrNoPeerScore
	}

	switch status.state {
	case peerStatusProtected:
		// If the peer's state is already protected, we don't need to
		// do anything more.
		return nil

	case peerStatusTemporary:
		// If the peer's state is temporary, we'll upgrade the peer to
		// a protected peer.
		peerCount, found := a.peerCounts[peerMapKey]
		if !found {
			// Error if we did not find any info in peerCounts.
			return ErrNoPendingPeerInfo
		}

		peerCount.HasOpenOrClosedChan = true
		a.peerCounts[peerMapKey] = peerCount

		newStatus := peerSlotStatus{
			state: peerStatusProtected,
		}
		a.peerScores[peerMapKey] = newStatus

		return nil

	case peerStatusRestricted:
		// This should not be possible. For the server to receive a
		// state-transition event via NewOpenChan, the server must have
		// previously granted this peer "temporary" access. This
		// temporary access would not have been revoked or downgraded
		// without `CloseChannel` being called with the pending
		// argument set to true. This means that an open-channel state
		// transition would be impossible. Therefore, we can return an
		// error.
		return fmt.Errorf("invalid peer access status")

	default:
		// This should not be possible.
		return fmt.Errorf("invalid peer access status")
	}
}

// checkIncomingConnBanScore checks whether, given the remote's public hex-
// encoded key, we should not accept this incoming connection or immediately
// disconnect. This does not assign to the server's peerScores maps. This is
// just an inbound filter that the brontide listeners use.
func (a *accessMan) checkIncomingConnBanScore(remotePub *btcec.PublicKey) (
	bool, error) {

	a.banScoreMtx.RLock()
	defer a.banScoreMtx.RUnlock()

	peerMapKey := string(remotePub.SerializeCompressed())

	if _, found := a.peerCounts[peerMapKey]; !found {
		// Check numRestricted to see if there is an available slot. In
		// the future, it's possible to add better heuristics.
		if a.numRestricted < a.cfg.maxRestrictedSlots {
			// There is an available slot.
			return true, nil
		}

		// If there are no slots left, then we reject this connection.
		return false, ErrNoMoreRestrictedAccessSlots
	}

	// Else, the peer is either protected or temporary.
	return true, nil
}

// addPeerAccess tracks a peer's access in the maps. This should be called when
// the peer has fully connected.
func (a *accessMan) addPeerAccess(remotePub *btcec.PublicKey,
	access peerAccessStatus) {

	// Add the remote public key to peerScores.
	a.banScoreMtx.Lock()
	defer a.banScoreMtx.Unlock()

	peerMapKey := string(remotePub.SerializeCompressed())

	a.peerScores[peerMapKey] = peerSlotStatus{state: access}

	// Increment numRestricted.
	if access == peerStatusRestricted {
		a.numRestricted++
	}
}

// removePeerAccess removes the peer's access from the maps. This should be
// called when the peer has been disconnected.
func (a *accessMan) removePeerAccess(remotePub *btcec.PublicKey) {
	a.banScoreMtx.Lock()
	defer a.banScoreMtx.Unlock()

	peerMapKey := string(remotePub.SerializeCompressed())

	status, found := a.peerScores[peerMapKey]
	if !found {
		return
	}

	if status.state == peerStatusRestricted {
		// If the status is restricted, then we decrement from
		// numRestrictedSlots.
		a.numRestricted--
	}

	delete(a.peerScores, peerMapKey)
}

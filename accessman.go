package lnd

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnutils"
)

// accessMan is responsible for managing the server's access permissions.
type accessMan struct {
	cfg *accessManConfig

	// banScoreMtx is used for the server's ban tracking. If the server
	// mutex is also going to be locked, ensure that this is locked after
	// the server mutex.
	banScoreMtx sync.RWMutex

	// peerChanInfo is a mapping from remote public key to {bool, uint64}
	// where the bool indicates that we have an open/closed channel with the
	// peer and where the uint64 indicates the number of pending-open
	// channels we currently have with them. This mapping will be used to
	// determine access permissions for the peer. The map key is the
	// string-version of the serialized public key.
	//
	// NOTE: This MUST be accessed with the banScoreMtx held.
	peerChanInfo map[string]channeldb.ChanCount

	// peerScores stores each connected peer's access status. The map key
	// is the string-version of the serialized public key.
	//
	// NOTE: This MUST be accessed with the banScoreMtx held.
	//
	// TODO(yy): unify `peerScores` and `peerChanInfo` - there's no need to
	// create two maps tracking essentially the same info. `numRestricted`
	// can also be derived from `peerChanInfo`.
	peerScores map[string]peerSlotStatus

	// numRestricted tracks the number of peers with restricted access in
	// peerScores. This MUST be accessed with the banScoreMtx held.
	numRestricted int64
}

type accessManConfig struct {
	// initAccessPerms checks the channeldb for initial access permissions
	// and then populates the peerChanInfo and peerScores maps.
	initAccessPerms func() (map[string]channeldb.ChanCount, error)

	// shouldDisconnect determines whether we should disconnect a peer or
	// not.
	shouldDisconnect func(*btcec.PublicKey) (bool, error)

	// maxRestrictedSlots is the number of restricted slots we'll allocate.
	maxRestrictedSlots int64
}

func newAccessMan(cfg *accessManConfig) (*accessMan, error) {
	a := &accessMan{
		cfg:          cfg,
		peerChanInfo: make(map[string]channeldb.ChanCount),
		peerScores:   make(map[string]peerSlotStatus),
	}

	counts, err := a.cfg.initAccessPerms()
	if err != nil {
		return nil, err
	}

	// We'll populate the server's peerChanInfo map with the counts fetched
	// via initAccessPerms. Also note that we haven't yet connected to the
	// peers.
	maps.Copy(a.peerChanInfo, counts)

	acsmLog.Info("Access Manager initialized")

	return a, nil
}

// hasPeer checks whether a given peer already exists in the internal maps.
func (a *accessMan) hasPeer(ctx context.Context,
	pub string) (peerAccessStatus, bool) {

	// Lock banScoreMtx for reading so that we can read the banning maps
	// below.
	a.banScoreMtx.RLock()
	defer a.banScoreMtx.RUnlock()

	count, found := a.peerChanInfo[pub]
	if found {
		if count.HasOpenOrClosedChan {
			acsmLog.DebugS(ctx, "Peer has open/closed channel, "+
				"assigning protected access")

			// Exit early if the peer is no longer restricted.
			return peerStatusProtected, true
		}

		if count.PendingOpenCount != 0 {
			acsmLog.DebugS(ctx, "Peer has pending channel(s), "+
				"assigning temporary access")

			// Exit early if the peer is no longer restricted.
			return peerStatusTemporary, true
		}

		return peerStatusRestricted, true
	}

	// Check if the peer is found in the scores map.
	status, found := a.peerScores[pub]
	if found {
		acsmLog.DebugS(ctx, "Peer already has access", "access",
			status.state)

		return status.state, true
	}

	return peerStatusRestricted, false
}

// assignPeerPerms assigns a new peer its permissions. This does not track the
// access in the maps. This is intentional.
func (a *accessMan) assignPeerPerms(remotePub *btcec.PublicKey) (
	peerAccessStatus, error) {

	ctx := btclog.WithCtx(
		context.TODO(), lnutils.LogPubKey("peer", remotePub),
	)

	peerMapKey := string(remotePub.SerializeCompressed())

	acsmLog.DebugS(ctx, "Assigning permissions")

	// Default is restricted unless the below filters say otherwise.
	access, peerExist := a.hasPeer(ctx, peerMapKey)

	// Exit early if the peer is not restricted.
	if access != peerStatusRestricted {
		return access, nil
	}

	// If we are here, it means the peer has peerStatusRestricted.
	//
	// Check whether this peer is banned.
	shouldDisconnect, err := a.cfg.shouldDisconnect(remotePub)
	if err != nil {
		acsmLog.ErrorS(ctx, "Error checking disconnect status", err)

		// Access is restricted here.
		return access, err
	}

	if shouldDisconnect {
		acsmLog.WarnS(ctx, "Peer is banned, assigning restricted access",
			ErrGossiperBan)

		// Access is restricted here.
		return access, ErrGossiperBan
	}

	// If we've reached this point and access hasn't changed from
	// restricted, then we need to check if we even have a slot for this
	// peer.
	acsmLog.DebugS(ctx, "Peer has no channels, assigning restricted access")

	// If this is an existing peer, there's no need to check for slot limit.
	if peerExist {
		acsmLog.DebugS(ctx, "Skipped slot check for existing peer")
		return access, nil
	}

	a.banScoreMtx.RLock()
	defer a.banScoreMtx.RUnlock()

	if a.numRestricted >= a.cfg.maxRestrictedSlots {
		acsmLog.WarnS(ctx, "No more restricted slots available, "+
			"denying peer", ErrNoMoreRestrictedAccessSlots,
			"num_restricted", a.numRestricted, "max_restricted",
			a.cfg.maxRestrictedSlots)

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

	ctx := btclog.WithCtx(
		context.TODO(), lnutils.LogPubKey("peer", remotePub),
	)

	acsmLog.DebugS(ctx, "Processing new pending open channel")

	peerMapKey := string(remotePub.SerializeCompressed())

	// Fetch the peer's access status from peerScores.
	status, found := a.peerScores[peerMapKey]
	if !found {
		acsmLog.ErrorS(ctx, "Peer score not found", ErrNoPeerScore)

		// If we didn't find the peer, we'll return an error.
		return ErrNoPeerScore
	}

	switch status.state {
	case peerStatusProtected:
		acsmLog.DebugS(ctx, "Peer already protected, no change")

		// If this peer's access status is protected, we don't need to
		// do anything.
		return nil

	case peerStatusTemporary:
		// If this peer's access status is temporary, we'll need to
		// update the peerChanInfo map. The peer's access status will
		// stay temporary.
		peerCount, found := a.peerChanInfo[peerMapKey]
		if !found {
			// Error if we did not find any info in peerChanInfo.
			acsmLog.ErrorS(ctx, "Pending peer info not found",
				ErrNoPendingPeerInfo)

			return ErrNoPendingPeerInfo
		}

		// Increment the pending channel amount.
		peerCount.PendingOpenCount += 1
		a.peerChanInfo[peerMapKey] = peerCount

		acsmLog.DebugS(ctx, "Peer is temporary, incremented "+
			"pending count",
			"pending_count", peerCount.PendingOpenCount)

	case peerStatusRestricted:
		// If the peer's access status is restricted, then we can
		// transition it to a temporary-access peer. We'll need to
		// update numRestricted and also peerScores. We'll also need to
		// update peerChanInfo.
		peerCount := channeldb.ChanCount{
			HasOpenOrClosedChan: false,
			PendingOpenCount:    1,
		}

		a.peerChanInfo[peerMapKey] = peerCount

		// A restricted-access slot has opened up.
		oldRestricted := a.numRestricted
		a.numRestricted -= 1

		a.peerScores[peerMapKey] = peerSlotStatus{
			state: peerStatusTemporary,
		}

		acsmLog.InfoS(ctx, "Peer transitioned restricted -> "+
			"temporary (pending open)",
			"old_restricted", oldRestricted,
			"new_restricted", a.numRestricted)

	default:
		// This should not be possible.
		err := fmt.Errorf("invalid peer access status %v for %x",
			status.state, peerMapKey)
		acsmLog.ErrorS(ctx, "Invalid peer access status", err)

		return err
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

	ctx := btclog.WithCtx(
		context.TODO(), lnutils.LogPubKey("peer", remotePub),
	)

	acsmLog.DebugS(ctx, "Processing pending channel close")

	peerMapKey := string(remotePub.SerializeCompressed())

	// Fetch the peer's access status from peerScores.
	status, found := a.peerScores[peerMapKey]
	if !found {
		acsmLog.ErrorS(ctx, "Peer score not found", ErrNoPeerScore)

		return ErrNoPeerScore
	}

	switch status.state {
	case peerStatusProtected:
		// If this peer is protected, we don't do anything.
		acsmLog.DebugS(ctx, "Peer is protected, no change")

		return nil

	case peerStatusTemporary:
		// If this peer is temporary, we need to check if it will
		// revert to a restricted-access peer.
		peerCount, found := a.peerChanInfo[peerMapKey]
		if !found {
			acsmLog.ErrorS(ctx, "Pending peer info not found",
				ErrNoPendingPeerInfo)

			// Error if we did not find any info in peerChanInfo.
			return ErrNoPendingPeerInfo
		}

		currentNumPending := peerCount.PendingOpenCount - 1

		acsmLog.DebugS(ctx, "Peer is temporary, decrementing "+
			"pending count",
			"pending_count", currentNumPending)

		if currentNumPending == 0 {
			// Remove the entry from peerChanInfo.
			delete(a.peerChanInfo, peerMapKey)

			// If this is the only pending-open channel for this
			// peer and it's getting removed, attempt to demote
			// this peer to a restricted peer.
			if a.numRestricted == a.cfg.maxRestrictedSlots {
				// There are no available restricted slots, so
				// we need to disconnect this peer. We leave
				// this up to the caller.
				acsmLog.WarnS(ctx, "Peer last pending "+
					"channel closed: ",
					ErrNoMoreRestrictedAccessSlots,
					"num_restricted", a.numRestricted,
					"max_restricted", a.cfg.maxRestrictedSlots)

				return ErrNoMoreRestrictedAccessSlots
			}

			// Otherwise, there is an available restricted-access
			// slot, so we can demote this peer.
			a.peerScores[peerMapKey] = peerSlotStatus{
				state: peerStatusRestricted,
			}

			// Update numRestricted.
			oldRestricted := a.numRestricted
			a.numRestricted++

			acsmLog.InfoS(ctx, "Peer transitioned "+
				"temporary -> restricted "+
				"(last pending closed)",
				"old_restricted", oldRestricted,
				"new_restricted", a.numRestricted)

			return nil
		}

		// Else, we don't need to demote this peer since it has other
		// pending-open channels with us.
		peerCount.PendingOpenCount = currentNumPending
		a.peerChanInfo[peerMapKey] = peerCount

		acsmLog.DebugS(ctx, "Peer still has other pending channels",
			"pending_count", currentNumPending)

		return nil

	case peerStatusRestricted:
		// This should not be possible. This indicates an error.
		err := fmt.Errorf("invalid peer access state transition: "+
			"pending close for restricted peer %x", peerMapKey)
		acsmLog.ErrorS(ctx, "Invalid peer access state transition", err)

		return err

	default:
		// This should not be possible.
		err := fmt.Errorf("invalid peer access status %v for %x",
			status.state, peerMapKey)
		acsmLog.ErrorS(ctx, "Invalid peer access status", err)

		return err
	}
}

// newOpenChan is called when a pending-open channel becomes an open channel
// (i.e. the funding transaction has confirmed). If the remote peer is a
// temporary-access peer, it will be promoted to a protected-access peer.
func (a *accessMan) newOpenChan(remotePub *btcec.PublicKey) error {
	a.banScoreMtx.Lock()
	defer a.banScoreMtx.Unlock()

	ctx := btclog.WithCtx(
		context.TODO(), lnutils.LogPubKey("peer", remotePub),
	)

	acsmLog.DebugS(ctx, "Processing new open channel")

	peerMapKey := string(remotePub.SerializeCompressed())

	// Fetch the peer's access status from peerScores.
	status, found := a.peerScores[peerMapKey]
	if !found {
		// If we didn't find the peer, we'll return an error.
		acsmLog.ErrorS(ctx, "Peer score not found", ErrNoPeerScore)

		return ErrNoPeerScore
	}

	switch status.state {
	case peerStatusProtected:
		acsmLog.DebugS(ctx, "Peer already protected, no change")

		// If the peer's state is already protected, we don't need to do
		// anything more.
		return nil

	case peerStatusTemporary:
		// If the peer's state is temporary, we'll upgrade the peer to
		// a protected peer.
		peerCount, found := a.peerChanInfo[peerMapKey]
		if !found {
			// Error if we did not find any info in peerChanInfo.
			acsmLog.ErrorS(ctx, "Pending peer info not found",
				ErrNoPendingPeerInfo)

			return ErrNoPendingPeerInfo
		}

		peerCount.HasOpenOrClosedChan = true
		peerCount.PendingOpenCount -= 1

		a.peerChanInfo[peerMapKey] = peerCount

		newStatus := peerSlotStatus{
			state: peerStatusProtected,
		}
		a.peerScores[peerMapKey] = newStatus

		acsmLog.InfoS(ctx, "Peer transitioned temporary -> "+
			"protected (channel opened)")

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
		err := fmt.Errorf("invalid peer access status: new open "+
			"channel for restricted peer %x", peerMapKey)

		acsmLog.ErrorS(ctx, "Invalid peer access status", err)

		return err

	default:
		// This should not be possible.
		err := fmt.Errorf("invalid peer access status %v for %x",
			status.state, peerMapKey)

		acsmLog.ErrorS(ctx, "Invalid peer access status", err)

		return err
	}
}

// checkAcceptIncomingConn checks whether, given the remote's public hex-
// encoded key, we should not accept this incoming connection or immediately
// disconnect. This does not assign to the server's peerScores maps. This is
// just an inbound filter that the brontide listeners use.
//
// TODO(yy): We should also consider removing this `checkAcceptIncomingConn`
// check as a) it doesn't check for ban score; and b) we should, and already
// have this check when we handle incoming connection in `InboundPeerConnected`.
func (a *accessMan) checkAcceptIncomingConn(remotePub *btcec.PublicKey) (
	bool, error) {

	ctx := btclog.WithCtx(
		context.TODO(), lnutils.LogPubKey("peer", remotePub),
	)

	peerMapKey := string(remotePub.SerializeCompressed())

	acsmLog.TraceS(ctx, "Checking incoming connection ban score")

	a.banScoreMtx.RLock()
	defer a.banScoreMtx.RUnlock()

	_, found := a.peerChanInfo[peerMapKey]

	// Exit early if found.
	if found {
		acsmLog.DebugS(ctx, "Peer found (protected/temporary), "+
			"accepting")

		return true, nil
	}

	_, found = a.peerScores[peerMapKey]

	// Exit early if found.
	if found {
		acsmLog.DebugS(ctx, "Found existing peer, accepting")

		return true, nil
	}

	acsmLog.DebugS(ctx, "Peer not found in counts, checking restricted "+
		"slots")

	// Check numRestricted to see if there is an available slot. In
	// the future, it's possible to add better heuristics.
	if a.numRestricted < a.cfg.maxRestrictedSlots {
		// There is an available slot.
		acsmLog.DebugS(ctx, "Restricted slot available, accepting ",
			"num_restricted", a.numRestricted, "max_restricted",
			a.cfg.maxRestrictedSlots)

		return true, nil
	}

	// If there are no slots left, then we reject this connection.
	acsmLog.WarnS(ctx, "No restricted slots available, rejecting ",
		ErrNoMoreRestrictedAccessSlots, "num_restricted",
		a.numRestricted, "max_restricted", a.cfg.maxRestrictedSlots)

	return false, ErrNoMoreRestrictedAccessSlots
}

// addPeerAccess tracks a peer's access in the maps. This should be called when
// the peer has fully connected.
func (a *accessMan) addPeerAccess(remotePub *btcec.PublicKey,
	access peerAccessStatus, inbound bool) {

	ctx := btclog.WithCtx(
		context.TODO(), lnutils.LogPubKey("peer", remotePub),
	)

	acsmLog.DebugS(ctx, "Adding peer access", "access", access)

	// Add the remote public key to peerScores.
	a.banScoreMtx.Lock()
	defer a.banScoreMtx.Unlock()

	peerMapKey := string(remotePub.SerializeCompressed())

	// Exit early if this is an existing peer, which means it won't take
	// another slot.
	_, found := a.peerScores[peerMapKey]
	if found {
		acsmLog.DebugS(ctx, "Skipped taking restricted slot for "+
			"existing peer")

		return
	}

	a.peerScores[peerMapKey] = peerSlotStatus{state: access}

	// Exit early if this is not a restricted peer.
	if access != peerStatusRestricted {
		acsmLog.DebugS(ctx, "Skipped taking restricted slot as peer "+
			"already has access", "access", access)

		return
	}

	// Increment numRestricted if this is an inbound connection.
	if inbound {
		oldRestricted := a.numRestricted
		a.numRestricted++

		acsmLog.DebugS(ctx, "Incremented restricted slots",
			"old_restricted", oldRestricted,
			"new_restricted", a.numRestricted)

		return
	}

	// Otherwise, this is a newly created outbound connection. We won't
	// place any restriction on it, instead, we will do a hot upgrade here
	// to move it from restricted to temporary.
	peerCount := channeldb.ChanCount{
		HasOpenOrClosedChan: false,
		PendingOpenCount:    0,
	}

	a.peerChanInfo[peerMapKey] = peerCount
	a.peerScores[peerMapKey] = peerSlotStatus{
		state: peerStatusTemporary,
	}

	acsmLog.InfoS(ctx, "Upgraded outbound peer: restricted -> temporary")
}

// removePeerAccess removes the peer's access from the maps. This should be
// called when the peer has been disconnected.
func (a *accessMan) removePeerAccess(ctx context.Context, peerPubKey string) {
	acsmLog.DebugS(ctx, "Removing access:")

	a.banScoreMtx.Lock()
	defer a.banScoreMtx.Unlock()

	status, found := a.peerScores[peerPubKey]
	if !found {
		acsmLog.InfoS(ctx, "Peer score not found during removal")
		return
	}

	if status.state == peerStatusRestricted {
		// If the status is restricted, then we decrement from
		// numRestrictedSlots.
		oldRestricted := a.numRestricted
		a.numRestricted--

		acsmLog.DebugS(ctx, "Decremented restricted slots",
			"old_restricted", oldRestricted,
			"new_restricted", a.numRestricted)
	}

	acsmLog.TraceS(ctx, "Deleting from peerScores:")

	delete(a.peerScores, peerPubKey)

	// We now check whether this peer has channels with us or not.
	info, found := a.peerChanInfo[peerPubKey]
	if !found {
		acsmLog.DebugS(ctx, "Chan info not found during removal:")
		return
	}

	// Exit early if the peer has channel(s) with us.
	if info.HasOpenOrClosedChan {
		acsmLog.DebugS(ctx, "Skipped removing peer with channels:")
		return
	}

	// Skip removing the peer if it has pending open/close with us.
	if info.PendingOpenCount != 0 {
		acsmLog.DebugS(ctx, "Skipped removing peer with pending "+
			"channels:")
		return
	}

	// Given this peer has no channels with us, we can now remove it.
	delete(a.peerChanInfo, peerPubKey)
	acsmLog.TraceS(ctx, "Removed peer from peerChanInfo:")
}

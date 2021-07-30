package contractcourt

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// minCommitPointPollTimeout is the minimum time we'll wait before
	// polling the database for a channel's commitpoint.
	minCommitPointPollTimeout = 1 * time.Second

	// maxCommitPointPollTimeout is the maximum time we'll wait before
	// polling the database for a channel's commitpoint.
	maxCommitPointPollTimeout = 10 * time.Minute
)

// LocalUnilateralCloseInfo encapsulates all the information we need to act on
// a local force close that gets confirmed.
type LocalUnilateralCloseInfo struct {
	*chainntnfs.SpendDetail
	*lnwallet.LocalForceCloseSummary
	*channeldb.ChannelCloseSummary

	// CommitSet is the set of known valid commitments at the time the
	// remote party's commitment hit the chain.
	CommitSet CommitSet
}

// CooperativeCloseInfo encapsulates all the information we need to act on a
// cooperative close that gets confirmed.
type CooperativeCloseInfo struct {
	*channeldb.ChannelCloseSummary
}

// RemoteUnilateralCloseInfo wraps the normal UnilateralCloseSummary to couple
// the CommitSet at the time of channel closure.
type RemoteUnilateralCloseInfo struct {
	*lnwallet.UnilateralCloseSummary

	// CommitSet is the set of known valid commitments at the time the
	// remote party's commitment hit the chain.
	CommitSet CommitSet
}

// CommitSet is a collection of the set of known valid commitments at a given
// instant. If ConfCommitKey is set, then the commitment identified by the
// HtlcSetKey has hit the chain. This struct will be used to examine all live
// HTLCs to determine if any additional actions need to be made based on the
// remote party's commitments.
type CommitSet struct {
	// ConfCommitKey if non-nil, identifies the commitment that was
	// confirmed in the chain.
	ConfCommitKey *HtlcSetKey

	// HtlcSets stores the set of all known active HTLC for each active
	// commitment at the time of channel closure.
	HtlcSets map[HtlcSetKey][]channeldb.HTLC
}

// IsEmpty returns true if there are no HTLCs at all within all commitments
// that are a part of this commitment diff.
func (c *CommitSet) IsEmpty() bool {
	if c == nil {
		return true
	}

	for _, htlcs := range c.HtlcSets {
		if len(htlcs) != 0 {
			return false
		}
	}

	return true
}

// toActiveHTLCSets returns the set of all active HTLCs across all commitment
// transactions.
func (c *CommitSet) toActiveHTLCSets() map[HtlcSetKey]htlcSet {
	htlcSets := make(map[HtlcSetKey]htlcSet)

	for htlcSetKey, htlcs := range c.HtlcSets {
		htlcSets[htlcSetKey] = newHtlcSet(htlcs)
	}

	return htlcSets
}

// ChainEventSubscription is a struct that houses a subscription to be notified
// for any on-chain events related to a channel. There are three types of
// possible on-chain events: a cooperative channel closure, a unilateral
// channel closure, and a channel breach. The fourth type: a force close is
// locally initiated, so we don't provide any event stream for said event.
type ChainEventSubscription struct {
	// ChanPoint is that channel that chain events will be dispatched for.
	ChanPoint wire.OutPoint

	// RemoteUnilateralClosure is a channel that will be sent upon in the
	// event that the remote party's commitment transaction is confirmed.
	RemoteUnilateralClosure chan *RemoteUnilateralCloseInfo

	// LocalUnilateralClosure is a channel that will be sent upon in the
	// event that our commitment transaction is confirmed.
	LocalUnilateralClosure chan *LocalUnilateralCloseInfo

	// CooperativeClosure is a signal that will be sent upon once a
	// cooperative channel closure has been detected confirmed.
	CooperativeClosure chan *CooperativeCloseInfo

	// ContractBreach is a channel that will be sent upon if we detect a
	// contract breach. The struct sent across the channel contains all the
	// material required to bring the cheating channel peer to justice.
	ContractBreach chan *lnwallet.BreachRetribution

	// Cancel cancels the subscription to the event stream for a particular
	// channel. This method should be called once the caller no longer needs to
	// be notified of any on-chain events for a particular channel.
	Cancel func()
}

// chainWatcherConfig encapsulates all the necessary functions and interfaces
// needed to watch and act on on-chain events for a particular channel.
type chainWatcherConfig struct {
	// chanState is a snapshot of the persistent state of the channel that
	// we're watching. In the event of an on-chain event, we'll query the
	// database to ensure that we act using the most up to date state.
	chanState *channeldb.OpenChannel

	// notifier is a reference to the channel notifier that we'll use to be
	// notified of output spends and when transactions are confirmed.
	notifier chainntnfs.ChainNotifier

	// signer is the main signer instances that will be responsible for
	// signing any HTLC and commitment transaction generated by the state
	// machine.
	signer input.Signer

	// contractBreach is a method that will be called by the watcher if it
	// detects that a contract breach transaction has been confirmed. A
	// callback should be passed that when called will mark the channel
	// pending close in the database. It will only return a non-nil error
	// when the breachArbiter has preserved the necessary breach info for
	// this channel point, and the callback has succeeded, meaning it is
	// safe to stop watching the channel.
	contractBreach func(*lnwallet.BreachRetribution, func() error) error

	// isOurAddr is a function that returns true if the passed address is
	// known to us.
	isOurAddr func(btcutil.Address) bool

	// extractStateNumHint extracts the encoded state hint using the passed
	// obfuscater. This is used by the chain watcher to identify which
	// state was broadcast and confirmed on-chain.
	extractStateNumHint func(*wire.MsgTx, [lnwallet.StateHintSize]byte) uint64
}

// chainWatcher is a system that's assigned to every active channel. The duty
// of this system is to watch the chain for spends of the channels chan point.
// If a spend is detected then with chain watcher will notify all subscribers
// that the channel has been closed, and also give them the materials necessary
// to sweep the funds of the channel on chain eventually.
type chainWatcher struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	quit chan struct{}
	wg   sync.WaitGroup

	cfg chainWatcherConfig

	// stateHintObfuscator is a 48-bit state hint that's used to obfuscate
	// the current state number on the commitment transactions.
	stateHintObfuscator [lnwallet.StateHintSize]byte

	// All the fields below are protected by this mutex.
	sync.Mutex

	// clientID is an ephemeral counter used to keep track of each
	// individual client subscription.
	clientID uint64

	// clientSubscriptions is a map that keeps track of all the active
	// client subscriptions for events related to this channel.
	clientSubscriptions map[uint64]*ChainEventSubscription
}

// newChainWatcher returns a new instance of a chainWatcher for a channel given
// the chan point to watch, and also a notifier instance that will allow us to
// detect on chain events.
func newChainWatcher(cfg chainWatcherConfig) (*chainWatcher, error) {
	// In order to be able to detect the nature of a potential channel
	// closure we'll need to reconstruct the state hint bytes used to
	// obfuscate the commitment state number encoded in the lock time and
	// sequence fields.
	var stateHint [lnwallet.StateHintSize]byte
	chanState := cfg.chanState
	if chanState.IsInitiator {
		stateHint = lnwallet.DeriveStateHintObfuscator(
			chanState.LocalChanCfg.PaymentBasePoint.PubKey,
			chanState.RemoteChanCfg.PaymentBasePoint.PubKey,
		)
	} else {
		stateHint = lnwallet.DeriveStateHintObfuscator(
			chanState.RemoteChanCfg.PaymentBasePoint.PubKey,
			chanState.LocalChanCfg.PaymentBasePoint.PubKey,
		)
	}

	return &chainWatcher{
		cfg:                 cfg,
		stateHintObfuscator: stateHint,
		quit:                make(chan struct{}),
		clientSubscriptions: make(map[uint64]*ChainEventSubscription),
	}, nil
}

// Start starts all goroutines that the chainWatcher needs to perform its
// duties.
func (c *chainWatcher) Start() error {
	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		return nil
	}

	chanState := c.cfg.chanState
	log.Debugf("Starting chain watcher for ChannelPoint(%v)",
		chanState.FundingOutpoint)

	// First, we'll register for a notification to be dispatched if the
	// funding output is spent.
	fundingOut := &chanState.FundingOutpoint

	// As a height hint, we'll try to use the opening height, but if the
	// channel isn't yet open, then we'll use the height it was broadcast
	// at.
	heightHint := c.cfg.chanState.ShortChanID().BlockHeight
	if heightHint == 0 {
		heightHint = chanState.FundingBroadcastHeight
	}

	localKey := chanState.LocalChanCfg.MultiSigKey.PubKey.SerializeCompressed()
	remoteKey := chanState.RemoteChanCfg.MultiSigKey.PubKey.SerializeCompressed()
	multiSigScript, err := input.GenMultiSigScript(
		localKey, remoteKey,
	)
	if err != nil {
		return err
	}
	pkScript, err := input.WitnessScriptHash(multiSigScript)
	if err != nil {
		return err
	}

	spendNtfn, err := c.cfg.notifier.RegisterSpendNtfn(
		fundingOut, pkScript, heightHint,
	)
	if err != nil {
		return err
	}

	// With the spend notification obtained, we'll now dispatch the
	// closeObserver which will properly react to any changes.
	c.wg.Add(1)
	go c.closeObserver(spendNtfn)

	return nil
}

// Stop signals the close observer to gracefully exit.
func (c *chainWatcher) Stop() error {
	if !atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		return nil
	}

	close(c.quit)

	c.wg.Wait()

	return nil
}

// SubscribeChannelEvents returns an active subscription to the set of channel
// events for the channel watched by this chain watcher. Once clients no longer
// require the subscription, they should call the Cancel() method to allow the
// watcher to regain those committed resources.
func (c *chainWatcher) SubscribeChannelEvents() *ChainEventSubscription {

	c.Lock()
	clientID := c.clientID
	c.clientID++
	c.Unlock()

	log.Debugf("New ChainEventSubscription(id=%v) for ChannelPoint(%v)",
		clientID, c.cfg.chanState.FundingOutpoint)

	sub := &ChainEventSubscription{
		ChanPoint:               c.cfg.chanState.FundingOutpoint,
		RemoteUnilateralClosure: make(chan *RemoteUnilateralCloseInfo, 1),
		LocalUnilateralClosure:  make(chan *LocalUnilateralCloseInfo, 1),
		CooperativeClosure:      make(chan *CooperativeCloseInfo, 1),
		ContractBreach:          make(chan *lnwallet.BreachRetribution, 1),
		Cancel: func() {
			c.Lock()
			delete(c.clientSubscriptions, clientID)
			c.Unlock()
		},
	}

	c.Lock()
	c.clientSubscriptions[clientID] = sub
	c.Unlock()

	return sub
}

// handleUnknownLocalState checks whether the passed spend _could_ be a local
// state that for some reason is unknown to us. This could be a state published
// by us before we lost state, which we will try to sweep. Or it could be one
// of our revoked states that somehow made it to the chain. If that's the case
// we cannot really hope that we'll be able to get our money back, but we'll
// try to sweep it anyway. If this is not an unknown local state, false is
// returned.
func (c *chainWatcher) handleUnknownLocalState(
	commitSpend *chainntnfs.SpendDetail, broadcastStateNum uint64,
	chainSet *chainSet) (bool, error) {

	// If the spend was a local commitment, at this point it must either be
	// a past state (we breached!) or a future state (we lost state!). In
	// either case, the only thing we can do is to attempt to sweep what is
	// there.

	// First, we'll re-derive our commitment point for this state since
	// this is what we use to randomize each of the keys for this state.
	commitSecret, err := c.cfg.chanState.RevocationProducer.AtIndex(
		broadcastStateNum,
	)
	if err != nil {
		return false, err
	}
	commitPoint := input.ComputeCommitmentPoint(commitSecret[:])

	// Now that we have the commit point, we'll derive the tweaked local
	// and remote keys for this state. We use our point as only we can
	// revoke our own commitment.
	commitKeyRing := lnwallet.DeriveCommitmentKeys(
		commitPoint, true, c.cfg.chanState.ChanType,
		&c.cfg.chanState.LocalChanCfg, &c.cfg.chanState.RemoteChanCfg,
	)

	// With the keys derived, we'll construct the remote script that'll be
	// present if they have a non-dust balance on the commitment.
	remoteScript, _, err := lnwallet.CommitScriptToRemote(
		c.cfg.chanState.ChanType, commitKeyRing.ToRemoteKey,
	)
	if err != nil {
		return false, err
	}

	// Next, we'll derive our script that includes the revocation base for
	// the remote party allowing them to claim this output before the CSV
	// delay if we breach.
	localScript, err := lnwallet.CommitScriptToSelf(
		commitKeyRing.ToLocalKey, commitKeyRing.RevocationKey,
		uint32(c.cfg.chanState.LocalChanCfg.CsvDelay),
	)
	if err != nil {
		return false, err
	}

	// With all our scripts assembled, we'll examine the outputs of the
	// commitment transaction to determine if this is a local force close
	// or not.
	ourCommit := false
	for _, output := range commitSpend.SpendingTx.TxOut {
		pkScript := output.PkScript

		switch {
		case bytes.Equal(localScript.PkScript, pkScript):
			ourCommit = true

		case bytes.Equal(remoteScript.PkScript, pkScript):
			ourCommit = true
		}
	}

	// If the script is not present, this cannot be our commit.
	if !ourCommit {
		return false, nil
	}

	log.Warnf("Detected local unilateral close of unknown state %v "+
		"(our state=%v)", broadcastStateNum,
		chainSet.localCommit.CommitHeight)

	// If this is our commitment transaction, then we try to act even
	// though we won't be able to sweep HTLCs.
	chainSet.commitSet.ConfCommitKey = &LocalHtlcSet
	if err := c.dispatchLocalForceClose(
		commitSpend, broadcastStateNum, chainSet.commitSet,
	); err != nil {
		return false, fmt.Errorf("unable to handle local"+
			"close for chan_point=%v: %v",
			c.cfg.chanState.FundingOutpoint, err)
	}

	return true, nil
}

// chainSet includes all the information we need to dispatch a channel close
// event to any subscribers.
type chainSet struct {
	// remoteStateNum is the commitment number of the lowest valid
	// commitment the remote party holds from our PoV. This value is used
	// to determine if the remote party is playing a state that's behind,
	// in line, or ahead of the latest state we know for it.
	remoteStateNum uint64

	// commitSet includes information pertaining to the set of active HTLCs
	// on each commitment.
	commitSet CommitSet

	// remoteCommit is the current commitment of the remote party.
	remoteCommit channeldb.ChannelCommitment

	// localCommit is our current commitment.
	localCommit channeldb.ChannelCommitment

	// remotePendingCommit points to the dangling commitment of the remote
	// party, if it exists. If there's no dangling commitment, then this
	// pointer will be nil.
	remotePendingCommit *channeldb.ChannelCommitment
}

// newChainSet creates a new chainSet given the current up to date channel
// state.
func newChainSet(chanState *channeldb.OpenChannel) (*chainSet, error) {
	// First, we'll grab the current unrevoked commitments for ourselves
	// and the remote party.
	localCommit, remoteCommit, err := chanState.LatestCommitments()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channel state for "+
			"chan_point=%v", chanState.FundingOutpoint)
	}

	log.Debugf("ChannelPoint(%v): local_commit_type=%v, local_commit=%v",
		chanState.FundingOutpoint, chanState.ChanType,
		spew.Sdump(localCommit))
	log.Debugf("ChannelPoint(%v): remote_commit_type=%v, remote_commit=%v",
		chanState.FundingOutpoint, chanState.ChanType,
		spew.Sdump(remoteCommit))

	// Fetch the current known commit height for the remote party, and
	// their pending commitment chain tip if it exists.
	remoteStateNum := remoteCommit.CommitHeight
	remoteChainTip, err := chanState.RemoteCommitChainTip()
	if err != nil && err != channeldb.ErrNoPendingCommit {
		return nil, fmt.Errorf("unable to obtain chain tip for "+
			"ChannelPoint(%v): %v",
			chanState.FundingOutpoint, err)
	}

	// Now that we have all the possible valid commitments, we'll make the
	// CommitSet the ChannelArbitrator will need in order to carry out its
	// duty.
	commitSet := CommitSet{
		HtlcSets: map[HtlcSetKey][]channeldb.HTLC{
			LocalHtlcSet:  localCommit.Htlcs,
			RemoteHtlcSet: remoteCommit.Htlcs,
		},
	}

	var remotePendingCommit *channeldb.ChannelCommitment
	if remoteChainTip != nil {
		remotePendingCommit = &remoteChainTip.Commitment
		log.Debugf("ChannelPoint(%v): remote_pending_commit_type=%v, "+
			"remote_pending_commit=%v", chanState.FundingOutpoint,
			chanState.ChanType,
			spew.Sdump(remoteChainTip.Commitment))

		htlcs := remoteChainTip.Commitment.Htlcs
		commitSet.HtlcSets[RemotePendingHtlcSet] = htlcs
	}

	// We'll now retrieve the latest state of the revocation store so we
	// can populate the revocation information within the channel state
	// object that we have.
	//
	// TODO(roasbeef): mutation is bad mkay
	_, err = chanState.RemoteRevocationStore()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch revocation state for "+
			"chan_point=%v", chanState.FundingOutpoint)
	}

	return &chainSet{
		remoteStateNum:      remoteStateNum,
		commitSet:           commitSet,
		localCommit:         *localCommit,
		remoteCommit:        *remoteCommit,
		remotePendingCommit: remotePendingCommit,
	}, nil
}

// closeObserver is a dedicated goroutine that will watch for any closes of the
// channel that it's watching on chain. In the event of an on-chain event, the
// close observer will assembled the proper materials required to claim the
// funds of the channel on-chain (if required), then dispatch these as
// notifications to all subscribers.
func (c *chainWatcher) closeObserver(spendNtfn *chainntnfs.SpendEvent) {
	defer c.wg.Done()

	log.Infof("Close observer for ChannelPoint(%v) active",
		c.cfg.chanState.FundingOutpoint)

	select {
	// We've detected a spend of the channel onchain! Depending on the type
	// of spend, we'll act accordingly, so we'll examine the spending
	// transaction to determine what we should do.
	//
	// TODO(Roasbeef): need to be able to ensure this only triggers
	// on confirmation, to ensure if multiple txns are broadcast, we
	// act on the one that's timestamped
	case commitSpend, ok := <-spendNtfn.Spend:
		// If the channel was closed, then this means that the notifier
		// exited, so we will as well.
		if !ok {
			return
		}

		// Otherwise, the remote party might have broadcast a prior
		// revoked state...!!!
		commitTxBroadcast := commitSpend.SpendingTx

		// First, we'll construct the chainset which includes all the
		// data we need to dispatch an event to our subscribers about
		// this possible channel close event.
		chainSet, err := newChainSet(c.cfg.chanState)
		if err != nil {
			log.Errorf("unable to create commit set: %v", err)
			return
		}

		// Decode the state hint encoded within the commitment
		// transaction to determine if this is a revoked state or not.
		obfuscator := c.stateHintObfuscator
		broadcastStateNum := c.cfg.extractStateNumHint(
			commitTxBroadcast, obfuscator,
		)

		// We'll go on to check whether it could be our own commitment
		// that was published and know is confirmed.
		ok, err = c.handleKnownLocalState(
			commitSpend, broadcastStateNum, chainSet,
		)
		if err != nil {
			log.Errorf("Unable to handle known local state: %v",
				err)
			return
		}

		if ok {
			return
		}

		// Now that we know it is neither a non-cooperative closure nor
		// a local close with the latest state, we check if it is the
		// remote that closed with any prior or current state.
		ok, err = c.handleKnownRemoteState(
			commitSpend, broadcastStateNum, chainSet,
		)
		if err != nil {
			log.Errorf("Unable to handle known remote state: %v",
				err)
			return
		}

		if ok {
			return
		}

		// Next, we'll check to see if this is a cooperative channel
		// closure or not. This is characterized by having an input
		// sequence number that's finalized. This won't happen with
		// regular commitment transactions due to the state hint
		// encoding scheme.
		if commitTxBroadcast.TxIn[0].Sequence == wire.MaxTxInSequenceNum {
			// TODO(roasbeef): rare but possible, need itest case
			// for
			err := c.dispatchCooperativeClose(commitSpend)
			if err != nil {
				log.Errorf("unable to handle co op close: %v", err)
			}
			return
		}

		log.Warnf("Unknown commitment broadcast for "+
			"ChannelPoint(%v) ", c.cfg.chanState.FundingOutpoint)

		// We'll try to recover as best as possible from losing state.
		// We first check if this was a local unknown state. This could
		// happen if we force close, then lose state or attempt
		// recovery before the commitment confirms.
		ok, err = c.handleUnknownLocalState(
			commitSpend, broadcastStateNum, chainSet,
		)
		if err != nil {
			log.Errorf("Unable to handle known local state: %v",
				err)
			return
		}

		if ok {
			return
		}

		// Since it was neither a known remote state, nor a local state
		// that was published, it most likely mean we lost state and
		// the remote node closed. In this case we must start the DLP
		// protocol in hope of getting our money back.
		ok, err = c.handleUnknownRemoteState(
			commitSpend, broadcastStateNum, chainSet,
		)
		if err != nil {
			log.Errorf("Unable to handle unknown remote state: %v",
				err)
			return
		}

		if ok {
			return
		}

		log.Warnf("Unable to handle spending tx %v of channel point %v",
			commitTxBroadcast.TxHash(), c.cfg.chanState.FundingOutpoint)
		return

	// The chainWatcher has been signalled to exit, so we'll do so now.
	case <-c.quit:
		return
	}
}

// handleKnownLocalState checks whether the passed spend is a local state that
// is known to us (the current state). If so we will act on this state using
// the passed chainSet. If this is not a known local state, false is returned.
func (c *chainWatcher) handleKnownLocalState(
	commitSpend *chainntnfs.SpendDetail, broadcastStateNum uint64,
	chainSet *chainSet) (bool, error) {

	// If the channel is recovered, we won't have a local commit to check
	// against, so immediately return.
	if c.cfg.chanState.HasChanStatus(channeldb.ChanStatusRestored) {
		return false, nil
	}

	commitTxBroadcast := commitSpend.SpendingTx
	commitHash := commitTxBroadcast.TxHash()

	// Check whether our latest local state hit the chain.
	if chainSet.localCommit.CommitTx.TxHash() != commitHash {
		return false, nil
	}

	chainSet.commitSet.ConfCommitKey = &LocalHtlcSet
	if err := c.dispatchLocalForceClose(
		commitSpend, broadcastStateNum, chainSet.commitSet,
	); err != nil {
		return false, fmt.Errorf("unable to handle local"+
			"close for chan_point=%v: %v",
			c.cfg.chanState.FundingOutpoint, err)
	}

	return true, nil
}

// handleKnownRemoteState checks whether the passed spend is a remote state
// that is known to us (a revoked, current or pending state). If so we will act
// on this state using the passed chainSet. If this is not a known remote
// state, false is returned.
func (c *chainWatcher) handleKnownRemoteState(
	commitSpend *chainntnfs.SpendDetail, broadcastStateNum uint64,
	chainSet *chainSet) (bool, error) {

	// If the channel is recovered, we won't have any remote commit to
	// check against, so imemdiately return.
	if c.cfg.chanState.HasChanStatus(channeldb.ChanStatusRestored) {
		return false, nil
	}

	commitTxBroadcast := commitSpend.SpendingTx
	commitHash := commitTxBroadcast.TxHash()
	spendHeight := uint32(commitSpend.SpendingHeight)

	switch {
	// If the spending transaction matches the current latest state, then
	// they've initiated a unilateral close. So we'll trigger the
	// unilateral close signal so subscribers can clean up the state as
	// necessary.
	case chainSet.remoteCommit.CommitTx.TxHash() == commitHash:
		log.Infof("Remote party broadcast base set, "+
			"commit_num=%v", chainSet.remoteStateNum)

		chainSet.commitSet.ConfCommitKey = &RemoteHtlcSet
		err := c.dispatchRemoteForceClose(
			commitSpend, chainSet.remoteCommit,
			chainSet.commitSet,
			c.cfg.chanState.RemoteCurrentRevocation,
		)
		if err != nil {
			return false, fmt.Errorf("unable to handle remote "+
				"close for chan_point=%v: %v",
				c.cfg.chanState.FundingOutpoint, err)
		}

		return true, nil

	// We'll also handle the case of the remote party broadcasting
	// their commitment transaction which is one height above ours.
	// This case can arise when we initiate a state transition, but
	// the remote party has a fail crash _after_ accepting the new
	// state, but _before_ sending their signature to us.
	case chainSet.remotePendingCommit != nil &&
		chainSet.remotePendingCommit.CommitTx.TxHash() == commitHash:

		log.Infof("Remote party broadcast pending set, "+
			"commit_num=%v", chainSet.remoteStateNum+1)

		chainSet.commitSet.ConfCommitKey = &RemotePendingHtlcSet
		err := c.dispatchRemoteForceClose(
			commitSpend, *chainSet.remotePendingCommit,
			chainSet.commitSet,
			c.cfg.chanState.RemoteNextRevocation,
		)
		if err != nil {
			return false, fmt.Errorf("unable to handle remote "+
				"close for chan_point=%v: %v",
				c.cfg.chanState.FundingOutpoint, err)
		}

		return true, nil
	}

	// We check if we have a revoked state at this state num that matches
	// the spend transaction.
	retribution, err := lnwallet.NewBreachRetribution(
		c.cfg.chanState, broadcastStateNum, spendHeight,
	)

	switch {

	// If we had no log entry at this height, this was not a revoked state.
	case err == channeldb.ErrLogEntryNotFound:
		return false, nil
	case err == channeldb.ErrNoPastDeltas:
		return false, nil

	case err != nil:
		return false, fmt.Errorf("unable to create breach "+
			"retribution: %v", err)
	}

	// We found a revoked state at this height, but it could still be our
	// own broadcasted state we are looking at. Therefore check that the
	// commit matches before assuming it was a breach.
	if retribution.BreachTransaction.TxHash() != commitHash {
		return false, nil
	}

	// THEY'RE ATTEMPTING TO VIOLATE THE CONTRACT LAID OUT WITHIN THE
	// PAYMENT CHANNEL. Therefore we close the signal indicating a revoked
	// broadcast to allow subscribers to swiftly dispatch justice!!!
	err = c.dispatchContractBreach(
		commitSpend, &chainSet.remoteCommit,
		broadcastStateNum, retribution,
	)
	if err != nil {
		return false, fmt.Errorf("unable to handle channel "+
			"breach for chan_point=%v: %v",
			c.cfg.chanState.FundingOutpoint, err)
	}

	return true, nil
}

// handleUnknownRemoteState is the last attempt we make at reclaiming funds
// from the closed channel, by checkin whether the passed spend _could_ be a
// remote spend that is unknown to us (we lost state). We will try to initiate
// Data Loss Protection in order to restore our commit point and reclaim our
// funds from the channel. If we are not able to act on it, false is returned.
func (c *chainWatcher) handleUnknownRemoteState(
	commitSpend *chainntnfs.SpendDetail, broadcastStateNum uint64,
	chainSet *chainSet) (bool, error) {

	log.Warnf("Remote node broadcast state #%v, "+
		"which is more than 1 beyond best known "+
		"state #%v!!! Attempting recovery...",
		broadcastStateNum, chainSet.remoteStateNum)

	// If this isn't a tweakless commitment, then we'll need to wait for
	// the remote party's latest unrevoked commitment point to be presented
	// to us as we need this to sweep. Otherwise, we can dispatch the
	// remote close and sweep immediately using a fake commitPoint as it
	// isn't actually needed for recovery anymore.
	commitPoint := c.cfg.chanState.RemoteCurrentRevocation
	tweaklessCommit := c.cfg.chanState.ChanType.IsTweakless()
	if !tweaklessCommit {
		commitPoint = c.waitForCommitmentPoint()
		if commitPoint == nil {
			return false, fmt.Errorf("unable to get commit point")
		}

		log.Infof("Recovered commit point(%x) for "+
			"channel(%v)! Now attempting to use it to "+
			"sweep our funds...",
			commitPoint.SerializeCompressed(),
			c.cfg.chanState.FundingOutpoint)

	} else {
		log.Infof("ChannelPoint(%v) is tweakless, "+
			"moving to sweep directly on chain",
			c.cfg.chanState.FundingOutpoint)
	}

	// Since we don't have the commitment stored for this state, we'll just
	// pass an empty commitment within the commitment set. Note that this
	// means we won't be able to recover any HTLC funds.
	//
	// TODO(halseth): can we try to recover some HTLCs?
	chainSet.commitSet.ConfCommitKey = &RemoteHtlcSet
	err := c.dispatchRemoteForceClose(
		commitSpend, channeldb.ChannelCommitment{},
		chainSet.commitSet, commitPoint,
	)
	if err != nil {
		return false, fmt.Errorf("unable to handle remote "+
			"close for chan_point=%v: %v",
			c.cfg.chanState.FundingOutpoint, err)
	}

	return true, nil
}

// toSelfAmount takes a transaction and returns the sum of all outputs that pay
// to a script that the wallet controls. If no outputs pay to us, then we
// return zero. This is possible as our output may have been trimmed due to
// being dust.
func (c *chainWatcher) toSelfAmount(tx *wire.MsgTx) btcutil.Amount {
	var selfAmt btcutil.Amount
	for _, txOut := range tx.TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			// Doesn't matter what net we actually pass in.
			txOut.PkScript, &chaincfg.TestNet3Params,
		)
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if c.cfg.isOurAddr(addr) {
				selfAmt += btcutil.Amount(txOut.Value)
			}
		}
	}

	return selfAmt
}

// dispatchCooperativeClose processed a detect cooperative channel closure.
// We'll use the spending transaction to locate our output within the
// transaction, then clean up the database state. We'll also dispatch a
// notification to all subscribers that the channel has been closed in this
// manner.
func (c *chainWatcher) dispatchCooperativeClose(commitSpend *chainntnfs.SpendDetail) error {
	broadcastTx := commitSpend.SpendingTx

	log.Infof("Cooperative closure for ChannelPoint(%v): %v",
		c.cfg.chanState.FundingOutpoint, spew.Sdump(broadcastTx))

	// If the input *is* final, then we'll check to see which output is
	// ours.
	localAmt := c.toSelfAmount(broadcastTx)

	// Once this is known, we'll mark the state as fully closed in the
	// database. We can do this as a cooperatively closed channel has all
	// its outputs resolved after only one confirmation.
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:               c.cfg.chanState.FundingOutpoint,
		ChainHash:               c.cfg.chanState.ChainHash,
		ClosingTXID:             *commitSpend.SpenderTxHash,
		RemotePub:               c.cfg.chanState.IdentityPub,
		Capacity:                c.cfg.chanState.Capacity,
		CloseHeight:             uint32(commitSpend.SpendingHeight),
		SettledBalance:          localAmt,
		CloseType:               channeldb.CooperativeClose,
		ShortChanID:             c.cfg.chanState.ShortChanID(),
		IsPending:               true,
		RemoteCurrentRevocation: c.cfg.chanState.RemoteCurrentRevocation,
		RemoteNextRevocation:    c.cfg.chanState.RemoteNextRevocation,
		LocalChanConfig:         c.cfg.chanState.LocalChanCfg,
	}

	// Attempt to add a channel sync message to the close summary.
	chanSync, err := c.cfg.chanState.ChanSyncMsg()
	if err != nil {
		log.Errorf("ChannelPoint(%v): unable to create channel sync "+
			"message: %v", c.cfg.chanState.FundingOutpoint, err)
	} else {
		closeSummary.LastChanSyncMsg = chanSync
	}

	// Create a summary of all the information needed to handle the
	// cooperative closure.
	closeInfo := &CooperativeCloseInfo{
		ChannelCloseSummary: closeSummary,
	}

	// With the event processed, we'll now notify all subscribers of the
	// event.
	c.Lock()
	for _, sub := range c.clientSubscriptions {
		select {
		case sub.CooperativeClosure <- closeInfo:
		case <-c.quit:
			c.Unlock()
			return fmt.Errorf("exiting")
		}
	}
	c.Unlock()

	return nil
}

// dispatchLocalForceClose processes a unilateral close by us being confirmed.
func (c *chainWatcher) dispatchLocalForceClose(
	commitSpend *chainntnfs.SpendDetail,
	stateNum uint64, commitSet CommitSet) error {

	log.Infof("Local unilateral close of ChannelPoint(%v) "+
		"detected", c.cfg.chanState.FundingOutpoint)

	forceClose, err := lnwallet.NewLocalForceCloseSummary(
		c.cfg.chanState, c.cfg.signer,
		commitSpend.SpendingTx, stateNum,
	)
	if err != nil {
		return err
	}

	// As we've detected that the channel has been closed, immediately
	// creating a close summary for future usage by related sub-systems.
	chanSnapshot := forceClose.ChanSnapshot
	closeSummary := &channeldb.ChannelCloseSummary{
		ChanPoint:               chanSnapshot.ChannelPoint,
		ChainHash:               chanSnapshot.ChainHash,
		ClosingTXID:             forceClose.CloseTx.TxHash(),
		RemotePub:               &chanSnapshot.RemoteIdentity,
		Capacity:                chanSnapshot.Capacity,
		CloseType:               channeldb.LocalForceClose,
		IsPending:               true,
		ShortChanID:             c.cfg.chanState.ShortChanID(),
		CloseHeight:             uint32(commitSpend.SpendingHeight),
		RemoteCurrentRevocation: c.cfg.chanState.RemoteCurrentRevocation,
		RemoteNextRevocation:    c.cfg.chanState.RemoteNextRevocation,
		LocalChanConfig:         c.cfg.chanState.LocalChanCfg,
	}

	// If our commitment output isn't dust or we have active HTLC's on the
	// commitment transaction, then we'll populate the balances on the
	// close channel summary.
	if forceClose.CommitResolution != nil {
		closeSummary.SettledBalance = chanSnapshot.LocalBalance.ToSatoshis()
		closeSummary.TimeLockedBalance = chanSnapshot.LocalBalance.ToSatoshis()
	}
	for _, htlc := range forceClose.HtlcResolutions.OutgoingHTLCs {
		htlcValue := btcutil.Amount(htlc.SweepSignDesc.Output.Value)
		closeSummary.TimeLockedBalance += htlcValue
	}

	// Attempt to add a channel sync message to the close summary.
	chanSync, err := c.cfg.chanState.ChanSyncMsg()
	if err != nil {
		log.Errorf("ChannelPoint(%v): unable to create channel sync "+
			"message: %v", c.cfg.chanState.FundingOutpoint, err)
	} else {
		closeSummary.LastChanSyncMsg = chanSync
	}

	// With the event processed, we'll now notify all subscribers of the
	// event.
	closeInfo := &LocalUnilateralCloseInfo{
		SpendDetail:            commitSpend,
		LocalForceCloseSummary: forceClose,
		ChannelCloseSummary:    closeSummary,
		CommitSet:              commitSet,
	}
	c.Lock()
	for _, sub := range c.clientSubscriptions {
		select {
		case sub.LocalUnilateralClosure <- closeInfo:
		case <-c.quit:
			c.Unlock()
			return fmt.Errorf("exiting")
		}
	}
	c.Unlock()

	return nil
}

// dispatchRemoteForceClose processes a detected unilateral channel closure by
// the remote party. This function will prepare a UnilateralCloseSummary which
// will then be sent to any subscribers allowing them to resolve all our funds
// in the channel on chain. Once this close summary is prepared, all registered
// subscribers will receive a notification of this event. The commitPoint
// argument should be set to the per_commitment_point corresponding to the
// spending commitment.
//
// NOTE: The remoteCommit argument should be set to the stored commitment for
// this particular state. If we don't have the commitment stored (should only
// happen in case we have lost state) it should be set to an empty struct, in
// which case we will attempt to sweep the non-HTLC output using the passed
// commitPoint.
func (c *chainWatcher) dispatchRemoteForceClose(
	commitSpend *chainntnfs.SpendDetail,
	remoteCommit channeldb.ChannelCommitment,
	commitSet CommitSet, commitPoint *btcec.PublicKey) error {

	log.Infof("Unilateral close of ChannelPoint(%v) "+
		"detected", c.cfg.chanState.FundingOutpoint)

	// First, we'll create a closure summary that contains all the
	// materials required to let each subscriber sweep the funds in the
	// channel on-chain.
	uniClose, err := lnwallet.NewUnilateralCloseSummary(
		c.cfg.chanState, c.cfg.signer, commitSpend,
		remoteCommit, commitPoint,
	)
	if err != nil {
		return err
	}

	// With the event processed, we'll now notify all subscribers of the
	// event.
	c.Lock()
	for _, sub := range c.clientSubscriptions {
		select {
		case sub.RemoteUnilateralClosure <- &RemoteUnilateralCloseInfo{
			UnilateralCloseSummary: uniClose,
			CommitSet:              commitSet,
		}:
		case <-c.quit:
			c.Unlock()
			return fmt.Errorf("exiting")
		}
	}
	c.Unlock()

	return nil
}

// dispatchContractBreach processes a detected contract breached by the remote
// party. This method is to be called once we detect that the remote party has
// broadcast a prior revoked commitment state. This method well prepare all the
// materials required to bring the cheater to justice, then notify all
// registered subscribers of this event.
func (c *chainWatcher) dispatchContractBreach(spendEvent *chainntnfs.SpendDetail,
	remoteCommit *channeldb.ChannelCommitment, broadcastStateNum uint64,
	retribution *lnwallet.BreachRetribution) error {

	log.Warnf("Remote peer has breached the channel contract for "+
		"ChannelPoint(%v). Revoked state #%v was broadcast!!!",
		c.cfg.chanState.FundingOutpoint, broadcastStateNum)

	if err := c.cfg.chanState.MarkBorked(); err != nil {
		return fmt.Errorf("unable to mark channel as borked: %v", err)
	}

	spendHeight := uint32(spendEvent.SpendingHeight)

	// Nil the curve before printing.
	if retribution.RemoteOutputSignDesc != nil &&
		retribution.RemoteOutputSignDesc.DoubleTweak != nil {
		retribution.RemoteOutputSignDesc.DoubleTweak.Curve = nil
	}
	if retribution.RemoteOutputSignDesc != nil &&
		retribution.RemoteOutputSignDesc.KeyDesc.PubKey != nil {
		retribution.RemoteOutputSignDesc.KeyDesc.PubKey.Curve = nil
	}
	if retribution.LocalOutputSignDesc != nil &&
		retribution.LocalOutputSignDesc.DoubleTweak != nil {
		retribution.LocalOutputSignDesc.DoubleTweak.Curve = nil
	}
	if retribution.LocalOutputSignDesc != nil &&
		retribution.LocalOutputSignDesc.KeyDesc.PubKey != nil {
		retribution.LocalOutputSignDesc.KeyDesc.PubKey.Curve = nil
	}

	log.Debugf("Punishment breach retribution created: %v",
		newLogClosure(func() string {
			retribution.KeyRing.CommitPoint.Curve = nil
			retribution.KeyRing.LocalHtlcKey = nil
			retribution.KeyRing.RemoteHtlcKey = nil
			retribution.KeyRing.ToLocalKey = nil
			retribution.KeyRing.ToRemoteKey = nil
			retribution.KeyRing.RevocationKey = nil
			return spew.Sdump(retribution)
		}))

	settledBalance := remoteCommit.LocalBalance.ToSatoshis()
	closeSummary := channeldb.ChannelCloseSummary{
		ChanPoint:               c.cfg.chanState.FundingOutpoint,
		ChainHash:               c.cfg.chanState.ChainHash,
		ClosingTXID:             *spendEvent.SpenderTxHash,
		CloseHeight:             spendHeight,
		RemotePub:               c.cfg.chanState.IdentityPub,
		Capacity:                c.cfg.chanState.Capacity,
		SettledBalance:          settledBalance,
		CloseType:               channeldb.BreachClose,
		IsPending:               true,
		ShortChanID:             c.cfg.chanState.ShortChanID(),
		RemoteCurrentRevocation: c.cfg.chanState.RemoteCurrentRevocation,
		RemoteNextRevocation:    c.cfg.chanState.RemoteNextRevocation,
		LocalChanConfig:         c.cfg.chanState.LocalChanCfg,
	}

	// Attempt to add a channel sync message to the close summary.
	chanSync, err := c.cfg.chanState.ChanSyncMsg()
	if err != nil {
		log.Errorf("ChannelPoint(%v): unable to create channel sync "+
			"message: %v", c.cfg.chanState.FundingOutpoint, err)
	} else {
		closeSummary.LastChanSyncMsg = chanSync
	}

	// We create a function closure that will mark the channel as pending
	// close in the database. We pass it to the contracBreach method such
	// that it can ensure safe handoff of the breach before we close the
	// channel.
	markClosed := func() error {
		// At this point, we've successfully received an ack for the
		// breach close, and we can mark the channel as pending force
		// closed.
		if err := c.cfg.chanState.CloseChannel(
			&closeSummary, channeldb.ChanStatusRemoteCloseInitiator,
		); err != nil {
			return err
		}

		log.Infof("Breached channel=%v marked pending-closed",
			c.cfg.chanState.FundingOutpoint)
		return nil
	}

	// Hand the retribution info over to the breach arbiter.
	if err := c.cfg.contractBreach(retribution, markClosed); err != nil {
		log.Errorf("unable to hand breached contract off to "+
			"breachArbiter: %v", err)
		return err
	}

	// With the event processed and channel closed, we'll now notify all
	// subscribers of the event.
	c.Lock()
	for _, sub := range c.clientSubscriptions {
		select {
		case sub.ContractBreach <- retribution:
		case <-c.quit:
			c.Unlock()
			return fmt.Errorf("quitting")
		}
	}
	c.Unlock()

	return nil
}

// waitForCommitmentPoint waits for the commitment point to be inserted into
// the local database. We'll use this method in the DLP case, to wait for the
// remote party to send us their point, as we can't proceed until we have that.
func (c *chainWatcher) waitForCommitmentPoint() *btcec.PublicKey {
	// If we are lucky, the remote peer sent us the correct commitment
	// point during channel sync, such that we can sweep our funds. If we
	// cannot find the commit point, there's not much we can do other than
	// wait for us to retrieve it. We will attempt to retrieve it from the
	// peer each time we connect to it.
	//
	// TODO(halseth): actively initiate re-connection to the peer?
	backoff := minCommitPointPollTimeout
	for {
		commitPoint, err := c.cfg.chanState.DataLossCommitPoint()
		if err == nil {
			return commitPoint
		}

		log.Errorf("Unable to retrieve commitment point for "+
			"channel(%v) with lost state: %v. Retrying in %v.",
			c.cfg.chanState.FundingOutpoint, err, backoff)

		select {
		// Wait before retrying, with an exponential backoff.
		case <-time.After(backoff):
			backoff = 2 * backoff
			if backoff > maxCommitPointPollTimeout {
				backoff = maxCommitPointPollTimeout
			}

		case <-c.quit:
			return nil
		}
	}
}

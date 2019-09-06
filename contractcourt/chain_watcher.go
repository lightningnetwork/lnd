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
	"github.com/lightningnetwork/lnd/shachain"
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
	// detects that a contract breach transaction has been confirmed. Only
	// when this method returns with a non-nil error it will be safe to mark
	// the channel as pending close in the database.
	contractBreach func(*lnwallet.BreachRetribution) error

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
			return
		},
	}

	c.Lock()
	c.clientSubscriptions[clientID] = sub
	c.Unlock()

	return sub
}

// isOurCommitment returns true if the passed commitSpend is a spend of the
// funding transaction using our commitment transaction (a local force close).
// In order to do this in a state agnostic manner, we'll make our decisions
// based off of only the set of outputs included.
func isOurCommitment(localChanCfg, remoteChanCfg channeldb.ChannelConfig,
	commitSpend *chainntnfs.SpendDetail, broadcastStateNum uint64,
	revocationProducer shachain.Producer) (bool, error) {

	// First, we'll re-derive our commitment point for this state since
	// this is what we use to randomize each of the keys for this state.
	commitSecret, err := revocationProducer.AtIndex(broadcastStateNum)
	if err != nil {
		return false, err
	}
	commitPoint := input.ComputeCommitmentPoint(commitSecret[:])

	// Now that we have the commit point, we'll derive the tweaked local
	// and remote keys for this state. We use our point as only we can
	// revoke our own commitment.
	localDelayBasePoint := localChanCfg.DelayBasePoint.PubKey
	localDelayKey := input.TweakPubKey(localDelayBasePoint, commitPoint)
	remoteNonDelayPoint := remoteChanCfg.PaymentBasePoint.PubKey
	remotePayKey := input.TweakPubKey(remoteNonDelayPoint, commitPoint)

	// With the keys derived, we'll construct the remote script that'll be
	// present if they have a non-dust balance on the commitment.
	remotePkScript, err := input.CommitScriptUnencumbered(remotePayKey)
	if err != nil {
		return false, err
	}

	// Next, we'll derive our script that includes the revocation base for
	// the remote party allowing them to claim this output before the CSV
	// delay if we breach.
	revocationKey := input.DeriveRevocationPubkey(
		remoteChanCfg.RevocationBasePoint.PubKey, commitPoint,
	)
	localScript, err := input.CommitScriptToSelf(
		uint32(localChanCfg.CsvDelay), localDelayKey, revocationKey,
	)
	if err != nil {
		return false, err
	}
	localPkScript, err := input.WitnessScriptHash(localScript)
	if err != nil {
		return false, err
	}

	// With all our scripts assembled, we'll examine the outputs of the
	// commitment transaction to determine if this is a local force close
	// or not.
	for _, output := range commitSpend.SpendingTx.TxOut {
		pkScript := output.PkScript

		switch {
		case bytes.Equal(localPkScript, pkScript):
			return true, nil

		case bytes.Equal(remotePkScript, pkScript):
			return true, nil
		}
	}

	// If neither of these scripts are present, then it isn't a local force
	// close.
	return false, nil
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
	// of spend, we'll act accordingly , so we'll examine the spending
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

		localCommit, remoteCommit, err := c.cfg.chanState.LatestCommitments()
		if err != nil {
			log.Errorf("Unable to fetch channel state for "+
				"chan_point=%v", c.cfg.chanState.FundingOutpoint)
			return
		}

		// Fetch the current known commit height for the remote party,
		// and their pending commitment chain tip if it exist.
		remoteStateNum := remoteCommit.CommitHeight
		remoteChainTip, err := c.cfg.chanState.RemoteCommitChainTip()
		if err != nil && err != channeldb.ErrNoPendingCommit {
			log.Errorf("unable to obtain chain tip for "+
				"ChannelPoint(%v): %v",
				c.cfg.chanState.FundingOutpoint, err)
			return
		}

		// Now that we have all the possible valid commitments, we'll
		// make the CommitSet the ChannelArbitrator will need it in
		// order to carry out its duty.
		commitSet := CommitSet{
			HtlcSets: make(map[HtlcSetKey][]channeldb.HTLC),
		}
		commitSet.HtlcSets[LocalHtlcSet] = localCommit.Htlcs
		commitSet.HtlcSets[RemoteHtlcSet] = remoteCommit.Htlcs
		if remoteChainTip != nil {
			htlcs := remoteChainTip.Commitment.Htlcs
			commitSet.HtlcSets[RemotePendingHtlcSet] = htlcs
		}

		// We'll not retrieve the latest sate of the revocation store
		// so we can populate the information within the channel state
		// object that we have.
		//
		// TODO(roasbeef): mutation is bad mkay
		_, err = c.cfg.chanState.RemoteRevocationStore()
		if err != nil {
			log.Errorf("Unable to fetch revocation state for "+
				"chan_point=%v", c.cfg.chanState.FundingOutpoint)
			return
		}

		// Decode the state hint encoded within the commitment
		// transaction to determine if this is a revoked state or not.
		obfuscator := c.stateHintObfuscator
		broadcastStateNum := c.cfg.extractStateNumHint(
			commitTxBroadcast, obfuscator,
		)

		// Based on the output scripts within this commitment, we'll
		// determine if this is our commitment transaction or not (a
		// self force close).
		isOurCommit, err := isOurCommitment(
			c.cfg.chanState.LocalChanCfg,
			c.cfg.chanState.RemoteChanCfg, commitSpend,
			broadcastStateNum, c.cfg.chanState.RevocationProducer,
		)
		if err != nil {
			log.Errorf("unable to determine self commit for "+
				"chan_point=%v: %v",
				c.cfg.chanState.FundingOutpoint, err)
			return
		}

		// If this is our commitment transaction, then we can exit here
		// as we don't have any further processing we need to do (we
		// can't cheat ourselves :p).
		if isOurCommit {
			commitSet.ConfCommitKey = &LocalHtlcSet

			if err := c.dispatchLocalForceClose(
				commitSpend, *localCommit, commitSet,
			); err != nil {
				log.Errorf("unable to handle local"+
					"close for chan_point=%v: %v",
					c.cfg.chanState.FundingOutpoint, err)
			}
			return
		}

		// Next, we'll check to see if this is a cooperative channel
		// closure or not. This is characterized by having an input
		// sequence number that's finalized.  This won't happen with
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

		log.Warnf("Unprompted commitment broadcast for "+
			"ChannelPoint(%v) ", c.cfg.chanState.FundingOutpoint)

		// If this channel has been recovered, then we'll modify our
		// behavior as it isn't possible for us to close out the
		// channel off-chain ourselves. It can only be the remote party
		// force closing, or a cooperative closure we signed off on
		// before losing data getting confirmed in the chain.
		isRecoveredChan := c.cfg.chanState.HasChanStatus(
			channeldb.ChanStatusRestored,
		)

		switch {
		// If state number spending transaction matches the current
		// latest state, then they've initiated a unilateral close. So
		// we'll trigger the unilateral close signal so subscribers can
		// clean up the state as necessary.
		case broadcastStateNum == remoteStateNum && !isRecoveredChan:
			commitSet.ConfCommitKey = &RemoteHtlcSet

			err := c.dispatchRemoteForceClose(
				commitSpend, *remoteCommit, commitSet,
				c.cfg.chanState.RemoteCurrentRevocation,
			)
			if err != nil {
				log.Errorf("unable to handle remote "+
					"close for chan_point=%v: %v",
					c.cfg.chanState.FundingOutpoint, err)
			}

		// We'll also handle the case of the remote party broadcasting
		// their commitment transaction which is one height above ours.
		// This case can arise when we initiate a state transition, but
		// the remote party has a fail crash _after_ accepting the new
		// state, but _before_ sending their signature to us.
		case broadcastStateNum == remoteStateNum+1 &&
			remoteChainTip != nil && !isRecoveredChan:

			commitSet.ConfCommitKey = &RemotePendingHtlcSet

			err := c.dispatchRemoteForceClose(
				commitSpend, *remoteCommit, commitSet,
				c.cfg.chanState.RemoteNextRevocation,
			)
			if err != nil {
				log.Errorf("unable to handle remote "+
					"close for chan_point=%v: %v",
					c.cfg.chanState.FundingOutpoint, err)
			}

		// If the remote party has broadcasted a state beyond our best
		// known state for them, and they don't have a pending
		// commitment (we write them to disk before sending out), then
		// this means that we've lost data. In this case, we'll enter
		// the DLP protocol. Otherwise, if we've recovered our channel
		// state from scratch, then we don't know what the precise
		// current state is, so we assume either the remote party
		// forced closed or we've been breached. In the latter case,
		// our tower will take care of us.
		case broadcastStateNum > remoteStateNum || isRecoveredChan:
			log.Warnf("Remote node broadcast state #%v, "+
				"which is more than 1 beyond best known "+
				"state #%v!!! Attempting recovery...",
				broadcastStateNum, remoteStateNum)

			// If we are lucky, the remote peer sent us the correct
			// commitment point during channel sync, such that we
			// can sweep our funds. If we cannot find the commit
			// point, there's not much we can do other than wait
			// for us to retrieve it. We will attempt to retrieve
			// it from the peer each time we connect to it.
			//
			// TODO(halseth): actively initiate re-connection to
			// the peer?
			var commitPoint *btcec.PublicKey
			backoff := minCommitPointPollTimeout
			for {
				commitPoint, err = c.cfg.chanState.DataLossCommitPoint()
				if err == nil {
					break
				}

				log.Errorf("Unable to retrieve commitment "+
					"point for channel(%v) with lost "+
					"state: %v. Retrying in %v.",
					c.cfg.chanState.FundingOutpoint,
					err, backoff)

				select {
				// Wait before retrying, with an exponential
				// backoff.
				case <-time.After(backoff):
					backoff = 2 * backoff
					if backoff > maxCommitPointPollTimeout {
						backoff = maxCommitPointPollTimeout
					}

				case <-c.quit:
					return
				}
			}

			log.Infof("Recovered commit point(%x) for "+
				"channel(%v)! Now attempting to use it to "+
				"sweep our funds...",
				commitPoint.SerializeCompressed(),
				c.cfg.chanState.FundingOutpoint)

			// Since we don't have the commitment stored for this
			// state, we'll just pass an empty commitment within
			// the commitment set. Note that this means we won't be
			// able to recover any HTLC funds.
			//
			// TODO(halseth): can we try to recover some HTLCs?
			commitSet.ConfCommitKey = &RemoteHtlcSet
			err = c.dispatchRemoteForceClose(
				commitSpend, channeldb.ChannelCommitment{},
				commitSet, commitPoint,
			)
			if err != nil {
				log.Errorf("unable to handle remote "+
					"close for chan_point=%v: %v",
					c.cfg.chanState.FundingOutpoint, err)
			}

		// If the state number broadcast is lower than the remote
		// node's current un-revoked height, then THEY'RE ATTEMPTING TO
		// VIOLATE THE CONTRACT LAID OUT WITHIN THE PAYMENT CHANNEL.
		// Therefore we close the signal indicating a revoked broadcast
		// to allow subscribers to swiftly dispatch justice!!!
		case broadcastStateNum < remoteStateNum:
			err := c.dispatchContractBreach(
				commitSpend, remoteCommit,
				broadcastStateNum,
			)
			if err != nil {
				log.Errorf("unable to handle channel "+
					"breach for chan_point=%v: %v",
					c.cfg.chanState.FundingOutpoint, err)
			}
		}

		// Now that a spend has been detected, we've done our job, so
		// we'll exit immediately.
		return

	// The chainWatcher has been signalled to exit, so we'll do so now.
	case <-c.quit:
		return
	}
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
	chanSync, err := c.cfg.chanState.ChanSyncMsg(
		c.cfg.chanState.HasChanStatus(channeldb.ChanStatusRestored),
	)
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
	localCommit channeldb.ChannelCommitment, commitSet CommitSet) error {

	log.Infof("Local unilateral close of ChannelPoint(%v) "+
		"detected", c.cfg.chanState.FundingOutpoint)

	forceClose, err := lnwallet.NewLocalForceCloseSummary(
		c.cfg.chanState, c.cfg.signer,
		commitSpend.SpendingTx, localCommit,
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
	chanSync, err := c.cfg.chanState.ChanSyncMsg(
		c.cfg.chanState.HasChanStatus(channeldb.ChanStatusRestored),
	)
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
	remoteCommit *channeldb.ChannelCommitment,
	broadcastStateNum uint64) error {

	log.Warnf("Remote peer has breached the channel contract for "+
		"ChannelPoint(%v). Revoked state #%v was broadcast!!!",
		c.cfg.chanState.FundingOutpoint, broadcastStateNum)

	if err := c.cfg.chanState.MarkBorked(); err != nil {
		return fmt.Errorf("unable to mark channel as borked: %v", err)
	}

	spendHeight := uint32(spendEvent.SpendingHeight)

	// Create a new reach retribution struct which contains all the data
	// needed to swiftly bring the cheating peer to justice.
	//
	// TODO(roasbeef): move to same package
	retribution, err := lnwallet.NewBreachRetribution(
		c.cfg.chanState, broadcastStateNum, spendHeight,
	)
	if err != nil {
		return fmt.Errorf("unable to create breach retribution: %v", err)
	}

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
			retribution.KeyRing.DelayKey = nil
			retribution.KeyRing.NoDelayKey = nil
			retribution.KeyRing.RevocationKey = nil
			return spew.Sdump(retribution)
		}))

	// Hand the retribution info over to the breach arbiter.
	if err := c.cfg.contractBreach(retribution); err != nil {
		log.Errorf("unable to hand breached contract off to "+
			"breachArbiter: %v", err)
		return err
	}

	// With the event processed, we'll now notify all subscribers of the
	// event.
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

	// At this point, we've successfully received an ack for the breach
	// close. We now construct and persist  the close summary, marking the
	// channel as pending force closed.
	//
	// TODO(roasbeef): instead mark we got all the monies?
	// TODO(halseth): move responsibility to breach arbiter?
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
	chanSync, err := c.cfg.chanState.ChanSyncMsg(
		c.cfg.chanState.HasChanStatus(channeldb.ChanStatusRestored),
	)
	if err != nil {
		log.Errorf("ChannelPoint(%v): unable to create channel sync "+
			"message: %v", c.cfg.chanState.FundingOutpoint, err)
	} else {
		closeSummary.LastChanSyncMsg = chanSync
	}

	if err := c.cfg.chanState.CloseChannel(&closeSummary); err != nil {
		return err
	}

	log.Infof("Breached channel=%v marked pending-closed",
		c.cfg.chanState.FundingOutpoint)

	return nil
}

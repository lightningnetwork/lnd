package contractcourt

import (
	"bytes"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
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

// BreachResolution wraps the outpoint of the breached channel.
type BreachResolution struct {
	FundingOutPoint wire.OutPoint
}

// BreachCloseInfo wraps the BreachResolution with a CommitSet for the latest,
// non-breached state, with the AnchorResolution for the breached state.
type BreachCloseInfo struct {
	*BreachResolution
	*lnwallet.AnchorResolution

	// CommitHash is the hash of the commitment transaction.
	CommitHash chainhash.Hash

	// CommitSet is the set of known valid commitments at the time the
	// breach occurred on-chain.
	CommitSet CommitSet

	// CloseSummary gives the recipient of the BreachCloseInfo information
	// to mark the channel closed in the database.
	CloseSummary channeldb.ChannelCloseSummary
}

// CommitSet is a collection of the set of known valid commitments at a given
// instant. If ConfCommitKey is set, then the commitment identified by the
// HtlcSetKey has hit the chain. This struct will be used to examine all live
// HTLCs to determine if any additional actions need to be made based on the
// remote party's commitments.
type CommitSet struct {
	// When the ConfCommitKey is set, it signals that the commitment tx was
	// confirmed in the chain.
	ConfCommitKey fn.Option[HtlcSetKey]

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

// String return a human-readable representation of the CommitSet.
func (c *CommitSet) String() string {
	if c == nil {
		return "nil"
	}

	// Create a descriptive string for the ConfCommitKey.
	commitKey := "none"
	c.ConfCommitKey.WhenSome(func(k HtlcSetKey) {
		commitKey = k.String()
	})

	// Create a map to hold all the htlcs.
	htlcSet := make(map[string]string)
	for k, htlcs := range c.HtlcSets {
		// Create a map for this particular set.
		desc := make([]string, len(htlcs))
		for i, htlc := range htlcs {
			desc[i] = fmt.Sprintf("%x", htlc.RHash)
		}

		// Add the description to the set key.
		htlcSet[k.String()] = fmt.Sprintf("count: %v, htlcs=%v",
			len(htlcs), desc)
	}

	return fmt.Sprintf("ConfCommitKey=%v, HtlcSets=%v", commitKey, htlcSet)
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
	ContractBreach chan *BreachCloseInfo

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
	// detects that a contract breach transaction has been confirmed. It
	// will only return a non-nil error when the BreachArbitrator has
	// preserved the necessary breach info for this channel point.
	contractBreach func(*lnwallet.BreachRetribution) error

	// isOurAddr is a function that returns true if the passed address is
	// known to us.
	isOurAddr func(btcutil.Address) bool

	// extractStateNumHint extracts the encoded state hint using the passed
	// obfuscater. This is used by the chain watcher to identify which
	// state was broadcast and confirmed on-chain.
	extractStateNumHint func(*wire.MsgTx, [lnwallet.StateHintSize]byte) uint64

	// auxLeafStore can be used to fetch information for custom channels.
	auxLeafStore fn.Option[lnwallet.AuxLeafStore]

	// auxResolver is used to supplement contract resolution.
	auxResolver fn.Option[lnwallet.AuxContractResolver]
}

// chainWatcher is a system that's assigned to every active channel. The duty
// of this system is to watch the chain for spends of the channels chan point.
// If a spend is detected then with chain watcher will notify all subscribers
// that the channel has been closed, and also give them the materials necessary
// to sweep the funds of the channel on chain eventually.
type chainWatcher struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	// Embed the blockbeat consumer struct to get access to the method
	// `NotifyBlockProcessed` and the `BlockbeatChan`.
	chainio.BeatConsumer

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

	// fundingSpendNtfn is the spending notification subscription for the
	// funding outpoint.
	fundingSpendNtfn *chainntnfs.SpendEvent

	// fundingConfirmedNtfn is the confirmation notification subscription
	// for the funding outpoint. This is only created if the channel is
	// both taproot and pending confirmation.
	//
	// For taproot pkscripts, `RegisterSpendNtfn` will only notify on the
	// outpoint being spent and not the outpoint+pkscript due to
	// `ComputePkScript` being unable to compute the pkscript if a key
	// spend is used. We need to add a `RegisterConfirmationsNtfn` here to
	// ensure that the outpoint+pkscript pair is confirmed before calling
	// `RegisterSpendNtfn`.
	fundingConfirmedNtfn *chainntnfs.ConfirmationEvent
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

	// Get the witness script for the funding output.
	fundingPkScript, err := deriveFundingPkScript(chanState)
	if err != nil {
		return nil, err
	}

	// Get the channel opening block height.
	heightHint := chanState.DeriveHeightHint()

	// We'll register for a notification to be dispatched if the funding
	// output is spent.
	spendNtfn, err := cfg.notifier.RegisterSpendNtfn(
		&chanState.FundingOutpoint, fundingPkScript, heightHint,
	)
	if err != nil {
		return nil, err
	}

	c := &chainWatcher{
		cfg:                 cfg,
		stateHintObfuscator: stateHint,
		quit:                make(chan struct{}),
		clientSubscriptions: make(map[uint64]*ChainEventSubscription),
		fundingSpendNtfn:    spendNtfn,
	}

	// If this is a pending taproot channel, we need to register for a
	// confirmation notification of the funding tx. Check the docs in
	// `fundingConfirmedNtfn` for details.
	if c.cfg.chanState.IsPending && c.cfg.chanState.ChanType.IsTaproot() {
		confNtfn, err := cfg.notifier.RegisterConfirmationsNtfn(
			&chanState.FundingOutpoint.Hash, fundingPkScript, 1,
			heightHint,
		)
		if err != nil {
			return nil, err
		}

		c.fundingConfirmedNtfn = confNtfn
	}

	// Mount the block consumer.
	c.BeatConsumer = chainio.NewBeatConsumer(c.quit, c.Name())

	return c, nil
}

// Compile-time check for the chainio.Consumer interface.
var _ chainio.Consumer = (*chainWatcher)(nil)

// Name returns the name of the watcher.
//
// NOTE: part of the `chainio.Consumer` interface.
func (c *chainWatcher) Name() string {
	return fmt.Sprintf("ChainWatcher(%v)", c.cfg.chanState.FundingOutpoint)
}

// Start starts all goroutines that the chainWatcher needs to perform its
// duties.
func (c *chainWatcher) Start() error {
	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		return nil
	}

	log.Debugf("Starting chain watcher for ChannelPoint(%v)",
		c.cfg.chanState.FundingOutpoint)

	c.wg.Add(1)
	go c.closeObserver()

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
		ContractBreach:          make(chan *BreachCloseInfo, 1),
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
		commitPoint, lntypes.Local, c.cfg.chanState.ChanType,
		&c.cfg.chanState.LocalChanCfg, &c.cfg.chanState.RemoteChanCfg,
	)

	auxResult, err := fn.MapOptionZ(
		c.cfg.auxLeafStore,
		//nolint:ll
		func(s lnwallet.AuxLeafStore) fn.Result[lnwallet.CommitDiffAuxResult] {
			return s.FetchLeavesFromCommit(
				lnwallet.NewAuxChanState(c.cfg.chanState),
				c.cfg.chanState.LocalCommitment, *commitKeyRing,
				lntypes.Local,
			)
		},
	).Unpack()
	if err != nil {
		return false, fmt.Errorf("unable to fetch aux leaves: %w", err)
	}

	// With the keys derived, we'll construct the remote script that'll be
	// present if they have a non-dust balance on the commitment.
	var leaseExpiry uint32
	if c.cfg.chanState.ChanType.HasLeaseExpiration() {
		leaseExpiry = c.cfg.chanState.ThawHeight
	}

	remoteAuxLeaf := fn.FlatMapOption(
		func(l lnwallet.CommitAuxLeaves) input.AuxTapLeaf {
			return l.RemoteAuxLeaf
		},
	)(auxResult.AuxLeaves)
	remoteScript, _, err := lnwallet.CommitScriptToRemote(
		c.cfg.chanState.ChanType, c.cfg.chanState.IsInitiator,
		commitKeyRing.ToRemoteKey, leaseExpiry,
		remoteAuxLeaf,
	)
	if err != nil {
		return false, err
	}

	// Next, we'll derive our script that includes the revocation base for
	// the remote party allowing them to claim this output before the CSV
	// delay if we breach.
	localAuxLeaf := fn.FlatMapOption(
		func(l lnwallet.CommitAuxLeaves) input.AuxTapLeaf {
			return l.LocalAuxLeaf
		},
	)(auxResult.AuxLeaves)
	localScript, err := lnwallet.CommitScriptToSelf(
		c.cfg.chanState.ChanType, c.cfg.chanState.IsInitiator,
		commitKeyRing.ToLocalKey, commitKeyRing.RevocationKey,
		uint32(c.cfg.chanState.LocalChanCfg.CsvDelay), leaseExpiry,
		localAuxLeaf,
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
		case bytes.Equal(localScript.PkScript(), pkScript):
			ourCommit = true

		case bytes.Equal(remoteScript.PkScript(), pkScript):
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
	chainSet.commitSet.ConfCommitKey = fn.Some(LocalHtlcSet)
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
			"chan_point=%v: %v", chanState.FundingOutpoint, err)
	}

	log.Tracef("ChannelPoint(%v): local_commit_type=%v, local_commit=%v",
		chanState.FundingOutpoint, chanState.ChanType,
		lnutils.SpewLogClosure(localCommit))
	log.Tracef("ChannelPoint(%v): remote_commit_type=%v, remote_commit=%v",
		chanState.FundingOutpoint, chanState.ChanType,
		lnutils.SpewLogClosure(remoteCommit))

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
		log.Tracef("ChannelPoint(%v): remote_pending_commit_type=%v, "+
			"remote_pending_commit=%v", chanState.FundingOutpoint,
			chanState.ChanType,
			lnutils.SpewLogClosure(remoteChainTip.Commitment))

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
func (c *chainWatcher) closeObserver() {
	defer c.wg.Done()
	defer c.fundingSpendNtfn.Cancel()

	log.Infof("Close observer for ChannelPoint(%v) active",
		c.cfg.chanState.FundingOutpoint)

	for {
		select {
		// A new block is received, we will check whether this block
		// contains a spending tx that we are interested in.
		case beat := <-c.BlockbeatChan:
			log.Debugf("ChainWatcher(%v) received blockbeat %v",
				c.cfg.chanState.FundingOutpoint, beat.Height())

			// Process the block.
			c.handleBlockbeat(beat)

		// If the funding outpoint is spent, we now go ahead and handle
		// it. Note that we cannot rely solely on the `block` event
		// above to trigger a close event, as deep down, the receiving
		// of block notifications and the receiving of spending
		// notifications are done in two different goroutines, so the
		// expected order: [receive block -> receive spend] is not
		// guaranteed .
		case spend, ok := <-c.fundingSpendNtfn.Spend:
			// If the channel was closed, then this means that the
			// notifier exited, so we will as well.
			if !ok {
				return
			}

			err := c.handleCommitSpend(spend)
			if err != nil {
				log.Errorf("Failed to handle commit spend: %v",
					err)
			}

		// The chainWatcher has been signalled to exit, so we'll do so
		// now.
		case <-c.quit:
			return
		}
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

	chainSet.commitSet.ConfCommitKey = fn.Some(LocalHtlcSet)
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

	switch {
	// If the spending transaction matches the current latest state, then
	// they've initiated a unilateral close. So we'll trigger the
	// unilateral close signal so subscribers can clean up the state as
	// necessary.
	case chainSet.remoteCommit.CommitTx.TxHash() == commitHash:
		log.Infof("Remote party broadcast base set, "+
			"commit_num=%v", chainSet.remoteStateNum)

		chainSet.commitSet.ConfCommitKey = fn.Some(RemoteHtlcSet)
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

		chainSet.commitSet.ConfCommitKey = fn.Some(RemotePendingHtlcSet)
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

	// This is neither a remote force close or a "future" commitment, we
	// now check whether it's a remote breach and properly handle it.
	return c.handlePossibleBreach(commitSpend, broadcastStateNum, chainSet)
}

// handlePossibleBreach checks whether the remote has breached and dispatches a
// breach resolution to claim funds.
func (c *chainWatcher) handlePossibleBreach(commitSpend *chainntnfs.SpendDetail,
	broadcastStateNum uint64, chainSet *chainSet) (bool, error) {

	// We check if we have a revoked state at this state num that matches
	// the spend transaction.
	spendHeight := uint32(commitSpend.SpendingHeight)
	retribution, err := lnwallet.NewBreachRetribution(
		c.cfg.chanState, broadcastStateNum, spendHeight,
		commitSpend.SpendingTx, c.cfg.auxLeafStore, c.cfg.auxResolver,
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
	commitHash := commitSpend.SpendingTx.TxHash()
	if retribution.BreachTxHash != commitHash {
		return false, nil
	}

	// Create an AnchorResolution for the breached state.
	anchorRes, err := lnwallet.NewAnchorResolution(
		c.cfg.chanState, commitSpend.SpendingTx, retribution.KeyRing,
		lntypes.Remote,
	)
	if err != nil {
		return false, fmt.Errorf("unable to create anchor "+
			"resolution: %v", err)
	}

	// We'll set the ConfCommitKey here as the remote htlc set. This is
	// only used to ensure a nil-pointer-dereference doesn't occur and is
	// not used otherwise. The HTLC's may not exist for the
	// RemotePendingHtlcSet.
	chainSet.commitSet.ConfCommitKey = fn.Some(RemoteHtlcSet)

	// THEY'RE ATTEMPTING TO VIOLATE THE CONTRACT LAID OUT WITHIN THE
	// PAYMENT CHANNEL. Therefore we close the signal indicating a revoked
	// broadcast to allow subscribers to swiftly dispatch justice!!!
	err = c.dispatchContractBreach(
		commitSpend, chainSet, broadcastStateNum, retribution,
		anchorRes,
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
	chainSet.commitSet.ConfCommitKey = fn.Some(RemoteHtlcSet)
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
// to a script that the wallet controls or the channel defines as its delivery
// script . If no outputs pay to us (determined by these criteria), then we
// return zero. This is possible as our output may have been trimmed due to
// being dust.
func (c *chainWatcher) toSelfAmount(tx *wire.MsgTx) btcutil.Amount {
	// There are two main cases we have to handle here. First, in the coop
	// close case we will always have saved the delivery address we used
	// whether it was from the upfront shutdown, from the delivery address
	// requested at close time, or even an automatically generated one. All
	// coop-close cases can be identified in the following manner:
	shutdown, _ := c.cfg.chanState.ShutdownInfo()
	oDeliveryAddr := fn.MapOption(
		func(i channeldb.ShutdownInfo) lnwire.DeliveryAddress {
			return i.DeliveryScript.Val
		})(shutdown)

	// Here we define a function capable of identifying whether an output
	// corresponds with our local delivery script from a ShutdownInfo if we
	// have a ShutdownInfo for this chainWatcher's underlying channel.
	//
	// isDeliveryOutput :: *TxOut -> bool
	isDeliveryOutput := func(o *wire.TxOut) bool {
		return fn.ElimOption(
			oDeliveryAddr,
			// If we don't have a delivery addr, then the output
			// can't match it.
			func() bool { return false },
			// Otherwise if the PkScript of the TxOut matches our
			// delivery script then this is a delivery output.
			func(a lnwire.DeliveryAddress) bool {
				return slices.Equal(a, o.PkScript)
			},
		)
	}

	// Here we define a function capable of identifying whether an output
	// belongs to the LND wallet. We use this as a heuristic in the case
	// where we might be looking for spendable force closure outputs.
	//
	// isWalletOutput :: *TxOut -> bool
	isWalletOutput := func(out *wire.TxOut) bool {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			// Doesn't matter what net we actually pass in.
			out.PkScript, &chaincfg.TestNet3Params,
		)
		if err != nil {
			return false
		}

		return fn.Any(addrs, c.cfg.isOurAddr)
	}

	// Grab all of the outputs that correspond with our delivery address
	// or our wallet is aware of.
	outs := fn.Filter(tx.TxOut, fn.PredOr(isDeliveryOutput, isWalletOutput))

	// Grab the values for those outputs.
	vals := fn.Map(outs, func(o *wire.TxOut) int64 { return o.Value })

	// Return the sum.
	return btcutil.Amount(fn.Sum(vals))
}

// dispatchCooperativeClose processed a detect cooperative channel closure.
// We'll use the spending transaction to locate our output within the
// transaction, then clean up the database state. We'll also dispatch a
// notification to all subscribers that the channel has been closed in this
// manner.
func (c *chainWatcher) dispatchCooperativeClose(commitSpend *chainntnfs.SpendDetail) error {
	broadcastTx := commitSpend.SpendingTx

	log.Infof("Cooperative closure for ChannelPoint(%v): %v",
		c.cfg.chanState.FundingOutpoint,
		lnutils.SpewLogClosure(broadcastTx))

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
		c.cfg.chanState, c.cfg.signer, commitSpend.SpendingTx,
		uint32(commitSpend.SpendingHeight), stateNum,
		c.cfg.auxLeafStore, c.cfg.auxResolver,
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

	resolutions, err := forceClose.ContractResolutions.UnwrapOrErr(
		fmt.Errorf("resolutions not found"),
	)
	if err != nil {
		return err
	}

	// If our commitment output isn't dust or we have active HTLC's on the
	// commitment transaction, then we'll populate the balances on the
	// close channel summary.
	if resolutions.CommitResolution != nil {
		localBalance := chanSnapshot.LocalBalance.ToSatoshis()
		closeSummary.SettledBalance = localBalance
		closeSummary.TimeLockedBalance = localBalance
	}

	if resolutions.HtlcResolutions != nil {
		for _, htlc := range resolutions.HtlcResolutions.OutgoingHTLCs {
			htlcValue := btcutil.Amount(
				htlc.SweepSignDesc.Output.Value,
			)
			closeSummary.TimeLockedBalance += htlcValue
		}
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
		c.cfg.chanState, c.cfg.signer, commitSpend, remoteCommit,
		commitPoint, c.cfg.auxLeafStore, c.cfg.auxResolver,
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
	chainSet *chainSet, broadcastStateNum uint64,
	retribution *lnwallet.BreachRetribution,
	anchorRes *lnwallet.AnchorResolution) error {

	log.Warnf("Remote peer has breached the channel contract for "+
		"ChannelPoint(%v). Revoked state #%v was broadcast!!!",
		c.cfg.chanState.FundingOutpoint, broadcastStateNum)

	if err := c.cfg.chanState.MarkBorked(); err != nil {
		return fmt.Errorf("unable to mark channel as borked: %w", err)
	}

	spendHeight := uint32(spendEvent.SpendingHeight)

	log.Debugf("Punishment breach retribution created: %v",
		lnutils.NewLogClosure(func() string {
			retribution.KeyRing.LocalHtlcKey = nil
			retribution.KeyRing.RemoteHtlcKey = nil
			retribution.KeyRing.ToLocalKey = nil
			retribution.KeyRing.ToRemoteKey = nil
			retribution.KeyRing.RevocationKey = nil
			return spew.Sdump(retribution)
		}))

	settledBalance := chainSet.remoteCommit.LocalBalance.ToSatoshis()
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

	// Hand the retribution info over to the BreachArbitrator. This function
	// will wait for a response from the breach arbiter and then proceed to
	// send a BreachCloseInfo to the channel arbitrator. The channel arb
	// will then mark the channel as closed after resolutions and the
	// commit set are logged in the arbitrator log.
	if err := c.cfg.contractBreach(retribution); err != nil {
		log.Errorf("unable to hand breached contract off to "+
			"BreachArbitrator: %v", err)
		return err
	}

	breachRes := &BreachResolution{
		FundingOutPoint: c.cfg.chanState.FundingOutpoint,
	}

	breachInfo := &BreachCloseInfo{
		CommitHash:       spendEvent.SpendingTx.TxHash(),
		BreachResolution: breachRes,
		AnchorResolution: anchorRes,
		CommitSet:        chainSet.commitSet,
		CloseSummary:     closeSummary,
	}

	// With the event processed and channel closed, we'll now notify all
	// subscribers of the event.
	c.Lock()
	for _, sub := range c.clientSubscriptions {
		select {
		case sub.ContractBreach <- breachInfo:
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

// deriveFundingPkScript derives the script used in the funding output.
func deriveFundingPkScript(chanState *channeldb.OpenChannel) ([]byte, error) {
	localKey := chanState.LocalChanCfg.MultiSigKey.PubKey
	remoteKey := chanState.RemoteChanCfg.MultiSigKey.PubKey

	var (
		err             error
		fundingPkScript []byte
	)

	if chanState.ChanType.IsTaproot() {
		fundingPkScript, _, err = input.GenTaprootFundingScript(
			localKey, remoteKey, 0, chanState.TapscriptRoot,
		)
		if err != nil {
			return nil, err
		}
	} else {
		multiSigScript, err := input.GenMultiSigScript(
			localKey.SerializeCompressed(),
			remoteKey.SerializeCompressed(),
		)
		if err != nil {
			return nil, err
		}
		fundingPkScript, err = input.WitnessScriptHash(multiSigScript)
		if err != nil {
			return nil, err
		}
	}

	return fundingPkScript, nil
}

// handleCommitSpend takes a spending tx of the funding output and handles the
// channel close based on the closure type.
func (c *chainWatcher) handleCommitSpend(
	commitSpend *chainntnfs.SpendDetail) error {

	commitTxBroadcast := commitSpend.SpendingTx

	// First, we'll construct the chainset which includes all the data we
	// need to dispatch an event to our subscribers about this possible
	// channel close event.
	chainSet, err := newChainSet(c.cfg.chanState)
	if err != nil {
		return fmt.Errorf("create commit set: %w", err)
	}

	// Decode the state hint encoded within the commitment transaction to
	// determine if this is a revoked state or not.
	obfuscator := c.stateHintObfuscator
	broadcastStateNum := c.cfg.extractStateNumHint(
		commitTxBroadcast, obfuscator,
	)

	// We'll go on to check whether it could be our own commitment that was
	// published and know is confirmed.
	ok, err := c.handleKnownLocalState(
		commitSpend, broadcastStateNum, chainSet,
	)
	if err != nil {
		return fmt.Errorf("handle known local state: %w", err)
	}
	if ok {
		return nil
	}

	// Now that we know it is neither a non-cooperative closure nor a local
	// close with the latest state, we check if it is the remote that
	// closed with any prior or current state.
	ok, err = c.handleKnownRemoteState(
		commitSpend, broadcastStateNum, chainSet,
	)
	if err != nil {
		return fmt.Errorf("handle known remote state: %w", err)
	}
	if ok {
		return nil
	}

	// Next, we'll check to see if this is a cooperative channel closure or
	// not. This is characterized by having an input sequence number that's
	// finalized. This won't happen with regular commitment transactions
	// due to the state hint encoding scheme.
	switch commitTxBroadcast.TxIn[0].Sequence {
	case wire.MaxTxInSequenceNum:
		fallthrough
	case mempool.MaxRBFSequence:
		// TODO(roasbeef): rare but possible, need itest case for
		err := c.dispatchCooperativeClose(commitSpend)
		if err != nil {
			return fmt.Errorf("handle coop close: %w", err)
		}

		return nil
	}

	log.Warnf("Unknown commitment broadcast for ChannelPoint(%v) ",
		c.cfg.chanState.FundingOutpoint)

	// We'll try to recover as best as possible from losing state.  We
	// first check if this was a local unknown state. This could happen if
	// we force close, then lose state or attempt recovery before the
	// commitment confirms.
	ok, err = c.handleUnknownLocalState(
		commitSpend, broadcastStateNum, chainSet,
	)
	if err != nil {
		return fmt.Errorf("handle known local state: %w", err)
	}
	if ok {
		return nil
	}

	// Since it was neither a known remote state, nor a local state that
	// was published, it most likely mean we lost state and the remote node
	// closed. In this case we must start the DLP protocol in hope of
	// getting our money back.
	ok, err = c.handleUnknownRemoteState(
		commitSpend, broadcastStateNum, chainSet,
	)
	if err != nil {
		return fmt.Errorf("handle unknown remote state: %w", err)
	}
	if ok {
		return nil
	}

	log.Errorf("Unable to handle spending tx %v of channel point %v",
		commitTxBroadcast.TxHash(), c.cfg.chanState.FundingOutpoint)

	return nil
}

// checkFundingSpend performs a non-blocking read on the spendNtfn channel to
// check whether there's a commit spend already. Returns the spend details if
// found.
func (c *chainWatcher) checkFundingSpend() *chainntnfs.SpendDetail {
	select {
	// We've detected a spend of the channel onchain! Depending on the type
	// of spend, we'll act accordingly, so we'll examine the spending
	// transaction to determine what we should do.
	//
	// TODO(Roasbeef): need to be able to ensure this only triggers
	// on confirmation, to ensure if multiple txns are broadcast, we
	// act on the one that's timestamped
	case spend, ok := <-c.fundingSpendNtfn.Spend:
		// If the channel was closed, then this means that the notifier
		// exited, so we will as well.
		if !ok {
			return nil
		}

		log.Debugf("Found spend details for funding output: %v",
			spend.SpenderTxHash)

		return spend

	default:
	}

	return nil
}

// chanPointConfirmed checks whether the given channel point has confirmed.
// This is used to ensure that the funding output has confirmed on chain before
// we proceed with the rest of the close observer logic for taproot channels.
// Check the docs in `fundingConfirmedNtfn` for details.
func (c *chainWatcher) chanPointConfirmed() bool {
	op := c.cfg.chanState.FundingOutpoint

	select {
	case _, ok := <-c.fundingConfirmedNtfn.Confirmed:
		// If the channel was closed, then this means that the notifier
		// exited, so we will as well.
		if !ok {
			return false
		}

		log.Debugf("Taproot ChannelPoint(%v) confirmed", op)

		// The channel point has confirmed on chain. We now cancel the
		// subscription.
		c.fundingConfirmedNtfn.Cancel()

		return true

	default:
		log.Infof("Taproot ChannelPoint(%v) not confirmed yet", op)

		return false
	}
}

// handleBlockbeat takes a blockbeat and queries for a spending tx for the
// funding output. If the spending tx is found, it will be handled based on the
// closure type.
func (c *chainWatcher) handleBlockbeat(beat chainio.Blockbeat) {
	// Notify the chain watcher has processed the block.
	defer c.NotifyBlockProcessed(beat, nil)

	// If we have a fundingConfirmedNtfn, it means this is a taproot
	// channel that is pending, before we proceed, we want to ensure that
	// the expected funding output has confirmed on chain. Check the docs
	// in `fundingConfirmedNtfn` for details.
	if c.fundingConfirmedNtfn != nil {
		// If the funding output hasn't confirmed in this block, we
		// will check it again in the next block.
		if !c.chanPointConfirmed() {
			return
		}
	}

	// Perform a non-blocking read to check whether the funding output was
	// spent.
	spend := c.checkFundingSpend()
	if spend == nil {
		log.Tracef("No spend found for ChannelPoint(%v) in block %v",
			c.cfg.chanState.FundingOutpoint, beat.Height())

		return
	}

	// The funding output was spent, we now handle it by sending a close
	// event to the channel arbitrator.
	err := c.handleCommitSpend(spend)
	if err != nil {
		log.Errorf("Failed to handle commit spend: %v", err)
	}
}

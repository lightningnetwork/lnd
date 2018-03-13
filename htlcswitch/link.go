package htlcswitch

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"crypto/sha256"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

const (
	// expiryGraceDelta is a grace period that the timeout of incoming
	// HTLC's that pay directly to us (i.e we're the "exit node") must up
	// hold. We'll reject any HTLC's who's timeout minus this value is less
	// that or equal to the current block height. We require this in order
	// to ensure that if the extending party goes to the chain, then we'll
	// be able to claim the HTLC still.
	//
	// TODO(roasbeef): must be < default delta
	expiryGraceDelta = 2
)

// ErrInternalLinkFailure is a generic error returned to the remote party so as
// to obfuscate the true failure.
var ErrInternalLinkFailure = errors.New("internal link failure")

// ForwardingPolicy describes the set of constraints that a given ChannelLink
// is to adhere to when forwarding HTLC's. For each incoming HTLC, this set of
// constraints will be consulted in order to ensure that adequate fees are
// paid, and our time-lock parameters are respected. In the event that an
// incoming HTLC violates any of these constraints, it is to be _rejected_ with
// the error possibly carrying along a ChannelUpdate message that includes the
// latest policy.
type ForwardingPolicy struct {
	// MinHTLC is the smallest HTLC that is to be forwarded. This is
	// set when a channel is first opened, and will be static for the
	// lifetime of the channel.
	MinHTLC lnwire.MilliSatoshi

	// BaseFee is the base fee, expressed in milli-satoshi that must be
	// paid for each incoming HTLC. This field, combined with FeeRate is
	// used to compute the required fee for a given HTLC.
	BaseFee lnwire.MilliSatoshi

	// FeeRate is the fee rate, expressed in milli-satoshi that must be
	// paid for each incoming HTLC. This field combined with BaseFee is
	// used to compute the required fee for a given HTLC.
	FeeRate lnwire.MilliSatoshi

	// TimeLockDelta is the absolute time-lock value, expressed in blocks,
	// that will be subtracted from an incoming HTLC's timelock value to
	// create the time-lock value for the forwarded outgoing HTLC. The
	// following constraint MUST hold for an HTLC to be forwarded:
	//
	//  * incomingHtlc.timeLock - timeLockDelta = fwdInfo.OutgoingCTLV
	//
	//    where fwdInfo is the forwarding information extracted from the
	//    per-hop payload of the incoming HTLC's onion packet.
	TimeLockDelta uint32

	// TODO(roasbeef): add fee module inside of switch
}

// ExpectedFee computes the expected fee for a given htlc amount. The value
// returned from this function is to be used as a sanity check when forwarding
// HTLC's to ensure that an incoming HTLC properly adheres to our propagated
// forwarding policy.
//
// TODO(roasbeef): also add in current available channel bandwidth, inverse
// func
func ExpectedFee(f ForwardingPolicy, htlcAmt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	// TODO(roasbeef): write some basic table driven tests
	return f.BaseFee + (htlcAmt*f.FeeRate)/1000000
}

// Ticker is an interface used to wrap a time.Ticker in a struct, making
// mocking it easier.
type Ticker interface {
	Start() <-chan time.Time

	Stop()
}

// BatchTicker implements the Ticker interface, and wraps a time.Ticker.
type BatchTicker struct {
	ticker *time.Ticker
}

// NewBatchTicker returns a new BatchTicker that wraps the passed time.Ticker.
func NewBatchTicker(t *time.Ticker) *BatchTicker {
	return &BatchTicker{t}
}

// Start returns the tick channel for the underlying time.Ticker.
func (t *BatchTicker) Start() <-chan time.Time {
	return t.ticker.C
}

// Stop stops the underlying time.Ticker.
func (t *BatchTicker) Stop() {
	t.ticker.Stop()
}

// ChannelLinkConfig defines the configuration for the channel link. ALL
// elements within the configuration MUST be non-nil for channel link to carry
// out its duties.
type ChannelLinkConfig struct {
	// FwrdingPolicy is the initial forwarding policy to be used when
	// deciding whether to forwarding incoming HTLC's or not. This value
	// can be updated with subsequent calls to UpdateForwardingPolicy
	// targeted at a given ChannelLink concrete interface implementation.
	FwrdingPolicy ForwardingPolicy

	// Circuits provides restricted access to the switch's circuit map,
	// allowing the link to open and close circuits.
	Circuits CircuitModifier

	// Switch provides a reference to the HTLC switch, we only use this in
	// testing to access circuit operations not typically exposed by the
	// CircuitModifier.
	//
	// TODO(conner): remove after refactoring htlcswitch testing framework.
	Switch *Switch

	// ForwardPackets attempts to forward the batch of htlcs through the
	// switch. Any failed packets will be returned to the provided
	// ChannelLink.
	ForwardPackets func(...*htlcPacket) chan error

	// DecodeHopIterators facilitates batched decoding of HTLC Sphinx onion
	// blobs, which are then used to inform how to forward an HTLC.
	//
	// NOTE: This function assumes the same set of readers and preimages
	// are always presented for the same identifier.
	DecodeHopIterators func([]byte, []DecodeHopIteratorRequest) (
		[]DecodeHopIteratorResponse, error)

	// DecodeOnionObfuscator function is responsible for decoding HTLC
	// Sphinx onion blob, and creating onion failure obfuscator.
	DecodeOnionObfuscator ErrorEncrypterExtracter

	// GetLastChannelUpdate retrieves the latest routing policy for this
	// particular channel. This will be used to provide payment senders our
	// latest policy when sending encrypted error messages.
	GetLastChannelUpdate func() (*lnwire.ChannelUpdate, error)

	// Peer is a lightning network node with which we have the channel link
	// opened.
	Peer Peer

	// Registry is a sub-system which responsible for managing the invoices
	// in thread-safe manner.
	Registry InvoiceDatabase

	// PreimageCache is a global witness beacon that houses any new
	// preimages discovered by other links. We'll use this to add new
	// witnesses that we discover which will notify any sub-systems
	// subscribed to new events.
	PreimageCache contractcourt.WitnessBeacon

	// UpdateContractSignals is a function closure that we'll use to update
	// outside sub-systems with the latest signals for our inner Lightning
	// channel. These signals will notify the caller when the channel has
	// been closed, or when the set of active HTLC's is updated.
	UpdateContractSignals func(*contractcourt.ContractSignals) error

	// ChainEvents is an active subscription to the chain watcher for this
	// channel to be notified of any on-chain activity related to this
	// channel.
	ChainEvents *contractcourt.ChainEventSubscription

	// FeeEstimator is an instance of a live fee estimator which will be
	// used to dynamically regulate the current fee of the commitment
	// transaction to ensure timely confirmation.
	FeeEstimator lnwallet.FeeEstimator

	// BlockEpochs is an active block epoch event stream backed by an
	// active ChainNotifier instance. The ChannelLink will use new block
	// notifications sent over this channel to decide when a _new_ HTLC is
	// too close to expiry, and also when any active HTLC's have expired
	// (or are close to expiry).
	BlockEpochs *chainntnfs.BlockEpochEvent

	// DebugHTLC should be turned on if you want all HTLCs sent to a node
	// with the debug htlc R-Hash are immediately settled in the next
	// available state transition.
	DebugHTLC bool

	// HodlHTLC should be active if you want this node to refrain from
	// settling all incoming HTLCs with the sender if it finds itself to be
	// the exit node.
	//
	// NOTE: HodlHTLC should be active in conjunction with DebugHTLC.
	HodlHTLC bool

	// SyncStates is used to indicate that we need send the channel
	// reestablishment message to the remote peer. It should be done if our
	// clients have been restarted, or remote peer have been reconnected.
	SyncStates bool

	// BatchTicker is the ticker that determines the interval that we'll
	// use to check the batch to see if there're any updates we should
	// flush out. By batching updates into a single commit, we attempt to
	// increase throughput by maximizing the number of updates coalesced
	// into a single commit.
	BatchTicker Ticker

	// FwdPkgGCTicker is the ticker determining the frequency at which
	// garbage collection of forwarding packages occurs. We use a
	// time-based approach, as opposed to block epochs, as to not hinder
	// syncing.
	FwdPkgGCTicker Ticker

	// BatchSize is the max size of a batch of updates done to the link
	// before we do a state update.
	BatchSize uint32

	// UnsafeReplay will cause a link to replay the adds in its latest
	// commitment txn after the link is restarted. This should only be used
	// in testing, it is here to ensure the sphinx replay detection on the
	// receiving node is persistent.
	UnsafeReplay bool
}

// channelLink is the service which drives a channel's commitment update
// state-machine. In the event that an HTLC needs to be propagated to another
// link, the forward handler from config is used which sends HTLC to the
// switch. Additionally, the link encapsulate logic of commitment protocol
// message ordering and updates.
type channelLink struct {
	// The following fields are only meant to be used *atomically*
	started  int32
	shutdown int32

	// batchCounter is the number of updates which we received from remote
	// side, but not include in commitment transaction yet and plus the
	// current number of settles that have been sent, but not yet committed
	// to the commitment.
	//
	// TODO(andrew.shvv) remove after we add additional BatchNumber()
	// method in state machine.
	batchCounter uint32

	// bestHeight is the best known height of the main chain. The link will
	// use this information to govern decisions based on HTLC timeouts.
	bestHeight uint32

	// keystoneBatch represents a volatile list of keystones that must be
	// written before attempting to sign the next commitment txn. These
	// represent all the HTLC's forwarded to the link from the switch. Once
	// we lock them into our outgoing commitment, then the circuit has a
	// keystone, and is fully opened.
	keystoneBatch []Keystone

	// openedCircuits is the set of all payment circuits that will be open
	// once we make our next commitment. After making the commitment we'll
	// ACK all these from our mailbox to ensure that they don't get
	// re-delivered if we reconnect.
	openedCircuits []CircuitKey

	// closedCircuits is the set of all payment circuits that will be
	// closed once we make our next commitment. After taking the commitment
	// we'll ACK all these to ensure that they don't get re-delivered if we
	// reconnect.
	closedCircuits []CircuitKey

	// channel is a lightning network channel to which we apply htlc
	// updates.
	channel *lnwallet.LightningChannel

	// shortChanID is the most up to date short channel ID for the link.
	shortChanID lnwire.ShortChannelID

	// cfg is a structure which carries all dependable fields/handlers
	// which may affect behaviour of the service.
	cfg ChannelLinkConfig

	// overflowQueue is used to store the htlc add updates which haven't
	// been processed because of the commitment transaction overflow.
	overflowQueue *packetQueue

	// startMailBox directs whether or not to start the mailbox when
	// starting the link. It may have already been started by the switch.
	startMailBox bool

	// mailBox is the main interface between the outside world and the
	// link. All incoming messages will be sent over this mailBox. Messages
	// include new updates from our connected peer, and new packets to be
	// forwarded sent by the switch.
	mailBox MailBox

	// upstream is a channel that new messages sent from the remote peer to
	// the local peer will be sent across.
	upstream chan lnwire.Message

	// downstream is a channel in which new multi-hop HTLC's to be
	// forwarded will be sent across. Messages from this channel are sent
	// by the HTLC switch.
	downstream chan *htlcPacket

	// linkControl is a channel which is used to query the state of the
	// link, or update various policies used which govern if an HTLC is to
	// be forwarded and/or accepted.
	linkControl chan interface{}

	// htlcUpdates is a channel that we'll use to update outside
	// sub-systems with the latest set of active HTLC's on our channel.
	htlcUpdates chan []channeldb.HTLC

	// logCommitTimer is a timer which is sent upon if we go an interval
	// without receiving/sending a commitment update. It's role is to
	// ensure both chains converge to identical state in a timely manner.
	//
	// TODO(roasbeef): timer should be >> then RTT
	logCommitTimer *time.Timer
	logCommitTick  <-chan time.Time

	sync.RWMutex

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewChannelLink creates a new instance of a ChannelLink given a configuration
// and active channel that will be used to verify/apply updates to.
func NewChannelLink(cfg ChannelLinkConfig, channel *lnwallet.LightningChannel,
	currentHeight uint32) ChannelLink {

	return &channelLink{
		cfg:         cfg,
		channel:     channel,
		shortChanID: channel.ShortChanID(),
		linkControl: make(chan interface{}),
		// TODO(roasbeef): just do reserve here?
		logCommitTimer: time.NewTimer(300 * time.Millisecond),
		overflowQueue:  newPacketQueue(lnwallet.MaxHTLCNumber / 2),
		bestHeight:     currentHeight,
		htlcUpdates:    make(chan []channeldb.HTLC),
		quit:           make(chan struct{}),
	}
}

// A compile time check to ensure channelLink implements the ChannelLink
// interface.
var _ ChannelLink = (*channelLink)(nil)

// Start starts all helper goroutines required for the operation of the channel
// link.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Start() error {
	if !atomic.CompareAndSwapInt32(&l.started, 0, 1) {
		err := errors.Errorf("channel link(%v): already started", l)
		log.Warn(err)
		return err
	}

	log.Infof("ChannelLink(%v) is starting", l)

	// Before we start the link, we'll update the ChainArbitrator with the
	// set of new channel signals for this channel.
	//
	// TODO(roasbeef): split goroutines within channel arb to avoid
	go func() {
		err := l.cfg.UpdateContractSignals(&contractcourt.ContractSignals{
			HtlcUpdates: l.htlcUpdates,
			ShortChanID: l.channel.ShortChanID(),
		})
		if err != nil {
			log.Errorf("Unable to update signals for "+
				"ChannelLink(%v)", l)
		}
	}()

	l.mailBox.ResetMessages()
	l.overflowQueue.Start()

	l.wg.Add(1)
	go l.htlcManager()

	return nil
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Stop() {
	if !atomic.CompareAndSwapInt32(&l.shutdown, 0, 1) {
		log.Warnf("channel link(%v): already stopped", l)
		return
	}

	log.Infof("ChannelLink(%v) is stopping", l)

	if l.cfg.ChainEvents.Cancel != nil {
		l.cfg.ChainEvents.Cancel()
	}

	l.channel.Stop()

	l.overflowQueue.Stop()

	close(l.quit)
	l.wg.Wait()
}

// EligibleToForward returns a bool indicating if the channel is able to
// actively accept requests to forward HTLC's. We're able to forward HTLC's if
// we know the remote party's next revocation point. Otherwise, we can't
// initiate new channel state.
func (l *channelLink) EligibleToForward() bool {
	return l.channel.RemoteNextRevocation() != nil
}

// sampleNetworkFee samples the current fee rate on the network to get into the
// chain in a timely manner. The returned value is expressed in fee-per-kw, as
// this is the native rate used when computing the fee for commitment
// transactions, and the second-level HTLC transactions.
func (l *channelLink) sampleNetworkFee() (lnwallet.SatPerKWeight, error) {
	// We'll first query for the sat/vbyte recommended to be confirmed
	// within 3 blocks.
	feePerVSize, err := l.cfg.FeeEstimator.EstimateFeePerVSize(3)
	if err != nil {
		return 0, err
	}

	// Once we have this fee rate, we'll convert to sat-per-kw.
	feePerKw := feePerVSize.FeePerKWeight()

	log.Debugf("ChannelLink(%v): sampled fee rate for 3 block conf: %v "+
		"sat/kw", l, int64(feePerKw))

	return feePerKw, nil
}

// shouldAdjustCommitFee returns true if we should update our commitment fee to
// match that of the network fee. We'll only update our commitment fee if the
// network fee is +/- 10% to our network fee.
func shouldAdjustCommitFee(netFee, chanFee lnwallet.SatPerKWeight) bool {
	switch {
	// If the network fee is greater than the commitment fee, then we'll
	// switch to it if it's at least 10% greater than the commit fee.
	case netFee > chanFee && netFee >= (chanFee+(chanFee*10)/100):
		return true

	// If the network fee is less than our commitment fee, then we'll
	// switch to it if it's at least 10% less than the commitment fee.
	case netFee < chanFee && netFee <= (chanFee-(chanFee*10)/100):
		return true

	// Otherwise, we won't modify our fee.
	default:
		return false
	}
}

// syncChanState attempts to synchronize channel states with the remote party.
// This method is to be called upon reconnection after the initial funding
// flow. We'll compare out commitment chains with the remote party, and re-send
// either a danging commit signature, a revocation, or both.
func (l *channelLink) syncChanStates() error {
	log.Infof("Attempting to re-resynchronize ChannelPoint(%v)",
		l.channel.ChannelPoint())

	// First, we'll generate our ChanSync message to send to the other
	// side. Based on this message, the remote party will decide if they
	// need to retransmit any data or not.
	localChanSyncMsg, err := l.channel.ChanSyncMsg()
	if err != nil {
		return fmt.Errorf("unable to generate chan sync message for "+
			"ChannelPoint(%v)", l.channel.ChannelPoint())
	}
	if err := l.cfg.Peer.SendMessage(localChanSyncMsg); err != nil {
		return fmt.Errorf("Unable to send chan sync message for "+
			"ChannelPoint(%v)", l.channel.ChannelPoint())
	}

	var msgsToReSend []lnwire.Message

	// Next, we'll wait to receive the ChanSync message with a timeout
	// period. The first message sent MUST be the ChanSync message,
	// otherwise, we'll terminate the connection.
	chanSyncDeadline := time.After(time.Second * 30)
	select {
	case msg := <-l.upstream:
		remoteChanSyncMsg, ok := msg.(*lnwire.ChannelReestablish)
		if !ok {
			return fmt.Errorf("first message sent to sync "+
				"should be ChannelReestablish, instead "+
				"received: %T", msg)
		}

		// If the remote party indicates that they think we haven't
		// done any state updates yet, then we'll retransmit the
		// funding locked message first. We do this, as at this point
		// we can't be sure if they've really received the
		// FundingLocked message.
		if remoteChanSyncMsg.NextLocalCommitHeight == 1 &&
			localChanSyncMsg.NextLocalCommitHeight == 1 &&
			!l.channel.IsPending() {

			log.Infof("ChannelPoint(%v): resending "+
				"FundingLocked message to peer",
				l.channel.ChannelPoint())

			nextRevocation, err := l.channel.NextRevocationKey()
			if err != nil {
				return fmt.Errorf("unable to create next "+
					"revocation: %v", err)
			}

			fundingLockedMsg := lnwire.NewFundingLocked(
				l.ChanID(), nextRevocation,
			)
			err = l.cfg.Peer.SendMessage(fundingLockedMsg)
			if err != nil {
				return fmt.Errorf("unable to re-send "+
					"FundingLocked: %v", err)
			}
		}

		// In any case, we'll then process their ChanSync message.
		log.Infof("Received re-establishment message from remote side "+
			"for channel(%v)", l.channel.ChannelPoint())

		var (
			openedCircuits []CircuitKey
			closedCircuits []CircuitKey
		)

		// We've just received a ChanSync message from the remote
		// party, so we'll process the message  in order to determine
		// if we need to re-transmit any messages to the remote party.
		msgsToReSend, openedCircuits, closedCircuits, err =
			l.channel.ProcessChanSyncMsg(remoteChanSyncMsg)
		if err != nil {
			// TODO(roasbeef): check concrete type of error, act
			// accordingly
			return fmt.Errorf("unable to handle upstream reestablish "+
				"message: %v", err)
		}

		// Repopulate any identifiers for circuits that may have been
		// opened or unclosed. This may happen if we needed to
		// retransmit a commitment signature message.
		l.openedCircuits = openedCircuits
		l.closedCircuits = closedCircuits

		// Ensure that all packets have been have been removed from the
		// link's mailbox.
		if err := l.ackDownStreamPackets(true); err != nil {
			return err
		}

		if len(msgsToReSend) > 0 {
			log.Infof("Sending %v updates to synchronize the "+
				"state for ChannelPoint(%v)", len(msgsToReSend),
				l.channel.ChannelPoint())
		}

		// If we have any messages to retransmit, we'll do so
		// immediately so we return to a synchronized state as soon as
		// possible.
		for _, msg := range msgsToReSend {
			l.cfg.Peer.SendMessage(msg)
		}

	case <-l.quit:
		return fmt.Errorf("shutting down")

	case <-chanSyncDeadline:
		return fmt.Errorf("didn't receive ChannelReestablish before " +
			"deadline")
	}

	return nil
}

// resolveFwdPkgs loads any forwarding packages for this link from disk, and
// reprocesses them in order. The primary goal is to make sure that any HTLCs
// we previously received are reinstated in memory, and forwarded to the switch
// if necessary. After a restart, this will also delete any previously
// completed packages.
func (l *channelLink) resolveFwdPkgs() error {
	fwdPkgs, err := l.channel.LoadFwdPkgs()
	if err != nil {
		return err
	}

	l.debugf("loaded %d fwd pks", len(fwdPkgs))

	var needUpdate bool
	for _, fwdPkg := range fwdPkgs {
		hasUpdate, err := l.resolveFwdPkg(fwdPkg)
		if err != nil {
			return err
		}

		needUpdate = needUpdate || hasUpdate
	}

	// If any of our reprocessing steps require an update to the commitment
	// txn, we initiate a state transition to capture all relevant changes.
	if needUpdate {
		return l.updateCommitTx()
	}

	return nil
}

// resolveFwdPkg interprets the FwdState of the provided package, either
// reprocesses any outstanding htlcs in the package, or performs garbage
// collection on the package.
func (l *channelLink) resolveFwdPkg(fwdPkg *channeldb.FwdPkg) (bool, error) {
	// Remove any completed packages to clear up space.
	if fwdPkg.State == channeldb.FwdStateCompleted {
		l.debugf("removing completed fwd pkg for height=%d",
			fwdPkg.Height)

		err := l.channel.RemoveFwdPkg(fwdPkg.Height)
		if err != nil {
			l.errorf("unable to remove fwd pkg for height=%d: %v",
				fwdPkg.Height, err)
			return false, err
		}
	}

	// Otherwise this is either a new package or one has gone through
	// processing, but contains htlcs that need to be restored in memory.
	// We replay this forwarding package to make sure our local mem state
	// is resurrected, we mimic any original responses back to the remote
	// party, and re-forward the relevant HTLCs to the switch.

	// If the package is fully acked but not completed, it must still have
	// settles and fails to propagate.
	if !fwdPkg.SettleFailFilter.IsFull() {
		settleFails := lnwallet.PayDescsFromRemoteLogUpdates(
			fwdPkg.Source, fwdPkg.Height, fwdPkg.SettleFails,
		)
		l.processRemoteSettleFails(fwdPkg, settleFails)
	}

	// Finally, replay *ALL ADDS* in this forwarding package. The
	// downstream logic is able to filter out any duplicates, but we must
	// shove the entire, original set of adds down the pipeline so that the
	// batch of adds presented to the sphinx router does not ever change.
	var needUpdate bool
	if !fwdPkg.AckFilter.IsFull() {
		adds := lnwallet.PayDescsFromRemoteLogUpdates(
			fwdPkg.Source, fwdPkg.Height, fwdPkg.Adds,
		)
		needUpdate = l.processRemoteAdds(fwdPkg, adds)
	}

	return needUpdate, nil
}

// fwdPkgGarbager periodically reads all forwarding packages from disk and
// removes those that can be discarded. It is safe to do this entirely in the
// background, since all state is coordinated on disk. This also ensures the
// link can continue to process messages and interleave database accesses.
//
// NOTE: This MUST be run as a goroutine.
func (l *channelLink) fwdPkgGarbager() {
	defer l.wg.Done()

	fwdPkgGcTick := l.cfg.FwdPkgGCTicker.Start()
	defer l.cfg.FwdPkgGCTicker.Stop()

	for {
		select {
		case <-fwdPkgGcTick:
			fwdPkgs, err := l.channel.LoadFwdPkgs()
			if err != nil {
				l.warnf("unable to load fwdpkgs for gc: %v", err)
				continue
			}

			// TODO(conner): batch removal of forward packages.
			for _, fwdPkg := range fwdPkgs {
				if fwdPkg.State != channeldb.FwdStateCompleted {
					continue
				}

				err = l.channel.RemoveFwdPkg(fwdPkg.Height)
				if err != nil {
					l.warnf("unable to remove fwd pkg "+
						"for height=%d: %v",
						fwdPkg.Height, err)
				}
			}
		case <-l.quit:
			return
		}
	}
}

// htlcManager is the primary goroutine which drives a channel's commitment
// update state-machine in response to messages received via several channels.
// This goroutine reads messages from the upstream (remote) peer, and also from
// downstream channel managed by the channel link. In the event that an htlc
// needs to be forwarded, then send-only forward handler is used which sends
// htlc packets to the switch. Additionally, the this goroutine handles acting
// upon all timeouts for any active HTLCs, manages the channel's revocation
// window, and also the htlc trickle queue+timer for this active channels.
//
// NOTE: This MUST be run as a goroutine.
func (l *channelLink) htlcManager() {
	defer func() {
		l.wg.Done()
		l.cfg.BlockEpochs.Cancel()
		log.Infof("ChannelLink(%v) has exited", l)
	}()

	log.Infof("HTLC manager for ChannelPoint(%v) started, "+
		"bandwidth=%v", l.channel.ChannelPoint(), l.Bandwidth())

	// Before handling any messages, revert any circuits that were marked
	// open in the switch's circuit map, but did not make it into a
	// commitment txn. We use the next local htlc index as the cut off
	// point, since all indexes below that are committed.
	//
	// NOTE: This is automatically done by the switch when it starts up,
	// but is necessary to prevent inconsistencies in the case that the
	// link flaps. This is a result of a link's life-cycle being shorter
	// than that of the switch.
	localHtlcIndex := l.channel.LocalHtlcIndex()
	err := l.cfg.Circuits.TrimOpenCircuits(l.ShortChanID(), localHtlcIndex)
	if err != nil {
		l.errorf("unable to trim circuits above local htlc index %d: %v",
			localHtlcIndex, err)
		l.fail(ErrInternalLinkFailure.Error())
		return
	}

	// TODO(roasbeef): need to call wipe chan whenever D/C?

	// If this isn't the first time that this channel link has been
	// created, then we'll need to check to see if we need to
	// re-synchronize state with the remote peer. settledHtlcs is a map of
	// HTLC's that we re-settled as part of the channel state sync.
	if l.cfg.SyncStates {
		if err := l.syncChanStates(); err != nil {
			l.errorf("unable to synchronize channel states: %v", err)
			l.fail(err.Error())
			return
		}
	}

	// With the channel states synced, we now reset the mailbox to ensure
	// we start processing all unacked packets in order. This is done here
	// to ensure that all acknowledgments that occur during channel
	// resynchronization have taken affect, causing us only to pull unacked
	// packets after starting to read from the downstream mailbox.
	l.mailBox.ResetPackets()

	// After cleaning up any memory pertaining to incoming packets, we now
	// replay our forwarding packages to handle any htlcs that can be
	// processed locally, or need to be forwarded out to the switch.
	if err := l.resolveFwdPkgs(); err != nil {
		l.errorf("unable to resolve fwd pkgs: %v", err)
		l.fail(ErrInternalLinkFailure.Error())
		return
	}

	// With our link's in-memory state fully reconstructed, spawn a
	// goroutine to manage the reclamation of disk space occupied by
	// completed forwarding packages.
	l.wg.Add(1)
	go l.fwdPkgGarbager()

	batchTick := l.cfg.BatchTicker.Start()
	defer l.cfg.BatchTicker.Stop()

	// TODO(roasbeef): fail chan in case of protocol violation
out:
	for {
		select {

		// A new block has arrived, we'll check the network fee to see
		// if we should adjust our commitment fee, and also update our
		// track of the best current height.
		case blockEpoch, ok := <-l.cfg.BlockEpochs.Epochs:
			if !ok {
				break out
			}

			l.bestHeight = uint32(blockEpoch.Height)

			// If we're not the initiator of the channel, don't we
			// don't control the fees, so we can ignore this.
			if !l.channel.IsInitiator() {
				continue
			}

			// If we are the initiator, then we'll sample the
			// current fee rate to get into the chain within 3
			// blocks.
			feePerKw, err := l.sampleNetworkFee()
			if err != nil {
				log.Errorf("unable to sample network fee: %v", err)
				continue
			}

			// We'll check to see if we should update the fee rate
			// based on our current set fee rate.
			commitFee := l.channel.CommitFeeRate()
			if !shouldAdjustCommitFee(feePerKw, commitFee) {
				continue
			}

			// If we do, then we'll send a new UpdateFee message to
			// the remote party, to be locked in with a new update.
			if err := l.updateChannelFee(feePerKw); err != nil {
				log.Errorf("unable to update fee rate: %v", err)
				continue
			}

		// The underlying channel has notified us of a unilateral close
		// carried out by the remote peer. In the case of such an
		// event, we'll wipe the channel state from the peer, and mark
		// the contract as fully settled. Afterwards we can exit.
		//
		// TODO(roasbeef): add force closure? also breach?
		case <-l.cfg.ChainEvents.UnilateralClosure:
			log.Warnf("Remote peer has closed ChannelPoint(%v) on-chain",
				l.channel.ChannelPoint())

			// TODO(roasbeef): remove all together
			go func() {
				chanPoint := l.channel.ChannelPoint()
				err := l.cfg.Peer.WipeChannel(chanPoint)
				if err != nil {
					log.Errorf("unable to wipe channel %v", err)
				}
			}()

			break out

		case <-l.logCommitTick:
			// If we haven't sent or received a new commitment
			// update in some time, check to see if we have any
			// pending updates we need to commit due to our
			// commitment chains being desynchronized.
			if l.channel.FullySynced() {
				continue
			}

			if err := l.updateCommitTx(); err != nil {
				l.fail("unable to update commitment: %v", err)
				break out
			}

		case <-batchTick:
			// If the current batch is empty, then we have no work
			// here.
			if l.batchCounter == 0 {
				continue
			}

			// Otherwise, attempt to extend the remote commitment
			// chain including all the currently pending entries.
			// If the send was unsuccessful, then abandon the
			// update, waiting for the revocation window to open
			// up.
			if err := l.updateCommitTx(); err != nil {
				l.fail("unable to update commitment: %v", err)
				break out
			}

		// A packet that previously overflowed the commitment
		// transaction is now eligible for processing once again. So
		// we'll attempt to re-process the packet in order to allow it
		// to continue propagating within the network.
		case packet := <-l.overflowQueue.outgoingPkts:
			msg := packet.htlc.(*lnwire.UpdateAddHTLC)
			log.Tracef("Reprocessing downstream add update "+
				"with payment hash(%x)", msg.PaymentHash[:])

			l.handleDownStreamPkt(packet, true)

		// A message from the switch was just received. This indicates
		// that the link is an intermediate hop in a multi-hop HTLC
		// circuit.
		case pkt := <-l.downstream:
			// If we have non empty processing queue then we'll add
			// this to the overflow rather than processing it
			// directly. Once an active HTLC is either settled or
			// failed, then we'll free up a new slot.
			htlc, ok := pkt.htlc.(*lnwire.UpdateAddHTLC)
			if ok && l.overflowQueue.Length() != 0 {
				log.Infof("Downstream htlc add update with "+
					"payment hash(%x) have been added to "+
					"reprocessing queue, batch_size=%v",
					htlc.PaymentHash[:],
					l.batchCounter)

				l.overflowQueue.AddPkt(pkt)
				continue
			}

			l.handleDownStreamPkt(pkt, false)

		// A message from the connected peer was just received. This
		// indicates that we have a new incoming HTLC, either directly
		// for us, or part of a multi-hop HTLC circuit.
		case msg := <-l.upstream:
			l.handleUpstreamMsg(msg)

		// TODO(roasbeef): make distinct goroutine to handle?
		case cmd := <-l.linkControl:

			switch req := cmd.(type) {
			case *policyUpdate:
				// In order to avoid overriding a valid policy
				// with a "null" field in the new policy, we'll
				// only update to the set sub policy if the new
				// value isn't uninitialized.
				if req.policy.BaseFee != 0 {
					l.cfg.FwrdingPolicy.BaseFee = req.policy.BaseFee
				}
				if req.policy.FeeRate != 0 {
					l.cfg.FwrdingPolicy.FeeRate = req.policy.FeeRate
				}
				if req.policy.TimeLockDelta != 0 {
					l.cfg.FwrdingPolicy.TimeLockDelta = req.policy.TimeLockDelta
				}

				if req.done != nil {
					close(req.done)
				}
			}

		case <-l.quit:
			break out
		}
	}
}

// handleDownStreamPkt processes an HTLC packet sent from the downstream HTLC
// Switch. Possible messages sent by the switch include requests to forward new
// HTLCs, timeout previously cleared HTLCs, and finally to settle currently
// cleared HTLCs with the upstream peer.
//
// TODO(roasbeef): add sync ntfn to ensure switch always has consistent view?
func (l *channelLink) handleDownStreamPkt(pkt *htlcPacket, isReProcess bool) {
	var isSettle bool
	switch htlc := pkt.htlc.(type) {
	case *lnwire.UpdateAddHTLC:

		// A new payment has been initiated via the downstream channel,
		// so we add the new HTLC to our local log, then update the
		// commitment chains.
		htlc.ChanID = l.ChanID()
		openCircuitRef := pkt.inKey()
		index, err := l.channel.AddHTLC(htlc, &openCircuitRef)
		if err != nil {
			switch err {

			// The channels spare bandwidth is fully allocated, so
			// we'll put this HTLC into the overflow queue.
			case lnwallet.ErrMaxHTLCNumber:
				l.infof("Downstream htlc add update with "+
					"payment hash(%x) have been added to "+
					"reprocessing queue, batch: %v",
					htlc.PaymentHash[:],
					l.batchCounter)

				l.overflowQueue.AddPkt(pkt)
				return

			// The HTLC was unable to be added to the state
			// machine, as a result, we'll signal the switch to
			// cancel the pending payment.
			default:
				l.warnf("Unable to handle downstream add HTLC: %v", err)

				var (
					localFailure = false
					reason       lnwire.OpaqueReason
				)

				failure := lnwire.NewTemporaryChannelFailure(nil)

				// Encrypt the error back to the source unless the payment was
				// generated locally.
				if pkt.obfuscator == nil {
					var b bytes.Buffer
					err := lnwire.EncodeFailure(&b, failure, 0)
					if err != nil {
						l.errorf("unable to encode failure: %v", err)
						return
					}
					reason = lnwire.OpaqueReason(b.Bytes())
					localFailure = true
				} else {
					var err error
					reason, err = pkt.obfuscator.EncryptFirstHop(failure)
					if err != nil {
						l.errorf("unable to obfuscate error: %v", err)
						return
					}
				}

				failPkt := &htlcPacket{
					incomingChanID: pkt.incomingChanID,
					incomingHTLCID: pkt.incomingHTLCID,
					circuit:        pkt.circuit,
					sourceRef:      pkt.sourceRef,
					hasSource:      true,
					localFailure:   localFailure,
					htlc: &lnwire.UpdateFailHTLC{
						Reason: reason,
					},
				}

				go l.forwardBatch(failPkt)

				// Remove this packet from the link's mailbox,
				// this prevents it from being reprocessed if
				// the link restarts and resets it mailbox. If
				// this response doesn't make it back to the
				// originating link, it will be rejected upon
				// attempting to reforward the Add to the
				// switch, since the circuit was never fully
				// opened, and the forwarding package shows it
				// as unacknowledged.
				l.mailBox.AckPacket(pkt.inKey())

				return
			}
		}

		l.tracef("Received downstream htlc: payment_hash=%x, "+
			"local_log_index=%v, batch_size=%v",
			htlc.PaymentHash[:], index, l.batchCounter+1)

		pkt.outgoingChanID = l.ShortChanID()
		pkt.outgoingHTLCID = index
		htlc.ID = index

		l.debugf("Queueing keystone of ADD open circuit: %s->%s",
			pkt.inKey(), pkt.outKey())

		l.openedCircuits = append(l.openedCircuits, pkt.inKey())
		l.keystoneBatch = append(l.keystoneBatch, pkt.keystone())

		l.cfg.Peer.SendMessage(htlc)

	case *lnwire.UpdateFulfillHTLC:
		// An HTLC we forward to the switch has just settled somewhere
		// upstream. Therefore we settle the HTLC within the our local
		// state machine.
		closedCircuitRef := pkt.inKey()
		if err := l.channel.SettleHTLC(
			htlc.PaymentPreimage,
			pkt.incomingHTLCID,
			pkt.sourceRef,
			pkt.destRef,
			&closedCircuitRef,
		); err != nil {
			// TODO(roasbeef): broadcast on-chain
			l.fail("unable to settle incoming HTLC: %v", err)
			return
		}

		l.debugf("Queueing removal of SETTLE closed circuit: %s->%s",
			pkt.inKey(), pkt.outKey())

		l.closedCircuits = append(l.closedCircuits, pkt.inKey())

		// With the HTLC settled, we'll need to populate the wire
		// message to target the specific channel and HTLC to be
		// cancelled.
		htlc.ChanID = l.ChanID()
		htlc.ID = pkt.incomingHTLCID

		// Then we send the HTLC settle message to the connected peer
		// so we can continue the propagation of the settle message.
		l.cfg.Peer.SendMessage(htlc)
		isSettle = true

	case *lnwire.UpdateFailHTLC:
		// An HTLC cancellation has been triggered somewhere upstream,
		// we'll remove then HTLC from our local state machine.
		closedCircuitRef := pkt.inKey()
		if err := l.channel.FailHTLC(
			pkt.incomingHTLCID,
			htlc.Reason,
			pkt.sourceRef,
			pkt.destRef,
			&closedCircuitRef,
		); err != nil {
			log.Errorf("unable to cancel HTLC: %v", err)
			return
		}

		l.debugf("Queueing removal of FAIL closed circuit: %s->%s",
			pkt.inKey(), pkt.outKey())

		l.closedCircuits = append(l.closedCircuits, pkt.inKey())

		// With the HTLC removed, we'll need to populate the wire
		// message to target the specific channel and HTLC to be
		// cancelled. The "Reason" field will have already been set
		// within the switch.
		htlc.ChanID = l.ChanID()
		htlc.ID = pkt.incomingHTLCID

		// Finally, we send the HTLC message to the peer which
		// initially created the HTLC.
		l.cfg.Peer.SendMessage(htlc)
		isSettle = true
	}

	l.batchCounter++

	// If this newly added update exceeds the min batch size for adds, or
	// this is a settle request, then initiate an update.
	if l.batchCounter >= l.cfg.BatchSize || isSettle {
		if err := l.updateCommitTx(); err != nil {
			l.fail("unable to update commitment: %v", err)
			return
		}
	}
}

// handleUpstreamMsg processes wire messages related to commitment state
// updates from the upstream peer. The upstream peer is the peer whom we have a
// direct channel with, updating our respective commitment chains.
func (l *channelLink) handleUpstreamMsg(msg lnwire.Message) {
	switch msg := msg.(type) {

	case *lnwire.UpdateAddHTLC:
		// We just received an add request from an upstream peer, so we
		// add it to our state machine, then add the HTLC to our
		// "settle" list in the event that we know the preimage.
		index, err := l.channel.ReceiveHTLC(msg)
		if err != nil {
			l.fail("unable to handle upstream add HTLC: %v", err)
			return
		}

		l.tracef("Receive upstream htlc with payment hash(%x), "+
			"assigning index: %v", msg.PaymentHash[:], index)

	case *lnwire.UpdateFulfillHTLC:
		pre := msg.PaymentPreimage
		idx := msg.ID
		if err := l.channel.ReceiveHTLCSettle(pre, idx); err != nil {
			// TODO(roasbeef): broadcast on-chain
			l.fail("unable to handle upstream settle HTLC: %v", err)
			return
		}

		// TODO(roasbeef): pipeline to switch

		// As we've learned of a new preimage for the first time, we'll
		// add it to to our preimage cache. By doing this, we ensure
		// any contested contracts watched by any on-chain arbitrators
		// can now sweep this HTLC on-chain.
		go func() {
			err := l.cfg.PreimageCache.AddPreimage(pre[:])
			if err != nil {
				l.errorf("unable to add preimage=%x to "+
					"cache", pre[:])
			}
		}()

	case *lnwire.UpdateFailMalformedHTLC:
		// Convert the failure type encoded within the HTLC fail
		// message to the proper generic lnwire error code.
		var failure lnwire.FailureMessage
		switch msg.FailureCode {
		case lnwire.CodeInvalidOnionVersion:
			failure = &lnwire.FailInvalidOnionVersion{
				OnionSHA256: msg.ShaOnionBlob,
			}
		case lnwire.CodeInvalidOnionHmac:
			failure = &lnwire.FailInvalidOnionHmac{
				OnionSHA256: msg.ShaOnionBlob,
			}

		case lnwire.CodeInvalidOnionKey:
			failure = &lnwire.FailInvalidOnionKey{
				OnionSHA256: msg.ShaOnionBlob,
			}
		default:
			log.Errorf("Unknown failure code: %v", msg.FailureCode)
			failure = &lnwire.FailTemporaryChannelFailure{}
		}

		// With the error parsed, we'll convert the into it's opaque
		// form.
		var b bytes.Buffer
		if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
			l.errorf("unable to encode malformed error: %v", err)
			return
		}

		// If remote side have been unable to parse the onion blob we
		// have sent to it, than we should transform the malformed HTLC
		// message to the usual HTLC fail message.
		err := l.channel.ReceiveFailHTLC(msg.ID, b.Bytes())
		if err != nil {
			l.fail("unable to handle upstream fail HTLC: %v", err)
			return
		}

	case *lnwire.UpdateFailHTLC:
		idx := msg.ID
		err := l.channel.ReceiveFailHTLC(idx, msg.Reason[:])
		if err != nil {
			l.fail("unable to handle upstream fail HTLC: %v", err)
			return
		}

	case *lnwire.CommitSig:
		// We just received a new updates to our local commitment
		// chain, validate this new commitment, closing the link if
		// invalid.
		err := l.channel.ReceiveNewCommitment(msg.CommitSig, msg.HtlcSigs)
		if err != nil {
			// If we were unable to reconstruct their proposed
			// commitment, then we'll examine the type of error. If
			// it's an InvalidCommitSigError, then we'll send a
			// direct error.
			//
			// TODO(roasbeef): force close chan
			if _, ok := err.(*lnwallet.InvalidCommitSigError); ok {
				l.cfg.Peer.SendMessage(&lnwire.Error{
					ChanID: l.ChanID(),
					Data:   []byte(err.Error()),
				})
			}

			l.fail("ChannelPoint(%v): unable to accept new "+
				"commitment: %v", l.channel.ChannelPoint(), err)
			return
		}

		// As we've just just accepted a new state, we'll now
		// immediately send the remote peer a revocation for our prior
		// state.
		nextRevocation, currentHtlcs, err := l.channel.RevokeCurrentCommitment()
		if err != nil {
			log.Errorf("unable to revoke commitment: %v", err)
			return
		}
		l.cfg.Peer.SendMessage(nextRevocation)

		// Since we just revoked our commitment, we may have a new set
		// of HTLC's on our commitment, so we'll send them over our
		// HTLC update channel so any callers can be notified.
		select {
		case l.htlcUpdates <- currentHtlcs:
		case <-l.quit:
			return
		}

		// As we've just received a commitment signature, we'll
		// re-start the log commit timer to wake up the main processing
		// loop to check if we need to send a commitment signature as
		// we owe one.
		//
		// TODO(roasbeef): instead after revocation?
		if !l.logCommitTimer.Stop() {
			select {
			case <-l.logCommitTimer.C:
			default:
			}
		}
		l.logCommitTimer.Reset(300 * time.Millisecond)
		l.logCommitTick = l.logCommitTimer.C

		// If both commitment chains are fully synced from our PoV,
		// then we don't need to reply with a signature as both sides
		// already have a commitment with the latest accepted l.
		if l.channel.FullySynced() {
			return
		}

		// Otherwise, the remote party initiated the state transition,
		// so we'll reply with a signature to provide them with their
		// version of the latest commitment.
		if err := l.updateCommitTx(); err != nil {
			l.fail("unable to update commitment: %v", err)
			return
		}

	case *lnwire.RevokeAndAck:
		// We've received a revocation from the remote chain, if valid,
		// this moves the remote chain forward, and expands our
		// revocation window.
		fwdPkg, adds, settleFails, err := l.channel.ReceiveRevocation(msg)
		if err != nil {
			l.fail("unable to accept revocation: %v", err)
			return
		}

		l.processRemoteSettleFails(fwdPkg, settleFails)

		needUpdate := l.processRemoteAdds(fwdPkg, adds)
		if needUpdate {
			if err := l.updateCommitTx(); err != nil {
				l.fail("unable to update commitment: %v", err)
				return
			}
		}

	case *lnwire.UpdateFee:
		// We received fee update from peer. If we are the initiator we
		// will fail the channel, if not we will apply the update.
		fee := lnwallet.SatPerKWeight(msg.FeePerKw)
		if err := l.channel.ReceiveUpdateFee(fee); err != nil {
			l.fail("error receiving fee update: %v", err)
			return
		}
	case *lnwire.Error:
		// Error received from remote, MUST fail channel, but should
		// only print the contents of the error message if all
		// characters are printable ASCII.
		errMsg := "non-ascii data"
		if isASCII(msg.Data) {
			errMsg = string(msg.Data)
		}
		l.fail("ChannelPoint(%v): recieved error from peer: %v",
			l.channel.ChannelPoint(), errMsg)
	default:
		log.Warnf("ChannelPoint(%v): received unknown message of type %T",
			l.channel.ChannelPoint(), msg)
	}

}

// ackDownStreamPackets is responsible for removing htlcs from a link's mailbox
// for packets delivered from server, and cleaning up any circuits closed by
// signing a previous commitment txn. This method ensures that the circuits are
// removed from the circuit map before removing them from the link's mailbox,
// otherwise it could be possible for some circuit to be missed if this link
// flaps.
//
// The `forgive` flag allows this method to tolerate restarts, and ignores
// errors that could be caused by a previous circuit deletion. Under normal
// operation, this is set to false so that we would fail the link if we were
// unable to remove a circuit.
func (l *channelLink) ackDownStreamPackets(forgive bool) error {
	// First, remove the downstream Add packets that were included in the
	// previous commitment signature. This will prevent the Adds from being
	// replayed if this link disconnects.
	for _, inKey := range l.openedCircuits {
		// In order to test the sphinx replay logic of the remote
		// party, unsafe replay does not acknowledge the packets from
		// the mailbox. We can then force a replay of any Add packets
		// held in memory by disconnecting and reconnecting the link.
		if l.cfg.UnsafeReplay {
			continue
		}

		l.debugf("removing Add packet %s from mailbox", inKey)
		l.mailBox.AckPacket(inKey)
	}

	// Now, we will delete all circuits closed by the previous commitment
	// signature, which is the result of downstream Settle/Fail packets. We
	// batch them here to ensure circuits are closed atomically and for
	// performance.
	err := l.cfg.Circuits.DeleteCircuits(l.closedCircuits...)
	switch err {
	case nil:
		// Successful deletion.

	case ErrUnknownCircuit:
		if forgive {
			// After a restart, we may have already removed this
			// circuit. Since it shouldn't be possible for a
			// circuit to be closed by different htlcs, we assume
			// this error signals that the whole batch was
			// successfully removed.
			l.warnf("forgiving unknown circuit error after " +
				"attempting deletion, circuit was probably " +
				"removed before shutting down.")
			break
		}

		return err

	default:
		l.errorf("unable to delete %d circuits: %v",
			len(l.closedCircuits), err)
		return err
	}

	// With the circuits removed from memory and disk, we now ack any
	// Settle/Fails in the mailbox to ensure they do not get redelivered
	// after startup. If forgive is enabled and we've reached this point,
	// the circuits must have been removed at some point, so it is now safe
	// to un-queue the corresponding Settle/Fails.
	for _, inKey := range l.closedCircuits {
		l.debugf("removing Fail/Settle packet %s from mailbox", inKey)
		l.mailBox.AckPacket(inKey)
	}

	// Lastly, reset our buffers to be empty while keeping any acquired
	// growth in the backing array.
	l.openedCircuits = l.openedCircuits[:0]
	l.closedCircuits = l.closedCircuits[:0]

	return nil
}

// updateCommitTx signs, then sends an update to the remote peer adding a new
// commitment to their commitment chain which includes all the latest updates
// we've received+processed up to this point.
func (l *channelLink) updateCommitTx() error {
	// Preemptively write all pending keystones to disk, just in case the
	// HTLCs we have in memory are included in the subsequent attempt to
	// sign a commitment state.
	err := l.cfg.Circuits.OpenCircuits(l.keystoneBatch...)
	if err != nil {
		return err
	}

	// Reset the batch, but keep the backing buffer to avoid reallocating.
	l.keystoneBatch = l.keystoneBatch[:0]

	theirCommitSig, htlcSigs, err := l.channel.SignNextCommitment()
	if err == lnwallet.ErrNoWindow {
		l.tracef("revocation window exhausted, unable to send: %v, "+
			"dangling_opens=%v, dangling_closes%v",
			l.batchCounter, spew.Sdump(l.openedCircuits),
			spew.Sdump(l.closedCircuits))
		return nil
	} else if err != nil {
		return err
	}

	if err := l.ackDownStreamPackets(false); err != nil {
		return err
	}

	commitSig := &lnwire.CommitSig{
		ChanID:    l.ChanID(),
		CommitSig: theirCommitSig,
		HtlcSigs:  htlcSigs,
	}
	l.cfg.Peer.SendMessage(commitSig)

	// We've just initiated a state transition, attempt to stop the
	// logCommitTimer. If the timer already ticked, then we'll consume the
	// value, dropping
	if l.logCommitTimer != nil && !l.logCommitTimer.Stop() {
		select {
		case <-l.logCommitTimer.C:
		default:
		}
	}
	l.logCommitTick = nil

	// Finally, clear our the current batch, so we can accurately make
	// further batch flushing decisions.
	l.batchCounter = 0

	return nil
}

// Peer returns the representation of remote peer with which we have the
// channel link opened.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Peer() Peer {
	return l.cfg.Peer
}

// ShortChanID returns the short channel ID for the channel link. The short
// channel ID encodes the exact location in the main chain that the original
// funding output can be found.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) ShortChanID() lnwire.ShortChannelID {
	return l.shortChanID
}

// UpdateShortChanID updates the short channel ID for a link. This may be
// required in the event that a link is created before the short chan ID for it
// is known, or a re-org occurs, and the funding transaction changes location
// within the chain.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) UpdateShortChanID(sid lnwire.ShortChannelID) {
	l.Lock()
	defer l.Unlock()

	log.Infof("Updating short chan ID for ChannelPoint(%v)", l)

	l.shortChanID = sid

	go func() {
		err := l.cfg.UpdateContractSignals(&contractcourt.ContractSignals{
			HtlcUpdates: l.htlcUpdates,
			ShortChanID: l.channel.ShortChanID(),
		})
		if err != nil {
			log.Errorf("Unable to update signals for "+
				"ChannelLink(%v)", l)
		}
	}()

	return
}

// ChanID returns the channel ID for the channel link. The channel ID is a more
// compact representation of a channel's full outpoint.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) ChanID() lnwire.ChannelID {
	return lnwire.NewChanIDFromOutPoint(l.channel.ChannelPoint())
}

// Bandwidth returns the total amount that can flow through the channel link at
// this given instance. The value returned is expressed in millisatoshi and can
// be used by callers when making forwarding decisions to determine if a link
// can accept an HTLC.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Bandwidth() lnwire.MilliSatoshi {
	channelBandwidth := l.channel.AvailableBalance()
	overflowBandwidth := l.overflowQueue.TotalHtlcAmount()

	// To compute the total bandwidth, we'll take the current available
	// bandwidth, then subtract the overflow bandwidth as we'll eventually
	// also need to evaluate those HTLC's once space on the commitment
	// transaction is free.
	linkBandwidth := channelBandwidth - overflowBandwidth

	// If the channel reserve is greater than the total available balance
	// of the link, just return 0.
	reserve := lnwire.NewMSatFromSatoshis(l.channel.LocalChanReserve())
	if linkBandwidth < reserve {
		return 0
	}

	// Else the amount that is available to flow through the link at this
	// point is the available balance minus the reserve amount we are
	// required to keep as collateral.
	return linkBandwidth - reserve
}

// AttachMailBox updates the current mailbox used by this link, and hooks up
// the mailbox's message and packet outboxes to the link's upstream and
// downstream chans, respectively.
func (l *channelLink) AttachMailBox(mailbox MailBox) {
	l.Lock()
	l.mailBox = mailbox
	l.upstream = mailbox.MessageOutBox()
	l.downstream = mailbox.PacketOutBox()
	l.Unlock()
}

// policyUpdate is a message sent to a channel link when an outside sub-system
// wishes to update the current forwarding policy.
type policyUpdate struct {
	policy ForwardingPolicy

	done chan struct{}
}

// UpdateForwardingPolicy updates the forwarding policy for the target
// ChannelLink. Once updated, the link will use the new forwarding policy to
// govern if it an incoming HTLC should be forwarded or not. Note that this
// processing of the new policy will ensure that uninitialized fields in the
// passed policy won't override already initialized fields in the current
// policy.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) UpdateForwardingPolicy(newPolicy ForwardingPolicy) {
	cmd := &policyUpdate{
		policy: newPolicy,
		done:   make(chan struct{}),
	}

	select {
	case l.linkControl <- cmd:
	case <-l.quit:
	}

	select {
	case <-cmd.done:
	case <-l.quit:
	}
}

// Stats returns the statistics of channel link.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Stats() (uint64, lnwire.MilliSatoshi, lnwire.MilliSatoshi) {
	snapshot := l.channel.StateSnapshot()

	return snapshot.ChannelCommitment.CommitHeight,
		snapshot.TotalMSatSent,
		snapshot.TotalMSatReceived
}

// String returns the string representation of channel link.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) String() string {
	return l.channel.ChannelPoint().String()
}

// HandleSwitchPacket handles the switch packets. This packets which might be
// forwarded to us from another channel link in case the htlc update came from
// another peer or if the update was created by user
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) HandleSwitchPacket(pkt *htlcPacket) error {
	l.tracef("received switch packet inkey=%v, outkey=%v",
		pkt.inKey(), pkt.outKey())

	l.mailBox.AddPacket(pkt)
	return nil
}

// HandleChannelUpdate handles the htlc requests as settle/add/fail which sent
// to us from remote peer we have a channel with.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) HandleChannelUpdate(message lnwire.Message) {
	l.mailBox.AddMessage(message)
}

// updateChannelFee updates the commitment fee-per-kw on this channel by
// committing to an update_fee message.
func (l *channelLink) updateChannelFee(feePerKw lnwallet.SatPerKWeight) error {

	log.Infof("ChannelPoint(%v): updating commit fee to %v sat/kw", l,
		feePerKw)

	// We skip sending the UpdateFee message if the channel is not
	// currently eligible to forward messages.
	if !l.EligibleToForward() {
		log.Debugf("ChannelPoint(%v): skipping fee update for "+
			"inactive channel", l.ChanID())
		return nil
	}

	// First, we'll update the local fee on our commitment.
	if err := l.channel.UpdateFee(feePerKw); err != nil {
		return err
	}

	// We'll then attempt to send a new UpdateFee message, and also lock it
	// in immediately by triggering a commitment update.
	msg := lnwire.NewUpdateFee(l.ChanID(), uint32(feePerKw))
	if err := l.cfg.Peer.SendMessage(msg); err != nil {
		return err
	}
	return l.updateCommitTx()
}

// processRemoteSettleFails accepts a batch of settle/fail payment descriptors
// after receiving a revocation from the remote party, and reprocesses them in
// the context of the provided forwarding package. Any settles or fails that
// have already been acknowledged in the forwarding package will not be sent to
// the switch.
func (l *channelLink) processRemoteSettleFails(fwdPkg *channeldb.FwdPkg,
	settleFails []*lnwallet.PaymentDescriptor) {

	if len(settleFails) == 0 {
		return
	}

	log.Debugf("ChannelLink(%v): settle-fail-filter %v",
		l.ShortChanID(), fwdPkg.SettleFailFilter)

	var switchPackets []*htlcPacket
	for i, pd := range settleFails {
		// Skip any settles or fails that have already been
		// acknowledged by the incoming link that originated the
		// forwarded Add.
		if fwdPkg.SettleFailFilter.Contains(uint16(i)) {
			continue
		}

		// TODO(roasbeef): rework log entries to a shared
		// interface.

		switch pd.EntryType {

		// A settle for an HTLC we previously forwarded HTLC has been
		// received. So we'll forward the HTLC to the switch which will
		// handle propagating the settle to the prior hop.
		case lnwallet.Settle:
			settlePacket := &htlcPacket{
				outgoingChanID: l.ShortChanID(),
				outgoingHTLCID: pd.ParentIndex,
				destRef:        pd.DestRef,
				htlc: &lnwire.UpdateFulfillHTLC{
					PaymentPreimage: pd.RPreimage,
				},
			}

			// Add the packet to the batch to be forwarded, and
			// notify the overflow queue that a spare spot has been
			// freed up within the commitment state.
			switchPackets = append(switchPackets, settlePacket)
			l.overflowQueue.SignalFreeSlot()

		// A failureCode message for a previously forwarded HTLC has
		// been received. As a result a new slot will be freed up in
		// our commitment state, so we'll forward this to the switch so
		// the backwards undo can continue.
		case lnwallet.Fail:
			// Fetch the reason the HTLC was cancelled so we can
			// continue to propagate it.
			failPacket := &htlcPacket{
				outgoingChanID: l.ShortChanID(),
				outgoingHTLCID: pd.ParentIndex,
				destRef:        pd.DestRef,
				htlc: &lnwire.UpdateFailHTLC{
					Reason: lnwire.OpaqueReason(pd.FailReason),
				},
			}

			// Add the packet to the batch to be forwarded, and
			// notify the overflow queue that a spare spot has been
			// freed up within the commitment state.
			switchPackets = append(switchPackets, failPacket)
			l.overflowQueue.SignalFreeSlot()
		}
	}

	go l.forwardBatch(switchPackets...)
}

// processRemoteAdds serially processes each of the Add payment descriptors
// which have been "locked-in" by receiving a revocation from the remote party.
// The forwarding package provided instructs how to process this batch,
// indicating whether this is the first time these Adds are being processed, or
// whether we are reprocessing as a result of a failure or restart. Adds that
// have already been acknowledged in the forwarding package will be ignored.
func (l *channelLink) processRemoteAdds(fwdPkg *channeldb.FwdPkg,
	lockedInHtlcs []*lnwallet.PaymentDescriptor) bool {

	l.tracef("processing %d remote adds for height %d",
		len(lockedInHtlcs), fwdPkg.Height)

	decodeReqs := make([]DecodeHopIteratorRequest, 0, len(lockedInHtlcs))
	for _, pd := range lockedInHtlcs {
		switch pd.EntryType {

		// TODO(conner): remove type switch?
		case lnwallet.Add:
			// Before adding the new htlc to the state machine,
			// parse the onion object in order to obtain the
			// routing information with DecodeHopIterator function
			// which process the Sphinx packet.
			onionReader := bytes.NewReader(pd.OnionBlob)

			req := DecodeHopIteratorRequest{
				OnionReader:  onionReader,
				RHash:        pd.RHash[:],
				IncomingCltv: pd.Timeout,
			}

			decodeReqs = append(decodeReqs, req)
		}
	}

	// Atomically decode the incoming htlcs, simultaneously checking for
	// replay attempts. A particular index in the returned, spare list of
	// channel iterators should only be used if the failure code at the
	// same index is lnwire.FailCodeNone.
	decodeResps, sphinxErr := l.cfg.DecodeHopIterators(
		fwdPkg.ID(), decodeReqs,
	)
	if sphinxErr != nil {
		l.errorf("unable to decode hop iterators: %v", sphinxErr)
		l.fail(ErrInternalLinkFailure.Error())
		return false
	}

	var (
		needUpdate    bool
		switchPackets []*htlcPacket
	)

	for i, pd := range lockedInHtlcs {
		idx := uint16(i)

		if fwdPkg.State == channeldb.FwdStateProcessed &&
			fwdPkg.AckFilter.Contains(idx) {

			// If this index is already found in the ack filter,
			// the response to this forwarding decision has already
			// been committed by one of our commitment txns. ADDs
			// in this state are waiting for the rest of the fwding
			// package to get acked before being garbage collected.
			continue
		}

		// An incoming HTLC add has been full-locked in. As a result we
		// can now examine the forwarding details of the HTLC, and the
		// HTLC itself to decide if: we should forward it, cancel it,
		// or are able to settle it (and it adheres to our fee related
		// constraints).

		// Fetch the onion blob that was included within this processed
		// payment descriptor.
		var onionBlob [lnwire.OnionPacketSize]byte
		copy(onionBlob[:], pd.OnionBlob)

		// Before adding the new htlc to the state machine, parse the
		// onion object in order to obtain the routing information with
		// DecodeHopIterator function which process the Sphinx packet.
		chanIterator, failureCode := decodeResps[i].Result()
		if failureCode != lnwire.CodeNone {
			// If we're unable to process the onion blob than we
			// should send the malformed htlc error to payment
			// sender.
			l.sendMalformedHTLCError(pd.HtlcIndex, failureCode,
				onionBlob[:], pd.SourceRef)
			needUpdate = true

			log.Errorf("unable to decode onion hop "+
				"iterator: %v", failureCode)
			continue
		}

		// Retrieve onion obfuscator from onion blob in order to
		// produce initial obfuscation of the onion failureCode.
		obfuscator, failureCode := chanIterator.ExtractErrorEncrypter(
			l.cfg.DecodeOnionObfuscator,
		)
		if failureCode != lnwire.CodeNone {
			// If we're unable to process the onion blob than we
			// should send the malformed htlc error to payment
			// sender.
			l.sendMalformedHTLCError(pd.HtlcIndex, failureCode,
				onionBlob[:], pd.SourceRef)
			needUpdate = true

			log.Errorf("unable to decode onion "+
				"obfuscator: %v", failureCode)
			continue
		}

		heightNow := l.bestHeight

		fwdInfo := chanIterator.ForwardingInstructions()
		switch fwdInfo.NextHop {
		case exitHop:
			if l.cfg.DebugHTLC && l.cfg.HodlHTLC {
				log.Warnf("hodl HTLC mode enabled, will not " +
					"attempt to settle HTLC with sender")
				continue
			}

			// First, we'll check the expiry of the HTLC itself
			// against, the current block height. If the timeout is
			// too soon, then we'll reject the HTLC.
			if pd.Timeout-expiryGraceDelta <= heightNow {
				log.Errorf("htlc(%x) has an expiry that's too "+
					"soon: expiry=%v, best_height=%v",
					pd.RHash[:], pd.Timeout, heightNow)

				failure := lnwire.FailFinalIncorrectCltvExpiry{}
				l.sendHTLCError(
					pd.HtlcIndex, &failure, obfuscator, pd.SourceRef,
				)
				needUpdate = true
				continue
			}

			// We're the designated payment destination.  Therefore
			// we attempt to see if we have an invoice locally
			// which'll allow us to settle this htlc.
			invoiceHash := chainhash.Hash(pd.RHash)
			invoice, err := l.cfg.Registry.LookupInvoice(invoiceHash)
			if err != nil {
				log.Errorf("unable to query invoice registry: "+
					" %v", err)
				failure := lnwire.FailUnknownPaymentHash{}
				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)

				needUpdate = true
				continue
			}

			// If the invoice is already settled, we choose to
			// accept the payment to simplify failure recovery.
			//
			// NOTE: Though our recovery and forwarding logic is
			// predominately batched, settling invoices happens
			// iteratively. We may reject one of of two payments
			// for the same rhash at first, but then restart and
			// reject both after seeing that the invoice has been
			// settled. Without any record of which one settles
			// first, it is ambiguous as to which one actually
			// settled the invoice. Thus, by accepting all
			// payments, we eliminate the race condition that can
			// lead to this inconsistency.
			//
			// TODO(conner): track ownership of settlements to
			// properly recover from failures? or add batch invoice
			// settlement
			if invoice.Terms.Settled {
				log.Warnf("Accepting duplicate payment for "+
					"hash=%x", pd.RHash[:])
			}

			// If we're not currently in debug mode, and the
			// extended htlc doesn't meet the value requested, then
			// we'll fail the htlc.  Otherwise, we settle this htlc
			// within our local state update log, then send the
			// update entry to the remote party.
			//
			// NOTE: We make an exception when the value requested
			// by the invoice is zero. This means the invoice
			// allows the payee to specify the amount of satoshis
			// they wish to send.  So since we expect the htlc to
			// have a different amount, we should not fail.
			if !l.cfg.DebugHTLC && invoice.Terms.Value > 0 &&
				pd.Amount < invoice.Terms.Value {

				log.Errorf("rejecting htlc due to incorrect "+
					"amount: expected %v, received %v",
					invoice.Terms.Value, pd.Amount)

				failure := lnwire.FailIncorrectPaymentAmount{}
				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)

				needUpdate = true
				continue
			}

			// As we're the exit hop, we'll double check the
			// hop-payload included in the HTLC to ensure that it
			// was crafted correctly by the sender and matches the
			// HTLC we were extended.
			//
			// NOTE: We make an exception when the value requested
			// by the invoice is zero. This means the invoice
			// allows the payee to specify the amount of satoshis
			// they wish to send.  So since we expect the htlc to
			// have a different amount, we should not fail.
			if !l.cfg.DebugHTLC && invoice.Terms.Value > 0 &&
				fwdInfo.AmountToForward != invoice.Terms.Value {

				log.Errorf("Onion payload of incoming htlc(%x) "+
					"has incorrect value: expected %v, "+
					"got %v", pd.RHash, invoice.Terms.Value,
					fwdInfo.AmountToForward)

				failure := lnwire.FailIncorrectPaymentAmount{}
				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)

				needUpdate = true
				continue
			}

			// We'll also ensure that our time-lock value has been
			// computed correctly.
			//
			// TODO(roasbeef): also accept global default?
			expectedHeight := heightNow + l.cfg.FwrdingPolicy.TimeLockDelta
			switch {

			case !l.cfg.DebugHTLC && fwdInfo.OutgoingCTLV < expectedHeight:
				log.Errorf("Onion payload of incoming "+
					"htlc(%x) has incorrect time-lock: "+
					"expected %v, got %v",
					pd.RHash[:], expectedHeight,
					fwdInfo.OutgoingCTLV)

				failure := lnwire.NewFinalIncorrectCltvExpiry(
					fwdInfo.OutgoingCTLV,
				)
				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)

				needUpdate = true
				continue

			case !l.cfg.DebugHTLC && pd.Timeout != fwdInfo.OutgoingCTLV:
				log.Errorf("HTLC(%x) has incorrect "+
					"time-lock: expected %v, got %v",
					pd.RHash[:], pd.Timeout,
					fwdInfo.OutgoingCTLV)

				failure := lnwire.NewFinalIncorrectCltvExpiry(
					fwdInfo.OutgoingCTLV,
				)
				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)

				needUpdate = true
				continue
			}

			preimage := invoice.Terms.PaymentPreimage
			err = l.channel.SettleHTLC(preimage,
				pd.HtlcIndex, pd.SourceRef, nil, nil)
			if err != nil {
				l.fail("unable to settle htlc: %v", err)
				return false
			}

			// Notify the invoiceRegistry of the invoices we just
			// settled with this latest commitment update.
			err = l.cfg.Registry.SettleInvoice(invoiceHash)
			if err != nil {
				l.fail("unable to settle invoice: %v", err)
				return false
			}

			l.infof("settling %x as exit hop", pd.RHash)

			// HTLC was successfully settled locally send
			// notification about it remote peer.
			l.cfg.Peer.SendMessage(&lnwire.UpdateFulfillHTLC{
				ChanID:          l.ChanID(),
				ID:              pd.HtlcIndex,
				PaymentPreimage: preimage,
			})
			needUpdate = true

		// There are additional channels left within this route. So
		// we'll verify that our forwarding constraints have been
		// properly met by by this incoming HTLC.
		default:
			switch fwdPkg.State {
			case channeldb.FwdStateProcessed:
				// This add was not forwarded on the previous
				// processing phase, run it through our
				// validation pipeline to reproduce an error.
				// This may trigger a different error due to
				// expiring timelocks, but we expect that an
				// error will be reproduced.
				if !fwdPkg.FwdFilter.Contains(idx) {
					break
				}

				// Otherwise, it was already processed, we can
				// can collect it and continue.
				addMsg := &lnwire.UpdateAddHTLC{
					Expiry:      fwdInfo.OutgoingCTLV,
					Amount:      fwdInfo.AmountToForward,
					PaymentHash: pd.RHash,
				}

				// Finally, we'll encode the onion packet for
				// the _next_ hop using the hop iterator
				// decoded for the current hop.
				buf := bytes.NewBuffer(addMsg.OnionBlob[0:0])

				// We know this cannot fail, as this ADD
				// was marked forwarded in a previous
				// round of processing.
				chanIterator.EncodeNextHop(buf)

				updatePacket := &htlcPacket{
					incomingChanID: l.ShortChanID(),
					incomingHTLCID: pd.HtlcIndex,
					outgoingChanID: fwdInfo.NextHop,
					sourceRef:      pd.SourceRef,
					incomingAmount: pd.Amount,
					amount:         addMsg.Amount,
					htlc:           addMsg,
					obfuscator:     obfuscator,
				}
				switchPackets = append(switchPackets,
					updatePacket)

				continue
			}

			// We want to avoid forwarding an HTLC which will
			// expire in the near future, so we'll reject an HTLC
			// if its expiration time is too close to the current
			// height.
			timeDelta := l.cfg.FwrdingPolicy.TimeLockDelta
			if pd.Timeout-timeDelta <= heightNow {
				log.Errorf("htlc(%x) has an expiry "+
					"that's too soon: outgoing_expiry=%v, "+
					"best_height=%v", pd.RHash[:],
					pd.Timeout-timeDelta, heightNow)

				var failure lnwire.FailureMessage
				update, err := l.cfg.GetLastChannelUpdate()
				if err != nil {
					failure = lnwire.NewTemporaryChannelFailure(nil)
				} else {
					failure = lnwire.NewExpiryTooSoon(*update)
				}

				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)
				needUpdate = true
				continue
			}

			// As our second sanity check, we'll ensure that the
			// passed HTLC isn't too small. If so, then
			// we'll cancel the HTLC directly.
			if pd.Amount < l.cfg.FwrdingPolicy.MinHTLC {
				log.Errorf("Incoming htlc(%x) is too "+
					"small: min_htlc=%v, htlc_value=%v",
					pd.RHash[:], l.cfg.FwrdingPolicy.MinHTLC,
					pd.Amount)

				// As part of the returned error, we'll send
				// our latest routing policy so the sending
				// node obtains the most up to date data.
				var failure lnwire.FailureMessage
				update, err := l.cfg.GetLastChannelUpdate()
				if err != nil {
					failure = lnwire.NewTemporaryChannelFailure(nil)
				} else {
					failure = lnwire.NewAmountBelowMinimum(
						pd.Amount, *update)
				}

				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)
				needUpdate = true
				continue
			}

			// Next, using the amount of the incoming HTLC, we'll
			// calculate the expected fee this incoming HTLC must
			// carry in order to be accepted.
			expectedFee := ExpectedFee(
				l.cfg.FwrdingPolicy,
				fwdInfo.AmountToForward,
			)

			// If the actual fee is less than our expected
			// fee, then we'll reject this HTLC as it didn't
			// provide a sufficient amount of fees, or the values
			// have been tampered with, or the send used
			// incorrect/dated information to construct the
			// forwarding information for this hop. In any case,
			// we'll cancel this HTLC.
			actualFee := pd.Amount - fwdInfo.AmountToForward
			if pd.Amount < fwdInfo.AmountToForward ||
				actualFee < expectedFee {

				log.Errorf("Incoming htlc(%x) has insufficient "+
					"fee: expected %v, got %v", pd.RHash[:],
					int64(expectedFee),
					int64(pd.Amount-fwdInfo.AmountToForward))

				// As part of the returned error, we'll send
				// our latest routing policy so the sending
				// node obtains the most up to date data.
				var failure lnwire.FailureMessage
				update, err := l.cfg.GetLastChannelUpdate()
				if err != nil {
					failure = lnwire.NewTemporaryChannelFailure(nil)
				} else {
					failure = lnwire.NewFeeInsufficient(pd.Amount,
						*update)
				}

				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)
				needUpdate = true
				continue
			}

			// Finally, we'll ensure that the time-lock on the
			// outgoing HTLC meets the following constraint: the
			// incoming time-lock minus our time-lock delta should
			// equal the outgoing time lock. Otherwise, whether the
			// sender messed up, or an intermediate node tampered
			// with the HTLC.
			if pd.Timeout-timeDelta < fwdInfo.OutgoingCTLV {
				log.Errorf("Incoming htlc(%x) has incorrect "+
					"time-lock value: expected at least "+
					"%v block delta, got %v block delta",
					pd.RHash[:], timeDelta,
					pd.Timeout-fwdInfo.OutgoingCTLV)

				// Grab the latest routing policy so the
				// sending node is up to date with our current
				// policy.
				update, err := l.cfg.GetLastChannelUpdate()
				if err != nil {
					l.fail("unable to create channel update "+
						"while handling the error: %v", err)
					return false
				}

				failure := lnwire.NewIncorrectCltvExpiry(
					pd.Timeout, *update)
				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)

				needUpdate = true
				continue
			}

			// TODO(roasbeef): also add max timeout value

			// With all our forwarding constraints met, we'll
			// create the outgoing HTLC using the parameters as
			// specified in the forwarding info.
			addMsg := &lnwire.UpdateAddHTLC{
				Expiry:      fwdInfo.OutgoingCTLV,
				Amount:      fwdInfo.AmountToForward,
				PaymentHash: pd.RHash,
			}

			// Finally, we'll encode the onion packet for the
			// _next_ hop using the hop iterator decoded for the
			// current hop.
			buf := bytes.NewBuffer(addMsg.OnionBlob[0:0])
			err := chanIterator.EncodeNextHop(buf)
			if err != nil {
				log.Errorf("unable to encode the "+
					"remaining route %v", err)

				failure := lnwire.NewTemporaryChannelFailure(nil)

				l.sendHTLCError(
					pd.HtlcIndex, failure, obfuscator, pd.SourceRef,
				)
				needUpdate = true
				continue
			}

			// Now that this add has been reprocessed, only append
			// it to our list of packets to forward to the switch
			// this is the first time processing the add. If the
			// fwd pkg has already been processed, then we entered
			// the above section to recreate a previous error.  If
			// the packet had previously been forwarded, it would
			// have been added to switchPackets at the top of this
			// section.
			if fwdPkg.State == channeldb.FwdStateLockedIn {
				updatePacket := &htlcPacket{
					incomingChanID: l.ShortChanID(),
					incomingHTLCID: pd.HtlcIndex,
					outgoingChanID: fwdInfo.NextHop,
					sourceRef:      pd.SourceRef,
					incomingAmount: pd.Amount,
					amount:         addMsg.Amount,
					htlc:           addMsg,
					obfuscator:     obfuscator,
				}

				fwdPkg.FwdFilter.Set(idx)
				switchPackets = append(switchPackets,
					updatePacket)
			}
		}
	}

	// Commit the htlcs we are intending to forward if this package has not
	// been fully processed.
	if fwdPkg.State == channeldb.FwdStateLockedIn {
		err := l.channel.SetFwdFilter(fwdPkg.Height, fwdPkg.FwdFilter)
		if err != nil {
			l.fail("unable to set fwd filter: %v", err)
			return false
		}
	}

	if len(switchPackets) == 0 {
		return needUpdate
	}

	l.debugf("forwarding %d packets to switch", len(switchPackets))

	go l.forwardBatch(switchPackets...)

	return needUpdate
}

// forwardBatch forwards the given htlcPackets to the switch, and waits on the
// err chan for the individual responses. This method is intended to be spawned
// as a goroutine so the responses can be handled in the background.
func (l *channelLink) forwardBatch(packets ...*htlcPacket) {
	// Don't forward packets for which we already have a response in our
	// mailbox. This could happen if a packet fails and is buffered in the
	// mailbox, and the incoming link flaps.
	var filteredPkts = make([]*htlcPacket, 0, len(packets))
	for _, pkt := range packets {
		if l.mailBox.HasPacket(pkt.inKey()) {
			continue
		}

		filteredPkts = append(filteredPkts, pkt)
	}

	errChan := l.cfg.ForwardPackets(filteredPkts...)
	l.handleBatchFwdErrs(errChan)
}

// handleBatchFwdErrs waits on the given errChan until it is closed, logging
// the errors returned from any unsuccessful forwarding attempts.
func (l *channelLink) handleBatchFwdErrs(errChan chan error) {
	for {
		err, ok := <-errChan
		if !ok {
			// Err chan has been drained or switch is shutting
			// down.  Either way, return.
			return
		}

		if err == nil {
			continue
		}

		l.errorf("unhandled error while forwarding htlc packet over "+
			"htlcswitch: %v", err)
	}
}

// sendHTLCError functions cancels HTLC and send cancel message back to the
// peer from which HTLC was received.
func (l *channelLink) sendHTLCError(htlcIndex uint64, failure lnwire.FailureMessage,
	e ErrorEncrypter, sourceRef *channeldb.AddRef) {

	reason, err := e.EncryptFirstHop(failure)
	if err != nil {
		log.Errorf("unable to obfuscate error: %v", err)
		return
	}

	err = l.channel.FailHTLC(htlcIndex, reason, sourceRef, nil, nil)
	if err != nil {
		log.Errorf("unable cancel htlc: %v", err)
		return
	}

	l.cfg.Peer.SendMessage(&lnwire.UpdateFailHTLC{
		ChanID: l.ChanID(),
		ID:     htlcIndex,
		Reason: reason,
	})
}

// sendMalformedHTLCError helper function which sends the malformed HTLC update
// to the payment sender.
func (l *channelLink) sendMalformedHTLCError(htlcIndex uint64,
	code lnwire.FailCode, onionBlob []byte, sourceRef *channeldb.AddRef) {

	shaOnionBlob := sha256.Sum256(onionBlob)
	err := l.channel.MalformedFailHTLC(htlcIndex, code, shaOnionBlob, sourceRef)
	if err != nil {
		log.Errorf("unable cancel htlc: %v", err)
		return
	}

	l.cfg.Peer.SendMessage(&lnwire.UpdateFailMalformedHTLC{
		ChanID:       l.ChanID(),
		ID:           htlcIndex,
		ShaOnionBlob: shaOnionBlob,
		FailureCode:  code,
	})
}

// fail helper function which is used to encapsulate the action necessary for
// proper disconnect.
func (l *channelLink) fail(format string, a ...interface{}) {
	reason := errors.Errorf(format, a...)
	log.Error(reason)
	go l.cfg.Peer.Disconnect(reason)
}

// infof prefixes the channel's identifier before printing to info log.
func (l *channelLink) infof(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	log.Infof("ChannelLink(%s) %s", l.ShortChanID(), msg)
}

// debugf prefixes the channel's identifier before printing to debug log.
func (l *channelLink) debugf(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	log.Debugf("ChannelLink(%s) %s", l.ShortChanID(), msg)
}

// warnf prefixes the channel's identifier before printing to warn log.
func (l *channelLink) warnf(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	log.Warnf("ChannelLink(%s) %s", l.ShortChanID(), msg)
}

// errorf prefixes the channel's identifier before printing to error log.
func (l *channelLink) errorf(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	log.Errorf("ChannelLink(%s) %s", l.ShortChanID(), msg)
}

// tracef prefixes the channel's identifier before printing to trace log.
func (l *channelLink) tracef(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	log.Tracef("ChannelLink(%s) %s", l.ShortChanID(), msg)
}

// isASCII is a helper method that checks whether all bytes in `data` would be
// printable ASCII characters if interpreted as a string.
func isASCII(data []byte) bool {
	isASCII := true
	for _, c := range data {
		if c < 32 || c > 126 {
			isASCII = false
			break
		}
	}
	return isASCII
}

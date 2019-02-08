package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	prand "math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/htlcswitch/hodl"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
)

func init() {
	prand.Seed(time.Now().UnixNano())
}

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

	// maxCltvExpiry is the maximum outgoing time lock that the node accepts
	// for forwarded payments. The value is relative to the current block
	// height. The reason to have a maximum is to prevent funds getting
	// locked up unreasonably long. Otherwise, an attacker willing to lock
	// its own funds too, could force the funds of this node to be locked up
	// for an indefinite (max int32) number of blocks.
	//
	// The value 5000 is based on the maximum number of hops (20), the
	// default cltv delta (144) and some extra margin.
	maxCltvExpiry = 5000

	// DefaultMinLinkFeeUpdateTimeout represents the minimum interval in
	// which a link should propose to update its commitment fee rate.
	DefaultMinLinkFeeUpdateTimeout = 10 * time.Minute

	// DefaultMaxLinkFeeUpdateTimeout represents the maximum interval in
	// which a link should propose to update its commitment fee rate.
	DefaultMaxLinkFeeUpdateTimeout = 60 * time.Minute
)

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
func ExpectedFee(f ForwardingPolicy,
	htlcAmt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	return f.BaseFee + (htlcAmt*f.FeeRate)/1000000
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
	// switch, any failed packets will be returned to the provided
	// ChannelLink. The link's quit signal should be provided to allow
	// cancellation of forwarding during link shutdown.
	ForwardPackets func(chan struct{}, ...*htlcPacket) chan error

	// DecodeHopIterators facilitates batched decoding of HTLC Sphinx onion
	// blobs, which are then used to inform how to forward an HTLC.
	//
	// NOTE: This function assumes the same set of readers and preimages
	// are always presented for the same identifier.
	DecodeHopIterators func([]byte, []DecodeHopIteratorRequest) (
		[]DecodeHopIteratorResponse, error)

	// ExtractErrorEncrypter function is responsible for decoding HTLC
	// Sphinx onion blob, and creating onion failure obfuscator.
	ExtractErrorEncrypter ErrorEncrypterExtracter

	// FetchLastChannelUpdate retrieves the latest routing policy for a
	// target channel. This channel will typically be the outgoing channel
	// specified when we receive an incoming HTLC.  This will be used to
	// provide payment senders our latest policy when sending encrypted
	// error messages.
	FetchLastChannelUpdate func(lnwire.ShortChannelID) (*lnwire.ChannelUpdate, error)

	// Peer is a lightning network node with which we have the channel link
	// opened.
	Peer lnpeer.Peer

	// Registry is a sub-system which responsible for managing the invoices
	// in thread-safe manner.
	Registry InvoiceDatabase

	// PreimageCache is a global witness beacon that houses any new
	// preimages discovered by other links. We'll use this to add new
	// witnesses that we discover which will notify any sub-systems
	// subscribed to new events.
	PreimageCache contractcourt.WitnessBeacon

	// OnChannelFailure is a function closure that we'll call if the
	// channel failed for some reason. Depending on the severity of the
	// error, the closure potentially must force close this channel and
	// disconnect the peer.
	//
	// NOTE: The method must return in order for the ChannelLink to be able
	// to shut down properly.
	OnChannelFailure func(lnwire.ChannelID, lnwire.ShortChannelID,
		LinkFailureError)

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

	// DebugHTLC should be turned on if you want all HTLCs sent to a node
	// with the debug htlc R-Hash are immediately settled in the next
	// available state transition.
	DebugHTLC bool

	// hodl.Mask is a bitvector composed of hodl.Flags, specifying breakpoints
	// for HTLC forwarding internal to the switch.
	//
	// NOTE: This should only be used for testing, and should only be used
	// simultaneously with DebugHTLC.
	HodlMask hodl.Mask

	// SyncStates is used to indicate that we need send the channel
	// reestablishment message to the remote peer. It should be done if our
	// clients have been restarted, or remote peer have been reconnected.
	SyncStates bool

	// BatchTicker is the ticker that determines the interval that we'll
	// use to check the batch to see if there're any updates we should
	// flush out. By batching updates into a single commit, we attempt to
	// increase throughput by maximizing the number of updates coalesced
	// into a single commit.
	BatchTicker ticker.Ticker

	// FwdPkgGCTicker is the ticker determining the frequency at which
	// garbage collection of forwarding packages occurs. We use a
	// time-based approach, as opposed to block epochs, as to not hinder
	// syncing.
	FwdPkgGCTicker ticker.Ticker

	// BatchSize is the max size of a batch of updates done to the link
	// before we do a state update.
	BatchSize uint32

	// UnsafeReplay will cause a link to replay the adds in its latest
	// commitment txn after the link is restarted. This should only be used
	// in testing, it is here to ensure the sphinx replay detection on the
	// receiving node is persistent.
	UnsafeReplay bool

	// MinFeeUpdateTimeout and MaxFeeUpdateTimeout represent the timeout
	// interval bounds in which a link will propose to update its commitment
	// fee rate. A random timeout will be selected between these values.
	MinFeeUpdateTimeout time.Duration
	MaxFeeUpdateTimeout time.Duration
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

	// failed should be set to true in case a link error happens, making
	// sure we don't process any more updates.
	failed bool

	// batchCounter is the number of updates which we received from remote
	// side, but not include in commitment transaction yet and plus the
	// current number of settles that have been sent, but not yet committed
	// to the commitment.
	//
	// TODO(andrew.shvv) remove after we add additional BatchNumber()
	// method in state machine.
	batchCounter uint32

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

	// updateFeeTimer is the timer responsible for updating the link's
	// commitment fee every time it fires.
	updateFeeTimer *time.Timer

	sync.RWMutex

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewChannelLink creates a new instance of a ChannelLink given a configuration
// and active channel that will be used to verify/apply updates to.
func NewChannelLink(cfg ChannelLinkConfig,
	channel *lnwallet.LightningChannel) ChannelLink {

	return &channelLink{
		cfg:         cfg,
		channel:     channel,
		shortChanID: channel.ShortChanID(),
		// TODO(roasbeef): just do reserve here?
		logCommitTimer: time.NewTimer(300 * time.Millisecond),
		overflowQueue:  newPacketQueue(input.MaxHTLCNumber / 2),
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

	l.mailBox.ResetMessages()
	l.overflowQueue.Start()

	// Before launching the htlcManager messages, revert any circuits that
	// were marked open in the switch's circuit map, but did not make it
	// into a commitment txn. We use the next local htlc index as the cut
	// off point, since all indexes below that are committed. This action
	// is only performed if the link's final short channel ID has been
	// assigned, otherwise we would try to trim the htlcs belonging to the
	// all-zero, sourceHop ID.
	if l.ShortChanID() != sourceHop {
		localHtlcIndex, err := l.channel.NextLocalHtlcIndex()
		if err != nil {
			return fmt.Errorf("unable to retrieve next local "+
				"htlc index: %v", err)
		}

		// NOTE: This is automatically done by the switch when it
		// starts up, but is necessary to prevent inconsistencies in
		// the case that the link flaps. This is a result of a link's
		// life-cycle being shorter than that of the switch.
		chanID := l.ShortChanID()
		err = l.cfg.Circuits.TrimOpenCircuits(chanID, localHtlcIndex)
		if err != nil {
			return fmt.Errorf("unable to trim circuits above "+
				"local htlc index %d: %v", localHtlcIndex, err)
		}

		// Since the link is live, before we start the link we'll update
		// the ChainArbitrator with the set of new channel signals for
		// this channel.
		//
		// TODO(roasbeef): split goroutines within channel arb to avoid
		go func() {
			signals := &contractcourt.ContractSignals{
				HtlcUpdates: l.htlcUpdates,
				ShortChanID: l.channel.ShortChanID(),
			}

			err := l.cfg.UpdateContractSignals(signals)
			if err != nil {
				log.Errorf("Unable to update signals for "+
					"ChannelLink(%v)", l)
			}
		}()
	}

	l.updateFeeTimer = time.NewTimer(l.randomFeeUpdateTimeout())

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

	l.updateFeeTimer.Stop()
	l.overflowQueue.Stop()

	close(l.quit)
	l.wg.Wait()
}

// WaitForShutdown blocks until the link finishes shutting down, which includes
// termination of all dependent goroutines.
func (l *channelLink) WaitForShutdown() {
	l.wg.Wait()
}

// EligibleToForward returns a bool indicating if the channel is able to
// actively accept requests to forward HTLC's. We're able to forward HTLC's if
// we know the remote party's next revocation point. Otherwise, we can't
// initiate new channel state. We also require that the short channel ID not be
// the all-zero source ID, meaning that the channel has had its ID finalized.
func (l *channelLink) EligibleToForward() bool {
	return l.channel.RemoteNextRevocation() != nil &&
		l.ShortChanID() != sourceHop
}

// sampleNetworkFee samples the current fee rate on the network to get into the
// chain in a timely manner. The returned value is expressed in fee-per-kw, as
// this is the native rate used when computing the fee for commitment
// transactions, and the second-level HTLC transactions.
func (l *channelLink) sampleNetworkFee() (lnwallet.SatPerKWeight, error) {
	// We'll first query for the sat/kw recommended to be confirmed within 3
	// blocks.
	feePerKw, err := l.cfg.FeeEstimator.EstimateFeePerKW(3)
	if err != nil {
		return 0, err
	}

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
	localChanSyncMsg, err := lnwallet.ChanSyncMsg(l.channel.State())
	if err != nil {
		return fmt.Errorf("unable to generate chan sync message for "+
			"ChannelPoint(%v)", l.channel.ChannelPoint())
	}
	if err := l.cfg.Peer.SendMessage(false, localChanSyncMsg); err != nil {
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
			err = l.cfg.Peer.SendMessage(false, fundingLockedMsg)
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
			return err
		}

		// Repopulate any identifiers for circuits that may have been
		// opened or unclosed. This may happen if we needed to
		// retransmit a commitment signature message.
		l.openedCircuits = openedCircuits
		l.closedCircuits = closedCircuits

		// Ensure that all packets have been have been removed from the
		// link's mailbox.
		if err := l.ackDownStreamPackets(); err != nil {
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
			l.cfg.Peer.SendMessage(false, msg)
		}

	case <-l.quit:
		return ErrLinkShuttingDown

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
		settleFails, err := lnwallet.PayDescsFromRemoteLogUpdates(
			fwdPkg.Source, fwdPkg.Height, fwdPkg.SettleFails,
		)
		if err != nil {
			l.errorf("Unable to process remote log updates: %v",
				err)
			return false, err
		}
		l.processRemoteSettleFails(fwdPkg, settleFails)
	}

	// Finally, replay *ALL ADDS* in this forwarding package. The
	// downstream logic is able to filter out any duplicates, but we must
	// shove the entire, original set of adds down the pipeline so that the
	// batch of adds presented to the sphinx router does not ever change.
	var needUpdate bool
	if !fwdPkg.AckFilter.IsFull() {
		adds, err := lnwallet.PayDescsFromRemoteLogUpdates(
			fwdPkg.Source, fwdPkg.Height, fwdPkg.Adds,
		)
		if err != nil {
			l.errorf("Unable to process remote log updates: %v",
				err)
			return false, err
		}
		needUpdate = l.processRemoteAdds(fwdPkg, adds)

		// If the link failed during processing the adds, we must
		// return to ensure we won't attempted to update the state
		// further.
		if l.failed {
			return false, fmt.Errorf("link failed while " +
				"processing remote adds")
		}
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

	l.cfg.FwdPkgGCTicker.Resume()
	defer l.cfg.FwdPkgGCTicker.Stop()

	for {
		select {
		case <-l.cfg.FwdPkgGCTicker.Ticks():
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
		l.cfg.BatchTicker.Stop()
		l.wg.Done()
		log.Infof("ChannelLink(%v) has exited", l)
	}()

	log.Infof("HTLC manager for ChannelPoint(%v) started, "+
		"bandwidth=%v", l.channel.ChannelPoint(), l.Bandwidth())

	// TODO(roasbeef): need to call wipe chan whenever D/C?

	// If this isn't the first time that this channel link has been
	// created, then we'll need to check to see if we need to
	// re-synchronize state with the remote peer. settledHtlcs is a map of
	// HTLC's that we re-settled as part of the channel state sync.
	if l.cfg.SyncStates {
		err := l.syncChanStates()
		if err != nil {
			switch {
			case err == ErrLinkShuttingDown:
				log.Debugf("unable to sync channel states, " +
					"link is shutting down")
				return

			// We failed syncing the commit chains, probably
			// because the remote has lost state. We should force
			// close the channel.
			case err == lnwallet.ErrCommitSyncRemoteDataLoss:
				fallthrough

			// The remote sent us an invalid last commit secret, we
			// should force close the channel.
			// TODO(halseth): and permanently ban the peer?
			case err == lnwallet.ErrInvalidLastCommitSecret:
				fallthrough

			// The remote sent us a commit point different from
			// what they sent us before.
			// TODO(halseth): ban peer?
			case err == lnwallet.ErrInvalidLocalUnrevokedCommitPoint:
				l.fail(
					LinkFailureError{
						code:       ErrSyncError,
						ForceClose: true,
					},
					"unable to synchronize channel "+
						"states: %v", err,
				)
				return

			// We have lost state and cannot safely force close the
			// channel. Fail the channel and wait for the remote to
			// hopefully force close it. The remote has sent us its
			// latest unrevoked commitment point, that we stored in
			// the database, that we can use to retrieve the funds
			// when the remote closes the channel.
			// TODO(halseth): mark this, such that we prevent
			// channel from being force closed by the user or
			// contractcourt etc.
			case err == lnwallet.ErrCommitSyncLocalDataLoss:

			// We determined the commit chains were not possible to
			// sync. We cautiously fail the channel, but don't
			// force close.
			// TODO(halseth): can we safely force close in any
			// cases where this error is returned?
			case err == lnwallet.ErrCannotSyncCommitChains:

			// Other, unspecified error.
			default:
			}

			l.fail(
				LinkFailureError{
					code:       ErrSyncError,
					ForceClose: false,
				},
				"unable to synchronize channel "+
					"states: %v", err,
			)
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
	// processed locally, or need to be forwarded out to the switch. We will
	// only attempt to resolve packages if our short chan id indicates that
	// the channel is not pending, otherwise we should have no htlcs to
	// reforward.
	if l.ShortChanID() != sourceHop {
		if err := l.resolveFwdPkgs(); err != nil {
			l.fail(LinkFailureError{code: ErrInternalError},
				"unable to resolve fwd pkgs: %v", err)
			return
		}

		// With our link's in-memory state fully reconstructed, spawn a
		// goroutine to manage the reclamation of disk space occupied by
		// completed forwarding packages.
		l.wg.Add(1)
		go l.fwdPkgGarbager()
	}

out:
	for {
		// We must always check if we failed at some point processing
		// the last update before processing the next.
		if l.failed {
			l.errorf("link failed, exiting htlcManager")
			break out
		}

		select {
		// Our update fee timer has fired, so we'll check the network
		// fee to see if we should adjust our commitment fee.
		case <-l.updateFeeTimer.C:
			l.updateFeeTimer.Reset(l.randomFeeUpdateTimeout())

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
		case <-l.cfg.ChainEvents.RemoteUnilateralClosure:
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
				l.fail(LinkFailureError{code: ErrInternalError},
					"unable to update commitment: %v", err)
				break out
			}

		case <-l.cfg.BatchTicker.Ticks():
			// If the current batch is empty, then we have no work
			// here. We also disable the batch ticker from waking up
			// the htlcManager while the batch is empty.
			if l.batchCounter == 0 {
				l.cfg.BatchTicker.Pause()
				continue
			}

			// Otherwise, attempt to extend the remote commitment
			// chain including all the currently pending entries.
			// If the send was unsuccessful, then abandon the
			// update, waiting for the revocation window to open
			// up.
			if err := l.updateCommitTx(); err != nil {
				l.fail(LinkFailureError{code: ErrInternalError},
					"unable to update commitment: %v", err)
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

			// If the downstream packet resulted in a non-empty
			// batch, reinstate the batch ticker so that it can be
			// cleared.
			if l.batchCounter > 0 {
				l.cfg.BatchTicker.Resume()
			}

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

			// If the downstream packet resulted in a non-empty
			// batch, reinstate the batch ticker so that it can be
			// cleared.
			if l.batchCounter > 0 {
				l.cfg.BatchTicker.Resume()
			}

		// A message from the connected peer was just received. This
		// indicates that we have a new incoming HTLC, either directly
		// for us, or part of a multi-hop HTLC circuit.
		case msg := <-l.upstream:
			l.handleUpstreamMsg(msg)

		case <-l.quit:
			break out
		}
	}
}

// randomFeeUpdateTimeout returns a random timeout between the bounds defined
// within the link's configuration that will be used to determine when the link
// should propose an update to its commitment fee rate.
func (l *channelLink) randomFeeUpdateTimeout() time.Duration {
	lower := int64(l.cfg.MinFeeUpdateTimeout)
	upper := int64(l.cfg.MaxFeeUpdateTimeout)
	return time.Duration(prand.Int63n(upper-lower) + lower)
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
		// If hodl.AddOutgoing mode is active, we exit early to simulate
		// arbitrary delays between the switch adding an ADD to the
		// mailbox, and the HTLC being added to the commitment state.
		if l.cfg.DebugHTLC && l.cfg.HodlMask.Active(hodl.AddOutgoing) {
			l.warnf(hodl.AddOutgoing.Warning())
			l.mailBox.AckPacket(pkt.inKey())
			return
		}

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

				var failure lnwire.FailureMessage
				update, err := l.cfg.FetchLastChannelUpdate(
					l.ShortChanID(),
				)
				if err != nil {
					failure = &lnwire.FailTemporaryNodeFailure{}
				} else {
					failure = lnwire.NewTemporaryChannelFailure(
						update,
					)
				}

				// Encrypt the error back to the source unless
				// the payment was generated locally.
				if pkt.obfuscator == nil {
					var b bytes.Buffer
					err := lnwire.EncodeFailure(&b, failure, 0)
					if err != nil {
						l.errorf("unable to encode failure: %v", err)
						l.mailBox.AckPacket(pkt.inKey())
						return
					}
					reason = lnwire.OpaqueReason(b.Bytes())
					localFailure = true
				} else {
					var err error
					reason, err = pkt.obfuscator.EncryptFirstHop(failure)
					if err != nil {
						l.errorf("unable to obfuscate error: %v", err)
						l.mailBox.AckPacket(pkt.inKey())
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

		l.cfg.Peer.SendMessage(false, htlc)

	case *lnwire.UpdateFulfillHTLC:
		// If hodl.SettleOutgoing mode is active, we exit early to
		// simulate arbitrary delays between the switch adding the
		// SETTLE to the mailbox, and the HTLC being added to the
		// commitment state.
		if l.cfg.DebugHTLC && l.cfg.HodlMask.Active(hodl.SettleOutgoing) {
			l.warnf(hodl.SettleOutgoing.Warning())
			l.mailBox.AckPacket(pkt.inKey())
			return
		}

		// An HTLC we forward to the switch has just settled somewhere
		// upstream. Therefore we settle the HTLC within the our local
		// state machine.
		inKey := pkt.inKey()
		err := l.channel.SettleHTLC(
			htlc.PaymentPreimage,
			pkt.incomingHTLCID,
			pkt.sourceRef,
			pkt.destRef,
			&inKey,
		)
		if err != nil {
			l.errorf("unable to settle incoming HTLC for "+
				"circuit-key=%v: %v", inKey, err)

			// If the HTLC index for Settle response was not known
			// to our commitment state, it has already been
			// cleaned up by a prior response. We'll thus try to
			// clean up any lingering state to ensure we don't
			// continue reforwarding.
			if _, ok := err.(lnwallet.ErrUnknownHtlcIndex); ok {
				l.cleanupSpuriousResponse(pkt)
			}

			// Remove the packet from the link's mailbox to ensure
			// it doesn't get replayed after a reconnection.
			l.mailBox.AckPacket(inKey)

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
		l.cfg.Peer.SendMessage(false, htlc)
		isSettle = true

	case *lnwire.UpdateFailHTLC:
		// If hodl.FailOutgoing mode is active, we exit early to
		// simulate arbitrary delays between the switch adding a FAIL to
		// the mailbox, and the HTLC being added to the commitment
		// state.
		if l.cfg.DebugHTLC && l.cfg.HodlMask.Active(hodl.FailOutgoing) {
			l.warnf(hodl.FailOutgoing.Warning())
			l.mailBox.AckPacket(pkt.inKey())
			return
		}

		// An HTLC cancellation has been triggered somewhere upstream,
		// we'll remove then HTLC from our local state machine.
		inKey := pkt.inKey()
		err := l.channel.FailHTLC(
			pkt.incomingHTLCID,
			htlc.Reason,
			pkt.sourceRef,
			pkt.destRef,
			&inKey,
		)
		if err != nil {
			l.errorf("unable to cancel incoming HTLC for "+
				"circuit-key=%v: %v", inKey, err)

			// If the HTLC index for Fail response was not known to
			// our commitment state, it has already been cleaned up
			// by a prior response. We'll thus try to clean up any
			// lingering state to ensure we don't continue
			// reforwarding.
			if _, ok := err.(lnwallet.ErrUnknownHtlcIndex); ok {
				l.cleanupSpuriousResponse(pkt)
			}

			// Remove the packet from the link's mailbox to ensure
			// it doesn't get replayed after a reconnection.
			l.mailBox.AckPacket(inKey)

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
		l.cfg.Peer.SendMessage(false, htlc)
		isSettle = true
	}

	l.batchCounter++

	// If this newly added update exceeds the min batch size for adds, or
	// this is a settle request, then initiate an update.
	if l.batchCounter >= l.cfg.BatchSize || isSettle {
		if err := l.updateCommitTx(); err != nil {
			l.fail(LinkFailureError{code: ErrInternalError},
				"unable to update commitment: %v", err)
			return
		}
	}
}

// cleanupSpuriousResponse attempts to ack any AddRef or SettleFailRef
// associated with this packet. If successful in doing so, it will also purge
// the open circuit from the circuit map and remove the packet from the link's
// mailbox.
func (l *channelLink) cleanupSpuriousResponse(pkt *htlcPacket) {
	inKey := pkt.inKey()

	l.debugf("Cleaning up spurious response for incoming circuit-key=%v",
		inKey)

	// If the htlc packet doesn't have a source reference, it is unsafe to
	// proceed, as skipping this ack may cause the htlc to be reforwarded.
	if pkt.sourceRef == nil {
		l.errorf("uanble to cleanup response for incoming "+
			"circuit-key=%v, does not contain source reference",
			inKey)
		return
	}

	// If the source reference is present,  we will try to prevent this link
	// from resending the packet to the switch. To do so, we ack the AddRef
	// of the incoming HTLC belonging to this link.
	err := l.channel.AckAddHtlcs(*pkt.sourceRef)
	if err != nil {
		l.errorf("unable to ack AddRef for incoming "+
			"circuit-key=%v: %v", inKey, err)

		// If this operation failed, it is unsafe to attempt removal of
		// the destination reference or circuit, so we exit early. The
		// cleanup may proceed with a different packet in the future
		// that succeeds on this step.
		return
	}

	// Now that we know this link will stop retransmitting Adds to the
	// switch, we can begin to teardown the response reference and circuit
	// map.
	//
	// If the packet includes a destination reference, then a response for
	// this HTLC was locked into the outgoing channel. Attempt to remove
	// this reference, so we stop retransmitting the response internally.
	// Even if this fails, we will proceed in trying to delete the circuit.
	// When retransmitting responses, the destination references will be
	// cleaned up if an open circuit is not found in the circuit map.
	if pkt.destRef != nil {
		err := l.channel.AckSettleFails(*pkt.destRef)
		if err != nil {
			l.errorf("unable to ack SettleFailRef "+
				"for incoming circuit-key=%v: %v",
				inKey, err)
		}
	}

	l.debugf("Deleting circuit for incoming circuit-key=%x", inKey)

	// With all known references acked, we can now safely delete the circuit
	// from the switch's circuit map, as the state is no longer needed.
	err = l.cfg.Circuits.DeleteCircuits(inKey)
	if err != nil {
		l.errorf("unable to delete circuit for "+
			"circuit-key=%v: %v", inKey, err)
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
			l.fail(LinkFailureError{code: ErrInvalidUpdate},
				"unable to handle upstream add HTLC: %v", err)
			return
		}

		l.tracef("Receive upstream htlc with payment hash(%x), "+
			"assigning index: %v", msg.PaymentHash[:], index)

	case *lnwire.UpdateFulfillHTLC:
		pre := msg.PaymentPreimage
		idx := msg.ID
		if err := l.channel.ReceiveHTLCSettle(pre, idx); err != nil {
			l.fail(
				LinkFailureError{
					code:       ErrInvalidUpdate,
					ForceClose: true,
				},
				"unable to handle upstream settle HTLC: %v", err,
			)
			return
		}

		// TODO(roasbeef): pipeline to switch

		// As we've learned of a new preimage for the first time, we'll
		// add it to our preimage cache. By doing this, we ensure
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
			l.fail(LinkFailureError{code: ErrInvalidUpdate},
				"unable to handle upstream fail HTLC: %v", err)
			return
		}

	case *lnwire.UpdateFailHTLC:
		idx := msg.ID
		err := l.channel.ReceiveFailHTLC(idx, msg.Reason[:])
		if err != nil {
			l.fail(LinkFailureError{code: ErrInvalidUpdate},
				"unable to handle upstream fail HTLC: %v", err)
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
			var sendData []byte
			switch err.(type) {
			case *lnwallet.InvalidCommitSigError:
				sendData = []byte(err.Error())
			case *lnwallet.InvalidHtlcSigError:
				sendData = []byte(err.Error())
			}
			l.fail(
				LinkFailureError{
					code:       ErrInvalidCommitment,
					ForceClose: true,
					SendData:   sendData,
				},
				"ChannelPoint(%v): unable to accept new "+
					"commitment: %v",
				l.channel.ChannelPoint(), err,
			)
			return
		}

		// As we've just accepted a new state, we'll now
		// immediately send the remote peer a revocation for our prior
		// state.
		nextRevocation, currentHtlcs, err := l.channel.RevokeCurrentCommitment()
		if err != nil {
			log.Errorf("unable to revoke commitment: %v", err)
			return
		}
		l.cfg.Peer.SendMessage(false, nextRevocation)

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
		// already have a commitment with the latest accepted.
		if l.channel.FullySynced() {
			return
		}

		// Otherwise, the remote party initiated the state transition,
		// so we'll reply with a signature to provide them with their
		// version of the latest commitment.
		if err := l.updateCommitTx(); err != nil {
			l.fail(LinkFailureError{code: ErrInternalError},
				"unable to update commitment: %v", err)
			return
		}

	case *lnwire.RevokeAndAck:
		// We've received a revocation from the remote chain, if valid,
		// this moves the remote chain forward, and expands our
		// revocation window.
		fwdPkg, adds, settleFails, err := l.channel.ReceiveRevocation(msg)
		if err != nil {
			// TODO(halseth): force close?
			l.fail(LinkFailureError{code: ErrInvalidRevocation},
				"unable to accept revocation: %v", err)
			return
		}

		l.processRemoteSettleFails(fwdPkg, settleFails)
		needUpdate := l.processRemoteAdds(fwdPkg, adds)

		// If the link failed during processing the adds, we must
		// return to ensure we won't attempted to update the state
		// further.
		if l.failed {
			return
		}

		if needUpdate {
			if err := l.updateCommitTx(); err != nil {
				l.fail(LinkFailureError{code: ErrInternalError},
					"unable to update commitment: %v", err)
				return
			}
		}

	case *lnwire.UpdateFee:
		// We received fee update from peer. If we are the initiator we
		// will fail the channel, if not we will apply the update.
		fee := lnwallet.SatPerKWeight(msg.FeePerKw)
		if err := l.channel.ReceiveUpdateFee(fee); err != nil {
			l.fail(LinkFailureError{code: ErrInvalidUpdate},
				"error receiving fee update: %v", err)
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
		l.fail(LinkFailureError{code: ErrRemoteError},
			"ChannelPoint(%v): received error from peer: %v",
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
func (l *channelLink) ackDownStreamPackets() error {
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

	// If hodl.Commit mode is active, we will refrain from attempting to
	// commit any in-memory modifications to the channel state. Exiting here
	// permits testing of either the switch or link's ability to trim
	// circuits that have been opened, but unsuccessfully committed.
	if l.cfg.DebugHTLC && l.cfg.HodlMask.Active(hodl.Commit) {
		l.warnf(hodl.Commit.Warning())
		return nil
	}

	theirCommitSig, htlcSigs, err := l.channel.SignNextCommitment()
	if err == lnwallet.ErrNoWindow {
		l.tracef("revocation window exhausted, unable to send: %v, "+
			"dangling_opens=%v, dangling_closes%v",
			l.batchCounter, newLogClosure(func() string {
				return spew.Sdump(l.openedCircuits)
			}),
			newLogClosure(func() string {
				return spew.Sdump(l.closedCircuits)
			}),
		)
		return nil
	} else if err != nil {
		return err
	}

	if err := l.ackDownStreamPackets(); err != nil {
		return err
	}

	commitSig := &lnwire.CommitSig{
		ChanID:    l.ChanID(),
		CommitSig: theirCommitSig,
		HtlcSigs:  htlcSigs,
	}
	l.cfg.Peer.SendMessage(false, commitSig)

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
func (l *channelLink) Peer() lnpeer.Peer {
	return l.cfg.Peer
}

// ChannelPoint returns the channel outpoint for the channel link.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) ChannelPoint() *wire.OutPoint {
	return l.channel.ChannelPoint()
}

// ShortChanID returns the short channel ID for the channel link. The short
// channel ID encodes the exact location in the main chain that the original
// funding output can be found.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) ShortChanID() lnwire.ShortChannelID {
	l.RLock()
	defer l.RUnlock()

	return l.shortChanID
}

// UpdateShortChanID updates the short channel ID for a link. This may be
// required in the event that a link is created before the short chan ID for it
// is known, or a re-org occurs, and the funding transaction changes location
// within the chain.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) UpdateShortChanID() (lnwire.ShortChannelID, error) {
	chanID := l.ChanID()

	// Refresh the channel state's short channel ID by loading it from disk.
	// This ensures that the channel state accurately reflects the updated
	// short channel ID.
	err := l.channel.State().RefreshShortChanID()
	if err != nil {
		l.errorf("unable to refresh short_chan_id for chan_id=%v: %v",
			chanID, err)
		return sourceHop, err
	}

	sid := l.channel.ShortChanID()

	l.infof("Updating to short_chan_id=%v for chan_id=%v", sid, chanID)

	l.Lock()
	l.shortChanID = sid
	l.Unlock()

	go func() {
		err := l.cfg.UpdateContractSignals(&contractcourt.ContractSignals{
			HtlcUpdates: l.htlcUpdates,
			ShortChanID: sid,
		})
		if err != nil {
			log.Errorf("Unable to update signals for "+
				"ChannelLink(%v)", l)
		}
	}()

	// Now that the short channel ID has been properly updated, we can begin
	// garbage collecting any forwarding packages we create.
	l.wg.Add(1)
	go l.fwdPkgGarbager()

	return sid, nil
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

// UpdateForwardingPolicy updates the forwarding policy for the target
// ChannelLink. Once updated, the link will use the new forwarding policy to
// govern if it an incoming HTLC should be forwarded or not. Note that this
// processing of the new policy will ensure that uninitialized fields in the
// passed policy won't override already initialized fields in the current
// policy.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) UpdateForwardingPolicy(newPolicy ForwardingPolicy) {
	l.Lock()
	defer l.Unlock()

	// In order to avoid overriding a valid policy with a "null" field in
	// the new policy, we'll only update to the set sub policy if the new
	// value isn't uninitialized.
	if newPolicy.BaseFee != 0 {
		l.cfg.FwrdingPolicy.BaseFee = newPolicy.BaseFee
	}
	if newPolicy.FeeRate != 0 {
		l.cfg.FwrdingPolicy.FeeRate = newPolicy.FeeRate
	}
	if newPolicy.TimeLockDelta != 0 {
		l.cfg.FwrdingPolicy.TimeLockDelta = newPolicy.TimeLockDelta
	}
	if newPolicy.MinHTLC != 0 {
		l.cfg.FwrdingPolicy.MinHTLC = newPolicy.MinHTLC
	}
}

// HtlcSatifiesPolicy should return a nil error if the passed HTLC details
// satisfy the current forwarding policy fo the target link.  Otherwise, a
// valid protocol failure message should be returned in order to signal to the
// source of the HTLC, the policy consistency issue.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) HtlcSatifiesPolicy(payHash [32]byte,
	incomingHtlcAmt, amtToForward lnwire.MilliSatoshi,
	incomingTimeout, outgoingTimeout uint32,
	heightNow uint32) lnwire.FailureMessage {

	l.RLock()
	policy := l.cfg.FwrdingPolicy
	l.RUnlock()

	// As our first sanity check, we'll ensure that the passed HTLC isn't
	// too small for the next hop. If so, then we'll cancel the HTLC
	// directly.
	if amtToForward < policy.MinHTLC {
		l.errorf("outgoing htlc(%x) is too small: min_htlc=%v, "+
			"htlc_value=%v", payHash[:], policy.MinHTLC,
			amtToForward)

		// As part of the returned error, we'll send our latest routing
		// policy so the sending node obtains the most up to date data.
		var failure lnwire.FailureMessage
		update, err := l.cfg.FetchLastChannelUpdate(l.ShortChanID())
		if err != nil {
			failure = &lnwire.FailTemporaryNodeFailure{}
		} else {
			failure = lnwire.NewAmountBelowMinimum(
				amtToForward, *update,
			)
		}

		return failure
	}

	// Next, using the amount of the incoming HTLC, we'll calculate the
	// expected fee this incoming HTLC must carry in order to satisfy the
	// constraints of the outgoing link.
	expectedFee := ExpectedFee(policy, amtToForward)

	// If the actual fee is less than our expected fee, then we'll reject
	// this HTLC as it didn't provide a sufficient amount of fees, or the
	// values have been tampered with, or the send used incorrect/dated
	// information to construct the forwarding information for this hop. In
	// any case, we'll cancel this HTLC.
	actualFee := incomingHtlcAmt - amtToForward
	if incomingHtlcAmt < amtToForward || actualFee < expectedFee {
		l.errorf("outgoing htlc(%x) has insufficient fee: expected %v, "+
			"got %v", payHash[:], int64(expectedFee), int64(actualFee))

		// As part of the returned error, we'll send our latest routing
		// policy so the sending node obtains the most up to date data.
		var failure lnwire.FailureMessage
		update, err := l.cfg.FetchLastChannelUpdate(l.ShortChanID())
		if err != nil {
			failure = &lnwire.FailTemporaryNodeFailure{}
		} else {
			failure = lnwire.NewFeeInsufficient(
				amtToForward, *update,
			)
		}

		return failure
	}

	// We want to avoid accepting an HTLC which will expire in the near
	// future, so we'll reject an HTLC if its expiration time is too close
	// to the current height.
	timeDelta := policy.TimeLockDelta
	if incomingTimeout-timeDelta <= heightNow {
		l.errorf("htlc(%x) has an expiry that's too soon: "+
			"outgoing_expiry=%v, best_height=%v", payHash[:],
			incomingTimeout-timeDelta, heightNow)

		var failure lnwire.FailureMessage
		update, err := l.cfg.FetchLastChannelUpdate(
			l.ShortChanID(),
		)
		if err != nil {
			failure = lnwire.NewTemporaryChannelFailure(update)
		} else {
			failure = lnwire.NewExpiryTooSoon(*update)
		}

		return failure
	}

	if outgoingTimeout-heightNow > maxCltvExpiry {
		l.errorf("outgoing htlc(%x) has a time lock too far in the "+
			"future: got %v, but maximum is %v", payHash[:],
			outgoingTimeout-heightNow, maxCltvExpiry)

		return &lnwire.FailExpiryTooFar{}
	}

	// Finally, we'll ensure that the time-lock on the outgoing HTLC meets
	// the following constraint: the incoming time-lock minus our time-lock
	// delta should equal the outgoing time lock. Otherwise, whether the
	// sender messed up, or an intermediate node tampered with the HTLC.
	if incomingTimeout-timeDelta < outgoingTimeout {
		l.errorf("Incoming htlc(%x) has incorrect time-lock value: "+
			"expected at least %v block delta, got %v block delta",
			payHash[:], timeDelta, incomingTimeout-outgoingTimeout)

		// Grab the latest routing policy so the sending node is up to
		// date with our current policy.
		var failure lnwire.FailureMessage
		update, err := l.cfg.FetchLastChannelUpdate(
			l.ShortChanID(),
		)
		if err != nil {
			failure = lnwire.NewTemporaryChannelFailure(update)
		} else {
			failure = lnwire.NewIncorrectCltvExpiry(
				incomingTimeout, *update,
			)
		}

		return failure
	}

	return nil
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
	if err := l.cfg.Peer.SendMessage(false, msg); err != nil {
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
			// If hodl.SettleIncoming is requested, we will not
			// forward the SETTLE to the switch and will not signal
			// a free slot on the commitment transaction.
			if l.cfg.DebugHTLC && l.cfg.HodlMask.Active(hodl.SettleIncoming) {
				l.warnf(hodl.SettleIncoming.Warning())
				continue
			}

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
			// If hodl.SettleIncoming is requested, we will not
			// forward the FAIL to the switch and will not signal a
			// free slot on the commitment transaction.
			if l.cfg.DebugHTLC && l.cfg.HodlMask.Active(hodl.FailIncoming) {
				l.warnf(hodl.FailIncoming.Warning())
				continue
			}

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

	// Only spawn the task forward packets we have a non-zero number.
	if len(switchPackets) > 0 {
		go l.forwardBatch(switchPackets...)
	}
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
		l.fail(LinkFailureError{code: ErrInternalError},
			"unable to decode hop iterators: %v", sphinxErr)
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
			l.cfg.ExtractErrorEncrypter,
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

		heightNow := l.cfg.Switch.BestHeight()

		fwdInfo := chanIterator.ForwardingInstructions()
		switch fwdInfo.NextHop {
		case exitHop:
			updated, err := l.processExitHop(
				pd, obfuscator, fwdInfo, heightNow,
			)
			if err != nil {
				l.fail(LinkFailureError{code: ErrInternalError},
					err.Error(),
				)

				return false
			}
			if updated {
				needUpdate = true
			}

		// There are additional channels left within this route. So
		// we'll simply do some forwarding package book-keeping.
		default:
			// If hodl.AddIncoming is requested, we will not
			// validate the forwarded ADD, nor will we send the
			// packet to the htlc switch.
			if l.cfg.DebugHTLC &&
				l.cfg.HodlMask.Active(hodl.AddIncoming) {
				l.warnf(hodl.AddIncoming.Warning())
				continue
			}

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
					incomingChanID:  l.ShortChanID(),
					incomingHTLCID:  pd.HtlcIndex,
					outgoingChanID:  fwdInfo.NextHop,
					sourceRef:       pd.SourceRef,
					incomingAmount:  pd.Amount,
					amount:          addMsg.Amount,
					htlc:            addMsg,
					obfuscator:      obfuscator,
					incomingTimeout: pd.Timeout,
					outgoingTimeout: fwdInfo.OutgoingCTLV,
				}
				switchPackets = append(
					switchPackets, updatePacket,
				)

				continue
			}

			// TODO(roasbeef): ensure don't accept outrageous
			// timeout for htlc

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

				var failure lnwire.FailureMessage
				update, err := l.cfg.FetchLastChannelUpdate(
					l.ShortChanID(),
				)
				if err != nil {
					failure = &lnwire.FailTemporaryNodeFailure{}
				} else {
					failure = lnwire.NewTemporaryChannelFailure(
						update,
					)
				}

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
					incomingChanID:  l.ShortChanID(),
					incomingHTLCID:  pd.HtlcIndex,
					outgoingChanID:  fwdInfo.NextHop,
					sourceRef:       pd.SourceRef,
					incomingAmount:  pd.Amount,
					amount:          addMsg.Amount,
					htlc:            addMsg,
					obfuscator:      obfuscator,
					incomingTimeout: pd.Timeout,
					outgoingTimeout: fwdInfo.OutgoingCTLV,
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
			l.fail(LinkFailureError{code: ErrInternalError},
				"unable to set fwd filter: %v", err)
			return false
		}
	}

	if len(switchPackets) == 0 {
		return needUpdate
	}

	l.debugf("forwarding %d packets to switch", len(switchPackets))

	// NOTE: This call is made synchronous so that we ensure all circuits
	// are committed in the exact order that they are processed in the link.
	// Failing to do this could cause reorderings/gaps in the range of
	// opened circuits, which violates assumptions made by the circuit
	// trimming.
	l.forwardBatch(switchPackets...)

	return needUpdate
}

// processExitHop handles an htlc for which this link is the exit hop. It
// returns a boolean indicating whether the commitment tx needs an update.
func (l *channelLink) processExitHop(pd *lnwallet.PaymentDescriptor,
	obfuscator ErrorEncrypter, fwdInfo ForwardingInfo, heightNow uint32) (
	bool, error) {

	// If hodl.ExitSettle is requested, we will not validate the final hop's
	// ADD, nor will we settle the corresponding invoice or respond with the
	// preimage.
	if l.cfg.DebugHTLC && l.cfg.HodlMask.Active(hodl.ExitSettle) {
		l.warnf(hodl.ExitSettle.Warning())

		return false, nil
	}

	// First, we'll check the expiry of the HTLC itself against, the current
	// block height. If the timeout is too soon, then we'll reject the HTLC.
	if pd.Timeout-expiryGraceDelta <= heightNow {
		log.Errorf("htlc(%x) has an expiry that's too soon: expiry=%v"+
			", best_height=%v", pd.RHash[:], pd.Timeout, heightNow)

		failure := lnwire.NewFinalExpiryTooSoon()
		l.sendHTLCError(pd.HtlcIndex, failure, obfuscator, pd.SourceRef)

		return true, nil
	}

	// We're the designated payment destination.  Therefore we attempt to
	// see if we have an invoice locally which'll allow us to settle this
	// htlc.
	invoiceHash := lntypes.Hash(pd.RHash)
	invoice, minCltvDelta, err := l.cfg.Registry.LookupInvoice(invoiceHash)
	if err != nil {
		log.Errorf("unable to query invoice registry: %v", err)
		failure := lnwire.NewFailUnknownPaymentHash(pd.Amount)
		l.sendHTLCError(pd.HtlcIndex, failure, obfuscator, pd.SourceRef)

		return true, nil
	}

	// Reject htlcs for canceled invoices.
	if invoice.Terms.State == channeldb.ContractCanceled {
		l.errorf("Rejecting htlc due to canceled invoice")

		failure := lnwire.NewFailUnknownPaymentHash(
			pd.Amount,
		)
		l.sendHTLCError(pd.HtlcIndex, failure, obfuscator, pd.SourceRef)

		return true, nil
	}

	// If the invoice is already settled, we choose to accept the payment to
	// simplify failure recovery.
	//
	// NOTE: Though our recovery and forwarding logic is predominately
	// batched, settling invoices happens iteratively. We may reject one of
	// two payments for the same rhash at first, but then restart and reject
	// both after seeing that the invoice has been settled. Without any
	// record of which one settles first, it is ambiguous as to which one
	// actually settled the invoice. Thus, by accepting all payments, we
	// eliminate the race condition that can lead to this inconsistency.
	//
	// TODO(conner): track ownership of settlements to properly recover from
	// failures? or add batch invoice settlement
	if invoice.Terms.State != channeldb.ContractOpen {
		log.Warnf("Accepting duplicate payment for hash=%x",
			pd.RHash[:])
	}

	// If we're not currently in debug mode, and the extended htlc doesn't
	// meet the value requested, then we'll fail the htlc.  Otherwise, we
	// settle this htlc within our local state update log, then send the
	// update entry to the remote party.
	//
	// NOTE: We make an exception when the value requested by the invoice is
	// zero. This means the invoice allows the payee to specify the amount
	// of satoshis they wish to send.  So since we expect the htlc to have a
	// different amount, we should not fail.
	if !l.cfg.DebugHTLC && invoice.Terms.Value > 0 &&
		pd.Amount < invoice.Terms.Value {

		log.Errorf("rejecting htlc due to incorrect amount: expected "+
			"%v, received %v", invoice.Terms.Value, pd.Amount)

		failure := lnwire.NewFailUnknownPaymentHash(pd.Amount)
		l.sendHTLCError(pd.HtlcIndex, failure, obfuscator, pd.SourceRef)

		return true, nil
	}

	// As we're the exit hop, we'll double check the hop-payload included in
	// the HTLC to ensure that it was crafted correctly by the sender and
	// matches the HTLC we were extended.
	//
	// NOTE: We make an exception when the value requested by the invoice is
	// zero. This means the invoice allows the payee to specify the amount
	// of satoshis they wish to send.  So since we expect the htlc to have a
	// different amount, we should not fail.
	if !l.cfg.DebugHTLC && invoice.Terms.Value > 0 &&
		fwdInfo.AmountToForward < invoice.Terms.Value {

		log.Errorf("Onion payload of incoming htlc(%x) has incorrect "+
			"value: expected %v, got %v", pd.RHash,
			invoice.Terms.Value, fwdInfo.AmountToForward)

		failure := lnwire.NewFailUnknownPaymentHash(pd.Amount)
		l.sendHTLCError(pd.HtlcIndex, failure, obfuscator, pd.SourceRef)

		return true, nil
	}

	// We'll also ensure that our time-lock value has been computed
	// correctly.
	expectedHeight := heightNow + minCltvDelta
	switch {
	case !l.cfg.DebugHTLC && pd.Timeout < expectedHeight:
		log.Errorf("Incoming htlc(%x) has an expiration that is too "+
			"soon: expected at least %v, got %v",
			pd.RHash[:], expectedHeight, pd.Timeout)

		failure := lnwire.FailFinalExpiryTooSoon{}
		l.sendHTLCError(pd.HtlcIndex, failure, obfuscator, pd.SourceRef)

		return true, nil

	case !l.cfg.DebugHTLC && pd.Timeout != fwdInfo.OutgoingCTLV:
		log.Errorf("HTLC(%x) has incorrect time-lock: expected %v, "+
			"got %v", pd.RHash[:], pd.Timeout, fwdInfo.OutgoingCTLV)

		failure := lnwire.NewFinalIncorrectCltvExpiry(
			fwdInfo.OutgoingCTLV,
		)
		l.sendHTLCError(pd.HtlcIndex, failure, obfuscator, pd.SourceRef)

		return true, nil
	}

	preimage := invoice.Terms.PaymentPreimage
	err = l.channel.SettleHTLC(
		preimage, pd.HtlcIndex, pd.SourceRef, nil, nil,
	)
	if err != nil {
		return false, fmt.Errorf("unable to settle htlc: %v", err)
	}

	// Notify the invoiceRegistry of the invoices we just settled (with the
	// amount accepted at settle time) with this latest commitment update.
	err = l.cfg.Registry.SettleInvoice(invoiceHash, pd.Amount)
	if err != nil {
		return false, fmt.Errorf("unable to settle invoice: %v", err)
	}

	l.infof("settling %x as exit hop", pd.RHash)

	// If the link is in hodl.BogusSettle mode, replace the preimage with a
	// fake one before sending it to the peer.
	if l.cfg.DebugHTLC &&
		l.cfg.HodlMask.Active(hodl.BogusSettle) {
		l.warnf(hodl.BogusSettle.Warning())
		preimage = [32]byte{}
		copy(preimage[:], bytes.Repeat([]byte{2}, 32))
	}

	// HTLC was successfully settled locally send notification about it
	// remote peer.
	l.cfg.Peer.SendMessage(false, &lnwire.UpdateFulfillHTLC{
		ChanID:          l.ChanID(),
		ID:              pd.HtlcIndex,
		PaymentPreimage: preimage,
	})

	return true, nil
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

	errChan := l.cfg.ForwardPackets(l.quit, filteredPkts...)
	go l.handleBatchFwdErrs(errChan)
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

	l.cfg.Peer.SendMessage(false, &lnwire.UpdateFailHTLC{
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

	l.cfg.Peer.SendMessage(false, &lnwire.UpdateFailMalformedHTLC{
		ChanID:       l.ChanID(),
		ID:           htlcIndex,
		ShaOnionBlob: shaOnionBlob,
		FailureCode:  code,
	})
}

// fail is a function which is used to encapsulate the action necessary for
// properly failing the link. It takes a LinkFailureError, which will be passed
// to the OnChannelFailure closure, in order for it to determine if we should
// force close the channel, and if we should send an error message to the
// remote peer.
func (l *channelLink) fail(linkErr LinkFailureError,
	format string, a ...interface{}) {
	reason := errors.Errorf(format, a...)

	// Return if we have already notified about a failure.
	if l.failed {
		l.warnf("Ignoring link failure (%v), as link already failed",
			reason)
		return
	}

	l.errorf("Failing link: %s", reason)

	// Set failed, such that we won't process any more updates, and notify
	// the peer about the failure.
	l.failed = true
	l.cfg.OnChannelFailure(l.ChanID(), l.ShortChanID(), linkErr)
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

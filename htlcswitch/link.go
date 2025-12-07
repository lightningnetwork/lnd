package htlcswitch

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	prand "math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hodl"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// DefaultMaxOutgoingCltvExpiry is the maximum outgoing time lock that
	// the node accepts for forwarded payments. The value is relative to the
	// current block height. The reason to have a maximum is to prevent
	// funds getting locked up unreasonably long. Otherwise, an attacker
	// willing to lock its own funds too, could force the funds of this node
	// to be locked up for an indefinite (max int32) number of blocks.
	//
	// The value 2016 corresponds to on average two weeks worth of blocks
	// and is based on the maximum number of hops (20), the default CLTV
	// delta (40), and some extra margin to account for the other lightning
	// implementations and past lnd versions which used to have a default
	// CLTV delta of 144.
	DefaultMaxOutgoingCltvExpiry = 2016

	// DefaultMinLinkFeeUpdateTimeout represents the minimum interval in
	// which a link should propose to update its commitment fee rate.
	DefaultMinLinkFeeUpdateTimeout = 10 * time.Minute

	// DefaultMaxLinkFeeUpdateTimeout represents the maximum interval in
	// which a link should propose to update its commitment fee rate.
	DefaultMaxLinkFeeUpdateTimeout = 60 * time.Minute

	// DefaultMaxLinkFeeAllocation is the highest allocation we'll allow
	// a channel's commitment fee to be of its balance. This only applies to
	// the initiator of the channel.
	DefaultMaxLinkFeeAllocation float64 = 0.5
)

// ExpectedFee computes the expected fee for a given htlc amount. The value
// returned from this function is to be used as a sanity check when forwarding
// HTLC's to ensure that an incoming HTLC properly adheres to our propagated
// forwarding policy.
//
// TODO(roasbeef): also add in current available channel bandwidth, inverse
// func
func ExpectedFee(f models.ForwardingPolicy,
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
	FwrdingPolicy models.ForwardingPolicy

	// Circuits provides restricted access to the switch's circuit map,
	// allowing the link to open and close circuits.
	Circuits CircuitModifier

	// BestHeight returns the best known height.
	BestHeight func() uint32

	// ForwardPackets attempts to forward the batch of htlcs through the
	// switch. The function returns and error in case it fails to send one or
	// more packets. The link's quit signal should be provided to allow
	// cancellation of forwarding during link shutdown.
	ForwardPackets func(<-chan struct{}, bool, ...*htlcPacket) error

	// DecodeHopIterators facilitates batched decoding of HTLC Sphinx onion
	// blobs, which are then used to inform how to forward an HTLC.
	//
	// NOTE: This function assumes the same set of readers and preimages
	// are always presented for the same identifier. The last boolean is
	// used to decide whether this is a reforwarding or not - when it's
	// reforwarding, we skip the replay check enforced in our decay log.
	DecodeHopIterators func([]byte, []hop.DecodeHopIteratorRequest, bool) (
		[]hop.DecodeHopIteratorResponse, error)

	// ExtractErrorEncrypter function is responsible for decoding HTLC
	// Sphinx onion blob, and creating onion failure obfuscator.
	ExtractErrorEncrypter hop.ErrorEncrypterExtracter

	// FetchLastChannelUpdate retrieves the latest routing policy for a
	// target channel. This channel will typically be the outgoing channel
	// specified when we receive an incoming HTLC.  This will be used to
	// provide payment senders our latest policy when sending encrypted
	// error messages.
	FetchLastChannelUpdate func(lnwire.ShortChannelID) (
		*lnwire.ChannelUpdate1, error)

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
	// outside sub-systems with this channel's latest ShortChannelID.
	UpdateContractSignals func(*contractcourt.ContractSignals) error

	// NotifyContractUpdate is a function closure that we'll use to update
	// the contractcourt and more specifically the ChannelArbitrator of the
	// latest channel state.
	NotifyContractUpdate func(*contractcourt.ContractUpdate) error

	// ChainEvents is an active subscription to the chain watcher for this
	// channel to be notified of any on-chain activity related to this
	// channel.
	ChainEvents *contractcourt.ChainEventSubscription

	// FeeEstimator is an instance of a live fee estimator which will be
	// used to dynamically regulate the current fee of the commitment
	// transaction to ensure timely confirmation.
	FeeEstimator chainfee.Estimator

	// hodl.Mask is a bitvector composed of hodl.Flags, specifying breakpoints
	// for HTLC forwarding internal to the switch.
	//
	// NOTE: This should only be used for testing.
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

	// PendingCommitTicker is a ticker that allows the link to determine if
	// a locally initiated commitment dance gets stuck waiting for the
	// remote party to revoke.
	PendingCommitTicker ticker.Ticker

	// BatchSize is the max size of a batch of updates done to the link
	// before we do a state update.
	BatchSize uint32

	// UnsafeReplay will cause a link to replay the adds in its latest
	// commitment txn after the link is restarted. This should only be used
	// in testing, it is here to ensure the sphinx replay detection on the
	// receiving node is persistent.
	UnsafeReplay bool

	// MinUpdateTimeout represents the minimum interval in which a link
	// will propose to update its commitment fee rate. A random timeout will
	// be selected between this and MaxUpdateTimeout.
	MinUpdateTimeout time.Duration

	// MaxUpdateTimeout represents the maximum interval in which a link
	// will propose to update its commitment fee rate. A random timeout will
	// be selected between this and MinUpdateTimeout.
	MaxUpdateTimeout time.Duration

	// OutgoingCltvRejectDelta defines the number of blocks before expiry of
	// an htlc where we don't offer an htlc anymore. This should be at least
	// the outgoing broadcast delta, because in any case we don't want to
	// risk offering an htlc that triggers channel closure.
	OutgoingCltvRejectDelta uint32

	// TowerClient is an optional engine that manages the signing,
	// encrypting, and uploading of justice transactions to the daemon's
	// configured set of watchtowers for legacy channels.
	TowerClient TowerClient

	// MaxOutgoingCltvExpiry is the maximum outgoing timelock that the link
	// should accept for a forwarded HTLC. The value is relative to the
	// current block height.
	MaxOutgoingCltvExpiry uint32

	// MaxFeeAllocation is the highest allocation we'll allow a channel's
	// commitment fee to be of its balance. This only applies to the
	// initiator of the channel.
	MaxFeeAllocation float64

	// MaxAnchorsCommitFeeRate is the max commitment fee rate we'll use as
	// the initiator for channels of the anchor type.
	MaxAnchorsCommitFeeRate chainfee.SatPerKWeight

	// NotifyActiveLink allows the link to tell the ChannelNotifier when a
	// link is first started.
	NotifyActiveLink func(wire.OutPoint)

	// NotifyActiveChannel allows the link to tell the ChannelNotifier when
	// channels becomes active.
	NotifyActiveChannel func(wire.OutPoint)

	// NotifyInactiveChannel allows the switch to tell the ChannelNotifier
	// when channels become inactive.
	NotifyInactiveChannel func(wire.OutPoint)

	// NotifyInactiveLinkEvent allows the switch to tell the
	// ChannelNotifier when a channel link become inactive.
	NotifyInactiveLinkEvent func(wire.OutPoint)

	// HtlcNotifier is an instance of a htlcNotifier which we will pipe htlc
	// events through.
	HtlcNotifier htlcNotifier

	// FailAliasUpdate is a function used to fail an HTLC for an
	// option_scid_alias channel.
	FailAliasUpdate func(sid lnwire.ShortChannelID,
		incoming bool) *lnwire.ChannelUpdate1

	// GetAliases is used by the link and switch to fetch the set of
	// aliases for a given link.
	GetAliases func(base lnwire.ShortChannelID) []lnwire.ShortChannelID

	// PreviouslySentShutdown is an optional value that is set if, at the
	// time of the link being started, persisted shutdown info was found for
	// the channel. This value being set means that we previously sent a
	// Shutdown message to our peer, and so we should do so again on
	// re-establish and should not allow anymore HTLC adds on the outgoing
	// direction of the link.
	PreviouslySentShutdown fn.Option[lnwire.Shutdown]

	// Adds the option to disable forwarding payments in blinded routes
	// by failing back any blinding-related payloads as if they were
	// invalid.
	DisallowRouteBlinding bool

	// DisallowQuiescence is a flag that can be used to disable the
	// quiescence protocol.
	DisallowQuiescence bool

	// MaxFeeExposure is the threshold in milli-satoshis after which we'll
	// restrict the flow of HTLCs and fee updates.
	MaxFeeExposure lnwire.MilliSatoshi

	// ShouldFwdExpEndorsement is a closure that indicates whether the link
	// should forward experimental endorsement signals.
	ShouldFwdExpEndorsement func() bool

	// AuxTrafficShaper is an optional auxiliary traffic shaper that can be
	// used to manage the bandwidth of the link.
	AuxTrafficShaper fn.Option[AuxTrafficShaper]

	// AuxChannelNegotiator is an optional interface that allows aux channel
	// implementations to inject and process custom records over channel
	// related wire messages.
	AuxChannelNegotiator fn.Option[lnwallet.AuxChannelNegotiator]

	// QuiescenceTimeout is the max duration that the channel can be
	// quiesced. Any dependent protocols (dynamic commitments, splicing,
	// etc.) must finish their operations under this timeout value,
	// otherwise the node will disconnect.
	QuiescenceTimeout time.Duration
}

// channelLink is the service which drives a channel's commitment update
// state-machine. In the event that an HTLC needs to be propagated to another
// link, the forward handler from config is used which sends HTLC to the
// switch. Additionally, the link encapsulate logic of commitment protocol
// message ordering and updates.
type channelLink struct {
	// The following fields are only meant to be used *atomically*
	started       int32
	reestablished int32
	shutdown      int32

	// failed should be set to true in case a link error happens, making
	// sure we don't process any more updates.
	failed bool

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

	// cfg is a structure which carries all dependable fields/handlers
	// which may affect behaviour of the service.
	cfg ChannelLinkConfig

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

	// updateFeeTimer is the timer responsible for updating the link's
	// commitment fee every time it fires.
	updateFeeTimer *time.Timer

	// uncommittedPreimages stores a list of all preimages that have been
	// learned since receiving the last CommitSig from the remote peer. The
	// batch will be flushed just before accepting the subsequent CommitSig
	// or on shutdown to avoid doing a write for each preimage received.
	uncommittedPreimages []lntypes.Preimage

	sync.RWMutex

	// hodlQueue is used to receive exit hop htlc resolutions from invoice
	// registry.
	hodlQueue *queue.ConcurrentQueue

	// hodlMap stores related htlc data for a circuit key. It allows
	// resolving those htlcs when we receive a message on hodlQueue.
	hodlMap map[models.CircuitKey]hodlHtlc

	// log is a link-specific logging instance.
	log btclog.Logger

	// isOutgoingAddBlocked tracks whether the channelLink can send an
	// UpdateAddHTLC.
	isOutgoingAddBlocked atomic.Bool

	// isIncomingAddBlocked tracks whether the channelLink can receive an
	// UpdateAddHTLC.
	isIncomingAddBlocked atomic.Bool

	// flushHooks is a hookMap that is triggered when we reach a channel
	// state with no live HTLCs.
	flushHooks hookMap

	// outgoingCommitHooks is a hookMap that is triggered after we send our
	// next CommitSig.
	outgoingCommitHooks hookMap

	// incomingCommitHooks is a hookMap that is triggered after we receive
	// our next CommitSig.
	incomingCommitHooks hookMap

	// quiescer is the state machine that tracks where this channel is with
	// respect to the quiescence protocol.
	quiescer Quiescer

	// quiescenceReqs is a queue of requests to quiesce this link. The
	// members of the queue are send-only channels we should call back with
	// the result.
	quiescenceReqs chan StfuReq

	// cg is a helper that encapsulates a wait group and quit channel and
	// allows contexts that either block or cancel on those depending on
	// the use case.
	cg *fn.ContextGuard
}

// hookMap is a data structure that is used to track the hooks that need to be
// called in various parts of the channelLink's lifecycle.
//
// WARNING: NOT thread-safe.
type hookMap struct {
	// allocIdx keeps track of the next id we haven't yet allocated.
	allocIdx atomic.Uint64

	// transient is a map of hooks that are only called the next time invoke
	// is called. These hooks are deleted during invoke.
	transient map[uint64]func()

	// newTransients is a channel that we use to accept new hooks into the
	// hookMap.
	newTransients chan func()
}

// newHookMap initializes a new empty hookMap.
func newHookMap() hookMap {
	return hookMap{
		allocIdx:      atomic.Uint64{},
		transient:     make(map[uint64]func()),
		newTransients: make(chan func()),
	}
}

// alloc allocates space in the hook map for the supplied hook, the second
// argument determines whether it goes into the transient or persistent part
// of the hookMap.
func (m *hookMap) alloc(hook func()) uint64 {
	// We assume we never overflow a uint64. Seems OK.
	hookID := m.allocIdx.Add(1)
	if hookID == 0 {
		panic("hookMap allocIdx overflow")
	}
	m.transient[hookID] = hook

	return hookID
}

// invoke is used on a hook map to call all the registered hooks and then clear
// out the transient hooks so they are not called again.
func (m *hookMap) invoke() {
	for _, hook := range m.transient {
		hook()
	}

	m.transient = make(map[uint64]func())
}

// hodlHtlc contains htlc data that is required for resolution.
type hodlHtlc struct {
	add        lnwire.UpdateAddHTLC
	sourceRef  channeldb.AddRef
	obfuscator hop.ErrorEncrypter
}

// NewChannelLink creates a new instance of a ChannelLink given a configuration
// and active channel that will be used to verify/apply updates to.
func NewChannelLink(cfg ChannelLinkConfig,
	channel *lnwallet.LightningChannel) ChannelLink {

	logPrefix := fmt.Sprintf("ChannelLink(%v):", channel.ChannelPoint())

	// If the max fee exposure isn't set, use the default.
	if cfg.MaxFeeExposure == 0 {
		cfg.MaxFeeExposure = DefaultMaxFeeExposure
	}

	var qsm Quiescer
	if !cfg.DisallowQuiescence {
		qsm = NewQuiescer(QuiescerCfg{
			chanID: lnwire.NewChanIDFromOutPoint(
				channel.ChannelPoint(),
			),
			channelInitiator: channel.Initiator(),
			sendMsg: func(s lnwire.Stfu) error {
				return cfg.Peer.SendMessage(false, &s)
			},
			timeoutDuration: cfg.QuiescenceTimeout,
			onTimeout: func() {
				cfg.Peer.Disconnect(ErrQuiescenceTimeout)
			},
		})
	} else {
		qsm = &quiescerNoop{}
	}

	quiescenceReqs := make(
		chan fn.Req[fn.Unit, fn.Result[lntypes.ChannelParty]], 1,
	)

	return &channelLink{
		cfg:                 cfg,
		channel:             channel,
		hodlMap:             make(map[models.CircuitKey]hodlHtlc),
		hodlQueue:           queue.NewConcurrentQueue(10),
		log:                 log.WithPrefix(logPrefix),
		flushHooks:          newHookMap(),
		outgoingCommitHooks: newHookMap(),
		incomingCommitHooks: newHookMap(),
		quiescer:            qsm,
		quiescenceReqs:      quiescenceReqs,
		cg:                  fn.NewContextGuard(),
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
		err := fmt.Errorf("channel link(%v): already started", l)
		l.log.Warn("already started")
		return err
	}

	l.log.Info("starting")

	// If the config supplied watchtower client, ensure the channel is
	// registered before trying to use it during operation.
	if l.cfg.TowerClient != nil {
		err := l.cfg.TowerClient.RegisterChannel(
			l.ChanID(), l.channel.State().ChanType,
		)
		if err != nil {
			return err
		}
	}

	l.mailBox.ResetMessages()
	l.hodlQueue.Start()

	// Before launching the htlcManager messages, revert any circuits that
	// were marked open in the switch's circuit map, but did not make it
	// into a commitment txn. We use the next local htlc index as the cut
	// off point, since all indexes below that are committed. This action
	// is only performed if the link's final short channel ID has been
	// assigned, otherwise we would try to trim the htlcs belonging to the
	// all-zero, hop.Source ID.
	if l.ShortChanID() != hop.Source {
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
				ShortChanID: l.channel.ShortChanID(),
			}

			err := l.cfg.UpdateContractSignals(signals)
			if err != nil {
				l.log.Errorf("unable to update signals")
			}
		}()
	}

	l.updateFeeTimer = time.NewTimer(l.randomFeeUpdateTimeout())

	l.cg.WgAdd(1)
	go l.htlcManager(context.TODO())

	return nil
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Stop() {
	if !atomic.CompareAndSwapInt32(&l.shutdown, 0, 1) {
		l.log.Warn("already stopped")
		return
	}

	l.log.Info("stopping")

	// As the link is stopping, we are no longer interested in htlc
	// resolutions coming from the invoice registry.
	l.cfg.Registry.HodlUnsubscribeAll(l.hodlQueue.ChanIn())

	if l.cfg.ChainEvents.Cancel != nil {
		l.cfg.ChainEvents.Cancel()
	}

	// Ensure the channel for the timer is drained.
	if l.updateFeeTimer != nil {
		if !l.updateFeeTimer.Stop() {
			select {
			case <-l.updateFeeTimer.C:
			default:
			}
		}
	}

	if l.hodlQueue != nil {
		l.hodlQueue.Stop()
	}

	l.cg.Quit()
	l.cg.WgWait()

	// Now that the htlcManager has completely exited, reset the packet
	// courier. This allows the mailbox to revaluate any lingering Adds that
	// were delivered but didn't make it on a commitment to be failed back
	// if the link is offline for an extended period of time. The error is
	// ignored since it can only fail when the daemon is exiting.
	_ = l.mailBox.ResetPackets()

	// As a final precaution, we will attempt to flush any uncommitted
	// preimages to the preimage cache. The preimages should be re-delivered
	// after channel reestablishment, however this adds an extra layer of
	// protection in case the peer never returns. Without this, we will be
	// unable to settle any contracts depending on the preimages even though
	// we had learned them at some point.
	err := l.cfg.PreimageCache.AddPreimages(l.uncommittedPreimages...)
	if err != nil {
		l.log.Errorf("unable to add preimages=%v to cache: %v",
			l.uncommittedPreimages, err)
	}
}

// WaitForShutdown blocks until the link finishes shutting down, which includes
// termination of all dependent goroutines.
func (l *channelLink) WaitForShutdown() {
	l.cg.WgWait()
}

// EligibleToForward returns a bool indicating if the channel is able to
// actively accept requests to forward HTLC's. We're able to forward HTLC's if
// we are eligible to update AND the channel isn't currently flushing the
// outgoing half of the channel.
//
// NOTE: MUST NOT be called from the main event loop.
func (l *channelLink) EligibleToForward() bool {
	l.RLock()
	defer l.RUnlock()

	return l.eligibleToForward()
}

// eligibleToForward returns a bool indicating if the channel is able to
// actively accept requests to forward HTLC's. We're able to forward HTLC's if
// we are eligible to update AND the channel isn't currently flushing the
// outgoing half of the channel.
//
// NOTE: MUST be called from the main event loop.
func (l *channelLink) eligibleToForward() bool {
	return l.eligibleToUpdate() && !l.IsFlushing(Outgoing)
}

// eligibleToUpdate returns a bool indicating if the channel is able to update
// channel state. We're able to update channel state if we know the remote
// party's next revocation point. Otherwise, we can't initiate new channel
// state. We also require that the short channel ID not be the all-zero source
// ID, meaning that the channel has had its ID finalized.
//
// NOTE: MUST be called from the main event loop.
func (l *channelLink) eligibleToUpdate() bool {
	return l.channel.RemoteNextRevocation() != nil &&
		l.channel.ShortChanID() != hop.Source &&
		l.isReestablished() &&
		l.quiescer.CanSendUpdates()
}

// EnableAdds sets the ChannelUpdateHandler state to allow UpdateAddHtlc's in
// the specified direction. It returns true if the state was changed and false
// if the desired state was already set before the method was called.
func (l *channelLink) EnableAdds(linkDirection LinkDirection) bool {
	if linkDirection == Outgoing {
		return l.isOutgoingAddBlocked.Swap(false)
	}

	return l.isIncomingAddBlocked.Swap(false)
}

// DisableAdds sets the ChannelUpdateHandler state to allow UpdateAddHtlc's in
// the specified direction. It returns true if the state was changed and false
// if the desired state was already set before the method was called.
func (l *channelLink) DisableAdds(linkDirection LinkDirection) bool {
	if linkDirection == Outgoing {
		return !l.isOutgoingAddBlocked.Swap(true)
	}

	return !l.isIncomingAddBlocked.Swap(true)
}

// IsFlushing returns true when UpdateAddHtlc's are disabled in the direction of
// the argument.
func (l *channelLink) IsFlushing(linkDirection LinkDirection) bool {
	if linkDirection == Outgoing {
		return l.isOutgoingAddBlocked.Load()
	}

	return l.isIncomingAddBlocked.Load()
}

// OnFlushedOnce adds a hook that will be called the next time the channel
// state reaches zero htlcs. This hook will only ever be called once. If the
// channel state already has zero htlcs, then this will be called immediately.
func (l *channelLink) OnFlushedOnce(hook func()) {
	select {
	case l.flushHooks.newTransients <- hook:
	case <-l.cg.Done():
	}
}

// OnCommitOnce adds a hook that will be called the next time a CommitSig
// message is sent in the argument's LinkDirection. This hook will only ever be
// called once. If no CommitSig is owed in the argument's LinkDirection, then
// we will call this hook be run immediately.
func (l *channelLink) OnCommitOnce(direction LinkDirection, hook func()) {
	var queue chan func()

	if direction == Outgoing {
		queue = l.outgoingCommitHooks.newTransients
	} else {
		queue = l.incomingCommitHooks.newTransients
	}

	select {
	case queue <- hook:
	case <-l.cg.Done():
	}
}

// InitStfu allows us to initiate quiescence on this link. It returns a receive
// only channel that will block until quiescence has been achieved, or
// definitively fails.
//
// This operation has been added to allow channels to be quiesced via RPC. It
// may be removed or reworked in the future as RPC initiated quiescence is a
// holdover until we have downstream protocols that use it.
func (l *channelLink) InitStfu() <-chan fn.Result[lntypes.ChannelParty] {
	req, out := fn.NewReq[fn.Unit, fn.Result[lntypes.ChannelParty]](
		fn.Unit{},
	)

	select {
	case l.quiescenceReqs <- req:
	case <-l.cg.Done():
		req.Resolve(fn.Err[lntypes.ChannelParty](ErrLinkShuttingDown))
	}

	return out
}

// isReestablished returns true if the link has successfully completed the
// channel reestablishment dance.
func (l *channelLink) isReestablished() bool {
	return atomic.LoadInt32(&l.reestablished) == 1
}

// markReestablished signals that the remote peer has successfully exchanged
// channel reestablish messages and that the channel is ready to process
// subsequent messages.
func (l *channelLink) markReestablished() {
	atomic.StoreInt32(&l.reestablished, 1)
}

// IsUnadvertised returns true if the underlying channel is unadvertised.
func (l *channelLink) IsUnadvertised() bool {
	state := l.channel.State()
	return state.ChannelFlags&lnwire.FFAnnounceChannel == 0
}

// sampleNetworkFee samples the current fee rate on the network to get into the
// chain in a timely manner. The returned value is expressed in fee-per-kw, as
// this is the native rate used when computing the fee for commitment
// transactions, and the second-level HTLC transactions.
func (l *channelLink) sampleNetworkFee() (chainfee.SatPerKWeight, error) {
	// We'll first query for the sat/kw recommended to be confirmed within 3
	// blocks.
	feePerKw, err := l.cfg.FeeEstimator.EstimateFeePerKW(3)
	if err != nil {
		return 0, err
	}

	l.log.Debugf("sampled fee rate for 3 block conf: %v sat/kw",
		int64(feePerKw))

	return feePerKw, nil
}

// shouldAdjustCommitFee returns true if we should update our commitment fee to
// match that of the network fee. We'll only update our commitment fee if the
// network fee is +/- 10% to our commitment fee or if our current commitment
// fee is below the minimum relay fee.
func shouldAdjustCommitFee(netFee, chanFee,
	minRelayFee chainfee.SatPerKWeight) bool {

	switch {
	// If the network fee is greater than our current commitment fee and
	// our current commitment fee is below the minimum relay fee then
	// we should switch to it no matter if it is less than a 10% increase.
	case netFee > chanFee && chanFee < minRelayFee:
		return true

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

// failCb is used to cut down on the argument verbosity.
type failCb func(update *lnwire.ChannelUpdate1) lnwire.FailureMessage

// createFailureWithUpdate creates a ChannelUpdate when failing an incoming or
// outgoing HTLC. It may return a FailureMessage that references a channel's
// alias. If the channel does not have an alias, then the regular channel
// update from disk will be returned.
func (l *channelLink) createFailureWithUpdate(incoming bool,
	outgoingScid lnwire.ShortChannelID, cb failCb) lnwire.FailureMessage {

	// Determine which SCID to use in case we need to use aliases in the
	// ChannelUpdate.
	scid := outgoingScid
	if incoming {
		scid = l.ShortChanID()
	}

	// Try using the FailAliasUpdate function. If it returns nil, fallback
	// to the non-alias behavior.
	update := l.cfg.FailAliasUpdate(scid, incoming)
	if update == nil {
		// Fallback to the non-alias behavior.
		var err error
		update, err = l.cfg.FetchLastChannelUpdate(l.ShortChanID())
		if err != nil {
			return &lnwire.FailTemporaryNodeFailure{}
		}
	}

	return cb(update)
}

// syncChanState attempts to synchronize channel states with the remote party.
// This method is to be called upon reconnection after the initial funding
// flow. We'll compare out commitment chains with the remote party, and re-send
// either a danging commit signature, a revocation, or both.
func (l *channelLink) syncChanStates(ctx context.Context) error {
	chanState := l.channel.State()

	l.log.Infof("Attempting to re-synchronize channel: %v", chanState)

	// First, we'll generate our ChanSync message to send to the other
	// side. Based on this message, the remote party will decide if they
	// need to retransmit any data or not.
	localChanSyncMsg, err := chanState.ChanSyncMsg()
	if err != nil {
		return fmt.Errorf("unable to generate chan sync message for "+
			"ChannelPoint(%v)", l.channel.ChannelPoint())
	}
	if err := l.cfg.Peer.SendMessage(true, localChanSyncMsg); err != nil {
		return fmt.Errorf("unable to send chan sync message for "+
			"ChannelPoint(%v): %v", l.channel.ChannelPoint(), err)
	}

	var msgsToReSend []lnwire.Message

	// Next, we'll wait indefinitely to receive the ChanSync message. The
	// first message sent MUST be the ChanSync message.
	select {
	case msg := <-l.upstream:
		l.log.Tracef("Received msg=%v from peer(%x)", msg.MsgType(),
			l.cfg.Peer.PubKey())

		remoteChanSyncMsg, ok := msg.(*lnwire.ChannelReestablish)
		if !ok {
			return fmt.Errorf("first message sent to sync "+
				"should be ChannelReestablish, instead "+
				"received: %T", msg)
		}

		// If the remote party indicates that they think we haven't
		// done any state updates yet, then we'll retransmit the
		// channel_ready message first. We do this, as at this point
		// we can't be sure if they've really received the
		// ChannelReady message.
		if remoteChanSyncMsg.NextLocalCommitHeight == 1 &&
			localChanSyncMsg.NextLocalCommitHeight == 1 &&
			!l.channel.IsPending() {

			l.log.Infof("resending ChannelReady message to peer")

			nextRevocation, err := l.channel.NextRevocationKey()
			if err != nil {
				return fmt.Errorf("unable to create next "+
					"revocation: %v", err)
			}

			channelReadyMsg := lnwire.NewChannelReady(
				l.ChanID(), nextRevocation,
			)

			// If this is a taproot channel, then we'll send the
			// very same nonce that we sent above, as they should
			// take the latest verification nonce we send.
			if chanState.ChanType.IsTaproot() {
				//nolint:ll
				channelReadyMsg.NextLocalNonce = localChanSyncMsg.LocalNonce
			}

			// For channels that negotiated the option-scid-alias
			// feature bit, ensure that we send over the alias in
			// the channel_ready message. We'll send the first
			// alias we find for the channel since it does not
			// matter which alias we send. We'll error out if no
			// aliases are found.
			if l.negotiatedAliasFeature() {
				aliases := l.getAliases()
				if len(aliases) == 0 {
					// This shouldn't happen since we
					// always add at least one alias before
					// the channel reaches the link.
					return fmt.Errorf("no aliases found")
				}

				// getAliases returns a copy of the alias slice
				// so it is ok to use a pointer to the first
				// entry.
				channelReadyMsg.AliasScid = &aliases[0]
			}

			err = l.cfg.Peer.SendMessage(false, channelReadyMsg)
			if err != nil {
				return fmt.Errorf("unable to re-send "+
					"ChannelReady: %v", err)
			}
		}

		// In any case, we'll then process their ChanSync message.
		l.log.Info("received re-establishment message from remote side")

		// If we have an AuxChannelNegotiator we notify any external
		// component for this message. This serves as a notification
		// that the reestablish message was received.
		l.cfg.AuxChannelNegotiator.WhenSome(
			func(acn lnwallet.AuxChannelNegotiator) {
				fundingPoint := l.channel.ChannelPoint()
				cid := lnwire.NewChanIDFromOutPoint(
					fundingPoint,
				)

				acn.ProcessReestablish(
					cid, l.cfg.Peer.PubKey(),
				)
			},
		)

		var (
			openedCircuits []CircuitKey
			closedCircuits []CircuitKey
		)

		// We've just received a ChanSync message from the remote
		// party, so we'll process the message  in order to determine
		// if we need to re-transmit any messages to the remote party.
		ctx, cancel := l.cg.Create(ctx)
		defer cancel()
		msgsToReSend, openedCircuits, closedCircuits, err =
			l.channel.ProcessChanSyncMsg(ctx, remoteChanSyncMsg)
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
			l.log.Infof("sending %v updates to synchronize the "+
				"state", len(msgsToReSend))
		}

		// If we have any messages to retransmit, we'll do so
		// immediately so we return to a synchronized state as soon as
		// possible.
		for _, msg := range msgsToReSend {
			err := l.cfg.Peer.SendMessage(false, msg)
			if err != nil {
				l.log.Errorf("failed to send %v: %v",
					msg.MsgType(), err)
			}
		}

	case <-l.cg.Done():
		return ErrLinkShuttingDown
	}

	return nil
}

// resolveFwdPkgs loads any forwarding packages for this link from disk, and
// reprocesses them in order. The primary goal is to make sure that any HTLCs
// we previously received are reinstated in memory, and forwarded to the switch
// if necessary. After a restart, this will also delete any previously
// completed packages.
func (l *channelLink) resolveFwdPkgs(ctx context.Context) error {
	fwdPkgs, err := l.channel.LoadFwdPkgs()
	if err != nil {
		return err
	}

	l.log.Debugf("loaded %d fwd pks", len(fwdPkgs))

	for _, fwdPkg := range fwdPkgs {
		if err := l.resolveFwdPkg(fwdPkg); err != nil {
			return err
		}
	}

	// If any of our reprocessing steps require an update to the commitment
	// txn, we initiate a state transition to capture all relevant changes.
	if l.channel.NumPendingUpdates(lntypes.Local, lntypes.Remote) > 0 {
		return l.updateCommitTx(ctx)
	}

	return nil
}

// resolveFwdPkg interprets the FwdState of the provided package, either
// reprocesses any outstanding htlcs in the package, or performs garbage
// collection on the package.
func (l *channelLink) resolveFwdPkg(fwdPkg *channeldb.FwdPkg) error {
	// Remove any completed packages to clear up space.
	if fwdPkg.State == channeldb.FwdStateCompleted {
		l.log.Debugf("removing completed fwd pkg for height=%d",
			fwdPkg.Height)

		err := l.channel.RemoveFwdPkgs(fwdPkg.Height)
		if err != nil {
			l.log.Errorf("unable to remove fwd pkg for height=%d: "+
				"%v", fwdPkg.Height, err)
			return err
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
		l.processRemoteSettleFails(fwdPkg)
	}

	// Finally, replay *ALL ADDS* in this forwarding package. The
	// downstream logic is able to filter out any duplicates, but we must
	// shove the entire, original set of adds down the pipeline so that the
	// batch of adds presented to the sphinx router does not ever change.
	if !fwdPkg.AckFilter.IsFull() {
		l.processRemoteAdds(fwdPkg)

		// If the link failed during processing the adds, we must
		// return to ensure we won't attempted to update the state
		// further.
		if l.failed {
			return fmt.Errorf("link failed while " +
				"processing remote adds")
		}
	}

	return nil
}

// fwdPkgGarbager periodically reads all forwarding packages from disk and
// removes those that can be discarded. It is safe to do this entirely in the
// background, since all state is coordinated on disk. This also ensures the
// link can continue to process messages and interleave database accesses.
//
// NOTE: This MUST be run as a goroutine.
func (l *channelLink) fwdPkgGarbager() {
	defer l.cg.WgDone()

	l.cfg.FwdPkgGCTicker.Resume()
	defer l.cfg.FwdPkgGCTicker.Stop()

	if err := l.loadAndRemove(); err != nil {
		l.log.Warnf("unable to run initial fwd pkgs gc: %v", err)
	}

	for {
		select {
		case <-l.cfg.FwdPkgGCTicker.Ticks():
			if err := l.loadAndRemove(); err != nil {
				l.log.Warnf("unable to remove fwd pkgs: %v",
					err)
				continue
			}
		case <-l.cg.Done():
			return
		}
	}
}

// loadAndRemove loads all the channels forwarding packages and determines if
// they can be removed. It is called once before the FwdPkgGCTicker ticks so that
// a longer tick interval can be used.
func (l *channelLink) loadAndRemove() error {
	fwdPkgs, err := l.channel.LoadFwdPkgs()
	if err != nil {
		return err
	}

	var removeHeights []uint64
	for _, fwdPkg := range fwdPkgs {
		if fwdPkg.State != channeldb.FwdStateCompleted {
			continue
		}

		removeHeights = append(removeHeights, fwdPkg.Height)
	}

	// If removeHeights is empty, return early so we don't use a db
	// transaction.
	if len(removeHeights) == 0 {
		return nil
	}

	return l.channel.RemoveFwdPkgs(removeHeights...)
}

// handleChanSyncErr performs the error handling logic in the case where we
// could not successfully syncChanStates with our channel peer.
func (l *channelLink) handleChanSyncErr(err error) {
	l.log.Warnf("error when syncing channel states: %v", err)

	var errDataLoss *lnwallet.ErrCommitSyncLocalDataLoss

	switch {
	case errors.Is(err, ErrLinkShuttingDown):
		l.log.Debugf("unable to sync channel states, link is " +
			"shutting down")
		return

	// We failed syncing the commit chains, probably because the remote has
	// lost state. We should force close the channel.
	case errors.Is(err, lnwallet.ErrCommitSyncRemoteDataLoss):
		fallthrough

	// The remote sent us an invalid last commit secret, we should force
	// close the channel.
	// TODO(halseth): and permanently ban the peer?
	case errors.Is(err, lnwallet.ErrInvalidLastCommitSecret):
		fallthrough

	// The remote sent us a commit point different from what they sent us
	// before.
	// TODO(halseth): ban peer?
	case errors.Is(err, lnwallet.ErrInvalidLocalUnrevokedCommitPoint):
		// We'll fail the link and tell the peer to force close the
		// channel. Note that the database state is not updated here,
		// but will be updated when the close transaction is ready to
		// avoid that we go down before storing the transaction in the
		// db.
		l.failf(
			LinkFailureError{
				code:          ErrSyncError,
				FailureAction: LinkFailureForceClose,
			},
			"unable to synchronize channel states: %v", err,
		)

	// We have lost state and cannot safely force close the channel. Fail
	// the channel and wait for the remote to hopefully force close it. The
	// remote has sent us its latest unrevoked commitment point, and we'll
	// store it in the database, such that we can attempt to recover the
	// funds if the remote force closes the channel.
	case errors.As(err, &errDataLoss):
		err := l.channel.MarkDataLoss(
			errDataLoss.CommitPoint,
		)
		if err != nil {
			l.log.Errorf("unable to mark channel data loss: %v",
				err)
		}

	// We determined the commit chains were not possible to sync. We
	// cautiously fail the channel, but don't force close.
	// TODO(halseth): can we safely force close in any cases where this
	// error is returned?
	case errors.Is(err, lnwallet.ErrCannotSyncCommitChains):
		if err := l.channel.MarkBorked(); err != nil {
			l.log.Errorf("unable to mark channel borked: %v", err)
		}

	// Other, unspecified error.
	default:
	}

	l.failf(
		LinkFailureError{
			code:          ErrRecoveryError,
			FailureAction: LinkFailureForceNone,
		},
		"unable to synchronize channel states: %v", err,
	)
}

// htlcManager is the primary goroutine which drives a channel's commitment
// update state-machine in response to messages received via several channels.
// This goroutine reads messages from the upstream (remote) peer, and also from
// downstream channel managed by the channel link. In the event that an htlc
// needs to be forwarded, then send-only forward handler is used which sends
// htlc packets to the switch. Additionally, this goroutine handles acting upon
// all timeouts for any active HTLCs, manages the channel's revocation window,
// and also the htlc trickle queue+timer for this active channels.
//
// NOTE: This MUST be run as a goroutine.
func (l *channelLink) htlcManager(ctx context.Context) {
	defer func() {
		l.cfg.BatchTicker.Stop()
		l.cg.WgDone()
		l.log.Infof("exited")
	}()

	l.log.Infof("HTLC manager started, bandwidth=%v", l.Bandwidth())

	// Notify any clients that the link is now in the switch via an
	// ActiveLinkEvent. We'll also defer an inactive link notification for
	// when the link exits to ensure that every active notification is
	// matched by an inactive one.
	l.cfg.NotifyActiveLink(l.ChannelPoint())
	defer l.cfg.NotifyInactiveLinkEvent(l.ChannelPoint())

	// If the link is not started for the first time, we need to take extra
	// steps to resume its state.
	err := l.resumeLink(ctx)
	if err != nil {
		l.log.Errorf("resuming link failed: %v", err)
		return
	}

	// Now that we've received both channel_ready and channel reestablish,
	// we can go ahead and send the active channel notification. We'll also
	// defer the inactive notification for when the link exits to ensure
	// that every active notification is matched by an inactive one.
	l.cfg.NotifyActiveChannel(l.ChannelPoint())
	defer l.cfg.NotifyInactiveChannel(l.ChannelPoint())

	for {
		// We must always check if we failed at some point processing
		// the last update before processing the next.
		if l.failed {
			l.log.Errorf("link failed, exiting htlcManager")
			return
		}

		// Pause or resume the batch ticker.
		l.toggleBatchTicker()

		select {
		// We have a new hook that needs to be run when we reach a clean
		// channel state.
		case hook := <-l.flushHooks.newTransients:
			if l.channel.IsChannelClean() {
				hook()
			} else {
				l.flushHooks.alloc(hook)
			}

		// We have a new hook that needs to be run when we have
		// committed all of our updates.
		case hook := <-l.outgoingCommitHooks.newTransients:
			if !l.channel.OweCommitment() {
				hook()
			} else {
				l.outgoingCommitHooks.alloc(hook)
			}

		// We have a new hook that needs to be run when our peer has
		// committed all of their updates.
		case hook := <-l.incomingCommitHooks.newTransients:
			if !l.channel.NeedCommitment() {
				hook()
			} else {
				l.incomingCommitHooks.alloc(hook)
			}

		// Our update fee timer has fired, so we'll check the network
		// fee to see if we should adjust our commitment fee.
		case <-l.updateFeeTimer.C:
			l.updateFeeTimer.Reset(l.randomFeeUpdateTimeout())
			err := l.handleUpdateFee(ctx)
			if err != nil {
				l.log.Errorf("failed to handle update fee: "+
					"%v", err)
			}

		// The underlying channel has notified us of a unilateral close
		// carried out by the remote peer. In the case of such an
		// event, we'll wipe the channel state from the peer, and mark
		// the contract as fully settled. Afterwards we can exit.
		//
		// TODO(roasbeef): add force closure? also breach?
		case <-l.cfg.ChainEvents.RemoteUnilateralClosure:
			l.log.Warnf("remote peer has closed on-chain")

			// TODO(roasbeef): remove all together
			go func() {
				chanPoint := l.channel.ChannelPoint()
				l.cfg.Peer.WipeChannel(&chanPoint)
			}()

			return

		case <-l.cfg.BatchTicker.Ticks():
			// Attempt to extend the remote commitment chain
			// including all the currently pending entries. If the
			// send was unsuccessful, then abandon the update,
			// waiting for the revocation window to open up.
			if !l.updateCommitTxOrFail(ctx) {
				return
			}

		case <-l.cfg.PendingCommitTicker.Ticks():
			l.failf(
				LinkFailureError{
					code:          ErrRemoteUnresponsive,
					FailureAction: LinkFailureDisconnect,
				},
				"unable to complete dance",
			)
			return

		// A message from the switch was just received. This indicates
		// that the link is an intermediate hop in a multi-hop HTLC
		// circuit.
		case pkt := <-l.downstream:
			l.handleDownstreamPkt(ctx, pkt)

		// A message from the connected peer was just received. This
		// indicates that we have a new incoming HTLC, either directly
		// for us, or part of a multi-hop HTLC circuit.
		case msg := <-l.upstream:
			l.handleUpstreamMsg(ctx, msg)

		// A htlc resolution is received. This means that we now have a
		// resolution for a previously accepted htlc.
		case hodlItem := <-l.hodlQueue.ChanOut():
			err := l.handleHtlcResolution(ctx, hodlItem)
			if err != nil {
				l.log.Errorf("failed to handle htlc "+
					"resolution: %v", err)
			}

		// A user-initiated quiescence request is received. We now
		// forward it to the quiescer.
		case qReq := <-l.quiescenceReqs:
			err := l.handleQuiescenceReq(qReq)
			if err != nil {
				l.log.Errorf("failed handle quiescence "+
					"req: %v", err)
			}

		case <-l.cg.Done():
			return
		}
	}
}

// processHodlQueue processes a received htlc resolution and continues reading
// from the hodl queue until no more resolutions remain. When this function
// returns without an error, the commit tx should be updated.
func (l *channelLink) processHodlQueue(ctx context.Context,
	firstResolution invoices.HtlcResolution) error {

	// Try to read all waiting resolution messages, so that they can all be
	// processed in a single commitment tx update.
	htlcResolution := firstResolution
loop:
	for {
		// Lookup all hodl htlcs that can be failed or settled with this event.
		// The hodl htlc must be present in the map.
		circuitKey := htlcResolution.CircuitKey()
		hodlHtlc, ok := l.hodlMap[circuitKey]
		if !ok {
			return fmt.Errorf("hodl htlc not found: %v", circuitKey)
		}

		if err := l.processHtlcResolution(htlcResolution, hodlHtlc); err != nil {
			return err
		}

		// Clean up hodl map.
		delete(l.hodlMap, circuitKey)

		select {
		case item := <-l.hodlQueue.ChanOut():
			htlcResolution = item.(invoices.HtlcResolution)

		// No need to process it if the link is broken.
		case <-l.cg.Done():
			return ErrLinkShuttingDown

		default:
			break loop
		}
	}

	// Update the commitment tx.
	if err := l.updateCommitTx(ctx); err != nil {
		return err
	}

	return nil
}

// processHtlcResolution applies a received htlc resolution to the provided
// htlc. When this function returns without an error, the commit tx should be
// updated.
func (l *channelLink) processHtlcResolution(resolution invoices.HtlcResolution,
	htlc hodlHtlc) error {

	circuitKey := resolution.CircuitKey()

	// Determine required action for the resolution based on the type of
	// resolution we have received.
	switch res := resolution.(type) {
	// Settle htlcs that returned a settle resolution using the preimage
	// in the resolution.
	case *invoices.HtlcSettleResolution:
		l.log.Debugf("received settle resolution for %v "+
			"with outcome: %v", circuitKey, res.Outcome)

		return l.settleHTLC(
			res.Preimage, htlc.add.ID, htlc.sourceRef,
		)

	// For htlc failures, we get the relevant failure message based
	// on the failure resolution and then fail the htlc.
	case *invoices.HtlcFailResolution:
		l.log.Debugf("received cancel resolution for "+
			"%v with outcome: %v", circuitKey, res.Outcome)

		// Get the lnwire failure message based on the resolution
		// result.
		failure := getResolutionFailure(res, htlc.add.Amount)

		l.sendHTLCError(
			htlc.add, htlc.sourceRef, failure, htlc.obfuscator,
			true,
		)
		return nil

	// Fail if we do not get a settle of fail resolution, since we
	// are only expecting to handle settles and fails.
	default:
		return fmt.Errorf("unknown htlc resolution type: %T",
			resolution)
	}
}

// getResolutionFailure returns the wire message that a htlc resolution should
// be failed with.
func getResolutionFailure(resolution *invoices.HtlcFailResolution,
	amount lnwire.MilliSatoshi) *LinkError {

	// If the resolution has been resolved as part of a MPP timeout,
	// we need to fail the htlc with lnwire.FailMppTimeout.
	if resolution.Outcome == invoices.ResultMppTimeout {
		return NewDetailedLinkError(
			&lnwire.FailMPPTimeout{}, resolution.Outcome,
		)
	}

	// If the htlc is not a MPP timeout, we fail it with
	// FailIncorrectDetails. This error is sent for invoice payment
	// failures such as underpayment/ expiry too soon and hodl invoices
	// (which return FailIncorrectDetails to avoid leaking information).
	incorrectDetails := lnwire.NewFailIncorrectDetails(
		amount, uint32(resolution.AcceptHeight),
	)

	return NewDetailedLinkError(incorrectDetails, resolution.Outcome)
}

// randomFeeUpdateTimeout returns a random timeout between the bounds defined
// within the link's configuration that will be used to determine when the link
// should propose an update to its commitment fee rate.
func (l *channelLink) randomFeeUpdateTimeout() time.Duration {
	lower := int64(l.cfg.MinUpdateTimeout)
	upper := int64(l.cfg.MaxUpdateTimeout)
	return time.Duration(prand.Int63n(upper-lower) + lower)
}

// handleDownstreamUpdateAdd processes an UpdateAddHTLC packet sent from the
// downstream HTLC Switch.
func (l *channelLink) handleDownstreamUpdateAdd(ctx context.Context,
	pkt *htlcPacket) error {

	htlc, ok := pkt.htlc.(*lnwire.UpdateAddHTLC)
	if !ok {
		return errors.New("not an UpdateAddHTLC packet")
	}

	// If we are flushing the link in the outgoing direction or we have
	// already sent Stfu, then we can't add new htlcs to the link and we
	// need to bounce it.
	if l.IsFlushing(Outgoing) || !l.quiescer.CanSendUpdates() {
		l.mailBox.FailAdd(pkt)

		return NewDetailedLinkError(
			&lnwire.FailTemporaryChannelFailure{},
			OutgoingFailureLinkNotEligible,
		)
	}

	// If hodl.AddOutgoing mode is active, we exit early to simulate
	// arbitrary delays between the switch adding an ADD to the
	// mailbox, and the HTLC being added to the commitment state.
	if l.cfg.HodlMask.Active(hodl.AddOutgoing) {
		l.log.Warnf(hodl.AddOutgoing.Warning())
		l.mailBox.AckPacket(pkt.inKey())
		return nil
	}

	// Check if we can add the HTLC here without exceededing the max fee
	// exposure threshold.
	if l.isOverexposedWithHtlc(htlc, false) {
		l.log.Debugf("Unable to handle downstream HTLC - max fee " +
			"exposure exceeded")

		l.mailBox.FailAdd(pkt)

		return NewDetailedLinkError(
			lnwire.NewTemporaryChannelFailure(nil),
			OutgoingFailureDownstreamHtlcAdd,
		)
	}

	// A new payment has been initiated via the downstream channel,
	// so we add the new HTLC to our local log, then update the
	// commitment chains.
	htlc.ChanID = l.ChanID()
	openCircuitRef := pkt.inKey()

	// We enforce the fee buffer for the commitment transaction because
	// we are in control of adding this htlc. Nothing has locked-in yet so
	// we can securely enforce the fee buffer which is only relevant if we
	// are the initiator of the channel.
	index, err := l.channel.AddHTLC(htlc, &openCircuitRef)
	if err != nil {
		// The HTLC was unable to be added to the state machine,
		// as a result, we'll signal the switch to cancel the
		// pending payment.
		l.log.Warnf("Unable to handle downstream add HTLC: %v",
			err)

		// Remove this packet from the link's mailbox, this
		// prevents it from being reprocessed if the link
		// restarts and resets it mailbox. If this response
		// doesn't make it back to the originating link, it will
		// be rejected upon attempting to reforward the Add to
		// the switch, since the circuit was never fully opened,
		// and the forwarding package shows it as
		// unacknowledged.
		l.mailBox.FailAdd(pkt)

		return NewDetailedLinkError(
			lnwire.NewTemporaryChannelFailure(nil),
			OutgoingFailureDownstreamHtlcAdd,
		)
	}

	l.log.Tracef("received downstream htlc: payment_hash=%x, "+
		"local_log_index=%v, pend_updates=%v",
		htlc.PaymentHash[:], index,
		l.channel.NumPendingUpdates(lntypes.Local, lntypes.Remote))

	pkt.outgoingChanID = l.ShortChanID()
	pkt.outgoingHTLCID = index
	htlc.ID = index

	l.log.Debugf("queueing keystone of ADD open circuit: %s->%s",
		pkt.inKey(), pkt.outKey())

	l.openedCircuits = append(l.openedCircuits, pkt.inKey())
	l.keystoneBatch = append(l.keystoneBatch, pkt.keystone())

	err = l.cfg.Peer.SendMessage(false, htlc)
	if err != nil {
		l.log.Errorf("failed to send UpdateAddHTLC: %v", err)
	}

	// Send a forward event notification to htlcNotifier.
	l.cfg.HtlcNotifier.NotifyForwardingEvent(
		newHtlcKey(pkt),
		HtlcInfo{
			IncomingTimeLock: pkt.incomingTimeout,
			IncomingAmt:      pkt.incomingAmount,
			OutgoingTimeLock: htlc.Expiry,
			OutgoingAmt:      htlc.Amount,
		},
		getEventType(pkt),
	)

	l.tryBatchUpdateCommitTx(ctx)

	return nil
}

// handleDownstreamPkt processes an HTLC packet sent from the downstream HTLC
// Switch. Possible messages sent by the switch include requests to forward new
// HTLCs, timeout previously cleared HTLCs, and finally to settle currently
// cleared HTLCs with the upstream peer.
//
// TODO(roasbeef): add sync ntfn to ensure switch always has consistent view?
func (l *channelLink) handleDownstreamPkt(ctx context.Context,
	pkt *htlcPacket) {

	if pkt.htlc.MsgType().IsChannelUpdate() &&
		!l.quiescer.CanSendUpdates() {

		l.log.Warnf("unable to process channel update. "+
			"ChannelID=%v is quiescent.", l.ChanID)

		return
	}

	switch htlc := pkt.htlc.(type) {
	case *lnwire.UpdateAddHTLC:
		// Handle add message. The returned error can be ignored,
		// because it is also sent through the mailbox.
		_ = l.handleDownstreamUpdateAdd(ctx, pkt)

	case *lnwire.UpdateFulfillHTLC:
		l.processLocalUpdateFulfillHTLC(ctx, pkt, htlc)

	case *lnwire.UpdateFailHTLC:
		l.processLocalUpdateFailHTLC(ctx, pkt, htlc)
	}
}

// tryBatchUpdateCommitTx updates the commitment transaction if the batch is
// full.
func (l *channelLink) tryBatchUpdateCommitTx(ctx context.Context) {
	pending := l.channel.NumPendingUpdates(lntypes.Local, lntypes.Remote)
	if pending < uint64(l.cfg.BatchSize) {
		return
	}

	l.updateCommitTxOrFail(ctx)
}

// cleanupSpuriousResponse attempts to ack any AddRef or SettleFailRef
// associated with this packet. If successful in doing so, it will also purge
// the open circuit from the circuit map and remove the packet from the link's
// mailbox.
func (l *channelLink) cleanupSpuriousResponse(pkt *htlcPacket) {
	inKey := pkt.inKey()

	l.log.Debugf("cleaning up spurious response for incoming "+
		"circuit-key=%v", inKey)

	// If the htlc packet doesn't have a source reference, it is unsafe to
	// proceed, as skipping this ack may cause the htlc to be reforwarded.
	if pkt.sourceRef == nil {
		l.log.Errorf("unable to cleanup response for incoming "+
			"circuit-key=%v, does not contain source reference",
			inKey)
		return
	}

	// If the source reference is present,  we will try to prevent this link
	// from resending the packet to the switch. To do so, we ack the AddRef
	// of the incoming HTLC belonging to this link.
	err := l.channel.AckAddHtlcs(*pkt.sourceRef)
	if err != nil {
		l.log.Errorf("unable to ack AddRef for incoming "+
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
			l.log.Errorf("unable to ack SettleFailRef "+
				"for incoming circuit-key=%v: %v",
				inKey, err)
		}
	}

	l.log.Debugf("deleting circuit for incoming circuit-key=%x", inKey)

	// With all known references acked, we can now safely delete the circuit
	// from the switch's circuit map, as the state is no longer needed.
	err = l.cfg.Circuits.DeleteCircuits(inKey)
	if err != nil {
		l.log.Errorf("unable to delete circuit for "+
			"circuit-key=%v: %v", inKey, err)
	}
}

// handleUpstreamMsg processes wire messages related to commitment state
// updates from the upstream peer. The upstream peer is the peer whom we have a
// direct channel with, updating our respective commitment chains.
func (l *channelLink) handleUpstreamMsg(ctx context.Context,
	msg lnwire.Message) {

	l.log.Tracef("receive upstream msg %v, handling now... ", msg.MsgType())
	defer l.log.Tracef("handled upstream msg %v", msg.MsgType())

	// First check if the message is an update and we are capable of
	// receiving updates right now.
	if msg.MsgType().IsChannelUpdate() && !l.quiescer.CanRecvUpdates() {
		l.stfuFailf("update received after stfu: %T", msg)
		return
	}

	var err error

	switch msg := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		err = l.processRemoteUpdateAddHTLC(msg)

	case *lnwire.UpdateFulfillHTLC:
		err = l.processRemoteUpdateFulfillHTLC(msg)

	case *lnwire.UpdateFailMalformedHTLC:
		err = l.processRemoteUpdateFailMalformedHTLC(msg)

	case *lnwire.UpdateFailHTLC:
		err = l.processRemoteUpdateFailHTLC(msg)

	case *lnwire.CommitSig:
		err = l.processRemoteCommitSig(ctx, msg)

	case *lnwire.RevokeAndAck:
		err = l.processRemoteRevokeAndAck(ctx, msg)

	case *lnwire.UpdateFee:
		err = l.processRemoteUpdateFee(msg)

	case *lnwire.Stfu:
		err = l.handleStfu(msg)
		if err != nil {
			l.stfuFailf("handleStfu: %v", err)
		}

	// In the case where we receive a warning message from our peer, just
	// log it and move on. We choose not to disconnect from our peer,
	// although we "MAY" do so according to the specification.
	case *lnwire.Warning:
		l.log.Warnf("received warning message from peer: %v",
			msg.Warning())

	case *lnwire.Error:
		l.processRemoteError(msg)

	default:
		l.log.Warnf("received unknown message of type %T", msg)
	}

	if err != nil {
		l.log.Errorf("failed to process remote %v: %v", msg.MsgType(),
			err)
	}
}

// handleStfu implements the top-level logic for handling the Stfu message from
// our peer.
func (l *channelLink) handleStfu(stfu *lnwire.Stfu) error {
	if !l.noDanglingUpdates(lntypes.Remote) {
		return ErrPendingRemoteUpdates
	}
	err := l.quiescer.RecvStfu(*stfu)
	if err != nil {
		return err
	}

	// If we can immediately send an Stfu response back, we will.
	if l.noDanglingUpdates(lntypes.Local) {
		return l.quiescer.SendOwedStfu()
	}

	return nil
}

// stfuFailf fails the link in the case where the requirements of the quiescence
// protocol are violated. In all cases we opt to drop the connection as only
// link state (as opposed to channel state) is affected.
func (l *channelLink) stfuFailf(format string, args ...interface{}) {
	l.failf(LinkFailureError{
		code:             ErrStfuViolation,
		FailureAction:    LinkFailureDisconnect,
		PermanentFailure: false,
		Warning:          true,
	}, format, args...)
}

// noDanglingUpdates returns true when there are 0 updates that were originally
// issued by whose on either the Local or Remote commitment transaction.
func (l *channelLink) noDanglingUpdates(whose lntypes.ChannelParty) bool {
	pendingOnLocal := l.channel.NumPendingUpdates(
		whose, lntypes.Local,
	)
	pendingOnRemote := l.channel.NumPendingUpdates(
		whose, lntypes.Remote,
	)

	return pendingOnLocal == 0 && pendingOnRemote == 0
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

		l.log.Debugf("removing Add packet %s from mailbox", inKey)
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
		l.log.Errorf("unable to delete %d circuits: %v",
			len(l.closedCircuits), err)
		return err
	}

	// With the circuits removed from memory and disk, we now ack any
	// Settle/Fails in the mailbox to ensure they do not get redelivered
	// after startup. If forgive is enabled and we've reached this point,
	// the circuits must have been removed at some point, so it is now safe
	// to un-queue the corresponding Settle/Fails.
	for _, inKey := range l.closedCircuits {
		l.log.Debugf("removing Fail/Settle packet %s from mailbox",
			inKey)
		l.mailBox.AckPacket(inKey)
	}

	// Lastly, reset our buffers to be empty while keeping any acquired
	// growth in the backing array.
	l.openedCircuits = l.openedCircuits[:0]
	l.closedCircuits = l.closedCircuits[:0]

	return nil
}

// updateCommitTxOrFail updates the commitment tx and if that fails, it fails
// the link.
func (l *channelLink) updateCommitTxOrFail(ctx context.Context) bool {
	err := l.updateCommitTx(ctx)
	switch {
	// No error encountered, success.
	case err == nil:

	// A duplicate keystone error should be resolved and is not fatal, so
	// we won't send an Error message to the peer.
	case errors.Is(err, ErrDuplicateKeystone):
		l.failf(LinkFailureError{code: ErrCircuitError},
			"temporary circuit error: %v", err)
		return false

	// Any other error is treated results in an Error message being sent to
	// the peer.
	default:
		l.failf(LinkFailureError{code: ErrInternalError},
			"unable to update commitment: %v", err)
		return false
	}

	return true
}

// updateCommitTx signs, then sends an update to the remote peer adding a new
// commitment to their commitment chain which includes all the latest updates
// we've received+processed up to this point.
func (l *channelLink) updateCommitTx(ctx context.Context) error {
	// Preemptively write all pending keystones to disk, just in case the
	// HTLCs we have in memory are included in the subsequent attempt to
	// sign a commitment state.
	err := l.cfg.Circuits.OpenCircuits(l.keystoneBatch...)
	if err != nil {
		// If ErrDuplicateKeystone is returned, the caller will catch
		// it.
		return err
	}

	// Reset the batch, but keep the backing buffer to avoid reallocating.
	l.keystoneBatch = l.keystoneBatch[:0]

	// If hodl.Commit mode is active, we will refrain from attempting to
	// commit any in-memory modifications to the channel state. Exiting here
	// permits testing of either the switch or link's ability to trim
	// circuits that have been opened, but unsuccessfully committed.
	if l.cfg.HodlMask.Active(hodl.Commit) {
		l.log.Warnf(hodl.Commit.Warning())
		return nil
	}

	ctx, done := l.cg.Create(ctx)
	defer done()

	newCommit, err := l.channel.SignNextCommitment(ctx)
	if err == lnwallet.ErrNoWindow {
		l.cfg.PendingCommitTicker.Resume()
		l.log.Trace("PendingCommitTicker resumed")

		n := l.channel.NumPendingUpdates(lntypes.Local, lntypes.Remote)
		l.log.Tracef("revocation window exhausted, unable to send: "+
			"%v, pend_updates=%v, dangling_closes%v", n,
			lnutils.SpewLogClosure(l.openedCircuits),
			lnutils.SpewLogClosure(l.closedCircuits))

		return nil
	} else if err != nil {
		return err
	}

	if err := l.ackDownStreamPackets(); err != nil {
		return err
	}

	l.cfg.PendingCommitTicker.Pause()
	l.log.Trace("PendingCommitTicker paused after ackDownStreamPackets")

	// The remote party now has a new pending commitment, so we'll update
	// the contract court to be aware of this new set (the prior old remote
	// pending).
	newUpdate := &contractcourt.ContractUpdate{
		HtlcKey: contractcourt.RemotePendingHtlcSet,
		Htlcs:   newCommit.PendingHTLCs,
	}
	err = l.cfg.NotifyContractUpdate(newUpdate)
	if err != nil {
		l.log.Errorf("unable to notify contract update: %v", err)
		return err
	}

	select {
	case <-l.cg.Done():
		return ErrLinkShuttingDown
	default:
	}

	auxBlobRecords, err := lnwire.ParseCustomRecords(newCommit.AuxSigBlob)
	if err != nil {
		return fmt.Errorf("error parsing aux sigs: %w", err)
	}

	commitSig := &lnwire.CommitSig{
		ChanID:        l.ChanID(),
		CommitSig:     newCommit.CommitSig,
		HtlcSigs:      newCommit.HtlcSigs,
		PartialSig:    newCommit.PartialSig,
		CustomRecords: auxBlobRecords,
	}
	err = l.cfg.Peer.SendMessage(false, commitSig)
	if err != nil {
		l.log.Errorf("failed to send CommitSig: %v", err)
	}

	// Now that we have sent out a new CommitSig, we invoke the outgoing set
	// of commit hooks.
	l.RWMutex.Lock()
	l.outgoingCommitHooks.invoke()
	l.RWMutex.Unlock()

	return nil
}

// Peer returns the representation of remote peer with which we have the
// channel link opened.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) PeerPubKey() [33]byte {
	return l.cfg.Peer.PubKey()
}

// ChannelPoint returns the channel outpoint for the channel link.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) ChannelPoint() wire.OutPoint {
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

	return l.channel.ShortChanID()
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
	err := l.channel.State().Refresh()
	if err != nil {
		l.log.Errorf("unable to refresh short_chan_id for chan_id=%v: "+
			"%v", chanID, err)
		return hop.Source, err
	}

	return hop.Source, nil
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
	// Get the balance available on the channel for new HTLCs. This takes
	// the channel reserve into account so HTLCs up to this value won't
	// violate it.
	return l.channel.AvailableBalance()
}

// MayAddOutgoingHtlc indicates whether we can add an outgoing htlc with the
// amount provided to the link. This check does not reserve a space, since
// forwards or other payments may use the available slot, so it should be
// considered best-effort.
func (l *channelLink) MayAddOutgoingHtlc(amt lnwire.MilliSatoshi) error {
	return l.channel.MayAddOutgoingHtlc(amt)
}

// getDustSum is a wrapper method that calls the underlying channel's dust sum
// method.
//
// NOTE: Part of the dustHandler interface.
func (l *channelLink) getDustSum(whoseCommit lntypes.ChannelParty,
	dryRunFee fn.Option[chainfee.SatPerKWeight]) lnwire.MilliSatoshi {

	return l.channel.GetDustSum(whoseCommit, dryRunFee)
}

// getFeeRate is a wrapper method that retrieves the underlying channel's
// feerate.
//
// NOTE: Part of the dustHandler interface.
func (l *channelLink) getFeeRate() chainfee.SatPerKWeight {
	return l.channel.CommitFeeRate()
}

// getDustClosure returns a closure that can be used by the switch or mailbox
// to evaluate whether a given HTLC is dust.
//
// NOTE: Part of the dustHandler interface.
func (l *channelLink) getDustClosure() dustClosure {
	localDustLimit := l.channel.State().LocalChanCfg.DustLimit
	remoteDustLimit := l.channel.State().RemoteChanCfg.DustLimit
	chanType := l.channel.State().ChanType

	return dustHelper(chanType, localDustLimit, remoteDustLimit)
}

// getCommitFee returns either the local or remote CommitFee in satoshis. This
// is used so that the Switch can have access to the commitment fee without
// needing to have a *LightningChannel. This doesn't include dust.
//
// NOTE: Part of the dustHandler interface.
func (l *channelLink) getCommitFee(remote bool) btcutil.Amount {
	if remote {
		return l.channel.State().RemoteCommitment.CommitFee
	}

	return l.channel.State().LocalCommitment.CommitFee
}

// exceedsFeeExposureLimit returns whether or not the new proposed fee-rate
// increases the total dust and fees within the channel past the configured
// fee threshold. It first calculates the dust sum over every update in the
// update log with the proposed fee-rate and taking into account both the local
// and remote dust limits. It uses every update in the update log instead of
// what is actually on the local and remote commitments because it is assumed
// that in a worst-case scenario, every update in the update log could
// theoretically be on either commitment transaction and this needs to be
// accounted for with this fee-rate. It then calculates the local and remote
// commitment fees given the proposed fee-rate. Finally, it tallies the results
// and determines if the fee threshold has been exceeded.
func (l *channelLink) exceedsFeeExposureLimit(
	feePerKw chainfee.SatPerKWeight) (bool, error) {

	dryRunFee := fn.Some[chainfee.SatPerKWeight](feePerKw)

	// Get the sum of dust for both the local and remote commitments using
	// this "dry-run" fee.
	localDustSum := l.getDustSum(lntypes.Local, dryRunFee)
	remoteDustSum := l.getDustSum(lntypes.Remote, dryRunFee)

	// Calculate the local and remote commitment fees using this dry-run
	// fee.
	localFee, remoteFee, err := l.channel.CommitFeeTotalAt(feePerKw)
	if err != nil {
		return false, err
	}

	// Finally, check whether the max fee exposure was exceeded on either
	// future commitment transaction with the fee-rate.
	totalLocalDust := localDustSum + lnwire.NewMSatFromSatoshis(localFee)
	if totalLocalDust > l.cfg.MaxFeeExposure {
		l.log.Debugf("ChannelLink(%v): exceeds fee exposure limit: "+
			"local dust: %v, local fee: %v", l.ShortChanID(),
			totalLocalDust, localFee)

		return true, nil
	}

	totalRemoteDust := remoteDustSum + lnwire.NewMSatFromSatoshis(
		remoteFee,
	)

	if totalRemoteDust > l.cfg.MaxFeeExposure {
		l.log.Debugf("ChannelLink(%v): exceeds fee exposure limit: "+
			"remote dust: %v, remote fee: %v", l.ShortChanID(),
			totalRemoteDust, remoteFee)

		return true, nil
	}

	return false, nil
}

// isOverexposedWithHtlc calculates whether the proposed HTLC will make the
// channel exceed the fee threshold. It first fetches the largest fee-rate that
// may be on any unrevoked commitment transaction. Then, using this fee-rate,
// determines if the to-be-added HTLC is dust. If the HTLC is dust, it adds to
// the overall dust sum. If it is not dust, it contributes to weight, which
// also adds to the overall dust sum by an increase in fees. If the dust sum on
// either commitment exceeds the configured fee threshold, this function
// returns true.
func (l *channelLink) isOverexposedWithHtlc(htlc *lnwire.UpdateAddHTLC,
	incoming bool) bool {

	dustClosure := l.getDustClosure()

	feeRate := l.channel.WorstCaseFeeRate()

	amount := htlc.Amount.ToSatoshis()

	// See if this HTLC is dust on both the local and remote commitments.
	isLocalDust := dustClosure(feeRate, incoming, lntypes.Local, amount)
	isRemoteDust := dustClosure(feeRate, incoming, lntypes.Remote, amount)

	// Calculate the dust sum for the local and remote commitments.
	localDustSum := l.getDustSum(
		lntypes.Local, fn.None[chainfee.SatPerKWeight](),
	)
	remoteDustSum := l.getDustSum(
		lntypes.Remote, fn.None[chainfee.SatPerKWeight](),
	)

	// Grab the larger of the local and remote commitment fees w/o dust.
	commitFee := l.getCommitFee(false)

	if l.getCommitFee(true) > commitFee {
		commitFee = l.getCommitFee(true)
	}

	commitFeeMSat := lnwire.NewMSatFromSatoshis(commitFee)

	localDustSum += commitFeeMSat
	remoteDustSum += commitFeeMSat

	// Calculate the additional fee increase if this is a non-dust HTLC.
	weight := lntypes.WeightUnit(input.HTLCWeight)
	additional := lnwire.NewMSatFromSatoshis(
		feeRate.FeeForWeight(weight),
	)

	if isLocalDust {
		// If this is dust, it doesn't contribute to weight but does
		// contribute to the overall dust sum.
		localDustSum += lnwire.NewMSatFromSatoshis(amount)
	} else {
		// Account for the fee increase that comes with an increase in
		// weight.
		localDustSum += additional
	}

	if localDustSum > l.cfg.MaxFeeExposure {
		// The max fee exposure was exceeded.
		l.log.Debugf("ChannelLink(%v): HTLC %v makes the channel "+
			"overexposed, total local dust: %v (current commit "+
			"fee: %v)", l.ShortChanID(), htlc, localDustSum)

		return true
	}

	if isRemoteDust {
		// If this is dust, it doesn't contribute to weight but does
		// contribute to the overall dust sum.
		remoteDustSum += lnwire.NewMSatFromSatoshis(amount)
	} else {
		// Account for the fee increase that comes with an increase in
		// weight.
		remoteDustSum += additional
	}

	if remoteDustSum > l.cfg.MaxFeeExposure {
		// The max fee exposure was exceeded.
		l.log.Debugf("ChannelLink(%v): HTLC %v makes the channel "+
			"overexposed, total remote dust: %v (current commit "+
			"fee: %v)", l.ShortChanID(), htlc, remoteDustSum)

		return true
	}

	return false
}

// dustClosure is a function that evaluates whether an HTLC is dust. It returns
// true if the HTLC is dust. It takes in a feerate, a boolean denoting whether
// the HTLC is incoming (i.e. one that the remote sent), a boolean denoting
// whether to evaluate on the local or remote commit, and finally an HTLC
// amount to test.
type dustClosure func(feerate chainfee.SatPerKWeight, incoming bool,
	whoseCommit lntypes.ChannelParty, amt btcutil.Amount) bool

// dustHelper is used to construct the dustClosure.
func dustHelper(chantype channeldb.ChannelType, localDustLimit,
	remoteDustLimit btcutil.Amount) dustClosure {

	isDust := func(feerate chainfee.SatPerKWeight, incoming bool,
		whoseCommit lntypes.ChannelParty, amt btcutil.Amount) bool {

		var dustLimit btcutil.Amount
		if whoseCommit.IsLocal() {
			dustLimit = localDustLimit
		} else {
			dustLimit = remoteDustLimit
		}

		return lnwallet.HtlcIsDust(
			chantype, incoming, whoseCommit, feerate, amt,
			dustLimit,
		)
	}

	return isDust
}

// zeroConfConfirmed returns whether or not the zero-conf channel has
// confirmed on-chain.
//
// Part of the scidAliasHandler interface.
func (l *channelLink) zeroConfConfirmed() bool {
	return l.channel.State().ZeroConfConfirmed()
}

// confirmedScid returns the confirmed SCID for a zero-conf channel. This
// should not be called for non-zero-conf channels.
//
// Part of the scidAliasHandler interface.
func (l *channelLink) confirmedScid() lnwire.ShortChannelID {
	return l.channel.State().ZeroConfRealScid()
}

// isZeroConf returns whether or not the underlying channel is a zero-conf
// channel.
//
// Part of the scidAliasHandler interface.
func (l *channelLink) isZeroConf() bool {
	return l.channel.State().IsZeroConf()
}

// negotiatedAliasFeature returns whether or not the underlying channel has
// negotiated the option-scid-alias feature bit. This will be true for both
// option-scid-alias and zero-conf channel-types. It will also be true for
// channels with the feature bit but without the above channel-types.
//
// Part of the scidAliasFeature interface.
func (l *channelLink) negotiatedAliasFeature() bool {
	return l.channel.State().NegotiatedAliasFeature()
}

// getAliases returns the set of aliases for the underlying channel.
//
// Part of the scidAliasHandler interface.
func (l *channelLink) getAliases() []lnwire.ShortChannelID {
	return l.cfg.GetAliases(l.ShortChanID())
}

// attachFailAliasUpdate sets the link's FailAliasUpdate function.
//
// Part of the scidAliasHandler interface.
func (l *channelLink) attachFailAliasUpdate(closure func(
	sid lnwire.ShortChannelID, incoming bool) *lnwire.ChannelUpdate1) {

	l.Lock()
	l.cfg.FailAliasUpdate = closure
	l.Unlock()
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

	// Set the mailbox's fee rate. This may be refreshing a feerate that was
	// never committed.
	l.mailBox.SetFeeRate(l.getFeeRate())

	// Also set the mailbox's dust closure so that it can query whether HTLC's
	// are dust given the current feerate.
	l.mailBox.SetDustClosure(l.getDustClosure())
}

// UpdateForwardingPolicy updates the forwarding policy for the target
// ChannelLink. Once updated, the link will use the new forwarding policy to
// govern if it an incoming HTLC should be forwarded or not. We assume that
// fields that are zero are intentionally set to zero, so we'll use newPolicy to
// update all of the link's FwrdingPolicy's values.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) UpdateForwardingPolicy(
	newPolicy models.ForwardingPolicy) {

	l.Lock()
	defer l.Unlock()

	l.cfg.FwrdingPolicy = newPolicy
}

// CheckHtlcForward should return a nil error if the passed HTLC details
// satisfy the current forwarding policy fo the target link. Otherwise,
// a LinkError with a valid protocol failure message should be returned
// in order to signal to the source of the HTLC, the policy consistency
// issue.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) CheckHtlcForward(payHash [32]byte, incomingHtlcAmt,
	amtToForward lnwire.MilliSatoshi, incomingTimeout,
	outgoingTimeout uint32, inboundFee models.InboundFee,
	heightNow uint32, originalScid lnwire.ShortChannelID,
	customRecords lnwire.CustomRecords) *LinkError {

	l.RLock()
	policy := l.cfg.FwrdingPolicy
	l.RUnlock()

	// Using the outgoing HTLC amount, we'll calculate the outgoing
	// fee this incoming HTLC must carry in order to satisfy the constraints
	// of the outgoing link.
	outFee := ExpectedFee(policy, amtToForward)

	// Then calculate the inbound fee that we charge based on the sum of
	// outgoing HTLC amount and outgoing fee.
	inFee := inboundFee.CalcFee(amtToForward + outFee)

	// Add up both fee components. It is important to calculate both fees
	// separately. An alternative way of calculating is to first determine
	// an aggregate fee and apply that to the outgoing HTLC amount. However,
	// rounding may cause the result to be slightly higher than in the case
	// of separately rounded fee components. This potentially causes failed
	// forwards for senders and is something to be avoided.
	expectedFee := inFee + int64(outFee)

	// If the actual fee is less than our expected fee, then we'll reject
	// this HTLC as it didn't provide a sufficient amount of fees, or the
	// values have been tampered with, or the send used incorrect/dated
	// information to construct the forwarding information for this hop. In
	// any case, we'll cancel this HTLC.
	actualFee := int64(incomingHtlcAmt) - int64(amtToForward)
	if incomingHtlcAmt < amtToForward || actualFee < expectedFee {
		l.log.Warnf("outgoing htlc(%x) has insufficient fee: "+
			"expected %v, got %v: incoming=%v, outgoing=%v, "+
			"inboundFee=%v",
			payHash[:], expectedFee, actualFee,
			incomingHtlcAmt, amtToForward, inboundFee,
		)

		// As part of the returned error, we'll send our latest routing
		// policy so the sending node obtains the most up to date data.
		cb := func(upd *lnwire.ChannelUpdate1) lnwire.FailureMessage {
			return lnwire.NewFeeInsufficient(amtToForward, *upd)
		}
		failure := l.createFailureWithUpdate(false, originalScid, cb)
		return NewLinkError(failure)
	}

	// Check whether the outgoing htlc satisfies the channel policy.
	err := l.canSendHtlc(
		policy, payHash, amtToForward, outgoingTimeout, heightNow,
		originalScid, customRecords,
	)
	if err != nil {
		return err
	}

	// Finally, we'll ensure that the time-lock on the outgoing HTLC meets
	// the following constraint: the incoming time-lock minus our time-lock
	// delta should equal the outgoing time lock. Otherwise, whether the
	// sender messed up, or an intermediate node tampered with the HTLC.
	timeDelta := policy.TimeLockDelta
	if incomingTimeout < outgoingTimeout+timeDelta {
		l.log.Warnf("incoming htlc(%x) has incorrect time-lock value: "+
			"expected at least %v block delta, got %v block delta",
			payHash[:], timeDelta, incomingTimeout-outgoingTimeout)

		// Grab the latest routing policy so the sending node is up to
		// date with our current policy.
		cb := func(upd *lnwire.ChannelUpdate1) lnwire.FailureMessage {
			return lnwire.NewIncorrectCltvExpiry(
				incomingTimeout, *upd,
			)
		}
		failure := l.createFailureWithUpdate(false, originalScid, cb)
		return NewLinkError(failure)
	}

	return nil
}

// CheckHtlcTransit should return a nil error if the passed HTLC details
// satisfy the current channel policy.  Otherwise, a LinkError with a
// valid protocol failure message should be returned in order to signal
// the violation. This call is intended to be used for locally initiated
// payments for which there is no corresponding incoming htlc.
func (l *channelLink) CheckHtlcTransit(payHash [32]byte,
	amt lnwire.MilliSatoshi, timeout uint32, heightNow uint32,
	customRecords lnwire.CustomRecords) *LinkError {

	l.RLock()
	policy := l.cfg.FwrdingPolicy
	l.RUnlock()

	// We pass in hop.Source here as this is only used in the Switch when
	// trying to send over a local link. This causes the fallback mechanism
	// to occur.
	return l.canSendHtlc(
		policy, payHash, amt, timeout, heightNow, hop.Source,
		customRecords,
	)
}

// canSendHtlc checks whether the given htlc parameters satisfy
// the channel's amount and time lock constraints.
func (l *channelLink) canSendHtlc(policy models.ForwardingPolicy,
	payHash [32]byte, amt lnwire.MilliSatoshi, timeout uint32,
	heightNow uint32, originalScid lnwire.ShortChannelID,
	customRecords lnwire.CustomRecords) *LinkError {

	// Validate HTLC amount against policy limits.
	linkErr := l.validateHtlcAmount(
		policy, payHash, amt, originalScid, customRecords,
	)
	if linkErr != nil {
		return linkErr
	}

	// We want to avoid offering an HTLC which will expire in the near
	// future, so we'll reject an HTLC if the outgoing expiration time is
	// too close to the current height.
	if timeout <= heightNow+l.cfg.OutgoingCltvRejectDelta {
		l.log.Warnf("htlc(%x) has an expiry that's too soon: "+
			"outgoing_expiry=%v, best_height=%v", payHash[:],
			timeout, heightNow)

		cb := func(upd *lnwire.ChannelUpdate1) lnwire.FailureMessage {
			return lnwire.NewExpiryTooSoon(*upd)
		}
		failure := l.createFailureWithUpdate(false, originalScid, cb)

		return NewLinkError(failure)
	}

	// Check absolute max delta.
	if timeout > l.cfg.MaxOutgoingCltvExpiry+heightNow {
		l.log.Warnf("outgoing htlc(%x) has a time lock too far in "+
			"the future: got %v, but maximum is %v", payHash[:],
			timeout-heightNow, l.cfg.MaxOutgoingCltvExpiry)

		return NewLinkError(&lnwire.FailExpiryTooFar{})
	}

	// We now check the available bandwidth to see if this HTLC can be
	// forwarded.
	availableBandwidth := l.Bandwidth()

	auxBandwidth, externalErr := fn.MapOptionZ(
		l.cfg.AuxTrafficShaper,
		func(ts AuxTrafficShaper) fn.Result[OptionalBandwidth] {
			var htlcBlob fn.Option[tlv.Blob]
			blob, err := customRecords.Serialize()
			if err != nil {
				return fn.Err[OptionalBandwidth](
					fmt.Errorf("unable to serialize "+
						"custom records: %w", err))
			}

			if len(blob) > 0 {
				htlcBlob = fn.Some(blob)
			}

			return l.AuxBandwidth(amt, originalScid, htlcBlob, ts)
		},
	).Unpack()
	if externalErr != nil {
		l.log.Errorf("Unable to determine aux bandwidth: %v",
			externalErr)

		return NewLinkError(&lnwire.FailTemporaryNodeFailure{})
	}

	if auxBandwidth.IsHandled && auxBandwidth.Bandwidth.IsSome() {
		auxBandwidth.Bandwidth.WhenSome(
			func(bandwidth lnwire.MilliSatoshi) {
				availableBandwidth = bandwidth
			},
		)
	}

	// Check to see if there is enough balance in this channel.
	if amt > availableBandwidth {
		l.log.Warnf("insufficient bandwidth to route htlc: %v is "+
			"larger than %v", amt, availableBandwidth)
		cb := func(upd *lnwire.ChannelUpdate1) lnwire.FailureMessage {
			return lnwire.NewTemporaryChannelFailure(upd)
		}
		failure := l.createFailureWithUpdate(false, originalScid, cb)

		return NewDetailedLinkError(
			failure, OutgoingFailureInsufficientBalance,
		)
	}

	return nil
}

// AuxBandwidth returns the bandwidth that can be used for a channel, expressed
// in milli-satoshi. This might be different from the regular BTC bandwidth for
// custom channels. This will always return fn.None() for a regular (non-custom)
// channel.
func (l *channelLink) AuxBandwidth(amount lnwire.MilliSatoshi,
	cid lnwire.ShortChannelID, htlcBlob fn.Option[tlv.Blob],
	ts AuxTrafficShaper) fn.Result[OptionalBandwidth] {

	fundingBlob := l.FundingCustomBlob()
	shouldHandle, err := ts.ShouldHandleTraffic(cid, fundingBlob, htlcBlob)
	if err != nil {
		return fn.Err[OptionalBandwidth](fmt.Errorf("traffic shaper "+
			"failed to decide whether to handle traffic: %w", err))
	}

	log.Debugf("ShortChannelID=%v: aux traffic shaper is handling "+
		"traffic: %v", cid, shouldHandle)

	// If this channel isn't handled by the aux traffic shaper, we'll return
	// early.
	if !shouldHandle {
		return fn.Ok(OptionalBandwidth{
			IsHandled: false,
		})
	}

	peerBytes := l.cfg.Peer.PubKey()

	peer, err := route.NewVertexFromBytes(peerBytes[:])
	if err != nil {
		return fn.Err[OptionalBandwidth](fmt.Errorf("failed to decode "+
			"peer pub key: %v", err))
	}

	// Ask for a specific bandwidth to be used for the channel.
	commitmentBlob := l.CommitmentCustomBlob()
	auxBandwidth, err := ts.PaymentBandwidth(
		fundingBlob, htlcBlob, commitmentBlob, l.Bandwidth(), amount,
		l.channel.FetchLatestAuxHTLCView(), peer,
	)
	if err != nil {
		return fn.Err[OptionalBandwidth](fmt.Errorf("failed to get "+
			"bandwidth from external traffic shaper: %w", err))
	}

	log.Debugf("ShortChannelID=%v: aux traffic shaper reported available "+
		"bandwidth: %v", cid, auxBandwidth)

	return fn.Ok(OptionalBandwidth{
		IsHandled: true,
		Bandwidth: fn.Some(auxBandwidth),
	})
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

// handleSwitchPacket handles the switch packets. This packets which might be
// forwarded to us from another channel link in case the htlc update came from
// another peer or if the update was created by user
//
// NOTE: Part of the packetHandler interface.
func (l *channelLink) handleSwitchPacket(pkt *htlcPacket) error {
	l.log.Tracef("received switch packet inkey=%v, outkey=%v",
		pkt.inKey(), pkt.outKey())

	return l.mailBox.AddPacket(pkt)
}

// HandleChannelUpdate handles the htlc requests as settle/add/fail which sent
// to us from remote peer we have a channel with.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) HandleChannelUpdate(message lnwire.Message) {
	select {
	case <-l.cg.Done():
		// Return early if the link is already in the process of
		// quitting. It doesn't make sense to hand the message to the
		// mailbox here.
		return
	default:
	}

	err := l.mailBox.AddMessage(message)
	if err != nil {
		l.log.Errorf("failed to add Message to mailbox: %v", err)
	}
}

// updateChannelFee updates the commitment fee-per-kw on this channel by
// committing to an update_fee message.
func (l *channelLink) updateChannelFee(ctx context.Context,
	feePerKw chainfee.SatPerKWeight) error {

	l.log.Infof("updating commit fee to %v", feePerKw)

	// We skip sending the UpdateFee message if the channel is not
	// currently eligible to forward messages.
	if !l.eligibleToUpdate() {
		l.log.Debugf("skipping fee update for inactive channel")
		return nil
	}

	// Check and see if our proposed fee-rate would make us exceed the fee
	// threshold.
	thresholdExceeded, err := l.exceedsFeeExposureLimit(feePerKw)
	if err != nil {
		// This shouldn't typically happen. If it does, it indicates
		// something is wrong with our channel state.
		return err
	}

	if thresholdExceeded {
		return fmt.Errorf("link fee threshold exceeded")
	}

	// First, we'll update the local fee on our commitment.
	if err := l.channel.UpdateFee(feePerKw); err != nil {
		return err
	}

	// The fee passed the channel's validation checks, so we update the
	// mailbox feerate.
	l.mailBox.SetFeeRate(feePerKw)

	// We'll then attempt to send a new UpdateFee message, and also lock it
	// in immediately by triggering a commitment update.
	msg := lnwire.NewUpdateFee(l.ChanID(), uint32(feePerKw))
	if err := l.cfg.Peer.SendMessage(false, msg); err != nil {
		return err
	}

	return l.updateCommitTx(ctx)
}

// processRemoteSettleFails accepts a batch of settle/fail payment descriptors
// after receiving a revocation from the remote party, and reprocesses them in
// the context of the provided forwarding package. Any settles or fails that
// have already been acknowledged in the forwarding package will not be sent to
// the switch.
func (l *channelLink) processRemoteSettleFails(fwdPkg *channeldb.FwdPkg) {
	if len(fwdPkg.SettleFails) == 0 {
		l.log.Trace("fwd package has no settle/fails to process " +
			"exiting early")

		return
	}

	// Exit early if the fwdPkg is already processed.
	if fwdPkg.State == channeldb.FwdStateCompleted {
		l.log.Debugf("skipped processing completed fwdPkg %v", fwdPkg)

		return
	}

	l.log.Debugf("settle-fail-filter: %v", fwdPkg.SettleFailFilter)

	var switchPackets []*htlcPacket
	for i, update := range fwdPkg.SettleFails {
		destRef := fwdPkg.DestRef(uint16(i))

		// Skip any settles or fails that have already been
		// acknowledged by the incoming link that originated the
		// forwarded Add.
		if fwdPkg.SettleFailFilter.Contains(uint16(i)) {
			continue
		}

		// TODO(roasbeef): rework log entries to a shared
		// interface.

		switch msg := update.UpdateMsg.(type) {
		// A settle for an HTLC we previously forwarded HTLC has been
		// received. So we'll forward the HTLC to the switch which will
		// handle propagating the settle to the prior hop.
		case *lnwire.UpdateFulfillHTLC:
			// If hodl.SettleIncoming is requested, we will not
			// forward the SETTLE to the switch and will not signal
			// a free slot on the commitment transaction.
			if l.cfg.HodlMask.Active(hodl.SettleIncoming) {
				l.log.Warnf(hodl.SettleIncoming.Warning())
				continue
			}

			settlePacket := &htlcPacket{
				outgoingChanID: l.ShortChanID(),
				outgoingHTLCID: msg.ID,
				destRef:        &destRef,
				htlc:           msg,
			}

			// Add the packet to the batch to be forwarded, and
			// notify the overflow queue that a spare spot has been
			// freed up within the commitment state.
			switchPackets = append(switchPackets, settlePacket)

		// A failureCode message for a previously forwarded HTLC has
		// been received. As a result a new slot will be freed up in
		// our commitment state, so we'll forward this to the switch so
		// the backwards undo can continue.
		case *lnwire.UpdateFailHTLC:
			// If hodl.SettleIncoming is requested, we will not
			// forward the FAIL to the switch and will not signal a
			// free slot on the commitment transaction.
			if l.cfg.HodlMask.Active(hodl.FailIncoming) {
				l.log.Warnf(hodl.FailIncoming.Warning())
				continue
			}

			// Fetch the reason the HTLC was canceled so we can
			// continue to propagate it. This failure originated
			// from another node, so the linkFailure field is not
			// set on the packet.
			failPacket := &htlcPacket{
				outgoingChanID: l.ShortChanID(),
				outgoingHTLCID: msg.ID,
				destRef:        &destRef,
				htlc:           msg,
			}

			l.log.Debugf("Failed to send HTLC with ID=%d", msg.ID)

			// If the failure message lacks an HMAC (but includes
			// the 4 bytes for encoding the message and padding
			// lengths, then this means that we received it as an
			// UpdateFailMalformedHTLC. As a result, we'll signal
			// that we need to convert this error within the switch
			// to an actual error, by encrypting it as if we were
			// the originating hop.
			convertedErrorSize := lnwire.FailureMessageLength + 4
			if len(msg.Reason) == convertedErrorSize {
				failPacket.convertedError = true
			}

			// Add the packet to the batch to be forwarded, and
			// notify the overflow queue that a spare spot has been
			// freed up within the commitment state.
			switchPackets = append(switchPackets, failPacket)
		}
	}

	// Only spawn the task forward packets we have a non-zero number.
	if len(switchPackets) > 0 {
		go l.forwardBatch(false, switchPackets...)
	}
}

// processRemoteAdds serially processes each of the Add payment descriptors
// which have been "locked-in" by receiving a revocation from the remote party.
// The forwarding package provided instructs how to process this batch,
// indicating whether this is the first time these Adds are being processed, or
// whether we are reprocessing as a result of a failure or restart. Adds that
// have already been acknowledged in the forwarding package will be ignored.
//
// NOTE: This function needs also be called for fwd packages with no ADDs
// because it marks the fwdPkg as processed by writing the FwdFilter into the
// database.
//
//nolint:funlen
func (l *channelLink) processRemoteAdds(fwdPkg *channeldb.FwdPkg) {
	// Exit early if the fwdPkg is already processed.
	if fwdPkg.State == channeldb.FwdStateCompleted {
		l.log.Debugf("skipped processing completed fwdPkg %v", fwdPkg)

		return
	}

	l.log.Tracef("processing %d remote adds for height %d",
		len(fwdPkg.Adds), fwdPkg.Height)

	// decodeReqs is a list of requests sent to the onion decoder. We expect
	// the same length of responses to be returned.
	decodeReqs := make([]hop.DecodeHopIteratorRequest, 0, len(fwdPkg.Adds))

	// unackedAdds is a list of ADDs that's waiting for the remote's
	// settle/fail update.
	unackedAdds := make([]*lnwire.UpdateAddHTLC, 0, len(fwdPkg.Adds))

	for i, update := range fwdPkg.Adds {
		// If this index is already found in the ack filter, the
		// response to this forwarding decision has already been
		// committed by one of our commitment txns. ADDs in this state
		// are waiting for the rest of the fwding package to get acked
		// before being garbage collected.
		if fwdPkg.State == channeldb.FwdStateProcessed &&
			fwdPkg.AckFilter.Contains(uint16(i)) {

			continue
		}

		if msg, ok := update.UpdateMsg.(*lnwire.UpdateAddHTLC); ok {
			// Before adding the new htlc to the state machine,
			// parse the onion object in order to obtain the
			// routing information with DecodeHopIterator function
			// which process the Sphinx packet.
			onionReader := bytes.NewReader(msg.OnionBlob[:])

			req := hop.DecodeHopIteratorRequest{
				OnionReader:    onionReader,
				RHash:          msg.PaymentHash[:],
				IncomingCltv:   msg.Expiry,
				IncomingAmount: msg.Amount,
				BlindingPoint:  msg.BlindingPoint,
			}

			decodeReqs = append(decodeReqs, req)
			unackedAdds = append(unackedAdds, msg)
		}
	}

	// If the fwdPkg has already been processed, it means we are
	// reforwarding the packets again, which happens only on a restart.
	reforward := fwdPkg.State == channeldb.FwdStateProcessed

	// Atomically decode the incoming htlcs, simultaneously checking for
	// replay attempts. A particular index in the returned, spare list of
	// channel iterators should only be used if the failure code at the
	// same index is lnwire.FailCodeNone.
	decodeResps, sphinxErr := l.cfg.DecodeHopIterators(
		fwdPkg.ID(), decodeReqs, reforward,
	)
	if sphinxErr != nil {
		l.failf(LinkFailureError{code: ErrInternalError},
			"unable to decode hop iterators: %v", sphinxErr)
		return
	}

	var switchPackets []*htlcPacket

	for i, update := range unackedAdds {
		idx := uint16(i)
		sourceRef := fwdPkg.SourceRef(idx)
		add := *update

		// An incoming HTLC add has been full-locked in. As a result we
		// can now examine the forwarding details of the HTLC, and the
		// HTLC itself to decide if: we should forward it, cancel it,
		// or are able to settle it (and it adheres to our fee related
		// constraints).

		// Before adding the new htlc to the state machine, parse the
		// onion object in order to obtain the routing information with
		// DecodeHopIterator function which process the Sphinx packet.
		chanIterator, failureCode := decodeResps[i].Result()
		if failureCode != lnwire.CodeNone {
			// If we're unable to process the onion blob then we
			// should send the malformed htlc error to payment
			// sender.
			l.sendMalformedHTLCError(
				add.ID, failureCode, add.OnionBlob, &sourceRef,
			)

			l.log.Errorf("unable to decode onion hop iterator "+
				"for htlc(id=%v, hash=%x): %v", add.ID,
				add.PaymentHash, failureCode)

			continue
		}

		heightNow := l.cfg.BestHeight()

		pld, routeRole, pldErr := chanIterator.HopPayload()
		if pldErr != nil {
			// If we're unable to process the onion payload, or we
			// received invalid onion payload failure, then we
			// should send an error back to the caller so the HTLC
			// can be canceled.
			var failedType uint64

			// We need to get the underlying error value, so we
			// can't use errors.As as suggested by the linter.
			//nolint:errorlint
			if e, ok := pldErr.(hop.ErrInvalidPayload); ok {
				failedType = uint64(e.Type)
			}

			// If we couldn't parse the payload, make our best
			// effort at creating an error encrypter that knows
			// what blinding type we were, but if we couldn't
			// parse the payload we have no way of knowing whether
			// we were the introduction node or not.
			//
			//nolint:ll
			obfuscator, failCode := chanIterator.ExtractErrorEncrypter(
				l.cfg.ExtractErrorEncrypter,
				// We need our route role here because we
				// couldn't parse or validate the payload.
				routeRole == hop.RouteRoleIntroduction,
			)
			if failCode != lnwire.CodeNone {
				l.log.Errorf("could not extract error "+
					"encrypter: %v", pldErr)

				// We can't process this htlc, send back
				// malformed.
				l.sendMalformedHTLCError(
					add.ID, failureCode, add.OnionBlob,
					&sourceRef,
				)

				continue
			}

			// TODO: currently none of the test unit infrastructure
			// is setup to handle TLV payloads, so testing this
			// would require implementing a separate mock iterator
			// for TLV payloads that also supports injecting invalid
			// payloads. Deferring this non-trival effort till a
			// later date
			failure := lnwire.NewInvalidOnionPayload(failedType, 0)

			l.sendHTLCError(
				add, sourceRef, NewLinkError(failure),
				obfuscator, false,
			)

			l.log.Errorf("unable to decode forwarding "+
				"instructions: %v", pldErr)

			continue
		}

		// Retrieve onion obfuscator from onion blob in order to
		// produce initial obfuscation of the onion failureCode.
		obfuscator, failureCode := chanIterator.ExtractErrorEncrypter(
			l.cfg.ExtractErrorEncrypter,
			routeRole == hop.RouteRoleIntroduction,
		)
		if failureCode != lnwire.CodeNone {
			// If we're unable to process the onion blob than we
			// should send the malformed htlc error to payment
			// sender.
			l.sendMalformedHTLCError(
				add.ID, failureCode, add.OnionBlob,
				&sourceRef,
			)

			l.log.Errorf("unable to decode onion "+
				"obfuscator: %v", failureCode)

			continue
		}

		fwdInfo := pld.ForwardingInfo()

		// Check whether the payload we've just processed uses our
		// node as the introduction point (gave us a blinding key in
		// the payload itself) and fail it back if we don't support
		// route blinding.
		if fwdInfo.NextBlinding.IsSome() &&
			l.cfg.DisallowRouteBlinding {

			failure := lnwire.NewInvalidBlinding(
				fn.Some(add.OnionBlob),
			)

			l.sendHTLCError(
				add, sourceRef, NewLinkError(failure),
				obfuscator, false,
			)

			l.log.Error("rejected htlc that uses use as an " +
				"introduction point when we do not support " +
				"route blinding")

			continue
		}

		switch fwdInfo.NextHop {
		case hop.Exit:
			err := l.processExitHop(
				add, sourceRef, obfuscator, fwdInfo,
				heightNow, pld,
			)
			if err != nil {
				l.failf(LinkFailureError{
					code: ErrInternalError,
				}, "%v", err)

				return
			}

		// There are additional channels left within this route. So
		// we'll simply do some forwarding package book-keeping.
		default:
			// If hodl.AddIncoming is requested, we will not
			// validate the forwarded ADD, nor will we send the
			// packet to the htlc switch.
			if l.cfg.HodlMask.Active(hodl.AddIncoming) {
				l.log.Warnf(hodl.AddIncoming.Warning())
				continue
			}

			endorseValue := l.experimentalEndorsement(
				record.CustomSet(add.CustomRecords),
			)
			endorseType := uint64(
				lnwire.ExperimentalEndorsementType,
			)

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
				outgoingAdd := &lnwire.UpdateAddHTLC{
					Expiry:        fwdInfo.OutgoingCTLV,
					Amount:        fwdInfo.AmountToForward,
					PaymentHash:   add.PaymentHash,
					BlindingPoint: fwdInfo.NextBlinding,
				}

				endorseValue.WhenSome(func(e byte) {
					custRecords := map[uint64][]byte{
						endorseType: {e},
					}

					outgoingAdd.CustomRecords = custRecords
				})

				// Finally, we'll encode the onion packet for
				// the _next_ hop using the hop iterator
				// decoded for the current hop.
				buf := bytes.NewBuffer(
					outgoingAdd.OnionBlob[0:0],
				)

				// We know this cannot fail, as this ADD
				// was marked forwarded in a previous
				// round of processing.
				chanIterator.EncodeNextHop(buf)

				inboundFee := l.cfg.FwrdingPolicy.InboundFee

				//nolint:ll
				updatePacket := &htlcPacket{
					incomingChanID:       l.ShortChanID(),
					incomingHTLCID:       add.ID,
					outgoingChanID:       fwdInfo.NextHop,
					sourceRef:            &sourceRef,
					incomingAmount:       add.Amount,
					amount:               outgoingAdd.Amount,
					htlc:                 outgoingAdd,
					obfuscator:           obfuscator,
					incomingTimeout:      add.Expiry,
					outgoingTimeout:      fwdInfo.OutgoingCTLV,
					inOnionCustomRecords: pld.CustomRecords(),
					inboundFee:           inboundFee,
					inWireCustomRecords:  add.CustomRecords.Copy(),
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
				Expiry:        fwdInfo.OutgoingCTLV,
				Amount:        fwdInfo.AmountToForward,
				PaymentHash:   add.PaymentHash,
				BlindingPoint: fwdInfo.NextBlinding,
			}

			endorseValue.WhenSome(func(e byte) {
				addMsg.CustomRecords = map[uint64][]byte{
					endorseType: {e},
				}
			})

			// Finally, we'll encode the onion packet for the
			// _next_ hop using the hop iterator decoded for the
			// current hop.
			buf := bytes.NewBuffer(addMsg.OnionBlob[0:0])
			err := chanIterator.EncodeNextHop(buf)
			if err != nil {
				l.log.Errorf("unable to encode the "+
					"remaining route %v", err)

				cb := func(upd *lnwire.ChannelUpdate1) lnwire.FailureMessage { //nolint:ll
					return lnwire.NewTemporaryChannelFailure(upd)
				}

				failure := l.createFailureWithUpdate(
					true, hop.Source, cb,
				)

				l.sendHTLCError(
					add, sourceRef, NewLinkError(failure),
					obfuscator, false,
				)
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
				inboundFee := l.cfg.FwrdingPolicy.InboundFee

				//nolint:ll
				updatePacket := &htlcPacket{
					incomingChanID:       l.ShortChanID(),
					incomingHTLCID:       add.ID,
					outgoingChanID:       fwdInfo.NextHop,
					sourceRef:            &sourceRef,
					incomingAmount:       add.Amount,
					amount:               addMsg.Amount,
					htlc:                 addMsg,
					obfuscator:           obfuscator,
					incomingTimeout:      add.Expiry,
					outgoingTimeout:      fwdInfo.OutgoingCTLV,
					inOnionCustomRecords: pld.CustomRecords(),
					inboundFee:           inboundFee,
					inWireCustomRecords:  add.CustomRecords.Copy(),
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
			l.failf(LinkFailureError{code: ErrInternalError},
				"unable to set fwd filter: %v", err)
			return
		}
	}

	if len(switchPackets) == 0 {
		return
	}

	l.log.Debugf("forwarding %d packets to switch: reforward=%v",
		len(switchPackets), reforward)

	// NOTE: This call is made synchronous so that we ensure all circuits
	// are committed in the exact order that they are processed in the link.
	// Failing to do this could cause reorderings/gaps in the range of
	// opened circuits, which violates assumptions made by the circuit
	// trimming.
	l.forwardBatch(reforward, switchPackets...)
}

// experimentalEndorsement returns the value to set for our outgoing
// experimental endorsement field, and a boolean indicating whether it should
// be populated on the outgoing htlc.
func (l *channelLink) experimentalEndorsement(
	customUpdateAdd record.CustomSet) fn.Option[byte] {

	// Only relay experimental signal if we are within the experiment
	// period.
	if !l.cfg.ShouldFwdExpEndorsement() {
		return fn.None[byte]()
	}

	// If we don't have any custom records or the experimental field is
	// not set, just forward a zero value.
	if len(customUpdateAdd) == 0 {
		return fn.Some[byte](lnwire.ExperimentalUnendorsed)
	}

	t := uint64(lnwire.ExperimentalEndorsementType)
	value, set := customUpdateAdd[t]
	if !set {
		return fn.Some[byte](lnwire.ExperimentalUnendorsed)
	}

	// We expect at least one byte for this field, consider it invalid if
	// it has no data and just forward a zero value.
	if len(value) == 0 {
		return fn.Some[byte](lnwire.ExperimentalUnendorsed)
	}

	// Only forward endorsed if the incoming link is endorsed.
	if value[0] == lnwire.ExperimentalEndorsed {
		return fn.Some[byte](lnwire.ExperimentalEndorsed)
	}

	// Forward as unendorsed otherwise, including cases where we've
	// received an invalid value that uses more than 3 bits of information.
	return fn.Some[byte](lnwire.ExperimentalUnendorsed)
}

// processExitHop handles an htlc for which this link is the exit hop. It
// returns a boolean indicating whether the commitment tx needs an update.
func (l *channelLink) processExitHop(add lnwire.UpdateAddHTLC,
	sourceRef channeldb.AddRef, obfuscator hop.ErrorEncrypter,
	fwdInfo hop.ForwardingInfo, heightNow uint32,
	payload invoices.Payload) error {

	// If hodl.ExitSettle is requested, we will not validate the final hop's
	// ADD, nor will we settle the corresponding invoice or respond with the
	// preimage.
	if l.cfg.HodlMask.Active(hodl.ExitSettle) {
		l.log.Warnf("%s for htlc(rhash=%x,htlcIndex=%v)",
			hodl.ExitSettle.Warning(), add.PaymentHash, add.ID)

		return nil
	}

	// In case the traffic shaper is active, we'll check if the HTLC has
	// custom records and skip the amount check in the onion payload below.
	isCustomHTLC := fn.MapOptionZ(
		l.cfg.AuxTrafficShaper,
		func(ts AuxTrafficShaper) bool {
			return ts.IsCustomHTLC(add.CustomRecords)
		},
	)

	// As we're the exit hop, we'll double check the hop-payload included in
	// the HTLC to ensure that it was crafted correctly by the sender and
	// is compatible with the HTLC we were extended. If an external
	// validator is active we might bypass the amount check.
	if !isCustomHTLC && add.Amount < fwdInfo.AmountToForward {
		l.log.Errorf("onion payload of incoming htlc(%x) has "+
			"incompatible value: expected <=%v, got %v",
			add.PaymentHash, add.Amount, fwdInfo.AmountToForward)

		failure := NewLinkError(
			lnwire.NewFinalIncorrectHtlcAmount(add.Amount),
		)
		l.sendHTLCError(add, sourceRef, failure, obfuscator, true)

		return nil
	}

	// We'll also ensure that our time-lock value has been computed
	// correctly.
	if add.Expiry < fwdInfo.OutgoingCTLV {
		l.log.Errorf("onion payload of incoming htlc(%x) has "+
			"incompatible time-lock: expected <=%v, got %v",
			add.PaymentHash, add.Expiry, fwdInfo.OutgoingCTLV)

		failure := NewLinkError(
			lnwire.NewFinalIncorrectCltvExpiry(add.Expiry),
		)

		l.sendHTLCError(add, sourceRef, failure, obfuscator, true)

		return nil
	}

	// Notify the invoiceRegistry of the exit hop htlc. If we crash right
	// after this, this code will be re-executed after restart. We will
	// receive back a resolution event.
	invoiceHash := lntypes.Hash(add.PaymentHash)

	circuitKey := models.CircuitKey{
		ChanID: l.ShortChanID(),
		HtlcID: add.ID,
	}

	event, err := l.cfg.Registry.NotifyExitHopHtlc(
		invoiceHash, add.Amount, add.Expiry, int32(heightNow),
		circuitKey, l.hodlQueue.ChanIn(), add.CustomRecords, payload,
	)
	if err != nil {
		return err
	}

	// Create a hodlHtlc struct and decide either resolved now or later.
	htlc := hodlHtlc{
		add:        add,
		sourceRef:  sourceRef,
		obfuscator: obfuscator,
	}

	// If the event is nil, the invoice is being held, so we save payment
	// descriptor for future reference.
	if event == nil {
		l.hodlMap[circuitKey] = htlc
		return nil
	}

	// Process the received resolution.
	return l.processHtlcResolution(event, htlc)
}

// settleHTLC settles the HTLC on the channel.
func (l *channelLink) settleHTLC(preimage lntypes.Preimage,
	htlcIndex uint64, sourceRef channeldb.AddRef) error {

	hash := preimage.Hash()

	l.log.Infof("settling htlc %v as exit hop", hash)

	err := l.channel.SettleHTLC(
		preimage, htlcIndex, &sourceRef, nil, nil,
	)
	if err != nil {
		return fmt.Errorf("unable to settle htlc: %w", err)
	}

	// If the link is in hodl.BogusSettle mode, replace the preimage with a
	// fake one before sending it to the peer.
	if l.cfg.HodlMask.Active(hodl.BogusSettle) {
		l.log.Warnf(hodl.BogusSettle.Warning())
		preimage = [32]byte{}
		copy(preimage[:], bytes.Repeat([]byte{2}, 32))
	}

	// HTLC was successfully settled locally send notification about it
	// remote peer.
	err = l.cfg.Peer.SendMessage(false, &lnwire.UpdateFulfillHTLC{
		ChanID:          l.ChanID(),
		ID:              htlcIndex,
		PaymentPreimage: preimage,
	})
	if err != nil {
		l.log.Errorf("failed to send UpdateFulfillHTLC: %v", err)
	}

	// Once we have successfully settled the htlc, notify a settle event.
	l.cfg.HtlcNotifier.NotifySettleEvent(
		HtlcKey{
			IncomingCircuit: models.CircuitKey{
				ChanID: l.ShortChanID(),
				HtlcID: htlcIndex,
			},
		},
		preimage,
		HtlcEventTypeReceive,
	)

	return nil
}

// forwardBatch forwards the given htlcPackets to the switch, and waits on the
// err chan for the individual responses. This method is intended to be spawned
// as a goroutine so the responses can be handled in the background.
func (l *channelLink) forwardBatch(replay bool, packets ...*htlcPacket) {
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

	err := l.cfg.ForwardPackets(l.cg.Done(), replay, filteredPkts...)
	if err != nil {
		log.Errorf("Unhandled error while reforwarding htlc "+
			"settle/fail over htlcswitch: %v", err)
	}
}

// sendHTLCError functions cancels HTLC and send cancel message back to the
// peer from which HTLC was received.
func (l *channelLink) sendHTLCError(add lnwire.UpdateAddHTLC,
	sourceRef channeldb.AddRef, failure *LinkError,
	e hop.ErrorEncrypter, isReceive bool) {

	reason, err := e.EncryptFirstHop(failure.WireMessage())
	if err != nil {
		l.log.Errorf("unable to obfuscate error: %v", err)
		return
	}

	err = l.channel.FailHTLC(add.ID, reason, &sourceRef, nil, nil)
	if err != nil {
		l.log.Errorf("unable cancel htlc: %v", err)
		return
	}

	// Send the appropriate failure message depending on whether we're
	// in a blinded route or not.
	if err := l.sendIncomingHTLCFailureMsg(
		add.ID, e, reason,
	); err != nil {
		l.log.Errorf("unable to send HTLC failure: %v", err)
		return
	}

	// Notify a link failure on our incoming link. Outgoing htlc information
	// is not available at this point, because we have not decrypted the
	// onion, so it is excluded.
	var eventType HtlcEventType
	if isReceive {
		eventType = HtlcEventTypeReceive
	} else {
		eventType = HtlcEventTypeForward
	}

	l.cfg.HtlcNotifier.NotifyLinkFailEvent(
		HtlcKey{
			IncomingCircuit: models.CircuitKey{
				ChanID: l.ShortChanID(),
				HtlcID: add.ID,
			},
		},
		HtlcInfo{
			IncomingTimeLock: add.Expiry,
			IncomingAmt:      add.Amount,
		},
		eventType,
		failure,
		true,
	)
}

// sendPeerHTLCFailure handles sending a HTLC failure message back to the
// peer from which the HTLC was received. This function is primarily used to
// handle the special requirements of route blinding, specifically:
// - Forwarding nodes must switch out any errors with MalformedFailHTLC
// - Introduction nodes should return regular HTLC failure messages.
//
// It accepts the original opaque failure, which will be used in the case
// that we're not part of a blinded route and an error encrypter that'll be
// used if we are the introduction node and need to present an error as if
// we're the failing party.
func (l *channelLink) sendIncomingHTLCFailureMsg(htlcIndex uint64,
	e hop.ErrorEncrypter,
	originalFailure lnwire.OpaqueReason) error {

	var msg lnwire.Message
	switch {
	// Our circuit's error encrypter will be nil if this was a locally
	// initiated payment. We can only hit a blinded error for a locally
	// initiated payment if we allow ourselves to be picked as the
	// introduction node for our own payments and in that case we
	// shouldn't reach this code. To prevent the HTLC getting stuck,
	// we fail it back and log an error.
	// code.
	case e == nil:
		msg = &lnwire.UpdateFailHTLC{
			ChanID: l.ChanID(),
			ID:     htlcIndex,
			Reason: originalFailure,
		}

		l.log.Errorf("Unexpected blinded failure when "+
			"we are the sending node, incoming htlc: %v(%v)",
			l.ShortChanID(), htlcIndex)

	// For cleartext hops (ie, non-blinded/normal) we don't need any
	// transformation on the error message and can just send the original.
	case !e.Type().IsBlinded():
		msg = &lnwire.UpdateFailHTLC{
			ChanID: l.ChanID(),
			ID:     htlcIndex,
			Reason: originalFailure,
		}

	// When we're the introduction node, we need to convert the error to
	// a UpdateFailHTLC.
	case e.Type() == hop.EncrypterTypeIntroduction:
		l.log.Debugf("Introduction blinded node switching out failure "+
			"error: %v", htlcIndex)

		// The specification does not require that we set the onion
		// blob.
		failureMsg := lnwire.NewInvalidBlinding(
			fn.None[[lnwire.OnionPacketSize]byte](),
		)
		reason, err := e.EncryptFirstHop(failureMsg)
		if err != nil {
			return err
		}

		msg = &lnwire.UpdateFailHTLC{
			ChanID: l.ChanID(),
			ID:     htlcIndex,
			Reason: reason,
		}

	// If we are a relaying node, we need to switch out any error that
	// we've received to a malformed HTLC error.
	case e.Type() == hop.EncrypterTypeRelaying:
		l.log.Debugf("Relaying blinded node switching out malformed "+
			"error: %v", htlcIndex)

		msg = &lnwire.UpdateFailMalformedHTLC{
			ChanID:      l.ChanID(),
			ID:          htlcIndex,
			FailureCode: lnwire.CodeInvalidBlinding,
		}

	default:
		return fmt.Errorf("unexpected encrypter: %d", e)
	}

	if err := l.cfg.Peer.SendMessage(false, msg); err != nil {
		l.log.Warnf("Send update fail failed: %v", err)
	}

	return nil
}

// sendMalformedHTLCError helper function which sends the malformed HTLC update
// to the payment sender.
func (l *channelLink) sendMalformedHTLCError(htlcIndex uint64,
	code lnwire.FailCode, onionBlob [lnwire.OnionPacketSize]byte,
	sourceRef *channeldb.AddRef) {

	shaOnionBlob := sha256.Sum256(onionBlob[:])
	err := l.channel.MalformedFailHTLC(htlcIndex, code, shaOnionBlob, sourceRef)
	if err != nil {
		l.log.Errorf("unable cancel htlc: %v", err)
		return
	}

	err = l.cfg.Peer.SendMessage(false, &lnwire.UpdateFailMalformedHTLC{
		ChanID:       l.ChanID(),
		ID:           htlcIndex,
		ShaOnionBlob: shaOnionBlob,
		FailureCode:  code,
	})
	if err != nil {
		l.log.Errorf("failed to send UpdateFailMalformedHTLC: %v", err)
	}
}

// failf is a function which is used to encapsulate the action necessary for
// properly failing the link. It takes a LinkFailureError, which will be passed
// to the OnChannelFailure closure, in order for it to determine if we should
// force close the channel, and if we should send an error message to the
// remote peer.
func (l *channelLink) failf(linkErr LinkFailureError, format string,
	a ...interface{}) {

	reason := fmt.Errorf(format, a...)

	// Return if we have already notified about a failure.
	if l.failed {
		l.log.Warnf("ignoring link failure (%v), as link already "+
			"failed", reason)
		return
	}

	l.log.Errorf("failing link: %s with error: %v", reason, linkErr)

	// Set failed, such that we won't process any more updates, and notify
	// the peer about the failure.
	l.failed = true
	l.cfg.OnChannelFailure(l.ChanID(), l.ShortChanID(), linkErr)
}

// FundingCustomBlob returns the custom funding blob of the channel that this
// link is associated with. The funding blob represents static information about
// the channel that was created at channel funding time.
func (l *channelLink) FundingCustomBlob() fn.Option[tlv.Blob] {
	if l.channel == nil {
		return fn.None[tlv.Blob]()
	}

	if l.channel.State() == nil {
		return fn.None[tlv.Blob]()
	}

	return l.channel.State().CustomBlob
}

// CommitmentCustomBlob returns the custom blob of the current local commitment
// of the channel that this link is associated with.
func (l *channelLink) CommitmentCustomBlob() fn.Option[tlv.Blob] {
	if l.channel == nil {
		return fn.None[tlv.Blob]()
	}

	return l.channel.LocalCommitmentBlob()
}

// handleHtlcResolution takes an HTLC resolution and processes it by draining
// the hodlQueue. Once processed, a commit_sig is sent to the remote to update
// their commitment.
func (l *channelLink) handleHtlcResolution(ctx context.Context,
	hodlItem any) error {

	htlcResolution, ok := hodlItem.(invoices.HtlcResolution)
	if !ok {
		return fmt.Errorf("expect HtlcResolution, got %T", hodlItem)
	}

	err := l.processHodlQueue(ctx, htlcResolution)
	// No error, success.
	if err == nil {
		return nil
	}

	switch {
	// If the duplicate keystone error was encountered, fail back
	// gracefully.
	case errors.Is(err, ErrDuplicateKeystone):
		l.failf(
			LinkFailureError{
				code: ErrCircuitError,
			},
			"process hodl queue: temporary circuit error: %v", err,
		)

	// Send an Error message to the peer.
	default:
		l.failf(
			LinkFailureError{
				code: ErrInternalError,
			},
			"process hodl queue: unable to update commitment: %v",
			err,
		)
	}

	return err
}

// handleQuiescenceReq takes a locally initialized (RPC) quiescence request and
// forwards it to the quiescer for further processing.
func (l *channelLink) handleQuiescenceReq(req StfuReq) error {
	l.quiescer.InitStfu(req)

	if !l.noDanglingUpdates(lntypes.Local) {
		return nil
	}

	err := l.quiescer.SendOwedStfu()
	if err != nil {
		l.stfuFailf("SendOwedStfu: %v", err)
		res := fn.Err[lntypes.ChannelParty](err)
		req.Resolve(res)
	}

	return err
}

// handleUpdateFee is called whenever the `updateFeeTimer` ticks. It is used to
// decide whether we should send an `update_fee` msg to update the commitment's
// feerate.
func (l *channelLink) handleUpdateFee(ctx context.Context) error {
	// If we're not the initiator of the channel, we don't control the fees,
	// so we can ignore this.
	if !l.channel.IsInitiator() {
		return nil
	}

	// If we are the initiator, then we'll sample the current fee rate to
	// get into the chain within 3 blocks.
	netFee, err := l.sampleNetworkFee()
	if err != nil {
		return fmt.Errorf("unable to sample network fee: %w", err)
	}

	minRelayFee := l.cfg.FeeEstimator.RelayFeePerKW()

	newCommitFee := l.channel.IdealCommitFeeRate(
		netFee, minRelayFee,
		l.cfg.MaxAnchorsCommitFeeRate,
		l.cfg.MaxFeeAllocation,
	)

	// We determine if we should adjust the commitment fee based on the
	// current commitment fee, the suggested new commitment fee and the
	// current minimum relay fee rate.
	commitFee := l.channel.CommitFeeRate()
	if !shouldAdjustCommitFee(newCommitFee, commitFee, minRelayFee) {
		return nil
	}

	// If we do, then we'll send a new UpdateFee message to the remote
	// party, to be locked in with a new update.
	err = l.updateChannelFee(ctx, newCommitFee)
	if err != nil {
		return fmt.Errorf("unable to update fee rate: %w", err)
	}

	return nil
}

// toggleBatchTicker checks whether we need to resume or pause the batch ticker.
// When we have no pending updates, the ticker is paused, otherwise resumed.
func (l *channelLink) toggleBatchTicker() {
	// If the previous event resulted in a non-empty batch, resume the batch
	// ticker so that it can be cleared. Otherwise pause the ticker to
	// prevent waking up the htlcManager while the batch is empty.
	numUpdates := l.channel.NumPendingUpdates(lntypes.Local, lntypes.Remote)
	if numUpdates > 0 {
		l.cfg.BatchTicker.Resume()
		l.log.Tracef("BatchTicker resumed, NumPendingUpdates(Local, "+
			"Remote)=%d", numUpdates)

		return
	}

	l.cfg.BatchTicker.Pause()
	l.log.Trace("BatchTicker paused due to zero NumPendingUpdates" +
		"(Local, Remote)")
}

// resumeLink is called when starting a previous link. It will go through the
// reestablishment protocol and reforwarding packets that are yet resolved.
func (l *channelLink) resumeLink(ctx context.Context) error {
	// If this isn't the first time that this channel link has been created,
	// then we'll need to check to see if we need to re-synchronize state
	// with the remote peer. settledHtlcs is a map of HTLC's that we
	// re-settled as part of the channel state sync.
	if l.cfg.SyncStates {
		err := l.syncChanStates(ctx)
		if err != nil {
			l.handleChanSyncErr(err)

			return err
		}
	}

	// If a shutdown message has previously been sent on this link, then we
	// need to make sure that we have disabled any HTLC adds on the outgoing
	// direction of the link and that we re-resend the same shutdown message
	// that we previously sent.
	//
	// TODO(yy): we should either move this to chanCloser, or move all
	// shutdown handling logic to be managed by the link, but not a mixed of
	// partial management by two subsystems.
	l.cfg.PreviouslySentShutdown.WhenSome(func(shutdown lnwire.Shutdown) {
		// Immediately disallow any new outgoing HTLCs.
		if !l.DisableAdds(Outgoing) {
			l.log.Warnf("Outgoing link adds already disabled")
		}

		// Re-send the shutdown message the peer. Since syncChanStates
		// would have sent any outstanding CommitSig, it is fine for us
		// to immediately queue the shutdown message now.
		err := l.cfg.Peer.SendMessage(false, &shutdown)
		if err != nil {
			l.log.Warnf("Error sending shutdown message: %v", err)
		}
	})

	// We've successfully reestablished the channel, mark it as such to
	// allow the switch to forward HTLCs in the outbound direction.
	l.markReestablished()

	// With the channel states synced, we now reset the mailbox to ensure we
	// start processing all unacked packets in order. This is done here to
	// ensure that all acknowledgments that occur during channel
	// resynchronization have taken affect, causing us only to pull unacked
	// packets after starting to read from the downstream mailbox.
	err := l.mailBox.ResetPackets()
	if err != nil {
		l.log.Errorf("failed to reset packets: %v", err)
	}

	// If the channel is pending, there's no need to reforwarding packets.
	if l.ShortChanID() == hop.Source {
		return nil
	}

	// After cleaning up any memory pertaining to incoming packets, we now
	// replay our forwarding packages to handle any htlcs that can be
	// processed locally, or need to be forwarded out to the switch. We will
	// only attempt to resolve packages if our short chan id indicates that
	// the channel is not pending, otherwise we should have no htlcs to
	// reforward.
	err = l.resolveFwdPkgs(ctx)
	switch {
	// No error was encountered, success.
	case err == nil:
		// With our link's in-memory state fully reconstructed, spawn a
		// goroutine to manage the reclamation of disk space occupied by
		// completed forwarding packages.
		l.cg.WgAdd(1)
		go l.fwdPkgGarbager()

		return nil

	// If the duplicate keystone error was encountered, we'll fail without
	// sending an Error message to the peer.
	case errors.Is(err, ErrDuplicateKeystone):
		l.failf(LinkFailureError{code: ErrCircuitError},
			"temporary circuit error: %v", err)

	// A non-nil error was encountered, send an Error message to
	// the peer.
	default:
		l.failf(LinkFailureError{code: ErrInternalError},
			"unable to resolve fwd pkgs: %v", err)
	}

	return err
}

// processRemoteUpdateAddHTLC takes an `UpdateAddHTLC` msg sent from the remote
// and processes it.
func (l *channelLink) processRemoteUpdateAddHTLC(
	msg *lnwire.UpdateAddHTLC) error {

	if l.IsFlushing(Incoming) {
		// This is forbidden by the protocol specification. The best
		// chance we have to deal with this is to drop the connection.
		// This should roll back the channel state to the last
		// CommitSig. If the remote has already sent a CommitSig we
		// haven't received yet, channel state will be re-synchronized
		// with a ChannelReestablish message upon reconnection and the
		// protocol state that caused us to flush the link will be
		// rolled back. In the event that there was some
		// non-deterministic behavior in the remote that caused them to
		// violate the protocol, we have a decent shot at correcting it
		// this way, since reconnecting will put us in the cleanest
		// possible state to try again.
		//
		// In addition to the above, it is possible for us to hit this
		// case in situations where we improperly handle message
		// ordering due to concurrency choices. An issue has been filed
		// to address this here:
		// https://github.com/lightningnetwork/lnd/issues/8393
		err := errors.New("received add while link is flushing")
		l.failf(
			LinkFailureError{
				code:             ErrInvalidUpdate,
				FailureAction:    LinkFailureDisconnect,
				PermanentFailure: false,
				Warning:          true,
			}, "%v", err,
		)

		return err
	}

	// Disallow htlcs with blinding points set if we haven't enabled the
	// feature. This saves us from having to process the onion at all, but
	// will only catch blinded payments where we are a relaying node (as the
	// blinding point will be in the payload when we're the introduction
	// node).
	if msg.BlindingPoint.IsSome() && l.cfg.DisallowRouteBlinding {
		err := errors.New("blinding point included when route " +
			"blinding is disabled")

		l.failf(LinkFailureError{code: ErrInvalidUpdate}, "%v", err)

		return err
	}

	// We have to check the limit here rather than later in the switch
	// because the counterparty can keep sending HTLC's without sending a
	// revoke. This would mean that the switch check would only occur later.
	if l.isOverexposedWithHtlc(msg, true) {
		err := errors.New("peer sent us an HTLC that exceeded our " +
			"max fee exposure")
		l.failf(LinkFailureError{code: ErrInternalError}, "%v", err)

		return err
	}

	// We just received an add request from an upstream peer, so we add it
	// to our state machine, then add the HTLC to our "settle" list in the
	// event that we know the preimage.
	index, err := l.channel.ReceiveHTLC(msg)
	if err != nil {
		l.failf(LinkFailureError{code: ErrInvalidUpdate},
			"unable to handle upstream add HTLC: %v", err)

		return err
	}

	l.log.Tracef("receive upstream htlc with payment hash(%x), "+
		"assigning index: %v", msg.PaymentHash[:], index)

	return nil
}

// processRemoteUpdateFulfillHTLC takes an `UpdateFulfillHTLC` msg sent from the
// remote and processes it.
func (l *channelLink) processRemoteUpdateFulfillHTLC(
	msg *lnwire.UpdateFulfillHTLC) error {

	pre := msg.PaymentPreimage
	idx := msg.ID

	// Before we pipeline the settle, we'll check the set of active htlc's
	// to see if the related UpdateAddHTLC has been fully locked-in.
	var lockedin bool
	htlcs := l.channel.ActiveHtlcs()
	for _, add := range htlcs {
		// The HTLC will be outgoing and match idx.
		if !add.Incoming && add.HtlcIndex == idx {
			lockedin = true
			break
		}
	}

	if !lockedin {
		err := errors.New("unable to handle upstream settle")
		l.failf(LinkFailureError{code: ErrInvalidUpdate}, "%v", err)

		return err
	}

	if err := l.channel.ReceiveHTLCSettle(pre, idx); err != nil {
		l.failf(
			LinkFailureError{
				code:          ErrInvalidUpdate,
				FailureAction: LinkFailureForceClose,
			},
			"unable to handle upstream settle HTLC: %v", err,
		)

		return err
	}

	settlePacket := &htlcPacket{
		outgoingChanID: l.ShortChanID(),
		outgoingHTLCID: idx,
		htlc: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: pre,
		},
	}

	// Add the newly discovered preimage to our growing list of uncommitted
	// preimage. These will be written to the witness cache just before
	// accepting the next commitment signature from the remote peer.
	l.uncommittedPreimages = append(l.uncommittedPreimages, pre)

	// Pipeline this settle, send it to the switch.
	go l.forwardBatch(false, settlePacket)

	return nil
}

// processRemoteUpdateFailMalformedHTLC takes an `UpdateFailMalformedHTLC` msg
// sent from the remote and processes it.
func (l *channelLink) processRemoteUpdateFailMalformedHTLC(
	msg *lnwire.UpdateFailMalformedHTLC) error {

	// Convert the failure type encoded within the HTLC fail message to the
	// proper generic lnwire error code.
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

	// Handle malformed errors that are part of a blinded route. This case
	// is slightly different, because we expect every relaying node in the
	// blinded portion of the route to send malformed errors. If we're also
	// a relaying node, we're likely going to switch this error out anyway
	// for our own malformed error, but we handle the case here for
	// completeness.
	case lnwire.CodeInvalidBlinding:
		failure = &lnwire.FailInvalidBlinding{
			OnionSHA256: msg.ShaOnionBlob,
		}

	default:
		l.log.Warnf("unexpected failure code received in "+
			"UpdateFailMailformedHTLC: %v", msg.FailureCode)

		// We don't just pass back the error we received from our
		// successor. Otherwise we might report a failure that penalizes
		// us more than needed. If the onion that we forwarded was
		// correct, the node should have been able to send back its own
		// failure. The node did not send back its own failure, so we
		// assume there was a problem with the onion and report that
		// back. We reuse the invalid onion key failure because there is
		// no specific error for this case.
		failure = &lnwire.FailInvalidOnionKey{
			OnionSHA256: msg.ShaOnionBlob,
		}
	}

	// With the error parsed, we'll convert the into it's opaque form.
	var b bytes.Buffer
	if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
		return fmt.Errorf("unable to encode malformed error: %w", err)
	}

	// If remote side have been unable to parse the onion blob we have sent
	// to it, than we should transform the malformed HTLC message to the
	// usual HTLC fail message.
	err := l.channel.ReceiveFailHTLC(msg.ID, b.Bytes())
	if err != nil {
		l.failf(LinkFailureError{code: ErrInvalidUpdate},
			"unable to handle upstream fail HTLC: %v", err)

		return err
	}

	return nil
}

// processRemoteUpdateFailHTLC takes an `UpdateFailHTLC` msg sent from the
// remote and processes it.
func (l *channelLink) processRemoteUpdateFailHTLC(
	msg *lnwire.UpdateFailHTLC) error {

	// Verify that the failure reason is at least 256 bytes plus overhead.
	const minimumFailReasonLength = lnwire.FailureMessageLength + 2 + 2 + 32

	if len(msg.Reason) < minimumFailReasonLength {
		// We've received a reason with a non-compliant length. Older
		// nodes happily relay back these failures that may originate
		// from a node further downstream. Therefore we can't just fail
		// the channel.
		//
		// We want to be compliant ourselves, so we also can't pass back
		// the reason unmodified. And we must make sure that we don't
		// hit the magic length check of 260 bytes in
		// processRemoteSettleFails either.
		//
		// Because the reason is unreadable for the payer anyway, we
		// just replace it by a compliant-length series of random bytes.
		msg.Reason = make([]byte, minimumFailReasonLength)
		_, err := crand.Read(msg.Reason[:])
		if err != nil {
			return fmt.Errorf("random generation error: %w", err)
		}
	}

	// Add fail to the update log.
	idx := msg.ID
	err := l.channel.ReceiveFailHTLC(idx, msg.Reason[:])
	if err != nil {
		l.failf(LinkFailureError{code: ErrInvalidUpdate},
			"unable to handle upstream fail HTLC: %v", err)

		return err
	}

	return nil
}

// processRemoteCommitSig takes a `CommitSig` msg sent from the remote and
// processes it.
func (l *channelLink) processRemoteCommitSig(ctx context.Context,
	msg *lnwire.CommitSig) error {

	// Since we may have learned new preimages for the first time, we'll add
	// them to our preimage cache. By doing this, we ensure any contested
	// contracts watched by any on-chain arbitrators can now sweep this HTLC
	// on-chain. We delay committing the preimages until just before
	// accepting the new remote commitment, as afterwards the peer won't
	// resend the Settle messages on the next channel reestablishment. Doing
	// so allows us to more effectively batch this operation, instead of
	// doing a single write per preimage.
	err := l.cfg.PreimageCache.AddPreimages(l.uncommittedPreimages...)
	if err != nil {
		l.failf(
			LinkFailureError{code: ErrInternalError},
			"unable to add preimages=%v to cache: %v",
			l.uncommittedPreimages, err,
		)

		return err
	}

	// Instead of truncating the slice to conserve memory allocations, we
	// simply set the uncommitted preimage slice to nil so that a new one
	// will be initialized if any more witnesses are discovered. We do this
	// because the maximum size that the slice can occupy is 15KB, and we
	// want to ensure we release that memory back to the runtime.
	l.uncommittedPreimages = nil

	// We just received a new updates to our local commitment chain,
	// validate this new commitment, closing the link if invalid.
	auxSigBlob, err := msg.CustomRecords.Serialize()
	if err != nil {
		l.failf(
			LinkFailureError{code: ErrInvalidCommitment},
			"unable to serialize custom records: %v", err,
		)

		return err
	}
	err = l.channel.ReceiveNewCommitment(&lnwallet.CommitSigs{
		CommitSig:  msg.CommitSig,
		HtlcSigs:   msg.HtlcSigs,
		PartialSig: msg.PartialSig,
		AuxSigBlob: auxSigBlob,
	})
	if err != nil {
		// If we were unable to reconstruct their proposed commitment,
		// then we'll examine the type of error. If it's an
		// InvalidCommitSigError, then we'll send a direct error.
		var sendData []byte
		switch {
		case lnutils.ErrorAs[*lnwallet.InvalidCommitSigError](err):
			sendData = []byte(err.Error())
		case lnutils.ErrorAs[*lnwallet.InvalidHtlcSigError](err):
			sendData = []byte(err.Error())
		}
		l.failf(
			LinkFailureError{
				code:          ErrInvalidCommitment,
				FailureAction: LinkFailureForceClose,
				SendData:      sendData,
			},
			"ChannelPoint(%v): unable to accept new "+
				"commitment: %v",
			l.channel.ChannelPoint(), err,
		)

		return err
	}

	// As we've just accepted a new state, we'll now immediately send the
	// remote peer a revocation for our prior state.
	nextRevocation, currentHtlcs, finalHTLCs, err :=
		l.channel.RevokeCurrentCommitment()
	if err != nil {
		l.log.Errorf("unable to revoke commitment: %v", err)

		// We need to fail the channel in case revoking our local
		// commitment does not succeed. We might have already advanced
		// our channel state which would lead us to proceed with an
		// unclean state.
		//
		// NOTE: We do not trigger a force close because this could
		// resolve itself in case our db was just busy not accepting new
		// transactions.
		l.failf(
			LinkFailureError{
				code:          ErrInternalError,
				Warning:       true,
				FailureAction: LinkFailureDisconnect,
			},
			"ChannelPoint(%v): unable to accept new "+
				"commitment: %v",
			l.channel.ChannelPoint(), err,
		)

		return err
	}

	// As soon as we are ready to send our next revocation, we can invoke
	// the incoming commit hooks.
	l.Lock()
	l.incomingCommitHooks.invoke()
	l.Unlock()

	err = l.cfg.Peer.SendMessage(false, nextRevocation)
	if err != nil {
		l.log.Errorf("failed to send RevokeAndAck: %v", err)
	}

	// Notify the incoming htlcs of which the resolutions were locked in.
	for id, settled := range finalHTLCs {
		l.cfg.HtlcNotifier.NotifyFinalHtlcEvent(
			models.CircuitKey{
				ChanID: l.ShortChanID(),
				HtlcID: id,
			},
			channeldb.FinalHtlcInfo{
				Settled:  settled,
				Offchain: true,
			},
		)
	}

	// Since we just revoked our commitment, we may have a new set of HTLC's
	// on our commitment, so we'll send them using our function closure
	// NotifyContractUpdate.
	newUpdate := &contractcourt.ContractUpdate{
		HtlcKey: contractcourt.LocalHtlcSet,
		Htlcs:   currentHtlcs,
	}
	err = l.cfg.NotifyContractUpdate(newUpdate)
	if err != nil {
		return fmt.Errorf("unable to notify contract update: %w", err)
	}

	select {
	case <-l.cg.Done():
		return nil
	default:
	}

	// If the remote party initiated the state transition, we'll reply with
	// a signature to provide them with their version of the latest
	// commitment. Otherwise, both commitment chains are fully synced from
	// our PoV, then we don't need to reply with a signature as both sides
	// already have a commitment with the latest accepted.
	if l.channel.OweCommitment() {
		if !l.updateCommitTxOrFail(ctx) {
			return nil
		}
	}

	// If we need to send out an Stfu, this would be the time to do so.
	if l.noDanglingUpdates(lntypes.Local) {
		err = l.quiescer.SendOwedStfu()
		if err != nil {
			l.stfuFailf("sendOwedStfu: %v", err)
		}
	}

	// Now that we have finished processing the incoming CommitSig and sent
	// out our RevokeAndAck, we invoke the flushHooks if the channel state
	// is clean.
	l.Lock()
	if l.channel.IsChannelClean() {
		l.flushHooks.invoke()
	}
	l.Unlock()

	return nil
}

// processRemoteRevokeAndAck takes a `RevokeAndAck` msg sent from the remote and
// processes it.
func (l *channelLink) processRemoteRevokeAndAck(ctx context.Context,
	msg *lnwire.RevokeAndAck) error {

	// We've received a revocation from the remote chain, if valid, this
	// moves the remote chain forward, and expands our revocation window.

	// We now process the message and advance our remote commit chain.
	fwdPkg, remoteHTLCs, err := l.channel.ReceiveRevocation(msg)
	if err != nil {
		// TODO(halseth): force close?
		l.failf(
			LinkFailureError{
				code:          ErrInvalidRevocation,
				FailureAction: LinkFailureDisconnect,
			},
			"unable to accept revocation: %v", err,
		)

		return err
	}

	// The remote party now has a new primary commitment, so we'll update
	// the contract court to be aware of this new set (the prior old remote
	// pending).
	newUpdate := &contractcourt.ContractUpdate{
		HtlcKey: contractcourt.RemoteHtlcSet,
		Htlcs:   remoteHTLCs,
	}
	err = l.cfg.NotifyContractUpdate(newUpdate)
	if err != nil {
		return fmt.Errorf("unable to notify contract update: %w", err)
	}

	select {
	case <-l.cg.Done():
		return nil
	default:
	}

	// If we have a tower client for this channel type, we'll create a
	// backup for the current state.
	if l.cfg.TowerClient != nil {
		state := l.channel.State()
		chanID := l.ChanID()

		err = l.cfg.TowerClient.BackupState(
			&chanID, state.RemoteCommitment.CommitHeight-1,
		)
		if err != nil {
			l.failf(LinkFailureError{
				code: ErrInternalError,
			}, "unable to queue breach backup: %v", err)

			return err
		}
	}

	// If we can send updates then we can process adds in case we are the
	// exit hop and need to send back resolutions, or in case there are
	// validity issues with the packets. Otherwise we defer the action until
	// resume.
	//
	// We are free to process the settles and fails without this check since
	// processing those can't result in further updates to this channel
	// link.
	if l.quiescer.CanSendUpdates() {
		l.processRemoteAdds(fwdPkg)
	} else {
		l.quiescer.OnResume(func() {
			l.processRemoteAdds(fwdPkg)
		})
	}
	l.processRemoteSettleFails(fwdPkg)

	// If the link failed during processing the adds, we must return to
	// ensure we won't attempted to update the state further.
	if l.failed {
		return nil
	}

	// The revocation window opened up. If there are pending local updates,
	// try to update the commit tx. Pending updates could already have been
	// present because of a previously failed update to the commit tx or
	// freshly added in by processRemoteAdds. Also in case there are no
	// local updates, but there are still remote updates that are not in the
	// remote commit tx yet, send out an update.
	if l.channel.OweCommitment() {
		if !l.updateCommitTxOrFail(ctx) {
			return nil
		}
	}

	// Now that we have finished processing the RevokeAndAck, we can invoke
	// the flushHooks if the channel state is clean.
	l.Lock()
	if l.channel.IsChannelClean() {
		l.flushHooks.invoke()
	}
	l.Unlock()

	return nil
}

// processRemoteUpdateFee takes an `UpdateFee` msg sent from the remote and
// processes it.
func (l *channelLink) processRemoteUpdateFee(msg *lnwire.UpdateFee) error {
	// Check and see if their proposed fee-rate would make us exceed the fee
	// threshold.
	fee := chainfee.SatPerKWeight(msg.FeePerKw)

	isDust, err := l.exceedsFeeExposureLimit(fee)
	if err != nil {
		// This shouldn't typically happen. If it does, it indicates
		// something is wrong with our channel state.
		l.log.Errorf("Unable to determine if fee threshold " +
			"exceeded")
		l.failf(LinkFailureError{code: ErrInternalError},
			"error calculating fee exposure: %v", err)

		return err
	}

	if isDust {
		// The proposed fee-rate makes us exceed the fee threshold.
		l.failf(LinkFailureError{code: ErrInternalError},
			"fee threshold exceeded: %v", err)
		return err
	}

	// We received fee update from peer. If we are the initiator we will
	// fail the channel, if not we will apply the update.
	if err := l.channel.ReceiveUpdateFee(fee); err != nil {
		l.failf(LinkFailureError{code: ErrInvalidUpdate},
			"error receiving fee update: %v", err)
		return err
	}

	// Update the mailbox's feerate as well.
	l.mailBox.SetFeeRate(fee)

	return nil
}

// processRemoteError takes an `Error` msg sent from the remote and fails the
// channel link.
func (l *channelLink) processRemoteError(msg *lnwire.Error) {
	// Error received from remote, MUST fail channel, but should only print
	// the contents of the error message if all characters are printable
	// ASCII.
	l.failf(
		// TODO(halseth): we currently don't fail the channel
		// permanently, as there are some sync issues with other
		// implementations that will lead to them sending an
		// error message, but we can recover from on next
		// connection. See
		// https://github.com/ElementsProject/lightning/issues/4212
		LinkFailureError{
			code:             ErrRemoteError,
			PermanentFailure: false,
		},
		"ChannelPoint(%v): received error from peer: %v",
		l.channel.ChannelPoint(), msg.Error(),
	)
}

// processLocalUpdateFulfillHTLC takes an `UpdateFulfillHTLC` from the local and
// processes it.
func (l *channelLink) processLocalUpdateFulfillHTLC(ctx context.Context,
	pkt *htlcPacket, htlc *lnwire.UpdateFulfillHTLC) {

	// If hodl.SettleOutgoing mode is active, we exit early to simulate
	// arbitrary delays between the switch adding the SETTLE to the mailbox,
	// and the HTLC being added to the commitment state.
	if l.cfg.HodlMask.Active(hodl.SettleOutgoing) {
		l.log.Warnf(hodl.SettleOutgoing.Warning())
		l.mailBox.AckPacket(pkt.inKey())

		return
	}

	// An HTLC we forward to the switch has just settled somewhere upstream.
	// Therefore we settle the HTLC within the our local state machine.
	inKey := pkt.inKey()
	err := l.channel.SettleHTLC(
		htlc.PaymentPreimage, pkt.incomingHTLCID, pkt.sourceRef,
		pkt.destRef, &inKey,
	)
	if err != nil {
		l.log.Errorf("unable to settle incoming HTLC for "+
			"circuit-key=%v: %v", inKey, err)

		// If the HTLC index for Settle response was not known to our
		// commitment state, it has already been cleaned up by a prior
		// response. We'll thus try to clean up any lingering state to
		// ensure we don't continue reforwarding.
		if lnutils.ErrorAs[lnwallet.ErrUnknownHtlcIndex](err) {
			l.cleanupSpuriousResponse(pkt)
		}

		// Remove the packet from the link's mailbox to ensure it
		// doesn't get replayed after a reconnection.
		l.mailBox.AckPacket(inKey)

		return
	}

	l.log.Debugf("queueing removal of SETTLE closed circuit: %s->%s",
		pkt.inKey(), pkt.outKey())

	l.closedCircuits = append(l.closedCircuits, pkt.inKey())

	// With the HTLC settled, we'll need to populate the wire message to
	// target the specific channel and HTLC to be canceled.
	htlc.ChanID = l.ChanID()
	htlc.ID = pkt.incomingHTLCID

	// Then we send the HTLC settle message to the connected peer so we can
	// continue the propagation of the settle message.
	err = l.cfg.Peer.SendMessage(false, htlc)
	if err != nil {
		l.log.Errorf("failed to send UpdateFulfillHTLC: %v", err)
	}

	// Send a settle event notification to htlcNotifier.
	l.cfg.HtlcNotifier.NotifySettleEvent(
		newHtlcKey(pkt), htlc.PaymentPreimage, getEventType(pkt),
	)

	// Immediately update the commitment tx to minimize latency.
	l.updateCommitTxOrFail(ctx)
}

// processLocalUpdateFailHTLC takes an `UpdateFailHTLC` from the local and
// processes it.
func (l *channelLink) processLocalUpdateFailHTLC(ctx context.Context,
	pkt *htlcPacket, htlc *lnwire.UpdateFailHTLC) {

	// If hodl.FailOutgoing mode is active, we exit early to simulate
	// arbitrary delays between the switch adding a FAIL to the mailbox, and
	// the HTLC being added to the commitment state.
	if l.cfg.HodlMask.Active(hodl.FailOutgoing) {
		l.log.Warnf(hodl.FailOutgoing.Warning())
		l.mailBox.AckPacket(pkt.inKey())

		return
	}

	// An HTLC cancellation has been triggered somewhere upstream, we'll
	// remove then HTLC from our local state machine.
	inKey := pkt.inKey()
	err := l.channel.FailHTLC(
		pkt.incomingHTLCID, htlc.Reason, pkt.sourceRef, pkt.destRef,
		&inKey,
	)
	if err != nil {
		l.log.Errorf("unable to cancel incoming HTLC for "+
			"circuit-key=%v: %v", inKey, err)

		// If the HTLC index for Fail response was not known to our
		// commitment state, it has already been cleaned up by a prior
		// response. We'll thus try to clean up any lingering state to
		// ensure we don't continue reforwarding.
		if lnutils.ErrorAs[lnwallet.ErrUnknownHtlcIndex](err) {
			l.cleanupSpuriousResponse(pkt)
		}

		// Remove the packet from the link's mailbox to ensure it
		// doesn't get replayed after a reconnection.
		l.mailBox.AckPacket(inKey)

		return
	}

	l.log.Debugf("queueing removal of FAIL closed circuit: %s->%s",
		pkt.inKey(), pkt.outKey())

	l.closedCircuits = append(l.closedCircuits, pkt.inKey())

	// With the HTLC removed, we'll need to populate the wire message to
	// target the specific channel and HTLC to be canceled. The "Reason"
	// field will have already been set within the switch.
	htlc.ChanID = l.ChanID()
	htlc.ID = pkt.incomingHTLCID

	// We send the HTLC message to the peer which initially created the
	// HTLC. If the incoming blinding point is non-nil, we know that we are
	// a relaying node in a blinded path. Otherwise, we're either an
	// introduction node or not part of a blinded path at all.
	err = l.sendIncomingHTLCFailureMsg(htlc.ID, pkt.obfuscator, htlc.Reason)
	if err != nil {
		l.log.Errorf("unable to send HTLC failure: %v", err)

		return
	}

	// If the packet does not have a link failure set, it failed further
	// down the route so we notify a forwarding failure. Otherwise, we
	// notify a link failure because it failed at our node.
	if pkt.linkFailure != nil {
		l.cfg.HtlcNotifier.NotifyLinkFailEvent(
			newHtlcKey(pkt), newHtlcInfo(pkt), getEventType(pkt),
			pkt.linkFailure, false,
		)
	} else {
		l.cfg.HtlcNotifier.NotifyForwardingFailEvent(
			newHtlcKey(pkt), getEventType(pkt),
		)
	}

	// Immediately update the commitment tx to minimize latency.
	l.updateCommitTxOrFail(ctx)
}

// validateHtlcAmount checks if the HTLC amount is within the policy's
// minimum and maximum limits. Returns a LinkError if validation fails.
func (l *channelLink) validateHtlcAmount(policy models.ForwardingPolicy,
	payHash [32]byte, amt lnwire.MilliSatoshi,
	originalScid lnwire.ShortChannelID,
	customRecords lnwire.CustomRecords) *LinkError {

	// In case we are dealing with a custom HTLC, we don't need to validate
	// the HTLC constraints.
	//
	// NOTE: Custom HTLCs are only locally sourced and will use custom
	// channels which are not routable channels and should have their policy
	// not restricted in the first place. However to be sure we skip this
	// check otherwise we might end up in a loop of sending to the same
	// route again and again because link errors are not persisted in
	// mission control.
	if fn.MapOptionZ(
		l.cfg.AuxTrafficShaper,
		func(ts AuxTrafficShaper) bool {
			return ts.IsCustomHTLC(customRecords)
		},
	) {

		l.log.Debugf("Skipping htlc amount policy validation for " +
			"custom htlc")

		return nil
	}

	// As our first sanity check, we'll ensure that the passed HTLC isn't
	// too small for the next hop. If so, then we'll cancel the HTLC
	// directly.
	if amt < policy.MinHTLCOut {
		l.log.Warnf("outgoing htlc(%x) is too small: min_htlc=%v, "+
			"htlc_value=%v", payHash[:], policy.MinHTLCOut,
			amt)

		// As part of the returned error, we'll send our latest routing
		// policy so the sending node obtains the most up to date data.
		cb := func(upd *lnwire.ChannelUpdate1) lnwire.FailureMessage {
			return lnwire.NewAmountBelowMinimum(amt, *upd)
		}
		failure := l.createFailureWithUpdate(false, originalScid, cb)

		return NewLinkError(failure)
	}

	// Next, ensure that the passed HTLC isn't too large. If so, we'll
	// cancel the HTLC directly.
	if policy.MaxHTLC != 0 && amt > policy.MaxHTLC {
		l.log.Warnf("outgoing htlc(%x) is too large: max_htlc=%v, "+
			"htlc_value=%v", payHash[:], policy.MaxHTLC, amt)

		// As part of the returned error, we'll send our latest routing
		// policy so the sending node obtains the most up-to-date data.
		cb := func(upd *lnwire.ChannelUpdate1) lnwire.FailureMessage {
			return lnwire.NewTemporaryChannelFailure(upd)
		}
		failure := l.createFailureWithUpdate(false, originalScid, cb)

		return NewDetailedLinkError(
			failure, OutgoingFailureHTLCExceedsMax,
		)
	}

	return nil
}

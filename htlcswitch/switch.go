package htlcswitch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
)

const (
	// DefaultFwdEventInterval is the duration between attempts to flush
	// pending forwarding events to disk.
	DefaultFwdEventInterval = 15 * time.Second

	// DefaultLogInterval is the duration between attempts to log statistics
	// about forwarding events.
	DefaultLogInterval = 10 * time.Second

	// DefaultAckInterval is the duration between attempts to ack any settle
	// fails in a forwarding package.
	DefaultAckInterval = 15 * time.Second

	// DefaultMailboxDeliveryTimeout is the duration after which Adds will
	// be cancelled if they could not get added to an outgoing commitment.
	DefaultMailboxDeliveryTimeout = time.Minute
)

var (
	// ErrChannelLinkNotFound is used when channel link hasn't been found.
	ErrChannelLinkNotFound = errors.New("channel link not found")

	// ErrDuplicateAdd signals that the ADD htlc was already forwarded
	// through the switch and is locked into another commitment txn.
	ErrDuplicateAdd = errors.New("duplicate add HTLC detected")

	// ErrUnknownErrorDecryptor signals that we were unable to locate the
	// error decryptor for this payment. This is likely due to restarting
	// the daemon.
	ErrUnknownErrorDecryptor = errors.New("unknown error decryptor")

	// ErrSwitchExiting signaled when the switch has received a shutdown
	// request.
	ErrSwitchExiting = errors.New("htlcswitch shutting down")

	// ErrNoLinksFound is an error returned when we attempt to retrieve the
	// active links in the switch for a specific destination.
	ErrNoLinksFound = errors.New("no channel links found")

	// ErrUnreadableFailureMessage is returned when the failure message
	// cannot be decrypted.
	ErrUnreadableFailureMessage = errors.New("unreadable failure message")

	// ErrLocalAddFailed signals that the ADD htlc for a local payment
	// failed to be processed.
	ErrLocalAddFailed = errors.New("local add HTLC failed")

	// errFeeExposureExceeded is only surfaced to callers of SendHTLC and
	// signals that sending the HTLC would exceed the outgoing link's fee
	// exposure threshold.
	errFeeExposureExceeded = errors.New("fee exposure exceeded")

	// DefaultMaxFeeExposure is the default threshold after which we'll
	// fail payments if they increase our fee exposure. This is currently
	// set to 500m msats.
	DefaultMaxFeeExposure = lnwire.MilliSatoshi(500_000_000)
)

// plexPacket encapsulates switch packet and adds error channel to receive
// error from request handler.
type plexPacket struct {
	pkt *htlcPacket
	err chan error
}

// ChanClose represents a request which close a particular channel specified by
// its id.
type ChanClose struct {
	// CloseType is a variable which signals the type of channel closure the
	// peer should execute.
	CloseType contractcourt.ChannelCloseType

	// ChanPoint represent the id of the channel which should be closed.
	ChanPoint *wire.OutPoint

	// TargetFeePerKw is the ideal fee that was specified by the caller.
	// This value is only utilized if the closure type is CloseRegular.
	// This will be the starting offered fee when the fee negotiation
	// process for the cooperative closure transaction kicks off.
	TargetFeePerKw chainfee.SatPerKWeight

	// MaxFee is the highest fee the caller is willing to pay.
	//
	// NOTE: This field is only respected if the caller is the initiator of
	// the channel.
	MaxFee chainfee.SatPerKWeight

	// DeliveryScript is an optional delivery script to pay funds out to.
	DeliveryScript lnwire.DeliveryAddress

	// Updates is used by request creator to receive the notifications about
	// execution of the close channel request.
	Updates chan interface{}

	// Err is used by request creator to receive request execution error.
	Err chan error

	// Ctx is a context linked to the lifetime of the caller.
	Ctx context.Context //nolint:containedctx
}

// Config defines the configuration for the service. ALL elements within the
// configuration MUST be non-nil for the service to carry out its duties.
type Config struct {
	// FwdingLog is an interface that will be used by the switch to log
	// forwarding events. A forwarding event happens each time a payment
	// circuit is successfully completed. So when we forward an HTLC, and a
	// settle is eventually received.
	FwdingLog ForwardingLog

	// LocalChannelClose kicks-off the workflow to execute a cooperative or
	// forced unilateral closure of the channel initiated by a local
	// subsystem.
	LocalChannelClose func(pubKey []byte, request *ChanClose)

	// DB is the database backend that will be used to back the switch's
	// persistent circuit map.
	DB kvdb.Backend

	// FetchAllOpenChannels is a function that fetches all currently open
	// channels from the channel database.
	FetchAllOpenChannels func() ([]*channeldb.OpenChannel, error)

	// FetchAllChannels is a function that fetches all pending open, open,
	// and waiting close channels from the database.
	FetchAllChannels func() ([]*channeldb.OpenChannel, error)

	// FetchClosedChannels is a function that fetches all closed channels
	// from the channel database.
	FetchClosedChannels func(
		pendingOnly bool) ([]*channeldb.ChannelCloseSummary, error)

	// SwitchPackager provides access to the forwarding packages of all
	// active channels. This gives the switch the ability to read arbitrary
	// forwarding packages, and ack settles and fails contained within them.
	SwitchPackager channeldb.FwdOperator

	// ExtractErrorEncrypter is an interface allowing switch to reextract
	// error encrypters stored in the circuit map on restarts, since they
	// are not stored directly within the database.
	ExtractErrorEncrypter hop.ErrorEncrypterExtracter

	// FetchLastChannelUpdate retrieves the latest routing policy for a
	// target channel. This channel will typically be the outgoing channel
	// specified when we receive an incoming HTLC.  This will be used to
	// provide payment senders our latest policy when sending encrypted
	// error messages.
	FetchLastChannelUpdate func(lnwire.ShortChannelID) (
		*lnwire.ChannelUpdate1, error)

	// Notifier is an instance of a chain notifier that we'll use to signal
	// the switch when a new block has arrived.
	Notifier chainntnfs.ChainNotifier

	// HtlcNotifier is an instance of a htlcNotifier which we will pipe htlc
	// events through.
	HtlcNotifier htlcNotifier

	// FwdEventTicker is a signal that instructs the htlcswitch to flush any
	// pending forwarding events.
	FwdEventTicker ticker.Ticker

	// LogEventTicker is a signal instructing the htlcswitch to log
	// aggregate stats about it's forwarding during the last interval.
	LogEventTicker ticker.Ticker

	// AckEventTicker is a signal instructing the htlcswitch to ack any settle
	// fails in forwarding packages.
	AckEventTicker ticker.Ticker

	// AllowCircularRoute is true if the user has configured their node to
	// allow forwards that arrive and depart our node over the same channel.
	AllowCircularRoute bool

	// RejectHTLC is a flag that instructs the htlcswitch to reject any
	// HTLCs that are not from the source hop.
	RejectHTLC bool

	// Clock is a time source for the switch.
	Clock clock.Clock

	// MailboxDeliveryTimeout is the interval after which Adds will be
	// cancelled if they have not been yet been delivered to a link. The
	// computed deadline will expiry this long after the Adds are added to
	// a mailbox via AddPacket.
	MailboxDeliveryTimeout time.Duration

	// MaxFeeExposure is the threshold in milli-satoshis after which we'll
	// fail incoming or outgoing payments for a particular channel.
	MaxFeeExposure lnwire.MilliSatoshi

	// SignAliasUpdate is used when sending FailureMessages backwards for
	// option_scid_alias channels. This avoids a potential privacy leak by
	// replacing the public, confirmed SCID with the alias in the
	// ChannelUpdate.
	SignAliasUpdate func(u *lnwire.ChannelUpdate1) (*ecdsa.Signature,
		error)

	// IsAlias returns whether or not a given SCID is an alias.
	IsAlias func(scid lnwire.ShortChannelID) bool
}

// Switch is the central messaging bus for all incoming/outgoing HTLCs.
// Connected peers with active channels are treated as named interfaces which
// refer to active channels as links. A link is the switch's message
// communication point with the goroutine that manages an active channel. New
// links are registered each time a channel is created, and unregistered once
// the channel is closed. The switch manages the hand-off process for multi-hop
// HTLCs, forwarding HTLCs initiated from within the daemon, and finally
// notifies users local-systems concerning their outstanding payment requests.
type Switch struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	// bestHeight is the best known height of the main chain. The links will
	// be used this information to govern decisions based on HTLC timeouts.
	// This will be retrieved by the registered links atomically.
	bestHeight uint32

	wg   sync.WaitGroup
	quit chan struct{}

	// cfg is a copy of the configuration struct that the htlc switch
	// service was initialized with.
	cfg *Config

	// networkResults stores the results of payments initiated by the user.
	// The store is used to later look up the payments and notify the
	// user of the result when they are complete. Each payment attempt
	// should be given a unique integer ID when it is created, otherwise
	// results might be overwritten.
	networkResults *networkResultStore

	// circuits is storage for payment circuits which are used to
	// forward the settle/fail htlc updates back to the add htlc initiator.
	circuits CircuitMap

	// mailOrchestrator manages the lifecycle of mailboxes used throughout
	// the switch, and facilitates delayed delivery of packets to links that
	// later come online.
	mailOrchestrator *mailOrchestrator

	// indexMtx is a read/write mutex that protects the set of indexes
	// below.
	indexMtx sync.RWMutex

	// pendingLinkIndex holds links that have not had their final, live
	// short_chan_id assigned.
	pendingLinkIndex map[lnwire.ChannelID]ChannelLink

	// links is a map of channel id and channel link which manages
	// this channel.
	linkIndex map[lnwire.ChannelID]ChannelLink

	// forwardingIndex is an index which is consulted by the switch when it
	// needs to locate the next hop to forward an incoming/outgoing HTLC
	// update to/from.
	//
	// TODO(roasbeef): eventually add a NetworkHop mapping before the
	// ChannelLink
	forwardingIndex map[lnwire.ShortChannelID]ChannelLink

	// interfaceIndex maps the compressed public key of a peer to all the
	// channels that the switch maintains with that peer.
	interfaceIndex map[[33]byte]map[lnwire.ChannelID]ChannelLink

	// linkStopIndex stores the currently stopping ChannelLinks,
	// represented by their ChannelID. The key is the link's ChannelID and
	// the value is a chan that is closed when the link has fully stopped.
	// This map is only added to if RemoveLink is called and is not added
	// to when the Switch is shutting down and calls Stop() on each link.
	//
	// MUST be used with the indexMtx.
	linkStopIndex map[lnwire.ChannelID]chan struct{}

	// htlcPlex is the channel which all connected links use to coordinate
	// the setup/teardown of Sphinx (onion routing) payment circuits.
	// Active links forward any add/settle messages over this channel each
	// state transition, sending new adds/settles which are fully locked
	// in.
	htlcPlex chan *plexPacket

	// chanCloseRequests is used to transfer the channel close request to
	// the channel close handler.
	chanCloseRequests chan *ChanClose

	// resolutionMsgs is the channel that all external contract resolution
	// messages will be sent over.
	resolutionMsgs chan *resolutionMsg

	// pendingFwdingEvents is the set of forwarding events which have been
	// collected during the current interval, but hasn't yet been written
	// to the forwarding log.
	fwdEventMtx         sync.Mutex
	pendingFwdingEvents []channeldb.ForwardingEvent

	// blockEpochStream is an active block epoch event stream backed by an
	// active ChainNotifier instance. This will be used to retrieve the
	// latest height of the chain.
	blockEpochStream *chainntnfs.BlockEpochEvent

	// pendingSettleFails is the set of settle/fail entries that we need to
	// ack in the forwarding package of the outgoing link. This was added to
	// make pipelining settles more efficient.
	pendingSettleFails []channeldb.SettleFailRef

	// resMsgStore is used to store the set of ResolutionMsg that come from
	// contractcourt. This is used so the Switch can properly forward them,
	// even on restarts.
	resMsgStore *resolutionStore

	// aliasToReal is a map used for option-scid-alias feature-bit links.
	// The alias SCID is the key and the real, confirmed SCID is the value.
	// If the channel is unconfirmed, there will not be a mapping for it.
	// Since channels can have multiple aliases, this map is essentially a
	// N->1 mapping for a channel. This MUST be accessed with the indexMtx.
	aliasToReal map[lnwire.ShortChannelID]lnwire.ShortChannelID

	// baseIndex is a map used for option-scid-alias feature-bit links.
	// The value is the SCID of the link's ShortChannelID. This value may
	// be an alias for zero-conf channels or a confirmed SCID for
	// non-zero-conf channels with the option-scid-alias feature-bit. The
	// key includes the value itself and also any other aliases. This MUST
	// be accessed with the indexMtx.
	baseIndex map[lnwire.ShortChannelID]lnwire.ShortChannelID
}

// New creates the new instance of htlc switch.
func New(cfg Config, currentHeight uint32) (*Switch, error) {
	resStore := newResolutionStore(cfg.DB)

	circuitMap, err := NewCircuitMap(&CircuitMapConfig{
		DB:                    cfg.DB,
		FetchAllOpenChannels:  cfg.FetchAllOpenChannels,
		FetchClosedChannels:   cfg.FetchClosedChannels,
		ExtractErrorEncrypter: cfg.ExtractErrorEncrypter,
		CheckResolutionMsg:    resStore.checkResolutionMsg,
	})
	if err != nil {
		return nil, err
	}

	s := &Switch{
		bestHeight:        currentHeight,
		cfg:               &cfg,
		circuits:          circuitMap,
		linkIndex:         make(map[lnwire.ChannelID]ChannelLink),
		forwardingIndex:   make(map[lnwire.ShortChannelID]ChannelLink),
		interfaceIndex:    make(map[[33]byte]map[lnwire.ChannelID]ChannelLink),
		pendingLinkIndex:  make(map[lnwire.ChannelID]ChannelLink),
		linkStopIndex:     make(map[lnwire.ChannelID]chan struct{}),
		networkResults:    newNetworkResultStore(cfg.DB),
		htlcPlex:          make(chan *plexPacket),
		chanCloseRequests: make(chan *ChanClose),
		resolutionMsgs:    make(chan *resolutionMsg),
		resMsgStore:       resStore,
		quit:              make(chan struct{}),
	}

	s.aliasToReal = make(map[lnwire.ShortChannelID]lnwire.ShortChannelID)
	s.baseIndex = make(map[lnwire.ShortChannelID]lnwire.ShortChannelID)

	s.mailOrchestrator = newMailOrchestrator(&mailOrchConfig{
		forwardPackets:    s.ForwardPackets,
		clock:             s.cfg.Clock,
		expiry:            s.cfg.MailboxDeliveryTimeout,
		failMailboxUpdate: s.failMailboxUpdate,
	})

	return s, nil
}

// resolutionMsg is a struct that wraps an existing ResolutionMsg with a done
// channel. We'll use this channel to synchronize delivery of the message with
// the caller.
type resolutionMsg struct {
	contractcourt.ResolutionMsg

	errChan chan error
}

// ProcessContractResolution is called by active contract resolvers once a
// contract they are watching over has been fully resolved. The message carries
// an external signal that *would* have been sent if the outgoing channel
// didn't need to go to the chain in order to fulfill a contract. We'll process
// this message just as if it came from an active outgoing channel.
func (s *Switch) ProcessContractResolution(msg contractcourt.ResolutionMsg) error {
	errChan := make(chan error, 1)

	select {
	case s.resolutionMsgs <- &resolutionMsg{
		ResolutionMsg: msg,
		errChan:       errChan,
	}:
	case <-s.quit:
		return ErrSwitchExiting
	}

	select {
	case err := <-errChan:
		return err
	case <-s.quit:
		return ErrSwitchExiting
	}
}

// HasAttemptResult reads the network result store to fetch the specified
// attempt. Returns true if the attempt result exists.
func (s *Switch) HasAttemptResult(attemptID uint64) (bool, error) {
	_, err := s.networkResults.getResult(attemptID)
	if err == nil {
		return true, nil
	}

	if !errors.Is(err, ErrPaymentIDNotFound) {
		return false, err
	}

	return false, nil
}

// GetAttemptResult returns the result of the HTLC attempt with the given
// attemptID. The paymentHash should be set to the payment's overall hash, or
// in case of AMP payments the payment's unique identifier.
//
// The method returns a channel where the HTLC attempt result will be sent when
// available, or an error is encountered during forwarding. When a result is
// received on the channel, the HTLC is guaranteed to no longer be in flight.
// The switch shutting down is signaled by closing the channel. If the
// attemptID is unknown, ErrPaymentIDNotFound will be returned.
func (s *Switch) GetAttemptResult(attemptID uint64, paymentHash lntypes.Hash,
	deobfuscator ErrorDecrypter) (<-chan *PaymentResult, error) {

	var (
		nChan <-chan *networkResult
		err   error
		inKey = CircuitKey{
			ChanID: hop.Source,
			HtlcID: attemptID,
		}
	)

	// If the HTLC is not found in the circuit map, check whether a result
	// is already available.
	// Assumption: no one will add this attempt ID other than the caller.
	if s.circuits.LookupCircuit(inKey) == nil {
		res, err := s.networkResults.getResult(attemptID)
		if err != nil {
			return nil, err
		}
		c := make(chan *networkResult, 1)
		c <- res
		nChan = c
	} else {
		// The HTLC was committed to the circuits, subscribe for a
		// result.
		nChan, err = s.networkResults.subscribeResult(attemptID)
		if err != nil {
			return nil, err
		}
	}

	resultChan := make(chan *PaymentResult, 1)

	// Since the attempt was known, we can start a goroutine that can
	// extract the result when it is available, and pass it on to the
	// caller.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		var n *networkResult
		select {
		case n = <-nChan:
		case <-s.quit:
			// We close the result channel to signal a shutdown. We
			// don't send any result in this case since the HTLC is
			// still in flight.
			close(resultChan)
			return
		}

		log.Debugf("Received network result %T for attemptID=%v", n.msg,
			attemptID)

		// Extract the result and pass it to the result channel.
		result, err := s.extractResult(
			deobfuscator, n, attemptID, paymentHash,
		)
		if err != nil {
			e := fmt.Errorf("unable to extract result: %w", err)
			log.Error(e)
			resultChan <- &PaymentResult{
				Error: e,
			}
			return
		}
		resultChan <- result
	}()

	return resultChan, nil
}

// CleanStore calls the underlying result store, telling it is safe to delete
// all entries except the ones in the keepPids map. This should be called
// preiodically to let the switch clean up payment results that we have
// handled.
func (s *Switch) CleanStore(keepPids map[uint64]struct{}) error {
	return s.networkResults.cleanStore(keepPids)
}

// SendHTLC is used by other subsystems which aren't belong to htlc switch
// package in order to send the htlc update. The attemptID used MUST be unique
// for this HTLC, and MUST be used only once, otherwise the switch might reject
// it.
func (s *Switch) SendHTLC(firstHop lnwire.ShortChannelID, attemptID uint64,
	htlc *lnwire.UpdateAddHTLC) error {

	// Generate and send new update packet, if error will be received on
	// this stage it means that packet haven't left boundaries of our
	// system and something wrong happened.
	packet := &htlcPacket{
		incomingChanID: hop.Source,
		incomingHTLCID: attemptID,
		outgoingChanID: firstHop,
		htlc:           htlc,
		amount:         htlc.Amount,
	}

	// Attempt to fetch the target link before creating a circuit so that
	// we don't leave dangling circuits. The getLocalLink method does not
	// require the circuit variable to be set on the *htlcPacket.
	link, linkErr := s.getLocalLink(packet, htlc)
	if linkErr != nil {
		// Notify the htlc notifier of a link failure on our outgoing
		// link. Incoming timelock/amount values are not set because
		// they are not present for local sends.
		s.cfg.HtlcNotifier.NotifyLinkFailEvent(
			newHtlcKey(packet),
			HtlcInfo{
				OutgoingTimeLock: htlc.Expiry,
				OutgoingAmt:      htlc.Amount,
			},
			HtlcEventTypeSend,
			linkErr,
			false,
		)

		return linkErr
	}

	// Evaluate whether this HTLC would bypass our fee exposure. If it
	// does, don't send it out and instead return an error.
	if s.dustExceedsFeeThreshold(link, htlc.Amount, false) {
		// Notify the htlc notifier of a link failure on our outgoing
		// link. We use the FailTemporaryChannelFailure in place of a
		// more descriptive error message.
		linkErr := NewLinkError(
			&lnwire.FailTemporaryChannelFailure{},
		)
		s.cfg.HtlcNotifier.NotifyLinkFailEvent(
			newHtlcKey(packet),
			HtlcInfo{
				OutgoingTimeLock: htlc.Expiry,
				OutgoingAmt:      htlc.Amount,
			},
			HtlcEventTypeSend,
			linkErr,
			false,
		)

		return errFeeExposureExceeded
	}

	circuit := newPaymentCircuit(&htlc.PaymentHash, packet)
	actions, err := s.circuits.CommitCircuits(circuit)
	if err != nil {
		log.Errorf("unable to commit circuit in switch: %v", err)
		return err
	}

	// Drop duplicate packet if it has already been seen.
	switch {
	case len(actions.Drops) == 1:
		return ErrDuplicateAdd

	case len(actions.Fails) == 1:
		return ErrLocalAddFailed
	}

	// Give the packet to the link's mailbox so that HTLC's are properly
	// canceled back if the mailbox timeout elapses.
	packet.circuit = circuit

	return link.handleSwitchPacket(packet)
}

// UpdateForwardingPolicies sends a message to the switch to update the
// forwarding policies for the set of target channels, keyed in chanPolicies.
//
// NOTE: This function is synchronous and will block until either the
// forwarding policies for all links have been updated, or the switch shuts
// down.
func (s *Switch) UpdateForwardingPolicies(
	chanPolicies map[wire.OutPoint]models.ForwardingPolicy) {

	log.Tracef("Updating link policies: %v", lnutils.SpewLogClosure(
		chanPolicies))

	s.indexMtx.RLock()

	// Update each link in chanPolicies.
	for targetLink, policy := range chanPolicies {
		cid := lnwire.NewChanIDFromOutPoint(targetLink)

		link, ok := s.linkIndex[cid]
		if !ok {
			log.Debugf("Unable to find ChannelPoint(%v) to update "+
				"link policy", targetLink)
			continue
		}

		link.UpdateForwardingPolicy(policy)
	}

	s.indexMtx.RUnlock()
}

// IsForwardedHTLC checks for a given channel and htlc index if it is related
// to an opened circuit that represents a forwarded payment.
func (s *Switch) IsForwardedHTLC(chanID lnwire.ShortChannelID,
	htlcIndex uint64) bool {

	circuit := s.circuits.LookupOpenCircuit(models.CircuitKey{
		ChanID: chanID,
		HtlcID: htlcIndex,
	})
	return circuit != nil && circuit.Incoming.ChanID != hop.Source
}

// ForwardPackets adds a list of packets to the switch for processing. Fails
// and settles are added on a first past, simultaneously constructing circuits
// for any adds. After persisting the circuits, another pass of the adds is
// given to forward them through the router. The sending link's quit channel is
// used to prevent deadlocks when the switch stops a link in the midst of
// forwarding.
func (s *Switch) ForwardPackets(linkQuit <-chan struct{},
	packets ...*htlcPacket) error {

	var (
		// fwdChan is a buffered channel used to receive err msgs from
		// the htlcPlex when forwarding this batch.
		fwdChan = make(chan error, len(packets))

		// numSent keeps a running count of how many packets are
		// forwarded to the switch, which determines how many responses
		// we will wait for on the fwdChan..
		numSent int
	)

	// No packets, nothing to do.
	if len(packets) == 0 {
		return nil
	}

	// Setup a barrier to prevent the background tasks from processing
	// responses until this function returns to the user.
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Done()

	// Before spawning the following goroutine to proxy our error responses,
	// check to see if we have already been issued a shutdown request. If
	// so, we exit early to avoid incrementing the switch's waitgroup while
	// it is already in the process of shutting down.
	select {
	case <-linkQuit:
		return nil
	case <-s.quit:
		return nil
	default:
		// Spawn a goroutine to log the errors returned from failed packets.
		s.wg.Add(1)
		go s.logFwdErrs(&numSent, &wg, fwdChan)
	}

	// Make a first pass over the packets, forwarding any settles or fails.
	// As adds are found, we create a circuit and append it to our set of
	// circuits to be written to disk.
	var circuits []*PaymentCircuit
	var addBatch []*htlcPacket
	for _, packet := range packets {
		switch htlc := packet.htlc.(type) {
		case *lnwire.UpdateAddHTLC:
			circuit := newPaymentCircuit(&htlc.PaymentHash, packet)
			packet.circuit = circuit
			circuits = append(circuits, circuit)
			addBatch = append(addBatch, packet)
		default:
			err := s.routeAsync(packet, fwdChan, linkQuit)
			if err != nil {
				return fmt.Errorf("failed to forward packet %w",
					err)
			}
			numSent++
		}
	}

	// If this batch did not contain any circuits to commit, we can return
	// early.
	if len(circuits) == 0 {
		return nil
	}

	// Write any circuits that we found to disk.
	actions, err := s.circuits.CommitCircuits(circuits...)
	if err != nil {
		log.Errorf("unable to commit circuits in switch: %v", err)
	}

	// Split the htlc packets by comparing an in-order seek to the head of
	// the added, dropped, or failed circuits.
	//
	// NOTE: This assumes each list is guaranteed to be a subsequence of the
	// circuits, and that the union of the sets results in the original set
	// of circuits.
	var addedPackets, failedPackets []*htlcPacket
	for _, packet := range addBatch {
		switch {
		case len(actions.Adds) > 0 && packet.circuit == actions.Adds[0]:
			addedPackets = append(addedPackets, packet)
			actions.Adds = actions.Adds[1:]

		case len(actions.Drops) > 0 && packet.circuit == actions.Drops[0]:
			actions.Drops = actions.Drops[1:]

		case len(actions.Fails) > 0 && packet.circuit == actions.Fails[0]:
			failedPackets = append(failedPackets, packet)
			actions.Fails = actions.Fails[1:]
		}
	}

	// Now, forward any packets for circuits that were successfully added to
	// the switch's circuit map.
	for _, packet := range addedPackets {
		err := s.routeAsync(packet, fwdChan, linkQuit)
		if err != nil {
			return fmt.Errorf("failed to forward packet %w", err)
		}
		numSent++
	}

	// Lastly, for any packets that failed, this implies that they were
	// left in a half added state, which can happen when recovering from
	// failures.
	if len(failedPackets) > 0 {
		var failure lnwire.FailureMessage
		incomingID := failedPackets[0].incomingChanID

		// If the incoming channel is an option_scid_alias channel,
		// then we'll need to replace the SCID in the ChannelUpdate.
		update := s.failAliasUpdate(incomingID, true)
		if update == nil {
			// Fallback to the original non-option behavior.
			update, err := s.cfg.FetchLastChannelUpdate(
				incomingID,
			)
			if err != nil {
				failure = &lnwire.FailTemporaryNodeFailure{}
			} else {
				failure = lnwire.NewTemporaryChannelFailure(
					update,
				)
			}
		} else {
			// This is an option_scid_alias channel.
			failure = lnwire.NewTemporaryChannelFailure(update)
		}

		linkError := NewDetailedLinkError(
			failure, OutgoingFailureIncompleteForward,
		)

		for _, packet := range failedPackets {
			// We don't handle the error here since this method
			// always returns an error.
			_ = s.failAddPacket(packet, linkError)
		}
	}

	return nil
}

// logFwdErrs logs any errors received on `fwdChan`.
func (s *Switch) logFwdErrs(num *int, wg *sync.WaitGroup, fwdChan chan error) {
	defer s.wg.Done()

	// Wait here until the outer function has finished persisting
	// and routing the packets. This guarantees we don't read from num until
	// the value is accurate.
	wg.Wait()

	numSent := *num
	for i := 0; i < numSent; i++ {
		select {
		case err := <-fwdChan:
			if err != nil {
				log.Errorf("Unhandled error while reforwarding htlc "+
					"settle/fail over htlcswitch: %v", err)
			}
		case <-s.quit:
			log.Errorf("unable to forward htlc packet " +
				"htlc switch was stopped")
			return
		}
	}
}

// routeAsync sends a packet through the htlc switch, using the provided err
// chan to propagate errors back to the caller. The link's quit channel is
// provided so that the send can be canceled if either the link or the switch
// receive a shutdown requuest. This method does not wait for a response from
// the htlcForwarder before returning.
func (s *Switch) routeAsync(packet *htlcPacket, errChan chan error,
	linkQuit <-chan struct{}) error {

	command := &plexPacket{
		pkt: packet,
		err: errChan,
	}

	select {
	case s.htlcPlex <- command:
		return nil
	case <-linkQuit:
		return ErrLinkShuttingDown
	case <-s.quit:
		return errors.New("htlc switch was stopped")
	}
}

// getLocalLink handles the addition of a htlc for a send that originates from
// our node. It returns the link that the htlc should be forwarded outwards on,
// and a link error if the htlc cannot be forwarded.
func (s *Switch) getLocalLink(pkt *htlcPacket, htlc *lnwire.UpdateAddHTLC) (
	ChannelLink, *LinkError) {

	// Try to find links by node destination.
	s.indexMtx.RLock()
	link, err := s.getLinkByShortID(pkt.outgoingChanID)
	if err != nil {
		// If the link was not found for the outgoingChanID, an outside
		// subsystem may be using the confirmed SCID of a zero-conf
		// channel. In this case, we'll consult the Switch maps to see
		// if an alias exists and use the alias to lookup the link.
		// This extra step is a consequence of not updating the Switch
		// forwardingIndex when a zero-conf channel is confirmed. We
		// don't need to change the outgoingChanID since the link will
		// do that upon receiving the packet.
		baseScid, ok := s.baseIndex[pkt.outgoingChanID]
		if !ok {
			s.indexMtx.RUnlock()
			log.Errorf("Link %v not found", pkt.outgoingChanID)
			return nil, NewLinkError(&lnwire.FailUnknownNextPeer{})
		}

		// The base SCID was found, so we'll use that to fetch the
		// link.
		link, err = s.getLinkByShortID(baseScid)
		if err != nil {
			s.indexMtx.RUnlock()
			log.Errorf("Link %v not found", baseScid)
			return nil, NewLinkError(&lnwire.FailUnknownNextPeer{})
		}
	}
	// We finished looking up the indexes, so we can unlock the mutex before
	// performing the link operations which might also acquire the lock
	// in case e.g. failAliasUpdate is called.
	s.indexMtx.RUnlock()

	if !link.EligibleToForward() {
		log.Errorf("Link %v is not available to forward",
			pkt.outgoingChanID)

		// The update does not need to be populated as the error
		// will be returned back to the router.
		return nil, NewDetailedLinkError(
			lnwire.NewTemporaryChannelFailure(nil),
			OutgoingFailureLinkNotEligible,
		)
	}

	// Ensure that the htlc satisfies the outgoing channel policy.
	currentHeight := atomic.LoadUint32(&s.bestHeight)
	htlcErr := link.CheckHtlcTransit(
		htlc.PaymentHash, htlc.Amount, htlc.Expiry, currentHeight,
		htlc.CustomRecords,
	)
	if htlcErr != nil {
		log.Errorf("Link %v policy for local forward not "+
			"satisfied", pkt.outgoingChanID)
		return nil, htlcErr
	}

	return link, nil
}

// handleLocalResponse processes a Settle or Fail responding to a
// locally-initiated payment. This is handled asynchronously to avoid blocking
// the main event loop within the switch, as these operations can require
// multiple db transactions. The guarantees of the circuit map are stringent
// enough such that we are able to tolerate reordering of these operations
// without side effects. The primary operations handled are:
//  1. Save the payment result to the pending payment store.
//  2. Notify subscribers about the payment result.
//  3. Ack settle/fail references, to avoid resending this response internally
//  4. Teardown the closing circuit in the circuit map
//
// NOTE: This method MUST be spawned as a goroutine.
func (s *Switch) handleLocalResponse(pkt *htlcPacket) {
	defer s.wg.Done()

	attemptID := pkt.incomingHTLCID

	// The error reason will be unencypted in case this a local
	// failure or a converted error.
	unencrypted := pkt.localFailure || pkt.convertedError
	n := &networkResult{
		msg:          pkt.htlc,
		unencrypted:  unencrypted,
		isResolution: pkt.isResolution,
	}

	// Store the result to the db. This will also notify subscribers about
	// the result.
	if err := s.networkResults.storeResult(attemptID, n); err != nil {
		log.Errorf("Unable to store attempt result for pid=%v: %v",
			attemptID, err)
		return
	}

	// First, we'll clean up any fwdpkg references, circuit entries, and
	// mark in our db that the payment for this payment hash has either
	// succeeded or failed.
	//
	// If this response is contained in a forwarding package, we'll start by
	// acking the settle/fail so that we don't continue to retransmit the
	// HTLC internally.
	if pkt.destRef != nil {
		if err := s.ackSettleFail(*pkt.destRef); err != nil {
			log.Warnf("Unable to ack settle/fail reference: %s: %v",
				*pkt.destRef, err)
			return
		}
	}

	// Next, we'll remove the circuit since we are about to complete an
	// fulfill/fail of this HTLC. Since we've already removed the
	// settle/fail fwdpkg reference, the response from the peer cannot be
	// replayed internally if this step fails. If this happens, this logic
	// will be executed when a provided resolution message comes through.
	// This can only happen if the circuit is still open, which is why this
	// ordering is chosen.
	if err := s.teardownCircuit(pkt); err != nil {
		log.Errorf("Unable to teardown circuit %s: %v",
			pkt.inKey(), err)
		return
	}

	// Finally, notify on the htlc failure or success that has been handled.
	key := newHtlcKey(pkt)
	eventType := getEventType(pkt)

	switch htlc := pkt.htlc.(type) {
	case *lnwire.UpdateFulfillHTLC:
		s.cfg.HtlcNotifier.NotifySettleEvent(key, htlc.PaymentPreimage,
			eventType)

	case *lnwire.UpdateFailHTLC:
		s.cfg.HtlcNotifier.NotifyForwardingFailEvent(key, eventType)
	}
}

// extractResult uses the given deobfuscator to extract the payment result from
// the given network message.
func (s *Switch) extractResult(deobfuscator ErrorDecrypter, n *networkResult,
	attemptID uint64, paymentHash lntypes.Hash) (*PaymentResult, error) {

	switch htlc := n.msg.(type) {

	// We've received a settle update which means we can finalize the user
	// payment and return successful response.
	case *lnwire.UpdateFulfillHTLC:
		return &PaymentResult{
			Preimage: htlc.PaymentPreimage,
		}, nil

	// We've received a fail update which means we can finalize the
	// user payment and return fail response.
	case *lnwire.UpdateFailHTLC:
		// TODO(yy): construct deobfuscator here to avoid creating it
		// in paymentLifecycle even for settled HTLCs.
		paymentErr := s.parseFailedPayment(
			deobfuscator, attemptID, paymentHash, n.unencrypted,
			n.isResolution, htlc,
		)

		return &PaymentResult{
			Error: paymentErr,
		}, nil

	default:
		return nil, fmt.Errorf("received unknown response type: %T",
			htlc)
	}
}

// parseFailedPayment determines the appropriate failure message to return to
// a user initiated payment. The three cases handled are:
//  1. An unencrypted failure, which should already plaintext.
//  2. A resolution from the chain arbitrator, which possibly has no failure
//     reason attached.
//  3. A failure from the remote party, which will need to be decrypted using
//     the payment deobfuscator.
func (s *Switch) parseFailedPayment(deobfuscator ErrorDecrypter,
	attemptID uint64, paymentHash lntypes.Hash, unencrypted,
	isResolution bool, htlc *lnwire.UpdateFailHTLC) error {

	switch {

	// The payment never cleared the link, so we don't need to
	// decrypt the error, simply decode it them report back to the
	// user.
	case unencrypted:
		r := bytes.NewReader(htlc.Reason)
		failureMsg, err := lnwire.DecodeFailure(r, 0)
		if err != nil {
			// If we could not decode the failure reason, return a link
			// error indicating that we failed to decode the onion.
			linkError := NewDetailedLinkError(
				// As this didn't even clear the link, we don't
				// need to apply an update here since it goes
				// directly to the router.
				lnwire.NewTemporaryChannelFailure(nil),
				OutgoingFailureDecodeError,
			)

			log.Errorf("%v: (hash=%v, pid=%d): %v",
				linkError.FailureDetail.FailureString(),
				paymentHash, attemptID, err)

			return linkError
		}

		// If we successfully decoded the failure reason, return it.
		return NewLinkError(failureMsg)

	// A payment had to be timed out on chain before it got past
	// the first hop. In this case, we'll report a permanent
	// channel failure as this means us, or the remote party had to
	// go on chain.
	case isResolution && htlc.Reason == nil:
		linkError := NewDetailedLinkError(
			&lnwire.FailPermanentChannelFailure{},
			OutgoingFailureOnChainTimeout,
		)

		log.Infof("%v: hash=%v, pid=%d",
			linkError.FailureDetail.FailureString(),
			paymentHash, attemptID)

		return linkError

	// A regular multi-hop payment error that we'll need to
	// decrypt.
	default:
		// We'll attempt to fully decrypt the onion encrypted
		// error. If we're unable to then we'll bail early.
		failure, err := deobfuscator.DecryptError(htlc.Reason)
		if err != nil {
			log.Errorf("unable to de-obfuscate onion failure "+
				"(hash=%v, pid=%d): %v",
				paymentHash, attemptID, err)

			return ErrUnreadableFailureMessage
		}

		return failure
	}
}

// handlePacketForward is used in cases when we need forward the htlc update
// from one channel link to another and be able to propagate the settle/fail
// updates back. This behaviour is achieved by creation of payment circuits.
func (s *Switch) handlePacketForward(packet *htlcPacket) error {
	switch htlc := packet.htlc.(type) {
	// Channel link forwarded us a new htlc, therefore we initiate the
	// payment circuit within our internal state so we can properly forward
	// the ultimate settle message back latter.
	case *lnwire.UpdateAddHTLC:
		return s.handlePacketAdd(packet, htlc)

	case *lnwire.UpdateFulfillHTLC:
		return s.handlePacketSettle(packet)

	// Channel link forwarded us an update_fail_htlc message.
	//
	// NOTE: when the channel link receives an update_fail_malformed_htlc
	// from upstream, it will convert the message into update_fail_htlc and
	// forward it. Thus there's no need to catch `UpdateFailMalformedHTLC`
	// here.
	case *lnwire.UpdateFailHTLC:
		return s.handlePacketFail(packet, htlc)

	default:
		return fmt.Errorf("wrong update type: %T", htlc)
	}
}

// checkCircularForward checks whether a forward is circular (arrives and
// departs on the same link) and returns a link error if the switch is
// configured to disallow this behaviour.
func (s *Switch) checkCircularForward(incoming, outgoing lnwire.ShortChannelID,
	allowCircular bool, paymentHash lntypes.Hash) *LinkError {

	log.Tracef("Checking for circular route: incoming=%v, outgoing=%v "+
		"(payment hash: %x)", incoming, outgoing, paymentHash[:])

	// If they are equal, we can skip the alias mapping checks.
	if incoming == outgoing {
		// The switch may be configured to allow circular routes, so
		// just log and return nil.
		if allowCircular {
			log.Debugf("allowing circular route over link: %v "+
				"(payment hash: %x)", incoming, paymentHash)
			return nil
		}

		// Otherwise, we'll return a temporary channel failure.
		return NewDetailedLinkError(
			lnwire.NewTemporaryChannelFailure(nil),
			OutgoingFailureCircularRoute,
		)
	}

	// We'll fetch the "base" SCID from the baseIndex for the incoming and
	// outgoing SCIDs. If either one does not have a base SCID, then the
	// two channels are not equal since one will be a channel that does not
	// need a mapping and SCID equality was checked above. If the "base"
	// SCIDs are equal, then this is a circular route. Otherwise, it isn't.
	s.indexMtx.RLock()
	incomingBaseScid, ok := s.baseIndex[incoming]
	if !ok {
		// This channel does not use baseIndex, bail out.
		s.indexMtx.RUnlock()
		return nil
	}

	outgoingBaseScid, ok := s.baseIndex[outgoing]
	if !ok {
		// This channel does not use baseIndex, bail out.
		s.indexMtx.RUnlock()
		return nil
	}
	s.indexMtx.RUnlock()

	// Check base SCID equality.
	if incomingBaseScid != outgoingBaseScid {
		log.Tracef("Incoming base SCID %v does not match outgoing "+
			"base SCID %v (payment hash: %x)", incomingBaseScid,
			outgoingBaseScid, paymentHash[:])

		// The base SCIDs are not equal so these are not the same
		// channel.
		return nil
	}

	// If the incoming and outgoing link are equal, the htlc is part of a
	// circular route which may be used to lock up our liquidity. If the
	// switch is configured to allow circular routes, log that we are
	// allowing the route then return nil.
	if allowCircular {
		log.Debugf("allowing circular route over link: %v "+
			"(payment hash: %x)", incoming, paymentHash)
		return nil
	}

	// If our node disallows circular routes, return a temporary channel
	// failure. There is nothing wrong with the policy used by the remote
	// node, so we do not include a channel update.
	return NewDetailedLinkError(
		lnwire.NewTemporaryChannelFailure(nil),
		OutgoingFailureCircularRoute,
	)
}

// failAddPacket encrypts a fail packet back to an add packet's source.
// The ciphertext will be derived from the failure message proivded by context.
// This method returns the failErr if all other steps complete successfully.
func (s *Switch) failAddPacket(packet *htlcPacket, failure *LinkError) error {
	// Encrypt the failure so that the sender will be able to read the error
	// message. Since we failed this packet, we use EncryptFirstHop to
	// obfuscate the failure for their eyes only.
	reason, err := packet.obfuscator.EncryptFirstHop(failure.WireMessage())
	if err != nil {
		err := fmt.Errorf("unable to obfuscate "+
			"error: %v", err)
		log.Error(err)
		return err
	}

	log.Error(failure.Error())

	// Create a failure packet for this htlc. The full set of
	// information about the htlc failure is included so that they can
	// be included in link failure notifications.
	failPkt := &htlcPacket{
		sourceRef:       packet.sourceRef,
		incomingChanID:  packet.incomingChanID,
		incomingHTLCID:  packet.incomingHTLCID,
		outgoingChanID:  packet.outgoingChanID,
		outgoingHTLCID:  packet.outgoingHTLCID,
		incomingAmount:  packet.incomingAmount,
		amount:          packet.amount,
		incomingTimeout: packet.incomingTimeout,
		outgoingTimeout: packet.outgoingTimeout,
		circuit:         packet.circuit,
		obfuscator:      packet.obfuscator,
		linkFailure:     failure,
		htlc: &lnwire.UpdateFailHTLC{
			Reason: reason,
		},
	}

	// Route a fail packet back to the source link.
	err = s.mailOrchestrator.Deliver(failPkt.incomingChanID, failPkt)
	if err != nil {
		err = fmt.Errorf("source chanid=%v unable to "+
			"handle switch packet: %v",
			packet.incomingChanID, err)
		log.Error(err)
		return err
	}

	return failure
}

// closeCircuit accepts a settle or fail htlc and the associated htlc packet and
// attempts to determine the source that forwarded this htlc. This method will
// set the incoming chan and htlc ID of the given packet if the source was
// found, and will properly [re]encrypt any failure messages.
func (s *Switch) closeCircuit(pkt *htlcPacket) (*PaymentCircuit, error) {
	// If the packet has its source, that means it was failed locally by
	// the outgoing link. We fail it here to make sure only one response
	// makes it through the switch.
	if pkt.hasSource {
		circuit, err := s.circuits.FailCircuit(pkt.inKey())
		switch err {

		// Circuit successfully closed.
		case nil:
			return circuit, nil

		// Circuit was previously closed, but has not been deleted.
		// We'll just drop this response until the circuit has been
		// fully removed.
		case ErrCircuitClosing:
			return nil, err

		// Failed to close circuit because it does not exist. This is
		// likely because the circuit was already successfully closed.
		// Since this packet failed locally, there is no forwarding
		// package entry to acknowledge.
		case ErrUnknownCircuit:
			return nil, err

		// Unexpected error.
		default:
			return nil, err
		}
	}

	// Otherwise, this is packet was received from the remote party.  Use
	// circuit map to find the incoming link to receive the settle/fail.
	circuit, err := s.circuits.CloseCircuit(pkt.outKey())
	switch err {

	// Open circuit successfully closed.
	case nil:
		pkt.incomingChanID = circuit.Incoming.ChanID
		pkt.incomingHTLCID = circuit.Incoming.HtlcID
		pkt.circuit = circuit
		pkt.sourceRef = &circuit.AddRef

		pktType := "SETTLE"
		if _, ok := pkt.htlc.(*lnwire.UpdateFailHTLC); ok {
			pktType = "FAIL"
		}

		log.Debugf("Closed completed %s circuit for %x: "+
			"(%s, %d) <-> (%s, %d)", pktType, pkt.circuit.PaymentHash,
			pkt.incomingChanID, pkt.incomingHTLCID,
			pkt.outgoingChanID, pkt.outgoingHTLCID)

		return circuit, nil

	// Circuit was previously closed, but has not been deleted. We'll just
	// drop this response until the circuit has been removed.
	case ErrCircuitClosing:
		return nil, err

	// Failed to close circuit because it does not exist. This is likely
	// because the circuit was already successfully closed.
	case ErrUnknownCircuit:
		if pkt.destRef != nil {
			// Add this SettleFailRef to the set of pending settle/fail entries
			// awaiting acknowledgement.
			s.pendingSettleFails = append(s.pendingSettleFails, *pkt.destRef)
		}

		// If this is a settle, we will not log an error message as settles
		// are expected to hit the ErrUnknownCircuit case. The only way fails
		// can hit this case if the link restarts after having just sent a fail
		// to the switch.
		_, isSettle := pkt.htlc.(*lnwire.UpdateFulfillHTLC)
		if !isSettle {
			err := fmt.Errorf("unable to find target channel "+
				"for HTLC fail: channel ID = %s, "+
				"HTLC ID = %d", pkt.outgoingChanID,
				pkt.outgoingHTLCID)
			log.Error(err)

			return nil, err
		}

		return nil, nil

	// Unexpected error.
	default:
		return nil, err
	}
}

// ackSettleFail is used by the switch to ACK any settle/fail entries in the
// forwarding package of the outgoing link for a payment circuit. We do this if
// we're the originator of the payment, so the link stops attempting to
// re-broadcast.
func (s *Switch) ackSettleFail(settleFailRefs ...channeldb.SettleFailRef) error {
	return kvdb.Batch(s.cfg.DB, func(tx kvdb.RwTx) error {
		return s.cfg.SwitchPackager.AckSettleFails(tx, settleFailRefs...)
	})
}

// teardownCircuit removes a pending or open circuit from the switch's circuit
// map and prints useful logging statements regarding the outcome.
func (s *Switch) teardownCircuit(pkt *htlcPacket) error {
	var pktType string
	switch htlc := pkt.htlc.(type) {
	case *lnwire.UpdateFulfillHTLC:
		pktType = "SETTLE"
	case *lnwire.UpdateFailHTLC:
		pktType = "FAIL"
	default:
		return fmt.Errorf("cannot tear down packet of type: %T", htlc)
	}

	var paymentHash lntypes.Hash

	// Perform a defensive check to make sure we don't try to access a nil
	// circuit.
	circuit := pkt.circuit
	if circuit != nil {
		copy(paymentHash[:], circuit.PaymentHash[:])
	}

	log.Debugf("Tearing down circuit with %s pkt, removing circuit=%v "+
		"with keystone=%v", pktType, pkt.inKey(), pkt.outKey())

	err := s.circuits.DeleteCircuits(pkt.inKey())
	if err != nil {
		log.Warnf("Failed to tear down circuit (%s, %d) <-> (%s, %d) "+
			"with payment_hash=%v using %s pkt", pkt.incomingChanID,
			pkt.incomingHTLCID, pkt.outgoingChanID,
			pkt.outgoingHTLCID, pkt.circuit.PaymentHash, pktType)

		return err
	}

	log.Debugf("Closed %s circuit for %v: (%s, %d) <-> (%s, %d)", pktType,
		paymentHash, pkt.incomingChanID, pkt.incomingHTLCID,
		pkt.outgoingChanID, pkt.outgoingHTLCID)

	return nil
}

// CloseLink creates and sends the close channel command to the target link
// directing the specified closure type. If the closure type is CloseRegular,
// targetFeePerKw parameter should be the ideal fee-per-kw that will be used as
// a starting point for close negotiation. The deliveryScript parameter is an
// optional parameter which sets a user specified script to close out to.
func (s *Switch) CloseLink(ctx context.Context, chanPoint *wire.OutPoint,
	closeType contractcourt.ChannelCloseType,
	targetFeePerKw, maxFee chainfee.SatPerKWeight,
	deliveryScript lnwire.DeliveryAddress) (chan interface{}, chan error) {

	// TODO(roasbeef) abstract out the close updates.
	updateChan := make(chan interface{}, 2)
	errChan := make(chan error, 1)

	command := &ChanClose{
		CloseType:      closeType,
		ChanPoint:      chanPoint,
		Updates:        updateChan,
		TargetFeePerKw: targetFeePerKw,
		DeliveryScript: deliveryScript,
		Err:            errChan,
		MaxFee:         maxFee,
		Ctx:            ctx,
	}

	select {
	case s.chanCloseRequests <- command:
		return updateChan, errChan

	case <-s.quit:
		errChan <- ErrSwitchExiting
		close(updateChan)
		return updateChan, errChan
	}
}

// htlcForwarder is responsible for optimally forwarding (and possibly
// fragmenting) incoming/outgoing HTLCs amongst all active interfaces and their
// links. The duties of the forwarder are similar to that of a network switch,
// in that it facilitates multi-hop payments by acting as a central messaging
// bus. The switch communicates will active links to create, manage, and tear
// down active onion routed payments. Each active channel is modeled as
// networked device with metadata such as the available payment bandwidth, and
// total link capacity.
//
// NOTE: This MUST be run as a goroutine.
func (s *Switch) htlcForwarder() {
	defer s.wg.Done()

	defer func() {
		s.blockEpochStream.Cancel()

		// Remove all links once we've been signalled for shutdown.
		var linksToStop []ChannelLink
		s.indexMtx.Lock()
		for _, link := range s.linkIndex {
			activeLink := s.removeLink(link.ChanID())
			if activeLink == nil {
				log.Errorf("unable to remove ChannelLink(%v) "+
					"on stop", link.ChanID())
				continue
			}
			linksToStop = append(linksToStop, activeLink)
		}
		for _, link := range s.pendingLinkIndex {
			pendingLink := s.removeLink(link.ChanID())
			if pendingLink == nil {
				log.Errorf("unable to remove ChannelLink(%v) "+
					"on stop", link.ChanID())
				continue
			}
			linksToStop = append(linksToStop, pendingLink)
		}
		s.indexMtx.Unlock()

		// Now that all pending and live links have been removed from
		// the forwarding indexes, stop each one before shutting down.
		// We'll shut them down in parallel to make exiting as fast as
		// possible.
		var wg sync.WaitGroup
		for _, link := range linksToStop {
			wg.Add(1)
			go func(l ChannelLink) {
				defer wg.Done()

				l.Stop()
			}(link)
		}
		wg.Wait()

		// Before we exit fully, we'll attempt to flush out any
		// forwarding events that may still be lingering since the last
		// batch flush.
		if err := s.FlushForwardingEvents(); err != nil {
			log.Errorf("unable to flush forwarding events: %v", err)
		}
	}()

	// TODO(roasbeef): cleared vs settled distinction
	var (
		totalNumUpdates uint64
		totalSatSent    btcutil.Amount
		totalSatRecv    btcutil.Amount
	)
	s.cfg.LogEventTicker.Resume()
	defer s.cfg.LogEventTicker.Stop()

	// Every 15 seconds, we'll flush out the forwarding events that
	// occurred during that period.
	s.cfg.FwdEventTicker.Resume()
	defer s.cfg.FwdEventTicker.Stop()

	defer s.cfg.AckEventTicker.Stop()

out:
	for {

		// If the set of pending settle/fail entries is non-zero,
		// reinstate the ack ticker so we can batch ack them.
		if len(s.pendingSettleFails) > 0 {
			s.cfg.AckEventTicker.Resume()
		}

		select {
		case blockEpoch, ok := <-s.blockEpochStream.Epochs:
			if !ok {
				break out
			}

			atomic.StoreUint32(&s.bestHeight, uint32(blockEpoch.Height))

		// A local close request has arrived, we'll forward this to the
		// relevant link (if it exists) so the channel can be
		// cooperatively closed (if possible).
		case req := <-s.chanCloseRequests:
			chanID := lnwire.NewChanIDFromOutPoint(*req.ChanPoint)

			s.indexMtx.RLock()
			link, ok := s.linkIndex[chanID]
			if !ok {
				s.indexMtx.RUnlock()

				req.Err <- fmt.Errorf("no peer for channel with "+
					"chan_id=%x", chanID[:])
				continue
			}
			s.indexMtx.RUnlock()

			peerPub := link.PeerPubKey()
			log.Debugf("Requesting local channel close: peer=%x, "+
				"chan_id=%x", link.PeerPubKey(), chanID[:])

			go s.cfg.LocalChannelClose(peerPub[:], req)

		case resolutionMsg := <-s.resolutionMsgs:
			// We'll persist the resolution message to the Switch's
			// resolution store.
			resMsg := resolutionMsg.ResolutionMsg
			err := s.resMsgStore.addResolutionMsg(&resMsg)
			if err != nil {
				// This will only fail if there is a database
				// error or a serialization error. Sending the
				// error prevents the contractcourt from being
				// in a state where it believes the send was
				// successful, when it wasn't.
				log.Errorf("unable to add resolution msg: %v",
					err)
				resolutionMsg.errChan <- err
				continue
			}

			// At this point, the resolution message has been
			// persisted. It is safe to signal success by sending
			// a nil error since the Switch will re-deliver the
			// resolution message on restart.
			resolutionMsg.errChan <- nil

			// Create a htlc packet for this resolution. We do
			// not have some of the information that we'll need
			// for blinded error handling here , so we'll rely on
			// our forwarding logic to fill it in later.
			pkt := &htlcPacket{
				outgoingChanID: resolutionMsg.SourceChan,
				outgoingHTLCID: resolutionMsg.HtlcIndex,
				isResolution:   true,
			}

			// Resolution messages will either be cancelling
			// backwards an existing HTLC, or settling a previously
			// outgoing HTLC. Based on this, we'll map the message
			// to the proper htlcPacket.
			if resolutionMsg.Failure != nil {
				pkt.htlc = &lnwire.UpdateFailHTLC{}
			} else {
				pkt.htlc = &lnwire.UpdateFulfillHTLC{
					PaymentPreimage: *resolutionMsg.PreImage,
				}
			}

			log.Debugf("Received outside contract resolution, "+
				"mapping to: %v", lnutils.SpewLogClosure(pkt))

			// We don't check the error, as the only failure we can
			// encounter is due to the circuit already being
			// closed. This is fine, as processing this message is
			// meant to be idempotent.
			err = s.handlePacketForward(pkt)
			if err != nil {
				log.Errorf("Unable to forward resolution msg: %v", err)
			}

		// A new packet has arrived for forwarding, we'll interpret the
		// packet concretely, then either forward it along, or
		// interpret a return packet to a locally initialized one.
		case cmd := <-s.htlcPlex:
			cmd.err <- s.handlePacketForward(cmd.pkt)

		// When this time ticks, then it indicates that we should
		// collect all the forwarding events since the last internal,
		// and write them out to our log.
		case <-s.cfg.FwdEventTicker.Ticks():
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()

				if err := s.FlushForwardingEvents(); err != nil {
					log.Errorf("Unable to flush "+
						"forwarding events: %v", err)
				}
			}()

		// The log ticker has fired, so we'll calculate some forwarding
		// stats for the last 10 seconds to display within the logs to
		// users.
		case <-s.cfg.LogEventTicker.Ticks():
			// First, we'll collate the current running tally of
			// our forwarding stats.
			prevSatSent := totalSatSent
			prevSatRecv := totalSatRecv
			prevNumUpdates := totalNumUpdates

			var (
				newNumUpdates uint64
				newSatSent    btcutil.Amount
				newSatRecv    btcutil.Amount
			)

			// Next, we'll run through all the registered links and
			// compute their up-to-date forwarding stats.
			s.indexMtx.RLock()
			for _, link := range s.linkIndex {
				// TODO(roasbeef): when links first registered
				// stats printed.
				updates, sent, recv := link.Stats()
				newNumUpdates += updates
				newSatSent += sent.ToSatoshis()
				newSatRecv += recv.ToSatoshis()
			}
			s.indexMtx.RUnlock()

			var (
				diffNumUpdates uint64
				diffSatSent    btcutil.Amount
				diffSatRecv    btcutil.Amount
			)

			// If this is the first time we're computing these
			// stats, then the diff is just the new value. We do
			// this in order to avoid integer underflow issues.
			if prevNumUpdates == 0 {
				diffNumUpdates = newNumUpdates
				diffSatSent = newSatSent
				diffSatRecv = newSatRecv
			} else {
				diffNumUpdates = newNumUpdates - prevNumUpdates
				diffSatSent = newSatSent - prevSatSent
				diffSatRecv = newSatRecv - prevSatRecv
			}

			// If the diff of num updates is zero, then we haven't
			// forwarded anything in the last 10 seconds, so we can
			// skip this update.
			if diffNumUpdates == 0 {
				continue
			}

			// If the diff of num updates is negative, then some
			// links may have been unregistered from the switch, so
			// we'll update our stats to only include our registered
			// links.
			if int64(diffNumUpdates) < 0 {
				totalNumUpdates = newNumUpdates
				totalSatSent = newSatSent
				totalSatRecv = newSatRecv
				continue
			}

			// Otherwise, we'll log this diff, then accumulate the
			// new stats into the running total.
			log.Debugf("Sent %d satoshis and received %d satoshis "+
				"in the last 10 seconds (%f tx/sec)",
				diffSatSent, diffSatRecv,
				float64(diffNumUpdates)/10)

			totalNumUpdates += diffNumUpdates
			totalSatSent += diffSatSent
			totalSatRecv += diffSatRecv

		// The ack ticker has fired so if we have any settle/fail entries
		// for a forwarding package to ack, we will do so here in a batch
		// db call.
		case <-s.cfg.AckEventTicker.Ticks():
			// If the current set is empty, pause the ticker.
			if len(s.pendingSettleFails) == 0 {
				s.cfg.AckEventTicker.Pause()
				continue
			}

			// Batch ack the settle/fail entries.
			if err := s.ackSettleFail(s.pendingSettleFails...); err != nil {
				log.Errorf("Unable to ack batch of settle/fails: %v", err)
				continue
			}

			log.Tracef("Acked %d settle fails: %v",
				len(s.pendingSettleFails),
				lnutils.SpewLogClosure(s.pendingSettleFails))

			// Reset the pendingSettleFails buffer while keeping acquired
			// memory.
			s.pendingSettleFails = s.pendingSettleFails[:0]

		case <-s.quit:
			return
		}
	}
}

// Start starts all helper goroutines required for the operation of the switch.
func (s *Switch) Start() error {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		log.Warn("Htlc Switch already started")
		return errors.New("htlc switch already started")
	}

	log.Infof("HTLC Switch starting")

	blockEpochStream, err := s.cfg.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return err
	}
	s.blockEpochStream = blockEpochStream

	s.wg.Add(1)
	go s.htlcForwarder()

	if err := s.reforwardResponses(); err != nil {
		s.Stop()
		log.Errorf("unable to reforward responses: %v", err)
		return err
	}

	if err := s.reforwardResolutions(); err != nil {
		// We are already stopping so we can ignore the error.
		_ = s.Stop()
		log.Errorf("unable to reforward resolutions: %v", err)
		return err
	}

	return nil
}

// reforwardResolutions fetches the set of resolution messages stored on-disk
// and reforwards them if their circuits are still open. If the circuits have
// been deleted, then we will delete the resolution message from the database.
func (s *Switch) reforwardResolutions() error {
	// Fetch all stored resolution messages, deleting the ones that are
	// resolved.
	resMsgs, err := s.resMsgStore.fetchAllResolutionMsg()
	if err != nil {
		return err
	}

	switchPackets := make([]*htlcPacket, 0, len(resMsgs))
	for _, resMsg := range resMsgs {
		// If the open circuit no longer exists, then we can remove the
		// message from the store.
		outKey := CircuitKey{
			ChanID: resMsg.SourceChan,
			HtlcID: resMsg.HtlcIndex,
		}

		if s.circuits.LookupOpenCircuit(outKey) == nil {
			// The open circuit doesn't exist.
			err := s.resMsgStore.deleteResolutionMsg(&outKey)
			if err != nil {
				return err
			}

			continue
		}

		// The circuit is still open, so we can assume that the link or
		// switch (if we are the source) hasn't cleaned it up yet.
		// We rely on our forwarding logic to fill in details that
		// are not currently available to us.
		resPkt := &htlcPacket{
			outgoingChanID: resMsg.SourceChan,
			outgoingHTLCID: resMsg.HtlcIndex,
			isResolution:   true,
		}

		if resMsg.Failure != nil {
			resPkt.htlc = &lnwire.UpdateFailHTLC{}
		} else {
			resPkt.htlc = &lnwire.UpdateFulfillHTLC{
				PaymentPreimage: *resMsg.PreImage,
			}
		}

		switchPackets = append(switchPackets, resPkt)
	}

	// We'll now dispatch the set of resolution messages to the proper
	// destination. An error is only encountered here if the switch is
	// shutting down.
	if err := s.ForwardPackets(nil, switchPackets...); err != nil {
		return err
	}

	return nil
}

// reforwardResponses for every known, non-pending channel, loads all associated
// forwarding packages and reforwards any Settle or Fail HTLCs found. This is
// used to resurrect the switch's mailboxes after a restart. This also runs for
// waiting close channels since there may be settles or fails that need to be
// reforwarded before they completely close.
func (s *Switch) reforwardResponses() error {
	openChannels, err := s.cfg.FetchAllChannels()
	if err != nil {
		return err
	}

	for _, openChannel := range openChannels {
		shortChanID := openChannel.ShortChanID()

		// Locally-initiated payments never need reforwarding.
		if shortChanID == hop.Source {
			continue
		}

		// If the channel is pending, it should have no forwarding
		// packages, and nothing to reforward.
		if openChannel.IsPending {
			continue
		}

		// Channels in open or waiting-close may still have responses in
		// their forwarding packages. We will continue to reattempt
		// forwarding on startup until the channel is fully-closed.
		//
		// Load this channel's forwarding packages, and deliver them to
		// the switch.
		fwdPkgs, err := s.loadChannelFwdPkgs(shortChanID)
		if err != nil {
			log.Errorf("unable to load forwarding "+
				"packages for %v: %v", shortChanID, err)
			return err
		}

		s.reforwardSettleFails(fwdPkgs)
	}

	return nil
}

// loadChannelFwdPkgs loads all forwarding packages owned by the `source` short
// channel identifier.
func (s *Switch) loadChannelFwdPkgs(source lnwire.ShortChannelID) ([]*channeldb.FwdPkg, error) {

	var fwdPkgs []*channeldb.FwdPkg
	if err := kvdb.View(s.cfg.DB, func(tx kvdb.RTx) error {
		var err error
		fwdPkgs, err = s.cfg.SwitchPackager.LoadChannelFwdPkgs(
			tx, source,
		)
		return err
	}, func() {
		fwdPkgs = nil
	}); err != nil {
		return nil, err
	}

	return fwdPkgs, nil
}

// reforwardSettleFails parses the Settle and Fail HTLCs from the list of
// forwarding packages, and reforwards those that have not been acknowledged.
// This is intended to occur on startup, in order to recover the switch's
// mailboxes, and to ensure that responses can be propagated in case the
// outgoing link never comes back online.
//
// NOTE: This should mimic the behavior processRemoteSettleFails.
func (s *Switch) reforwardSettleFails(fwdPkgs []*channeldb.FwdPkg) {
	for _, fwdPkg := range fwdPkgs {
		switchPackets := make([]*htlcPacket, 0, len(fwdPkg.SettleFails))
		for i, update := range fwdPkg.SettleFails {
			// Skip any settles or fails that have already been
			// acknowledged by the incoming link that originated the
			// forwarded Add.
			if fwdPkg.SettleFailFilter.Contains(uint16(i)) {
				continue
			}

			switch msg := update.UpdateMsg.(type) {
			// A settle for an HTLC we previously forwarded HTLC has
			// been received. So we'll forward the HTLC to the
			// switch which will handle propagating the settle to
			// the prior hop.
			case *lnwire.UpdateFulfillHTLC:
				destRef := fwdPkg.DestRef(uint16(i))
				settlePacket := &htlcPacket{
					outgoingChanID: fwdPkg.Source,
					outgoingHTLCID: msg.ID,
					destRef:        &destRef,
					htlc:           msg,
				}

				// Add the packet to the batch to be forwarded, and
				// notify the overflow queue that a spare spot has been
				// freed up within the commitment state.
				switchPackets = append(switchPackets, settlePacket)

			// A failureCode message for a previously forwarded HTLC has been
			// received. As a result a new slot will be freed up in our
			// commitment state, so we'll forward this to the switch so the
			// backwards undo can continue.
			case *lnwire.UpdateFailHTLC:
				// Fetch the reason the HTLC was canceled so
				// we can continue to propagate it. This
				// failure originated from another node, so
				// the linkFailure field is not set on this
				// packet. We rely on the link to fill in
				// additional circuit information for us.
				failPacket := &htlcPacket{
					outgoingChanID: fwdPkg.Source,
					outgoingHTLCID: msg.ID,
					destRef: &channeldb.SettleFailRef{
						Source: fwdPkg.Source,
						Height: fwdPkg.Height,
						Index:  uint16(i),
					},
					htlc: msg,
				}

				// Add the packet to the batch to be forwarded, and
				// notify the overflow queue that a spare spot has been
				// freed up within the commitment state.
				switchPackets = append(switchPackets, failPacket)
			}
		}

		// Since this send isn't tied to a specific link, we pass a nil
		// link quit channel, meaning the send will fail only if the
		// switch receives a shutdown request.
		if err := s.ForwardPackets(nil, switchPackets...); err != nil {
			log.Errorf("Unhandled error while reforwarding packets "+
				"settle/fail over htlcswitch: %v", err)
		}
	}
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
func (s *Switch) Stop() error {
	if !atomic.CompareAndSwapInt32(&s.shutdown, 0, 1) {
		log.Warn("Htlc Switch already stopped")
		return errors.New("htlc switch already shutdown")
	}

	log.Info("HTLC Switch shutting down...")
	defer log.Debug("HTLC Switch shutdown complete")

	close(s.quit)

	s.wg.Wait()

	// Wait until all active goroutines have finished exiting before
	// stopping the mailboxes, otherwise the mailbox map could still be
	// accessed and modified.
	s.mailOrchestrator.Stop()

	return nil
}

// CreateAndAddLink will create a link and then add it to the internal maps
// when given a ChannelLinkConfig and LightningChannel.
func (s *Switch) CreateAndAddLink(linkCfg ChannelLinkConfig,
	lnChan *lnwallet.LightningChannel) error {

	link := NewChannelLink(linkCfg, lnChan)
	return s.AddLink(link)
}

// AddLink is used to initiate the handling of the add link command. The
// request will be propagated and handled in the main goroutine.
func (s *Switch) AddLink(link ChannelLink) error {
	s.indexMtx.Lock()
	defer s.indexMtx.Unlock()

	chanID := link.ChanID()

	// First, ensure that this link is not already active in the switch.
	_, err := s.getLink(chanID)
	if err == nil {
		return fmt.Errorf("unable to add ChannelLink(%v), already "+
			"active", chanID)
	}

	// Get and attach the mailbox for this link, which buffers packets in
	// case there packets that we tried to deliver while this link was
	// offline.
	shortChanID := link.ShortChanID()
	mailbox := s.mailOrchestrator.GetOrCreateMailBox(chanID, shortChanID)
	link.AttachMailBox(mailbox)

	// Attach the Switch's failAliasUpdate function to the link.
	link.attachFailAliasUpdate(s.failAliasUpdate)

	if err := link.Start(); err != nil {
		log.Errorf("AddLink failed to start link with chanID=%v: %v",
			chanID, err)
		s.removeLink(chanID)
		return err
	}

	if shortChanID == hop.Source {
		log.Infof("Adding pending link chan_id=%v, short_chan_id=%v",
			chanID, shortChanID)

		s.pendingLinkIndex[chanID] = link
	} else {
		log.Infof("Adding live link chan_id=%v, short_chan_id=%v",
			chanID, shortChanID)

		s.addLiveLink(link)
		s.mailOrchestrator.BindLiveShortChanID(
			mailbox, chanID, shortChanID,
		)
	}

	return nil
}

// addLiveLink adds a link to all associated forwarding index, this makes it a
// candidate for forwarding HTLCs.
func (s *Switch) addLiveLink(link ChannelLink) {
	linkScid := link.ShortChanID()

	// We'll add the link to the linkIndex which lets us quickly
	// look up a channel when we need to close or register it, and
	// the forwarding index which'll be used when forwarding HTLC's
	// in the multi-hop setting.
	s.linkIndex[link.ChanID()] = link
	s.forwardingIndex[linkScid] = link

	// Next we'll add the link to the interface index so we can
	// quickly look up all the channels for a particular node.
	peerPub := link.PeerPubKey()
	if _, ok := s.interfaceIndex[peerPub]; !ok {
		s.interfaceIndex[peerPub] = make(map[lnwire.ChannelID]ChannelLink)
	}
	s.interfaceIndex[peerPub][link.ChanID()] = link

	s.updateLinkAliases(link)
}

// UpdateLinkAliases is the externally exposed wrapper for updating link
// aliases. It acquires the indexMtx and calls the internal method.
func (s *Switch) UpdateLinkAliases(link ChannelLink) {
	s.indexMtx.Lock()
	defer s.indexMtx.Unlock()

	s.updateLinkAliases(link)
}

// updateLinkAliases updates the aliases for a given link. This will cause the
// htlcswitch to consult the alias manager on the up to date values of its
// alias maps.
//
// NOTE: this MUST be called with the indexMtx held.
func (s *Switch) updateLinkAliases(link ChannelLink) {
	linkScid := link.ShortChanID()

	aliases := link.getAliases()
	if link.isZeroConf() {
		if link.zeroConfConfirmed() {
			// Since the zero-conf channel has confirmed, we can
			// populate the aliasToReal mapping.
			confirmedScid := link.confirmedScid()

			for _, alias := range aliases {
				s.aliasToReal[alias] = confirmedScid
			}

			// Add the confirmed SCID as a key in the baseIndex.
			s.baseIndex[confirmedScid] = linkScid
		}

		// Now we populate the baseIndex which will be used to fetch
		// the link given any of the channel's alias SCIDs or the real
		// SCID. The link's SCID is an alias, so we don't need to
		// special-case it like the option-scid-alias feature-bit case
		// further down.
		for _, alias := range aliases {
			s.baseIndex[alias] = linkScid
		}
	} else if link.negotiatedAliasFeature() {
		// First, we flush any alias mappings for this link's scid
		// before we populate the map again, in order to get rid of old
		// values that no longer exist.
		for alias, real := range s.aliasToReal {
			if real == linkScid {
				delete(s.aliasToReal, alias)
			}
		}

		for alias, real := range s.baseIndex {
			if real == linkScid {
				delete(s.baseIndex, alias)
			}
		}

		// The link's SCID is the confirmed SCID for non-zero-conf
		// option-scid-alias feature bit channels.
		for _, alias := range aliases {
			s.aliasToReal[alias] = linkScid
			s.baseIndex[alias] = linkScid
		}

		// Since the link's SCID is confirmed, it was not included in
		// the baseIndex above as a key. Add it now.
		s.baseIndex[linkScid] = linkScid
	}
}

// GetLink is used to initiate the handling of the get link command. The
// request will be propagated/handled to/in the main goroutine.
func (s *Switch) GetLink(chanID lnwire.ChannelID) (ChannelUpdateHandler,
	error) {

	s.indexMtx.RLock()
	defer s.indexMtx.RUnlock()

	return s.getLink(chanID)
}

// getLink returns the link stored in either the pending index or the live
// lindex.
func (s *Switch) getLink(chanID lnwire.ChannelID) (ChannelLink, error) {
	link, ok := s.linkIndex[chanID]
	if !ok {
		link, ok = s.pendingLinkIndex[chanID]
		if !ok {
			return nil, ErrChannelLinkNotFound
		}
	}

	return link, nil
}

// GetLinkByShortID attempts to return the link which possesses the target short
// channel ID.
func (s *Switch) GetLinkByShortID(chanID lnwire.ShortChannelID) (ChannelLink,
	error) {

	s.indexMtx.RLock()
	defer s.indexMtx.RUnlock()

	link, err := s.getLinkByShortID(chanID)
	if err != nil {
		// If we failed to find the link under the passed-in SCID, we
		// consult the Switch's baseIndex map to see if the confirmed
		// SCID was used for a zero-conf channel.
		aliasID, ok := s.baseIndex[chanID]
		if !ok {
			return nil, err
		}

		// An alias was found, use it to lookup if a link exists.
		return s.getLinkByShortID(aliasID)
	}

	return link, nil
}

// getLinkByShortID attempts to return the link which possesses the target
// short channel ID.
//
// NOTE: This MUST be called with the indexMtx held.
func (s *Switch) getLinkByShortID(chanID lnwire.ShortChannelID) (ChannelLink, error) {
	link, ok := s.forwardingIndex[chanID]
	if !ok {
		log.Debugf("Link not found in forwarding index using "+
			"chanID=%v", chanID)

		return nil, ErrChannelLinkNotFound
	}

	return link, nil
}

// getLinkByMapping attempts to fetch the link via the htlcPacket's
// outgoingChanID, possibly using a mapping. If it finds the link via mapping,
// the outgoingChanID will be changed so that an error can be properly
// attributed when looping over linkErrs in handlePacketForward.
//
// * If the outgoingChanID is an alias, we'll fetch the link regardless if it's
// public or not.
//
// * If the outgoingChanID is a confirmed SCID, we'll need to do more checks.
//   - If there is no entry found in baseIndex, fetch the link. This channel
//     did not have the option-scid-alias feature negotiated (which includes
//     zero-conf and option-scid-alias channel-types).
//   - If there is an entry found, fetch the link from forwardingIndex and
//     fail if this is a private link.
//
// NOTE: This MUST be called with the indexMtx read lock held.
func (s *Switch) getLinkByMapping(pkt *htlcPacket) (ChannelLink, error) {
	// Determine if this ShortChannelID is an alias or a confirmed SCID.
	chanID := pkt.outgoingChanID
	aliasID := s.cfg.IsAlias(chanID)

	log.Debugf("Querying outgoing link using chanID=%v, aliasID=%v", chanID,
		aliasID)

	// Set the originalOutgoingChanID so the proper channel_update can be
	// sent back if the option-scid-alias feature bit was negotiated.
	pkt.originalOutgoingChanID = chanID

	if aliasID {
		// Since outgoingChanID is an alias, we'll fetch the link via
		// baseIndex.
		baseScid, ok := s.baseIndex[chanID]
		if !ok {
			// No mapping exists, bail.
			return nil, ErrChannelLinkNotFound
		}

		// A mapping exists, so use baseScid to find the link in the
		// forwardingIndex.
		link, ok := s.forwardingIndex[baseScid]
		if !ok {
			log.Debugf("Forwarding index not found using "+
				"baseScid=%v", baseScid)

			// Link not found, bail.
			return nil, ErrChannelLinkNotFound
		}

		// Change the packet's outgoingChanID field so that errors are
		// properly attributed.
		pkt.outgoingChanID = baseScid

		// Return the link without checking if it's private or not.
		return link, nil
	}

	// The outgoingChanID is a confirmed SCID. Attempt to fetch the base
	// SCID from baseIndex.
	baseScid, ok := s.baseIndex[chanID]
	if !ok {
		// outgoingChanID is not a key in base index meaning this
		// channel did not have the option-scid-alias feature bit
		// negotiated. We'll fetch the link and return it.
		link, ok := s.forwardingIndex[chanID]
		if !ok {
			log.Debugf("Forwarding index not found using "+
				"chanID=%v", chanID)

			// The link wasn't found, bail out.
			return nil, ErrChannelLinkNotFound
		}

		return link, nil
	}

	// Fetch the link whose internal SCID is baseScid.
	link, ok := s.forwardingIndex[baseScid]
	if !ok {
		log.Debugf("Forwarding index not found using baseScid=%v",
			baseScid)

		// Link wasn't found, bail out.
		return nil, ErrChannelLinkNotFound
	}

	// If the link is unadvertised, we fail since the real SCID was used to
	// forward over it and this is a channel where the option-scid-alias
	// feature bit was negotiated.
	if link.IsUnadvertised() {
		log.Debugf("Link is unadvertised, chanID=%v, baseScid=%v",
			chanID, baseScid)

		return nil, ErrChannelLinkNotFound
	}

	// The link is public so the confirmed SCID can be used to forward over
	// it. We'll also replace pkt's outgoingChanID field so errors can
	// properly be attributed in the calling function.
	pkt.outgoingChanID = baseScid
	return link, nil
}

// HasActiveLink returns true if the given channel ID has a link in the link
// index AND the link is eligible to forward.
func (s *Switch) HasActiveLink(chanID lnwire.ChannelID) bool {
	s.indexMtx.RLock()
	defer s.indexMtx.RUnlock()

	if link, ok := s.linkIndex[chanID]; ok {
		return link.EligibleToForward()
	}

	return false
}

// RemoveLink purges the switch of any link associated with chanID. If a pending
// or active link is not found, this method does nothing. Otherwise, the method
// returns after the link has been completely shutdown.
func (s *Switch) RemoveLink(chanID lnwire.ChannelID) {
	s.indexMtx.Lock()
	link, err := s.getLink(chanID)
	if err != nil {
		// If err is non-nil, this means that link is also nil. The
		// link variable cannot be nil without err being non-nil.
		s.indexMtx.Unlock()
		log.Tracef("Unable to remove link for ChannelID(%v): %v",
			chanID, err)
		return
	}

	// Check if the link is already stopping and grab the stop chan if it
	// is.
	stopChan, ok := s.linkStopIndex[chanID]
	if !ok {
		// If the link is non-nil, it is not currently stopping, so
		// we'll add a stop chan to the linkStopIndex.
		stopChan = make(chan struct{})
		s.linkStopIndex[chanID] = stopChan
	}
	s.indexMtx.Unlock()

	if ok {
		// If the stop chan exists, we will wait for it to be closed.
		// Once it is closed, we will exit.
		select {
		case <-stopChan:
			return
		case <-s.quit:
			return
		}
	}

	// Stop the link before removing it from the maps.
	link.Stop()

	s.indexMtx.Lock()
	_ = s.removeLink(chanID)

	// Close stopChan and remove this link from the linkStopIndex.
	// Deleting from the index and removing from the link must be done
	// in the same block while the mutex is held.
	close(stopChan)
	delete(s.linkStopIndex, chanID)
	s.indexMtx.Unlock()
}

// removeLink is used to remove and stop the channel link.
//
// NOTE: This MUST be called with the indexMtx held.
func (s *Switch) removeLink(chanID lnwire.ChannelID) ChannelLink {
	log.Infof("Removing channel link with ChannelID(%v)", chanID)

	link, err := s.getLink(chanID)
	if err != nil {
		return nil
	}

	// Remove the channel from live link indexes.
	delete(s.pendingLinkIndex, link.ChanID())
	delete(s.linkIndex, link.ChanID())
	delete(s.forwardingIndex, link.ShortChanID())

	// If the link has been added to the peer index, then we'll move to
	// delete the entry within the index.
	peerPub := link.PeerPubKey()
	if peerIndex, ok := s.interfaceIndex[peerPub]; ok {
		delete(peerIndex, link.ChanID())

		// If after deletion, there are no longer any links, then we'll
		// remove the interface map all together.
		if len(peerIndex) == 0 {
			delete(s.interfaceIndex, peerPub)
		}
	}

	return link
}

// UpdateShortChanID locates the link with the passed-in chanID and updates the
// underlying channel state. This is only used in zero-conf channels to allow
// the confirmed SCID to be updated.
func (s *Switch) UpdateShortChanID(chanID lnwire.ChannelID) error {
	s.indexMtx.Lock()
	defer s.indexMtx.Unlock()

	// Locate the target link in the link index. If no such link exists,
	// then we will ignore the request.
	link, ok := s.linkIndex[chanID]
	if !ok {
		return fmt.Errorf("link %v not found", chanID)
	}

	// Try to update the link's underlying channel state, returning early
	// if this update failed.
	_, err := link.UpdateShortChanID()
	if err != nil {
		return err
	}

	// Since the zero-conf channel is confirmed, we should populate the
	// aliasToReal map and update the baseIndex.
	aliases := link.getAliases()

	confirmedScid := link.confirmedScid()

	for _, alias := range aliases {
		s.aliasToReal[alias] = confirmedScid
	}

	s.baseIndex[confirmedScid] = link.ShortChanID()

	return nil
}

// GetLinksByInterface fetches all the links connected to a particular node
// identified by the serialized compressed form of its public key.
func (s *Switch) GetLinksByInterface(hop [33]byte) ([]ChannelUpdateHandler,
	error) {

	s.indexMtx.RLock()
	defer s.indexMtx.RUnlock()

	var handlers []ChannelUpdateHandler

	links, err := s.getLinks(hop)
	if err != nil {
		return nil, err
	}

	// Range over the returned []ChannelLink to convert them into
	// []ChannelUpdateHandler.
	for _, link := range links {
		handlers = append(handlers, link)
	}

	return handlers, nil
}

// getLinks is function which returns the channel links of the peer by hop
// destination id.
//
// NOTE: This MUST be called with the indexMtx held.
func (s *Switch) getLinks(destination [33]byte) ([]ChannelLink, error) {
	links, ok := s.interfaceIndex[destination]
	if !ok {
		return nil, ErrNoLinksFound
	}

	channelLinks := make([]ChannelLink, 0, len(links))
	for _, link := range links {
		channelLinks = append(channelLinks, link)
	}

	return channelLinks, nil
}

// CircuitModifier returns a reference to subset of the interfaces provided by
// the circuit map, to allow links to open and close circuits.
func (s *Switch) CircuitModifier() CircuitModifier {
	return s.circuits
}

// CircuitLookup returns a reference to subset of the interfaces provided by the
// circuit map, to allow looking up circuits.
func (s *Switch) CircuitLookup() CircuitLookup {
	return s.circuits
}

// commitCircuits persistently adds a circuit to the switch's circuit map.
func (s *Switch) commitCircuits(circuits ...*PaymentCircuit) (
	*CircuitFwdActions, error) {

	return s.circuits.CommitCircuits(circuits...)
}

// FlushForwardingEvents flushes out the set of pending forwarding events to
// the persistent log. This will be used by the switch to periodically flush
// out the set of forwarding events to disk. External callers can also use this
// method to ensure all data is flushed to dis before querying the log.
func (s *Switch) FlushForwardingEvents() error {
	// First, we'll obtain a copy of the current set of pending forwarding
	// events.
	s.fwdEventMtx.Lock()

	// If we won't have any forwarding events, then we can exit early.
	if len(s.pendingFwdingEvents) == 0 {
		s.fwdEventMtx.Unlock()
		return nil
	}

	events := make([]channeldb.ForwardingEvent, len(s.pendingFwdingEvents))
	copy(events[:], s.pendingFwdingEvents[:])

	// With the copy obtained, we can now clear out the header pointer of
	// the current slice. This way, we can re-use the underlying storage
	// allocated for the slice.
	s.pendingFwdingEvents = s.pendingFwdingEvents[:0]
	s.fwdEventMtx.Unlock()

	// Finally, we'll write out the copied events to the persistent
	// forwarding log.
	return s.cfg.FwdingLog.AddForwardingEvents(events)
}

// BestHeight returns the best height known to the switch.
func (s *Switch) BestHeight() uint32 {
	return atomic.LoadUint32(&s.bestHeight)
}

// dustExceedsFeeThreshold takes in a ChannelLink, HTLC amount, and a boolean
// to determine whether the default fee threshold has been exceeded. This
// heuristic takes into account the trimmed-to-dust mechanism. The sum of the
// commitment's dust with the mailbox's dust with the amount is checked against
// the fee exposure threshold. If incoming is true, then the amount is not
// included in the sum as it was already included in the commitment's dust. A
// boolean is returned telling the caller whether the HTLC should be failed
// back.
func (s *Switch) dustExceedsFeeThreshold(link ChannelLink,
	amount lnwire.MilliSatoshi, incoming bool) bool {

	// Retrieve the link's current commitment feerate and dustClosure.
	feeRate := link.getFeeRate()
	isDust := link.getDustClosure()

	// Evaluate if the HTLC is dust on either sides' commitment.
	isLocalDust := isDust(
		feeRate, incoming, lntypes.Local, amount.ToSatoshis(),
	)
	isRemoteDust := isDust(
		feeRate, incoming, lntypes.Remote, amount.ToSatoshis(),
	)

	if !(isLocalDust || isRemoteDust) {
		// If the HTLC is not dust on either commitment, it's fine to
		// forward.
		return false
	}

	// Fetch the dust sums currently in the mailbox for this link.
	cid := link.ChanID()
	sid := link.ShortChanID()
	mailbox := s.mailOrchestrator.GetOrCreateMailBox(cid, sid)
	localMailDust, remoteMailDust := mailbox.DustPackets()

	// If the htlc is dust on the local commitment, we'll obtain the dust
	// sum for it.
	if isLocalDust {
		localSum := link.getDustSum(
			lntypes.Local, fn.None[chainfee.SatPerKWeight](),
		)
		localSum += localMailDust

		// Optionally include the HTLC amount only for outgoing
		// HTLCs.
		if !incoming {
			localSum += amount
		}

		// Finally check against the defined fee threshold.
		if localSum > s.cfg.MaxFeeExposure {
			return true
		}
	}

	// Also check if the htlc is dust on the remote commitment, if we've
	// reached this point.
	if isRemoteDust {
		remoteSum := link.getDustSum(
			lntypes.Remote, fn.None[chainfee.SatPerKWeight](),
		)
		remoteSum += remoteMailDust

		// Optionally include the HTLC amount only for outgoing
		// HTLCs.
		if !incoming {
			remoteSum += amount
		}

		// Finally check against the defined fee threshold.
		if remoteSum > s.cfg.MaxFeeExposure {
			return true
		}
	}

	// If we reached this point, this HTLC is fine to forward.
	return false
}

// failMailboxUpdate is passed to the mailbox orchestrator which in turn passes
// it to individual mailboxes. It allows the mailboxes to construct a
// FailureMessage when failing back HTLC's due to expiry and may include an
// alias in the ShortChannelID field. The outgoingScid is the SCID originally
// used in the onion. The mailboxScid is the SCID that the mailbox and link
// use. The mailboxScid is only used in the non-alias case, so it is always
// the confirmed SCID.
func (s *Switch) failMailboxUpdate(outgoingScid,
	mailboxScid lnwire.ShortChannelID) lnwire.FailureMessage {

	// Try to use the failAliasUpdate function in case this is a channel
	// that uses aliases. If it returns nil, we'll fallback to the original
	// pre-alias behavior.
	update := s.failAliasUpdate(outgoingScid, false)
	if update == nil {
		// Execute the fallback behavior.
		var err error
		update, err = s.cfg.FetchLastChannelUpdate(mailboxScid)
		if err != nil {
			return &lnwire.FailTemporaryNodeFailure{}
		}
	}

	return lnwire.NewTemporaryChannelFailure(update)
}

// failAliasUpdate prepares a ChannelUpdate for a failed incoming or outgoing
// HTLC on a channel where the option-scid-alias feature bit was negotiated. If
// the associated channel is not one of these, this function will return nil
// and the caller is expected to handle this properly. In this case, a return
// to the original non-alias behavior is expected.
func (s *Switch) failAliasUpdate(scid lnwire.ShortChannelID,
	incoming bool) *lnwire.ChannelUpdate1 {

	// This function does not defer the unlocking because of the database
	// lookups for ChannelUpdate.
	s.indexMtx.RLock()

	if s.cfg.IsAlias(scid) {
		// The alias SCID was used. In the incoming case this means
		// the channel is zero-conf as the link sets the scid. In the
		// outgoing case, the sender set the scid to use and may be
		// either the alias or the confirmed one, if it exists.
		realScid, ok := s.aliasToReal[scid]
		if !ok {
			// The real, confirmed SCID does not exist yet. Find
			// the "base" SCID that the link uses via the
			// baseIndex. If we can't find it, return nil. This
			// means the channel is zero-conf.
			baseScid, ok := s.baseIndex[scid]
			s.indexMtx.RUnlock()
			if !ok {
				return nil
			}

			update, err := s.cfg.FetchLastChannelUpdate(baseScid)
			if err != nil {
				return nil
			}

			// Replace the baseScid with the passed-in alias.
			update.ShortChannelID = scid
			sig, err := s.cfg.SignAliasUpdate(update)
			if err != nil {
				return nil
			}

			update.Signature, err = lnwire.NewSigFromSignature(sig)
			if err != nil {
				return nil
			}

			return update
		}

		s.indexMtx.RUnlock()

		// Fetch the SCID via the confirmed SCID and replace it with
		// the alias.
		update, err := s.cfg.FetchLastChannelUpdate(realScid)
		if err != nil {
			return nil
		}

		// In the incoming case, we want to ensure that we don't leak
		// the UTXO in case the channel is private. In the outgoing
		// case, since the alias was used, we do the same thing.
		update.ShortChannelID = scid
		sig, err := s.cfg.SignAliasUpdate(update)
		if err != nil {
			return nil
		}

		update.Signature, err = lnwire.NewSigFromSignature(sig)
		if err != nil {
			return nil
		}

		return update
	}

	// If the confirmed SCID is not in baseIndex, this is not an
	// option-scid-alias or zero-conf channel.
	baseScid, ok := s.baseIndex[scid]
	if !ok {
		s.indexMtx.RUnlock()
		return nil
	}

	// Fetch the link so we can get an alias to use in the ShortChannelID
	// of the ChannelUpdate.
	link, ok := s.forwardingIndex[baseScid]
	s.indexMtx.RUnlock()
	if !ok {
		// This should never happen, but if it does for some reason,
		// fallback to the old behavior.
		return nil
	}

	aliases := link.getAliases()
	if len(aliases) == 0 {
		// This should never happen, but if it does, fallback.
		return nil
	}

	// Fetch the ChannelUpdate via the real, confirmed SCID.
	update, err := s.cfg.FetchLastChannelUpdate(scid)
	if err != nil {
		return nil
	}

	// The incoming case will replace the ShortChannelID in the retrieved
	// ChannelUpdate with the alias to ensure no privacy leak occurs. This
	// would happen if a private non-zero-conf option-scid-alias
	// feature-bit channel leaked its UTXO here rather than supplying an
	// alias. In the outgoing case, the confirmed SCID was actually used
	// for forwarding in the onion, so no replacement is necessary as the
	// sender knows the scid.
	if incoming {
		// We will replace and sign the update with the first alias.
		// Since this happens on the incoming side, it's not actually
		// possible to know what the sender used in the onion.
		update.ShortChannelID = aliases[0]
		sig, err := s.cfg.SignAliasUpdate(update)
		if err != nil {
			return nil
		}

		update.Signature, err = lnwire.NewSigFromSignature(sig)
		if err != nil {
			return nil
		}
	}

	return update
}

// AddAliasForLink instructs the Switch to update its in-memory maps to reflect
// that a link has a new alias.
func (s *Switch) AddAliasForLink(chanID lnwire.ChannelID,
	alias lnwire.ShortChannelID) error {

	// Fetch the link so that we can update the underlying channel's set of
	// aliases.
	s.indexMtx.RLock()
	link, err := s.getLink(chanID)
	s.indexMtx.RUnlock()
	if err != nil {
		return err
	}

	// If the link is a channel where the option-scid-alias feature bit was
	// not negotiated, we'll return an error.
	if !link.negotiatedAliasFeature() {
		return fmt.Errorf("attempted to update non-alias channel")
	}

	linkScid := link.ShortChanID()

	// We'll update the maps so the Switch includes this alias in its
	// forwarding decisions.
	if link.isZeroConf() {
		if link.zeroConfConfirmed() {
			// If the channel has confirmed on-chain, we'll
			// add this alias to the aliasToReal map.
			confirmedScid := link.confirmedScid()

			s.aliasToReal[alias] = confirmedScid
		}

		// Add this alias to the baseIndex mapping.
		s.baseIndex[alias] = linkScid
	} else if link.negotiatedAliasFeature() {
		// The channel is confirmed, so we'll populate the aliasToReal
		// and baseIndex maps.
		s.aliasToReal[alias] = linkScid
		s.baseIndex[alias] = linkScid
	}

	return nil
}

// handlePacketAdd handles forwarding an Add packet.
func (s *Switch) handlePacketAdd(packet *htlcPacket,
	htlc *lnwire.UpdateAddHTLC) error {

	// Check if the node is set to reject all onward HTLCs and also make
	// sure that HTLC is not from the source node.
	if s.cfg.RejectHTLC {
		failure := NewDetailedLinkError(
			&lnwire.FailChannelDisabled{},
			OutgoingFailureForwardsDisabled,
		)

		return s.failAddPacket(packet, failure)
	}

	// Before we attempt to find a non-strict forwarding path for this
	// htlc, check whether the htlc is being routed over the same incoming
	// and outgoing channel. If our node does not allow forwards of this
	// nature, we fail the htlc early. This check is in place to disallow
	// inefficiently routed htlcs from locking up our balance. With
	// channels where the option-scid-alias feature was negotiated, we also
	// have to be sure that the IDs aren't the same since one or both could
	// be an alias.
	linkErr := s.checkCircularForward(
		packet.incomingChanID, packet.outgoingChanID,
		s.cfg.AllowCircularRoute, htlc.PaymentHash,
	)
	if linkErr != nil {
		return s.failAddPacket(packet, linkErr)
	}

	s.indexMtx.RLock()
	targetLink, err := s.getLinkByMapping(packet)
	if err != nil {
		s.indexMtx.RUnlock()

		log.Debugf("unable to find link with "+
			"destination %v", packet.outgoingChanID)

		// If packet was forwarded from another channel link than we
		// should notify this link that some error occurred.
		linkError := NewLinkError(
			&lnwire.FailUnknownNextPeer{},
		)

		return s.failAddPacket(packet, linkError)
	}
	targetPeerKey := targetLink.PeerPubKey()
	interfaceLinks, _ := s.getLinks(targetPeerKey)
	s.indexMtx.RUnlock()

	// We'll keep track of any HTLC failures during the link selection
	// process. This way we can return the error for precise link that the
	// sender selected, while optimistically trying all links to utilize
	// our available bandwidth.
	linkErrs := make(map[lnwire.ShortChannelID]*LinkError)

	// Find all destination channel links with appropriate bandwidth.
	var destinations []ChannelLink
	for _, link := range interfaceLinks {
		var failure *LinkError

		// We'll skip any links that aren't yet eligible for
		// forwarding.
		if !link.EligibleToForward() {
			failure = NewDetailedLinkError(
				&lnwire.FailUnknownNextPeer{},
				OutgoingFailureLinkNotEligible,
			)
		} else {
			// We'll ensure that the HTLC satisfies the current
			// forwarding conditions of this target link.
			currentHeight := atomic.LoadUint32(&s.bestHeight)
			failure = link.CheckHtlcForward(
				htlc.PaymentHash, packet.incomingAmount,
				packet.amount, packet.incomingTimeout,
				packet.outgoingTimeout, packet.inboundFee,
				currentHeight, packet.originalOutgoingChanID,
				htlc.CustomRecords,
			)
		}

		// If this link can forward the htlc, add it to the set of
		// destinations.
		if failure == nil {
			destinations = append(destinations, link)
			continue
		}

		linkErrs[link.ShortChanID()] = failure
	}

	// If we had a forwarding failure due to the HTLC not satisfying the
	// current policy, then we'll send back an error, but ensure we send
	// back the error sourced at the *target* link.
	if len(destinations) == 0 {
		// At this point, some or all of the links rejected the HTLC so
		// we couldn't forward it. So we'll try to look up the error
		// that came from the source.
		linkErr, ok := linkErrs[packet.outgoingChanID]
		if !ok {
			// If we can't find the error of the source, then we'll
			// return an unknown next peer, though this should
			// never happen.
			linkErr = NewLinkError(
				&lnwire.FailUnknownNextPeer{},
			)
			log.Warnf("unable to find err source for "+
				"outgoing_link=%v, errors=%v",
				packet.outgoingChanID,
				lnutils.SpewLogClosure(linkErrs))
		}

		log.Tracef("incoming HTLC(%x) violated "+
			"target outgoing link (id=%v) policy: %v",
			htlc.PaymentHash[:], packet.outgoingChanID,
			linkErr)

		return s.failAddPacket(packet, linkErr)
	}

	// Choose a random link out of the set of links that can forward this
	// htlc. The reason for randomization is to evenly distribute the htlc
	// load without making assumptions about what the best channel is.
	//nolint:gosec
	destination := destinations[rand.Intn(len(destinations))]

	// Retrieve the incoming link by its ShortChannelID. Note that the
	// incomingChanID is never set to hop.Source here.
	s.indexMtx.RLock()
	incomingLink, err := s.getLinkByShortID(packet.incomingChanID)
	s.indexMtx.RUnlock()
	if err != nil {
		// If we couldn't find the incoming link, we can't evaluate the
		// incoming's exposure to dust, so we just fail the HTLC back.
		linkErr := NewLinkError(
			&lnwire.FailTemporaryChannelFailure{},
		)

		return s.failAddPacket(packet, linkErr)
	}

	// Evaluate whether this HTLC would increase our fee exposure over the
	// threshold on the incoming link. If it does, fail it backwards.
	if s.dustExceedsFeeThreshold(
		incomingLink, packet.incomingAmount, true,
	) {
		// The incoming dust exceeds the threshold, so we fail the add
		// back.
		linkErr := NewLinkError(
			&lnwire.FailTemporaryChannelFailure{},
		)

		return s.failAddPacket(packet, linkErr)
	}

	// Also evaluate whether this HTLC would increase our fee exposure over
	// the threshold on the destination link. If it does, fail it back.
	if s.dustExceedsFeeThreshold(
		destination, packet.amount, false,
	) {
		// The outgoing dust exceeds the threshold, so we fail the add
		// back.
		linkErr := NewLinkError(
			&lnwire.FailTemporaryChannelFailure{},
		)

		return s.failAddPacket(packet, linkErr)
	}

	// Send the packet to the destination channel link which manages the
	// channel.
	packet.outgoingChanID = destination.ShortChanID()

	return destination.handleSwitchPacket(packet)
}

// handlePacketSettle handles forwarding a settle packet.
func (s *Switch) handlePacketSettle(packet *htlcPacket) error {
	// If the source of this packet has not been set, use the circuit map
	// to lookup the origin.
	circuit, err := s.closeCircuit(packet)

	// If the circuit is in the process of closing, we will return a nil as
	// there's another packet handling undergoing.
	if errors.Is(err, ErrCircuitClosing) {
		log.Debugf("Circuit is closing for packet=%v", packet)
		return nil
	}

	// Exit early if there's another error.
	if err != nil {
		return err
	}

	// closeCircuit returns a nil circuit when a settle packet returns an
	// ErrUnknownCircuit error upon the inner call to CloseCircuit.
	//
	// NOTE: We can only get a nil circuit when it has already been deleted
	// and when `UpdateFulfillHTLC` is received. After which `RevokeAndAck`
	// is received, which invokes `processRemoteSettleFails` in its link.
	if circuit == nil {
		log.Debugf("Circuit already closed for packet=%v", packet)
		return nil
	}

	localHTLC := packet.incomingChanID == hop.Source

	// If this is a locally initiated HTLC, we need to handle the packet by
	// storing the network result.
	//
	// A blank IncomingChanID in a circuit indicates that it is a pending
	// user-initiated payment.
	//
	// NOTE: `closeCircuit` modifies the state of `packet`.
	if localHTLC {
		// TODO(yy): remove the goroutine and send back the error here.
		s.wg.Add(1)
		go s.handleLocalResponse(packet)

		// If this is a locally initiated HTLC, there's no need to
		// forward it so we exit.
		return nil
	}

	// If this is an HTLC settle, and it wasn't from a locally initiated
	// HTLC, then we'll log a forwarding event so we can flush it to disk
	// later.
	if circuit.Outgoing != nil {
		log.Infof("Forwarded HTLC(%x) of %v (fee: %v) "+
			"from IncomingChanID(%v) to OutgoingChanID(%v)",
			circuit.PaymentHash[:], circuit.OutgoingAmount,
			circuit.IncomingAmount-circuit.OutgoingAmount,
			circuit.Incoming.ChanID, circuit.Outgoing.ChanID)

		s.fwdEventMtx.Lock()
		s.pendingFwdingEvents = append(
			s.pendingFwdingEvents,
			channeldb.ForwardingEvent{
				Timestamp:      time.Now(),
				IncomingChanID: circuit.Incoming.ChanID,
				OutgoingChanID: circuit.Outgoing.ChanID,
				AmtIn:          circuit.IncomingAmount,
				AmtOut:         circuit.OutgoingAmount,
				IncomingHtlcID: fn.Some(
					circuit.Incoming.HtlcID,
				),
				OutgoingHtlcID: fn.Some(
					circuit.Outgoing.HtlcID,
				),
			},
		)
		s.fwdEventMtx.Unlock()
	}

	// Deliver this packet.
	return s.mailOrchestrator.Deliver(packet.incomingChanID, packet)
}

// handlePacketFail handles forwarding a fail packet.
func (s *Switch) handlePacketFail(packet *htlcPacket,
	htlc *lnwire.UpdateFailHTLC) error {

	// If the source of this packet has not been set, use the circuit map
	// to lookup the origin.
	circuit, err := s.closeCircuit(packet)
	if err != nil {
		return err
	}

	// If this is a locally initiated HTLC, we need to handle the packet by
	// storing the network result.
	//
	// A blank IncomingChanID in a circuit indicates that it is a pending
	// user-initiated payment.
	//
	// NOTE: `closeCircuit` modifies the state of `packet`.
	if packet.incomingChanID == hop.Source {
		// TODO(yy): remove the goroutine and send back the error here.
		s.wg.Add(1)
		go s.handleLocalResponse(packet)

		// If this is a locally initiated HTLC, there's no need to
		// forward it so we exit.
		return nil
	}

	// Exit early if this hasSource is true. This flag is only set via
	// mailbox's `FailAdd`. This method has two callsites,
	// - the packet has timed out after `MailboxDeliveryTimeout`, defaults
	//   to 1 min.
	// - the HTLC fails the validation in `channel.AddHTLC`.
	// In either case, the `Reason` field is populated. Thus there's no
	// need to proceed and extract the failure reason below.
	if packet.hasSource {
		// Deliver this packet.
		return s.mailOrchestrator.Deliver(packet.incomingChanID, packet)
	}

	// HTLC resolutions and messages restored from disk don't have the
	// obfuscator set from the original htlc add packet - set it here for
	// use in blinded errors.
	packet.obfuscator = circuit.ErrorEncrypter

	switch {
	// No message to encrypt, locally sourced payment.
	case circuit.ErrorEncrypter == nil:
		// TODO(yy) further check this case as we shouldn't end up here
		// as `isLocal` is already false.

	// If this is a resolution message, then we'll need to encrypt it as
	// it's actually internally sourced.
	case packet.isResolution:
		var err error
		// TODO(roasbeef): don't need to pass actually?
		failure := &lnwire.FailPermanentChannelFailure{}
		htlc.Reason, err = circuit.ErrorEncrypter.EncryptFirstHop(
			failure,
		)
		if err != nil {
			err = fmt.Errorf("unable to obfuscate error: %w", err)
			log.Error(err)
		}

	// Alternatively, if the remote party sends us an
	// UpdateFailMalformedHTLC, then we'll need to convert this into a
	// proper well formatted onion error as there's no HMAC currently.
	case packet.convertedError:
		log.Infof("Converting malformed HTLC error for circuit for "+
			"Circuit(%x: (%s, %d) <-> (%s, %d))",
			packet.circuit.PaymentHash,
			packet.incomingChanID, packet.incomingHTLCID,
			packet.outgoingChanID, packet.outgoingHTLCID)

		htlc.Reason = circuit.ErrorEncrypter.EncryptMalformedError(
			htlc.Reason,
		)

	default:
		// Otherwise, it's a forwarded error, so we'll perform a
		// wrapper encryption as normal.
		htlc.Reason = circuit.ErrorEncrypter.IntermediateEncrypt(
			htlc.Reason,
		)
	}

	// Deliver this packet.
	return s.mailOrchestrator.Deliver(packet.incomingChanID, packet)
}

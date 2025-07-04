package htlcswitch

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrFwdNotExists is an error returned when the caller tries to resolve
	// a forward that doesn't exist anymore.
	ErrFwdNotExists = errors.New("forward does not exist")

	// ErrUnsupportedFailureCode when processing of an unsupported failure
	// code is attempted.
	ErrUnsupportedFailureCode = errors.New("unsupported failure code")

	errBlockStreamStopped = errors.New("block epoch stream stopped")
)

// InterceptableSwitch is an implementation of ForwardingSwitch interface.
// This implementation is used like a proxy that wraps the switch and
// intercepts forward requests. A reference to the Switch is held in order
// to communicate back the interception result where the options are:
// Resume - forwards the original request to the switch as is.
// Settle - routes UpdateFulfillHTLC to the originating link.
// Fail - routes UpdateFailHTLC to the originating link.
type InterceptableSwitch struct {
	started atomic.Bool
	stopped atomic.Bool

	// htlcSwitch is the underline switch
	htlcSwitch *Switch

	// intercepted is where we stream all intercepted packets coming from
	// the switch.
	intercepted chan *interceptedPackets

	// resolutionChan is where we stream all responses coming from the
	// interceptor client.
	resolutionChan chan *fwdResolution

	onchainIntercepted chan InterceptedForward

	// interceptorRegistration is a channel that we use to synchronize
	// client connect and disconnect.
	interceptorRegistration chan ForwardInterceptor

	// requireInterceptor indicates whether processing should block if no
	// interceptor is connected.
	requireInterceptor bool

	// interceptor is the handler for intercepted packets.
	interceptor ForwardInterceptor

	// heldHtlcSet keeps track of outstanding intercepted forwards.
	heldHtlcSet *heldHtlcSet

	// cltvRejectDelta defines the number of blocks before the expiry of the
	// htlc where we no longer intercept it and instead cancel it back.
	cltvRejectDelta uint32

	// cltvInterceptDelta defines the number of blocks before the expiry of
	// the htlc where we don't intercept anymore. This value must be greater
	// than CltvRejectDelta, because we don't want to offer htlcs to the
	// interceptor client for which there is no time left to resolve them
	// anymore.
	cltvInterceptDelta uint32

	// notifier is an instance of a chain notifier that we'll use to signal
	// the switch when a new block has arrived.
	notifier chainntnfs.ChainNotifier

	// blockEpochStream is an active block epoch event stream backed by an
	// active ChainNotifier instance. This will be used to retrieve the
	// latest height of the chain.
	blockEpochStream *chainntnfs.BlockEpochEvent

	// currentHeight is the currently best known height.
	currentHeight int32

	wg   sync.WaitGroup
	quit chan struct{}
}

type interceptedPackets struct {
	packets  []*htlcPacket
	linkQuit <-chan struct{}
	isReplay bool
}

// FwdAction defines the various resolution types.
type FwdAction int

const (
	// FwdActionResume forwards the intercepted packet to the switch.
	FwdActionResume FwdAction = iota

	// FwdActionSettle settles the intercepted packet with a preimage.
	FwdActionSettle

	// FwdActionFail fails the intercepted packet back to the sender.
	FwdActionFail

	// FwdActionResumeModified forwards the intercepted packet to the switch
	// with modifications.
	FwdActionResumeModified
)

// FwdResolution defines the action to be taken on an intercepted packet.
type FwdResolution struct {
	// Key is the incoming circuit key of the htlc.
	Key models.CircuitKey

	// Action is the action to take on the intercepted htlc.
	Action FwdAction

	// Preimage is the preimage that is to be used for settling if Action is
	// FwdActionSettle.
	Preimage lntypes.Preimage

	// InAmountMsat is the amount that is to be used for validating if
	// Action is FwdActionResumeModified.
	InAmountMsat fn.Option[lnwire.MilliSatoshi]

	// OutAmountMsat is the amount that is to be used for forwarding if
	// Action is FwdActionResumeModified.
	OutAmountMsat fn.Option[lnwire.MilliSatoshi]

	// OutWireCustomRecords is the custom records that are to be used for
	// forwarding if Action is FwdActionResumeModified.
	OutWireCustomRecords fn.Option[lnwire.CustomRecords]

	// FailureMessage is the encrypted failure message that is to be passed
	// back to the sender if action is FwdActionFail.
	FailureMessage []byte

	// FailureCode is the failure code that is to be passed back to the
	// sender if action is FwdActionFail.
	FailureCode lnwire.FailCode
}

type fwdResolution struct {
	resolution *FwdResolution
	errChan    chan error
}

// InterceptableSwitchConfig contains the configuration of InterceptableSwitch.
type InterceptableSwitchConfig struct {
	// Switch is a reference to the actual switch implementation that
	// packets get sent to on resume.
	Switch *Switch

	// Notifier is an instance of a chain notifier that we'll use to signal
	// the switch when a new block has arrived.
	Notifier chainntnfs.ChainNotifier

	// CltvRejectDelta defines the number of blocks before the expiry of the
	// htlc where we auto-fail an intercepted htlc to prevent channel
	// force-closure.
	CltvRejectDelta uint32

	// CltvInterceptDelta defines the number of blocks before the expiry of
	// the htlc where we don't intercept anymore. This value must be greater
	// than CltvRejectDelta, because we don't want to offer htlcs to the
	// interceptor client for which there is no time left to resolve them
	// anymore.
	CltvInterceptDelta uint32

	// RequireInterceptor indicates whether processing should block if no
	// interceptor is connected.
	RequireInterceptor bool
}

// NewInterceptableSwitch returns an instance of InterceptableSwitch.
func NewInterceptableSwitch(cfg *InterceptableSwitchConfig) (
	*InterceptableSwitch, error) {

	if cfg.CltvInterceptDelta <= cfg.CltvRejectDelta {
		return nil, fmt.Errorf("cltv intercept delta %v not greater "+
			"than cltv reject delta %v",
			cfg.CltvInterceptDelta, cfg.CltvRejectDelta)
	}

	return &InterceptableSwitch{
		htlcSwitch:              cfg.Switch,
		intercepted:             make(chan *interceptedPackets),
		onchainIntercepted:      make(chan InterceptedForward),
		interceptorRegistration: make(chan ForwardInterceptor),
		heldHtlcSet:             newHeldHtlcSet(),
		resolutionChan:          make(chan *fwdResolution),
		requireInterceptor:      cfg.RequireInterceptor,
		cltvRejectDelta:         cfg.CltvRejectDelta,
		cltvInterceptDelta:      cfg.CltvInterceptDelta,
		notifier:                cfg.Notifier,

		quit: make(chan struct{}),
	}, nil
}

// SetInterceptor sets the ForwardInterceptor to be used. A nil argument
// unregisters the current interceptor.
func (s *InterceptableSwitch) SetInterceptor(
	interceptor ForwardInterceptor) {

	// Synchronize setting the handler with the main loop to prevent race
	// conditions.
	select {
	case s.interceptorRegistration <- interceptor:

	case <-s.quit:
	}
}

func (s *InterceptableSwitch) Start() error {
	log.Info("InterceptableSwitch starting...")

	if s.started.Swap(true) {
		return fmt.Errorf("InterceptableSwitch started more than once")
	}

	blockEpochStream, err := s.notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return err
	}
	s.blockEpochStream = blockEpochStream

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		err := s.run()
		if err != nil {
			log.Errorf("InterceptableSwitch stopped: %v", err)
		}
	}()

	log.Debug("InterceptableSwitch started")

	return nil
}

func (s *InterceptableSwitch) Stop() error {
	log.Info("InterceptableSwitch shutting down...")

	if s.stopped.Swap(true) {
		return fmt.Errorf("InterceptableSwitch stopped more than once")
	}

	close(s.quit)
	s.wg.Wait()

	// We need to check whether the start routine run and initialized the
	// `blockEpochStream`.
	if s.blockEpochStream != nil {
		s.blockEpochStream.Cancel()
	}

	log.Debug("InterceptableSwitch shutdown complete")

	return nil
}

func (s *InterceptableSwitch) run() error {
	// The block epoch stream will immediately stream the current height.
	// Read it out here.
	select {
	case currentBlock, ok := <-s.blockEpochStream.Epochs:
		if !ok {
			return errBlockStreamStopped
		}
		s.currentHeight = currentBlock.Height

	case <-s.quit:
		return nil
	}

	log.Debugf("InterceptableSwitch running: height=%v, "+
		"requireInterceptor=%v", s.currentHeight, s.requireInterceptor)

	for {
		select {
		// An interceptor registration or de-registration came in.
		case interceptor := <-s.interceptorRegistration:
			s.setInterceptor(interceptor)

		case packets := <-s.intercepted:
			var notIntercepted []*htlcPacket
			for _, p := range packets.packets {
				intercepted, err := s.interceptForward(
					p, packets.isReplay,
				)
				if err != nil {
					return err
				}

				if !intercepted {
					notIntercepted = append(
						notIntercepted, p,
					)
				}
			}
			err := s.htlcSwitch.ForwardPackets(
				packets.linkQuit, notIntercepted...,
			)
			if err != nil {
				log.Errorf("Cannot forward packets: %v", err)
			}

		case fwd := <-s.onchainIntercepted:
			// For on-chain interceptions, we don't know if it has
			// already been offered before. This information is in
			// the forwarding package which isn't easily accessible
			// from contractcourt. It is likely though that it was
			// already intercepted in the off-chain flow. And even
			// if not, it is safe to signal replay so that we won't
			// unexpectedly skip over this htlc.
			if _, err := s.forward(fwd, true); err != nil {
				return err
			}

		case res := <-s.resolutionChan:
			res.errChan <- s.resolve(res.resolution)

		case currentBlock, ok := <-s.blockEpochStream.Epochs:
			if !ok {
				return errBlockStreamStopped
			}

			s.currentHeight = currentBlock.Height

			// A new block is appended. Fail any held htlcs that
			// expire at this height to prevent channel force-close.
			s.failExpiredHtlcs()

		case <-s.quit:
			return nil
		}
	}
}

func (s *InterceptableSwitch) failExpiredHtlcs() {
	s.heldHtlcSet.popAutoFails(
		uint32(s.currentHeight),
		func(fwd InterceptedForward) {
			err := fwd.FailWithCode(
				lnwire.CodeTemporaryChannelFailure,
			)
			if err != nil {
				log.Errorf("Cannot fail packet: %v", err)
			}
		},
	)
}

func (s *InterceptableSwitch) sendForward(fwd InterceptedForward) {
	err := s.interceptor(fwd.Packet())
	if err != nil {
		// Only log the error. If we couldn't send the packet, we assume
		// that the interceptor will reconnect so that we can retry.
		log.Debugf("Interceptor cannot handle forward: %v", err)
	}
}

func (s *InterceptableSwitch) setInterceptor(interceptor ForwardInterceptor) {
	s.interceptor = interceptor

	// Replay all currently held htlcs. When an interceptor is not required,
	// there may be none because they've been cleared after the previous
	// disconnect.
	if interceptor != nil {
		log.Debugf("Interceptor connected")

		s.heldHtlcSet.forEach(s.sendForward)

		return
	}

	// The interceptor disconnects. If an interceptor is required, keep the
	// held htlcs.
	if s.requireInterceptor {
		log.Infof("Interceptor disconnected, retaining held packets")

		return
	}

	// Interceptor is not required. Release held forwards.
	log.Infof("Interceptor disconnected, resolving held packets")

	s.heldHtlcSet.popAll(func(fwd InterceptedForward) {
		err := fwd.Resume()
		if err != nil {
			log.Errorf("Failed to resume hold forward %v", err)
		}
	})
}

// resolve processes a HTLC given the resolution type specified by the
// intercepting client.
func (s *InterceptableSwitch) resolve(res *FwdResolution) error {
	intercepted, err := s.heldHtlcSet.pop(res.Key)
	if err != nil {
		return err
	}

	switch res.Action {
	case FwdActionResume:
		return intercepted.Resume()

	case FwdActionResumeModified:
		return intercepted.ResumeModified(
			res.InAmountMsat, res.OutAmountMsat,
			res.OutWireCustomRecords,
		)

	case FwdActionSettle:
		return intercepted.Settle(res.Preimage)

	case FwdActionFail:
		if len(res.FailureMessage) > 0 {
			return intercepted.Fail(res.FailureMessage)
		}

		return intercepted.FailWithCode(res.FailureCode)

	default:
		return fmt.Errorf("unrecognized action %v", res.Action)
	}
}

// Resolve resolves an intercepted packet.
func (s *InterceptableSwitch) Resolve(res *FwdResolution) error {
	internalRes := &fwdResolution{
		resolution: res,
		errChan:    make(chan error, 1),
	}

	select {
	case s.resolutionChan <- internalRes:

	case <-s.quit:
		return errors.New("switch shutting down")
	}

	select {
	case err := <-internalRes.errChan:
		return err

	case <-s.quit:
		return errors.New("switch shutting down")
	}
}

// ForwardPackets attempts to forward the batch of htlcs to a connected
// interceptor. If the interceptor signals the resume action, the htlcs are
// forwarded to the switch. The link's quit signal should be provided to allow
// cancellation of forwarding during link shutdown.
func (s *InterceptableSwitch) ForwardPackets(linkQuit <-chan struct{},
	isReplay bool, packets ...*htlcPacket) error {

	// Synchronize with the main event loop. This should be light in the
	// case where there is no interceptor.
	select {
	case s.intercepted <- &interceptedPackets{
		packets:  packets,
		linkQuit: linkQuit,
		isReplay: isReplay,
	}:

	case <-linkQuit:
		log.Debugf("Forward cancelled because link quit")

	case <-s.quit:
		return errors.New("interceptable switch quit")
	}

	return nil
}

// ForwardPacket forwards a single htlc to the external interceptor.
func (s *InterceptableSwitch) ForwardPacket(
	fwd InterceptedForward) error {

	select {
	case s.onchainIntercepted <- fwd:

	case <-s.quit:
		return errors.New("interceptable switch quit")
	}

	return nil
}

// interceptForward forwards the packet to the external interceptor after
// checking the interception criteria.
func (s *InterceptableSwitch) interceptForward(packet *htlcPacket,
	isReplay bool) (bool, error) {

	switch htlc := packet.htlc.(type) {
	case *lnwire.UpdateAddHTLC:
		// We are not interested in intercepting initiated payments.
		if packet.incomingChanID == hop.Source {
			return false, nil
		}

		intercepted := &interceptedForward{
			htlc:       htlc,
			packet:     packet,
			htlcSwitch: s.htlcSwitch,
			autoFailHeight: int32(packet.incomingTimeout -
				s.cltvRejectDelta),
		}

		// Handle forwards that are too close to expiry.
		handled, err := s.handleExpired(intercepted)
		if err != nil {
			log.Errorf("Error handling intercepted htlc "+
				"that expires too soon: circuit=%v, "+
				"incoming_timeout=%v, err=%v",
				packet.inKey(), packet.incomingTimeout, err)

			// Return false so that the packet is offered as normal
			// to the switch. This isn't ideal because interception
			// may be configured as always-on and is skipped now.
			// Returning true isn't great either, because the htlc
			// will remain stuck and potentially force-close the
			// channel. But in the end, we should never get here, so
			// the actual return value doesn't matter that much.
			return false, nil
		}
		if handled {
			return true, nil
		}

		return s.forward(intercepted, isReplay)

	default:
		return false, nil
	}
}

// forward records the intercepted htlc and forwards it to the interceptor.
func (s *InterceptableSwitch) forward(
	fwd InterceptedForward, isReplay bool) (bool, error) {

	inKey := fwd.Packet().IncomingCircuit

	// Ignore already held htlcs.
	if s.heldHtlcSet.exists(inKey) {
		return true, nil
	}

	// If there is no interceptor currently registered, configuration and packet
	// replay status determine how the packet is handled.
	if s.interceptor == nil {
		// Process normally if an interceptor is not required.
		if !s.requireInterceptor {
			return false, nil
		}

		// We are in interceptor-required mode. If this is a new packet, it is
		// still safe to fail back. The interceptor has never seen this packet
		// yet. This limits the backlog of htlcs when the interceptor is down.
		if !isReplay {
			err := fwd.FailWithCode(
				lnwire.CodeTemporaryChannelFailure,
			)
			if err != nil {
				log.Errorf("Cannot fail packet: %v", err)
			}

			return true, nil
		}

		// This packet is a replay. It is not safe to fail back, because the
		// interceptor may still signal otherwise upon reconnect. Keep the
		// packet in the queue until then.
		if err := s.heldHtlcSet.push(inKey, fwd); err != nil {
			return false, err
		}

		return true, nil
	}

	// There is an interceptor registered. We can forward the packet right now.
	// Hold it in the queue too to track what is outstanding.
	if err := s.heldHtlcSet.push(inKey, fwd); err != nil {
		return false, err
	}

	s.sendForward(fwd)

	return true, nil
}

// handleExpired checks that the htlc isn't too close to the channel
// force-close broadcast height. If it is, it is cancelled back.
func (s *InterceptableSwitch) handleExpired(fwd *interceptedForward) (
	bool, error) {

	height := uint32(s.currentHeight)
	if fwd.packet.incomingTimeout >= height+s.cltvInterceptDelta {
		return false, nil
	}

	log.Debugf("Interception rejected because htlc "+
		"expires too soon: circuit=%v, "+
		"height=%v, incoming_timeout=%v",
		fwd.packet.inKey(), height,
		fwd.packet.incomingTimeout)

	err := fwd.FailWithCode(
		lnwire.CodeExpiryTooSoon,
	)
	if err != nil {
		return false, err
	}

	return true, nil
}

// interceptedForward implements the InterceptedForward interface.
// It is passed from the switch to external interceptors that are interested
// in holding forwards and resolve them manually.
type interceptedForward struct {
	htlc           *lnwire.UpdateAddHTLC
	packet         *htlcPacket
	htlcSwitch     *Switch
	autoFailHeight int32
}

// Packet returns the intercepted htlc packet.
func (f *interceptedForward) Packet() InterceptedPacket {
	return InterceptedPacket{
		IncomingCircuit: models.CircuitKey{
			ChanID: f.packet.incomingChanID,
			HtlcID: f.packet.incomingHTLCID,
		},
		OutgoingChanID:       f.packet.outgoingChanID,
		Hash:                 f.htlc.PaymentHash,
		OutgoingExpiry:       f.htlc.Expiry,
		OutgoingAmount:       f.htlc.Amount,
		IncomingAmount:       f.packet.incomingAmount,
		IncomingExpiry:       f.packet.incomingTimeout,
		InOnionCustomRecords: f.packet.inOnionCustomRecords,
		OnionBlob:            f.htlc.OnionBlob,
		AutoFailHeight:       f.autoFailHeight,
		InWireCustomRecords:  f.packet.inWireCustomRecords,
	}
}

// Resume resumes the default behavior as if the packet was not intercepted.
func (f *interceptedForward) Resume() error {
	// Forward to the switch. A link quit channel isn't needed, because we
	// are on a different thread now.
	return f.htlcSwitch.ForwardPackets(nil, f.packet)
}

// ResumeModified resumes the default behavior with field modifications. The
// input amount (if provided) specifies that the value of the inbound HTLC
// should be interpreted differently from the on-chain amount during further
// validation. The presence of an output amount and/or custom records indicates
// that those values should be modified on the outgoing HTLC.
func (f *interceptedForward) ResumeModified(
	inAmountMsat fn.Option[lnwire.MilliSatoshi],
	outAmountMsat fn.Option[lnwire.MilliSatoshi],
	outWireCustomRecords fn.Option[lnwire.CustomRecords]) error {

	// Convert the optional custom records to the correct type and validate
	// them.
	validatedRecords, err := fn.MapOptionZ(
		outWireCustomRecords,
		func(cr lnwire.CustomRecords) fn.Result[lnwire.CustomRecords] {
			if len(cr) == 0 {
				return fn.Ok[lnwire.CustomRecords](nil)
			}

			// Type cast and validate custom records.
			err := cr.Validate()
			if err != nil {
				return fn.Err[lnwire.CustomRecords](
					fmt.Errorf("failed to validate "+
						"custom records: %w", err),
				)
			}

			return fn.Ok(cr)
		},
	).Unpack()
	if err != nil {
		return fmt.Errorf("failed to encode custom records: %w",
			err)
	}

	// Set the incoming amount, if it is provided, on the packet.
	inAmountMsat.WhenSome(func(amount lnwire.MilliSatoshi) {
		f.packet.incomingAmount = amount
	})

	// Modify the wire message contained in the packet.
	switch htlc := f.packet.htlc.(type) {
	case *lnwire.UpdateAddHTLC:
		outAmountMsat.WhenSome(func(amount lnwire.MilliSatoshi) {
			f.packet.amount = amount
			htlc.Amount = amount
		})

		// Merge custom records with any validated records that were
		// added in the modify request, overwriting any existing values
		// with those supplied in the modifier API.
		htlc.CustomRecords = htlc.CustomRecords.MergedCopy(
			validatedRecords,
		)

	case *lnwire.UpdateFulfillHTLC:
		if len(validatedRecords) > 0 {
			htlc.CustomRecords = validatedRecords
		}
	}

	log.Tracef("Forwarding packet %v", lnutils.SpewLogClosure(f.packet))

	// Forward to the switch. A link quit channel isn't needed, because we
	// are on a different thread now.
	return f.htlcSwitch.ForwardPackets(nil, f.packet)
}

// Fail notifies the intention to Fail an existing hold forward with an
// encrypted failure reason.
func (f *interceptedForward) Fail(reason []byte) error {
	obfuscatedReason := f.packet.obfuscator.IntermediateEncrypt(reason)

	return f.resolve(&lnwire.UpdateFailHTLC{
		Reason: obfuscatedReason,
	})
}

// FailWithCode notifies the intention to fail an existing hold forward with the
// specified failure code.
func (f *interceptedForward) FailWithCode(code lnwire.FailCode) error {
	shaOnionBlob := func() [32]byte {
		return sha256.Sum256(f.htlc.OnionBlob[:])
	}

	// Create a local failure.
	var failureMsg lnwire.FailureMessage

	switch code {
	case lnwire.CodeInvalidOnionVersion:
		failureMsg = &lnwire.FailInvalidOnionVersion{
			OnionSHA256: shaOnionBlob(),
		}

	case lnwire.CodeInvalidOnionHmac:
		failureMsg = &lnwire.FailInvalidOnionHmac{
			OnionSHA256: shaOnionBlob(),
		}

	case lnwire.CodeInvalidOnionKey:
		failureMsg = &lnwire.FailInvalidOnionKey{
			OnionSHA256: shaOnionBlob(),
		}

	case lnwire.CodeTemporaryChannelFailure:
		update := f.htlcSwitch.failAliasUpdate(
			f.packet.incomingChanID, true,
		)
		if update == nil {
			// Fallback to the original, non-alias behavior.
			var err error
			update, err = f.htlcSwitch.cfg.FetchLastChannelUpdate(
				f.packet.incomingChanID,
			)
			if err != nil {
				return err
			}
		}

		failureMsg = lnwire.NewTemporaryChannelFailure(update)

	case lnwire.CodeExpiryTooSoon:
		update, err := f.htlcSwitch.cfg.FetchLastChannelUpdate(
			f.packet.incomingChanID,
		)
		if err != nil {
			return err
		}

		failureMsg = lnwire.NewExpiryTooSoon(*update)

	default:
		return ErrUnsupportedFailureCode
	}

	// Encrypt the failure for the first hop. This node will be the origin
	// of the failure.
	reason, err := f.packet.obfuscator.EncryptFirstHop(failureMsg)
	if err != nil {
		return fmt.Errorf("failed to encrypt failure reason %w", err)
	}

	return f.resolve(&lnwire.UpdateFailHTLC{
		Reason: reason,
	})
}

// Settle forwards a settled packet to the switch.
func (f *interceptedForward) Settle(preimage lntypes.Preimage) error {
	if !preimage.Matches(f.htlc.PaymentHash) {
		return errors.New("preimage does not match hash")
	}
	return f.resolve(&lnwire.UpdateFulfillHTLC{
		PaymentPreimage: preimage,
	})
}

// resolve is used for both Settle and Fail and forwards the message to the
// switch.
func (f *interceptedForward) resolve(message lnwire.Message) error {
	pkt := &htlcPacket{
		incomingChanID: f.packet.incomingChanID,
		incomingHTLCID: f.packet.incomingHTLCID,
		outgoingChanID: f.packet.outgoingChanID,
		outgoingHTLCID: f.packet.outgoingHTLCID,
		isResolution:   true,
		circuit:        f.packet.circuit,
		htlc:           message,
		obfuscator:     f.packet.obfuscator,
		sourceRef:      f.packet.sourceRef,
	}
	return f.htlcSwitch.mailOrchestrator.Deliver(pkt.incomingChanID, pkt)
}

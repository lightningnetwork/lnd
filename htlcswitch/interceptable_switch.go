package htlcswitch

import (
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrFwdNotExists is an error returned when the caller tries to resolve
	// a forward that doesn't exist anymore.
	ErrFwdNotExists = errors.New("forward does not exist")

	// ErrUnsupportedFailureCode when processing of an unsupported failure
	// code is attempted.
	ErrUnsupportedFailureCode = errors.New("unsupported failure code")
)

// InterceptableSwitch is an implementation of ForwardingSwitch interface.
// This implementation is used like a proxy that wraps the switch and
// intercepts forward requests. A reference to the Switch is held in order
// to communicate back the interception result where the options are:
// Resume - forwards the original request to the switch as is.
// Settle - routes UpdateFulfillHTLC to the originating link.
// Fail - routes UpdateFailHTLC to the originating link.
type InterceptableSwitch struct {
	// htlcSwitch is the underline switch
	htlcSwitch *Switch

	// intercepted is where we stream all intercepted packets coming from
	// the switch.
	intercepted chan *interceptedPackets

	// resolutionChan is where we stream all responses coming from the
	// interceptor client.
	resolutionChan chan *fwdResolution

	// interceptorRegistration is a channel that we use to synchronize
	// client connect and disconnect.
	interceptorRegistration chan ForwardInterceptor

	// requireInterceptor indicates whether processing should block if no
	// interceptor is connected.
	requireInterceptor bool

	// interceptor is the handler for intercepted packets.
	interceptor ForwardInterceptor

	// holdForwards keeps track of outstanding intercepted forwards.
	holdForwards map[channeldb.CircuitKey]InterceptedForward

	wg   sync.WaitGroup
	quit chan struct{}
}

type interceptedPackets struct {
	packets  []*htlcPacket
	linkQuit chan struct{}
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
)

// FwdResolution defines the action to be taken on an intercepted packet.
type FwdResolution struct {
	// Key is the incoming circuit key of the htlc.
	Key channeldb.CircuitKey

	// Action is the action to take on the intercepted htlc.
	Action FwdAction

	// Preimage is the preimage that is to be used for settling if Action is
	// FwdActionSettle.
	Preimage lntypes.Preimage

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

// NewInterceptableSwitch returns an instance of InterceptableSwitch.
func NewInterceptableSwitch(s *Switch,
	requireInterceptor bool) *InterceptableSwitch {

	return &InterceptableSwitch{
		htlcSwitch:              s,
		intercepted:             make(chan *interceptedPackets),
		interceptorRegistration: make(chan ForwardInterceptor),
		holdForwards:            make(map[channeldb.CircuitKey]InterceptedForward),
		resolutionChan:          make(chan *fwdResolution),
		requireInterceptor:      requireInterceptor,

		quit: make(chan struct{}),
	}
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
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		s.run()
	}()

	return nil
}

func (s *InterceptableSwitch) Stop() error {
	close(s.quit)
	s.wg.Wait()

	return nil
}

func (s *InterceptableSwitch) run() {
	for {
		select {
		// An interceptor registration or de-registration came in.
		case interceptor := <-s.interceptorRegistration:
			s.setInterceptor(interceptor)

		case packets := <-s.intercepted:
			var notIntercepted []*htlcPacket
			for _, p := range packets.packets {
				if !s.interceptForward(p, packets.isReplay) {
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

		case res := <-s.resolutionChan:
			res.errChan <- s.resolve(res.resolution)

		case <-s.quit:
			return
		}
	}
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

		for _, fwd := range s.holdForwards {
			s.sendForward(fwd)
		}

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

	for _, fwd := range s.holdForwards {
		if err := fwd.Resume(); err != nil {
			log.Errorf("Failed to resume hold forward %v", err)
		}
	}
	s.holdForwards = make(map[channeldb.CircuitKey]InterceptedForward)
}

func (s *InterceptableSwitch) resolve(res *FwdResolution) error {
	intercepted, ok := s.holdForwards[res.Key]
	if !ok {
		return fmt.Errorf("fwd %v not found", res.Key)
	}
	delete(s.holdForwards, res.Key)

	switch res.Action {
	case FwdActionResume:
		return intercepted.Resume()

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
func (s *InterceptableSwitch) ForwardPackets(linkQuit chan struct{}, isReplay bool,
	packets ...*htlcPacket) error {

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

// interceptForward forwards the packet to the external interceptor after
// checking the interception criteria.
func (s *InterceptableSwitch) interceptForward(packet *htlcPacket,
	isReplay bool) bool {

	// Process normally if an interceptor is not required and not
	// registered.
	if !s.requireInterceptor && s.interceptor == nil {
		return false
	}

	switch htlc := packet.htlc.(type) {
	case *lnwire.UpdateAddHTLC:
		// We are not interested in intercepting initiated payments.
		if packet.incomingChanID == hop.Source {
			return false
		}

		inKey := channeldb.CircuitKey{
			ChanID: packet.incomingChanID,
			HtlcID: packet.incomingHTLCID,
		}

		// Ignore already held htlcs.
		if _, ok := s.holdForwards[inKey]; ok {
			return true
		}

		intercepted := &interceptedForward{
			htlc:       htlc,
			packet:     packet,
			htlcSwitch: s.htlcSwitch,
		}

		if s.interceptor == nil && !isReplay {
			// There is no interceptor registered, we are in
			// interceptor-required mode, and this is a new packet
			//
			// Because the interceptor has never seen this packet
			// yet, it is still safe to fail back. This limits the
			// backlog of htlcs when the interceptor is down.
			err := intercepted.FailWithCode(
				lnwire.CodeTemporaryChannelFailure,
			)
			if err != nil {
				log.Errorf("Cannot fail packet: %v", err)
			}

			return true
		}

		s.holdForwards[inKey] = intercepted

		// If there is no interceptor registered, we must be in
		// interceptor-required mode. The packet is kept in the queue
		// until the interceptor registers itself.
		if s.interceptor != nil {
			s.sendForward(intercepted)
		}

		return true

	default:
		return false
	}
}

// interceptedForward implements the InterceptedForward interface.
// It is passed from the switch to external interceptors that are interested
// in holding forwards and resolve them manually.
type interceptedForward struct {
	htlc       *lnwire.UpdateAddHTLC
	packet     *htlcPacket
	htlcSwitch *Switch
}

// Packet returns the intercepted htlc packet.
func (f *interceptedForward) Packet() InterceptedPacket {
	return InterceptedPacket{
		IncomingCircuit: channeldb.CircuitKey{
			ChanID: f.packet.incomingChanID,
			HtlcID: f.packet.incomingHTLCID,
		},
		OutgoingChanID: f.packet.outgoingChanID,
		Hash:           f.htlc.PaymentHash,
		OutgoingExpiry: f.htlc.Expiry,
		OutgoingAmount: f.htlc.Amount,
		IncomingAmount: f.packet.incomingAmount,
		IncomingExpiry: f.packet.incomingTimeout,
		CustomRecords:  f.packet.customRecords,
		OnionBlob:      f.htlc.OnionBlob,
	}
}

// Resume resumes the default behavior as if the packet was not intercepted.
func (f *interceptedForward) Resume() error {
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
		update, err := f.htlcSwitch.cfg.FetchLastChannelUpdate(
			f.packet.incomingChanID,
		)
		if err != nil {
			return err
		}

		failureMsg = lnwire.NewTemporaryChannelFailure(update)

	default:
		return ErrUnsupportedFailureCode
	}

	// Encrypt the failure for the first hop. This node will be the origin
	// of the failure.
	reason, err := f.packet.obfuscator.EncryptFirstHop(failureMsg)
	if err != nil {
		return fmt.Errorf("failed to encrypt failure reason %v", err)
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

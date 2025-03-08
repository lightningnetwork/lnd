package routerrpc

import (
	"errors"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// ErrFwdNotExists is an error returned when the caller tries to resolve
	// a forward that doesn't exist anymore.
	ErrFwdNotExists = errors.New("forward does not exist")

	// ErrMissingPreimage is an error returned when the caller tries to settle
	// a forward and doesn't provide a preimage.
	ErrMissingPreimage = errors.New("missing preimage")
)

// forwardInterceptor is a helper struct that handles the lifecycle of an RPC
// interceptor streaming session.
// It is created when the stream opens and disconnects when the stream closes.
type forwardInterceptor struct {
	// stream is the bidirectional RPC stream
	stream Router_HtlcInterceptorServer

	htlcSwitch htlcswitch.InterceptableHtlcForwarder
}

// newForwardInterceptor creates a new forwardInterceptor.
func newForwardInterceptor(htlcSwitch htlcswitch.InterceptableHtlcForwarder,
	stream Router_HtlcInterceptorServer) *forwardInterceptor {

	return &forwardInterceptor{
		htlcSwitch: htlcSwitch,
		stream:     stream,
	}
}

// run sends the intercepted packets to the client and receives the
// corresponding responses. On one hand it registered itself as an interceptor
// that receives the switch packets and on the other hand launches a go routine
// to read from the client stream.
// To coordinate all this and make sure it is safe for concurrent access all
// packets are sent to the main where they are handled.
func (r *forwardInterceptor) run() error {
	// Register our interceptor so we receive all forwarded packets.
	r.htlcSwitch.SetInterceptor(r.onIntercept)
	defer r.htlcSwitch.SetInterceptor(nil)

	for {
		resp, err := r.stream.Recv()
		if err != nil {
			return err
		}

		log.Tracef("Received packet from stream: %v",
			lnutils.SpewLogClosure(resp))

		if err := r.resolveFromClient(resp); err != nil {
			return err
		}
	}
}

// onIntercept is the function that is called by the switch for every forwarded
// packet. Our interceptor makes sure we hold the packet and then signal to the
// main loop to handle the packet. We only return true if we were able
// to deliver the packet to the main loop.
func (r *forwardInterceptor) onIntercept(
	htlc htlcswitch.InterceptedPacket) error {

	log.Tracef("Sending intercepted packet to client %v",
		lnutils.SpewLogClosure(htlc))

	inKey := htlc.IncomingCircuit

	// First hold the forward, then send to client.
	interceptionRequest := &ForwardHtlcInterceptRequest{
		IncomingCircuitKey: &CircuitKey{
			ChanId: inKey.ChanID.ToUint64(),
			HtlcId: inKey.HtlcID,
		},
		OutgoingRequestedChanId: htlc.OutgoingChanID.ToUint64(),
		PaymentHash:             htlc.Hash[:],
		OutgoingAmountMsat:      uint64(htlc.OutgoingAmount),
		OutgoingExpiry:          htlc.OutgoingExpiry,
		IncomingAmountMsat:      uint64(htlc.IncomingAmount),
		IncomingExpiry:          htlc.IncomingExpiry,
		CustomRecords:           htlc.InOnionCustomRecords,
		OnionBlob:               htlc.OnionBlob[:],
		AutoFailHeight:          htlc.AutoFailHeight,
		InWireCustomRecords:     htlc.InWireCustomRecords,
	}

	return r.stream.Send(interceptionRequest)
}

// resolveFromClient handles a resolution arrived from the client.
func (r *forwardInterceptor) resolveFromClient(
	in *ForwardHtlcInterceptResponse) error {

	if in.IncomingCircuitKey == nil {
		return status.Errorf(codes.InvalidArgument,
			"CircuitKey missing from ForwardHtlcInterceptResponse")
	}

	log.Tracef("Resolving intercepted packet %v", in)

	circuitKey := models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(
			in.IncomingCircuitKey.ChanId,
		),
		HtlcID: in.IncomingCircuitKey.HtlcId,
	}

	switch in.Action {
	case ResolveHoldForwardAction_RESUME:
		return r.htlcSwitch.Resolve(&htlcswitch.FwdResolution{
			Key:    circuitKey,
			Action: htlcswitch.FwdActionResume,
		})

	case ResolveHoldForwardAction_RESUME_MODIFIED:
		// Modify HTLC and resume forward.
		inAmtMsat := fn.None[lnwire.MilliSatoshi]()
		if in.InAmountMsat > 0 {
			inAmtMsat = fn.Some(lnwire.MilliSatoshi(
				in.InAmountMsat,
			))
		}

		outAmtMsat := fn.None[lnwire.MilliSatoshi]()
		if in.OutAmountMsat > 0 {
			outAmtMsat = fn.Some(lnwire.MilliSatoshi(
				in.OutAmountMsat,
			))
		}

		outWireCustomRecords := fn.None[lnwire.CustomRecords]()
		if len(in.OutWireCustomRecords) > 0 {
			// Validate custom records.
			cr := lnwire.CustomRecords(in.OutWireCustomRecords)
			if err := cr.Validate(); err != nil {
				return status.Errorf(
					codes.InvalidArgument,
					"failed to validate custom records: %v",
					err,
				)
			}

			outWireCustomRecords = fn.Some[lnwire.CustomRecords](cr)
		}

		//nolint:ll
		return r.htlcSwitch.Resolve(&htlcswitch.FwdResolution{
			Key:                  circuitKey,
			Action:               htlcswitch.FwdActionResumeModified,
			InAmountMsat:         inAmtMsat,
			OutAmountMsat:        outAmtMsat,
			OutWireCustomRecords: outWireCustomRecords,
		})

	case ResolveHoldForwardAction_FAIL:
		// Fail with an encrypted reason.
		if in.FailureMessage != nil {
			if in.FailureCode != 0 {
				return status.Errorf(
					codes.InvalidArgument,
					"failure message and failure code "+
						"are mutually exclusive",
				)
			}

			// Verify that the size is equal to the fixed failure
			// message size + hmac + two uint16 lengths. See BOLT
			// #4.
			if len(in.FailureMessage) !=
				lnwire.FailureMessageLength+32+2+2 {

				return status.Errorf(
					codes.InvalidArgument,
					"failure message length invalid",
				)
			}

			return r.htlcSwitch.Resolve(&htlcswitch.FwdResolution{
				Key:            circuitKey,
				Action:         htlcswitch.FwdActionFail,
				FailureMessage: in.FailureMessage,
			})
		}

		var code lnwire.FailCode
		switch in.FailureCode {
		case lnrpc.Failure_INVALID_ONION_HMAC:
			code = lnwire.CodeInvalidOnionHmac

		case lnrpc.Failure_INVALID_ONION_KEY:
			code = lnwire.CodeInvalidOnionKey

		case lnrpc.Failure_INVALID_ONION_VERSION:
			code = lnwire.CodeInvalidOnionVersion

		// Default to TemporaryChannelFailure.
		case 0, lnrpc.Failure_TEMPORARY_CHANNEL_FAILURE:
			code = lnwire.CodeTemporaryChannelFailure

		default:
			return status.Errorf(
				codes.InvalidArgument,
				"unsupported failure code: %v", in.FailureCode,
			)
		}

		return r.htlcSwitch.Resolve(&htlcswitch.FwdResolution{
			Key:         circuitKey,
			Action:      htlcswitch.FwdActionFail,
			FailureCode: code,
		})

	case ResolveHoldForwardAction_SETTLE:
		if in.Preimage == nil {
			return ErrMissingPreimage
		}
		preimage, err := lntypes.MakePreimage(in.Preimage)
		if err != nil {
			return err
		}

		return r.htlcSwitch.Resolve(&htlcswitch.FwdResolution{
			Key:      circuitKey,
			Action:   htlcswitch.FwdActionSettle,
			Preimage: preimage,
		})

	default:
		return status.Errorf(
			codes.InvalidArgument,
			"unrecognized resolve action %v", in.Action,
		)
	}
}

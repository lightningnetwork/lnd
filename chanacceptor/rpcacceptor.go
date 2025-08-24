package chanacceptor

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	errShuttingDown = errors.New("server shutting down")

	// errCustomLength is returned when our custom error's length exceeds
	// our maximum.
	errCustomLength = fmt.Errorf("custom error message exceeds length "+
		"limit: %v", maxErrorLength)

	// errInvalidUpfrontShutdown is returned when we cannot parse the
	// upfront shutdown address returned.
	errInvalidUpfrontShutdown = fmt.Errorf("could not parse upfront " +
		"shutdown address")

	// errInsufficientReserve is returned when the reserve proposed by for
	// a channel is less than the dust limit originally supplied.
	errInsufficientReserve = fmt.Errorf("reserve lower than proposed dust " +
		"limit")

	// errAcceptWithError is returned when we get a response which accepts
	// a channel but ambiguously also sets a custom error message.
	errAcceptWithError = errors.New("channel acceptor response accepts " +
		"channel, but also includes custom error")

	// errMaxHtlcTooHigh is returned if our htlc count exceeds the number
	// hard-set by BOLT 2.
	errMaxHtlcTooHigh = fmt.Errorf("htlc limit exceeds spec limit of: %v",
		input.MaxHTLCNumber/2)

	// maxErrorLength is the maximum error length we allow the error we
	// send to our peer to be.
	maxErrorLength = 500
)

// chanAcceptInfo contains a request for a channel acceptor decision, and a
// channel that the response should be sent on.
type chanAcceptInfo struct {
	request  *ChannelAcceptRequest
	response chan *ChannelAcceptResponse
}

// RPCAcceptor represents the RPC-controlled variant of the ChannelAcceptor.
// One RPCAcceptor allows one RPC client.
type RPCAcceptor struct {
	// receive is a function from which we receive channel acceptance
	// decisions. Note that this function is expected to block.
	receive func() (*lnrpc.ChannelAcceptResponse, error)

	// send is a function which sends requests for channel acceptance
	// decisions into our rpc stream.
	send func(request *lnrpc.ChannelAcceptRequest) error

	// requests is a channel that we send requests for a acceptor response
	// into.
	requests chan *chanAcceptInfo

	// timeout is the amount of time we allow the channel acceptance
	// decision to take. This time includes the time to send a query to the
	// acceptor, and the time it takes to receive a response.
	timeout time.Duration

	// params are our current chain params.
	params *chaincfg.Params

	// done is closed when the rpc client terminates.
	done chan struct{}

	// quit is closed when lnd is shutting down.
	quit chan struct{}

	wg sync.WaitGroup
}

// Accept is a predicate on the ChannelAcceptRequest which is sent to the RPC
// client who will respond with the ultimate decision. This function passes the
// request into the acceptor's requests channel, and returns the response it
// receives, failing the request if the timeout elapses.
//
// NOTE: Part of the ChannelAcceptor interface.
func (r *RPCAcceptor) Accept(req *ChannelAcceptRequest) *ChannelAcceptResponse {
	respChan := make(chan *ChannelAcceptResponse, 1)

	newRequest := &chanAcceptInfo{
		request:  req,
		response: respChan,
	}

	// timeout is the time after which ChannelAcceptRequests expire.
	timeout := time.After(r.timeout)

	// Create a rejection response which we can use for the cases where we
	// reject the channel.
	rejectChannel := NewChannelAcceptResponse(
		false, errChannelRejected, nil, 0, 0, 0, 0, 0, 0, false,
	)

	// Send the request to the newRequests channel.
	select {
	case r.requests <- newRequest:

	case <-timeout:
		log.Errorf("RPCAcceptor returned false - reached timeout of %v",
			r.timeout)
		return rejectChannel

	case <-r.done:
		return rejectChannel

	case <-r.quit:
		return rejectChannel
	}

	// Receive the response and return it. If no response has been received
	// in AcceptorTimeout, then return false.
	select {
	case resp := <-respChan:
		return resp

	case <-timeout:
		log.Errorf("RPCAcceptor returned false - reached timeout of %v",
			r.timeout)
		return rejectChannel

	case <-r.done:
		return rejectChannel

	case <-r.quit:
		return rejectChannel
	}
}

// NewRPCAcceptor creates and returns an instance of the RPCAcceptor.
func NewRPCAcceptor(receive func() (*lnrpc.ChannelAcceptResponse, error),
	send func(*lnrpc.ChannelAcceptRequest) error, timeout time.Duration,
	params *chaincfg.Params, quit chan struct{}) *RPCAcceptor {

	return &RPCAcceptor{
		receive:  receive,
		send:     send,
		requests: make(chan *chanAcceptInfo),
		timeout:  timeout,
		params:   params,
		done:     make(chan struct{}),
		quit:     quit,
	}
}

// Run is the main loop for the RPC Acceptor. This function will block until
// it receives the signal that lnd is shutting down, or the rpc stream is
// cancelled by the client.
func (r *RPCAcceptor) Run() error {
	// Wait for our goroutines to exit before we return.
	defer r.wg.Wait()

	// Create a channel that responses from acceptors are sent into.
	responses := make(chan *lnrpc.ChannelAcceptResponse)

	// errChan is used by the receive loop to signal any errors that occur
	// during reading from the stream. This is primarily used to shutdown
	// the send loop in the case of an RPC client disconnecting.
	errChan := make(chan error, 1)

	// Start a goroutine to receive responses from the channel acceptor.
	// We expect the receive function to block, so it must be run in a
	// goroutine (otherwise we could not send more than one channel accept
	// request to the client).
	r.wg.Add(1)
	go func() {
		r.receiveResponses(errChan, responses)
		r.wg.Done()
	}()

	return r.sendAcceptRequests(errChan, responses)
}

// receiveResponses receives responses for our channel accept requests and
// dispatches them into the responses channel provided, sending any errors that
// occur into the error channel provided.
func (r *RPCAcceptor) receiveResponses(errChan chan error,
	responses chan *lnrpc.ChannelAcceptResponse) {

	for {
		resp, err := r.receive()
		if err != nil {
			errChan <- err
			return
		}

		var pendingID [32]byte
		copy(pendingID[:], resp.PendingChanId)

		openChanResp := &lnrpc.ChannelAcceptResponse{
			Accept:          resp.Accept,
			PendingChanId:   pendingID[:],
			Error:           resp.Error,
			UpfrontShutdown: resp.UpfrontShutdown,
			CsvDelay:        resp.CsvDelay,
			ReserveSat:      resp.ReserveSat,
			InFlightMaxMsat: resp.InFlightMaxMsat,
			MaxHtlcCount:    resp.MaxHtlcCount,
			MinHtlcIn:       resp.MinHtlcIn,
			MinAcceptDepth:  resp.MinAcceptDepth,
			ZeroConf:        resp.ZeroConf,
		}

		// We have received a decision for one of our channel
		// acceptor requests.
		select {
		case responses <- openChanResp:

		case <-r.done:
			return

		case <-r.quit:
			return
		}
	}
}

// sendAcceptRequests handles channel acceptor requests sent to us by our
// Accept() function, dispatching them to our acceptor stream and coordinating
// return of responses to their callers.
func (r *RPCAcceptor) sendAcceptRequests(errChan chan error,
	responses chan *lnrpc.ChannelAcceptResponse) error {

	// Close the done channel to indicate that the acceptor is no longer
	// listening and any in-progress requests should be terminated.
	defer close(r.done)

	// Create a map of pending channel IDs to our original open channel
	// request and a response channel. We keep the original channel open
	// message so that we can validate our response against it.
	acceptRequests := make(map[[32]byte]*chanAcceptInfo)

	for {
		//nolint:ll
		select {
		// Consume requests passed to us from our Accept() function and
		// send them into our stream.
		case newRequest := <-r.requests:

			req := newRequest.request
			pendingChanID := req.OpenChanMsg.PendingChannelID

			// Map the channel commitment type to its RPC
			// counterpart. Also determine whether the zero-conf or
			// scid-alias channel types are set.
			var (
				commitmentType lnrpc.CommitmentType
				wantsZeroConf  bool
				wantsScidAlias bool
			)

			if req.OpenChanMsg.ChannelType != nil {
				channelFeatures := lnwire.RawFeatureVector(
					*req.OpenChanMsg.ChannelType,
				)
				switch {
				case channelFeatures.OnlyContains(
					lnwire.ZeroConfRequired,
					lnwire.ScidAliasRequired,
					lnwire.ScriptEnforcedLeaseRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.StaticRemoteKeyRequired,
				):
					commitmentType = lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE

				case channelFeatures.OnlyContains(
					lnwire.ZeroConfRequired,
					lnwire.ScriptEnforcedLeaseRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.StaticRemoteKeyRequired,
				):
					commitmentType = lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE

				case channelFeatures.OnlyContains(
					lnwire.ScidAliasRequired,
					lnwire.ScriptEnforcedLeaseRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.StaticRemoteKeyRequired,
				):
					commitmentType = lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE

				case channelFeatures.OnlyContains(
					lnwire.ScriptEnforcedLeaseRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.StaticRemoteKeyRequired,
				):
					commitmentType = lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE

				case channelFeatures.OnlyContains(
					lnwire.ZeroConfRequired,
					lnwire.ScidAliasRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.StaticRemoteKeyRequired,
				):
					commitmentType = lnrpc.CommitmentType_ANCHORS

				case channelFeatures.OnlyContains(
					lnwire.ZeroConfRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.StaticRemoteKeyRequired,
				):
					commitmentType = lnrpc.CommitmentType_ANCHORS

				case channelFeatures.OnlyContains(
					lnwire.ScidAliasRequired,
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.StaticRemoteKeyRequired,
				):
					commitmentType = lnrpc.CommitmentType_ANCHORS

				case channelFeatures.OnlyContains(
					lnwire.AnchorsZeroFeeHtlcTxRequired,
					lnwire.StaticRemoteKeyRequired,
				):
					commitmentType = lnrpc.CommitmentType_ANCHORS

				case channelFeatures.OnlyContains(
					lnwire.SimpleTaprootChannelsRequiredStaging,
					lnwire.ZeroConfRequired,
					lnwire.ScidAliasRequired,
				):
					commitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT

				case channelFeatures.OnlyContains(
					lnwire.SimpleTaprootChannelsRequiredStaging,
					lnwire.ZeroConfRequired,
				):
					commitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT

				case channelFeatures.OnlyContains(
					lnwire.SimpleTaprootChannelsRequiredStaging,
					lnwire.ScidAliasRequired,
				):
					commitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT

				case channelFeatures.OnlyContains(
					lnwire.SimpleTaprootChannelsRequiredStaging,
				):
					commitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT

				case channelFeatures.OnlyContains(
					lnwire.SimpleTaprootOverlayChansRequired,
					lnwire.ZeroConfRequired,
					lnwire.ScidAliasRequired,
				):
					commitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT_OVERLAY

				case channelFeatures.OnlyContains(
					lnwire.SimpleTaprootOverlayChansRequired,
					lnwire.ZeroConfRequired,
				):
					commitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT_OVERLAY

				case channelFeatures.OnlyContains(
					lnwire.SimpleTaprootOverlayChansRequired,
					lnwire.ScidAliasRequired,
				):
					commitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT_OVERLAY

				case channelFeatures.OnlyContains(
					lnwire.SimpleTaprootOverlayChansRequired,
				):
					commitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT_OVERLAY

				case channelFeatures.OnlyContains(
					lnwire.StaticRemoteKeyRequired,
				):
					commitmentType = lnrpc.CommitmentType_STATIC_REMOTE_KEY

				case channelFeatures.OnlyContains():
					commitmentType = lnrpc.CommitmentType_LEGACY

				default:
					log.Warnf("Unhandled commitment type "+
						"in channel acceptor request: %v",
						req.OpenChanMsg.ChannelType)
				}

				if channelFeatures.IsSet(
					lnwire.ZeroConfRequired,
				) {

					wantsZeroConf = true
				}

				if channelFeatures.IsSet(
					lnwire.ScidAliasRequired,
				) {

					wantsScidAlias = true
				}
			}

			acceptRequests[pendingChanID] = newRequest

			// A ChannelAcceptRequest has been received, send it to the client.
			chanAcceptReq := &lnrpc.ChannelAcceptRequest{
				NodePubkey:       req.Node.SerializeCompressed(),
				ChainHash:        req.OpenChanMsg.ChainHash[:],
				PendingChanId:    req.OpenChanMsg.PendingChannelID[:],
				FundingAmt:       uint64(req.OpenChanMsg.FundingAmount),
				PushAmt:          uint64(req.OpenChanMsg.PushAmount),
				DustLimit:        uint64(req.OpenChanMsg.DustLimit),
				MaxValueInFlight: uint64(req.OpenChanMsg.MaxValueInFlight),
				ChannelReserve:   uint64(req.OpenChanMsg.ChannelReserve),
				MinHtlc:          uint64(req.OpenChanMsg.HtlcMinimum),
				FeePerKw:         uint64(req.OpenChanMsg.FeePerKiloWeight),
				CsvDelay:         uint32(req.OpenChanMsg.CsvDelay),
				MaxAcceptedHtlcs: uint32(req.OpenChanMsg.MaxAcceptedHTLCs),
				ChannelFlags:     uint32(req.OpenChanMsg.ChannelFlags),
				CommitmentType:   commitmentType,
				WantsZeroConf:    wantsZeroConf,
				WantsScidAlias:   wantsScidAlias,
			}

			if err := r.send(chanAcceptReq); err != nil {
				return err
			}

		// Process newly received responses from our channel acceptor,
		// looking the original request up in our map of requests and
		// dispatching the response.
		case resp := <-responses:
			// Look up the appropriate channel to send on given the
			// pending ID. If a channel is found, send the response
			// over it.
			var pendingID [32]byte
			copy(pendingID[:], resp.PendingChanId)
			requestInfo, ok := acceptRequests[pendingID]
			if !ok {
				continue
			}

			// Validate the response we have received. If it is not
			// valid, we log our error and proceed to deliver the
			// rejection.
			accept, acceptErr, shutdown, err := r.validateAcceptorResponse(
				requestInfo.request.OpenChanMsg.DustLimit, resp,
			)
			if err != nil {
				log.Errorf("Invalid acceptor response: %v", err)
			}

			requestInfo.response <- NewChannelAcceptResponse(
				accept, acceptErr, shutdown,
				uint16(resp.CsvDelay),
				uint16(resp.MaxHtlcCount),
				uint16(resp.MinAcceptDepth),
				btcutil.Amount(resp.ReserveSat),
				lnwire.MilliSatoshi(resp.InFlightMaxMsat),
				lnwire.MilliSatoshi(resp.MinHtlcIn),
				resp.ZeroConf,
			)

			// Delete the channel from the acceptRequests map.
			delete(acceptRequests, pendingID)

		// If we failed to receive from our acceptor, we exit.
		case err := <-errChan:
			log.Errorf("Received an error: %v, shutting down", err)
			return err

		// Exit if we are shutting down.
		case <-r.quit:
			return errShuttingDown
		}
	}
}

// validateAcceptorResponse validates the response we get from the channel
// acceptor, returning a boolean indicating whether to accept the channel, an
// error to send to the peer, and any validation errors that occurred.
func (r *RPCAcceptor) validateAcceptorResponse(dustLimit btcutil.Amount,
	req *lnrpc.ChannelAcceptResponse) (bool, error, lnwire.DeliveryAddress,
	error) {

	channelStr := hex.EncodeToString(req.PendingChanId)

	// Check that the max htlc count is within the BOLT 2 hard-limit of 483.
	// The initiating side should fail values above this anyway, but we
	// catch the invalid user input here.
	if req.MaxHtlcCount > input.MaxHTLCNumber/2 {
		log.Errorf("Max htlc count: %v for channel: %v is greater "+
			"than limit of: %v", req.MaxHtlcCount, channelStr,
			input.MaxHTLCNumber/2)

		return false, errChannelRejected, nil, errMaxHtlcTooHigh
	}

	// Ensure that the reserve that has been proposed, if it is set, is at
	// least the dust limit that was proposed by the remote peer. This is
	// required by BOLT 2.
	reserveSat := btcutil.Amount(req.ReserveSat)
	if reserveSat != 0 && reserveSat < dustLimit {
		log.Errorf("Remote reserve: %v sat for channel: %v must be "+
			"at least equal to proposed dust limit: %v",
			req.ReserveSat, channelStr, dustLimit)

		return false, errChannelRejected, nil, errInsufficientReserve
	}

	// Attempt to parse the upfront shutdown address provided.
	upfront, err := chancloser.ParseUpfrontShutdownAddress(
		req.UpfrontShutdown, r.params,
	)
	if err != nil {
		log.Errorf("Could not parse upfront shutdown for "+
			"%v: %v", channelStr, err)

		return false, errChannelRejected, nil, errInvalidUpfrontShutdown
	}

	// Check that the custom error provided is valid.
	if len(req.Error) > maxErrorLength {
		return false, errChannelRejected, nil, errCustomLength
	}

	var haveCustomError = len(req.Error) != 0

	switch {
	// If accept is true, but we also have an error specified, we fail
	// because this result is ambiguous.
	case req.Accept && haveCustomError:
		return false, errChannelRejected, nil, errAcceptWithError

	// If we accept without an error message, we can just return a nil
	// error.
	case req.Accept:
		return true, nil, upfront, nil

	// If we reject the channel, and have a custom error, then we use it.
	case haveCustomError:
		return false, fmt.Errorf("%s", req.Error), nil, nil

	// Otherwise, we have rejected the channel with no custom error, so we
	// just use a generic error to fail the channel.
	default:
		return false, errChannelRejected, nil, nil
	}
}

// A compile-time constraint to ensure RPCAcceptor implements the ChannelAcceptor
// interface.
var _ ChannelAcceptor = (*RPCAcceptor)(nil)

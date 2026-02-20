//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tlv"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize it as the name of our
	// RPC service.
	subServerName = "SwitchRPC"
)

var (
	// macaroonOps are the set of capabilities that our minted macaroon (if
	// it doesn't already exist) will have.
	macaroonOps = []bakery.Op{
		{
			Entity: "offchain",
			Action: "read",
		},
		{
			Entity: "offchain",
			Action: "write",
		},
	}

	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/switchrpc.Switch/SendOnion": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/switchrpc.Switch/TrackOnion": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/switchrpc.Switch/BuildOnion": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/switchrpc.Switch/DisableRemoteRouter": {{
			Entity: "offchain",
			Action: "write",
		}},
	}

	// DefaultSwitchMacFilename is the default name of the switch macaroon
	// that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultSwitchMacFilename = "switch.macaroon"

	// ErrAmbiguousPaymentState is an error returned when a payment attempt
	// completes in an ambiguous state: no error and no valid preimage.
	ErrAmbiguousPaymentState = errors.New("payment completed in an " +
		"ambiguous state: no error and no valid preimage")
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	SwitchServer
}

// Server is a stand-alone sub RPC server which exposes functionality that
// allows clients to dispatch htlc attempts through the Lightning Network.
type Server struct {
	cfg *Config

	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	UnimplementedSwitchServer
}

var _ SwitchServer = (*Server)(nil)

// New creates a new instance of the SwitchServer given a configuration struct
// that contains all external dependencies. If the target macaroon exists, and
// we're unable to create it, then an error will be returned. We also return
// the set of permissions that we require as a server. At the time of writing
// of this documentation, this is the same macaroon as the admin macaroon.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	// If the path of the switch macaroon wasn't generated, then we'll
	// assume that it's found at the default network directory.
	if cfg.SwitchMacPath == "" {
		cfg.SwitchMacPath = filepath.Join(
			cfg.NetworkDir, DefaultSwitchMacFilename,
		)
	}

	// Now that we know the full path of the switch macaroon, we can check
	// to see if we need to create it or not. If stateless_init is set
	// then we don't write the macaroons.
	macFilePath := cfg.SwitchMacPath
	if cfg.MacService != nil && !cfg.MacService.StatelessInit &&
		!lnrpc.FileExists(macFilePath) {

		log.Infof("Making macaroons for Switch RPC Server at: %v",
			macFilePath)

		// At this point, we know that the switch macaroon doesn't yet,
		// exist, so we need to create it with the help of the main
		// macaroon service.
		switchMac, err := cfg.MacService.NewMacaroon(
			context.TODO(), macaroons.DefaultRootKeyID,
			macaroonOps...,
		)
		if err != nil {
			return nil, nil, err
		}
		switchMacBytes, err := switchMac.M().MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		err = os.WriteFile(macFilePath, switchMacBytes, 0644)
		if err != nil {
			_ = os.Remove(macFilePath)
			return nil, nil, err
		}
	}

	switchServer := &Server{
		cfg: cfg,
	}

	return switchServer, macPermissions, nil
}

// Start launches any helper goroutines required for the Server to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a
// sub RPC server to register itself with the main gRPC root server. Until this
// is called, each sub-server won't be able to have
// requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterSwitchServer(grpcServer, r)

	log.Debugf("Switch RPC server successfully register with root " +
		"gRPC server")

	return nil
}

// RegisterWithRestServer will be called by the root REST mux to direct a sub
// RPC server to register itself with the main REST mux server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRestServer(ctx context.Context,
	mux *runtime.ServeMux, dest string, opts []grpc.DialOption) error {

	// We make sure that we register it with the main REST server to ensure
	// all our methods are routed properly.
	err := RegisterSwitchHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register Switch REST server "+
			"with root REST server: %v", err)
		return err
	}

	log.Debugf("Switch REST server successfully registered with " +
		"root REST server")

	return nil
}

// CreateSubServer populates the subserver's dependencies using the passed
// SubServerConfigDispatcher. This method should fully initialize the
// sub-server instance, making it ready for action. It returns the macaroon
// permissions that the sub-server wishes to pass on to the root server for all
// methods routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) CreateSubServer(
	configRegistry lnrpc.SubServerConfigDispatcher) (
	lnrpc.SubServer, lnrpc.MacaroonPerms, error) {

	subServer, macPermissions, err := createNewSubServer(configRegistry)
	if err != nil {
		return nil, nil, err
	}

	r.SwitchServer = subServer

	return subServer, macPermissions, nil
}

// SendOnion provides an idempotent API for dispatching a pre-formed onion
// packet. This RPC is the primary entry point for a remote router that wishes
// to forward a payment through this lnd instance.
//
// To safely handle network failures, a client can and should retry this RPC
// after a timeout or disconnection. Retries MUST use the exact same
// attempt id to allow the server to correctly detect duplicate requests.
//
// The server guarantees safety against duplicate attempts by following a
// "write-ahead" style approach. Upon receiving a request, it first writes a
// durable record of the intent to dispatch the payment, marking the attempt as
// PENDING. Only after this record is secured does it proceed with validation
// and dispatch. If any of these subsequent steps fail, the server synchronously
// transitions the PENDING record to a final FAILED state, ensuring attempts are
// not "orphaned" in an initialized but not actually dispatched state.
//
// A client interacting with this RPC must handle four distinct categories of
// outcomes:
//
// 1. SUCCESS: This is a definitive confirmation that the HTLC has been
// successfully dispatched and is now in-flight. The client can proceed to track
// the payment's final result via the `TrackOnion` RPC.
//
// 2. DUPLICATE ACKNOWLEDGMENT: This is not a payment-level failure. It is a
// definitive acknowledgment that a request with the same attempt id has already
// been successfully processed. A retrying client should interpret this as a
// success for the attempt and proceed to tracking the payment's result.
//
// 3. AMBIGUOUS FAILURE (e.g, gRPC `codes.Unavailable` status):
// This error is returned if the server fails during the critical initial step
// of writing the PENDING record. In this state, the server cannot be certain
// whether the HTLC was dispatched. The client MUST retry the *exact same
// request* with the original attempt id to resolve this ambiguity. A client
// MUST NOT treat this error as a definitive failure and move on to a new
// attempt ID; doing so is the definition of a "leaked attempt" and risks a
// duplicate payment if the original ambiguous attempt succeeded.
//
// 4. DEFINITIVE FAILURE (any other error): For all other failures (e.g.,
// invalid parameters, local policy violations), this RPC will return a
// definitive error. A definitive failure is a guarantee that the HTLC was not
// and will not be dispatched. To prevent orphaned records, the server is
// responsible for transitioning the PENDING attempt to a FAILED state.
func (s *Server) SendOnion(_ context.Context,
	req *SendOnionRequest) (*SendOnionResponse, error) {

	// First, we initialize the attempt in our persistent store. This serves
	// as a durable record of our intent to send and gates the attempt id
	// for concurrent callers, allowing them to safely retry.
	//
	// NOTE: This durable write MUST happen before any dynamic validation
	// (e.g., liquidity, peer connectivity). This ordering guarantees that a
	// client's retry of a timed-out request will receive a stable acknowle-
	// dgement as duplicate, rather than a new, misleading transient error.
	// This prevents the client from incorrectly concluding the original
	// attempt failed, which would risk a duplicate attempt.
	attemptID := req.AttemptId
	err := s.cfg.AttemptStore.InitAttempt(attemptID)
	if err != nil {
		// A record for this attempt ID already exists. This indicates a
		// client-side retry of a request that was already successfully
		// registered. We return a distinct error to signal that the
		// dispatch request is acknowledged and that the caller can
		// safely proceed to track the result.
		if errors.Is(err, htlcswitch.ErrPaymentIDAlreadyExists) {
			log.Debugf("Attempt id=%v already exists", attemptID)

			return nil, status.Errorf(codes.AlreadyExists,
				"payment with attempt ID %d already exists",
				attemptID)
		}

		// If we receive an initialization error, we'll return the error
		// directly to the caller so they can handle the ambiguity.
		//
		// TODO(calvin): actually transport the error signal across the
		// rpc boundary possibly using grpc st.WithDetails().
		log.Errorf("Unable to initialize attempt id=%d: %v", attemptID,
			err)

		return nil, status.Errorf(codes.Unavailable, "unable to "+
			"initialize attempt id=%d: %v", attemptID, err)
	}

	// Perform all RPC-level pre-dispatch checks.
	chanID, htlcAdd, validationErr := s.validateAndPrepareOnion(req)

	// If request validation fails, we transition the attempt from an
	// initialized to a terminally failed state to prevent an orphaned
	// attempt. An orphaned PENDING record would cause any subsequent
	// TrackOnion call for this attempt to hang indefinitely.
	if validationErr != nil {
		log.Warnf("Validation failed for attempt %d: %v. Failing "+
			"pending attempt.", req.AttemptId, validationErr)

		s.failPendingAttempt(req.AttemptId, "validation failure")

		// Return the original, more specific validation error to the
		// client.
		return nil, validationErr
	}

	log.Debugf("Dispatching HTLC attempt(id=%v, amt=%v) for payment=%x "+
		"via channel=%s", req.AttemptId, req.Amount,
		htlcAdd.PaymentHash, chanID)

	// With the attempt initialized, we now dispatch the HTLC.
	dispatchErr := s.cfg.HtlcDispatcher.SendHTLC(chanID, attemptID, htlcAdd)
	if dispatchErr != nil {
		// The core dispatch logic failed definitively. We'll
		// transition the attempt from an initialized to a terminally
		// failed state to prevent an orphaned attempt.
		log.Warnf("Dispatch failed for attempt %d: %v. Failing "+
			"pending attempt.", req.AttemptId, dispatchErr)

		s.failPendingAttempt(req.AttemptId, "dispatch failure")

		// Translate the internal dispatch error into a gRPC status
		// with rich details for the client.
		message, code := translateErrorForRPC(dispatchErr)

		return &SendOnionResponse{
			Success:      false,
			ErrorMessage: message,
			ErrorCode:    code,
		}, nil
	}

	// The onion attempt was successfully dispatched.
	return &SendOnionResponse{Success: true}, nil
}

// failPendingAttempt is a helper which transitions the given attempt from an
// initialized (PENDING) state to a FAILED state in the persistent store.
func (s *Server) failPendingAttempt(attemptID uint64, context string) {
	// We use a generic failure reason, as this is an internal failure.
	// The original, more specific error is returned to the client.
	failReason := &lnwire.FailTemporaryNodeFailure{}

	err := s.cfg.AttemptStore.FailPendingAttempt(
		attemptID, htlcswitch.NewLinkError(failReason),
	)
	if err != nil {
		// If we fail to transition the attempt to a FAILED state
		// here, the htlc switch's own recovery mechanisms will
		// eventually clean up any orphaned PENDING attempts after a
		// restart.
		log.Errorf("Unable to fail pending attempt %d after %s: %v",
			attemptID, context, err)
	}
}

// validateAndPrepareOnion performs the pre-checks and preparation for a
// SendOnion request. It returns the channel ID, the HTLC to be sent, and any
// validation error.
func (s *Server) validateAndPrepareOnion(req *SendOnionRequest) (
	lnwire.ShortChannelID, *lnwire.UpdateAddHTLC, error) {

	var (
		chanID  lnwire.ShortChannelID
		htlcAdd *lnwire.UpdateAddHTLC
		err     error
	)

	if len(req.OnionBlob) != lnwire.OnionPacketSize {
		err = status.Errorf(
			codes.InvalidArgument,
			"onion blob size=%d does not match expected %d bytes",
			len(req.OnionBlob), lnwire.OnionPacketSize,
		)

		return chanID, htlcAdd, err
	}

	if len(req.PaymentHash) == 0 {
		err = status.Error(
			codes.InvalidArgument, "payment hash is required")

		return chanID, htlcAdd, err
	}

	if req.Amount <= 0 {
		err = status.Error(codes.InvalidArgument,
			"amount must be greater than zero",
		)

		return chanID, htlcAdd, err
	}

	var (
		amount       = lnwire.MilliSatoshi(req.Amount)
		pubkeySet    = len(req.FirstHopPubkey) != 0
		channelIDSet = req.FirstHopChanId != 0
	)

	switch {
	case pubkeySet == channelIDSet:
		err = status.Error(
			codes.InvalidArgument,
			"must specify exactly one of first_hop_pubkey or "+
				"first_hop_chan_id",
		)

		return chanID, htlcAdd, err

	case channelIDSet:
		// Case 1: The caller provided the first hop chan id directly.
		chanID = lnwire.NewShortChanIDFromInt(req.FirstHopChanId)

	case pubkeySet:
		// Case 2: Convert the first hop pubkey into a format usable by
		// the forwarding subsystem.
		firstHop, parseErr := btcec.ParsePubKey(req.FirstHopPubkey)
		if parseErr != nil {
			err = status.Errorf(codes.InvalidArgument,
				"invalid first hop pubkey=%x: %v",
				req.FirstHopPubkey, parseErr)

			return chanID, htlcAdd, err
		}

		// Find an eligible channel ID for the given first-hop pubkey.
		chanID, err = s.findEligibleChannelID(firstHop, amount)
		if err != nil {
			err = status.Errorf(codes.FailedPrecondition,
				"unable to find eligible channel for "+
					"pubkey=%x: %v",
				firstHop.SerializeCompressed(), err)

			return chanID, htlcAdd, err
		}
	}

	hash, parseErr := lntypes.MakeHash(req.PaymentHash)
	if parseErr != nil {
		err = status.Errorf(codes.InvalidArgument,
			"invalid payment_hash=%x: %v", req.PaymentHash,
			parseErr)

		return chanID, htlcAdd, err
	}

	var blindingPoint lnwire.BlindingPointRecord
	if len(req.BlindingPoint) > 0 {
		pubkey, parseErr := btcec.ParsePubKey(req.BlindingPoint)
		if parseErr != nil {
			err = status.Errorf(codes.InvalidArgument,
				"invalid blinding point: %v", parseErr)

			return chanID, htlcAdd, err
		}

		blindingPoint = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](
				pubkey,
			),
		)
	}

	// Craft an HTLC packet to send to the htlcswitch. The metadata within
	// this packet will be used to route the payment through the network,
	// starting with the first-hop.
	htlcAdd = &lnwire.UpdateAddHTLC{
		Amount:        amount,
		Expiry:        req.Timelock,
		PaymentHash:   hash,
		OnionBlob:     [lnwire.OnionPacketSize]byte(req.OnionBlob),
		BlindingPoint: blindingPoint,
		CustomRecords: req.CustomRecords,
		ExtraData:     lnwire.ExtraOpaqueData(req.ExtraData),
	}

	return chanID, htlcAdd, err
}

// findEligibleChannelID attempts to find an eligible channel based on the
// provided public key and the amount to be sent. It returns a channel ID that
// can carry the given payment amount.
func (s *Server) findEligibleChannelID(pubKey *btcec.PublicKey,
	amount lnwire.MilliSatoshi) (lnwire.ShortChannelID, error) {

	var pubKeyArray [33]byte
	copy(pubKeyArray[:], pubKey.SerializeCompressed())

	links, err := s.cfg.ChannelInfoAccessor.GetLinksByPubkey(pubKeyArray)
	if err != nil {
		return lnwire.ShortChannelID{},
			fmt.Errorf("failed to retrieve channels: %w", err)
	}

	for _, link := range links {
		log.Tracef("Considering channel link scid=%v",
			link.ShortChanID())

		// Ensure the link is eligible to forward payments.
		if !link.EligibleToForward() {
			continue
		}

		// Check if the channel has sufficient bandwidth.
		if link.Bandwidth() >= amount {
			// Check if adding an HTLC of this amount is possible.
			if err := link.MayAddOutgoingHtlc(amount); err == nil {
				return link.ShortChanID(), nil
			}
		}
	}

	return lnwire.ShortChannelID{},
		fmt.Errorf("no suitable channel found for amount: %d msat",
			amount)
}

// TrackOnion provides callers the means to query whether or not a payment
// dispatched via SendOnion succeeded or failed.
//
// This is a unary, long-polling RPC which blocks until a definitive result
// (success with preimage or a terminal failure) for the specified attempt ID
// is available.
//
// Clients must be aware of this blocking nature and should typically use a
// `context` with a timeout. If the RPC call is interrupted (e.g., due to a
// timeout or network error) before a result is returned, the client is left
// in an ambiguous state regarding the HTLC's status. In such cases, the
// client must safely retry the *same* request with the original `paymentHash`
// and `attemptID`. The server will either return the final result if it has
// become available or resume waiting.
//
// NOTE: It is crucial that clients **do not** interpret an ambiguous error as a
// definitive failure. Doing so may cause the client to incorrectly mark the
// attempt as failed and proceed to retry the payment with a new, distinct
// `attempt_id`, which risks a duplicate payment if the original attempt
// eventually succeeds.
func (s *Server) TrackOnion(ctx context.Context,
	req *TrackOnionRequest) (*TrackOnionResponse, error) {

	hash, err := lntypes.MakeHash(req.PaymentHash)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid payment_hash=%x: %v", req.PaymentHash, err)
	}

	log.Debugf("Looking up status of onion attempt_id=%d for payment=%v",
		req.AttemptId, hash)

	// Attempt to build the error decryptor with the provided session key
	// and hop public keys.
	errorDecryptor, err := buildErrorDecryptor(
		req.SessionKey, req.HopPubkeys,
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"error building decryptor: %v", err)
	}

	if errorDecryptor == nil {
		log.Debug("Unable to build error decrypter with information " +
			"provided. Will defer error handling to caller")
	}

	// Query the switch for the result of the payment attempt via onion.
	resultChan, err := s.cfg.HtlcDispatcher.GetAttemptResult(
		req.AttemptId, hash, errorDecryptor,
	)
	if err != nil {
		log.Errorf("GetAttemptResult failed for attempt_id=%d of "+
			" payment=%x: %v", req.AttemptId, hash, err)

		// If the payment ID is not found, we return a NotFound error.
		if errors.Is(err, htlcswitch.ErrPaymentIDNotFound) {
			return nil, status.Errorf(codes.NotFound,
				"payment with attempt ID %d not found",
				req.AttemptId)
		}

		// For other errors, we return an internal error.
		return nil, status.Errorf(codes.Internal,
			"GetAttemptResult failed: %v", err)
	}

	// The switch knows about this payment, we'll wait for a result to be
	// available.
	var (
		result *htlcswitch.PaymentResult
		ok     bool
	)

	select {
	case result, ok = <-resultChan:
		if !ok {
			// This channel is closed when the Switch shuts down. We
			// return a gRPC error to the client.
			return nil, status.Error(codes.Unavailable,
				htlcswitch.ErrSwitchExiting.Error())
		}

	case <-ctx.Done():
		// ctx.Done can be triggered by either client cancellation or a
		// deadline timeout. Return the canonical gRPC status code.
		return nil, status.FromContextError(ctx.Err()).Err()
	}

	// The attempt result arrived so the HTLC is no longer in-flight. If
	// the payment failed, we build a structured response for the client.
	if result.Error != nil {
		log.Debugf("HTLC attempt %d failed for payment=%v: %v",
			req.AttemptId, hash, result.Error)

		details := marshallFailureDetails(result.Error)

		return &TrackOnionResponse{
			Result: &TrackOnionResponse_FailureDetails{
				FailureDetails: details,
			},
		}, nil
	}

	// If no decryption keys were provided, the raw onion-encrypted error
	// is returned as-is, deferring decryption to the client.
	if len(result.EncryptedError) > 0 {
		log.Debugf("HTLC attempt %d failed for payment=%v with "+
			"encrypted error", req.AttemptId, hash)

		details := &FailureDetails{
			ErrorMessage: "payment attempt failed with encrypted " +
				"error (client-side decryption required)",
			Failure: &FailureDetails_EncryptedErrorData{
				EncryptedErrorData: result.EncryptedError,
			},
		}

		return &TrackOnionResponse{
			Result: &TrackOnionResponse_FailureDetails{
				FailureDetails: details,
			},
		}, nil
	}

	// If we have reached this point, we expect a valid preimage for a
	// successful payment.
	if result.Preimage == (lntypes.Preimage{}) {
		log.Errorf("Payment %v completed without a valid preimage "+
			"or error", hash)

		return nil, status.Error(codes.Internal,
			ErrAmbiguousPaymentState.Error())
	}

	log.Debugf("Received preimage via onion attempt_id=%d for payment=%v",
		req.AttemptId, hash)

	return &TrackOnionResponse{
		Result: &TrackOnionResponse_Preimage{
			Preimage: result.Preimage[:],
		},
	}, nil
}

// buildErrorDecryptor constructs an error decrypter given a sphinx session
// key and hop public keys for a payment route.
func buildErrorDecryptor(sessionKeyBytes []byte,
	hopPubkeys [][]byte) (htlcswitch.ErrorDecrypter, error) {

	sessionKeyProvided := len(sessionKeyBytes) > 0
	hopPubkeysProvided := len(hopPubkeys) > 0

	// If neither is provided, the caller wants to handle decryption. This
	// is a valid use case, so we return no decryptor and no error.
	if !sessionKeyProvided && !hopPubkeysProvided {
		return nil, nil
	}

	// If only one of the two is provided, it's a client error. Both are
	// required for decryption.
	if sessionKeyProvided != hopPubkeysProvided {
		return nil, fmt.Errorf("session_key and hop_pubkeys must be " +
			"provided together")
	}

	if err := validateSessionKey(sessionKeyBytes); err != nil {
		return nil, fmt.Errorf("invalid session key: %w", err)
	}

	sessionKey, _ := btcec.PrivKeyFromBytes(sessionKeyBytes)

	pubKeys := make([]*btcec.PublicKey, 0, len(hopPubkeys))
	for _, keyBytes := range hopPubkeys {
		pubKey, err := btcec.ParsePubKey(keyBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid public key: %w", err)
		}
		pubKeys = append(pubKeys, pubKey)
	}

	// Construct the sphinx circuit needed for error decryption using the
	// provided session key and hop public keys.
	circuit := reconstructCircuit(sessionKey, pubKeys)

	// Using the created circuit, initialize the error decrypter so we can
	// parse+decode any failures incurred by this payment within the
	// switch.
	return &htlcswitch.SphinxErrorDecrypter{
		OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(circuit),
	}, nil
}

// validateSessionKey validates the session key to ensure it has the correct
// length and is within the expected range [1, N-1] for the secp256k1 curve. If
// the session key is invalid, an error is returned.
func validateSessionKey(sessionKeyBytes []byte) error {
	const expectedKeyLength = 32

	// Check length of session key.
	if len(sessionKeyBytes) != expectedKeyLength {
		return fmt.Errorf("invalid session key length: got %d, "+
			"expected %d", len(sessionKeyBytes), expectedKeyLength)
	}

	// Interpret the key as a big-endian unsigned integer.
	keyValue := new(big.Int).SetBytes(sessionKeyBytes)

	// Check if the key is in the valid range [1, N-1].
	if keyValue.Sign() <= 0 || keyValue.Cmp(btcec.S256().N) >= 0 {
		return fmt.Errorf("session key is out of range")
	}

	return nil
}

// reconstructCircuit is a simple helper to assemble a sphinx Circuit from its
// consituent parts, namely ephemeral session key and hop public keys.
func reconstructCircuit(sessionKey *btcec.PrivateKey,
	pubKeys []*btcec.PublicKey) *sphinx.Circuit {

	return &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: pubKeys,
	}
}

// BuildOnion constructs a sphinx onion packet for the given route.
func (s *Server) BuildOnion(_ context.Context,
	req *BuildOnionRequest) (*BuildOnionResponse, error) {

	if req.Route == nil {
		return nil, status.Error(codes.InvalidArgument,
			"route information is required")
	}
	if len(req.PaymentHash) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"payment hash is required")
	}

	var (
		sessionKey *btcec.PrivateKey
		err        error
	)

	if len(req.SessionKey) == 0 {
		sessionKey, err = routing.GenerateNewSessionKey()
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to generate session key: %v", err)
		}
	} else {
		if err := validateSessionKey(req.SessionKey); err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid session key: %v", err)
		}

		sessionKey, _ = btcec.PrivKeyFromBytes(req.SessionKey)
	}

	// Convert the route to a Sphinx path.
	route, err := s.cfg.RouteProcessor.UnmarshallRoute(req.Route)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid route: %v", err)
	}

	// Generate the onion packet.
	onionBlob, circuit, err := paymentsdb.GenerateSphinxPacket(
		route, req.PaymentHash, sessionKey,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to create onion blob: %v", err)
	}

	// We'll provide the list of hop public keys for caller convenience.
	// They may wish to use them + the session key in a future call to
	// SendOnion so that the server can decrypt and handle errors.
	hopPubKeys := make([][]byte, len(circuit.PaymentPath))
	for i, pubKey := range circuit.PaymentPath {
		hopPubKeys[i] = pubKey.SerializeCompressed()
	}

	return &BuildOnionResponse{
		OnionBlob:  onionBlob,
		SessionKey: sessionKey.Serialize(),
		HopPubkeys: hopPubKeys,
	}, nil
}

// DisableRemoteRouter disables the remote router, allowing a migration back to
// the embedded router.
func (s *Server) DisableRemoteRouter(ctx context.Context,
	req *DisableRemoteRouterRequest) (*DisableRemoteRouterResponse, error) {

	err := s.cfg.RemoteRouterController.DisableRemoteRouter()
	if err != nil {
		if errors.Is(err, htlcswitch.ErrAttemptEntriesExist) {
			return nil, status.Errorf(
				codes.FailedPrecondition,
				"unable to disable remote router: %v",
				err,
			)
		}

		return nil, status.Errorf(
			codes.Internal,
			"unable to disable remote router: %v", err,
		)
	}

	return &DisableRemoteRouterResponse{}, nil
}

// translateErrorForRPC converts an error from the underlying HTLC switch to
// a form that we can package for delivery to SendOnion rpc clients.
func translateErrorForRPC(err error) (string, ErrorCode) {
	var (
		clearTextErr htlcswitch.ClearTextError
	)

	switch {
	case errors.Is(err, htlcswitch.ErrPaymentIDNotFound):
		return err.Error(), ErrorCode_PAYMENT_ID_NOT_FOUND

	case errors.Is(err, htlcswitch.ErrDuplicateAdd):
		return err.Error(), ErrorCode_DUPLICATE_HTLC

	case errors.Is(err, htlcswitch.ErrUnreadableFailureMessage):
		return err.Error(),
			ErrorCode_UNREADABLE_FAILURE_MESSAGE

	case errors.Is(err, htlcswitch.ErrSwitchExiting):
		return err.Error(), ErrorCode_SWITCH_EXITING

	case errors.As(err, &clearTextErr):
		var buf bytes.Buffer
		encodeErr := lnwire.EncodeFailure(
			&buf, clearTextErr.WireMessage(), 0,
		)
		if encodeErr != nil {
			return fmt.Sprintf("failed to encode wire "+
					"message: %v", encodeErr),
				ErrorCode_INTERNAL
		}

		return hex.EncodeToString(buf.Bytes()),
			ErrorCode_CLEAR_TEXT_ERROR

	default:
		return err.Error(), ErrorCode_INTERNAL
	}
}

// marshallFailureDetails creates the FailureDetails message for the
// TrackOnion response body.
func marshallFailureDetails(err error) *FailureDetails {
	var (
		clearTextErr htlcswitch.ClearTextError
		fwdErr       *htlcswitch.ForwardingError
	)

	details := &FailureDetails{
		ErrorMessage: err.Error(),
	}

	switch {
	// A forwarding error from an intermediate routing node. This is
	// checked first because ForwardingError is more specific and also
	// satisfies the ClearTextError interface.
	case errors.As(err, &fwdErr):
		fwdFailure := &ForwardingFailure{
			FailureSourceIndex: uint32(
				fwdErr.FailureSourceIdx,
			),
		}

		// The wire message may be nil when the onion error was
		// successfully decrypted but the inner failure message
		// could not be decoded (NewUnknownForwardingError). In
		// that case we still return the failure source index.
		if fwdErr.WireMessage() != nil {
			var buf bytes.Buffer

			encodeErr := lnwire.EncodeFailure(
				&buf, fwdErr.WireMessage(), 0,
			)
			if encodeErr != nil {
				log.Errorf("Failed to encode wire "+
					"message: %v", encodeErr)
			} else {
				fwdFailure.WireMessage = buf.Bytes()
			}
		}

		details.Failure = &FailureDetails_ForwardingFailure{
			ForwardingFailure: fwdFailure,
		}

	// A ClearTextError covers local failures (e.g. LinkError) where the
	// wire message is available in plaintext without decryption.
	case errors.As(err, &clearTextErr):
		var buf bytes.Buffer

		encodeErr := lnwire.EncodeFailure(
			&buf, clearTextErr.WireMessage(), 0,
		)
		if encodeErr != nil {
			log.Errorf("failed to encode wire message: %v",
				encodeErr)
			details.Failure = &FailureDetails_SwitchError{
				SwitchError: &SwitchError{},
			}

			return details
		}

		details.Failure = &FailureDetails_LocalFailure{
			LocalFailure: &LocalFailure{
				WireMessage: buf.Bytes(),
			},
		}

	// NOTE: ErrPaymentIDNotFound and ErrSwitchExiting are handled at
	// the transport level and will not reach this function.
	case errors.Is(err, htlcswitch.ErrUnreadableFailureMessage):
		details.Failure = &FailureDetails_UnreadableFailure{
			UnreadableFailure: &UnreadableFailure{},
		}

	// All other unexpected errors will be mapped to a generic internal
	// error. The specific reason is still available in the top-level
	// error_message.
	default:
		details.Failure = &FailureDetails_SwitchError{
			SwitchError: &SwitchError{},
		}
	}

	return details
}

// UnmarshallFailureDetails translates a FailureDetails message from a
// TrackOnion response into a concrete Go error. It handles all cases of the
// 'oneof failure' field.
func UnmarshallFailureDetails(details *FailureDetails,
	deobfuscator htlcswitch.ErrorDecrypter) (error, error) {

	if details == nil {
		return nil, errors.New("cannot unmarshall nil FailureDetails")
	}

	// Use a type switch on the 'oneof failure' field to handle the primary
	// structured error cases.
	switch failure := details.Failure.(type) {
	case *FailureDetails_ForwardingFailure:
		return UnmarshallForwardingError(failure.ForwardingFailure)

	case *FailureDetails_LocalFailure:
		return UnmarshallLinkError(failure.LocalFailure)

	case *FailureDetails_EncryptedErrorData:
		if deobfuscator == nil {
			return htlcswitch.ErrUnreadableFailureMessage, nil
		}

		// The client provides the decryption key/logic.
		return deobfuscator.DecryptError(failure.EncryptedErrorData)

	case *FailureDetails_UnreadableFailure:
		return htlcswitch.ErrUnreadableFailureMessage, nil

	case *FailureDetails_SwitchError:
		// The specific reason is in the top-level message.
		return errors.New(details.ErrorMessage), nil
	}

	// Fallback for safety, though the oneof should always be populated
	// on a failure response.
	return nil, fmt.Errorf("unknown or empty failure reason in "+
		"response: %v", details.ErrorMessage)
}

// UnmarshallForwardingError converts a protobuf ForwardingFailure message into
// an htlcswitch.ForwardingError.
func UnmarshallForwardingError(f *ForwardingFailure) (
	*htlcswitch.ForwardingError, error) {

	if f == nil {
		return nil, fmt.Errorf("cannot parse nil ForwardingFailure")
	}

	wireMsg, err := UnmarshallFailureMessage(f.WireMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to decode wire message: %w", err)
	}

	return htlcswitch.NewForwardingError(
		wireMsg, int(f.FailureSourceIndex),
	), nil
}

// UnmarshallLinkError converts a protobuf LocalFailure message into an
// htlcswitch.LinkError.
func UnmarshallLinkError(f *LocalFailure) (*htlcswitch.LinkError, error) {
	if f == nil {
		return nil, fmt.Errorf("cannot parse nil LocalFailure")
	}

	wireMsg, err := UnmarshallFailureMessage(f.WireMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to decode wire message: %w", err)
	}

	return htlcswitch.NewLinkError(wireMsg), nil
}

// UnmarshallFailureMessage decodes a raw wire message byte slice into a rich
// lnwire.FailureMessage object.
func UnmarshallFailureMessage(wireMsg []byte) (lnwire.FailureMessage, error) {
	r := bytes.NewReader(wireMsg)

	return lnwire.DecodeFailure(r, 0)
}

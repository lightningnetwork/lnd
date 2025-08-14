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
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
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

// SendOnion handles the incoming request to send a payment using a
// preconstructed onion blob provided by the caller.
func (s *Server) SendOnion(_ context.Context,
	req *SendOnionRequest) (*SendOnionResponse, error) {

	if len(req.OnionBlob) != lnwire.OnionPacketSize {
		return nil, status.Errorf(codes.InvalidArgument,
			"onion blob size=%d does not match expected %d bytes",
			len(req.OnionBlob), lnwire.OnionPacketSize)
	}

	if len(req.PaymentHash) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"payment hash is required")
	}

	if req.Amount <= 0 {
		return nil, status.Error(codes.InvalidArgument,
			"amount must be greater than zero")
	}

	var (
		amount       = lnwire.MilliSatoshi(req.Amount)
		pubkeySet    = len(req.FirstHopPubkey) != 0
		channelIDSet = req.FirstHopChanId != 0
		chanID       lnwire.ShortChannelID
	)

	switch {
	case pubkeySet == channelIDSet:
		return nil, status.Error(codes.InvalidArgument,
			"must specify exactly one of first_hop_pubkey or "+
				"first_hop_chan_id")

	case channelIDSet:
		// Case 1: The caller provided the first hop chan id directly.
		chanID = lnwire.NewShortChanIDFromInt(req.FirstHopChanId)

	case pubkeySet:
		// Case 2: Convert the first hop pubkey into a format usable by
		// the forwarding subsystem.
		firstHop, err := btcec.ParsePubKey(req.FirstHopPubkey)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid first hop pubkey=%x: %v",
				req.FirstHopPubkey, err)
		}

		// Find an eligible channel ID for the given first-hop pubkey.
		chanID, err = s.findEligibleChannelID(firstHop, amount)
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"unable to find eligible channel for "+
					"pubkey=%x: %v",
				firstHop.SerializeCompressed(), err)
		}
	}

	hash, err := lntypes.MakeHash(req.PaymentHash)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid payment_hash=%x: %v", req.PaymentHash, err)
	}

	var blindingPoint lnwire.BlindingPointRecord
	if len(req.BlindingPoint) > 0 {
		pubkey, err := btcec.ParsePubKey(req.BlindingPoint)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid blinding point: %v", err)
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
	htlcAdd := &lnwire.UpdateAddHTLC{
		Amount:        amount,
		Expiry:        req.Timelock,
		PaymentHash:   hash,
		OnionBlob:     [lnwire.OnionPacketSize]byte(req.OnionBlob),
		BlindingPoint: blindingPoint,
		CustomRecords: req.CustomRecords,
		ExtraData:     lnwire.ExtraOpaqueData(req.ExtraData),
	}

	log.Debugf("Dispatching HTLC attempt(id=%v, amt=%v) for payment=%v "+
		"via channel=%s", req.AttemptId, req.Amount, hash, chanID)

	// Send the HTLC to the first hop directly by way of the HTLCSwitch.
	err = s.cfg.HtlcDispatcher.SendHTLC(chanID, req.AttemptId, htlcAdd)
	if err != nil {
		message, code := translateErrorForRPC(err)
		return &SendOnionResponse{
			Success:      false,
			ErrorMessage: message,
			ErrorCode:    code,
		}, nil
	}

	return &SendOnionResponse{Success: true}, nil
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
		message, code := translateErrorForRPC(err)

		log.Errorf("GetAttemptResult failed for attempt_id=%d of "+
			" payment=%x: %v", req.AttemptId, hash, message)

		return &TrackOnionResponse{
			ErrorCode:    code,
			ErrorMessage: message,
		}, nil
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
			// This channel is closed when the Switch shuts down.
			return &TrackOnionResponse{
				ErrorCode: ErrorCode_SWITCH_EXITING,
				ErrorMessage: htlcswitch.ErrSwitchExiting.
					Error(),
			}, nil
		}

	case <-ctx.Done():
		// ctx.Done can be triggered by either client cancellation or a
		// deadline timeout. Return the canonical gRPC status code.
		return nil, status.FromContextError(ctx.Err()).Err()
	}

	// The attempt result arrived so the HTLC is no longer in-flight.
	if result.Error != nil {
		message, code := translateErrorForRPC(result.Error)

		log.Errorf("Payment via onion failed for payment=%v: %v",
			hash, message)

		return &TrackOnionResponse{
			ErrorCode:    code,
			ErrorMessage: message,
		}, nil
	}

	if len(result.EncryptedError) > 0 {
		log.Errorf("Payment via onion failed for payment=%v", hash)

		return &TrackOnionResponse{
			EncryptedError: result.EncryptedError,
		}, nil
	}

	// If we have reached this point, we expect a valid preimage for a
	// successful payment.
	if result.Preimage == (lntypes.Preimage{}) {
		log.Errorf("Payment %v completed without a valid preimage or "+
			"error", hash)

		return &TrackOnionResponse{
			ErrorCode:    ErrorCode_INTERNAL,
			ErrorMessage: ErrAmbiguousPaymentState.Error(),
		}, nil
	}

	log.Debugf("Received preimage via onion attempt_id=%d for payment=%v",
		req.AttemptId, hash)

	return &TrackOnionResponse{
		Preimage: result.Preimage[:],
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

// translateErrorForRPC converts an error from the underlying HTLC switch to
// a form that we can package for delivery to SendOnion rpc clients.
func translateErrorForRPC(err error) (string, ErrorCode) {
	var (
		clearTextErr htlcswitch.ClearTextError
		fwdErr       *htlcswitch.ForwardingError
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
		// If this is a forwarding error, we'll handle it specially.
		if errors.As(err, &fwdErr) {
			encodedError, encodeErr := encodeForwardingError(fwdErr)
			if encodeErr != nil {
				return fmt.Sprintf("failed to encode wire "+
						"message: %v", encodeErr),
					ErrorCode_INTERNAL
			}

			return encodedError,
				ErrorCode_FORWARDING_ERROR
		}

		// Otherwise, we'll just encode the clear text error.
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

// encodeForwardingError converts a forwarding error from the switch to the
// format we can package for delivery to SendOnion rpc clients. We preserve the
// failure message from the wire as well as the index along the route where the
// failure occurred.
func encodeForwardingError(e *htlcswitch.ForwardingError) (string, error) {
	var buf bytes.Buffer
	err := lnwire.EncodeFailure(&buf, e.WireMessage(), 0)
	if err != nil {
		return "", fmt.Errorf("failed to encode wire message: %w", err)
	}

	return fmt.Sprintf("%d@%s", e.FailureSourceIdx,
		hex.EncodeToString(buf.Bytes())), nil
}

// ParseForwardingError converts an error from the format in SendOnion rpc
// protos to a forwarding error type.
func ParseForwardingError(errStr string) (*htlcswitch.ForwardingError, error) {
	parts := strings.SplitN(errStr, "@", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid forwarding error format: %s",
			errStr)
	}

	idx, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid forwarding error index: %s",
			errStr)
	}

	wireMsgBytes, err := hex.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid forwarding error wire "+
			"message: %s", errStr)
	}

	r := bytes.NewReader(wireMsgBytes)
	wireMsg, err := lnwire.DecodeFailure(r, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to decode wire message: %w",
			err)
	}

	return htlcswitch.NewForwardingError(wireMsg, idx), nil
}

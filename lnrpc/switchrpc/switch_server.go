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
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
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
	}

	// DefaultSwitchMacFilename is the default name of the switch macaroon
	// that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultSwitchMacFilename = "switch.macaroon"
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	SwitchServer
}

type Server struct {
	cfg *Config

	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	UnimplementedSwitchServer
}

// New creates a new instance of the SwitchServer given a configuration struct
// that contains all external dependencies. If the target macaroon exists, and
// we're unable to create it, then an error will be returned. We also return
// the set of permissions that we require as a server. At the time of writing
// of this documentation, this is the same macaroon as the admin macaroon.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	// If the path of the router macaroon wasn't generated, then we'll
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
			context.Background(), macaroons.DefaultRootKeyID,
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
		// quit: make(chan struct{}),
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
func (r *ServerShell) CreateSubServer(configRegistry lnrpc.SubServerConfigDispatcher) (
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

	if len(req.OnionBlob) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"onion blob is required")
	}
	if len(req.OnionBlob) != lnwire.OnionPacketSize {
		return nil, status.Errorf(codes.InvalidArgument,
			"onion blob size=%d does not match expected %d bytes",
			len(req.OnionBlob), lnwire.OnionPacketSize)
	}

	if len(req.PaymentHash) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"payment hash is required")
	}

	amount := lnwire.MilliSatoshi(req.Amount)
	if amount <= 0 {
		return nil, status.Error(codes.InvalidArgument,
			"amount must be greater than zero")
	}

	pubkeySet := len(req.FirstHopPubkey) != 0
	channelIDSet := req.FirstHopChanId != 0

	if pubkeySet == channelIDSet {
		return nil, status.Error(codes.InvalidArgument,
			"must specify exactly one of first_hop_pubkey or "+
				"first_hop_chan_id")
	}

	var chanID lnwire.ShortChannelID

	// Case 1: The caller provided the first hop channel ID directly.
	if channelIDSet {
		chanID = lnwire.NewShortChanIDFromInt(req.FirstHopChanId)
	} else {
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
				"unable to find eligible channel ID for pubkey=%x: %v",
				firstHop.SerializeCompressed(), err)
		}
	}

	hash, err := lntypes.MakeHash(req.PaymentHash)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid payment_hash=%x: %v", req.PaymentHash, err)
	}

	blindingPoint := lnwire.BlindingPointRecord{}
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
		Amount:      amount,
		Expiry:      req.Timelock,
		PaymentHash: hash,
		OnionBlob:   [lnwire.OnionPacketSize]byte(req.OnionBlob),
		// TODO: Add these from SendOnionRequest.
		BlindingPoint: blindingPoint,
		CustomRecords: req.CustomRecords,
		ExtraData:     lnwire.ExtraOpaqueData(req.ExtraData),
	}

	log.Debugf("Dispatching HTLC attempt(id=%v, amt=%v) for payment=%v via "+
		"first_hop=%x over channel=%s", req.AttemptId, req.Amount,
		hash, req.FirstHopPubkey, chanID)

	// Send the HTLC to the first hop directly by way of the HTLCSwitch.
	err = s.cfg.HtlcDispatcher.SendHTLC(chanID, req.AttemptId, htlcAdd)
	if err != nil {
		message, code := TranslateErrorForRPC(err)
		return &SendOnionResponse{
			Success:      false,
			ErrorMessage: message,
			ErrorCode:    code,
		}, nil
	}

	return &SendOnionResponse{Success: true}, nil
}

// ChannelInfoAccessor defines an interface for accessing channel information
// necessary for routing payments, specifically methods for fetching links by
// public key.
type ChannelInfoAccessor interface {
	GetLinksByPubkey(pubKey [33]byte) ([]htlcswitch.ChannelInfoProvider,
		error)
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

	// NOTE(calvin): This is NOT duplicating the checks that the Switch
	// itself will perform as those are only performed in ForwardPackets().
	for _, link := range links {
		log.Debugf("Considering channel link scid=%v",
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
func (s *Server) TrackOnion(ctx context.Context,
	req *TrackOnionRequest) (*TrackOnionResponse, error) {

	hash, err := lntypes.MakeHash(req.PaymentHash)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"invalid payment_hash=%x: %v", req.PaymentHash, err)
	}

	if req.SharedSecrets != nil {
		return nil, status.Errorf(codes.Unimplemented,
			"unable to process shared secrets")
	}

	// NOTE(calvin): In order to decrypt errors server side we require
	// either the combination of session key and hop public keys from which
	// we can construct the shared secrets used to build the onion or,
	// alternatively, the caller can provide the list of shared secrets used
	// during onion construction directly if they wish to maintain route
	// privacy from the server.
	//
	// TODO: If we want to support "oblivious sends", then we'll need to
	// pass the shared secrets to the sphinx library.
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
		message, code := TranslateErrorForRPC(err)

		log.Errorf("GetAttemptResult failed for attempt_id=%d of "+
			"payment=%v: %v", req.AttemptId, hash, message)

		// If this is a htlcswitch.ErrPaymentIDNotFound error, we need
		// to transmit that fact to the client so they can know with
		// certainty that no HTLC by that attempt ID is in-flight and
		// that it is safe to retry.
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
			// NOTE(calvin): Transport via protobuf message if
			// string matching is not preferred in client.
			return nil, status.Errorf(codes.Internal,
				"failed locate payment attempt: %v",
				htlcswitch.ErrSwitchExiting)
		}

	case <-ctx.Done():
		return nil, nil
	}

	// The attempt result arrived so the HTLC is no longer in-flight.
	if result.Error != nil {
		message, code := TranslateErrorForRPC(result.Error)

		log.Errorf("Payment via onion failed for payment=%v: %v",
			hash, message)

		return &TrackOnionResponse{
			ErrorCode:    code,
			ErrorMessage: message,
		}, nil
	}

	// In case we don't process the error, we'll return the encrypted
	// error blob for handling by the caller.
	if result.EncryptedError != nil {
		log.Errorf("Payment via onion failed for payment=%v", hash)

		return &TrackOnionResponse{
			EncryptedError: result.EncryptedError,
		}, nil
	}

	return &TrackOnionResponse{
		Preimage: result.Preimage[:],
	}, nil
}

// buildErrorDecryptor constructs an error decrypter given a sphinx session
// key and hop public keys for a payment route.
func buildErrorDecryptor(sessionKeyBytes []byte,
	hopPubkeys [][]byte) (htlcswitch.ErrorDecrypter, error) {

	if len(sessionKeyBytes) == 0 || len(hopPubkeys) == 0 {
		return nil, nil
	}

	if err := validateSessionKey(sessionKeyBytes); err != nil {
		return nil, fmt.Errorf("invalid session key: %w", err)
	}

	// TODO(calvin): Validate that the session key makes a valid private
	// key? This is untrusted input received via RPC.
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

	var sessionKey *btcec.PrivateKey
	var err error
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
	onionBlob, circuit, err := channeldb.GenerateSphinxPacket(
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

// TranslateErrorForRPC converts an error from the underlying HTLC switch to
// a form that we can package for delivery to SendOnion rpc clients.
func TranslateErrorForRPC(err error) (string, ErrorCode) {
	var clearTextErr htlcswitch.ClearTextError

	switch {
	case errors.Is(err, htlcswitch.ErrPaymentIDNotFound):
		return err.Error(), ErrorCode_ERROR_CODE_PAYMENT_ID_NOT_FOUND

	case errors.Is(err, htlcswitch.ErrDuplicateAdd):
		return err.Error(), ErrorCode_ERROR_CODE_DUPLICATE_HTLC

	case errors.Is(err, htlcswitch.ErrUnreadableFailureMessage):
		return err.Error(), ErrorCode_ERROR_CODE_UNREADABLE_FAILURE_MESSAGE

	case errors.Is(err, htlcswitch.ErrSwitchExiting):
		return err.Error(), ErrorCode_ERROR_CODE_SWITCH_EXITING

	case errors.As(err, &clearTextErr):
		switch e := err.(type) {
		case *htlcswitch.ForwardingError:
			encodedError, encodeErr := encodeForwardingError(e)
			if encodeErr != nil {
				return fmt.Sprintf("failed to encode wire "+
						"message: %v", encodeErr),
					ErrorCode_ERROR_CODE_INTERNAL
			}

			return encodedError,
				ErrorCode_ERROR_CODE_FORWARDING_ERROR
		default:
			var buf bytes.Buffer
			encodeErr := lnwire.EncodeFailure(
				&buf, clearTextErr.WireMessage(), 0,
			)
			if encodeErr != nil {
				return fmt.Sprintf("failed to encode wire "+
						"message: %v", encodeErr),
					ErrorCode_ERROR_CODE_INTERNAL
			}

			return hex.EncodeToString(buf.Bytes()),
				ErrorCode_ERROR_CODE_CLEAR_TEXT_ERROR
		}

	default:
		return err.Error(), ErrorCode_ERROR_CODE_INTERNAL
	}
}

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
		return nil, fmt.Errorf("invalid forwarding error wire message: %s",
			errStr)
	}

	r := bytes.NewReader(wireMsgBytes)
	wireMsg, err := lnwire.DecodeFailure(r, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to decode wire message: %v",
			err)
	}

	return htlcswitch.NewForwardingError(wireMsg, idx), nil
}

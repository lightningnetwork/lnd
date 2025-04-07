package rpcwallet

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

type (
	StreamClient = walletrpc.WalletKit_SignCoordinatorStreamsClient
	StreamServer = walletrpc.WalletKit_SignCoordinatorStreamsServer
)

// RemoteSignerConnection is an interface that abstracts the communication with
// a remote signer. It extends the RemoteSignerRequests interface, and adds som
// additional methods to manage the connection and verify the health of the
// remote signer.
type RemoteSignerConnection interface {
	// RemoteSignerRequests is an interface that defines the requests that
	// can be sent to a remote signer.
	RemoteSignerRequests

	// Timeout returns the set connection timeout for the remote signer.
	Timeout() time.Duration

	// Ready returns a channel that nil gets sent over once the remote
	// signer is ready to accept requests.
	Ready(ctx context.Context) chan error

	// Stop gracefully disconnects from the remote signer.
	Stop()

	// Ping verifies that the remote signer is still responsive.
	Ping(ctx context.Context, timeout time.Duration) error
}

// RemoteSignerRequests is an interface that defines the requests that can be
// sent to a remote signer. It's a subset of the signrpc.SignerClient and
// walletrpc.WalletKitClient interfaces.
type RemoteSignerRequests interface {
	// DeriveSharedKey sends a SharedKeyRequest to the remote signer and
	// waits for the corresponding response.
	DeriveSharedKey(ctx context.Context,
		in *signrpc.SharedKeyRequest,
		opts ...grpc.CallOption) (*signrpc.SharedKeyResponse, error)

	// MuSig2Cleanup sends a MuSig2CleanupRequest to the remote signer and
	// waits for the corresponding response.
	MuSig2Cleanup(ctx context.Context,
		in *signrpc.MuSig2CleanupRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2CleanupResponse, error)

	// MuSig2CombineSig sends a MuSig2CombineSigRequest to the remote signer
	// and waits for the corresponding response.
	MuSig2CombineSig(ctx context.Context,
		in *signrpc.MuSig2CombineSigRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2CombineSigResponse,
		error)

	// MuSig2CreateSession sends a MuSig2SessionRequest to the remote signer
	// and waits for the corresponding response.
	MuSig2CreateSession(ctx context.Context,
		in *signrpc.MuSig2SessionRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2SessionResponse, error)

	// MuSig2RegisterNonces sends a MuSig2RegisterNoncesRequest to the
	// remote signer and waits for the corresponding response.
	MuSig2RegisterNonces(ctx context.Context,
		in *signrpc.MuSig2RegisterNoncesRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2RegisterNoncesResponse,
		error)

	// MuSig2Sign sends a MuSig2SignRequest to the remote signer and waits
	// for the corresponding response.
	MuSig2Sign(ctx context.Context,
		in *signrpc.MuSig2SignRequest,
		opts ...grpc.CallOption) (*signrpc.MuSig2SignResponse, error)

	// SignMessage sends a SignMessageReq to the remote signer and waits for
	// the corresponding response.
	SignMessage(ctx context.Context,
		in *signrpc.SignMessageReq,
		opts ...grpc.CallOption) (*signrpc.SignMessageResp, error)

	// SignPsbt sends a SignPsbtRequest to the remote signer and waits for
	// the corresponding response.
	SignPsbt(ctx context.Context, in *walletrpc.SignPsbtRequest,
		opts ...grpc.CallOption) (*walletrpc.SignPsbtResponse, error)
}

// OutboundConnection is an abstraction of the outbound connection made to an
// inbound remote signer. An inbound remote signer is a remote signer that
// allows the watch-only node to connect to it via an inbound GRPC connection.
type OutboundConnection struct {
	// Embedded signrpc.SignerClient and walletrpc.WalletKitClient to
	// implement the RemoteSigner interface.
	signrpc.SignerClient
	walletrpc.WalletKitClient

	// The ConnectionCfg containing connection details of the remote signer.
	cfg lncfg.ConnectionCfg

	// conn represents the connection to the remote signer.
	conn *grpc.ClientConn
}

// NewOutboundConnection creates a new OutboundConnection instance.
// The function sets up a connection to the remote signer node.
func NewOutboundConnection(ctx context.Context,
	cfg lncfg.ConnectionCfg) (*OutboundConnection, error) {

	remoteSigner := &OutboundConnection{
		cfg: cfg,
	}

	err := remoteSigner.connect(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("error connecting to the remote "+
			"signing node through RPC: %v", err)
	}

	return remoteSigner, nil
}

// Ready returns a channel that nil gets sent over once the connection to the
// remote signer is set up and the remote signer is ready to accept requests.
//
// NOTE: This is part of the RemoteSignerConnection interface.
func (r *OutboundConnection) Ready(_ context.Context) chan error {
	// The inbound remote signer is ready as soon we have connected to the
	// remote signer node in the constructor. Therefore, we always send
	// nil here to signal that we are ready.
	readyChan := make(chan error, 1)
	readyChan <- nil

	return readyChan
}

// Ping verifies that the remote signer is still responsive.
//
// NOTE: This is part of the RemoteSignerConnection interface.
func (r *OutboundConnection) Ping(ctx context.Context, _ time.Duration) error {
	if r.conn == nil || r.conn.GetState() != connectivity.Ready {
		return errors.New("remote signer is not connected")
	}

	pingMsg := []byte("ping test")
	keyLoc := &signrpc.KeyLocator{
		KeyFamily: int32(keychain.KeyFamilyNodeKey),
		KeyIndex:  1,
	}

	// Sign a message with the default ECDSA.
	signMsgReq := &signrpc.SignMessageReq{
		Msg:        pingMsg,
		KeyLoc:     keyLoc,
		SchnorrSig: false,
	}

	_, err := r.SignMessage(ctx, signMsgReq)

	return err
}

// Timeout returns the set connection timeout for the remote signer.
//
// NOTE: This is part of the RemoteSignerConnection interface.
func (r *OutboundConnection) Timeout() time.Duration {
	return r.cfg.Timeout
}

// Stop closes the connection to the remote signer.
//
// NOTE: This is part of the RemoteSignerConnection interface.
func (r *OutboundConnection) Stop() {
	if r.conn != nil {
		r.conn.Close()
	}
}

// connect tries to establish an RPC connection to the configured host:port with
// the supplied certificate and macaroon.
func (r *OutboundConnection) connect(ctx context.Context,
	cfg lncfg.ConnectionCfg) error {

	certBytes, err := os.ReadFile(cfg.TLSCertPath)
	if err != nil {
		return fmt.Errorf("error reading TLS cert file %v: %w",
			cfg.TLSCertPath, err)
	}

	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(certBytes) {
		return fmt.Errorf("credentials: failed to append certificate")
	}

	macBytes, err := os.ReadFile(cfg.MacaroonPath)
	if err != nil {
		return fmt.Errorf("error reading macaroon file %v: %w",
			cfg.MacaroonPath, err)
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return fmt.Errorf("error decoding macaroon: %w", err)
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return fmt.Errorf("error creating creds: %w", err)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(
			cp, "",
		)),
		grpc.WithPerRPCCredentials(macCred),
		grpc.WithBlock(),
	}

	ctxt, cancel := context.WithTimeout(ctx, cfg.Timeout)

	// In the blocking case, ctx can be used to cancel or expire the pending
	// connection. Once this function returns, the cancellation and
	// expiration of ctx will be noop. Users should call ClientConn.Close to
	// terminate all the pending operations after this function returns.
	defer cancel()

	conn, err := grpc.DialContext(ctxt, cfg.RPCHost, opts...)
	if err != nil {
		return fmt.Errorf("unable to connect to RPC server: %w", err)
	}

	// If we were able to connect to the remote signer, we store the
	// connection in the OutboundConnection struct.
	r.conn = conn
	r.SignerClient = signrpc.NewSignerClient(conn)
	r.WalletKitClient = walletrpc.NewWalletKitClient(conn)

	return nil
}

// A compile time assertion to ensure OutboundConnection meets the
// RemoteSignerConnection interface.
var _ RemoteSignerConnection = (*OutboundConnection)(nil)

// InboundRemoteSignerConnection is an interface that abstracts the
// communication with an outbound remote signer. It extends the
// RemoteSignerConnection insterface.
type InboundRemoteSignerConnection interface {
	RemoteSignerConnection

	// AddConnection feeds the inbound connection handler with the incoming
	// stream set up by an outbound remote signer and then blocks until the
	// stream is closed. Lnd can then send any requests to the remote signer
	// through the stream.
	AddConnection(stream StreamServer) error
}

// InboundConnection is an abstraction that manages the inbound connection that
// is set up by an outbound remote signer that connects to the watch-only node.
type InboundConnection struct {
	*SignCoordinator

	connectionTimeout time.Duration
}

// NewInboundConnection creates a new InboundConnection instance.
func NewInboundConnection(requestTimeout time.Duration,
	connectionTimeout time.Duration) *InboundConnection {

	remoteSigner := &InboundConnection{
		connectionTimeout: connectionTimeout,
	}

	remoteSigner.SignCoordinator = NewSignCoordinator(
		requestTimeout, connectionTimeout,
	)

	return remoteSigner
}

// Timeout returns the set connection timeout for the remote signer.
//
// NOTE: This is part of the RemoteSignerConnection interface.
func (r *InboundConnection) Timeout() time.Duration {
	return r.connectionTimeout
}

// Ready returns a channel that nil gets sent over once the remote signer
// connected and is ready to accept requests.
//
// NOTE: This is part of the RemoteSignerConnection interface.
func (r *InboundConnection) Ready(ctx context.Context) chan error {
	readyChan := make(chan error, 1)

	// We wait for the remote signer to connect in a go func and signal
	// over the channel once it's ready.
	go func() {
		log.Infof("Waiting for the remote signer to connect")

		readyChan <- r.SignCoordinator.WaitUntilConnected(ctx)
		close(readyChan)
	}()

	return readyChan
}

// Ping verifies that the remote signer is still responsive.
//
// NOTE: This is part of the RemoteSignerConnection interface.
func (r *InboundConnection) Ping(ctx context.Context,
	timeout time.Duration) error {

	pong, err := r.SignCoordinator.Ping(ctx, timeout)
	if err != nil {
		return fmt.Errorf("ping request to remote signer "+
			"errored: %w", err)
	}

	if !pong {
		return errors.New("incorrect Pong response from remote signer")
	}

	return nil
}

// AddConnection feeds the inbound connection handler with the incoming stream
// set up by an outbound remote signer and then blocks until the stream is
// closed. Lnd can then send any requests to the remote signer through the
// stream.
//
// NOTE: This is part of the InboundRemoteSignerConnection interface.
func (r *InboundConnection) AddConnection(stream StreamServer) error {
	return r.SignCoordinator.Run(stream)
}

// A compile time assertion to ensure InboundConnection meets the
// RemoteSigner interface.
var _ InboundRemoteSignerConnection = (*InboundConnection)(nil)
